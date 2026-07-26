package bot

import (
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Case SLA evaluation.
//
// Nothing here is stored. A case carries only timestamps (sessionstore/sla.go)
// and its project carries only targets (config.SLATarget); "breached" is derived
// from the two every time it is rendered. A stored breach flag would be a lie
// within a minute of being written — the deadline passes with no writer running,
// and the only thing that could refresh it is a sweeper whose whole job is to
// re-derive what this function derives for free.

// SLAClock is one deadline on one case, evaluated at an instant.
type SLAClock struct {
	// Active is false when the case's severity has no target for this clock, or
	// the case has no round start to measure from (pre-SLA data).
	Active bool
	// Target is the allowance; Elapsed is how much of it has been used at the
	// evaluation instant, or at the clock's stop when it has one.
	Target  time.Duration
	Elapsed time.Duration
	// Stopped means the clock has finished for good: the response happened, or
	// the case is resolved. Held means it is frozen but not finished — see
	// caseSLAHold for the pause rule.
	Stopped  bool
	Held     bool
	Breached bool
}

// Remaining is the allowance left, zero once breached.
func (c SLAClock) Remaining() time.Duration {
	if !c.Active || c.Elapsed >= c.Target {
		return 0
	}
	return c.Target - c.Elapsed
}

// Over is how far past target the clock is, zero when inside it.
func (c SLAClock) Over() time.Duration {
	if !c.Active || c.Elapsed <= c.Target {
		return 0
	}
	return c.Elapsed - c.Target
}

// CaseSLA is a case's standing against its project's SLA targets. Breached and
// Held are the two facts the board renders; the clocks carry the numbers behind
// them.
type CaseSLA struct {
	// Active is true when at least one clock applies. False means this case has
	// no SLA — an unset target, an unsevered case, or one filed before the
	// timestamps existed — and nothing about SLAs should be shown for it.
	Active        bool
	Breached      bool
	Held          bool
	FirstResponse SLAClock
	Resolution    SLAClock
	// Badge is the board chip's text ("SLA · first response 2h over"), empty
	// when no clock applies. Detail spells both clocks out for its tooltip.
	Badge  string
	Detail string
}

// caseSLAHold reports whether the resolution clock is paused, and the instant it
// froze at.
//
// PAUSE SEMANTICS — decided, not accidental. A case in phase `answered` is
// waiting on the customer, and that time is not ours to spend, so its resolution
// clock freezes at the moment we handed the case back (AnsweredAt). The
// first-response clock is deliberately unaffected: reaching `answered` *is* a
// response, so that clock has already stopped on its own.
//
// The pause is honored only while the case is actually in `answered`. If the
// customer replies and the case moves back to investigate or fixing, the waiting
// time is counted again. That is a real inaccuracy and it is the chosen one:
// subtracting it would need a durable ledger of pause intervals plus a writer on
// every transition out of `answered`, and a missed writer leaves a case paused
// forever — an SLA badge that silently under-reports is worse than useless,
// because the case it hides is exactly the case it exists to surface. This
// version can only over-report, which someone sees and corrects.
//
// A case sitting in `answered` with no AnsweredAt stamp (answered before this
// existed) keeps its clock running rather than freezing at an unknown instant.
func caseSLAHold(e sessionstore.Entry) (time.Time, bool) {
	if e.CasePhase() != sessionstore.PhaseAnswered {
		return time.Time{}, false
	}
	return sessionstore.ParseStamp(e.AnsweredAt)
}

// CaseSLAFor computes a case's SLA standing now, against its project's targets.
func (b *Bot) CaseSLAFor(e sessionstore.Entry) CaseSLA {
	return b.caseSLAAt(e, time.Now())
}

func (b *Bot) caseSLAAt(e sessionstore.Entry, now time.Time) CaseSLA {
	if b == nil || b.cfg == nil || !e.IsCase() {
		return CaseSLA{}
	}
	firstResponse, resolution := b.cfg.ProjectSLA(e.Project, e.Severity)
	return computeCaseSLA(e, firstResponse, resolution, now)
}

// computeCaseSLA is the whole policy, with the targets already resolved so it
// can be tested without a config.
func computeCaseSLA(e sessionstore.Entry, firstResponseTarget, resolutionTarget time.Duration, now time.Time) CaseSLA {
	var out CaseSLA
	if !e.IsCase() || (firstResponseTarget <= 0 && resolutionTarget <= 0) {
		return out
	}
	start, ok := e.SLARoundStart()
	if !ok {
		return out
	}
	responded, hasResponse := sessionstore.ParseStamp(e.FirstResponseAt)
	resolved, hasResolution := sessionstore.ParseStamp(e.ResolvedAt)
	heldAt, held := caseSLAHold(e)

	if firstResponseTarget > 0 {
		c := SLAClock{Active: true, Target: firstResponseTarget}
		switch {
		case hasResponse:
			c.Elapsed, c.Stopped = responded.Sub(start), true
		case hasResolution:
			// Closed without ever replying (duplicate, wontfix): the clock stops
			// at the close, because no first response is coming now. Leaving it
			// running would grow a breach on a finished case forever.
			c.Elapsed, c.Stopped = resolved.Sub(start), true
		default:
			c.Elapsed = now.Sub(start)
		}
		out.FirstResponse = finishClock(c)
	}
	if resolutionTarget > 0 {
		c := SLAClock{Active: true, Target: resolutionTarget}
		switch {
		case hasResolution:
			c.Elapsed, c.Stopped = resolved.Sub(start), true
		case held:
			c.Elapsed, c.Held = heldAt.Sub(start), true
		default:
			c.Elapsed = now.Sub(start)
		}
		out.Resolution = finishClock(c)
	}

	out.Active = out.FirstResponse.Active || out.Resolution.Active
	out.Breached = out.FirstResponse.Breached || out.Resolution.Breached
	out.Held = out.Resolution.Held
	out.Badge, out.Detail = caseSLAText(out)
	return out
}

// finishClock clamps and decides the breach.
//
// A negative elapsed means a stop stamped before the start — clock skew, or
// hand-edited JSON. Clamping to zero keeps that case reading as "just opened"
// rather than as an impossible deadline, and the breach test stays strictly
// greater-than so being exactly on target is met, not missed.
func finishClock(c SLAClock) SLAClock {
	if c.Elapsed < 0 {
		c.Elapsed = 0
	}
	c.Breached = c.Elapsed > c.Target
	return c
}

// caseSLAText phrases the standing for a board chip and its tooltip. Breaches
// come first, and the first-response breach outranks the resolution one: it is
// the promise the customer noticed being broken.
func caseSLAText(s CaseSLA) (badge, detail string) {
	if !s.Active {
		return "", ""
	}
	var parts []string
	for _, c := range []struct {
		name  string
		clock SLAClock
	}{
		{"first response", s.FirstResponse},
		{"resolution", s.Resolution},
	} {
		if !c.clock.Active {
			continue
		}
		line := c.name + " " + formatCoarseDuration(c.clock.Elapsed) + " of " + formatCoarseDuration(c.clock.Target)
		switch {
		case c.clock.Breached:
			line += " · breached"
		case c.clock.Held:
			line += " · on hold"
		case c.clock.Stopped:
			line += " · met"
		}
		parts = append(parts, line)
	}
	detail = strings.Join(parts, " · ")

	switch {
	case s.FirstResponse.Breached:
		return "SLA · first response " + formatCoarseDuration(s.FirstResponse.Over()) + " over", detail
	case s.Resolution.Breached:
		return "SLA · resolution " + formatCoarseDuration(s.Resolution.Over()) + " over", detail
	}
	// Not breached: name the tightest clock still running, so a chip is a
	// warning before it is a complaint.
	var tightest SLAClock
	name := ""
	for _, c := range []struct {
		name  string
		clock SLAClock
	}{
		{"first response", s.FirstResponse},
		{"resolution", s.Resolution},
	} {
		if !c.clock.Active || c.clock.Stopped {
			continue
		}
		if name == "" || c.clock.Remaining() < tightest.Remaining() {
			name, tightest = c.name, c.clock
		}
	}
	switch {
	case name == "":
		// Every active clock has stopped inside its target.
		return "SLA · met", detail
	case tightest.Held:
		return "SLA · on hold", detail
	default:
		return "SLA · " + name + " " + formatCoarseDuration(tightest.Remaining()) + " left", detail
	}
}

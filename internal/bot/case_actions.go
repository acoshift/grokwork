package bot

import (
	"fmt"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// Case action errors (web + tests).
var (
	ErrNotACase       = fmt.Errorf("not a case session")
	ErrCaseClosed     = fmt.Errorf("case is closed")
	ErrCaseNotClosed  = fmt.Errorf("case is not closed")
	ErrCaseBadPhase   = fmt.Errorf("invalid reopen phase (use investigate or fixing)")
	ErrCaseForbidden  = fmt.Errorf("not allowed for this case action")
	ErrCaseNoSession  = fmt.Errorf("unknown session")
	ErrCaseEmptyTitle = fmt.Errorf("customer update empty after sanitizer")
)

// EscalateCaseOpts is one escalation. Caps must be checked by the caller
// (FileEscalation / builder-class); TakeOwnership carries the part of that
// decision the escalation itself depends on.
type EscalateCaseOpts struct {
	ThreadID string
	Actor    Actor
	Note     string
	// TakeOwnership is true when the actor is builder-class: escalating is them
	// picking the work up, so they become the case engineer. False is a support-side
	// escalation — it *clears* the engineer, which is what puts the case on the
	// "needs an engineer" filter instead of looking claimed by whoever filed it.
	TakeOwnership bool
}

// EscalateOutcome reports what an escalation actually did to the engineer
// assignment. Callers phrase their reply from this rather than from the caps they
// passed in: the two disagree whenever there is no actor identity to assign (web
// auth disabled), and a reply that claims "assigned to you" while the store says
// nobody is the kind of drift nobody notices until triage goes wrong.
type EscalateOutcome struct {
	// PreviousEngineerID is who held the case before this escalation ("" = nobody).
	PreviousEngineerID string
	// EngineerID is who holds it after ("" = nobody).
	EngineerID string
	// Assigned is true when this escalation claimed the case for the actor.
	Assigned bool
	// Released is true when this escalation cleared someone else's assignment.
	Released bool
}

// EscalateCase moves Mode=case → Phase=fixing (K17: Mode stays case) and settles
// who owns the escalation. Shared by web and Discord so both surfaces assign the
// engineer the same way.
func (b *Bot) EscalateCase(opts EscalateCaseOpts) (EscalateOutcome, error) {
	var out EscalateOutcome
	if b == nil || b.sessions == nil {
		return out, fmt.Errorf("bot unavailable")
	}
	threadID := strings.TrimSpace(opts.ThreadID)
	e, ok := b.sessions.Get(threadID)
	if !ok {
		return out, ErrCaseNoSession
	}
	if !e.IsCase() {
		return out, ErrNotACase
	}
	if e.IsCaseClosed() {
		return out, ErrCaseClosed
	}
	note := strings.TrimSpace(opts.Note)
	actorID := strings.TrimSpace(opts.Actor.ID)
	now := time.Now().UTC().Format(time.RFC3339)
	_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		// Read the assignment decision off the entry as it stands, before this patch
		// moves the phase.
		alreadyWithEng := ent.Phase == sessionstore.PhaseFixing || ent.Phase == sessionstore.PhaseShipping
		out = EscalateOutcome{PreviousEngineerID: ent.EngineerID, EngineerID: ent.EngineerID}
		switch {
		case opts.TakeOwnership && actorID != "":
			ent.EngineerID = actorID
			ent.EngineerName = opts.Actor.String()
			out.EngineerID = actorID
			out.Assigned = true
		case opts.TakeOwnership:
			// Builder-class but no identity to record (web auth disabled). Claiming is
			// impossible, so leave the assignment exactly as it was rather than
			// clearing it as a side effect of an unrelated deployment mode.
		case alreadyWithEng && ent.EngineerID != "":
			// Already with engineering and someone is on it: a support-side re-escalate
			// is a nudge, not a reassignment. Yanking the engineer here would erase a
			// name the escalator cannot even see on this page.
		default:
			// Entering engineering from the support side: hand it to engineering as a
			// whole so the board shows it needs an engineer, rather than looking claimed
			// by whoever filed it.
			out.Released = ent.EngineerID != ""
			out.EngineerID = ""
			ent.EngineerID = ""
			ent.EngineerName = ""
		}
		ent.Mode = ModeCase
		ent.Phase = sessionstore.PhaseFixing
		ent.EscalatedAt = now
		ent.EscalatedBy = actorID
		if note != "" {
			if ent.Dossier == nil {
				ent.Dossier = &sessionstore.Dossier{}
			}
			ent.Dossier.NextActions = append(ent.Dossier.NextActions, "Escalate note: "+note)
		}
		if ent.Label == sessionstore.LabelBlocked || ent.Label == sessionstore.LabelOpen {
			ent.Label = sessionstore.LabelInProgress
		}
		_ = sessionstore.ClampCaseFields(ent)
	})
	if err != nil {
		return EscalateOutcome{}, err
	}
	return out, nil
}

// AnswerCase moves Mode=case → Phase=answered; optional note becomes customer update.
func (b *Bot) AnswerCase(threadID, actorID, note string) error {
	if b == nil || b.sessions == nil {
		return fmt.Errorf("bot unavailable")
	}
	threadID = strings.TrimSpace(threadID)
	e, ok := b.sessions.Get(threadID)
	if !ok {
		return ErrCaseNoSession
	}
	if !e.IsCase() {
		return ErrNotACase
	}
	if e.IsCaseClosed() {
		return ErrCaseClosed
	}
	note = strings.TrimSpace(note)
	_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.Mode = ModeCase
		ent.Phase = sessionstore.PhaseAnswered
		ent.Label = sessionstore.LabelBlocked
		// Answering is both a response and a handoff: it stops the first-response
		// clock and holds the resolution one while the customer has it.
		sessionstore.MarkCaseWaitingOnCustomer(ent, time.Now())
		if note != "" {
			clean, _ := SanitizeCustomerUpdate(note)
			if clean != "" {
				ent.CustomerUpdate = clean
			}
		}
		_ = sessionstore.ClampCaseFields(ent)
	})
	return err
}

// CloseCase freezes the case (Phase=closed). Caller enforces ownership.
func (b *Bot) CloseCase(threadID, actorID, resolution, note string) error {
	if b == nil || b.sessions == nil {
		return fmt.Errorf("bot unavailable")
	}
	threadID = strings.TrimSpace(threadID)
	e, ok := b.sessions.Get(threadID)
	if !ok {
		return ErrCaseNoSession
	}
	if !e.IsCase() {
		return ErrNotACase
	}
	if e.IsCaseClosed() {
		return ErrCaseClosed
	}
	res := strings.ToLower(strings.TrimSpace(resolution))
	if res == "" {
		res = "answered"
	}
	switch res {
	case "answered", "fixed", "duplicate", "wontfix", "escalated_external":
	default:
		// treat free text as answered + note
		if note == "" {
			note = resolution
		}
		res = "answered"
	}
	label := sessionstore.LabelDone
	switch res {
	case "wontfix", "escalated_external":
		label = sessionstore.LabelAbandoned
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.Mode = ModeCase
		ent.Phase = sessionstore.PhaseClosed
		ent.Resolution = res
		ent.ResolutionNote = strings.TrimSpace(note)
		ent.ResolvedAt = now
		ent.ResolvedBy = strings.TrimSpace(actorID)
		ent.Label = label
		_ = sessionstore.ClampCaseFields(ent)
	})
	return err
}

// ReopenCase reopens a closed Mode=case session.
// phase is investigate (default) or fixing; clears resolution fields; preserves Dossier.
// Caller enforces investigator-class capability (or session control).
func (b *Bot) ReopenCase(threadID, actorID, phase string) error {
	if b == nil || b.sessions == nil {
		return fmt.Errorf("bot unavailable")
	}
	threadID = strings.TrimSpace(threadID)
	e, ok := b.sessions.Get(threadID)
	if !ok {
		return ErrCaseNoSession
	}
	if !e.IsCase() {
		return ErrNotACase
	}
	if !e.IsCaseClosed() {
		return ErrCaseNotClosed
	}
	phase = strings.ToLower(strings.TrimSpace(phase))
	if phase == "" {
		phase = sessionstore.PhaseInvestigate
	}
	switch phase {
	case sessionstore.PhaseInvestigate, sessionstore.PhaseFixing:
	default:
		return ErrCaseBadPhase
	}
	label := sessionstore.LabelOpen
	if phase == sessionstore.PhaseFixing {
		label = sessionstore.LabelInProgress
	}
	now := time.Now().UTC().Format(time.RFC3339)
	actorID = strings.TrimSpace(actorID)
	_, _, err := b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.Mode = ModeCase
		ent.Phase = phase
		ent.Resolution = ""
		ent.ResolutionNote = ""
		ent.ResolvedAt = ""
		ent.ResolvedBy = ""
		ent.ReopenedAt = now
		ent.ReopenedBy = actorID
		// A reopen starts a fresh SLA round from ReopenedAt: the customer is
		// waiting again and has to be responded to again.
		sessionstore.ResetCaseSLARound(ent)
		ent.Label = label
		// Leave LabelManual as-is so a prior manual label can still be cleared via /label auto.
		_ = sessionstore.ClampCaseFields(ent)
	})
	return err
}

// CanReopenCaseCaps is the shared reopen gate (Discord + web): investigator-class.
func CanReopenCaseCaps(caps config.Capabilities) bool {
	return caps.Investigate || caps.FileEscalation || caps.StartSessions
}

// SetCaseCustomerUpdate sanitizes and stores customer-facing text.
// Returns cleaned text and redaction hits.
func (b *Bot) SetCaseCustomerUpdate(threadID, text string) (clean string, hits []string, err error) {
	if b == nil || b.sessions == nil {
		return "", nil, fmt.Errorf("bot unavailable")
	}
	threadID = strings.TrimSpace(threadID)
	e, ok := b.sessions.Get(threadID)
	if !ok {
		return "", nil, ErrCaseNoSession
	}
	if !e.IsCase() {
		return "", nil, ErrNotACase
	}
	if e.IsCaseClosed() {
		return "", nil, ErrCaseClosed
	}
	clean, hits = SanitizeCustomerUpdate(text)
	if clean == "" {
		return "", hits, ErrCaseEmptyTitle
	}
	_, _, err = b.sessions.Patch(threadID, func(ent *sessionstore.Entry) {
		ent.CustomerUpdate = clean
		// Customer-facing text is what the first-response clock measures.
		sessionstore.MarkCaseResponded(ent, time.Now())
		_ = sessionstore.ClampCaseFields(ent)
	})
	return clean, hits, err
}

// CanEscalateCaseCaps is the shared escalate gate (Discord + web).
func CanEscalateCaseCaps(caps config.Capabilities) bool {
	return canEscalateCase(caps)
}

// CanDraftCaseCaps is the answer / customer-update gate.
func CanDraftCaseCaps(caps config.Capabilities) bool {
	return caps.DraftCustomerReply || canEscalateCase(caps)
}

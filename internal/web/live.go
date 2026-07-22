package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/acoshift/grokwork/internal/bot"
)

// live domain event names (htmx hx-trigger="sse:<name>").
const (
	sseEventDashboard = "dashboard"
	sseEventShip      = "ship"
	sseEventCases     = "cases"
	sseEventHistory   = "history"
	sseEventWorktrees = "worktrees"
	sseEventConfig    = "config"
)

// liveRevs are content fingerprints for each live domain.
// Empty string means "unknown / not computed".
type liveRevs struct {
	Dashboard string `json:"dashboard"`
	Ship      string `json:"ship"`
	Cases     string `json:"cases"`
	History   string `json:"history"`
	Worktrees string `json:"worktrees"`
	Config    string `json:"config"`
}

func hashFingerprint(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// liveTextFingerprint is a compact rev input for streaming assistant text.
func liveTextFingerprint(text string) string {
	n := len(text)
	if n == 0 {
		return "0"
	}
	// Tail sample catches appends even when length is briefly stable across encodings.
	start := n - 64
	if start < 0 {
		start = 0
	}
	return fmt.Sprintf("%d:%s", n, hashFingerprint(text[start:]))
}

func (s *Server) computeLiveRevs() liveRevs {
	return liveRevs{
		Dashboard: s.fpDashboard(),
		Ship:      s.fpShip(),
		Cases:     s.fpCases(),
		History:   s.fpHistory(),
		Worktrees: s.fpWorktrees(),
		Config:    s.fpConfig(),
	}
}

func (s *Server) fpDashboard() string {
	snap := s.bot.StatusSnapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "a=%d q=%d s=%d p=%d empty=%d\n",
		snap.ActiveCount, snap.QueuedTotal, snap.SessionCount,
		snap.ProjectCount, snap.EmptyMemberProjects)
	for _, r := range snap.ActiveRuns {
		// Elapsed is recomputed each snapshot — include it so the UI ticks while runs are active.
		// LiveText/activity drive session-detail streaming; fingerprint length + a short tail hash
		// so the domain rev moves as the reply grows without hashing multi-100k bodies.
		liveFP := liveTextFingerprint(r.LiveText)
		fmt.Fprintf(&b, "%s|%s|%s|%d|%s|%s|%s\n",
			r.ThreadID, r.Project, r.Elapsed, r.QueueLen, r.Activity, r.Phases, liveFP)
	}
	return hashFingerprint(b.String())
}

func (s *Server) fpShip() string {
	// Unfiltered board fingerprint: any PR/session/run state change notifies ship listeners.
	// Partials re-apply the client's project/state filters on fetch.
	board := s.bot.ListShipBoard("", "all")
	var b strings.Builder
	fmt.Fprintf(&b, "total=%d open=%d draft=%d fail=%d chg=%d appr=%d m=%d c=%d\n",
		board.Total, board.Open, board.Draft, board.ChecksFailing,
		board.ChangesRequested, board.Approved, board.Merged, board.Closed)
	for _, r := range board.Rows {
		fmt.Fprintf(&b, "%s|%d|%s|%s|%s|%s|%v|%v|%d|%s\n",
			r.ThreadID, r.Number, r.State, r.Checks, r.Review, r.Label,
			r.Running, r.ChecksFailing, r.Queue, r.UpdatedAt)
	}
	return hashFingerprint(b.String())
}

func (s *Server) fpCases() string {
	// Unfiltered board fingerprint: any case change notifies cases listeners.
	// Partials re-apply the client's project/phase/severity filters on fetch.
	board := s.bot.ListCaseBoard("", "", "", "all")
	var b strings.Builder
	fmt.Fprintf(&b, "total=%d open=%d closed=%d\n", board.Total, board.OpenTotal, board.Closed)
	for _, g := range board.Groups {
		for _, r := range g.Rows {
			fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%v|%d|%s|%s|%s|%v\n",
				r.ThreadID, r.Project, r.Phase, r.Severity, r.Title,
				r.Running, r.Queue, r.UpdatedAt, r.PRState, r.Resolution, r.PRChecksFailing)
		}
	}
	return hashFingerprint(b.String())
}

func (s *Server) fpHistory() string {
	threads, err := s.history.List()
	if err != nil {
		return hashFingerprint("err", err.Error())
	}
	threads = mergeSessionRows(threads, s.sessions.List())
	var b strings.Builder
	for _, t := range threads {
		fmt.Fprintf(&b, "%s|%s|%d|%s|%s|%s\n",
			t.ThreadID, t.Project, t.TurnCount, t.UpdatedAt, t.LastUser, t.LastStatus)
	}
	return hashFingerprint(b.String())
}

func (s *Server) fpWorktrees() string {
	list := s.bot.ListWorktrees()
	var b strings.Builder
	fmt.Fprintf(&b, "ttl=%d n=%d\n", s.cfg.WorktreeIdleTTLDaysValue(), len(list))
	for _, w := range list {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%v|%v|%v|%v\n",
			w.ThreadID, w.Project, w.Branch, w.LastActiveAt, w.IdleFor,
			w.Busy, w.OnDisk, w.HasSession, w.IdlePastTTL)
	}
	return hashFingerprint(b.String())
}

func (s *Server) fpConfig() string {
	snap := s.cfg.Snapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "ttl=%d af=%v max=%d riskyDef=%v turns=%d timeoutMs=%d\n",
		snap.WorktreeIdleTTLDays, snap.AutoFixCI, snap.AutoFixCIMax, snap.RiskyPathUseDefault,
		snap.MaxTurns, snap.TimeoutMs)
	fmt.Fprintf(&b, "risky=%s\n", snap.RiskyPathGlobsText)
	fmt.Fprintf(&b, "invite=%s|%s\n", snap.ClientID, snap.InviteURL)
	for _, p := range snap.Projects {
		fmt.Fprintf(&b, "p|%s|%s|%v|%v\n", p.Name, p.Path, p.AllowedUserIDs, p.AllowedRoleIDs)
	}
	for _, c := range snap.Channels {
		fmt.Fprintf(&b, "c|%s|%s\n", c.ChannelID, c.Project)
	}
	return hashFingerprint(b.String())
}

// sseEvent is a domain change notification for htmx.
type sseEvent struct {
	Domain string `json:"domain"`
	Rev    string `json:"rev,omitempty"`
	// Revs is set on the initial hello event so mid-session reconnects can
	// refresh only domains that changed while the socket was down.
	Revs *liveRevs `json:"revs,omitempty"`
	// StatusSnapshot is included on the initial "message" event for tests/compat.
	*bot.StatusSnapshot
	Tick int64 `json:"tick,omitempty"`
}

// sse streams domain change events. Clients subscribe with hx-trigger="sse:<domain>"
// and only fetch the partials that match.
//
// First event is unnamed ("message") hello: StatusSnapshot + full liveRevs.
// The browser keeps last-seen revs; on reconnect it compares and re-fetches
// only changed domains (see layout.tmpl). Later ticks emit only domains whose
// fingerprint changed since this connection's baseline.
func (s *Server) sse(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	writeEvent := func(event string, payload any) bool {
		raw, err := json.Marshal(payload)
		if err != nil {
			log.Printf("web sse marshal: %v", err)
			return false
		}
		if event != "" {
			if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
				return false
			}
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Immediate hello so clients and tests do not wait on the ticker.
	// Include full revs for reconnect catch-up (client compares to last seen).
	// StatusSnapshot is ACL-filtered so members do not learn other projects' runs.
	prev := s.computeLiveRevs()
	snap := s.statusVisibleHTTP(r)
	if !writeEvent("", sseEvent{
		Domain:         "hello",
		Revs:           &prev,
		StatusSnapshot: &snap,
		Tick:           1,
	}) {
		return
	}

	var tick int64 = 1

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			tick++
			curr := s.computeLiveRevs()
			type pair struct {
				name string
				rev  string
				prev string
			}
			for _, p := range []pair{
				{sseEventDashboard, curr.Dashboard, prev.Dashboard},
				{sseEventShip, curr.Ship, prev.Ship},
				{sseEventCases, curr.Cases, prev.Cases},
				{sseEventHistory, curr.History, prev.History},
				{sseEventWorktrees, curr.Worktrees, prev.Worktrees},
				{sseEventConfig, curr.Config, prev.Config},
			} {
				if p.rev == p.prev {
					continue
				}
				if !writeEvent(p.name, sseEvent{
					Domain: p.name,
					Rev:    p.rev,
					Tick:   tick,
				}) {
					return
				}
			}
			prev = curr
		}
	}
}

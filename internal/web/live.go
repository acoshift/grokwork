package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/acoshift/grok-discord/internal/bot"
)

// live domain event names (htmx hx-trigger="sse:<name>").
const (
	sseEventDashboard = "dashboard"
	sseEventShip      = "ship"
	sseEventHistory   = "history"
	sseEventWorktrees = "worktrees"
	sseEventConfig    = "config"
)

// liveRevs are content fingerprints for each live domain.
// Empty string means "unknown / not computed".
type liveRevs struct {
	Dashboard string `json:"dashboard"`
	Ship      string `json:"ship"`
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

func (s *Server) computeLiveRevs() liveRevs {
	return liveRevs{
		Dashboard: s.fpDashboard(),
		Ship:      s.fpShip(),
		History:   s.fpHistory(),
		Worktrees: s.fpWorktrees(),
		Config:    s.fpConfig(),
	}
}

func (s *Server) fpDashboard() string {
	snap := s.bot.StatusSnapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "a=%d q=%d s=%d p=%d u=%d r=%d\n",
		snap.ActiveCount, snap.QueuedTotal, snap.SessionCount,
		snap.ProjectCount, snap.AllowUsers, snap.AllowRoles)
	for _, r := range snap.ActiveRuns {
		// Elapsed is recomputed each snapshot — include it so the UI ticks while runs are active.
		fmt.Fprintf(&b, "%s|%s|%s|%d\n", r.ThreadID, r.Project, r.Elapsed, r.QueueLen)
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
	// Digest includes today's date; exclude pure date churn by not hashing Digest.
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
	fmt.Fprintf(&b, "ttl=%d af=%v max=%d riskyDef=%v\n",
		snap.WorktreeIdleTTLDays, snap.AutoFixCI, snap.AutoFixCIMax, snap.RiskyPathUseDefault)
	fmt.Fprintf(&b, "risky=%s\n", snap.RiskyPathGlobsText)
	fmt.Fprintf(&b, "invite=%s|%s\n", snap.ClientID, snap.InviteURL)
	for _, p := range snap.Projects {
		fmt.Fprintf(&b, "p|%s|%s\n", p.Name, p.Path)
	}
	for _, c := range snap.Channels {
		fmt.Fprintf(&b, "c|%s|%s\n", c.ChannelID, c.Project)
	}
	users := append([]string(nil), snap.AllowedUserIDs...)
	roles := append([]string(nil), snap.AllowedRoleIDs...)
	sort.Strings(users)
	sort.Strings(roles)
	for _, u := range users {
		fmt.Fprintf(&b, "u|%s\n", u)
	}
	for _, r := range roles {
		fmt.Fprintf(&b, "r|%s\n", r)
	}
	return hashFingerprint(b.String())
}

// sseEvent is a domain change notification for htmx.
type sseEvent struct {
	Domain string `json:"domain"`
	Rev    string `json:"rev"`
	// StatusSnapshot is included on the initial "message" event for tests/compat.
	*bot.StatusSnapshot
	Tick int64 `json:"tick,omitempty"`
}

// sse streams domain change events. Clients subscribe with hx-trigger="sse:<domain>"
// and only fetch the partials that match.
//
// First event is unnamed ("message") with StatusSnapshot for connect/tests.
// Later ticks emit only domains whose fingerprint changed.
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

	// Immediate first event so clients and tests do not wait on the ticker.
	snap := s.bot.StatusSnapshot()
	if !writeEvent("", sseEvent{
		Domain:         "hello",
		StatusSnapshot: &snap,
		Tick:           1,
	}) {
		return
	}

	// Baseline revs: no domain events until something actually changes.
	prev := s.computeLiveRevs()
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

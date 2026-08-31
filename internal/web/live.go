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
	"github.com/acoshift/grokwork/internal/sessionstore"
)

// live domain event names (htmx hx-trigger="sse:<name>").
const (
	sseEventDashboard = "dashboard"
	sseEventShip      = "ship"
	sseEventCases     = "cases"
	sseEventHistory   = "history"
	sseEventWorktrees = "worktrees"
	sseEventConfig    = "config"
	sseEventDeploy    = "deploy"
	sseEventInbox     = "inbox"
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
	Deploy    string `json:"deploy"`
	Inbox     string `json:"inbox,omitempty"`
}

// fpInbox is this connection's inbox fingerprint. computeLiveRevs stays
// host-wide; stuffing a per-viewer value in there would make
// TestLiveRevsStableAndChange compare two users' feeds.
func (s *Server) fpInbox(r *http.Request) string {
	if r == nil || s == nil || s.bot == nil {
		return ""
	}
	if s.cfg != nil && !s.cfg.WebAuthEnabled() {
		return ""
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		sess = s.sessionFromRequest(r)
	}
	if sess == nil || strings.TrimSpace(sess.DiscordUserID) == "" {
		return ""
	}
	store := s.bot.Inbox()
	if store == nil {
		return ""
	}
	id := sess.DiscordUserID
	cur := store.ReadCursor(id)
	return hashFingerprint(fmt.Sprintf("%d:%d:%v", store.LastSeq(id), cur.Through, cur.Read))
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

// liveRevCache is the last host-wide domain fingerprint set. Dashboard/config/
// deploy are cheap and always refreshed; the rest are reused until a store
// generation, busy-set, SLA deadline, or idle-duration bucket moves.
type liveRevCache struct {
	have       bool
	revs       liveRevs
	sessionRev uint64
	historyRev uint64
	busyKey    string
	wtBucket   int64
	casesUntil time.Time
}

func (s *Server) computeLiveRevs() liveRevs {
	now := time.Now()
	var sessionRev, historyRev uint64
	if s.sessions != nil {
		sessionRev = s.sessions.Rev()
	}
	if s.history != nil {
		historyRev = s.history.Rev()
	}
	busyKey := ""
	if s.bot != nil {
		busyKey = s.bot.LiveBusyKey()
	}
	wtBucket := now.Unix() / 60

	s.liveMu.Lock()
	defer s.liveMu.Unlock()

	revs := liveRevs{
		Dashboard: s.fpDashboard(),
		Config:    s.fpConfig(),
		Deploy:    s.fpDeploys(),
	}
	if s.liveCache.have &&
		s.liveCache.sessionRev == sessionRev &&
		s.liveCache.historyRev == historyRev &&
		s.liveCache.busyKey == busyKey &&
		s.liveCache.wtBucket == wtBucket &&
		now.Before(s.liveCache.casesUntil) {
		revs.Ship = s.liveCache.revs.Ship
		revs.Cases = s.liveCache.revs.Cases
		revs.History = s.liveCache.revs.History
		revs.Worktrees = s.liveCache.revs.Worktrees
		return revs
	}

	cases, casesUntil := s.fpCases()
	revs.Ship = s.fpShip()
	revs.Cases = cases
	revs.History = s.fpHistory()
	revs.Worktrees = s.fpWorktrees()
	s.liveCache = liveRevCache{
		have:       true,
		revs:       revs,
		sessionRev: sessionRev,
		historyRev: historyRev,
		busyKey:    busyKey,
		wtBucket:   wtBucket,
		casesUntil: casesUntil,
	}
	return revs
}

// fpDeploys folds the engine's monotonic revision in with the active count.
//
// The rev is load-bearing: a lane claimed, run and released inside one 2s tick
// produces an identical RAM-derived fingerprint before and after, so a passive
// viewer's board would never refresh for that deploy.
func (s *Server) fpDeploys() string {
	if s.deploys == nil {
		return ""
	}
	return hashFingerprint(fmt.Sprintf("rev=%d active=%d", s.deploys.Rev(), s.deploys.ActiveCount()))
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
		fmt.Fprintf(&b, "%s|%s|%s|%d|%s|%s|%s",
			r.ThreadID, r.Project, r.Elapsed, r.QueueLen, r.Activity, r.Phases, liveFP)
		for _, a := range r.Artifacts {
			fmt.Fprintf(&b, "|a|%s", a.Name)
		}
		b.WriteByte('\n')
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

func (s *Server) fpCases() (string, time.Time) {
	// Unfiltered board fingerprint: any case change notifies cases listeners.
	// Partials re-apply the client's project/phase/severity filters on fetch.
	board := s.bot.ListCaseBoard("", "", "", "all")
	var b strings.Builder
	fmt.Fprintf(&b, "total=%d open=%d closed=%d\n", board.Total, board.OpenTotal, board.Closed)
	for _, g := range board.Groups {
		for _, r := range g.Rows {
			// SLA standing rides along even though nothing wrote it: a case
			// crossing its deadline changes no stored byte, so without this the
			// badge would only appear on the next navigation or store write.
			fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%v|%d|%s|%s|%s|%v|%v|%v\n",
				r.ThreadID, r.Project, r.Phase, r.Severity, r.Title,
				r.Running, r.Queue, r.UpdatedAt, r.PRState, r.Resolution, r.PRChecksFailing,
				r.SLABreached, r.SLAHeld)
		}
	}
	return hashFingerprint(b.String()), nextSLARecompute(board, time.Now())
}

// nextSLARecompute is when an unbreached, running SLA clock will next change
// standing. With no such clock the cases fingerprint is stable until a store
// write, so the idle SSE tick can skip the board walk until then.
func nextSLARecompute(board bot.CaseBoard, now time.Time) time.Time {
	var soonest time.Duration
	found := false
	for _, g := range board.Groups {
		for _, r := range g.Rows {
			for _, c := range []bot.SLAClock{r.SLA.FirstResponse, r.SLA.Resolution} {
				if !c.Active || c.Stopped || c.Held || c.Breached {
					continue
				}
				rem := c.Remaining()
				if rem <= 0 {
					continue
				}
				if !found || rem < soonest {
					soonest = rem
					found = true
				}
			}
		}
	}
	if !found {
		return now.Add(24 * time.Hour)
	}
	return now.Add(soonest)
}

func (s *Server) fpHistory() string {
	threads, err := s.history.List()
	if err != nil {
		return hashFingerprint("err", err.Error())
	}
	var sessions []sessionstore.Listed
	if s.sessions != nil {
		sessions = s.sessions.List()
	}
	threads = mergeSessionRows(threads, sessions)
	// Running is the idle/busy edge. history.Append lands before refreshPR /
	// postCompletion / finishRun, and Patch does not stamp UpdatedAt, so a
	// TurnCount-only fingerprint fires too early (or not again) and the
	// session page keeps stale Work unit / completion / case chrome.
	annotateSessionRunning(threads, s.bot)
	var b strings.Builder
	for _, t := range threads {
		// Goal is patched without stamping UpdatedAt (title summarize, /goal).
		// The session live region listens on history, so omitting it would leave
		// the generated title invisible until the next navigation.
		fmt.Fprintf(&b, "%s|%s|%d|%s|%s|%s|%s|%v\n",
			t.ThreadID, t.Project, t.TurnCount, t.UpdatedAt, t.LastUser, t.LastStatus, t.Goal, t.Running)
	}
	appendSessionLiveChrome(&b, sessions)
	return hashFingerprint(b.String())
}

// appendSessionLiveChrome folds the session-detail record region into the
// history fingerprint: Work unit (label, PRs, issues, branch), case dossier,
// last verify. None of these stamp UpdatedAt (Patch never invents it).
func appendSessionLiveChrome(b *strings.Builder, sessions []sessionstore.Listed) {
	for _, se := range sessions {
		e := se.Entry
		e.NormalizePRs()
		fmt.Fprintf(b, "s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s",
			se.ThreadID, e.EffectiveLabel(), e.Mode, e.CasePhase(), e.Resolution, e.ResolutionNote,
			e.WorktreeBranch, e.OwnerName, e.CustomerTitle, e.CustomerUpdate)
		for _, key := range e.RelatedCases {
			fmt.Fprintf(b, "|r|%s", key)
		}
		for _, pr := range e.PRs {
			fmt.Fprintf(b, "|p|%d|%s|%s|%v", pr.Number, pr.State, pr.Title, pr.IsDraft)
		}
		for _, iss := range e.Issues {
			fmt.Fprintf(b, "|i|%s|%s", iss.DisplayRef(), iss.EffectiveKeyword())
		}
		if e.LastVerify != nil {
			fmt.Fprintf(b, "|v|%s|%v|%s", e.LastVerify.At, e.LastVerify.OK, e.LastVerify.Name)
		}
		if e.Dossier != nil {
			fmt.Fprintf(b, "|d|%s|%s", e.Dossier.UpdatedAt, e.Dossier.Summary)
			for _, a := range e.Dossier.NextActions {
				fmt.Fprintf(b, "|n|%s", a)
			}
		}
		b.WriteByte('\n')
	}
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
		// MemberIDs is a deduped union of direct members and every team's members,
		// so it is not sufficient on its own: moving someone from one team to
		// another leaves the union byte-identical, and the Access tab — which
		// renders per-team rosters, and is the audit surface for adminProject now
		// that the Discord mod bypass is gone — would keep showing the old
		// assignment to every other viewer. Each team therefore contributes its own
		// member list, not just its key/label/template.
		fmt.Fprintf(&b, "p|%s|%s|%v\n", p.Name, p.Path, p.MemberIDs)
		for _, t := range p.Teams {
			fmt.Fprintf(&b, "t|%s|%s|%s|%s|%v\n", p.Name, t.Key, t.Label, t.Capabilities, t.Members)
		}
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
	prev.Inbox = s.fpInbox(r)
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
			curr.Inbox = s.fpInbox(r)
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
				{sseEventDeploy, curr.Deploy, prev.Deploy},
				{sseEventInbox, curr.Inbox, prev.Inbox},
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

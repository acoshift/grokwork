package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/sessionstore"
	"github.com/acoshift/grokwork/internal/timeline"
)

// Default Fix-with-Grok rate limit: max starts per actor per window.
const (
	fixStartRateMax    = 5
	fixStartRateWindow = time.Minute
)

// startRateLimiter is a simple sliding-window per-actor limiter.
type startRateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
	now    func() time.Time
}

func newStartRateLimiter(max int, window time.Duration) *startRateLimiter {
	if max <= 0 {
		max = fixStartRateMax
	}
	if window <= 0 {
		window = fixStartRateWindow
	}
	return &startRateLimiter{
		hits:   make(map[string][]time.Time),
		max:    max,
		window: window,
		now:    time.Now,
	}
}

// Allow reports whether actor may start now and records the hit when allowed.
func (l *startRateLimiter) Allow(actor string) bool {
	if l == nil {
		return true
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "anonymous"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-l.window)
	prev := l.hits[actor]
	kept := prev[:0]
	for _, t := range prev {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[actor] = kept
		return false
	}
	l.hits[actor] = append(kept, now)
	return true
}

func (s *Server) fixLimiter() *startRateLimiter {
	if s == nil {
		return nil
	}
	if s.startLimit == nil {
		s.startLimit = newStartRateLimiter(fixStartRateMax, fixStartRateWindow)
	}
	return s.startLimit
}

// Max issues accepted by list bulk Fix in one request.
const fixBulkMax = 10

func (s *Server) postIssuesBulkFix(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return ctx.Status(http.StatusForbidden).Error(err.Error())
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))

	project, ref, path, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.issuesListRedirect(ctx, project, owner, repo, "", err.Error())
	}
	owner, repo = ref.Owner, ref.Repo

	if err := ctx.Request.ParseForm(); err != nil {
		return s.issuesListRedirect(ctx, project, owner, repo, "", "invalid form")
	}
	numbers, err := parseIssueNumbers(ctx.Request.PostForm["numbers"])
	if err != nil {
		return s.issuesListRedirect(ctx, project, owner, repo, "", err.Error())
	}
	if len(numbers) == 0 {
		return s.issuesListRedirect(ctx, project, owner, repo, "", "select at least one issue")
	}
	if len(numbers) > fixBulkMax {
		return s.issuesListRedirect(ctx, project, owner, repo, "",
			fmt.Sprintf("too many issues (max %d per bulk Fix)", fixBulkMax))
	}

	if err := s.checkFixRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"project": project, "kind": "github-bulk", "owner": owner, "repo": repo, "count": len(numbers),
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	actor := s.fixActor(ctx)
	reqCtx := ctx.Context()
	gh := s.ghRun()

	// Start each issue in parallel: gh issue view + Discord thread create dominate latency.
	type bulkOne struct {
		n   int
		res bot.FixStartResult
		err error
	}
	out := make([]bulkOne, len(numbers))
	var wg sync.WaitGroup
	for i, n := range numbers {
		wg.Add(1)
		go func(i, n int) {
			defer wg.Done()
			info, _ := ghpr.ViewIssueWith(reqCtx, gh, path, n, owner, repo)
			title := strings.TrimSpace(info.Title)
			body := info.Body
			issueURL := info.URL
			if issueURL == "" {
				issueURL = fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, n)
			}
			res, startErr := s.bot.StartFix(bot.FixStartOpts{
				Kind:     bot.FixKindGitHub,
				Project:  project,
				Actor:    actor,
				ForceNew: true,
				Owner:    owner,
				Repo:     repo,
				Number:   n,
				Title:    title,
				URL:      issueURL,
				Body:     body,
			})
			out[i] = bulkOne{n: n, res: res, err: startErr}
			detail := map[string]any{
				"project": project, "kind": "github-bulk",
				"owner": owner, "repo": repo, "number": n,
				"threadId": res.ThreadID, "status": string(res.Status),
				"queuePos": res.QueuePos, "created": res.Created,
			}
			s.auditAction(ctx, audit.ActionSessionStart, startErr, detail)
		}(i, n)
	}
	wg.Wait()

	started := 0
	var failMsgs []string
	for _, r := range out {
		if r.err != nil {
			failMsgs = append(failMsgs, fmt.Sprintf("#%d: %s", r.n, r.err.Error()))
			continue
		}
		started++
	}
	if started > 0 {
		s.invalidateIssueListCache(project, owner, repo)
	}

	switch {
	case started == 0:
		msg := "No fix sessions started"
		if len(failMsgs) > 0 {
			msg = strings.Join(failMsgs, "; ")
		}
		return s.issuesListRedirect(ctx, project, owner, repo, "", msg)
	default:
		q := url.Values{}
		if len(failMsgs) > 0 {
			q.Set("ok", fmt.Sprintf("Started %d of %d fix sessions", started, len(numbers)))
			q.Set("err", strings.Join(failMsgs, "; "))
		} else {
			q.Set("ok", fmt.Sprintf("Started %d fix session%s", started, pluralS(started)))
		}
		loc := fmt.Sprintf("/projects/%s/sessions", url.PathEscape(project))
		if enc := q.Encode(); enc != "" {
			loc += "?" + enc
		}
		return ctx.Redirect(loc)
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// fixStatusFlash renders a FixStartResult.Status as a user-facing flash
// sentence, shared by every dispatch redirect (Fix, Start, Continue, Address
// CI/review) so the raw enum value never leaks into a query-param flash.
// FixStatusPicker is included for completeness even though callers resolve
// ErrPickerRequired into its own redirect before reaching this helper.
func fixStatusFlash(status bot.FixStartStatus) string {
	switch status {
	case bot.FixStatusStarted:
		return "Session started"
	case bot.FixStatusQueued:
		return "Queued — will start when a slot frees"
	case bot.FixStatusPicker:
		return "Multiple sessions match — pick one"
	default:
		return string(status)
	}
}

// parseIssueNumbers dedupes positive ints from form multi-values (order preserved).
func parseIssueNumbers(raw []string) ([]int, error) {
	seen := make(map[int]struct{}, len(raw))
	out := make([]int, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid issue number %q", s)
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out, nil
}

func (s *Server) issuesListRedirect(ctx *hime.Context, project, owner, repo, ok, errMsg string) error {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if ok != "" {
		q.Set("ok", ok)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	loc := fmt.Sprintf("/projects/%s/issues", url.PathEscape(project))
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	return ctx.Redirect(loc)
}

func (s *Server) postIssueFix(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	n, err := strconv.Atoi(strings.TrimSpace(ctx.PathValue("n")))
	if err != nil || n <= 0 {
		return ctx.Status(http.StatusBadRequest).Error("invalid issue number")
	}
	owner := strings.TrimSpace(ctx.PostFormValue("owner"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	forceNew := formBool(ctx.PostFormValue("force_new"))
	pickThread := strings.TrimSpace(ctx.PostFormValue("thread_id"))

	project, ref, path, err := s.resolveCatalogRepoAccess(ctx, project, owner, repo)
	if err != nil {
		return s.issueFixRedirect(ctx, project, owner, repo, n, "", err)
	}
	owner, repo = ref.Owner, ref.Repo

	if err := s.checkFixRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"project": project, "kind": "github", "owner": owner, "repo": repo, "number": n,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	// Fetch issue for title/body (best-effort).
	info, _ := ghpr.ViewIssueWith(ctx.Context(), s.ghRun(), path, n, owner, repo)
	title := strings.TrimSpace(info.Title)
	body := info.Body
	issueURL := info.URL
	if issueURL == "" {
		issueURL = fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, n)
	}

	actor := s.fixActor(ctx)
	res, startErr := s.bot.StartFix(bot.FixStartOpts{
		Kind:     bot.FixKindGitHub,
		Project:  project,
		Actor:    actor,
		ForceNew: forceNew,
		ThreadID: pickThread,
		Owner:    owner,
		Repo:     repo,
		Number:   n,
		Title:    title,
		URL:      issueURL,
		Body:     body,
	})
	return s.handleFixResult(ctx, startErr, res, fixRedirectContext{
		Kind: "github", Project: project, Owner: owner, Repo: repo, Number: n,
	})
}

func (s *Server) postLinearFix(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.PathValue("project"))
	if err := s.ensureProjectAccess(ctx, project); err != nil {
		return forbiddenProject(ctx, err)
	}
	identifier := strings.TrimSpace(ctx.PathValue("identifier"))
	if !s.cfg.ProjectLinearEnabled(project) {
		return ctx.Status(http.StatusBadRequest).Error(bot.ErrLinearDisabled.Error())
	}
	forceNew := formBool(ctx.PostFormValue("force_new"))
	pickThread := strings.TrimSpace(ctx.PostFormValue("thread_id"))

	if err := s.checkFixRate(ctx); err != nil {
		s.auditAction(ctx, audit.ActionSessionStart, err, map[string]any{
			"project": project, "kind": "linear", "identifier": identifier,
		})
		return ctx.Status(http.StatusTooManyRequests).Error(err.Error())
	}

	title, body, issueURL, state, linearID := "", "", "", "", ""
	if s.cfg.ProjectLinearCanResolve(project) {
		if iss, err := s.linearClient(project).GetByIdentifier(ctx.Context(), identifier); err == nil {
			title = iss.Title
			body = iss.Description
			issueURL = iss.URL
			state = iss.State
			linearID = iss.ID
			identifier = iss.Identifier
		}
	}

	actor := s.fixActor(ctx)
	res, startErr := s.bot.StartFix(bot.FixStartOpts{
		Kind:       bot.FixKindLinear,
		Project:    project,
		Actor:      actor,
		ForceNew:   forceNew,
		ThreadID:   pickThread,
		Identifier: identifier,
		LinearID:   linearID,
		Title:      title,
		URL:        issueURL,
		Body:       body,
		State:      state,
	})
	return s.handleFixResult(ctx, startErr, res, fixRedirectContext{
		Kind: "linear", Project: project, Identifier: identifier,
	})
}

type fixRedirectContext struct {
	Kind       string
	Project    string
	Owner      string
	Repo       string
	Number     int
	Identifier string
}

func (s *Server) handleFixResult(ctx *hime.Context, startErr error, res bot.FixStartResult, rc fixRedirectContext) error {
	detail := map[string]any{
		"project": rc.Project, "kind": rc.Kind,
		"threadId": res.ThreadID, "status": string(res.Status),
		"queuePos": res.QueuePos, "created": res.Created,
	}
	if rc.Kind == "github" {
		detail["owner"] = rc.Owner
		detail["repo"] = rc.Repo
		detail["number"] = rc.Number
	} else {
		detail["identifier"] = rc.Identifier
	}

	if errors.Is(startErr, bot.ErrPickerRequired) {
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detail)
		return s.fixPickerRedirect(ctx, rc, res.Hits)
	}
	if startErr != nil {
		s.auditAction(ctx, audit.ActionSessionStart, startErr, detail)
		return s.mapFixError(ctx, startErr, rc)
	}
	s.auditAction(ctx, audit.ActionSessionStart, nil, detail)
	if rc.Kind == "github" {
		s.invalidateIssueListCache(rc.Project, rc.Owner, rc.Repo)
	}

	ok := fixStatusFlash(res.Status)
	if res.DiscordOffline {
		ok = ok + "&discord=offline"
	}
	return s.sessionRedirect(ctx, res.ThreadID, ok, "")
}

func (s *Server) mapFixError(ctx *hime.Context, err error, rc fixRedirectContext) error {
	msg := err.Error()
	switch {
	case errors.Is(err, bot.ErrDiscordNotReady):
		return s.fixSourceRedirect(ctx, rc, "", msg, http.StatusServiceUnavailable)
	case errors.Is(err, bot.ErrQueueFull):
		return s.fixSourceRedirect(ctx, rc, "", msg, http.StatusConflict)
	case errors.Is(err, bot.ErrLinearDisabled):
		return s.fixSourceRedirect(ctx, rc, "", msg, http.StatusBadRequest)
	case errors.Is(err, bot.ErrInvalidIssue), errors.Is(err, bot.ErrProjectRequired):
		return s.fixSourceRedirect(ctx, rc, "", msg, http.StatusBadRequest)
	case errors.Is(err, bot.ErrCannotStartFix):
		return s.fixSourceRedirect(ctx, rc, "", msg, http.StatusForbidden)
	default:
		// PreferDiscordChannel and other config errors → 400
		low := strings.ToLower(msg)
		if strings.Contains(low, "channel") || strings.Contains(low, "mapped") {
			return s.fixSourceRedirect(ctx, rc, "", msg, http.StatusBadRequest)
		}
		return s.fixSourceRedirect(ctx, rc, "", msg, http.StatusBadRequest)
	}
}

func (s *Server) fixPickerRedirect(ctx *hime.Context, rc fixRedirectContext, hits []bot.IssueSessionHit) error {
	// Render issue page with picker embedded via query flash + store hits on page via GET param marker.
	// Simpler: redirect to GET detail?picker=1 and re-find hits on GET when picker=1.
	// Or post-render: for tests we redirect with err=picker and picker=1.
	q := url.Values{}
	q.Set("picker", "1")
	q.Set("err", "Multiple sessions bind this issue — pick one or force a new thread.")
	return s.fixSourceRedirectValues(ctx, rc, q, http.StatusFound)
}

func (s *Server) issueFixRedirect(ctx *hime.Context, project, owner, repo string, n int, ok string, err error) error {
	rc := fixRedirectContext{Kind: "github", Project: project, Owner: owner, Repo: repo, Number: n}
	if err != nil {
		return s.fixSourceRedirect(ctx, rc, ok, err.Error(), http.StatusFound)
	}
	return s.fixSourceRedirect(ctx, rc, ok, "", http.StatusFound)
}

func (s *Server) fixSourceRedirect(ctx *hime.Context, rc fixRedirectContext, ok, errMsg string, status int) error {
	q := url.Values{}
	if ok != "" {
		q.Set("ok", ok)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	return s.fixSourceRedirectValues(ctx, rc, q, status)
}

func (s *Server) fixSourceRedirectValues(ctx *hime.Context, rc fixRedirectContext, q url.Values, status int) error {
	var loc string
	switch rc.Kind {
	case "linear":
		loc = fmt.Sprintf("/projects/%s/linear/%s", url.PathEscape(rc.Project), url.PathEscape(rc.Identifier))
	default:
		loc = fmt.Sprintf("/projects/%s/issues/%d", url.PathEscape(rc.Project), rc.Number)
		if q == nil {
			q = url.Values{}
		}
		if rc.Owner != "" {
			q.Set("owner", rc.Owner)
		}
		if rc.Repo != "" {
			q.Set("repo", rc.Repo)
		}
	}
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	if status == http.StatusFound || status == http.StatusSeeOther || status == 0 {
		return ctx.Redirect(loc)
	}
	// Non-3xx: still redirect for browser UX when possible; for API-ish tests return status.
	// Use Redirect for 409/503 with flash is lossy; return Error with status for tests.
	if status == http.StatusTooManyRequests || status == http.StatusConflict ||
		status == http.StatusServiceUnavailable || status == http.StatusForbidden ||
		status == http.StatusBadRequest {
		// Prefer redirect with err query for 400-ish browser UX when flash works;
		// rate limit / conflict / 503 keep status codes for handlers/tests.
		if status == http.StatusBadRequest {
			return ctx.Redirect(loc)
		}
		return ctx.Status(status).Error(q.Get("err"))
	}
	return ctx.Redirect(loc)
}

func (s *Server) sessionRedirect(ctx *hime.Context, threadID, ok, errMsg string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ctx.Status(http.StatusInternalServerError).Error("missing thread id")
	}
	q := url.Values{}
	if ok != "" {
		// ok may already contain extra query fragments like "started&discord=offline"
		if strings.Contains(ok, "&") {
			parts := strings.SplitN(ok, "&", 2)
			q.Set("ok", parts[0])
			// parse remaining as key=value pairs
			for _, pair := range strings.Split(parts[1], "&") {
				kv := strings.SplitN(pair, "=", 2)
				if len(kv) == 2 {
					q.Set(kv[0], kv[1])
				} else if kv[0] != "" {
					q.Set(kv[0], "1")
				}
			}
		} else {
			q.Set("ok", ok)
		}
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	// Keep the workspace shell scoped after Fix/Continue/reset redirects.
	// threadProject falls back to history when the session store entry is gone
	// (e.g. right after a successful reset).
	if q.Get("project") == "" {
		if p := s.threadProject(threadID); p != "" {
			q.Set("project", p)
		}
	}
	loc := "/sessions/" + url.PathEscape(threadID)
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	return ctx.Redirect(loc)
}

func (s *Server) checkFixRate(ctx *hime.Context) error {
	actor, _ := s.auditActor(ctx)
	if !s.fixLimiter().Allow(actor) {
		return fmt.Errorf("rate limit exceeded: max %d Fix starts per minute", fixStartRateMax)
	}
	return nil
}

func (s *Server) fixActor(ctx *hime.Context) bot.Actor {
	sess := sessionFromContext(ctx.Context())
	if sess == nil {
		sess = s.sessionFromRequest(ctx.Request)
	}
	if sess == nil {
		return bot.Actor{}
	}
	name := sess.DisplayName
	if name == "" {
		name = sess.DiscordUserID
	}
	return bot.Actor{ID: sess.DiscordUserID, DisplayName: name}
}

func formBool(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true" || v == "on" || v == "yes"
}

// sessionPage is a thin dual-surface session view (redirect target after Fix).
func (s *Server) sessionPage(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if err := s.ensureSessionPageAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	d := s.sessionPageData(ctx, threadID)
	d.Title = "Session · " + threadID
	d.IsSessions = true
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
	if ctx.FormValue("discord") == "offline" {
		if d.Flash != "" {
			d.Flash += " · "
		}
		d.Flash += "Discord is offline — this run is not mirrored there."
	}
	return s.viewPage(ctx, "session", d)
}

// partialSession streams work-unit status + turns (including the in-flight reply).
func (s *Server) partialSession(ctx *hime.Context) error {
	threadID := strings.TrimSpace(ctx.PathValue("threadID"))
	if threadID == "" {
		return ctx.Status(http.StatusBadRequest).Error("missing thread id")
	}
	if err := s.ensureSessionPageAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	return s.viewFragment(ctx, "session", "session_live", s.sessionPageData(ctx, threadID))
}

// ensureSessionPageAccess is ensureThreadAccess plus a narrow fallback for the
// post-reset landing page only. When the store entry is gone and history has
// no project yet, a stamped ?project= that the caller may access is enough to
// view the dead session shell. Mutation POSTs still use ensureThreadAccess and
// refuse when the unit is unknown.
func (s *Server) ensureSessionPageAccess(ctx *hime.Context, threadID string) error {
	if _, err := s.ensureThreadAccess(ctx, threadID); err == nil {
		return nil
	} else if s.threadProject(threadID) != "" {
		// Project known from store/history — denial is real, do not override.
		return err
	} else {
		qp := strings.TrimSpace(ctx.FormValue("project"))
		if qp == "" {
			return err
		}
		return s.ensureProjectAccess(ctx, qp)
	}
}

// agentLabel names the CLI a thread's runs use ("Grok" / "Claude") for reply
// bubbles and run-status copy. It resolves through the bot so the caption
// matches what actually answers there — a thread stamped on claude must not
// have its transcript labelled "Grok". No bot wired means the default agent.
func (s *Server) agentLabel(threadID string) string {
	if s.bot == nil {
		return grokrun.AgentGrok.Label()
	}
	return s.bot.ThreadAgent(threadID).Label()
}

func (s *Server) sessionPageData(ctx *hime.Context, threadID string) pageData {
	d := s.basePage(ctx)
	d.ThreadID = threadID
	if ent, ok := s.sessions.Get(threadID); ok {
		d.HasSession = true
		ent.NormalizePRs()
		d.SessionEntry = ent
		d.Project = ent.Project
		d.DiscordURL = ent.DiscordURL
		if d.DiscordURL == "" {
			d.DiscordURL = bot.DiscordThreadURL(s.cfg.ProjectDiscordGuildID(ent.Project), threadID)
		}
		cwd, _ := s.resolveSessionDiffCwd(ent, threadID)
		d.HasWorktree = cwd != ""
	}
	d.AgentLabel = s.agentLabel(threadID)
	// Live run chips + streaming reply from bot snapshot.
	if s.bot != nil {
		snap := s.bot.StatusSnapshot()
		for _, r := range snap.ActiveRuns {
			if r.ThreadID == threadID {
				d.RunActivity = r.Activity
				d.RunPhases = r.Phases
				d.RunElapsed = r.Elapsed
				d.RunBusy = true
				d.RunQueue = r.QueueLen
				d.RunPrompt = r.Prompt
				d.RunLiveText = r.LiveText
				break
			}
		}
	}
	// Durable output for the newest run. history.Turn.Response is result.Text
	// only, so a run that ended without a final reply (cancelled, max turns)
	// records nothing there — on Discord the sealed chunks were still in the
	// thread, but a web-native unit had no other copy. The timeline is that copy.
	if s.bot != nil {
		if events := s.bot.Events(); events != nil {
			if evs, err := events.Read(threadID); err == nil {
				d.RunTranscript = timeline.LastRunTranscript(evs)
				// Newest completion record. A web-native unit has no Discord card,
				// so this is the only place its diff summary is visible.
				for i := len(evs) - 1; i >= 0; i-- {
					if evs[i].Kind != timeline.KindCompletion {
						continue
					}
					var c bot.CompletionCardInput
					if err := evs[i].DecodeData(&c); err == nil {
						d.Completion = &c
					}
					break
				}
			}
		}
	}
	if s.history != nil {
		if th, err := s.history.Get(threadID); err == nil {
			d.Thread = th
			if d.Project == "" {
				d.Project = th.Project
			}
		} else {
			d.Thread.ThreadID = threadID
		}
	} else {
		d.Thread.ThreadID = threadID
	}
	// URL workspace scope wins so ← Sessions returns to the project the user
	// was browsing (also covers history-only threads with ?project=).
	if d.NavProject != "" {
		d.Project = d.NavProject
	}
	d.BackHref, d.BackLabel = s.sessionBackLink(ctx, d)
	// Session lifecycle controls: fold the ownership/admin check into the
	// startSessions gate so control affordances only render when the matching
	// POST would actually run (auth-off keeps them hidden — the POSTs 404 there).
	d.CanControlSession = d.CanStartSession && s.canControlSession(ctx, d.SessionEntry)
	if s.bot != nil {
		d.QueueItems = s.bot.QueueItems(threadID)
	}
	// Case panel affordances when Mode=case.
	// On eng phases (fixing/shipping), hide support desk actions (investigate,
	// escalate, answer); keep customer-update + close and the Grok continue box.
	if d.SessionEntry.IsCase() && d.CanStartSession {
		caps := s.cfg.ResolveCapabilities(d.SessionEntry.Project, d.UserID, nil)
		closed := d.SessionEntry.IsCaseClosed()
		shipPhase := d.SessionEntry.IsCaseShipPhase()
		d.CanCaseEscalate = !closed && !shipPhase && bot.CanEscalateCaseCaps(caps)
		d.CanCaseDraft = !closed && bot.CanDraftCaseCaps(caps) // customer-update
		d.CanCaseAnswer = !closed && !shipPhase && bot.CanDraftCaseCaps(caps)
		d.CanCaseClose = !closed && d.CanControlSession
		d.CanCaseInvestigate = !closed && !shipPhase && (caps.Investigate || caps.FileEscalation || caps.StartSessions || d.CanControlSession)
		d.CanCaseReopen = closed && (bot.CanReopenCaseCaps(caps) || d.CanControlSession)
		// Cross-referencing stays available on a closed case: "the same thing
		// happened again" is exactly the moment someone reaches for the case
		// that was already resolved.
		d.CanLinkCase = caps.Investigate || caps.FileEscalation || caps.StartSessions || d.CanControlSession
	}
	if d.SessionEntry.IsCase() && s.bot != nil {
		d.CaseLinks = s.bot.RelatedCaseLinks(threadID)
	}
	return d
}

// attachFixPicker populates pageData.FixHits when detail is shown with ?picker=1.
func (s *Server) attachFixPicker(d *pageData, project, owner, repo string, number int, identifier string) {
	if d == nil || s.bot == nil {
		return
	}
	d.CanStartSession = s.canStartSession(*d)
	if identifier != "" {
		d.FixHits = s.bot.FindByLinearIssue(project, identifier, false)
		return
	}
	if number > 0 {
		d.FixHits = s.bot.FindByIssue(project, owner, repo, number, false)
	}
}

func (s *Server) canStartSession(d pageData) bool {
	if !s.cfg.FeatureStartSessions() {
		return false
	}
	if !s.cfg.WebAuthEnabled() {
		return false // features fail closed without auth in production
	}
	return config.RoleAtLeast(config.WebRole(d.UserRole), config.WebRoleMember)
}

// attachModelPicker fills the model-picker fields for a page that can dispatch a
// session. Gated on builder-class project caps, the same bar as Fix & ship: naming
// a model names the spend.
//
// def is the model the dispatch uses when nothing is picked — the task model on the
// start composer, the review model on the commit/PR cards. It is rendered on the
// "Default" option so the two defaults are never confused for each other. The
// options themselves carry no selection: a page can only ever start a *new*
// session, never re-point an existing one.
func (s *Server) attachModelPicker(d *pageData, project, def string) {
	if d == nil || !d.CanStartSession {
		return
	}
	if !s.cfg.ResolveCapabilities(project, d.UserID, nil).CanShip() {
		return
	}
	d.CanSelectModel = true
	d.ModelGroups = config.ModelGroups("")
	d.ModelDefaultLabel = strings.TrimSpace(def)
	if d.ModelDefaultLabel == "" {
		d.ModelDefaultLabel = "CLI default"
	}
}

// silence unused import if template-only types shift
var _ sessionstore.Entry

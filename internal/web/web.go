package web

import (
	"embed"
	"fmt"
	"io/fs"
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
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/linear"
	"github.com/acoshift/grokwork/internal/markdown"
	"github.com/acoshift/grokwork/internal/reviewstore"
	"github.com/acoshift/grokwork/internal/sessionstore"
)

//go:embed templates/*
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server is the private-network admin UI.
type Server struct {
	cfg         *config.Config
	sessions    *sessionstore.Store
	history     *history.Store
	bot         *bot.Bot
	app         *hime.App
	webSessions *sessionStore
	webUsers    *userStore   // durable name/avatar; survives logout
	oauth       DiscordOAuth // nil → HTTPDiscordOAuth
	audit       *audit.Logger
	// Test injectables (nil → production defaults).
	ghRunner  ghpr.Runner
	linearNew func(apiKey string) *linear.Client
	// Fix-with-Grok rate limit (lazy init).
	startLimit *startRateLimiter
	// PR raw-patch cache (page + per-file fragments share one gh pr diff).
	prPatchMu sync.Mutex
	prPatches map[string]prPatchEntry
	// Short-TTL GitHub issue list cache (page shell + partial share one gh call).
	issueListMu sync.Mutex
	issueLists  map[string]issueListCacheEntry
}

// New builds a hime app with dashboard, history, config, and SSE routes.
func New(cfg *config.Config, sessions *sessionstore.Store, hist *history.Store, b *bot.Bot) *Server {
	if err := cfg.ValidateWebAuth(); err != nil {
		panic("web: " + err.Error())
	}
	webSess, err := newSessionStore(cfg.DataDir)
	if err != nil {
		panic("web: session store: " + err.Error())
	}
	webUsers, err := newUserStore(cfg.DataDir)
	if err != nil {
		panic("web: user store: " + err.Error())
	}
	auditLog, err := audit.New(cfg.DataDir)
	if err != nil {
		panic("web: audit: " + err.Error())
	}
	s := &Server{cfg: cfg, sessions: sessions, history: hist, bot: b, webSessions: webSess, webUsers: webUsers, audit: auditLog}
	app := hime.New()
	app.Address(cfg.ListenAddr())
	// POST forms under hx-boost still use 3xx; non-boosted htmx posts get HX-Redirect.
	app.HTMXAwareRedirect = true
	// SSE needs an unbounded write timeout; page requests finish quickly.
	app.Server().WriteTimeout = 0
	app.Server().ReadTimeout = 15 * time.Second
	app.Server().IdleTimeout = 120 * time.Second
	// Do not sleep before stop, and do not wait for open SSE streams on exit.
	// (GraceTimeout==0 would use context.Background and hang until all conns end.)
	app.Server().WaitBeforeShutdown = 0
	app.Server().GraceTimeout = time.Millisecond

	app.Routes(hime.Routes{
		"home":                               "/",
		"login":                              "/login",
		"auth.discord":                       "/auth/discord",
		"auth.discord.callback":              "/auth/discord/callback",
		"logout":                             "/logout",
		"history":                            "/history",
		"history.thread":                     "/history/",
		"sessions":                           "/sessions",
		"sessions.thread":                    "/sessions/",
		"ship":                               "/ship",
		"worktrees":                          "/worktrees",
		"worktrees.prune":                    "/worktrees/prune",
		"worktrees.pruneIdle":                "/worktrees/prune-idle",
		"config":                             "/config",
		"config.addProject":                  "/config/projects",
		"config.removeProject":               "/config/projects/remove",
		"config.setProjectLinear":            "/config/projects/linear",
		"config.setProjectGitHub":            "/config/projects/github",
		"config.setProjectChannel":           "/config/projects/channel",
		"config.setProjectFetch":             "/config/projects/fetch",
		"config.setProjectShip":              "/config/projects/ship",
		"config.setProjectSafeTeam":          "/config/projects/safe-team",
		"config.setProjectVerify":            "/config/projects/verify",
		"config.setProjectCapabilityUser":    "/config/projects/capabilities/users",
		"config.removeProjectCapabilityUser": "/config/projects/capabilities/users/remove",
		"config.setProjectCapabilityRole":    "/config/projects/capabilities/roles",
		"config.removeProjectCapabilityRole": "/config/projects/capabilities/roles/remove",
		"config.setGuild":                    "/config/guild",
		"config.addProjectUser":              "/config/projects/users",
		"config.removeProjectUser":           "/config/projects/users/remove",
		"config.addProjectRole":              "/config/projects/roles",
		"config.removeProjectRole":           "/config/projects/roles/remove",
		"config.addChannel":                  "/config/channels",
		"config.removeChannel":               "/config/channels/remove",
		"config.settings":                    "/config/settings",
		"issues":                             "/issues",
		"issues.project":                     "/projects/",
		"commits":                            "/commits",
		"pr.detail":                          "/prs/",
		"sse":                                "/events",
		// Live partials (htmx SSE domain swaps) — separate URLs so each region
		// can refresh independently. Fragments render via View("page#define").
		"partial.home.projects":   "/partials/home/projects",
		"partial.home.runs":       "/partials/home/runs",
		"partial.project.pulse":   "/partials/projects/pulse",
		"partial.ship.stats":      "/partials/ship/stats",
		"partial.ship.table":      "/partials/ship/table",
		"partial.cases.pipeline":  "/partials/cases/pipeline",
		"partial.cases.list":      "/partials/cases/list",
		"partial.history.table":   "/partials/history/table",
		"partial.history.turns":   "/partials/history/turns/",
		"partial.session":         "/partials/sessions/",
		"partial.worktrees.table": "/partials/worktrees/table",
		"partial.issues.table":    "/partials/issues/table",
		"partial.config.lists":    "/partials/config/lists",
	})

	app.TemplateFunc("add", func(a, b int) int { return a + b })
	app.TemplateFunc("markdown", markdown.Render)
	// shortTime formats a time.Time or RFC3339 string as "2006-01-02 15:04"
	// (same layout as the commits list Date column).
	app.TemplateFunc("shortTime", shortTime)

	// One template set per page: layout root for full documents; named {{define}}s
	// for SSE fragments (ctx.View("dashboard#dashboard_stats", …)).
	tp := app.Template()
	tp.FS(templateFS)
	tp.Dir("templates")
	tp.Root("layout")
	tp.ParseFiles("home", "layout.tmpl", "home.tmpl")
	tp.ParseFiles("project_overview", "layout.tmpl", "project_overview.tmpl")
	tp.ParseFiles("history", "layout.tmpl", "history.tmpl")
	tp.ParseFiles("history_detail", "layout.tmpl", "history_detail.tmpl")
	tp.ParseFiles("sessions", "layout.tmpl", "sessions.tmpl")
	tp.ParseFiles("ship", "layout.tmpl", "ship.tmpl")
	tp.ParseFiles("cases", "layout.tmpl", "cases.tmpl")
	tp.ParseFiles("worktrees", "layout.tmpl", "worktrees.tmpl")
	tp.ParseFiles("config", "layout.tmpl", "config.tmpl")
	tp.ParseFiles("project_config", "layout.tmpl", "project_config.tmpl")
	tp.ParseFiles("login", "layout.tmpl", "login.tmpl")
	tp.ParseFiles("issues", "layout.tmpl", "issues.tmpl")
	tp.ParseFiles("issue_detail", "layout.tmpl", "issue_detail.tmpl")
	tp.ParseFiles("linear_issues", "layout.tmpl", "linear_issues.tmpl")
	tp.ParseFiles("linear_detail", "layout.tmpl", "linear_detail.tmpl")
	tp.ParseFiles("pr_detail", "layout.tmpl", "pr_detail.tmpl")
	tp.ParseFiles("reviews", "layout.tmpl", "reviews.tmpl")
	tp.ParseFiles("diff", "layout.tmpl", "diff.tmpl", "diff_review.tmpl")
	tp.ParseFiles("session", "layout.tmpl", "session.tmpl")
	tp.ParseFiles("start", "layout.tmpl", "start.tmpl")
	tp.ParseFiles("commits", "layout.tmpl", "commits.tmpl")
	tp.ParseFiles("commit_detail", "layout.tmpl", "commit_detail.tmpl", "diff_review.tmpl")

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: static fs: " + err.Error())
	}

	mux := http.NewServeMux()
	// Public (static + PWA install assets + auth)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	registerPWA(mux)
	mux.Handle("GET /login", hime.Handler(s.loginPage))
	mux.Handle("GET /auth/discord", hime.Handler(s.oauthDiscordStart))
	mux.Handle("GET /auth/discord/callback", hime.Handler(s.oauthDiscordCallback))
	mux.Handle("POST /logout", hime.Handler(s.logout))

	// Authenticated pages + SSE + partials
	mux.Handle("GET /{$}", s.requireAuth(hime.Handler(s.home)))
	mux.Handle("GET /history", s.requireAuth(hime.Handler(s.historyList)))
	mux.Handle("GET /history/{threadID}", s.requireAuth(hime.Handler(s.historyDetail)))
	mux.Handle("GET /sessions", s.requireAuth(hime.Handler(s.sessionsList)))
	mux.Handle("GET /sessions/{threadID}/diff", s.requireAuth(hime.Handler(s.sessionDiffPage)))
	mux.Handle("GET /sessions/{threadID}/diff/file", s.requireAuth(hime.Handler(s.sessionDiffFile)))
	mux.Handle("GET /sessions/{threadID}", s.requireAuth(hime.Handler(s.sessionPage)))
	mux.Handle("GET /ship", s.requireAuth(hime.Handler(s.shipPage)))
	mux.Handle("GET /worktrees", s.requireAuth(hime.Handler(s.worktreesPage)))
	mux.Handle("GET /config", s.requireAdmin(hime.Handler(s.configPage)))
	mux.Handle("GET /config/projects/{name}", s.requireAdmin(hime.Handler(s.projectConfigPage)))
	// Project workspace (project-first UX): overview + scoped list pages.
	mux.Handle("GET /projects/{project}", s.requireAuth(hime.Handler(s.projectOverview)))
	mux.Handle("GET /projects/{project}/start", s.requireAuth(hime.Handler(s.startComposer)))
	mux.Handle("GET /projects/{project}/ship", s.requireAuth(hime.Handler(s.shipScoped)))
	mux.Handle("GET /projects/{project}/cases", s.requireAuth(hime.Handler(s.casesScoped)))
	mux.Handle("GET /projects/{project}/sessions", s.requireAuth(hime.Handler(s.sessionsScoped)))
	mux.Handle("GET /projects/{project}/worktrees", s.requireAuth(hime.Handler(s.worktreesScoped)))
	// Retired feature-first hubs → launcher.
	mux.Handle("GET /issues", s.requireAuth(hime.Handler(s.redirectHome)))
	mux.Handle("GET /projects/{project}/issues", s.requireAuth(hime.Handler(s.issuesList)))
	mux.Handle("GET /projects/{project}/issues/{n}", s.requireAuth(hime.Handler(s.issueDetail)))
	mux.Handle("GET /projects/{project}/linear", s.requireAuth(hime.Handler(s.linearList)))
	mux.Handle("GET /projects/{project}/linear/{identifier}", s.requireAuth(hime.Handler(s.linearDetail)))
	mux.Handle("GET /commits", s.requireAuth(hime.Handler(s.redirectHome)))
	mux.Handle("GET /projects/{project}/commits", s.requireAuth(hime.Handler(s.commitsList)))
	mux.Handle("POST /projects/{project}/commits/fetch", s.requireMember(hime.Handler(s.postCommitsFetch)))
	mux.Handle("GET /projects/{project}/commits/{sha}", s.requireAuth(hime.Handler(s.commitDetail)))
	mux.Handle("GET /projects/{project}/commits/{sha}/file", s.requireAuth(hime.Handler(s.commitDiffFile)))
	mux.Handle("GET /prs/{owner}/{repo}/{n}", s.requireAuth(hime.Handler(s.prDetail)))
	mux.Handle("GET /prs/{owner}/{repo}/{n}/diff", s.requireAuth(hime.Handler(s.prDiffPage)))
	mux.Handle("GET /prs/{owner}/{repo}/{n}/diff/file", s.requireAuth(hime.Handler(s.prDiffFile)))
	// GitHub writes (PR8–9): always registered; request-time feature + role gates.
	mux.Handle("POST /projects/{project}/issues/{n}/comments",
		s.requireFeature("githubWrites", s.requireMember(hime.Handler(s.postIssueComment))))
	mux.Handle("POST /projects/{project}/issues/{n}/close",
		s.requireFeature("githubWrites", s.requireMember(hime.Handler(s.postIssueClose))))
	mux.Handle("POST /prs/{owner}/{repo}/{n}/comments",
		s.requireFeature("githubWrites", s.requireMember(hime.Handler(s.postPRComment))))
	mux.Handle("POST /prs/{owner}/{repo}/{n}/close",
		s.requireFeature("githubWrites", s.requireMember(hime.Handler(s.postPRClose))))
	mux.Handle("POST /prs/{owner}/{repo}/{n}/merge",
		s.requireFeature("merge", s.requireMember(hime.Handler(s.postPRMerge))))
	mux.Handle("POST /prs/{owner}/{repo}/{n}/reviews",
		s.requireFeature("prReviews", s.requireMember(hime.Handler(s.postPRReview))))
	mux.Handle("POST /prs/{owner}/{repo}/{n}/review-requests",
		s.requireFeature("prReviews", s.requireMember(hime.Handler(s.postPRReviewRequest))))
	mux.Handle("POST /prs/{owner}/{repo}/{n}/review-requests/cancel",
		s.requireFeature("prReviews", s.requireMember(hime.Handler(s.postPRReviewCancel))))
	mux.Handle("GET /reviews", s.requireAuth(hime.Handler(s.myReviews)))
	mux.Handle("GET /projects/{project}/reviews", s.requireAuth(hime.Handler(s.projectMyReviews)))
	// Start a freeform task from the web (project workspace composer).
	mux.Handle("POST /projects/{project}/start",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postStart))))
	// Fix with Grok (PR11a)
	mux.Handle("POST /projects/{project}/issues/fix",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postIssuesBulkFix))))
	mux.Handle("POST /projects/{project}/issues/{n}/fix",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postIssueFix))))
	mux.Handle("POST /projects/{project}/linear/{identifier}/fix",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postLinearFix))))
	// Address CI / Continue / Address review (PR11b–11c)
	mux.Handle("POST /prs/{owner}/{repo}/{n}/address-ci",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postPRAddressCI))))
	mux.Handle("POST /prs/{owner}/{repo}/{n}/address-review",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postPRAddressReview))))
	mux.Handle("POST /sessions/{threadID}/continue",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postSessionContinue))))
	// Session lifecycle controls (cancel/reset/dequeue/label/goal/claim).
	mux.Handle("POST /sessions/{threadID}/cancel",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postSessionCancel))))
	mux.Handle("POST /sessions/{threadID}/reset",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postSessionReset))))
	mux.Handle("POST /sessions/{threadID}/queue/remove",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postSessionQueueRemove))))
	mux.Handle("POST /sessions/{threadID}/label",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postSessionLabel))))
	mux.Handle("POST /sessions/{threadID}/goal",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postSessionGoal))))
	mux.Handle("POST /sessions/{threadID}/claim",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postSessionClaim))))
	// Commit review → new Discord/web session; Grok opens issues agentically
	mux.Handle("POST /projects/{project}/commits/{sha}/review",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCommitReview))))
	mux.Handle("GET /events", s.requireAuth(http.HandlerFunc(s.sse)))
	mux.Handle("GET /partials/home/projects", s.requireAuth(hime.Handler(s.partialHomeProjects)))
	mux.Handle("GET /partials/home/runs", s.requireAuth(hime.Handler(s.partialHomeRuns)))
	mux.Handle("GET /partials/projects/pulse", s.requireAuth(hime.Handler(s.partialProjectPulse)))
	mux.Handle("GET /partials/ship/stats", s.requireAuth(hime.Handler(s.partialShipStats)))
	mux.Handle("GET /partials/ship/table", s.requireAuth(hime.Handler(s.partialShipTable)))
	mux.Handle("GET /partials/cases/pipeline", s.requireAuth(hime.Handler(s.partialCasesPipeline)))
	mux.Handle("GET /partials/cases/list", s.requireAuth(hime.Handler(s.partialCasesList)))
	mux.Handle("GET /partials/history/table", s.requireAuth(hime.Handler(s.partialHistoryTable)))
	mux.Handle("GET /partials/history/turns/{threadID}", s.requireAuth(hime.Handler(s.partialHistoryTurns)))
	mux.Handle("GET /partials/sessions/{threadID}", s.requireAuth(hime.Handler(s.partialSession)))
	mux.Handle("GET /partials/worktrees/table", s.requireAuth(hime.Handler(s.partialWorktreesTable)))
	mux.Handle("GET /partials/issues/table", s.requireAuth(hime.Handler(s.partialIssuesTable)))
	mux.Handle("GET /partials/config/lists", s.requireAdmin(hime.Handler(s.partialConfigLists)))

	// Admin + CSRF mutations (no-op gates when auth disabled)
	mux.Handle("POST /worktrees/prune", s.requireAdmin(hime.Handler(s.pruneWorktree)))
	mux.Handle("POST /worktrees/prune-idle", s.requireAdmin(hime.Handler(s.pruneIdleWorktrees)))
	mux.Handle("POST /config/projects", s.requireAdmin(hime.Handler(s.addProject)))
	mux.Handle("POST /config/projects/remove", s.requireAdmin(hime.Handler(s.removeProject)))
	mux.Handle("POST /config/projects/linear", s.requireAdmin(hime.Handler(s.setProjectLinear)))
	mux.Handle("POST /config/projects/github", s.requireAdmin(hime.Handler(s.setProjectGitHub)))
	mux.Handle("POST /config/projects/channel", s.requireAdmin(hime.Handler(s.setProjectChannel)))
	mux.Handle("POST /config/projects/fetch", s.requireAdmin(hime.Handler(s.setProjectFetch)))
	mux.Handle("POST /config/projects/ship", s.requireAdmin(hime.Handler(s.setProjectShip)))
	mux.Handle("POST /config/projects/safe-team", s.requireAdmin(hime.Handler(s.setProjectSafeTeam)))
	mux.Handle("POST /config/projects/verify", s.requireAdmin(hime.Handler(s.setProjectVerify)))
	mux.Handle("POST /config/projects/capabilities/users", s.requireAdmin(hime.Handler(s.setProjectCapabilityUser)))
	mux.Handle("POST /config/projects/capabilities/users/remove", s.requireAdmin(hime.Handler(s.removeProjectCapabilityUser)))
	mux.Handle("POST /config/projects/capabilities/roles", s.requireAdmin(hime.Handler(s.setProjectCapabilityRole)))
	mux.Handle("POST /config/projects/capabilities/roles/remove", s.requireAdmin(hime.Handler(s.removeProjectCapabilityRole)))
	mux.Handle("POST /config/guild", s.requireAdmin(hime.Handler(s.setGuild)))
	mux.Handle("POST /config/projects/users", s.requireAdmin(hime.Handler(s.addProjectUser)))
	mux.Handle("POST /config/projects/users/remove", s.requireAdmin(hime.Handler(s.removeProjectUser)))
	mux.Handle("POST /config/projects/roles", s.requireAdmin(hime.Handler(s.addProjectRole)))
	mux.Handle("POST /config/projects/roles/remove", s.requireAdmin(hime.Handler(s.removeProjectRole)))
	mux.Handle("POST /config/channels", s.requireAdmin(hime.Handler(s.addChannel)))
	mux.Handle("POST /config/channels/remove", s.requireAdmin(hime.Handler(s.removeChannel)))
	mux.Handle("POST /config/settings", s.requireAdmin(hime.Handler(s.updateSettings)))

	app.Handler(mux)
	s.app = app
	return s
}

// App returns the underlying hime app (for ListenAndServe / ServeHTTP).
func (s *Server) App() *hime.App { return s.app }

// Handler returns the HTTP handler for tests (hime app implements ServeHTTP).
func (s *Server) Handler() http.Handler { return s.app }

// ListenAndServe starts the web UI on the configured address.
func (s *Server) ListenAndServe() error {
	return s.app.ListenAndServe()
}

// Shutdown stops the HTTP server.
func (s *Server) Shutdown() error {
	return s.app.Shutdown()
}

type pageData struct {
	Title       string
	IsDashboard bool
	IsOverview  bool
	IsHistory   bool
	IsSessions  bool
	IsShip      bool
	IsCases     bool
	IsWorktrees bool
	IsConfig    bool
	IsLogin     bool
	IsIssues    bool
	IsLinear    bool
	IsCommits   bool
	IsReviews   bool
	IsStart     bool
	Flash       string
	Error       string
	Status      bot.StatusSnapshot
	Threads     []history.Summary
	Thread      history.Thread
	Ship        bot.ShipBoard
	Cases       bot.CaseBoard
	Worktrees   []bot.WorktreeInfo
	IdleTTLDays int
	Config      config.Snapshot
	// Per-project config page (/config/projects/{name}).
	ProjectItem      config.ProjectItem
	DiscordUserNames map[string]string // Discord user id → display name (best-effort)
	SSEPath          string
	// Project-first shell scope: NavProject switches the sidebar into
	// workspace mode. URL-derived only (see navScopeFromURL) so history
	// restores can recompute it client-side.
	NavProject       string
	NavProjects      []string // visible projects for the sidebar switcher
	NavLinearEnabled bool     // workspace nav: show the Linear item
	// Home launcher cards.
	ProjectCards []projectCard
	// Auth chrome
	AuthEnabled bool
	IsAdmin     bool // true when auth off, or session role ≥ admin
	CSRF        string
	UserName    string
	UserRole    string
	UserID      string
	UserAvatar  string // Discord CDN avatar URL; empty → letter fallback
	LoginNext   string
	// Workflow read UI (PR4–7)
	Project       string
	RepoCatalog   []config.GitHubRepoRef
	ActiveOwner   string
	ActiveRepo    string
	IssueState    string
	Issues        []ghpr.IssueInfo
	Issue         ghpr.IssueInfo
	LinearEnabled bool
	LinearTeam    string
	LinearIssues  []linear.Issue
	LinearIssue   linear.Issue
	PR            ghpr.PRDetail
	PRNumber      int
	// PR detail shippability strip (nil when the PR snapshot failed to load).
	PRGates     []prGate
	PRShipReady bool // every gate green → merge affordance opens expanded
	DiffBase    string
	ThreadID    string
	// Diff review UI (commit / session / PR diff pages + per-file fragments)
	DiffReview *diffReviewData
	FileFrag   *fileFragData
	// Commits UI
	Commits       []ghpr.CommitSummary
	Commit        ghpr.CommitDetail
	CommitRef     string
	CommitPage    int
	CommitHasPrev bool
	CommitHasNext bool
	// 1-based position of the first/last row on this page within the full
	// log (total is unknown — git log has no cheap count). Zero when empty.
	CommitRangeStart int
	CommitRangeEnd   int
	CanReviewCommit  bool
	// Write UI flags (from config snapshot + session)
	CanGitHubWrite  bool
	CanMerge        bool
	CanStartSession bool
	CanPRReview     bool
	WebMergeMethod  string
	// Team PR reviews
	TeamReviews         []teamReviewRow
	TeamPendingRequests []reviewstore.Request
	TeamRollup          string
	TeamRollupText      string
	TeamRollupBadge     string
	ReviewerOptions     []reviewerOption
	ReviewRequests      []reviewRequestRow
	ReviewStatusFilter  string
	ReviewProjectFilter string
	ReviewPendingCount  int
	// Fix-with-Grok / session view
	FixHits       []bot.IssueSessionHit
	ShowFixPicker bool
	SessionEntry  sessionstore.Entry
	DiscordURL    string
	HasWorktree   bool // session worktree still on disk (enables Worktree diff)
	RunActivity   string
	RunPhases     string
	RunElapsed    string
	RunBusy       bool
	RunQueue      int
	// In-flight turn (session detail streaming, mirrors Discord live message).
	RunPrompt   string
	RunLiveText string
	// Session lifecycle controls (cancel/reset/dequeue/claim on the detail page).
	// CanControlSession gates control affordances: it already folds in
	// CanStartSession (feature+role), so the buttons never render when the POST
	// would 404/403 on the feature gate.
	CanControlSession bool
	QueueItems        []bot.QueueItem
	// Start-task composer (/projects/{project}/start).
	StartDirectShip  bool   // ship mode badge: true → Direct to primary, false → PR mode
	StartDiscordDest bool   // a start would open a Discord thread (gateway up + mapped channel)
	StartDefaultMode string // project default mode (empty normalized to "fix")
}

func (s *Server) basePage(ctx *hime.Context) pageData {
	d := pageData{
		SSEPath:        ctx.Route("sse"),
		AuthEnabled:    s.cfg.WebAuthEnabled(),
		WebMergeMethod: s.cfg.WebMergeMethodValue(),
	}
	// Sidebar workspace scope (project-first shell).
	d.NavProject = s.navScope(ctx)
	if d.NavProject != "" {
		d.NavProjects = s.filterProjectNames(ctx)
		d.NavLinearEnabled = s.cfg.ProjectLinearEnabled(d.NavProject)
	}
	// Write affordances: feature on + (auth off never enables Feature*; auth on needs role).
	d.CanGitHubWrite = s.cfg.FeatureGitHubWrites()
	d.CanMerge = s.cfg.FeatureMerge()
	d.CanStartSession = s.cfg.FeatureStartSessions()
	d.CanPRReview = s.cfg.FeaturePRReviews()
	// Auth off = private-network trust model; treat as admin for chrome (Config nav, etc.).
	d.IsAdmin = !d.AuthEnabled
	if !d.AuthEnabled {
		return d
	}
	sess := sessionFromContext(ctx.Context())
	if sess == nil {
		sess = s.sessionFromRequest(ctx.Request)
	}
	if sess != nil {
		d.CSRF = sess.CSRF
		d.UserName = sess.DisplayName
		d.UserID = sess.DiscordUserID
		d.UserAvatar = sess.AvatarURL
		d.UserRole = string(sess.Role)
		d.IsAdmin = config.RoleAtLeast(sess.Role, config.WebRoleAdmin)
		// Gate UI by role (handlers still enforce). Member+ for writes/merge/sessions.
		if !config.RoleAtLeast(sess.Role, config.WebRoleMember) {
			d.CanGitHubWrite = false
			d.CanMerge = false
			d.CanStartSession = false
			d.CanPRReview = false
		}
	} else {
		d.CanGitHubWrite = false
		d.CanMerge = false
		d.CanStartSession = false
		d.CanPRReview = false
	}
	return d
}

// viewPage renders a full layout document. Admin UI is always live/private data.
func (s *Server) viewPage(ctx *hime.Context, name string, d pageData) error {
	return ctx.NoCache().View(name, d)
}

// viewFragment renders a named {{define}} from a page template set (hime
// "page#fragment" syntax). Used by SSE live-region endpoints that always want
// content-only HTML, not ViewPartial (which returns a full document for
// non-htmx / boosted / history-restore clients).
func (s *Server) viewFragment(ctx *hime.Context, page, fragment string, d pageData) error {
	return ctx.NoCache().View(page+"#"+fragment, d)
}

func (s *Server) historyList(ctx *hime.Context) error {
	threads, err := s.history.List()
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error("history list: " + err.Error())
	}
	// Also surface sessions that have no turns yet (legacy / mid-run).
	threads = mergeSessionRows(threads, s.sessions.List())
	threads = s.filterThreadsVisible(ctx, threads)
	d := s.basePage(ctx)
	d.Title = "History"
	d.IsHistory = true
	d.Threads = threads
	return s.viewPage(ctx, "history", d)
}

// sessionsList is the sessions hub: work units from history + sessionstore.
func (s *Server) sessionsList(ctx *hime.Context) error {
	threads, err := s.history.List()
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error("sessions list: " + err.Error())
	}
	threads = mergeSessionRows(threads, s.sessions.List())
	threads = s.filterThreadsVisible(ctx, threads)
	d := s.basePage(ctx)
	d.Title = "Sessions"
	d.IsSessions = true
	d.Threads = threads
	return s.viewPage(ctx, "sessions", d)
}

func (s *Server) historyDetail(ctx *hime.Context) error {
	threadID := ctx.PathValue("threadID")
	if _, err := s.ensureThreadAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	th, err := s.history.Get(threadID)
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	// Fill project from session store when history is empty/partial.
	if th.Project == "" {
		if e, ok := s.sessions.Get(threadID); ok {
			th.Project = e.Project
		}
	}
	title := "Thread " + threadID
	if th.Project != "" {
		title = th.Project + " · " + threadID
	}
	d := s.basePage(ctx)
	d.Title = title
	// Turn log is a session-adjacent detail surface; highlight Sessions when
	// the workspace shell is scoped via ?project= (History is not a nav tab).
	d.IsHistory = true
	if d.NavProject != "" {
		d.IsSessions = true
		d.Project = d.NavProject
	} else if th.Project != "" {
		d.Project = th.Project
	}
	d.Thread = th
	return s.viewPage(ctx, "history_detail", d)
}

func (s *Server) shipPage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.FormValue("project"))
	state := strings.TrimSpace(ctx.FormValue("state"))
	d := s.basePage(ctx)
	d.Title = "Ship board"
	d.IsShip = true
	d.Ship = s.listShipBoardVisible(ctx, project, state)
	return s.viewPage(ctx, "ship", d)
}

func (s *Server) configPage(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Config"
	d.IsConfig = true
	d.Config = s.cfg.Snapshot()
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	return s.viewPage(ctx, "config", d)
}

func (s *Server) projectConfigPage(ctx *hime.Context) error {
	name := ctx.PathValue("name")
	snap := s.cfg.Snapshot()
	var item *config.ProjectItem
	for i := range snap.Projects {
		if snap.Projects[i].Name == name {
			item = &snap.Projects[i]
			break
		}
	}
	if item == nil {
		return ctx.RedirectTo("config", map[string]string{"err": fmt.Sprintf("unknown project %q", name)})
	}
	d := s.basePage(ctx)
	d.Title = item.Name + " · Config"
	d.IsConfig = true
	d.Config = snap
	d.Project = item.Name
	d.ProjectItem = *item
	nameIDs := append([]string{}, item.AllowedUserIDs...)
	for _, m := range item.CapabilityByUser {
		nameIDs = append(nameIDs, m.ID)
	}
	d.DiscordUserNames = s.resolveDiscordUserNames(nameIDs)
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	return s.viewPage(ctx, "project_config", d)
}

// resolveDiscordUserNames maps Discord user snowflakes to display names.
// Best-effort: durable web-users profiles, active web sessions, past thread
// owners, then live Discord User lookup.
func (s *Server) resolveDiscordUserNames(ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	need := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		need[id] = struct{}{}
	}
	if len(need) == 0 {
		return out
	}
	take := func(id, name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := need[id]; !ok {
			return
		}
		out[id] = name
		delete(need, id)
	}
	// Prefer durable profiles (survive logout) over ephemeral sessions.
	if s.webUsers != nil {
		for id, name := range s.webUsers.displayNames() {
			take(id, name)
		}
	}
	if s.webSessions != nil {
		for id, name := range s.webSessions.displayNames() {
			take(id, name)
		}
	}
	if s.sessions != nil {
		for _, listed := range s.sessions.List() {
			e := listed.Entry
			take(e.OwnerID, e.OwnerName)
			if created := strings.TrimSpace(e.CreatedBy); created != "" && !strings.HasPrefix(created, "web:") {
				take(created, e.CreatedByName)
			}
			if len(need) == 0 {
				return out
			}
		}
	}
	if len(need) == 0 || s.bot == nil {
		return out
	}
	dg := s.bot.Discord()
	if dg == nil {
		return out
	}
	for id := range need {
		u, err := dg.User(id)
		if err != nil || u == nil {
			continue
		}
		take(id, u.DisplayName())
	}
	return out
}

func (s *Server) worktreesPage(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Worktrees"
	d.IsWorktrees = true
	d.Worktrees = s.filterWorktreesVisible(ctx, s.bot.ListWorktrees())
	d.IdleTTLDays = s.cfg.WorktreeIdleTTLDaysValue()
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	return s.viewPage(ctx, "worktrees", d)
}

// --- Live partial handlers (content-only, no layout) ---

func (s *Server) shipPartialData(ctx *hime.Context) pageData {
	project := strings.TrimSpace(ctx.FormValue("project"))
	state := strings.TrimSpace(ctx.FormValue("state"))
	d := s.basePage(ctx)
	d.Ship = s.listShipBoardVisible(ctx, project, state)
	// Workspace ship pages refresh with &scoped=1 so fragments keep the
	// scoped layout (no Project column). The global board also passes
	// ?project= as a data filter but must keep the column — hence the
	// explicit marker instead of inferring from the filter.
	if ctx.FormValue("scoped") == "1" {
		d.Project = project
	}
	return d
}

func (s *Server) partialShipStats(ctx *hime.Context) error {
	return s.viewFragment(ctx, "ship", "ship_stats", s.shipPartialData(ctx))
}

func (s *Server) partialShipTable(ctx *hime.Context) error {
	return s.viewFragment(ctx, "ship", "ship_table", s.shipPartialData(ctx))
}

func (s *Server) partialHistoryTable(ctx *hime.Context) error {
	threads, err := s.history.List()
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error("history list: " + err.Error())
	}
	threads = mergeSessionRows(threads, s.sessions.List())
	threads = s.filterThreadsVisible(ctx, threads)
	d := s.basePage(ctx)
	d.Threads = threads
	return s.viewFragment(ctx, "history", "history_table", d)
}

func (s *Server) partialHistoryTurns(ctx *hime.Context) error {
	threadID := ctx.PathValue("threadID")
	if _, err := s.ensureThreadAccess(ctx, threadID); err != nil {
		return forbiddenProject(ctx, err)
	}
	th, err := s.history.Get(threadID)
	if err != nil {
		return ctx.Status(http.StatusBadRequest).Error(err.Error())
	}
	if th.Project == "" {
		if e, ok := s.sessions.Get(threadID); ok {
			th.Project = e.Project
		}
	}
	d := s.basePage(ctx)
	d.Thread = th
	if d.NavProject != "" {
		d.Project = d.NavProject
	} else if th.Project != "" {
		d.Project = th.Project
	}
	return s.viewFragment(ctx, "history_detail", "history_turns", d)
}

func (s *Server) partialWorktreesTable(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Worktrees = s.filterWorktreesVisible(ctx, s.bot.ListWorktrees())
	// Workspace pages refresh with ?project= so the region stays scoped.
	if p := strings.TrimSpace(ctx.FormValue("project")); p != "" {
		if err := s.ensureProjectAccess(ctx, p); err != nil {
			return forbiddenProject(ctx, err)
		}
		d.Project = p
		d.Worktrees = filterWorktreesProject(d.Worktrees, p)
	}
	d.IdleTTLDays = s.cfg.WorktreeIdleTTLDaysValue()
	return s.viewFragment(ctx, "worktrees", "worktrees_table", d)
}

func (s *Server) partialConfigLists(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Config = s.cfg.Snapshot()
	return s.viewFragment(ctx, "config", "config_lists", d)
}

func (s *Server) worktreesRedirect(ctx *hime.Context, okMsg string, err error) error {
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	} else {
		q.Set("ok", okMsg)
	}
	// Prune forms on workspace pages carry the project → return to that scope.
	if p := strings.TrimSpace(ctx.PostFormValue("project")); p != "" {
		return ctx.Redirect("/projects/" + url.PathEscape(p) + "/worktrees?" + q.Encode())
	}
	return ctx.Redirect(ctx.Route("worktrees") + "?" + q.Encode())
}

func (s *Server) pruneWorktree(ctx *hime.Context) error {
	threadID := ctx.PostFormValue("threadId")
	err := s.bot.PruneWorktree(threadID)
	s.auditAction(ctx, audit.ActionWorktreePrune, err, map[string]any{"threadId": threadID})
	if err != nil {
		return s.worktreesRedirect(ctx, "", err)
	}
	return s.worktreesRedirect(ctx, fmt.Sprintf("Pruned worktree for thread %s", threadID), nil)
}

func (s *Server) pruneIdleWorktrees(ctx *hime.Context) error {
	n, err := s.bot.PruneIdleNow()
	s.auditAction(ctx, audit.ActionWorktreePruneIdle, err, map[string]any{"count": n})
	if err != nil {
		return s.worktreesRedirect(ctx, "", err)
	}
	return s.worktreesRedirect(ctx, fmt.Sprintf("Pruned %d idle worktree(s)", n), nil)
}

func (s *Server) configRedirect(ctx *hime.Context, okMsg string, err error) error {
	if err != nil {
		return ctx.RedirectTo("config", map[string]string{"err": err.Error()})
	}
	return ctx.RedirectTo("config", map[string]string{"ok": okMsg})
}

// projectConfigRedirect returns project-scoped saves to that project's own
// settings page; falls back to the config hub when the name is missing.
func (s *Server) projectConfigRedirect(ctx *hime.Context, name, okMsg string, err error) error {
	if strings.TrimSpace(name) == "" {
		return s.configRedirect(ctx, okMsg, err)
	}
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	} else {
		q.Set("ok", okMsg)
	}
	return ctx.Redirect("/config/projects/" + url.PathEscape(name) + "?" + q.Encode())
}

func (s *Server) addProject(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	path := ctx.PostFormValue("path")
	err := s.cfg.AddProject(name, path)
	s.auditAction(ctx, audit.ActionConfigAddProject, err, map[string]any{"name": name})
	if err != nil {
		return s.configRedirect(ctx, "", err)
	}
	// Land on the new project's settings page so repos/Discord/Linear can be
	// configured right away.
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Added project %q", name), nil)
}

func (s *Server) removeProject(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	err := s.cfg.RemoveProject(name)
	s.auditAction(ctx, audit.ActionConfigRemoveProject, err, map[string]any{"name": name})
	return s.configRedirect(ctx, fmt.Sprintf("Removed project %q", name), err)
}

func (s *Server) setProjectLinear(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	enabled := ctx.PostFormValue("enabled") == "1" || strings.EqualFold(ctx.PostFormValue("enabled"), "on")
	clearKey := ctx.PostFormValue("clearApiKey") == "1" || strings.EqualFold(ctx.PostFormValue("clearApiKey"), "on")
	teamKey := ctx.PostFormValue("teamKey")
	apiKey := ctx.PostFormValue("apiKey")
	err := s.cfg.SetProjectLinear(name, enabled, teamKey, apiKey, clearKey)
	s.auditAction(ctx, audit.ActionConfigSetLinear, err, map[string]any{"name": name, "enabled": enabled})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Updated Linear for project %q", name), err)
}

func (s *Server) setProjectGitHub(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	text := ctx.PostFormValue("repos")
	var repos []config.GitHubRepoRef
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "/", 2)
		if len(parts) != 2 {
			return s.projectConfigRedirect(ctx, name, "", fmt.Errorf("invalid repo line %q (want owner/repo)", line))
		}
		repos = append(repos, config.GitHubRepoRef{Owner: strings.TrimSpace(parts[0]), Repo: strings.TrimSpace(parts[1])})
	}
	err := s.cfg.SetProjectGitHubRepos(name, repos)
	s.auditAction(ctx, "config.set_project_github", err, map[string]any{"name": name, "count": len(repos)})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Updated GitHub repos for project %q", name), err)
}

func (s *Server) setProjectChannel(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	channelID := ctx.PostFormValue("channelId")
	guildID := ctx.PostFormValue("guildId")
	// Single save: preferred channel + project guild (multi-guild deep links).
	err := s.cfg.SetProjectDiscord(name, channelID, guildID)
	s.auditAction(ctx, "config.set_project_channel", err, map[string]any{
		"name": name, "channelId": channelID, "guildId": guildID,
	})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Updated Discord settings for project %q", name), err)
}

func (s *Server) setProjectFetch(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	raw := strings.TrimSpace(ctx.PostFormValue("repoFetchIntervalMinutes"))
	if raw == "" {
		return s.projectConfigRedirect(ctx, name, "", fmt.Errorf("repoFetchIntervalMinutes is required"))
	}
	mins, err := strconv.Atoi(raw)
	if err != nil {
		return s.projectConfigRedirect(ctx, name, "", fmt.Errorf("repoFetchIntervalMinutes must be an integer"))
	}
	err = s.cfg.SetProjectRepoFetchIntervalMinutes(name, mins)
	s.auditAction(ctx, "config.set_project_fetch", err, map[string]any{
		"name": name, "repoFetchIntervalMinutes": mins,
	})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Updated idle repo fetch interval for project %q", name), err)
}

func (s *Server) setProjectShip(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	enabled := ctx.PostFormValue("directToPrimary") == "1"
	err := s.cfg.SetProjectDirectToPrimary(name, enabled)
	s.auditAction(ctx, "config.set_project_ship", err, map[string]any{
		"name": name, "directToPrimary": enabled,
	})
	msg := fmt.Sprintf("Updated ship workflow for project %q (pull request mode)", name)
	if enabled {
		msg = fmt.Sprintf("Updated ship workflow for project %q (direct to primary)", name)
	}
	return s.projectConfigRedirect(ctx, name, msg, err)
}

func (s *Server) setProjectSafeTeam(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	enabled := ctx.PostFormValue("safeTeamMode") == "1"
	defaultTpl := ctx.PostFormValue("safeTeamDefaultTemplate")
	defaultMode := ctx.PostFormValue("defaultMode")
	err := s.cfg.SetProjectSafeTeam(name, enabled, defaultTpl, defaultMode)
	s.auditAction(ctx, "config.set_project_safe_team", err, map[string]any{
		"name": name, "safeTeamMode": enabled,
		"safeTeamDefaultTemplate": defaultTpl, "defaultMode": defaultMode,
	})
	msg := fmt.Sprintf("Updated safe team settings for project %q", name)
	if enabled {
		msg = fmt.Sprintf("Safe team mode ON for project %q — unmapped members use the default template", name)
	}
	return s.projectConfigRedirect(ctx, name, msg, err)
}

func (s *Server) setProjectVerify(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	raw := ctx.PostFormValue("verifyCommands")
	cmds, err := config.ParseVerifyCommandsText(raw)
	if err != nil {
		return s.projectConfigRedirect(ctx, name, "", err)
	}
	err = s.cfg.SetProjectVerifyCommands(name, cmds)
	s.auditAction(ctx, "config.set_project_verify", err, map[string]any{
		"name": name, "count": len(cmds),
	})
	msg := fmt.Sprintf("Cleared verify commands for project %q", name)
	if len(cmds) == 1 {
		msg = fmt.Sprintf("Saved 1 verify command for project %q", name)
	} else if len(cmds) > 1 {
		msg = fmt.Sprintf("Saved %d verify commands for project %q", len(cmds), name)
	}
	return s.projectConfigRedirect(ctx, name, msg, err)
}

func (s *Server) setProjectCapabilityUser(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	id := ctx.PostFormValue("id")
	tpl := ctx.PostFormValue("template")
	err := s.cfg.SetProjectCapabilityByUser(name, id, tpl)
	s.auditAction(ctx, "config.set_project_capability_user", err, map[string]any{
		"name": name, "id": id, "template": tpl,
	})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Mapped user %s → %s", id, tpl), err)
}

func (s *Server) removeProjectCapabilityUser(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	id := ctx.PostFormValue("id")
	err := s.cfg.RemoveProjectCapabilityByUser(name, id)
	s.auditAction(ctx, "config.remove_project_capability_user", err, map[string]any{
		"name": name, "id": id,
	})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Removed capability map for user %s", id), err)
}

func (s *Server) setProjectCapabilityRole(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	id := ctx.PostFormValue("id")
	tpl := ctx.PostFormValue("template")
	err := s.cfg.SetProjectCapabilityByRole(name, id, tpl)
	s.auditAction(ctx, "config.set_project_capability_role", err, map[string]any{
		"name": name, "id": id, "template": tpl,
	})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Mapped role %s → %s", id, tpl), err)
}

func (s *Server) removeProjectCapabilityRole(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	id := ctx.PostFormValue("id")
	err := s.cfg.RemoveProjectCapabilityByRole(name, id)
	s.auditAction(ctx, "config.remove_project_capability_role", err, map[string]any{
		"name": name, "id": id,
	})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Removed capability map for role %s", id), err)
}

func (s *Server) setGuild(ctx *hime.Context) error {
	id := ctx.PostFormValue("discordGuildId")
	err := s.cfg.SetDiscordGuildID(id)
	s.auditAction(ctx, "config.set_guild", err, map[string]any{"guildId": id})
	return s.configRedirect(ctx, "Updated default Discord guild id (fallback)", err)
}

func (s *Server) addProjectUser(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	id := ctx.PostFormValue("id")
	err := s.cfg.AddProjectAllowedUser(name, id)
	s.auditAction(ctx, audit.ActionConfigAddUser, err, map[string]any{"project": name, "id": id})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Added user %s", id), err)
}

func (s *Server) removeProjectUser(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	id := ctx.PostFormValue("id")
	err := s.cfg.RemoveProjectAllowedUser(name, id)
	s.auditAction(ctx, audit.ActionConfigRemoveUser, err, map[string]any{"project": name, "id": id})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Removed user %s", id), err)
}

func (s *Server) addProjectRole(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	id := ctx.PostFormValue("id")
	err := s.cfg.AddProjectAllowedRole(name, id)
	s.auditAction(ctx, audit.ActionConfigAddRole, err, map[string]any{"project": name, "id": id})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Added role %s", id), err)
}

func (s *Server) removeProjectRole(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	id := ctx.PostFormValue("id")
	err := s.cfg.RemoveProjectAllowedRole(name, id)
	s.auditAction(ctx, audit.ActionConfigRemoveRole, err, map[string]any{"project": name, "id": id})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Removed role %s", id), err)
}

func (s *Server) addChannel(ctx *hime.Context) error {
	channelID := ctx.PostFormValue("channelId")
	project := ctx.PostFormValue("project")
	err := s.cfg.AddChannel(channelID, project)
	s.auditAction(ctx, audit.ActionConfigAddChannel, err, map[string]any{"channelId": channelID, "project": project})
	msg := fmt.Sprintf("Mapped channel %s → %s", channelID, project)
	// Channel forms live on both the config hub and project settings pages.
	if ctx.PostFormValue("return_to") == "project" {
		return s.projectConfigRedirect(ctx, project, msg, err)
	}
	return s.configRedirect(ctx, msg, err)
}

func (s *Server) removeChannel(ctx *hime.Context) error {
	channelID := ctx.PostFormValue("channelId")
	err := s.cfg.RemoveChannel(channelID)
	s.auditAction(ctx, audit.ActionConfigRemoveChannel, err, map[string]any{"channelId": channelID})
	msg := fmt.Sprintf("Removed channel %s", channelID)
	if ctx.PostFormValue("return_to") == "project" {
		return s.projectConfigRedirect(ctx, ctx.PostFormValue("project"), msg, err)
	}
	return s.configRedirect(ctx, msg, err)
}

func (s *Server) updateSettings(ctx *hime.Context) error {
	section := strings.TrimSpace(ctx.PostFormValue("section"))
	if section == "" {
		// Backward-compatible posts without section.
		switch {
		case strings.TrimSpace(ctx.PostFormValue("autoFixCIMax")) != "":
			section = "ci"
		case ctx.PostFormValue("riskyPathGlobs") != "" ||
			ctx.PostFormValue("riskyPathUseDefault") != "":
			section = "risky"
		default:
			section = "worktree"
		}
	}

	var err error
	switch section {
	case "ci":
		err = s.updateCISettingsErr(ctx)
	case "risky":
		err = s.updateRiskyPathSettingsErr(ctx)
	case "worktree":
		err = s.updateWorktreeSettingsErr(ctx)
	case "run":
		err = s.updateRunSettingsErr(ctx)
	case "board":
		err = s.updateBoardSettingsErr(ctx)
	case "resume":
		err = s.updateResumeSettingsErr(ctx)
	case "discordPRLink":
		err = s.updateDiscordPRLinkSettingsErr(ctx)
	default:
		err = fmt.Errorf("unknown settings section %q", section)
	}
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": section})
	if err != nil {
		return s.configRedirect(ctx, "", err)
	}
	// Success messages match previous helpers.
	switch section {
	case "ci":
		enabled := ctx.PostFormValue("autoFixCI") == "1" || strings.EqualFold(ctx.PostFormValue("autoFixCI"), "on")
		rawMax := strings.TrimSpace(ctx.PostFormValue("autoFixCIMax"))
		maxAttempts, _ := strconv.Atoi(rawMax)
		msg := "Auto CI fix disabled"
		if enabled {
			msg = fmt.Sprintf("Auto CI fix enabled (max %d attempt(s) per thread)", maxAttempts)
		}
		return s.configRedirect(ctx, msg, nil)
	case "risky":
		useDefault := ctx.PostFormValue("riskyPathUseDefault") == "1" ||
			strings.EqualFold(ctx.PostFormValue("riskyPathUseDefault"), "on")
		text := ctx.PostFormValue("riskyPathGlobs")
		msg := "Risky path globs set to built-in defaults"
		if !useDefault {
			n := 0
			for _, line := range strings.Split(text, "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					n++
				}
			}
			if n == 0 {
				msg = "Risky path flags disabled (empty custom list)"
			} else {
				msg = fmt.Sprintf("Saved %d risky path glob(s)", n)
			}
		}
		return s.configRedirect(ctx, msg, nil)
	case "worktree":
		raw := strings.TrimSpace(ctx.PostFormValue("worktreeIdleTTLDays"))
		days, _ := strconv.Atoi(raw)
		msg := fmt.Sprintf("Worktree idle TTL set to %d day(s)", days)
		if days == 0 {
			msg = "Worktree idle cleanup disabled"
		}
		return s.configRedirect(ctx, msg, nil)
	case "run":
		maxTurns, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("maxTurns")))
		timeoutMs, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("timeoutMs")))
		mins := float64(timeoutMs) / 60000
		msg := fmt.Sprintf("Grok run limits: maxTurns=%d, timeoutMs=%d (%.1f min)", maxTurns, timeoutMs, mins)
		return s.configRedirect(ctx, msg, nil)
	case "board":
		days, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("boardStaleDays")))
		channel := strings.TrimSpace(ctx.PostFormValue("boardDigestChannel"))
		msg := fmt.Sprintf("Board stale threshold set to %d day(s)", days)
		if channel == "" {
			msg += "; nightly digest disabled"
		} else {
			msg += fmt.Sprintf("; digest channel %s", channel)
		}
		return s.configRedirect(ctx, msg, nil)
	case "resume":
		enabled := ctx.PostFormValue("resumeActiveRuns") == "1" || strings.EqualFold(ctx.PostFormValue("resumeActiveRuns"), "on")
		msg := "Crash-safe resume disabled"
		if enabled {
			msg = "Crash-safe resume enabled"
		}
		return s.configRedirect(ctx, msg, nil)
	case "discordPRLink":
		mode := strings.TrimSpace(ctx.PostFormValue("discordPRLink"))
		msg := "Discord PR links: GitHub"
		if mode == config.DiscordPRLinkWeb {
			msg = "Discord PR links: web UI"
			if s.cfg.WebPublicBaseURLValue() == "" {
				msg += " (set webPublicBaseURL so links resolve)"
			}
		}
		return s.configRedirect(ctx, msg, nil)
	default:
		return s.configRedirect(ctx, "Settings saved", nil)
	}
}

func (s *Server) updateDiscordPRLinkSettingsErr(ctx *hime.Context) error {
	return s.cfg.SetDiscordPRLink(ctx.PostFormValue("discordPRLink"))
}

func (s *Server) updateResumeSettingsErr(ctx *hime.Context) error {
	enabled := ctx.PostFormValue("resumeActiveRuns") == "1" || strings.EqualFold(ctx.PostFormValue("resumeActiveRuns"), "on")
	return s.cfg.SetResumeActiveRuns(enabled)
}

func (s *Server) updateRunSettingsErr(ctx *hime.Context) error {
	rawTurns := strings.TrimSpace(ctx.PostFormValue("maxTurns"))
	rawTimeout := strings.TrimSpace(ctx.PostFormValue("timeoutMs"))
	if rawTurns == "" {
		return fmt.Errorf("maxTurns is required")
	}
	if rawTimeout == "" {
		return fmt.Errorf("timeoutMs is required")
	}
	maxTurns, err := strconv.Atoi(rawTurns)
	if err != nil {
		return fmt.Errorf("maxTurns must be an integer")
	}
	timeoutMs, err := strconv.Atoi(rawTimeout)
	if err != nil {
		return fmt.Errorf("timeoutMs must be an integer")
	}
	return s.cfg.SetGrokRunLimits(maxTurns, timeoutMs)
}

func (s *Server) updateCISettingsErr(ctx *hime.Context) error {
	enabled := ctx.PostFormValue("autoFixCI") == "1" || strings.EqualFold(ctx.PostFormValue("autoFixCI"), "on")
	rawMax := strings.TrimSpace(ctx.PostFormValue("autoFixCIMax"))
	if rawMax == "" {
		return fmt.Errorf("autoFixCIMax is required")
	}
	maxAttempts, err := strconv.Atoi(rawMax)
	if err != nil {
		return fmt.Errorf("autoFixCIMax must be an integer")
	}
	return s.cfg.SetAutoFixCI(enabled, maxAttempts)
}

func (s *Server) updateRiskyPathSettingsErr(ctx *hime.Context) error {
	useDefault := ctx.PostFormValue("riskyPathUseDefault") == "1" ||
		strings.EqualFold(ctx.PostFormValue("riskyPathUseDefault"), "on")
	text := ctx.PostFormValue("riskyPathGlobs")
	return s.cfg.SetRiskyPathGlobsFromText(text, useDefault)
}

func (s *Server) updateWorktreeSettingsErr(ctx *hime.Context) error {
	raw := strings.TrimSpace(ctx.PostFormValue("worktreeIdleTTLDays"))
	if raw == "" {
		return fmt.Errorf("worktreeIdleTTLDays is required")
	}
	days, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("worktreeIdleTTLDays must be an integer")
	}
	return s.cfg.SetWorktreeIdleTTLDays(days)
}

func (s *Server) updateBoardSettingsErr(ctx *hime.Context) error {
	rawDays := strings.TrimSpace(ctx.PostFormValue("boardStaleDays"))
	if rawDays == "" {
		return fmt.Errorf("boardStaleDays is required")
	}
	days, err := strconv.Atoi(rawDays)
	if err != nil {
		return fmt.Errorf("boardStaleDays must be an integer")
	}
	channel := strings.TrimSpace(ctx.PostFormValue("boardDigestChannel"))
	return s.cfg.SetBoardSettings(days, channel)
}

// auditAction records a web mutation (auth-off → actor anonymous).
func (s *Server) auditAction(ctx *hime.Context, action string, err error, detail map[string]any) {
	if s == nil || s.audit == nil {
		return
	}
	actor, role := s.auditActor(ctx)
	ev := audit.Event{
		Action: action,
		Actor:  actor,
		Role:   role,
		Detail: detail,
		OK:     err == nil,
	}
	if err != nil {
		ev.Error = err.Error()
	}
	_ = s.audit.Append(ev)
}

func (s *Server) auditActor(ctx *hime.Context) (actor, role string) {
	if ctx == nil {
		return audit.ActorAnonymous, ""
	}
	sess := sessionFromContext(ctx.Context())
	if sess == nil {
		sess = s.sessionFromRequest(ctx.Request)
	}
	if sess == nil {
		return audit.ActorAnonymous, ""
	}
	actor = sess.DiscordUserID
	if actor == "" {
		actor = sess.DisplayName
	}
	if actor == "" {
		actor = audit.ActorAnonymous
	}
	return actor, string(sess.Role)
}

// mergeSessionRows adds session-store threads that have no history turns yet.
func mergeSessionRows(hist []history.Summary, sessions []sessionstore.Listed) []history.Summary {
	seen := make(map[string]struct{}, len(hist))
	for _, h := range hist {
		seen[h.ThreadID] = struct{}{}
	}
	for _, se := range sessions {
		if _, ok := seen[se.ThreadID]; ok {
			continue
		}
		hist = append(hist, history.Summary{
			ThreadID:  se.ThreadID,
			Project:   se.Project,
			LastUser:  se.LastUser,
			UpdatedAt: se.UpdatedAt,
			TurnCount: 0,
		})
	}
	return hist
}

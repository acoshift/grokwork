package web

import (
	"context"
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
	"github.com/acoshift/grokwork/internal/grokrun"
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
	// One-shot Grok drafts for project verify commands (filled into the
	// workflow textarea after "Suggest with Grok"; not persisted until Save).
	verifyDraftMu sync.Mutex
	verifyDrafts  map[string]string
	// Test injectable; nil → grokrun.SuggestVerifyCommands (SSE stream hooks optional).
	suggestVerify func(ctx context.Context, grokBin, model, cwd string, timeout time.Duration, hooks *grokrun.SuggestStreamHooks) (string, error)
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
		"cases":                              "/cases",
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
		"config.generateProjectVerify":       "/config/projects/verify/generate",
		"config.setProjectMode":              "/config/projects/mode",
		"config.addProjectMember":            "/config/projects/members",
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
		"config.setGitHubIdentity":           "/config/github-identities",
		"config.removeGitHubIdentity":        "/config/github-identities/remove",
		"config.bot":                         "/config/bot",
		"config.channels":                    "/config/channels",
		"config.identities":                  "/config/github-identities",
		"config.projectNew":                  "/config/projects/new",
		"config.run":                         "/config/run",
		"config.worktrees":                   "/config/worktrees",
		"config.board":                       "/config/board",
		"config.ci":                          "/config/ci",
		"config.prlinks":                     "/config/pr-links",
		"config.risky":                       "/config/risky",
		"config.resume":                      "/config/resume",
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
		"partial.pr.gates":        "/partials/prs/",
		"partial.config.lists":    "/partials/config/lists",
		"partial.config.channels": "/partials/config/channels",
	})

	app.TemplateFunc("add", func(a, b int) int { return a + b })
	app.TemplateFunc("sub", func(a, b int) int { return a - b })
	// Millisecond durations as compact human text (config hub row values).
	app.TemplateFunc("msDur", func(ms int) string {
		d := time.Duration(ms) * time.Millisecond
		if d >= time.Hour && d%time.Hour == 0 {
			return fmt.Sprintf("%dh", int(d/time.Hour))
		}
		if d >= time.Minute {
			return fmt.Sprintf("%dm", int(d/time.Minute))
		}
		return fmt.Sprintf("%ds", int(d/time.Second))
	})
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
	tp.ParseFiles("case_new", "layout.tmpl", "case_new.tmpl")
	tp.ParseFiles("worktrees", "layout.tmpl", "worktrees.tmpl")
	tp.ParseFiles("config", "layout.tmpl", "config.tmpl")
	tp.ParseFiles("config_bot", "layout.tmpl", "config_bot.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_channels", "layout.tmpl", "config_channels.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_identities", "layout.tmpl", "config_identities.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_project_new", "layout.tmpl", "config_project_new.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_run", "layout.tmpl", "config_run.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_worktrees", "layout.tmpl", "config_worktrees.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_board", "layout.tmpl", "config_board.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_ci", "layout.tmpl", "config_ci.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_prlinks", "layout.tmpl", "config_prlinks.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_risky", "layout.tmpl", "config_risky.tmpl", "config_shared.tmpl")
	tp.ParseFiles("project_config", "layout.tmpl", "project_config.tmpl", "project_config_shared.tmpl")
	tp.ParseFiles("project_config_workflow", "layout.tmpl", "project_config_workflow.tmpl", "project_config_shared.tmpl")
	tp.ParseFiles("project_config_integrations", "layout.tmpl", "project_config_integrations.tmpl", "project_config_shared.tmpl")
	tp.ParseFiles("project_config_danger", "layout.tmpl", "project_config_danger.tmpl", "project_config_shared.tmpl")
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
	mux.Handle("GET /cases", s.requireAuth(hime.Handler(s.casesGlobal)))
	mux.Handle("GET /worktrees", s.requireAuth(hime.Handler(s.worktreesPage)))
	mux.Handle("GET /config", s.requireAdmin(hime.Handler(s.configPage)))
	mux.Handle("GET /config/projects/{name}", s.requireAdmin(hime.Handler(s.projectConfigPage)))
	mux.Handle("GET /config/projects/{name}/workflow", s.requireAdmin(hime.Handler(s.projectConfigWorkflowPage)))
	mux.Handle("GET /config/projects/{name}/integrations", s.requireAdmin(hime.Handler(s.projectConfigIntegrationsPage)))
	mux.Handle("GET /config/projects/{name}/danger", s.requireAdmin(hime.Handler(s.projectConfigDangerPage)))
	// Project workspace (project-first UX): overview + scoped list pages.
	mux.Handle("GET /projects/{project}", s.requireAuth(hime.Handler(s.projectOverview)))
	mux.Handle("GET /projects/{project}/start", s.requireAuth(hime.Handler(s.startComposer)))
	mux.Handle("GET /projects/{project}/ship", s.requireAuth(hime.Handler(s.shipScoped)))
	mux.Handle("GET /projects/{project}/cases", s.requireAuth(hime.Handler(s.casesScoped)))
	mux.Handle("GET /projects/{project}/cases/new", s.requireAuth(hime.Handler(s.caseNewPage)))
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
	// Web case intake (Discord "/case" parity): case shell only; investigate run
	// queues only when intake notes are given.
	mux.Handle("POST /projects/{project}/cases/new",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCaseNew))))
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
	// Case phase actions (Mode=case) — feature gate startSessions so members can act;
	// per-action caps checked in handlers (investigate vs escalate vs draft).
	mux.Handle("POST /sessions/{threadID}/case/escalate",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCaseEscalate))))
	mux.Handle("POST /sessions/{threadID}/case/answer",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCaseAnswer))))
	mux.Handle("POST /sessions/{threadID}/case/close",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCaseClose))))
	mux.Handle("POST /sessions/{threadID}/case/reopen",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCaseReopen))))
	mux.Handle("POST /sessions/{threadID}/case/customer-update",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCaseCustomerUpdate))))
	mux.Handle("POST /sessions/{threadID}/case/investigate",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCaseInvestigate))))
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
	mux.Handle("GET /partials/prs/{owner}/{repo}/{n}/gates", s.requireAuth(hime.Handler(s.partialPRGates)))
	mux.Handle("GET /partials/config/lists", s.requireAdmin(hime.Handler(s.partialConfigLists)))
	mux.Handle("GET /partials/config/channels", s.requireAdmin(hime.Handler(s.partialConfigChannels)))

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
	mux.Handle("POST /config/projects/mode", s.requireAdmin(hime.Handler(s.setProjectMode)))
	mux.Handle("POST /config/projects/members", s.requireAdmin(hime.Handler(s.addProjectMember)))
	mux.Handle("POST /config/projects/verify", s.requireAdmin(hime.Handler(s.setProjectVerify)))
	mux.Handle("POST /config/projects/verify/generate", s.requireAdmin(hime.Handler(s.generateProjectVerify)))
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
	mux.Handle("POST /config/github-identities", s.requireAdmin(hime.Handler(s.setGitHubIdentity)))
	mux.Handle("POST /config/github-identities/remove", s.requireAdmin(hime.Handler(s.removeGitHubIdentity)))
	// Config drill-in pages: the hub keeps grouped rows only; every section
	// lives on a focused sub-page with its own POST handler (no shared
	// "section" dispatcher — each write has a distinct route + audit entry).
	mux.Handle("GET /config/bot", s.requireAdmin(hime.Handler(s.configSubPage("config_bot", "Discord bot", false))))
	mux.Handle("GET /config/channels", s.requireAdmin(hime.Handler(s.configSubPage("config_channels", "Channel map", false))))
	mux.Handle("GET /config/github-identities", s.requireAdmin(hime.Handler(s.configSubPage("config_identities", "GitHub attribution", true))))
	mux.Handle("GET /config/projects/new", s.requireAdmin(hime.Handler(s.configSubPage("config_project_new", "Add project", false))))
	mux.Handle("GET /config/run", s.requireAdmin(hime.Handler(s.configSubPage("config_run", "Run limits", false))))
	mux.Handle("GET /config/worktrees", s.requireAdmin(hime.Handler(s.configSubPage("config_worktrees", "Worktrees", false))))
	mux.Handle("GET /config/board", s.requireAdmin(hime.Handler(s.configSubPage("config_board", "Team activity board", false))))
	mux.Handle("GET /config/ci", s.requireAdmin(hime.Handler(s.configSubPage("config_ci", "CI triage", false))))
	mux.Handle("GET /config/pr-links", s.requireAdmin(hime.Handler(s.configSubPage("config_prlinks", "Discord PR links", false))))
	mux.Handle("GET /config/risky", s.requireAdmin(hime.Handler(s.configSubPage("config_risky", "Completion risk paths", false))))
	mux.Handle("POST /config/run", s.requireAdmin(hime.Handler(s.updateRunSettings)))
	mux.Handle("POST /config/worktrees", s.requireAdmin(hime.Handler(s.updateWorktreeSettings)))
	mux.Handle("POST /config/board", s.requireAdmin(hime.Handler(s.updateBoardSettings)))
	mux.Handle("POST /config/ci", s.requireAdmin(hime.Handler(s.updateCISettings)))
	mux.Handle("POST /config/pr-links", s.requireAdmin(hime.Handler(s.updateDiscordPRLinkSettings)))
	mux.Handle("POST /config/risky", s.requireAdmin(hime.Handler(s.updateRiskyPathSettings)))
	mux.Handle("POST /config/resume", s.requireAdmin(hime.Handler(s.updateResumeSettings)))

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
	// Per-project settings tabs (/config/projects/{name}[/tab]).
	ProjectItem      config.ProjectItem
	DiscordUserNames map[string]string // Discord user id → display name (best-effort)
	ProjectTab       string            // access | workflow | integrations | danger
	Members          []memberRow       // Access: unified allowlist + role roster
	CapMatrix        []capMatrixRow    // Access: role → capability legend
	CapNames         []string          // Access: legend column labels
	// Effective role for roster members without an explicit one (safe team
	// off → builder, on → the project's default template).
	DefaultRoleFallback string
	SSEPath             string
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
	// CanStartFixMode: project CanShip (startSessions + githubWrites). Hides Fix &
	// ship in the start dropdown; POSTs hard-deny without these caps.
	CanStartFixMode bool
	// Case intake (/projects/{project}/cases/new + board CTAs): Discord /case
	// parity — startSessions feature+role AND investigator-class capability.
	CanOpenCase bool
	// Session case panel affordances (Mode=case only).
	// Investigate/escalate/answer hide on fixing|shipping (eng phases).
	CanCaseEscalate    bool
	CanCaseDraft       bool // customer-update (open cases)
	CanCaseAnswer      bool // knowledge-path answer; not shown on eng phases
	CanCaseClose       bool // owner/co/admin
	CanCaseInvestigate bool
	CanCaseReopen      bool // closed cases only; investigator-class or session control
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
	if len(d.Config.GitHubIdentities) > 0 {
		ids := make([]string, 0, len(d.Config.GitHubIdentities))
		for _, row := range d.Config.GitHubIdentities {
			ids = append(ids, row.DiscordUserID)
		}
		d.DiscordUserNames = s.resolveDiscordUserNames(ids)
	}
	return s.viewPage(ctx, "config", d)
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

func (s *Server) partialConfigChannels(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Config = s.cfg.Snapshot()
	return s.viewFragment(ctx, "config_channels", "config_channels_list", d)
}

// configSubPage renders one focused config drill-in page. All sub-pages share
// the hub's data shape (a fresh config snapshot + flash/err from the query);
// withIdentities additionally resolves Discord display names, which may call
// the Discord API — only the attribution page pays that cost.
func (s *Server) configSubPage(tmpl, title string, withIdentities bool) func(*hime.Context) error {
	return func(ctx *hime.Context) error {
		d := s.basePage(ctx)
		d.Title = title + " · Config"
		d.IsConfig = true
		d.Config = s.cfg.Snapshot()
		d.Flash = ctx.FormValue("ok")
		d.Error = ctx.FormValue("err")
		if withIdentities && len(d.Config.GitHubIdentities) > 0 {
			ids := make([]string, 0, len(d.Config.GitHubIdentities))
			for _, row := range d.Config.GitHubIdentities {
				ids = append(ids, row.DiscordUserID)
			}
			d.DiscordUserNames = s.resolveDiscordUserNames(ids)
		}
		return s.viewPage(ctx, tmpl, d)
	}
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

// projectConfigTabRedirect returns a project-scoped save to one of that
// project's settings tabs ("" = Access, the default tab); falls back to the
// config hub when the name is missing.
func (s *Server) projectConfigTabRedirect(ctx *hime.Context, name, tab, okMsg string, err error) error {
	if strings.TrimSpace(name) == "" {
		return s.configRedirect(ctx, okMsg, err)
	}
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	} else {
		q.Set("ok", okMsg)
	}
	p := "/config/projects/" + url.PathEscape(name)
	if tab != "" {
		p += "/" + tab
	}
	return ctx.Redirect(p + "?" + q.Encode())
}

// projectConfigRedirect returns a project-scoped save to the Access tab.
func (s *Server) projectConfigRedirect(ctx *hime.Context, name, okMsg string, err error) error {
	return s.projectConfigTabRedirect(ctx, name, "", okMsg, err)
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
	return s.projectConfigTabRedirect(ctx, name, "integrations", fmt.Sprintf("Updated Linear for project %q", name), err)
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
			return s.projectConfigTabRedirect(ctx, name, "integrations", "", fmt.Errorf("invalid repo line %q (want owner/repo)", line))
		}
		repos = append(repos, config.GitHubRepoRef{Owner: strings.TrimSpace(parts[0]), Repo: strings.TrimSpace(parts[1])})
	}
	err := s.cfg.SetProjectGitHubRepos(name, repos)
	s.auditAction(ctx, "config.set_project_github", err, map[string]any{"name": name, "count": len(repos)})
	return s.projectConfigTabRedirect(ctx, name, "integrations", fmt.Sprintf("Updated GitHub repos for project %q", name), err)
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
	return s.projectConfigTabRedirect(ctx, name, "integrations", fmt.Sprintf("Updated Discord settings for project %q", name), err)
}

func (s *Server) setProjectFetch(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	raw := strings.TrimSpace(ctx.PostFormValue("repoFetchIntervalMinutes"))
	if raw == "" {
		return s.projectConfigTabRedirect(ctx, name, "integrations", "", fmt.Errorf("repoFetchIntervalMinutes is required"))
	}
	mins, err := strconv.Atoi(raw)
	if err != nil {
		return s.projectConfigTabRedirect(ctx, name, "integrations", "", fmt.Errorf("repoFetchIntervalMinutes must be an integer"))
	}
	err = s.cfg.SetProjectRepoFetchIntervalMinutes(name, mins)
	s.auditAction(ctx, "config.set_project_fetch", err, map[string]any{
		"name": name, "repoFetchIntervalMinutes": mins,
	})
	return s.projectConfigTabRedirect(ctx, name, "integrations", fmt.Sprintf("Updated idle repo fetch interval for project %q", name), err)
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
	return s.projectConfigTabRedirect(ctx, name, "workflow", msg, err)
}

// setProjectSafeTeam saves the Access tab's team policy: role-based (safe
// team) on/off + the default role for members without an explicit one.
func (s *Server) setProjectSafeTeam(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	enabled := ctx.PostFormValue("safeTeamMode") == "1"
	defaultTpl := ctx.PostFormValue("safeTeamDefaultTemplate")
	err := s.cfg.SetProjectSafeTeamPolicy(name, enabled, defaultTpl)
	s.auditAction(ctx, "config.set_project_safe_team", err, map[string]any{
		"name": name, "safeTeamMode": enabled,
		"safeTeamDefaultTemplate": defaultTpl,
	})
	msg := fmt.Sprintf("Team policy for project %q: trusted — members without a role act as builder", name)
	if enabled {
		msg = fmt.Sprintf("Team policy for project %q: role-based — members without a role use the default role", name)
	}
	return s.projectConfigRedirect(ctx, name, msg, err)
}

// setProjectMode saves the Workflow tab's default mode for new sessions.
func (s *Server) setProjectMode(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	mode := ctx.PostFormValue("defaultMode")
	err := s.cfg.SetProjectDefaultMode(name, mode)
	s.auditAction(ctx, "config.set_project_mode", err, map[string]any{
		"name": name, "defaultMode": mode,
	})
	return s.projectConfigTabRedirect(ctx, name, "workflow", fmt.Sprintf("Updated default mode for project %q", name), err)
}

func (s *Server) setProjectVerify(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	raw := ctx.PostFormValue("verifyCommands")
	cmds, err := config.ParseVerifyCommandsText(raw)
	if err != nil {
		return s.projectConfigTabRedirect(ctx, name, "workflow", "", err)
	}
	err = s.cfg.SetProjectVerifyCommands(name, cmds)
	s.auditAction(ctx, "config.set_project_verify", err, map[string]any{
		"name": name, "count": len(cmds),
	})
	// Discard any pending Grok draft once the admin saves.
	s.clearVerifyDraft(name)
	msg := fmt.Sprintf("Cleared verify commands for project %q", name)
	if len(cmds) == 1 {
		msg = fmt.Sprintf("Saved 1 verify command for project %q", name)
	} else if len(cmds) > 1 {
		msg = fmt.Sprintf("Saved %d verify commands for project %q", len(cmds), name)
	}
	return s.projectConfigTabRedirect(ctx, name, "workflow", msg, err)
}

// generateProjectVerify streams a short Grok inspect of the project checkout
// as SSE (status / activity / text / result / done). On success stores a draft
// for the verify textarea (not saved until Save). Client: GrokStream modal.
func (s *Server) generateProjectVerify(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	if name == "" {
		// Query fallback so GET debug still works; prefer form body.
		name = strings.TrimSpace(ctx.FormValue("name"))
	}
	w := ctx.ResponseWriter()
	stream, err := newSSEStream(w)
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error(err.Error())
	}

	fail := func(msg string) error {
		_ = stream.Error(msg)
		_ = stream.Done()
		return nil
	}
	if name == "" {
		return fail("project name is required")
	}
	path, ok := s.cfg.ProjectPath(name)
	if !ok {
		return fail(fmt.Sprintf("project %q not found", name))
	}

	_ = stream.Status("Inspecting repository…")

	snap := s.cfg.Snapshot()
	suggest := s.suggestVerify
	if suggest == nil {
		suggest = grokrun.SuggestVerifyCommands
	}
	hooks := &grokrun.SuggestStreamHooks{
		OnTextDelta: func(delta string) { _ = stream.TextDelta(delta) },
		OnThought:   func(delta string) { _ = stream.ThoughtDelta(delta) },
		OnActivity:  func(line string) { _ = stream.Activity(line) },
	}
	raw, err := suggest(ctx.Context(), snap.GrokBin, snap.Model, path, 3*time.Minute, hooks)
	s.auditAction(ctx, "config.generate_project_verify", err, map[string]any{
		"name": name,
	})
	if err != nil {
		return fail(err.Error())
	}
	// Production suggest already cleans; still extract so injectable/mocks and
	// partial model prose parse reliably.
	if cleaned := grokrun.ExtractVerifyCommandsText(raw); cleaned != "" {
		raw = cleaned
	}
	cmds, err := config.ParseVerifyCommandsText(raw)
	if err != nil {
		return fail(fmt.Sprintf("could not parse Grok output: %v", err))
	}
	if len(cmds) == 0 {
		return fail("Grok returned no verify commands")
	}
	text := config.FormatVerifyCommandsText(cmds)
	s.putVerifyDraft(name, text)
	msg := "Suggested 1 verify command — review and Save to apply"
	if len(cmds) != 1 {
		msg = fmt.Sprintf("Suggested %d verify commands — review and Save to apply", len(cmds))
	}
	_ = stream.Result(map[string]any{
		"ok":      true,
		"text":    text,
		"count":   len(cmds),
		"message": msg,
		"project": name,
	})
	_ = stream.Done()
	return nil
}

func (s *Server) putVerifyDraft(name, text string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	s.verifyDraftMu.Lock()
	defer s.verifyDraftMu.Unlock()
	if s.verifyDrafts == nil {
		s.verifyDrafts = make(map[string]string)
	}
	s.verifyDrafts[name] = text
}

// peekVerifyDraft returns a pending draft without clearing it (survives refresh).
func (s *Server) peekVerifyDraft(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	s.verifyDraftMu.Lock()
	defer s.verifyDraftMu.Unlock()
	if s.verifyDrafts == nil {
		return ""
	}
	return s.verifyDrafts[name]
}

func (s *Server) clearVerifyDraft(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	s.verifyDraftMu.Lock()
	defer s.verifyDraftMu.Unlock()
	delete(s.verifyDrafts, name)
}

// setProjectCapabilityUser saves a roster role select: an explicit template,
// or empty to reset the user to the policy's default fallback.
func (s *Server) setProjectCapabilityUser(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	id := ctx.PostFormValue("id")
	tpl := ctx.PostFormValue("template")
	err := s.cfg.SetProjectCapabilityByUser(name, id, tpl)
	s.auditAction(ctx, "config.set_project_capability_user", err, map[string]any{
		"name": name, "id": id, "template": tpl,
	})
	msg := fmt.Sprintf("Reset user %s to the default role", id)
	if strings.TrimSpace(tpl) != "" {
		msg = fmt.Sprintf("Set role %s for user %s", tpl, id)
	}
	return s.projectConfigRedirect(ctx, name, msg, err)
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
	msg := fmt.Sprintf("Reset Discord role %s to the default role", id)
	if strings.TrimSpace(tpl) != "" {
		msg = fmt.Sprintf("Set role %s for Discord role %s", tpl, id)
	}
	return s.projectConfigRedirect(ctx, name, msg, err)
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
	return s.configPageRedirect(ctx, "config.bot", "Updated default Discord guild id (fallback)", err)
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
	if err == nil {
		// Drop the explicit role too so the roster loses the whole member
		// (no inert capability map left behind).
		err = s.cfg.RemoveProjectCapabilityByUser(name, id)
	}
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
	if err == nil {
		err = s.cfg.RemoveProjectCapabilityByRole(name, id)
	}
	s.auditAction(ctx, audit.ActionConfigRemoveRole, err, map[string]any{"project": name, "id": id})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Removed role %s", id), err)
}

// addProjectMember adds one roster principal in a single post: allowlist
// entry plus an optional explicit role (capability template).
func (s *Server) addProjectMember(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	kind := ctx.PostFormValue("kind")
	id := ctx.PostFormValue("id")
	tpl := strings.TrimSpace(ctx.PostFormValue("template"))
	var err error
	switch kind {
	case "user":
		err = s.cfg.AddProjectAllowedUser(name, id)
		if err == nil && tpl != "" {
			err = s.cfg.SetProjectCapabilityByUser(name, id, tpl)
		}
	case "role":
		err = s.cfg.AddProjectAllowedRole(name, id)
		if err == nil && tpl != "" {
			err = s.cfg.SetProjectCapabilityByRole(name, id, tpl)
		}
	default:
		err = fmt.Errorf("kind must be user or role")
	}
	s.auditAction(ctx, "config.add_project_member", err, map[string]any{
		"project": name, "kind": kind, "id": id, "template": tpl,
	})
	msg := fmt.Sprintf("Added %s %s", kind, id)
	if tpl != "" {
		msg = fmt.Sprintf("Added %s %s as %s", kind, id, tpl)
	}
	return s.projectConfigRedirect(ctx, name, msg, err)
}

func (s *Server) addChannel(ctx *hime.Context) error {
	channelID := ctx.PostFormValue("channelId")
	project := ctx.PostFormValue("project")
	err := s.cfg.AddChannel(channelID, project)
	s.auditAction(ctx, audit.ActionConfigAddChannel, err, map[string]any{"channelId": channelID, "project": project})
	msg := fmt.Sprintf("Mapped channel %s → %s", channelID, project)
	// Channel forms live on both the config hub and project settings pages.
	if ctx.PostFormValue("return_to") == "project" {
		return s.projectConfigTabRedirect(ctx, project, "integrations", msg, err)
	}
	if err != nil {
		return s.configPageRedirect(ctx, "config.channels", "", err)
	}
	return s.configPageRedirect(ctx, "config.channels", msg, nil)
}

func (s *Server) removeChannel(ctx *hime.Context) error {
	channelID := ctx.PostFormValue("channelId")
	err := s.cfg.RemoveChannel(channelID)
	s.auditAction(ctx, audit.ActionConfigRemoveChannel, err, map[string]any{"channelId": channelID})
	msg := fmt.Sprintf("Removed channel %s", channelID)
	if ctx.PostFormValue("return_to") == "project" {
		return s.projectConfigTabRedirect(ctx, ctx.PostFormValue("project"), "integrations", msg, err)
	}
	if err != nil {
		return s.configPageRedirect(ctx, "config.channels", "", err)
	}
	return s.configPageRedirect(ctx, "config.channels", msg, nil)
}

func (s *Server) setGitHubIdentity(ctx *hime.Context) error {
	discordID := strings.TrimSpace(ctx.PostFormValue("discordUserId"))
	login := strings.TrimSpace(ctx.PostFormValue("login"))
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	email := strings.TrimSpace(ctx.PostFormValue("email"))
	err := s.cfg.SetGitHubIdentity(discordID, config.GitHubIdentity{
		Login: login, Name: name, Email: email,
	})
	s.auditAction(ctx, audit.ActionConfigSetGitHubIdent, err, map[string]any{
		"discordUserId": discordID, "login": login,
	})
	if err != nil {
		return s.configPageRedirect(ctx, "config.identities", "", err)
	}
	msg := fmt.Sprintf("Mapped Discord %s → @%s", discordID, strings.TrimPrefix(login, "@"))
	return s.configPageRedirect(ctx, "config.identities", msg, nil)
}

func (s *Server) removeGitHubIdentity(ctx *hime.Context) error {
	discordID := strings.TrimSpace(ctx.PostFormValue("discordUserId"))
	err := s.cfg.RemoveGitHubIdentity(discordID)
	s.auditAction(ctx, audit.ActionConfigRemoveGitHubIdent, err, map[string]any{
		"discordUserId": discordID,
	})
	if err != nil {
		return s.configPageRedirect(ctx, "config.identities", "", err)
	}
	return s.configPageRedirect(ctx, "config.identities", fmt.Sprintf("Removed GitHub map for Discord %s", discordID), nil)
}

// configPageRedirect sends a config write back to the page it came from
// (each section's own drill-in page) with a flash or error in the query.
func (s *Server) configPageRedirect(ctx *hime.Context, routeName, okMsg string, err error) error {
	if err != nil {
		return ctx.RedirectTo(routeName, map[string]string{"err": err.Error()})
	}
	return ctx.RedirectTo(routeName, map[string]string{"ok": okMsg})
}

func (s *Server) updateRunSettings(ctx *hime.Context) error {
	err := s.updateRunSettingsErr(ctx)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "run"})
	if err != nil {
		return s.configPageRedirect(ctx, "config.run", "", err)
	}
	maxTurns, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("maxTurns")))
	timeoutMs, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("timeoutMs")))
	mins := float64(timeoutMs) / 60000
	msg := fmt.Sprintf("Grok run limits: maxTurns=%d, timeoutMs=%d (%.1f min)", maxTurns, timeoutMs, mins)
	return s.configPageRedirect(ctx, "config.run", msg, nil)
}

func (s *Server) updateWorktreeSettings(ctx *hime.Context) error {
	err := s.updateWorktreeSettingsErr(ctx)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "worktree"})
	if err != nil {
		return s.configPageRedirect(ctx, "config.worktrees", "", err)
	}
	days, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("worktreeIdleTTLDays")))
	worktreeDir := strings.TrimSpace(ctx.PostFormValue("worktreeDir"))
	msg := fmt.Sprintf("Worktree idle TTL set to %d day(s)", days)
	if days == 0 {
		msg = "Worktree idle cleanup disabled"
	}
	if worktreeDir == "" {
		msg += "; new worktrees use data/worktrees"
	} else {
		msg += "; new worktrees use " + worktreeDir
	}
	return s.configPageRedirect(ctx, "config.worktrees", msg, nil)
}

func (s *Server) updateBoardSettings(ctx *hime.Context) error {
	err := s.updateBoardSettingsErr(ctx)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "board"})
	if err != nil {
		return s.configPageRedirect(ctx, "config.board", "", err)
	}
	days, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("boardStaleDays")))
	channel := strings.TrimSpace(ctx.PostFormValue("boardDigestChannel"))
	msg := fmt.Sprintf("Board stale threshold set to %d day(s)", days)
	if channel == "" {
		msg += "; nightly digest disabled"
	} else {
		msg += fmt.Sprintf("; digest channel %s", channel)
	}
	return s.configPageRedirect(ctx, "config.board", msg, nil)
}

func (s *Server) updateCISettings(ctx *hime.Context) error {
	err := s.updateCISettingsErr(ctx)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "ci"})
	if err != nil {
		return s.configPageRedirect(ctx, "config.ci", "", err)
	}
	enabled := ctx.PostFormValue("autoFixCI") == "1" || strings.EqualFold(ctx.PostFormValue("autoFixCI"), "on")
	maxAttempts, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("autoFixCIMax")))
	msg := "Auto CI fix disabled"
	if enabled {
		msg = fmt.Sprintf("Auto CI fix enabled (max %d attempt(s) per thread)", maxAttempts)
	}
	return s.configPageRedirect(ctx, "config.ci", msg, nil)
}

func (s *Server) updateRiskyPathSettings(ctx *hime.Context) error {
	err := s.updateRiskyPathSettingsErr(ctx)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "risky"})
	if err != nil {
		return s.configPageRedirect(ctx, "config.risky", "", err)
	}
	useDefault := ctx.PostFormValue("riskyPathUseDefault") == "1" ||
		strings.EqualFold(ctx.PostFormValue("riskyPathUseDefault"), "on")
	msg := "Risky path globs set to built-in defaults"
	if !useDefault {
		n := 0
		for _, line := range strings.Split(ctx.PostFormValue("riskyPathGlobs"), "\n") {
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
	return s.configPageRedirect(ctx, "config.risky", msg, nil)
}

func (s *Server) updateDiscordPRLinkSettings(ctx *hime.Context) error {
	err := s.updateDiscordPRLinkSettingsErr(ctx)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "discordPRLink"})
	if err != nil {
		return s.configPageRedirect(ctx, "config.prlinks", "", err)
	}
	msg := "Discord PR links: GitHub"
	if strings.TrimSpace(ctx.PostFormValue("discordPRLink")) == config.DiscordPRLinkWeb {
		msg = "Discord PR links: web UI"
		if s.cfg.WebPublicBaseURLValue() == "" {
			msg += " (set webPublicBaseURL so links resolve)"
		}
	}
	return s.configPageRedirect(ctx, "config.prlinks", msg, nil)
}

// updateResumeSettings has no drill-in page — its toggle lives on the hub.
func (s *Server) updateResumeSettings(ctx *hime.Context) error {
	err := s.updateResumeSettingsErr(ctx)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "resume"})
	if err != nil {
		return s.configRedirect(ctx, "", err)
	}
	enabled := ctx.PostFormValue("resumeActiveRuns") == "1" || strings.EqualFold(ctx.PostFormValue("resumeActiveRuns"), "on")
	msg := "Crash-safe resume disabled"
	if enabled {
		msg = "Crash-safe resume enabled"
	}
	return s.configRedirect(ctx, msg, nil)
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
	worktreeDir := strings.TrimSpace(ctx.PostFormValue("worktreeDir"))
	return s.cfg.SetWorktreeSettings(days, worktreeDir)
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

// mergeSessionRows adds session-store threads that have no history turns yet,
// and overlays label/PR/case closed state onto every matching history row so
// the sessions list still shows final state after worktree cleanup.
func mergeSessionRows(hist []history.Summary, sessions []sessionstore.Listed) []history.Summary {
	byID := make(map[string]sessionstore.Listed, len(sessions))
	for _, se := range sessions {
		byID[se.ThreadID] = se
	}
	seen := make(map[string]struct{}, len(hist))
	for i := range hist {
		seen[hist[i].ThreadID] = struct{}{}
		if se, ok := byID[hist[i].ThreadID]; ok {
			applySessionOverlay(&hist[i], se)
		}
	}
	for _, se := range sessions {
		if _, ok := seen[se.ThreadID]; ok {
			continue
		}
		row := history.Summary{
			ThreadID:  se.ThreadID,
			Project:   se.Project,
			LastUser:  se.LastUser,
			UpdatedAt: se.UpdatedAt,
			TurnCount: 0,
		}
		applySessionOverlay(&row, se)
		// Prefer sticky goal as last-prompt preview when no turns exist.
		if row.LastPrompt == "" {
			if g := strings.TrimSpace(se.Goal); g != "" {
				row.LastPrompt = g
			} else if se.IsCase() {
				if t := strings.TrimSpace(se.CustomerTitle); t != "" {
					row.LastPrompt = t
				}
			}
		}
		hist = append(hist, row)
	}
	return hist
}

// applySessionOverlay copies lifecycle + primary PR fields from a session entry
// onto a list row (history may already have turns / project).
func applySessionOverlay(row *history.Summary, se sessionstore.Listed) {
	if row == nil {
		return
	}
	e := se.Entry
	e.NormalizePRs()
	if row.Project == "" {
		row.Project = e.Project
	}
	if row.LastUser == "" {
		row.LastUser = e.LastUser
	}
	// Prefer the more recent of history turn time vs session UpdatedAt.
	if e.UpdatedAt != "" && (row.UpdatedAt == "" || e.UpdatedAt > row.UpdatedAt) {
		row.UpdatedAt = e.UpdatedAt
	}
	row.Label = e.EffectiveLabel()
	row.Mode = strings.TrimSpace(e.Mode)
	row.Phase = e.CasePhase()
	row.Resolution = strings.TrimSpace(e.Resolution)
	row.HasPRs = e.HasAnyPR()
	if pr, ok := e.PrimaryPR(); ok {
		row.PRNumber = pr.Number
		row.PRState = strings.ToUpper(strings.TrimSpace(pr.State))
		row.PROwner = pr.Owner
		row.PRRepo = pr.Repo
		row.PRURL = pr.URL
		row.PRTitle = pr.Title
	}
}

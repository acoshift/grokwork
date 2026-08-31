package web

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grokwork/internal/apitoken"
	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/clickup"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/deploy"
	"github.com/acoshift/grokwork/internal/errsrc"
	"github.com/acoshift/grokwork/internal/errsrc/deploys"
	"github.com/acoshift/grokwork/internal/errsrc/gcperr"
	sentrycli "github.com/acoshift/grokwork/internal/errsrc/sentry"
	"github.com/acoshift/grokwork/internal/filestore"
	"github.com/acoshift/grokwork/internal/gcs"
	"github.com/acoshift/grokwork/internal/gdrive"
	"github.com/acoshift/grokwork/internal/ghpr"
	"github.com/acoshift/grokwork/internal/gitworktree"
	"github.com/acoshift/grokwork/internal/grokrun"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/identity"
	"github.com/acoshift/grokwork/internal/linear"
	"github.com/acoshift/grokwork/internal/markdown"
	"github.com/acoshift/grokwork/internal/reviewstore"
	"github.com/acoshift/grokwork/internal/sessionstore"
	"github.com/acoshift/grokwork/internal/skills"
	"github.com/acoshift/grokwork/internal/spend"
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
	oauthGoogle GoogleOAuth  // nil → HTTPGoogleOAuth
	oauthGitHub GitHubOAuth  // nil → HTTPGitHubOAuth
	audit       *audit.Logger
	// identity resolves a login to the account it belongs to, at the one place a
	// web actor id is minted (oauthCallback). It is the bot's instance, not a
	// second one over the same file — see bot.SetIdentity.
	identity  *identity.Store
	apiTokens *apitoken.Store
	// Test injectables (nil → production defaults).
	ghRunner  ghpr.Runner
	gcsRunner gcs.Runner
	// githubImageGet, when set, replaces the authenticated GitHub image GET.
	githubImageGet func(ctx context.Context, rawURL string) (contentType string, body []byte, err error)
	ghTokenMu      sync.Mutex
	ghToken        string
	ghTokenUntil   time.Time
	githubImgMu    sync.Mutex
	githubImgCache map[string]githubImageCacheEntry
	// Drive Files injectables (nil → production JWT + default HTTP clients).
	driveHTTP *http.Client
	driveAuth gdrive.TokenSource
	// filesBackendFn, when set, replaces filesBackend construction (unit tests).
	filesBackendFn func(filestore.Target) (filestore.Backend, error)
	deploys        *deploy.Engine
	// deployScanLimit bounds the /deploys board's fold over the run store
	// (0 → deploy.DefaultLaneScanLimit); tests shrink it to reach the clipped
	// path without writing hundreds of records.
	deployScanLimit int
	linearNew       func(apiKey string) *linear.Client
	clickupNew      func(apiKey string) *clickup.Client
	// Official vendor pricing docs (nil/empty → xAI + Anthropic + Cursor
	// markdown URLs + 15s client). Tests point these at httptest.Server.
	officialRateHTTP *http.Client
	officialRateURLs config.OfficialRateURLs
	deploysNew       func(opts deploys.Options) *deploys.Client
	sentryNew        func(token, org, project, baseURL string) *sentrycli.Client
	gcpNew           func(project string) *gcperr.Client
	// Fix-with-Grok rate limit (lazy init).
	startLimit *startRateLimiter
	// PR raw-patch cache (page + per-file fragments share one gh pr diff).
	prPatchMu sync.Mutex
	prPatches map[string]prPatchEntry
	// Short-TTL GitHub issue list cache (page shell + partial share one gh call).
	issueListMu sync.Mutex
	issueLists  map[string]issueListCacheEntry
	// Short-TTL production-error count cache for nav badges.
	errorCountMu sync.Mutex
	errorCounts  map[string]errorCountCacheEntry
	// Short-TTL GitHub Actions caches (run list + workflows list).
	actionsRunsMu      sync.Mutex
	actionsRuns        map[string]actionsRunsCacheEntry
	actionsWorkflowsMu sync.Mutex
	actionsWorkflows   map[string]actionsWorkflowsCacheEntry
	// One-shot Grok drafts for project verify commands (filled into the
	// workflow textarea after "Suggest with Grok"; not persisted until Save).
	verifyDraftMu sync.Mutex
	verifyDrafts  map[string]string
	// Test injectable; nil → grokrun.SuggestVerifyCommands (SSE stream hooks optional).
	suggestVerify func(ctx context.Context, cli grokrun.CLI, cwd string, timeout time.Duration, hooks *grokrun.SuggestStreamHooks) (string, error)
	// Test injectable; nil → grokrun.SuggestConflictResolution.
	suggestConflict func(ctx context.Context, cli grokrun.CLI, cwd string, timeout time.Duration, files []string, target, sha string, hooks *grokrun.SuggestStreamHooks) (string, error)
	// deploysCLI, when set, replaces exec of the deploys binary (errors token mint).
	deploysCLI deploys.CLIRunner
	// liveMu guards liveCache. SSE connections share host-wide fingerprints so
	// an idle tab does not re-walk sessions, history, and boards every 2s.
	liveMu    sync.Mutex
	liveCache liveRevCache
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
	deployEngine, err := deploy.NewEngine(cfg, cfg.DataDir)
	if err != nil {
		panic("web: deploy engine: " + err.Error())
	}
	tokens, err := apitoken.New(cfg.DataDir)
	if err != nil {
		panic("web: api tokens: " + err.Error())
	}
	if _, err := tokens.BootstrapFromEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] api token bootstrap: %v\n", err)
	}
	s := &Server{cfg: cfg, sessions: sessions, history: hist, bot: b, webSessions: webSess, webUsers: webUsers, audit: auditLog, deploys: deployEngine, identity: b.Identity(), apiTokens: tokens}
	s.wireDeployNotifier()
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

	// One "auth.<provider>" / "auth.<provider>.callback" pair per login
	// provider, registered unconditionally: route names are static, and whether
	// a provider may actually be used is decided by providerConfigured inside
	// the handler (see oauth_provider.go). TestAuthRoutesMatchProviderTable pins
	// these against authStartPath / authCallbackPath so the two cannot drift.
	// Linking another login runs the same handshake but starts at ONE route,
	// "account.link" — a POST carrying the provider and a CSRF token, because
	// starting a link mutates the account (see postAccountLink). There is
	// deliberately no per-provider GET that begins it.
	app.Routes(hime.Routes{
		"home":                               "/",
		"login":                              "/login",
		"auth.discord":                       "/auth/discord",
		"auth.discord.callback":              "/auth/discord/callback",
		"auth.google":                        "/auth/google",
		"auth.google.callback":               "/auth/google/callback",
		"auth.github":                        "/auth/github",
		"auth.github.callback":               "/auth/github/callback",
		"logout":                             "/logout",
		"account":                            "/account",
		"account.link":                       "/account/link",
		"account.unlink":                     "/account/unlink",
		"history":                            "/history",
		"history.thread":                     "/history/",
		"sessions":                           "/sessions",
		"sessions.thread":                    "/sessions/",
		"ship":                               "/ship",
		"cases":                              "/cases",
		"search":                             "/search",
		"inbox":                              "/inbox",
		"spend":                              "/spend",
		"deploys":                            "/deploys",
		"worktrees":                          "/worktrees",
		"worktrees.prune":                    "/worktrees/prune",
		"worktrees.pruneIdle":                "/worktrees/prune-idle",
		"config":                             "/config",
		"config.addProject":                  "/config/projects",
		"config.removeProject":               "/config/projects/remove",
		"config.setProjectLinear":            "/config/projects/linear",
		"config.setProjectClickUp":           "/config/projects/clickup",
		"config.setProjectErrorsGCP":         "/config/projects/errors-gcp",
		"config.setProjectErrorsSentry":      "/config/projects/errors-sentry",
		"config.setProjectErrorsDeploys":     "/config/projects/errors-deploys",
		"config.generateDeploysErrorsToken":  "/config/projects/errors-deploys/generate-token",
		"config.setProjectGitHub":            "/config/projects/github",
		"config.setProjectStorage":           "/config/projects/storage",
		"config.storage":                     "/config/storage",
		"config.setProjectChannel":           "/config/projects/channel",
		"config.setProjectFetch":             "/config/projects/fetch",
		"config.setProjectShip":              "/config/projects/ship",
		"config.setProjectAgentMCP":          "/config/projects/agent-mcp",
		"config.setProjectSafeTeam":          "/config/projects/safe-team",
		"config.setProjectVerify":            "/config/projects/verify",
		"config.setProjectDeployEnabled":     "/config/projects/deploy/enabled",
		"config.setProjectDeployEnv":         "/config/projects/deploy/environment",
		"config.removeProjectDeployEnv":      "/config/projects/deploy/environment/remove",
		"config.setProjectDeployEnvVar":      "/config/projects/deploy/var",
		"config.removeProjectDeployEnvVar":   "/config/projects/deploy/var/remove",
		"config.setProjectActionsRule":       "/config/projects/actions-rule",
		"config.removeProjectActionsRule":    "/config/projects/actions-rule/remove",
		"config.generateProjectVerify":       "/config/projects/verify/generate",
		"config.setProjectMode":              "/config/projects/mode",
		"config.setProjectCaseKey":           "/config/projects/case-key",
		"config.setProjectPrimaryBranch":     "/config/projects/primary-branch",
		"config.setProjectCherryPickTargets": "/config/projects/cherry-pick-targets",
		"config.setProjectSLA":               "/config/projects/sla",
		"config.addProjectMember":            "/config/projects/members",
		"config.setProjectCapabilityUser":    "/config/projects/capabilities/users",
		"config.removeProjectCapabilityUser": "/config/projects/capabilities/users/remove",
		"config.setProjectTeam":              "/config/projects/teams",
		"config.removeProjectTeam":           "/config/projects/teams/remove",
		"config.addProjectTeamMember":        "/config/projects/teams/members",
		"config.removeProjectTeamMember":     "/config/projects/teams/members/remove",
		"config.setGuild":                    "/config/guild",
		"config.addProjectUser":              "/config/projects/users",
		"config.removeProjectUser":           "/config/projects/users/remove",
		"config.addChannel":                  "/config/channels",
		"config.removeChannel":               "/config/channels/remove",
		"config.bot":                         "/config/bot",
		"config.channels":                    "/config/channels",
		"config.projectNew":                  "/config/projects/new",
		"config.run":                         "/config/run",
		"config.agent":                       "/config/agent",
		"config.worktrees":                   "/config/worktrees",
		"config.board":                       "/config/board",
		"config.notify":                      "/config/notify",
		"config.apiTokens":                   "/config/api-tokens",
		"config.apiTokens.revoke":            "/config/api-tokens/revoke",
		"config.ci":                          "/config/ci",
		"config.prlinks":                     "/config/pr-links",
		"config.risky":                       "/config/risky",
		"config.skills":                      "/config/skills",
		"config.rates":                       "/config/model-rates",
		"config.rates.official":              "/config/model-rates/official",
		"config.resume":                      "/config/resume",
		"issues":                             "/issues",
		"issues.project":                     "/projects/",
		"commits":                            "/commits",
		"pr.detail":                          "/prs/",
		"sse":                                "/events",
		// Live partials (htmx SSE domain swaps) — separate URLs so each region
		// can refresh independently. Fragments render via View("page#define").
		"partial.home.projects":        "/partials/home/projects",
		"partial.home.counts":          "/partials/home/counts",
		"partial.home.runs":            "/partials/home/runs",
		"partial.project.pulse":        "/partials/projects/pulse",
		"partial.project.pulse.counts": "/partials/projects/pulse/counts",
		"partial.project.pulse.runs":   "/partials/projects/pulse/runs",
		"partial.ship.stats":           "/partials/ship/stats",
		"partial.ship.table":           "/partials/ship/table",
		"partial.cases.counts":         "/partials/cases/counts",
		"partial.cases.pipeline":       "/partials/cases/pipeline",
		"partial.cases.list":           "/partials/cases/list",
		"partial.history.table":        "/partials/history/table",
		"partial.history.turns":        "/partials/history/turns/",
		"partial.session":              "/partials/sessions/",
		"partial.worktrees.table":      "/partials/worktrees/table",
		"partial.deploys.board":        "/partials/deploys/board",
		"partial.issues.table":         "/partials/issues/table",
		"partial.nav.counts":           "/partials/nav/counts",
		"partial.inbox.list":           "/partials/inbox/list",
		"inbox.read":                   "/inbox/read",
		"partial.pr.gates":             "/partials/prs/",
		"partial.config.lists":         "/partials/config/lists",
		"partial.config.channels":      "/partials/config/channels",
	})

	app.TemplateFunc("add", func(a, b int) int { return a + b })
	app.TemplateFunc("sub", func(a, b int) int { return a - b })
	// Millisecond durations as compact unit-suffixed text (config form + hub).
	app.TemplateFunc("msDur", formatMsDur)
	app.TemplateFunc("markdown", markdown.Render)
	app.TemplateFunc("githubMarkdown", s.githubMarkdown)
	// shortTime formats a time.Time or RFC3339 string as "2006-01-02 15:04"
	// (same layout as the commits list Date column).
	app.TemplateFunc("shortTime", shortTime)
	// relativeAge formats a time.Time or RFC3339 string as a coarse age ("2h ago").
	app.TemplateFunc("relativeAge", relativeAge)
	// runBucketBadge maps ghpr.RunBucket → layout badge CSS class.
	app.TemplateFunc("runBucketBadge", runBucketBadge)
	// Bound-issue list on the session page (GitHub / Linear / ClickUp).
	app.TemplateFunc("trackedIssueHref", trackedIssueHref)
	app.TemplateFunc("sessionFile", sessionFileURL)
	app.TemplateFunc("sessionRunFile", sessionRunFileURL)
	app.TemplateFunc("sessionArtifact", sessionArtifactURL)
	// Spend report formatting. cost/models take a whole row rather than a number
	// because an unpriced row must render "—" and not "$0.00" — the decision needs
	// the row's Priced/Unpriced counts, so it cannot live in the template.
	app.TemplateFunc("tokens", formatTokens)
	app.TemplateFunc("usd", formatUSD)
	app.TemplateFunc("cost", spendCost)
	app.TemplateFunc("models", spendModels)

	// One template set per page: layout root for full documents; named {{define}}s
	// for SSE fragments (ctx.View("dashboard#dashboard_stats", …)).
	tp := app.Template()
	tp.FS(templateFS)
	tp.Dir("templates")
	tp.Root("layout")
	tp.ParseFiles("home", "layout.tmpl", "home.tmpl")
	tp.ParseFiles("project_overview", "layout.tmpl", "project_overview.tmpl", "session_badges.tmpl")
	tp.ParseFiles("history", "layout.tmpl", "history.tmpl")
	tp.ParseFiles("history_detail", "layout.tmpl", "history_detail.tmpl")
	tp.ParseFiles("sessions", "layout.tmpl", "sessions.tmpl", "session_badges.tmpl")
	tp.ParseFiles("ship", "layout.tmpl", "ship.tmpl")
	tp.ParseFiles("cases", "layout.tmpl", "cases.tmpl")
	tp.ParseFiles("inbox", "layout.tmpl", "inbox.tmpl")
	tp.ParseFiles("search", "layout.tmpl", "search.tmpl")
	tp.ParseFiles("case_new", "layout.tmpl", "case_new.tmpl")
	tp.ParseFiles("worktrees", "layout.tmpl", "worktrees.tmpl")
	tp.ParseFiles("spend", "layout.tmpl", "spend.tmpl")
	tp.ParseFiles("config", "layout.tmpl", "config.tmpl")
	tp.ParseFiles("config_bot", "layout.tmpl", "config_bot.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_channels", "layout.tmpl", "config_channels.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_project_new", "layout.tmpl", "config_project_new.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_run", "layout.tmpl", "config_run.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_agent", "layout.tmpl", "config_agent.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_worktrees", "layout.tmpl", "config_worktrees.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_board", "layout.tmpl", "config_board.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_notify", "layout.tmpl", "config_notify.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_api_tokens", "layout.tmpl", "config_api_tokens.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_ci", "layout.tmpl", "config_ci.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_prlinks", "layout.tmpl", "config_prlinks.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_risky", "layout.tmpl", "config_risky.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_skills", "layout.tmpl", "config_skills.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_rates", "layout.tmpl", "config_rates.tmpl", "config_shared.tmpl")
	tp.ParseFiles("config_storage", "layout.tmpl", "config_storage.tmpl", "config_shared.tmpl")
	tp.ParseFiles("project_config", "layout.tmpl", "project_config.tmpl", "project_config_shared.tmpl")
	tp.ParseFiles("project_config_workflow", "layout.tmpl", "project_config_workflow.tmpl", "project_config_shared.tmpl")
	tp.ParseFiles("project_config_integrations", "layout.tmpl", "project_config_integrations.tmpl", "project_config_shared.tmpl")
	tp.ParseFiles("project_config_deploy", "layout.tmpl", "project_config_deploy.tmpl", "project_config_shared.tmpl")
	tp.ParseFiles("project_config_danger", "layout.tmpl", "project_config_danger.tmpl", "project_config_shared.tmpl")
	tp.ParseFiles("login", "layout.tmpl", "login.tmpl")
	tp.ParseFiles("account", "layout.tmpl", "account.tmpl")
	tp.ParseFiles("issues", "layout.tmpl", "issues.tmpl")
	tp.ParseFiles("issue_new", "layout.tmpl", "issue_new.tmpl")
	tp.ParseFiles("issue_detail", "layout.tmpl", "issue_detail.tmpl")
	tp.ParseFiles("linear_issues", "layout.tmpl", "linear_issues.tmpl")
	tp.ParseFiles("linear_detail", "layout.tmpl", "linear_detail.tmpl")
	tp.ParseFiles("clickup_issues", "layout.tmpl", "clickup_issues.tmpl")
	tp.ParseFiles("clickup_detail", "layout.tmpl", "clickup_detail.tmpl")
	tp.ParseFiles("errors", "layout.tmpl", "errors.tmpl")
	tp.ParseFiles("error_detail", "layout.tmpl", "error_detail.tmpl")
	tp.ParseFiles("pr_detail", "layout.tmpl", "pr_detail.tmpl")
	tp.ParseFiles("reviews", "layout.tmpl", "reviews.tmpl")
	tp.ParseFiles("diff", "layout.tmpl", "diff.tmpl", "diff_review.tmpl")
	tp.ParseFiles("session", "layout.tmpl", "session.tmpl")
	tp.ParseFiles("start", "layout.tmpl", "start.tmpl")
	tp.ParseFiles("commits", "layout.tmpl", "commits.tmpl")
	tp.ParseFiles("deploys", "layout.tmpl", "deploys.tmpl")
	tp.ParseFiles("deploys_board", "layout.tmpl", "deploys_board.tmpl")
	tp.ParseFiles("deploy_run", "layout.tmpl", "deploy_run.tmpl")
	tp.ParseFiles("files", "layout.tmpl", "files.tmpl")
	tp.ParseFiles("actions", "layout.tmpl", "actions.tmpl")
	tp.ParseFiles("actions_run", "layout.tmpl", "actions_run.tmpl")
	tp.ParseFiles("commit_detail", "layout.tmpl", "commit_detail.tmpl", "diff_review.tmpl")
	tp.ParseFiles("cherrypick_conflict", "layout.tmpl", "cherrypick_conflict.tmpl")

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: static fs: " + err.Error())
	}

	mux := http.NewServeMux()
	// Public (static + PWA install assets + auth)
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(static)))))
	registerPWA(mux)
	s.registerAPI(mux)
	mux.Handle("GET /login", hime.Handler(s.loginPage))
	// {provider} matches exactly one segment, so /auth/discord/callback cannot
	// be swallowed by /auth/{provider}; an unknown key 404s in the handler.
	mux.Handle("GET /auth/{provider}", hime.Handler(s.oauthStart))
	mux.Handle("GET /auth/{provider}/callback", hime.Handler(s.oauthCallback))
	mux.Handle("POST /logout", hime.Handler(s.logout))
	// Self-service account: your logins, whatever your role.
	//
	// Starting a link is a POST under requireAccount (session + CSRF), because it
	// ends in an irreversible merge of two identities: as a GET, one cross-site
	// click completed the whole flow through SameSite=Lax cookies and a silent
	// provider re-authorization. The callback stays public — the provider
	// redirects to it — and re-checks the session against the one baked into the
	// state cookie.
	mux.Handle("GET /account", s.requireAuth(hime.Handler(s.accountPage)))
	mux.Handle("POST /account/link", s.requireAccount(hime.Handler(s.postAccountLink)))
	mux.Handle("POST /account/unlink", s.requireAccount(hime.Handler(s.postAccountUnlink)))

	// Authenticated pages + SSE + partials
	mux.Handle("GET /{$}", s.requireAuth(hime.Handler(s.home)))
	mux.Handle("GET /history", s.requireAuth(hime.Handler(s.historyList)))
	mux.Handle("GET /history/{threadID}", s.requireAuth(hime.Handler(s.historyDetail)))
	mux.Handle("GET /sessions", s.requireAuth(hime.Handler(s.sessionsList)))
	mux.Handle("GET /sessions/{threadID}/diff", s.requireAuth(hime.Handler(s.sessionDiffPage)))
	mux.Handle("GET /sessions/{threadID}/diff/file", s.requireAuth(hime.Handler(s.sessionDiffFile)))
	mux.Handle("GET /sessions/{threadID}/turns/{n}/files/{name}", s.requireAuth(hime.Handler(s.sessionTurnFile)))
	mux.Handle("GET /sessions/{threadID}/run/files/{name}", s.requireAuth(hime.Handler(s.sessionRunFile)))
	mux.Handle("GET /sessions/{threadID}/artifacts/{name}", s.requireAuth(hime.Handler(s.sessionArtifact)))
	mux.Handle("GET /sessions/{threadID}", s.requireAuth(hime.Handler(s.sessionPage)))
	mux.Handle("GET /ship", s.requireAuth(hime.Handler(s.shipPage)))
	mux.Handle("GET /cases", s.requireAuth(hime.Handler(s.casesGlobal)))
	// One box over sessions, cases, tracked PRs/issues and recent commits.
	// Global shell with ?project= as a data filter (like /ship): results are
	// ACL-filtered before they are ranked — see search.go.
	mux.Handle("GET /search", s.requireAuth(hime.Handler(s.searchPage)))
	mux.Handle("GET /inbox", s.requireAuth(hime.Handler(s.inboxPage)))
	mux.Handle("POST /inbox/read", s.requireAuth(hime.Handler(s.postInboxRead)))
	mux.Handle("GET /partials/inbox/list", s.requireAuth(hime.Handler(s.partialInboxList)))
	mux.Handle("GET /worktrees", s.requireAuth(hime.Handler(s.worktreesPage)))
	// Cross-project deploy board. Read-only and global — triggering stays on
	// /projects/{p}/deploys, where the manifest and the environment gates are.
	mux.Handle("GET /deploys", s.requireAuth(hime.Handler(s.deploysBoard)))
	mux.Handle("GET /spend", s.requireAuth(hime.Handler(s.spendPage)))
	mux.Handle("GET /config", s.requireAdmin(hime.Handler(s.configPage)))
	mux.Handle("GET /config/projects/{name}", s.requireAdmin(hime.Handler(s.projectConfigPage)))
	mux.Handle("GET /config/projects/{name}/workflow", s.requireAdmin(hime.Handler(s.projectConfigWorkflowPage)))
	mux.Handle("GET /config/projects/{name}/integrations", s.requireAdmin(hime.Handler(s.projectConfigIntegrationsPage)))
	mux.Handle("GET /config/projects/{name}/deploy", s.requireAdmin(hime.Handler(s.projectConfigDeployPage)))
	mux.Handle("GET /config/projects/{name}/danger", s.requireAdmin(hime.Handler(s.projectConfigDangerPage)))
	// Deploy policy and credentials are admin-only, like every other config write.
	mux.Handle("POST /config/projects/deploy/enabled", s.requireAdmin(hime.Handler(s.setProjectDeployEnabled)))
	mux.Handle("POST /config/projects/deploy/environment", s.requireAdmin(hime.Handler(s.setProjectDeployEnv)))
	mux.Handle("POST /config/projects/deploy/environment/remove", s.requireAdmin(hime.Handler(s.removeProjectDeployEnv)))
	mux.Handle("POST /config/projects/deploy/var", s.requireAdmin(hime.Handler(s.setProjectDeployEnvVar)))
	mux.Handle("POST /config/projects/deploy/var/remove", s.requireAdmin(hime.Handler(s.removeProjectDeployEnvVar)))
	mux.Handle("POST /config/projects/actions-rule", s.requireAdmin(hime.Handler(s.setProjectActionsRule)))
	mux.Handle("POST /config/projects/actions-rule/remove", s.requireAdmin(hime.Handler(s.removeProjectActionsRule)))
	// Project workspace (project-first UX): overview + scoped list pages.
	mux.Handle("GET /projects/{project}", s.requireAuth(hime.Handler(s.projectOverview)))
	mux.Handle("GET /projects/{project}/start", s.requireAuth(hime.Handler(s.startComposer)))
	mux.Handle("GET /projects/{project}/ship", s.requireAuth(hime.Handler(s.shipScoped)))
	mux.Handle("GET /projects/{project}/cases", s.requireAuth(hime.Handler(s.casesScoped)))
	mux.Handle("GET /projects/{project}/cases/new", s.requireAuth(hime.Handler(s.caseNewPage)))
	// Case key → session page. Both spellings resolve the same reference: the
	// workspace one for links inside the app, /c/{key} for what a person types
	// or pastes into a commit message. {key} cannot shadow "new" above —
	// ServeMux prefers the literal segment.
	mux.Handle("GET /projects/{project}/cases/{key}", s.requireAuth(hime.Handler(s.caseByKey)))
	mux.Handle("GET /c/{key}", s.requireAuth(hime.Handler(s.caseByKey)))
	mux.Handle("GET /projects/{project}/sessions", s.requireAuth(hime.Handler(s.sessionsScoped)))
	mux.Handle("GET /projects/{project}/worktrees", s.requireAuth(hime.Handler(s.worktreesScoped)))
	mux.Handle("GET /projects/{project}/spend", s.requireAuth(hime.Handler(s.spendScoped)))
	// Retired feature-first hubs → launcher.
	mux.Handle("GET /issues", s.requireAuth(hime.Handler(s.redirectHome)))
	mux.Handle("GET /projects/{project}/issues", s.requireAuth(hime.Handler(s.issuesList)))
	// Literal /new before {n}: ServeMux prefers the more specific pattern, but
	// keep the order obvious so a future catch-all cannot swallow the form.
	mux.Handle("GET /projects/{project}/issues/new", s.requireAuth(hime.Handler(s.issueNewPage)))
	mux.Handle("GET /projects/{project}/issues/{n}", s.requireAuth(hime.Handler(s.issueDetail)))
	mux.Handle("GET /projects/{project}/github-images", s.requireAuth(hime.Handler(s.githubImage)))
	mux.Handle("GET /projects/{project}/linear", s.requireAuth(hime.Handler(s.linearList)))
	mux.Handle("GET /projects/{project}/linear/{identifier}", s.requireAuth(hime.Handler(s.linearDetail)))
	mux.Handle("GET /projects/{project}/clickup", s.requireAuth(hime.Handler(s.clickupList)))
	mux.Handle("GET /projects/{project}/clickup/{id}", s.requireAuth(hime.Handler(s.clickupDetail)))
	mux.Handle("GET /projects/{project}/errors", s.requireAuth(hime.Handler(s.errorsLanding)))
	mux.Handle("GET /projects/{project}/errors/{src}/{id}", s.requireAuth(hime.Handler(s.errorDetail)))
	mux.Handle("GET /commits", s.requireAuth(hime.Handler(s.redirectHome)))
	mux.Handle("GET /projects/{project}/deploys", s.requireAuth(hime.Handler(s.deploysPage)))
	mux.Handle("GET /projects/{project}/deploys/{runID}", s.requireAuth(hime.Handler(s.deployRunPage)))
	mux.Handle("GET /projects/{project}/deploys/{runID}/log", s.requireAuth(hime.Handler(s.deployRunLog)))
	mux.Handle("GET /projects/{project}/deploys/{runID}/log.txt", s.requireAuth(hime.Handler(s.deployRunLogRaw)))
	mux.Handle("POST /projects/{project}/deploys",
		s.requireFeature("deploy", s.requireMember(hime.Handler(s.postDeploy))))
	// Gated on startSessions, not the deploy feature: writing the pipeline is
	// ordinary agent work that ends in a PR, and it has to be possible before
	// deploys are switched on.
	mux.Handle("POST /projects/{project}/deploys/generate",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postDeployGenerate))))
	mux.Handle("POST /projects/{project}/deploys/{runID}/cancel",
		s.requireFeature("deploy", s.requireMember(hime.Handler(s.postDeployCancel))))
	mux.Handle("POST /projects/{project}/deploys/{runID}/redeploy",
		s.requireFeature("deploy", s.requireMember(hime.Handler(s.postDeployRedeploy))))
	// Project file storage. Page + download + preview are membership-only reads;
	// upload/delete need the storage feature flag and CanStorageWrite in the handler.
	mux.Handle("GET /projects/{project}/files", s.requireAuth(hime.Handler(s.filesPage)))
	mux.Handle("GET /projects/{project}/files/download", s.requireAuth(hime.Handler(s.fileDownload)))
	mux.Handle("GET /projects/{project}/files/preview", s.requireAuth(hime.Handler(s.filePreview)))
	mux.Handle("POST /projects/{project}/files/upload",
		s.requireFeature("storage", s.requireMember(hime.Handler(s.postFileUpload))))
	mux.Handle("POST /projects/{project}/files/delete",
		s.requireFeature("storage", s.requireMember(hime.Handler(s.postFileDelete))))
	// GitHub Actions: list/dispatch workflows + run history (polling, not SSE).
	mux.Handle("GET /projects/{project}/actions", s.requireAuth(hime.Handler(s.actionsPage)))
	mux.Handle("GET /projects/{project}/actions/runs/{runID}", s.requireAuth(hime.Handler(s.actionsRunPage)))
	mux.Handle("GET /projects/{project}/actions/runs/{runID}/job", s.requireAuth(hime.Handler(s.actionsJobLog)))
	mux.Handle("GET /partials/projects/{project}/actions/runs", s.requireAuth(hime.Handler(s.actionsRunsPartial)))
	mux.Handle("GET /partials/projects/{project}/actions/workflows", s.requireAuth(hime.Handler(s.actionsWorkflowsPartial)))
	mux.Handle("POST /projects/{project}/actions/dispatch",
		s.requireFeature("githubWrites", s.requireMember(hime.Handler(s.postActionsDispatch))))
	mux.Handle("GET /projects/{project}/commits", s.requireAuth(hime.Handler(s.commitsList)))
	mux.Handle("POST /projects/{project}/commits/fetch", s.requireMember(hime.Handler(s.postCommitsFetch)))
	mux.Handle("POST /projects/{project}/commits/cherrypick", s.requireMember(hime.Handler(s.postCommitsCherryPick)))
	mux.Handle("POST /projects/{project}/commits/force-push", s.requireMember(hime.Handler(s.postCommitsForcePush)))
	mux.Handle("GET /projects/{project}/cherrypick/{id}", s.requireAuth(hime.Handler(s.cherryPickConflictPage)))
	mux.Handle("POST /projects/{project}/cherrypick/{id}/file", s.requireMember(hime.Handler(s.postCherryPickFile)))
	mux.Handle("POST /projects/{project}/cherrypick/{id}/ours", s.requireMember(hime.Handler(s.postCherryPickOurs)))
	mux.Handle("POST /projects/{project}/cherrypick/{id}/theirs", s.requireMember(hime.Handler(s.postCherryPickTheirs)))
	mux.Handle("POST /projects/{project}/cherrypick/{id}/continue", s.requireMember(hime.Handler(s.postCherryPickContinue)))
	mux.Handle("POST /projects/{project}/cherrypick/{id}/abort", s.requireMember(hime.Handler(s.postCherryPickAbort)))
	mux.Handle("POST /projects/{project}/cherrypick/{id}/suggest", s.requireMember(hime.Handler(s.postCherryPickSuggest)))
	mux.Handle("GET /projects/{project}/commits/{sha}", s.requireAuth(hime.Handler(s.commitDetail)))
	mux.Handle("GET /projects/{project}/commits/{sha}/file", s.requireAuth(hime.Handler(s.commitDiffFile)))
	mux.Handle("GET /prs/{owner}/{repo}/{n}", s.requireAuth(hime.Handler(s.prDetail)))
	mux.Handle("GET /prs/{owner}/{repo}/{n}/diff", s.requireAuth(hime.Handler(s.prDiffPage)))
	mux.Handle("GET /prs/{owner}/{repo}/{n}/diff/file", s.requireAuth(hime.Handler(s.prDiffFile)))
	// GitHub writes (PR8–9): always registered; request-time feature + role gates.
	mux.Handle("POST /projects/{project}/issues/new",
		s.requireFeature("githubWrites", s.requireMember(hime.Handler(s.postIssueNew))))
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
	// A real `gh pr review` as the host gh user — the one review that can satisfy
	// branch protection. Gated by githubWrites (not prReviews) because it spends
	// the GitHub credential, and kept separate from POST …/reviews above, which
	// only records a grokwork-local team verdict.
	mux.Handle("POST /prs/{owner}/{repo}/{n}/github-reviews",
		s.requireFeature("githubWrites", s.requireMember(hime.Handler(s.postPRGitHubReview))))
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
	// Feature epic Phase 2: plan breakdown + per-item session starts.
	mux.Handle("POST /projects/{project}/issues/{n}/plan",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postIssuePlan))))
	mux.Handle("POST /projects/{project}/issues/{n}/items/start",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postIssueItemStart))))
	mux.Handle("POST /projects/{project}/clickup/{id}/fix",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postClickUpFix))))
	mux.Handle("POST /projects/{project}/linear/{identifier}/fix",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postLinearFix))))
	mux.Handle("POST /projects/{project}/errors/{src}/{id}/investigate",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postErrorInvestigate))))
	mux.Handle("POST /projects/{project}/errors/{src}/{id}/fix",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postErrorFix))))
	mux.Handle("POST /projects/{project}/errors/{src}/{id}/resolve",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postErrorResolve))))
	// Address CI / Continue / Address review (PR11b–11c)
	mux.Handle("POST /prs/{owner}/{repo}/{n}/address-ci",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postPRAddressCI))))
	mux.Handle("POST /prs/{owner}/{repo}/{n}/address-review",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postPRAddressReview))))
	// Agentic PR review → one PR comment (never reuses a session, files no issues).
	// Distinct from POST …/reviews, which records a human team-review verdict.
	mux.Handle("POST /prs/{owner}/{repo}/{n}/agent-review",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postPRAgentReview))))
	mux.Handle("POST /prs/{owner}/{repo}/{n}/ask",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postPRAsk))))
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
	mux.Handle("POST /sessions/{threadID}/watch",
		s.requireMember(hime.Handler(s.postSessionWatch)))
	mux.Handle("POST /sessions/{threadID}/unwatch",
		s.requireMember(hime.Handler(s.postSessionUnwatch)))
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
	mux.Handle("POST /sessions/{threadID}/case/link",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCaseLink))))
	mux.Handle("POST /sessions/{threadID}/case/unlink",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCaseUnlink))))
	// Commit review → new Discord/web session; Grok opens issues agentically
	mux.Handle("POST /projects/{project}/commits/{sha}/review",
		s.requireFeature("startSessions", s.requireMember(hime.Handler(s.postCommitReview))))
	mux.Handle("GET /events", s.requireAuth(http.HandlerFunc(s.sse)))
	mux.Handle("GET /partials/home/projects", s.requireAuth(hime.Handler(s.partialHomeProjects)))
	mux.Handle("GET /partials/home/counts", s.requireAuth(hime.Handler(s.partialHomeCounts)))
	mux.Handle("GET /partials/home/runs", s.requireAuth(hime.Handler(s.partialHomeRuns)))
	mux.Handle("GET /partials/projects/pulse", s.requireAuth(hime.Handler(s.partialProjectPulse)))
	mux.Handle("GET /partials/projects/pulse/counts", s.requireAuth(hime.Handler(s.partialProjectPulseCounts)))
	mux.Handle("GET /partials/projects/pulse/runs", s.requireAuth(hime.Handler(s.partialProjectPulseRuns)))
	mux.Handle("GET /partials/ship/stats", s.requireAuth(hime.Handler(s.partialShipStats)))
	mux.Handle("GET /partials/ship/table", s.requireAuth(hime.Handler(s.partialShipTable)))
	mux.Handle("GET /partials/cases/pipeline", s.requireAuth(hime.Handler(s.partialCasesPipeline)))
	mux.Handle("GET /partials/cases/list", s.requireAuth(hime.Handler(s.partialCasesList)))
	mux.Handle("GET /partials/cases/counts", s.requireAuth(hime.Handler(s.partialCasesCounts)))
	mux.Handle("GET /partials/history/table", s.requireAuth(hime.Handler(s.partialHistoryTable)))
	mux.Handle("GET /partials/history/turns/{threadID}", s.requireAuth(hime.Handler(s.partialHistoryTurns)))
	mux.Handle("GET /partials/sessions/{threadID}", s.requireAuth(hime.Handler(s.partialSession)))
	mux.Handle("GET /partials/sessions/{threadID}/run", s.requireAuth(hime.Handler(s.partialSessionRun)))
	mux.Handle("GET /partials/worktrees/table", s.requireAuth(hime.Handler(s.partialWorktreesTable)))
	mux.Handle("GET /partials/deploys/board", s.requireAuth(hime.Handler(s.partialDeploysBoard)))
	mux.Handle("GET /partials/issues/table", s.requireAuth(hime.Handler(s.partialIssuesTable)))
	mux.Handle("GET /partials/nav/counts", s.requireAuth(hime.Handler(s.partialNavCounts)))
	mux.Handle("GET /partials/prs/{owner}/{repo}/{n}/gates", s.requireAuth(hime.Handler(s.partialPRGates)))
	mux.Handle("GET /partials/prs/{owner}/{repo}/{n}/ask", s.requireAuth(hime.Handler(s.partialPRAsk)))
	mux.Handle("GET /partials/prs/{owner}/{repo}/{n}/ask/run", s.requireAuth(hime.Handler(s.partialPRAskRun)))
	mux.Handle("GET /partials/config/lists", s.requireAdmin(hime.Handler(s.partialConfigLists)))
	mux.Handle("GET /partials/config/channels", s.requireAdmin(hime.Handler(s.partialConfigChannels)))

	// Admin + CSRF mutations (no-op gates when auth disabled)
	mux.Handle("POST /worktrees/prune", s.requireAdmin(hime.Handler(s.pruneWorktree)))
	mux.Handle("POST /worktrees/prune-idle", s.requireAdmin(hime.Handler(s.pruneIdleWorktrees)))
	mux.Handle("POST /config/projects", s.requireAdmin(hime.Handler(s.addProject)))
	mux.Handle("POST /config/projects/remove", s.requireAdmin(hime.Handler(s.removeProject)))
	mux.Handle("POST /config/projects/linear", s.requireAdmin(hime.Handler(s.setProjectLinear)))
	mux.Handle("POST /config/projects/clickup", s.requireAdmin(hime.Handler(s.setProjectClickUp)))
	mux.Handle("POST /config/projects/errors-gcp", s.requireAdmin(hime.Handler(s.setProjectErrorsGCP)))
	mux.Handle("POST /config/projects/errors-sentry", s.requireAdmin(hime.Handler(s.setProjectErrorsSentry)))
	mux.Handle("POST /config/projects/errors-deploys", s.requireAdmin(hime.Handler(s.setProjectErrorsDeploys)))
	mux.Handle("POST /config/projects/errors-deploys/generate-token", s.requireAdmin(hime.Handler(s.generateDeploysErrorsToken)))
	mux.Handle("POST /config/projects/github", s.requireAdmin(hime.Handler(s.setProjectGitHub)))
	mux.Handle("POST /config/projects/storage", s.requireAdmin(hime.Handler(s.setProjectStorage)))
	mux.Handle("POST /config/projects/channel", s.requireAdmin(hime.Handler(s.setProjectChannel)))
	mux.Handle("POST /config/projects/fetch", s.requireAdmin(hime.Handler(s.setProjectFetch)))
	mux.Handle("POST /config/projects/ship", s.requireAdmin(hime.Handler(s.setProjectShip)))
	mux.Handle("POST /config/projects/agent-mcp", s.requireAdmin(hime.Handler(s.setProjectAgentMCP)))
	mux.Handle("POST /config/projects/safe-team", s.requireAdmin(hime.Handler(s.setProjectSafeTeam)))
	mux.Handle("POST /config/projects/mode", s.requireAdmin(hime.Handler(s.setProjectMode)))
	mux.Handle("POST /config/projects/case-key", s.requireAdmin(hime.Handler(s.setProjectCaseKey)))
	mux.Handle("POST /config/projects/primary-branch", s.requireAdmin(hime.Handler(s.setProjectPrimaryBranch)))
	mux.Handle("POST /config/projects/cherry-pick-targets", s.requireAdmin(hime.Handler(s.setProjectCherryPickTargets)))
	mux.Handle("POST /config/projects/sla", s.requireAdmin(hime.Handler(s.setProjectSLA)))
	mux.Handle("POST /config/projects/members", s.requireAdmin(hime.Handler(s.addProjectMember)))
	mux.Handle("POST /config/projects/verify", s.requireAdmin(hime.Handler(s.setProjectVerify)))
	mux.Handle("POST /config/projects/verify/generate", s.requireAdmin(hime.Handler(s.generateProjectVerify)))
	mux.Handle("POST /config/projects/capabilities/users", s.requireAdmin(hime.Handler(s.setProjectCapabilityUser)))
	mux.Handle("POST /config/projects/capabilities/users/remove", s.requireAdmin(hime.Handler(s.removeProjectCapabilityUser)))
	mux.Handle("POST /config/projects/teams", s.requireAdmin(hime.Handler(s.setProjectTeam)))
	mux.Handle("POST /config/projects/teams/remove", s.requireAdmin(hime.Handler(s.removeProjectTeam)))
	mux.Handle("POST /config/projects/teams/members", s.requireAdmin(hime.Handler(s.addProjectTeamMember)))
	mux.Handle("POST /config/projects/teams/members/remove", s.requireAdmin(hime.Handler(s.removeProjectTeamMember)))
	mux.Handle("POST /config/guild", s.requireAdmin(hime.Handler(s.setGuild)))
	mux.Handle("POST /config/projects/users", s.requireAdmin(hime.Handler(s.addProjectUser)))
	mux.Handle("POST /config/projects/users/remove", s.requireAdmin(hime.Handler(s.removeProjectUser)))
	mux.Handle("POST /config/channels", s.requireAdmin(hime.Handler(s.addChannel)))
	mux.Handle("POST /config/channels/remove", s.requireAdmin(hime.Handler(s.removeChannel)))
	// Config drill-in pages: the hub keeps grouped rows only; every section
	// lives on a focused sub-page with its own POST handler (no shared
	// "section" dispatcher — each write has a distinct route + audit entry).
	mux.Handle("GET /config/bot", s.requireAdmin(hime.Handler(s.configSubPage("config_bot", "Discord bot"))))
	mux.Handle("GET /config/channels", s.requireAdmin(hime.Handler(s.configSubPage("config_channels", "Channel map"))))
	mux.Handle("GET /config/projects/new", s.requireAdmin(hime.Handler(s.configSubPage("config_project_new", "Add project"))))
	mux.Handle("GET /config/run", s.requireAdmin(hime.Handler(s.configSubPage("config_run", "Run limits"))))
	mux.Handle("GET /config/agent", s.requireAdmin(hime.Handler(s.configSubPage("config_agent", "Coding agent"))))
	mux.Handle("GET /config/worktrees", s.requireAdmin(hime.Handler(s.configSubPage("config_worktrees", "Worktrees"))))
	mux.Handle("GET /config/board", s.requireAdmin(hime.Handler(s.configSubPage("config_board", "Team activity board"))))
	mux.Handle("GET /config/notify", s.requireAdmin(hime.Handler(s.configSubPage("config_notify", "Run notifications"))))
	mux.Handle("GET /config/api-tokens", s.requireTokenAdmin(hime.Handler(s.apiTokensPage)))
	mux.Handle("POST /config/api-tokens", s.requireTokenAdmin(hime.Handler(s.postAPITokenMint)))
	mux.Handle("POST /config/api-tokens/revoke", s.requireTokenAdmin(hime.Handler(s.postAPITokenRevoke)))
	mux.Handle("GET /config/ci", s.requireAdmin(hime.Handler(s.configSubPage("config_ci", "CI triage"))))
	mux.Handle("GET /config/pr-links", s.requireAdmin(hime.Handler(s.configSubPage("config_prlinks", "Discord PR links"))))
	mux.Handle("GET /config/risky", s.requireAdmin(hime.Handler(s.configSubPage("config_risky", "Completion risk paths"))))
	mux.Handle("GET /config/skills", s.requireAdmin(hime.Handler(s.configSkillsPage)))
	mux.Handle("GET /config/model-rates", s.requireAdmin(hime.Handler(s.configSubPage("config_rates", "Model rates"))))
	mux.Handle("POST /config/model-rates", s.requireAdmin(hime.Handler(s.updateModelRates)))
	mux.Handle("POST /config/model-rates/official", s.requireAdmin(hime.Handler(s.fillModelRatesFromOfficial)))
	mux.Handle("GET /config/storage", s.requireAdmin(hime.Handler(s.storageConfigPage)))
	mux.Handle("POST /config/storage", s.requireAdmin(hime.Handler(s.setGlobalStorage)))
	mux.Handle("POST /config/run", s.requireAdmin(hime.Handler(s.updateRunSettings)))
	mux.Handle("POST /config/agent", s.requireAdmin(hime.Handler(s.updateAgentSettings)))
	mux.Handle("POST /config/worktrees", s.requireAdmin(hime.Handler(s.updateWorktreeSettings)))
	mux.Handle("POST /config/board", s.requireAdmin(hime.Handler(s.updateBoardSettings)))
	mux.Handle("POST /config/notify", s.requireAdmin(hime.Handler(s.updateNotifySettings)))
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
	Title          string
	IsDashboard    bool
	IsOverview     bool
	IsHistory      bool
	IsSessions     bool
	IsShip         bool
	IsCases        bool
	IsInbox        bool
	IsWorktrees    bool
	IsConfig       bool
	IsLogin        bool
	IsIssues       bool
	IsLinear       bool
	IsClickUp      bool
	IsErrors       bool
	IsCommits      bool
	IsReviews      bool
	IsStart        bool
	IsDeploys      bool
	IsFiles        bool
	IsActions      bool
	IsSpend        bool
	IsAccount      bool
	APIEnabled     bool
	APITokens      []apitoken.Record
	APITokenSecret string
	APIProjects    []string
	Flash          string
	Error          string
	// ErrorAlertTitle, when set with Error, opens the appAlert modal on the
	// PR page (merge failures, etc.) instead of relying on the flash alone.
	ErrorAlertTitle string
	Status          bot.StatusSnapshot
	Threads         []history.Summary
	// Sessions list filters (global hub + workspace sessions pages).
	SessionFilters sessionFilters
	Thread         history.Thread
	Ship           bot.ShipBoard
	Cases          bot.CaseBoard
	// CasesBoardURL is this board including its current filters, stamped onto
	// each row's session link as ?back= so the detail page's ← crumb returns
	// here instead of to the sessions list (see backlink.go).
	CasesBoardURL string
	Worktrees     []bot.WorktreeInfo
	IdleTTLDays   int
	// Spend rollup (cost report pages). SpendActor is the ?actor= filter as
	// submitted; RatesConfigured is how many models have a price, which is what
	// turns "no dollars anywhere" from a mystery into a setup step.
	Spend           spend.Report
	SpendActor      string
	RatesConfigured int
	// SessionSpend is one session's cost, shown on its detail page.
	SessionSpend spend.Row
	// Search results (/search). Already ACL-filtered and capped — see search.go.
	Search searchResults
	Config config.Snapshot
	// Skills is the host-discovered coding-agent skill list for /config/skills
	// (and the hub count). Read-only filesystem inventory — see internal/skills.
	Skills []skills.Info
	// Per-project settings tabs (/config/projects/{name}[/tab]).
	ProjectItem      config.ProjectItem
	DiscordUserNames map[string]string // Discord user id → display name (best-effort)
	ProjectTab       string            // access | workflow | integrations | danger
	Members          []memberRow       // Access: direct-member roster
	TeamRoster       []teamRosterRow   // Access: teams + their members
	CapMatrix        []capMatrixRow    // Access: role → capability legend
	CapNames         []string          // Access: legend column labels
	// Effective role for roster members without an explicit one (safe team
	// off → builder, on → the project's default template).
	DefaultRoleFallback string
	SSEPath             string
	// Project-first shell scope: NavProject switches the sidebar into
	// workspace mode. URL-derived only (see navScopeFromURL) so history
	// restores can recompute it client-side.
	NavProject        string
	NavProjects       []string // visible projects for the sidebar switcher
	NavLinearEnabled  bool     // workspace nav: show the Linear item
	NavClickUpEnabled bool     // workspace nav: show the ClickUp item
	NavErrorsEnabled  bool     // workspace nav: show the Errors item
	// Home launcher cards.
	ProjectCards []projectCard
	// Auth chrome
	AuthEnabled bool
	IsAdmin     bool // true when auth off, or session role ≥ admin
	CSRF        string
	UserName    string
	UserRole    string
	UserID      string
	UserAvatar  string // provider CDN avatar URL; empty → letter fallback
	LoginNext   string
	// LoginProviders are the login buttons to render — exactly the providers
	// that are fully configured. Empty renders the misconfiguration notice
	// instead of a gate with no way through it.
	LoginProviders []loginProviderView
	// Account page (/account): the logins that resolve to this account, and the
	// configured providers it has none from yet. AccountLinkingOn is false when
	// no identity store is installed, which turns the page into a read-only
	// explanation rather than buttons that would refuse.
	AccountIdentities  []accountIdentityRow
	AccountLinkOptions []accountLinkOption
	AccountLinkingOn   bool
	// AccountHandleStale is true when some GitHub login on the account has a
	// handle too old to be trusted (identity.MaxHandleAge). It gates the note
	// explaining the expiry, which is worth saying exactly when it bites and is
	// noise on an account with no GitHub login at all.
	AccountHandleStale bool
	// Workflow read UI (PR4–7)
	Project             string
	RepoCatalog         []config.GitHubRepoRef
	ActiveOwner         string
	ActiveRepo          string
	IssueState          string
	Issues              []ghpr.IssueInfo
	Issue               ghpr.IssueInfo
	LinearEnabled       bool
	LinearTeam          string
	LinearIssues        []linear.Issue
	LinearIssue         linear.Issue
	ClickUpEnabled      bool
	ClickUpListID       string
	ClickUpTasks        []clickup.Task
	ClickUpTask         clickup.Task
	ErrorSrc            string
	ErrorProviderLabel  string
	ErrorGroups         []errsrc.Group
	ErrorDetail         errsrc.GroupDetail
	ErrorTabs           []ErrorTab
	ErrorGrokBanner     bool
	ErrorGrokBannerCopy string
	ErrorNextCursor     string
	ErrorClipped        bool
	ErrorStatus         string
	ErrorSort           string
	ErrorLocation       string
	ErrorName           string
	// Deploy pipeline (read surface). DeployNotConfigured distinguishes "this
	// project has no .grokwork/deploy.yaml yet" from "the manifest is broken":
	// the first is the normal state and gets instructions, not an error.
	DeployNotConfigured bool
	DeployManifestPath  string
	DeployRef           string
	DeploySHA           string
	DeployShortSHA      string
	DeployEnvs          []string
	DeployRows          []deployRow
	DeployEnabled       bool
	// DeployEnvGated marks environments whose gate is stronger than builder, so
	// the confirm modal can read as dangerous for those and not for dev.
	DeployEnvGated map[string]bool
	DeployRecent   []deploy.Run
	// DeployBoard is the cross-project /deploys lead view (one row per lane).
	DeployBoard deployBoard
	// CanGenerateManifest gates the "write this with an agent" form.
	CanGenerateManifest bool
	// Project Files page. StorageNotConfigured is the empty state when nothing
	// is linked (no global and no override); StorageDisabled is an explicit
	// project opt-out. I/O uses EffectiveStorage. Backend is gcs | gdrive.
	StorageNotConfigured bool
	StorageDisabled      bool
	StorageInherited     bool
	StorageBackend       string // "gcs" | "gdrive"
	StorageBucket        string
	StoragePrefix        string
	StorageDriveFolderID string
	// StorageIsolation is the Drive child-folder name when the project shares
	// the global folder (inherit or same-folder override). Empty otherwise.
	StorageIsolation string
	// StorageInheritCount is for the global /config/storage confirm (count only).
	StorageInheritCount int
	FilesPath           string
	FilesCrumbs         []fileCrumb
	FilesRows           []fileRow
	// FilesFolderOpenURL is the Google Drive link for the folder currently
	// listed (isolation child or nested ?path=). Empty on GCS / empty states.
	FilesFolderOpenURL string
	FilesTotal         int
	FilesClipped       bool
	FilesPreview       *filesPreview
	// Listing summary for the panel header: counts over the rows shown, and
	// the byte total of those files (folders report no size).
	FilesDirCount   int
	FilesFileCount  int
	FilesBytes      int64
	FilesBytesHuman string
	// CanStorageWrite is feature on + SafeOps. StorageFeatureOn is the flag alone
	// so the page can explain why write controls are missing.
	CanStorageWrite  bool
	StorageFeatureOn bool
	// Deploy settings tab.
	DeployCapabilityNames []string
	DeployFeatureOn       bool
	// Deploy run detail + live log fragment.
	DeployRun        *deploy.Run
	DeployLogStep    int
	DeployLogChunk   string
	DeployLogClipped bool
	CanDeploy        bool
	// GitHub Actions page (/projects/{p}/actions).
	ActionsWorkflows      []actionsWorkflowRow
	ActionsRuns           []actionsRunRow
	ActionsWorkflowFilter string
	ActionsBranchFilter   string
	ActionsRun            ghpr.RunDetail
	ActionsRunBucket      string
	ActionsRunShortSHA    string
	ActionsRunLive        bool
	ActionsJobs           []actionsJobRow
	ActionsJobID          int64
	ActionsJobLog         string
	ActionsJobLogClipped  bool
	ActionsJobLogSummary  ghpr.JobLogSummary
	ActionsJobURL         string
	PR                    ghpr.PRDetail
	PRNumber              int
	// PR detail shippability strip (nil when the PR snapshot failed to load).
	PRGates     []prGate
	PRShipReady bool // every gate green → merge affordance opens expanded
	DiffBase    string
	ThreadID    string
	// Session detail "←" crumb. Resolved from ?back= (provenance stamped by
	// the linking board) or from the unit itself; never echoed from the query
	// unvalidated — see backlink.go.
	BackHref  string
	BackLabel string
	// Related cases, outbound then inbound (see bot.RelatedCaseLinks).
	CaseLinks   []bot.CaseLink
	CanLinkCase bool
	// CaseSLA is this case's standing against its project's SLA targets,
	// computed for this render (bot.CaseSLAFor).
	CaseSLA bot.CaseSLA
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
	CommitRangeStart  int
	CommitRangeEnd    int
	CanReviewCommit   bool
	CherryPickTargets []string
	CanCherryPick     bool
	ForcePushTargets  bool
	OpenCherryPickJob *gitworktree.Job
	CherryPickJob     gitworktree.Job
	CherryPickFiles   []cherryPickFileView
	// Write UI flags (from config snapshot + session)
	CanGitHubWrite  bool
	CanMerge        bool
	CanStartSession bool
	CanPRReview     bool
	// CanGitHubReview gates the PR rail's *real* GitHub review action. Narrower
	// than CanGitHubWrite (feature + web role): it also requires the per-project
	// githubWrites capability, the same gate postPRGitHubReview enforces, so the
	// affordance is never offered where the POST would 403.
	CanGitHubReview bool
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
	// IssueSessions is every unit in this project that binds the viewed GitHub
	// issue (bot.FindByIssue with includeTerminal). Terminal sessions are the
	// feature's history — the hub shows them, unlike the fix reuse picker which
	// only offers live units to continue.
	IssueSessions []bot.IssueSessionHit
	// IssueBackURL is this issue detail page (owner/repo query included),
	// stamped onto session row links as ?back= so the session crumb returns
	// here instead of to the sessions list (see backlink.go).
	IssueBackURL string
	// PRSessions is every unit that binds the viewed PR (bot.FindPRSessions),
	// including agent-review units and terminal labels. Distinct from FixHits,
	// which is the Address reuse picker and excludes review-only units.
	PRSessions []bot.IssueSessionHit
	PRBackURL  string
	// PRAskThreadID is this viewer's throwaway in-page ask on this PR, if any.
	// Hidden from /sessions; conversation is embedded on the PR page.
	PRAskThreadID string
	// IssueTasklist is the parsed GitHub tasklist from the issue body (Phase 2
	// breakdown). Empty when the body has no checkbox items.
	IssueTasklist []bot.TasklistItem
	// CanPlanFeature gates "Plan this feature" / per-item Start session; same
	// as CanStartSession (feature+role).
	CanPlanFeature bool
	// TasklistDone/Total are checked/total counts for the progress strip.
	TasklistDone  int
	TasklistTotal int
	// PR detail "Session" head link: the unit to jump to from a PR, and how
	// many bind it (>1 means the link is the most recent, not the only one).
	PRSessionThreadID string
	PRSessionCount    int
	SessionEntry      sessionstore.Entry
	// AgentLabel names the CLI a session runs on ("Grok" / "Claude"), for the
	// session page's reply bubbles and run-status line.
	AgentLabel  string
	DiscordURL  string
	HasSession  bool // live sessionstore entry exists (false after reset)
	HasWorktree bool // session worktree still on disk (enables Worktree diff)
	RunActivity string
	RunPhases   string
	RunElapsed  string
	RunBusy     bool
	RunQueue    int
	// In-flight turn (session detail streaming, mirrors Discord live message).
	RunPrompt      string
	RunLiveText    string
	RunAttachments []history.Attachment
	RunArtifacts   []history.Attachment
	// RunTranscript is the newest run's output from the per-unit timeline. Used
	// as the fallback when a turn has no recorded response (cancelled / max
	// turns), which is the only copy a web-native unit ever had.
	RunTranscript string
	// Completion is the newest recorded completion summary for this unit, if any.
	Completion *bot.CompletionCardInput
	// InboxItems are the viewer's queued notifications (newest first).
	InboxItems InboxItems
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
	// Model picker (start composer + commit/PR dispatch cards). CanSelectModel is
	// the same builder-class gate as CanStartFixMode; ModelGroups holds the curated
	// options grouped by CLI, and ModelDefaultLabel names what "Default" resolves
	// to so the choice on offer is never a mystery.
	CanSelectModel    bool
	ModelGroups       []config.ModelGroup
	ModelDefaultLabel string
	// ModelDefaultLimits is the Default option's harness caveat (the CLI that
	// `def` would run on). ReviewModelDefaultLabel / ReviewModelDefaultLimits
	// are the same pair for a review-model Default on pages that also host a
	// task-model card (issue Fix vs Plan).
	ModelDefaultLimits       string
	ReviewModelDefaultLabel  string
	ReviewModelDefaultLimits string
	// Case intake (/projects/{project}/cases/new + board CTAs): Discord /case
	// parity — startSessions feature+role AND investigator-class capability.
	CanOpenCase bool
	// Issue create (/projects/{project}/issues/new + list CTA): githubWrites
	// feature+role (CanGitHubWrite) AND per-project GithubWrites capability.
	CanCreateIssue bool
	// IssueKind is the kind radio prefill on the new-issue form ("feature"|"bug").
	IssueKind string
	// Session case panel affordances (Mode=case only).
	// Escalate/answer hide on fixing|shipping (eng phases). Agent investigate
	// runs use the docked composer (continue), not a separate rail form.
	CanCaseEscalate bool
	CanCaseDraft    bool // customer-update (open cases)
	CanCaseAnswer   bool // knowledge-path answer; not shown on eng phases
	CanCaseClose    bool // owner/co/admin
	CanCaseReopen   bool // closed cases only; investigator-class or session control
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
		d.NavClickUpEnabled = s.cfg.ProjectClickUpEnabled(d.NavProject)
		d.NavErrorsEnabled = s.cfg.ProjectErrorsAnyEnabled(d.NavProject)
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
	threads = dropPRAskRows(threads)
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
	threads = dropPRAskRows(threads)
	annotateSessionRunning(threads, s.bot)
	f := parseSessionFilters(ctx, true)
	f.Projects = s.filterProjectNames(ctx)
	f.Total = len(threads)
	d := s.basePage(ctx)
	d.Title = "Sessions"
	d.IsSessions = true
	d.Threads = filterSessionRows(threads, f, time.Now())
	d.SessionFilters = f
	d.Flash = strings.TrimSpace(ctx.FormValue("ok"))
	if e := strings.TrimSpace(ctx.FormValue("err")); e != "" {
		d.Error = e
	}
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
	d.AgentLabel = s.agentLabel(threadID)
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
	d.Skills = s.listInstalledSkills()
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	return s.viewPage(ctx, "config", d)
}

// configSkillsPage lists coding-agent skills discovered on the host.
func (s *Server) configSkillsPage(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Skills · Config"
	d.IsConfig = true
	d.Config = s.cfg.Snapshot()
	d.Skills = s.listInstalledSkills()
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	return s.viewPage(ctx, "config_skills", d)
}

// listInstalledSkills inventories user/bundled/project skill packages the
// coding CLIs typically load. Pure filesystem read — no agent exec.
func (s *Server) listInstalledSkills() []skills.Info {
	projects := map[string]string{}
	if s.cfg != nil {
		for _, p := range s.cfg.Snapshot().Projects {
			if p.Name == "" || p.Path == "" {
				continue
			}
			projects[p.Name] = p.Path
		}
	}
	return skills.List(skills.ListOpts{Projects: projects})
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
	threads = dropPRAskRows(threads)
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
	d.AgentLabel = s.agentLabel(threadID)
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
// the hub's data shape: a fresh config snapshot plus flash/err from the query.
func (s *Server) configSubPage(tmpl, title string) func(*hime.Context) error {
	return func(ctx *hime.Context) error {
		d := s.basePage(ctx)
		d.Title = title + " · Config"
		d.IsConfig = true
		d.Config = s.cfg.Snapshot()
		d.Flash = ctx.FormValue("ok")
		d.Error = ctx.FormValue("err")
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

func (s *Server) setProjectClickUp(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	enabled := ctx.PostFormValue("enabled") == "1" || strings.EqualFold(ctx.PostFormValue("enabled"), "on")
	clearKey := ctx.PostFormValue("clearApiKey") == "1" || strings.EqualFold(ctx.PostFormValue("clearApiKey"), "on")
	workspaceID := ctx.PostFormValue("workspaceId")
	listID := ctx.PostFormValue("listId")
	prefix := ctx.PostFormValue("customIdPrefix")
	apiKey := ctx.PostFormValue("apiKey")
	err := s.cfg.SetProjectClickUp(name, enabled, workspaceID, listID, prefix, apiKey, clearKey)
	s.auditAction(ctx, audit.ActionConfigSetClickUp, err, map[string]any{"name": name, "enabled": enabled})
	return s.projectConfigTabRedirect(ctx, name, "integrations", fmt.Sprintf("Updated ClickUp for project %q", name), err)
}

func (s *Server) setProjectErrorsGCP(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	enabled := ctx.PostFormValue("enabled") == "1" || strings.EqualFold(ctx.PostFormValue("enabled"), "on")
	err := s.cfg.SetProjectErrorsGCP(name, enabled,
		ctx.PostFormValue("projectId"),
		ctx.PostFormValue("projectNumber"),
		ctx.PostFormValue("credentialsFile"),
		ctx.PostFormValue("service"),
	)
	s.auditAction(ctx, audit.ActionConfigSetErrorsGCP, err, map[string]any{
		"name": name, "enabled": enabled, "projectId": strings.TrimSpace(ctx.PostFormValue("projectId")),
	})
	return s.projectConfigTabRedirect(ctx, name, "integrations", fmt.Sprintf("Updated GCP Error Reporting for project %q", name), err)
}

func (s *Server) setProjectErrorsSentry(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	enabled := ctx.PostFormValue("enabled") == "1" || strings.EqualFold(ctx.PostFormValue("enabled"), "on")
	clearKey := ctx.PostFormValue("clearAuthToken") == "1" || strings.EqualFold(ctx.PostFormValue("clearAuthToken"), "on")
	err := s.cfg.SetProjectErrorsSentry(name, enabled,
		ctx.PostFormValue("org"),
		ctx.PostFormValue("project"),
		ctx.PostFormValue("authToken"),
		ctx.PostFormValue("baseURL"),
		clearKey,
	)
	s.auditAction(ctx, audit.ActionConfigSetErrorsSentry, err, map[string]any{
		"name": name, "enabled": enabled, "org": strings.TrimSpace(ctx.PostFormValue("org")),
	})
	return s.projectConfigTabRedirect(ctx, name, "integrations", fmt.Sprintf("Updated Sentry for project %q", name), err)
}

func (s *Server) setProjectErrorsDeploys(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	enabled := ctx.PostFormValue("enabled") == "1" || strings.EqualFold(ctx.PostFormValue("enabled"), "on")
	clearKey := ctx.PostFormValue("clearApiToken") == "1" || strings.EqualFold(ctx.PostFormValue("clearApiToken"), "on")
	err := s.cfg.SetProjectErrorsDeploys(name, enabled,
		ctx.PostFormValue("project"),
		ctx.PostFormValue("location"),
		ctx.PostFormValue("deployment"),
		ctx.PostFormValue("apiToken"),
		clearKey,
	)
	s.auditAction(ctx, audit.ActionConfigSetErrorsDeploys, err, map[string]any{
		"name": name, "enabled": enabled, "deploysProject": strings.TrimSpace(ctx.PostFormValue("project")),
	})
	return s.projectConfigTabRedirect(ctx, name, "integrations", fmt.Sprintf("Updated deploys.app errors for project %q", name), err)
}

func (s *Server) generateDeploysErrorsToken(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	enabled := ctx.PostFormValue("enabled") == "1" || strings.EqualFold(ctx.PostFormValue("enabled"), "on")
	project := strings.TrimSpace(ctx.PostFormValue("project"))
	location := ctx.PostFormValue("location")
	deployment := ctx.PostFormValue("deployment")
	fail := func(err error) error {
		s.auditAction(ctx, audit.ActionConfigGenerateErrorsDeploysToken, err, map[string]any{
			"name": name, "enabled": enabled, "deploysProject": project,
		})
		return s.projectConfigTabRedirect(ctx, name, "integrations", "", err)
	}
	if name == "" {
		return fail(fmt.Errorf("project name is required"))
	}
	if _, ok := s.cfg.ProjectPath(name); !ok {
		return fail(fmt.Errorf("project %q not found", name))
	}
	tok, err := deploys.GenerateToken(ctx.Context(), s.deploysCLI, project)
	if err != nil {
		return fail(err)
	}
	err = s.cfg.SetProjectErrorsDeploys(name, enabled, project, location, deployment, tok.Value, false)
	detail := map[string]any{
		"name": name, "enabled": enabled, "deploysProject": project,
	}
	if !tok.ExpiresAt.IsZero() {
		detail["expiresAt"] = tok.ExpiresAt.UTC().Format(time.RFC3339)
	}
	s.auditAction(ctx, audit.ActionConfigGenerateErrorsDeploysToken, err, detail)
	if err != nil {
		return s.projectConfigTabRedirect(ctx, name, "integrations", "", err)
	}
	msg := fmt.Sprintf("Minted a 1-year deploys.app token for %q", project)
	if !tok.ExpiresAt.IsZero() {
		msg += " (expires " + tok.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC") + ")"
	}
	return s.projectConfigTabRedirect(ctx, name, "integrations", msg, nil)
}

func (s *Server) setProjectActionsRule(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	rule := config.ActionsDispatchRule{
		Repo:     strings.TrimSpace(ctx.PostFormValue("repo")),
		Workflow: strings.TrimSpace(ctx.PostFormValue("workflow")),
		Branches: splitCommaList(ctx.PostFormValue("branches")),
	}
	err := s.cfg.SetProjectActionsRule(name, rule)
	s.auditAction(ctx, "config.set_project_actions_rule", err, map[string]any{
		"name": name, "workflow": rule.Workflow, "repo": rule.Repo, "branches": len(rule.Branches),
	})
	if err != nil {
		return s.projectConfigTabRedirect(ctx, name, "integrations", "", err)
	}
	return s.projectConfigTabRedirect(ctx, name, "integrations",
		fmt.Sprintf("Saved Actions branch lock for %q", rule.Workflow), nil)
}

func (s *Server) removeProjectActionsRule(ctx *hime.Context) error {
	name := strings.TrimSpace(ctx.PostFormValue("name"))
	repo := strings.TrimSpace(ctx.PostFormValue("repo"))
	workflow := strings.TrimSpace(ctx.PostFormValue("workflow"))
	err := s.cfg.RemoveProjectActionsRule(name, repo, workflow)
	s.auditAction(ctx, "config.remove_project_actions_rule", err, map[string]any{
		"name": name, "workflow": workflow, "repo": repo,
	})
	if err != nil {
		return s.projectConfigTabRedirect(ctx, name, "integrations", "", err)
	}
	return s.projectConfigTabRedirect(ctx, name, "integrations",
		fmt.Sprintf("Removed Actions branch lock for %q", workflow), nil)
}

// splitCommaList splits a comma-separated form field into trimmed non-empty parts.
func splitCommaList(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (s *Server) setProjectGitHub(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	text := ctx.PostFormValue("repos")
	var repos []config.GitHubRepoRef
	for line := range strings.SplitSeq(text, "\n") {
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

func (s *Server) setProjectAgentMCP(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	enabled := ctx.PostFormValue("agentMCPAlways") == "1"
	err := s.cfg.SetProjectAgentMCPAlways(name, enabled)
	s.auditAction(ctx, "config.set_project_agent_mcp", err, map[string]any{
		"name": name, "agentMCPAlways": enabled,
	})
	msg := fmt.Sprintf("Updated grokwork MCP attach for project %q (default)", name)
	if enabled {
		msg = fmt.Sprintf("Updated grokwork MCP attach for project %q (always attach)", name)
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
	msg := fmt.Sprintf("Team policy for %q: trusted", name)
	if enabled {
		msg = fmt.Sprintf("Team policy for %q: role-based", name)
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

// setProjectCaseKey saves the Workflow tab's case-key prefix override. It only
// affects keys minted after the save — cases already numbered keep their ids,
// because those ids are what other cases and commits point at.
func (s *Server) setProjectCaseKey(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	key := ctx.PostFormValue("caseKey")
	err := s.cfg.SetProjectCaseKey(name, key)
	s.auditAction(ctx, "config.set_project_case_key", err, map[string]any{
		"name": name, "caseKey": key,
	})
	return s.projectConfigTabRedirect(ctx, name, "workflow", fmt.Sprintf("Updated case id prefix for project %q", name), err)
}

// setProjectPrimaryBranch saves the Workflow tab's primary branch override.
func (s *Server) setProjectPrimaryBranch(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	branch := ctx.PostFormValue("primaryBranch")
	err := s.cfg.SetProjectPrimaryBranch(name, branch)
	s.auditAction(ctx, audit.ActionConfigSetProjectPrimaryBranch, err, map[string]any{
		"name": name, "primaryBranch": branch,
	})
	msg := fmt.Sprintf("Updated primary branch for project %q", name)
	if strings.TrimSpace(branch) == "" {
		msg = fmt.Sprintf("Cleared primary branch override for project %q (using origin/HEAD heuristic)", name)
	}
	return s.projectConfigTabRedirect(ctx, name, "workflow", msg, err)
}

func (s *Server) setProjectCherryPickTargets(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	var lines []string
	for line := range strings.SplitSeq(ctx.PostFormValue("cherryPickTargets"), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	force := ctx.PostFormValue("forcePushTargets") == "1"
	err := s.cfg.SetProjectCherryPickConfig(name, lines, force)
	s.auditAction(ctx, audit.ActionConfigSetProjectCherryPickTargets, err, map[string]any{
		"name": name, "n": len(lines), "forcePush": force,
	})
	msg := fmt.Sprintf("Updated target branches for project %q", name)
	if len(lines) == 0 {
		msg = fmt.Sprintf("Cleared target branches for project %q", name)
	}
	return s.projectConfigTabRedirect(ctx, name, "workflow", msg, err)
}

// setProjectSLA saves the Workflow tab's per-severity case deadlines. One form
// covers every severity, so a save is a full replacement: an emptied box clears
// that target rather than leaving the old value behind, which is the only way to
// turn an SLA back off from the UI.
func (s *Server) setProjectSLA(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	targets := make(map[string]config.SLATarget, len(config.SLASeverities))
	for _, sev := range config.SLASeverities {
		first, err := config.ParseSLAMinutes(ctx.PostFormValue("first_" + sev))
		if err != nil {
			return s.projectConfigTabRedirect(ctx, name, "workflow", "", fmt.Errorf("%s first response: %w", sev, err))
		}
		resolution, err := config.ParseSLAMinutes(ctx.PostFormValue("resolution_" + sev))
		if err != nil {
			return s.projectConfigTabRedirect(ctx, name, "workflow", "", fmt.Errorf("%s resolution: %w", sev, err))
		}
		if first == nil && resolution == nil {
			continue
		}
		targets[sev] = config.SLATarget{FirstResponseMinutes: first, ResolutionMinutes: resolution}
	}
	err := s.cfg.SetProjectSLA(name, targets)
	// Minutes are policy, not customer data — log what was set so a "why did this
	// case go red" question has an answer.
	s.auditAction(ctx, "config.set_project_sla", err, map[string]any{
		"name": name, "severities": len(targets),
	})
	msg := fmt.Sprintf("Updated SLA targets for project %q", name)
	if len(targets) == 0 {
		msg = fmt.Sprintf("Cleared SLA targets for project %q", name)
	}
	return s.projectConfigTabRedirect(ctx, name, "workflow", msg, err)
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

	suggest := s.suggestVerify
	if suggest == nil {
		suggest = grokrun.SuggestVerifyCommands
	}
	hooks := &grokrun.SuggestStreamHooks{
		OnTextDelta: func(delta string) { _ = stream.TextDelta(delta) },
		OnThought:   func(delta string) { _ = stream.ThoughtDelta(delta) },
		OnActivity:  func(line string) { _ = stream.Activity(line) },
	}
	// Verify suggestion runs on the host default agent: it is not tied to a session.
	raw, err := suggest(ctx.Context(), s.cfg.ResolveAgentCLI("").CLI(), path, 3*time.Minute, hooks)
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

// setProjectTeam creates a team or edits an existing one's label / capability
// template. Members are managed by their own routes so the roster's per-member
// × does not have to round-trip the whole team.
func (s *Server) setProjectTeam(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	key := ctx.PostFormValue("key")
	label := ctx.PostFormValue("label")
	caps := ctx.PostFormValue("capabilities")
	err := s.cfg.SetProjectTeam(name, key, label, caps)
	s.auditAction(ctx, audit.ActionConfigSetTeam, err, map[string]any{
		"project": name, "key": key, "label": label, "capabilities": caps,
	})
	msg := fmt.Sprintf("Saved team %s with no role — members fall back to the default", key)
	if strings.TrimSpace(caps) != "" {
		msg = fmt.Sprintf("Saved team %s as %s", key, caps)
	}
	return s.projectConfigRedirect(ctx, name, msg, err)
}

func (s *Server) removeProjectTeam(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	key := ctx.PostFormValue("key")
	err := s.cfg.RemoveProjectTeam(name, key)
	s.auditAction(ctx, audit.ActionConfigRemoveTeam, err, map[string]any{
		"project": name, "key": key,
	})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Removed team %s and every grant it carried", key), err)
}

func (s *Server) addProjectTeamMember(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	key := ctx.PostFormValue("key")
	id := ctx.PostFormValue("id")
	err := s.cfg.AddProjectTeamMember(name, key, id)
	s.auditAction(ctx, audit.ActionConfigAddTeamMember, err, map[string]any{
		"project": name, "key": key, "id": id,
	})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Added %s to team %s", id, key), err)
}

func (s *Server) removeProjectTeamMember(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	key := ctx.PostFormValue("key")
	id := ctx.PostFormValue("id")
	err := s.cfg.RemoveProjectTeamMember(name, key, id)
	s.auditAction(ctx, audit.ActionConfigRemoveTeamMember, err, map[string]any{
		"project": name, "key": key, "id": id,
	})
	return s.projectConfigRedirect(ctx, name, fmt.Sprintf("Removed %s from team %s", id, key), err)
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

// addProjectMember adds one direct member in a single post: allowlist entry
// plus an optional explicit role (capability template). "Direct" means not via
// a team — a per-person grant that no other project shares.
func (s *Server) addProjectMember(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	id := ctx.PostFormValue("id")
	tpl := strings.TrimSpace(ctx.PostFormValue("template"))
	err := s.cfg.AddProjectAllowedUser(name, id)
	if err == nil && tpl != "" {
		err = s.cfg.SetProjectCapabilityByUser(name, id, tpl)
	}
	s.auditAction(ctx, "config.add_project_member", err, map[string]any{
		"project": name, "id": id, "template": tpl,
	})
	msg := fmt.Sprintf("Added %s", id)
	if tpl != "" {
		msg = fmt.Sprintf("Added %s as %s", id, tpl)
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

// configPageRedirect sends a config write back to the page it came from
// (each section's own drill-in page) with a flash or error in the query.
func (s *Server) configPageRedirect(ctx *hime.Context, routeName, okMsg string, err error) error {
	if err != nil {
		return ctx.RedirectTo(routeName, map[string]string{"err": err.Error()})
	}
	return ctx.RedirectTo(routeName, map[string]string{"ok": okMsg})
}

// updateAgentSettings persists the default coding CLI and its claude-side
// binary/model. Sessions already stamped with an agent are unaffected.
func (s *Server) updateAgentSettings(ctx *hime.Context) error {
	in := config.AgentSettings{
		Agent:          strings.TrimSpace(ctx.PostFormValue("agent")),
		Model:          strings.TrimSpace(ctx.PostFormValue("model")),
		SummarizeModel: strings.TrimSpace(ctx.PostFormValue("summarizeModel")),
		ReviewModel:    strings.TrimSpace(ctx.PostFormValue("reviewModel")),
		GrokBin:        strings.TrimSpace(ctx.PostFormValue("grokBin")),
		ClaudeBin:      strings.TrimSpace(ctx.PostFormValue("claudeBin")),
		CursorBin:      strings.TrimSpace(ctx.PostFormValue("cursorBin")),
		IncludeAnthropicEnv: ctx.PostFormValue("claudeIncludeAnthropicEnv") == "1" ||
			strings.EqualFold(ctx.PostFormValue("claudeIncludeAnthropicEnv"), "on"),
	}

	err := s.cfg.SetAgentSettings(in)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "agent"})
	if err != nil {
		return s.configPageRedirect(ctx, "config.agent", "", err)
	}
	msg := fmt.Sprintf("Default agent: %s", s.cfg.DefaultAgent().Label())
	if model, ok := s.cfg.ModelAgent(); ok {
		msg = fmt.Sprintf("Model %q runs on %s", in.Model, model.Label())
	}
	return s.configPageRedirect(ctx, "config.agent", msg, nil)
}

func (s *Server) updateRunSettings(ctx *hime.Context) error {
	err := s.updateRunSettingsErr(ctx)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "run"})
	if err != nil {
		return s.configPageRedirect(ctx, "config.run", "", err)
	}
	maxTurns, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("maxTurns")))
	timeoutMs, _ := parseTimeoutMs(ctx.PostFormValue("timeoutMs"))
	msg := fmt.Sprintf("Run limits: maxTurns=%d, timeout=%s", maxTurns, formatMsDur(timeoutMs))
	if maxConcurrent := s.cfg.MaxConcurrentRunsValue(); maxConcurrent > 0 {
		msg += fmt.Sprintf(", host cap=%d", maxConcurrent)
	} else {
		msg += ", host cap=unlimited"
	}
	if maxConcurrentUser := s.cfg.MaxConcurrentRunsUserValue(); maxConcurrentUser > 0 {
		msg += fmt.Sprintf(", per-user cap=%d", maxConcurrentUser)
	} else {
		msg += ", per-user cap=unlimited"
	}
	return s.configPageRedirect(ctx, "config.run", msg, nil)
}

func (s *Server) updateWorktreeSettings(ctx *hime.Context) error {
	err := s.updateWorktreeSettingsErr(ctx)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "worktree"})
	if err != nil {
		return s.configPageRedirect(ctx, "config.worktrees", "", err)
	}
	days, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("worktreeIdleTTLDays")))
	termDays, _ := strconv.Atoi(strings.TrimSpace(ctx.PostFormValue("terminalSessionTTLDays")))
	worktreeDir := strings.TrimSpace(ctx.PostFormValue("worktreeDir"))
	msg := fmt.Sprintf("Worktree idle TTL %d day(s)", days)
	if days == 0 {
		msg = "Worktree idle cleanup disabled"
	}
	if termDays == 0 {
		msg += "; done/abandoned session cleanup disabled"
	} else {
		msg += fmt.Sprintf("; done/abandoned TTL %d day(s)", termDays)
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

func (s *Server) updateNotifySettings(ctx *hime.Context) error {
	mode := strings.TrimSpace(ctx.PostFormValue("notifyOnDone"))
	longMs := 0
	if raw := strings.TrimSpace(ctx.PostFormValue("notifyOnDoneLongMs")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			err = fmt.Errorf("notifyOnDoneLongMs must be an integer")
			s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{"section": "notify"})
			return s.configPageRedirect(ctx, "config.notify", "", err)
		}
		longMs = n
	}
	err := s.cfg.SetNotifyOnDone(mode, longMs)
	s.auditAction(ctx, audit.ActionConfigSettings, err, map[string]any{
		"section": "notify", "notifyOnDone": mode, "longMs": longMs,
	})
	if err != nil {
		return s.configPageRedirect(ctx, "config.notify", "", err)
	}
	msg := fmt.Sprintf("Author notify policy: %s", s.cfg.NotifyOnDoneValue())
	if s.cfg.NotifyOnDoneValue() == config.NotifyOnDoneLongOnly {
		msg += fmt.Sprintf(" (threshold %d ms)", s.cfg.NotifyOnDoneLongMsValue())
	}
	return s.configPageRedirect(ctx, "config.notify", msg, nil)
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
		for line := range strings.SplitSeq(ctx.PostFormValue("riskyPathGlobs"), "\n") {
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

// parseOptionalCap parses a tri-state concurrency-cap form field: an empty
// string or "0" means unlimited (nil), a negative number is an error, and a
// positive number is stored as-is. Never returns a pointer to 0 — callers
// that persist the result must be able to tell "unset" from "explicit zero"
// via nil alone.
func parseOptionalCap(raw string) (*int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("must be a whole number")
	}
	if n < 0 {
		return nil, fmt.Errorf("must be >= 0")
	}
	if n == 0 {
		return nil, nil
	}
	return &n, nil
}

func (s *Server) updateRunSettingsErr(ctx *hime.Context) error {
	rawTurns := strings.TrimSpace(ctx.PostFormValue("maxTurns"))
	if rawTurns == "" {
		return fmt.Errorf("maxTurns is required")
	}
	maxTurns, err := strconv.Atoi(rawTurns)
	if err != nil {
		return fmt.Errorf("maxTurns must be an integer")
	}
	timeoutMs, err := parseTimeoutMs(ctx.PostFormValue("timeoutMs"))
	if err != nil {
		return err
	}
	// Parse (and validate) every field before persisting anything: maxTurns/
	// timeoutMs and the two concurrency caps are one form and one "Save", so a
	// bad concurrency value must not leave maxTurns/timeoutMs half-saved.
	maxConcurrent, err := parseOptionalCap(ctx.PostFormValue("maxConcurrentRuns"))
	if err != nil {
		return fmt.Errorf("maxConcurrentRuns %v", err)
	}
	maxConcurrentUser, err := parseOptionalCap(ctx.PostFormValue("maxConcurrentRunsUser"))
	if err != nil {
		return fmt.Errorf("maxConcurrentRunsUser %v", err)
	}
	if err := s.cfg.SetGrokRunLimits(maxTurns, timeoutMs); err != nil {
		return err
	}
	return s.cfg.SetConcurrencyLimits(maxConcurrent, maxConcurrentUser)
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
	rawTerm := strings.TrimSpace(ctx.PostFormValue("terminalSessionTTLDays"))
	if rawTerm == "" {
		return fmt.Errorf("terminalSessionTTLDays is required")
	}
	termDays, err := strconv.Atoi(rawTerm)
	if err != nil {
		return fmt.Errorf("terminalSessionTTLDays must be an integer")
	}
	worktreeDir := strings.TrimSpace(ctx.PostFormValue("worktreeDir"))
	return s.cfg.SetWorktreeSettings(days, worktreeDir, termDays)
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

// annotateSessionRunning marks rows whose thread has an in-memory active run.
// Lifecycle Label "in_progress" is sticky after a turn ends; Running is the
// live agent job (StatusSnapshot), so a Claude (or Grok) session that is
// actually working shows a distinct "running" badge on the list.
func annotateSessionRunning(threads []history.Summary, b *bot.Bot) {
	if b == nil || len(threads) == 0 {
		return
	}
	snap := b.StatusSnapshot()
	if snap.ActiveCount == 0 {
		return
	}
	busy := make(map[string]struct{}, snap.ActiveCount)
	for _, r := range snap.ActiveRuns {
		if r.ThreadID != "" {
			busy[r.ThreadID] = struct{}{}
		}
	}
	for i := range threads {
		if _, ok := busy[threads[i].ThreadID]; ok {
			threads[i].Running = true
		}
	}
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
	// Sticky goal is list identity; cases fall back to customer title.
	goal := strings.TrimSpace(e.Goal)
	if goal == "" && e.IsCase() {
		goal = strings.TrimSpace(e.CustomerTitle)
	}
	row.Goal = goal
	row.Label = e.EffectiveLabel()
	row.Mode = strings.TrimSpace(e.Mode)
	row.SessionKind = strings.TrimSpace(e.SessionKind)
	row.Phase = e.CasePhase()
	row.Resolution = strings.TrimSpace(e.Resolution)
	row.HasPRs = e.HasAnyPR()
	row.AllPRsTerminal = e.AllPRsTerminal()
	if pr, ok := e.PrimaryPR(); ok {
		row.PRNumber = pr.Number
		row.PRState = strings.ToUpper(strings.TrimSpace(pr.State))
		row.PROwner = pr.Owner
		row.PRRepo = pr.Repo
		row.PRURL = pr.URL
		row.PRTitle = pr.Title
	}
}

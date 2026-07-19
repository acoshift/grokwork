package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grok-discord/internal/bot"
	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/history"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

//go:embed templates/*
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server is the private-network admin UI.
type Server struct {
	cfg      *config.Config
	sessions *sessionstore.Store
	history  *history.Store
	bot      *bot.Bot
	app      *hime.App
}

// New builds a hime app with dashboard, history, config, and SSE routes.
func New(cfg *config.Config, sessions *sessionstore.Store, hist *history.Store, b *bot.Bot) *Server {
	s := &Server{cfg: cfg, sessions: sessions, history: hist, bot: b}
	app := hime.New()
	app.Address(cfg.ListenAddr())
	// SSE needs an unbounded write timeout; page requests finish quickly.
	app.Server().WriteTimeout = 0
	app.Server().ReadTimeout = 15 * time.Second
	app.Server().IdleTimeout = 120 * time.Second
	// Do not sleep before stop, and do not wait for open SSE streams on exit.
	// (GraceTimeout==0 would use context.Background and hang until all conns end.)
	app.Server().WaitBeforeShutdown = 0
	app.Server().GraceTimeout = time.Millisecond

	app.Routes(hime.Routes{
		"dashboard":            "/",
		"history":              "/history",
		"history.thread":       "/history/",
		"ship":                 "/ship",
		"worktrees":            "/worktrees",
		"worktrees.prune":      "/worktrees/prune",
		"worktrees.pruneIdle":  "/worktrees/prune-idle",
		"config":               "/config",
		"config.addProject":    "/config/projects",
		"config.removeProject": "/config/projects/remove",
		"config.addUser":       "/config/users",
		"config.removeUser":    "/config/users/remove",
		"config.addRole":       "/config/roles",
		"config.removeRole":    "/config/roles/remove",
		"config.addChannel":    "/config/channels",
		"config.removeChannel": "/config/channels/remove",
		"config.settings":      "/config/settings",
		"sse":                  "/events",
	})

	app.TemplateFunc("add", func(a, b int) int { return a + b })

	// Full pages (layout root).
	tp := app.Template()
	tp.FS(templateFS)
	tp.Dir("templates")
	tp.Root("layout")
	tp.ParseFiles("dashboard", "layout.tmpl", "dashboard.tmpl")
	tp.ParseFiles("history", "layout.tmpl", "history.tmpl")
	tp.ParseFiles("history_detail", "layout.tmpl", "history_detail.tmpl")
	tp.ParseFiles("ship", "layout.tmpl", "ship.tmpl")
	tp.ParseFiles("worktrees", "layout.tmpl", "worktrees.tmpl")
	tp.ParseFiles("config", "layout.tmpl", "config.tmpl")

	// Content-only fragments for htmx live swaps (HX-Request).
	// Same page templates, root is the "content" define — no layout/CSS/nav.
	frag := app.Template()
	frag.FS(templateFS)
	frag.Dir("templates")
	frag.Root("content")
	frag.ParseFiles("dashboard_frag", "dashboard.tmpl")
	frag.ParseFiles("history_frag", "history.tmpl")
	frag.ParseFiles("history_detail_frag", "history_detail.tmpl")
	frag.ParseFiles("ship_frag", "ship.tmpl")
	frag.ParseFiles("worktrees_frag", "worktrees.tmpl")
	frag.ParseFiles("config_frag", "config.tmpl")

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: static fs: " + err.Error())
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.Handle("GET /{$}", hime.Handler(s.dashboard))
	mux.Handle("GET /history", hime.Handler(s.historyList))
	mux.Handle("GET /history/{threadID}", hime.Handler(s.historyDetail))
	mux.Handle("GET /ship", hime.Handler(s.shipPage))
	mux.Handle("GET /worktrees", hime.Handler(s.worktreesPage))
	mux.Handle("POST /worktrees/prune", hime.Handler(s.pruneWorktree))
	mux.Handle("POST /worktrees/prune-idle", hime.Handler(s.pruneIdleWorktrees))
	mux.Handle("GET /config", hime.Handler(s.configPage))
	mux.Handle("POST /config/projects", hime.Handler(s.addProject))
	mux.Handle("POST /config/projects/remove", hime.Handler(s.removeProject))
	mux.Handle("POST /config/users", hime.Handler(s.addUser))
	mux.Handle("POST /config/users/remove", hime.Handler(s.removeUser))
	mux.Handle("POST /config/roles", hime.Handler(s.addRole))
	mux.Handle("POST /config/roles/remove", hime.Handler(s.removeRole))
	mux.Handle("POST /config/channels", hime.Handler(s.addChannel))
	mux.Handle("POST /config/channels/remove", hime.Handler(s.removeChannel))
	mux.Handle("POST /config/settings", hime.Handler(s.updateSettings))
	mux.Handle("GET /events", http.HandlerFunc(s.sse))

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
	IsHistory   bool
	IsShip      bool
	IsWorktrees bool
	IsConfig    bool
	Flash       string
	Error       string
	Status      bot.StatusSnapshot
	Threads     []history.Summary
	Thread      history.Thread
	Ship        bot.ShipBoard
	Worktrees   []bot.WorktreeInfo
	IdleTTLDays int
	Config      config.Snapshot
	SSEPath     string
	// LivePath is the path+query used by htmx to re-fetch this page's content fragment.
	LivePath string
}

func isHTMX(ctx *hime.Context) bool {
	return ctx.Request.Header.Get("HX-Request") == "true"
}

func (s *Server) basePage(ctx *hime.Context) pageData {
	return pageData{
		SSEPath:  ctx.Route("sse"),
		LivePath: ctx.Request.URL.RequestURI(),
	}
}

// renderPage serves the full layout, or only the content fragment when htmx
// requests a live swap (HX-Request).
func (s *Server) renderPage(ctx *hime.Context, name string, data pageData) error {
	if isHTMX(ctx) {
		return ctx.View(name+"_frag", data)
	}
	return ctx.View(name, data)
}

func (s *Server) dashboard(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Dashboard"
	d.IsDashboard = true
	d.Status = s.bot.StatusSnapshot()
	return s.renderPage(ctx, "dashboard", d)
}

func (s *Server) historyList(ctx *hime.Context) error {
	threads, err := s.history.List()
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error("history list: " + err.Error())
	}
	// Also surface sessions that have no turns yet (legacy / mid-run).
	threads = mergeSessionRows(threads, s.sessions.List())
	d := s.basePage(ctx)
	d.Title = "History"
	d.IsHistory = true
	d.Threads = threads
	return s.renderPage(ctx, "history", d)
}

func (s *Server) historyDetail(ctx *hime.Context) error {
	threadID := ctx.PathValue("threadID")
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
	d.IsHistory = true
	d.Thread = th
	return s.renderPage(ctx, "history_detail", d)
}

func (s *Server) shipPage(ctx *hime.Context) error {
	project := strings.TrimSpace(ctx.FormValue("project"))
	state := strings.TrimSpace(ctx.FormValue("state"))
	d := s.basePage(ctx)
	d.Title = "Ship board"
	d.IsShip = true
	d.Ship = s.bot.ListShipBoard(project, state)
	return s.renderPage(ctx, "ship", d)
}

func (s *Server) configPage(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Config"
	d.IsConfig = true
	d.Config = s.cfg.Snapshot()
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	return s.renderPage(ctx, "config", d)
}

func (s *Server) worktreesPage(ctx *hime.Context) error {
	d := s.basePage(ctx)
	d.Title = "Worktrees"
	d.IsWorktrees = true
	d.Worktrees = s.bot.ListWorktrees()
	d.IdleTTLDays = s.cfg.WorktreeIdleTTLDaysValue()
	d.Flash = ctx.FormValue("ok")
	d.Error = ctx.FormValue("err")
	return s.renderPage(ctx, "worktrees", d)
}

func (s *Server) worktreesRedirect(ctx *hime.Context, okMsg string, err error) error {
	if err != nil {
		return ctx.Redirect(ctx.Route("worktrees") + "?err=" + url.QueryEscape(err.Error()))
	}
	return ctx.Redirect(ctx.Route("worktrees") + "?ok=" + url.QueryEscape(okMsg))
}

func (s *Server) pruneWorktree(ctx *hime.Context) error {
	threadID := ctx.PostFormValue("threadId")
	err := s.bot.PruneWorktree(threadID)
	if err != nil {
		return s.worktreesRedirect(ctx, "", err)
	}
	return s.worktreesRedirect(ctx, fmt.Sprintf("Pruned worktree for thread %s", threadID), nil)
}

func (s *Server) pruneIdleWorktrees(ctx *hime.Context) error {
	n, err := s.bot.PruneIdleNow()
	if err != nil {
		return s.worktreesRedirect(ctx, "", err)
	}
	return s.worktreesRedirect(ctx, fmt.Sprintf("Pruned %d idle worktree(s)", n), nil)
}

func (s *Server) configRedirect(ctx *hime.Context, okMsg string, err error) error {
	if err != nil {
		return ctx.Redirect(ctx.Route("config") + "?err=" + url.QueryEscape(err.Error()))
	}
	return ctx.Redirect(ctx.Route("config") + "?ok=" + url.QueryEscape(okMsg))
}

func (s *Server) addProject(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	path := ctx.PostFormValue("path")
	return s.configRedirect(ctx, fmt.Sprintf("Added project %q", name), s.cfg.AddProject(name, path))
}

func (s *Server) removeProject(ctx *hime.Context) error {
	name := ctx.PostFormValue("name")
	return s.configRedirect(ctx, fmt.Sprintf("Removed project %q", name), s.cfg.RemoveProject(name))
}

func (s *Server) addUser(ctx *hime.Context) error {
	id := ctx.PostFormValue("id")
	return s.configRedirect(ctx, fmt.Sprintf("Added user %s", id), s.cfg.AddAllowedUser(id))
}

func (s *Server) removeUser(ctx *hime.Context) error {
	id := ctx.PostFormValue("id")
	return s.configRedirect(ctx, fmt.Sprintf("Removed user %s", id), s.cfg.RemoveAllowedUser(id))
}

func (s *Server) addRole(ctx *hime.Context) error {
	id := ctx.PostFormValue("id")
	return s.configRedirect(ctx, fmt.Sprintf("Added role %s", id), s.cfg.AddAllowedRole(id))
}

func (s *Server) removeRole(ctx *hime.Context) error {
	id := ctx.PostFormValue("id")
	return s.configRedirect(ctx, fmt.Sprintf("Removed role %s", id), s.cfg.RemoveAllowedRole(id))
}

func (s *Server) addChannel(ctx *hime.Context) error {
	channelID := ctx.PostFormValue("channelId")
	project := ctx.PostFormValue("project")
	return s.configRedirect(ctx, fmt.Sprintf("Mapped channel %s → %s", channelID, project), s.cfg.AddChannel(channelID, project))
}

func (s *Server) removeChannel(ctx *hime.Context) error {
	channelID := ctx.PostFormValue("channelId")
	return s.configRedirect(ctx, fmt.Sprintf("Removed channel %s", channelID), s.cfg.RemoveChannel(channelID))
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

	switch section {
	case "ci":
		return s.updateCISettings(ctx)
	case "risky":
		return s.updateRiskyPathSettings(ctx)
	case "worktree":
		return s.updateWorktreeSettings(ctx)
	default:
		return s.configRedirect(ctx, "", fmt.Errorf("unknown settings section %q", section))
	}
}

func (s *Server) updateCISettings(ctx *hime.Context) error {
	enabled := ctx.PostFormValue("autoFixCI") == "1" || strings.EqualFold(ctx.PostFormValue("autoFixCI"), "on")
	rawMax := strings.TrimSpace(ctx.PostFormValue("autoFixCIMax"))
	if rawMax == "" {
		return s.configRedirect(ctx, "", fmt.Errorf("autoFixCIMax is required"))
	}
	maxAttempts, err := strconv.Atoi(rawMax)
	if err != nil {
		return s.configRedirect(ctx, "", fmt.Errorf("autoFixCIMax must be an integer"))
	}
	if err := s.cfg.SetAutoFixCI(enabled, maxAttempts); err != nil {
		return s.configRedirect(ctx, "", err)
	}
	msg := "Auto CI fix disabled"
	if enabled {
		msg = fmt.Sprintf("Auto CI fix enabled (max %d attempt(s) per thread)", maxAttempts)
	}
	return s.configRedirect(ctx, msg, nil)
}

func (s *Server) updateRiskyPathSettings(ctx *hime.Context) error {
	useDefault := ctx.PostFormValue("riskyPathUseDefault") == "1" ||
		strings.EqualFold(ctx.PostFormValue("riskyPathUseDefault"), "on")
	text := ctx.PostFormValue("riskyPathGlobs")
	if err := s.cfg.SetRiskyPathGlobsFromText(text, useDefault); err != nil {
		return s.configRedirect(ctx, "", err)
	}
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
}

func (s *Server) updateWorktreeSettings(ctx *hime.Context) error {
	raw := strings.TrimSpace(ctx.PostFormValue("worktreeIdleTTLDays"))
	if raw == "" {
		return s.configRedirect(ctx, "", fmt.Errorf("worktreeIdleTTLDays is required"))
	}
	days, err := strconv.Atoi(raw)
	if err != nil {
		return s.configRedirect(ctx, "", fmt.Errorf("worktreeIdleTTLDays must be an integer"))
	}
	if err := s.cfg.SetWorktreeIdleTTLDays(days); err != nil {
		return s.configRedirect(ctx, "", err)
	}
	msg := fmt.Sprintf("Worktree idle TTL set to %d day(s)", days)
	if days == 0 {
		msg = "Worktree idle cleanup disabled"
	}
	return s.configRedirect(ctx, msg, nil)
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

// sseTick is the lightweight SSE payload. Clients use "tick" events as a signal
// for htmx to re-fetch the current page's content fragment. StatusSnapshot
// fields are kept so existing tests and any custom clients still work.
type sseTick struct {
	bot.StatusSnapshot
	Tick int64 `json:"tick"`
}

// sse streams live ticks as Server-Sent Events (stdlib text/event-stream).
// The first event is unnamed (event type "message") for connect/tests;
// subsequent ticks use event type "tick" so htmx only refreshes on the ticker.
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

	var n int64
	send := func(event string) bool {
		n++
		payload := sseTick{
			StatusSnapshot: s.bot.StatusSnapshot(),
			Tick:           n,
		}
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
	if !send("") {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send("tick") {
				return
			}
		}
	}
}

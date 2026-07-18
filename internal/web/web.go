package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/moonrhythm/hime"

	"github.com/acoshift/grok-discord/internal/bot"
	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/history"
	"github.com/acoshift/grok-discord/internal/sessionstore"
)

//go:embed templates/*
var templateFS embed.FS

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
		"config":               "/config",
		"config.addProject":    "/config/projects",
		"config.removeProject": "/config/projects/remove",
		"config.addUser":       "/config/users",
		"config.removeUser":    "/config/users/remove",
		"config.addRole":       "/config/roles",
		"config.removeRole":    "/config/roles/remove",
		"config.addChannel":    "/config/channels",
		"config.removeChannel": "/config/channels/remove",
		"sse":                  "/events",
	})

	app.TemplateFunc("add", func(a, b int) int { return a + b })

	tp := app.Template()
	tp.FS(templateFS)
	tp.Dir("templates")
	tp.Root("layout")
	tp.ParseFiles("dashboard", "layout.tmpl", "dashboard.tmpl")
	tp.ParseFiles("history", "layout.tmpl", "history.tmpl")
	tp.ParseFiles("history_detail", "layout.tmpl", "history_detail.tmpl")
	tp.ParseFiles("config", "layout.tmpl", "config.tmpl")

	mux := http.NewServeMux()
	mux.Handle("GET /{$}", hime.Handler(s.dashboard))
	mux.Handle("GET /history", hime.Handler(s.historyList))
	mux.Handle("GET /history/{threadID}", hime.Handler(s.historyDetail))
	mux.Handle("GET /config", hime.Handler(s.configPage))
	mux.Handle("POST /config/projects", hime.Handler(s.addProject))
	mux.Handle("POST /config/projects/remove", hime.Handler(s.removeProject))
	mux.Handle("POST /config/users", hime.Handler(s.addUser))
	mux.Handle("POST /config/users/remove", hime.Handler(s.removeUser))
	mux.Handle("POST /config/roles", hime.Handler(s.addRole))
	mux.Handle("POST /config/roles/remove", hime.Handler(s.removeRole))
	mux.Handle("POST /config/channels", hime.Handler(s.addChannel))
	mux.Handle("POST /config/channels/remove", hime.Handler(s.removeChannel))
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
	IsConfig    bool
	Flash       string
	Error       string
	Status      bot.StatusSnapshot
	Threads     []history.Summary
	Thread      history.Thread
	Config      config.Snapshot
	SSEPath     string
}

func (s *Server) dashboard(ctx *hime.Context) error {
	return ctx.View("dashboard", pageData{
		Title:       "Dashboard",
		IsDashboard: true,
		Status:      s.bot.StatusSnapshot(),
		SSEPath:     ctx.Route("sse"),
	})
}

func (s *Server) historyList(ctx *hime.Context) error {
	threads, err := s.history.List()
	if err != nil {
		return ctx.Status(http.StatusInternalServerError).Error("history list: " + err.Error())
	}
	// Also surface sessions that have no turns yet (legacy / mid-run).
	threads = mergeSessionRows(threads, s.sessions.List())
	return ctx.View("history", pageData{
		Title:     "History",
		IsHistory: true,
		Threads:   threads,
	})
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
	return ctx.View("history_detail", pageData{
		Title:     title,
		IsHistory: true,
		Thread:    th,
	})
}

func (s *Server) configPage(ctx *hime.Context) error {
	return ctx.View("config", pageData{
		Title:    "Config",
		IsConfig: true,
		Config:   s.cfg.Snapshot(),
		Flash:    ctx.FormValue("ok"),
		Error:    ctx.FormValue("err"),
	})
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

// sse streams dashboard status as Server-Sent Events (stdlib text/event-stream).
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

	send := func() bool {
		snap := s.bot.StatusSnapshot()
		raw, err := json.Marshal(snap)
		if err != nil {
			log.Printf("web sse marshal: %v", err)
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Immediate first event so clients and tests do not wait on the ticker.
	if !send() {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

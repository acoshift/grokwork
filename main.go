package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/deploy"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/identity"
	"github.com/acoshift/grokwork/internal/sessionstore"
	"github.com/acoshift/grokwork/internal/web"
)

func main() {
	boot := time.Now()
	phase := func(name string, start time.Time) {
		log.Printf("startup: phase=%s elapsed=%s total=%s",
			name,
			time.Since(start).Round(time.Millisecond),
			time.Since(boot).Round(time.Millisecond))
	}

	t := time.Now()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	phase("config", t)

	t = time.Now()
	sessions, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}

	hist, err := history.New(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}

	// One store, shared by both surfaces: it decides which account a login is, so
	// two copies over one file would each cache their own map and disagree the
	// moment somebody links. Fatal on error for the same reason identity.New
	// refuses a malformed file — booting with an empty link table silently demotes
	// every linked user to a stranger, and the next link would overwrite the file.
	links, err := identity.New(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	phase("stores", t)

	t = time.Now()
	b := bot.New(cfg, sessions, hist)
	b.SetIdentity(links)
	// Gate claims until RecoverActiveRuns finishes so a crash re-drive cannot
	// race a user (or web) task on the same thread. Browse/read paths stay open.
	b.EnableReadyGate()

	// Single-instance lock under data/runs/.lock (when journal store is available).
	if runs := b.Runs(); runs != nil {
		host, _ := os.Hostname()
		if err := runs.TryLock(os.Getpid(), time.Now(), host); err != nil {
			log.Fatalf("run journal lock: %v\n\nAnother grokwork process may be using this data directory.", err)
		}
	}
	phase("bot", t)

	t = time.Now()
	dg, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		log.Fatalf("discord session: %v", err)
	}
	b.Register(dg)
	dg.LogLevel = discordgo.LogWarning
	phase("discord_session", t)

	// Open the gateway in parallel with web setup. Discord Open is usually the
	// multi-second stall after restart; the HTTP UI must not wait on it.
	// Recover still runs after Open succeeds so resume can post/heal messages.
	openErr := make(chan error, 1)
	go func() {
		openErr <- dg.Open()
	}()

	t = time.Now()
	addr := cfg.ListenAddr()
	webSrv := web.New(cfg, sessions, hist, b)
	// Reconcile deploy records before the server accepts triggers, so a lane
	// left behind by a crash cannot race a fresh deploy. Nothing is auto-resumed:
	// shell steps are not idempotent, so recovery is an explicit human redeploy.
	webSrv.Deploys().RecoverAtStartup()
	go func() {
		log.Printf("bg: web UI listening on http://%s (dashboard, ship, sessions, worktrees, config)", addr)
		if err := webSrv.ListenAndServe(); err != nil {
			log.Printf("bg: web server stopped: %v", err)
		}
	}()
	phase("web", t)

	t = time.Now()
	if err := <-openErr; err != nil {
		if strings.Contains(err.Error(), "4014") {
			log.Fatalf("open gateway: %v\n\n"+
				"Discord rejected privileged intents.\n"+
				"In https://discord.com/developers/applications → your app → Bot →\n"+
				"Privileged Gateway Intents, enable:\n"+
				"  • MESSAGE CONTENT INTENT\n"+
				"Then restart this process. (Server Members Intent is not required.)\n", err)
		}
		log.Fatalf("open gateway: %v", err)
	}
	phase("discord_open", t)
	defer dg.Close()

	// Recover while ready=false so user claims get ErrNotReady (no double-Grok).
	t = time.Now()
	if err := b.RecoverActiveRuns(context.Background()); err != nil {
		log.Printf("warn: recover active runs: %v", err)
	}
	phase("recover", t)
	b.SetReady(true)
	log.Printf("startup: ready total=%s (discord open, active-run recovery done)",
		time.Since(boot).Round(time.Millisecond))

	fmt.Println("Grok Work bridge running. Ctrl+C to stop.")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	fmt.Println("Shutting down…")

	// Order matters. The web server closes first so a trigger cannot land
	// mid-shutdown; then deploys drain with their own context, because anything
	// placed after the bot's cancel() below would be handed a dead context and
	// force-kill every in-flight step.
	// Web stop is configured for near-instant close (no wait for SSE).
	_ = webSrv.Shutdown()

	depCtx, depCancel := context.WithTimeout(context.Background(), deploy.StopTimeout)
	webSrv.Deploys().Stop(depCtx)
	depCancel()

	stopCtx, cancel := context.WithTimeout(context.Background(), b.ShutdownTimeout())
	b.Stop(stopCtx)
	cancel()
}

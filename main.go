package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grokwork/internal/bot"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/history"
	"github.com/acoshift/grokwork/internal/sessionstore"
	"github.com/acoshift/grokwork/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	sessions, err := sessionstore.New(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}

	hist, err := history.New(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}

	b := bot.New(cfg, sessions, hist)

	addr := cfg.ListenAddr()
	webSrv := web.New(cfg, sessions, hist, b)
	go func() {
		log.Printf("bg: web UI listening on http://%s (dashboard, ship, sessions, worktrees, config)", addr)
		if err := webSrv.ListenAndServe(); err != nil {
			log.Printf("bg: web server stopped: %v", err)
		}
	}()

	dg, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		log.Fatalf("discord session: %v", err)
	}

	b.Register(dg)
	dg.LogLevel = discordgo.LogWarning

	if err := dg.Open(); err != nil {
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
	defer dg.Close()

	fmt.Println("Grok Work bridge running. Ctrl+C to stop.")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	fmt.Println("Shutting down…")
	// Web stop is configured for near-instant close (no wait for SSE).
	_ = webSrv.Shutdown()
}

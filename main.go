package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"

	"github.com/acoshift/grok-discord/internal/bot"
	"github.com/acoshift/grok-discord/internal/config"
	"github.com/acoshift/grok-discord/internal/sessionstore"
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

	dg, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		log.Fatalf("discord session: %v", err)
	}

	b := bot.New(cfg, sessions)
	b.Register(dg)

	if err := dg.Open(); err != nil {
		log.Fatalf("open gateway: %v", err)
	}
	defer dg.Close()

	fmt.Println("Grok Discord bridge running. Ctrl+C to stop.")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	fmt.Println("Shutting down…")
}

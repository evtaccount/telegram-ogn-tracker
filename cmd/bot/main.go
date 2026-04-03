package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-telegram/bot"

	"telegram-ogn-tracker/internal/tracker"
)

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN must be set")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	t := tracker.NewTracker()

	b, err := bot.New(token, bot.WithDefaultHandler(t.DefaultHandler))
	if err != nil {
		log.Fatal(err)
	}

	t.RegisterHandlers(b)
	b.Start(ctx)
}

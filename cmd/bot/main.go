package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	// Embed the IANA tzdata so /tz works on minimal images (alpine without tzdata pkg).
	_ "time/tzdata"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"telegram-ogn-tracker/internal/tracker"
)

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN must be set")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// The default handler is wired to the Tracker via a closure because the
	// Tracker needs the *bot.Bot at construction (so its t.bot field is set
	// before any goroutine could read it). t is assigned before b.Start, so
	// the closure observes a non-nil Tracker by the time updates arrive.
	var t *tracker.Tracker
	b, err := bot.New(token, bot.WithDefaultHandler(func(ctx context.Context, b *bot.Bot, u *models.Update) {
		t.DefaultHandler(ctx, b, u)
	}))
	if err != nil {
		log.Fatal(err)
	}

	t = tracker.NewTracker(b)
	t.RegisterHandlers(b)
	b.Start(ctx)
	t.Shutdown()
}

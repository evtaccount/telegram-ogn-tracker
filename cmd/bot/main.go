package main

import (
	"context"
	"log"
	"log/slog"
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
	// Structured logger: text handler for human-readable output in `docker logs`,
	// with timestamps and levels. Pipe stdlib log through slog so any third-party
	// code that uses log.Printf still goes through the same handler.
	level := slog.LevelInfo
	if os.Getenv("DEBUG") == "1" {
		level = slog.LevelDebug
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
	log.SetFlags(0)
	log.SetOutput(slog.NewLogLogger(handler, slog.LevelInfo).Writer())

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		slog.Error("TELEGRAM_BOT_TOKEN must be set")
		os.Exit(1)
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
		slog.Error("failed to construct bot", "err", err)
		os.Exit(1)
	}

	t = tracker.NewTracker(b)
	t.RegisterHandlers(b)
	b.Start(ctx)
	t.Shutdown()
}

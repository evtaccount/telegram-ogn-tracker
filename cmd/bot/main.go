package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	// Embed the IANA tzdata so /tz works on minimal images (alpine without tzdata pkg).
	_ "time/tzdata"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"telegram-ogn-tracker/internal/tracker"
)

// stdlogClassifier wraps an slog.Handler so stdlib `log` output (used by the
// OGN client and go-telegram-bot internals) lands at the right slog level
// instead of being uniformly INFO.
//
// Rationale: ogn-client emits "Sent keepalive" via log.Printf every ~4 min.
// That single line was ~95% of INFO traffic in 2026-05's log audit and drowned
// out everything actionable. Reclassifying it to DEBUG buys us a clean INFO
// stream without touching the upstream library.
//
// Patterns are matched substring-style — string match is fragile but the
// alternative (typed errors) requires changes in libraries we don't own.
type stdlogClassifier struct{ h slog.Handler }

func (w stdlogClassifier) Write(p []byte) (int, error) {
	s := strings.TrimRight(string(p), "\n")
	lvl := slog.LevelInfo
	switch {
	case strings.Contains(s, "Sent keepalive"):
		lvl = slog.LevelDebug
	case strings.Contains(s, "[ERROR]"):
		// go-telegram-bot logs its own errors as "[TGBOT] [ERROR] …" via the
		// shared stdlog. Most are transient (502s, network blips); WARN keeps
		// them visible without polluting ERROR-grep audits.
		lvl = slog.LevelWarn
	}
	if !w.h.Enabled(context.Background(), lvl) {
		return len(p), nil
	}
	r := slog.NewRecord(time.Now(), lvl, s, 0)
	_ = w.h.Handle(context.Background(), r)
	return len(p), nil
}

// defaultLogPath is the destination used when LOG_FILE is unset.
// Resolved relative to the process's working directory; in Docker this
// becomes /root/logs/bot.log which is mounted to ./logs/ on the host.
const defaultLogPath = "logs/bot.log"

// openLogSink resolves the log destination from LOG_FILE (default
// `logs/bot.log`) and returns an io.Writer plus a one-line description.
// On any failure it falls back to os.Stderr and prints the reason there
// so the operator can spot the misconfiguration.
func openLogSink() (io.Writer, string) {
	path := os.Getenv("LOG_FILE")
	if path == "" {
		path = defaultLogPath
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "log: cannot create dir %s: %v; falling back to stderr\n", dir, err)
			return os.Stderr, "stderr"
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log: cannot open %s: %v; falling back to stderr\n", path, err)
		return os.Stderr, "stderr"
	}
	return f, path
}

func main() {
	// Structured logger: text handler with timestamps and levels, written to a
	// file so the bot doesn't pollute stdout/stderr. `tail -f logs/bot.log` to
	// watch live. DEBUG=1 raises verbosity to include the OGN beacon trace.
	level := slog.LevelInfo
	if os.Getenv("DEBUG") == "1" {
		level = slog.LevelDebug
	}
	logOut, logDest := openLogSink()
	handler := slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
	// Pipe stdlib log through slog so any third-party code that uses log.Printf
	// (notably the OGN client's logger field) ends up in the same structured
	// stream. Lines pass through stdlogClassifier first, which demotes known
	// low-signal patterns (keepalives) and promotes known errors (TGBOT) before
	// the slog handler decides whether to emit.
	log.SetFlags(0)
	log.SetOutput(stdlogClassifier{h: handler})
	slog.Info("logger initialised", "destination", logDest, "level", level.String())

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

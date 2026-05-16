package tracker

import (
	"context"
	"log/slog"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// answerCallback acknowledges a callback query to remove the loading spinner in Telegram.
func (t *Tracker) answerCallback(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	if _, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cq.ID,
	}); err != nil {
		slog.Error("failed to answer callback query", "err", err)
	}
}

// sessionChatID returns the current session's chat ID (0 if no session).
// Must be called with t.mu held.
func (t *Tracker) sessionChatID() int64 {
	if t.session != nil {
		return t.session.ChatID
	}
	return 0
}

// deleteCallbackMessage removes the inline-button message that triggered the callback.
func deleteCallbackMessage(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	if cq.Message.Message != nil {
		_, _ = b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    cq.Message.Message.Chat.ID,
			MessageID: cq.Message.Message.ID,
		})
	}
}

// callbackChatAllowed answers the query, checks trust, and verifies the source
// chat is in the allow-list. Returns the source chatID or 0 if denied.
func (t *Tracker) callbackChatAllowed(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) int64 {
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return 0
	}
	if cq.Message.Message == nil {
		return 0
	}
	chatID := cq.Message.Message.Chat.ID
	if !t.isAllowedChat(chatID) {
		return 0
	}
	return chatID
}

// handleCallback is the common wrapper for callback handlers:
// answer the query, check trust + allow-list, resolve session chatID, and call fn.
func (t *Tracker) handleCallback(ctx context.Context, b *bot.Bot, update *models.Update, fn func(int64)) {
	if t.callbackChatAllowed(ctx, b, update.CallbackQuery) == 0 {
		return
	}
	t.mu.Lock()
	chatID := t.sessionChatID()
	t.mu.Unlock()
	if chatID != 0 {
		fn(chatID)
	}
}

// handleCallbackWithDelete is like handleCallback but also deletes the prompt message.
func (t *Tracker) handleCallbackWithDelete(ctx context.Context, b *bot.Bot, update *models.Update, fn func(int64)) {
	cq := update.CallbackQuery
	if t.callbackChatAllowed(ctx, b, cq) == 0 {
		return
	}
	deleteCallbackMessage(ctx, b, cq)
	t.mu.Lock()
	chatID := t.sessionChatID()
	t.mu.Unlock()
	if chatID != 0 {
		fn(chatID)
	}
}

func (t *Tracker) cbTrackOn(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallback(ctx, b, update, func(chatID int64) {
		t.execTrackOn(ctx, b, chatID)
	})
}

func (t *Tracker) cbTrackOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.handleCallback(ctx, b, update, func(chatID int64) {
		// Callback re-entry has no triggering user message; pass 0 so the
		// confirm prompt isn't paired with anything for cleanup.
		t.askTrackOffConfirm(ctx, b, chatID, cq.From.ID, 0)
	})
}

func (t *Tracker) cbList(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallback(ctx, b, update, func(chatID int64) {
		t.execList(ctx, b, chatID)
	})
}

func (t *Tracker) cbLanding(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.handleCallback(ctx, b, update, func(chatID int64) {
		t.execLanding(ctx, b, chatID, cq.From.ID, 0)
	})
}

func (t *Tracker) cbDriver(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	if t.callbackChatAllowed(ctx, b, cq) == 0 {
		return
	}
	t.mu.Lock()
	chatID := t.sessionChatID()
	t.mu.Unlock()
	if chatID != 0 {
		t.execDriver(ctx, b, chatID, cq.From.ID, cq.From.Username, 0)
	}
}

func (t *Tracker) cbDriverOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	if t.callbackChatAllowed(ctx, b, cq) == 0 {
		return
	}
	t.mu.Lock()
	chatID := t.sessionChatID()
	t.mu.Unlock()
	if chatID != 0 {
		t.execDriverOff(ctx, b, chatID, cq.From.ID)
	}
}

func (t *Tracker) cbArea(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	if t.callbackChatAllowed(ctx, b, cq) == 0 {
		return
	}
	t.mu.Lock()
	chatID := t.sessionChatID()
	radius := defaultAreaRadius
	if t.session != nil && t.session.TrackAreaRadius > 0 {
		radius = t.session.TrackAreaRadius
	}
	t.mu.Unlock()
	if chatID != 0 {
		t.execArea(ctx, b, chatID, radius, cq.From.ID, 0)
	}
}

func (t *Tracker) cbAreaOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallback(ctx, b, update, func(chatID int64) {
		t.execAreaOff(ctx, b, chatID)
	})
}

func (t *Tracker) cbSessionReset(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.handleCallback(ctx, b, update, func(chatID int64) {
		t.askSessionResetConfirm(ctx, b, chatID, cq.From.ID, 0)
	})
}

func (t *Tracker) cbSessionResetConfirm(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.handleCallbackWithDelete(ctx, b, update, func(chatID int64) {
		ackID := t.execSessionReset(ctx, b, chatID, false)
		t.finalizePendingCleanup(cq.From.ID, chatID, ackID)
	})
}

func (t *Tracker) cbSessionResetWipe(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.handleCallbackWithDelete(ctx, b, update, func(chatID int64) {
		ackID := t.execSessionReset(ctx, b, chatID, true)
		t.finalizePendingCleanup(cq.From.ID, chatID, ackID)
	})
}

func (t *Tracker) cbSessionResetCancel(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	chatID := t.callbackChatAllowed(ctx, b, cq)
	if chatID == 0 {
		return
	}
	deleteCallbackMessage(ctx, b, cq)
	t.finalizePendingCleanup(cq.From.ID, chatID)
}

func (t *Tracker) cbTrackOffConfirm(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.handleCallbackWithDelete(ctx, b, update, func(chatID int64) {
		ackID := t.execTrackOff(ctx, b, chatID)
		t.finalizePendingCleanup(cq.From.ID, chatID, ackID)
	})
}

func (t *Tracker) cbTrackOffCancel(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	chatID := t.callbackChatAllowed(ctx, b, cq)
	if chatID == 0 {
		return
	}
	deleteCallbackMessage(ctx, b, cq)
	// No final ack on cancel — drain the queued user-command + prompt IDs and
	// schedule them for deletion. The prompt was just removed synchronously by
	// deleteCallbackMessage; the duplicate delete attempt 5s later will warn
	// once and is harmless.
	t.finalizePendingCleanup(cq.From.ID, chatID)
}

func (t *Tracker) cbStartResume(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.handleCallbackWithDelete(ctx, b, update, func(chatID int64) {
		ackID := t.execTrackOn(ctx, b, chatID)
		t.finalizePendingCleanup(cq.From.ID, chatID, ackID)
	})
}

func (t *Tracker) cbStartFresh(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	if t.callbackChatAllowed(ctx, b, cq) == 0 {
		return
	}
	deleteCallbackMessage(ctx, b, cq)
	t.mu.Lock()
	chatID := t.sessionChatID()
	if t.session != nil {
		// Delete the old dashboard BEFORE stopTrackingAsync zeros DashboardMsgID.
		t.clearDashboardForReset()
		t.stopTrackingAsync()
		t.stopRadarAsync()
	}
	t.session = &GroupSession{
		ChatID:   chatID,
		Tracking: make(map[string]*TrackInfo),
		Drivers:  make(map[int64]*DriverInfo),
	}
	t.saveState()
	t.mu.Unlock()
	if chatID != 0 {
		ackID := t.execTrackOn(ctx, b, chatID)
		t.finalizePendingCleanup(cq.From.ID, chatID, ackID)
	}
}

// cbDashboardAction is the single entry point for all dashboard:* callback
// queries. It answers the callback (clears the spinner), routes to the
// appropriate exec* helper, then triggers a dashboard refresh so the user
// sees the new state without waiting for the heartbeat tick.
func (t *Tracker) cbDashboardAction(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	if cq == nil {
		return
	}
	// Acknowledge first so the client UI stops spinning.
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cq.ID})

	chatID := int64(0)
	if msg := cq.Message.Message; msg != nil {
		chatID = msg.Chat.ID
	}
	if chatID == 0 {
		return
	}
	userID := cq.From.ID
	// Guard: drop dashboard taps from non-allow-listed chats or untrusted users.
	// Other callbacks gate on callbackChatAllowed; do the same here.
	if !t.isAllowedChat(chatID) {
		slog.Warn("dropping dashboard action from non-allowed chat", "chat_id", chatID, "user_id", userID)
		return
	}
	if !t.isTrusted(userID) {
		return
	}
	username := cq.From.Username
	action := strings.TrimPrefix(cq.Data, "dashboard:")
	slog.Info("dashboard action", "action", action, "chat_id", chatID, "user_id", userID)

	switch action {
	case "start":
		t.execTrackOn(ctx, b, chatID)
	case "stop":
		// Route through confirmation, matching the old reply-keyboard "⏹ Стоп"
		// behaviour. Silently halting a live tracking session on a stray tap
		// would be a sharp edge for the group.
		t.askTrackOffConfirm(ctx, b, chatID, userID, 0)
	case "list":
		t.execList(ctx, b, chatID)
	case "add":
		t.execAddNoArgsPrompt(ctx, b, chatID, userID, 0)
	case "area":
		t.execArea(ctx, b, chatID, defaultAreaRadius, userID, 0)
	case "driver":
		t.execDriver(ctx, b, chatID, userID, username, 0)
	case "end":
		t.askSessionResetConfirm(ctx, b, chatID, userID, 0)
	case "radar_stop":
		t.execRadarOff(ctx, b, chatID)
	case "radar_radius":
		t.execRadarAskRadius(ctx, b, chatID, userID, 0)
	default:
		slog.Warn("unknown dashboard action", "action", action)
	}

	// Safe to call even when an action just opened a confirm prompt or hit the
	// default branch — refreshDashboard no-ops if the session/chatID no longer
	// matches, and otherwise it harmlessly re-renders the current state.
	t.refreshDashboard(ctx, chatID)
}

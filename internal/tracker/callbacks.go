package tracker

import (
	"context"
	"log"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// answerCallback acknowledges a callback query to remove the loading spinner in Telegram.
func (t *Tracker) answerCallback(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	if _, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cq.ID,
	}); err != nil {
		log.Printf("failed to answer callback query: %v", err)
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
		b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    cq.Message.Message.Chat.ID,
			MessageID: cq.Message.Message.ID,
		})
	}
}

// handleCallback is the common wrapper for callback handlers:
// answer the query, check trust, resolve session chatID, and call fn.
func (t *Tracker) handleCallback(ctx context.Context, b *bot.Bot, update *models.Update, fn func(int64)) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
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
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
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
	t.handleCallback(ctx, b, update, func(chatID int64) {
		t.askTrackOffConfirm(ctx, b, chatID)
	})
}

func (t *Tracker) cbList(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallback(ctx, b, update, func(chatID int64) {
		t.execList(ctx, b, chatID)
	})
}

func (t *Tracker) cbLanding(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallback(ctx, b, update, func(chatID int64) {
		t.execLanding(ctx, b, chatID)
	})
}

func (t *Tracker) cbDriver(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := t.sessionChatID()
	t.mu.Unlock()
	if chatID != 0 {
		t.execDriver(ctx, b, chatID, cq.From.ID, cq.From.Username)
	}
}

func (t *Tracker) cbDriverOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
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
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
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
		t.execArea(ctx, b, chatID, radius)
	}
}

func (t *Tracker) cbAreaOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallback(ctx, b, update, func(chatID int64) {
		t.execAreaOff(ctx, b, chatID)
	})
}

func (t *Tracker) cbSessionReset(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallback(ctx, b, update, func(chatID int64) {
		t.askSessionResetConfirm(ctx, b, chatID)
	})
}

func (t *Tracker) cbSessionResetConfirm(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallbackWithDelete(ctx, b, update, func(chatID int64) {
		t.execSessionReset(ctx, b, chatID, false)
	})
}

func (t *Tracker) cbSessionResetWipe(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallbackWithDelete(ctx, b, update, func(chatID int64) {
		t.execSessionReset(ctx, b, chatID, true)
	})
}

func (t *Tracker) cbSessionResetCancel(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	deleteCallbackMessage(ctx, b, cq)
}

func (t *Tracker) cbTrackOffConfirm(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallbackWithDelete(ctx, b, update, func(chatID int64) {
		t.execTrackOff(ctx, b, chatID)
	})
}

func (t *Tracker) cbTrackOffCancel(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	deleteCallbackMessage(ctx, b, cq)
}

func (t *Tracker) cbStartResume(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.handleCallbackWithDelete(ctx, b, update, func(chatID int64) {
		t.execTrackOn(ctx, b, chatID)
	})
}

func (t *Tracker) cbStartFresh(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	deleteCallbackMessage(ctx, b, cq)
	t.mu.Lock()
	chatID := t.sessionChatID()
	if t.session != nil {
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
		t.execTrackOn(ctx, b, chatID)
	}
}

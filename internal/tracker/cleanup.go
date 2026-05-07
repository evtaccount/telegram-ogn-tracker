package tracker

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-telegram/bot"
)

// ephemeralDeleteDelay is the grace period between sending the final ack of
// an operation and removing the transient message chain from the chat. Long
// enough for the user to read the result, short enough to keep the chat tidy.
const ephemeralDeleteDelay = 5 * time.Second

// scheduleEphemeralDelete fires a goroutine that sleeps for the grace period
// and then issues a single deleteMessages call for the whole batch. Best-
// effort: errors (missing perms, message already deleted by user) are logged
// at warn but never retried.
//
// Safe to call with t.mu held — the goroutine captures the bot pointer at
// scheduling time and never touches Tracker state.
func (t *Tracker) scheduleEphemeralDelete(chatID int64, msgIDs ...int) {
	if t.bot == nil || len(msgIDs) == 0 {
		return
	}
	b := t.bot
	ids := append([]int(nil), msgIDs...)
	go func() {
		time.Sleep(ephemeralDeleteDelay)
		if _, err := b.DeleteMessages(context.Background(), &bot.DeleteMessagesParams{
			ChatID:     chatID,
			MessageIDs: ids,
		}); err != nil {
			slog.Warn("failed to delete ephemeral messages", "chat_id", chatID, "ids", ids, "err", err)
		}
	}()
}

// appendPendingCleanup adds msgIDs to the per-user cleanup queue for the
// active session. Used by multi-step flows whose intermediate transient
// messages should disappear together with the final ack.
//
// Caller must hold t.mu.
func (t *Tracker) appendPendingCleanup(userID int64, msgIDs ...int) {
	if t.session == nil || userID == 0 || len(msgIDs) == 0 {
		return
	}
	if t.session.PendingCleanup == nil {
		t.session.PendingCleanup = make(map[int64][]int)
	}
	t.session.PendingCleanup[userID] = append(t.session.PendingCleanup[userID], msgIDs...)
}

// drainPendingCleanup removes and returns the user's queue. Returns nil if
// nothing was queued. Caller must hold t.mu.
func (t *Tracker) drainPendingCleanup(userID int64) []int {
	if t.session == nil || t.session.PendingCleanup == nil {
		return nil
	}
	ids := t.session.PendingCleanup[userID]
	delete(t.session.PendingCleanup, userID)
	return ids
}

// finalizePendingCleanup drains the user's queue, appends finalIDs, and
// schedules the whole batch for deletion. Locks t.mu internally so callers
// don't have to coordinate.
func (t *Tracker) finalizePendingCleanup(userID int64, chatID int64, finalIDs ...int) {
	t.mu.Lock()
	queued := t.drainPendingCleanup(userID)
	t.mu.Unlock()
	all := append(queued, finalIDs...)
	t.scheduleEphemeralDelete(chatID, all...)
}

// forgetPendingCleanup discards the user's queue without scheduling any
// deletion. Used on flow abandonment (timeout, session reset) where the
// transient messages should remain in the chat.
func (t *Tracker) forgetPendingCleanup(userID int64) {
	t.mu.Lock()
	t.drainPendingCleanup(userID)
	t.mu.Unlock()
}

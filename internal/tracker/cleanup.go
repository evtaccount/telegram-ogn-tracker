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

// deleteOneByOne issues a separate DeleteMessage call per id and logs failures
// individually. The batch DeleteMessages endpoint is all-or-nothing: if any id
// belongs to a message the bot can't delete (e.g. a user's /add command in a
// group where the bot isn't admin), the entire batch is rejected and even the
// bot's own messages survive. Per-message deletion isolates those failures.
func deleteOneByOne(b *bot.Bot, chatID int64, ids []int, errLabel string) {
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, err := b.DeleteMessage(context.Background(), &bot.DeleteMessageParams{
			ChatID:    chatID,
			MessageID: id,
		}); err != nil {
			slog.Warn(errLabel, "chat_id", chatID, "msg_id", id, "err", err)
		}
	}
}

// deleteMessagesAsync deletes msgIDs in a goroutine — no grace period. Used to
// clean up label / live-location messages that the session is abandoning right
// now (pilot removed, tracking reset, session wiped) so the chat doesn't carry
// stale "Eugene (FE0E4A)" cards once their owner is gone. Best-effort: errors
// are logged at warn.
func (t *Tracker) deleteMessagesAsync(chatID int64, msgIDs ...int) {
	if t.bot == nil || len(msgIDs) == 0 {
		return
	}
	b := t.bot
	ids := append([]int(nil), msgIDs...)
	go deleteOneByOne(b, chatID, ids, "failed to delete orphaned message")
}

// scheduleEphemeralDelete fires a goroutine that sleeps for the grace period
// and then deletes the messages one by one. Best-effort: errors (missing
// perms, message already deleted by user) are logged at warn but never
// retried.
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
		deleteOneByOne(b, chatID, ids, "failed to delete ephemeral message")
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

// sendAck sends a Telegram message and returns its ID. errLabel is logged on
// failure. Returns 0 if the bot is unset or the send failed.
//
// Used by exec'es that may be invoked from multiple callsites (direct
// command, button press, callback) — each callsite then decides how to
// handle the resulting ID (single-step schedule, queue append, finalize).
func (t *Tracker) sendAck(ctx context.Context, params *bot.SendMessageParams, errLabel string) int {
	if t.bot == nil {
		return 0
	}
	msg, err := t.bot.SendMessage(ctx, params)
	if err != nil {
		slog.Error(errLabel, "err", err)
		return 0
	}
	return msg.ID
}

// scheduleAck is the single-step convenience wrapper: send an ack message
// and, on success, schedule both it and the triggering user message for
// ephemeral deletion after the grace period. Returns the ack message ID for
// chaining (0 on failure).
//
// Pass userMsgID == 0 to skip cleanup scheduling — useful for info-only acks
// that should remain visible.
func (t *Tracker) scheduleAck(ctx context.Context, chatID int64, userMsgID int, params *bot.SendMessageParams, errLabel string) int {
	ackID := t.sendAck(ctx, params, errLabel)
	if ackID != 0 && userMsgID != 0 {
		t.scheduleEphemeralDelete(chatID, userMsgID, ackID)
	}
	return ackID
}

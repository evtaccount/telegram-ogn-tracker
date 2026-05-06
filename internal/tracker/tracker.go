package tracker

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ogn/client"
	"ogn/ddb"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Tracker is the central controller that bridges Telegram bot and OGN APRS feed.
// It manages a single group session, user registry, and APRS client lifecycle.
type Tracker struct {
	bot           *bot.Bot
	botUsername   string
	aprs          *client.Client
	devices       map[string]ddb.Device // OGN Device Database cache for model/registration display
	mu            sync.Mutex            // guards session, users, devices, shuttingDown
	session       *GroupSession
	users         map[int64]*UserInfo
	resumeOnStart bool // whether to auto-resume tracking on the next restart
	// allowedChats is a whitelist of group chat IDs allowed to use the bot.
	// Nil means "allow all" — preserves behaviour when ALLOWED_CHATS is unset.
	// Populated once in NewTracker and never mutated thereafter.
	allowedChats map[int64]bool
	// Asynchronous persistence: saveCh delivers marshalled snapshots to a
	// background worker so callers do not block on disk I/O. After Shutdown
	// flips shuttingDown=true the channel is closed and saveDone signals exit.
	saveCh       chan []byte
	saveDone     chan struct{}
	shuttingDown bool // guarded by mu
}

// parseAllowedChats parses a comma-separated list of chat IDs from env.
// Returns nil for empty input (meaning "allow all chats"). Invalid entries
// are logged and skipped.
func parseAllowedChats(env string) map[int64]bool {
	env = strings.TrimSpace(env)
	if env == "" {
		return nil
	}
	m := make(map[int64]bool)
	for _, part := range strings.Split(env, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			slog.Warn("ALLOWED_CHATS: ignoring invalid entry", "entry", part, "err", err)
			continue
		}
		m[id] = true
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// isAllowedChat returns true if the chat may use the bot.
// When the allow-list is unset (nil), every chat is allowed.
func (t *Tracker) isAllowedChat(chatID int64) bool {
	return t.allowedChats == nil || t.allowedChats[chatID]
}

// tz returns the session timezone, defaulting to UTC.
// Must be called with t.mu held.
func (t *Tracker) tz() *time.Location {
	if t.session != nil {
		return t.session.tz()
	}
	return time.UTC
}

// dmReplyKeyboard returns a reply keyboard for private chat.
// Shows "📍 Посадка" only if the user is actively tracked and flying.
// Must be called with t.mu held.
func (t *Tracker) dmReplyKeyboard(userID int64) *models.ReplyKeyboardMarkup {
	u, ok := t.users[userID]
	if !ok || u.OGNID == "" {
		return nil
	}
	s := t.session
	if s == nil || !s.TrackingOn {
		return nil
	}
	info, ok := s.Tracking[u.OGNID]
	if !ok || info.Status != StatusFlying {
		return nil
	}
	return &models.ReplyKeyboardMarkup{
		Keyboard: [][]models.KeyboardButton{
			{{Text: "🪂 Сел"}, {Text: "📍 Посадка"}},
		},
		ResizeKeyboard: true,
	}
}

// loadDevices fetches the OGN DDB (device database) in the background.
// The result is used to render aircraft model and registration next to the OGN ID.
func (t *Tracker) loadDevices() {
	devices, err := ddb.GetDevices()
	if err != nil {
		slog.Error("failed to load OGN device database", "err", err)
		return
	}
	m := ddb.LookupByID(devices)
	t.mu.Lock()
	t.devices = m
	t.mu.Unlock()
	slog.Info("loaded devices from OGN database", "count", len(m))
}

// NewTracker creates a Tracker, restores persisted state, fetches the bot
// username, and loads the OGN DDB.
// The *bot.Bot is taken at construction so the field is fixed before any
// goroutine could read it — no further writes, no race.
func NewTracker(b *bot.Bot) *Tracker {
	t := &Tracker{
		bot:          b,
		aprs:         client.New("N0CALL", ""),
		users:        make(map[int64]*UserInfo),
		allowedChats: parseAllowedChats(os.Getenv("ALLOWED_CHATS")),
		saveCh:       make(chan []byte, 1),
		saveDone:     make(chan struct{}),
	}
	t.aprs.Logger = log.Default()
	go t.saveWorker()
	if t.allowedChats != nil {
		ids := make([]string, 0, len(t.allowedChats))
		for id := range t.allowedChats {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		slog.Info("ALLOWED_CHATS active", "ids", strings.Join(ids, ","))
	} else {
		slog.Info("ALLOWED_CHATS not set; all chats allowed")
	}

	if me, err := b.GetMe(context.Background()); err == nil {
		t.botUsername = me.Username
		slog.Info("bot identity resolved", "username", t.botUsername)
	} else {
		slog.Error("failed to get bot info", "err", err)
	}

	// Restore previous session.
	t.mu.Lock()
	resumeTracking := t.loadState()
	if resumeTracking {
		t.updateFilter()
	}
	t.mu.Unlock()
	t.resumeOnStart = resumeTracking

	go t.loadDevices()
	return t
}

// Shutdown stops background goroutines, persists final state synchronously,
// and disconnects the APRS client. Idempotent — extra calls return immediately.
// Designed to be invoked from main() after the bot's update loop exits.
func (t *Tracker) Shutdown() {
	t.mu.Lock()
	if t.shuttingDown {
		t.mu.Unlock()
		return
	}
	t.shuttingDown = true
	final := t.marshalStateLocked()

	var stopCh, radarStopCh chan struct{}
	if t.session != nil {
		if t.session.TrackingOn {
			stopCh = t.session.StopCh
			t.session.StopCh = nil
			t.session.TrackingOn = false
		}
		if t.session.RadarOn {
			radarStopCh = t.session.RadarStopCh
			t.session.RadarStopCh = nil
			t.session.RadarOn = false
		}
	}
	aprs := t.aprs
	t.mu.Unlock()

	if stopCh != nil {
		close(stopCh)
	}
	if radarStopCh != nil {
		close(radarStopCh)
	}

	// Drain the async writer and force a final synchronous write so the very
	// last in-memory edits reach disk before the process exits.
	close(t.saveCh)
	<-t.saveDone
	if final != nil {
		writeStateBytes(final)
	}

	if aprs != nil {
		_ = aprs.Disconnect()
	}

	slog.Info("[shutdown] state saved, goroutines stopped")
}

// ensureUser returns or creates a UserInfo for the given Telegram user.
// Must be called with t.mu held.
func (t *Tracker) ensureUser(from *models.User) *UserInfo {
	u, ok := t.users[from.ID]
	if !ok {
		u = &UserInfo{UserID: from.ID}
		t.users[from.ID] = u
	}
	u.Username = from.Username
	if u.DisplayName == "" {
		u.DisplayName = strings.TrimSpace(from.FirstName + " " + from.LastName)
	}
	return u
}

// RegisterHandlers wires all Telegram command and callback handlers
// and auto-resumes tracking if it was active before the restart.
func (t *Tracker) RegisterHandlers(b *bot.Bot) {
	b.RegisterHandler(bot.HandlerTypeMessageText, "start_session", bot.MatchTypeCommand, t.cmdStartSession)
	b.RegisterHandler(bot.HandlerTypeMessageText, "session_reset", bot.MatchTypeCommand, t.cmdSessionReset)
	b.RegisterHandler(bot.HandlerTypeMessageText, "add", bot.MatchTypeCommand, t.cmdAdd)
	b.RegisterHandler(bot.HandlerTypeMessageText, "remove", bot.MatchTypeCommand, t.cmdRemove)
	b.RegisterHandler(bot.HandlerTypeMessageText, "track_on", bot.MatchTypeCommand, t.cmdTrackOn)
	b.RegisterHandler(bot.HandlerTypeMessageText, "track_off", bot.MatchTypeCommand, t.cmdTrackOff)
	b.RegisterHandler(bot.HandlerTypeMessageText, "list", bot.MatchTypeCommand, t.cmdList)
	b.RegisterHandler(bot.HandlerTypeMessageText, "status", bot.MatchTypeCommand, t.cmdStatus)
	b.RegisterHandler(bot.HandlerTypeMessageText, "landing", bot.MatchTypeCommand, t.cmdLanding)
	b.RegisterHandler(bot.HandlerTypeMessageText, "driver", bot.MatchTypeCommand, t.cmdDriver)
	b.RegisterHandler(bot.HandlerTypeMessageText, "driver_off", bot.MatchTypeCommand, t.cmdDriverOff)
	b.RegisterHandler(bot.HandlerTypeMessageText, "area", bot.MatchTypeCommand, t.cmdArea)
	b.RegisterHandler(bot.HandlerTypeMessageText, "area_off", bot.MatchTypeCommand, t.cmdAreaOff)
	b.RegisterHandler(bot.HandlerTypeMessageText, "radar", bot.MatchTypeCommand, t.cmdRadar)
	b.RegisterHandler(bot.HandlerTypeMessageText, "tz", bot.MatchTypeCommand, t.cmdTz)
	b.RegisterHandler(bot.HandlerTypeMessageText, "help", bot.MatchTypeCommand, t.cmdHelp)
	if os.Getenv("DEBUG") == "1" {
		b.RegisterHandler(bot.HandlerTypeMessageText, "debug_wipe", bot.MatchTypeCommand, t.cmdDebugWipe)
		slog.Info("[debug] /debug_wipe handler registered (DEBUG=1)")
	}
	b.RegisterHandler(bot.HandlerTypeMessageText, "myid", bot.MatchTypeCommand, t.cmdMyID)
	b.RegisterHandler(bot.HandlerTypeMessageText, "confirm", bot.MatchTypeCommand, t.cmdConfirm)
	b.RegisterHandler(bot.HandlerTypeMessageText, "start", bot.MatchTypeCommand, t.cmdStart)

	// Inline button callbacks.
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "track_on", bot.MatchTypeExact, t.cbTrackOn)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "track_off", bot.MatchTypeExact, t.cbTrackOff)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "list", bot.MatchTypeExact, t.cbList)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "landing", bot.MatchTypeExact, t.cbLanding)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "driver", bot.MatchTypeExact, t.cbDriver)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "driver_off", bot.MatchTypeExact, t.cbDriverOff)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "area", bot.MatchTypeExact, t.cbArea)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "area_off", bot.MatchTypeExact, t.cbAreaOff)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "session_reset", bot.MatchTypeExact, t.cbSessionReset)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "start_resume", bot.MatchTypeExact, t.cbStartResume)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "start_fresh", bot.MatchTypeExact, t.cbStartFresh)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "track_off_confirm", bot.MatchTypeExact, t.cbTrackOffConfirm)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "track_off_cancel", bot.MatchTypeExact, t.cbTrackOffCancel)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "session_reset_confirm", bot.MatchTypeExact, t.cbSessionResetConfirm)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "session_reset_wipe", bot.MatchTypeExact, t.cbSessionResetWipe)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "session_reset_cancel", bot.MatchTypeExact, t.cbSessionResetCancel)

	// Auto-resume tracking if it was active before restart.
	if t.resumeOnStart && t.session != nil {
		if !t.isAllowedChat(t.session.ChatID) {
			slog.Warn("not auto-resuming tracking: chat not in ALLOWED_CHATS", "chat_id", t.session.ChatID)
			return
		}
		t.mu.Lock()
		t.session.TrackingOn = true
		t.session.StopCh = make(chan struct{})
		stopCh := t.session.StopCh
		aprs := t.aprs
		t.mu.Unlock()
		go t.runClient(stopCh, aprs)
		go t.sendUpdates(stopCh)
		slog.Info("auto-resumed tracking from saved session")
	}
}

// DefaultHandler processes updates that don't match any registered command:
// pickup callbacks, driver live-location edits, location messages, DM text input,
// and reply-keyboard button presses in groups.
func (t *Tracker) DefaultHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	// Handle pickup callback queries (dynamic IDs, can't use exact match).
	if update.CallbackQuery != nil {
		cq := update.CallbackQuery
		if cq.Message.Message != nil && !t.isAllowedChat(cq.Message.Message.Chat.ID) {
			t.answerCallback(ctx, b, cq)
			return
		}
		if strings.HasPrefix(cq.Data, "pickup:") {
			t.answerCallback(ctx, b, cq)
			if !t.isTrusted(cq.From.ID) {
				return
			}
			id := cq.Data[7:]
			t.execPickup(ctx, b, id)
			return
		}
		return
	}

	// Handle driver live location updates (edited messages).
	if update.EditedMessage != nil && update.EditedMessage.Location != nil {
		if !t.isAllowedChat(update.EditedMessage.Chat.ID) {
			return
		}
		t.mu.Lock()
		if t.session != nil {
			for _, d := range t.session.Drivers {
				if d.MsgID != 0 && update.EditedMessage.ID == d.MsgID {
					d.Pos = &Coordinates{
						Latitude:  update.EditedMessage.Location.Latitude,
						Longitude: update.EditedMessage.Location.Longitude,
					}
					break
				}
			}
		}
		t.mu.Unlock()
		return
	}

	if update.Message == nil || update.Message.From == nil {
		return
	}
	m := update.Message
	if !t.isTrusted(m.From.ID) {
		return
	}

	// Drop any group-chat update that is not in the allow-list. DMs are
	// unaffected — they need to remain reachable so deep-link /add works.
	if isGroupChat(m.Chat) && !t.isAllowedChat(m.Chat.ID) {
		slog.Warn("dropping group update from non-allowed chat", "chat_id", m.Chat.ID, "user_id", m.From.ID)
		return
	}

	// Handle location messages.
	if m.Location != nil {
		if isPrivateChat(m.Chat) {
			t.handleDMLanding(ctx, b, m)
		} else {
			t.handleLocation(ctx, b, m)
		}
		return
	}

	// Handle DM text/buttons.
	if m.Text != "" && isPrivateChat(m.Chat) && !strings.HasPrefix(m.Text, "/") {
		if m.Text == "🪂 Сел" {
			t.execDMConfirmLanding(ctx, b, m)
			return
		}
		if m.Text == "📍 Посадка" {
			t.execDMLanding(ctx, b, m)
			return
		}
		t.handleDMText(ctx, b, m)
		return
	}

	// Handle pending radar radius input (group only).
	if m.Text != "" && isGroupChat(m.Chat) && !strings.HasPrefix(m.Text, "/") {
		t.mu.Lock()
		waiting := t.session != nil && t.session.WaitingRadarRadius && time.Now().Before(t.session.RadarRadiusExpiry)
		if waiting {
			t.session.WaitingRadarRadius = false
			chatID := m.Chat.ID
			t.mu.Unlock()
			if r, err := strconv.Atoi(strings.TrimSpace(m.Text)); err != nil || r <= 0 || r > maxAreaRadius {
				if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: chatID,
					Text:   fmt.Sprintf("Укажите число от 1 до %d", maxAreaRadius),
				}); err != nil {
					slog.Error("failed to send invalid radar radius message", "err", err)
				}
			} else {
				t.execRadarSetRadius(ctx, b, chatID, r)
			}
			return
		}
		t.mu.Unlock()
	}

	// Route reply keyboard button presses (group only).
	if m.Text != "" && isGroupChat(m.Chat) {
		chatID := m.Chat.ID
		switch m.Text {
		case "➕ Добавить":
			if !t.requireSession(ctx, b, chatID) {
				break
			}
			// Simulate /add without arguments — initiate DM flow.
			// Override text so commandArgs() returns "" (no spurious arg).
			fakeMsgCopy := *m
			fakeMsgCopy.Text = "/add"
			t.cmdAdd(ctx, b, &models.Update{Message: &fakeMsgCopy})
		case "▶️ Старт":
			if !t.requireSession(ctx, b, chatID) {
				break
			}
			t.execTrackOn(ctx, b, chatID)
		case "⏹ Стоп":
			if t.requireSession(ctx, b, chatID) {
				t.askTrackOffConfirm(ctx, b, chatID)
			}
		case "📋 Список":
			if t.requireSession(ctx, b, chatID) {
				t.execList(ctx, b, chatID)
			}
		case "📡 Зона":
			if t.requireSession(ctx, b, chatID) {
				t.execArea(ctx, b, chatID, defaultAreaRadius)
			}
		case "📡 Зона ✕":
			if t.requireSession(ctx, b, chatID) {
				t.execAreaOff(ctx, b, chatID)
			}
		case "🚗 Водитель":
			if t.requireSession(ctx, b, chatID) {
				t.execDriver(ctx, b, chatID, m.From.ID, m.From.Username)
			}
		case "📡 Радар":
			if t.requireSession(ctx, b, chatID) {
				t.execRadarOn(ctx, b, chatID, 0)
			}
		case "⏹ Радар стоп":
			if t.requireSession(ctx, b, chatID) {
				t.execRadarOff(ctx, b, chatID)
			}
		case "📡 Радиус":
			if t.requireSession(ctx, b, chatID) {
				t.execRadarAskRadius(ctx, b, chatID)
			}
		case "🔄 Завершить":
			if t.requireSession(ctx, b, chatID) {
				t.askSessionResetConfirm(ctx, b, chatID)
			}
		}
	}
}

// handleDMText processes plain text in DMs — used for OGN ID input
// during the /add flow when a pilot adds themselves via deep link.
func (t *Tracker) handleDMText(ctx context.Context, b *bot.Bot, m *models.Message) {
	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	if u.PendingGroup == 0 {
		t.mu.Unlock()
		return
	}

	// Check that the pending group session exists.
	s := t.session
	if s == nil || s.ChatID != u.PendingGroup {
		pendingGroup := u.PendingGroup
		u.PendingGroup = 0
		t.saveState()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   fmt.Sprintf("Группа %d не найдена. Попросите добавить вас заново.", pendingGroup),
		}); err != nil {
			slog.Error("failed to send pending group not found", "err", err)
		}
		return
	}

	id := shortID(m.Text)
	if !isValidShortID(id) {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Это не похоже на OGN ID. Пришлите 6 шестнадцатеричных символов (0-9, A-F), например FE0E4A.",
		}); err != nil {
			slog.Error("failed to send invalid ognid message", "err", err)
		}
		return
	}
	groupChatID := s.ChatID

	// Add to session (reset status — fresh add).
	name := u.DisplayName
	if info, ok := s.Tracking[id]; ok {
		info.Name = name
		info.Username = u.Username
		info.OwnerUserID = u.UserID
		info.AutoDiscovered = false
		info.Status = StatusFlying
		info.LandingTime = time.Time{}
	} else {
		s.Tracking[id] = &TrackInfo{
			Name:        name,
			Username:    u.Username,
			OwnerUserID: u.UserID,
		}
	}

	u.OGNID = id
	u.PendingGroup = 0
	t.updateFilter()
	kb := s.replyKeyboard()
	dmKb := t.dmReplyKeyboard(u.UserID)
	t.saveState()
	t.mu.Unlock()

	// Confirm in DM.
	dmParams := &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   fmt.Sprintf("Добавлен %s в группу", id),
	}
	if dmKb != nil {
		dmParams.ReplyMarkup = dmKb
	}
	if _, err := b.SendMessage(ctx, dmParams); err != nil {
		slog.Error("failed to confirm add in DM", "err", err)
	}

	// Confirm in group.
	label := id
	if name != "" {
		label = id + " (" + name + ")"
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      groupChatID,
		Text:        "Добавлен " + label,
		ReplyMarkup: kb,
	}); err != nil {
		slog.Error("failed to confirm add in group", "err", err)
	}
}

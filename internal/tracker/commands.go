package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	defaultAreaRadius = 100             // km — default radius for /area
	maxAreaRadius     = 500             // upper bound for /area and /radar
	waitTimeout       = 2 * time.Minute // pending location/driver window
	driverReminder    = 2 * time.Minute // delay between driver reminders
)

// isTrusted checks whether a Telegram user is allowed to interact with the bot.
// Currently allows all users; will be restricted when multi-group support is added.
func (t *Tracker) isTrusted(userID int64) bool {
	return true
}

// requireGroupChat is a guard that replies with an error if called outside a group.
// When ALLOWED_CHATS is configured, commands from non-listed groups are dropped
// silently — we don't want strangers to confirm the bot's presence.
func (t *Tracker) requireGroupChat(ctx context.Context, b *bot.Bot, m *models.Message) bool {
	if !isGroupChat(m.Chat) {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Эта команда работает только в группе.",
		}); err != nil {
			slog.Error("failed to send group-only message", "err", err)
		}
		return false
	}
	if !t.isAllowedChat(m.Chat.ID) {
		slog.Warn("dropping command from non-allowed chat", "chat_id", m.Chat.ID, "user_id", m.From.ID)
		return false
	}
	return true
}

// requireSession is a guard that replies with an error if no active session exists.
func (t *Tracker) requireSession(ctx context.Context, b *bot.Bot, chatID int64) bool {
	t.mu.Lock()
	active := t.session != nil && t.session.ChatID != 0
	t.mu.Unlock()
	if !active {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "Сначала выполните /start",
		}); err != nil {
			slog.Error("failed to send session required message", "err", err)
		}
	}
	return active
}

// requireGroupSession combines requireGroupChat + requireSession guards.
// Returns true if the command can proceed.
func (t *Tracker) requireGroupSession(ctx context.Context, b *bot.Bot, m *models.Message) bool {
	return t.requireGroupChat(ctx, b, m) && t.requireSession(ctx, b, m.Chat.ID)
}

// --- Command dispatchers ---
//
// Each cmdX is a thin wrapper: validate sender, run guards, then delegate to
// the corresponding execX in flows.go (or to the DM handler in dm.go).

// cmdStart handles /start: in groups — creates a fresh session,
// in DMs — registers the user and processes deep-link payloads for /add flow.
func (t *Tracker) cmdStart(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	slog.Info("cmd /start", "user_id", m.From.ID, "chat_id", m.Chat.ID, "chat_type", m.Chat.Type)

	if isPrivateChat(m.Chat) {
		t.cmdStartPrivate(ctx, b, m)
		return
	}

	if !t.isAllowedChat(m.Chat.ID) {
		slog.Warn("dropping /start from non-allowed chat", "chat_id", m.Chat.ID, "user_id", m.From.ID)
		return
	}

	// Group chat: if session exists with pilots, ask before resetting.
	t.mu.Lock()
	if t.session != nil && t.session.ChatID == m.Chat.ID && len(t.session.Tracking) > 0 {
		t.mu.Unlock()
		t.askStartChoice(ctx, b, m.Chat.ID)
		return
	}
	// No session or empty session — create fresh.
	if t.session != nil {
		t.stopTrackingAsync()
		t.stopRadarAsync()
	}
	t.session = &GroupSession{
		ChatID:   m.Chat.ID,
		Tracking: make(map[string]*TrackInfo),
		Drivers:  make(map[int64]*DriverInfo),
	}
	kb := t.session.replyKeyboard()
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      m.Chat.ID,
		Text:        "Сессия начата. Используйте /add <id> или /area.",
		ReplyMarkup: kb,
	}); err != nil {
		slog.Error("failed to send start message", "err", err)
	}
}

// cmdStartSession unconditionally creates a fresh session in the current group
// chat, dropping any pilot list and stopping tracking/radar.
// Unlike /start (which prompts when pilots already exist), this is the explicit
// "wipe and restart" entry point documented in README.
func (t *Tracker) cmdStartSession(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupChat(ctx, b, m) {
		return
	}

	t.mu.Lock()
	if t.session != nil {
		t.stopTrackingAsync()
		t.stopRadarAsync()
	}
	t.session = &GroupSession{
		ChatID:   m.Chat.ID,
		Tracking: make(map[string]*TrackInfo),
		Drivers:  make(map[int64]*DriverInfo),
	}
	kb := t.session.replyKeyboard()
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      m.Chat.ID,
		Text:        "Сессия пересоздана. Все пилоты удалены. Используйте /add <id> или /area.",
		ReplyMarkup: kb,
	}); err != nil {
		slog.Error("failed to send start_session message", "err", err)
	}
}

func (t *Tracker) cmdSessionReset(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}
	t.askSessionResetConfirm(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdAdd(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}

	// /add only works in groups.
	if isPrivateChat(m.Chat) {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Эта команда работает только в группе.",
		}); err != nil {
			slog.Error("failed to send group-only message", "err", err)
		}
		return
	}

	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}

	args := strings.Fields(commandArgs(m.Text))

	// /add with arguments: direct add (existing behavior).
	if len(args) > 0 {
		t.execAddDirect(ctx, b, m, args)
		return
	}

	// /add without arguments: initiate DM flow.
	// Try sending DM directly (works if user ever started the bot).
	t.mu.Lock()
	s := t.session
	u := t.ensureUser(m.From)
	u.PendingGroup = s.ChatID
	hasOGNID := u.OGNID != ""
	ognID := u.OGNID
	t.saveState()
	botUsername := t.botUsername
	groupChatID := s.ChatID
	t.mu.Unlock()

	// Try to send DM using user ID as chat ID.
	var dmText string
	if hasOGNID {
		dmText = fmt.Sprintf("Ваш OGN ID: %s\nОтправьте новый ID или /confirm чтобы использовать текущий.", ognID)
	} else {
		dmText = "Отправьте ваш OGN ID (6-значный адрес трекера):"
	}
	_, dmErr := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.From.ID,
		Text:   dmText,
	})

	if dmErr == nil {
		// DM sent successfully — register DMChatID.
		t.mu.Lock()
		u.DMChatID = m.From.ID
		t.saveState()
		t.mu.Unlock()

		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Написал вам в личку",
		}); err != nil {
			slog.Error("failed to confirm DM sent in group", "err", err)
		}
		return
	}

	// Can't send DM — show deep link button.
	slog.Warn("failed to send DM, falling back to deep link", "user_id", m.From.ID, "err", dmErr)
	deepLink := fmt.Sprintf("https://t.me/%s?start=add_%d", botUsername, groupChatID)
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "Добавить свой трекер", URL: deepLink},
			},
		},
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      m.Chat.ID,
		Text:        "Напишите мне в личку, чтобы добавить свой трекер",
		ReplyMarkup: kb,
	}); err != nil {
		slog.Error("failed to send deep link button", "err", err)
	}
}

func (t *Tracker) cmdRemove(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}

	args := commandArgs(m.Text)
	if args == "" {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Использование: /remove <ogn_id>",
		}); err != nil {
			slog.Error("failed to send usage", "err", err)
		}
		return
	}
	id := shortID(args)
	slog.Info("cmd /remove", "id", id, "user_id", m.From.ID)

	t.mu.Lock()
	s := t.session
	s.ChatID = m.Chat.ID
	delete(s.Tracking, id)
	t.updateFilter()
	kb := s.replyKeyboard()
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      m.Chat.ID,
		Text:        "Удалён " + id,
		ReplyMarkup: kb,
	}); err != nil {
		slog.Error("failed to confirm remove", "err", err)
	}
}

func (t *Tracker) cmdTrackOn(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}
	t.execTrackOn(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdTrackOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}
	t.askTrackOffConfirm(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdList(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}
	t.execList(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdStatus(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupChat(ctx, b, m) {
		return
	}

	t.mu.Lock()
	status := "выкл"
	if t.session != nil && t.session.TrackingOn {
		status = "вкл"
	}
	count := 0
	if t.session != nil {
		count = len(t.session.Tracking)
	}
	var kb *models.ReplyKeyboardMarkup
	if t.session != nil {
		kb = t.session.replyKeyboard()
	}
	t.mu.Unlock()

	text := fmt.Sprintf("Трекинг %s. Адресов: %d.", status, count)
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      m.Chat.ID,
		Text:        text,
		ReplyMarkup: kb,
	}); err != nil {
		slog.Error("failed to send status", "err", err)
	}
}

func (t *Tracker) cmdLanding(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}
	t.execLanding(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdTz(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}

	arg := commandArgs(m.Text)
	if arg == "" {
		t.mu.Lock()
		cur := t.tz().String()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   fmt.Sprintf("Часовой пояс: %s\nИспользование: /tz Europe/Kyiv", cur),
		}); err != nil {
			slog.Error("failed to send tz usage", "err", err)
		}
		return
	}

	loc, err := time.LoadLocation(arg)
	if err != nil {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Неизвестный часовой пояс. Используйте формат IANA, например: Europe/Kyiv, America/New_York, Asia/Tokyo",
		}); err != nil {
			slog.Error("failed to send tz error", "err", err)
		}
		return
	}

	t.mu.Lock()
	t.session.Timezone = loc
	t.saveState()
	t.mu.Unlock()
	slog.Info("timezone set", "tz", loc.String())

	now := time.Now().In(loc).Format("15:04:05")
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   fmt.Sprintf("Часовой пояс: %s (сейчас: %s)", loc.String(), now),
	}); err != nil {
		slog.Error("failed to confirm tz", "err", err)
	}
}

func (t *Tracker) cmdHelp(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}

	text := strings.Join([]string{
		"Групповые команды:",
		"/start — запуск / сброс бота",
		"/add <id> [имя] — добавить OGN адрес",
		"/add — добавить себя через личку",
		"/remove <id> — удалить из отслеживания",
		"/track_on — включить трекинг",
		"/track_off — выключить трекинг",
		"/landing — задать точку посадки",
		"/driver — стать водителем (live-локация)",
		"/driver_off — перестать быть водителем",
		"/area [радиус] — зона отслеживания (по умолчанию 100км)",
		"/area_off — отключить зону",
		"/tz [зона] — часовой пояс (например Europe/Kyiv)",
		"/list — список отслеживаемых",
		"/status — текущее состояние",
		"/session_reset — остановить и очистить всё",
		"",
		"Личные команды:",
		"/myid [id] — показать / задать свой OGN ID",
		"/confirm — подтвердить добавление текущего ID в группу",
		"",
		"/help — эта справка",
	}, "\n")
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   text,
	}); err != nil {
		slog.Error("failed to send help", "err", err)
	}
}

// cmdDebugWipe wipes all bot state. Registered only when DEBUG=1.
func (t *Tracker) cmdDebugWipe(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}

	t.mu.Lock()
	if t.session != nil {
		t.stopTrackingAsync()
		t.stopRadarAsync()
	}
	t.session = nil
	t.users = make(map[int64]*UserInfo)
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Все данные бота полностью очищены.",
		ReplyMarkup: &models.ReplyKeyboardRemove{
			RemoveKeyboard: true,
		},
	}); err != nil {
		slog.Error("failed to send wipe confirmation", "err", err)
	}
}

func (t *Tracker) cmdDriver(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}
	t.execDriver(ctx, b, m.Chat.ID, m.From.ID, m.From.Username)
}

func (t *Tracker) cmdDriverOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}
	t.execDriverOff(ctx, b, m.Chat.ID, m.From.ID)
}

func (t *Tracker) cmdArea(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}
	radius := defaultAreaRadius
	if arg := commandArgs(m.Text); arg != "" {
		if r, err := strconv.Atoi(arg); err == nil && r > 0 && r <= maxAreaRadius {
			radius = r
		}
	}
	t.execArea(ctx, b, m.Chat.ID, radius)
}

func (t *Tracker) cmdAreaOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}
	t.execAreaOff(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdRadar(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupSession(ctx, b, m) {
		return
	}
	var radius int
	if arg := commandArgs(m.Text); arg != "" {
		r, err := strconv.Atoi(arg)
		if err != nil {
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   fmt.Sprintf("Неверный радиус. Используйте /radar <число> (1–%d)", maxAreaRadius),
			}); err != nil {
				slog.Error("failed to send invalid radar arg message", "err", err)
			}
			return
		}
		if r <= 0 || r > maxAreaRadius {
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   fmt.Sprintf("Радиус должен быть от 1 до %d км", maxAreaRadius),
			}); err != nil {
				slog.Error("failed to send radar range message", "err", err)
			}
			return
		}
		radius = r
	}
	// If radar is already running and radius is specified, just change the radius.
	t.mu.Lock()
	radarOn := t.session != nil && t.session.RadarOn
	t.mu.Unlock()
	if radarOn && radius > 0 {
		t.execRadarSetRadius(ctx, b, m.Chat.ID, radius)
		return
	}
	t.execRadarOn(ctx, b, m.Chat.ID, radius)
}

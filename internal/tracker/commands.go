package tracker

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"ogn/client"
)

const (
	defaultAreaRadius = 100           // км по умолчанию для /area
	maxAreaRadius     = 500           // максимальный радиус зоны
	waitTimeout       = 2 * time.Minute // таймаут ожидания локации/водителя
	driverReminder    = 2 * time.Minute // напоминание водителю
)

// isTrusted checks whether a Telegram user is allowed to interact with the bot.
// Currently allows all users; will be restricted when multi-group support is added.
func (t *Tracker) isTrusted(userID int64) bool {
	return true
}

// requireGroupChat is a guard that replies with an error if called outside a group.
func (t *Tracker) requireGroupChat(ctx context.Context, b *bot.Bot, m *models.Message) bool {
	if isGroupChat(m.Chat) {
		return true
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Эта команда работает только в группе.",
	}); err != nil {
		log.Printf("failed to send group-only message: %v", err)
	}
	return false
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
			log.Printf("failed to send session required message: %v", err)
		}
	}
	return active
}

// requireGroupSession combines requireGroupChat + requireSession guards.
// Returns true if the command can proceed.
func (t *Tracker) requireGroupSession(ctx context.Context, b *bot.Bot, m *models.Message) bool {
	return t.requireGroupChat(ctx, b, m) && t.requireSession(ctx, b, m.Chat.ID)
}

// --- Command handlers ---

// cmdStart handles /start: in groups — creates a fresh session,
// in DMs — registers the user and processes deep-link payloads for /add flow.
func (t *Tracker) cmdStart(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	log.Printf("[cmd] /start from user=%d chat=%d type=%s", m.From.ID, m.Chat.ID, m.Chat.Type)

	if isPrivateChat(m.Chat) {
		t.cmdStartPrivate(ctx, b, m)
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
		t.session.stopTracking(t.aprs)
		t.session.stopRadar(t.aprs)
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
		log.Printf("failed to send start message: %v", err)
	}
}

// cmdStartPrivate handles /start in DMs.
// Deep link payload "add_<groupChatID>" initiates the OGN ID input flow.
func (t *Tracker) cmdStartPrivate(ctx context.Context, b *bot.Bot, m *models.Message) {
	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	// Check for deep link payload: /start add_<groupChatID>
	payload := commandArgs(m.Text)
	if strings.HasPrefix(payload, "add_") {
		groupIDStr := payload[4:]
		groupChatID, err := strconv.ParseInt(groupIDStr, 10, 64)
		if err != nil {
			t.saveState()
			t.mu.Unlock()
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   "Неверная ссылка.",
			}); err != nil {
				log.Printf("failed to send invalid deep link: %v", err)
			}
			return
		}

		u.PendingGroup = groupChatID
		hasOGNID := u.OGNID != ""
		ognID := u.OGNID
		t.saveState()
		t.mu.Unlock()

		if hasOGNID {
			text := fmt.Sprintf("Ваш OGN ID: %s\nОтправьте новый ID или /confirm чтобы использовать текущий.", ognID)
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   text,
			}); err != nil {
				log.Printf("failed to send DM with existing ID: %v", err)
			}
		} else {
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   "Отправьте ваш OGN ID (6-значный адрес трекера):",
			}); err != nil {
				log.Printf("failed to send DM ask for OGN ID: %v", err)
			}
		}
		return
	}

	// No deep link: just register DM.
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Бот готов. Используйте /myid чтобы задать свой OGN ID.",
	}); err != nil {
		log.Printf("failed to send private start message: %v", err)
	}
}

func (t *Tracker) cmdStartSession(ctx context.Context, b *bot.Bot, update *models.Update) {
	t.cmdStart(ctx, b, update)
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

func (t *Tracker) askSessionResetConfirm(ctx context.Context, b *bot.Bot, chatID int64) {
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "Сбросить", CallbackData: "session_reset_confirm"},
			},
			{
				{Text: "Сбросить и удалить пилотов", CallbackData: "session_reset_wipe"},
			},
			{
				{Text: "Отмена", CallbackData: "session_reset_cancel"},
			},
		},
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "⚠️ Сбросить сессию? Это действие необратимо.\n\n\"Сбросить\" — трекинг остановится, но добавленные пилоты сохранятся.\n\"Сбросить и удалить пилотов\" — полная очистка.",
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to send session reset confirmation: %v", err)
	}
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
			log.Printf("failed to send group-only message: %v", err)
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
			log.Printf("failed to confirm DM sent in group: %v", err)
		}
		return
	}

	// Can't send DM — show deep link button.
	log.Printf("failed to send DM to user %d, showing deep link: %v", m.From.ID, dmErr)
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
		log.Printf("failed to send deep link button: %v", err)
	}
}

// execAddDirect adds a pilot by OGN ID directly from /add <id> [name] [@username] in a group.
// If @username is provided, the bot links the pilot to that Telegram user for DM features.
// If only a name is provided without @username, the bot cannot send DM to the pilot.
func (t *Tracker) execAddDirect(ctx context.Context, b *bot.Bot, m *models.Message, args []string) {
	id := shortID(args[0])

	// Parse remaining args: name tokens and optional @username.
	var display string
	var pilotUsername string
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "@") {
			pilotUsername = strings.TrimPrefix(arg, "@")
		} else {
			if display != "" {
				display += " "
			}
			display += arg
		}
	}

	// Determine the username for display and linking.
	username := pilotUsername
	if username == "" && display == "" {
		// Legacy behavior: use adder's info when no name or @username given.
		username = m.From.Username
		if username == "" {
			username = strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
		}
	}

	log.Printf("[cmd] /add id=%s name=%q username=%q from user=%d", id, display, username, m.From.ID)

	t.mu.Lock()
	s := t.session
	s.ChatID = m.Chat.ID

	// Try to link to an existing user by @username.
	var ownerUID int64
	if pilotUsername != "" {
		for _, u := range t.users {
			if strings.EqualFold(u.Username, pilotUsername) {
				u.OGNID = id
				ownerUID = u.UserID
				break
			}
		}
	}

	if info, ok := s.Tracking[id]; ok {
		info.Name = display
		info.Username = username
		info.AutoDiscovered = false
		if ownerUID != 0 {
			info.OwnerUserID = ownerUID
		}
	} else {
		s.Tracking[id] = &TrackInfo{Name: display, Username: username, OwnerUserID: ownerUID}
	}
	t.updateFilter()

	var ddbInfo string
	if info := formatDDBInfo(t.devices, id); info != "" {
		ddbInfo = "\n📋 " + info
	}
	kb := s.replyKeyboard()
	t.saveState()
	t.mu.Unlock()

	text := "Добавлен " + id
	if display != "" {
		text += " (" + display + ")"
	}
	if pilotUsername != "" {
		text += " @" + pilotUsername
	}
	text += ddbInfo

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      m.Chat.ID,
		Text:        text,
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm add: %v", err)
	}
}

func (t *Tracker) cmdRemove(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}

	args := commandArgs(m.Text)
	if args == "" {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Использование: /remove <ogn_id>",
		}); err != nil {
			log.Printf("failed to send usage: %v", err)
		}
		return
	}
	id := shortID(args)
	log.Printf("[cmd] /remove id=%s from user=%d", id, m.From.ID)

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
		log.Printf("failed to confirm remove: %v", err)
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
		log.Printf("failed to send status: %v", err)
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
			log.Printf("failed to send tz usage: %v", err)
		}
		return
	}

	loc, err := time.LoadLocation(arg)
	if err != nil {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Неизвестный часовой пояс. Используйте формат IANA, например: Europe/Kyiv, America/New_York, Asia/Tokyo",
		}); err != nil {
			log.Printf("failed to send tz error: %v", err)
		}
		return
	}

	t.mu.Lock()
	t.session.Timezone = loc
	t.saveState()
	t.mu.Unlock()
	log.Printf("[tz] set to %s", loc.String())

	now := time.Now().In(loc).Format("15:04:05")
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   fmt.Sprintf("Часовой пояс: %s (сейчас: %s)", loc.String(), now),
	}); err != nil {
		log.Printf("failed to confirm tz: %v", err)
	}
}

func (t *Tracker) cmdMyID(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !isPrivateChat(m.Chat) {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Эта команда работает только в личке.",
		}); err != nil {
			log.Printf("failed to send private-only message: %v", err)
		}
		return
	}

	arg := commandArgs(m.Text)

	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	if arg == "" {
		// Show current OGN ID.
		ognID := u.OGNID
		t.mu.Unlock()
		if ognID == "" {
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   "OGN ID не задан. Используйте /myid <id>",
			}); err != nil {
				log.Printf("failed to send myid empty: %v", err)
			}
		} else {
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   fmt.Sprintf("Ваш OGN ID: %s", ognID),
			}); err != nil {
				log.Printf("failed to send myid value: %v", err)
			}
		}
		return
	}

	// Set new OGN ID.
	newID := shortID(arg)
	oldID := u.OGNID
	u.OGNID = newID

	// Update any TrackInfo entries owned by this user.
	s := t.session
	if s != nil && oldID != "" && oldID != newID {
		if info, ok := s.Tracking[oldID]; ok && info.OwnerUserID == u.UserID {
			delete(s.Tracking, oldID)
			info.Name = u.DisplayName
			info.Username = u.Username
			s.Tracking[newID] = info
			t.updateFilter()
		}
	}
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   fmt.Sprintf("OGN ID обновлён: %s", newID),
	}); err != nil {
		log.Printf("failed to confirm myid update: %v", err)
	}
}

func (t *Tracker) cmdConfirm(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !isPrivateChat(m.Chat) {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Эта команда работает только в личке.",
		}); err != nil {
			log.Printf("failed to send private-only message: %v", err)
		}
		return
	}

	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	if u.PendingGroup == 0 {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Нет ожидающей группы. Используйте /add в группе.",
		}); err != nil {
			log.Printf("failed to send no pending group: %v", err)
		}
		return
	}
	if u.OGNID == "" {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "OGN ID не задан. Отправьте ID сообщением или используйте /myid <id>.",
		}); err != nil {
			log.Printf("failed to send no ogn id: %v", err)
		}
		return
	}

	s := t.session
	if s == nil || s.ChatID != u.PendingGroup {
		u.PendingGroup = 0
		t.saveState()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Группа не найдена. Попросите добавить вас заново.",
		}); err != nil {
			log.Printf("failed to send pending group not found: %v", err)
		}
		return
	}

	id := u.OGNID
	name := u.DisplayName
	groupChatID := s.ChatID

	if info, ok := s.Tracking[id]; ok {
		info.Name = name
		info.Username = u.Username
		info.OwnerUserID = u.UserID
		info.AutoDiscovered = false
	} else {
		s.Tracking[id] = &TrackInfo{
			Name:        name,
			Username:    u.Username,
			OwnerUserID: u.UserID,
		}
	}

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
		log.Printf("failed to confirm in DM: %v", err)
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
		log.Printf("failed to confirm in group: %v", err)
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
		log.Printf("failed to send help: %v", err)
	}
}

// cmdDebugWipe полностью очищает все данные бота (для отладки).
func (t *Tracker) cmdDebugWipe(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}

	t.mu.Lock()
	if t.session != nil {
		t.session.stopTracking(t.aprs)
		t.session.stopRadar(t.aprs)
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
		log.Printf("failed to send wipe confirmation: %v", err)
	}
}

// execDMLanding initiates the landing flow from a private chat.
// Asks the pilot to send their location pin.
func (t *Tracker) execDMLanding(ctx context.Context, b *bot.Bot, m *models.Message) {
	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	s := t.session
	if s == nil || !s.TrackingOn || u.OGNID == "" {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Трекинг не активен или OGN ID не задан.",
		}); err != nil {
			log.Printf("failed to send DM landing unavailable: %v", err)
		}
		return
	}

	info, ok := s.Tracking[u.OGNID]
	if !ok || info.Status != StatusFlying {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Вы не в списке летающих пилотов.",
		}); err != nil {
			log.Printf("failed to send DM not flying: %v", err)
		}
		return
	}

	s.WaitingLanding = true
	s.LandingExpiry = time.Now().Add(waitTimeout)
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Отправьте точку посадки в течение 2 минут",
	}); err != nil {
		log.Printf("failed to request DM landing location: %v", err)
	}
}

// execDMConfirmLanding handles the "🪂 Сел" button in DM.
// Marks the pilot as landed with confirmation (no location pin required).
func (t *Tracker) execDMConfirmLanding(ctx context.Context, b *bot.Bot, m *models.Message) {
	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	s := t.session
	if s == nil || !s.TrackingOn || u.OGNID == "" {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Трекинг не активен или OGN ID не задан.",
		}); err != nil {
			log.Printf("failed to send DM confirm landing unavailable: %v", err)
		}
		return
	}

	info, ok := s.Tracking[u.OGNID]
	if !ok || info.Status != StatusFlying {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Вы не в списке летающих пилотов.",
		}); err != nil {
			log.Printf("failed to send DM not flying: %v", err)
		}
		return
	}

	info.Status = StatusLanded
	info.LandingTime = time.Now()
	info.LandingConfirmed = true

	var alert *landingEvent
	if info.Position != nil {
		alert = &landingEvent{
			id:   u.OGNID,
			name: info.DisplayName(),
			lat:  info.Position.Latitude,
			lon:  info.Position.Longitude,
			alt:  info.Position.Altitude,
			time: info.LandingTime,
			tz:   s.tz(),
		}
	}
	chatID := s.ChatID
	dmKb := t.dmReplyKeyboard(u.UserID)
	t.saveState()
	t.mu.Unlock()

	// Confirm in DM.
	params := &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "🪂 Посадка подтверждена",
	}
	if dmKb != nil {
		params.ReplyMarkup = dmKb
	} else {
		params.ReplyMarkup = &models.ReplyKeyboardRemove{RemoveKeyboard: true}
	}
	if _, err := b.SendMessage(ctx, params); err != nil {
		log.Printf("failed to confirm DM landing: %v", err)
	}

	// Notify the group.
	if alert != nil {
		t.sendLandingAlert(alert, chatID)
	} else {
		// No position data — send a simple text notification.
		label := u.OGNID
		if name := info.DisplayName(); name != "" {
			label = name
		}
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   fmt.Sprintf("🪂 %s сел! (подтверждено пилотом)", label),
		}); err != nil {
			log.Printf("failed to send landing notification for %s: %v", u.OGNID, err)
		}
	}
}

// handleDMLanding processes a location sent in DM during the landing flow.
// Sets the landing point in the group session and marks the sender as landed.
func (t *Tracker) handleDMLanding(ctx context.Context, b *bot.Bot, m *models.Message) {
	loc := m.Location

	t.mu.Lock()
	s := t.session
	if s == nil {
		t.mu.Unlock()
		return
	}

	if !s.WaitingLanding || !time.Now().Before(s.LandingExpiry) {
		t.mu.Unlock()
		return
	}

	log.Printf("[landing/DM] location set at %.5f,%.5f by user=%d", loc.Latitude, loc.Longitude, m.From.ID)
	s.Landing = &Coordinates{Latitude: loc.Latitude, Longitude: loc.Longitude}
	s.WaitingLanding = false

	// Mark the sender as landed.
	var landedName string
	if u, ok := t.users[m.From.ID]; ok && u.OGNID != "" {
		if info, ok := s.Tracking[u.OGNID]; ok && info.Status == StatusFlying {
			info.Status = StatusLanded
			info.LandingTime = time.Now()
			info.LandingConfirmed = true
			landedName = info.DisplayName()
			log.Printf("[landing/DM] marked %s as landed (user=%d)", u.OGNID, m.From.ID)
		}
	}

	groupChatID := s.ChatID
	groupKb := s.replyKeyboard()
	dmKb := t.dmReplyKeyboard(m.From.ID)
	t.saveState()
	t.mu.Unlock()

	// Confirm in DM.
	dmText := "Точка посадки сохранена"
	if landedName != "" {
		dmText += fmt.Sprintf("\n🪂 %s отмечен как севший", landedName)
	}
	params := &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   dmText,
	}
	if dmKb != nil {
		params.ReplyMarkup = dmKb
	} else {
		params.ReplyMarkup = &models.ReplyKeyboardRemove{RemoveKeyboard: true}
	}
	if _, err := b.SendMessage(ctx, params); err != nil {
		log.Printf("failed to confirm DM landing: %v", err)
	}

	// Notify the group.
	groupText := "📍 Точка посадки обновлена"
	if landedName != "" {
		groupText += fmt.Sprintf("\n🪂 %s сел", landedName)
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      groupChatID,
		Text:        groupText,
		ReplyMarkup: groupKb,
	}); err != nil {
		log.Printf("failed to notify group about DM landing: %v", err)
	}
}

// handleLocation dispatches an incoming location message to the appropriate handler:
// driver (live location), landing point, or area center — depending on what's being awaited.
func (t *Tracker) handleLocation(ctx context.Context, b *bot.Bot, m *models.Message) {
	loc := m.Location

	t.mu.Lock()
	s := t.session
	if s == nil {
		t.mu.Unlock()
		return
	}

	// Driver: check if this user is waiting.
	if d, ok := s.Drivers[m.From.ID]; ok && d.Waiting && time.Now().Before(d.Expiry) {
		if loc.LivePeriod > 0 {
			log.Printf("[driver] live location received from user=%d at %.5f,%.5f", m.From.ID, loc.Latitude, loc.Longitude)
			d.Pos = &Coordinates{Latitude: loc.Latitude, Longitude: loc.Longitude}
			d.MsgID = m.ID
			d.Waiting = false
			kb := s.replyKeyboard()
			t.mu.Unlock()
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:      m.Chat.ID,
				Text:        "🚗 Водитель активен. Расстояния будут в сводке.",
				ReplyMarkup: kb,
			}); err != nil {
				log.Printf("failed to confirm driver location: %v", err)
			}
			return
		}
		// Static pin — use as temporary position, keep waiting for live.
		d.Pos = &Coordinates{Latitude: loc.Latitude, Longitude: loc.Longitude}
		kb := s.replyKeyboard()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      m.Chat.ID,
			Text:        "📍 Позиция принята. Для непрерывного отслеживания отправьте live-локацию.",
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to send driver static hint: %v", err)
		}
		return
	}

	// Landing: expecting a static location pin.
	if s.WaitingLanding && time.Now().Before(s.LandingExpiry) {
		log.Printf("[landing] location set at %.5f,%.5f by user=%d", loc.Latitude, loc.Longitude, m.From.ID)
		s.Landing = &Coordinates{Latitude: loc.Latitude, Longitude: loc.Longitude}
		s.WaitingLanding = false

		// Mark the sender as landed if they have a tracked OGN ID.
		var landedName string
		if u, ok := t.users[m.From.ID]; ok && u.OGNID != "" {
			if info, ok := s.Tracking[u.OGNID]; ok && info.Status == StatusFlying {
				info.Status = StatusLanded
				info.LandingTime = time.Now()
				landedName = info.DisplayName()
				log.Printf("[landing] marked %s as landed (user=%d)", u.OGNID, m.From.ID)
			}
		}

		kb := s.replyKeyboard()
		t.saveState()
		t.mu.Unlock()

		text := "Точка посадки сохранена"
		if landedName != "" {
			text += fmt.Sprintf("\n🪂 %s отмечен как севший", landedName)
		}
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      m.Chat.ID,
			Text:        text,
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to confirm landing location: %v", err)
		}
		return
	}

	// Area: expecting center location.
	if s.WaitingArea && time.Now().Before(s.AreaExpiry) {
		log.Printf("[area] center set at %.5f,%.5f radius=%dkm by user=%d", loc.Latitude, loc.Longitude, s.TrackAreaRadius, m.From.ID)
		s.TrackArea = &Coordinates{Latitude: loc.Latitude, Longitude: loc.Longitude}
		s.WaitingArea = false
		// Remove previously auto-discovered entries when area changes.
		for id, info := range s.Tracking {
			if info.AutoDiscovered {
				delete(s.Tracking, id)
			}
		}
		t.updateFilter()
		radius := s.TrackAreaRadius
		kb := s.replyKeyboard()
		t.saveState()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      m.Chat.ID,
			Text:        fmt.Sprintf("📡 Зона активна: радиус %dкм", radius),
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to confirm area location: %v", err)
		}
		return
	}

	t.mu.Unlock()
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
	if !t.requireGroupChat(ctx, b, m) {
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
	if !t.requireGroupChat(ctx, b, m) {
		return
	}
	t.execAreaOff(ctx, b, m.Chat.ID)
}

// --- Core logic used by both command and callback handlers ---

// execSessionReset stops tracking and creates a new session.
// If wipePilots is false, the pilot list is preserved for quick restart.
func (t *Tracker) execSessionReset(ctx context.Context, b *bot.Bot, chatID int64, wipePilots bool) {
	log.Printf("[session] reset chat=%d wipePilots=%v", chatID, wipePilots)
	t.mu.Lock()
	if t.session != nil {
		t.session.stopTracking(t.aprs)
		t.session.stopRadar(t.aprs)
	}
	newSession := &GroupSession{
		ChatID:   chatID,
		Tracking: make(map[string]*TrackInfo),
		Drivers:  make(map[int64]*DriverInfo),
	}
	// Keep existing pilots unless explicitly wiping.
	if !wipePilots && t.session != nil {
		for id, info := range t.session.Tracking {
			newSession.Tracking[id] = &TrackInfo{
				Name:        info.Name,
				Username:    info.Username,
				OwnerUserID: info.OwnerUserID,
			}
		}
	}
	t.session = newSession
	t.updateFilter()
	t.saveState()
	var markup models.ReplyMarkup
	if wipePilots {
		markup = &models.ReplyKeyboardRemove{RemoveKeyboard: true}
	} else {
		markup = t.session.replyKeyboard()
	}
	t.mu.Unlock()

	text := "Сессия сброшена. Пилоты сохранены."
	if wipePilots {
		text = "Сессия сброшена. Все пилоты удалены. Используйте /start для начала."
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: markup,
	}); err != nil {
		log.Printf("failed to send session_reset message: %v", err)
	}
}

// execTrackOn starts live tracking: resets pilot statuses, connects to OGN APRS,
// and launches the client + update goroutines.
func (t *Tracker) execTrackOn(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	s := t.session
	if len(s.Tracking) == 0 && s.TrackArea == nil {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "Нет адресов. Используйте /add <id> или /area.",
		}); err != nil {
			log.Printf("failed to send no addresses message: %v", err)
		}
		return
	}
	if s.RadarOn {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "Остановите радар перед запуском трекинга.",
		}); err != nil {
			log.Printf("failed to send radar conflict message: %v", err)
		}
		return
	}
	if s.TrackingOn {
		kb := s.replyKeyboard()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      chatID,
			Text:        "Трекинг уже включён",
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to confirm track_on: %v", err)
		}
		return
	}
	// Reset pilot statuses for a fresh tracking session.
	for _, info := range s.Tracking {
		info.Status = StatusFlying
		info.LandingTime = time.Time{}
		info.LowSpeedSince = time.Time{}
		info.MessageID = 0
		info.Position = nil
		info.LastUpdate = time.Time{}
	}
	s.SummaryMsgID = 0
	s.ChatID = chatID
	// Set filter before enabling tracking so updateFilter doesn't restart goroutines.
	t.updateFilter()
	// Create a fresh APRS client — previous Disconnect() sets killed=true permanently.
	t.aprs = client.New("N0CALL", t.aprs.Filter)
	t.aprs.Logger = log.Default()
	s.TrackingOn = true
	s.StopCh = make(chan struct{})
	stopCh := s.StopCh
	kb := s.replyKeyboard()
	t.saveState()
	count := len(s.Tracking)
	hasArea := s.TrackArea != nil
	t.mu.Unlock()

	log.Printf("[tracking] ON: %d pilots, area=%v, chat=%d", count, hasArea, chatID)
	go t.runClient(stopCh)
	go t.sendUpdates(stopCh)

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "Трекинг включён",
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm track_on: %v", err)
	}
}

// askStartChoice shows inline buttons to resume with existing pilots or start fresh.
func (t *Tracker) askStartChoice(ctx context.Context, b *bot.Bot, chatID int64) {
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "Продолжить", CallbackData: "start_resume"},
				{Text: "Новая сессия", CallbackData: "start_fresh"},
			},
		},
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "Есть пилоты из прошлой сессии. Продолжить или начать новую?",
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to send start choice: %v", err)
	}
}

// askTrackOffConfirm shows a confirmation prompt before stopping tracking.
func (t *Tracker) askTrackOffConfirm(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	s := t.session
	if !s.TrackingOn {
		kb := s.replyKeyboard()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      chatID,
			Text:        "Трекинг уже выключен",
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to confirm track_off: %v", err)
		}
		return
	}
	t.mu.Unlock()

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "Остановить", CallbackData: "track_off_confirm"},
				{Text: "Отмена", CallbackData: "track_off_cancel"},
			},
		},
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "Остановить трекинг?",
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to send track_off confirmation: %v", err)
	}
}

func (t *Tracker) execTrackOff(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	s := t.session
	if !s.TrackingOn {
		t.mu.Unlock()
		return
	}
	s.stopTracking(t.aprs)
	kb := s.replyKeyboard()
	t.saveState()
	t.mu.Unlock()
	log.Printf("[tracking] OFF chat=%d", chatID)

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "Трекинг выключен",
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm track_off: %v", err)
	}
}

func (t *Tracker) execList(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	s := t.session
	var entries []string
	for id, info := range s.Tracking {
		entry := info.StatusEmoji() + " " + id
		if info.Name != "" {
			entry += " — " + info.Name
			if info.Username != "" {
				entry += " (" + info.Username + ")"
			}
		} else if info.Username != "" {
			entry += " — " + info.Username
		}
		if info.Status == StatusLanded && !info.LandingTime.IsZero() {
			entry += fmt.Sprintf(" (сел %s)", info.LandingTime.In(t.tz()).Format("15:04"))
		}
		if info.Status == StatusPickedUp {
			entry += " (забран)"
		}
		if info := formatDDBInfo(t.devices, id); info != "" {
			entry += " [" + info + "]"
		}
		entries = append(entries, entry)
	}
	track := "выкл"
	if s.TrackingOn {
		track = "вкл"
	}

	// Copy tracking map for pilotButtons (still under lock).
	localCopy := make(map[string]*TrackInfo, len(s.Tracking))
	for id, info := range s.Tracking {
		cp := *info
		localCopy[id] = &cp
	}
	t.mu.Unlock()

	// Only contextual inline buttons (navigate + pickup per pilot).
	var replyMarkup models.ReplyMarkup
	if navKb := pilotButtons(localCopy); navKb != nil {
		replyMarkup = navKb
	}

	list := strings.Join(entries, "\n")
	if list == "" {
		list = "нет"
	}
	text := "Трекинг: " + track + "\n" + list
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: replyMarkup,
	}); err != nil {
		log.Printf("failed to send list: %v", err)
	}
}

func (t *Tracker) execLanding(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	s := t.session
	s.WaitingLanding = true
	s.LandingExpiry = time.Now().Add(waitTimeout)
	s.ChatID = chatID
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   "Отправьте точку посадки в течение 2 минут",
	}); err != nil {
		log.Printf("failed to request landing location: %v", err)
	}
}

func (t *Tracker) execArea(ctx context.Context, b *bot.Bot, chatID int64, radiusKm int) {
	t.mu.Lock()
	s := t.session
	s.WaitingArea = true
	s.AreaExpiry = time.Now().Add(waitTimeout)
	s.TrackAreaRadius = radiusKm
	s.ChatID = chatID
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   fmt.Sprintf("Отправьте центр зоны (радиус %dкм) в течение 2 минут", radiusKm),
	}); err != nil {
		log.Printf("failed to request area location: %v", err)
	}
}

func (t *Tracker) execAreaOff(ctx context.Context, b *bot.Bot, chatID int64) {
	log.Printf("[area] off chat=%d", chatID)
	t.mu.Lock()
	s := t.session
	// Stop radar if it's running — radar requires an area.
	if s.RadarOn {
		s.stopRadar(t.aprs)
		t.aprs = client.New("N0CALL", "")
		t.aprs.Logger = log.Default()
	}
	s.TrackArea = nil
	s.WaitingArea = false
	// Remove auto-discovered entries.
	for id, info := range s.Tracking {
		if info.AutoDiscovered {
			delete(s.Tracking, id)
		}
	}
	t.updateFilter()
	kb := s.replyKeyboard()
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "📡 Зона отключена",
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm area off: %v", err)
	}
}

// --- Radar mode commands ---

func (t *Tracker) execRadarOn(ctx context.Context, b *bot.Bot, chatID int64, radiusKm int) {
	t.mu.Lock()
	s := t.session
	if s.TrackArea == nil {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "Сначала задайте зону: /area",
		}); err != nil {
			log.Printf("failed to send radar no-area message: %v", err)
		}
		return
	}
	if s.TrackingOn {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "Остановите трекинг перед включением радара.",
		}); err != nil {
			log.Printf("failed to send radar conflict message: %v", err)
		}
		return
	}
	if s.RadarOn {
		kb := s.replyKeyboard()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      chatID,
			Text:        "Радар уже включён",
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to confirm radar on: %v", err)
		}
		return
	}

	// Use provided radius, fall back to area radius, then default.
	if radiusKm <= 0 {
		radiusKm = s.TrackAreaRadius
	}
	if radiusKm <= 0 {
		radiusKm = defaultAreaRadius
	}
	if radiusKm > maxAreaRadius {
		radiusKm = maxAreaRadius
	}

	s.RadarOn = true
	s.RadarRadius = radiusKm
	s.RadarEntries = make(map[string]*RadarEntry)
	s.RadarMsgID = 0

	filter := client.RangeFilter(s.TrackArea.Latitude, s.TrackArea.Longitude, radiusKm)
	t.aprs = client.New("N0CALL", filter)
	t.aprs.Logger = log.Default()
	s.RadarStopCh = make(chan struct{})
	stopCh := s.RadarStopCh
	kb := s.replyKeyboard()
	t.mu.Unlock()

	log.Printf("[radar] ON: area=%.5f,%.5f r=%dkm chat=%d",
		s.TrackArea.Latitude, s.TrackArea.Longitude, radiusKm, chatID)
	go t.runRadarClient(stopCh)
	go t.sendRadarUpdates(stopCh)

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        fmt.Sprintf("📡 Радар включён (зона %dкм)", radiusKm),
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm radar on: %v", err)
	}
}

func (t *Tracker) execRadarOff(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	s := t.session
	if !s.RadarOn {
		t.mu.Unlock()
		return
	}
	s.stopRadar(t.aprs)
	t.aprs = client.New("N0CALL", "")
	t.aprs.Logger = log.Default()
	kb := s.replyKeyboard()
	t.mu.Unlock()

	log.Printf("[radar] OFF chat=%d", chatID)
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "📡 Радар выключен",
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm radar off: %v", err)
	}
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
				log.Printf("failed to send invalid radar arg message: %v", err)
			}
			return
		}
		if r <= 0 || r > maxAreaRadius {
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   fmt.Sprintf("Радиус должен быть от 1 до %d км", maxAreaRadius),
			}); err != nil {
				log.Printf("failed to send radar range message: %v", err)
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

// execRadarAskRadius prompts the user to enter a new radius for radar mode.
func (t *Tracker) execRadarAskRadius(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	s := t.session
	if s == nil || !s.RadarOn {
		t.mu.Unlock()
		return
	}
	s.WaitingRadarRadius = true
	s.RadarRadiusExpiry = time.Now().Add(waitTimeout)
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   fmt.Sprintf("Введите радиус в км (1–%d). Текущий: %dкм", maxAreaRadius, s.RadarRadius),
	}); err != nil {
		log.Printf("failed to send radar radius prompt: %v", err)
	}
}

// execRadarSetRadius changes the radar radius while radar is running.
func (t *Tracker) execRadarSetRadius(ctx context.Context, b *bot.Bot, chatID int64, radiusKm int) {
	if radiusKm <= 0 {
		radiusKm = defaultAreaRadius
	}
	if radiusKm > maxAreaRadius {
		radiusKm = maxAreaRadius
	}
	t.mu.Lock()
	s := t.session
	if s == nil || !s.RadarOn {
		t.mu.Unlock()
		return
	}
	s.stopRadar(t.aprs)

	s.RadarOn = true
	s.RadarRadius = radiusKm
	s.RadarEntries = make(map[string]*RadarEntry)
	s.RadarMsgID = 0

	filter := client.RangeFilter(s.TrackArea.Latitude, s.TrackArea.Longitude, radiusKm)
	t.aprs = client.New("N0CALL", filter)
	t.aprs.Logger = log.Default()
	s.RadarStopCh = make(chan struct{})
	stopCh := s.RadarStopCh
	kb := s.replyKeyboard()
	t.mu.Unlock()

	log.Printf("[radar] radius changed to %dkm chat=%d", radiusKm, chatID)
	go t.runRadarClient(stopCh)
	go t.sendRadarUpdates(stopCh)

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        fmt.Sprintf("📡 Радиус радара: %dкм", radiusKm),
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm radar radius: %v", err)
	}
}

func (t *Tracker) execDriver(ctx context.Context, b *bot.Bot, chatID int64, userID int64, username string) {
	t.mu.Lock()
	s := t.session
	if d, ok := s.Drivers[userID]; ok && d.MsgID != 0 {
		kb := s.replyKeyboard()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      chatID,
			Text:        "🚗 Вы уже водитель. /driver_off чтобы остановить.",
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to send driver active message: %v", err)
		}
		return
	}

	gen := 1
	if existing, ok := s.Drivers[userID]; ok {
		gen = existing.WaitGen + 1
	}
	s.Drivers[userID] = &DriverInfo{
		Waiting: true,
		Expiry:  time.Now().Add(waitTimeout),
		WaitGen: gen,
	}
	s.ChatID = chatID
	t.mu.Unlock()
	log.Printf("[driver] waiting for location from user=%d @%s", userID, username)

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   "Отправьте live-локацию в течение 2 минут, чтобы стать водителем.",
	}); err != nil {
		log.Printf("failed to send driver prompt: %v", err)
	}

	go t.driverWaitTimeout(gen, userID, chatID, username)
}

// driverWaitTimeout sends a reminder after 2 minutes and cleans up after 4 minutes
// if the driver hasn't sent a live location. Uses WaitGen to avoid acting on stale requests.
func (t *Tracker) driverWaitTimeout(gen int, userID int64, chatID int64, username string) {
	time.Sleep(driverReminder)

	t.mu.Lock()
	if t.session == nil {
		t.mu.Unlock()
		return
	}
	d, ok := t.session.Drivers[userID]
	if !ok || d.WaitGen != gen || !d.Waiting {
		t.mu.Unlock()
		return
	}
	d.Expiry = time.Now().Add(waitTimeout)
	b := t.bot
	t.mu.Unlock()

	if b == nil {
		return
	}

	var mention string
	if username != "" {
		mention = "@" + username
	} else {
		mention = fmt.Sprintf(`<a href="tg://user?id=%d">водитель</a>`, userID)
	}
	text := fmt.Sprintf("⏰ %s, отправьте live-локацию в течение 2 минут.", mention)

	ctx := context.Background()
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	}); err != nil {
		log.Printf("failed to send driver reminder: %v", err)
	}

	time.Sleep(driverReminder)

	t.mu.Lock()
	if t.session == nil {
		t.mu.Unlock()
		return
	}
	d, ok = t.session.Drivers[userID]
	if !ok || d.WaitGen != gen || !d.Waiting {
		t.mu.Unlock()
		return
	}
	d.Waiting = false
	if d.Pos == nil {
		delete(t.session.Drivers, userID)
	}
	t.mu.Unlock()
	log.Printf("driver wait timed out for user %d", userID)
}

func (t *Tracker) execDriverOff(ctx context.Context, b *bot.Bot, chatID int64, userID int64) {
	log.Printf("[driver] off user=%d", userID)
	t.mu.Lock()
	s := t.session
	_, was := s.Drivers[userID]
	delete(s.Drivers, userID)
	kb := s.replyKeyboard()
	t.mu.Unlock()

	text := "🚗 Вы не водитель"
	if was {
		text = "🚗 Водитель отключён"
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm driver off: %v", err)
	}
}

// execPickup marks a pilot as picked up (StatusPickedUp) and confirms in the group.
func (t *Tracker) execPickup(ctx context.Context, b *bot.Bot, id string) {
	log.Printf("[pickup] id=%s", id)
	t.mu.Lock()
	s := t.session
	if s == nil {
		t.mu.Unlock()
		return
	}
	chatID := s.ChatID
	info, ok := s.Tracking[id]
	if ok {
		info.Status = StatusPickedUp
	}
	if ok {
		t.saveState()
	}
	t.mu.Unlock()

	if !ok {
		return
	}

	label := id
	if name := info.DisplayName(); name != "" {
		label = name
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   fmt.Sprintf("✅ %s забран", label),
	}); err != nil {
		log.Printf("failed to confirm pickup for %s: %v", id, err)
	}
}

// --- Callback query handlers for inline buttons ---

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
		t.session.stopTracking(t.aprs)
		t.session.stopRadar(t.aprs)
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

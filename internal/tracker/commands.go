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
)

func (t *Tracker) isTrusted(userID int64) bool {
	return true
}

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

// --- Command handlers ---

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

	// Group chat: always create a fresh empty session.
	t.mu.Lock()
	if t.session != nil {
		t.session.stopTracking(t.aprs)
	}
	t.session = &GroupSession{
		ChatID:   m.Chat.ID,
		Tracking: make(map[string]*TrackInfo),
		Drivers:  make(map[int64]*DriverInfo),
	}
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Сессия начата. Используйте /add <id> или /area.",
		ReplyMarkup: &models.ReplyKeyboardRemove{
			RemoveKeyboard: true,
		},
	}); err != nil {
		log.Printf("failed to send start message: %v", err)
	}
}

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
	if !t.requireGroupChat(ctx, b, m) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
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

func (t *Tracker) execAddDirect(ctx context.Context, b *bot.Bot, m *models.Message, args []string) {
	id := shortID(args[0])
	display := strings.Join(args[1:], " ")
	log.Printf("[cmd] /add id=%s name=%q from user=%d", id, display, m.From.ID)
	username := m.From.Username
	if username == "" {
		username = strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	}

	t.mu.Lock()
	s := t.session
	s.ChatID = m.Chat.ID
	if info, ok := s.Tracking[id]; ok {
		info.Name = display
		info.Username = username
		info.AutoDiscovered = false
	} else {
		s.Tracking[id] = &TrackInfo{Name: display, Username: username}
	}
	t.updateFilter()

	// DDB lookup for device info.
	var ddbInfo string
	if t.devices != nil {
		if dev, ok := t.devices[id]; ok {
			var parts []string
			if dev.AircraftModel != "" {
				parts = append(parts, dev.AircraftModel)
			}
			if dev.Registration != "" {
				parts = append(parts, dev.Registration)
			}
			if dev.CN != "" {
				parts = append(parts, "CN:"+dev.CN)
			}
			if len(parts) > 0 {
				ddbInfo = "\n📋 " + strings.Join(parts, " | ")
			}
		}
	}
	kb := s.replyKeyboard()
	t.saveState()
	t.mu.Unlock()

	text := "Добавлен " + id
	if display != "" {
		text += " (" + display + ")"
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
	if !t.requireGroupChat(ctx, b, m) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}
	t.execTrackOn(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdTrackOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupChat(ctx, b, m) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}
	t.askTrackOffConfirm(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdList(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireGroupChat(ctx, b, m) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
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
	if !t.requireGroupChat(ctx, b, m) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
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
	t.saveState()
	t.mu.Unlock()

	// Confirm in DM.
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   fmt.Sprintf("Добавлен %s в группу", id),
	}); err != nil {
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

// cmdDebugWipe полностью очищает все данные бота. TODO: удалить после отладки.
func (t *Tracker) cmdDebugWipe(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}

	t.mu.Lock()
	if t.session != nil {
		t.session.stopTracking(t.aprs)
	}
	t.session = nil
	t.users = make(map[int64]*UserInfo)
	t.saveState()
	t.mu.Unlock()

	clearStateFile()

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
	if !t.requireGroupChat(ctx, b, m) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
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
	if !t.requireGroupChat(ctx, b, m) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}
	radius := 100
	if arg := commandArgs(m.Text); arg != "" {
		if r, err := strconv.Atoi(arg); err == nil && r > 0 && r <= 500 {
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

func (t *Tracker) execSessionReset(ctx context.Context, b *bot.Bot, chatID int64, wipePilots bool) {
	log.Printf("[session] reset chat=%d wipePilots=%v", chatID, wipePilots)
	t.mu.Lock()
	if t.session != nil {
		t.session.stopTracking(t.aprs)
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
	t.mu.Unlock()

	text := "Сессия сброшена. Пилоты сохранены."
	if wipePilots {
		text = "Сессия сброшена. Все пилоты удалены. Используйте /add для начала."
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	}); err != nil {
		log.Printf("failed to send session_reset message: %v", err)
	}
}

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
	s.TrackingOn = true
	s.StopCh = make(chan struct{})
	stopCh := s.StopCh
	t.updateFilter()
	s.ChatID = chatID
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
		entry := info.Status.Emoji() + " " + id
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
		if t.devices != nil {
			if dev, ok := t.devices[id]; ok {
				var parts []string
				if dev.AircraftModel != "" {
					parts = append(parts, dev.AircraftModel)
				}
				if dev.Registration != "" {
					parts = append(parts, dev.Registration)
				}
				if len(parts) > 0 {
					entry += " [" + strings.Join(parts, ", ") + "]"
				}
			}
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
	s.LandingExpiry = time.Now().Add(2 * time.Minute)
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
	s.AreaExpiry = time.Now().Add(2 * time.Minute)
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
		Expiry:  time.Now().Add(2 * time.Minute),
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

func (t *Tracker) driverWaitTimeout(gen int, userID int64, chatID int64, username string) {
	time.Sleep(2 * time.Minute)

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
	d.Expiry = time.Now().Add(2 * time.Minute)
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

	time.Sleep(2 * time.Minute)

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

func (t *Tracker) answerCallback(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	if _, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cq.ID,
	}); err != nil {
		log.Printf("failed to answer callback query: %v", err)
	}
}

func (t *Tracker) cbTrackOn(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	t.execTrackOn(ctx, b, chatID)
}

func (t *Tracker) cbTrackOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	t.askTrackOffConfirm(ctx, b, chatID)
}

func (t *Tracker) cbList(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	t.execList(ctx, b, chatID)
}

func (t *Tracker) cbLanding(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	t.execLanding(ctx, b, chatID)
}

func (t *Tracker) cbDriver(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	t.execDriver(ctx, b, chatID, cq.From.ID, cq.From.Username)
}

func (t *Tracker) cbDriverOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	t.execDriverOff(ctx, b, chatID, cq.From.ID)
}

func (t *Tracker) cbArea(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := int64(0)
	radius := 100
	if t.session != nil {
		chatID = t.session.ChatID
		radius = t.session.TrackAreaRadius
	}
	t.mu.Unlock()
	if radius == 0 {
		radius = 100
	}
	t.execArea(ctx, b, chatID, radius)
}

func (t *Tracker) cbAreaOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	t.execAreaOff(ctx, b, chatID)
}

func (t *Tracker) cbSessionReset(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	if chatID != 0 {
		t.askSessionResetConfirm(ctx, b, chatID)
	}
}

func (t *Tracker) cbSessionResetConfirm(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	// Remove the confirmation message.
	if cq.Message.Message != nil {
		b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    cq.Message.Message.Chat.ID,
			MessageID: cq.Message.Message.ID,
		})
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	if chatID != 0 {
		t.execSessionReset(ctx, b, chatID, false)
	}
}

func (t *Tracker) cbSessionResetWipe(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	if cq.Message.Message != nil {
		b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    cq.Message.Message.Chat.ID,
			MessageID: cq.Message.Message.ID,
		})
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	if chatID != 0 {
		t.execSessionReset(ctx, b, chatID, true)
	}
}

func (t *Tracker) cbSessionResetCancel(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if cq.Message.Message != nil {
		b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    cq.Message.Message.Chat.ID,
			MessageID: cq.Message.Message.ID,
		})
	}
}

func (t *Tracker) cbTrackOffConfirm(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	if cq.Message.Message != nil {
		b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    cq.Message.Message.Chat.ID,
			MessageID: cq.Message.Message.ID,
		})
	}
	t.mu.Lock()
	chatID := int64(0)
	if t.session != nil {
		chatID = t.session.ChatID
	}
	t.mu.Unlock()
	if chatID != 0 {
		t.execTrackOff(ctx, b, chatID)
	}
}

func (t *Tracker) cbTrackOffCancel(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if cq.Message.Message != nil {
		b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    cq.Message.Message.Chat.ID,
			MessageID: cq.Message.Message.ID,
		})
	}
}

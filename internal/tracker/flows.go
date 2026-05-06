package tracker

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"ogn/client"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

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

// askSessionResetConfirm shows the inline confirm/wipe/cancel dialog for /session_reset.
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

// execSessionReset stops tracking and creates a new session.
// If wipePilots is false, the pilot list is preserved for quick restart.
func (t *Tracker) execSessionReset(ctx context.Context, b *bot.Bot, chatID int64, wipePilots bool) {
	log.Printf("[session] reset chat=%d wipePilots=%v", chatID, wipePilots)
	t.mu.Lock()
	if t.session != nil {
		t.stopTrackingAsync()
		t.stopRadarAsync()
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
	aprs := t.aprs
	kb := s.replyKeyboard()
	t.saveState()
	count := len(s.Tracking)
	hasArea := s.TrackArea != nil
	t.mu.Unlock()

	log.Printf("[tracking] ON: %d pilots, area=%v, chat=%d", count, hasArea, chatID)
	go t.runClient(stopCh, aprs)
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
	t.stopTrackingAsync()
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
		t.stopRadarAsync()
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

// --- Radar mode flows ---

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
	aprs := t.aprs
	kb := s.replyKeyboard()
	areaLat, areaLon := s.TrackArea.Latitude, s.TrackArea.Longitude
	t.mu.Unlock()

	log.Printf("[radar] ON: area=%.5f,%.5f r=%dkm chat=%d", areaLat, areaLon, radiusKm, chatID)
	go t.runRadarClient(stopCh, aprs)
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
	t.stopRadarAsync()
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
	t.stopRadarAsync()

	s.RadarOn = true
	s.RadarRadius = radiusKm
	s.RadarEntries = make(map[string]*RadarEntry)
	s.RadarMsgID = 0

	filter := client.RangeFilter(s.TrackArea.Latitude, s.TrackArea.Longitude, radiusKm)
	t.aprs = client.New("N0CALL", filter)
	t.aprs.Logger = log.Default()
	s.RadarStopCh = make(chan struct{})
	stopCh := s.RadarStopCh
	aprs := t.aprs
	kb := s.replyKeyboard()
	t.mu.Unlock()

	log.Printf("[radar] radius changed to %dkm chat=%d", radiusKm, chatID)
	go t.runRadarClient(stopCh, aprs)
	go t.sendRadarUpdates(stopCh)

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        fmt.Sprintf("📡 Радиус радара: %dкм", radiusKm),
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm radar radius: %v", err)
	}
}

// --- Driver flow ---

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

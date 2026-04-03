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

func (t *Tracker) requireSession(ctx context.Context, b *bot.Bot, chatID int64) bool {
	t.mu.Lock()
	active := t.sessionActive
	t.mu.Unlock()
	if !active {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "Run /start first",
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

	t.mu.Lock()
	t.chatID = m.Chat.ID
	t.stopTracking()
	t.tracking = make(map[string]*TrackInfo)
	t.sessionActive = true
	t.landing = nil
	t.waitingLanding = false
	t.summaryMsgID = 0
	t.drivers = make(map[int64]*DriverInfo)
	t.trackArea = nil
	t.waitingArea = false
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Session started. Use /add <id> or /area to start tracking.",
		ReplyMarkup: &models.ReplyKeyboardRemove{
			RemoveKeyboard: true,
		},
	}); err != nil {
		log.Printf("failed to send start message: %v", err)
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
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}
	t.execSessionReset(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdAdd(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}

	args := strings.Fields(commandArgs(m.Text))
	if len(args) == 0 {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Usage: /add <ogn_id> [name]",
		}); err != nil {
			log.Printf("failed to send usage: %v", err)
		}
		return
	}

	id := shortID(args[0])
	display := strings.Join(args[1:], " ")
	username := m.From.Username
	if username == "" {
		username = strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	}

	t.mu.Lock()
	t.chatID = m.Chat.ID
	if info, ok := t.tracking[id]; ok {
		info.Name = display
		info.Username = username
		info.AutoDiscovered = false
	} else {
		t.tracking[id] = &TrackInfo{Name: display, Username: username}
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
	kb := t.keyboard()
	t.saveState()
	t.mu.Unlock()

	text := "Added " + id
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
			Text:   "Usage: /remove <ogn_id>",
		}); err != nil {
			log.Printf("failed to send usage: %v", err)
		}
		return
	}
	id := shortID(args)

	t.mu.Lock()
	t.chatID = m.Chat.ID
	delete(t.tracking, id)
	t.updateFilter()
	kb := t.keyboard()
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      m.Chat.ID,
		Text:        "Removed " + id,
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
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}
	t.execTrackOff(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdList(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
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

	t.mu.Lock()
	status := "disabled"
	if t.trackingOn {
		status = "enabled"
	}
	count := len(t.tracking)
	kb := t.keyboard()
	t.mu.Unlock()

	text := fmt.Sprintf("Tracking %s. %d address(es) added.", status, count)
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
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}
	t.execLanding(ctx, b, m.Chat.ID)
}

func (t *Tracker) cmdHelp(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}

	text := strings.Join([]string{
		"/start — start or reset the bot",
		"/add <id> [name] — track an OGN address",
		"/remove <id> — stop tracking",
		"/track_on — enable live tracking",
		"/track_off — disable tracking",
		"/landing — set landing location",
		"/driver — become the driver (share live location)",
		"/driver_off — stop being the driver",
		"/area [radius] — track all aircraft in area (default 100km)",
		"/area_off — disable area tracking",
		"/list — show tracked addresses",
		"/status — show current state",
		"/session_reset — stop and clear all",
		"/help — show this help",
	}, "\n")
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   text,
	}); err != nil {
		log.Printf("failed to send help: %v", err)
	}
}

func (t *Tracker) handleLocation(ctx context.Context, b *bot.Bot, m *models.Message) {
	loc := m.Location

	t.mu.Lock()

	// Driver: check if this user is waiting.
	if d, ok := t.drivers[m.From.ID]; ok && d.Waiting && time.Now().Before(d.Expiry) {
		if loc.LivePeriod > 0 {
			d.Pos = &Coordinates{Latitude: loc.Latitude, Longitude: loc.Longitude}
			d.MsgID = m.ID
			d.Waiting = false
			kb := t.keyboard()
			t.mu.Unlock()
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:      m.Chat.ID,
				Text:        "🚗 Driver location active. Distances will appear in the summary.",
				ReplyMarkup: kb,
			}); err != nil {
				log.Printf("failed to confirm driver location: %v", err)
			}
			return
		}
		// Static pin — use as temporary position, keep waiting for live.
		d.Pos = &Coordinates{Latitude: loc.Latitude, Longitude: loc.Longitude}
		kb := t.keyboard()
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
	if t.waitingLanding && time.Now().Before(t.landingExpiry) {
		t.landing = &Coordinates{Latitude: loc.Latitude, Longitude: loc.Longitude}
		t.waitingLanding = false
		kb := t.keyboard()
		t.saveState()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      m.Chat.ID,
			Text:        "Landing location saved",
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to confirm landing location: %v", err)
		}
		return
	}

	// Area: expecting center location.
	if t.waitingArea && time.Now().Before(t.areaExpiry) {
		t.trackArea = &Coordinates{Latitude: loc.Latitude, Longitude: loc.Longitude}
		t.waitingArea = false
		// Remove previously auto-discovered entries when area changes.
		for id, info := range t.tracking {
			if info.AutoDiscovered {
				delete(t.tracking, id)
			}
		}
		t.updateFilter()
		radius := t.trackAreaRadius
		kb := t.keyboard()
		t.saveState()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      m.Chat.ID,
			Text:        fmt.Sprintf("📡 Area tracking active: %dkm radius", radius),
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
	t.execDriverOff(ctx, b, m.Chat.ID, m.From.ID)
}

func (t *Tracker) cmdArea(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
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
	t.execAreaOff(ctx, b, m.Chat.ID)
}

// --- Core logic used by both command and callback handlers ---

func (t *Tracker) execSessionReset(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	t.stopTracking()
	t.tracking = make(map[string]*TrackInfo)
	t.updateFilter()
	t.landing = nil
	t.waitingLanding = false
	t.summaryMsgID = 0
	t.drivers = make(map[int64]*DriverInfo)
	t.trackArea = nil
	t.waitingArea = false
	t.chatID = chatID
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   "Session reset. Use /add <id> to start again.",
	}); err != nil {
		log.Printf("failed to send session_reset message: %v", err)
	}
}

func (t *Tracker) execTrackOn(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	if len(t.tracking) == 0 && t.trackArea == nil {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "No addresses added. Use /add <id> or /area first.",
		}); err != nil {
			log.Printf("failed to send no addresses message: %v", err)
		}
		return
	}
	if t.trackingOn {
		kb := t.keyboard()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      chatID,
			Text:        "Tracking already enabled",
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to confirm track_on: %v", err)
		}
		return
	}
	t.trackingOn = true
	t.stopCh = make(chan struct{})
	stopCh := t.stopCh
	t.updateFilter()
	t.chatID = chatID
	kb := t.keyboard()
	t.saveState()
	t.mu.Unlock()

	go t.runClient(stopCh)
	go t.sendUpdates(stopCh)

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "Tracking enabled",
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm track_on: %v", err)
	}
}

func (t *Tracker) execTrackOff(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	if !t.trackingOn {
		kb := t.keyboard()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      chatID,
			Text:        "Tracking already disabled",
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to confirm track_off: %v", err)
		}
		return
	}
	t.stopTracking()
	kb := t.keyboard()
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "Tracking disabled",
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm track_off: %v", err)
	}
}

func (t *Tracker) execList(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	var entries []string
	for id, info := range t.tracking {
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
			entry += fmt.Sprintf(" (landed %s)", info.LandingTime.Format("15:04"))
		}
		if info.Status == StatusPickedUp {
			entry += " (picked up)"
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
	track := "off"
	if t.trackingOn {
		track = "on"
	}

	// Copy tracking map for pilotButtons (still under lock).
	localCopy := make(map[string]*TrackInfo, len(t.tracking))
	for id, info := range t.tracking {
		cp := *info
		localCopy[id] = &cp
	}
	kb := t.keyboard()
	t.mu.Unlock()

	// Merge general keyboard with nav+pickup buttons for landed pilots.
	navKb := pilotButtons(localCopy)
	var replyMarkup models.ReplyMarkup
	if navKb != nil {
		merged := *navKb
		if kb != nil {
			merged.InlineKeyboard = append(merged.InlineKeyboard, kb.InlineKeyboard...)
		}
		replyMarkup = &merged
	} else {
		replyMarkup = kb
	}

	list := strings.Join(entries, "\n")
	if list == "" {
		list = "none"
	}
	text := "Tracking: " + track + "\n" + list
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
	t.waitingLanding = true
	t.landingExpiry = time.Now().Add(2 * time.Minute)
	t.chatID = chatID
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   "Send landing location within 2 minutes",
	}); err != nil {
		log.Printf("failed to request landing location: %v", err)
	}
}

func (t *Tracker) execArea(ctx context.Context, b *bot.Bot, chatID int64, radiusKm int) {
	t.mu.Lock()
	t.waitingArea = true
	t.areaExpiry = time.Now().Add(2 * time.Minute)
	t.trackAreaRadius = radiusKm
	t.chatID = chatID
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   fmt.Sprintf("Send a location for the area center (%dkm radius) within 2 minutes", radiusKm),
	}); err != nil {
		log.Printf("failed to request area location: %v", err)
	}
}

func (t *Tracker) execAreaOff(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	t.trackArea = nil
	t.waitingArea = false
	// Remove auto-discovered entries.
	for id, info := range t.tracking {
		if info.AutoDiscovered {
			delete(t.tracking, id)
		}
	}
	t.updateFilter()
	kb := t.keyboard()
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "📡 Area tracking disabled",
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm area off: %v", err)
	}
}

func (t *Tracker) execDriver(ctx context.Context, b *bot.Bot, chatID int64, userID int64, username string) {
	t.mu.Lock()
	if d, ok := t.drivers[userID]; ok && d.MsgID != 0 {
		kb := t.keyboard()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      chatID,
			Text:        "🚗 You're already driving. Use /driver_off to stop.",
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to send driver active message: %v", err)
		}
		return
	}

	gen := 1
	if existing, ok := t.drivers[userID]; ok {
		gen = existing.WaitGen + 1
	}
	t.drivers[userID] = &DriverInfo{
		Waiting: true,
		Expiry:  time.Now().Add(2 * time.Minute),
		WaitGen: gen,
	}
	t.chatID = chatID
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   "Share your live location within 2 minutes to become the driver.",
	}); err != nil {
		log.Printf("failed to send driver prompt: %v", err)
	}

	go t.driverWaitTimeout(gen, userID, chatID, username)
}

func (t *Tracker) driverWaitTimeout(gen int, userID int64, chatID int64, username string) {
	time.Sleep(2 * time.Minute)

	t.mu.Lock()
	d, ok := t.drivers[userID]
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
	text := fmt.Sprintf("⏰ %s, вы не расшарили локацию. Отправьте live-локацию в течение 2 минут.", mention)

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
	d, ok = t.drivers[userID]
	if !ok || d.WaitGen != gen || !d.Waiting {
		t.mu.Unlock()
		return
	}
	d.Waiting = false
	if d.Pos == nil {
		delete(t.drivers, userID)
	}
	t.mu.Unlock()
	log.Printf("driver wait timed out for user %d", userID)
}

func (t *Tracker) execDriverOff(ctx context.Context, b *bot.Bot, chatID int64, userID int64) {
	t.mu.Lock()
	_, was := t.drivers[userID]
	delete(t.drivers, userID)
	kb := t.keyboard()
	t.mu.Unlock()

	text := "🚗 You're not driving"
	if was {
		text = "🚗 Driver location cleared"
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
	t.mu.Lock()
	chatID := t.chatID
	info, ok := t.tracking[id]
	if ok {
		info.Status = StatusPickedUp
	}
	kb := t.keyboard()
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
		ChatID:      chatID,
		Text:        fmt.Sprintf("✅ %s picked up", label),
		ReplyMarkup: kb,
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
	chatID := t.chatID
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
	chatID := t.chatID
	t.mu.Unlock()
	t.execTrackOff(ctx, b, chatID)
}

func (t *Tracker) cbList(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	t.answerCallback(ctx, b, cq)
	if !t.isTrusted(cq.From.ID) {
		return
	}
	t.mu.Lock()
	chatID := t.chatID
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
	chatID := t.chatID
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
	chatID := t.chatID
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
	chatID := t.chatID
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
	chatID := t.chatID
	radius := t.trackAreaRadius
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
	chatID := t.chatID
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
	chatID := t.chatID
	t.mu.Unlock()
	t.execSessionReset(ctx, b, chatID)
}

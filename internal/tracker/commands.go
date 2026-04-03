package tracker

import (
	"context"
	"fmt"
	"log"
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
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Session started. Use /add <id> to track a glider.",
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

func (t *Tracker) handleLandingLocation(ctx context.Context, b *bot.Bot, m *models.Message) {
	t.mu.Lock()
	waiting := t.waitingLanding && time.Now().Before(t.landingExpiry)
	if waiting {
		t.landing = &Coordinates{Latitude: m.Location.Latitude, Longitude: m.Location.Longitude}
		t.waitingLanding = false
	}
	kb := t.keyboard()
	t.mu.Unlock()
	if waiting {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      m.Chat.ID,
			Text:        "Landing location saved",
			ReplyMarkup: kb,
		}); err != nil {
			log.Printf("failed to confirm landing location: %v", err)
		}
	}
}

// --- Core logic used by both command and callback handlers ---

func (t *Tracker) execSessionReset(ctx context.Context, b *bot.Bot, chatID int64) {
	t.mu.Lock()
	t.stopTracking()
	t.tracking = make(map[string]*TrackInfo)
	t.updateFilter()
	t.landing = nil
	t.waitingLanding = false
	t.chatID = chatID
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
	if len(t.tracking) == 0 {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "No addresses added. Use /add <id> first.",
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
	kb := t.keyboard()
	t.mu.Unlock()

	list := strings.Join(entries, "\n")
	if list == "" {
		list = "none"
	}
	text := "Tracking: " + track + "\n" + list
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: kb,
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

func (t *Tracker) execPickup(ctx context.Context, b *bot.Bot, id string) {
	t.mu.Lock()
	chatID := t.chatID
	info, ok := t.tracking[id]
	if ok {
		info.Status = StatusPickedUp
	}
	kb := t.keyboard()
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

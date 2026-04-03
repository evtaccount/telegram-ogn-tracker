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
			Text:   "Run /start_session first",
		}); err != nil {
			log.Printf("failed to send session required message: %v", err)
		}
	}
	return active
}

func (t *Tracker) cmdStart(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}

	t.mu.Lock()
	t.chatID = m.Chat.ID
	t.stopTracking()
	t.sessionActive = false
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "This bot tracks gliders on the OGN network. After /start, run /start_session to enable commands.",
		ReplyMarkup: &models.ReplyKeyboardRemove{
			RemoveKeyboard: true,
		},
	}); err != nil {
		log.Printf("failed to send start message: %v", err)
	}
}

func (t *Tracker) cmdStartSession(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}

	t.mu.Lock()
	if t.sessionActive {
		t.stopTracking()
		t.tracking = make(map[string]*TrackInfo)
	}
	t.sessionActive = true
	t.chatID = m.Chat.ID
	t.updateFilter()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Session started. You can now use all commands.",
		ReplyMarkup: &models.ReplyKeyboardRemove{
			RemoveKeyboard: true,
		},
	}); err != nil {
		log.Printf("failed to send start_session message: %v", err)
	}
}

func (t *Tracker) cmdSessionReset(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}

	t.mu.Lock()
	t.stopTracking()
	t.tracking = make(map[string]*TrackInfo)
	t.updateFilter()
	t.chatID = m.Chat.ID
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Session reset",
	}); err != nil {
		log.Printf("failed to send session_reset message: %v", err)
	}
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
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Added " + id,
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
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Removed " + id,
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

	t.mu.Lock()
	if len(t.tracking) == 0 {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "No addresses added",
		}); err != nil {
			log.Printf("failed to send no addresses message: %v", err)
		}
		return
	}
	if t.trackingOn {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Tracking already enabled",
		}); err != nil {
			log.Printf("failed to confirm track_on: %v", err)
		}
		return
	}
	t.trackingOn = true
	t.stopCh = make(chan struct{})
	stopCh := t.stopCh
	t.updateFilter()
	t.chatID = m.Chat.ID
	t.mu.Unlock()

	go t.runClient(stopCh)
	go t.sendUpdates(stopCh)

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Tracking enabled",
	}); err != nil {
		log.Printf("failed to confirm track_on: %v", err)
	}
}

func (t *Tracker) cmdTrackOff(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}

	t.mu.Lock()
	if !t.trackingOn {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Tracking already disabled",
		}); err != nil {
			log.Printf("failed to confirm track_off: %v", err)
		}
		return
	}
	t.stopTracking()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Tracking disabled",
	}); err != nil {
		log.Printf("failed to confirm track_off: %v", err)
	}
}

func (t *Tracker) cmdList(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !t.requireSession(ctx, b, m.Chat.ID) {
		return
	}

	t.mu.Lock()
	var entries []string
	for id, info := range t.tracking {
		entry := id
		if info.Name != "" {
			entry += " — " + info.Name
			if info.Username != "" {
				entry += " (" + info.Username + ")"
			}
		} else if info.Username != "" {
			entry += " — " + info.Username
		}
		entries = append(entries, entry)
	}
	track := "off"
	if t.trackingOn {
		track = "on"
	}
	t.mu.Unlock()

	list := strings.Join(entries, "\n")
	if list == "" {
		list = "none"
	}
	text := "Tracking: " + track + "\n" + list
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   text,
	}); err != nil {
		log.Printf("failed to send list: %v", err)
	}
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
	t.mu.Unlock()

	text := fmt.Sprintf("Tracking %s. %d address(es) added.", status, count)
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   text,
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

	t.mu.Lock()
	t.waitingLanding = true
	t.landingExpiry = time.Now().Add(2 * time.Minute)
	t.chatID = m.Chat.ID
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Send landing location within 2 minutes",
	}); err != nil {
		log.Printf("failed to request landing location: %v", err)
	}
}

func (t *Tracker) cmdHelp(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}

	text := strings.Join([]string{
		"/start - display a welcome message",
		"/start_session - enable full commands or reset the session",
		"/add <id> [name] - start tracking the given OGN id; the optional name is shown in messages",
		"/remove <id> - stop tracking the id",
		"/landing - set default landing location",
		"/track_on - enable tracking",
		"/track_off - disable tracking",
		"/session_reset - stop tracking and clear all addresses",
		"/list - show current tracked ids and state",
		"/status - show current state",
		"/help - show this help",
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
	t.mu.Unlock()
	if waiting {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Landing location saved",
		}); err != nil {
			log.Printf("failed to confirm landing location: %v", err)
		}
	}
}

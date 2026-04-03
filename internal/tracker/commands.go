package tracker

import (
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func (t *Tracker) isTrusted(userID int64) bool {
	return true
}

func (t *Tracker) requireSession(m *tgbotapi.Message) bool {
	t.mu.Lock()
	active := t.sessionActive
	t.mu.Unlock()
	if !active {
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Run /start_session first")); err != nil {
			log.Printf("failed to send session required message: %v", err)
		}
	}
	return active
}

func (t *Tracker) cmdStart(m *tgbotapi.Message) {
	t.mu.Lock()
	t.chatID = m.Chat.ID
	t.stopTracking()
	t.sessionActive = false
	t.mu.Unlock()

	msg := tgbotapi.NewMessage(m.Chat.ID, "This bot tracks gliders on the OGN network. After /start, run /start_session to enable commands.")
	msg.ReplyMarkup = tgbotapi.NewRemoveKeyboard(false)
	if _, err := t.bot.Send(msg); err != nil {
		log.Printf("failed to send start message: %v", err)
	}
}

func (t *Tracker) cmdStartSession(m *tgbotapi.Message) {
	t.mu.Lock()
	if t.sessionActive {
		t.stopTracking()
		t.tracking = make(map[string]*TrackInfo)
	}
	t.sessionActive = true
	t.chatID = m.Chat.ID
	t.updateFilter()
	t.mu.Unlock()

	msg := tgbotapi.NewMessage(m.Chat.ID, "Session started. You can now use all commands.")
	msg.ReplyMarkup = tgbotapi.NewRemoveKeyboard(false)
	if _, err := t.bot.Send(msg); err != nil {
		log.Printf("failed to send start_session message: %v", err)
	}
}

func (t *Tracker) cmdSessionReset(m *tgbotapi.Message) {
	if !t.requireSession(m) {
		return
	}
	t.mu.Lock()
	t.stopTracking()
	t.tracking = make(map[string]*TrackInfo)
	t.updateFilter()
	t.chatID = m.Chat.ID
	t.mu.Unlock()

	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Session reset")); err != nil {
		log.Printf("failed to send session_reset message: %v", err)
	}
}

func (t *Tracker) cmdAdd(m *tgbotapi.Message) {
	if !t.requireSession(m) {
		return
	}
	args := strings.Fields(m.CommandArguments())
	if len(args) == 0 {
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Usage: /add <ogn_id> [name]")); err != nil {
			log.Printf("failed to send usage: %v", err)
		}
		return
	}

	id := shortID(args[0])
	display := strings.Join(args[1:], " ")
	username := m.From.UserName
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

	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Added "+id)); err != nil {
		log.Printf("failed to confirm add: %v", err)
	}
}

func (t *Tracker) cmdRemove(m *tgbotapi.Message) {
	if !t.requireSession(m) {
		return
	}
	args := m.CommandArguments()
	if args == "" {
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Usage: /remove <ogn_id>")); err != nil {
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
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Removed "+id)); err != nil {
		log.Printf("failed to confirm remove: %v", err)
	}
}

func (t *Tracker) cmdTrackOn(m *tgbotapi.Message) {
	if !t.requireSession(m) {
		return
	}
	t.mu.Lock()
	if len(t.tracking) == 0 {
		t.mu.Unlock()
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "No addresses added")); err != nil {
			log.Printf("failed to send no addresses message: %v", err)
		}
		return
	}
	if t.trackingOn {
		t.mu.Unlock()
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking already enabled")); err != nil {
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
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking enabled")); err != nil {
		log.Printf("failed to confirm track_on: %v", err)
	}
}

func (t *Tracker) cmdTrackOff(m *tgbotapi.Message) {
	if !t.requireSession(m) {
		return
	}
	t.mu.Lock()
	if !t.trackingOn {
		t.mu.Unlock()
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking already disabled")); err != nil {
			log.Printf("failed to confirm track_off: %v", err)
		}
		return
	}
	t.stopTracking()
	t.mu.Unlock()
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking disabled")); err != nil {
		log.Printf("failed to confirm track_off: %v", err)
	}
}

func (t *Tracker) cmdList(m *tgbotapi.Message) {
	if !t.requireSession(m) {
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
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, text)); err != nil {
		log.Printf("failed to send list: %v", err)
	}
}

func (t *Tracker) cmdStatus(m *tgbotapi.Message) {
	t.mu.Lock()
	status := "disabled"
	if t.trackingOn {
		status = "enabled"
	}
	count := len(t.tracking)
	t.mu.Unlock()
	text := fmt.Sprintf("Tracking %s. %d address(es) added.", status, count)
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, text)); err != nil {
		log.Printf("failed to send status: %v", err)
	}
}

func (t *Tracker) cmdLanding(m *tgbotapi.Message) {
	if !t.requireSession(m) {
		return
	}
	t.mu.Lock()
	t.waitingLanding = true
	t.landingExpiry = time.Now().Add(2 * time.Minute)
	t.chatID = m.Chat.ID
	t.mu.Unlock()
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Send landing location within 2 minutes")); err != nil {
		log.Printf("failed to request landing location: %v", err)
	}
}

func (t *Tracker) cmdHelp(m *tgbotapi.Message) {
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
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, text)); err != nil {
		log.Printf("failed to send help: %v", err)
	}
}

func (t *Tracker) handleLandingLocation(m *tgbotapi.Message) {
	t.mu.Lock()
	waiting := t.waitingLanding && time.Now().Before(t.landingExpiry)
	if waiting {
		t.landing = &Coordinates{Latitude: m.Location.Latitude, Longitude: m.Location.Longitude}
		t.waitingLanding = false
	}
	t.mu.Unlock()
	if waiting {
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Landing location saved")); err != nil {
			log.Printf("failed to confirm landing location: %v", err)
		}
	}
}

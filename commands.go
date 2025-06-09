package main

import (
	"fmt"
	"log"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func (t *Tracker) isTrusted(userID int64) bool {
	return true
}

func (t *Tracker) cmdStart(m *tgbotapi.Message) {
	t.mu.Lock()
	t.chatID = m.Chat.ID
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
		t.tracking = make(map[string]*TrackInfo)
		if t.trackingOn {
			t.trackingOn = false
			t.aprs.Disconnect()
		}
	}
	t.sessionActive = true
	t.chatID = m.Chat.ID
	t.mu.Unlock()

	msg := tgbotapi.NewMessage(m.Chat.ID, "Session started. You can now use all commands.")
	msg.ReplyMarkup = tgbotapi.NewRemoveKeyboard(false)
	if _, err := t.bot.Send(msg); err != nil {
		log.Printf("failed to send start_session message: %v", err)
	}
}

func (t *Tracker) cmdAdd(m *tgbotapi.Message) {
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
	t.mu.Unlock()

	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Added "+id)); err != nil {
		log.Printf("failed to confirm add: %v", err)
	}
}

func (t *Tracker) cmdRemove(m *tgbotapi.Message) {
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
	t.mu.Unlock()
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Removed "+id)); err != nil {
		log.Printf("failed to confirm remove: %v", err)
	}
}

func (t *Tracker) cmdTrackOn(m *tgbotapi.Message) {
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
	t.chatID = m.Chat.ID
	t.mu.Unlock()
	go t.runClient()
	go t.sendUpdates()
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking enabled")); err != nil {
		log.Printf("failed to confirm track_on: %v", err)
	}
}

func (t *Tracker) cmdTrackOff(m *tgbotapi.Message) {
	t.mu.Lock()
	if !t.trackingOn {
		t.mu.Unlock()
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking already disabled")); err != nil {
			log.Printf("failed to confirm track_off: %v", err)
		}
		return
	}
	t.trackingOn = false
	t.mu.Unlock()
	t.aprs.Disconnect()
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking disabled")); err != nil {
		log.Printf("failed to confirm track_off: %v", err)
	}
}

func (t *Tracker) cmdList(m *tgbotapi.Message) {
	t.mu.Lock()
	var users []string
	for _, info := range t.tracking {
		entry := ""
		if info.Name != "" {
			entry = info.Name
			if info.Username != "" {
				entry += " (@" + info.Username + ")"
			}
		} else if info.Username != "" {
			entry = "@" + info.Username
		}
		if entry != "" {
			users = append(users, entry)
		}
	}
	track := "off"
	if t.trackingOn {
		track = "on"
	}
	t.mu.Unlock()
	list := strings.Join(users, "\n")
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

func (t *Tracker) cmdHelp(m *tgbotapi.Message) {
	text := strings.Join([]string{
		"/start - display a welcome message",
		"/start_session - enable full commands or reset the session",
		"/add <id> [name] - start tracking the given OGN id; the optional name is shown in messages",
		"/remove <id> - stop tracking the id",
		"/track_on - enable tracking",
		"/track_off - disable tracking",
		"/list - show current tracked ids and state",
		"/status - show current state",
		"/help - show this help",
	}, "\n")
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, text)); err != nil {
		log.Printf("failed to send help: %v", err)
	}
}

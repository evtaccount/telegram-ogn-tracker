package main

import (
	"log"
	"os"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"ogn/client"
	"ogn/parser"
)

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN must be set")
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Fatal(err)
	}

	tracker := NewTracker(bot)
	tracker.Run()
}

// Tracker holds state for tracking gliders.
// TrackInfo stores state for a single tracked address.
type TrackInfo struct {
	MessageID int
	Position  *parser.PositionMessage
}

// Tracker holds state for tracking gliders.
type Tracker struct {
	bot        *tgbotapi.BotAPI
	aprs       *client.Client
	tracking   map[string]*TrackInfo
	mu         sync.Mutex
	trackingOn bool
	chatID     int64
}

// shortID returns the last 6 characters of id in upper case. If id is shorter
// than 6 characters it returns the whole string in upper case.
func shortID(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	if len(id) <= 6 {
		return id
	}
	return id[len(id)-6:]
}

// NewTracker creates a tracker with given Telegram bot.
func NewTracker(bot *tgbotapi.BotAPI) *Tracker {
	return &Tracker{
		bot:      bot,
		aprs:     client.New("N0CALL", ""),
		tracking: make(map[string]*TrackInfo),
	}
}

// Run starts the Telegram bot and waits for updates.
func (t *Tracker) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := t.bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil { // ignore non-message updates
			continue
		}

		if !t.isTrusted(update.Message.From.ID) {
			continue
		}

		switch update.Message.Command() {
		case "start":
			t.cmdStart(update.Message)
		case "add":
			t.cmdAdd(update.Message)
		case "remove":
			t.cmdRemove(update.Message)
		case "track_on":
			t.cmdTrackOn(update.Message)
		case "track_off":
			t.cmdTrackOff(update.Message)
		case "list":
			t.cmdList(update.Message)
		}
	}
}

func (t *Tracker) isTrusted(userID int64) bool {
	return true
}

func (t *Tracker) cmdStart(m *tgbotapi.Message) {
	t.mu.Lock()
	t.chatID = m.Chat.ID
	t.mu.Unlock()
	msg := tgbotapi.NewMessage(m.Chat.ID, "OGN tracker bot ready. Use /add <id> to track gliders.")
	if _, err := t.bot.Send(msg); err != nil {
		log.Printf("failed to send start message: %v", err)
	}
}

func (t *Tracker) cmdAdd(m *tgbotapi.Message) {
	args := m.CommandArguments()
	if args == "" {
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Usage: /add <ogn_id>")); err != nil {
			log.Printf("failed to send usage: %v", err)
		}
		return
	}
	id := shortID(args)
	t.mu.Lock()
	t.chatID = m.Chat.ID
	if _, ok := t.tracking[id]; !ok {
		t.tracking[id] = &TrackInfo{}
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
	ids := ""
	for id := range t.tracking {
		if ids != "" {
			ids += ", "
		}
		ids += id
	}
	track := "off"
	if t.trackingOn {
		track = "on"
	}
	t.mu.Unlock()
	text := "Tracking: " + track + "\nIDs: " + ids
	if ids == "" {
		text = "Tracking: " + track + "\nIDs: none"
	}
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, text)); err != nil {
		log.Printf("failed to send list: %v", err)
	}
}

func (t *Tracker) runClient() {
	log.Println("OGN client started")
	err := t.aprs.Run(func(line string) {
		log.Printf("raw OGN line: %s", line)
		msg, err := parser.Parse(line)
		if err != nil {
			log.Printf("failed to parse line: %v", err)
			return
		}
		origID := msg.Callsign
		id := shortID(origID)
		t.mu.Lock()
		if info, ok := t.tracking[id]; ok {
			info.Position = msg
			t.tracking[id] = info
			log.Printf("received beacon for %s (orig %s): lat %.5f lon %.5f", id, origID, msg.Latitude, msg.Longitude)
		} else {
			log.Printf("ignoring untracked id %s", origID)
		}
		t.mu.Unlock()
	}, true)
	if err != nil {
		log.Printf("OGN client error: %v", err)
	}
	log.Println("OGN client stopped")
}

// sendUpdates periodically posts positions to Telegram.
func (t *Tracker) sendUpdates() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		t.mu.Lock()
		if !t.trackingOn {
			t.mu.Unlock()
			return
		}
		chatID := t.chatID
		local := make(map[string]*TrackInfo)
		for id, info := range t.tracking {
			// copy pointer to avoid holding lock
			cp := *info
			local[id] = &cp
		}
		t.mu.Unlock()

		for id, info := range local {
			if info.Position == nil {
				continue
			}
			msgID := info.MessageID
			if msgID != 0 {
				edit := tgbotapi.EditMessageLiveLocationConfig{
					BaseEdit: tgbotapi.BaseEdit{
						ChatID:    chatID,
						MessageID: msgID,
					},
					Latitude:  info.Position.Latitude,
					Longitude: info.Position.Longitude,
				}
				if _, err := t.bot.Request(edit); err != nil {
					log.Printf("failed to edit location for %s: %v", id, err)
				} else {
					log.Printf("updated location for %s", id)
				}
			} else {
				loc := tgbotapi.NewLocation(chatID, info.Position.Latitude, info.Position.Longitude)
				loc.LivePeriod = 86400
				msg, err := t.bot.Send(loc)
				if err != nil {
					log.Printf("failed to send location for %s: %v", id, err)
					continue
				}
				if _, err := t.bot.Send(tgbotapi.NewMessage(chatID, "Address: "+id)); err != nil {
					log.Printf("failed to send address message for %s: %v", id, err)
				}
				t.mu.Lock()
				if info, ok := t.tracking[id]; ok {
					info.MessageID = msg.MessageID
					t.tracking[id] = info
				}
				t.mu.Unlock()
				log.Printf("sent location for %s", id)
			}
		}
	}
}

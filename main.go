package main

import (
	"log"
	"os"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	ogn "gitlab.eqipe.ch/sgw/go-ogn-client/ogn"
)

// ownerID is the Telegram ID of the user allowed to control the bot.
const ownerID int64 = 182255461

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
type Tracker struct {
	bot         *tgbotapi.BotAPI
	aprs        *ogn.Client
	trackingIDs map[string]int
	positions   map[string]*ogn.APRSMessage
	mu          sync.Mutex
	tracking    bool
	chatID      int64
}

// NewTracker creates a tracker with given Telegram bot.
func NewTracker(bot *tgbotapi.BotAPI) *Tracker {
	return &Tracker{
		bot:         bot,
		aprs:        ogn.New("", true),
		trackingIDs: make(map[string]int),
		positions:   make(map[string]*ogn.APRSMessage),
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
	return userID == ownerID
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
	id := args
	t.mu.Lock()
	t.chatID = m.Chat.ID
	t.trackingIDs[id] = 0
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
	id := args
	t.mu.Lock()
	t.chatID = m.Chat.ID
	delete(t.trackingIDs, id)
	delete(t.positions, id)
	t.mu.Unlock()
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Removed "+id)); err != nil {
		log.Printf("failed to confirm remove: %v", err)
	}
}

func (t *Tracker) cmdTrackOn(m *tgbotapi.Message) {
	t.mu.Lock()
	if t.tracking {
		t.mu.Unlock()
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking already enabled")); err != nil {
			log.Printf("failed to confirm track_on: %v", err)
		}
		return
	}
	t.tracking = true
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
	if !t.tracking {
		t.mu.Unlock()
		if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking already disabled")); err != nil {
			log.Printf("failed to confirm track_off: %v", err)
		}
		return
	}
	t.tracking = false
	t.mu.Unlock()
	t.aprs.Close()
	if _, err := t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking disabled")); err != nil {
		log.Printf("failed to confirm track_off: %v", err)
	}
}

func (t *Tracker) cmdList(m *tgbotapi.Message) {
	t.mu.Lock()
	ids := ""
	for id := range t.trackingIDs {
		if ids != "" {
			ids += ", "
		}
		ids += id
	}
	track := "off"
	if t.tracking {
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
	t.aprs.MessageHandler = func(v interface{}) {
		m, ok := v.(*ogn.APRSMessage)
		if !ok {
			return
		}
		pos, ok := m.Body.(*ogn.APRSPosition)
		if !ok {
			return
		}
		id := m.CallSign
		t.mu.Lock()
		if _, ok := t.trackingIDs[id]; ok {
			t.positions[id] = &ogn.APRSMessage{CallSign: m.CallSign, Body: pos}
			log.Printf("received beacon for %s: lat %.5f lon %.5f", id, pos.Latitude, pos.Longitude)
		}
		t.mu.Unlock()
	}

	log.Println("OGN client started")
	if err := t.aprs.Run(); err != nil {
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
		if !t.tracking {
			t.mu.Unlock()
			return
		}
		chatID := t.chatID
		ids := make(map[string]int)
		for id, msgID := range t.trackingIDs {
			ids[id] = msgID
		}
		positions := make(map[string]*ogn.APRSMessage)
		for id, msg := range t.positions {
			positions[id] = msg
		}
		t.mu.Unlock()

		for id, msg := range positions {
			pos, ok := msg.Body.(*ogn.APRSPosition)
			if !ok {
				continue
			}
			msgID := ids[id]
			if msgID != 0 {
				edit := tgbotapi.EditMessageLiveLocationConfig{
					BaseEdit: tgbotapi.BaseEdit{
						ChatID:    chatID,
						MessageID: msgID,
					},
					Latitude:  pos.Latitude,
					Longitude: pos.Longitude,
				}
				if _, err := t.bot.Request(edit); err != nil {
					log.Printf("failed to edit location for %s: %v", id, err)
				} else {
					log.Printf("updated location for %s", id)
				}
			} else {
				loc := tgbotapi.NewLocation(chatID, pos.Latitude, pos.Longitude)
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
				t.trackingIDs[id] = msg.MessageID
				t.mu.Unlock()
				log.Printf("sent location for %s", id)
			}
		}
	}
}

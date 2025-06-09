package main

import (
	"log"
	"os"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	ogn "gitlab.eqipe.ch/sgw/go-ogn-client/ogn"
)

const targetChatID int64 = 0 // replace with your chat id
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
	msg := tgbotapi.NewMessage(m.Chat.ID, "OGN tracker bot ready. Use /add <id> to track gliders.")
	t.bot.Send(msg)
}

func (t *Tracker) cmdAdd(m *tgbotapi.Message) {
	args := m.CommandArguments()
	if args == "" {
		t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Usage: /add <ogn_id>"))
		return
	}
	id := args
	t.mu.Lock()
	t.trackingIDs[id] = 0
	t.mu.Unlock()
	t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Added "+id))
}

func (t *Tracker) cmdRemove(m *tgbotapi.Message) {
	args := m.CommandArguments()
	if args == "" {
		t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Usage: /remove <ogn_id>"))
		return
	}
	id := args
	t.mu.Lock()
	delete(t.trackingIDs, id)
	delete(t.positions, id)
	t.mu.Unlock()
	t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Removed "+id))
}

func (t *Tracker) cmdTrackOn(m *tgbotapi.Message) {
	t.mu.Lock()
	if t.tracking {
		t.mu.Unlock()
		t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking already enabled"))
		return
	}
	t.tracking = true
	t.mu.Unlock()
	go t.runClient()
	t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking enabled"))
}

func (t *Tracker) cmdTrackOff(m *tgbotapi.Message) {
	t.mu.Lock()
	if !t.tracking {
		t.mu.Unlock()
		t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking already disabled"))
		return
	}
	t.tracking = false
	t.mu.Unlock()
	t.aprs.Close()
	t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, "Tracking disabled"))
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
	t.bot.Send(tgbotapi.NewMessage(m.Chat.ID, text))
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
		}
		t.mu.Unlock()
	}

	if err := t.aprs.Run(); err != nil {
		log.Printf("OGN client error: %v", err)
	}
}

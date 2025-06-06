package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gitlab.eqipe.ch/sgw/go-ogn-client/ogn"
	aprsparser "gitlab.eqipe.ch/sgw/go-ogn-client/ogn/subparser/aprsparser"
	flarmparser "gitlab.eqipe.ch/sgw/go-ogn-client/ogn/subparser/flarmparser"
)

type Position struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Timestamp int64   `json:"timestamp"`
}

type TrackerBot struct {
	bot             *tgbotapi.BotAPI
	targetChatID    int64
	trackingAddrs   map[string]int
	trackingEnabled bool
	mu              sync.Mutex
	ognClient       *ogn.Client
	positions       map[string]*Position
}

func NewTrackerBot(token string, chatID int64) (*TrackerBot, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	tb := &TrackerBot{
		bot:           bot,
		targetChatID:  chatID,
		trackingAddrs: make(map[string]int),
		positions:     make(map[string]*Position),
	}
	client := ogn.New("", true)
	client.MessageHandler = tb.handleOGNMessage
	tb.ognClient = client
	go func() {
		if err := client.Run(); err != nil {
			log.Printf("ogn client error: %v", err)
		}
	}()
	return tb, nil
}

func (tb *TrackerBot) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := tb.bot.GetUpdatesChan(u)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case update := <-updates:
			tb.handleUpdate(update)
		case <-ticker.C:
			tb.updatePositions()
		}
	}
}

func (tb *TrackerBot) handleUpdate(update tgbotapi.Update) {
	if update.Message == nil || !update.Message.IsCommand() {
		return
	}
	// Automatically set target chat if it wasn't configured
	tb.ensureTargetChat(update.Message.Chat.ID)
	switch update.Message.Command() {
	case "add":
		tb.cmdAdd(update)
	case "remove":
		tb.cmdRemove(update)
	case "track_on":
		tb.cmdTrackOn(update)
	case "track_off":
		tb.cmdTrackOff(update)
	case "list":
		tb.cmdList(update)
	case "chat_id":
		tb.cmdChatID(update)
	case "set_chat":
		tb.cmdSetChat(update)
	}
}

func (tb *TrackerBot) cmdAdd(update tgbotapi.Update) {
	args := update.Message.CommandArguments()
	if args == "" {
		tb.reply(update.Message.Chat.ID, "Usage: /add <ogn_id>")
		return
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()
	args = strings.ToUpper(args)
	if _, exists := tb.trackingAddrs[args]; exists {
		tb.reply(update.Message.Chat.ID, args+" already added")
		return
	}
	tb.trackingAddrs[args] = 0
	tb.reply(update.Message.Chat.ID, "Added "+args)
}

func (tb *TrackerBot) cmdRemove(update tgbotapi.Update) {
	args := update.Message.CommandArguments()
	if args == "" {
		tb.reply(update.Message.Chat.ID, "Usage: /remove <ogn_id>")
		return
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()
	args = strings.ToUpper(args)
	if _, exists := tb.trackingAddrs[args]; exists {
		delete(tb.trackingAddrs, args)
		tb.reply(update.Message.Chat.ID, "Removed "+args)
	} else {
		tb.reply(update.Message.Chat.ID, args+" not found")
	}
}

func (tb *TrackerBot) cmdTrackOn(update tgbotapi.Update) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if tb.trackingEnabled {
		tb.reply(update.Message.Chat.ID, "Tracking already enabled")
		return
	}
	tb.trackingEnabled = true
	tb.reply(update.Message.Chat.ID, "Tracking enabled")
}

func (tb *TrackerBot) cmdTrackOff(update tgbotapi.Update) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if !tb.trackingEnabled {
		tb.reply(update.Message.Chat.ID, "Tracking already disabled")
		return
	}
	tb.trackingEnabled = false
	tb.reply(update.Message.Chat.ID, "Tracking disabled")
}

func (tb *TrackerBot) cmdList(update tgbotapi.Update) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	ids := ""
	for id := range tb.trackingAddrs {
		if ids != "" {
			ids += ", "
		}
		ids += id
	}
	if ids == "" {
		ids = "No ids"
	}
	status := "off"
	if tb.trackingEnabled {
		status = "on"
	}
	tb.reply(update.Message.Chat.ID, "Tracking: "+status+"\nIDs: "+ids)
}

func (tb *TrackerBot) cmdChatID(update tgbotapi.Update) {
	tb.mu.Lock()
	current := tb.targetChatID
	tb.mu.Unlock()
	reply := fmt.Sprintf("Chat ID: %d", update.Message.Chat.ID)
	if current == update.Message.Chat.ID {
		reply += " (target chat)"
	}
	tb.reply(update.Message.Chat.ID, reply)
}

func (tb *TrackerBot) cmdSetChat(update tgbotapi.Update) {
	tb.mu.Lock()
	tb.targetChatID = update.Message.Chat.ID
	tb.mu.Unlock()
	tb.reply(update.Message.Chat.ID, fmt.Sprintf("Target chat set to %d", tb.targetChatID))
}

func (tb *TrackerBot) ensureTargetChat(chatID int64) {
	tb.mu.Lock()
	if tb.targetChatID == 0 {
		tb.targetChatID = chatID
	}
	tb.mu.Unlock()
}

func (tb *TrackerBot) reply(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	tb.bot.Send(msg)
}

func (tb *TrackerBot) updatePositions() {
	tb.mu.Lock()
	if !tb.trackingEnabled || len(tb.trackingAddrs) == 0 || tb.targetChatID == 0 {
		tb.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(tb.trackingAddrs))
	for id := range tb.trackingAddrs {
		ids = append(ids, id)
	}
	positions := make(map[string]*Position)
	for _, id := range ids {
		if p, ok := tb.positions[id]; ok {
			positions[id] = p
		}
	}
	tb.mu.Unlock()

	for id, pos := range positions {
		if pos == nil {
			continue
		}
		text := "ID: " + id + "\nLat: " + strconv.FormatFloat(pos.Lat, 'f', 6, 64) +
			"\nLon: " + strconv.FormatFloat(pos.Lon, 'f', 6, 64) +
			"\nTime: " + time.Unix(pos.Timestamp, 0).UTC().Format(time.RFC3339)

		tb.mu.Lock()
		msgID := tb.trackingAddrs[id]
		tb.mu.Unlock()

		if msgID != 0 {
			edit := tgbotapi.NewEditMessageText(tb.targetChatID, msgID, text)
			if _, err := tb.bot.Send(edit); err != nil {
				log.Printf("failed to edit message for %s: %v", id, err)
			}
		} else {
			msg := tgbotapi.NewMessage(tb.targetChatID, text)
			sent, err := tb.bot.Send(msg)
			if err == nil {
				tb.mu.Lock()
				tb.trackingAddrs[id] = sent.MessageID
				tb.mu.Unlock()
			}
		}
	}
}

func (tb *TrackerBot) handleOGNMessage(msg interface{}) {
	m, ok := msg.(*ogn.APRSMessage)
	if !ok || m == nil {
		return
	}
	pos, ok := m.Body.(*ogn.APRSPosition)
	if !ok || pos == nil {
		return
	}
	var addr string
	switch c := pos.Comment.(type) {
	case *flarmparser.Beacon:
		addr = c.Details.Address
	case *aprsparser.AircraftBeacon:
		addr = c.Details.Address
	}
	addr = strings.ToUpper(addr)

	tb.mu.Lock()
	if _, tracked := tb.trackingAddrs[addr]; tracked {
		p := &Position{Lat: pos.Latitude, Lon: pos.Longitude}
		if pos.Time != nil {
			p.Timestamp = pos.Time.UTC().Unix()
		}
		tb.positions[addr] = p
	}
	tb.mu.Unlock()
}

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN must be set")
	}
	bot, err := NewTrackerBot(token, 0)
	if err != nil {
		log.Fatal(err)
	}
	bot.Run()
}

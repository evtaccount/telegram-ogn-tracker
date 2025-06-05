package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Position struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Timestamp int64   `json:"timestamp"`
}

type TrackerBot struct {
	bot             *tgbotapi.BotAPI
	targetChatID    int64
	trackingIDs     map[string]int
	trackingEnabled bool
	mu              sync.Mutex
}

func NewTrackerBot(token string, chatID int64) (*TrackerBot, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	return &TrackerBot{
		bot:          bot,
		targetChatID: chatID,
		trackingIDs:  make(map[string]int),
	}, nil
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
	if _, exists := tb.trackingIDs[args]; exists {
		tb.reply(update.Message.Chat.ID, args+" already added")
		return
	}
	tb.trackingIDs[args] = 0
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
	if _, exists := tb.trackingIDs[args]; exists {
		delete(tb.trackingIDs, args)
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
	for id := range tb.trackingIDs {
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

func (tb *TrackerBot) reply(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	tb.bot.Send(msg)
}

func (tb *TrackerBot) updatePositions() {
	tb.mu.Lock()
	if !tb.trackingEnabled || len(tb.trackingIDs) == 0 {
		tb.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(tb.trackingIDs))
	for id := range tb.trackingIDs {
		ids = append(ids, id)
	}
	tb.mu.Unlock()

	for _, id := range ids {
		pos, err := getOGNPosition(id)
		if err != nil || pos == nil {
			continue
		}
		text := "ID: " + id + "\nLat: " + strconv.FormatFloat(pos.Lat, 'f', 6, 64) +
			"\nLon: " + strconv.FormatFloat(pos.Lon, 'f', 6, 64) +
			"\nTime: " + time.Unix(pos.Timestamp, 0).UTC().Format(time.RFC3339)

		tb.mu.Lock()
		msgID := tb.trackingIDs[id]
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
				tb.trackingIDs[id] = sent.MessageID
				tb.mu.Unlock()
			}
		}
	}
}

func getOGNPosition(id string) (*Position, error) {
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://api.glidernet.org/tracker/" + id)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var p Position
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, err
	}
	if p.Lat == 0 && p.Lon == 0 {
		return nil, nil
	}
	return &p, nil
}

func main() {
	token := os.Getenv("TELEGRAM_TOKEN")
	chatIDStr := os.Getenv("TARGET_CHAT_ID")
	if token == "" || chatIDStr == "" {
		log.Fatal("TELEGRAM_TOKEN and TARGET_CHAT_ID must be set")
	}
	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		log.Fatalf("invalid TARGET_CHAT_ID: %v", err)
	}
	bot, err := NewTrackerBot(token, chatID)
	if err != nil {
		log.Fatal(err)
	}
	bot.Run()
}

package main

import (
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"ogn/client"
	"ogn/parser"
)

type TrackInfo struct {
	MessageID  int
	TextID     int
	Position   *parser.PositionMessage
	Name       string
	Username   string
	LastUpdate time.Time
}

type Coordinates struct {
	Latitude  float64
	Longitude float64
}

type Tracker struct {
	bot            *tgbotapi.BotAPI
	aprs           *client.Client
	tracking       map[string]*TrackInfo
	mu             sync.Mutex
	trackingOn     bool
	chatID         int64
	sessionActive  bool
	landing        *Coordinates
	waitingLanding bool
	landingExpiry  time.Time
}

func shortID(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	if len(id) <= 6 {
		return id
	}
	return id[len(id)-6:]
}

func NewTracker(bot *tgbotapi.BotAPI) *Tracker {
	return &Tracker{
		bot:            bot,
		aprs:           client.New("N0CALL", ""),
		tracking:       make(map[string]*TrackInfo),
		trackingOn:     false,
		sessionActive:  false,
		landing:        nil,
		waitingLanding: false,
		landingExpiry:  time.Time{},
	}
}

func (t *Tracker) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := t.bot.GetUpdatesChan(u)

	for update := range updates {
		if update.MyChatMember != nil {
			// ignore chat member updates
		}

		if update.Message == nil {
			continue
		}

		if update.Message.NewChatMembers != nil {
			// ignore join messages
		}

		if !t.isTrusted(update.Message.From.ID) {
			continue
		}

		cmd := update.Message.Command()
		if cmd != "" {
			t.mu.Lock()
			if cmd != "landing" {
				t.waitingLanding = false
			}
			t.mu.Unlock()
		}

		switch cmd {
		case "start":
			t.cmdStart(update.Message)
		case "start_session":
			t.cmdStartSession(update.Message)
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
		case "status":
			t.cmdStatus(update.Message)
		case "landing":
			t.cmdLanding(update.Message)
		case "help":
			t.cmdHelp(update.Message)
		}

		if update.Message.Location != nil {
			t.handleLandingLocation(update.Message)
		}
	}
}

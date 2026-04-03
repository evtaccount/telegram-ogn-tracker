package tracker

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
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

func distanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0 // Earth radius in kilometers
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return r * c
}

type Tracker struct {
	bot            *bot.Bot
	aprs           *client.Client
	tracking       map[string]*TrackInfo
	mu             sync.Mutex
	trackingOn     bool
	stopCh         chan struct{}
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

// updateFilter rebuilds the APRS filter based on the currently tracked IDs.
// When tracking is active, the client connection is restarted so the new
// filter takes effect.
func (t *Tracker) updateFilter() {
	ids := make([]string, 0, len(t.tracking))
	filterable := true
	for id := range t.tracking {
		ids = append(ids, id)
		if len(id) <= 6 {
			filterable = false
		}
	}
	if len(ids) > 0 && filterable {
		t.aprs.Filter = "b/" + strings.Join(ids, "/")
	} else {
		t.aprs.Filter = ""
	}
	if t.trackingOn {
		t.aprs.Disconnect()
	}
}

// stopTracking disables tracking and signals goroutines to exit.
// Must be called with t.mu held.
func (t *Tracker) stopTracking() {
	if !t.trackingOn {
		return
	}
	t.trackingOn = false
	if t.stopCh != nil {
		close(t.stopCh)
		t.stopCh = nil
	}
	t.aprs.Disconnect()
}

func NewTracker() *Tracker {
	return &Tracker{
		aprs:     client.New("N0CALL", ""),
		tracking: make(map[string]*TrackInfo),
	}
}

// RegisterHandlers registers all Telegram command handlers on the bot.
func (t *Tracker) RegisterHandlers(b *bot.Bot) {
	t.bot = b
	b.RegisterHandler(bot.HandlerTypeMessageText, "/start_session", bot.MatchTypeCommand, t.cmdStartSession)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/session_reset", bot.MatchTypeCommand, t.cmdSessionReset)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/add", bot.MatchTypeCommand, t.cmdAdd)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/remove", bot.MatchTypeCommand, t.cmdRemove)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/track_on", bot.MatchTypeCommand, t.cmdTrackOn)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/track_off", bot.MatchTypeCommand, t.cmdTrackOff)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/list", bot.MatchTypeCommand, t.cmdList)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/status", bot.MatchTypeCommand, t.cmdStatus)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/landing", bot.MatchTypeCommand, t.cmdLanding)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/help", bot.MatchTypeCommand, t.cmdHelp)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/start", bot.MatchTypeCommand, t.cmdStart)
}

// DefaultHandler handles updates not matched by registered handlers (e.g. location messages).
func (t *Tracker) DefaultHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	if update.Message.From == nil {
		return
	}
	if !t.isTrusted(update.Message.From.ID) {
		return
	}
	if update.Message.Location != nil {
		t.handleLandingLocation(ctx, b, update.Message)
	}
}

// commandArgs extracts the arguments after a /command from the message text.
func commandArgs(text string) string {
	if i := strings.Index(text, " "); i != -1 {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

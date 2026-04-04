package tracker

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"ogn/client"
	"ogn/ddb"
	"ogn/parser"
)

// PilotStatus represents the current state of a tracked pilot.
type PilotStatus int

const (
	StatusFlying   PilotStatus = iota
	StatusLanded
	StatusPickedUp
)

func (s PilotStatus) Emoji() string {
	switch s {
	case StatusLanded:
		return "🪂"
	case StatusPickedUp:
		return "✅"
	default:
		return "✈️"
	}
}

type TrackInfo struct {
	MessageID      int
	Position       *parser.PositionMessage
	Name           string
	Username       string
	LastUpdate     time.Time
	Status         PilotStatus
	LandingTime    time.Time
	LowSpeedSince time.Time
	AutoDiscovered bool
}

func (ti *TrackInfo) DisplayName() string {
	if ti.Name != "" {
		return ti.Name
	}
	return ti.Username
}

type Coordinates struct {
	Latitude  float64
	Longitude float64
}

type DriverInfo struct {
	Pos     *Coordinates
	MsgID   int
	Waiting bool
	Expiry  time.Time
	WaitGen int
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
	devices        map[string]ddb.Device
	summaryMsgID   int
	drivers         map[int64]*DriverInfo
	trackArea       *Coordinates
	trackAreaRadius int
	waitingArea     bool
	areaExpiry      time.Time
	resumeOnStart   bool
	timezone        *time.Location
}

var aircraftTypes = map[int]string{
	0: "Unknown", 1: "Glider", 2: "Tow plane", 3: "Helicopter",
	4: "Parachute", 5: "Drop plane", 6: "Hang glider", 7: "Paraglider",
	8: "Powered aircraft", 9: "Jet", 10: "UFO", 11: "Balloon",
	12: "Airship", 13: "Drone", 15: "Static object",
}

func shortID(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	if len(id) <= 6 {
		return id
	}
	return id[len(id)-6:]
}

func distanceAndBearing(lat1, lon1, lat2, lon2 float64) (distKm float64, bearing float64) {
	ruler := parser.NewCheapRuler((lat1 + lat2) / 2)
	a := [2]float64{lon1, lat1}
	b := [2]float64{lon2, lat2}
	return ruler.Distance(a, b) / 1000, ruler.Bearing(a, b)
}

func bearingEmoji(deg float64) string {
	deg = math.Mod(deg+360, 360)
	arrows := []string{"⬆️", "↗️", "➡️", "↘️", "⬇️", "↙️", "⬅️", "↖️"}
	idx := int(math.Round(deg/45)) % 8
	return arrows[idx]
}

func formatBearing(deg float64) string {
	deg = math.Mod(deg+360, 360)
	return fmt.Sprintf("%s %03.0f°", bearingEmoji(deg), deg)
}

// formatTime formats a time value using the tracker's timezone.
// Must be called with t.mu held, or use the returned location outside the lock.
func (t *Tracker) tz() *time.Location {
	if t.timezone != nil {
		return t.timezone
	}
	return time.UTC
}

func mapsNavURL(lat, lon float64) string {
	return fmt.Sprintf("https://www.google.com/maps/dir/?api=1&destination=%.6f,%.6f", lat, lon)
}

// updateFilter rebuilds the APRS filter based on tracked IDs and area.
func (t *Tracker) updateFilter() {
	var filters []string

	// Budlist for explicitly added IDs.
	ids := make([]string, 0, len(t.tracking))
	filterable := true
	for id, info := range t.tracking {
		if info.AutoDiscovered {
			continue
		}
		ids = append(ids, id)
		if len(id) <= 6 {
			filterable = false
		}
	}
	if len(ids) > 0 && filterable {
		filters = append(filters, client.BudlistFilter(ids...))
	}

	// Range filter for area tracking.
	if t.trackArea != nil {
		filters = append(filters, client.RangeFilter(t.trackArea.Latitude, t.trackArea.Longitude, t.trackAreaRadius))
	}

	if len(filters) > 0 {
		t.aprs.Filter = client.CombineFilters(filters...)
	} else if len(ids) > 0 {
		// Short IDs without area — no filter (full feed).
		t.aprs.Filter = ""
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
	t.summaryMsgID = 0
	if t.stopCh != nil {
		close(t.stopCh)
		t.stopCh = nil
	}
	t.aprs.Disconnect()
}

func (t *Tracker) loadDevices() {
	devices, err := ddb.GetDevices()
	if err != nil {
		log.Printf("failed to load OGN device database: %v", err)
		return
	}
	m := ddb.LookupByID(devices)
	t.mu.Lock()
	t.devices = m
	t.mu.Unlock()
	log.Printf("loaded %d devices from OGN database", len(m))
}

// keyboard returns an inline keyboard based on current tracker state.
// Must be called with t.mu held.
func (t *Tracker) keyboard() *models.InlineKeyboardMarkup {
	if !t.sessionActive {
		return nil
	}
	hasContent := len(t.tracking) > 0 || t.trackArea != nil
	if !hasContent {
		return nil
	}

	var driverRow []models.InlineKeyboardButton
	driverRow = append(driverRow, models.InlineKeyboardButton{Text: "🚗 Driver", CallbackData: "driver"})
	for _, d := range t.drivers {
		if d.Pos != nil {
			driverRow = append(driverRow, models.InlineKeyboardButton{Text: "🚗 Stop", CallbackData: "driver_off"})
			break
		}
	}

	areaBtn := models.InlineKeyboardButton{Text: "📡 Area", CallbackData: "area"}
	if t.trackArea != nil {
		areaBtn = models.InlineKeyboardButton{Text: "📡 Area off", CallbackData: "area_off"}
	}

	if t.trackingOn {
		return &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{
					{Text: "⏹ Stop", CallbackData: "track_off"},
					{Text: "📋 List", CallbackData: "list"},
					{Text: "📍 Landing", CallbackData: "landing"},
				},
				{areaBtn, driverRow[0]},
			},
		}
	}
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "▶️ Track", CallbackData: "track_on"},
				{Text: "📋 List", CallbackData: "list"},
				{Text: "🔄 Reset", CallbackData: "session_reset"},
			},
		},
	}
}

func NewTracker() *Tracker {
	t := &Tracker{
		aprs:     client.New("N0CALL", ""),
		tracking: make(map[string]*TrackInfo),
		drivers:  make(map[int64]*DriverInfo),
	}

	// Restore previous session.
	t.mu.Lock()
	resumeTracking := t.loadState()
	if resumeTracking {
		t.updateFilter()
	}
	t.mu.Unlock()
	t.resumeOnStart = resumeTracking

	go t.loadDevices()
	return t
}

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
	b.RegisterHandler(bot.HandlerTypeMessageText, "/driver", bot.MatchTypeCommand, t.cmdDriver)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/driver_off", bot.MatchTypeCommand, t.cmdDriverOff)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/area", bot.MatchTypeCommand, t.cmdArea)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/area_off", bot.MatchTypeCommand, t.cmdAreaOff)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/tz", bot.MatchTypeCommand, t.cmdTz)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/help", bot.MatchTypeCommand, t.cmdHelp)
	b.RegisterHandler(bot.HandlerTypeMessageText, "/start", bot.MatchTypeCommand, t.cmdStart)

	// Inline button callbacks.
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "track_on", bot.MatchTypeExact, t.cbTrackOn)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "track_off", bot.MatchTypeExact, t.cbTrackOff)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "list", bot.MatchTypeExact, t.cbList)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "landing", bot.MatchTypeExact, t.cbLanding)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "driver", bot.MatchTypeExact, t.cbDriver)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "driver_off", bot.MatchTypeExact, t.cbDriverOff)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "area", bot.MatchTypeExact, t.cbArea)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "area_off", bot.MatchTypeExact, t.cbAreaOff)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "session_reset", bot.MatchTypeExact, t.cbSessionReset)

	// Auto-resume tracking if it was active before restart.
	if t.resumeOnStart {
		t.mu.Lock()
		t.trackingOn = true
		t.stopCh = make(chan struct{})
		stopCh := t.stopCh
		t.mu.Unlock()
		go t.runClient(stopCh)
		go t.sendUpdates(stopCh)
		log.Println("auto-resumed tracking from saved session")
	}
}

func (t *Tracker) DefaultHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	log.Printf("DefaultHandler: msg=%v, cb=%v, edited=%v", update.Message != nil, update.CallbackQuery != nil, update.EditedMessage != nil)
	// Handle pickup callback queries (dynamic IDs, can't use exact match).
	if update.CallbackQuery != nil {
		cq := update.CallbackQuery
		if strings.HasPrefix(cq.Data, "pickup:") {
			t.answerCallback(ctx, b, cq)
			if !t.isTrusted(cq.From.ID) {
				return
			}
			id := cq.Data[7:]
			t.execPickup(ctx, b, id)
			return
		}
		return
	}

	// Handle driver live location updates (edited messages).
	if update.EditedMessage != nil && update.EditedMessage.Location != nil {
		t.mu.Lock()
		for _, d := range t.drivers {
			if d.MsgID != 0 && update.EditedMessage.ID == d.MsgID {
				d.Pos = &Coordinates{
					Latitude:  update.EditedMessage.Location.Latitude,
					Longitude: update.EditedMessage.Location.Longitude,
				}
				break
			}
		}
		t.mu.Unlock()
		return
	}

	if update.Message == nil || update.Message.From == nil {
		return
	}
	if !t.isTrusted(update.Message.From.ID) {
		return
	}
	if update.Message.Location != nil {
		t.handleLocation(ctx, b, update.Message)
	}
}

func commandArgs(text string) string {
	if i := strings.Index(text, " "); i != -1 {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

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
	OwnerUserID    int64
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

// GroupSession holds all session-specific state for a single chat.
type GroupSession struct {
	ChatID          int64
	Tracking        map[string]*TrackInfo
	TrackingOn      bool
	Landing         *Coordinates
	TrackArea       *Coordinates
	TrackAreaRadius int
	Timezone        *time.Location
	Drivers         map[int64]*DriverInfo
	SummaryMsgID    int
	// Runtime (not persisted):
	StopCh         chan struct{}
	WaitingLanding bool
	LandingExpiry  time.Time
	WaitingArea    bool
	AreaExpiry     time.Time
}

// tz returns the session's timezone, defaulting to UTC.
func (s *GroupSession) tz() *time.Location {
	if s != nil && s.Timezone != nil {
		return s.Timezone
	}
	return time.UTC
}

// replyKeyboard returns an inline keyboard based on current session state.
// Must be called with t.mu held.
func (s *GroupSession) replyKeyboard() *models.ReplyKeyboardMarkup {
	if s == nil {
		return nil
	}
	hasContent := len(s.Tracking) > 0 || s.TrackArea != nil
	if !hasContent {
		return nil
	}

	if s.TrackingOn {
		areaText := "📡 Зона"
		if s.TrackArea != nil {
			areaText = "📡 Зона ✕"
		}
		row2 := []models.KeyboardButton{{Text: areaText}, {Text: "🚗 Водитель"}}
		return &models.ReplyKeyboardMarkup{
			Keyboard: [][]models.KeyboardButton{
				{
					{Text: "⏹ Стоп"},
					{Text: "📋 Список"},
					{Text: "📍 Посадка"},
				},
				row2,
			},
			ResizeKeyboard: true,
		}
	}
	return &models.ReplyKeyboardMarkup{
		Keyboard: [][]models.KeyboardButton{
			{
				{Text: "▶️ Трекинг"},
				{Text: "📋 Список"},
				{Text: "🔄 Сброс"},
			},
		},
		ResizeKeyboard: true,
	}
}

// stopTracking disables tracking and signals goroutines to exit.
// Must be called with t.mu held. Requires aprs client to disconnect.
func (s *GroupSession) stopTracking(aprs *client.Client) {
	if s == nil || !s.TrackingOn {
		return
	}
	s.TrackingOn = false
	s.SummaryMsgID = 0
	if s.StopCh != nil {
		close(s.StopCh)
		s.StopCh = nil
	}
	aprs.Disconnect()
}

// UserInfo represents a known user across sessions.
type UserInfo struct {
	UserID       int64
	Username     string
	OGNID        string
	DisplayName  string
	DMChatID     int64
	PendingGroup int64
}

type Tracker struct {
	bot           *bot.Bot
	botUsername   string
	aprs          *client.Client
	devices       map[string]ddb.Device
	mu            sync.Mutex
	session       *GroupSession
	users         map[int64]*UserInfo
	resumeOnStart bool
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

func bearingName(deg float64) string {
	deg = math.Mod(deg+360, 360)
	names := []string{"N", "NE", "E", "SE", "S", "SW", "W", "NW"}
	idx := int(math.Round(deg/45)) % 8
	return names[idx]
}

func formatBearing(deg float64) string {
	deg = math.Mod(deg+360, 360)
	return fmt.Sprintf("(%.0f° | %s)", deg, bearingName(deg))
}

// tz returns the session timezone, defaulting to UTC.
// Must be called with t.mu held.
func (t *Tracker) tz() *time.Location {
	if t.session != nil {
		return t.session.tz()
	}
	return time.UTC
}

func isGroupChat(chat models.Chat) bool {
	return chat.Type == "group" || chat.Type == "supergroup"
}

func isPrivateChat(chat models.Chat) bool {
	return chat.Type == "private"
}

func mapsNavURL(lat, lon float64) string {
	return fmt.Sprintf("https://www.google.com/maps/dir/?api=1&destination=%.6f,%.6f", lat, lon)
}

// updateFilter rebuilds the APRS filter based on tracked IDs and area.
func (t *Tracker) updateFilter() {
	if t.session == nil {
		return
	}
	s := t.session
	var filters []string

	// Budlist for explicitly added IDs.
	ids := make([]string, 0, len(s.Tracking))
	filterable := true
	for id, info := range s.Tracking {
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
	if s.TrackArea != nil {
		filters = append(filters, client.RangeFilter(s.TrackArea.Latitude, s.TrackArea.Longitude, s.TrackAreaRadius))
	}

	if len(filters) > 0 {
		t.aprs.Filter = client.CombineFilters(filters...)
	} else if len(ids) > 0 {
		// Short IDs without area — no filter (full feed).
		t.aprs.Filter = ""
	} else {
		t.aprs.Filter = ""
	}
	log.Printf("[filter] updated: %q (ids=%d, area=%v)", t.aprs.Filter, len(ids), s.TrackArea != nil)
	if s.TrackingOn {
		t.aprs.Disconnect()
	}
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

func NewTracker() *Tracker {
	t := &Tracker{
		aprs:  client.New("N0CALL", ""),
		users: make(map[int64]*UserInfo),
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

// ensureUser returns or creates a UserInfo for the given Telegram user.
// Must be called with t.mu held.
func (t *Tracker) ensureUser(from *models.User) *UserInfo {
	u, ok := t.users[from.ID]
	if !ok {
		u = &UserInfo{UserID: from.ID}
		t.users[from.ID] = u
	}
	u.Username = from.Username
	if u.DisplayName == "" {
		u.DisplayName = strings.TrimSpace(from.FirstName + " " + from.LastName)
	}
	return u
}

func (t *Tracker) RegisterHandlers(b *bot.Bot) {
	t.bot = b

	// Fetch bot username for deep links.
	if me, err := b.GetMe(context.Background()); err == nil {
		t.botUsername = me.Username
		log.Printf("bot username: @%s", t.botUsername)
	} else {
		log.Printf("failed to get bot info: %v", err)
	}

	b.RegisterHandler(bot.HandlerTypeMessageText, "start_session", bot.MatchTypeCommand, t.cmdStartSession)
	b.RegisterHandler(bot.HandlerTypeMessageText, "session_reset", bot.MatchTypeCommand, t.cmdSessionReset)
	b.RegisterHandler(bot.HandlerTypeMessageText, "add", bot.MatchTypeCommand, t.cmdAdd)
	b.RegisterHandler(bot.HandlerTypeMessageText, "remove", bot.MatchTypeCommand, t.cmdRemove)
	b.RegisterHandler(bot.HandlerTypeMessageText, "track_on", bot.MatchTypeCommand, t.cmdTrackOn)
	b.RegisterHandler(bot.HandlerTypeMessageText, "track_off", bot.MatchTypeCommand, t.cmdTrackOff)
	b.RegisterHandler(bot.HandlerTypeMessageText, "list", bot.MatchTypeCommand, t.cmdList)
	b.RegisterHandler(bot.HandlerTypeMessageText, "status", bot.MatchTypeCommand, t.cmdStatus)
	b.RegisterHandler(bot.HandlerTypeMessageText, "landing", bot.MatchTypeCommand, t.cmdLanding)
	b.RegisterHandler(bot.HandlerTypeMessageText, "driver", bot.MatchTypeCommand, t.cmdDriver)
	b.RegisterHandler(bot.HandlerTypeMessageText, "driver_off", bot.MatchTypeCommand, t.cmdDriverOff)
	b.RegisterHandler(bot.HandlerTypeMessageText, "area", bot.MatchTypeCommand, t.cmdArea)
	b.RegisterHandler(bot.HandlerTypeMessageText, "area_off", bot.MatchTypeCommand, t.cmdAreaOff)
	b.RegisterHandler(bot.HandlerTypeMessageText, "tz", bot.MatchTypeCommand, t.cmdTz)
	b.RegisterHandler(bot.HandlerTypeMessageText, "help", bot.MatchTypeCommand, t.cmdHelp)
	b.RegisterHandler(bot.HandlerTypeMessageText, "debug_wipe", bot.MatchTypeCommand, t.cmdDebugWipe)
	b.RegisterHandler(bot.HandlerTypeMessageText, "myid", bot.MatchTypeCommand, t.cmdMyID)
	b.RegisterHandler(bot.HandlerTypeMessageText, "confirm", bot.MatchTypeCommand, t.cmdConfirm)
	b.RegisterHandler(bot.HandlerTypeMessageText, "start", bot.MatchTypeCommand, t.cmdStart)

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
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "track_off_confirm", bot.MatchTypeExact, t.cbTrackOffConfirm)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "track_off_cancel", bot.MatchTypeExact, t.cbTrackOffCancel)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "session_reset_confirm", bot.MatchTypeExact, t.cbSessionResetConfirm)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "session_reset_wipe", bot.MatchTypeExact, t.cbSessionResetWipe)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "session_reset_cancel", bot.MatchTypeExact, t.cbSessionResetCancel)

	// Auto-resume tracking if it was active before restart.
	if t.resumeOnStart && t.session != nil {
		t.mu.Lock()
		t.session.TrackingOn = true
		t.session.StopCh = make(chan struct{})
		stopCh := t.session.StopCh
		t.mu.Unlock()
		go t.runClient(stopCh)
		go t.sendUpdates(stopCh)
		log.Println("auto-resumed tracking from saved session")
	}
}

func (t *Tracker) DefaultHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
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
		if t.session != nil {
			for _, d := range t.session.Drivers {
				if d.MsgID != 0 && update.EditedMessage.ID == d.MsgID {
					d.Pos = &Coordinates{
						Latitude:  update.EditedMessage.Location.Latitude,
						Longitude: update.EditedMessage.Location.Longitude,
					}
					break
				}
			}
		}
		t.mu.Unlock()
		return
	}

	if update.Message == nil || update.Message.From == nil {
		return
	}
	m := update.Message
	if !t.isTrusted(m.From.ID) {
		return
	}

	// Handle location messages (landing, area, driver).
	if m.Location != nil {
		t.handleLocation(ctx, b, m)
		return
	}

	// Handle DM text (not a command) for pending OGN ID input.
	if m.Text != "" && isPrivateChat(m.Chat) && !strings.HasPrefix(m.Text, "/") {
		t.handleDMText(ctx, b, m)
		return
	}

	// Route reply keyboard button presses (group only).
	if m.Text != "" && isGroupChat(m.Chat) {
		chatID := m.Chat.ID
		switch m.Text {
		case "▶️ Трекинг":
			if t.requireSession(ctx, b, chatID) {
				t.execTrackOn(ctx, b, chatID)
			}
		case "⏹ Стоп":
			if t.requireSession(ctx, b, chatID) {
				t.askTrackOffConfirm(ctx, b, chatID)
			}
		case "📋 Список":
			if t.requireSession(ctx, b, chatID) {
				t.execList(ctx, b, chatID)
			}
		case "📍 Посадка":
			if t.requireSession(ctx, b, chatID) {
				t.execLanding(ctx, b, chatID)
			}
		case "📡 Зона":
			if t.requireSession(ctx, b, chatID) {
				t.execArea(ctx, b, chatID, 100)
			}
		case "📡 Зона ✕":
			if t.requireSession(ctx, b, chatID) {
				t.execAreaOff(ctx, b, chatID)
			}
		case "🚗 Водитель":
			if t.requireSession(ctx, b, chatID) {
				t.execDriver(ctx, b, chatID, m.From.ID, m.From.Username)
			}
		case "🔄 Сброс":
			if t.requireSession(ctx, b, chatID) {
				t.askSessionResetConfirm(ctx, b, chatID)
			}
		}
	}
}

func (t *Tracker) handleDMText(ctx context.Context, b *bot.Bot, m *models.Message) {
	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	if u.PendingGroup == 0 {
		t.mu.Unlock()
		return
	}

	// Check that the pending group session exists.
	s := t.session
	if s == nil || s.ChatID != u.PendingGroup {
		pendingGroup := u.PendingGroup
		u.PendingGroup = 0
		t.saveState()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   fmt.Sprintf("Группа %d не найдена. Попросите добавить вас заново.", pendingGroup),
		}); err != nil {
			log.Printf("failed to send pending group not found: %v", err)
		}
		return
	}

	id := shortID(m.Text)
	groupChatID := s.ChatID

	// Add to session (reset status — fresh add).
	name := u.DisplayName
	if info, ok := s.Tracking[id]; ok {
		info.Name = name
		info.Username = u.Username
		info.OwnerUserID = u.UserID
		info.AutoDiscovered = false
		info.Status = StatusFlying
		info.LandingTime = time.Time{}
	} else {
		s.Tracking[id] = &TrackInfo{
			Name:        name,
			Username:    u.Username,
			OwnerUserID: u.UserID,
		}
	}

	u.OGNID = id
	u.PendingGroup = 0
	t.updateFilter()
	kb := s.replyKeyboard()
	t.saveState()
	t.mu.Unlock()

	// Confirm in DM.
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   fmt.Sprintf("Добавлен %s в группу", id),
	}); err != nil {
		log.Printf("failed to confirm add in DM: %v", err)
	}

	// Confirm in group.
	label := id
	if name != "" {
		label = id + " (" + name + ")"
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      groupChatID,
		Text:        "Добавлен " + label,
		ReplyMarkup: kb,
	}); err != nil {
		log.Printf("failed to confirm add in group: %v", err)
	}
}

func commandArgs(text string) string {
	if i := strings.Index(text, " "); i != -1 {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

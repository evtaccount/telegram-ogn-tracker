package tracker

import (
	"time"

	"ogn/parser"

	"github.com/go-telegram/bot/models"
)

// PilotStatus represents the current state of a tracked pilot.
type PilotStatus int

const (
	StatusFlying PilotStatus = iota
	StatusLanded
	StatusPickedUp
)

// TrackInfo holds tracking state for a single pilot/aircraft.
type TrackInfo struct {
	MessageID        int                     // Telegram message ID for the live-location pin
	Position         *parser.PositionMessage // last position received from OGN
	Name             string
	Username         string
	LastUpdate       time.Time
	Status           PilotStatus
	LandingTime      time.Time
	LandingConfirmed bool      // true if pilot confirmed landing via DM button
	LowSpeedSince    time.Time // start of the low-speed window used for landing detection
	AutoDiscovered   bool      // discovered automatically via the area-tracking zone
	OwnerUserID      int64     // Telegram user ID of the tracker's owner
	// LastHeading caches the most recent non-zero course in degrees so the
	// live-location arrow does not jump back to north when OGN reports
	// Course=0 with non-zero speed (a known limitation of the data feed).
	// Runtime-only; not persisted.
	LastHeading int
}

// StatusEmoji returns an emoji reflecting the pilot's current state.
// Confirmed landings get ✅, auto-detected (unconfirmed) get 🪂.
func (ti *TrackInfo) StatusEmoji() string {
	switch ti.Status {
	case StatusLanded:
		if ti.LandingConfirmed {
			return "✅"
		}
		return "🪂"
	case StatusPickedUp:
		return "✅"
	default:
		return "✈️"
	}
}

func (ti *TrackInfo) DisplayName() string {
	if ti.Name != "" {
		return ti.Name
	}
	return ti.Username
}

// RadarEntry holds a single aircraft observation during radar mode.
type RadarEntry struct {
	Position     *parser.PositionMessage
	LastSeen     time.Time
	AircraftType int
	DDBInfo      string
}

// Coordinates represents a geographic point (WGS84).
type Coordinates struct {
	Latitude  float64
	Longitude float64
}

// DriverInfo holds state for a driver who can pick up landed pilots.
type DriverInfo struct {
	Pos     *Coordinates
	MsgID   int       // Telegram message ID for the driver's live-location pin
	Waiting bool      // true while waiting for the driver to send a live location
	Expiry  time.Time // deadline for sending the location
	WaitGen int       // wait generation — used to cancel stale timers
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
	// DM landing flow uses a per-user flag so a stray location pin from
	// another DM user can't satisfy a different user's pending request.
	WaitingDMLandingFor int64
	DMLandingExpiry     time.Time
	WaitingArea         bool
	AreaExpiry          time.Time
	// Radar mode (runtime only):
	RadarOn            bool
	RadarRadius        int // radar-specific radius (may differ from TrackAreaRadius)
	RadarEntries       map[string]*RadarEntry
	RadarMsgID         int
	RadarStopCh        chan struct{}
	WaitingRadarRadius bool
	RadarRadiusExpiry  time.Time
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

	if s.RadarOn {
		return &models.ReplyKeyboardMarkup{
			Keyboard: [][]models.KeyboardButton{
				{
					{Text: "⏹ Радар стоп"},
					{Text: "📡 Радиус"},
				},
			},
			ResizeKeyboard: true,
		}
	}

	if s.TrackingOn {
		areaText := "📡 Зона"
		if s.TrackArea != nil {
			areaText = "📡 Зона ✕"
		}
		return &models.ReplyKeyboardMarkup{
			Keyboard: [][]models.KeyboardButton{
				{
					{Text: "⏹ Стоп"},
					{Text: "📋 Список"},
				},
				{
					{Text: areaText},
					{Text: "🚗 Водитель"},
				},
			},
			ResizeKeyboard: true,
		}
	}
	var rows [][]models.KeyboardButton
	if hasContent {
		row1 := []models.KeyboardButton{
			{Text: "▶️ Старт"},
			{Text: "📋 Список"},
			{Text: "🔄 Завершить"},
		}
		rows = append(rows, row1)
		if s.TrackArea != nil {
			rows = append(rows, []models.KeyboardButton{{Text: "📡 Радар"}})
		}
	} else {
		rows = append(rows, []models.KeyboardButton{
			{Text: "➕ Добавить"},
			{Text: "📡 Зона"},
			{Text: "🔄 Завершить"},
		})
	}
	return &models.ReplyKeyboardMarkup{
		Keyboard:       rows,
		ResizeKeyboard: true,
	}
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

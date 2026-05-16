package tracker

import (
	"time"

	"ogn/parser"
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
	// LabelMsgID is the Telegram message ID of the text "label" sent right
	// before the live-location pin. The pin replies to this label so the chat
	// shows "Eugene (FE0E4A)" above the otherwise-anonymous map preview.
	LabelMsgID int
	// LabelStatus is the PilotStatus reflected by the label's emoji on the
	// last edit. Used to skip Telegram round-trips when nothing has changed.
	LabelStatus PilotStatus
	// LiveLocationDead is set after Telegram permanently refuses to edit the
	// live-location pin (see isMessageGone). Once true, the ticker stops
	// editing this pin to avoid log spam — and does not re-send a new pin,
	// since the pilot already has earlier history in the chat.
	LiveLocationDead bool
	// LabelDead — same for the per-pilot text label (info.LabelMsgID).
	LabelDead bool
	// LandedFinalEditDone marks that we performed exactly one edit cycle after
	// landing was detected (to move the pin to the final landing coordinates).
	// Subsequent cycles skip the pin so the chat doesn't keep getting silent
	// edits on a stationary message.
	LandedFinalEditDone bool
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
	DashboardMsgID    int
	// DashboardPinned is true once PinChatMessage succeeded for the current
	// DashboardMsgID. Persisted so a restart doesn't re-pin (and re-notify) an
	// already-pinned message.
	DashboardPinned bool
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
	// PendingCleanup tracks transient group-message IDs that should be
	// deleted in one batch when the user's currently-active flow concludes.
	// Keyed by Telegram user ID. Used only for multi-step flows (/add no-arg,
	// /track_off confirm, /session_reset confirm, /landing). Runtime-only,
	// not persisted — bot restart drops pending batches.
	PendingCleanup map[int64][]int
	// InactivityWarnedAt is the time the chat-side warning about a long beacon
	// silence was posted on this session. Used to make sure the warning fires
	// exactly once before auto-stop. Runtime only — a restart resets it, which
	// is fine: if silence persists past the threshold, the warning re-fires.
	InactivityWarnedAt time.Time
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

// UserInfo represents a known user across sessions.
type UserInfo struct {
	UserID       int64
	Username     string
	OGNID        string
	DisplayName  string
	DMChatID     int64
	PendingGroup int64
}

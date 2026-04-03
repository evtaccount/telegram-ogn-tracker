package tracker

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

const sessionFile = "data/session.json"

// sessionState is the JSON-serialisable snapshot of the tracker.
type sessionState struct {
	ChatID          int64              `json:"chat_id"`
	SessionActive   bool               `json:"session_active"`
	TrackingOn      bool               `json:"tracking_on"`
	Tracking        map[string]*pilotState `json:"tracking,omitempty"`
	Landing         *Coordinates       `json:"landing,omitempty"`
	TrackArea       *Coordinates       `json:"track_area,omitempty"`
	TrackAreaRadius int                `json:"track_area_radius,omitempty"`
	Timezone        string             `json:"timezone,omitempty"`
}

type pilotState struct {
	Name           string      `json:"name,omitempty"`
	Username       string      `json:"username,omitempty"`
	Status         PilotStatus `json:"status"`
	LandingTime    time.Time   `json:"landing_time,omitempty"`
	AutoDiscovered bool        `json:"auto_discovered,omitempty"`
}

// saveState writes the current session to disk. Must be called with t.mu held.
func (t *Tracker) saveState() {
	state := sessionState{
		ChatID:          t.chatID,
		SessionActive:   t.sessionActive,
		TrackingOn:      t.trackingOn,
		Landing:         t.landing,
		TrackArea:       t.trackArea,
		TrackAreaRadius: t.trackAreaRadius,
	}
	if t.timezone != nil {
		state.Timezone = t.timezone.String()
	}
	if len(t.tracking) > 0 {
		state.Tracking = make(map[string]*pilotState, len(t.tracking))
		for id, info := range t.tracking {
			state.Tracking[id] = &pilotState{
				Name:           info.Name,
				Username:       info.Username,
				Status:         info.Status,
				LandingTime:    info.LandingTime,
				AutoDiscovered: info.AutoDiscovered,
			}
		}
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("failed to marshal session state: %v", err)
		return
	}

	dir := filepath.Dir(sessionFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("failed to create data dir: %v", err)
		return
	}

	tmp := sessionFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("failed to write session file: %v", err)
		return
	}
	if err := os.Rename(tmp, sessionFile); err != nil {
		log.Printf("failed to rename session file: %v", err)
	}
}

// loadState restores session from disk. Must be called with t.mu held.
func (t *Tracker) loadState() bool {
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("failed to read session file: %v", err)
		}
		return false
	}

	var state sessionState
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("failed to unmarshal session state: %v", err)
		return false
	}

	t.chatID = state.ChatID
	t.sessionActive = state.SessionActive
	t.landing = state.Landing
	t.trackArea = state.TrackArea
	t.trackAreaRadius = state.TrackAreaRadius
	if state.Timezone != "" {
		if loc, err := time.LoadLocation(state.Timezone); err == nil {
			t.timezone = loc
		}
	}

	if len(state.Tracking) > 0 {
		for id, ps := range state.Tracking {
			t.tracking[id] = &TrackInfo{
				Name:           ps.Name,
				Username:       ps.Username,
				Status:         ps.Status,
				LandingTime:    ps.LandingTime,
				AutoDiscovered: ps.AutoDiscovered,
			}
		}
	}

	log.Printf("restored session: %d tracked, active=%v, tracking=%v",
		len(t.tracking), state.SessionActive, state.TrackingOn)
	return state.TrackingOn
}

// clearStateFile removes the session file from disk.
func clearStateFile() {
	if err := os.Remove(sessionFile); err != nil && !os.IsNotExist(err) {
		log.Printf("failed to remove session file: %v", err)
	}
}

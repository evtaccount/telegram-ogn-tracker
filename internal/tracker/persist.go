package tracker

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

const sessionFile = "data/session.json"

// appState is the new top-level JSON-serialisable format.
type appState struct {
	Session *sessionState            `json:"session,omitempty"`
	Users   map[int64]*userState     `json:"users,omitempty"`
}

type userState struct {
	Username    string `json:"username,omitempty"`
	OGNID       string `json:"ogn_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	DMChatID    int64  `json:"dm_chat_id,omitempty"`
}

// sessionState is the JSON-serialisable snapshot of a group session.
type sessionState struct {
	ChatID          int64                  `json:"chat_id"`
	TrackingOn      bool                   `json:"tracking_on"`
	Tracking        map[string]*pilotState `json:"tracking,omitempty"`
	Landing         *Coordinates           `json:"landing,omitempty"`
	TrackArea       *Coordinates           `json:"track_area,omitempty"`
	TrackAreaRadius int                    `json:"track_area_radius,omitempty"`
	Timezone        string                 `json:"timezone,omitempty"`
}

type pilotState struct {
	Name           string      `json:"name,omitempty"`
	Username       string      `json:"username,omitempty"`
	Status         PilotStatus `json:"status"`
	LandingTime    time.Time   `json:"landing_time,omitempty"`
	AutoDiscovered bool        `json:"auto_discovered,omitempty"`
	OwnerUserID    int64       `json:"owner_user_id,omitempty"`
}

// legacySessionState represents the old format (pre-Phase 1) for migration.
type legacySessionState struct {
	ChatID          int64                  `json:"chat_id"`
	SessionActive   bool                   `json:"session_active"`
	TrackingOn      bool                   `json:"tracking_on"`
	Tracking        map[string]*pilotState `json:"tracking,omitempty"`
	Landing         *Coordinates           `json:"landing,omitempty"`
	TrackArea       *Coordinates           `json:"track_area,omitempty"`
	TrackAreaRadius int                    `json:"track_area_radius,omitempty"`
	Timezone        string                 `json:"timezone,omitempty"`
}

// saveState writes the current session to disk. Must be called with t.mu held.
func (t *Tracker) saveState() {
	state := appState{}

	if t.session != nil {
		s := t.session
		ss := &sessionState{
			ChatID:          s.ChatID,
			TrackingOn:      s.TrackingOn,
			Landing:         s.Landing,
			TrackArea:       s.TrackArea,
			TrackAreaRadius: s.TrackAreaRadius,
		}
		if s.Timezone != nil {
			ss.Timezone = s.Timezone.String()
		}
		if len(s.Tracking) > 0 {
			ss.Tracking = make(map[string]*pilotState, len(s.Tracking))
			for id, info := range s.Tracking {
				ss.Tracking[id] = &pilotState{
					Name:           info.Name,
					Username:       info.Username,
					Status:         info.Status,
					LandingTime:    info.LandingTime,
					AutoDiscovered: info.AutoDiscovered,
					OwnerUserID:    info.OwnerUserID,
				}
			}
		}
		state.Session = ss
	}

	if len(t.users) > 0 {
		state.Users = make(map[int64]*userState, len(t.users))
		for uid, u := range t.users {
			state.Users[uid] = &userState{
				Username:    u.Username,
				OGNID:       u.OGNID,
				DisplayName: u.DisplayName,
				DMChatID:    u.DMChatID,
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
// Returns true if tracking should be auto-resumed.
func (t *Tracker) loadState() bool {
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("failed to read session file: %v", err)
		}
		return false
	}

	// Try new format first.
	var state appState
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("failed to unmarshal session state: %v", err)
		return false
	}

	// Migration: if "session" field is absent, try old format.
	// Old format has "chat_id" at top level and "session_active" field.
	if state.Session == nil {
		var legacy legacySessionState
		if err := json.Unmarshal(data, &legacy); err != nil {
			log.Printf("failed to unmarshal legacy session state: %v", err)
			return false
		}
		// Only migrate if the old format had an active session.
		if legacy.ChatID != 0 && legacy.SessionActive {
			state.Session = &sessionState{
				ChatID:          legacy.ChatID,
				TrackingOn:      legacy.TrackingOn,
				Tracking:        legacy.Tracking,
				Landing:         legacy.Landing,
				TrackArea:       legacy.TrackArea,
				TrackAreaRadius: legacy.TrackAreaRadius,
				Timezone:        legacy.Timezone,
			}
			log.Printf("migrated legacy session format to new format")
		} else if legacy.ChatID != 0 {
			// Old format existed but session was not active — still restore.
			state.Session = &sessionState{
				ChatID:          legacy.ChatID,
				TrackingOn:      legacy.TrackingOn,
				Tracking:        legacy.Tracking,
				Landing:         legacy.Landing,
				TrackArea:       legacy.TrackArea,
				TrackAreaRadius: legacy.TrackAreaRadius,
				Timezone:        legacy.Timezone,
			}
			log.Printf("migrated legacy session format (inactive) to new format")
		}
	}

	// Restore users.
	if len(state.Users) > 0 {
		for uid, us := range state.Users {
			t.users[uid] = &UserInfo{
				UserID:      uid,
				Username:    us.Username,
				OGNID:       us.OGNID,
				DisplayName: us.DisplayName,
				DMChatID:    us.DMChatID,
			}
		}
	}

	// Restore session.
	if state.Session == nil {
		log.Printf("no session to restore")
		return false
	}

	ss := state.Session
	session := &GroupSession{
		ChatID:          ss.ChatID,
		Tracking:        make(map[string]*TrackInfo),
		TrackingOn:      false, // will be set by caller if resuming
		Landing:         ss.Landing,
		TrackArea:       ss.TrackArea,
		TrackAreaRadius: ss.TrackAreaRadius,
		Drivers:         make(map[int64]*DriverInfo),
	}
	if ss.Timezone != "" {
		if loc, err := time.LoadLocation(ss.Timezone); err == nil {
			session.Timezone = loc
		}
	}
	if len(ss.Tracking) > 0 {
		for id, ps := range ss.Tracking {
			session.Tracking[id] = &TrackInfo{
				Name:           ps.Name,
				Username:       ps.Username,
				Status:         ps.Status,
				LandingTime:    ps.LandingTime,
				AutoDiscovered: ps.AutoDiscovered,
				OwnerUserID:    ps.OwnerUserID,
			}
		}
	}

	t.session = session

	log.Printf("restored session: %d tracked, chatID=%d, tracking=%v",
		len(session.Tracking), ss.ChatID, ss.TrackingOn)
	return ss.TrackingOn
}

// clearStateFile removes the session file from disk.
func clearStateFile() {
	if err := os.Remove(sessionFile); err != nil && !os.IsNotExist(err) {
		log.Printf("failed to remove session file: %v", err)
	}
}

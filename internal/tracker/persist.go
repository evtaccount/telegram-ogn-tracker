package tracker

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const sessionFile = "data/session.json"

// appState is the new top-level JSON-serialisable format.
type appState struct {
	Session *sessionState        `json:"session,omitempty"`
	Users   map[int64]*userState `json:"users,omitempty"`
}

// userState is the JSON-serialisable snapshot of a user's profile.
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
	SummaryMsgID    int                    `json:"summary_msg_id,omitempty"`
	SummaryPinned   bool                   `json:"summary_pinned,omitempty"`
}

// pilotState is the JSON-serialisable snapshot of a tracked pilot.
type pilotState struct {
	Name             string      `json:"name,omitempty"`
	Username         string      `json:"username,omitempty"`
	Status           PilotStatus `json:"status"`
	LandingTime      time.Time   `json:"landing_time,omitempty"`
	LandingConfirmed bool        `json:"landing_confirmed,omitempty"`
	AutoDiscovered   bool        `json:"auto_discovered,omitempty"`
	OwnerUserID      int64       `json:"owner_user_id,omitempty"`
	// MessageID lets us continue editing the existing live-location message
	// after a restart instead of orphaning it. Telegram messages live for 24h.
	MessageID int `json:"message_id,omitempty"`
	// LowSpeedSince preserves landing-detector progress across restarts. On
	// load, values older than the staleness window are reset (see loadState).
	LowSpeedSince time.Time `json:"low_speed_since,omitempty"`
	// LabelMsgID and LabelStatus persist the text-label state so a restart
	// keeps editing the same label instead of orphaning it next to a fresh one.
	LabelMsgID  int         `json:"label_msg_id,omitempty"`
	LabelStatus PilotStatus `json:"label_status,omitempty"`
	// LiveLocationDead / LabelDead persist the "Telegram refused to edit"
	// verdict so a restart doesn't waste an API round-trip rediscovering a
	// dead message.
	LiveLocationDead bool `json:"live_location_dead,omitempty"`
	LabelDead        bool `json:"label_dead,omitempty"`
	// LandedFinalEditDone is set after the post-landing grace edit cycle
	// completes, so we never repeat that edit across restarts.
	LandedFinalEditDone bool `json:"landed_final_edit_done,omitempty"`
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

// staleLowSpeedWindow caps how old a persisted LowSpeedSince timestamp can be
// before we treat it as obsolete. Detector progress is preserved across short
// restarts but reset after long downtime to avoid false-positive landings.
const staleLowSpeedWindow = 5 * time.Minute

// marshalStateLocked builds a JSON snapshot of the in-memory state.
// Must be called with t.mu held. Returns nil on marshalling failure.
func (t *Tracker) marshalStateLocked() []byte {
	state := appState{}

	if t.session != nil {
		s := t.session
		ss := &sessionState{
			ChatID:          s.ChatID,
			TrackingOn:      s.TrackingOn,
			Landing:         s.Landing,
			TrackArea:       s.TrackArea,
			TrackAreaRadius: s.TrackAreaRadius,
			SummaryMsgID:    s.SummaryMsgID,
			SummaryPinned:   s.SummaryPinned,
		}
		if s.Timezone != nil {
			ss.Timezone = s.Timezone.String()
		}
		if len(s.Tracking) > 0 {
			ss.Tracking = make(map[string]*pilotState, len(s.Tracking))
			for id, info := range s.Tracking {
				ss.Tracking[id] = &pilotState{
					Name:                info.Name,
					Username:            info.Username,
					Status:              info.Status,
					LandingTime:         info.LandingTime,
					LandingConfirmed:    info.LandingConfirmed,
					AutoDiscovered:      info.AutoDiscovered,
					OwnerUserID:         info.OwnerUserID,
					MessageID:           info.MessageID,
					LowSpeedSince:       info.LowSpeedSince,
					LabelMsgID:          info.LabelMsgID,
					LabelStatus:         info.LabelStatus,
					LiveLocationDead:    info.LiveLocationDead,
					LabelDead:           info.LabelDead,
					LandedFinalEditDone: info.LandedFinalEditDone,
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
		slog.Error("failed to marshal session state", "err", err)
		return nil
	}
	return data
}

// writeStateBytes atomically persists the given snapshot. Safe to call without
// holding t.mu — performs no Tracker access.
func writeStateBytes(data []byte) {
	dir := filepath.Dir(sessionFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("failed to create data dir", "err", err)
		return
	}
	// Atomic write: stage to a temp file then rename, so a crash mid-write
	// does not corrupt the canonical state file.
	tmp := sessionFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Error("failed to write session file", "err", err)
		return
	}
	if err := os.Rename(tmp, sessionFile); err != nil {
		slog.Error("failed to rename session file", "err", err)
	}
}

// saveState requests asynchronous persistence. Must be called with t.mu held.
// Snapshots the state under the lock and hands it off to the save worker;
// pending older snapshots are dropped in favour of the newest one. After
// Shutdown the call becomes a no-op.
func (t *Tracker) saveState() {
	if t.shuttingDown {
		return
	}
	data := t.marshalStateLocked()
	if data == nil {
		return
	}
	// Replace any stale pending snapshot with the fresh one.
	select {
	case <-t.saveCh:
	default:
	}
	select {
	case t.saveCh <- data:
	default:
	}
}

// saveWorker drains queued snapshots and writes them to disk. Exits when
// saveCh is closed (during Shutdown).
func (t *Tracker) saveWorker() {
	defer close(t.saveDone)
	for data := range t.saveCh {
		writeStateBytes(data)
	}
}

// loadState restores session from disk. Must be called with t.mu held.
// Returns true if tracking should be auto-resumed.
func (t *Tracker) loadState() bool {
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("failed to read session file", "err", err)
		}
		return false
	}

	// Try new format first.
	var state appState
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Error("failed to unmarshal session state", "err", err)
		return false
	}

	// Migration: if "session" field is absent, try old format.
	// Old format has "chat_id" at top level and "session_active" field.
	if state.Session == nil {
		var legacy legacySessionState
		if err := json.Unmarshal(data, &legacy); err != nil {
			slog.Error("failed to unmarshal legacy session state", "err", err)
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
			slog.Info("migrated legacy session format to new format")
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
			slog.Info("migrated legacy session format (inactive) to new format")
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
		slog.Info("no session to restore")
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
		SummaryMsgID:    ss.SummaryMsgID,
		SummaryPinned:   ss.SummaryPinned,
	}
	if ss.Timezone != "" {
		if loc, err := time.LoadLocation(ss.Timezone); err == nil {
			session.Timezone = loc
		}
	}
	if len(ss.Tracking) > 0 {
		for id, ps := range ss.Tracking {
			low := ps.LowSpeedSince
			// Drop detector progress if the persisted window is older than the
			// staleness threshold — long downtime would otherwise produce a
			// spurious "landed" verdict on the next beacon.
			if !low.IsZero() && time.Since(low) > staleLowSpeedWindow {
				low = time.Time{}
			}
			session.Tracking[id] = &TrackInfo{
				Name:                ps.Name,
				Username:            ps.Username,
				Status:              ps.Status,
				LandingTime:         ps.LandingTime,
				LandingConfirmed:    ps.LandingConfirmed,
				AutoDiscovered:      ps.AutoDiscovered,
				OwnerUserID:         ps.OwnerUserID,
				MessageID:           ps.MessageID,
				LowSpeedSince:       low,
				LabelMsgID:          ps.LabelMsgID,
				LabelStatus:         ps.LabelStatus,
				LiveLocationDead:    ps.LiveLocationDead,
				LabelDead:           ps.LabelDead,
				LandedFinalEditDone: ps.LandedFinalEditDone,
			}
		}
	}

	t.session = session

	slog.Info("restored session",
		"tracked", len(session.Tracking),
		"chat_id", ss.ChatID,
		"tracking", ss.TrackingOn)
	return ss.TrackingOn
}

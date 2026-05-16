package tracker

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"sort"
	"strings"
	"time"

	"ogn/client"
	"ogn/parser"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	staleThreshold     = 5 * time.Minute  // beacons older than this get a "stale data" badge
	updateInterval     = 30 * time.Second // interval for refreshing the summary and live-locations
	liveLocationPeriod = 86400            // Telegram live-location lifetime in seconds (24h)
	reconnectDelay     = 1 * time.Second  // initial delay before retrying after an OGN connection error
	reconnectMaxDelay  = 60 * time.Second // ceiling for the exponential backoff
	// inactivityWarnAfter — how long since the last beacon before posting a
	// "no beacons, will stop soon" warning to the chat. Reset when beacons
	// start flowing again.
	inactivityWarnAfter = 2 * time.Hour
	// inactivityStopAfter — how long since the last beacon before turning the
	// tracker off automatically. Catches the common "everyone landed and we
	// forgot to /track_off" case so the bot doesn't sit idle on an open APRS
	// connection for days.
	inactivityStopAfter = 8 * time.Hour
)

// nextReconnectDelay doubles the current backoff, capped at reconnectMaxDelay.
// Used by runClient and runRadarClient to avoid hammering the OGN APRS server
// when it is unavailable for an extended period.
func nextReconnectDelay(d time.Duration) time.Duration {
	d *= 2
	if d > reconnectMaxDelay {
		return reconnectMaxDelay
	}
	return d
}

// markLiveLocationDead flags a pilot's live-location pin as permanently
// uneditable after Telegram returned an isMessageGone error. The flag prevents
// the ticker from spamming further EditMessageLiveLocation calls (and the log
// from filling up with the same error). Logged once at WARN.
func (t *Tracker) markLiveLocationDead(id string, err error) {
	t.mu.Lock()
	var msgID int
	if t.session != nil {
		if ti, ok := t.session.Tracking[id]; ok {
			if ti.LiveLocationDead {
				t.mu.Unlock()
				return
			}
			ti.LiveLocationDead = true
			msgID = ti.MessageID
		}
	}
	t.mu.Unlock()
	slog.Warn("live location gone", "id", id, "msg_id", msgID, "err", err)
}

// markLabelDead is the label-message equivalent of markLiveLocationDead.
func (t *Tracker) markLabelDead(id string, err error) {
	t.mu.Lock()
	var msgID int
	if t.session != nil {
		if ti, ok := t.session.Tracking[id]; ok {
			if ti.LabelDead {
				t.mu.Unlock()
				return
			}
			ti.LabelDead = true
			msgID = ti.LabelMsgID
		}
	}
	t.mu.Unlock()
	slog.Warn("live label gone", "id", id, "msg_id", msgID, "err", err)
}

// refreshDashboard renders the dashboard text and inline keyboard for the
// current session state and either edits the existing dashboard message or
// posts a new one. Idempotent: safe to call any number of times per tick and
// from any goroutine. Handles the "edit on a dead message" path by reposting
// and re-pinning, same semantics as the previous summary block.
//
// Returns the live MessageID after the call. Must be called outside t.mu.
//
// Pin policy: unlike the old summary, the dashboard is the chat's anchor from
// /start onward — we pin on first render regardless of whether any pilot has
// reported yet. There is no withPos gate.
func (t *Tracker) refreshDashboard(ctx context.Context, chatID int64) int {
	t.mu.Lock()
	if t.session == nil || t.session.ChatID != chatID {
		t.mu.Unlock()
		return 0
	}
	s := t.session
	b := t.bot
	dashID := s.DashboardMsgID
	dashPinned := s.DashboardPinned
	// Deep-copy every collection the renderer touches so we can release the
	// lock before any Telegram round-trip without racing other goroutines.
	// runRadarClient mutates s.RadarEntries; command handlers mutate s.Drivers,
	// s.Tracking, s.Landing, s.TrackArea. Reading any of them off-lock would
	// risk a "concurrent map iteration and write" fatal.
	tracking := make(map[string]*TrackInfo, len(s.Tracking))
	for id, info := range s.Tracking {
		cp := *info
		tracking[id] = &cp
	}
	radarEntries := make(map[string]*RadarEntry, len(s.RadarEntries))
	for id, e := range s.RadarEntries {
		cp := *e
		radarEntries[id] = &cp
	}
	driverCount := len(s.Drivers)
	var landingCopy *Coordinates
	if s.Landing != nil {
		c := *s.Landing
		landingCopy = &c
	}
	var areaCopy *Coordinates
	if s.TrackArea != nil {
		c := *s.TrackArea
		areaCopy = &c
	}
	devices := t.devices
	tz := s.tz()
	// Snapshot every primitive used by the renderer; rebuild a session shell
	// whose pointer/map fields all point at the deep copies above.
	sCopy := GroupSession{
		ChatID:             s.ChatID,
		Tracking:           tracking,
		TrackingOn:         s.TrackingOn,
		Landing:            landingCopy,
		TrackArea:          areaCopy,
		TrackAreaRadius:    s.TrackAreaRadius,
		Timezone:           s.Timezone,
		Drivers:            make(map[int64]*DriverInfo, driverCount), // only len() is read; entries don't need to be live
		DashboardMsgID:     s.DashboardMsgID,
		DashboardPinned:    s.DashboardPinned,
		InactivityWarnedAt: s.InactivityWarnedAt,
		RadarOn:            s.RadarOn,
		RadarRadius:        s.RadarRadius,
		RadarEntries:       radarEntries,
	}
	// Fill placeholder entries so len(sCopy.Drivers) is correct in the
	// renderer's `🚗 N водитель(ей)` line. The renderer never reads driver
	// values, so a nil placeholder is safe.
	for i := range driverCount {
		sCopy.Drivers[int64(i)] = nil
	}
	t.mu.Unlock()

	if b == nil {
		return dashID
	}

	text := buildDashboard(&sCopy, devices, tz)
	kb := dashboardButtons(&sCopy)
	// Pilot-specific buttons (nav, pickup) live on the dashboard too, below the
	// action row, so the chat has one place for everything.
	pilotKb := pilotButtons(tracking)
	if pilotKb != nil {
		if kb == nil {
			kb = pilotKb
		} else {
			kb.InlineKeyboard = append(kb.InlineKeyboard, pilotKb.InlineKeyboard...)
		}
	}

	newID := 0
	if dashID != 0 {
		_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      chatID,
			MessageID:   dashID,
			Text:        text,
			ReplyMarkup: kb,
		})
		switch {
		case err == nil, isMessageNotModified(err):
			newID = dashID
		case isMessageGone(err):
			slog.Warn("dashboard gone, will repost", "msg_id", dashID, "err", err)
			t.mu.Lock()
			if t.session != nil {
				t.session.DashboardMsgID = 0
				t.session.DashboardPinned = false
			}
			t.mu.Unlock()
			dashID = 0
			dashPinned = false
		default:
			slog.Error("failed to edit dashboard", "err", err)
			newID = dashID
		}
	}
	if dashID == 0 {
		params := &bot.SendMessageParams{ChatID: chatID, Text: text}
		if kb != nil {
			params.ReplyMarkup = kb
		}
		msg, err := b.SendMessage(ctx, params)
		if err != nil {
			slog.Error("failed to send dashboard", "err", err)
			return 0
		}
		newID = msg.ID
		t.mu.Lock()
		if t.session != nil {
			t.session.DashboardMsgID = msg.ID
		}
		t.mu.Unlock()
	}

	if newID != 0 && !dashPinned {
		if _, err := b.PinChatMessage(ctx, &bot.PinChatMessageParams{
			ChatID:              chatID,
			MessageID:           newID,
			DisableNotification: true,
		}); err != nil {
			slog.Warn("failed to pin dashboard", "err", err)
		} else {
			slog.Info("dashboard pinned", "msg_id", newID)
			t.mu.Lock()
			if t.session != nil {
				t.session.DashboardPinned = true
			}
			t.mu.Unlock()
		}
	}
	return newID
}

// checkInactivity implements the post-silence warn-and-auto-stop policy. Age
// is the time since the most recent beacon across tracked pilots. The method
// holds t.mu only while touching session state; chat messages are sent without
// the lock. Returns true when tracking was auto-stopped (caller should exit
// its goroutine — no further work is meaningful).
func (t *Tracker) checkInactivity(ctx context.Context, b *bot.Bot, chatID int64, age time.Duration) (stopped bool) {
	if age >= inactivityStopAfter {
		t.mu.Lock()
		if t.session == nil || !t.session.TrackingOn {
			t.mu.Unlock()
			return false
		}
		t.stopTrackingAsync()
		t.saveState()
		t.mu.Unlock()
		slog.Info("tracking auto-stopped due to inactivity", "chat_id", chatID, "age", age.Round(time.Minute))
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   fmt.Sprintf("⏹ Трекинг остановлен автоматически: нет beacon-ов %s.", age.Round(time.Minute)),
		}); err != nil {
			slog.Error("failed to send auto-stop notice", "err", err)
		}
		return true
	}
	if age >= inactivityWarnAfter {
		t.mu.Lock()
		warned := t.session == nil || !t.session.InactivityWarnedAt.IsZero()
		if !warned && t.session != nil {
			t.session.InactivityWarnedAt = time.Now()
		}
		t.mu.Unlock()
		if !warned {
			slog.Info("inactivity warning surfaced", "chat_id", chatID, "age", age.Round(time.Minute))
		}
		// The warning text appears inside the dashboard via buildDashboard,
		// rendered by the heartbeat refresh on the next tick. No separate
		// chat message — keeps the chat tidy.
		return false
	}
	// Beacons are flowing again — clear the warning flag so silence later in
	// the session re-triggers the policy.
	t.mu.Lock()
	if t.session != nil && !t.session.InactivityWarnedAt.IsZero() {
		t.session.InactivityWarnedAt = time.Time{}
	}
	t.mu.Unlock()
	return false
}

// shouldAttemptPin reports whether the summary message warrants a pin attempt
// on this tick. We only pin once per session (so we don't re-notify after a
// transient unpin race) and only after at least one pilot has reported a
// position — pinning an empty "waiting for data" board would be premature.
func shouldAttemptPin(pinned bool, withPos int) bool {
	return !pinned && withPos > 0
}

// buildFilter assembles the APRS filter for a session from the explicitly
// tracked pilots and the optional area zone. Pure function, no Tracker state
// touched — easy to unit-test.
//
// Returns:
//   - filter:     the combined APRS filter string (empty if nothing to track)
//   - callsigns:  expanded budlist (each short ID becomes one entry per OGN prefix)
//   - trackedIDs: short IDs explicitly added (excludes auto-discovered)
func buildFilter(s *GroupSession) (filter string, callsigns []string, trackedIDs []string) {
	if s == nil {
		return "", nil, nil
	}
	for id, info := range s.Tracking {
		if info.AutoDiscovered {
			continue
		}
		trackedIDs = append(trackedIDs, id)
		// APRS budlist requires full callsigns (e.g. FLRFD0E8D).
		// Short 6-char IDs are expanded with all known OGN prefixes.
		if len(id) <= 6 {
			for _, prefix := range ognPrefixes {
				callsigns = append(callsigns, prefix+id)
			}
		} else {
			callsigns = append(callsigns, id)
		}
	}
	var parts []string
	if len(callsigns) > 0 {
		parts = append(parts, client.BudlistFilter(callsigns...))
	}
	if s.TrackArea != nil {
		parts = append(parts, client.RangeFilter(s.TrackArea.Latitude, s.TrackArea.Longitude, s.TrackAreaRadius))
	}
	if len(parts) > 0 {
		filter = client.CombineFilters(parts...)
	}
	return filter, callsigns, trackedIDs
}

// updateFilter rebuilds the APRS filter based on tracked IDs and area, and
// either patches the idle client or restarts the goroutine fleet with a fresh
// client. Caller must hold t.mu.
func (t *Tracker) updateFilter() {
	if t.session == nil {
		return
	}
	s := t.session
	filter, callsigns, trackedIDs := buildFilter(s)

	if !s.TrackingOn {
		// No goroutines using the client — just patch the filter for next start.
		t.aprs.Filter = filter
		slog.Info("aprs filter updated (idle)",
			"filter", filter,
			"tracked_ids", trackedIDs,
			"callsigns", callsigns,
			"area", s.TrackArea != nil)
		return
	}

	// Restart client goroutines with a fresh client.
	// Disconnect() permanently kills the client (killed=true), so we must create
	// a new instance. The shutdown of the old client is dispatched to a goroutine
	// to avoid holding t.mu across Disconnect — the APRS callback also takes t.mu.
	oldStopCh := s.StopCh
	oldAprs := t.aprs
	newAprs := client.New("N0CALL", filter)
	newAprs.Logger = log.Default()
	t.aprs = newAprs
	newStopCh := make(chan struct{})
	s.StopCh = newStopCh

	slog.Info("aprs filter restarting",
		"filter", filter,
		"tracked_ids", trackedIDs,
		"callsigns", callsigns,
		"area", s.TrackArea != nil)

	go func() {
		if oldStopCh != nil {
			close(oldStopCh)
		}
		_ = oldAprs.Disconnect()
	}()
	go t.runClient(newStopCh, newAprs)
	go t.sendUpdates(newStopCh)
}

// stopTrackingAsync flips tracking off and disconnects the APRS client in a
// detached goroutine. The async hand-off prevents a deadlock: the APRS callback
// inside Run() acquires t.mu, so calling Disconnect() while holding t.mu would
// wedge if Disconnect ever waited for the callback to return.
// Caller must hold t.mu.
func (t *Tracker) stopTrackingAsync() {
	s := t.session
	if s == nil || !s.TrackingOn {
		return
	}
	chatID := s.ChatID
	oldSummaryID := s.DashboardMsgID
	wasPinned := s.DashboardPinned
	s.TrackingOn = false
	s.DashboardMsgID = 0
	s.DashboardPinned = false
	stopCh := s.StopCh
	s.StopCh = nil
	aprs := t.aprs
	go func() {
		if stopCh != nil {
			close(stopCh)
		}
		_ = aprs.Disconnect()
	}()
	if wasPinned {
		t.unpinSummaryAsync(chatID, oldSummaryID)
	}
}

// clearDashboardForReset deletes the pinned dashboard message and resets the
// session's dashboard bookkeeping. Caller must hold t.mu. Telegram deletion
// runs in a detached goroutine so the caller can stay under the lock.
func (t *Tracker) clearDashboardForReset() {
	s := t.session
	if s == nil {
		return
	}
	msgID := s.DashboardMsgID
	chatID := s.ChatID
	s.DashboardMsgID = 0
	s.DashboardPinned = false
	if msgID != 0 {
		t.deleteMessagesAsync(chatID, msgID)
	}
}

// unpinSummaryAsync fires a best-effort UnpinChatMessage in a goroutine so the
// caller (which typically holds t.mu) doesn't block on a Telegram round-trip
// or risk a deadlock with the bot's update path. No-ops when there's nothing
// to unpin or the bot isn't wired yet.
func (t *Tracker) unpinSummaryAsync(chatID int64, msgID int) {
	if msgID == 0 || t.bot == nil {
		return
	}
	b := t.bot
	go func() {
		if _, err := b.UnpinChatMessage(context.Background(), &bot.UnpinChatMessageParams{
			ChatID:    chatID,
			MessageID: msgID,
		}); err != nil {
			slog.Warn("failed to unpin summary", "err", err)
		}
	}()
}

// stopRadarAsync flips radar off and disconnects the APRS client asynchronously.
// See stopTrackingAsync for rationale. Caller must hold t.mu.
func (t *Tracker) stopRadarAsync() {
	s := t.session
	if s == nil || !s.RadarOn {
		return
	}
	s.RadarOn = false
	s.RadarMsgID = 0
	s.RadarEntries = nil
	s.WaitingRadarRadius = false
	stopCh := s.RadarStopCh
	s.RadarStopCh = nil
	aprs := t.aprs
	go func() {
		if stopCh != nil {
			close(stopCh)
		}
		_ = aprs.Disconnect()
	}()
}

// runClient connects to the OGN APRS server and processes position messages
// in an infinite reconnect loop until stopCh is closed.
// The aprs client is passed explicitly so the goroutine binds to the client it
// was launched with; the Tracker.aprs field can be reassigned by other goroutines
// without racing on this read path.
func (t *Tracker) runClient(stopCh <-chan struct{}, aprs *client.Client) {
	slog.Info("OGN client started")
	delay := reconnectDelay
	for {
		select {
		case <-stopCh:
			slog.Info("OGN client stopped")
			return
		default:
		}

		err := aprs.Run(func(line string) {
			// APRS server status lines ("# aprsc 2.1.x …") dominate the raw
			// feed — they outnumber real beacons ~30:1. Drop them at source so
			// even DEBUG logs stay readable; the parse path also rejects them,
			// so nothing downstream cares.
			if strings.HasPrefix(line, "#") {
				return
			}
			slog.Debug("ogn line", "line", line)
			msg, err := parser.ParsePosition(line)
			if err != nil {
				return
			}
			origID := msg.Callsign
			id := shortID(origID)

			slog.Debug("ogn beacon parsed",
				"callsign", msg.Callsign, "dst", msg.DstCall,
				"receiver", msg.ReceiverName, "relay", msg.Relay,
				"ts", msg.Timestamp.Format("15:04:05"),
				"lat", msg.Latitude, "lon", msg.Longitude, "alt", msg.Altitude,
				"course", msg.Course, "speed", msg.GroundSpeed, "climb", msg.ClimbRate,
				"turn", msg.TurnRate, "snr", msg.SignalQuality, "err_count", msg.ErrorCount,
				"freq_offset", msg.FreqOffset, "gps", msg.GPSQuality, "fl", msg.FlightLevel,
				"power", msg.SignalPower, "sw", msg.SoftwareVer, "hw", msg.HardwareVer,
				"addr", msg.Address, "aircraft_type", msg.AircraftType,
				"real_addr", msg.RealAddress, "stealth", msg.Stealth,
				"no_tracking", msg.NoTracking, "comment", msg.UserComment)

			var alert *landingEvent

			t.mu.Lock()
			s := t.session
			if s == nil {
				t.mu.Unlock()
				return
			}
			info, ok := s.Tracking[id]
			// Auto-discover aircraft from area tracking.
			if !ok && s.TrackArea != nil {
				info = &TrackInfo{AutoDiscovered: true}
				s.Tracking[id] = info
				ok = true
				slog.Info("auto-discovered aircraft in area", "id", id)
			}
			if !ok {
				// Beacon passed the upstream filter but is not currently tracked.
				// Useful when debugging "why isn't my id showing": confirms the
				// beacon reaches us but no Tracking entry matches.
				slog.Debug("ogn beacon not tracked", "id", id, "callsign", origID)
			} else if info.Status == StatusPickedUp || (info.Status == StatusLanded && info.LandingConfirmed) {
				// Pilot is no longer of interest (picked up or confirmed landed)
				// — silently drop. Logging every beacon here adds hundreds of
				// debug lines per session for no diagnostic value; the status
				// transition itself is already logged when it happens.
			} else {
				slog.Debug("ogn beacon matched",
					"id", id, "callsign", origID,
					"lat", msg.Latitude, "lon", msg.Longitude,
					"speed", msg.GroundSpeed, "climb", msg.ClimbRate,
					"course", msg.Course, "alt", msg.Altitude,
					"status", info.Status)
				info.Position = msg
				info.LastUpdate = time.Now()
				if msg.Course > 0 {
					info.LastHeading = msg.Course
				}
				if updateLandingState(info, msg, time.Now()) {
					alert = &landingEvent{
						id:   id,
						name: info.DisplayName(),
						lat:  msg.Latitude,
						lon:  msg.Longitude,
						alt:  msg.Altitude,
						time: info.LandingTime,
						tz:   s.tz(),
					}
					slog.Info("landing detected", "id", id, "lat", msg.Latitude, "lon", msg.Longitude)
				}
			}
			chatID := s.ChatID
			if alert != nil {
				t.saveState()
			}
			t.mu.Unlock()

			if alert != nil {
				t.sendLandingAlert(alert, chatID)
			}
		}, false)
		if err != nil {
			slog.Error("ogn client error", "retry_in", delay, "err", err)
			select {
			case <-stopCh:
				slog.Info("OGN client stopped")
				return
			case <-time.After(delay):
			}
			delay = nextReconnectDelay(delay)
		} else {
			delay = reconnectDelay
		}
	}
}

// sendLandingAlert sends a notification to the group when a pilot lands,
// with navigation and pickup buttons.
func (t *Tracker) sendLandingAlert(e *landingEvent, chatID int64) {
	b := t.bot
	if b == nil {
		return
	}

	label := e.id
	if e.name != "" {
		label = e.name
	}
	text := fmt.Sprintf("🪂 %s сел!", label)
	text += fmt.Sprintf("\nВысота: %.0fм  ⏱ %s", e.alt, e.time.In(e.tz).Format("15:04:05"))

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "🗺 Навигация", URL: mapsNavURL(e.lat, e.lon)},
				{Text: "✅ Забрал", CallbackData: "pickup:" + e.id},
			},
		},
	}

	ctx := context.Background()
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: kb,
	}); err != nil {
		slog.Error("failed to send landing alert", "id", e.id, "err", err)
	}
}

// sendUpdates runs a 30-second ticker that updates live locations on the map
// and edits (or sends) the pinned summary message in the group chat.
func (t *Tracker) sendUpdates(stopCh <-chan struct{}) {
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()
	// Per-cycle heartbeat counter: stats are logged once every statsLogEvery
	// cycles, ~5 min at 30s ticks. The first cycle (cycleN==0) also logs so
	// startup snapshot is visible.
	const statsLogEvery = 10
	var cycleN int
	// lastStats captures the previous heartbeat so we can also log on change,
	// not just on the timed cadence.
	var lastStats [3]int // tracked, withPos, withoutPos
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
		}
		cycleN++

		t.mu.Lock()
		s := t.session
		if s == nil {
			t.mu.Unlock()
			continue
		}
		chatID := s.ChatID
		b := t.bot
		local := make(map[string]*TrackInfo)
		for id, info := range s.Tracking {
			cp := *info
			local[id] = &cp
		}
		t.mu.Unlock()

		// Heartbeat: emit a per-cycle snapshot so it's obvious from the logs
		// whether OGN beacons are reaching the bot, which pilots have valid
		// positions, and how stale the most recent fix is.
		var withPos, withoutPos int
		var latest time.Time
		for _, info := range local {
			if info.Position != nil {
				withPos++
				if info.LastUpdate.After(latest) {
					latest = info.LastUpdate
				}
			} else {
				withoutPos++
			}
		}
		// Log a per-cycle heartbeat snapshot, but throttle it: once per
		// statsLogEvery cycles, plus on any state change. That cuts the steady-
		// state debug volume by ~10× without losing visibility into transitions
		// (a new beacon arriving, a pilot dropping off, etc).
		cur := [3]int{len(local), withPos, withoutPos}
		if cycleN == 1 || cycleN%statsLogEvery == 0 || cur != lastStats {
			stats := []any{
				"tracked", cur[0],
				"with_position", cur[1],
				"without_position", cur[2],
			}
			if !latest.IsZero() {
				stats = append(stats, "last_beacon_age", time.Since(latest).Round(time.Second))
			}
			slog.Debug("ogn cycle stats", stats...)
			lastStats = cur
		}

		if b == nil || len(local) == 0 {
			continue
		}

		ctx := context.Background()

		// Inactivity policy: warn the chat once after inactivityWarnAfter and
		// auto-stop after inactivityStopAfter. Only runs once we have seen at
		// least one beacon during this session — a freshly-started tracker with
		// nothing yet to compare against is left alone (operator's call to make).
		if !latest.IsZero() {
			if stopped := t.checkInactivity(ctx, b, chatID, time.Since(latest)); stopped {
				return
			}
		}

		// Update per-pilot live locations on the map (skip auto-discovered).
		// Each pilot has a paired text label that names them; the label is
		// sent right before the first live-location pin and the pin replies to
		// it so the chat shows "Eugene (FE0E4A)" above the otherwise-anonymous
		// map preview. The label's emoji tracks status (✈️ → 🪂 → ✅) and is
		// edited in place on status transitions.
		for id, info := range local {
			if info.AutoDiscovered {
				continue
			}

			// Reflect status changes on the label even after we stop touching
			// the live-location pin (picked up / confirmed landing). Without
			// this the emoji would be frozen at whatever it was at the last
			// position update.
			if info.LabelMsgID != 0 && !info.LabelDead && info.Status != info.LabelStatus {
				newText := pilotLabelText(id, info)
				_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    chatID,
					MessageID: info.LabelMsgID,
					Text:      newText,
				})
				switch {
				case err == nil, isMessageNotModified(err):
					t.mu.Lock()
					if t.session != nil {
						if ti, ok := t.session.Tracking[id]; ok {
							ti.LabelStatus = info.Status
						}
					}
					t.mu.Unlock()
				case isMessageGone(err):
					t.markLabelDead(id, err)
				default:
					slog.Error("failed to edit pilot label", "id", id, "err", err)
				}
			}

			if info.Position == nil || info.Status == StatusPickedUp {
				continue
			}
			// After landing we do one final edit (to move the pin to the actual
			// landing spot) and then freeze. LandedFinalEditDone is set after
			// that single edit so subsequent cycles short-circuit here.
			if info.Status == StatusLanded && (info.LandingConfirmed || info.LandedFinalEditDone) {
				continue
			}

			heading := chooseHeading(info.Position.Course, info.LastHeading, info.Position.GroundSpeed)

			if info.MessageID != 0 {
				if info.LiveLocationDead {
					continue
				}
				editParams := &bot.EditMessageLiveLocationParams{
					ChatID:    chatID,
					MessageID: info.MessageID,
					Latitude:  info.Position.Latitude,
					Longitude: info.Position.Longitude,
				}
				if heading > 0 {
					editParams.Heading = heading
				}
				_, err := b.EditMessageLiveLocation(ctx, editParams)
				switch {
				case err == nil, isMessageNotModified(err):
					// no-op — edit accepted or message was identical
				case isMessageGone(err):
					t.markLiveLocationDead(id, err)
				default:
					slog.Error("failed to edit location", "id", id, "err", err)
				}
				// If this was the post-landing grace cycle, record that we did
				// it so the next tick stops editing entirely.
				if info.Status == StatusLanded && !info.LandedFinalEditDone {
					t.mu.Lock()
					if t.session != nil {
						if ti, ok := t.session.Tracking[id]; ok {
							ti.LandedFinalEditDone = true
						}
					}
					t.mu.Unlock()
				}
				continue
			}

			// First-time send: ensure the label exists, then send the
			// live-location as a reply to it. If the label send fails we
			// skip the pin so they retry together on the next tick — better
			// to delay tracking by 30s than to orphan an unlabelled pin.
			if info.LabelMsgID == 0 {
				labelMsg, err := b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: chatID,
					Text:   pilotLabelText(id, info),
				})
				if err != nil {
					slog.Error("failed to send pilot label", "id", id, "err", err)
					continue
				}
				t.mu.Lock()
				if t.session != nil {
					if ti, ok := t.session.Tracking[id]; ok {
						ti.LabelMsgID = labelMsg.ID
						ti.LabelStatus = info.Status
					}
				}
				t.mu.Unlock()
				info.LabelMsgID = labelMsg.ID
				info.LabelStatus = info.Status
			}

			sendParams := &bot.SendLocationParams{
				ChatID:     chatID,
				Latitude:   info.Position.Latitude,
				Longitude:  info.Position.Longitude,
				LivePeriod: liveLocationPeriod,
				ReplyParameters: &models.ReplyParameters{
					MessageID:                info.LabelMsgID,
					AllowSendingWithoutReply: true,
				},
			}
			if heading > 0 {
				sendParams.Heading = heading
			}
			locMsg, err := b.SendLocation(ctx, sendParams)
			if err != nil {
				slog.Error("failed to send location", "id", id, "err", err)
				continue
			}
			t.mu.Lock()
			if t.session != nil {
				if ti, ok := t.session.Tracking[id]; ok {
					ti.MessageID = locMsg.ID
				}
			}
			t.mu.Unlock()
			slog.Info("sent location", "id", id)
		}

		// Refresh the dashboard. Heartbeat path; explicit state changes also
		// call refreshDashboard directly so the UI doesn't lag a tick behind.
		t.refreshDashboard(ctx, chatID)
	}
}

// --- Radar mode ---

// runRadarClient connects to OGN APRS and collects all positions in the area.
// Unlike runClient, it does not do landing detection or modify session.Tracking.
// See runClient for why aprs is passed explicitly.
func (t *Tracker) runRadarClient(stopCh <-chan struct{}, aprs *client.Client) {
	slog.Info("Radar client started")
	delay := reconnectDelay
	for {
		select {
		case <-stopCh:
			slog.Info("Radar client stopped")
			return
		default:
		}

		err := aprs.Run(func(line string) {
			msg, err := parser.ParsePosition(line)
			if err != nil {
				return
			}
			id := shortID(msg.Callsign)

			t.mu.Lock()
			s := t.session
			if s == nil || !s.RadarOn {
				t.mu.Unlock()
				return
			}
			entry, ok := s.RadarEntries[id]
			if !ok {
				entry = &RadarEntry{DDBInfo: formatDDBInfo(t.devices, id)}
				s.RadarEntries[id] = entry
			}
			entry.Position = msg
			entry.LastSeen = time.Now()
			entry.AircraftType = msg.AircraftType
			t.mu.Unlock()
		}, false)
		if err != nil {
			slog.Error("radar client error", "retry_in", delay, "err", err)
			select {
			case <-stopCh:
				slog.Info("Radar client stopped")
				return
			case <-time.After(delay):
			}
			delay = nextReconnectDelay(delay)
		} else {
			delay = reconnectDelay
		}
	}
}

// sendRadarUpdates periodically sends/edits a summary message with all aircraft in the area.
func (t *Tracker) sendRadarUpdates(stopCh <-chan struct{}) {
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
		}

		t.mu.Lock()
		s := t.session
		if s == nil || !s.RadarOn {
			t.mu.Unlock()
			continue
		}
		chatID := s.ChatID
		radarMsgID := s.RadarMsgID
		center := s.TrackArea
		radius := s.RadarRadius
		tz := s.tz()

		// Prune stale entries.
		now := time.Now()
		for id, e := range s.RadarEntries {
			if now.Sub(e.LastSeen) > staleThreshold {
				delete(s.RadarEntries, id)
			}
		}

		// Snapshot entries.
		lines := make([]radarLine, 0, len(s.RadarEntries))
		for id, e := range s.RadarEntries {
			lines = append(lines, radarLine{id, *e})
		}
		b := t.bot
		t.mu.Unlock()

		if b == nil {
			continue
		}

		// Sort by altitude descending.
		sort.Slice(lines, func(i, j int) bool {
			if lines[i].entry.Position == nil {
				return false
			}
			if lines[j].entry.Position == nil {
				return true
			}
			return lines[i].entry.Position.Altitude > lines[j].entry.Position.Altitude
		})

		summary := buildRadarSummary(lines, center, radius, tz)
		kb := radarButtons(lines)
		ctx := context.Background()

		if radarMsgID != 0 {
			ep := &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: radarMsgID,
				Text:      summary,
			}
			if kb != nil {
				ep.ReplyMarkup = kb
			}
			_, err := b.EditMessageText(ctx, ep)
			switch {
			case err == nil, isMessageNotModified(err):
				// no-op
			case isMessageGone(err):
				slog.Warn("radar summary gone, will repost", "msg_id", radarMsgID, "err", err)
				t.mu.Lock()
				if t.session != nil {
					t.session.RadarMsgID = 0
				}
				t.mu.Unlock()
				radarMsgID = 0
			default:
				slog.Error("failed to edit radar summary", "err", err)
			}
		}
		if radarMsgID == 0 {
			sp := &bot.SendMessageParams{
				ChatID: chatID,
				Text:   summary,
			}
			if kb != nil {
				sp.ReplyMarkup = kb
			}
			msg, err := b.SendMessage(ctx, sp)
			if err != nil {
				slog.Error("failed to send radar summary", "err", err)
			} else {
				t.mu.Lock()
				if t.session != nil {
					t.session.RadarMsgID = msg.ID
				}
				t.mu.Unlock()
			}
		}
	}
}

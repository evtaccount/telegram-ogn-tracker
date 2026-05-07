package tracker

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"sort"
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
	oldSummaryID := s.SummaryMsgID
	wasPinned := s.SummaryPinned
	s.TrackingOn = false
	s.SummaryMsgID = 0
	s.SummaryPinned = false
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
			} else if info.Status == StatusPickedUp {
				slog.Debug("ogn beacon dropped: pilot picked up", "id", id)
			} else if info.Status == StatusLanded && info.LandingConfirmed {
				slog.Debug("ogn beacon dropped: landing confirmed", "id", id)
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
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
		}

		t.mu.Lock()
		s := t.session
		if s == nil {
			t.mu.Unlock()
			continue
		}
		chatID := s.ChatID
		landing := s.Landing
		var drivers []*Coordinates
		for _, d := range s.Drivers {
			if d.Pos != nil {
				cp := *d.Pos
				drivers = append(drivers, &cp)
			}
		}
		areaRadius := 0
		if s.TrackArea != nil {
			areaRadius = s.TrackAreaRadius
		}
		b := t.bot
		summaryMsgID := s.SummaryMsgID
		summaryPinned := s.SummaryPinned
		local := make(map[string]*TrackInfo)
		for id, info := range s.Tracking {
			cp := *info
			local[id] = &cp
		}
		// Capture renderer dependencies under the mutex so the renderer can
		// run lock-free without racing on t.devices / session.Timezone.
		devices := t.devices
		tz := s.tz()
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
		stats := []any{
			"tracked", len(local),
			"with_position", withPos,
			"without_position", withoutPos,
		}
		if !latest.IsZero() {
			stats = append(stats, "last_beacon_age", time.Since(latest).Round(time.Second))
		}
		slog.Debug("ogn cycle stats", stats...)

		if b == nil || len(local) == 0 {
			continue
		}

		ctx := context.Background()

		// Update per-pilot live locations on the map (skip auto-discovered).
		for id, info := range local {
			if info.Position == nil || info.Status == StatusPickedUp || info.AutoDiscovered {
				continue
			}
			if info.Status == StatusLanded && info.LandingConfirmed {
				continue
			}

			heading := chooseHeading(info.Position.Course, info.LastHeading, info.Position.GroundSpeed)

			if info.MessageID != 0 {
				editParams := &bot.EditMessageLiveLocationParams{
					ChatID:    chatID,
					MessageID: info.MessageID,
					Latitude:  info.Position.Latitude,
					Longitude: info.Position.Longitude,
				}
				if heading > 0 {
					editParams.Heading = heading
				}
				if _, err := b.EditMessageLiveLocation(ctx, editParams); err != nil && !isMessageNotModified(err) {
					slog.Error("failed to edit location", "id", id, "err", err)
				}
			} else {
				sendParams := &bot.SendLocationParams{
					ChatID:     chatID,
					Latitude:   info.Position.Latitude,
					Longitude:  info.Position.Longitude,
					LivePeriod: liveLocationPeriod,
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
		}

		// Send or update the single summary message. newSummaryID tracks
		// whichever ID the summary now lives at — used by the pin block below
		// to know whether we have a stable target to pin.
		summary := buildSummary(local, landing, drivers, areaRadius, devices, tz)
		kb := pilotButtons(local)
		newSummaryID := 0
		if summaryMsgID != 0 {
			editParams := &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: summaryMsgID,
				Text:      summary,
			}
			if kb != nil {
				editParams.ReplyMarkup = kb
			}
			if _, err := b.EditMessageText(ctx, editParams); err != nil && !isMessageNotModified(err) {
				slog.Error("failed to edit summary", "err", err)
			}
			// Treat the message as still alive even on transient edit errors —
			// next tick will retry the edit. Hard "message not found" cases
			// will surface as repeated warnings; addressed separately if it
			// becomes noisy.
			newSummaryID = summaryMsgID
		} else {
			sendParams := &bot.SendMessageParams{
				ChatID: chatID,
				Text:   summary,
			}
			if kb != nil {
				sendParams.ReplyMarkup = kb
			}
			msg, err := b.SendMessage(ctx, sendParams)
			if err != nil {
				slog.Error("failed to send summary", "err", err)
			} else {
				newSummaryID = msg.ID
				t.mu.Lock()
				if t.session != nil {
					t.session.SummaryMsgID = msg.ID
				}
				t.mu.Unlock()
			}
		}

		// Pin the summary so it stays visible regardless of new chat traffic
		// or freshly-sent live-location pins. Silent pin (DisableNotification)
		// suppresses both the push and the "Bot pinned a message" service
		// message. Failures (e.g. missing admin rights) leave SummaryPinned
		// false so the next tick retries; once pinned, the snapshot path
		// short-circuits forever.
		if newSummaryID != 0 && shouldAttemptPin(summaryPinned, withPos) {
			if _, err := b.PinChatMessage(ctx, &bot.PinChatMessageParams{
				ChatID:              chatID,
				MessageID:           newSummaryID,
				DisableNotification: true,
			}); err != nil {
				slog.Warn("failed to pin summary", "err", err)
			} else {
				slog.Info("summary pinned", "msg_id", newSummaryID)
				t.mu.Lock()
				if t.session != nil && t.session.SummaryMsgID == newSummaryID {
					t.session.SummaryPinned = true
					t.saveState()
				}
				t.mu.Unlock()
			}
		}
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
			if _, err := b.EditMessageText(ctx, ep); err != nil && !isMessageNotModified(err) {
				slog.Error("failed to edit radar summary", "err", err)
			}
		} else {
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

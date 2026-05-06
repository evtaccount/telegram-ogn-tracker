package tracker

import (
	"context"
	"fmt"
	"log"
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

// runClient connects to the OGN APRS server and processes position messages
// in an infinite reconnect loop until stopCh is closed.
// The aprs client is passed explicitly so the goroutine binds to the client it
// was launched with; the Tracker.aprs field can be reassigned by other goroutines
// without racing on this read path.
func (t *Tracker) runClient(stopCh <-chan struct{}, aprs *client.Client) {
	log.Println("OGN client started")
	delay := reconnectDelay
	for {
		select {
		case <-stopCh:
			log.Println("OGN client stopped")
			return
		default:
		}

		err := aprs.Run(func(line string) {
			log.Printf("[OGN line] %s", line)
			msg, err := parser.ParsePosition(line)
			if err != nil {
				return
			}
			origID := msg.Callsign
			id := shortID(origID)

			log.Printf("[OGN raw] callsign=%s dst=%s receiver=%s relay=%s ts=%s lat=%.5f lon=%.5f alt=%.0f crs=%d spd=%.1f climb=%.1f turn=%.1f snr=%.1f err=%d foff=%.1f gps=%s fl=%.0f pwr=%.1f sw=%.1f hw=%d addr=%s atype=%d real=%s stealth=%v notrack=%v comment=%q",
				msg.Callsign, msg.DstCall, msg.ReceiverName, msg.Relay,
				msg.Timestamp.Format("15:04:05"), msg.Latitude, msg.Longitude,
				msg.Altitude, msg.Course, msg.GroundSpeed, msg.ClimbRate,
				msg.TurnRate, msg.SignalQuality, msg.ErrorCount, msg.FreqOffset,
				msg.GPSQuality, msg.FlightLevel, msg.SignalPower, msg.SoftwareVer,
				msg.HardwareVer, msg.Address, msg.AircraftType, msg.RealAddress,
				msg.Stealth, msg.NoTracking, msg.UserComment)

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
				log.Printf("auto-discovered %s in area", id)
			}
			if ok && info.Status != StatusPickedUp && !(info.Status == StatusLanded && info.LandingConfirmed) {
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
					log.Printf("landing detected for %s at %.5f,%.5f", id, msg.Latitude, msg.Longitude)
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
			log.Printf("OGN client error (retry in %v): %v", delay, err)
			select {
			case <-stopCh:
				log.Println("OGN client stopped")
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
		log.Printf("failed to send landing alert for %s: %v", e.id, err)
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

			heading := info.Position.Course
			if heading == 0 && info.Position.GroundSpeed > 0 {
				// OGN often reports Course=0 for moving aircraft; reuse the
				// previously seen heading so the live-location arrow doesn't
				// flip back to north. Fall back to 360 only when we have
				// never observed a real heading for this pilot.
				if info.LastHeading > 0 {
					heading = info.LastHeading
				} else {
					heading = 360
				}
			}

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
					log.Printf("failed to edit location for %s: %v", id, err)
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
					log.Printf("failed to send location for %s: %v", id, err)
					continue
				}
				t.mu.Lock()
				if t.session != nil {
					if ti, ok := t.session.Tracking[id]; ok {
						ti.MessageID = locMsg.ID
					}
				}
				t.mu.Unlock()
				log.Printf("sent location for %s", id)
			}
		}

		// Send or update the single summary message.
		summary := buildSummary(local, landing, drivers, areaRadius, devices, tz)
		kb := pilotButtons(local)
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
				log.Printf("failed to edit summary: %v", err)
			}
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
				log.Printf("failed to send summary: %v", err)
			} else {
				t.mu.Lock()
				if t.session != nil {
					t.session.SummaryMsgID = msg.ID
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
	log.Println("Radar client started")
	delay := reconnectDelay
	for {
		select {
		case <-stopCh:
			log.Println("Radar client stopped")
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
			log.Printf("Radar client error (retry in %v): %v", delay, err)
			select {
			case <-stopCh:
				log.Println("Radar client stopped")
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
				log.Printf("failed to edit radar summary: %v", err)
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
				log.Printf("failed to send radar summary: %v", err)
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

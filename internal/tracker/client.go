package tracker

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"ogn/client"
	"ogn/parser"
)

const (
	staleThreshold         = 5 * time.Minute  // данные старше этого помечаются предупреждением
	landingSpeedThreshold  = 5.0              // km/h — ниже скорости пешехода, отсекает GPS-шум
	landingClimbThreshold  = 0.3              // m/s — любой термик даёт > 0.3 м/с набора
	landingConfirmDuration = 90 * time.Second // сколько пилот должен быть неподвижен для подтверждения посадки
	updateInterval         = 30 * time.Second // интервал обновления сводки и live-локаций
	liveLocationPeriod     = 86400            // секунды жизни live-локации в Telegram (24ч)
	reconnectDelay         = 5 * time.Second  // задержка перед переподключением к OGN APRS
)

// landingEvent captures data for a landing alert sent outside the mutex.
type landingEvent struct {
	id   string
	name string
	lat  float64
	lon  float64
	alt  float64
	time time.Time
	tz   *time.Location
}

// runClient connects to the OGN APRS server and processes position messages
// in an infinite reconnect loop until stopCh is closed.
// The aprs client is passed explicitly so the goroutine binds to the client it
// was launched with; the Tracker.aprs field can be reassigned by other goroutines
// without racing on this read path.
func (t *Tracker) runClient(stopCh <-chan struct{}, aprs *client.Client) {
	log.Println("OGN client started")
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

				// Landing detection: speed AND climb rate near zero for landingConfirmDuration.
				// Speed alone is insufficient — a pilot thermalling in a weak lift
				// can have low ground speed but positive climb rate.
				// Altitude above terrain is unavailable without DEM data, so we rely
				// on the combination of near-zero speed and near-zero vertical speed.
				if info.Status == StatusFlying {
					onGround := msg.GroundSpeed < landingSpeedThreshold &&
						math.Abs(msg.ClimbRate) < landingClimbThreshold
					if onGround {
						if info.LowSpeedSince.IsZero() {
							info.LowSpeedSince = time.Now()
						} else if time.Since(info.LowSpeedSince) > landingConfirmDuration {
							info.Status = StatusLanded
							info.LandingTime = time.Now()
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
					} else if !onGround {
						info.LowSpeedSince = time.Time{}
					}
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
			select {
			case <-stopCh:
				log.Println("OGN client stopped")
				return
			default:
				log.Printf("OGN client error: %v", err)
				time.Sleep(reconnectDelay)
			}
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

// nearestDriver returns the distance and bearing from the closest driver to the given point.
func nearestDriver(lat, lon float64, drivers []*Coordinates) (distKm float64, bearing float64, found bool) {
	minDist := math.MaxFloat64
	for _, d := range drivers {
		dist, b := distanceAndBearing(d.Latitude, d.Longitude, lat, lon)
		if dist < minDist {
			minDist = dist
			bearing = b
		}
	}
	if minDist == math.MaxFloat64 {
		return 0, 0, false
	}
	return minDist, bearing, true
}

// formatTrackText builds a multi-line text block for one pilot in the summary message:
// status, altitude, speed, distance to landing, distance from nearest driver.
func (t *Tracker) formatTrackText(id string, info *TrackInfo, landing *Coordinates, drivers []*Coordinates) string {
	pos := info.Position

	// Header: status emoji + ID + name/DDB info.
	text := info.StatusEmoji() + " " + id
	if info.Name != "" {
		text += " — " + info.Name
	} else if info.Username != "" {
		text += " — " + info.Username
	} else if info.AutoDiscovered {
		if ddb := formatDDBInfo(t.devices, id); ddb != "" {
			text += " — " + ddb
		}
		if name, ok := aircraftTypes[pos.AircraftType]; ok && pos.AircraftType > 0 {
			text += " [" + name + "]"
		}
	}
	if info.Status == StatusLanded && !info.LandingTime.IsZero() {
		label := "сел"
		if info.LandingConfirmed {
			label = "подтв."
		}
		text += fmt.Sprintf(" (%s %s)", label, info.LandingTime.In(t.tz()).Format("15:04"))
	}

	// Stale data warning.
	if info.Status == StatusFlying && !info.LastUpdate.IsZero() && time.Since(info.LastUpdate) > staleThreshold {
		mins := int(time.Since(info.LastUpdate).Minutes())
		text += fmt.Sprintf("\n⚠️ Нет данных %d мин", mins)
		text += "\n⏱ " + info.LastUpdate.In(t.tz()).Format("15:04:05")
		return text
	}

	// Flight data lines.
	altLine := fmt.Sprintf("\nВысота: %.0fм", pos.Altitude)
	if info.Status == StatusFlying {
		altLine += fmt.Sprintf(" (%+.1fм/с)", pos.ClimbRate)
	}
	text += altLine
	if pos.GroundSpeed > 0 {
		spdLine := fmt.Sprintf("\nСкорость: %.0fкм/ч  Курс: %s", pos.GroundSpeed, formatBearing(float64(pos.Course)))
		text += spdLine
	}

	// Distance and bearing to landing.
	if landing != nil {
		distKm, bearing := distanceAndBearing(pos.Latitude, pos.Longitude, landing.Latitude, landing.Longitude)
		text += fmt.Sprintf("\n📍 %.1fкм до посадки (%s)", distKm, formatBearing(bearing))
	}

	// Distance from nearest driver to landed pilot.
	if info.Status == StatusLanded {
		if distKm, bearing, ok := nearestDriver(pos.Latitude, pos.Longitude, drivers); ok {
			text += fmt.Sprintf("\n🚗 %.1fкм от водителя (%s)", distKm, formatBearing(bearing))
		}
	}

	// Last update time.
	if !info.LastUpdate.IsZero() {
		text += "\n⏱ " + info.LastUpdate.In(t.tz()).Format("15:04:05")
	}

	return text
}

// pilotButtons returns inline buttons for pilots with known positions.
// Flying pilots get a navigate button; landed pilots get navigate + pickup.
func pilotButtons(local map[string]*TrackInfo) *models.InlineKeyboardMarkup {
	type entry struct {
		id   string
		info *TrackInfo
	}
	var flying, landed []entry
	for id, info := range local {
		if info.Position == nil {
			continue
		}
		switch info.Status {
		case StatusFlying:
			flying = append(flying, entry{id, info})
		case StatusLanded:
			landed = append(landed, entry{id, info})
		}
	}
	if len(flying) == 0 && len(landed) == 0 {
		return nil
	}
	sortByID := func(entries []entry) {
		sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	}
	sortByID(flying)
	sortByID(landed)

	var rows [][]models.InlineKeyboardButton
	for _, e := range flying {
		label := e.id
		if name := e.info.DisplayName(); name != "" {
			label = name
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "🗺 " + label, URL: mapsNavURL(e.info.Position.Latitude, e.info.Position.Longitude)},
		})
	}
	for _, e := range landed {
		label := e.id
		if name := e.info.DisplayName(); name != "" {
			label = name
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "🗺 " + label, URL: mapsNavURL(e.info.Position.Latitude, e.info.Position.Longitude)},
			{Text: "✅ Забрал " + label, CallbackData: "pickup:" + e.id},
		})
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// buildSummary composes the full tracking summary message with header counts
// and per-pilot sections grouped by status (flying, landed, picked up, waiting).
func (t *Tracker) buildSummary(local map[string]*TrackInfo, landing *Coordinates, drivers []*Coordinates, areaRadius int) string {
	type entry struct {
		id   string
		info *TrackInfo
	}

	var flying, landed, pickedUp, waiting []entry
	for id, info := range local {
		e := entry{id, info}
		if info.Position == nil {
			waiting = append(waiting, e)
		} else {
			switch info.Status {
			case StatusFlying:
				flying = append(flying, e)
			case StatusLanded:
				landed = append(landed, e)
			case StatusPickedUp:
				pickedUp = append(pickedUp, e)
			}
		}
	}

	sortByID := func(entries []entry) {
		sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	}
	sortByID(flying)
	// Sort landed pilots by distance from nearest driver (nearest first).
	if len(drivers) > 0 && len(landed) > 0 {
		sort.Slice(landed, func(i, j int) bool {
			pi := landed[i].info.Position
			pj := landed[j].info.Position
			if pi == nil && pj == nil {
				return landed[i].id < landed[j].id
			}
			if pi == nil {
				return false
			}
			if pj == nil {
				return true
			}
			di, _, _ := nearestDriver(pi.Latitude, pi.Longitude, drivers)
			dj, _, _ := nearestDriver(pj.Latitude, pj.Longitude, drivers)
			return di < dj
		})
	} else {
		sortByID(landed)
	}
	sortByID(pickedUp)
	sortByID(waiting)

	// Header with counts.
	total := len(local)
	header := fmt.Sprintf("📊 %d пилот(ов)", total)
	var counts []string
	if len(flying) > 0 {
		counts = append(counts, fmt.Sprintf("%d в воздухе", len(flying)))
	}
	if len(landed) > 0 {
		counts = append(counts, fmt.Sprintf("%d сели", len(landed)))
	}
	if len(pickedUp) > 0 {
		counts = append(counts, fmt.Sprintf("%d забрали", len(pickedUp)))
	}
	if len(counts) > 0 {
		header += " — " + strings.Join(counts, ", ")
	}
	if areaRadius > 0 {
		header += fmt.Sprintf("\n📡 Зона: радиус %dкм", areaRadius)
	}
	if len(drivers) > 0 {
		header += fmt.Sprintf("\n🚗 %d водитель(ей)", len(drivers))
	}

	// Build per-pilot sections.
	var sections []string
	for _, e := range flying {
		sections = append(sections, t.formatTrackText(e.id, e.info, landing, drivers))
	}
	for _, e := range landed {
		sections = append(sections, t.formatTrackText(e.id, e.info, landing, drivers))
	}
	for _, e := range pickedUp {
		label := "✅ " + e.id
		if name := e.info.DisplayName(); name != "" {
			label += " — " + name
		}
		sections = append(sections, label)
	}
	for _, e := range waiting {
		label := "⏳ " + e.id
		if name := e.info.DisplayName(); name != "" {
			label += " — " + name
		}
		sections = append(sections, label)
	}

	return header + "\n\n" + strings.Join(sections, "\n\n")
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
				heading = 360
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
				if _, err := b.EditMessageLiveLocation(ctx, editParams); err != nil && !strings.Contains(err.Error(), "message is not modified") {
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
		summary := t.buildSummary(local, landing, drivers, areaRadius)
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
			if _, err := b.EditMessageText(ctx, editParams); err != nil && !strings.Contains(err.Error(), "message is not modified") {
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
			select {
			case <-stopCh:
				log.Println("Radar client stopped")
				return
			default:
				log.Printf("Radar client error: %v", err)
				time.Sleep(reconnectDelay)
			}
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

		summary := t.buildRadarSummary(lines, center, radius, tz)
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
			if _, err := b.EditMessageText(ctx, ep); err != nil && !strings.Contains(err.Error(), "message is not modified") {
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

type radarLine struct {
	id    string
	entry RadarEntry
}

func (t *Tracker) buildRadarSummary(lines []radarLine, center *Coordinates, radius int, tz *time.Location) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📡 Радар: %d ВС в зоне %dкм\n", len(lines), radius)

	for _, l := range lines {
		pos := l.entry.Position
		if pos == nil {
			continue
		}
		sb.WriteString("\n")
		sb.WriteString(l.id)
		if name, ok := aircraftTypes[l.entry.AircraftType]; ok && l.entry.AircraftType > 0 {
			sb.WriteString(" [")
			sb.WriteString(name)
			sb.WriteString("]")
		}
		if l.entry.DDBInfo != "" {
			sb.WriteString(" — ")
			sb.WriteString(l.entry.DDBInfo)
		}
		dist, _ := distanceAndBearing(center.Latitude, center.Longitude, pos.Latitude, pos.Longitude)
		fmt.Fprintf(&sb, "\n  %.0fм ↕ | %.0fкм/ч | %.1fкм | %s",
			pos.Altitude, pos.GroundSpeed, dist,
			l.entry.LastSeen.In(tz).Format("15:04:05"))
		fmt.Fprintf(&sb, "\n  📍 %.4f, %.4f", pos.Latitude, pos.Longitude)

		// Truncate to fit Telegram message limit.
		if sb.Len() > 3900 {
			fmt.Fprintf(&sb, "\n\n…и ещё ВС")
			break
		}
	}

	if len(lines) == 0 {
		sb.WriteString("\nНет ВС в зоне")
	}
	return sb.String()
}

// radarButtons builds inline keyboard with map links for radar entries.
func radarButtons(lines []radarLine) *models.InlineKeyboardMarkup {
	var rows [][]models.InlineKeyboardButton
	for _, l := range lines {
		if l.entry.Position == nil {
			continue
		}
		label := l.id
		if name, ok := aircraftTypes[l.entry.AircraftType]; ok && l.entry.AircraftType > 0 {
			label += " " + name
		}
		url := mapsNavURL(l.entry.Position.Latitude, l.entry.Position.Longitude)
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "🗺 " + label, URL: url},
		})
		if len(rows) >= 20 {
			break
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

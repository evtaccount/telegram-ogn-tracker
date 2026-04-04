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
	"ogn/parser"
)

const (
	staleThreshold         = 5 * time.Minute
	landingSpeedThreshold  = 5.0  // km/h — below walking speed, filters out GPS noise
	landingClimbThreshold  = 0.3  // m/s — any thermal produces > 0.3 m/s climb
	landingConfirmDuration = 90 * time.Second
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

func (t *Tracker) runClient(stopCh <-chan struct{}) {
	log.Println("OGN client started")
	for {
		select {
		case <-stopCh:
			log.Println("OGN client stopped")
			return
		default:
		}

		err := t.aprs.Run(func(line string) {
			msg, err := parser.ParsePosition(line)
			if err != nil {
				return
			}
			origID := msg.Callsign
			id := shortID(origID)

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
			if ok && info.Status != StatusPickedUp && info.Status != StatusLanded {
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
				time.Sleep(5 * time.Second)
			}
		}
	}
}

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

func (t *Tracker) formatTrackText(id string, info *TrackInfo, landing *Coordinates, drivers []*Coordinates) string {
	pos := info.Position

	// Header: status emoji + ID + name/DDB info.
	text := info.Status.Emoji() + " " + id
	if info.Name != "" {
		text += " — " + info.Name
	} else if info.Username != "" {
		text += " — " + info.Username
	} else if info.AutoDiscovered && t.devices != nil {
		if dev, ok := t.devices[id]; ok {
			var parts []string
			if dev.AircraftModel != "" {
				parts = append(parts, dev.AircraftModel)
			}
			if dev.Registration != "" {
				parts = append(parts, dev.Registration)
			}
			if dev.CN != "" {
				parts = append(parts, "CN:"+dev.CN)
			}
			if len(parts) > 0 {
				text += " — " + strings.Join(parts, " | ")
			}
		}
		if name, ok := aircraftTypes[pos.AircraftType]; ok && pos.AircraftType > 0 {
			text += " [" + name + "]"
		}
	}
	if info.Status == StatusLanded && !info.LandingTime.IsZero() {
		text += fmt.Sprintf(" (сел %s)", info.LandingTime.In(t.tz()).Format("15:04"))
	}

	// Stale data warning.
	if !info.LastUpdate.IsZero() && time.Since(info.LastUpdate) > staleThreshold {
		mins := int(time.Since(info.LastUpdate).Minutes())
		text += fmt.Sprintf("\n⚠️ Нет данных %d мин", mins)
		text += "\n⏱ " + info.LastUpdate.In(t.tz()).Format("15:04:05")
		return text
	}

	// Flight data lines.
	altLine := fmt.Sprintf("\nВысота: %.0fм", pos.Altitude)
	if pos.ClimbRate != 0 {
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

func (t *Tracker) sendUpdates(stopCh <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
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
			if info.Position == nil || info.Status == StatusPickedUp || info.Status == StatusLanded || info.AutoDiscovered {
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
					LivePeriod: 86400,
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
			if _, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:      chatID,
				MessageID:   summaryMsgID,
				Text:        summary,
				ReplyMarkup: kb,
			}); err != nil && !strings.Contains(err.Error(), "message is not modified") {
				log.Printf("failed to edit summary: %v", err)
			}
		} else {
			msg, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:      chatID,
				Text:        summary,
				ReplyMarkup: kb,
			})
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

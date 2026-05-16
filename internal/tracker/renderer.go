package tracker

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"ogn/ddb"

	"github.com/go-telegram/bot/models"
)

// radarLine pairs an OGN id with a snapshot of the latest radar entry.
// It's the transport type between the radar collector goroutine and the
// renderer; defined here because the renderer is its primary consumer.
type radarLine struct {
	id    string
	entry RadarEntry
}

// nearestDriver returns the distance and bearing from the closest driver to
// the given point, or found=false if drivers is empty.
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

// formatTrackText builds a multi-line text block for one pilot in the summary
// message: status, altitude, speed, distance to landing, distance from the
// nearest driver. devices and tz are explicit dependencies so the function
// can be called without holding the Tracker mutex (and so it's straightforward
// to test).
func formatTrackText(id string, info *TrackInfo, landing *Coordinates, drivers []*Coordinates, devices map[string]ddb.Device, tz *time.Location) string {
	pos := info.Position

	// Header: status emoji + ID + name/DDB info.
	text := info.StatusEmoji() + " " + id
	if info.Name != "" {
		text += " — " + info.Name
	} else if info.Username != "" {
		text += " — " + info.Username
	} else if info.AutoDiscovered {
		if info := formatDDBInfo(devices, id); info != "" {
			text += " — " + info
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
		text += fmt.Sprintf(" (%s %s)", label, info.LandingTime.In(tz).Format("15:04"))
	}

	// Stale data warning.
	if info.Status == StatusFlying && !info.LastUpdate.IsZero() && time.Since(info.LastUpdate) > staleThreshold {
		mins := int(time.Since(info.LastUpdate).Minutes())
		text += fmt.Sprintf("\n⚠️ Нет данных %d мин", mins)
		text += "\n⏱ " + info.LastUpdate.In(tz).Format("15:04:05")
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
		text += "\n⏱ " + info.LastUpdate.In(tz).Format("15:04:05")
	}

	return text
}

// buildSummary composes the full tracking summary message with header counts
// and per-pilot sections grouped by status (flying, landed, picked up, waiting).
func buildSummary(local map[string]*TrackInfo, landing *Coordinates, drivers []*Coordinates, areaRadius int, devices map[string]ddb.Device, tz *time.Location) string {
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
		sections = append(sections, formatTrackText(e.id, e.info, landing, drivers, devices, tz))
	}
	for _, e := range landed {
		sections = append(sections, formatTrackText(e.id, e.info, landing, drivers, devices, tz))
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

// buildDashboard composes the persistent group-chat dashboard. The dashboard
// always exists between /start and /session_reset and replaces both the older
// "summary" message and the reply keyboard. It is one Telegram text body that
// folds together: a status header (always present), an inactivity warning
// (when applicable), and either the radar summary or the pilot summary.
func buildDashboard(s *GroupSession, devices map[string]ddb.Device, tz *time.Location) string {
	if s == nil {
		return "Нет активной сессии. /start чтобы начать."
	}

	var sb strings.Builder

	// Status header.
	tracking := "⏸ выкл."
	if s.TrackingOn {
		tracking = "✅ вкл."
	}
	if s.RadarOn {
		tracking = "📡 радар"
	}
	fmt.Fprintf(&sb, "📌 Сессия активна · Трекинг: %s", tracking)

	var meta []string
	if s.TrackArea != nil {
		meta = append(meta, fmt.Sprintf("📡 Зона: %dкм", s.TrackAreaRadius))
	}
	if len(s.Drivers) > 0 {
		meta = append(meta, fmt.Sprintf("🚗 %d водитель(ей)", len(s.Drivers)))
	}
	if len(meta) > 0 {
		sb.WriteString("\n")
		sb.WriteString(strings.Join(meta, " · "))
	}

	// Inactivity warning: surface here instead of as a separate chat message
	// so the dashboard is the single source of UI truth.
	if !s.InactivityWarnedAt.IsZero() {
		var lastBeacon time.Time
		for _, info := range s.Tracking {
			if info.LastUpdate.After(lastBeacon) {
				lastBeacon = info.LastUpdate
			}
		}
		if !lastBeacon.IsZero() {
			age := time.Since(lastBeacon).Round(time.Minute)
			fmt.Fprintf(&sb, "\n⚠️ Нет beacon-ов %s", age)
		}
	}

	// Body: radar mode shows the radar summary, otherwise the pilot summary.
	if s.RadarOn {
		// Reuse existing radar renderer when there's enough info; otherwise
		// fall back to a one-liner.
		if s.TrackArea != nil && len(s.RadarEntries) >= 0 {
			lines := make([]radarLine, 0, len(s.RadarEntries))
			for id, e := range s.RadarEntries {
				lines = append(lines, radarLine{id, *e})
			}
			sort.Slice(lines, func(i, j int) bool {
				if lines[i].entry.Position == nil {
					return false
				}
				if lines[j].entry.Position == nil {
					return true
				}
				return lines[i].entry.Position.Altitude > lines[j].entry.Position.Altitude
			})
			sb.WriteString("\n\n")
			sb.WriteString(buildRadarSummary(lines, s.TrackArea, s.RadarRadius, tz))
		}
		return sb.String()
	}

	if len(s.Tracking) == 0 {
		sb.WriteString("\n\nСписок пуст. /add <id> чтобы добавить пилота.")
		return sb.String()
	}

	// Reuse the existing pilot summary as the body. Drivers list passed empty —
	// driver coordinates are not part of the dashboard's body (they are summarised
	// in the status meta line).
	body := buildSummary(s.Tracking, s.Landing, nil, s.TrackAreaRadius, devices, tz)
	sb.WriteString("\n\n")
	sb.WriteString(body)
	return sb.String()
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

// buildRadarSummary composes the multi-line radar summary message.
func buildRadarSummary(lines []radarLine, center *Coordinates, radius int, tz *time.Location) string {
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

// radarButtons builds an inline keyboard with map links for radar entries.
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

package tracker

import (
	"fmt"
	"math"
	"strings"

	"ogn/ddb"
	"ogn/parser"

	"github.com/go-telegram/bot/models"
)

// ognPrefixes are the standard OGN APRS callsign prefixes.
// Short 6-char IDs are expanded to all five variants for the budlist filter.
var ognPrefixes = []string{"FLR", "OGN", "ICA", "NAV", "FNT"}

// removeReplyKB is a ReplyKeyboardRemove value that tells Telegram clients to
// hide whatever reply keyboard is currently visible. Used in /start so users
// with the legacy bottom bar lose it the moment they restart the session,
// without waiting for them to reopen the chat.
var removeReplyKB = &models.ReplyKeyboardRemove{RemoveKeyboard: true}

// aircraftTypes maps OGN aircraft type codes to human-readable names.
var aircraftTypes = map[int]string{
	0: "Unknown", 1: "Glider", 2: "Tow plane", 3: "Helicopter",
	4: "Parachute", 5: "Drop plane", 6: "Hang glider", 7: "Paraglider",
	8: "Powered aircraft", 9: "Jet", 10: "UFO", 11: "Balloon",
	12: "Airship", 13: "Drone", 15: "Static object",
}

// shortID normalizes an OGN address to its last 6 hex characters.
// OGN APRS uses full callsigns like "FLR123ABC", but for matching
// we only need the 6-char device address suffix.
func shortID(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	if len(id) <= 6 {
		return id
	}
	return id[len(id)-6:]
}

// isValidShortID reports whether s is exactly 6 uppercase hex characters,
// which is the canonical form of an OGN device address. Used at user-input
// boundaries (commands, DM messages) so non-hex garbage never reaches the
// APRS budlist filter.
func isValidShortID(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// isMessageNotModified detects the harmless Telegram error returned when an
// edit would not change the message contents. The library does not expose a
// typed error, so we match on the substring; centralised here so a future
// SDK change only needs one fix.
func isMessageNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

// isMessageGone matches the family of Telegram responses that mean the target
// message is permanently no longer editable: message deleted, live-location
// expired, edit window closed. Retrying these is pure log spam — the caller
// should mark the message as dead and stop touching it.
func isMessageGone(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "message can't be edited") ||
		strings.Contains(s, "message to edit not found") ||
		strings.Contains(s, "MESSAGE_ID_INVALID")
}

// distanceAndBearing computes distance (km) and bearing between two points
// using CheapRuler for fast approximate calculations at paragliding distances.
func distanceAndBearing(lat1, lon1, lat2, lon2 float64) (distKm float64, bearing float64) {
	ruler := parser.NewCheapRuler((lat1 + lat2) / 2)
	a := [2]float64{lon1, lat1}
	b := [2]float64{lon2, lat2}
	return ruler.Distance(a, b) / 1000, ruler.Bearing(a, b)
}

// bearingName converts a bearing in degrees to a cardinal direction (N, NE, E, ...).
func bearingName(deg float64) string {
	deg = math.Mod(deg+360, 360)
	names := []string{"N", "NE", "E", "SE", "S", "SW", "W", "NW"}
	idx := int(math.Round(deg/45)) % 8
	return names[idx]
}

func formatBearing(deg float64) string {
	deg = math.Mod(deg+360, 360)
	return fmt.Sprintf("(%.0f° | %s)", deg, bearingName(deg))
}

// formatDDBInfo returns a human-readable summary of the OGN DDB entry for a device,
// e.g. "ASG 29 | D-1234 | CN:AB". Returns "" if unknown.
func formatDDBInfo(devices map[string]ddb.Device, id string) string {
	if devices == nil {
		return ""
	}
	dev, ok := devices[id]
	if !ok {
		return ""
	}
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
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " | ")
}

func isGroupChat(chat models.Chat) bool {
	return chat.Type == "group" || chat.Type == "supergroup"
}

func isPrivateChat(chat models.Chat) bool {
	return chat.Type == "private"
}

func mapsNavURL(lat, lon float64) string {
	return fmt.Sprintf("https://www.google.com/maps/dir/?api=1&destination=%.6f,%.6f", lat, lon)
}

// commandArgs extracts the argument string after the first space in a command.
func commandArgs(text string) string {
	if i := strings.Index(text, " "); i != -1 {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

// pilotLabelText builds the short caption shown above each pilot's
// live-location pin. The status emoji is the same one used by the summary
// (✈️ flying, 🪂 landed-unconfirmed, ✅ landed-confirmed / picked up). When a
// human-friendly name is known we render "{emoji} {Name} ({id})"; otherwise
// just "{emoji} {id}". The OGN ID is always present so the retriever can
// cross-reference with /list output.
func pilotLabelText(id string, info *TrackInfo) string {
	emoji := info.StatusEmoji()
	name := info.DisplayName()
	if name == "" {
		return emoji + " " + id
	}
	return fmt.Sprintf("%s %s (%s)", emoji, name, id)
}

// chooseHeading picks a heading value to send to Telegram for the live-location
// arrow given the latest course, cached last-known heading and current ground speed.
//
// Telegram only renders the arrow for non-zero headings, but OGN reports
// Course=0 frequently for moving aircraft. The rules are:
//   - course > 0   → use it (and update LastHeading at the call site).
//   - speed <= 0   → return 0; the pilot is stationary, no arrow.
//   - lastHeading > 0 → reuse the cached heading so the arrow doesn't snap to north.
//   - otherwise    → 360 as a one-time fallback to force an arrow at all.
func chooseHeading(course int, lastHeading int, groundSpeed float64) int {
	if course > 0 {
		return course
	}
	if groundSpeed <= 0 {
		return 0
	}
	if lastHeading > 0 {
		return lastHeading
	}
	return 360
}

package tracker

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/go-telegram/bot"
	"ogn/parser"
)

const staleThreshold = 5 * time.Minute

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
			t.mu.Lock()
			if info, ok := t.tracking[id]; ok {
				info.Position = msg
				info.LastUpdate = time.Now()
				log.Printf("beacon %s: %.5f,%.5f %.0fm %.0fkm/h", id, msg.Latitude, msg.Longitude, msg.Altitude, msg.GroundSpeed)
			}
			t.mu.Unlock()
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

func (t *Tracker) formatTrackText(id string, info *TrackInfo, landing *Coordinates) string {
	pos := info.Position

	// Header: ID and name.
	text := id
	if info.Name != "" {
		text += " — " + info.Name
	} else if info.Username != "" {
		text += " — " + info.Username
	}

	// Stale data warning.
	if !info.LastUpdate.IsZero() && time.Since(info.LastUpdate) > staleThreshold {
		mins := int(time.Since(info.LastUpdate).Minutes())
		text += fmt.Sprintf("\n⚠️ No data for %d min", mins)
		text += "\n⏱ " + info.LastUpdate.Format("15:04:05")
		return text
	}

	// Flight data line.
	text += fmt.Sprintf("\n⬆️ %.0fm", pos.Altitude)
	if pos.ClimbRate != 0 {
		text += fmt.Sprintf("  %+.1fm/s", pos.ClimbRate)
	}
	if pos.GroundSpeed > 0 {
		text += fmt.Sprintf("  %.0fkm/h", pos.GroundSpeed)
	}
	if pos.Course > 0 || pos.GroundSpeed > 0 {
		text += "  " + formatBearing(float64(pos.Course))
	}

	// Distance and bearing to landing.
	if landing != nil {
		distKm, bearing := distanceAndBearing(pos.Latitude, pos.Longitude, landing.Latitude, landing.Longitude)
		text += fmt.Sprintf("\n📍 %.1fkm to landing (%s)", distKm, formatBearing(bearing))
	}

	// Last update time.
	if !info.LastUpdate.IsZero() {
		text += "\n⏱ " + info.LastUpdate.Format("15:04:05")
	}

	return text
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
		chatID := t.chatID
		landing := t.landing
		b := t.bot
		local := make(map[string]*TrackInfo)
		for id, info := range t.tracking {
			cp := *info
			local[id] = &cp
		}
		t.mu.Unlock()

		if b == nil {
			continue
		}

		ctx := context.Background()

		for id, info := range local {
			if info.Position == nil {
				continue
			}

			text := t.formatTrackText(id, info, landing)

			// Heading for Telegram live location (1-360).
			heading := info.Position.Course
			if heading == 0 && info.Position.GroundSpeed > 0 {
				heading = 360
			}

			msgID := info.MessageID
			textID := info.TextID

			if msgID != 0 {
				editParams := &bot.EditMessageLiveLocationParams{
					ChatID:    chatID,
					MessageID: msgID,
					Latitude:  info.Position.Latitude,
					Longitude: info.Position.Longitude,
				}
				if heading > 0 {
					editParams.Heading = heading
				}
				if _, err := b.EditMessageLiveLocation(ctx, editParams); err != nil {
					log.Printf("failed to edit location for %s: %v", id, err)
				}
				if textID != 0 {
					if _, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
						ChatID:    chatID,
						MessageID: textID,
						Text:      text,
					}); err != nil {
						log.Printf("failed to edit text for %s: %v", id, err)
					}
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
				textMsg, err := b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: chatID,
					Text:   text,
				})
				if err != nil {
					log.Printf("failed to send text for %s: %v", id, err)
				}
				t.mu.Lock()
				if ti, ok := t.tracking[id]; ok {
					ti.MessageID = locMsg.ID
					if textMsg != nil {
						ti.TextID = textMsg.ID
					}
				}
				t.mu.Unlock()
				log.Printf("sent location for %s", id)
			}
		}
	}
}

package tracker

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/go-telegram/bot"
	"ogn/parser"
)

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
			log.Printf("raw OGN line: %s", line)
			msg, err := parser.ParsePosition(line)
			if err != nil {
				log.Printf("failed to parse line: %v", err)
				return
			}
			origID := msg.Callsign
			id := shortID(origID)
			t.mu.Lock()
			if info, ok := t.tracking[id]; ok {
				info.Position = msg
				info.LastUpdate = time.Now()
				t.tracking[id] = info
				log.Printf("received beacon for %s (orig %s): lat %.5f lon %.5f", id, origID, msg.Latitude, msg.Longitude)
			} else {
				log.Printf("ignoring untracked id %s", origID)
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

			text := "Address: " + id
			if info.Name != "" || info.Username != "" {
				text += "\n"
				if info.Name != "" {
					text += info.Name
					if info.Username != "" {
						text += " (" + info.Username + ")"
					}
				} else {
					text += info.Username
				}
			}
			if !info.LastUpdate.IsZero() {
				text += "\nLast update: " + info.LastUpdate.Format("2006-01-02 15:04:05")
			}
			if landing != nil {
				dist := distanceKm(info.Position.Latitude, info.Position.Longitude, landing.Latitude, landing.Longitude)
				text += fmt.Sprintf("\nDistance to landing: %.1f km", dist)
			}

			msgID := info.MessageID
			textID := info.TextID

			if msgID != 0 {
				if _, err := b.EditMessageLiveLocation(ctx, &bot.EditMessageLiveLocationParams{
					ChatID:    chatID,
					MessageID: msgID,
					Latitude:  info.Position.Latitude,
					Longitude: info.Position.Longitude,
				}); err != nil {
					log.Printf("failed to edit location for %s: %v", id, err)
				} else {
					log.Printf("updated location for %s", id)
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
				locMsg, err := b.SendLocation(ctx, &bot.SendLocationParams{
					ChatID:     chatID,
					Latitude:   info.Position.Latitude,
					Longitude:  info.Position.Longitude,
					LivePeriod: 86400,
				})
				if err != nil {
					log.Printf("failed to send location for %s: %v", id, err)
					continue
				}
				textMsg, err := b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: chatID,
					Text:   text,
				})
				if err != nil {
					log.Printf("failed to send address message for %s: %v", id, err)
				}
				t.mu.Lock()
				if info, ok := t.tracking[id]; ok {
					info.MessageID = locMsg.ID
					if textMsg != nil {
						info.TextID = textMsg.ID
					}
					t.tracking[id] = info
				}
				t.mu.Unlock()
				log.Printf("sent location for %s", id)
			}
		}
	}
}

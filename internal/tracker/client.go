package tracker

import (
	"fmt"
	"log"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"ogn/parser"
)

func (t *Tracker) runClient() {
	log.Println("OGN client started")
	for {
		t.mu.Lock()
		if !t.trackingOn {
			t.mu.Unlock()
			break
		}
		t.mu.Unlock()

		err := t.aprs.Run(func(line string) {
			log.Printf("raw OGN line: %s", line)
			msg, err := parser.Parse(line)
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
			t.mu.Lock()
			active := t.trackingOn
			t.mu.Unlock()
			if active {
				log.Printf("OGN client error: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}
		}
	}
	log.Println("OGN client stopped")
}

func (t *Tracker) sendUpdates() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		t.mu.Lock()
		if !t.trackingOn {
			t.mu.Unlock()
			return
		}
		chatID := t.chatID
		landing := t.landing
		local := make(map[string]*TrackInfo)
		for id, info := range t.tracking {
			cp := *info
			local[id] = &cp
		}
		t.mu.Unlock()

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
				edit := tgbotapi.EditMessageLiveLocationConfig{
					BaseEdit: tgbotapi.BaseEdit{
						ChatID:    chatID,
						MessageID: msgID,
					},
					Latitude:  info.Position.Latitude,
					Longitude: info.Position.Longitude,
				}
				if _, err := t.bot.Request(edit); err != nil {
					log.Printf("failed to edit location for %s: %v", id, err)
				} else {
					log.Printf("updated location for %s", id)
				}
				if textID != 0 {
					editText := tgbotapi.NewEditMessageText(chatID, textID, text)
					if _, err := t.bot.Send(editText); err != nil {
						log.Printf("failed to edit text for %s: %v", id, err)
					}
				}
			} else {
				loc := tgbotapi.NewLocation(chatID, info.Position.Latitude, info.Position.Longitude)
				loc.LivePeriod = 86400
				msg, err := t.bot.Send(loc)
				if err != nil {
					log.Printf("failed to send location for %s: %v", id, err)
					continue
				}
				textMsg, err := t.bot.Send(tgbotapi.NewMessage(chatID, text))
				if err != nil {
					log.Printf("failed to send address message for %s: %v", id, err)
				}
				t.mu.Lock()
				if info, ok := t.tracking[id]; ok {
					info.MessageID = msg.MessageID
					info.TextID = textMsg.MessageID
					t.tracking[id] = info
				}
				t.mu.Unlock()
				log.Printf("sent location for %s", id)
			}
		}
	}
}

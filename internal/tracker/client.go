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
						text += " (@" + info.Username + ")"
					}
				} else {
					text += "@" + info.Username
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

			// Remove previous messages to avoid duplicates.
			if msgID != 0 {
				del := tgbotapi.NewDeleteMessage(chatID, msgID)
				if _, err := t.bot.Request(del); err != nil {
					log.Printf("failed to delete old location for %s: %v", id, err)
				}
				if textID != 0 {
					delTxt := tgbotapi.NewDeleteMessage(chatID, textID)
					if _, err := t.bot.Request(delTxt); err != nil {
						log.Printf("failed to delete old text for %s: %v", id, err)
					}
				}
			}

			title := id
			if info.Name != "" {
				title = info.Name
				if info.Username != "" {
					title += " (@" + info.Username + ")"
				}
			} else if info.Username != "" {
				title = "@" + info.Username
			}

			addr := id
			if info.Username != "" {
				addr = "@" + info.Username
			}

			venue := tgbotapi.NewVenue(chatID, title, addr, info.Position.Latitude, info.Position.Longitude)
			msg, err := t.bot.Send(venue)
			if err != nil {
				log.Printf("failed to send venue for %s: %v", id, err)
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
			log.Printf("sent venue for %s", id)
		}
	}
}

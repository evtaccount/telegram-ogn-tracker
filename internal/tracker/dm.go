package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// cmdStartPrivate handles /start in DMs.
// Deep link payload "add_<groupChatID>" initiates the OGN ID input flow.
func (t *Tracker) cmdStartPrivate(ctx context.Context, b *bot.Bot, m *models.Message) {
	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	// Check for deep link payload: /start add_<groupChatID>
	payload := commandArgs(m.Text)
	if strings.HasPrefix(payload, "add_") {
		groupIDStr := payload[4:]
		groupChatID, err := strconv.ParseInt(groupIDStr, 10, 64)
		if err != nil {
			t.saveState()
			t.mu.Unlock()
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   "Неверная ссылка.",
			}); err != nil {
				slog.Error("failed to send invalid deep link", "err", err)
			}
			return
		}

		u.PendingGroup = groupChatID
		hasOGNID := u.OGNID != ""
		ognID := u.OGNID
		t.saveState()
		t.mu.Unlock()

		if hasOGNID {
			text := fmt.Sprintf("Ваш OGN ID: %s\nОтправьте новый ID или /confirm чтобы использовать текущий.", ognID)
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   text,
			}); err != nil {
				slog.Error("failed to send DM with existing ID", "err", err)
			}
		} else {
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   "Отправьте ваш OGN ID (6-значный адрес трекера):",
			}); err != nil {
				slog.Error("failed to send DM ask for OGN ID", "err", err)
			}
		}
		return
	}

	// No deep link: just register DM.
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Бот готов. Используйте /myid чтобы задать свой OGN ID.",
	}); err != nil {
		slog.Error("failed to send private start message", "err", err)
	}
}

// cmdMyID lets the user view or set their OGN ID in DM.
// If a tracked entry already exists for the previous ID owned by this user,
// it is migrated to the new ID under the same TrackInfo.
func (t *Tracker) cmdMyID(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !isPrivateChat(m.Chat) {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Эта команда работает только в личке.",
		}); err != nil {
			slog.Error("failed to send private-only message", "err", err)
		}
		return
	}

	arg := commandArgs(m.Text)

	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	if arg == "" {
		// Show current OGN ID.
		ognID := u.OGNID
		t.mu.Unlock()
		if ognID == "" {
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   "OGN ID не задан. Используйте /myid <id>",
			}); err != nil {
				slog.Error("failed to send myid empty", "err", err)
			}
		} else {
			if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: m.Chat.ID,
				Text:   fmt.Sprintf("Ваш OGN ID: %s", ognID),
			}); err != nil {
				slog.Error("failed to send myid value", "err", err)
			}
		}
		return
	}

	// Set new OGN ID.
	newID := shortID(arg)
	if !isValidShortID(newID) {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   fmt.Sprintf("OGN ID %q некорректен. Нужно 6 hex-символов (0-9, A-F).", arg),
		}); err != nil {
			slog.Error("failed to send invalid ognid message", "err", err)
		}
		return
	}
	oldID := u.OGNID
	u.OGNID = newID

	// Update any TrackInfo entries owned by this user.
	s := t.session
	if s != nil && oldID != "" && oldID != newID {
		if info, ok := s.Tracking[oldID]; ok && info.OwnerUserID == u.UserID {
			delete(s.Tracking, oldID)
			info.Name = u.DisplayName
			info.Username = u.Username
			s.Tracking[newID] = info
			t.updateFilter()
		}
	}
	t.saveState()
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   fmt.Sprintf("OGN ID обновлён: %s", newID),
	}); err != nil {
		slog.Error("failed to confirm myid update", "err", err)
	}
}

// cmdConfirm completes a pending /add flow in DM by adding the user's
// previously-set OGN ID into their PendingGroup session.
func (t *Tracker) cmdConfirm(ctx context.Context, b *bot.Bot, update *models.Update) {
	m := update.Message
	if m.From == nil || !t.isTrusted(m.From.ID) {
		return
	}
	if !isPrivateChat(m.Chat) {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Эта команда работает только в личке.",
		}); err != nil {
			slog.Error("failed to send private-only message", "err", err)
		}
		return
	}

	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	if u.PendingGroup == 0 {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Нет ожидающей группы. Используйте /add в группе.",
		}); err != nil {
			slog.Error("failed to send no pending group", "err", err)
		}
		return
	}
	if u.OGNID == "" {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "OGN ID не задан. Отправьте ID сообщением или используйте /myid <id>.",
		}); err != nil {
			slog.Error("failed to send no ogn id", "err", err)
		}
		return
	}

	s := t.session
	if s == nil || s.ChatID != u.PendingGroup {
		u.PendingGroup = 0
		t.saveState()
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Группа не найдена. Попросите добавить вас заново.",
		}); err != nil {
			slog.Error("failed to send pending group not found", "err", err)
		}
		return
	}

	id := u.OGNID
	name := u.DisplayName
	groupChatID := s.ChatID

	if info, ok := s.Tracking[id]; ok {
		info.Name = name
		info.Username = u.Username
		info.OwnerUserID = u.UserID
		info.AutoDiscovered = false
	} else {
		s.Tracking[id] = &TrackInfo{
			Name:        name,
			Username:    u.Username,
			OwnerUserID: u.UserID,
		}
	}

	u.PendingGroup = 0
	t.updateFilter()
	dmKb := t.dmReplyKeyboard(u.UserID)
	t.saveState()
	t.mu.Unlock()

	// Confirm in DM.
	dmParams := &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   fmt.Sprintf("Добавлен %s в группу", id),
	}
	if dmKb != nil {
		dmParams.ReplyMarkup = dmKb
	}
	if _, err := b.SendMessage(ctx, dmParams); err != nil {
		slog.Error("failed to confirm in DM", "err", err)
	}

	// Confirm in group.
	label := id
	if name != "" {
		label = id + " (" + name + ")"
	}
	groupAckID := t.sendAck(ctx, &bot.SendMessageParams{
		ChatID: groupChatID,
		Text:   "Добавлен " + label,
	}, "failed to confirm in group")
	// Same finalize as handleDMText: drain queued (cmd + "Написал в личку")
	// for this user and clean together with the group ack.
	t.finalizePendingCleanup(m.From.ID, groupChatID, groupAckID)
	t.refreshDashboard(ctx, groupChatID)
}

// execDMLanding initiates the landing flow from a private chat.
// Asks the pilot to send their location pin.
func (t *Tracker) execDMLanding(ctx context.Context, b *bot.Bot, m *models.Message) {
	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	s := t.session
	if s == nil || !s.TrackingOn || u.OGNID == "" {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Трекинг не активен или OGN ID не задан.",
		}); err != nil {
			slog.Error("failed to send DM landing unavailable", "err", err)
		}
		return
	}

	info, ok := s.Tracking[u.OGNID]
	if !ok || info.Status != StatusFlying {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Вы не в списке летающих пилотов.",
		}); err != nil {
			slog.Error("failed to send DM not flying", "err", err)
		}
		return
	}

	s.WaitingDMLandingFor = m.From.ID
	s.DMLandingExpiry = time.Now().Add(waitTimeout)
	t.mu.Unlock()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "Отправьте точку посадки в течение 2 минут",
	}); err != nil {
		slog.Error("failed to request DM landing location", "err", err)
	}
}

// execDMConfirmLanding handles the "🪂 Сел" button in DM.
// Marks the pilot as landed with confirmation (no location pin required).
func (t *Tracker) execDMConfirmLanding(ctx context.Context, b *bot.Bot, m *models.Message) {
	t.mu.Lock()
	u := t.ensureUser(m.From)
	u.DMChatID = m.Chat.ID

	s := t.session
	if s == nil || !s.TrackingOn || u.OGNID == "" {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Трекинг не активен или OGN ID не задан.",
		}); err != nil {
			slog.Error("failed to send DM confirm landing unavailable", "err", err)
		}
		return
	}

	info, ok := s.Tracking[u.OGNID]
	if !ok || info.Status != StatusFlying {
		t.mu.Unlock()
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: m.Chat.ID,
			Text:   "Вы не в списке летающих пилотов.",
		}); err != nil {
			slog.Error("failed to send DM not flying", "err", err)
		}
		return
	}

	info.Status = StatusLanded
	info.LandingTime = time.Now()
	info.LandingConfirmed = true

	var alert *landingEvent
	if info.Position != nil {
		alert = &landingEvent{
			id:   u.OGNID,
			name: info.DisplayName(),
			lat:  info.Position.Latitude,
			lon:  info.Position.Longitude,
			alt:  info.Position.Altitude,
			time: info.LandingTime,
			tz:   s.tz(),
		}
	}
	chatID := s.ChatID
	dmKb := t.dmReplyKeyboard(u.UserID)
	t.saveState()
	t.mu.Unlock()

	// Confirm in DM.
	params := &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   "🪂 Посадка подтверждена",
	}
	if dmKb != nil {
		params.ReplyMarkup = dmKb
	} else {
		params.ReplyMarkup = &models.ReplyKeyboardRemove{RemoveKeyboard: true}
	}
	if _, err := b.SendMessage(ctx, params); err != nil {
		slog.Error("failed to confirm DM landing", "err", err)
	}

	// Notify the group.
	if alert != nil {
		t.sendLandingAlert(alert, chatID)
	} else {
		// No position data — send a simple text notification.
		label := u.OGNID
		if name := info.DisplayName(); name != "" {
			label = name
		}
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   fmt.Sprintf("🪂 %s сел! (подтверждено пилотом)", label),
		}); err != nil {
			slog.Error("failed to send landing notification", "ogn_id", u.OGNID, "err", err)
		}
	}
}

// handleDMLanding processes a location sent in DM during the landing flow.
// Sets the landing point in the group session and marks the sender as landed.
func (t *Tracker) handleDMLanding(ctx context.Context, b *bot.Bot, m *models.Message) {
	loc := m.Location

	t.mu.Lock()
	s := t.session
	if s == nil {
		t.mu.Unlock()
		return
	}

	if s.WaitingDMLandingFor != m.From.ID || !time.Now().Before(s.DMLandingExpiry) {
		t.mu.Unlock()
		return
	}

	slog.Info("dm landing location set", "lat", loc.Latitude, "lon", loc.Longitude, "user_id", m.From.ID)
	s.Landing = &Coordinates{Latitude: loc.Latitude, Longitude: loc.Longitude}
	s.WaitingDMLandingFor = 0

	// Mark the sender as landed.
	var landedName string
	if u, ok := t.users[m.From.ID]; ok && u.OGNID != "" {
		if info, ok := s.Tracking[u.OGNID]; ok && info.Status == StatusFlying {
			info.Status = StatusLanded
			info.LandingTime = time.Now()
			info.LandingConfirmed = true
			landedName = info.DisplayName()
			slog.Info("dm landing marked", "ogn_id", u.OGNID, "user_id", m.From.ID)
		}
	}

	groupChatID := s.ChatID
	dmKb := t.dmReplyKeyboard(m.From.ID)
	t.saveState()
	t.mu.Unlock()

	// Confirm in DM.
	dmText := "Точка посадки сохранена"
	if landedName != "" {
		dmText += fmt.Sprintf("\n🪂 %s отмечен как севший", landedName)
	}
	params := &bot.SendMessageParams{
		ChatID: m.Chat.ID,
		Text:   dmText,
	}
	if dmKb != nil {
		params.ReplyMarkup = dmKb
	} else {
		params.ReplyMarkup = &models.ReplyKeyboardRemove{RemoveKeyboard: true}
	}
	if _, err := b.SendMessage(ctx, params); err != nil {
		slog.Error("failed to confirm DM landing", "err", err)
	}

	// Notify the group.
	groupText := "📍 Точка посадки обновлена"
	if landedName != "" {
		groupText += fmt.Sprintf("\n🪂 %s сел", landedName)
	}
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: groupChatID,
		Text:   groupText,
	}); err != nil {
		slog.Error("failed to notify group about DM landing", "err", err)
	}

	t.refreshDashboard(ctx, groupChatID)
}

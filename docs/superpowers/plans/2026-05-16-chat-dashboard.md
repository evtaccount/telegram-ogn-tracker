# Persistent Chat Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the existing transient summary + reply-keyboard pair with one pinned Dashboard message per group session — always visible, always reflecting current state, always carrying the available actions as inline buttons.

**Architecture:** Rename `SummaryMsgID` → `DashboardMsgID`, build dashboard text and inline keyboard via two new pure functions, drive lifecycle from a single `refreshDashboard` helper that callers invoke after any state change. `sendUpdates` becomes one of those callers (heartbeat refresh on the 30 s ticker). The reply keyboard is removed everywhere. Session-reset deletes the dashboard message; `/start` creates a fresh one.

**Tech Stack:** Go 1.24, `log/slog`, `github.com/go-telegram/bot v1.20.0`. Existing test conventions: pure-function unit tests in `internal/tracker/tracker_test.go`; persistence tests use a temp dir + chdir helper.

**Spec:** `docs/superpowers/specs/2026-05-16-chat-dashboard-design.md`

**Branch:** `feat/persistent-dashboard` (already created)

---

## File map

| File | Role in this plan |
|---|---|
| `internal/tracker/types.go` | Rename `SummaryMsgID`/`SummaryPinned` → `DashboardMsgID`/`DashboardPinned`. Delete `replyKeyboard()`. |
| `internal/tracker/persist.go` | JSON tags renamed; legacy `summary_msg_id`/`summary_pinned` fields retained for read-side migration. |
| `internal/tracker/renderer.go` | New `buildDashboard()` (text) and `dashboardButtons()` (inline keyboard). |
| `internal/tracker/client.go` | New `refreshDashboard()` helper. `sendUpdates` summary block replaced by a `refreshDashboard()` call. Inactivity warning text is now produced inside the dashboard, not sent as a separate message. |
| `internal/tracker/callbacks.go` | New `dashboard:*` callback handlers. |
| `internal/tracker/tracker.go` | Register the new callback handlers. |
| `internal/tracker/commands.go`, `internal/tracker/flows.go` | Drop `ReplyMarkup: kb` assignments where `kb` came from `replyKeyboard()`. Call `refreshDashboard()` after every state-changing command. `cmdStart`/`cmdStartSession` also send a `ReplyKeyboardRemove` once to clear the old bottom bar on existing clients. |
| `internal/tracker/cleanup.go` | (no change in this plan) |
| `internal/tracker/tracker_test.go` | Tests for `buildDashboard`, `dashboardButtons`, persistence migration, and dashboard lifecycle helpers. |

---

## Task 1: Rename `SummaryMsgID` and add persistence migration

**Files:**
- Modify: `internal/tracker/types.go`
- Modify: `internal/tracker/persist.go`
- Modify: `internal/tracker/client.go`
- Modify: `internal/tracker/tracker.go` (uses `SummaryPinned` in `Shutdown`/elsewhere — search-and-update)
- Test: `internal/tracker/tracker_test.go`

- [ ] **Step 1: Find all references to `SummaryMsgID` and `SummaryPinned`**

```bash
grep -rn "SummaryMsgID\|SummaryPinned\|summary_msg_id\|summary_pinned" internal/tracker/
```

Note every hit — they all need updating in this task except the legacy JSON tag fallback.

- [ ] **Step 2: Write the failing migration test**

Append to `internal/tracker/tracker_test.go`:

```go
func TestLoadStateMigratesSummaryToDashboard(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	// Write a session.json that uses the old "summary_msg_id" / "summary_pinned"
	// field names — what live deployments currently have on disk.
	if err := os.MkdirAll("data", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const legacy = `{
	  "session": {
	    "chat_id": -100,
	    "tracking_on": false,
	    "summary_msg_id": 42,
	    "summary_pinned": true
	  }
	}`
	if err := os.WriteFile("data/session.json", []byte(legacy), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tr := &Tracker{users: make(map[int64]*UserInfo)}
	tr.mu.Lock()
	tr.loadState()
	tr.mu.Unlock()
	if tr.session == nil {
		t.Fatal("expected session restored")
	}
	if tr.session.DashboardMsgID != 42 {
		t.Errorf("DashboardMsgID = %d, want 42", tr.session.DashboardMsgID)
	}
	if !tr.session.DashboardPinned {
		t.Errorf("DashboardPinned = false, want true (migrated from summary_pinned)")
	}
}
```

- [ ] **Step 3: Run the test, confirm it fails to compile**

Run: `go test ./internal/tracker/ -run TestLoadStateMigratesSummaryToDashboard 2>&1 | tail -10`
Expected: build error because `DashboardMsgID` / `DashboardPinned` don't exist yet.

- [ ] **Step 4: Rename fields in `types.go`**

In `internal/tracker/types.go`, in the `GroupSession` struct, replace:

```go
SummaryMsgID    int
// SummaryPinned is true once PinChatMessage succeeded for the current
// SummaryMsgID. Persisted so a restart doesn't re-pin (and re-notify) an
// already-pinned message.
SummaryPinned bool
```

with:

```go
DashboardMsgID    int
// DashboardPinned is true once PinChatMessage succeeded for the current
// DashboardMsgID. Persisted so a restart doesn't re-pin (and re-notify) an
// already-pinned message.
DashboardPinned bool
```

- [ ] **Step 5: Update `sessionState` JSON struct with migration tags**

In `internal/tracker/persist.go`, in `sessionState`, replace:

```go
SummaryMsgID    int                    `json:"summary_msg_id,omitempty"`
SummaryPinned   bool                   `json:"summary_pinned,omitempty"`
```

with:

```go
DashboardMsgID  int                    `json:"dashboard_msg_id,omitempty"`
DashboardPinned bool                   `json:"dashboard_pinned,omitempty"`
// Legacy field names used by deployments prior to the dashboard rename.
// Read-only on load (see loadState); never written.
LegacySummaryMsgID  int  `json:"summary_msg_id,omitempty"`
LegacySummaryPinned bool `json:"summary_pinned,omitempty"`
```

- [ ] **Step 6: Update `marshalStateLocked` to write the new field names**

In `internal/tracker/persist.go`, in `marshalStateLocked()`, replace:

```go
SummaryMsgID:    s.SummaryMsgID,
SummaryPinned:   s.SummaryPinned,
```

with:

```go
DashboardMsgID:  s.DashboardMsgID,
DashboardPinned: s.DashboardPinned,
```

- [ ] **Step 7: Update `loadState` to read either field name**

In `internal/tracker/persist.go`, in `loadState()`, in the `session := &GroupSession{...}` block, replace:

```go
SummaryMsgID:    ss.SummaryMsgID,
SummaryPinned:   ss.SummaryPinned,
```

with:

```go
DashboardMsgID:  ss.DashboardMsgID,
DashboardPinned: ss.DashboardPinned,
```

Then, immediately after the struct literal, add the migration fallback:

```go
// Migrate from the pre-rename field names: if the new dashboard fields are
// zero and the legacy ones are present, copy them across.
if session.DashboardMsgID == 0 && ss.LegacySummaryMsgID != 0 {
    session.DashboardMsgID = ss.LegacySummaryMsgID
    session.DashboardPinned = ss.LegacySummaryPinned
}
```

- [ ] **Step 8: Update all `SummaryMsgID`/`SummaryPinned` references in `client.go`**

Using the grep results from Step 1, replace every remaining `SummaryMsgID` with `DashboardMsgID` and `SummaryPinned` with `DashboardPinned` in `internal/tracker/client.go`. There are references in `sendUpdates` (summary edit, pin block) and in `stopTrackingAsync` (clears the IDs). Variables named `summaryMsgID` / `summaryPinned` (lowercase locals) in `sendUpdates` should also be renamed to `dashboardMsgID` / `dashboardPinned` for consistency.

After this step the substring `Summary` should remain only in `persist.go` (for legacy tags), in log message strings (`"summary gone, will repost"` etc — leave those, they are user-facing log keys that don't need to change).

- [ ] **Step 9: Verify build and tests**

Run: `go build ./... && go test ./internal/tracker/ -run TestLoadStateMigratesSummaryToDashboard -v`
Expected: build succeeds, the new migration test passes.

- [ ] **Step 10: Run full test suite to confirm no regressions**

Run: `go test ./... 2>&1 | tail -5`
Expected: `ok  telegram-ogn-tracker/internal/tracker`. The existing `TestSummaryPinPersistence` test should keep passing — update it inline if it references the old field names.

- [ ] **Step 11: Commit**

```bash
git add internal/tracker/
git commit -m "Rename SummaryMsgID to DashboardMsgID with persistence migration"
```

---

## Task 2: `buildDashboard()` — dashboard text

**Files:**
- Modify: `internal/tracker/renderer.go`
- Test: `internal/tracker/tracker_test.go`

The text rendering takes the same inputs as `buildSummary` plus a `dashboardState` value, so test it as a pure function.

- [ ] **Step 1: Write the failing test (states matrix)**

Append to `internal/tracker/tracker_test.go`:

```go
func TestBuildDashboard(t *testing.T) {
	tz := time.UTC
	devices := map[string]ddb.Device{}

	t.Run("session active, no pilots, tracking off", func(t *testing.T) {
		s := &GroupSession{Tracking: map[string]*TrackInfo{}}
		got := buildDashboard(s, devices, tz)
		if !strings.Contains(got, "Сессия активна") {
			t.Errorf("missing status header in: %q", got)
		}
		if !strings.Contains(got, "Список пуст") {
			t.Errorf("missing empty-list hint in: %q", got)
		}
	})

	t.Run("tracking on shows pilots", func(t *testing.T) {
		s := &GroupSession{
			TrackingOn: true,
			Tracking: map[string]*TrackInfo{
				"AABBCC": {Name: "Eugene", Status: StatusFlying,
					Position:   &parser.PositionMessage{Latitude: 41.7, Longitude: 44.7, GroundSpeed: 30, Altitude: 1500},
					LastUpdate: time.Now()},
			},
		}
		got := buildDashboard(s, devices, tz)
		if !strings.Contains(got, "Трекинг: ✅") {
			t.Errorf("expected tracking-on indicator, got: %q", got)
		}
		if !strings.Contains(got, "AABBCC") {
			t.Errorf("expected pilot id in body, got: %q", got)
		}
	})

	t.Run("inactivity warning surfaces in header", func(t *testing.T) {
		s := &GroupSession{
			TrackingOn:         true,
			InactivityWarnedAt: time.Now().Add(-3 * time.Hour),
			Tracking: map[string]*TrackInfo{
				"AABBCC": {LastUpdate: time.Now().Add(-2*time.Hour - 30*time.Minute)},
			},
		}
		got := buildDashboard(s, devices, tz)
		if !strings.Contains(got, "Нет beacon-ов") {
			t.Errorf("expected inactivity warning in: %q", got)
		}
	})
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `go test ./internal/tracker/ -run TestBuildDashboard -v 2>&1 | tail -5`
Expected: `undefined: buildDashboard`.

- [ ] **Step 3: Implement `buildDashboard`**

Add to `internal/tracker/renderer.go`, near `buildSummary`:

```go
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

	// Reuse the existing pilot summary as the body. drivers list passed empty —
	// driver coordinates are not part of the dashboard's body (they are summarised
	// in the status meta line).
	body := buildSummary(s.Tracking, s.Landing, nil, s.TrackAreaRadius, devices, tz)
	sb.WriteString("\n\n")
	sb.WriteString(body)
	return sb.String()
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/tracker/ -run TestBuildDashboard -v 2>&1 | tail -20`
Expected: PASS for all three subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/tracker/renderer.go internal/tracker/tracker_test.go
git commit -m "Add buildDashboard renderer"
```

---

## Task 3: `dashboardButtons()` — inline keyboard

**Files:**
- Modify: `internal/tracker/renderer.go`
- Test: `internal/tracker/tracker_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/tracker/tracker_test.go`:

```go
func TestDashboardButtons(t *testing.T) {
	collect := func(kb *models.InlineKeyboardMarkup) []string {
		var out []string
		if kb == nil {
			return nil
		}
		for _, row := range kb.InlineKeyboard {
			for _, b := range row {
				out = append(out, b.CallbackData)
			}
		}
		return out
	}

	t.Run("tracking on", func(t *testing.T) {
		s := &GroupSession{TrackingOn: true, Tracking: map[string]*TrackInfo{"AA": {}}}
		got := collect(dashboardButtons(s))
		want := []string{"dashboard:stop", "dashboard:list", "dashboard:area", "dashboard:driver"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("tracking off with pilots", func(t *testing.T) {
		s := &GroupSession{Tracking: map[string]*TrackInfo{"AA": {}}}
		got := collect(dashboardButtons(s))
		want := []string{"dashboard:start", "dashboard:list", "dashboard:add", "dashboard:end"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("tracking off no pilots", func(t *testing.T) {
		s := &GroupSession{Tracking: map[string]*TrackInfo{}}
		got := collect(dashboardButtons(s))
		want := []string{"dashboard:add", "dashboard:area", "dashboard:end"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("radar on", func(t *testing.T) {
		s := &GroupSession{RadarOn: true}
		got := collect(dashboardButtons(s))
		want := []string{"dashboard:radar_stop", "dashboard:radar_radius"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `go test ./internal/tracker/ -run TestDashboardButtons -v 2>&1 | tail -5`
Expected: `undefined: dashboardButtons`.

- [ ] **Step 3: Implement `dashboardButtons`**

Add to `internal/tracker/renderer.go`, near `pilotButtons`:

```go
// dashboardButtons builds the state-dependent inline action row for the
// dashboard message. Returns nil if no actions are available.
func dashboardButtons(s *GroupSession) *models.InlineKeyboardMarkup {
	if s == nil {
		return nil
	}
	btn := func(text, action string) models.InlineKeyboardButton {
		return models.InlineKeyboardButton{Text: text, CallbackData: "dashboard:" + action}
	}
	var rows [][]models.InlineKeyboardButton
	switch {
	case s.RadarOn:
		rows = [][]models.InlineKeyboardButton{{
			btn("⏹ Радар стоп", "radar_stop"),
			btn("📡 Радиус", "radar_radius"),
		}}
	case s.TrackingOn:
		rows = [][]models.InlineKeyboardButton{
			{btn("⏹ Стоп", "stop"), btn("📋 Список", "list")},
			{btn("📡 Зона", "area"), btn("🚗 Водитель", "driver")},
		}
	case len(s.Tracking) > 0:
		rows = [][]models.InlineKeyboardButton{
			{btn("▶️ Старт", "start"), btn("📋 Список", "list")},
			{btn("➕ Добавить", "add"), btn("🔄 Завершить", "end")},
		}
	default:
		rows = [][]models.InlineKeyboardButton{{
			btn("➕ Добавить", "add"),
			btn("📡 Зона", "area"),
			btn("🔄 Завершить", "end"),
		}}
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/tracker/ -run TestDashboardButtons -v 2>&1 | tail -20`
Expected: PASS for all four subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/tracker/renderer.go internal/tracker/tracker_test.go
git commit -m "Add dashboardButtons inline keyboard"
```

---

## Task 4: `refreshDashboard()` helper + replace summary block

**Files:**
- Modify: `internal/tracker/client.go`
- Test: `internal/tracker/tracker_test.go` (lifecycle assertions on state, not bot calls)

This task replaces the existing "edit or send summary" block in `sendUpdates` with a single `refreshDashboard` call. The block already handles `isMessageGone` and re-send, so we keep that logic but move it into the new helper.

- [ ] **Step 1: Locate the current summary edit block**

In `internal/tracker/client.go`, find the block that begins with `// Send or update the single summary message.` (around line 606-662 after the patch A changes). Read it now — this is the logic that moves into `refreshDashboard`.

- [ ] **Step 2: Add the `refreshDashboard` method**

Add to `internal/tracker/client.go`, near `markLiveLocationDead`:

```go
// refreshDashboard renders the dashboard text and inline keyboard for the
// current session state and either edits the existing dashboard message or
// posts a new one. Idempotent: safe to call any number of times per tick and
// from any goroutine. Handles the "edit on a dead message" path by reposting
// and re-pinning, same semantics as patch A.
//
// Returns the live MessageID after the call. Must be called outside t.mu.
func (t *Tracker) refreshDashboard(ctx context.Context, chatID int64) int {
	t.mu.Lock()
	if t.session == nil || t.session.ChatID != chatID {
		t.mu.Unlock()
		return 0
	}
	s := t.session
	b := t.bot
	dashID := s.DashboardMsgID
	dashPinned := s.DashboardPinned
	tracking := make(map[string]*TrackInfo, len(s.Tracking))
	for id, info := range s.Tracking {
		cp := *info
		tracking[id] = &cp
	}
	devices := t.devices
	tz := s.tz()
	// We work on a shallow copy of the session header to render off-lock.
	sCopy := *s
	sCopy.Tracking = tracking
	t.mu.Unlock()

	if b == nil {
		return dashID
	}

	text := buildDashboard(&sCopy, devices, tz)
	kb := dashboardButtons(&sCopy)
	// Pilot-specific buttons (nav, pickup) live on the dashboard too, below the
	// action row, so the chat has one place for everything.
	pilotKb := pilotButtons(tracking)
	if pilotKb != nil {
		if kb == nil {
			kb = pilotKb
		} else {
			kb.InlineKeyboard = append(kb.InlineKeyboard, pilotKb.InlineKeyboard...)
		}
	}

	newID := 0
	if dashID != 0 {
		_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      chatID,
			MessageID:   dashID,
			Text:        text,
			ReplyMarkup: kb,
		})
		switch {
		case err == nil, isMessageNotModified(err):
			newID = dashID
		case isMessageGone(err):
			slog.Warn("dashboard gone, will repost", "msg_id", dashID, "err", err)
			t.mu.Lock()
			if t.session != nil {
				t.session.DashboardMsgID = 0
				t.session.DashboardPinned = false
			}
			t.mu.Unlock()
			dashID = 0
			dashPinned = false
		default:
			slog.Error("failed to edit dashboard", "err", err)
			newID = dashID
		}
	}
	if dashID == 0 {
		params := &bot.SendMessageParams{ChatID: chatID, Text: text}
		if kb != nil {
			params.ReplyMarkup = kb
		}
		msg, err := b.SendMessage(ctx, params)
		if err != nil {
			slog.Error("failed to send dashboard", "err", err)
			return 0
		}
		newID = msg.ID
		t.mu.Lock()
		if t.session != nil {
			t.session.DashboardMsgID = msg.ID
		}
		t.mu.Unlock()
	}

	if newID != 0 && !dashPinned {
		if _, err := b.PinChatMessage(ctx, &bot.PinChatMessageParams{
			ChatID:              chatID,
			MessageID:           newID,
			DisableNotification: true,
		}); err != nil {
			slog.Warn("failed to pin dashboard", "err", err)
		} else {
			slog.Info("dashboard pinned", "msg_id", newID)
			t.mu.Lock()
			if t.session != nil {
				t.session.DashboardPinned = true
			}
			t.mu.Unlock()
		}
	}
	return newID
}
```

- [ ] **Step 3: Replace the summary block in `sendUpdates`**

Find this block (in `sendUpdates`, after the per-pilot loop and `summary := buildSummary(...)`):

```go
		// Send or update the single summary message. newSummaryID tracks
		// whichever ID the summary now lives at — used by the pin block below
		// to know whether we have a stable target to pin.
		summary := buildSummary(local, landing, drivers, areaRadius, devices, tz)
		kb := pilotButtons(local)
		newSummaryID := 0
		if dashboardMsgID != 0 {
		    /* ... ~60 lines through the pin block ... */
		}
```

Replace the whole "summary build + edit/send + pin" section (everything from `summary := buildSummary(...)` through the closing of the pin block) with:

```go
		// Refresh the dashboard. Heartbeat path; explicit state changes also
		// call refreshDashboard directly so the UI doesn't lag a tick behind.
		t.refreshDashboard(ctx, chatID)
```

Remove the now-unused local variables (`summary`, `kb`, `newSummaryID`, etc.) and the now-unused capture of `dashboardMsgID` / `dashboardPinned` / `landing` / `drivers` / `areaRadius` from earlier in the loop — they were only used by the deleted block. Keep `chatID`, `local`, `b`, `devices`, `tz` (still used by the live-location loop and the inactivity check).

- [ ] **Step 4: Remove the standalone inactivity warning chat message**

In `checkInactivity` (added by patch C), the warn branch currently sends a chat message. Remove that send so the warning surfaces only through the dashboard:

Find:

```go
	if age >= inactivityWarnAfter {
		t.mu.Lock()
		warned := t.session == nil || !t.session.InactivityWarnedAt.IsZero()
		if !warned && t.session != nil {
			t.session.InactivityWarnedAt = time.Now()
		}
		t.mu.Unlock()
		if warned {
			return false
		}
		slog.Info("inactivity warning sent", "chat_id", chatID, "age", age.Round(time.Minute))
		remaining := (inactivityStopAfter - age).Round(time.Minute)
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   fmt.Sprintf("⚠️ Нет beacon-ов уже %s. Трекинг остановится автоматически через %s, если ничего не появится.", age.Round(time.Minute), remaining),
		}); err != nil {
			slog.Error("failed to send inactivity warning", "err", err)
		}
		return false
	}
```

Replace with:

```go
	if age >= inactivityWarnAfter {
		t.mu.Lock()
		warned := t.session == nil || !t.session.InactivityWarnedAt.IsZero()
		if !warned && t.session != nil {
			t.session.InactivityWarnedAt = time.Now()
		}
		t.mu.Unlock()
		if !warned {
			slog.Info("inactivity warning surfaced", "chat_id", chatID, "age", age.Round(time.Minute))
		}
		// The warning text appears inside the dashboard via buildDashboard,
		// rendered by the heartbeat refresh on the next tick. No separate
		// chat message — keeps the chat tidy.
		return false
	}
```

The auto-stop notice (in the `age >= inactivityStopAfter` branch) stays as a standalone message — it's a one-shot event, not a state.

- [ ] **Step 5: Verify build**

Run: `go build ./... 2>&1`
Expected: no output (clean build).

- [ ] **Step 6: Run full test suite**

Run: `go test ./... 2>&1 | tail -5`
Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add internal/tracker/client.go
git commit -m "Add refreshDashboard helper; route sendUpdates through it"
```

---

## Task 5: Extract `execAddNoArgsPrompt` from `cmdAdd`

**Files:**
- Modify: `internal/tracker/commands.go` (extraction)
- Modify: `internal/tracker/flows.go` (new exec function)

The `cmdAdd` no-args branch (sets `PendingGroup`, tries DM, falls back to deep link) is inlined inside the command handler. The dashboard callback needs the same flow without a `*models.Update`. Extract.

- [ ] **Step 1: Read current `cmdAdd` no-args branch**

In `internal/tracker/commands.go`, find lines 199–~260 inside `cmdAdd` — the `// /add without arguments: initiate DM flow.` block down to the end of the function. This whole block moves into a new helper.

- [ ] **Step 2: Add the new helper to `flows.go`**

Append to `internal/tracker/flows.go`:

```go
// execAddNoArgsPrompt runs the "/add without arguments" flow: sets the user's
// PendingGroup, tries to DM the user, and falls back to posting a deep-link
// button in the group when the DM cannot be delivered. Returns the ID of the
// in-group ack message (0 if none was sent). Extracted from cmdAdd so the
// dashboard callback can reuse the same path.
func (t *Tracker) execAddNoArgsPrompt(ctx context.Context, b *bot.Bot, chatID int64, userID int64) int {
	t.mu.Lock()
	s := t.session
	if s == nil || s.ChatID != chatID {
		t.mu.Unlock()
		return 0
	}
	u := t.ensureUserByID(userID)
	u.PendingGroup = s.ChatID
	hasOGNID := u.OGNID != ""
	ognID := u.OGNID
	t.saveState()
	botUsername := t.botUsername
	groupChatID := s.ChatID
	t.mu.Unlock()

	var dmText string
	if hasOGNID {
		dmText = fmt.Sprintf("Ваш OGN ID: %s\nОтправьте новый ID или /confirm чтобы использовать текущий.", ognID)
	} else {
		dmText = "Отправьте ваш OGN ID (6-значный адрес трекера):"
	}
	_, dmErr := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: userID,
		Text:   dmText,
	})
	if dmErr == nil {
		t.mu.Lock()
		u.DMChatID = userID
		t.saveState()
		t.mu.Unlock()
		return t.sendAck(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "Написал вам в личку",
		}, "failed to confirm DM sent in group")
	}

	slog.Warn("failed to send DM, falling back to deep link", "user_id", userID, "err", dmErr)
	deepLink := fmt.Sprintf("https://t.me/%s?start=add_%d", botUsername, groupChatID)
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "Добавить свой трекер", URL: deepLink}},
		},
	}
	return t.sendAck(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        "Откройте бота в личке, чтобы добавить себя.",
		ReplyMarkup: kb,
	}, "failed to send deep-link prompt")
}
```

**Note:** if `ensureUserByID` does not already exist, add it next to `ensureUser` in `tracker.go`:

```go
// ensureUserByID looks up a user record by Telegram user ID, creating an
// empty UserInfo if absent. Caller must hold t.mu.
func (t *Tracker) ensureUserByID(userID int64) *UserInfo {
	if u, ok := t.users[userID]; ok {
		return u
	}
	u := &UserInfo{UserID: userID}
	t.users[userID] = u
	return u
}
```

- [ ] **Step 3: Rewrite the `cmdAdd` no-args branch to call the new helper**

In `internal/tracker/commands.go`, replace the original no-args block (lines ~199–end of cmdAdd) with:

```go
	// /add without arguments: initiate DM flow.
	ackID := t.execAddNoArgsPrompt(ctx, b, m.Chat.ID, m.From.ID)
	if ackID != 0 {
		t.mu.Lock()
		t.appendPendingCleanup(m.From.ID, m.ID, ackID)
		t.mu.Unlock()
	}
```

`scheduleEphemeralDelete` doesn't fire here because the DM bridge's group ack finalizes the cleanup chain — preserve existing semantics.

- [ ] **Step 4: Verify build and tests**

Run: `go build ./... && go test ./... 2>&1 | tail -5`
Expected: clean build, all green.

- [ ] **Step 5: Commit**

```bash
git add internal/tracker/
git commit -m "Extract execAddNoArgsPrompt from cmdAdd"
```

---

## Task 6: Dashboard callback handlers

**Files:**
- Modify: `internal/tracker/callbacks.go`
- Modify: `internal/tracker/tracker.go` (register handlers)

- [ ] **Step 1: Read the current `callbacks.go` to copy style**

```bash
head -50 internal/tracker/callbacks.go
```

Note: the imports, how the file calls `b.AnswerCallbackQuery`, and how existing callbacks dispatch.

- [ ] **Step 2: Add the dashboard callback dispatcher**

Append to `internal/tracker/callbacks.go`:

```go
// cbDashboardAction is the single entry point for all dashboard:* callback
// queries. It answers the callback (clears the spinner), routes to the
// appropriate exec* helper, then triggers a dashboard refresh so the user
// sees the new state without waiting for the heartbeat tick.
func (t *Tracker) cbDashboardAction(ctx context.Context, b *bot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	if cq == nil {
		return
	}
	// Acknowledge first so the client UI stops spinning.
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cq.ID})

	chatID := int64(0)
	if msg := cq.Message.Message; msg != nil {
		chatID = msg.Chat.ID
	}
	if chatID == 0 {
		return
	}
	userID := cq.From.ID
	username := cq.From.Username
	action := strings.TrimPrefix(cq.Data, "dashboard:")
	slog.Info("dashboard action", "action", action, "chat_id", chatID, "user_id", userID)

	switch action {
	case "start":
		t.execTrackOn(ctx, b, chatID)
	case "stop":
		t.execTrackOff(ctx, b, chatID)
	case "list":
		t.execList(ctx, b, chatID)
	case "add":
		t.execAddNoArgsPrompt(ctx, b, chatID, userID)
	case "area":
		t.execArea(ctx, b, chatID, defaultAreaRadius, userID, 0)
	case "driver":
		t.execDriver(ctx, b, chatID, userID, username, 0)
	case "end":
		t.askSessionResetConfirm(ctx, b, chatID, userID, 0)
	case "radar_stop":
		t.execRadarOff(ctx, b, chatID)
	case "radar_radius":
		t.execRadarAskRadius(ctx, b, chatID, userID, 0)
	default:
		slog.Warn("unknown dashboard action", "action", action)
	}

	t.refreshDashboard(ctx, chatID)
}
```

- [ ] **Step 3: Add the `strings` import to `callbacks.go` if missing**

Run: `head -15 internal/tracker/callbacks.go` and verify imports. Add `"strings"` if absent.

- [ ] **Step 4: Register the handler in `tracker.go`**

In `RegisterHandlers` in `internal/tracker/tracker.go`, after the existing `b.RegisterHandler(bot.HandlerTypeCallbackQueryData, ...)` registrations, add:

```go
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "dashboard:", bot.MatchTypePrefix, t.cbDashboardAction)
```

`MatchTypePrefix` matches any callback data starting with `dashboard:` so one handler dispatches all sub-actions.

- [ ] **Step 5: Verify build**

Run: `go build ./... 2>&1`
Expected: clean.

- [ ] **Step 6: Run tests**

Run: `go test ./... 2>&1 | tail -5`
Expected: green.

- [ ] **Step 7: Commit**

```bash
git add internal/tracker/callbacks.go internal/tracker/tracker.go
git commit -m "Add dashboard callback dispatcher"
```

---

## Task 7: Remove reply keyboard, call `refreshDashboard` after state changes

**Files:**
- Modify: `internal/tracker/types.go`
- Modify: `internal/tracker/commands.go`, `internal/tracker/flows.go`
- Test: (existing tests must keep passing)

- [ ] **Step 1: Find all reply-keyboard usages**

```bash
grep -rn "replyKeyboard\|ReplyMarkup: kb" internal/tracker/ --include="*.go"
```

Note every call site. Each one of these needs handling.

- [ ] **Step 2: Add a `RemoveReplyKeyboard` helper**

So a single message can carry the "remove the old bottom bar" instruction, add to `internal/tracker/util.go` (or near the top of `commands.go` if it feels more local):

```go
// removeReplyKB is a ReplyKeyboardRemove value that tells Telegram clients to
// hide whatever reply keyboard is currently visible. Used in /start so users
// with the legacy bottom bar lose it the moment they restart the session,
// without waiting for them to reopen the chat.
var removeReplyKB = &models.ReplyKeyboardRemove{RemoveKeyboard: true}
```

- [ ] **Step 3: Update `cmdStart` and `cmdStartSession` to remove the reply keyboard and trigger a dashboard refresh**

In `internal/tracker/commands.go`, replace the `kb := t.session.replyKeyboard()` line in `cmdStart` and use `removeReplyKB` instead of `kb` in the corresponding `scheduleAck`. Then, immediately before returning, call `t.refreshDashboard(ctx, m.Chat.ID)`.

Concretely, in `cmdStart`:

```go
	t.session = &GroupSession{
		ChatID:   m.Chat.ID,
		Tracking: make(map[string]*TrackInfo),
		Drivers:  make(map[int64]*DriverInfo),
	}
	kb := t.session.replyKeyboard()
	t.saveState()
	t.mu.Unlock()

	t.scheduleAck(ctx, m.Chat.ID, m.ID, &bot.SendMessageParams{
		ChatID:      m.Chat.ID,
		Text:        "Сессия начата. Используйте /add <id> или /area.",
		ReplyMarkup: kb,
	}, "failed to send start message")
```

becomes:

```go
	t.session = &GroupSession{
		ChatID:   m.Chat.ID,
		Tracking: make(map[string]*TrackInfo),
		Drivers:  make(map[int64]*DriverInfo),
	}
	t.saveState()
	t.mu.Unlock()

	t.scheduleAck(ctx, m.Chat.ID, m.ID, &bot.SendMessageParams{
		ChatID:      m.Chat.ID,
		Text:        "Сессия начата. Используйте /add <id> или /area.",
		ReplyMarkup: removeReplyKB,
	}, "failed to send start message")

	t.refreshDashboard(ctx, m.Chat.ID)
```

Apply the same pattern in `cmdStartSession`.

- [ ] **Step 4: Strip reply keyboard from every other call site**

For each remaining `kb := s.replyKeyboard()` (or `kb := t.session.replyKeyboard()`) found in Step 1:

- Remove the line.
- Remove `ReplyMarkup: kb` from the surrounding `SendMessage`/`scheduleAck` params.
- Right before the function returns, call `t.refreshDashboard(ctx, chatID)` so the dashboard reflects the new state immediately.

This applies to (verify with the grep from Step 1, your exact list may differ):
- `cmdSessionReset` confirmation flow (after wipe)
- `execTrackOff` (after stop)
- `execTrackOn` (after start)
- Any `/add`, `/remove`, `/area`, `/landing`, `/driver` exit points

If a call site does **not** change session state (e.g. an error reply), do not add `refreshDashboard` there — only after real state changes.

- [ ] **Step 5: Delete the `replyKeyboard()` method**

In `internal/tracker/types.go`, delete the entire `replyKeyboard()` method on `*GroupSession`. The compiler will flag any remaining callers.

- [ ] **Step 6: Verify build**

Run: `go build ./... 2>&1`
Expected: clean. Any error means a `replyKeyboard()` call site was missed in Step 4 — fix it.

- [ ] **Step 7: Run tests**

Run: `go test ./... 2>&1 | tail -5`
Expected: green.

- [ ] **Step 8: Commit**

```bash
git add internal/tracker/
git commit -m "Remove reply keyboard; refresh dashboard after state changes"
```

---

## Task 8: Delete dashboard on session reset

**Files:**
- Modify: `internal/tracker/flows.go` (session reset flow)
- Test: `internal/tracker/tracker_test.go`

- [ ] **Step 1: Locate session reset implementation**

```bash
grep -n "session_reset\|SessionReset\|wipe_pilots" internal/tracker/flows.go internal/tracker/commands.go | head -10
```

Find the function that actually clears the session (likely `execSessionReset` in flows.go, called from a confirm callback).

- [ ] **Step 2: Write a failing test for dashboard cleanup on reset**

Append to `internal/tracker/tracker_test.go`:

```go
func TestSessionResetClearsDashboard(t *testing.T) {
	tr := &Tracker{
		users: make(map[int64]*UserInfo),
		session: &GroupSession{
			ChatID:          -100,
			Tracking:        map[string]*TrackInfo{},
			DashboardMsgID:  77,
			DashboardPinned: true,
		},
	}
	tr.mu.Lock()
	tr.clearDashboardForReset()
	if tr.session.DashboardMsgID != 0 {
		t.Errorf("DashboardMsgID = %d, want 0", tr.session.DashboardMsgID)
	}
	if tr.session.DashboardPinned {
		t.Errorf("DashboardPinned should be false after reset")
	}
	tr.mu.Unlock()
}
```

- [ ] **Step 3: Run the test, verify it fails**

Run: `go test ./internal/tracker/ -run TestSessionResetClearsDashboard -v 2>&1 | tail -5`
Expected: `undefined: clearDashboardForReset`.

- [ ] **Step 4: Implement `clearDashboardForReset`**

Add to `internal/tracker/client.go` near `unpinSummaryAsync`:

```go
// clearDashboardForReset deletes the pinned dashboard message and resets the
// session's dashboard bookkeeping. Caller must hold t.mu. Telegram deletion
// runs in a detached goroutine so the caller can stay under the lock.
func (t *Tracker) clearDashboardForReset() {
	s := t.session
	if s == nil {
		return
	}
	msgID := s.DashboardMsgID
	chatID := s.ChatID
	s.DashboardMsgID = 0
	s.DashboardPinned = false
	if msgID != 0 {
		t.deleteMessagesAsync(chatID, msgID)
	}
}
```

- [ ] **Step 5: Run the test**

Run: `go test ./internal/tracker/ -run TestSessionResetClearsDashboard -v 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 6: Wire it into the actual reset flow**

In the function found in Step 1 (likely `execSessionReset` in `flows.go`), where the session is cleared / pilots wiped under `t.mu`, add a call to `t.clearDashboardForReset()` right before unlocking. The `unpinSummaryAsync` call that already lives in `stopTrackingAsync` becomes redundant for the dashboard case but only fires for the summary pin path that no longer exists — verify the call to `unpinSummaryAsync` (if any) in the reset path is removed too. Replace it with `clearDashboardForReset`.

- [ ] **Step 7: Run full test suite**

Run: `go test ./... 2>&1 | tail -5`
Expected: green.

- [ ] **Step 8: Commit**

```bash
git add internal/tracker/
git commit -m "Delete dashboard on session reset"
```

---

## Task 9: Manual integration verification

**Files:** none (operator-driven check).

The bot path is hard to unit-test end-to-end without a Telegram mock; this task is the sign-off checklist.

- [ ] **Step 1: Start local bot**

Run: `go run ./cmd/bot` (requires `TELEGRAM_BOT_TOKEN` in env).

- [ ] **Step 2: In a test group, walk the lifecycle**

In order, verify the dashboard text and buttons after each action:

| Action | Expect |
|---|---|
| `/start` | Dashboard appears, pinned. Header "Сессия активна · Трекинг: ⏸ выкл.". Buttons: `[➕ Добавить] [📡 Зона] [🔄 Завершить]`. Reply keyboard at bottom is gone. |
| `/add ABCDEF Eve` | Dashboard edits in place. Body: `📊 1 пилот(ов) — …`. Buttons gain `[▶️ Старт] [📋 Список]`. |
| `/track_on` (or tap `▶️ Старт`) | Header changes to `Трекинг: ✅ вкл.`. Buttons become `[⏹ Стоп] [📋 Список] [📡 Зона] [🚗 Водитель]`. |
| Wait for first beacon | Body shows pilot row with position. Live-location pin appears below dashboard. |
| Simulate landing (or wait) | Pilot row shows 🪂. Per-pilot `[🗺 …] [✅ Забрал …]` buttons appear under the action row inside the same dashboard message. |
| Tap `[⏹ Стоп]` | Header `Трекинг: ⏸ выкл.`, buttons back to off-with-pilots set. |
| Tap `[🔄 Завершить]` (confirm) | Dashboard message disappears entirely. No reply keyboard. |
| `/start` again | New Dashboard appears, pinned, empty list. |

- [ ] **Step 3: Restart the bot in the middle of a session**

While tracking is on:
- Stop the bot (Ctrl+C).
- Restart `go run ./cmd/bot`.
- In the group, send any message that triggers a refresh (e.g. tap a dashboard button or wait for the 30 s ticker).
- Verify: the same dashboard message is edited in place (no new dashboard, no duplicate pin). This proves the persistence migration works.

- [ ] **Step 4: Force an `isMessageGone` scenario**

- Manually delete the pinned dashboard message in the group.
- Wait one tick (≤30 s) for the heartbeat refresh.
- Verify: a new dashboard is posted and pinned, log shows `WARN dashboard gone, will repost`.

- [ ] **Step 5: Trigger inactivity warning surface**

- Lower `inactivityWarnAfter` to e.g. `2 * time.Minute` and `inactivityStopAfter` to `5 * time.Minute` for the test, rebuild.
- With tracking on and a previously-received beacon, stop the beacon feed (or wait).
- Verify: at ~2 min, the dashboard header gains the `⚠️ Нет beacon-ов 2м` line. **No** standalone `⚠️ …` chat message is posted.
- At ~5 min, the auto-stop notice arrives as a separate chat message (one-shot event, as designed), tracking flips off, dashboard header updates accordingly.
- Restore the original constants after the test.

- [ ] **Step 6: No commit (manual verification only)**

If something failed in this task, roll back to the previous task and fix the underlying code.

---

## Final commit hygiene

After Task 9 passes:

- [ ] Move the spec file out of "ready for plan" status — update its `Status:` header to `implemented`.
- [ ] Run `graphify update .` per project convention.
- [ ] Open the merge PR (or wait for user's "сливай").

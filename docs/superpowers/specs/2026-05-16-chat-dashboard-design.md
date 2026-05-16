# Persistent chat dashboard — design

**Status:** approved, ready for implementation plan
**Date:** 2026-05-16
**Branch:** `feat/persistent-dashboard`

## Problem

After the last cleanup pass the bot deletes its transient messages aggressively. The reply keyboard (bottom button bar) attaches to one of those messages, so when the message goes the keyboard goes with it — users in the group then have no idea whether tracking is on, who is being tracked, or what actions are available.

The existing pinned **summary** message is closer to what's needed, but it only exists when tracking is on **and** at least one pilot has a position. Outside that window the chat is silent.

## Goal

One persistent pinned **Dashboard** message per group session that always shows the current state and the actions available right now. It survives message cleanup, replaces the reply keyboard, and is the single source of UI truth between `/start` and `/session_reset`.

## Out of scope

- Removing `/track_on`, `/track_off`, etc. They keep working as slash commands alongside the inline buttons.
- Inline workflows for adding pilots (the `➕ Добавить` button continues to launch the existing DM deep-link flow).
- Landing alerts and per-pilot live-location pins. They stay independent of the Dashboard.

## Lifecycle

| Trigger | Effect on Dashboard |
|---|---|
| `/start` in group, no prior session | Create Dashboard message. Pin silently. Persist `DashboardMsgID`. |
| `/start` in group with existing session | No-op (session already has a Dashboard; re-pin if missing). |
| Any state-changing event (track on/off, add/remove pilot, area set/unset, driver register, beacon, landing detect, picked up, radar on/off, status auto-warning) | `EditMessageText` on the existing `DashboardMsgID`. |
| Bot restart, session was active | Reuse existing `DashboardMsgID` from persisted state — edit, do not repost. |
| Edit fails with `isMessageGone` | Post a new Dashboard, repin, update `DashboardMsgID`. Existing logic from patch A applies. |
| `/session_reset` | Delete the Dashboard message via `deleteMessagesAsync`, clear `DashboardMsgID` and `DashboardPinned`. |

The Dashboard never lives across sessions: it appears with `/start` and disappears with `/session_reset`.

## Content

Rendered by a new `buildDashboard(s *GroupSession, devices, tz, drivers)` function in `internal/tracker/renderer.go`. It composes:

1. **Status header** (always 1–2 lines)
   - `📌 Сессия активна · Трекинг: ✅ вкл.` / `⏸ выкл.`
   - Inline annotations: `📡 Зона: 100км · 🚗 Водитель: —` when set.
2. **Inactivity warning** (only when triggered by patch C)
   - `⚠️ Нет beacon-ов 2ч12м` — replaces the standalone chat message that patch C used to send. Cleared automatically when beacons resume.
3. **Pilot summary** (existing `buildSummary` output, reused verbatim) — header line with counts, then per-pilot sections grouped by status. Empty fallback: `Список пуст. /add <id> чтобы добавить пилота.`
4. **Radar mode** — when `RadarOn`, the body switches to the existing radar summary content instead of the pilot list.

The Dashboard is one Telegram message; multiple visual sections live inside one text body separated by blank lines.

## Inline keyboard

The Dashboard is the only place inline action buttons live (per-pilot pickup buttons remain inside it as today).

| State | Top-level buttons |
|---|---|
| Session active, tracking **off**, no pilots | `[➕ Добавить]` `[📡 Зона]` `[🔄 Завершить]` |
| Session active, tracking **off**, has pilots | `[▶️ Старт]` `[📋 Список]` `[➕ Добавить]` `[🔄 Завершить]` |
| Tracking **on** | `[⏹ Стоп]` `[📋 Список]` `[📡 Зона]` `[🚗 Водитель]` |
| Radar **on** | `[⏹ Радар стоп]` `[📡 Радиус]` |

Each button uses `CallbackData: "dashboard:<action>"`. Handlers reuse the existing `exec*` functions (no new business logic).

`[📋 Список]` opens the current `execList` flow which posts a transient list message; the Dashboard itself already shows the same data so this button is mostly a backwards-compat hook — it stays for users who learned it. Worth considering removal later, not in this scope.

## Reply keyboard

Removed. All call sites that currently attach `s.replyKeyboard()` to outgoing messages stop doing so. After this change, a `ReplyKeyboardRemove` is sent once in the same message that creates the Dashboard so the bottom bar disappears on existing clients without waiting for them to reopen the chat.

## Code changes

| File | Change |
|---|---|
| `types.go` | Rename `GroupSession.SummaryMsgID` → `DashboardMsgID`, `SummaryPinned` → `DashboardPinned`. Delete the `replyKeyboard()` method. |
| `persist.go` | Rename the JSON tags accordingly. Migration: keep reading the old `summary_msg_id` / `summary_pinned` field names as fallback on load so deployed instances don't lose their pin reference. New writes use the new tags. |
| `renderer.go` | New `buildDashboard()` that wraps `buildSummary()` and adds the status header and inactivity warning. New `dashboardButtons(s)` builds the state-dependent inline keyboard. |
| `client.go` `sendUpdates` | Replace the "edit summary or send new" block with the Dashboard edit/repost block. Remove the "only send when withPos>0" guard — the Dashboard exists from the moment a session does. The pin block remains, gated on `DashboardPinned`. |
| `client.go` patch C | Drop the standalone "inactivity warning" chat message; surface the same info inside the Dashboard text and through a single `slog.Info("inactivity warning surfaced")`. The auto-stop notice keeps its standalone chat message — it's a one-shot event, not a state. |
| `commands.go` / `flows.go` | Remove all `ReplyMarkup: kb` assignments where `kb` came from `s.replyKeyboard()`. After each state-changing command, call `t.refreshDashboard(ctx, chatID)` so the dashboard reflects the new state immediately (instead of waiting up to 30 s for the ticker). |
| `callbacks.go` | New callback handlers for `dashboard:start`, `dashboard:stop`, `dashboard:add`, `dashboard:list`, `dashboard:area`, `dashboard:driver`, `dashboard:end`, `dashboard:radar_stop`, `dashboard:radar_radius`. Each calls the existing `exec*` function. |
| `tracker.go` | Register the new callback handlers in `RegisterHandlers`. |
| `cleanup.go` | No change. The Dashboard is not ephemeral. |

## Edge cases

- **Telegram 48-hour edit limit.** Caught by `isMessageGone` (patch A). The repost path handles it: send new, pin, update `DashboardMsgID`.
- **Bot not admin → cannot pin.** `failed to pin` warning, `DashboardPinned` stays false, Dashboard still works (unpinned). Same behaviour as today's summary.
- **Race: user taps button while ticker is editing.** Callback acquires `t.mu` like all command handlers. Worst case: stale UI for one tick (≤30 s) — acceptable.
- **Old `session.json` after upgrade.** Migration code reads `summary_msg_id` as fallback so the pinned message survives the rename.
- **Group chat changes.** Same `data/session.json` model as today; one chat per session is preserved.

## Testing

- Unit: `buildDashboard()` output for each state combination (off/no pilots, off/pilots, on/pilots, radar on, plus inactivity warning).
- Unit: `dashboardButtons()` returns the right button set per state.
- Unit: persistence round-trip with both old and new JSON tag forms.
- Manual: in a test group, run through `/start → /add → /track_on → simulate landing → /track_off → /session_reset` and verify the Dashboard reflects each state without spawning transient duplicates.

## Open questions

None at design time. All four user-facing decisions are made:

1. Persistent block is a pinned message with inline keyboard.
2. Reply keyboard is removed entirely.
3. Dashboard disappears on `/session_reset`, returns with the next `/start`.
4. Inactivity warning surfaces inside the Dashboard instead of as a standalone message.

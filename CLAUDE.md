# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# telegram-ogn-tracker

Telegram bot for real-time paraglider/glider tracking via OGN (Open Glider Network) APRS.

## Build & Run

```bash
# Local dev
go run ./cmd/bot

# Docker
docker compose up --build
docker compose build && docker compose up -d   # make up equivalent
docker compose down && docker compose up --build  # full rebuild
```

CI: GitHub Actions on `v*` tags → multi-platform binaries + GHCR image.

## Config

- `TELEGRAM_BOT_TOKEN` env var (from `.env`, see `.env.example`)
- State persisted to `data/session.json` (mounted as Docker volume)
- `DEBUG=1` raises slog level from INFO to DEBUG. Leave unset in prod — DEBUG
  emits ~6 k lines per active hour (every parsed beacon + cycle stats) and
  bloats `logs/bot.log` quickly.
- `LOG_FILE` overrides the default `logs/bot.log` destination.

## Structure

```
cmd/bot/main.go              — entry point: wires Tracker, bot, signal handling
internal/tracker/
  tracker.go                 — Tracker struct, GroupSession, TrackInfo, helpers, filter logic
  client.go                  — OGN APRS goroutines (runClient, sendUpdates, radar)
  commands.go                — all Telegram command/callback handlers
  persist.go                 — JSON save/load, migration from legacy format
```

## Architecture

**Tracker** is the single central struct. It owns:
- `session *GroupSession` — the active group chat session (one group at a time)
- `users map[int64]*UserInfo` — registered users across sessions (persistent)
- `aprs *client.Client` — OGN APRS client instance
- `devices map[string]ddb.Device` — OGN Device Database cache (model/registration display)
- `sync.Mutex` — guards all of the above

**GroupSession** holds per-session state:
- `Tracking map[string]*TrackInfo` — pilots being tracked, keyed by 6-char OGN ID
- Radar mode fields, driver map, area/landing coords (see `tracker.go`)
- Runtime-only fields (StopCh, WaitingLanding, etc.) are NOT persisted

**Goroutine lifecycle** (`/track_on`):
1. `runClient(stopCh)` — APRS feed → landing detection → updates `Tracking` map
2. `sendUpdates(stopCh)` — 30s ticker → edits live locations + summary message

Radar mode uses a parallel pair: `runRadarClient` + `sendRadarUpdates` with their own `RadarStopCh`.

When APRS filter changes (adding/removing IDs, area change), `updateFilter()` tears down the current client and spawns fresh goroutines with the new filter.

**State persistence** (`persist.go`): `saveState()` / `loadState()` serialize to `data/session.json`. On restart, if `TrackingOn` was true, `NewTracker()` + `RegisterHandlers()` auto-resume tracking goroutines.

## Commands

| Command | Where | Effect |
|---------|-------|--------|
| `/start` | group/DM | Group: create session. DM: register user, handle deep-link payload |
| `/start_session` | group | Enable session, clear tracking |
| `/add <id> [name]` | group | Add pilot by OGN ID; without args → initiates DM flow |
| `/remove <id>` | group | Remove pilot |
| `/track_on` | group | Start OGN + update goroutines |
| `/track_off` | group | Stop goroutines (with confirm), keep list |
| `/session_reset` | group | Stop all, clear everything (with confirm) |
| `/landing` | group | Set landing location (2 min timeout) |
| `/driver` / `/driver_off` | group | Register/unregister a driver (expects live location) |
| `/area [km]` / `/area_off` | group | Enable/disable area tracking zone |
| `/radar [km]` | group | Show all aircraft in the observation area |
| `/tz <timezone>` | group | Set session timezone (IANA name) |
| `/list` | group | Show tracked pilots |
| `/status` | group | Tracking on/off + count |
| `/help` | group | List commands |
| `/myid` | DM | Show your Telegram/OGN IDs |
| `/confirm` | group | Confirm a pending action |
| `/debug_wipe` | group | Wipe all state (dev only) |

Reply keyboard buttons in groups map to the same exec functions as commands.

## APRS Filter Logic

- Short IDs (≤6 chars) are expanded with all OGN callsign prefixes (`FLR`, `OGN`, `ICA`, `NAV`, `FNT`) for the APRS budlist filter
- Full callsigns (>6 chars) are used verbatim in the budlist
- Auto-discovered pilots (via area zone) are excluded from the budlist
- If a TrackArea is set, a range filter is added alongside the budlist
- Filter is rebuilt and goroutines restarted whenever pilots are added/removed or area changes

## DM Add Flow

When `/add` is sent without an ID in the group, the bot generates a deep link to the bot's DM. The user opens DM, `/start` captures the `add:<chatID>` payload, sets `PendingGroup` on the UserInfo, and the next plain-text DM message is treated as the OGN ID. This lets pilots add themselves without revealing their tracker ID publicly.

## Pilot Status

`PilotStatus` enum: `StatusFlying` → `StatusLanded` → `StatusPickedUp`.

Landing is auto-detected when `GroundSpeed < 5 km/h` AND `|ClimbRate| < 0.3 m/s` for 90 continuous seconds. The "pickup" inline button marks a pilot as `StatusPickedUp`.

## Dependencies

- `ogn` (alias for `github.com/evtaccount/ogn-client`) — APRS client & parser
- `github.com/go-telegram/bot v1.20.0` — Telegram API

## graphify

This project has a graphify knowledge graph at graphify-out/.

Rules:
- Before answering architecture or codebase questions, read graphify-out/GRAPH_REPORT.md for god nodes and community structure
- If graphify-out/wiki/index.md exists, navigate it instead of reading raw files
- After modifying code files in this session, run `graphify update .` to keep the graph current (AST-only, no API cost)

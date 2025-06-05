# Telegram OGN Tracker

Simple Telegram bot written in Go that tracks glider positions from Open Glider Network (OGN).
It supports adding/removing OGN identifiers and toggling tracking. When tracking is enabled,
the bot periodically fetches positions for every configured id and sends (or updates)
a message in a configured chat.

## Usage

1. Install Go and download dependencies
   ```sh
   go mod download
   ```
2. Set environment variables:
   - `TELEGRAM_TOKEN` – your Telegram bot token.
   - `TARGET_CHAT_ID` – chat where updates should be posted.
3. Run the bot
   ```sh
   go run ./...
   ```

Commands inside Telegram:
- `/add <id>` – start tracking given OGN id.
- `/remove <id>` – stop tracking id.
- `/track_on` – enable tracking.
- `/track_off` – disable tracking.
- `/list` – show current state.

Positions are requested from `https://api.glidernet.org/tracker/<id>`; you may
need to adjust this endpoint if the API changes.

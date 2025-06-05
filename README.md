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
2. Set environment variable `TELEGRAM_BOT_TOKEN` with your bot token.
   The target chat will be determined automatically from the first command or can be set using `/set_chat`.
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
- `/chat_id` – display the id of the current chat.
- `/set_chat` – use the current chat for position updates.

Positions are requested from `https://api.glidernet.org/tracker/<id>`; you may
need to adjust this endpoint if the API changes.

## Docker

Build the image and run the bot using docker-compose:
```sh
docker-compose up --build
```

Environment variable `TELEGRAM_BOT_TOKEN` can be placed in a `.env` file or exported before running compose.

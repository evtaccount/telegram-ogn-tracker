# Telegram OGN Tracker

Simple Telegram bot written in Python that tracks glider positions from the
Open Glider Network (OGN). It supports adding/removing OGN identifiers and
toggling tracking. When tracking is enabled the bot periodically fetches
positions for every configured id using
[`python-ogn-client`](https://github.com/glidernet/python-ogn-client) and sends
(or updates) a message in a configured chat.

## Usage

1. Install Python dependencies
   ```sh
   pip install -r requirements.txt
   ```
2. Set environment variable `TELEGRAM_BOT_TOKEN` with your bot token.
   The target chat will be determined automatically from the first command or can be set using `/set_chat`.
3. Run the bot
   ```sh
   python tracker_bot.py
   ```

Commands inside Telegram:
- `/add <id>` – start tracking given OGN id.
- `/remove <id>` – stop tracking id.
- `/track_on` – enable tracking.
- `/track_off` – disable tracking.
- `/list` – show current state.
- `/chat_id` – display the id of the current chat.
- `/set_chat` – use the current chat for position updates.

Positions are obtained from the OGN network via `python-ogn-client` which
connects to an APRS server and streams live data.

## Docker

Build the image and run the bot using docker-compose:
```sh
docker-compose up --build
```

Environment variable `TELEGRAM_BOT_TOKEN` can be placed in a `.env` file or exported before running compose.

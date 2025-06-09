# Telegram OGN Tracker
This repository now includes a basic Go implementation (`main.go`) that performs similar tracking functionality using the [go-ogn-client](https://gitlab.eqipe.ch/sgw/go-ogn-client) library.

Simple Telegram bot written in Python that tracks glider positions from the Open Glider Network (OGN). The chat for updates is configured in the code and cannot be changed at runtime. The bot processes commands only from trusted users.

## Usage

1. Install the dependencies (includes python-telegram-bot with job queue support)
   ```sh
   make install
   ```
2. Create a `.env` file in this directory with your bot token:
   ```
   TELEGRAM_BOT_TOKEN=YOUR_TELEGRAM_TOKEN
   ```
   Set the `TARGET_CHAT_ID` constant in `bot.py` to the chat where updates should be sent.
3. Start the bot
   ```sh
   make run
   ```

Alternatively, build and run the Go version:
```sh
go build -o ogn-go-bot main.go
./ogn-go-bot
```

Commands inside Telegram:
- `/start` – display a welcome message.
- `/add <id>` – start tracking given OGN id.
- `/remove <id>` – stop tracking id.
- `/clear` – remove all tracked ids.
- `/clearid <id>` – remove the specified id.
- `/track_on` – enable tracking.
- `/track_off` – disable tracking.
- `/list` – show current state.
- `/new_session` – start a new tracking session.
- `/add_trusted <user_id>` – add a user to the trusted list (owner only).
- `/list_trusted` – list trusted users (owner only).

When tracking IDs, the bot sends a live location message for each address. The
address and associated user name are posted in a reply to that message. If a new
beacon arrives for the same address, the live location message is updated with
the new coordinates instead of sending a new message.

Positions are requested from `https://api.glidernet.org/tracker/<id>`; you may
need to adjust this endpoint if the API changes.

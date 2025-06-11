# Telegram OGN Tracker
This repository contains a Go implementation (`cmd/bot/main.go`) of a Telegram bot that tracks glider positions from the [Open Glider Network](https://www.glidernet.org/). It uses the [go-ogn-client](https://github.com/evtaccount/ogn-client) library for receiving beacons.

## Usage

1. Create a `.env` file in this directory with your bot token:
   ```
   TELEGRAM_BOT_TOKEN=YOUR_TELEGRAM_TOKEN
   ```
2. Run the bot:
   ```sh
   make run
   ```
   This starts `go run ./cmd/bot`.
3. To build a standalone binary:
    ```sh
    make build-go
    ```

Alternatively you can run the bot with Docker:

```sh
docker-compose up -d
```

The container reads the token from the `.env` file.

The set of available commands depends on the current session state:

1. Before you run `/start` only `/start` and `/help` are available.
2. After `/start` you can also use `/start_session` and `/status`.
3. Running `/start_session` unlocks the full command set: `/add`, `/remove`, `/track_on`, `/list`, `/status`, and `/session_reset`. When tracking is active, `/track_on` is replaced by `/track_off`.
4. Calling `/start_session` again clears all added addresses and restarts the session.

Commands inside Telegram:
 - `/start` – display a welcome message and show how to enable commands.
 - `/start_session` – start or reset the session and unlock commands.
- `/add <id> [name]` – start tracking the given OGN id. The optional name may contain spaces and will appear before your username in location messages.
- `/remove <id>` – stop tracking the id.
- `/landing` – set the default landing location. After sending the command, send a Telegram location within two minutes.
- `/track_on` – enable tracking (replaced by `/track_off` once active).
- `/track_off` – disable tracking and keep addresses.
- `/session_reset` – stop tracking and clear all addresses.
- `/list` – show current tracked ids and state (with the Telegram name of the user who added each).
- `/status` – show current state.
- `/help` – show the list of available commands.

Tracking compares only the last six characters of each callsign. This means you
can add IDs in their short form (e.g. `FE0E4A`) and they will match beacons with
longer prefixes like `ICA3FE0E4A`.
When any ID is added in this short form, APRS server filtering is disabled so
that beacons with longer prefixes still reach the bot.

When tracking IDs, the bot sends a venue message for each address. The venue
title displays the name provided with `/add` followed by the Telegram username
in parentheses. Each time a beacon is received, the previous venue message is
deleted and a new one is sent so the information stays up to date. The text
message below each location still shows when that glider was last seen on the
network and is updated independently for every address.

The bot prints debug logs for every received OGN line and any parse errors to help diagnose missing data.

Positions are requested from `https://api.glidernet.org/tracker/<id>`; you may
need to adjust this endpoint if the API changes.

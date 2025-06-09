# Telegram OGN Tracker
This repository contains a Go implementation (`main.go`) of a Telegram bot that tracks glider positions from the [Open Glider Network](https://www.glidernet.org/). It uses the [go-ogn-client](https://github.com/evtaccount/ogn-client) library for receiving beacons.

## Usage

1. Create a `.env` file in this directory with your bot token:
   ```
   TELEGRAM_BOT_TOKEN=YOUR_TELEGRAM_TOKEN
   ```
2. Run the bot:
   ```sh
   make run
   ```
   This starts `go run main.go`.
3. To build a standalone binary:
    ```sh
    make build-go
    ```

Alternatively you can run the bot with Docker:

```sh
docker-compose up -d
```

The container reads the token from the `.env` file.

Commands inside Telegram (also accessible via the menu button). The keyboard is hidden automatically when you send `/start`:
- `/start` – display a welcome message.
- `/add <id> [name]` – start tracking the given OGN id. If a name is supplied it will appear before your username in the location message.
- `/remove <id>` – stop tracking the id.
- `/track_on` – enable tracking.
- `/track_off` – disable tracking.
- `/list` – show current tracked ids and state (with the Telegram name of the user who added each).
- `/help` – show the list of available commands.

Tracking compares only the last six characters of each callsign. This means you
can add IDs in their short form (e.g. `FE0E4A`) and they will match beacons with
longer prefixes like `ICA3FE0E4A`.

When tracking IDs, the bot sends a separate live location message for every
address. By default the message shows your Telegram username. If you provide a
name with `/add`, that name is shown followed by your username in parentheses.
The text message below each location also shows when that glider was last seen
on the network. Each message is updated independently when a new beacon is
received for that address.

The bot prints debug logs for every received OGN line and any parse errors to help diagnose missing data.

Positions are requested from `https://api.glidernet.org/tracker/<id>`; you may
need to adjust this endpoint if the API changes.

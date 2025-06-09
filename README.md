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

Commands inside Telegram:
- `/start` – display a welcome message.
- `/add <id>` – start tracking the given OGN id.
- `/remove <id>` – stop tracking the id.
- `/track_on` – enable tracking.
- `/track_off` – disable tracking.
- `/list` – show current tracked ids and state.

When tracking IDs, the bot sends a live location message for each address. The
address and associated user name are posted in a reply to that message. If a new
beacon arrives for the same address, the live location message is updated with
the new coordinates instead of sending a new message.

Positions are requested from `https://api.glidernet.org/tracker/<id>`; you may
need to adjust this endpoint if the API changes.

# Telegram OGN Tracker

Simple Telegram bot written in Python that tracks glider positions from the Open Glider Network (OGN). It supports adding and removing OGN identifiers, toggling tracking and setting the chat where updates are posted.

## Usage

1. Install the dependencies (includes python-telegram-bot with job queue support)
   ```sh
   make install
   ```
2. Create a `.env` file in this directory with your bot token:
   ```
   TELEGRAM_BOT_TOKEN=YOUR_TELEGRAM_TOKEN
   ```
   The target chat will be determined automatically from the first command or
   can be set using `/set_chat`.
3. Start the bot
   ```sh
   make run
   ```

Commands inside Telegram:
- `/start` – display a welcome message and set target chat.
- `/add <id>` – start tracking given OGN id.
- `/remove <id>` – stop tracking id.
- `/track_on` – enable tracking.
- `/track_off` – disable tracking.
- `/list` – show current state.
- `/chat_id` – display the id of the current chat.
- `/set_chat` – use the current chat for position updates.

When tracking IDs, the bot sends a live location message for each address. If a
new beacon arrives for the same address, that message is updated with the new
coordinates instead of sending a new message.

Positions are requested from `https://api.glidernet.org/tracker/<id>`; you may
need to adjust this endpoint if the API changes.

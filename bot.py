import os
import logging
import threading
from datetime import datetime
from typing import Dict

from ogn.client import AprsClient
from ogn.parser import parse, AprsParseError
from telegram import Update
from telegram.ext import Updater, CommandHandler, CallbackContext


class TrackerBot:
    def __init__(self, token: str, aprs_user: str = "N0CALL") -> None:
        self.updater = Updater(token, use_context=True)
        self.dispatcher = self.updater.dispatcher

        self.tracking_ids: Dict[str, int] = {}
        self.positions: Dict[str, Dict[str, float]] = {}
        self.tracking_enabled = False
        self.target_chat_id: int | None = None
        self.lock = threading.Lock()

        # Telegram command handlers
        self.dispatcher.add_handler(CommandHandler("add", self.cmd_add))
        self.dispatcher.add_handler(CommandHandler("remove", self.cmd_remove))
        self.dispatcher.add_handler(CommandHandler("track_on", self.cmd_track_on))
        self.dispatcher.add_handler(CommandHandler("track_off", self.cmd_track_off))
        self.dispatcher.add_handler(CommandHandler("list", self.cmd_list))
        self.dispatcher.add_handler(CommandHandler("chat_id", self.cmd_chat_id))
        self.dispatcher.add_handler(CommandHandler("set_chat", self.cmd_set_chat))

        # OGN client
        self.client = AprsClient(aprs_user=aprs_user)
        try:
            self.client.connect()
            logging.info("Connected to OGN")
        except Exception as exc:
            logging.error("Failed to connect to OGN: %s", exc)
        self.client_thread = threading.Thread(target=self.run_client, daemon=True)
        self.client_thread.start()

        # Periodic updates every 30 seconds
        self.updater.job_queue.run_repeating(self.send_updates, interval=30, first=30)

    def run_client(self) -> None:
        try:
            self.client.run(callback=self.process_beacon, autoreconnect=True)
        except Exception as exc:
            logging.error("AprsClient error: %s", exc)
        finally:
            self.client.disconnect()

    def process_beacon(self, raw_message: str) -> None:
        try:
            beacon = parse(raw_message)
            beacon_id = beacon.get("address", "").upper()
            if not beacon_id:
                logging.warning("Beacon without address: %s", raw_message)
                return
            with self.lock:
                if beacon_id in self.tracking_ids:
                    self.positions[beacon_id] = {
                        "lat": beacon.get("latitude"),
                        "lon": beacon.get("longitude"),
                        "timestamp": beacon.get("timestamp")
                    }
                else:
                    logging.debug("Beacon %s filtered out", beacon_id)
        except AprsParseError as exc:
            logging.warning("Failed to parse beacon: %s", exc)
        except Exception as exc:
            logging.error("Error processing beacon: %s", exc)

    def ensure_chat(self, chat_id: int) -> None:
        with self.lock:
            if self.target_chat_id is None:
                self.target_chat_id = chat_id

    def cmd_add(self, update: Update, context: CallbackContext) -> None:
        self.ensure_chat(update.effective_chat.id)
        if not context.args:
            update.message.reply_text("Usage: /add <ogn_id>")
            return
        ogn_id = context.args[0].upper()
        with self.lock:
            if ogn_id in self.tracking_ids:
                update.message.reply_text(f"{ogn_id} already added")
                return
            self.tracking_ids[ogn_id] = 0
        update.message.reply_text(f"Added {ogn_id}")

    def cmd_remove(self, update: Update, context: CallbackContext) -> None:
        if not context.args:
            update.message.reply_text("Usage: /remove <ogn_id>")
            return
        ogn_id = context.args[0].upper()
        with self.lock:
            if ogn_id in self.tracking_ids:
                self.tracking_ids.pop(ogn_id)
                self.positions.pop(ogn_id, None)
                update.message.reply_text(f"Removed {ogn_id}")
            else:
                update.message.reply_text(f"{ogn_id} not found")

    def cmd_track_on(self, update: Update, context: CallbackContext) -> None:
        with self.lock:
            if self.tracking_enabled:
                update.message.reply_text("Tracking already enabled")
                return
            self.tracking_enabled = True
        update.message.reply_text("Tracking enabled")

    def cmd_track_off(self, update: Update, context: CallbackContext) -> None:
        with self.lock:
            if not self.tracking_enabled:
                update.message.reply_text("Tracking already disabled")
                return
            self.tracking_enabled = False
        update.message.reply_text("Tracking disabled")

    def cmd_list(self, update: Update, context: CallbackContext) -> None:
        with self.lock:
            ids = ", ".join(self.tracking_ids.keys()) or "No ids"
            status = "on" if self.tracking_enabled else "off"
        update.message.reply_text(f"Tracking: {status}\nIDs: {ids}")

    def cmd_chat_id(self, update: Update, context: CallbackContext) -> None:
        with self.lock:
            current = self.target_chat_id
        reply = f"Chat ID: {update.effective_chat.id}"
        if current == update.effective_chat.id:
            reply += " (target chat)"
        update.message.reply_text(reply)

    def cmd_set_chat(self, update: Update, context: CallbackContext) -> None:
        with self.lock:
            self.target_chat_id = update.effective_chat.id
        update.message.reply_text(f"Target chat set to {self.target_chat_id}")

    def send_updates(self, context: CallbackContext) -> None:
        with self.lock:
            if not self.tracking_enabled or self.target_chat_id is None:
                return
            ids = list(self.tracking_ids.keys())
            positions = {i: self.positions.get(i) for i in ids}
        for ogn_id, pos in positions.items():
            if not pos:
                continue
            time_str = ""
            if isinstance(pos.get("timestamp"), datetime):
                time_str = pos["timestamp"].strftime("%Y-%m-%dT%H:%M:%SZ")
            text = (
                f"ID: {ogn_id}\n"
                f"Lat: {pos.get('lat')}\n"
                f"Lon: {pos.get('lon')}\n"
                f"Time: {time_str}"
            )
            with self.lock:
                msg_id = self.tracking_ids.get(ogn_id, 0)
            if msg_id:
                try:
                    context.bot.edit_message_text(chat_id=self.target_chat_id, message_id=msg_id, text=text)
                except Exception as exc:
                    logging.error("Failed to edit message for %s: %s", ogn_id, exc)
            else:
                try:
                    msg = context.bot.send_message(chat_id=self.target_chat_id, text=text)
                except Exception as exc:
                    logging.error("Failed to send message for %s: %s", ogn_id, exc)
                else:
                    with self.lock:
                        self.tracking_ids[ogn_id] = msg.message_id

    def run(self) -> None:
        logging.info("Bot started")
        self.updater.start_polling()
        self.updater.idle()


def main() -> None:
    token = os.environ.get("TELEGRAM_BOT_TOKEN")
    if not token:
        raise RuntimeError("TELEGRAM_BOT_TOKEN must be set")
    bot = TrackerBot(token)
    bot.run()


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO)
    main()

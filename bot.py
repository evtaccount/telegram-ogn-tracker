import os
import logging
import threading
from datetime import datetime
from typing import Dict
from dotenv import load_dotenv

from ogn.client import AprsClient
from ogn.parser import parse, AprsParseError
from telegram import Update
from telegram.ext import (
    ApplicationBuilder,
    CommandHandler,
    ContextTypes,
)


class TrackerBot:
    def __init__(self, token: str, aprs_user: str = "N0CALL") -> None:
        self.app = ApplicationBuilder().token(token).build()

        self.tracking_ids: Dict[str, int] = {}
        self.positions: Dict[str, Dict[str, float]] = {}
        self.tracking_enabled = False
        self.target_chat_id: int | None = None
        self.lock = threading.Lock()

        # Telegram command handlers
        self.app.add_handler(CommandHandler("start", self.cmd_start))
        self.app.add_handler(CommandHandler("add", self.cmd_add))
        self.app.add_handler(CommandHandler("remove", self.cmd_remove))
        self.app.add_handler(CommandHandler("track_on", self.cmd_track_on))
        self.app.add_handler(CommandHandler("track_off", self.cmd_track_off))
        self.app.add_handler(CommandHandler("list", self.cmd_list))
        self.app.add_handler(CommandHandler("chat_id", self.cmd_chat_id))
        self.app.add_handler(CommandHandler("set_chat", self.cmd_set_chat))

        # OGN client (started on /track_on)
        self.client = AprsClient(aprs_user=aprs_user)
        self.client_thread: threading.Thread | None = None

        # Periodic updates every 30 seconds
        if self.app.job_queue:
            self.app.job_queue.run_repeating(self.send_updates, interval=30, first=30)
        else:
            logging.warning("JobQueue not available; periodic updates disabled")

    def run_client(self) -> None:
        try:
            self.client.run(callback=self.process_beacon, autoreconnect=True)
        except Exception as exc:
            logging.error("AprsClient error: %s", exc)
        finally:
            self.client.disconnect()

    def start_client(self) -> None:
        if self.client_thread and self.client_thread.is_alive():
            return
        try:
            self.client.connect()
            logging.info("Connected to OGN")
        except Exception as exc:
            logging.error("Failed to connect to OGN: %s", exc)
            return
        self.client_thread = threading.Thread(target=self.run_client, daemon=True)
        self.client_thread.start()

    def stop_client(self) -> None:
        if self.client_thread and self.client_thread.is_alive():
            self.client.disconnect()
            self.client_thread.join(timeout=2)
        self.client_thread = None

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
                        "timestamp": beacon.get("timestamp"),
                        "name": beacon.get("name"),
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

    async def cmd_start(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        """Handle /start by setting the chat and showing basic help."""
        self.ensure_chat(update.effective_chat.id)
        await update.message.reply_text(
            "OGN tracker bot ready. Use /add <id> to track gliders."
        )

    async def cmd_add(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        self.ensure_chat(update.effective_chat.id)
        if not context.args:
            await update.message.reply_text("Usage: /add <ogn_id>")
            return
        ogn_id = context.args[0].upper()
        with self.lock:
            if ogn_id in self.tracking_ids:
                await update.message.reply_text(f"{ogn_id} already added")
                return
            self.tracking_ids[ogn_id] = 0
        await update.message.reply_text(f"Added {ogn_id}")

    async def cmd_remove(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        if not context.args:
            await update.message.reply_text("Usage: /remove <ogn_id>")
            return
        ogn_id = context.args[0].upper()
        with self.lock:
            if ogn_id in self.tracking_ids:
                self.tracking_ids.pop(ogn_id)
                self.positions.pop(ogn_id, None)
                await update.message.reply_text(f"Removed {ogn_id}")
            else:
                await update.message.reply_text(f"{ogn_id} not found")

    async def cmd_track_on(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        with self.lock:
            if self.tracking_enabled:
                await update.message.reply_text("Tracking already enabled")
                return
            self.tracking_enabled = True
        self.start_client()
        await update.message.reply_text("Tracking enabled")

    async def cmd_track_off(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        with self.lock:
            if not self.tracking_enabled:
                await update.message.reply_text("Tracking already disabled")
                return
            self.tracking_enabled = False
        self.stop_client()
        await update.message.reply_text("Tracking disabled")

    async def cmd_list(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        with self.lock:
            ids = ", ".join(self.tracking_ids.keys()) or "No ids"
            status = "on" if self.tracking_enabled else "off"
        await update.message.reply_text(f"Tracking: {status}\nIDs: {ids}")

    async def cmd_chat_id(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        with self.lock:
            current = self.target_chat_id
        reply = f"Chat ID: {update.effective_chat.id}"
        if current == update.effective_chat.id:
            reply += " (target chat)"
        await update.message.reply_text(reply)

    async def cmd_set_chat(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        with self.lock:
            self.target_chat_id = update.effective_chat.id
        await update.message.reply_text(f"Target chat set to {self.target_chat_id}")

    async def send_updates(self, context: ContextTypes.DEFAULT_TYPE) -> None:
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
            with self.lock:
                msg_id = self.tracking_ids.get(ogn_id, 0)
            if msg_id:
                try:
                    await context.bot.edit_message_live_location(
                        chat_id=self.target_chat_id,
                        message_id=msg_id,
                        latitude=pos.get("lat"),
                        longitude=pos.get("lon"),
                    )
                except Exception as exc:
                    logging.error("Failed to edit location for %s: %s", ogn_id, exc)
            else:
                try:
                    msg = await context.bot.send_location(
                        chat_id=self.target_chat_id,
                        latitude=pos.get("lat"),
                        longitude=pos.get("lon"),
                        live_period=86400,
                    )
                    text = f"Address: {ogn_id}"
                    if pos.get("name"):
                        text += f"\nUser: {pos['name']}"
                    await context.bot.send_message(
                        chat_id=self.target_chat_id,
                        text=text,
                        reply_to_message_id=msg.message_id,
                    )
                except Exception as exc:
                    logging.error("Failed to send location for %s: %s", ogn_id, exc)
                else:
                    with self.lock:
                        self.tracking_ids[ogn_id] = msg.message_id

    def run(self) -> None:
        logging.info("Bot started")
        self.app.run_polling()


def main() -> None:
    load_dotenv()
    token = os.environ.get("TELEGRAM_BOT_TOKEN")
    if not token:
        raise RuntimeError("TELEGRAM_BOT_TOKEN must be set")
    bot = TrackerBot(token)
    bot.run()


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO)
    main()

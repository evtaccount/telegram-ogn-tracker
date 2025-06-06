import os
import logging
import threading
from datetime import datetime
from typing import Dict, Set

OWNER_ID = 182255461
TARGET_CHAT_ID = 0  # replace with your chat id
TRUSTED_USERS_FILE = "trusted_users.txt"
from dotenv import load_dotenv

from ogn.client import AprsClient
from ogn.parser import parse, AprsParseError
from telegram import Update, InlineKeyboardButton, InlineKeyboardMarkup
from telegram.ext import (
    ApplicationBuilder,
    CommandHandler,
    ContextTypes,
    CallbackQueryHandler,
)


class TrackerBot:
    def __init__(self, token: str, aprs_user: str = "N0CALL") -> None:
        self.app = ApplicationBuilder().token(token).build()

        self.tracking_ids: Dict[str, int] = {}
        self.positions: Dict[str, Dict[str, float]] = {}
        self.tracking_enabled = False
        self.target_chat_id: int = TARGET_CHAT_ID
        self.lock = threading.Lock()
        self.session_started = False
        self.trusted_users: Set[int] = set()
        self.load_trusted_users()

        # Telegram command handlers
        self.app.add_handler(CommandHandler("start", self.cmd_start))
        self.app.add_handler(CommandHandler("add", self.cmd_add))
        self.app.add_handler(CommandHandler("remove", self.cmd_remove))
        self.app.add_handler(CommandHandler("track_on", self.cmd_track_on))
        self.app.add_handler(CommandHandler("track_off", self.cmd_track_off))
        self.app.add_handler(CommandHandler("list", self.cmd_list))
        self.app.add_handler(CommandHandler("clear", self.cmd_clear))
        self.app.add_handler(CommandHandler("clearid", self.cmd_clear_id))
        self.app.add_handler(CommandHandler("new_session", self.cmd_new_session))
        self.app.add_handler(CommandHandler("add_trusted", self.cmd_add_trusted))
        self.app.add_handler(CommandHandler("list_trusted", self.cmd_list_trusted))
        self.app.add_handler(CallbackQueryHandler(self.handle_query))

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

    def load_trusted_users(self) -> None:
        """Load trusted user ids from file."""
        if os.path.exists(TRUSTED_USERS_FILE):
            with open(TRUSTED_USERS_FILE, "r", encoding="utf-8") as fh:
                for line in fh:
                    line = line.strip()
                    if line:
                        try:
                            self.trusted_users.add(int(line))
                        except ValueError:
                            logging.warning("Invalid user id in trusted file: %s", line)
        else:
            self.trusted_users.add(OWNER_ID)
            self.save_trusted_users()

    def save_trusted_users(self) -> None:
        with open(TRUSTED_USERS_FILE, "w", encoding="utf-8") as fh:
            for uid in sorted(self.trusted_users):
                fh.write(f"{uid}\n")

    def is_trusted(self, update: Update) -> bool:
        user = update.effective_user
        if not user or user.id not in self.trusted_users:
            return False
        return True

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
        except AprsParseError as exc:
            logging.warning("Failed to parse beacon: %s", exc)
        except Exception as exc:
            logging.error("Error processing beacon: %s", exc)


    async def cmd_start(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        """Handle /start by setting the chat and showing basic help."""
        if not self.is_trusted(update):
            return
        await update.message.reply_text(
            "OGN tracker bot ready. Use /add <id> to track gliders."
        )

    async def cmd_add(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        if not self.is_trusted(update):
            return
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
        if not self.is_trusted(update):
            return
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
        if not self.is_trusted(update):
            return
        with self.lock:
            if self.tracking_enabled:
                await update.message.reply_text("Tracking already enabled")
                return
            self.tracking_enabled = True
        self.start_client()
        await update.message.reply_text("Tracking enabled")

    async def cmd_track_off(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        if not self.is_trusted(update):
            return
        with self.lock:
            if not self.tracking_enabled:
                await update.message.reply_text("Tracking already disabled")
                return
            self.tracking_enabled = False
        self.stop_client()
        await update.message.reply_text("Tracking disabled")

    async def cmd_list(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        if not self.is_trusted(update):
            return
        with self.lock:
            ids = ", ".join(self.tracking_ids.keys()) or "No ids"
            status = "on" if self.tracking_enabled else "off"
        await update.message.reply_text(f"Tracking: {status}\nIDs: {ids}")

    async def cmd_clear(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        """Clear all tracked OGN ids."""
        if not self.is_trusted(update):
            return
        with self.lock:
            self.tracking_ids.clear()
            self.positions.clear()
        await update.message.reply_text("All IDs cleared")

    async def cmd_clear_id(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        """Remove the specified OGN id."""
        if not self.is_trusted(update):
            return
        if not context.args:
            await update.message.reply_text("Usage: /clearid <ogn_id>")
            return
        ogn_id = context.args[0].upper()
        with self.lock:
            removed = ogn_id in self.tracking_ids
            self.tracking_ids.pop(ogn_id, None)
            self.positions.pop(ogn_id, None)
        if removed:
            await update.message.reply_text(f"Cleared {ogn_id}")
        else:
            await update.message.reply_text(f"{ogn_id} not found")

    async def cmd_new_session(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        if not self.is_trusted(update):
            return
        with self.lock:
            if self.session_started:
                buttons = [
                    [
                        InlineKeyboardButton("Начать", callback_data="new_session_start"),
                        InlineKeyboardButton("Отмена", callback_data="new_session_cancel"),
                    ]
                ]
                await update.message.reply_text(
                    "Начать новую сессию?",
                    reply_markup=InlineKeyboardMarkup(buttons),
                )
                return
            self.session_started = True
        await update.message.reply_text("Новая сессия начата")
        await update.message.reply_text("Добавьте адреса командой /add <id>")

    async def handle_query(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        query = update.callback_query
        if not query:
            return
        if not self.is_trusted(update):
            return
        await query.answer()
        data = query.data
        if data == "new_session_start":
            await query.edit_message_reply_markup(reply_markup=None)
            with self.lock:
                ids_list = ", ".join(self.tracking_ids.keys()) or "нет"
            buttons = [
                [
                    InlineKeyboardButton("Оставить старые", callback_data="keep_old"),
                    InlineKeyboardButton("Сбросить", callback_data="reset_old"),
                ]
            ]
            await query.message.reply_text(
                f"Текущие адреса: {ids_list}\nОставить старые или сбросить?",
                reply_markup=InlineKeyboardMarkup(buttons),
            )
        elif data == "new_session_cancel":
            await query.edit_message_reply_markup(reply_markup=None)
        elif data in {"keep_old", "reset_old"}:
            await query.edit_message_reply_markup(reply_markup=None)
            with self.lock:
                self.session_started = True
                self.tracking_enabled = False
                self.stop_client()
                if data == "reset_old":
                    self.tracking_ids.clear()
                    self.positions.clear()
            await query.message.reply_text("Новая сессия начата")
            if data == "reset_old":
                await query.message.reply_text("Добавьте адреса командой /add <id>")

    async def cmd_add_trusted(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        if update.effective_user.id != OWNER_ID:
            return
        if not context.args:
            await update.message.reply_text("Usage: /add_trusted <user_id>")
            return
        try:
            user_id = int(context.args[0])
        except ValueError:
            await update.message.reply_text("Invalid user id")
            return
        with self.lock:
            self.trusted_users.add(user_id)
            self.save_trusted_users()
        await update.message.reply_text(f"User {user_id} added to trusted list")

    async def cmd_list_trusted(self, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
        if update.effective_user.id != OWNER_ID:
            return
        with self.lock:
            ids = ", ".join(str(u) for u in sorted(self.trusted_users)) or "none"
        await update.message.reply_text(f"Trusted users: {ids}")

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

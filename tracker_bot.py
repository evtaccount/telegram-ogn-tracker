import logging
import os
import time

from telegram import Update
from telegram.ext import (ApplicationBuilder, CommandHandler,
                          ContextTypes)

# Library for accessing OGN data
from ogn.client import OGNClient


class TrackerBot:
    def __init__(self, token: str):
        self.app = ApplicationBuilder().token(token).build()
        self.client = OGNClient()
        self.ids: dict[str, int] = {}
        self.tracking_enabled = False
        self.target_chat_id: int | None = None
        self.positions = {}

        self.app.add_handler(CommandHandler('add', self.cmd_add))
        self.app.add_handler(CommandHandler('remove', self.cmd_remove))
        self.app.add_handler(CommandHandler('track_on', self.cmd_track_on))
        self.app.add_handler(CommandHandler('track_off', self.cmd_track_off))
        self.app.add_handler(CommandHandler('list', self.cmd_list))
        self.app.add_handler(CommandHandler('chat_id', self.cmd_chat_id))
        self.app.add_handler(CommandHandler('set_chat', self.cmd_set_chat))
        self.app.job_queue.run_repeating(self.update_positions, 30)

        # subscribe to incoming OGN messages
        self.client.add_listener(self.ogn_message_handler)
        self.client.start()

    def ogn_message_handler(self, msg):
        """Store the last known position for subscribed IDs."""
        if msg.address in self.ids:
            self.positions[msg.address] = msg

    async def cmd_add(self, update: Update, context: ContextTypes.DEFAULT_TYPE):
        if not context.args:
            await update.message.reply_text('Usage: /add <ogn_id>')
            return
        ogn_id = context.args[0]
        if ogn_id in self.ids:
            await update.message.reply_text(f'{ogn_id} already added')
            return
        self.ids[ogn_id] = 0
        await update.message.reply_text(f'Added {ogn_id}')

    async def cmd_remove(self, update: Update, context: ContextTypes.DEFAULT_TYPE):
        if not context.args:
            await update.message.reply_text('Usage: /remove <ogn_id>')
            return
        ogn_id = context.args[0]
        if ogn_id in self.ids:
            del self.ids[ogn_id]
            await update.message.reply_text(f'Removed {ogn_id}')
        else:
            await update.message.reply_text(f'{ogn_id} not found')

    async def cmd_track_on(self, update: Update, context: ContextTypes.DEFAULT_TYPE):
        if self.tracking_enabled:
            await update.message.reply_text('Tracking already enabled')
            return
        self.tracking_enabled = True
        await update.message.reply_text('Tracking enabled')

    async def cmd_track_off(self, update: Update, context: ContextTypes.DEFAULT_TYPE):
        if not self.tracking_enabled:
            await update.message.reply_text('Tracking already disabled')
            return
        self.tracking_enabled = False
        await update.message.reply_text('Tracking disabled')

    async def cmd_list(self, update: Update, context: ContextTypes.DEFAULT_TYPE):
        ids = ', '.join(self.ids.keys()) if self.ids else 'No ids'
        status = 'on' if self.tracking_enabled else 'off'
        await update.message.reply_text(f'Tracking: {status}\nIDs: {ids}')

    async def cmd_chat_id(self, update: Update, context: ContextTypes.DEFAULT_TYPE):
        current = self.target_chat_id
        reply = f'Chat ID: {update.message.chat_id}'
        if current == update.message.chat_id:
            reply += ' (target chat)'
        await update.message.reply_text(reply)

    async def cmd_set_chat(self, update: Update, context: ContextTypes.DEFAULT_TYPE):
        self.target_chat_id = update.message.chat_id
        await update.message.reply_text(f'Target chat set to {self.target_chat_id}')

    async def update_positions(self, context: ContextTypes.DEFAULT_TYPE):
        if not self.tracking_enabled or not self.ids or self.target_chat_id is None:
            return
        for ogn_id in list(self.ids.keys()):
            msg = self.positions.get(ogn_id)
            if not msg:
                continue
            text = (
                f'ID: {ogn_id}\n'
                f'Lat: {msg.latitude:.6f}\n'
                f'Lon: {msg.longitude:.6f}\n'
                f'Time: {time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(msg.timestamp))}'
            )
            msg_id = self.ids[ogn_id]
            if msg_id:
                await context.bot.edit_message_text(text, self.target_chat_id, msg_id)
            else:
                sent = await context.bot.send_message(self.target_chat_id, text)
                self.ids[ogn_id] = sent.message_id

    def run(self):
        self.app.run_polling()


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    token = os.getenv('TELEGRAM_BOT_TOKEN')
    if not token:
        raise SystemExit('TELEGRAM_BOT_TOKEN must be set')
    TrackerBot(token).run()


if __name__ == '__main__':
    main()

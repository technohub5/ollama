import telebot
import os
from yt_dlp import YoutubeDL

# Bot API token
API_TOKEN = 'YOUR_BOT_API_TOKEN'
bot = telebot.TeleBot('7223183103:AAHmLhs3rdoVmN_1kvTYEJ3J3xZVkanBHTA')

# Dictionary to store user quality preferences
user_quality = {}

@bot.message_handler(commands=['start'])
def send_welcome(message):
    bot.reply_to(
        message,
        "Hi! 👋 Send me a YouTube link to download the video.\n\n"
        "You can also choose the quality by sending /quality first."
    )

@bot.message_handler(commands=['quality'])
def set_quality(message):
    bot.reply_to(
        message,
        "Choose the quality:\n"
        "1️⃣ High (best)\n"
        "2️⃣ Medium (480p)\n"
        "3️⃣ Low (worst)\n\n"
        "Reply with the number (1, 2, or 3) to set your choice."
    )

@bot.message_handler(func=lambda message: message.text in ['1', '2', '3'])
def save_quality(message):
    quality = message.text
    user_quality[message.chat.id] = quality
    quality_text = "High" if quality == '1' else "Medium" if quality == '2' else "Low"
    bot.reply_to(message, f"✅ Quality set to {quality_text}.")

@bot.message_handler(func=lambda message: message.text.startswith("http"))
def download_video(message):
    url = message.text
    quality = user_quality.get(message.chat.id, '1')  # Default to 'High' if not set
    bot.reply_to(message, "⏳ Downloading video, please wait...")

    # Set format options based on user-selected quality
    format_option = 'best' if quality == '1' else \
                    'bestvideo[height<=480]+bestaudio' if quality == '2' else \
                    'worst'

    try:
        ydl_opts = {
            'outtmpl': 'video.mp4',
            'format': format_option,
            'socket_timeout': 60,
            'overwrites': True  # Overwrite if file exists
        }
        with YoutubeDL(ydl_opts) as ydl:
            ydl.download([url])
        with open('video.mp4', 'rb') as video:
            bot.send_video(message.chat.id, video)
        os.remove('video.mp4')  # Cleanup after sending the file
    except Exception as e:
        bot.reply_to(message, f"❌ Error: {e}")

bot.polling()
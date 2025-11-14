# VoiceActivityBot

A Discord bot that monitors voice channels and notifies when server members join or leave.

## Features

- 🔊 Monitor voice channel activity (joins/leaves)
- 📢 Send notifications to subscribed text channels
- ⚙️ Configurable via `/subscribe` and `/unsubscribe` commands
- 🎯 Support for multiple subscriptions per voice channel
- ⏱️ Debounced notifications to prevent spam from quick channel hopping

## Setup

### Prerequisites

- Go 1.25 or higher
- A Discord Bot Token

### Installation

1. Clone the repository:
```bash
git clone https://github.com/CS-5/VoiceActivityBot.git
cd VoiceActivityBot
```

2. Build the bot:
```bash
go build -o VoiceActivityBot .
```

3. Create a Discord application and bot:
   - Go to [Discord Developer Portal](https://discord.com/developers/applications)
   - Create a new application
   - Go to the "Bot" section and create a bot
   - Copy the bot token
   - Enable the following Privileged Gateway Intents:
     - Server Members Intent
     - Message Content Intent (if needed)

4. Invite the bot to your server:
   - Go to OAuth2 → URL Generator
   - Select scopes: `bot` and `applications.commands`
   - Select bot permissions: `Send Messages`, `Read Messages/View Channels`
   - Copy and visit the generated URL

### Running

Set your Discord bot token as an environment variable and run:

```bash
export DISCORD_TOKEN="your-bot-token-here"
./VoiceActivityBot
```

Or in one line:
```bash
DISCORD_TOKEN="your-bot-token-here" ./VoiceActivityBot
```

### Configuration

Optional environment variables:

- `DISCORD_TOKEN` (required): Your Discord bot token
- `DEBOUNCE_INTERVAL` (optional): Time to wait before sending notifications (default: `3s`)
  - Format: Go duration string (e.g., `5s`, `500ms`, `1m`)
  - Example: `DEBOUNCE_INTERVAL=5s ./VoiceActivityBot`

## Usage

### Subscribe to Voice Channel Notifications

Use the `/subscribe` command in any text channel to start receiving notifications:

#### With a specific channel:
```
/subscribe voice-channel: <voice-channel-name>
```

#### Without arguments:
```
/subscribe
```
This will show a select menu to choose a voice channel.

### Unsubscribe from Voice Channel Notifications

Use the `/unsubscribe` command to stop receiving notifications:

#### With a specific channel:
```
/unsubscribe voice-channel: <voice-channel-name>
```

#### Without arguments:
```
/unsubscribe
```
- If there's only one active subscription in the current text channel, it will automatically unsubscribe
- If there are multiple subscriptions, a select menu will appear to choose which one to unsubscribe from

### How it works

1. Run `/subscribe` in a text channel
2. Select or specify the voice channel you want to monitor
3. The bot will send notifications to that text channel whenever someone:
   - Joins the monitored voice channel
   - Leaves the monitored voice channel
   - Moves to/from the monitored voice channel

Notifications are debounced (3 seconds by default) to prevent spam when users quickly hop between channels.

### Example Notifications

- 🔊 **Username** joined **General Voice**
- 🔇 **Username** left **General Voice**

## Technical Details

- Written in Go
- Uses [discordgo](https://github.com/bwmarrin/discordgo) library
- Stores subscriptions in-memory (resets on bot restart)
- Supports multiple text channels subscribing to the same voice channel
- Implements notification debouncing to reduce message spam

## License

See [LICENSE](LICENSE) file for details.
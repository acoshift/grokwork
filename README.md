# grok-discord

Tag **@Grok** in Discord; a Go bot on **your machine** runs [Grok Build](https://x.ai) headless against local project code. Your team can investigate bugs and propose fixes without sitting at the desk.

```
Discord  @Grok fix payment timeout
    → bot on your machine
    → grok -p "..." --cwd /path/to/project --yolo
    → reply in a Discord thread (session resumes on follow-up)
```

## Prerequisites

- Go 1.26.5+
- `grok` installed and signed in (`grok login` or `XAI_API_KEY`)
- A Discord server you can add bots to
- This process running while the team uses it

## 1. Create the Discord bot

1. Open [Discord Developer Portal](https://discord.com/developers/applications) → **New Application**.
2. **Bot** → **Add Bot** → copy token.
3. Under **Privileged Gateway Intents**, enable **Message Content Intent** (required). Leave Server Members / Presence off unless you change the code.
4. **OAuth2 → URL Generator**: scope `bot`; permissions: Send Messages, Create Public Threads, Send Messages in Threads, Read Message History.
5. Invite the bot to your server.

If you see `websocket: close 4014: Disallowed intent(s)`, the bot is requesting a privileged intent that is still off in the portal — turn **Message Content Intent** on and restart.

## 2. Configure

```bash
git clone https://github.com/acoshift/grok-discord.git
cd grok-discord
cp config.example.json config.json
# edit token, user IDs, project paths, channel map
```

| Field | Purpose |
|--------|---------|
| `discordToken` | Bot token (or `DISCORD_BOT_TOKEN` env) |
| `allowedUserIds` | Who may invoke Grok (fail-closed if empty **and** no roles) |
| `allowedRoleIds` | Optional role allowlist |
| `projects` | Name → **absolute** path on this machine |
| `channels` | Discord channel ID → project name (**required**; only way to select a project) |
| `yolo` | Auto-approve Grok tools (needed for unattended fix/investigate) |
| `summarizeThreadTitle` | Call Grok once to name the Discord thread before work (default true) |
| `summarizeTimeoutMs` | Timeout for the title summary call (default 45000) |

`config.json` is gitignored. Never commit tokens, user IDs, or private project paths.

## 3. Run

```bash
go run .
# or
go build -o grok-discord .
./grok-discord
```

### Docker

```bash
docker build -t grok-discord .
```

The image only runs the Discord bridge. Mount `grok`, auth, config, and project trees at runtime:

```bash
docker run --rm \
  -v "$PWD/config.json:/config/config.json:ro" \
  -v "$HOME/.grok:/home/nonroot/.grok" \
  -v /path/to/your/code:/path/to/your/code \
  -v "$(which grok):/usr/local/bin/grok:ro" \
  -e GROK_DISCORD_CONFIG=/config/config.json \
  -e HOME=/home/nonroot \
  grok-discord
```

Project paths in `config.json` must match paths **inside** the container. Set `"grokBin": "/usr/local/bin/grok"` if needed. For day-to-day use on a laptop, the host binary is simpler than Docker.

### Smoke test

In a channel listed under `channels` in config:

```
@Grok /help
@Grok /projects
@Grok summarize the repo layout in 5 bullets
```

Real task (uses that channel’s project only):

```
@Grok investigate the flaky test; don't change code yet
```

Continue in the **same thread**:

```
@Grok ok, now propose a minimal fix
```

## Team usage

Project is chosen **only** from `channels` config (parent channel when inside a thread). Users cannot switch projects in chat.

| Message | Effect |
|---------|--------|
| `@Grok <task>` | Run against this channel’s project |
| `@Grok <follow-up>` in thread | Resume session (same project) |
| `@Grok /projects` | Show this channel’s mapped project |
| `@Grok /reset` | Drop session for this thread |
| `@Grok /status` | Show project + session id |

## Security

- Allowlist users/roles. Empty both lists → everyone is denied.
- Prefer a private Discord server/channels.
- `yolo: true` lets Grok edit files and run commands under project cwd. Review diffs.
- Keep `config.json` local only (gitignored).

## Keep running (macOS launchd)

Adjust paths to where you cloned/built the binary. Example `~/Library/LaunchAgents/com.example.grok-discord.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.example.grok-discord</string>
  <key>ProgramArguments</key>
  <array>
    <string>/path/to/grok-discord/grok-discord</string>
  </array>
  <key>WorkingDirectory</key>
  <string>/path/to/grok-discord</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/path/to/grok-discord/data/stdout.log</string>
  <key>StandardErrorPath</key>
  <string>/path/to/grok-discord/data/stderr.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/local/bin:/usr/bin:/bin</string>
  </dict>
</dict>
</plist>
```

```bash
go build -o grok-discord .
mkdir -p data
launchctl load ~/Library/LaunchAgents/com.example.grok-discord.plist
```

## Env vars

| Variable | Purpose |
|----------|---------|
| `DISCORD_BOT_TOKEN` | Override token |
| `GROK_DISCORD_CONFIG` | Path to config.json |
| `GROK_DISCORD_DEBUG` | Post grok stderr into the thread |
| `XAI_API_KEY` | Auth for headless grok if not logged in |

## Layout

```
main.go
internal/config/       # config.json loader
internal/bot/          # Discord handlers, prompt parsing
internal/grokrun/      # exec grok -p
internal/sessionstore/ # thread → session persistence
config.example.json
```

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
| `worktreeIsolation` | Per-thread git worktree under `data/worktrees/` (default true; non-git projects use main cwd) |

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
| `@Grok <follow-up>` while busy | Queue the follow-up (up to 5); runs after the current task |
| `@Grok /projects` | Show this channel’s mapped project |
| `@Grok /reset` | Drop session + remove this thread’s git worktree |
| `@Grok /status` | Show project, session, worktree branch, and queue depth |
| `@Grok /cancel` | Stop the in-progress run (queued follow-ups still run) |
| `@Grok <task>` + attachments | Download files for Grok to read (logs, screenshots, patches) |
| Reply to a message with `@Grok <task>` | Include the referenced message text + attachments (e.g. image, then ask Grok) |

While a task is running, the bot updates the status message every few seconds with elapsed time (and a short thought/tool activity snippet when available). Assistant text streams into the thread via Grok’s `streaming-json` output: a live message shows the **latest** text (tail window), Discord edits run asynchronously so they never block reading Grok’s stdout, and when a reply outgrows one Discord message the bot seals that message and continues in a new one (finish does not re-post sealed chunks). Typing is pulsed while streaming. Use `/cancel` (or `/stop`) in that thread to kill the Grok process (the live stream is finalized without a stuck “streaming…” footer). Follow-ups sent while a run is active are queued in order (max 5) and start automatically when the current run finishes; the bot replies with `Queued (#N)`.

**Worktrees:** when `worktreeIsolation` is on (default) and the project is a git repo, each Discord thread gets its own worktree at `data/worktrees/<project>/<threadId>` on branch `grok/discord/<threadId>`, created from the main checkout’s `HEAD`. Grok runs with `--cwd` set to that worktree so concurrent threads do not share a working tree. `/reset` removes the worktree and deletes the branch. If the branch’s PR is already **merged** or **closed**, the next task in that thread automatically removes the worktree/branch (and session) and starts a fresh worktree from `HEAD`. Set `"worktreeIsolation": false` to always use the main project path.

**Pull requests:** Discord runs are remote, so Grok is instructed to never leave changes as local-only commits. When it makes code changes it should commit on the thread branch (or a feature branch), `git push`, and open/update a PR with `gh pr create`, then include the PR URL in the reply. Requires `gh` auth on the host (`gh auth login` or `GH_TOKEN`) and push access to the project remotes.

**Attachments:** files on the `@Grok` message are downloaded under `data/attachments/<messageId>/`, absolute paths are added to the prompt, and the directory is deleted when the run finishes. Limits: 10 files, 25 MiB each, 50 MiB total. A mention with only attachments (no text) still starts a task.

**Replies:** if you **reply** to another Discord message when tagging Grok (e.g. someone posts a screenshot, then you reply `@Grok what's wrong?`), the bot includes that referenced message’s text and downloads its attachments as well. A bare `@Grok` reply (no extra text) still starts a review task.

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
internal/gitworktree/  # per-thread git worktree isolation
internal/sessionstore/ # thread → session persistence
config.example.json
```

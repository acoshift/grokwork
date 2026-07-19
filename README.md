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
4. **OAuth2 → URL Generator**: scope `bot`; permissions: Send Messages, Create Public Threads, Send Messages in Threads, Read Message History, Manage Messages (needed to pin the continuity brief card).
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
| `discordClientId` | Optional application/client ID for the install URL (decoded from the token when empty) |
| `allowedUserIds` | Who may invoke Grok (fail-closed if empty **and** no roles) |
| `allowedRoleIds` | Optional role allowlist |
| `projects` | Name → **absolute** path on this machine |
| `channels` | Discord channel ID → project name (**required**; only way to select a project) |
| `yolo` | Auto-approve Grok tools (needed for unattended fix/investigate) |
| `summarizeThreadTitle` | Call Grok once to name the Discord thread before work (default true) |
| `summarizeTimeoutMs` | Timeout for the title summary call (default 45000) |
| `worktreeIsolation` | Per-thread git worktree under `data/worktrees/` (default true; non-git projects use main cwd) |
| `worktreeIdleTTLDays` | Days of inactivity before pruning idle worktrees (default `30`; `0` disables). Editable on the Config page |
| `boardStaleDays` | Days without session activity before `/board` lists a thread as **stale** (default `3`). Editable on the Config page |
| `boardDigestChannel` | Optional Discord channel ID for a nightly team board post (empty = disabled). Editable on the Config page |
| `httpListen` | Private-network web UI bind address (default `:8787`; override with `GROK_DISCORD_HTTP_LISTEN`) |

`config.json` is gitignored. Never commit tokens, user IDs, or private project paths.

### Web UI (private network / Tailscale)

While the process runs it also serves a small server-rendered admin UI (hime + `html/template` + stdlib SSE):

| Path | View |
|------|------|
| `/` | Dashboard — live active runs / session counts (SSE refresh) |
| `/ship` | Ship board — all bot-tracked PRs per project, CI/review status, copyable lead digest |
| `/history` | Thread list; open a thread to read each user/Grok turn |
| `/worktrees` | List per-thread git worktrees; prune one or all past idle TTL |
| `/config` | Add/remove projects, channel→project map, allowed users/roles, worktree idle TTL, team board digest, CI auto-fix, completion risk globs |

Bind for Tailscale or LAN with `"httpListen": "0.0.0.0:8787"` (or a Tailscale IP). There is **no auth** on this UI — only expose it on a private network or VPN.

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
| `@Grok /status` | Show owner, project, session, lifecycle label, worktree branch, PR, and queue depth |
| `@Grok /brief` | Pin/update the continuity card (goal, label, done/left, branch, PR, key files) |
| `@Grok /brief goal <text>` | Set the sticky goal, then refresh the brief card |
| `@Grok /label` | Show lifecycle label; `/label <state>` sets manual; `/label auto` re-enables auto |
| `@Grok /board [project] [running\|queued\|waiting\|stale\|label\|all]` | Team activity board (running, queued, waiting on human, stale) |
| `@Grok /claim` | Take ownership of this thread |
| `@Grok /hand-off @user` | Transfer ownership and post a short hand-off card |
| `@Grok /reset` | Drop session + remove this thread’s git worktree (owner/mod) |
| `@Grok /cancel` | Stop the in-progress run (owner/mod; queued follow-ups still run) |
| `@Grok /fix-ci` | Fetch failing CI checks for this thread’s PR and queue a minimal fix |
| `@Grok <task>` + attachments | Download files for Grok to read (logs, screenshots, patches) |
| Reply to a message with `@Grok <task>` | Include the referenced message text + attachments (e.g. image, then ask Grok) |

**Thread ownership:** the first `@Grok` author on a thread becomes **owner** (stored on the session). Anyone on the allowlist may still queue tasks (soft open). `@Grok /cancel` and `@Grok /reset` require the owner, a co-owner, or a Discord moderator (Administrator, Manage Messages, or Manage Threads). `@Grok /claim` takes primary ownership (previous owner stays as co-owner). `@Grok /hand-off @user` transfers ownership and posts a short card (goal, status, PR, queue). Unowned legacy sessions stay open for cancel/reset until someone claims or the next task sets an owner.

**Continuity brief:** each thread keeps **one editable (and preferably pinned) brief card** with sticky goal, recent done turns, what’s left (queue/CI/PR), branch, PR links, key changed files, and open questions scraped from the last assistant reply. It refreshes after each non-cancelled run, on `@Grok /hand-off`, and on `@Grok /brief`. Goal defaults to the first task prompt; override with `@Grok /brief goal <text>`. Pinning needs **Manage Messages** for the bot (card still updates without pin).

**Lifecycle labels:** each thread has a label `open → in_progress → blocked → needs_review → done | abandoned` (empty = open). Auto: first task → `in_progress`; ready (non-draft) open PR → `needs_review`; all PRs merged → `done`; all closed without merge → `abandoned`. Draft-only PRs stay `in_progress`. `@Grok /label blocked` (etc.) sets a **manual** label and pauses auto until `@Grok /label auto` (merge/close still force terminal labels). Shown on `/status`, brief, and hand-off.

**Team activity board:** `@Grok /board` lists non-terminal threads grouped by **activity**: **running** (active Grok job), **queued** (follow-ups waiting), **waiting on human** (blocked / needs review / changes requested / CI failing), **stale** (no session activity for `boardStaleDays`, default 3), and **active** (everything else). Filter with `/board waiting`, `/board stale`, `/board <project>`, lifecycle label (`/board needs_review`), or `/board all` (includes done/abandoned). Optional nightly digest posts the same card to `boardDigestChannel` (Config page or `config.json`).

**Issue / ticket binding:** tasks that mention `#42`, `owner/repo#42`, or a GitHub issue URL are bound on the session (max 5). Close-intent wording (`fix` / `closes` / `resolve` near the ref) stores **Fixes**; otherwise **Refs**. `@Grok /link #42` (or `/link fix #42`, full issue URL) binds manually; `/unlink #42` and `/link clear` remove. Bound issues appear on `/status`, brief, and hand-off; Discord thread titles get a `#N` prefix when retitled; the Grok remote-work prompt requires PR body lines (`Fixes #N` / `Refs #N`) and a matching title prefix when opening a PR. One-way GitHub parse only (no Linear/Jira sync).

While a task is running, the bot updates the status message every few seconds with elapsed time, **phase chips** (`read → edit → test → PR`, bold = current, ✓ = seen), and a short thought/tool activity snippet. Tool activity is read live from the Grok session’s `updates.jsonl` (streaming-json only emits thought/text/end). Assistant text streams into the thread via Grok’s `streaming-json` output: a live message shows the **latest** text (tail window), Discord edits run asynchronously so they never block reading Grok’s stdout, and when a reply outgrows one Discord message the bot seals that message and continues in a new one (finish does not re-post sealed chunks). Typing is pulsed while streaming. Use `/cancel` (or `/stop`) in that thread to kill the Grok process (the live stream is finalized without a stuck “streaming…” footer). Follow-ups sent while a run is active are queued in order (max 5) and start automatically when the current run finishes; the bot replies with `Queued (#N)`.

**Worktrees:** when `worktreeIsolation` is on (default) and the project is a git repo, each Discord thread gets its own worktree at `data/worktrees/<project>/<threadId>` on branch `grok/discord/<threadId>`, created from the main checkout’s `HEAD`. Grok runs with `--cwd` set to that worktree so concurrent threads do not share a working tree. `/reset` removes the worktree and deletes the branch. If the branch’s PR is already **merged** or **closed**, the next task in that thread automatically removes the worktree/branch (and session) and starts a fresh worktree from `HEAD`. Idle worktrees are also pruned after **`worktreeIdleTTLDays`** days without activity (default 30; session `updatedAt`, or directory mtime for orphans). Set to `0` to disable. A background sweep runs daily and skips threads that are currently running or queued. Set `"worktreeIsolation": false` to always use the main project path.

**Pull requests:** Discord runs are remote, so Grok is instructed to never leave changes as local-only commits. When it makes code changes it should commit on the thread branch (or a feature branch), `git push`, and open/update a PR with `gh pr create`, then include the PR URL in the reply. Requires `gh` auth on the host (`gh auth login` or `GH_TOKEN`) and push access to the project remotes.

**PR status cards:** after a run, the bot resolves **all** PR URLs in the reply plus the worktree-branch PR (`gh pr list --head`), stores them on the session (multi-PR / multi-repo supported), and keeps **one editable status message per PR** (state, checks, review, link). Open PRs are polled about every 90s. On transitions (approved, changes requested, CI green, merged/closed) the poller posts a short **PR event** line in the thread. Worktree/session cleanup runs only when **all** tracked PRs are merged or closed (and the thread is idle). `@Grok /status` lists every tracked PR.

**Completion summary:** after a non-cancelled run in a git checkout, the bot posts a **Summary** card with branch/SHA, base (`origin/main` / `main` / …), `git diff --stat` rollup, name-status file list (capped), optional **risk** paths (migrations, auth, deploy, secrets, …), and PR link when known. No extra model call — pure git. Override globs with optional config `riskyPathGlobs` (omit = defaults; `[]` = disable risk flags). Skipped when there are no commits ahead of base and no dirty files.

**CI triage:** while a thread has open PRs, the poller watches checks **per PR**. On failure it posts a **CI failed** digest once per head SHA (per PR) and suggests `@Grok /fix-ci`. That command queues a fix for all currently failing tracked PRs (or one if only a single PR is red). Optional `"autoFixCI": true` auto-queues a fix (default **off**); `"autoFixCIMax"` caps auto attempts **per PR** (default 2).

**Attachments (user → Grok):** files on the `@Grok` message are downloaded under `data/attachments/<messageId>/`, absolute paths are added to the prompt, and the directory is deleted when the run finishes. Limits: 10 files, 25 MiB each, 50 MiB total. A mention with only attachments (no text) still starts a task.

**Uploads (Grok → Discord):** when the thread has an isolated git worktree, Grok can attach artifacts by ending its reply with a `DISCORD_UPLOAD:` block listing paths **inside that worktree** (e.g. APK, Excel). Paths outside the worktree are rejected. Limits: 10 files, 25 MiB each. Requires the bot **Attach Files** permission (included in the Config page install URL).

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
| `GROK_DISCORD_HTTP_LISTEN` | Override `httpListen` for the web UI |
| `GROK_DISCORD_DEBUG` | Post grok stderr into the thread |
| `XAI_API_KEY` | Auth for headless grok if not logged in |

## Layout

```
main.go
internal/config/       # config.json loader + runtime add/persist
internal/bot/          # Discord handlers, prompt parsing, status snapshot
internal/web/          # private admin UI (hime, templates, SSE)
internal/grokrun/      # exec grok -p
internal/gitworktree/  # per-thread git worktree isolation
internal/sessionstore/ # thread → session persistence
internal/history/      # per-turn conversation log for the web UI
config.example.json
```

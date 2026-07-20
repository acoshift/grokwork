# grokwork

**Grok Work** — Discord-first Grok workflow, with a full web ship surface.

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
4. **OAuth2 → URL Generator**: scope `bot`; permissions: View Channel, Send Messages, Manage Messages, **Pin Messages** (brief card), Attach Files, Read Message History, Create Public Threads, Send Messages in Threads. Or use the install URL on the admin Config page (same bit set). Pinning is a separate permission from Manage Messages.
5. Invite / re-authorize the bot so those permissions land on its role (changing the URL alone does not upgrade an already-joined bot).

If you see `websocket: close 4014: Disallowed intent(s)`, the bot is requesting a privileged intent that is still off in the portal — turn **Message Content Intent** on and restart.

## 2. Configure

```bash
git clone https://github.com/acoshift/grokwork.git
cd grokwork
cp config.example.json config.json
# edit token, user IDs, project paths, channel map
```

| Field | Purpose |
|--------|---------|
| `discordToken` | Bot token (or `DISCORD_BOT_TOKEN` env) |
| `discordClientId` | Optional application/client ID for the install URL (decoded from the token when empty) |
| `allowedUserIds` | Who may invoke Grok (fail-closed if empty **and** no roles) |
| `allowedRoleIds` | Optional role allowlist |
| `projects` | Name → **absolute** path string, or object `{ "path", "github", "linear", "discordChannelId", "discordGuildId" }` |
| `channels` | Discord channel ID → project name (**required**; only way to select a project) |
| `yolo` | Auto-approve Grok tools (needed for unattended fix/investigate) |
| `summarizeThreadTitle` | Call Grok once to name the Discord thread before work (default true) |
| `summarizeTimeoutMs` | Timeout for the title summary call (default 45000) |
| `worktreeIsolation` | Per-thread git worktree under `data/worktrees/` (default true; non-git projects use main cwd) |
| `worktreeIdleTTLDays` | Days of inactivity before pruning idle worktrees (default `30`; `0` disables). Editable on the Config page |
| `autoFixCI` / `autoFixCIMax` | Auto-queue CI fixes when checks fail (default off; max attempts per PR, default 2) |
| `boardStaleDays` | Days without session activity before `/board` lists a thread as **stale** (default `3`). Editable on the Config page |
| `boardDigestChannel` | Optional Discord channel ID for a nightly team board post (empty = disabled). Editable on the Config page |
| `httpListen` | Private-network web UI bind address (default `:8787`; override with `GROK_WORK_HTTP_LISTEN`) |
| `webPublicBaseURL` | Absolute origin for OAuth redirect URIs (e.g. `http://100.x.y.z:8787`). Required when `webAuth.enabled` |
| `discordClientSecret` | Discord OAuth2 client secret for web login (or env `DISCORD_CLIENT_SECRET` / `GROK_WORK_DISCORD_CLIENT_SECRET`) |
| `webAuth` | Optional Discord OAuth for the web UI (see below). Default / omitted = open LAN mode (no login) |
| `discordGuildId` | Optional default Discord server id for deep links when a project omits its own |
| `projects.<name>.discordGuildId` | Per-project Discord server id (multi-guild); used for “Open in Discord” / web thread URLs |
| `projects.*.github.repos` | Optional multi-repo catalog (`owner`/`repo`) for Issues UI; omit to discover from git remotes |
| `projects.*.linear` | Optional Linear ticket binding (`enabled`, `apiKey`, `teamKey`). Key may also be `LINEAR_API_KEY_<PROJECT>` |
| `projects.*.discordChannelId` | Preferred Discord channel for web-started threads; must be mapped to this project in `channels` |

`config.json` is gitignored. Never commit tokens, user IDs, client secrets, or private project paths.

### Web UI (private network / Tailscale)

While the process runs it also serves a small server-rendered admin UI (hime + `html/template` + stdlib SSE). Nav: **Dashboard · Ship · Issues · Sessions · Worktrees · Config**.

| Path | View |
|------|------|
| `/` | Dashboard — live active runs / session counts (SSE refresh) |
| `/ship` | Ship board — all bot-tracked PRs per project, CI/review status, copyable lead digest |
| `/sessions` | Work units (history + session store); open a thread for status, PR links, continue/cancel when gated |
| `/sessions/{id}` · `/sessions/{id}/diff` | Session detail and worktree unified diff |
| `/history` · `/history/{id}` | Turn-by-turn conversation log (also linked from Discord action bar **History**) |
| `/worktrees` | List per-thread git worktrees; prune one or all past idle TTL |
| `/config` | Add/remove projects, channel→project map, allowed users/roles, Linear/GitHub project fields, worktree idle TTL, team board digest, CI auto-fix, completion risk globs |
| `/login` | Discord OAuth login (only when `webAuth.enabled`) |
| `/issues` | Project picker for GitHub issues |
| `/projects/{project}/issues` | Issue list with multi-repo picker |
| `/projects/{project}/linear` | Linear issues (when Linear enabled on the project) |
| `/prs/{owner}/{repo}/{n}` | PR detail (ship board links here); address CI / address review when `startSessions` is on |
| `/prs/.../diff` | Unified diff browser for a PR |

**Web writes (optional, require `webAuth.enabled`):**

| Feature flag | Effect |
|--------------|--------|
| `webAuth.features.githubWrites` | Members can comment / close issues & PRs |
| `webAuth.features.merge` | Admins can merge (default `webMergeMethod`: `squash`). Never passes `--admin` |
| `webAuth.features.startSessions` | Members can **Fix** from a GitHub/Linear issue and **Address CI / Address review** from a PR (starts/queues a Grok run on the project’s Discord channel) |

Bind for Tailscale or LAN with `"httpListen": "0.0.0.0:8787"` (or a Tailscale IP).

#### Web auth (optional Discord OAuth)

By default the UI stays **open on the private network** (no login) so existing configs keep working. To require Discord login:

1. Developer Portal → your app → **OAuth2** → add redirect  
   `http://{host}:8787/auth/discord/callback` (use your real `webPublicBaseURL`).
2. Copy the **Client Secret** into `discordClientSecret` (or `DISCORD_CLIENT_SECRET`).
3. Set config (or env):

```json
"webPublicBaseURL": "http://100.x.y.z:8787",
"webAuth": {
  "enabled": true,
  "adminDiscordIds": ["YOUR_DISCORD_USER_ID"],
  "memberDiscordIds": [],
  "viewerDiscordIds": [],
  "features": { "githubWrites": false, "merge": false, "startSessions": false }
}
```

| Field / env | Purpose |
|-------------|---------|
| `webAuth.enabled` | Turn on OAuth gates |
| `webAuth.sessionSecret` | Optional / reserved (web sessions are opaque server-side IDs, not HMAC cookies) |
| `webAuth.adminDiscordIds` | Discord user IDs who may change config / prune worktrees |
| `webAuth.memberDiscordIds` / `viewerDiscordIds` | Optional explicit lists |
| `webAuth.features.*` | `githubWrites`, `merge`, `startSessions` (see table above; all default false) |
| Bot `allowedUserIds` | Allowlisted users get **member** if not in the lists above |
| `GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID` | If `adminDiscordIds` is empty, merged on boot as the first admin |

When enabled: unauthenticated page GETs redirect to `/login`; config and worktree **POST**s require an **admin** session + CSRF. Static assets stay public. Discord `@Grok` is unchanged (still uses the bot allowlist).

## 3. Run

```bash
go run .
# or
go build -o grokwork .
./grokwork
```

### Docker

```bash
docker build -t grokwork .
```

The image is a distroless binary of `grokwork` (Discord bot **and** web UI). It does **not** include `grok`, `git`, or `gh` — mount those (and auth, config, project trees) at runtime:

```bash
docker run --rm \
  -v "$PWD/config.json:/config/config.json:ro" \
  -v "$HOME/.grok:/home/nonroot/.grok" \
  -v /path/to/your/code:/path/to/your/code \
  -v "$(which grok):/usr/local/bin/grok:ro" \
  -e GROK_WORK_CONFIG=/config/config.json \
  -e HOME=/home/nonroot \
  -p 8787:8787 \
  grokwork
```

Project paths in `config.json` must match paths **inside** the container. Set `"grokBin": "/usr/local/bin/grok"` if needed. For day-to-day use on a laptop, the host binary (or launchd) is simpler than Docker.

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
| `@Grok /help` | Command help |
| `@Grok /projects` | Show this channel’s mapped project |
| `@Grok /status` | Show owner, project, session, lifecycle label, worktree branch, PR, and queue depth |
| `@Grok /brief` | Pin/update the continuity card (goal, label, done/left, branch, PR, key files) |
| `@Grok /brief goal <text>` | Set the sticky goal, then refresh the brief card |
| `@Grok /label` | Show lifecycle label; `/label <state>` sets manual; `/label auto` re-enables auto |
| `@Grok /board [running\|queued\|waiting\|stale\|label\|all]` | Team activity board for this channel’s project (running, queued, waiting on human, stale) |
| `@Grok /link #N` · `/link ENG-123` | Bind GitHub/Linear tickets (Linear only when enabled for the project); `/link fix …` stores **Fixes**; `/unlink`; `/link clear` |
| `@Grok /claim` | Take ownership of this thread |
| `@Grok /hand-off @user` | Transfer ownership and post a short hand-off card |
| `@Grok /reset` | Drop session + remove this thread’s git worktree (owner/mod) |
| `@Grok /cancel` · `/stop` | Stop the in-progress run (owner/mod; queued follow-ups still run) |
| `@Grok /fix-ci` | Fetch failing CI checks for this thread’s PR(s) and queue a minimal fix |
| `@Grok <task>` + attachments | Download files for Grok to read (logs, screenshots, patches) |
| Reply to a message with `@Grok <task>` | Include the referenced message text + attachments (e.g. image, then ask Grok) |

**Run action bar:** buttons on the live status / done message and on `/status` — **Cancel · Continue** (modal) · **Reset** (confirm) · **History** (admin UI path). Same ownership rules as the text commands.

**Thread ownership:** the first `@Grok` author on a thread becomes **owner** (stored on the session). Anyone on the allowlist may still queue tasks (soft open). `@Grok /cancel` and `@Grok /reset` require the owner, a co-owner, or a Discord moderator (Administrator, Manage Messages, or Manage Threads). `@Grok /claim` takes primary ownership (previous owner stays as co-owner). `@Grok /hand-off @user` transfers ownership and posts a short card (goal, status, PR, queue). Unowned legacy sessions stay open for cancel/reset until someone claims or the next task sets an owner.

**Continuity brief:** each thread keeps **one editable (and preferably pinned) brief card** with sticky goal, recent done turns, what’s left (queue/CI/PR), branch, PR links, key changed files, and open questions scraped from the last assistant reply. It refreshes after each non-cancelled run, on `@Grok /hand-off`, and on `@Grok /brief`. Goal defaults to the first task prompt; override with `@Grok /brief goal <text>`. Pinning needs **Pin Messages** for the bot (card still updates without pin). Manage Messages alone is not enough — Discord split pin into its own permission.

**Lifecycle labels:** each thread has a label `open → in_progress → blocked → needs_review → done | abandoned` (empty = open). Auto: first task → `in_progress`; ready (non-draft) open PR → `needs_review`; all PRs merged → `done`; all closed without merge → `abandoned`. Draft-only PRs stay `in_progress`. `@Grok /label blocked` (etc.) sets a **manual** label and pauses auto until `@Grok /label auto` (merge/close still force terminal labels). Shown on `/status`, brief, and hand-off.

**Team activity board:** `@Grok /board` lists non-terminal threads for **this channel’s mapped project**, grouped by **activity**: **running** (active Grok job), **queued** (follow-ups waiting), **waiting on human** (blocked / needs review / changes requested / CI failing), **stale** (no session activity for `boardStaleDays`, default 3), and **active** (everything else). Filter with `/board waiting`, `/board stale`, lifecycle label (`/board needs_review`), or `/board all` (includes done/abandoned). Optional nightly digest posts an all-projects card to `boardDigestChannel` (Config page or `config.json`).

**Issue / ticket binding:** tasks that mention `#42`, `owner/repo#42`, a GitHub issue URL, or (when Linear is enabled for the project) identifiers like `ENG-123` are bound on the session (max 5). Close-intent wording (`fix` / `closes` / `resolve` near the ref) stores **Fixes**; otherwise **Refs**. `@Grok /link #42` or `/link ENG-123` (or `/link fix …`, full issue URL) binds manually; `/unlink …` and `/link clear` remove. Bound tickets appear on `/status`, brief, and hand-off; Discord thread titles get a `#N` / identifier prefix when retitled; the Grok remote-work prompt requires PR body lines (`Fixes #N` / `Refs #N` or Linear identifiers) and a matching title prefix when opening a PR. Binding is one-way into the session (no push of state back to GitHub/Linear except via normal PR body conventions / Linear’s own GitHub integration).

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

User agent that starts on login, restarts on crash, and keeps the process in the background. Label used below: `com.acoshift.grokwork` — change it if you prefer another reverse-DNS name (plist filename, `Label`, and every `launchctl` command must match).

### 1. Build and prepare

```bash
cd /path/to/grokwork
go build -o grokwork .
mkdir -p data
```

### 2. Install the plist (does not start)

Write `~/Library/LaunchAgents/com.acoshift.grokwork.plist` (adjust absolute paths, `HOME`, and `PATH` so `grok`, `gh`, `git`, etc. resolve under launchd — it does **not** load your shell profile):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.acoshift.grokwork</string>
  <key>ProgramArguments</key>
  <array>
    <string>/path/to/grokwork/grokwork</string>
  </array>
  <key>WorkingDirectory</key>
  <string>/path/to/grokwork</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>/path/to/grokwork/data/stdout.log</string>
  <key>StandardErrorPath</key>
  <string>/path/to/grokwork/data/stderr.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>/Users/YOU</string>
    <key>PATH</key>
    <string>/Users/YOU/.grok/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
</dict>
</plist>
```

```bash
plutil -lint ~/Library/LaunchAgents/com.acoshift.grokwork.plist
```

Copying the file into `~/Library/LaunchAgents/` only installs it for this session until you bootstrap. On the next login, launchd loads agents in that directory automatically (`RunAtLoad` starts the binary).

### 3. Start / stop / restart

```bash
# Start (loads the job; RunAtLoad starts the binary immediately)
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.acoshift.grokwork.plist

# Stop (unload job; process exits)
launchctl bootout gui/$(id -u)/com.acoshift.grokwork

# Restart after rebuilding the binary (keeps the same installed plist)
go build -o grokwork .
launchctl kickstart -k gui/$(id -u)/com.acoshift.grokwork
```

`kickstart -k` kills the current process and starts a new one. A plain `go build -o grokwork .` overwrites the file on disk but does **not** replace the running process — always kickstart (or bootout + bootstrap) after a rebuild.

If bootstrap fails with “service already loaded”, either kickstart or bootout first, then bootstrap again.

### 4. Status and logs

```bash
launchctl print gui/$(id -u)/com.acoshift.grokwork
tail -f data/stdout.log data/stderr.log
```

### 5. Uninstall

```bash
# Stop if running, then remove the plist
launchctl bootout gui/$(id -u)/com.acoshift.grokwork 2>/dev/null || true
rm -f ~/Library/LaunchAgents/com.acoshift.grokwork.plist
```

### Notes

- Prefer modern `bootstrap` / `bootout` / `kickstart` over legacy `launchctl load` / `unload`.
- `WorkingDirectory` must be the repo root so relative `config.json` and `data/` resolve correctly (or set `GROK_WORK_CONFIG` in `EnvironmentVariables`).
- Logs grow without rotation; truncate or rotate `data/stdout.log` / `data/stderr.log` if needed.
- Do not put secrets in the plist; keep tokens in `config.json` (gitignored) or env vars you inject carefully.

## Env vars

| Variable | Purpose |
|----------|---------|
| `DISCORD_BOT_TOKEN` | Override token |
| `GROK_WORK_CONFIG` | Path to config.json |
| `GROK_WORK_HTTP_LISTEN` | Override `httpListen` for the web UI |
| `GROK_WORK_DEBUG` | Post grok stderr into the thread |
| `GROK_WORK_PUBLIC_BASE_URL` | OAuth public base URL override |
| `GROK_WORK_DISCORD_CLIENT_SECRET` / `DISCORD_CLIENT_SECRET` | OAuth client secret |
| `GROK_WORK_SESSION_SECRET` | Optional / reserved (`webAuth.sessionSecret`) |
| `GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID` | First admin when `adminDiscordIds` empty |
| `LINEAR_API_KEY_<PROJECT>` | Per-project Linear API key when not set in config (`PROJECT` uppercased; non-alnum → `_`) |
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
internal/ghpr/         # gh CLI wrapper (PR/issue state, checks, writes)
internal/linear/       # Linear GraphQL client
internal/audit/        # web write audit log under data/audit/
config.example.json
```

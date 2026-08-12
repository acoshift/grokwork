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
- **Pre-ship review skill:** this repo vendors `.grok/skills/scrutinize/` (full procedure) and `.grok/skills/scrutinize-before-ship/` (gate). Shipping runs also inject a pre-ship scrutinize contract into the agent prompt for **every** mapped project. For non-grokwork projects, either install a host user skill (`~/.grok/skills/scrutinize/`, Claude: `~/.claude/skills/scrutinize/`) or copy the vendored skill into that project’s `.grok/skills/`. Extra skill dirs: `~/.grok/config.toml` → `[skills] paths = ["~/team-skills"]`.

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
| `projects.<name>.allowedUserIds` | Per-project direct members. Namespaced actor ids (`discord:123`, `oidc:alice`); a bare id still means Discord. |
| `projects.<name>.teams` | `{ "<key>": { "label", "members", "capabilities" } }`. Team membership grants **both** project access and the named capability template. Fail-closed when there are no `allowedUserIds` and no team with members. Replaces the removed `allowedRoleIds` / `capabilityByRole`. **Teams are per project and are not shared:** a `support` team on eight projects is eight `members` lists to keep in sync — for one person on many projects, `allowedUserIds` + `capabilityByUser` is less to maintain. |
| `projects.<name>.caseKey` | Override the prefix new case keys take (`WEBAPP` → `WEBAPP-14`). Empty derives it from the project name; a non-ASCII name falls back to `CASE`. Read **only when minting**, so changing it never renames a case already quoted. Editable on project settings → Workflow |
| `projects.<name>.sla` | Per-severity case deadlines: `{ "<severity>": { "firstResponseMinutes", "resolutionMinutes" } }`. Both are tri-state — omit one and that clock simply does not run, so a project with no SLA reads as nothing being late rather than everything. An unknown severity key is a **load error**, not an inert row. Editable on project settings → Workflow |
| `projects` | Name → **absolute** path string, or object `{ "path", "github", "linear", "discordChannelId", "discordGuildId", "repoFetchIntervalMinutes", "caseKey", "sla" }` |
| `channels` | Discord channel ID → project name (**required**; only way to select a project) |
| `yolo` | Auto-approve Grok tools (needed for unattended fix/investigate) |
| `summarizeThreadTitle` | Call Grok once to name the Discord thread before work (default true) |
| `summarizeTimeoutMs` | Timeout for the title summary call (default 45000) |
| `worktreeIsolation` | Per-thread git worktree under the worktree root (default true; non-git projects use main cwd) |
| `worktreeDir` | Root folder for new worktrees (`<root>/<project>/<threadId>`). Empty → `data/worktrees` next to config. Absolute or relative to the config file dir. Editable on the Config page; existing sessions keep their cwd until reset |
| `worktreeIdleTTLDays` | Days of inactivity before pruning idle worktrees (default `30`; `0` disables). Editable on the Config page |
| `projects.*.repoFetchIntervalMinutes` | Idle background `git fetch` interval for the main checkout (default `5`; `0` disables). Editable on the project settings page. New worktrees always fetch with a hardcoded 5s throttle. |
| `projects.*.directToPrimary` | When true, new sessions use **No-PR / direct-to-primary** ship: worktree per session, bot fast-forwards primary and pushes (default false / PR mode). Editable on project settings. Sticky per thread after first run. |
| `projects.*.primaryBranch` | Optional short branch name grokwork treats as this project's primary (worktree base, direct ship, `/sync`, commits default, deploy empty allowlist, Actions tip). Empty = `origin/HEAD` + common names. May differ from GitHub's default. Single path component only. Editable on project Workflow settings. |
| `resumeActiveRuns` | Persist in-flight runs under `data/runs/` and auto-resume after restart (default `true`). Editable on the Config page |
| `autoFixCI` / `autoFixCIMax` | Auto-queue CI fixes when checks fail (default off; max attempts per PR, default 2) |
| `maxConcurrentRuns` | Host-wide cap on simultaneous agent runs (nil/`0` = unlimited). Editable on the Config page |
| `maxConcurrentRunsUser` | Per-actor cap on simultaneous agent runs (nil/`0` = unlimited). A follow-up queued on a thread the actor is *already* running is not charged against it — the cap counts concurrent runs, not queued work. Editable on the Config page |
| `modelRates` | Model name → `{ inputPerMTok, outputPerMTok, cacheReadPerMTok, cacheWritePerMTok }`, dollars per **million** tokens. Every field is tri-state because **unset ≠ free**: a turn is priced only when its model has a rate for every class it used, and `/spend` reports the rest as `≥ $X` plus an unpriced count rather than a total that is quietly too low. Cache **write** is its own column on purpose — providers charge a premium for it (1.25× on Anthropic) and defaulting it to the input rate is a wrong number wearing a right one's clothes. Editable at `/config/model-rates` |
| `maxConcurrentDeploys` | Host-wide cap on simultaneous deploy runs (default `4`). |
| `deployRunRetention` | Deploy runs kept per service+environment (default `50`). |
| `projects.*.deploy.enabled` | Turn deploys on for a project. Editable on project settings → Deploy |
| `projects.*.deploy.manifestPath` | Override the in-repo manifest path (default `.grokwork/deploy.yaml`) |
| `projects.*.deploy.environments.<env>.requireCapability` | Capability needed to deploy here: `approve`, `adminProject`, `safeOps`, `merge`. Empty = builder-class |
| `projects.*.deploy.environments.<env>.allowedRefs` | Refs deployable to this environment. Empty = the project primary branch only; `*` allows any. **This is what stops a pushed branch rewriting the pipeline and reaching production credentials** |
| `projects.*.deploy.environments.<env>.env` | Environment variables injected into every step (highest precedence). Never rendered back in the UI |
| `projects.*.deploy.environments.<env>.secretKeys` | Which of those keys the log redactor masks. Marked, not inferred from length: `K8S_NAMESPACE` should stay readable in a failed deploy's log, `DB_URL` should not |
| `projects.*.deploy.environments.<env>.stepTimeoutMaxMs` | Lower the per-step ceiling for this environment (hard max 1h) |
| `boardStaleDays` | Days without session activity before `/board` lists a thread as **stale** (default `3`). Editable on the Config page |
| `boardDigestChannel` | Optional Discord channel ID for a nightly team board post (empty = disabled). Editable on the Config page |
| `api.enabled` | Machine JSON API (`/api/v1`). Default off. See `docs/api-v1.md` |
| `httpListen` | Private-network web UI bind address (default `:8787`; override with `GROK_WORK_HTTP_LISTEN`) |
| `webPublicBaseURL` | Absolute origin for OAuth redirect URIs (e.g. `http://100.x.y.z:8787`). Required when `webAuth.enabled` |
| `discordClientSecret` | Discord OAuth2 client secret for web login (or env `DISCORD_CLIENT_SECRET` / `GROK_WORK_DISCORD_CLIENT_SECRET`) |
| `webAuth` | Optional OAuth login for the web UI — Discord, Google and/or GitHub (see below). Default / omitted = open LAN mode (no login) |
| `webAuth.providers.google` | `{ clientId, clientSecret }` for Google login; secret may instead come from env `GOOGLE_CLIENT_SECRET` / `GROK_WORK_GOOGLE_CLIENT_SECRET` |
| `webAuth.providers.github` | `{ clientId, clientSecret }` for GitHub login; secret may instead come from env `GITHUB_CLIENT_SECRET` / `GROK_WORK_GITHUB_CLIENT_SECRET` |
| `discordGuildId` | Optional default Discord server id for deep links when a project omits its own |
| `projects.<name>.discordGuildId` | Per-project Discord server id (multi-guild); used for “Open in Discord” / web thread URLs |
| `projects.*.github.repos` | Optional multi-repo catalog (`owner`/`repo`) for Issues UI; omit to discover from git remotes |
| `projects.*.linear` | Optional Linear ticket binding (`enabled`, `apiKey`, `teamKey`). Key may also be `LINEAR_API_KEY_<PROJECT>` |
| `projects.*.discordChannelId` | Preferred Discord channel for web-started threads; must be mapped to this project in `channels` |

`config.json` is gitignored. Never commit tokens, user IDs, client secrets, or private project paths.

### Web UI (private network / Tailscale)

While the process runs it also serves a small server-rendered admin UI (hime + `html/template` + stdlib SSE). The shell is project-first and its mode is derived from the URL alone. Global nav: **Projects · Ship · Cases · Reviews · Deploys · Sessions · Worktrees · Spend · Inbox · Config**; inside `/projects/{name}` it becomes that project's workspace (**Overview · Start task · Ship · Cases · Reviews · Issues · Linear · Commits · Deploys · Sessions · Worktrees · Spend · Settings**). A search box sits above both.

| Path | View |
|------|------|
| `/` | Dashboard — live active runs / session counts (SSE refresh) |
| `/ship` | Ship board — all bot-tracked PRs per project, CI/review status, copyable lead digest |
| `/sessions` | Work units (history + session store); open a thread for status, PR links, continue/cancel when gated |
| `/sessions/{id}` · `/sessions/{id}/diff` | Session detail and worktree unified diff |
| `/search?q=` | One box over cases, sessions, tracked PRs/issues and recent commits; an exact case key (`WEBAPP-14`) jumps straight to the case |
| `/history` · `/history/{id}` | Turn-by-turn conversation log (also linked from Discord action bar **History**) |
| `/worktrees` | List per-thread git worktrees; prune one or all past idle TTL |
| `/deploys` | Cross-project deploy board — one row per project × service × environment: what is live, how old, and what has happened since. A pure read of the run store (no manifest is opened), bounded to the most recently touched runs and it says so when the scan clipped |
| `/spend` · `/projects/{project}/spend` | What the agent runs cost, by project, actor and session. Tokens always; dollars only for models `modelRates` fully covers, otherwise `≥ $X` plus the unpriced models named |
| `/config` | Add/remove projects, channel→project map, allowed users and teams, Linear/GitHub project fields, worktree idle TTL, concurrency caps, team board digest, CI auto-fix, completion risk globs |
| `/config/model-rates` | Per-model token prices behind `/spend` (admin) |
| `/login` | OAuth login — one button per **configured** provider (only when `webAuth.enabled`) |
| `/issues` | Project picker for GitHub issues |
| `/projects/{project}/issues` | Issue list with multi-repo picker |
| `/projects/{project}/linear` | Linear issues (when Linear enabled on the project) |
| `/prs/{owner}/{repo}/{n}` | PR detail (ship board links here); address CI / address review / review-in-new-session when `startSessions` is on |
| `/prs/.../diff` | Unified diff browser for a PR |

**Web writes (optional, require `webAuth.enabled`):**

| Feature flag | Effect |
|--------------|--------|
| `webAuth.features.githubWrites` | Members can comment / close issues & PRs, and submit a **real GitHub review** (`gh pr review --approve\|--request-changes\|--comment`) from the PR page. The GitHub review additionally needs the per-project `githubWrites` capability, checked in the handler and not merely hidden in the rail — it is the only one of grokwork's three "review" actions branch protection counts, and it files under the host `gh` account |
| `webAuth.features.merge` | Members can merge (default `webMergeMethod`: `squash`). Never passes `--admin` |
| `webAuth.features.startSessions` | Members can **Fix** from a GitHub/Linear issue, **Address CI / Address review** from a PR, and **Review** a PR or commit (a PR review posts one PR comment; a commit review files issues) |

Bind for Tailscale or LAN with `"httpListen": "0.0.0.0:8787"` (or a Tailscale IP).

#### Web auth (optional OAuth: Discord / Google / GitHub)

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
| `webAuth.adminDiscordIds` | Actors who may change config / prune worktrees. Namespaced actor ids (`discord:123`, `google:<sub>`, `github:<numeric id>`); a bare snowflake still means Discord. The JSON key keeps its historical name |
| `webAuth.memberDiscordIds` / `viewerDiscordIds` | Optional explicit lists, same namespaced-actor-id form |
| `webAuth.providers.google` / `.github` | `{ clientId, clientSecret }` per provider — see **Login providers** below |
| `webAuth.features.*` | `githubWrites`, `merge`, `startSessions` (see table above; all default false) |
| Project membership | Actors on any project (`allowedUserIds` **or** a `teams.<key>.members` list) get **member** if not in the lists above |
| `GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID` | If `adminDiscordIds` is empty, merged on boot as the first admin |

When enabled: unauthenticated page GETs redirect to `/login`; config and worktree **POST**s require an **admin** session + CSRF. Static assets stay public. Discord `@Grok` is unchanged (still uses the bot allowlist).

#### Login providers

`/login` renders **one button per fully configured provider** — a provider missing either half of its credential pair shows no button **and** its `/auth/<provider>` + `/auth/<provider>/callback` routes refuse before any network call. At least one provider must be complete when `webAuth.enabled`, or the process refuses to boot; a deployment that logs people in with Google needs no Discord OAuth app.

| Provider | Credentials | Env fallback for the secret | Redirect URI to register | Scopes |
|----------|-------------|-----------------------------|--------------------------|--------|
| Discord | `discordClientId` (or decoded from the bot token) + `discordClientSecret` | `DISCORD_CLIENT_SECRET`, `GROK_WORK_DISCORD_CLIENT_SECRET` | `{webPublicBaseURL}/auth/discord/callback` | `identify` |
| Google | `webAuth.providers.google.clientId` + `.clientSecret` | `GOOGLE_CLIENT_SECRET`, `GROK_WORK_GOOGLE_CLIENT_SECRET` | `{webPublicBaseURL}/auth/google/callback` | `openid email profile` |
| GitHub | `webAuth.providers.github.clientId` + `.clientSecret` | `GITHUB_CLIENT_SECRET`, `GROK_WORK_GITHUB_CLIENT_SECRET` | `{webPublicBaseURL}/auth/github/callback` | `read:user` |

Client **ids** are read from config only (they are public and travel in the authorize URL); only the **secret** has an env fallback, and a config value always wins over the environment.

```json
"webAuth": {
  "enabled": true,
  "adminDiscordIds": ["YOUR_DISCORD_USER_ID", "google:1029384756", "github:583231"],
  "providers": {
    "google": { "clientId": "…apps.googleusercontent.com", "clientSecret": "" },
    "github": { "clientId": "Iv1.…", "clientSecret": "" }
  }
}
```

**Actor ids.** A non-Discord login is identified by the provider's **immutable subject**, never by an email or a login handle — both can be changed by their owner and the freed value re-registered by a stranger. Google contributes its OIDC `sub`, GitHub its numeric account `id`, and each provider gets its own namespace (`google:` / `github:`), so the same number arriving from two issuers is two different actors. Discord's actor id stays a **bare** snowflake, unchanged from before. Those namespaced strings are what goes in `adminDiscordIds` / `memberDiscordIds` / `viewerDiscordIds`, `projects.*.allowedUserIds` and `projects.*.teams.*.members`.

A brand-new provider account that is a member of nothing logs in at the provider and is then **denied by name**: the login page reports the exact actor id an admin has to allowlist. That is the intended fail-closed path, not an error.

Finding your ids: `curl -sH "Authorization: Bearer <token>" https://api.github.com/user | jq .id`, or read the actor id straight off the denial message after one attempt.

### Linking your logins (`/account`)

By default each login is its own actor, so one person arriving through Discord and Google is two strangers — separate sessions, grants, ownership, spend and run caps. **`/account`** fixes that: any signed-in user can attach another login to the account they are signed in as, and detach it again. There is no capability gate, deliberately — gating it would leave the least-privileged person permanently split in two with no way to ask for a fix.

The account you were signed in as when you linked stays **canonical**; the login you attached becomes an **alias**, and signing in with it afterwards lands you in the canonical account. Existing Discord-only deployments therefore need no migration: the snowflake everyone is already allowlisted under remains the account id.

Two consequences worth knowing before you configure anything:

- **Grants must name the account, never an alias.** An `allowedUserIds`, team-member or `capabilityByUser` entry naming an alias will never match, because a session never carries one. Linking *absorbs* what the alias already had — its web-role entries, project access, team memberships, ownership, watcher and case-role ids are rewritten onto the account — so a login that already had access does not lose it. Absorbing is one-way: **unlinking does not give the grants back**, and the confirm text says so.
- **Unlink is refused if it would lock you out** — you cannot detach the only login that can still be minted for your account (on a Google-only deployment, that is the Google alias of a Discord-canonical account).

Linking a login that is already an alias elsewhere, or that is itself an account with aliases of its own, is refused: that is an account *merge*, which is out of scope. Both link and unlink are audited (`identity.link` / `identity.unlink`), **including refusals** — "someone tried to attach a login to my account" is exactly the report that answers.

### Git attribution comes from a linked GitHub login

The GitHub `@login` that shows up in public output — the PR footer and `Co-authored-by` trailer on ship prompts, the "On behalf of @…" on web-posted comments, and `@Grok /review`'s `gh pr edit --add-reviewer` — now comes **only** from a GitHub login the person proved by signing in with it at `/account`. No admin asserts it any more.

The trailer address is `<numericID>+<login>@users.noreply.github.com`. GitHub matches on the **numeric id**, so the commit lands on that person's profile and contribution graph, no personal email enters public git history, and a later rename keeps working. Both halves are required: an unlinked (or handle-expired) actor gets **no trailer and no mention at all**, which is deliberate — a trailer that attributes to nobody looks like the feature worked.

The cached login is **re-proved on every GitHub sign-in** and **expires after 30 days**, because signing in with Discord proves nothing about a GitHub name and a renamed-then-squatted handle must not keep being mentioned. `/account` marks such a row `handle unverified`; recovery is one GitHub sign-in. This needs no extra OAuth scope — `read:user` already returns the id and login.

> **Removed config key:** `discordUserGitHub` (and its `/config/github-identities` page) is **gone**. A config that still carries it loads fine and prints a startup warning naming the affected ids so you can ask those people to link GitHub themselves; the mappings are ignored. There are no env vars to change — the key never had an env fallback, and the replacement adds none: the GitHub credentials under `webAuth.providers.github` you already need for login are the whole requirement. Until a user links, they simply get no trailer, exactly like an unmapped user before.

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
| `@Grok /reset` | Drop session + remove this thread’s git worktree (owner, co-owner, or project admin) |
| `@Grok /cancel` · `/stop` | Stop the in-progress run (owner, co-owner, or project admin; queued follow-ups still run) |
| `@Grok /fix-ci` | Fetch failing CI checks for this thread’s PR(s) and queue a minimal fix |
| `@Grok <task>` + attachments | Download files for Grok to read (logs, screenshots, patches) |
| Reply to a message with `@Grok <task>` | Include the referenced message text + attachments (e.g. image, then ask Grok) |

**Run action bar:** buttons on the live status / done message and on `/status` — **Cancel · Continue** (modal) · **Reset** (confirm) · **History** (admin UI path). Same ownership rules as the text commands.

**Thread ownership:** the first `@Grok` author on a thread becomes **owner** (stored on the session). Anyone on the allowlist may still queue tasks (soft open). `@Grok /cancel` and `@Grok /reset` require the owner, a co-owner, or a project admin (a member of a team whose capability template grants `adminProject`). Discord channel permissions no longer grant anything — authorization is per project, not per guild. `@Grok /claim` takes primary ownership (previous owner stays as co-owner). `@Grok /hand-off @user` transfers ownership and posts a short card (goal, status, PR, queue). Unowned legacy sessions stay open for cancel/reset until someone claims or the next task sets an owner.

**Audit:** both surfaces write the same JSONL records under `data/audit/<date>.jsonl`. The web UI logs its mutations; the Discord command surface logs every mutating command **and every refusal**, since a refused `/reset` is exactly what an operator goes looking for. `detail.source` names which surface produced a row, and run dispatches record which affordance started them (a thread task, a completion-card button, `/fix-ci`, `/address`, a decision card) — the action name alone cannot tell them apart. Two things never reach the log on the Discord side: message content and prompts (a task or a customer update can carry customer data — free-text arguments like a checkpoint label are dropped, not trimmed), and local filesystem paths, which are stripped centrally because most strings that land there are git/gh stderr nobody controls.

**Continuity brief:** each thread keeps **one editable (and preferably pinned) brief card** with sticky goal, recent done turns, what’s left (queue/CI/PR), branch, PR links, key changed files, and open questions scraped from the last assistant reply. It refreshes after each non-cancelled run, on `@Grok /hand-off`, and on `@Grok /brief`. Goal defaults to the first task prompt; override with `@Grok /brief goal <text>`. Pinning needs **Pin Messages** for the bot (card still updates without pin). Manage Messages alone is not enough — Discord split pin into its own permission.

**Lifecycle labels:** each thread has a label `open → in_progress → blocked → needs_review → done | abandoned` (empty = open). Auto: first task → `in_progress`; ready (non-draft) open PR → `needs_review`. Draft-only PRs stay `in_progress`. Merged/closed PRs do **not** auto-set `done`/`abandoned` — mark those yourself with `@Grok /label done` (or the session rail), or `@Grok /close` for cases. `@Grok /label blocked` (etc.) sets a **manual** label and pauses auto until `@Grok /label auto`. Shown on `/status`, brief, and hand-off.

**Team activity board:** `@Grok /board` lists non-terminal threads for **this channel’s mapped project**, grouped by **activity**: **running** (active Grok job), **queued** (follow-ups waiting), **waiting on human** (blocked / needs review / changes requested / CI failing), **stale** (no session activity for `boardStaleDays`, default 3), and **active** (everything else). Filter with `/board waiting`, `/board stale`, lifecycle label (`/board needs_review`), or `/board all` (includes done/abandoned). Optional nightly digest posts an all-projects card to `boardDigestChannel` (Config page or `config.json`).

**Issue / ticket binding:** tasks that mention `#42`, `owner/repo#42`, a GitHub issue URL, or (when Linear is enabled for the project) identifiers like `ENG-123` are bound on the session (max 5). Close-intent wording (`fix` / `closes` / `resolve` near the ref) stores **Fixes**; otherwise **Refs**. `@Grok /link #42` or `/link ENG-123` (or `/link fix …`, full issue URL) binds manually; `/unlink …` and `/link clear` remove. Bound tickets appear on `/status`, brief, and hand-off; Discord thread titles get a `#N` / identifier prefix when retitled; the Grok remote-work prompt requires PR body lines (`Fixes #N` / `Refs #N` or Linear identifiers) and a matching title prefix when opening a PR. Binding is one-way into the session (no push of state back to GitHub/Linear except via normal PR body conventions / Linear’s own GitHub integration).

While a task is running, the bot updates the status message every few seconds with elapsed time, **phase chips** (ship: `read → edit → test → PR`/`ship`; investigate: `read → dig → report`; explain: `draft`; bold = current, ✓ = seen), and a short thought/tool activity snippet. Tool activity is read live from the Grok session’s `updates.jsonl` (streaming-json only emits thought/text/end). Assistant text streams into the thread via Grok’s `streaming-json` output: a live message shows the **latest** text (tail window), Discord edits run asynchronously so they never block reading Grok’s stdout, and when a reply outgrows one Discord message the bot seals that message and continues in a new one (finish does not re-post sealed chunks). Typing is pulsed while streaming. Use `/cancel` (or `/stop`) in that thread to kill the Grok process (the live stream is finalized without a stuck “streaming…” footer). Follow-ups sent while a run is active are queued in order (max 5) and start automatically when the current run finishes; the bot replies with `Queued (#N)`.

**Search:** the sidebar box and `/search?q=` answer over cases (key, customer ref, title), sessions (goal, label, thread id), tracked pull requests and issues, and recent commits, grouped by kind. Results only ever include projects the signed-in viewer can open, and an exact case key resolves straight to that case exactly as `/c/<key>` does — a key in a project you cannot open answers identically to one that does not exist. Work per query is bounded and printed at the foot of the page: each kind returns at most 20 matches, and commits are read as the newest 300 on the primary ref of **one** project (pick it in the Project dropdown; an unscoped search reads no git log at all).

**Spend:** every finished turn records the agent, the model it ran on, and its token usage, and `/spend` (plus `/projects/<name>/spend`) folds those into a report by project, actor and session. Tokens and dollars are separate answers on purpose: a turn always contributes its tokens, but contributes dollars only when `modelRates` covers **every** class it used, so a half-filled rate table yields `≥ $X`, a count of unpriced turns, and the offending model names — never a total that is quietly too low. Visibility is applied per turn, so the actor and session tables cannot carry a hidden project's burn rate. The session page carries the same figure for one thread.

**Case SLA:** with `projects.<name>.sla` set, the case board and case pages badge a first-response or resolution deadline that has passed, and `?sla=breached` filters to them. Nothing is stored but timestamps — breach is computed at render time, because a stored flag is wrong the moment a deadline passes with no writer running (the live-region fingerprint folds the flag in, so a clock crossing refreshes the badge without anyone navigating). A round is one open period: reopening a case restarts its response clocks, because a customer who came back is waiting again. Phase `answered` freezes the resolution clock only while the case sits there — the wait is the customer's — and the first-response clock is unaffected, since reaching `answered` *is* a response. A case filed before the stamps existed never breaches.

**Deploys:** `/deploys` is the cross-project board — one row per project × service × environment, with what is live, its age, and whether a newer run failed or is still going. It reads only the run store, so it costs the same for forty services as for one, and it says out loud when the scan clipped rather than reporting a long-quiet lane as "never deployed". `/projects/<name>/deploys` shows the pipeline the repo declares in `.grokwork/deploy.yaml`, read from the deployed commit rather than the working tree, as a services × environments board. With deploys enabled and the `deploy` feature flag on, each cell gets a Deploy button behind a confirm modal naming service, environment and short SHA; protected environments read as dangerous and need the capability their config demands. A run gets its own page with per-step status and a live log tail, a Cancel button while it runs, and Redeploy once it is terminal — replaying that run's frozen steps at its own commit, which is the phase-1 rollback. Runs queue per service+environment rather than being refused, and are never auto-resumed after a restart (shell steps are not idempotent): an interrupted run is marked as such and recovery is an explicit Redeploy. Note the deploy feature flag is fail-closed with web auth off, like every other write feature — a production deploy needs a signed-in identity for the audit trail and the capability check. See `docs/design-deploy-pipeline.md`.

**Worktrees:** when `worktreeIsolation` is on (default) and the project is a git repo, each Discord thread gets its own worktree at `<worktreeDir>/<project>/<threadId>` (default `data/worktrees/…` next to config; override with global `worktreeDir`) on branch `grokwork/<threadId>` (legacy `grok/discord/<threadId>` branches remain managed). New worktrees always run a short-throttle `git fetch --all --prune` (hardcoded 5s) on the main checkout and branch from `origin/*` when present (else local `HEAD`). Separately, an idle background loop keeps remotes fresh for the Commits UI using per-project `repoFetchIntervalMinutes` (default 5; `0` off). Grok runs with `--cwd` set to that worktree so concurrent threads do not share a working tree. `/reset` removes the worktree and deletes the branch. If the branch’s PR is already **merged** or **closed**, the next task (and the PR poller) automatically removes the worktree/branch but **keeps the session entry** so PR links and the closed state stay visible on the sessions list and detail. A follow-up task starts a fresh worktree the same way. Idle worktrees are also pruned after **`worktreeIdleTTLDays`** days without activity (default 30; session `updatedAt`, or directory mtime for orphans). Set to `0` to disable. A background sweep runs daily and skips threads that are currently running or queued. Set `"worktreeIsolation": false` to always use the main project path.

**Pull requests (default):** Discord runs are remote, so Grok is instructed to never leave changes as local-only commits. When it makes code changes it should commit on the thread branch (or a feature branch), `git push`, and open/update a PR with `gh pr create`, then include the PR URL in the reply. Requires `gh` auth on the host (`gh auth login` or `GH_TOKEN`) and push access to the project remotes.

**Direct to primary (No-PR mode):** per-project opt-in (`directToPrimary` on project settings). New threads stamp sticky direct ship mode. Grok still works on a managed worktree branch; after a successful run (exit 0, not cancelled, not max-turns, clean tracked tree) the bot fast-forwards the project primary via `git push origin <sessionSHA>:refs/heads/<primary>` (never force) and does **not** open a PR. Session entry is kept for follow-ups; the worktree is removed when the queue is empty. Non-fast-forward fails closed — use `@Grok /reset` and re-run. There is no CI watcher on primary after a direct ship (v1). Requires push access to the primary branch for the bot host identity.

**PR status cards:** after a run, the bot resolves **all** PR URLs in the reply plus the worktree-branch PR (`gh pr list --head`), stores them on the session (multi-PR / multi-repo supported), and keeps **one editable status message per PR** (state, checks, review, link). Open PRs are polled about every 90s. On transitions (approved, changes requested, CI green, merged/closed) the poller posts a short **PR event** line in the thread. Worktree cleanup runs only when **all** tracked PRs are merged or closed (and the thread is idle); the session entry is kept so PR links and final state remain on the sessions UI. `@Grok /status` lists every tracked PR.

**Completion summary:** after a non-cancelled run in a git checkout, the bot posts a **Summary** card with branch/SHA, base (`origin/main` / `main` / …), `git diff --stat` rollup, name-status file list (capped), optional **risk** paths (migrations, auth, deploy, secrets, …), and PR link when known. No extra model call — pure git. Override globs with optional config `riskyPathGlobs` (omit = defaults; `[]` = disable risk flags). Skipped when there are no commits ahead of base and no dirty files.

**CI triage:** while a thread has open PRs, the poller watches checks **per PR**. On failure it posts a **CI failed** digest once per head SHA (per PR) and suggests `@Grok /fix-ci`. That command queues a fix for all currently failing tracked PRs (or one if only a single PR is red). Optional `"autoFixCI": true` auto-queues a fix (default **off**); `"autoFixCIMax"` caps auto attempts **per PR** (default 2).

**Attachments (user → Grok):** files on the `@Grok` message are downloaded under `data/attachments/<messageId>/`, absolute paths are added to the prompt, and the directory is deleted when the run finishes. Limits: 10 files, 25 MiB each, 50 MiB total. A mention with only attachments (no text) still starts a task.

**Uploads (Grok → Discord):** when the thread has an isolated git worktree, Grok can attach artifacts by ending its reply with a `DISCORD_UPLOAD:` block listing paths **inside that worktree** (e.g. APK, Excel). Paths outside the worktree are rejected. Limits: 10 files, 25 MiB each. Requires the bot **Attach Files** permission (included in the Config page install URL).

**Replies:** if you **reply** to another Discord message when tagging Grok (e.g. someone posts a screenshot, then you reply `@Grok what's wrong?`), the bot includes that referenced message’s text and downloads its attachments as well. A bare `@Grok` reply (no extra text) still starts a review task.

## Security

- Per-project members (users/roles). Empty both lists on a project → nobody can `@Grok` there.
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
| `GROK_WORK_DISCORD_CLIENT_SECRET` / `DISCORD_CLIENT_SECRET` | Discord OAuth client secret (`discordClientSecret` wins) |
| `GROK_WORK_GOOGLE_CLIENT_SECRET` / `GOOGLE_CLIENT_SECRET` | Google login client secret (`webAuth.providers.google.clientSecret` wins) |
| `GROK_WORK_GITHUB_CLIENT_SECRET` / `GITHUB_CLIENT_SECRET` | GitHub login client secret (`webAuth.providers.github.clientSecret` wins). Distinct from `GH_TOKEN`/`GITHUB_TOKEN`, which are push credentials |
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
internal/identity/     # login → account links (data/identity-links.json) + GitHub attribution
internal/atomicfile/   # crash-safe file writes (temp → fsync → rename → fsync dir)
internal/history/      # per-turn conversation log + token usage for the web UI
internal/spend/        # folds history turns into a cost report against modelRates
internal/ghpr/         # gh CLI wrapper (PR/issue state, checks, writes)
internal/linear/       # Linear GraphQL client
internal/audit/        # write audit log under data/audit/ (web *and* Discord commands)
config.example.json
```

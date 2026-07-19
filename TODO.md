# TODO

Feature backlog for grok-discord as a **team development workflow** (Discord-first). Order is suggested priority, not a commitment.

Synthesized from multi-agent discussion (2026-07-18): collaboration, PR/CI ship loop, safety/governance, and Discord DX.

**Command surface:** keep **@Grok + text commands** as the primary UX. Native Discord slash commands are demoted (see Later) — registration is guild-wide by default and needs channel-permission sync to avoid showing in unmapped channels.

## Done

- [x] Channel → project mapping, allowlist, thread sessions
- [x] Commands: `/help`, `/projects`, `/reset`, `/status` (mention + text parse)
- [x] Grok-named Discord thread titles
- [x] Hide local project paths from Discord messages
- [x] Live progress heartbeats + `/cancel` (aliases: `cancel`, `/stop`, `stop`)
- [x] Discord attachments → prompt context (download, path list, cleanup)
- [x] Reply context: include referenced message text + attachments when tagging Grok
- [x] Per-thread git worktree isolation (`data/worktrees/`, `/reset` cleanup)
- [x] Stream Grok output (`streaming-json` → live Discord message edits)
- [x] Queue follow-ups when a thread is busy (instead of reject)
- [x] Idle worktree TTL cleanup (`worktreeIdleTTLDays`, default 30; daily sweep; config page)
- [x] Thread PR status card (session PR fields, Discord card, `gh` poller, eager cleanup on MERGED/CLOSED, `/status`)
- [x] Multi-PR per thread (multiple URLs/repos, per-PR cards, poll + CI + cleanup when all terminal)
- [x] Completion summary card (git diff --stat / name-status, risk globs, PR link; after each non-cancelled run)
- [x] CI fail → triage loop (digest per head SHA, `@Grok /fix-ci`, optional `autoFixCI` + cap)
- [x] Thread ownership & hand-off (`owner` / co-owners, `/claim`, `/hand-off @user`, cancel/reset rights + mod override)
- [x] Continuity / brief card (pinned; goal, done/left, branch, PR, files, questions; `/brief`, hand-off + post-run refresh)
- [x] Issue / ticket binding (`#N` / issue URL auto-parse, `/link` `/unlink`, PR body Fixes/Refs + title prefix)
- [x] PR event timeline (poller state machine: approve, changes requested, CI green, merged/closed)

## Design principles (team workflow)

1. **One thread = one worktree = one branch = one Grok session** — collaboration metadata wraps that, does not split it.
2. **Bot owns deterministic git/gh; Grok owns judgment** (fix, address review, investigate).
3. **Human authority is explicit** — owner, optional gates; model does not vote or merge.
4. **Queue is a social object** — authors and intents visible, not an opaque buffer.
5. **Pins/cards over chat archaeology** — one updated status/brief beats perfect history search.
6. **Prefer `gh` + session fields + one Discord status message** over new infrastructure.
7. **Never merge from the bot** unless a future role-gated `/merge` is designed with hard checks.
8. **Mention path stays primary** — new commands ship as `@Grok /…` first; slash only if channel-scoped visibility is solved.

## Next (P0 — multi-person daily use)

### 1. Thread ownership & hand-off — done

Stop “who owns this thread?” thrash when multiple engineers share a channel.

- [x] Session metadata: `owner` (first `@Grok` author), co-owners on claim/hand-off
- [x] `@Grok /claim`, `@Grok /hand-off @user` with a short hand-off card (goal, last status, PR, queue)
- [x] Soft default: open queue for anyone; optional strict lock later
- [x] Cancel/reset rights: owner + co-owner + Discord mod (Manage Messages / Manage Threads / Admin)
- [ ] Watchers (notify on complete) — see P1

### 2. Queue discipline (anti-thrash)

Build on existing max-5 FIFO so multi-user follow-ups do not contradict each other blindly.

- Store author + intent preview on each queued item: `Queued (#2) by @bob: "add tests only"`
- `@Grok /queue` list; `/dequeue N` / `/cancel-mine`
- Max pending per user per thread; same-user follow-up **replaces** last queued item
- Status shows `Running · queue 2 (alice, bob)`

### 3. Minimum safe team mode (governance baseline)

Ship before broad eng-VPN rollout (trusted-but-fallible teammates).

- [ ] Web UI auth + admin-only config mutations (no anonymous allowlist/path edits)
- [ ] Worktree enforced + path denylist when isolation is on
- [ ] Filtered process env for Grok children (no inherited host cloud superpowers by default)
- [ ] Immutable audit log: who, prompt, tools, commits, PR URL, canceler
- [ ] Rate limits + global concurrency caps (per-user and host-wide)
- [x] Thread ownership for cancel/reset + moderator override
- [ ] PR/commit attribution: prompter + thread URL in PR body / commit trailer; host remains pusher only

## Soon (P1 — durable team artifacts & ship loop)

### Collaboration & DX

- [x] **Continuity / brief card** — pin or update one message: goal, done/left, branch, PR, key files, open questions; refresh on `/brief` and hand-off
- [x] **Thread labels & lifecycle** — `open → in_progress → blocked → needs_review → done | abandoned`; auto on PR open/merge; `/label`, `/board` filters
- [x] **Team activity board** — `@Grok /board`: running, queued, waiting on human, stale (scoped to channel project); optional nightly digest channel
- [ ] **Task templates / presets** — Investigate · Fix tests · Review PR · Minimal fix via `@Grok /start …` or short aliases; inject fixed preambles; freeform always allowed
- [x] **Run action bar** — buttons on status/done: Cancel · Continue (modal) · Reset (confirm) · History (admin path; no slash required)
- [ ] **Notification hygiene** — `notifyOnDone: never | errors | always | long_only`; parent channel quiet, thread local
- [ ] **Watchers** — `@Grok /watch` or 👀; mention once on complete/fail (not every stream edit)

### PR / review / tickets

- [x] **Issue / ticket binding** — parse `#N` / issue URL; `/link`; PR body `Fixes`/`Refs` convention; title prefix
- [ ] **Review request from Discord** — `/ready`, `/review @user` with Discord→GitHub login map; optional `#code-review` radar post
- [ ] **Review comments → address loop** — `/comments` list unresolved; `/address` fix + push; offer `/rereview`
- [x] **PR event timeline** — poller state machine first (approve, changes requested, CI green, merged); webhook later on private HTTP
- [ ] **Path scope (monorepo)** — `/scope api/ mapi/`; inject into prompt; warn if diff escapes scope
- [ ] **Worktree fleet in Discord** — `/worktrees` list; fetch + create from `origin/main` (not stale local HEAD); idle warn before prune
- [ ] **Project conventions blurb** — inject from config or repo `GROK_DISCORD.md` (hard-capped); `/conventions`

### Linear (ticket system bridge)

Research note (2026-07): Linear is GraphQL + personal API keys or OAuth (`actor=app` for agents). Native **GitHub integration** already links PRs via `ENG-123` in branch/title/body and automates status — **do not reimplement that**. Prefer Discord-side parse/bind + `attachmentCreate` / comments for our thread unit. Agent Session APIs are Developer Preview (mention/delegate → webhook → agent activities). Attachments are idempotent by URL (good for Discord thread / PR cards). Webhooks cover Issues, Comments, Attachments, Agent session events (needs reachable HTTPS — private Tailscale tunnel or later public edge).

**Design fit:** extend existing issue binding (`session.Issues` / `/link`); one Discord thread still maps to one worktree/session; Linear issue is **metadata + external card**, not a second run owner. Human authority stays in Discord (owner/co-owner); Linear assignee/delegate is optional mirror.

#### L1 — bind + prompt (no webhooks)

Ship on top of current GitHub `#N` binding; **opt-in + API key per project**.

- [x] Parse Linear identifiers & URLs: `ENG-123`, `https://linear.app/<workspace>/issue/ENG-123/…` (and Discord `<…>` wraps)
- [x] Resolve via GraphQL (team key + number filter): title, state, URL, team; fail soft without key
- [x] Session fields: unified `TrackedIssue` with `provider: linear` + `linearId`, `identifier`, `url`, `title`, `state`
- [x] `@Grok /link ENG-123` · `/unlink ENG-123` · show on `/status`, brief, hand-off
- [x] Inject into remote-work prompt: identifier + title/state; branch name hint `eng-123-…`; PR body `Fixes ENG-123`
- [x] PR/title convention: put **`ENG-123`** in PR title and body so **Linear’s GitHub integration** moves state — bot does not call `issueUpdate`
- [x] Config: per-project `projects.*.linear.{enabled,apiKey,teamKey}` (+ env `LINEAR_API_KEY_<PROJECT>`); dual-shape `projects` JSON (string path or object)

#### L2 — write-back cards (API mutations, still no inbound webhooks)

- [ ] On bind / first run: `attachmentCreate` Discord thread URL on the Linear issue (title e.g. “Discord thread”, subtitle idle/running/done; metadata: threadId, project, owner)
- [ ] Refresh attachment on run start/end, PR open, CI fail, terminal PR (idempotent same URL)
- [ ] Optional one-shot Linear comment on run complete/fail (not every stream edit): summary + PR URL + Discord jump link
- [ ] `@Grok /linear comment <text>` — post a human/agent note to the bound issue
- [ ] Optional: create issue from Discord — `@Grok /linear new <title>` or `/file` from thread goal + last error; return `ENG-123` and auto-bind

#### L3 — inbound events (private HTTP)

Requires bot HTTP already used by web UI; webhook secret verify; allowlist Linear IPs/signatures.

- [ ] Webhook: Issue state/title changes → refresh Discord brief/status line for bound threads
- [ ] Webhook: new comment on bound issue → soft notify in Discord thread (once; respect notification hygiene)
- [ ] Optional: “Start in Discord” — comment command or label in Linear creates/links a thread in the mapped channel (project→channel reverse map)

#### L4 — Linear Agent (Developer Preview; later)

Only if team wants “assign to Grok in Linear” as a first-class entry point.

- [ ] OAuth app `actor=app` + scopes `app:assignable`, `app:mentionable`; Agent session event webhooks
- [ ] On delegate/mention: emit thought activity &lt;10s; open or resume Discord thread / worktree run; stream progress as agent activities; final result + PR URL
- [ ] Map Linear delegate ≠ Discord owner (Discord owner stays human; agent is co-worker)
- [ ] Explicit non-goal until stable: multi-workspace marketplace listing, billing for agent seats

#### Linear non-goals (keep)

- Full bidirectional field sync (labels, priority, cycles, projects as competing source of truth)
- Replacing Linear↔GitHub PR status automation
- Jira/Linear parity mega-adapter; start Linear-only when ticket provider is configured
- Polling Linear as a second CI/PR state machine when GitHub+Linear native link already covers merge→Done

### Safety (beyond minimum)

- [ ] **Tiered tool policy** — safe auto / notify / Discord approve for destructive, force-push, cloud CLIs, egress
- [ ] **Secrets hygiene** — redact history/stream; pre-push high-entropy scan; block PR with Discord warning
- [ ] **Push / PR gate modes** — `prMode: auto | propose | owner-only`; propose posts preview + Open PR button
- [ ] **Plan → approve → implement** — plan-only preset; buttons Approve & implement | Edit plan | Reject

## Later / nice-to-have (P2)

### Native Discord slash commands (demoted)

Optional complement to mention + text parse — **not** required for team workflow.

- Register guild-scoped `/task`, `/cancel`, `/status`, `/projects`, `/reset`, `/help` (and later peers)
- **Must not show in unmapped channels:** after register, sync Application Command Permissions so only `config.channels` IDs are allowed; re-sync when the map changes; handler still rejects unmapped channels
- Keep mention path as primary forever (or until slash visibility is solid)
- Threads inherit parent-channel visibility; thread-only commands still validated in the handler
- Caveats: permission client lag; server Integration overrides; multi-guild sync

### Other

- [ ] `/model` or per-channel model override
- [ ] Cross-thread dedupe (“possible duplicate of …”) + `/link` related threads
- [ ] Multi-repo attached worktrees (`/with web`) — opt-in; sequential sub-runs first
- [x] Ship board web page + lead digest (all bot PRs for a project)
- [ ] Searchable `/history` in Discord + fork-continue
- [ ] Continue from web (deeplink + optional queue follow-up with audit)
- [ ] Message context menu: **Ask Grok…** (preset + note on a screenshot/log)
- [x] Richer live progress (phase chips: read → edit → test → PR)
- [ ] Network/command egress allowlist or OS sandbox (container/bubblewrap)
- [ ] Dual-control for blast-radius config changes (add project path, full yolo)
- [ ] History retention TTL / project-scoped visibility
- [ ] Split PR by scope (`/split-pr`)
- [ ] Optional human push approval after local commits (`requirePushApproval`)

### Engineering / Discord library

**Stay on `discordgo` for now** (v0.29.x). It is under-maintained / lags Discord API bits (e.g. no `PIN_MESSAGES` = `1<<51` — we own that in `BotInvitePermissions`), but still receives occasional commits and covers our surface (gateway, REST, threads, components/modals). The brief-pin 403 was our invite bitset, not a library bug.

- [ ] **Do not migrate until a real trigger** — gateway/API breakage we can’t patch in a day; repeated need for new Discord features missing upstream; or multi-month silence that blocks us
- [ ] **If migrating: prefer [disgo](https://github.com/disgoorg/disgo)** over arikawa/disgord (active, modular gateway/rest/events; pre-v1 so expect breaks). Full rewrite ~23 files under `internal/bot/` + `main.go` — multi-day, high regression risk on stream/action bar/brief/ownership
- [ ] **Cheap risk reduction meanwhile:** keep owning lagging permission/flag constants locally; optionally thicken Discord ports (`Messenger`-style) so a future swap is adapters, not every handler
- [ ] Revisit ~6–12 months or on the next Discord API hard break

## Suggested build slices

| Slice | Includes | Outcome |
|-------|----------|---------|
| **A. Multi-person basics** | Ownership, claim/hand-off, queue author/replace | Threads feel intentional; less thrash |
| **B. PR-aware thread** | ~~PR status card~~ → ~~completion diff card~~ → ~~CI triage~~ → ~~PR event timeline~~ | Ship loop stays in Discord |
| **C. Safe team mode** | Web auth, audit log, env filter, rate limits, attribution | OK to widen allowlist on shared host |
| **D. Team artifacts** | ~~Continuity card~~, ~~labels + `/board`~~, ~~team activity board~~, templates, action buttons | Durable work items + one-tap controls |
| **E. Review loop** | ~~Issue bind~~, `/review`, `/comments`+`/address` | Close the inner review cycle |
| **F. Slash (optional)** | Guild register + channel permission allowlist = `config.channels` | Mobile autocomplete without polluting unmapped channels |
| **G. Linear bridge** | L1 bind+prompt → L2 attachments/comments → L3 webhooks → (optional) L4 agent | Tickets stay in Linear; execution stays Discord+Grok |

## Explicit non-goals (for now)

- Multi-agent debate or multiple Grok processes per thread
- In-chat project switching (channel map stays source of truth)
- Replacing GitHub PR review / branch protection
- Bot auto-merge
- Full Linear/Jira **field-level two-way sync** (labels, priority, cycles as dual source of truth) — prefer one-way bind + PR identifier convention + optional attachments/comments (see Linear L1–L3)
- Replacing Linear’s native GitHub PR status automation with bot-owned `issueUpdate` state machines
- Multi-tenant hard isolation between hostile coworkers
- Auth-heavy public web app (keep web private; put team UX in Discord)
- Slash commands that appear in every channel of the server

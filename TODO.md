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
- [ ] **Thread labels & lifecycle** — `open → in_progress → blocked → needs_review → done | abandoned`; auto on PR open/merge; `/board` filters
- [ ] **Team activity board** — `@Grok /board [project]`: running, queued, waiting on human, stale; optional nightly digest channel
- [ ] **Task templates / presets** — Investigate · Fix tests · Review PR · Minimal fix via `@Grok /start …` or short aliases; inject fixed preambles; freeform always allowed
- [x] **Run action bar** — buttons on status/done: Cancel · Continue (modal) · Reset (confirm) · History (admin path; no slash required)
- [ ] **Notification hygiene** — `notifyOnDone: never | errors | always | long_only`; parent channel quiet, thread local
- [ ] **Watchers** — `@Grok /watch` or 👀; mention once on complete/fail (not every stream edit)

### PR / review / tickets

- [ ] **Issue / ticket binding** — parse `#N` / issue URL; `/link`; PR body `Fixes`/`Refs` convention; title prefix
- [ ] **Review request from Discord** — `/ready`, `/review @user` with Discord→GitHub login map; optional `#code-review` radar post
- [ ] **Review comments → address loop** — `/comments` list unresolved; `/address` fix + push; offer `/rereview`
- [ ] **PR event timeline** — poller state machine first (approve, changes requested, CI green, merged); webhook later on private HTTP
- [ ] **Path scope (monorepo)** — `/scope api/ mapi/`; inject into prompt; warn if diff escapes scope
- [ ] **Worktree fleet in Discord** — `/worktrees` list; fetch + create from `origin/main` (not stale local HEAD); idle warn before prune
- [ ] **Project conventions blurb** — inject from config or repo `GROK_DISCORD.md` (hard-capped); `/conventions`

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
- [ ] Ship board web page + lead digest (all bot PRs for a project)
- [ ] Searchable `/history` in Discord + fork-continue
- [ ] Continue from web (deeplink + optional queue follow-up with audit)
- [ ] Message context menu: **Ask Grok…** (preset + note on a screenshot/log)
- [x] Richer live progress (phase chips: read → edit → test → PR)
- [ ] Network/command egress allowlist or OS sandbox (container/bubblewrap)
- [ ] Dual-control for blast-radius config changes (add project path, full yolo)
- [ ] History retention TTL / project-scoped visibility
- [ ] Split PR by scope (`/split-pr`)
- [ ] Optional human push approval after local commits (`requirePushApproval`)

## Suggested build slices

| Slice | Includes | Outcome |
|-------|----------|---------|
| **A. Multi-person basics** | Ownership, claim/hand-off, queue author/replace | Threads feel intentional; less thrash |
| **B. PR-aware thread** | ~~PR status card~~ → ~~completion diff card~~ → ~~CI triage~~ | Ship loop stays in Discord |
| **C. Safe team mode** | Web auth, audit log, env filter, rate limits, attribution | OK to widen allowlist on shared host |
| **D. Team artifacts** | ~~Continuity card~~, labels, `/board`, templates, action buttons | Durable work items + one-tap controls |
| **E. Review loop** | Issue bind, `/review`, `/comments`+`/address` | Close the inner review cycle |
| **F. Slash (optional)** | Guild register + channel permission allowlist = `config.channels` | Mobile autocomplete without polluting unmapped channels |

## Explicit non-goals (for now)

- Multi-agent debate or multiple Grok processes per thread
- In-chat project switching (channel map stays source of truth)
- Replacing GitHub PR review / branch protection
- Bot auto-merge
- Full Linear/Jira two-way sync (one-way issue parse + optional `gh` comment only)
- Multi-tenant hard isolation between hostile coworkers
- Auth-heavy public web app (keep web private; put team UX in Discord)
- Slash commands that appear in every channel of the server

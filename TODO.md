# TODO

Feature backlog for **Grok Work** (`grokwork`): Discord-first team development workflow + private web UI.

Order is suggested priority, not a commitment. **Code on `main` wins** if this file and a design doc disagree.

**Command surface:** keep **@Grok + text commands** as the primary UX. Native Discord slash commands stay demoted (see Later) — registration is guild-wide by default and needs channel-permission sync to avoid showing in unmapped channels.

**Related:** `docs/design-agentic-team-runtime.md` (rev 7 status), `docs/roles/`, `docs/support-case-guide.md`, `docs/design-per-user-github-identity.md`, `docs/design-agent-sandbox.md`, `docs/design-web-primary.md`.

---

## Design principles (team workflow)

1. **One thread = one worktree = one branch = one Grok session** — collaboration metadata wraps that, does not split it.
2. **Bot owns deterministic git/gh; Grok owns judgment** (fix, address review, investigate).
3. **Human authority is explicit** — owner, optional gates; model does not vote or merge.
4. **Queue is a social object** — authors and intents visible, not an opaque buffer.
5. **Pins/cards over chat archaeology** — one updated status/brief beats perfect history search.
6. **Prefer `gh` + session fields + one Discord status message** over new infrastructure.
7. **Never merge from the bot** unless a future role-gated `/merge` is designed with hard checks.
8. **Mention path stays primary** — new commands ship as `@Grok /…` first; slash only if channel-scoped visibility is solved.

---

## Done

### Core bot / Discord

- [x] Channel → project mapping, per-project allowlist, thread sessions
- [x] Commands: `/help`, `/projects`, `/reset`, `/status` (mention + text parse)
- [x] Grok-named Discord thread titles; hide local project paths from Discord
- [x] Live progress + `/cancel`; attachments + reply context into prompts
- [x] Per-thread git worktree isolation; idle worktree TTL + idle repo fetch
- [x] Stream Grok output; queue when busy (max 5)
- [x] **Queue social object** — author + intent; `/queue`, `/dequeue`, `/cancel-mine`; same-user replace last pending
- [x] Thread ownership & hand-off (`/claim`, `/hand-off`); cancel/reset owner/co/mod
- [x] Continuity / brief card; labels + lifecycle; `/board` (activity + **cases**)
- [x] Issue binding (GitHub + Linear L1 bind/prompt); PR multi-card + poller + CI triage + `/fix-ci`
- [x] Run action bar (Cancel · Continue · Reset · History)
- [x] Phase chips / activity from session updates

### Modes, Safe Team, IDE-free (Waves 1–2)

- [x] Session **Mode** (`investigate` / `explain` / `fix` / `case`) + RunPolicy hard gates
- [x] Capabilities + **SafeTeamMode** + templates + project config UI
- [x] Layer A Grok child env (denylist / strip secrets; omit GH when !ship)
- [x] `/start` presets; freeform inherits mode; half-fix coerce to investigate
- [x] Checkpoints `/checkpoint` `/undo` `/restore` (local refs, K8 checklist)
- [x] Project **verifyCommands** + `/verify` + config UI + session last-verify panel
- [x] `/sync` (fetch + merge origin primary)
- [x] Decision cards (`DECISION:` → Discord buttons → OpenQuestions)
- [x] `/comments` + `/address` (unresolved review threads → address run)

### Support / cases (Wave 3)

- [x] Discord `/case` lifecycle: investigate, escalate, answer, customer-update, close
- [x] Web cases board, create case, session case panel, phase POSTs, overview case counts
- [x] Ship board “from case” badge; keep case sessions after terminal PR cleanup (as shipped)
- [x] Role docs (`docs/roles/`); support case guide (`docs/support-case-guide.md`)
- [x] Real-project smoke of support + eng paths (ops)
- [x] **Case SLA** — per-project, per-severity `firstResponseMinutes` / `resolutionMinutes`; breach computed at render time (never stored), board badge + `?sla=breached` filter, `answered` pauses only the resolution clock, reopen restarts the round
- [ ] **SLA notifications** — nothing pings when a case breaches; the badge only appears to whoever is looking at the board

### Web UI (selected)

- [x] Project-first shell; ship board; sessions; worktrees; config; OAuth-optional web auth
- [x] Start task web; session continue / cancel / reset / label / goal / claim
- [x] Issues / Linear list / commits / PR detail / diff review / team PR reviews
- [x] Bulk fix, commit-review-as-session, markdown bodies, live SSE regions
- [x] **Search** (`/search?q=`) over cases, sessions, tracked PRs/issues and one project's recent commits; exact case key redirects; visibility applied before ranking; per-kind caps printed on the page
- [x] **Spend** (`/spend`, `/projects/{p}/spend`, session strip) from per-turn token usage in `internal/history`, priced against `config.modelRates` — tokens always, dollars only where the rate table is complete
- [x] **Cross-project deploy board** (`/deploys`) — pure read of the run store, bounded scan, says so when clipped
- [x] **Real GitHub reviews** (`POST /prs/…/github-reviews` → `gh pr review`), kept a separate route and rail action from the grokwork-local team verdict and the agentic review
- [ ] **Audit reader UI** — the log exists for both surfaces but there is no page over it
- [ ] **Spend budgets / alerts** — `/spend` reports, it does not enforce; no per-project cap, no warning when a rate is missing until someone opens the page

### Linear L1

- [x] Parse/bind `ENG-123` / URLs; GraphQL resolve; session fields; `/link`; prompt + PR identifier convention
- [x] Per-project Linear config (+ env key suffix)

---

## Next (recommended order)

### 1. Attribution trailers (Tier A) — **shipped**

See `docs/design-per-user-github-identity.md` Tier A. **Host still pushes/opens PRs.**

- [x] Discord user → GitHub login map (`config.discordUserGitHub` + web Config UI)
- [x] Ship prompt trailers + Co-authored-by; PR footer: display name + optional `@login` + session id (no Discord snowflake / thread jump link in human-visible text)
- [x] Web comment prefix “On behalf of @login …” when mapped
- [x] `/review @user` → formal GitHub review request when mapped (`gh pr edit --add-reviewer`)

### 2. Team DX — daily notifications — **shipped**

- [x] **Watchers** — `@Grok /watch` / `/unwatch`; mention once on complete/fail
- [x] **Notification hygiene** — `notifyOnDone: never | errors | always | long_only` (author @mention policy; Config → Run notifications)

### 3. Governance depth — **partial**

| Item | Status |
|------|--------|
| Web auth + feature flags + project visibility | **Partial** — OAuth optional; config admin-gated when auth on; not forced for all deploys |
| Per-project **teams** replace Discord roles | **Done** — `projects.<name>.teams` grants access *and* capabilities; `allowedRoleIds` / `capabilityByRole` removed, the Discord mod bypass for `/cancel` `/reset` with them. A named-but-unresolvable capability template fails closed (it used to fall through to *builder*) and `Load` warns for every dangling reference |
| Layer A env filter | **Done** |
| Layer B full env allowlist (per-project / host flag) | **Not started** |
| Audit log (web mutations, case/session actions) | **Done for the command surface** — both web mutations and every Discord command, denials included, in one `audit.Event` shape with `detail.source`; run dispatches record which affordance started them. **Still not a tool/run trail:** individual tool calls, file writes and model output are not audited, and there is no reader UI — `data/audit/*.jsonl` is read with `jq` |
| Rate limits + concurrency caps | **Done** — `maxConcurrentRuns` (host) + `maxConcurrentRunsUser` (per actor), checked outside the thread lock, not charged to same-thread follow-ups; case-investigate now goes through the same start rate limit as Fix and bulk Fix costs N |
| Crash-safe state writes | **Done** — `config.json` and `sessions.json` write tmp+fsync+rename+dir-fsync; the session store hands out deep copies so callers cannot alias (or race) its backing slices |
| OS sandbox for Grok children | **Design only** — `docs/design-agent-sandbox.md` |

### 4. Team DX leftovers — **partial / open**

- [ ] **Discord `/review` depth** — optional `#code-review` radar (formal GH review-request via map is shipped)
- [ ] **`/rerequest` / re-review** after address (if still desired)
- [ ] **Path scope (monorepo)** — `/scope api/`; warn if diff escapes
- [ ] **Project conventions blurb** — config or repo `GROK_DISCORD.md` (capped); `/conventions`
- [ ] **Worktree fleet in Discord** — `/worktrees` list (web worktrees page already exists)
- [ ] **autoCheckpoint** before fix runs (opt-in)

### 5. Linear L2+ (still open)

- [ ] L2 — Discord thread attachment on Linear issue; refresh on run/PR; optional complete comment; `/linear comment`; optional `/linear new`
- [ ] L3 — inbound Linear webhooks → Discord notify / brief refresh
- [ ] L4 — Linear Agent (Developer Preview; later)

### 5b. Deploy pipeline (manual) — **shipped**

Replaces GitHub Actions for **deploys only** (not CI). See `docs/design-deploy-pipeline.md`.

- [x] In-repo `.grokwork/deploy.yaml`, read from the deployed SHA; per-env pipelines + step `envs` filter
- [x] Per-project policy + per-environment credentials in `config.json` (masked, redacted in logs)
- [x] Step runner: own process-group kill path, env allowlist, log redaction, head+tail log cap
- [x] Trigger / cancel / run page / live log; `deploy` SSE domain
- [x] FIFO queue per service+environment; restart recovery (never auto-resumed)
- [x] Redeploy a past commit (the phase-1 rollback story); Discord notice with inbox fallback
- [ ] Automatic triggers (push / cron / merge) — explicit non-goal for now

### 6. Safety beyond minimum

- [ ] Tiered tool policy (safe auto / notify / Discord approve)
- [ ] Secrets hygiene (redact stream/history; high-entropy pre-push warn)
- [ ] Push/PR gate modes (`auto | propose | owner-only`)
- [ ] Plan → approve → implement preset + buttons

### 7. Wave 4 power (deferred)

From `design-agentic-team-runtime.md` — only after gates proven:

- Conflict clinic after `/sync`
- Ephemeral previews / investigate sandboxes
- SafeOps runbooks; multi-env policy
- Voice → task; “needs you” personal feed
- `/fork-fix`, case reopen / `/detach-case` (explicit non-goals for Wave 3)

---

## Later / nice-to-have (P2)

### Native Discord slash commands (demoted)

- Guild-scoped commands + **channel permission allowlist = `config.channels`**
- Re-sync on map change; handler still rejects unmapped channels
- Mention path remains primary

### Other

- [ ] `/model` or per-channel model override
- [ ] Cross-thread dedupe + link related threads
- [ ] Multi-repo attached worktrees (`/with web`)
- [ ] Searchable `/history` in Discord + fork-continue
- [ ] Message context menu: **Ask Grok…**
- [ ] Dual-control for blast-radius config changes
- [ ] History retention TTL
- [ ] Split PR by scope (`/split-pr`)
- [ ] Optional human push approval after local commits
- [ ] True per-user GitHub write identity (**Tier B** — large; after Tier A)

### Engineering / Discord library

**Stay on `discordgo` for now.** Do not migrate until a real trigger (API break we cannot patch). Prefer [disgo](https://github.com/disgoorg/disgo) if migrating. Own lagging permission constants locally.

---

## Suggested build slices (updated)

| Slice | Status | Outcome |
|-------|--------|---------|
| **A. Multi-person basics** | **Done** | Ownership, claim/hand-off, queue social |
| **B. PR-aware thread** | **Done** | PR cards, completion, CI triage, timeline |
| **C. Safe team mode** | **Mostly done** | Caps/modes/env Layer A + **attribution Tier A** + **per-project teams** + command-surface audit + concurrency caps shipped; **Layer B env allowlist, a tool/run-level audit trail and any reader UI remain** |
| **D. Team artifacts** | **Mostly done** | Brief, labels, board, action bar, watchers/notifyOnDone, case SLA badges, spend report; SLA and budget *notifications* not started |
| **E. Review loop** | **Mostly done** | Issue bind, `/comments`+`/address`, map→GH review request; radar optional |
| **F. Support / cases** | **Done** | Discord + web case lifecycle |
| **G. IDE-free (Wave 2)** | **Done** | Checkpoint, verify, sync, decisions |
| **H. Linear bridge** | **L1 done** | L2–L4 open |
| **I. Slash (optional)** | Open | Channel-scoped registration |
| **J. Sandbox / Tier B identity** | Design | Separate trains |

---

## Explicit non-goals (for now)

- Multi-agent debate or multiple Grok processes per thread
- In-chat project switching (channel map stays source of truth)
- Replacing GitHub PR review / branch protection
- Bot auto-merge (unless a future role-gated design lands)
- Full Linear/Jira field-level two-way sync
- Replacing Linear’s native GitHub PR status automation
- Multi-tenant hard isolation between hostile coworkers
- Auth-heavy **public** web app (web stays private; team UX in Discord + private admin UI)
- Slash commands that appear in every channel of the server

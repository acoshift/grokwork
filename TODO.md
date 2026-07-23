# TODO

Feature backlog for **Grok Work** (`grokwork`): Discord-first team development workflow + private web UI.

Order is suggested priority, not a commitment. **Code on `main` wins** if this file and a design doc disagree.

**Command surface:** keep **@Grok + text commands** as the primary UX. Native Discord slash commands stay demoted (see Later) — registration is guild-wide by default and needs channel-permission sync to avoid showing in unmapped channels.

**Related:** `docs/design-agentic-team-runtime.md` (rev 7 status), `docs/roles/`, `docs/support-case-guide.md`, `docs/design-per-user-github-identity.md`, `docs/design-agent-sandbox.md`.

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

### Web UI (selected)

- [x] Project-first shell; ship board; sessions; worktrees; config; OAuth-optional web auth
- [x] Start task web; session continue / cancel / reset / label / goal / claim
- [x] Issues / Linear list / commits / PR detail / diff review / team PR reviews
- [x] Bulk fix, commit-review-as-session, markdown bodies, live SSE regions

### Linear L1

- [x] Parse/bind `ENG-123` / URLs; GraphQL resolve; session fields; `/link`; prompt + PR identifier convention
- [x] Per-project Linear config (+ env key suffix)

---

## Next (recommended order)

### 1. Attribution trailers (Tier A) — **not started**

See `docs/design-per-user-github-identity.md` Tier A. **Host still pushes/opens PRs.**

- [ ] Discord user → GitHub login map (config and/or web)
- [ ] Commit trailers / optional `GIT_AUTHOR_*` (prompter + Co-authored-by / noreply email)
- [ ] PR body footer: Discord prompter, mapped `@login`, thread URL, session id
- [ ] Web comment prefix “On behalf of …” when map exists
- [ ] Optional: use map for `/review @user` → GitHub review request

### 2. Governance depth — **partial**

| Item | Status |
|------|--------|
| Web auth + feature flags + project visibility | **Partial** — OAuth optional; config admin-gated when auth on; not forced for all deploys |
| Layer A env filter | **Done** |
| Layer B full env allowlist (per-project / host flag) | **Not started** |
| Audit log (web mutations, case/session actions) | **Partial** — not full tool/run/capability trail |
| Rate limits + concurrency caps | **Partial** — some start rate limits; host/user concurrent-run product incomplete |
| OS sandbox for Grok children | **Design only** — `docs/design-agent-sandbox.md` |

### 3. Team DX leftovers — **partial / open**

- [ ] **Watchers** — `@Grok /watch` or 👀; mention once on complete/fail
- [ ] **Notification hygiene** — `notifyOnDone: never | errors | always | long_only`
- [ ] **Discord `/review` depth** — GitHub review-request + optional `#code-review` radar (request map already exists)
- [ ] **`/rerequest` / re-review** after address (if still desired)
- [ ] **Path scope (monorepo)** — `/scope api/`; warn if diff escapes
- [ ] **Project conventions blurb** — config or repo `GROK_DISCORD.md` (capped); `/conventions`
- [ ] **Worktree fleet in Discord** — `/worktrees` list (web worktrees page already exists)
- [ ] **autoCheckpoint** before fix runs (opt-in)

### 4. Linear L2+ (still open)

- [ ] L2 — Discord thread attachment on Linear issue; refresh on run/PR; optional complete comment; `/linear comment`; optional `/linear new`
- [ ] L3 — inbound Linear webhooks → Discord notify / brief refresh
- [ ] L4 — Linear Agent (Developer Preview; later)

### 5. Safety beyond minimum

- [ ] Tiered tool policy (safe auto / notify / Discord approve)
- [ ] Secrets hygiene (redact stream/history; high-entropy pre-push warn)
- [ ] Push/PR gate modes (`auto | propose | owner-only`)
- [ ] Plan → approve → implement preset + buttons

### 6. Wave 4 power (deferred)

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
| **C. Safe team mode** | **Mostly done** | Caps/modes/env Layer A shipped; **attribution + audit depth + Layer B** remain |
| **D. Team artifacts** | **Mostly done** | Brief, labels, board, action bar; templates/watchers optional |
| **E. Review loop** | **Mostly done** | Issue bind, `/comments`+`/address`; full `/review` radar optional |
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

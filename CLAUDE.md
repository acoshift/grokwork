# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

**Grok Work** (`grokwork`): a single Go process that bridges Discord and the `grok` CLI. Users tag `@Grok <task>` in a mapped channel; the bot runs Grok Build headless (`grok -p … --cwd <project>`) against a local checkout and streams the reply into a Discord thread. It also serves a private-network admin web UI (OAuth-optional) on `:8787` with a **project-first IA**: `/` is a project launcher; `/projects/{name}` is a per-project workspace (overview, ship, issues, Linear, commits, sessions, worktrees, settings); cross-project lead views (`/ship`, `/sessions`, `/worktrees`) and global `/config` remain in the global shell.

Module: `github.com/acoshift/grokwork`. Binary: `grokwork`. Env: `GROK_WORK_*` only.

## Commands

```bash
go build -o grokwork .                # binary
go build ./...                        # packages
go vet ./...                          # vet
go test ./...                         # full test suite (stdlib testing only, no external deps)
go test ./internal/bot -run TestName  # single test
go run .                              # run (needs config.json, see below)

# Visual review of web UI changes: real server on :18787 with seeded demo data.
# DELAY_MS adds artificial latency so loading states are observable.
GROKWORK_WEB_PREVIEW=1 [GROKWORK_WEB_PREVIEW_DELAY_MS=800] \
  go test ./internal/web -run TestPreviewServer -timeout 0
```

Running the bot requires `config.json` (copy `config.example.json`). Go 1.26.5+.

## Workflow

Multiple agents often work on this repo **in parallel**. Avoid editing the shared main checkout in place.

1. **Start in a git worktree** — create an isolated worktree from current `origin/main` (or local `main` after fetch) and do all file edits, builds, and tests there. Do not leave uncommitted WIP on the primary checkout where another agent may race you.
2. **Ship straight to `main`** — when done: `/scrutinize`, then **commit and push directly to `main`** (fast-forward or rebase onto latest `main` first if needed). Prefer no long-lived feature branches / open PRs for routine agent work — parallel agents stacking branches causes merge conflicts and stolen worktrees. Small, finished commits on `main` keep everyone unblocked.
3. After push, remove the temporary worktree (and its local branch if any) so the next agent does not inherit stale state.

**Caution:** `config.json` in the repo root is a real, gitignored config containing a live Discord token and private paths — never commit it or print its contents.

**Searching:** restrict searches to `main.go` and `internal/` — `data/` is runtime state (gitignored) and `data/worktrees/` contains full checkouts of *other* repositories that will pollute repo-wide grep results.

## Architecture

Wiring lives in `main.go`: `config.Load()` → `sessionstore.New` → `history.New` → `bot.New` → `web.New`. The bot and web UI share the same `*config.Config`, `*sessionstore.Store`, and `*history.Store` instances; web reads live bot state via `bot.StatusSnapshot()`.

### Core invariant (see TODO.md "Design principles")

**One Discord thread = one git worktree = one branch (`grokwork/<threadId>`, legacy `grok/discord/<threadId>` still managed) = one agent session.** All collaboration metadata (ownership, brief card, PR cards, queue) wraps that unit. The bot owns deterministic git/gh operations; Grok owns judgment. The bot never merges **GitHub PRs**. When a project has `directToPrimary` enabled, sessions stamp sticky `ShipMode=direct` and the bot may fast-forward a managed session branch onto the project primary and push (No-PR mode) — not `gh pr merge`.

### Message pipeline (`internal/bot`, the bulk of the code)

`onMessage` (bot.go) gates: not-a-bot → in-guild → mentions bot → resolve channel→project → **per-project** allowlist (fail-closed when that project’s user and role lists are both empty). Then `ParseMessage` (prompt.go) classifies into text commands (`/status`, `/reset`, `/cancel`, `/claim`, `/hand-off`, `/brief`, `/fix-ci`, …) vs `KindTask`. Text commands via `@Grok` mention are the deliberate primary UX — native Discord slash commands were intentionally rejected (TODO.md).

A task then flows through `handleTask` (async):
1. `resolveProject` — project comes **only** from the channel→project config map (parent channel when inside a thread); users can never switch projects in chat.
2. Thread creation + title (optionally one extra `grok` call to summarize, `grokrun.SummarizeTitle`).
3. Per-thread state machine — `threadState` in `Bot.states` (sync.Map) holds one active `runJob` + FIFO queue (max 5). `claimOrEnqueue`/`finishRun`/`replaceJob` are the only mutation points; queued follow-ups auto-run when the current run ends.
4. Worktree resolution (`internal/gitworktree`) — per-thread worktree under `data/worktrees/<project>/<threadId>` created from main checkout HEAD.
5. Prompt assembly — `remoteWorkPromptPrefixMode` (bot.go) injects the contract Grok must follow: PR mode = commit on the thread branch, push, open a PR via `gh`; direct mode = commit on the branch only, no PR (bot ships). Optional `DISCORD_UPLOAD:` block for artifacts. Attachments (attachments.go) and replied-to message context are appended.
6. `grokrun.Run` executes, streaming into Discord (stream.go): live-edited tail message, sealed chunks when >1900 chars (`maxMsg`), phase chips + activity from the session's `updates.jsonl` (grokrun/activity.go).
7. Post-run: completion summary card (completion.go — pure git, no model call), brief card refresh (brief.go), PR URL resolution → per-PR status cards + ~90s poller (pr_status.go), CI-failure digest / auto-fix (ci_triage.go), history log.

### Supporting packages

- `internal/grokrun` — execs the coding CLI. `Options.Agent` picks a **driver** (`driver.go`): `grok_driver.go` execs `grok -p` with the prompt in a temp file + `--verbatim` (never inline, to survive `#`/`?`/`&`), `claude_driver.go` execs `claude --print` with the prompt on **stdin** and no `--cwd` (`cmd.Dir` carries it). Run owns everything shared — process group, timeout, kill, Result assembly; drivers own only arg building and stream decoding. Streaming vs single-JSON output is chosen by whether streaming callbacks are set. grok learns tool activity by tailing `~/.grok/…/updates.jsonl` and context size from `signals.json`; claude gets both from its own stream, so those hooks are no-ops for it. Session creation differs and the difference bites on recovery: grok's `-s` adopts a caller-minted id, while claude needs `--session-id` to create and `--resume` to continue and **errors on the wrong one** — so `Run` flips the guess once when the driver recognizes either refusal (`sessionAlreadyExists` / `sessionMissing`), which is what makes crash recovery work for claude at all. Three things about claude's `result` event are easy to get wrong and are pinned by tests: the reply is in `result` only on `subtype=success` (every error subtype puts the reason in `errors[]`, so reading `result` alone loses the only diagnostic); the event's `usage` is a *cumulative* bill across all API calls, so context occupancy must come from the last `usage.iterations` entry or it overstates by roughly the turn count; and one run is several assistant messages, which need a paragraph break or they fuse mid-sentence. Tool payloads are absolute paths, so activity detail is made cwd-relative before it can reach Discord.
- **Coding agent selection** — a session runs on `grok` or `claude`, decided **only by global config** (`agent` + `model`); Discord users cannot choose or change it, and there is no chat command for it. The choice is stamped on `Entry.Agent` + `Entry.Model` when the session's **first run starts** (`bot.ensureSessionCLI`) and is immutable after that, because session IDs are not portable between CLIs and a mid-thread model change makes a thread answer inconsistently. Stamping at run *start* rather than run end is load-bearing: config is editable while the bot runs, so the global model can change between a run starting and its crash-recovery re-drive, and an unstamped thread would then be resolved against the *new* config and try to resume the old CLI's session id. `bot.threadCLI` is the single place that resolves this (stamped → `config.PinnedAgentCLI`, unstamped → `config.ResolveAgentCLI`). Entries with a session ID but no stamp predate the feature and stay on grok with the global model.
- **Model → agent inference** — `model` and `summarizeModel` are shared across agents: `grokrun.AgentForModel` decides which CLI a name belongs to (`grok*` → grok; `claude`/`anthropic`/`opus`/`sonnet`/`haiku`/`fable` → claude). An unrecognized name is **never guessed** — it falls back to `config.agent` and is passed through so the CLI reports the bad model itself. A model belonging to the other agent is dropped rather than passed, so that run uses its own CLI's default. The config page offers `grokrun.ModelOptions()` as a grouped dropdown (no free text); `TestModelOptionsMatchInference` pins the list against the inference table so the two cannot drift. `summarizeModel` may name the other CLI even on a pinned thread — a title is one tools-off turn whose session ID is discarded, so nothing about the thread depends on who wrote it, and crossing CLIs is what lets an expensive thread get a cheap title. A failed title call just keeps the locally derived name. Extra args and **tool names** stay per-agent: a project `investigateTools` override written for grok is ignored for claude, which would otherwise get an allowlist of zero real tools and be unable to read the repo.
- `internal/sessionstore` — `data/sessions.json`; `Entry` per thread (session ID, agent, cwd, branch, owner/co-owners, goal, tracked PRs). Multi-PR list `PRs` is the source of truth; legacy single-PR fields are mirrored for old data — call `NormalizePRs()` before reading PR state. Mutate via `Patch` (load-apply-save under one lock).
- `internal/gitworktree` — only managed branch prefixes may ever be deleted (`IsManagedBranch`: `grokwork/`, legacy `grok/discord/`, `grok/web/`); cleanup triggers are PR merged/closed (all tracked PRs terminal) and idle TTL (daily sweep, skips running/queued threads). Terminal-PR cleanup frees the worktree but **keeps the session entry** (PR links + final label/state for the sessions UI); idle TTL also keeps sessions that still have PRs, terminal labels, or are cases.
- `internal/ghpr` — `gh` CLI wrapper (PR state/checks/reviews, issue read/create) plus `git log`/`show` parsing for the commits browser and diff rendering.
- Commit review (web) — `bot.StartCommitReview` opens a new Discord thread (or web-native unit) and runs a normal Grok session; the model agentically inspects the commit and files GitHub issues via `gh` (labels, commit links). No separate `commitreview` job store.
- `internal/config` — mutable at runtime: the web config pages edit and persist `config.json` while the bot runs, hence the RWMutex + `Snapshot()` accessors. Tri-state fields use pointers (`*bool`, `*int`) to distinguish "unset → default" from explicit false/0 (e.g. `Yolo` nil means true, `WorktreeIdleTTLDays` nil means 30 but 0 disables) — preserve this pattern when adding config.
- `internal/web` — hime (v1.6+ htmx helpers: `View("page#fragment")`, `HTMXAwareRedirect`, `NoCache`) + embedded `html/template` + stdlib SSE. Live-region endpoints render named template defines; full pages use the layout root. Shutdown is tuned to not wait for open SSE streams (`GraceTimeout = 1ms`); `live_test.go` boots the real TCP listener. See "Web UI conventions" below before touching templates.
- `internal/audit` — JSONL audit log (daily files under `data/audit/`) for web-initiated mutations (config writes, prunes, PR actions, commit reviews).
- `internal/history` — per-turn JSON log per thread under `data/history/`, feeds the web history views.

### Web UI conventions (`internal/web/templates`)

- `layout.tmpl` owns the entire design system: Grok monochrome HSL tokens (light default, dark via `prefers-color-scheme`), sidebar shell, all component CSS, and the shared scripts (SSE status, nav sync, copy buttons, `data-autosubmit` selects, page-loader bar, submit-button double-click guard, `data-confirm` modal). Pages contain content only. Never use `window.confirm`/`alert` — boosted forms ignore `onsubmit` cancellation (htmx never checks `defaultPrevented`); put `data-confirm` (+ optional `data-confirm-title`/`-ok`/`-danger`) on the form or a specific submit button, or call `appConfirm`/`appAlert` (Promise-based) from script.
- **SPA shell contract:** `hx-boost` on the shell swaps only `#live-root`, so the SSE `EventSource` survives navigation. htmx runs with `disableInheritance=true` + `hx-inherit="*"` on the shell. SSE-refreshed regions must keep exactly `class="live-region"` with `hx-target="this" hx-select="unset"` — anything else lets a partial wipe the page.
- **Project-first shell:** the sidebar (`#side-nav`, outside `#live-root`) renders global or workspace mode and is re-swapped on every boosted navigation via `hx-select-oob="#side-nav"` on the shell. The mode is **URL-derived only** — `navScopeFromURL` (project_home.go) and `scopeFromLocation()` (layout.tmpl JS) must stay mirrors: path scopes `/projects/{p}…` and `/config/projects/{p}`; `?project=` scopes only `/sessions/{id…}` and `/prs/…` detail pages. Never derive shell scope from page data. Retired hubs `/issues` and `/commits` redirect to `/`.
- **Scoped fragments:** ship live regions pass `&scoped=1` so SSE refreshes keep the workspace layout (no Project column) — the global board uses `?project=` as a data filter and must keep the column; worktrees regions scope with plain `?project=`. `TestShipPartialScopedLayout` pins this.
- **Diff review** (commit detail, session diff, PR diff — diffreview.go + `diff_review.tmpl`): pages render only a file index (`ghpr.DiffIndex` — numstat/name-status for local git, `StatPatch` scan for PRs); hunks stream per file from `…/file?path=` fragment endpoints with per-file caps (`ghpr.FileCaps`). Normal files lazy-load via `hx-trigger="intersect once"`; large (>500 changed lines), generated, deleted, and binary files gate behind a click. PR patches go through `Server.prPatch` (60s TTL cache) so fragments don't each re-run `gh pr diff`. Viewed-tracking/filter/`j k v o` keys live in layout.tmpl's diff-review script keyed off `#diff-review[data-review-key]` (localStorage only, no server state).
- **`web_test.go` pins markup byte-for-byte.** Notably: every page needs its `id="page-*"` marker; nav anchors must render `class="{{if .IsX}}active{{end}}">Label</a>` (class attribute last, bare label — `assertNavActive` matches the contiguous string; icons come from `data-icon`, placed before `class`); partials must not contain layout chrome (`<nav`, `sse-status`, the htmx script). Check the tests before renaming anything in a template.
- Per-project settings live at `/config/projects/{name}`; project-scoped POSTs redirect back there via `projectConfigRedirect`, and channel map forms submitted from that page carry `return_to=project`. The `/config` hub keeps only global settings.
- Local project paths may appear in the web UI (private network) but must never leak into Discord messages.

### Discord-facing conventions

- Message cap is 1900 chars (`maxMsg`); long output is chunked/sealed.
- Local project paths must never leak into Discord messages.
- Ownership: first `@Grok` author owns the thread; `/cancel` and `/reset` require owner, co-owner, or a Discord mod (Admin / Manage Messages / Manage Threads); anyone on that **project’s** allowlist may queue tasks.
- Project members: `projects.<name>.allowedUserIds` / `allowedRoleIds`. Web UI filters projects by user ID membership (admins see all).

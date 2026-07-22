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

- Before marking any task done, run `/scrutinize` on the change and address what it finds.
- Then always commit and push to `main`.

**Caution:** `config.json` in the repo root is a real, gitignored config containing a live Discord token and private paths — never commit it or print its contents.

**Searching:** restrict searches to `main.go` and `internal/` — `data/` is runtime state (gitignored) and `data/worktrees/` contains full checkouts of *other* repositories that will pollute repo-wide grep results.

## Architecture

Wiring lives in `main.go`: `config.Load()` → `sessionstore.New` → `history.New` → `bot.New` → `web.New`. The bot and web UI share the same `*config.Config`, `*sessionstore.Store`, and `*history.Store` instances; web reads live bot state via `bot.StatusSnapshot()`.

### Core invariant (see TODO.md "Design principles")

**One Discord thread = one git worktree = one branch (`grok/discord/<threadId>`) = one Grok session.** All collaboration metadata (ownership, brief card, PR cards, queue) wraps that unit. The bot owns deterministic git/gh operations; Grok owns judgment. The bot never merges **GitHub PRs**. When a project has `directToPrimary` enabled, sessions stamp sticky `ShipMode=direct` and the bot may fast-forward a managed session branch onto the project primary and push (No-PR mode) — not `gh pr merge`.

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

- `internal/grokrun` — execs `grok -p`; prompt passed via temp file + `--verbatim` (never inline, to survive `#`/`?`/`&`); `json` vs `streaming-json` output chosen by whether streaming callbacks are set.
- `internal/sessionstore` — `data/sessions.json`; `Entry` per thread (session ID, cwd, branch, owner/co-owners, goal, tracked PRs). Multi-PR list `PRs` is the source of truth; legacy single-PR fields are mirrored for old data — call `NormalizePRs()` before reading PR state. Mutate via `Patch` (load-apply-save under one lock).
- `internal/gitworktree` — only branches matching `grok/discord/` prefix may ever be deleted (`IsManagedBranch`); cleanup triggers are PR merged/closed (all tracked PRs terminal) and idle TTL (daily sweep, skips running/queued threads).
- `internal/ghpr` — `gh` CLI wrapper (PR state/checks/reviews, issue read/create) plus `git log`/`show` parsing for the commits browser and diff rendering.
- Commit review (web) — `bot.StartCommitReview` opens a new Discord thread (or web-native unit) and runs a normal Grok session; the model agentically inspects the commit and files GitHub issues via `gh` (labels, commit links). No separate `commitreview` job store.
- `internal/config` — mutable at runtime: the web config pages edit and persist `config.json` while the bot runs, hence the RWMutex + `Snapshot()` accessors. Tri-state fields use pointers (`*bool`, `*int`) to distinguish "unset → default" from explicit false/0 (e.g. `Yolo` nil means true, `WorktreeIdleTTLDays` nil means 30 but 0 disables) — preserve this pattern when adding config.
- `internal/web` — hime (v1.6+ htmx helpers: `View("page#fragment")`, `HTMXAwareRedirect`, `NoCache`) + embedded `html/template` + stdlib SSE. Live-region endpoints render named template defines; full pages use the layout root. Shutdown is tuned to not wait for open SSE streams (`GraceTimeout = 1ms`); `live_test.go` boots the real TCP listener. See "Web UI conventions" below before touching templates.
- `internal/audit` — JSONL audit log (daily files under `data/audit/`) for web-initiated mutations (config writes, prunes, PR actions, commit reviews).
- `internal/history` — per-turn JSON log per thread under `data/history/`, feeds the web history views.

### Web UI conventions (`internal/web/templates`)

- `layout.tmpl` owns the entire design system: Grok monochrome HSL tokens (light default, dark via `prefers-color-scheme`), sidebar shell, all component CSS, and the shared scripts (SSE status, nav sync, copy buttons, `data-autosubmit` selects, page-loader bar, submit-button double-click guard). Pages contain content only.
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

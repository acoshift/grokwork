# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

**Grok Work** (`grokwork`): a single Go process that bridges Discord and the `grok` CLI. Users tag `@Grok <task>` in a mapped channel; the bot runs Grok Build headless (`grok -p … --cwd <project>`) against a local checkout and streams the reply into a Discord thread. It also serves a private-network admin web UI (OAuth-optional; dashboard, ship, issues, sessions, worktrees, config) on `:8787`.

Module: `github.com/acoshift/grokwork`. Binary: `grokwork`. Env prefers `GROK_WORK_*` with legacy `GROK_DISCORD_*` still accepted.

## Commands

```bash
go build -o grokwork .                # binary
go build ./...                        # packages
go vet ./...                          # vet
go test ./...                         # full test suite (stdlib testing only, no external deps)
go test ./internal/bot -run TestName  # single test
go run .                              # run (needs config.json, see below)
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

**One Discord thread = one git worktree = one branch (`grok/discord/<threadId>`) = one Grok session.** All collaboration metadata (ownership, brief card, PR cards, queue) wraps that unit. The bot owns deterministic git/gh operations; Grok owns judgment. The bot never merges PRs.

### Message pipeline (`internal/bot`, the bulk of the code)

`onMessage` (bot.go) gates: not-a-bot → in-guild → mentions bot → allowlist (fail-closed when both user and role lists are empty). Then `ParseMessage` (prompt.go) classifies into text commands (`/status`, `/reset`, `/cancel`, `/claim`, `/hand-off`, `/brief`, `/fix-ci`, …) vs `KindTask`. Text commands via `@Grok` mention are the deliberate primary UX — native Discord slash commands were intentionally rejected (TODO.md).

A task then flows through `handleTask` (async):
1. `resolveProject` — project comes **only** from the channel→project config map (parent channel when inside a thread); users can never switch projects in chat.
2. Thread creation + title (optionally one extra `grok` call to summarize, `grokrun.SummarizeTitle`).
3. Per-thread state machine — `threadState` in `Bot.states` (sync.Map) holds one active `runJob` + FIFO queue (max 5). `claimOrEnqueue`/`finishRun`/`replaceJob` are the only mutation points; queued follow-ups auto-run when the current run ends.
4. Worktree resolution (`internal/gitworktree`) — per-thread worktree under `data/worktrees/<project>/<threadId>` created from main checkout HEAD.
5. Prompt assembly — `remoteWorkPromptPrefix` (bot.go) injects the contract Grok must follow: commit on the thread branch only, push, open a PR via `gh`, include the PR URL, optional `DISCORD_UPLOAD:` block for artifacts. Attachments (attachments.go) and replied-to message context are appended.
6. `grokrun.Run` executes, streaming into Discord (stream.go): live-edited tail message, sealed chunks when >1900 chars (`maxMsg`), phase chips + activity from the session's `updates.jsonl` (grokrun/activity.go).
7. Post-run: completion summary card (completion.go — pure git, no model call), brief card refresh (brief.go), PR URL resolution → per-PR status cards + ~90s poller (pr_status.go), CI-failure digest / auto-fix (ci_triage.go), history log.

### Supporting packages

- `internal/grokrun` — execs `grok -p`; prompt passed via temp file + `--verbatim` (never inline, to survive `#`/`?`/`&`); `json` vs `streaming-json` output chosen by whether streaming callbacks are set.
- `internal/sessionstore` — `data/sessions.json`; `Entry` per thread (session ID, cwd, branch, owner/co-owners, goal, tracked PRs). Multi-PR list `PRs` is the source of truth; legacy single-PR fields are mirrored for old data — call `NormalizePRs()` before reading PR state. Mutate via `Patch` (load-apply-save under one lock).
- `internal/gitworktree` — only branches matching `grok/discord/` prefix may ever be deleted (`IsManagedBranch`); cleanup triggers are PR merged/closed (all tracked PRs terminal) and idle TTL (daily sweep, skips running/queued threads).
- `internal/ghpr` — `gh` CLI wrapper for PR state/checks/reviews.
- `internal/config` — mutable at runtime: the web config page edits and persists `config.json` while the bot runs, hence the RWMutex + `Snapshot()` accessors. Tri-state fields use pointers (`*bool`, `*int`) to distinguish "unset → default" from explicit false/0 (e.g. `Yolo` nil means true, `WorktreeIdleTTLDays` nil means 30 but 0 disables) — preserve this pattern when adding config.
- `internal/web` — hime (v1.6+ htmx helpers: `View("page#fragment")`, `HTMXAwareRedirect`, `NoCache`) + embedded `html/template` + stdlib SSE. Live-region endpoints render named template defines; full pages use the layout root. Shutdown is tuned to not wait for open SSE streams (`GraceTimeout = 1ms`); `live_test.go` boots the real TCP listener.
- `internal/history` — per-turn JSON log per thread under `data/history/`, feeds the web history views.

### Discord-facing conventions

- Message cap is 1900 chars (`maxMsg`); long output is chunked/sealed.
- Local project paths must never leak into Discord messages.
- Ownership: first `@Grok` author owns the thread; `/cancel` and `/reset` require owner, co-owner, or a Discord mod (Admin / Manage Messages / Manage Threads); anyone allowlisted may queue tasks.

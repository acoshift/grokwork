# TODO

Feature backlog for grok-discord. Order is suggested priority, not a commitment.

## Done

- [x] Channel → project mapping, allowlist, thread sessions
- [x] Commands: `/help`, `/projects`, `/reset`, `/status`
- [x] Grok-named Discord thread titles
- [x] Hide local project paths from Discord messages
- [x] Live progress heartbeats + `/cancel` (aliases: `cancel`, `/stop`, `stop`)
- [x] Discord attachments → prompt context (download, path list, cleanup)

## Next

### 1. Per-thread git worktree isolation

Critical for multi-user YOLO on one machine: concurrent threads on the same project currently share cwd and can stomp each other.

- Create a worktree (or temp branch) per Discord thread/run
- Point grok `--cwd` at the worktree
- Cleanup policy on thread archive / idle / `/reset`

### 2. Native Discord slash commands

Replace (or complement) mention + text parse with application commands.

- Register `/grok`, `/cancel`, `/status`, `/projects`, `/reset`, `/help`
- Keep mention path for compatibility during migration

### 3. True streaming of Grok output

Best long-run UX beyond heartbeats; depends on headless `grok` emitting incremental events.

- Stream/parse progressive stdout if the CLI supports it
- Post or edit partial assistant text in the thread
- Fall back to heartbeats + final reply when streaming is unavailable

## Later / nice-to-have

- [ ] Queue follow-ups when a thread is busy (instead of reject)
- [ ] `/model` or per-channel model override
- [ ] Rate limiting per user
- [ ] Optional non-yolo / approval gate for destructive tools
- [ ] Summarize final git diff in the completion message

# Role: builder

**Capability template** for engineering.  
Builtin flags: `investigate`, `startSessions`, `githubWrites`.

`CanShip()` is true (start + github writes). This is the standard eng persona under Safe Team Mode when explicitly mapped.

**Not** included on the builtin: `draftCustomerReply`, `fileEscalation`, `approve`, `merge`, `adminProject`.  
Escalation and customer-draft gates still succeed for builders via **builder-class** rules (`startSessions` / `githubWrites` count as escalate/draft-capable for case commands).

---

## Who this is for

- Software engineers implementing fixes  
- Anyone who should open PRs (or direct-ship when the project allows) from Discord/web sessions  
- Eng roles that must be **mapped before Safe Team Mode** is enabled on a shared project  

---

## What they can do

- Start **fix** sessions: freeform `@Grok <task>`, `/start fix <task>`  
- **Investigate** and **explain** without shipping: `/investigate`, `/start investigate|explain`  
- Commit on the thread branch, push, open PRs via the bot’s eng contract (PR mode)  
- **Direct ship** when the project/session is in No-PR / direct-to-primary mode (bot ships; never `gh pr merge`)  
- `/fix-ci`, `/address` (review comments), `/sync`, `/checkpoint` (ship-class)  
- `/verify`, `/comments`, `/link`, `/review @user`, labels, board  
- On **cases** (after escalate or while collaborating): implement in phase `fixing` with ship policy when caps allow  
- Escalate / answer / customer-update on cases via builder-class gates (even without the draft/escalate flag bits)  
- Claim ownership, queue tasks, cancel/reset when owner/co/mod  

---

## What they cannot do

| Action | Notes |
|--------|-------|
| Builtin **merge** capability flag | Only on **admin** template; Discord bot still **never** merges GitHub PRs |
| Host **web merge** button | Separate: needs web **member+** + feature `merge` (not the Discord template alone) |
| Config UI / global web admin | Needs **web admin** role, not Discord `builder` |
| Approve-only gates that require `approve` without CanShip | Builders pass many gates via `CanShip()`; pure `approve` is for approver/admin |

Reserved flags (`requestChange`, `safeOps`) have no command gates yet.

---

## Primary Discord commands

Always `@Grok …` first.

| Command | Purpose |
|---------|---------|
| `@Grok <task>` | Freeform fix (empty mode + ship caps → fix policy) |
| `/start fix\|investigate\|explain <task>` | Explicit mode |
| `/investigate <task>` | Read-only dig |
| `/fix-ci` | Queue CI fix on PR branch |
| `/address` | Address unresolved review comments |
| `/sync` | Merge origin primary into thread branch |
| `/checkpoint` · `/undo` · `/restore` | Local git checkpoints |
| `/verify [name]` | Project verify commands |
| `/comments` | Unresolved PR review comments |
| `/link` · `/unlink` | Bind GitHub/Linear issues |
| `/review @user` | Request team review |
| `/status` · `/brief` · `/board` · `/label` | Continuity |
| `/claim` · `/hand-off` · `/cancel` · `/reset` · queue cmds | Ownership / lifecycle |
| Case cmds when collaborating | `/escalate`, `/answer`, `/customer-update`, `/close`, `/board cases` |

Full help: `@Grok /help`. Case detail: [support-case-guide.md](../support-case-guide.md).

---

## Web UI surfaces

With OAuth **member+** (or admin) and host features:

| Feature flag | Surfaces |
|--------------|----------|
| `startSessions` | Start task composer, continue session, fix-with-Grok, address CI/review, commit review, case POSTs, session cancel/reset/claim |
| `githubWrites` | Issue/PR comment and close |
| `merge` | Merge PR (method from web config; default squash) |
| `prReviews` | Team review submit / request / cancel |

Also: project **Ship** board, sessions, commits, issues, Linear (if enabled), worktrees, history.

Project visibility: Discord user id must be on that project’s `allowedUserIds` (web **admin** sees all projects).

Web capability resolution for start/case uses **user id** only—map builders in `capabilityByUser` if they use web without Discord role context.

---

## Typical workflow

1. In an eng channel (or escalated case thread):  
   `@Grok Fix null deref in checkout webhook when Stripe omits order_id`
2. Bot creates/uses thread + worktree, runs Grok, opens PR (PR mode) or prepares direct ship.  
3. Watch checks; on failure: `@Grok /fix-ci` or freeform follow-up.  
4. Review loop: `@Grok /comments` → `/address` as needed.  
5. Human merges on GitHub (or uses web merge if feature enabled).  

**From a support case:** support escalates; eng in the same thread:  
`@Grok /start fix …` or freeform with ship caps while phase is `fixing`.

---

## Common errors

| Bot says… | What to do |
|-----------|------------|
| Coerced to investigate / no PR | Caps missing `githubWrites` (mis-mapped as investigator) — admin must map to `builder` |
| “You're not allowed to start fix tasks…” | No start/github/investigate path |
| “You're not allowed to `/sync` (need builder caps or thread control).” | Not CanShip and not thread owner/co/mod |
| “You're not allowed to `/address`…” | Same as sync |
| Demoted after Safe Team Mode on | Unmapped eng → default investigator — map role/user to `builder` |
| Web POST 404 “not found” | Host feature flag off or auth off (features fail closed) |
| Web 403 member required | Web role is **viewer** only |

---

## How an admin assigns this role

1. Project allowlist eng Discord roles/users.  
2. **Capability maps:** role ID or user ID → **`builder`**.  
3. Enable **Safe Team Mode** only **after** eng maps exist.  
4. Optional: leave project default mode empty (legacy fix) for eng channels; use `case` only for support projects.  
5. For web-first eng: ensure Discord user id is in project `allowedUserIds` (or webAuth member/admin lists) and map `capabilityByUser` → `builder`.

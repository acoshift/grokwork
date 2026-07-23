# Grok Work roles

Operator-facing guide to **who can do what** in Grok Work (`grokwork`).

There are two separate permission systems:

| Layer | Where it applies | What it controls |
|-------|------------------|------------------|
| **Capability templates** (Safe Team Mode) | Discord bot (and web case/start checks that re-resolve caps) | Investigate vs ship, escalate, customer draft, approve, project-admin flags |
| **Web roles** | Private web UI when Discord OAuth is enabled | Login, project visibility, config access, host-level write feature flags |

Membership still comes first: a user must be on the project’s Discord allowlist (`allowedUserIds` / `allowedRoleIds`) before capability templates apply. Empty allowlists are fail-closed for that project.

---

## Capability templates (Discord / project membership)

| Role | Audience | One-line purpose | Doc |
|------|----------|------------------|-----|
| **investigator** | Support / CS | Open cases, investigate read-only, draft customer replies, escalate to eng | [investigator.md](./investigator.md) |
| **operator** | Support / triage | Investigate and draft customer replies; **cannot escalate** | [operator.md](./operator.md) |
| **builder** | Engineering | Investigate + start fix sessions with GitHub writes (PR / direct ship when project allows) | [builder.md](./builder.md) |
| **approver** | Senior eng / tech lead | Builder-class ship + case draft/escalate + approve-class gates | [approver.md](./approver.md) |
| **admin** | Project admin | Full project capability set (ship, merge flag, approve, adminProject) | [admin.md](./admin.md) |

Built-in flag matrix (source: `internal/config/capabilities.go`):

| Flag | investigator | operator | builder | approver | admin |
|------|:------------:|:--------:|:-------:|:--------:|:-----:|
| `investigate` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `draftCustomerReply` | ✓ | ✓ | | ✓ | ✓ |
| `fileEscalation` | ✓ | | | ✓ | ✓ |
| `startSessions` | | | ✓ | ✓ | ✓ |
| `githubWrites` | | | ✓ | ✓ | ✓ |
| `approve` | | | | ✓ | ✓ |
| `merge` | | | | | ✓ |
| `adminProject` | | | | | ✓ |

`RequestChange` and `SafeOps` exist on the struct but have **no command gates** yet (reserved).

**Ship rule:** PR/direct ship requires **both** `startSessions` and `githubWrites` (`CanShip()`). Missing `githubWrites` coerces the run to investigate-only (never half-fix).

---

## Unmapped users and Safe Team Mode

| Safe Team Mode | Unmapped allowlisted user | Effective template |
|----------------|---------------------------|--------------------|
| **On** | No `capabilityByUser` / `capabilityByRole` hit | `safeTeamDefaultTemplate` (**default `investigator`**) |
| **Off** / unset | Same | Builtin **`builder`** (legacy eng-only deploys) |

Details and rollout warnings: [safe-team-unmapped.md](./safe-team-unmapped.md).

**Rollout:** map eng Discord roles → `builder` (or higher) **before** enabling Safe Team Mode, or engineers are demoted immediately—including already-queued tasks.

Configure under web: **Config → project → Safe team mode** and **Capability maps**.

---

## Web roles (OAuth)

| Role | Audience | One-line purpose | Doc |
|------|----------|------------------|-----|
| **viewer** | Read-only stakeholders | Browse UI; no write POSTs | [web-viewer.md](./web-viewer.md) |
| **member** | Day-to-day operators | Writes when host feature flags are on | [web-member.md](./web-member.md) |
| **admin** | Host operators | Config UI + all member capabilities | [web-admin.md](./web-admin.md) |

Web write features (host-level, fail-closed when OAuth is off):

- `startSessions` — start tasks, case intake, session controls, case phase POSTs  
- `githubWrites` — issue/PR comments and close  
- `merge` — merge PR from web  
- `prReviews` — team PR review actions  

Resolution order for Discord user id: **admin list → member list → viewer list → any project `allowedUserIds` → deny**. Project role IDs alone do **not** grant web login membership; put eng/support user snowflakes on project allowlists (or webAuth lists).

Web capability checks for case open/escalate resolve with **user id only** (no Discord guild role list). Prefer `capabilityByUser` for people who work primarily from the web.

---

## Related docs

- [Support case guide](../support-case-guide.md) — Discord case command detail for support  
- Design background: `docs/design-agentic-team-runtime.md` (code wins if design and code disagree)

---

## Mental model for leadership

Grok Work ties **one Discord thread = one worktree = one session**. Safe Team Mode splits **support** (cases, investigate, customer draft) from **engineering** (fix sessions that can open PRs or direct-ship). The web UI adds a second axis—viewer / member / admin—so private-network ops can browse safely while host feature flags gate destructive GitHub actions. Capabilities fail closed; unmapped users under Safe Team Mode become investigators by default, not builders.

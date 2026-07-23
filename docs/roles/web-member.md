# Web role: member

OAuth web role `member` (`config.WebRoleMember`).  
Day-to-day operator of the private UI: can perform write actions when **host feature flags** are enabled.

---

## Who this is for

- Engineers and support staff who start tasks, open cases, comment on PRs/issues, or submit team reviews from the browser  
- Anyone on a project `allowedUserIds` list (auto-resolves to **member** if not on admin/viewer lists)  

Resolution order: admin → member → viewer → **any project allowedUserIds → member** → deny.

---

## What chrome they see

- Same browse surface as viewer for **visible** projects  
- Write affordances when features are on: start composer, case new, ship merge (if `merge`), review forms, issue/PR actions, session controls  
- Case panel actions gated further by **project capability templates** (investigate / escalate / draft)  
- **No** global Config shell (admin only), unless they are also web admin  

---

## Feature flags (host-level)

Configured under `webAuth.features`. All are **false** when OAuth is disabled (fail-closed open LAN).

| Flag | Enables |
|------|---------|
| `startSessions` | POST start task, new case, fix-with-Grok, address CI/review, continue, session cancel/reset/dequeue/label/goal/claim, case phase POSTs, commit review |
| `githubWrites` | Issue/PR comment and close |
| `merge` | Merge PR (method: squash/merge/rebase from web config; default squash) |
| `prReviews` | Submit team review, request review, cancel request |

UI flags require **member+** role **and** the feature bit.

---

## Project capability interaction

Member is necessary but not always sufficient:

| Action | Extra gate |
|--------|------------|
| Open case (form + POST) | `Investigate \|\| FileEscalation \|\| StartSessions` via `ResolveCapabilities(project, userID, nil)` |
| Escalate / draft on case panel | Shared Discord rules (`CanEscalateCaseCaps` / `CanDraftCaseCaps`) |
| Ship from Grok run | Session runs still apply Discord-style caps (githubWrites for PR policy) |
| Control session buttons | Feature + member + ownership/admin control check |

**Role maps do not apply on web** (nil role IDs). Map active web users in `capabilityByUser`.

Under Safe Team Mode, unmapped members default to **investigator** (or configured default)—they can open cases if investigate is present, but freeform fix will not ship.

---

## Project visibility

Same as viewer: only projects listing their Discord user id (unless they are web admin).  
Being web member without project allowlist → may log in via member list but see no projects.

---

## Relation to Discord

| Discord | Web member |
|---------|------------|
| Allowlist + capability template | Controls `@Grok` in mapped channels |
| Same Discord user id | Used for OAuth identity and cap resolution |
| Builder template | Needed for eng ship policy on runs started from web |
| Investigator template | Support case path from web |

Web member does **not** replace Discord allowlist for bot chat, and does **not** grant Config access.

---

## Typical workflows

**Eng:** open project → Start task → mode fix → run → watch session → ship board / PR page → review or merge if features on.

**Support:** Cases → New case → investigate from session panel → answer / customer-update → close (if control allowed) or escalate.

---

## Common errors

| Symptom | Cause |
|---------|-------|
| POST 404 not found | Feature flag off or auth off |
| 403 member required | Actually viewer/none (should not happen for true member) |
| 403 not allowed to open cases | Caps: Safe Team default without investigate, or zero caps |
| Start works but no PR | User mapped as investigator under Safe Team |
| Cannot see project | User id missing from that project’s `allowedUserIds` |
| CSRF forbidden | Stale page / missing token — reload |

---

## How an admin assigns this role

1. Add Discord id to `webAuth.memberDiscordIds`, **or** rely on project `allowedUserIds` auto-member.  
2. Enable the feature flags they need.  
3. Map capability templates for Discord + `capabilityByUser` for web-accurate caps.  
4. Keep eng on `builder` under Safe Team Mode.

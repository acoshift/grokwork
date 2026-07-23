# Web role: viewer

OAuth web role `viewer` (`config.WebRoleViewer`).  
Read-oriented access to the private admin UI when Discord OAuth is enabled.

---

## Who this is for

- Stakeholders who should **browse** sessions, ship status, cases, and history  
- People who must not trigger GitHub writes, merges, or start Grok runs from the web  

Assigned via `webAuth.viewerDiscordIds` (only if not already admin/member — resolution prefers higher lists first).

---

## What chrome they see

- Global launcher `/` and project workspaces they can access  
- Sessions, ship board, cases (read), commits, issues, Linear (if enabled), worktrees, history, PR detail, reviews (read)  
- Live SSE status regions  

Write CTAs are suppressed: `CanGitHubWrite`, `CanMerge`, `CanStartSession`, `CanPRReview` are forced **false** for roles below member.

**Config** nav and `/config` require web **admin** — viewers get 403 if they hit those routes.

---

## What POSTs / features work

| Action | Viewer |
|--------|--------|
| Login / browse GETs | Yes (if project visible) |
| Start session / new case / continue / fix-with-Grok | **No** (member required + feature) |
| Issue/PR comment, close | **No** |
| Merge PR | **No** |
| Team PR review submit | **No** |
| Session cancel/reset/claim | **No** |
| Case escalate/answer/close from web | **No** |
| Config mutations | **No** |

Handlers return **403** `member required` (or 404 when feature flags are off).

---

## Project visibility

- **Not** web admin → only projects that list the user’s Discord id in `projects.*.allowedUserIds`  
- Discord **role** membership on the bot allowlist does **not** by itself expand web project lists  
- If the user is only a viewer and not on any project user allowlist, the launcher may be empty  

Admins see all projects; members/viewers are filtered the same way for visibility.

---

## Relation to Discord allowlist and capability templates

| Layer | Viewer impact |
|-------|----------------|
| Discord bot | Independent — if allowlisted in Discord, they can still `@Grok` with their capability template |
| Web capability checks | Mostly irrelevant while write CTAs are off |
| Safe Team Mode | Affects Discord (and would affect web writes if they were member+) |

A support investigator often wants **web member** (not viewer) if they open cases from the UI, plus host feature `startSessions` and investigate caps on their user id.

---

## How an admin assigns this role

1. Enable `webAuth.enabled` with OAuth prerequisites.  
2. Add Discord user snowflake to `webAuth.viewerDiscordIds` (web config UI or `config.json`).  
3. Put their user id on each project’s `allowedUserIds` they should see.  
4. Do **not** also put them only on member/admin lists if you want pure viewer (higher lists win).

---

## Common errors

| Symptom | Cause |
|---------|-------|
| Login denied | Not on admin/member/viewer lists and not on any project `allowedUserIds` |
| Empty project list | Not on project user allowlists |
| Buttons missing | Expected for viewer |
| 403 on POST | Expected — need member+ |

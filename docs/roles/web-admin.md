# Web role: admin

OAuth web role `admin` (`config.WebRoleAdmin`).  
Host operator for the private Grok Work UI: full config access and all member-level writes (when features are on).

This is **independent** of the Discord capability template named [`admin`](./admin.md), though the same person often has both.

---

## Who this is for

- People who edit `config.json` through the UI (projects, channels, Safe Team, allowlists, webAuth feature flags)  
- Operators who must see **all** projects regardless of membership lists  
- Bootstrap: `GROK_WORK_BOOTSTRAP_ADMIN_DISCORD_ID` when `adminDiscordIds` is empty  

Listed in `webAuth.adminDiscordIds`. Resolution prefers admin over member/viewer.

---

## What chrome they see

- Everything members see, on **all** projects  
- **Config** hub (`/config`) and per-project settings (`/config/projects/{name}`)  
- Safe Team Mode toggles, default template/mode, capability maps (user/role → template)  
- Global bot settings exposed in the config UI  
- `IsAdmin` chrome (config nav, etc.)  

When OAuth is **disabled**, the UI treats the connection as admin-equivalent for chrome, but **write features stay off** (fail-closed).

---

## What POSTs / features work

| Area | Gate |
|------|------|
| All member write routes | Feature flag + CSRF; admin counts as member+ |
| Config mutations | `requireAdmin` + CSRF |
| Capability map add/remove | Admin |
| Safe Team save | Admin |
| Project allowlist / channels / etc. | Admin (as implemented on config routes) |

Feature flags still apply to GitHub merge/start/etc. Turning features on is an admin configuration act; performing merge still requires `features.merge == true`.

---

## Project visibility

`ProjectsVisibleTo` / `CanAccessProject`: **web admin sees every configured project**.  
They do not need to be on each project’s `allowedUserIds` for UI access.

Discord bot access remains channel-map + that project’s allowlist when they `@Grok` in Discord—web admin alone does not grant Discord allowlist.

---

## Relation to capability template `admin`

| | Web admin | Capability template admin |
|--|-----------|---------------------------|
| Config UI | Yes | No (unless also web admin) |
| See all projects | Yes | N/A (Discord path) |
| Ship / case caps in bot | Via their Discord template map | Full flag set if mapped to template `admin` |
| Merge PR in web | Feature `merge` + member+ (admin qualifies) | Template `merge` flag is separate |

Recommend: ops accounts get **web admin**; day-to-day eng get web **member** + Discord `builder`.

---

## Safe Team configuration (admin duty)

On each mixed project:

1. Map eng roles/users → `builder` (or higher).  
2. Map support → `investigator` / `operator` as needed.  
3. Set default template (usually `investigator`) and default mode (`case` for support).  
4. Enable Safe Team Mode.  
5. Fix **Unmapped members** warnings.  

See [safe-team-unmapped.md](./safe-team-unmapped.md) and [support-case-guide.md](../support-case-guide.md).

---

## Typical workflow

1. Log in with Discord OAuth as admin.  
2. Config → add project paths, channel maps, allowlists.  
3. Enable web features (`startSessions`, optionally `githubWrites` / `merge` / `prReviews`).  
4. Configure Safe Team + capability maps.  
5. Spot-check Discord denials and web case open as a non-admin test user.

---

## Common errors

| Symptom | Cause |
|---------|-------|
| WebAuth validation errors at boot | Missing client id/secret, public base URL, or empty admin list |
| 403 admin required | User not in `adminDiscordIds` |
| Features never turn on | `webAuth.enabled` false — Feature* always false |
| Changed Safe Team, eng broken | Unmapped eng demoted — map before enable |
| Audit log | Web mutations append to `data/audit/` JSONL |

---

## How to assign

1. `webAuth.enabled = true` with OAuth secrets and `webPublicBaseURL`.  
2. Set `webAuth.adminDiscordIds` to Discord user snowflakes (or bootstrap env once).  
3. Optionally mirror capability template `admin` on the same users for Discord superpowers.  
4. Prefer least privilege: few web admins; most staff as web members + Discord templates.

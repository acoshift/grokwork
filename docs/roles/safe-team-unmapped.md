# Unmapped members and Safe Team Mode

How Grok Work chooses capabilities when an actor is allowlisted on a project but named by neither `capabilityByUser` nor a team that carries a `capabilities` template.

Source: `Config.ResolveCapabilities` in `internal/config/capabilities.go`.

---

## Resolution order (Discord path)

Caller must already pass `AccessAllowed` (project allowlist). Then:

1. If the **actor id** maps in `capabilityByUser` → resolve that template (project overlay, then builtin).  
2. Else/also: every **team** the actor belongs to, in sorted-key order → OR-merge the flags of its `capabilities` template. A team with no `capabilities` grants access only and is not a hit.  
3. If any map hit → return merged caps.  
4. **Unmapped:**  
   - **Safe Team Mode on** → `safeTeamDefaultTemplate` (default **`investigator`**). Unknown template name → **zero caps** (fail closed).  
   - **Safe Team Mode off / unset** → builtin **`builder`** (backward compatible eng-only deploys).

Multiple templates OR-merge boolean flags (any true wins).

---

## Safe Team Mode **on** (default unmapped = investigator)

| Effect | Detail |
|--------|--------|
| Unmapped allowlisted users | Get investigator-class caps (unless default template overridden) |
| No silent eng elevation | Eng **must** be mapped to `builder` / `approver` / `admin` |
| Queued tasks | Execute-time re-check can **tighten** already-queued work (PR/token off) when caps demote |
| Config UI | Warns on unmapped member IDs when Safe Team is enabled |

**Default template** is configurable (`safeTeamDefaultTemplate`). Common choices:

- `investigator` — support-safe (default)  
- `operator` — investigate + draft, no escalate  

---

## Safe Team Mode **off**

| Effect | Detail |
|--------|--------|
| Unmapped | Builtin **builder** (`investigate` + `startSessions` + `githubWrites`) |
| Intent | Legacy: whole project acts as eng |
| Risk | Support on the same allowlist can freeform-fix |

Turn Safe Team Mode **on** for mixed support+eng projects.

---

## Rollout checklist

1. Put eng on a team with `capabilities: "builder"` (or higher).  
2. Give the support team `investigator` or `operator` (optional if the default is already investigator).  
3. Review **Unmapped members** warning on project config.  
4. Enable Safe Team Mode.  
5. Spot-check: support freeform does not open PRs; eng freeform still ships.  

---

## Web and Discord resolve identically

Both call `ResolveCapabilities(project, userID)`. There is no guild-role lookup on either path, so a team membership means the same thing in chat and in the web UI.

| Implication | Action |
|-------------|--------|
| Nothing is Discord-only any more | One team grant covers both surfaces |
| Unmapped actor under Safe Team | Gets the default template (investigator) on both paths |

---

## Common denials after enabling Safe Team

| Symptom | Cause |
|---------|-------|
| Eng cannot open PRs | Unmapped → investigator |
| “You're not allowed to start fix tasks” | Zero or investigate-only caps |
| Support suddenly can ship | Safe Team **off** (builder default) or mis-mapped to builder |
| Zero caps / hard deny | Default template name unknown or empty maps with fail-closed edge |

See individual role docs and [support-case-guide.md](../support-case-guide.md) admin note.

# Web as the primary surface (Discord becomes one surface of several)

| Field | Value |
|-------|-------|
| **Status** | Partially implemented — see [Implementation status](#implementation-status) |
| **Date** | 2026-07-25 |
| **Repo** | `github.com/acoshift/grokwork` |
| **Audience** | Operators and engineers familiar with this codebase |
| **Related** | `TODO.md` (Design principles; non-goal “Auth-heavy public web app”), `docs/design-full-workflow-web-ui.md`, `docs/design-agentic-team-runtime.md`, `docs/design-crash-safe-active-runs.md`, `docs/roles/` |

> **Superseded (2026-07-27):** Discord-role authorization is gone. `allowedRoleIds` and
> `capabilityByRole` were removed, along with the `roleIDs []string` parameter on
> `AccessAllowed` / `ResolveCapabilities`. Membership **and** capabilities now come from
> per-project `teams.<key>` (`members` = namespaced actor ids, `capabilities` = template
> name); `allowedUserIds` and `capabilityByUser` are unchanged. Read the role references
> below as history. See `README.md`, `docs/roles/README.md`, and `internal/config/team.go`.

---

## Overview

**Goal:** make the web UI the primary surface, so a team member **without a Discord account** can use grokwork end to end — while the current Discord flow keeps working unchanged.

**Verdict:** achievable incrementally. The discordgo *library* is already well contained; what leaked is Discord **identity** and Discord **messages-as-storage**. Five core changes, each shippable on its own, in strangler-fig order. No rewrite.

Today a run started from the web already works — `bot.StartCommitReview`, `startPRCreate`, and the web start composer allocate a **web-native unit** with no Discord thread. But that unit is a *degraded* Discord thread, not a first-class citizen: it loses the completion card, the brief, PR status cards, the CI digest, decision cards, artifact uploads, and reviewer pings, and its streamed output is wiped from memory when the run ends. And no matter how good the web UI gets, **authorization still requires a Discord snowflake** — so a non-Discord user cannot be granted access at all.

This doc freezes the plan into **five decisions (D1–D5)** and **six phases (P0–P5)**, plus the back-compat rules that keep Discord unbroken.

---

## Background: what is already surface-neutral

Good news first, because it sets the size of the job.

```
                       ┌───────────────────────────────┐
   Discord gateway ───► │  internal/bot  (32/55 files   │
        (Register)      │   import discordgo, ~349 refs)│
                        └───────────────┬───────────────┘
                                        │  ~38 exported methods,
                                        │  only 1 returns a discordgo type
                        ┌───────────────▼───────────────┐
   HTTP :8787 ────────► │  internal/web  (0 discordgo)  │
                        └───────────────────────────────┘

   config · sessionstore · history · runjournal · reviewstore
   grokrun · gitworktree · ghpr · linear · markdown · audit   ── all 0 discordgo
```

| Already neutral | Evidence |
|---|---|
| Construction | `bot.New(cfg, sessions, hist)` takes no session (`internal/bot/bot.go:124`); the session is injected later by `Register` (`bot.go:483`) |
| Background loops | `startIdleWorktreeCleanup` / `startIdleRepoFetch` / `startPRStatusPoller` start before gateway-ready **on purpose** (`bot.go:140-145`) so web units keep working |
| web → bot boundary | ~38 exported methods, all string/struct-typed except `Discord() *discordgo.Session` |
| Streaming transport | `messageMessenger` interface, discordgo-free (`internal/bot/stream.go:23`) |
| Thread creation | `threadAPI` interface, discordgo-free (`internal/bot/task_start.go:68`) |
| Worktree / branch model | `gitworktree` already namespaces `grok/web/` vs `grokwork/` (`worktree.go:88-94`) |
| Queue, claim, ownership, PR bind, history append | no surface branch at all — keyed by unit id |

So the containment boundary is right. The work is *inside* `internal/bot` and in the shared persistent types.

---

## Problem today: five couplings

### C1 — Identity is a Discord snowflake, everywhere

A user with no Discord account cannot be named in any allowlist:

- `WebAuthConfig.AdminDiscordIDs` / `MemberDiscordIDs` / `ViewerDiscordIDs` (`internal/config/webauth.go:31-38`) — web RBAC is entirely snowflake-keyed
- per-project `AllowedUserIDs` / `AllowedRoleIDs` (`internal/config/config.go:200-201`)
- `CapabilityByUser` / `CapabilityByRole` (`config.go:208-209`)
- ~~`DiscordUserGitHub map[string]GitHubIdentity` — attribution Tier A~~ **resolved**: removed. Attribution reads a **proven** link instead (`internal/identity`, alias actor id → `{canonical, handle}`), so it is keyed on the account and not on a snowflake, and no admin asserts it.
- `AccessAllowed(project, userID string, roleIDs []string)` (`internal/config/project_members.go:27`) and `ResolveCapabilities(project, userID string, roleIDs []string)` (`internal/config/capabilities.go:157`)

And a sharper problem than the naming: **every web site that resolves capabilities in order to *enforce* something passes `roleIDs = nil`** — `internal/web/fix.go:684,735`, `start.go:35`, `case_new.go:45,58`, `case_actions.go:16,29,51`, `reviews.go:429,487`, `project_config.go:120`. Role-based capability grants — the normal way a team is administered — silently do not apply on the web at all.

The exception proves the point: the one call passing a group id, `project_config.go:133`, passes `("", []string{roleID})` purely to **render** what that Discord role *would* grant. So the web already knows roles are load-bearing; it displays them and then never enforces them for the actual viewer. Web authorization is a lossy projection of Discord authorization.

Seed of the fix already in the tree: `Entry.CreatedBy` is documented as “Discord snowflake **or** `web:<id>`” (`internal/sessionstore/store.go:30`).

### C2 — The unit's primary key *is* a Discord thread id

`sessionstore` is keyed by thread id (`Listed.ThreadID`, `store.go:354`). Web units mint a synthetic key `w_<32 hex>` (`gitworktree.NewWebUnitID`, `worktree.go:131-138`) and `gitworktree.IsWebUnitID` (`worktree.go:115-129`) exists **solely** to tell the two apart — “which surface is this?” is answered by sniffing a string prefix off the primary key, at **14 non-test sites**.

Worse, the branch the run actually takes is a *different* signal:

```go
// internal/bot/bot.go:1403
present := s != nil // Discord presentation available
```

That is derived from **gateway presence**, not from the unit. Consequence, with Discord up and healthy: every web-native run issues one doomed `discordSendComponents` to channel `w_…`, logs `error: status message thread=w_…`, and only then flips `present = false` (`bot.go:1459-1467`). Degradation happens by API rejection instead of by design.

`present` then gates ~20 blocks in `executeTask`: status card, streamer target, worktree-fail card, attachment download, done-header edit, unposted-text flush, stderr dump, `DISCORD_UPLOAD:` uploads, decision cards, direct-ship notice, completion card, brief pin.

### C3 — Discord messages are the durable output; web re-derives

This is the largest functional gap. Concretely missing or degraded on a web-native unit:

| # | Lost capability | Site |
|---|---|---|
| 1 | Completion summary card (diff stats, risky paths) | `bot.go:1945`, `completion.go:615` |
| 2 | Pinned brief card | `bot.go:1950`, `brief.go:292`, `web_control.go:136-146` |
| 3 | PR status card + PR timeline embeds | `pr_status.go:377,448-452` |
| 4 | CI failure digest (checks + log tail) | `ci_triage.go:110-121` |
| 5 | CI / ops notices | `ci_triage.go:23-26` |
| 6 | Decision cards from `DECISION:` blocks | `bot.go:1904-1908` |
| 7 | `DISCORD_UPLOAD:` artifact delivery | `bot.go:1874-1880` |
| 8 | stderr debug dump | `bot.go:1866-1872` |
| 9 | Resume / interruption announcements | `recover.go:319-345` |
| 10 | PR review-request ping to reviewer | `internal/web/reviews.go:256-265` |

Plus a data-loss asymmetry: streamed text lives only in `job.liveText` in memory (`task_start.go:294-314`), **wiped at run end** by `clearRunActivity` (`task_start.go:316-337`, deferred at `bot.go:1500`), and `history.Turn.Response` is assigned `result.Text` only (`bot.go:2019-2024`). A run that is cancelled or dies without a final text leaves the web with **nothing**, where the Discord path had already posted every chunk into the thread. The thread is doing storage duty that no store is doing.

### C4 — Notification policy is Discord-shaped

`notify_done.go` forks one policy into two deliveries: an in-thread `@mention` for a thread, per-recipient DMs for a web unit (`notify_done.go:136-140,196-220`). The DM path **silently drops any recipient whose id is not snowflake-shaped** and caps fan-out at `maxNotifyDMs = 10` (`notify_done.go:191`). A web-only user has no reachable channel — they simply are not notified. And `/watch` is a Discord thread command, so a web unit's `WatcherIDs` is always empty.

### C5 — A project is defined by its Discord channel

The channel→project map is the project registry in practice. `startFixCreate` **errors out** when Discord is ready but the project has no mapped channel (`fix_start.go:230-233`), while `StartWebTask` (`web_task_start.go:89-102`) and `StartCase` (`case_start.go:85-89`) fall back web-native in exactly the same situation. Three paths, two behaviours.

---

## Non-goals (this train)

- Migrating off `discordgo` (`TODO.md` already freezes this: no migration without a real trigger).
- Removing or demoting the Discord UX. Mention + text commands stay primary **on Discord**; this train makes web equal, not superior.
- A public, internet-exposed multi-tenant web app. Web stays private-network by default; “supports non-Discord users” means *pluggable auth*, not *open signup*. (This does, however, retire the flat non-goal “Auth-heavy public web app” — see D3.)
- Native Discord slash commands (still demoted).
- Per-user GitHub write identity (Tier B — separate train, `docs/design-per-user-github-identity.md`).
- Changing the core isolation invariant. One unit = one worktree = one branch = one agent session, unchanged.
- Real-time collaborative editing / multi-viewer presence on the session page.
- Replacing `internal/history`. The timeline (D3) is additive and sits beside it.

---

## Key decisions

### D1 — Unit identity becomes opaque; Discord coordinates move into an optional sub-struct

`Entry` grows one field and loses its Discord flatness:

```go
// internal/sessionstore/store.go
type DiscordRef struct {
    GuildID      string `json:"guildId,omitempty"`
    ChannelID    string `json:"channelId,omitempty"` // parent channel
    ThreadID     string `json:"threadId,omitempty"`
    TriggerMsgID string `json:"triggerMsgId,omitempty"`
    URL          string `json:"url,omitempty"`       // jump link

    // Card message ids — meaningless off Discord, so they live here.
    StatusMsgID         string `json:"statusMsgId,omitempty"`
    BriefMsgID          string `json:"briefMsgId,omitempty"`
    PRStatusMsgID       string `json:"prStatusMsgId,omitempty"`
    CaseMsgID           string `json:"caseMsgId,omitempty"`
    DossierMsgID        string `json:"dossierMsgId,omitempty"`
    CustomerUpdateMsgID string `json:"customerUpdateMsgId,omitempty"`
    VerifyMsgID         string `json:"verifyMsgId,omitempty"`
}

type Entry struct {
    Discord *DiscordRef `json:"discord,omitempty"`
    // …
}

func (e *Entry) HasDiscord() bool // Discord != nil && Discord.ThreadID != ""
```

**`Entry.HasDiscord()` becomes the single answer to “does this unit have a Discord surface?”**, replacing both `gitworktree.IsWebUnitID` prefix-sniffing (14 sites) and gateway-derived `present` (`bot.go:1403`). Rendering to Discord then requires *two* conditions, checked in the right order: the unit has a Discord ref **and** the gateway is live. A web unit stops making doomed API calls; a Discord unit during an outage degrades without losing its ref.

`TrackedPR.StatusMsgID` (`internal/sessionstore/pr.go:24`) stays where it is — it is per-PR, not per-unit — but only written when `HasDiscord()`.

**Migration is nearly free.** Keep the existing map key. On load, when the key is snowflake-shaped and `Discord == nil`, synthesize `Discord{ThreadID: key}` and fold the seven flat `*MsgID` fields into it; keep writing the legacy flat fields for one release, exactly as `NormalizePRs()` mirrors legacy single-PR fields (`internal/sessionstore/pr.go:110,338`).

**A Discord unit stays keyed by its thread id — permanently.** An earlier draft of this decision had *all* new units get an opaque id, with `Discord.ThreadID` carrying the thread separately. That is conceptually tidier and operationally much worse: there are **98 non-test `sessions.Get()` and 45 `Patch()` call sites**, and the Discord path feeds the thread id in raw (`internal/bot/checkpoint.go:24,56,81` pass `m.ChannelID` straight as the key). Decoupling key from thread id puts a thread→unit reverse index — persisted, and maintained under concurrent `Patch` — on the path every Discord message takes, in exchange for no user-visible gain.

So the id space stays deliberately heterogeneous, exactly as it is today: a Discord unit's id **is** its thread id, and a web unit's id is opaque (`w_<32 hex>`, already the case). `Entry.Discord` still delivers the whole point of D1 — it retires the 14 prefix-sniffing sites and gateway-derived `present`, and gives the card message-ids a home — with zero change to the hot path. `HasDiscord()` reads the ref, never the shape of the key, so the *predicate* is clean even though the key space is not.

The corollary is a rule, not a preference: **no new code may infer a surface, or anything else, from the shape of a unit id.** `IsWebUnitID` survives only for the id *allocator* and the branch-prefix choice (`gitworktree.PrefixForUnitID`).

`runjournal.TaskRecord` (`DiscordURL`, `TriggerMsgID`, `StatusMsgID`, `AuthorID`, `RoleIDs`) gets the same treatment, since crash re-drive reads it (`docs/design-crash-safe-active-runs.md`).

### D2 — Actors replace snowflakes; ids are namespaced

```go
// internal/config (or a new internal/actor)
type ActorKind string // "discord" | "local" | "oidc" | "system"

type Actor struct {
    ID          string   // namespaced: "discord:1234…", "local:alice", "oidc:sub-…"
    Kind        ActorKind
    DisplayName string
    Groups      []string // namespaced too: "discord:role:987…", "local:group:eng"
}

func (c *Config) AccessAllowed(project string, a Actor) bool
func (c *Config) ResolveCapabilities(project string, a Actor) Capabilities
```

`Groups` unifies what is today the `roleIDs []string` parameter, so **Discord roles and future local groups take the same path** — which is what finally fixes web dropping roles (C1). Web resolves the viewer's groups from its own session (`internal/web/session.go`), including Discord roles cached at OAuth time, instead of passing `nil`.

Config keys gain neutral names with the old ones kept as **read-aliases** (never rewritten on save until a later release):

| Legacy key | New key |
|---|---|
| `allowedUserIds` | `allowedActorIds` |
| `allowedRoleIds` | `allowedGroupIds` |
| `capabilityByUser` / `capabilityByRole` | `capabilityByActor` / `capabilityByGroup` |
| `webAuth.adminDiscordIds` (+member/viewer) | `webAuth.adminActorIds` (+…) |
| ~~`discordUserGitHub`~~ | *n/a — removed, not renamed* (`internal/identity` owns attribution now) |

**Normalization rule (one function, one pinning test):** a bare snowflake-shaped id read from a legacy key normalizes to `discord:<id>`; an id already carrying a known namespace prefix passes through; anything else is rejected at load with a config error rather than guessed. Follow the `TestModelOptionsMatchInference` precedent (`internal/grokrun`) — one table, one test that pins it, so the mapping cannot drift.

Auth becomes a provider seam so Discord OAuth is one of N:

```go
type AuthProvider interface {
    Name() string                              // "discord" | "local" | "oidc"
    Begin(w, r) error                          // redirect or render form
    Complete(w, r) (Actor, error)
}
```

`internal/web/oauth.go` becomes the `discord` provider unchanged. **The second provider must be another OAuth2/OIDC issuer, not a local password store.** A local-accounts implementation was built and then removed: owning credentials means owning password reset, rotation, lockout, and breach response forever, and an OIDC issuer the team already runs answers "users who do not use Discord" without any of it. Non-Discord identity therefore arrives as `oidc:<sub>` (or a per-issuer namespace), never as a row in `config.json`. `UserProfile.DiscordUserID` (`internal/web/users.go:17`) becomes `ActorID`; `discordAvatarURL`'s snowflake bit-shift (`oauth.go:37-51`) moves behind the discord provider and falls back to initials for other kinds.

### D3 — A per-unit timeline store; Discord becomes a renderer

The single highest-value change. Introduce `internal/timeline`, modelled directly on `internal/history` (one append-only file per unit under `<DataDir>/timeline/<unitID>.jsonl`, size-capped, mutex-guarded):

```go
type EventKind string

const (
    EventRunStarted  EventKind = "run.started"
    EventTextBlock   EventKind = "text.block"    // sealed chunk of assistant output
    EventPhase       EventKind = "phase"
    EventActivity    EventKind = "activity"      // tool activity, cwd-relative
    EventCompletion  EventKind = "completion"    // diff stats, risky paths
    EventBrief       EventKind = "brief"
    EventPRStatus    EventKind = "pr.status"
    EventCIDigest    EventKind = "ci.digest"
    EventDecision    EventKind = "decision"
    EventArtifact    EventKind = "artifact"      // path + mime, host-local
    EventNotice      EventKind = "notice"        // ops / warn / resume
    EventRunDone     EventKind = "run.done"
)

type Event struct {
    Seq  int64           `json:"seq"`
    At   string          `json:"at"`
    Kind EventKind       `json:"kind"`
    Data json.RawMessage `json:"data"`           // per-kind payload struct
}
```

**The bot writes events; surfaces render them.** Both surfaces then read the same source of truth:

```
                         ┌──────────────────┐
   grokrun stream ──────► │ internal/timeline│ ──► web session page (SSE tail)
   completion/brief/PR ─► │  <unit>.jsonl    │ ──► discord renderer (cards, pins)
   ci triage / decisions  └──────────────────┘ ──► notifier (D4)
```

Why this is cheap: the card builders are **already pure functions** over data — `FormatCompletionEmbed` (`internal/bot/completion.go:419`), the brief formatter (`brief.go`), decision cards (`decision.go`), board digests (`board_digest.go`). They move behind a `Renderer` without being rewritten. The Discord renderer keeps its message-id upserts (now in `Entry.Discord`, per D1) so cards still edit in place rather than spam.

```go
type Renderer interface {
    Render(ctx context.Context, unit string, ev timeline.Event) error
}
// discordRenderer — existing embed/card builders + upsert by Entry.Discord.*MsgID
// webRenderer     — no-op; the web reads the timeline directly
```

This retires ~10 of the ~20 `present`-gated blocks in `executeTask`, closes all 10 rows of the C3 table in one move, and fixes the cancelled-run data loss: `EventTextBlock` is appended as chunks seal, so output survives a run that never produces a final `result.Text`.

Two constraints on the renderer that are **requirements, not implementation detail** — get either wrong and the Discord DX regresses:

**The live tail does not go through the timeline.** `EventTextBlock` is a *sealed* chunk only. Per-delta events would be absurd for an append-only file (one write per token) and would wreck the streaming cadence: `stream.go` coalesces deltas behind a 100ms ticker plus a `lastEdit` debounce (`internal/bot/stream.go:285,314`) and edits one message in place. That stays exactly as it is, fed from in-memory `job.liveText` (`task_start.go:294-314`), for both surfaces. The timeline is the *durable* record, not the live transport. Web already reads `liveText` via `StatusSnapshot()` for its SSE tail and keeps doing so.

**Rendering is best-effort and off the append path.** Today a failed Discord send logs and the run continues (`bot.go:1459-1467` is the pattern). A timeline append must never fail, block, or roll back because Discord returned 500 or the gateway is mid-reconnect — otherwise the new store makes runs *less* robust than the thread-only design it replaces. Renderer errors are logged against the unit and the event stays committed, which is also what makes replay-after-outage possible.

**Retention:** timeline files are capped and swept by the same idle policy as worktrees (`internal/bot/idle_cleanup.go`), except for units that are cases or still have tracked PRs — matching the existing session-retention rule. `TODO.md`'s open “History retention TTL” item should be resolved for both stores together.

**`DISCORD_UPLOAD:` becomes `ARTIFACT:`** in the prompt contract (`remoteWorkPromptPrefixMode`, `bot.go`), accepting the old keyword as an alias, since an artifact is now a timeline event that the web can serve and Discord can attach.

### D4 — A notifier registry, with a web inbox as the always-available channel

```go
type Notification struct {
    UnitID   string
    Project  string
    Kind     string   // run.done | review.requested | ci.failed | case.escalated
    Subject  string
    Body     string
    URL      string   // web session page
}

// Notify takes the whole recipient SET, never one actor at a time — see below.
type Notifier interface {
    Name() string                                        // "discord-thread" | "discord-dm" | "web-inbox" | …
    CanReach(a Actor) bool
    Notify(ctx context.Context, to []Actor, n Notification) error
}
```

**The recipient set is the load-bearing part of that signature.** An earlier draft had `Notify(ctx, a Actor, n)` — per-actor — which structurally cannot express what Discord does today. `notifyRunDoneSend` builds **one** message via `formatNotifyDoneMessage(ids, …)` and sends it **once** with every recipient attached (`internal/bot/notify_done.go:172-188`). A per-actor interface turns three watchers on a thread into three DMs instead of one in-thread ping: a straight DX regression, invisible in review, and only noticed by the people getting spammed.

So delivery is chosen per *unit*, then per recipient:

1. Unit has a Discord thread and the gateway is live → `discord-thread` takes the **entire** set and posts one message mentioning all of them. Identical to today, byte for byte.
2. Otherwise, route each remaining recipient to their best reachable channel, falling back to `web-inbox` **rather than dropping** — which is what fixes the silent loss of non-snowflake recipients (`notify_done.go:196-220`).

`web-inbox` is a small per-actor append-only feed the shell can badge — which also gives the long-deferred `TODO.md` item “‘needs you’ personal feed” a home. `maxNotifyDMs = 10` stays a *Discord DM* concern, not a policy concern; the thread path never had a cap and the inbox does not need one.

`notifyOnDone: never | errors | always | long_only` is unchanged — it is already surface-neutral policy. What changes is only *delivery*. `formatNotifyDoneDM`'s existing behaviour (name the project + goal, link the session page via `webPublicBaseURL`) becomes the shared formatter for every non-thread channel, since none of them have ambient context.

`/watch` gains a web equivalent (a watch toggle on the session page) so `WatcherIDs` stops being structurally empty for web units.

### D5 — A project no longer requires a Discord channel

`discordChannelId` becomes explicitly optional per project. The channel→project map stays exactly what it is today — a **routing table for inbound Discord messages** — but stops being the project registry; `config.Projects` already is that.

Two rules:

1. **In-chat project switching stays a non-goal.** On Discord, the channel map remains the only source of project. Nothing about D5 weakens that.
2. **Uniform fallback.** No mapped channel → allocate web-native. `startFixCreate` (`fix_start.go:230-233`) stops erroring and matches `StartWebTask` / `StartCase`.

The start composer's existing honesty (`startOpensDiscordThread` → “web-native — no Discord thread; the run streams here only”, `internal/web/templates/start.tmpl:75`) keeps working, and after D3 the second clause becomes simply true rather than a warning.

---

## Phases

Each phase ships independently and leaves Discord working. Ordered so the highest user-visible payoff (P2) lands before the largest surface-area change (P3).

| Phase | Scope | Risk | Unblocks |
|---|---|---|---|
| **P0** | Mechanical containment: route the ~92 raw `ChannelMessageSendReply` calls through `discord_msg.go` wrappers; widen `messageMessenger` / `threadAPI` and export the test seams (`internal/web/fix_test.go:50-52` already complains they are unexported). Shrinks the 103 non-test signatures carrying `*discordgo.Session`. | none | testability for all later phases |
| **P1** | D1 — `Entry.Discord` + `HasDiscord()`; retire `IsWebUnitID` sniffing and gateway-derived `present`; opaque ids for new units; same for `runjournal.TaskRecord`. | low | correct surface dispatch; kills the doomed-API-call log noise |
| **P2** | D3 — `internal/timeline`; bot writes events; Discord renderer wraps existing card builders; web session page reads the timeline. | medium | **all 10 lost capabilities; the cancelled-run data loss** |
| **P3** | D2 — `Actor`, namespaced ids, config read-aliases, `AccessAllowed`/`ResolveCapabilities` signature change, group-aware caps on web, `AuthProvider` seam + `local` provider. | medium | **non-Discord users** |
| **P4** | D4 — notifier registry + web inbox + web watch toggle. | low | non-Discord users get notified |
| **P5** | D5 — optional `discordChannelId`; uniform web-native fallback. | low | projects with no Discord presence |

P0–P2 are worth doing even if the web-primary goal were abandoned: they fix real bugs (doomed API calls, lost output on cancel) and close a 10-item capability gap that already affects today's web-native units.

---

## Implementation status

Code on `main` wins where this and the plan above disagree.

| Phase | State | Notes |
|---|---|---|
| **P0** | **Done** | 92 raw `ChannelMessageSendReply` calls behind `discordReply`. The plan called this "risk: none" and was **wrong**: `discordSendReply` sets `Parse: []` + `SuppressEmbeds`, so converting to it would have silently stopped the `/review` reviewer ping (the mention still *renders*, so nothing looks broken) and stopped PR/issue links unfurling. Mention/embed policy is now an explicit choice at a pure payload layer. The "export the test seams" half was already done in the codebase — the `fix_test.go` comment cited as evidence was stale narration, since removed. |
| **P1** | **Done** | `sessionstore.DiscordRef` + `Entry.HasDiscord()`; nine id-shape sniffs replaced by `bot.hasDiscordSurface`; `present` no longer derived from gateway liveness, so a web-native run with Discord up stops firing a doomed API call. `Origin` is unusable as the migration signal — `web_task_start.go` stamps `Origin=web` even when it *does* open a thread — so migration keys off the store key, in one documented function. Checked for lock reentrancy first: a static walk found no converted call inside any `Patch` closure. |
| **P2** | **Done (data); renderer interface deferred** | `internal/timeline` + the data-loss fix, and **all 10 C3 rows now record for every unit**: completion, brief, PR status + lifecycle transitions, CI digest, ops notices, decisions, artifacts, stderr, resume announcements, and the reviewer ping (which routes to the inbox when the reviewer has no Discord identity). Each followed one shape — collect → `appendTimeline` → render if a surface is present — which also removed three blind Discord calls that were guaranteed 4xx on a web-native unit (`announceResume`, `announcePRTimeline`, and the brief). The completion summary is rendered on the session page; the rest are recorded but not all individually rendered yet. **Deferred:** the general `Renderer` interface. With the split applied at all ten sites it would only re-describe what the code already does — the card builders are pure functions called directly, and there is no third surface to abstract over. |
| **P3** | **Partial — ids done, login deferred by design** | Namespaced actor ids (`discord:`/`web:`/`oidc:`) compared normalized, so a non-Discord person can be **named and authorized** in allowlists and capability maps, and `ResolveWebRole` matches either spelling everywhere (including `ProjectUserSet`, a raw map probe that was the one place they diverged). **Login for such a person is not implemented.** A local-accounts provider (PBKDF2, `webAuth.localAccounts`, `POST /auth/local`) was built and then deliberately removed — see D2: the project will add OAuth2/OIDC providers rather than run its own user management. So the id layer is ready and the second provider is the remaining work. **Also not done:** an `Actor` struct or group namespacing. The `roleIDs=nil` enforcement gap is **closed** — not by teaching the web session about Discord roles, but by deleting role-based grants: `AccessAllowed`/`ResolveCapabilities` no longer take a `roleIDs` parameter, so the 16 nil-passing web sites cannot under-grant, and per-project `teams` carry both membership and capabilities for actors of any provider. |
| **P4** | **Done** | `internal/inbox` + routing: a recipient no push channel can reach gets a queued entry instead of being dropped (they were silently removed from the list before), readable at `/inbox`. Invariant I1 is pinned by a test that counts *sends* — a thread still produces exactly one message naming everyone. **Not done:** an unread badge, and the `Notifier` interface itself — delivery is a routing function over two concrete channels, since a registry with two implementations and no third in sight is indirection without a payer. |
| **P5** | **Done** | `config.ErrNoDiscordChannel` splits absent from broken. The plan only recorded `startFixCreate` erroring; in fact `StartWebTask`/`StartCase` had the *opposite* bug — falling back on any error, hiding real channel misconfiguration. All three now agree: absent → web-native, broken → surface. |

**Deviations worth knowing about**

- **One text block per run, not per sealed chunk.** The plan said blocks are appended "as chunks seal". Sealing happens under the stream poster's lock, so appending there puts a file write inside the streaming critical section — violating I2. Blocks are written once per run instead; a hard crash mid-run loses the in-flight text, which is also true today, and `runjournal` owns crash re-drive.
- **Unknown actor namespaces pass through** rather than being rejected at config load, which keeps a live-edited config from gaining a new failure mode while preserving the same safety property (they match nothing).
- **Card message-ids stayed flat on `Entry`.** Moving them into `DiscordRef` is organization, not capability, and touches far more read sites than it earns.
- **Discord unit ids stay thread ids**, as amended — no reverse index on the message path.
- **No `Actor` struct and no `Notifier`/`AuthProvider` interfaces.** The plan specified all three. In each case the namespaced id string or a plain routing function carried the same semantics without the churn: `Actor` would have changed ~12 authorization signatures for data the id already encodes, and both interfaces would have had exactly two implementations with no third pending. The *invariants* they existed to protect (batched thread ping, one generic auth failure, normalized comparison) are pinned by tests instead of by types. Add the types when a third surface or provider actually arrives.
- **No local password store.** Built, then removed at the owner's direction: future non-Discord access comes from additional OAuth2/OIDC providers, so the product never holds a credential it would have to reset, rotate, or lock out. The namespaced-id layer is what those providers plug into, and it stayed.

---

## Discord DX invariants

Making web primary must not cost the Discord users anything. These are the things a reviewer should reject a PR over, not aspirations — each maps to a test in the next section.

| # | Invariant | Why it is at risk |
|---|---|---|
| **I1** | **One finished run = one in-thread ping**, mentioning every recipient, for any unit with a live thread. Never N DMs. | The natural per-actor notifier shape breaks this silently (D4). |
| **I2** | **Live streaming cadence is untouched**: one message edited in place, 100ms ticker + `lastEdit` debounce, sealed at 1900 chars. | Routing the live tail through an append-only store would destroy it (D3). |
| **I3** | **A Discord API failure never fails a run or a timeline append.** Renderer errors log and continue. | Introducing a store *upstream* of Discord invites making the store depend on it (D3). |
| **I4** | **No reverse index on the Discord message path.** A thread id remains a direct `sessions.Get` key. | 98 `Get` + 45 `Patch` sites; the tidier id scheme silently taxes all of them (D1). |
| **I5** | **Cards still edit in place, never re-post.** Message ids survive restart. | Message ids move into `Entry.Discord`; a missed read path turns an edit into a new message every poll (D1/D3). |
| **I6** | **Mention + text commands stay the whole Discord command surface.** No slash commands, no in-chat project switching, no new required syntax. | Already a `TODO.md` non-goal; restated because a web-primary train is exactly when someone proposes "just add a slash command". |
| **I7** | **Bare snowflakes keep working everywhere** an id is accepted, with no operator migration step. | D2's namespacing. |
| **I8** | **Local project paths still never reach Discord.** | The timeline stores host-absolute artifact paths; a renderer that echoes them leaks (existing rule, new exposure). |

Two changes are **accepted** as visible, because they cannot be avoided and are small:

- `DISCORD_UPLOAD:` → `ARTIFACT:` in the prompt contract. The old keyword is aliased, so no run breaks — but the prompt *text* changes, and prompt text changes model behaviour. Needs a golden test on `remoteWorkPromptPrefixMode` output, and it should ship in its own commit so a behaviour shift is attributable.
- After D2, the first config save through the web UI rewrites `config.json` into the new key names. Back-compat rule #1 keeps legacy keys written for one release, so the file stays loadable by the previous binary — but operators should be told in the release note rather than discovering it in a diff.

---

## Back-compat rules (how Discord stays unbroken)

1. **Legacy JSON keys are read forever, written for one release.** Precedent: `NormalizePRs()` mirroring single-PR fields (`internal/sessionstore/pr.go:110,338`). Never require an operator to hand-edit `config.json` or `sessions.json`.
2. **Tri-state pointers stay pointers.** `*bool` / `*int` distinguish unset-→-default from explicit false/0 (`Yolo` nil = true; `WorktreeIdleTTLDays` nil = 30 but 0 disables). Any new config field follows this.
3. **Bare snowflakes remain valid input** in every allowlist. `discord:` is a namespace, not a migration requirement.
4. **`web_test.go` pins markup byte-for-byte** (2286 lines). Read it before renaming anything in a template; the `id="page-*"` markers, nav-anchor class-attribute-last convention, and partial-has-no-chrome assertions are contracts.
5. **One pinning test per mapping table** — the id-namespace normalizer and the event-kind ↔ renderer table both get a `TestXMatchesY` in the `TestModelOptionsMatchInference` mould.
6. **Per-agent asymmetries stay per-agent.** A project `investigateTools` override written for grok must keep being ignored for claude; nothing in this train touches that.

---

## Testing

| Area | Test |
|---|---|
| D1 | Load a legacy `sessions.json` (snowflake keys, flat `*MsgID`) → `Discord.ThreadID` and card ids populated; round-trip preserves legacy keys. A `w_…`-keyed entry loads with `Discord == nil`. |
| D1 | With gateway **up**, a web-native run makes **zero** Discord API calls (assert on a fake session that records calls) — the current doomed-send regression. |
| D2 | `AccessAllowed` / `ResolveCapabilities` accept bare snowflakes, `discord:`-prefixed, and `local:` ids equivalently; an unknown namespace is a load error, not a guess. |
| D2 | A web viewer with a Discord role mapped in `capabilityByGroup` gets those caps — the C1 `roleIDs=nil` bug, pinned. |
| D3 | A cancelled run whose agent produced streamed text but no `result.Text` leaves that text readable on the session page. |
| D3 | Golden test: the same timeline replayed through the Discord renderer produces byte-identical embeds to today's `FormatCompletionEmbed` output. |
| D3 | Card upserts still edit in place across a restart (message ids read from `Entry.Discord`). → **I5** |
| D4 | A recipient with no reachable Discord channel lands in the web inbox instead of being dropped. |
| **I1** | A thread unit with author + 2 watchers produces **exactly one** `discord-thread` delivery naming all three — and **zero** DM deliveries. Assert on delivery count, not just content; content-only assertions pass under the regressing per-actor shape. |
| **I2** | Feeding a fixed delta sequence through the streamer yields the same edit count and the same sealed-chunk boundaries as today. Pin the count — it is the only thing that catches a per-delta timeline write. |
| **I3** | With a renderer that always errors, a run still completes and every event is present in the timeline. |
| **I4** | A Discord-created unit is retrievable by `sessions.Get(threadID)` with no index consulted (assert via a store that fails on secondary-index lookup). |
| **I8** | An `ARTIFACT:` event with a host-absolute path renders to Discord with the path stripped/relativized — extends the existing no-local-paths rule to the new event type. |
| D5 | Project with no `discordChannelId`: `StartFix`, `StartWebTask`, `StartCase` all allocate web-native; none error. |
| Preview | `GROKWORK_WEB_PREVIEW=1 go test ./internal/web -run TestPreviewServer` seeds a timeline so the session page renders every event kind. |

---

## Doc / principle updates this train implies

- **`TODO.md` design principle #1** — “One **thread** = one worktree = one branch = one Grok session” → “One **unit** = one worktree = one branch = one agent session (a unit may have a Discord thread).” `CLAUDE.md` carries the same sentence and needs the same edit.
- **`TODO.md` principle #5** — “Pins/cards over chat archaeology” becomes surface-independent: “one updated **timeline** beats chat archaeology”, of which pins are the Discord rendering.
- **`TODO.md` non-goal “Auth-heavy public web app”** — retire. Web stays private-network by default, but pluggable auth with non-Discord accounts is now in scope. Restate the boundary as: *no public signup, no multi-tenant hostile isolation.*
- **`TODO.md` framing line** — “Discord-first team development workflow + private web UI” → “web-primary team development workflow with a first-class Discord surface”.
- **`docs/roles/web-*.md`** — role docs describe web roles as Discord-id-keyed; update after P3.
- **`TODO.md` open item “Later: `/model` or per-channel model override”** — partially superseded; the web model picker shipped. Note that the *Discord* side remains open by design (Discord users cannot pick a model).

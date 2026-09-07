# Design: SLA notifications into inbox

| Field | Value |
|-------|--------|
| **Status** | Shipped (implementation scrutinize 2026-09-07: ship) |
| **Date** | 2026-09-06 |
| **Repo** | `github.com/acoshift/grokwork` |
| **Audience** | Implementors of this train |
| **Related** | [research-single-platform.md](research-single-platform.md) §4 item 1, `TODO.md` (“SLA notifications”), [design-today-waiting-on-you.md](design-today-waiting-on-you.md) (inbox writers were explicitly out of that train), [design-web-primary.md](design-web-primary.md) D4 / I1, `internal/inbox`, `internal/bot/case_sla.go`, `internal/bot/notify_route.go`, `internal/bot/notify_done.go`, `internal/bot/discord_msg.go`, `internal/sessionstore/sla.go` |

Research #4 first slice and the open Wave 3 TODO: **nothing pings when a case breaches**. The badge only appears to whoever is already looking at the board. This design is the PR plan for that writer.

Today ([design-today-waiting-on-you.md](design-today-waiting-on-you.md)) is the **derived current-state** surface (`sla` chip while the case is still late). Inbox is the **event log**. Mark-all-read may hide the ping; Today / `?sla=breached` still show the case. Do not fold one into the other.

---

## Implementation scrutinize contract (mandatory)

This is part of the plan, not a suggestion. A PR that skips it is not done.

1. **Each PR / step** in [PR Plan](#pr-plan) is its own change. When that step’s code is green (`go test` of the packages it touches), load `/scrutinize` and run it on **that step’s full change** (worktree vs primary tip — not a summary). Fix blocker and major findings before starting the next step. Verdict must be `ship` (or `fix-then-ship` after the fixes land).
2. **Before the implementation is marked green** (last PR ready to land, or the whole stack about to go to `main`): run `/scrutinize` again on **all changes in the train together**. Cross-PR seams (watermark × reopen × Discord AllowedMentions × inbox SSE × involvement) are exactly what a per-step review will miss.
3. Do not treat a one-line LGTM, a passing test list, or “the design said so” as scrutinize. Follow the skill: intent → trace real paths → verify each claim → report.
4. Inbox UI copy still needs the closest substitute for browser verification: httptest of `/inbox` + the list partial, and `GROKWORK_WEB_PREVIEW` if the subtitle/kind badge is touched. Confirm an `sla.breached` row renders, is unread, marks read without hiding Today (if Today is present), and that `sse:inbox` still drives the nav pill.

`AGENTS.md` already requires scrutinize-then-ship on this repo. This section makes it load-bearing **inside the train**, not only at the final commit.

---

## Problem

Case SLA is computed at render time from timestamps + project targets (`computeCaseSLA` in `internal/bot/case_sla.go`). A clock crossing writes **no byte**, which is why `fpCases` folds `SLABreached` and `nextSLARecompute` exists — the badge would otherwise appear only on the next navigation.

That is correct for a **badge**. It is why there is still no **ping**:

| What exists | Why it is not a notification |
|-------------|------------------------------|
| Board chip + `?sla=breached` | Only whoever already opened Cases |
| Today `sla` reason (if that train has landed) | Derived current state, not an event; empty if you never open `/today` |
| Inbox (`run.done`, `review.requested`) | No SLA writer. Comment on `inbox.Item.Kind` already reserves `ci.failed` as a future kind; SLA is the same hole |
| Discord run-done / review ping | Event-shaped, and they have a writer. SLA has none |

Support’s morning tab is supposed to show a breached first-response without hunting the board (research “A day in the life”). Right now it does not.

---

## Goal

When an **open** case’s first-response or resolution clock becomes breached, every actor **involved** in that case gets one inbox item for that clock for that SLA round. A Discord thread, if the case has one, gets **one** in-thread message naming them (I1). Nobody has to be looking at the board.

Non-goals for this train: helpdesk wrap, customer status token, CI/decision/escalate/deploy inbox writers, Priority vs FYI inbox IA, warning-before-breach, auto-email/SMS, a stored `breached` flag, a new SSE domain.

---

## Alternatives (rejected)

| Option | Why not |
|--------|---------|
| **Write from `fpCases` / `nextSLARecompute`** | Read path on the SSE tick. Fires only while a browser is connected; N connections would be N writers. Fingerprints must stay pure. |
| **Write when the case board / Today renders** | That is today’s bug: the ping happens only for whoever looked. |
| **Store `breached bool` on the case** | Forbidden. A stored flag is a lie the moment the deadline passes with no writer. `computeCaseSLA` stays the only policy. |
| **Dedup by scanning each actor’s inbox** | `List` caps at `maxItemsPerActor = 500` (oldest dropped on **read**). Round identity is not on `inbox.Item`, so reopen would either suppress the new round or always re-ping. Per-recipient `List` on every sweep is the wrong index. |
| **In-memory “already pinged” set** | Lost on restart → every boot re-pings every open breach. |
| **Sidecar `data/sla-alerts.json`** | Second crash-safe file for two strings that travel with the case. `Entry` strings need no `clone()` change. |
| **24h idle sweeper** | A 4h SLA would notify up to a day late. |
| **Sleep-until-`nextSLARecompute`** | Must wake on session writes, SLA config edits, and hold/unhold. More machinery than every other bg worker. A 30s ticker is at most 30s late; SLAs are minutes-to-hours. |
| **DM each recipient (run-done web-unit path)** | SLA can fire hours after last activity, including night. Inbox is the web interrupt; a thread ping is the Discord one. DMs are out of v1. |
| **`NotifyThread` / `discordSend` for the ping** | `discordSend` (`internal/bot/discord_msg.go`) sets `AllowedMentions.Parse = []` and does **not** list `Users`, so `<@id>` is inert. Run-done uses `discordNotifySend` with `Users`. Copy that, not `NotifyThread` (team-review currently has this hole — do not copy it). |
| **`QueueInbox` as the success signal** | `QueueInbox` returns `nil` when `b.inbox == nil` (`notify_route.go`). Treating that as “delivered” would stamp watermarks and never retry. Call `b.inbox.Append`. Leave `QueueInbox`’s contract alone. |
| **Broadcast to the project allowlist** | Empty owner/engineer must not become a read primitive over every member. Log and skip. |
| **`caseIsMine` as the recipient set** | Watchers see Today’s `sla` chip (`unitInvolves`) but would miss the ping. One involvement set. |
| **Phrase the item from `CaseSLA.Badge`** | `caseSLAText` prefers first-response whenever that clock is breached, even if this pass is only alerting resolution. Duplicate-looking FR pings. Phrase from the **clock being delivered**. |
| **Helpdesk wrap / status token in the same train** | Research #4 items 2–3. This train is the S-sized ping. |

---

## Key Decisions

1. **Sweeper, not a render hook.** A Discord-independent background loop (same family as `startPRStatusPoller` / `startIdleWorktreeCleanup`) walks open cases on a 30s tick, evaluates `caseSLAAt(e, now)`, and writes inbox items for newly-breached clocks. Start it from `Bot.New` next to the other Discord-independent workers (~line 227), **not** only from `onReady` — web-only hosts must still ping. Re-call from `onReady` like idle/PR (`sync.Once` makes it a no-op). Board digest stays onReady-only; it needs a Discord session. This sweep does not.
2. **Breach stays derived.** The sweeper calls the existing `caseSLAAt` / `computeCaseSLA`. No second policy. Tests freeze `now` via `sweepSLAInboxAt(now time.Time)` (no query object). They must not call `CaseSLAFor` (it uses `time.Now()`) and must not sleep.
3. **Watermarks are “we delivered this round’s clock”, not “this case is breached.”** Two optional RFC3339 strings on `sessionstore.Entry`:
   - `SLAAlertedFirstResponse`
   - `SLAAlertedResolution`
   Each stores the **round start** (`Entry.SLARoundStart`) that was already inbox’d for that clock. Compare with `time.Time.Equal` after `sessionstore.ParseStamp`, not string `==` (`OpenedAt` may have a different offset spelling). They are never read by `computeCaseSLA`, the board, Today, or `fpCases`.
4. **Patch the watermark without `StampTurn`.** `Store.Patch` does **not** invent `UpdatedAt` — the field comment already says background writers (PR poller, ownership) leave it alone so boards and the terminal-session sweeper track real work. `TouchTurn` is the only helper that stamps it. A sweeper must not look like a human turn. `Patch` still bumps `Rev()`, which is enough for live regions that key off the session generation.
5. **Reopen is a new round for free.** `ResetCaseSLARound` does **not** need to clear the watermarks. The new `SLARoundStart` (ReopenedAt) will not `Equal` the stamped one, so the new round can alert again when it actually breaches. Do not notify on reopen itself.
6. **Open cases only.** `IsCase() && !IsCaseClosed()`. Closed-while-down misses a ping — accepted. First deploy **does** ping every currently-open breach (that is the feature). Pre-SLA cases with no round start stay silent (`computeCaseSLA` already returns inactive) — same archive-protection as the badge.
7. **One inbox item per clock per round.** Kind `inbox.KindSLABreached = "sla.breached"`. Subject is phrased from **that clock** (`SLA · first response 2h over` / `SLA · resolution 2h over`) plus case key, using `formatCoarseDuration(clock.Over())` — the same helper as `caseSLAText`, not `CaseSLA.Badge`. If both clocks newly breach in one sweep, two inbox items. If first-response was already alerted and resolution later breaches, a second item whose subject names resolution.
8. **Recipients = the Today involvement set**, not `caseIsMine`. Owner, co-owner, engineer, watchers. Canonical actor ids as stored (canonical-at-mint). Dedup the set. Skip empty ids. Do **not** teach `SameActor` about aliases. Do **not** add `ReporterID` unless it already appears in that set (it usually is `OwnerID`).
9. **Inbox first, then Discord I1, then watermark.** Durable record before a Discord 4xx. Then one thread message if `hasDiscordSurface`. Then `Patch` the watermark(s) that were delivered. Crash between notify and Patch may duplicate once — accepted. **Do not Patch first** (that would drop the ping forever). A Discord send error after a successful Append still stamps — no Discord retry. Same as run-done (`deliverInbox` then send). Inbox is the record.
10. **No DMs in v1.** Web-native cases are inbox-only. Discord cases get inbox + one in-thread message.
11. **Discord mentions must actually ping.** Reuse `notifySend` / `discordNotifySend` (`AllowedMentions.Users` = snowflake-shaped ids only). `discordMentionIDs` may keep an unmapped `google:` id in the **text** (I1: do not drop them from a message addressed to everyone); those ids must **not** be placed in `Users` (Discord 400). Do not call `NotifyThread`.
12. **No new SSE domain.** `inbox.Append` → `LastSeq` moves → existing per-connection `fpInbox` → `sse:inbox` → inbox list + nav unread pill. `fpCases` / `nextSLARecompute` already move the badge without a store write; the watermark `Patch` is extra, not required for the badge.
13. **No project-wide kill switch in v1.** A project with no SLA targets already evaluates inactive. Configuring targets is opt-in to pings.
14. **No audit row.** Run-done and review inbox appends are not audited. SLA is a system writer. `log.Printf` on per-recipient errors (ids, not titles); one failure must not skip the rest.
15. **Hand-off after alert does not re-ping.** Watermarks are per clock per round, not per actor. The new engineer sees Today / `?sla=breached`. Inbox is the event at T. Accepted.
16. **Stopped or Held clocks that are still `Breached` still notify** if not yet watermarked. A first response that landed 2h late is a missed promise; skipping `Stopped` would drop it if they replied in the 30s between deadline and tick. Held+inside-target is not `Breached` and stays silent.

---

## Shape

### Watermarks (`internal/sessionstore`)

On `Entry`, **after** the SLA timestamp block (do not put them inside the “only timestamps live here” comment — they are delivery receipts, not standing):

```go
// SLAAlerted* are delivery receipts for sla.breached inbox rows: the
// SLARoundStart that was already pinged for that clock. Not a stored
// breach flag; computeCaseSLA never reads them.
SLAAlertedFirstResponse string `json:"slaAlertedFirstResponse,omitempty"`
SLAAlertedResolution    string `json:"slaAlertedResolution,omitempty"`
```

Strings only — `Entry.clone` does not change. `TestEntryCloneDetachesEveryField` stays green without a fixture edit.

`ResetCaseSLARound` stays response/hold stamps only.

### Kind (`internal/inbox`)

```go
KindRunDone         = "run.done"
KindReviewRequested = "review.requested"
KindSLABreached     = "sla.breached"
```

Unknown kinds are still stored (existing rule). `TestKindConstants` must include the new one.

### Title helper

Do **not** call `waitingTitle` — `internal/bot/waiting.go` is not guaranteed on `main`. Local helper in `sla_notify.go`:

```go
func slaNotifyTitle(e sessionstore.Entry) string {
    if e.IsCase() {
        if t := strings.TrimSpace(e.CustomerTitle); t != "" {
            return t
        }
    }
    return strings.TrimSpace(e.Goal) // empty is fine; Body is omitempty
}
```

Never a prompt, never `e.Cwd`, never the customer-update draft.

### Recipients helper

`unitInvolves` may also be missing on `main`. Local helper:

```go
func slaRecipients(e sessionstore.Entry) []string
```

Collect unique non-empty `OwnerID`, `EngineerID`, each `CoOwnerIDs`, each `WatcherIDs`. When Today is on `main`, a later one-line can filter through `unitInvolves` — not required to ship. Do **not** fall back to `caseIsMine` (drops watchers).

### Sweep (`internal/bot`)

New file `internal/bot/sla_notify.go` (keep `case_sla.go` a pure evaluator).

```go
const slaNotifyInterval = 30 * time.Second
const slaNotifyInitialDelay = 30 * time.Second
```

```go
func (b *Bot) startSLAInboxSweep()
func (b *Bot) runSLAInboxSweep() // bgContext + sleepCtx + ticker; stop on cancel
func (b *Bot) sweepSLAInbox() int
func (b *Bot) sweepSLAInboxAt(now time.Time, send notifySend) int
```

`sweepSLAInbox` is `sweepSLAInboxAt(time.Now(), discordNotifySend(b.Discord()))`. PR 1 may pass `send == nil` (inbox only). Return how many **inbox items** were appended (not how many cases were walked).

Unit tests construct `&Bot{cfg, sessions, inbox}` like `newRouteBot` in `notify_route_test.go`. **Do not** call `bot.New` in these tests — `New` starts the real ticker (and already starts the PR poller). Do not call `startSLAInboxSweep` in unit tests; drive `sweepSLAInboxAt` directly. Config SLA table: the same `ProjectsMap` + `SLATarget` pointer minutes as `case_board_test.go`.

Per case, in order:

1. If `b.inbox == nil` at the start of the sweep, return 0 (init failed — do not stamp watermarks that would suppress a later retry).
2. `sessions.List()` — same clone-walk as the idle sweeper / PR poller. The map key is `listed.ThreadID`; `Entry` has no thread-id field. Do not add an index.
3. Skip `!e.IsCase() || e.IsCaseClosed()`.
4. `sla := b.caseSLAAt(e, now)`. Skip if `!sla.Breached`.
5. `start, ok := e.SLARoundStart()` — if `!ok`, skip (cannot happen if `Breached`, but keep the guard).
6. Recipients = `slaRecipients(e)`. If empty, `log.Printf` (thread id + project, no title) and continue — **do not stamp**. A later `/claim` should still ping.
7. For each clock `{name, clock, watermark}` in `{("first response", sla.FirstResponse, e.SLAAlertedFirstResponse), ("resolution", sla.Resolution, e.SLAAlertedResolution)}`:
   - Skip if `!clock.Breached`.
   - Skip if `ParseStamp(watermark)` is ok **and** `Equal(start)`.
   - Subject: `"SLA · " + name + " " + formatCoarseDuration(clock.Over()) + " over"` plus ` · ` + `CaseKey` when present, else ` · ` + project. (`Append` refuses an empty subject; this is never empty when `Breached`.)
   - Body: `truncateRunes(slaNotifyTitle(e), 200)` (existing helper in `completion.go`).
   - For each recipient: `b.inbox.Append(id, inbox.Item{Kind: inbox.KindSLABreached, Subject, Body, URL: inboxSessionPath(listed.ThreadID, e.Project), UnitID: listed.ThreadID, Project: e.Project})`. Log and continue on error. Count successes.
   - If **every** recipient failed (or there were none — already skipped), do **not** mark the clock delivered.
   - If ≥1 Append succeeded, remember the clock as delivered this pass.
8. If `hasDiscordSurface(listed.ThreadID)` **and** `send != nil` **and** at least one clock was delivered this pass: one thread message covering those clocks (not one message per clock — I1 is per *event*; two clocks in one sweep are one Discord message, two inbox rows). Phrase from the clocks delivered this pass, first-response first. `AllowedMentions.Users` = snowflake-shaped ids only (`discordDMTarget` / `looksLikeDiscordUserID`). Content mentions `discordMentionIDs(recipients)`. `sanitizeDiscordContent` + `clampDiscord`. No `e.Cwd`. Case key, not a filesystem path. A `webPublicBaseURL` session link is optional.
9. `Patch(listed.ThreadID, …)` the delivered watermark(s) to `start.UTC().Format(time.RFC3339)`. Do not `StampTurn`. Do not touch phase, owner, or SLA timestamps. Assert in tests that a pre-set `UpdatedAt` is unchanged.

### Discord message (PR 2)

One message, all Discord-reachable recipients:

```
<@a> <@b> — SLA · first response 2h over · WEBAPP-14
```

If this pass delivered only resolution:

```
<@a> <@b> — SLA · resolution 2h over · WEBAPP-14
```

If both: first-response line first, then resolution, still **one** message.

Injectable `notifySend` so tests do not need a Session. `send == nil` or `!hasDiscordSurface` → 0 Discord calls.

### Wiring

- `Bot` gets `slaNotifyOnce sync.Once` next to `prStatusOnce`.
- `startSLAInboxSweep()` from the Discord-independent block in `Bot.New` **and** the `onReady` re-call cluster (Once-guarded). Do not put it only next to `startBoardDigest` (that helper needs Discord and is onReady-only).
- Stop: `bgContext` cancel already owned by `Bot.Stop`. No extra teardown.

### Inbox page

No new route. Kind already renders as a badge (`inbox.tmpl` `{{.Kind}}`). Update the muted subtitle so it is not only “review requests and finished runs”: mention SLA breaches. GET still does not mark read.

---

## What must not break

- `computeCaseSLA` / pause / no-round-start / closed-without-reply semantics. Sweeper is a caller, not a second clock.
- `fpCases` purity. No inbox writes from `live.go`.
- I1: a Discord case produces **one** thread message per sweep-that-delivered, not one per recipient.
- Inbox-always: a failed Discord send must not mean the row is missing (watermark still waits until inbox succeeded for ≥1 recipient).
- `inboxItemVisible`: a row with `Project` set is hidden from a viewer without access. Recipients are involved members; still set `Project`.
- Canonical-at-mint: Append keys are account ids as stored on the case. Absorb (`inbox.RewriteActor`) already merges feeds. Do not call `identity.Canonical` at notify time.
- No customer text in audit (there is no audit). No local paths on Discord.
- Today / case board unchanged. Do not add a `sla` reason here; do not change `caseIsMine`.
- `TestLiveRevsStableAndChange`: `fpInbox` stays per-connection; do not stuff it into `computeLiveRevs`.
- `QueueInbox`’s nil-inbox → nil error contract, used by review notify. Do not “fix” it as part of this train.

---

## Tests

Frozen `now`. No `time.Sleep`. No `CaseSLAFor`. Lightweight `&Bot{…}`, not `bot.New`.

1. First-response breach → one inbox item per involved actor; stranger gets 0; subject names **first response** (not a generic badge) and the case key; URL is `inboxSessionPath`.
2. Second `sweepSLAInboxAt` at the same `now` → 0 new items (watermark).
3. Restart: new `Bot` / new inbox store over the same `sessions.json` + watermark still set → 0 new items.
4. Resolution-only breach **while first-response is also still `Breached`** (late FR already watermarked) → one new item whose subject names **resolution**, not first response.
5. Both clocks newly breach in one sweep → two inbox items, one Discord message (when send is non-nil and the unit has a Discord surface), both watermarks set.
6. Reopen (new `SLARoundStart`) then later breach → notifies again; does not notify at the reopen instant if still inside target. `ResetCaseSLARound` is not required to clear watermarks.
7. Closed case that is numerically breached → 0 items.
8. No round start / no SLA targets / not a case → 0 items.
9. Watcher is a recipient; random allowlisted user is not. Empty involvement → 0 items, no watermark (a later `/claim` can still ping).
10. Inbox nil → 0 watermarks.
11. One recipient Append error, others succeed → watermark **is** set (do not retry-spam the successful ones). All recipients error → watermark **not** set. Seam: `inbox.Append` already refuses an actor id with unsupported characters (`actorFileName`, e.g. `"bad id!"`). Use that as a co-owner next to a valid owner; do not add a func field on `Bot`.
12. Discord: `hasDiscordSurface` true + non-nil send → exactly one `notifySend` call, `Users` only snowflakes, content mentions everyone in the involved set that mapped. Web-native unit → 0 Discord sends, inbox still written. `send == nil` → 0 Discord even on a Discord unit (PR 1 tests).
13. `AllowedMentions.Users` does not contain `google:…` even if that id is involved (it may appear in content).
14. `Patch` does not change `UpdatedAt` (set a distinct UpdatedAt before sweep; assert it stuck).
15. `TestKindConstants` includes `sla.breached`.
16. Inbox page / partial: an `sla.breached` row with a hidden `Project` is omitted (`inboxItemVisible`); a visible one renders the kind badge.
17. Answered **inside** target (Held, not Breached) → 0 items.
18. First-response deadline passed, then they respond **before the sweep** (Stopped+Breached, watermark empty) → still one FR item.

Do not sleep to wait for the ticker.

---

## Risks

| Risk | Mitigation |
|------|------------|
| First deploy pings every open breach | Intended. Operators who configured SLA and never got pings want this once. |
| 30s lateness on a 15m target | Accepted. Documented. Do not sleep-until-deadline in v1. |
| Crash window duplicates one ping | Accepted. Never Patch-before-write. |
| Hand-off misses the event | Accepted. Today / case board are current state. |
| `waiting.go` not on `main` | Local `slaRecipients` / `slaNotifyTitle`. |
| Sweeper walks all sessions every 30s | Same order of cost as the 90s PR poller. Filter cases in the loop. |
| Watermark `Patch` bumps `sessions.Rev()` and invalidates `liveRevCache` | Rare (only on a newly delivered clock). Do not add a sidecar to avoid it. |
| Customer title on a private inbox | Same residual as the case board. Truncate. Not on audit. Discord message uses clock phrase + case key, not the title. |
| Discord `Users` 400 on namespaced ids | Test 13. Filter snowflakes. |
| `bot.New` in other tests starts the ticker | Existing pattern (PR poller already does). SLA unit tests use `&Bot{}`. Preview server may ping demo cases 30s after boot — acceptable. |
| `QueueInbox` nil-inbox looks like success | Do not use it for this writer. |

---

## Open Questions

None that block implementation. Locked above:

- Sweeper vs render hook → sweeper from `Bot.New`.
- Watermarks vs inbox scan vs stored breach → round-start watermarks on `Entry`.
- DMs → no in v1.
- Recipients → involved set, not `caseIsMine`, not allowlist.
- Item copy → per-clock phrase, not `CaseSLA.Badge`.
- Discord → PR 2, real `AllowedMentions.Users`, not `NotifyThread`.
- Success signal → `inbox.Append`, not `QueueInbox`.

---

## PR Plan

Each PR is independently reviewable and mergeable. **Scrutinize that PR before starting the next** ([contract](#implementation-scrutinize-contract)).

### PR 1 — Sweep + inbox writer

- **Title:** `bot: inbox ping when a case SLA clock breaches`
- **Files:** `internal/inbox/inbox.go` (+ `inbox_test.go` kind constant), `internal/sessionstore/store.go` (two string fields + comment), `internal/bot/sla_notify.go`, `internal/bot/sla_notify_test.go`, `internal/bot/bot.go` (`slaNotifyOnce` + `startSLAInboxSweep` in the Discord-independent start block **and** the `onReady` Once re-call cluster). `internal/web/templates/inbox.tmpl` subtitle. `internal/web/inbox_page_test.go` if a kind-visibility case is cheap here.
- **Depends on:** nothing. Compiles against current `main` without Today.
- **Does:** Kind, watermarks, sweep, recipients, `inbox.Append`, tests 1–11, 14–18. **`send` is nil — no Discord.**
- **Scrutinize focus:** no stored breach flag; `computeCaseSLA` unchanged; Patch without `StampTurn`; empty involvement does not stamp; closed / no-round-start silent; reopen can re-alert; per-clock subject (not `Badge`); `fpCases` untouched; start from `Bot.New` not gateway-only; `QueueInbox` not used as the success signal.

### PR 2 — Discord I1 ping

- **Title:** `bot: one in-thread SLA mention with real AllowedMentions`
- **Files:** `internal/bot/sla_notify.go`, tests 12–13. Call existing package-local `discordNotifySend` from `notify_done.go` (same package — do not copy the AllowedMentions struct).
- **Depends on:** PR 1
- **Does:** One thread message per delivering sweep when `hasDiscordSurface` and `send != nil`. Inbox still first. No DMs. Unmapped ids stay in text, not in `Users`.
- **Scrutinize focus:** I1 (one message, not N); `NotifyThread` / `discordSend` **not** used; web-native → 0 Discord; sanitize/clamp; no local paths; resolution-only pass does not Discord-quote the first-response badge.

### After PR 2 lands on `main`

Run **full-train `/scrutinize`** on the accumulated diff vs the pre-train tip. Then mark this design **shipped** (the plan is already Accepted).

Do not mark the **implementation** green from a per-PR ship verdict alone.

---

## Out of scope (this train)

- Helpdesk wrap (Zendesk / Intercom → `CustomerRef`) and customer status token (research #4 items 2–3)
- Inbox writers for CI / decisions / escalate / deploy
- Priority vs FYI inbox IA
- Warning-before-breach / digest of “N cases at risk”
- DMs, auto-email, auto-SMS
- Stored breach flag; second SLA policy
- New SSE domain; Notifier interface (still deferred in web-primary P4)
- Changing `caseIsMine`, Today’s `unitInvolves`, or `fpCases`
- Discord `/sla` command
- Fixing team-review `NotifyThread` not pinging (pre-existing; do not copy)
- Changing `QueueInbox`’s nil-inbox → nil error contract

---

## Plan scrutinize (2026-09-06)

Intent: when an open case’s SLA clock crosses, involved people get an inbox event without looking at the board, without storing breach.

Traced against: `computeCaseSLA` / `caseSLAAt` / `CaseSLAFor` / `caseSLAText` / `caseSLAHold`, `fpCases` + `nextSLARecompute`, `inbox.Append` / `QueueInbox` (nil inbox → nil error) / `inboxItemVisible` / `fpInbox`, `discordSend` AllowedMentions vs `discordNotifySend`, `NotifyThread`, `hasDiscordSurface`, `discordMentionIDs` / `discordDMTarget`, `Store.Patch` vs `StampTurn` / `UpdatedAt` comment, `sessions.List` (`Listed.ThreadID`), `ResetCaseSLARound` / `ReopenCase`, `caseIsMine` vs Today `unitInvolves`, `Bot.New` Discord-independent starts vs `onReady` (`startBoardDigest` is onReady-only), idle/PR `sync.Once` pattern.

| Sev | Finding | Resolution |
|-----|---------|------------|
| Blocker | Phrasing items from `CaseSLA.Badge` — `caseSLAText` prefers first-response whenever that clock is breached, so a later resolution ping would look like a second FR ping. | Per-clock subject via `formatCoarseDuration(clock.Over())`. Test 4 pins resolution-while-FR-still-breached. |
| Major | `QueueInbox` returns nil when inbox is nil; using it as “delivered” would stamp watermarks and never retry. | `b.inbox.Append`. Leave `QueueInbox` alone. Sweep no-ops if `b.inbox == nil`. |
| Major | `waitingTitle` / `unitInvolves` may be absent on `main` (Today uncommitted). | Local `slaNotifyTitle` / `slaRecipients`. Never `caseIsMine`. |
| Major | `sweepSLAInboxAt(q, now)` implied a query object that does not exist. | `sweepSLAInboxAt(now, send)`. |
| Major | `discordSend` / `NotifyThread` would not actually mention anyone. | `discordNotifySend` + `Users` = snowflakes only. |
| Major | `Entry` has no thread id; `List` returns `Listed.ThreadID`. | UnitID / Patch / Discord target that key. |
| Nit | `Bot.New` in lifecycle tests would start the real ticker. | SLA tests use `&Bot{}` like `newRouteBot`. |
| Nit | Stopped+Breached in the 30s gap before the tick could be skipped if the sweep ignored `Stopped`. | Decision 16 + test 18. |
| Nit | Per-actor `appendInbox` hook on `Bot` for test 11. | Use `Append`’s existing unsupported-id error (`"bad id!"`). |
| Nit | Discord 4xx after a successful Append. | Stamp anyway; no Discord retry (run-done shape). |

Verdict after edits: **ship** (plan). Implementation still runs the [scrutinize contract](#implementation-scrutinize-contract) per PR and on the full train.

SCRUTINIZE_VERDICT: ship

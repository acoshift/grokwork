# Plan: deploy pipeline (manual trigger)

## Goal

Replace GitHub Actions **for deploys only** — not for CI or tests — with a manual, monorepo-aware
pipeline inside grokwork. One repo, many services, many environments (dev/stag/prod). A human opens
the project workspace, picks a service, an environment, and a git ref, and clicks Deploy.

grokwork already owns every other step from "a human asks for a change" to "the change is on `main`":
a Discord mention starts an agent in a per-thread worktree, the web UI reviews the diff, the ship
board pushes or merges. Getting that commit onto an environment is the one step that lives elsewhere,
which means the deploy plan is invisible from the workspace where the work happened, the credentials
live in a second system, and "who deployed what, when, at which SHA" is a browser tab away in a
different product.

The pipeline definition lives **in the repo** at `.grokwork/deploy.yaml` and is read **from the
deployed SHA**, so it is versioned with the code it deploys and reviewed like any other change.
Policy and credentials live in per-project `config.json`, which is never committed and differs per
host.

**Phase 1 trigger is a human clicking a button.** Nothing automatic. `docs/design-no-pr-mode.md:36`
already lists "Auto-deploy after push" as an explicit non-goal, and TODO.md design principle 3 is
"human authority is explicit".

## Threat model

This has a higher blast radius than anything else in the product. A step is `sh -c` running with
production credentials against live infrastructure, and **the command text comes from a file in a git
repo that anyone with push access can author**. Most decisions below are about that, not the UI.

Three controls, in order of how much they actually protect:

1. **Per-environment ref allowlist** (K7) — prod accepts only the project primary branch, so
   "push a branch that rewrites `deploy.yaml`, deploy it to prod" is not a path.
2. **Per-environment capability gate** (K8) — deploying to a protected environment needs a
   capability an ordinary builder lacks.
3. **Manifest validation** (K7) — bounds the work one click can request. It cannot make a hostile
   *command* safe; it is a blast-radius limit, not a security boundary.

Log redaction (K6) is defence-in-depth only. A step can encode a secret; nothing stops that.

## Non-goals (phase 1)

- Automatic triggers of any kind (push, cron, merge, webhook)
- Parallel steps or a step DAG; cross-service dependency ordering
- Approval flows needing a second person
- Drift detection
- Rollback beyond "redeploy an older SHA"
- Discord commands for deploying (the trigger is a web button)
- Typed platform-specific step kinds
- Secrets anywhere but `config.json`

---

## Key decisions

### K1 — One new package, `internal/deploy`

Disjoint files: `manifest.go`, `env.go`, `redact.go`, `step.go`, `store.go`, `engine.go`,
`recover.go`, `notify.go`.

Rejected `internal/bot`: it would give free access to `b.drainWG` (`internal/bot/stop.go:35`) and the
Discord seams, but `bot.New` starts background goroutines and builds five stores, so every deploy
unit test would boot all of that. Rejected a third `grokrun` driver: the `driver` interface
(`internal/grokrun/driver.go:14`) has 11 methods of which 7 are meaningless for a shell step, it is
unexported, and its selection switch is closed (`internal/grokrun/agent.go:164`) — a `shellDriver`
would be seven no-op stubs to reach `args()`.

### K2 — The manifest is read from the revision, never the working tree

`LoadAt` runs `git ls-tree`/`git cat-file` against the rev. The shared main checkout is routinely
dirty — CLAUDE.md's workflow section has parallel agents editing it — so reading the working tree
would show, and run, a pipeline that is not the one committed with the code being deployed.

Size is probed with `git ls-tree -l` **before** any read, because a `Runner` buffers all of stdout
in memory (`internal/ghpr/ghpr.go:34-53`): checking a 64 KiB cap after reading would not be a cap.
`ls-tree` also exits 0 with empty output for a path that does not exist, so "no manifest" is a value
(`ErrNoManifest`) rather than an error string to pattern-match.

### K2b — Fetch, never pull; and fetch at trigger time

Everything reads through **remote-tracking refs**: `PrimaryStartRef`
(`internal/gitworktree/fetch.go:89`) resolves `refs/remotes/origin/HEAD`, then
`origin/main`, `origin/master`, … and only falls back to `HEAD`. Those move on
`git fetch` alone, so **a pull is never required** and the local branch is never
consulted — which is what stops a stale local checkout from deciding what
production runs.

The corollary is that a fetch *is* required, and the background idle fetch is not
enough to rely on: it is throttled by `repoFetchIntervalMinutes` (default 5) and
an operator may set it to `0`, at which point the remote-tracking ref can be
arbitrarily old. `Trigger` therefore runs `FetchOrigin` before resolving the ref.
Without it, "deploy main" silently deploys whatever the last fetch saw — always
wrong in the stale direction, which is the worst shape for a deploy.
`TestTriggerFetchesBeforeResolvingRef` pins this against a real clone and fails
when the fetch is removed.

Two deliberate exceptions:

- **A fetch failure is not fatal.** It is logged and the run proceeds on what is
  already known. A rollback during a GitHub outage still has to work, and the
  SHA that actually ran is recorded on the run either way.
- **Redeploy never fetches.** Its SHA is pinned, so there is nothing to refresh
  and it must work with the remote unreachable — exactly when a rollback is
  needed. `TestRedeployDoesNotNeedTheNetwork` deletes the origin and pins it.

The board fetches too, throttled by the project's own interval so a page load
stays cheap and an operator who disabled idle fetch keeps that choice.

### K2c — A protected environment deploys the commit that was reviewed

K2b's fetch closes a staleness hole but opens a small one: the trigger resolves
the ref *after* fetching, so a push between page load and click would ship a
commit nobody looked at. Seconds wide, and irrelevant for dev — but "I reviewed
that exact commit" is the entire point of a capability gate.

So a gated environment's trigger carries `expect_sha`, and the engine refuses
when the resolved SHA differs, naming both commits and telling the operator to
reload. It is **fail-closed**: a gated trigger with no expectation is refused
rather than silently exempted, because the only caller that omits one is a
caller that has not shown anyone a commit.

The board reinforces it rather than relying on the check alone: a gated cell
renders the ref as static text plus hidden inputs, so there is no free-text box
whose value could disagree with the SHA beside it. To ship a different commit to
a protected environment, use Redeploy on a past run — SHA-pinned, and therefore
exempt from the expectation by construction, which is also what keeps rollback
working during an incident.

Ungated environments impose nothing: deploying the current tip is the intent
there, and the ref stays editable.

### K3 — Two composition mechanisms, because environments need different pipelines

Real environments do not differ by a variable: dev applies straight from a branch, stag gates on a
migration and a smoke test, prod promotes the image stag already verified, backs up, and canaries.

- `pipelines.<env>` fully replaces the default `steps` for that environment.
- a step-level `envs: [prod]` filter is the economical answer to "same pipeline, one extra step".

Resolution, in `Manifest.Resolve`: environment must be declared → service `envs` narrowing must
allow it → pipeline is `pipelines[env]` if present else `steps` → drop steps whose `envs` excludes
this environment. The resolved list is **frozen onto the run record**, so a redeploy replays exactly
what ran even if the branch has moved.

A `pipelines` entry for an environment the service's `envs` excludes is a hard error rather than
silently honouring one of the two.

### K4 — A reserved `x-anchors` key, typed `yaml.Node`

`KnownFields(true)` is on, because a typo silently dropping a deploy step is far worse than a hard
failure — and it rejects **every** unknown top-level key, so anchor definitions would have nowhere to
live. `x-anchors` is a declared home for them, held as a raw node: nothing in it is expanded,
validated, or executed.

Verified against `yaml.v3` v3.0.1: aliases defined there resolve where they are *used*; merge keys
(`<<: *base`) survive strict decoding with local keys winning; and an alias bomb does not expand
against a typed schema, because YAML has no sequence splat — `[*a, *a]` is a *nested* sequence and
fails as `cannot unmarshal !!seq into Step` at the first level. What bounds the result is the 64 KiB
probe plus the post-parse limits, not any library-internal alias counter.

### K5 — Env is built by allowlist, never inherited

The user rejected both host-env inheritance and per-environment env files, so a step's environment is
exactly three layers, in increasing precedence:

1. a fixed base copied **by name** from the host: `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`,
   `LC_ALL`, `TZ`, `TMPDIR`, `SSH_AUTH_SOCK`, plus `TERM=dumb`. Without `PATH` and `HOME` nothing
   resolves and `docker`/`kubectl`/`gcloud` cannot find their own configs.
2. injected run vars: `GW_PROJECT`, `GW_SERVICE`, `GW_ENV`, `GW_REF`, `GW_SHA`, `GW_SHORT_SHA`,
   `GW_RUN_ID`, `GW_STEP`, `GW_ACTOR` (display name only, never a Discord snowflake). A manifest or
   env-map key starting with `GW_` or `GROK_WORK_` is rejected so identity cannot be forged.
3. the per-environment map from config — highest precedence, so an environment may deliberately
   override `KUBECONFIG`, `DOCKER_CONFIG`, or `HOME`.

This is an **allowlist**, deliberately stronger than `grokrun.FilterChildEnv`'s denylist
(`internal/grokrun/env.go:32`), which still passes any host credential whose name it does not know.
The asymmetry with agent children is intentional: do not "unify" the two.

### K6 — Secrecy is marked, not inferred; redaction happens once, at the log writer

Per-environment values carry an explicit secret flag (`secretKeys`). A length heuristic was rejected:
the same map holds non-secret config like `K8S_NAMESPACE` and `IMAGE_REPO`, and redacting those makes
a failed deploy's log unreadable exactly when it matters, while still missing short secrets.

The redactor wraps the log `io.Writer`, so occurrences become `••••` before bytes reach disk and
every downstream surface (web tail, Discord, raw log) inherits it from one place. The value set is
each marked value plus its base64 **decoding** when it decodes cleanly to ≥ 8 printable bytes — the
`base64 -d` kubeconfig pattern means the decoded form is what a tool echoes; adding the *encoding* of
an already-encoded value matches nothing. Carry-over across writes is bounded by an actual partial
prefix match, not by `max(len(secret))`, or an 8 KiB value stalls the live tail by 8 KiB.

`config.Snapshot()` exposes only `EnvKeys`/`EnvCount`, never a value — the precedent is
`LinearAPIKeySet bool` (`internal/config/config.go:194`). Values never enter audit `Detail`, the
timeline, or a template.

### K7 — Manifest limits and the ref allowlist

Limits (all in `manifest.go`): ≤ 64 KiB, ≤ 16 environments, ≤ 20 services, ≤ 30 steps per pipeline,
command ≤ 4000 bytes, step timeout ≤ 1h. Names match `^[a-z0-9][a-z0-9._-]{0,63}$` (environments the
stricter `^[a-z][a-z0-9-]{0,31}$`, since they become path segments) and are unique. `dir` must be
relative, `..`-free, and resolve inside the repo.

The control that matters is per-environment `allowedRefs`: prod defaults to the project primary branch
only (`gitworktree.ResolvePrimaryBranch`, `internal/gitworktree/direct_ship.go:21`), dev to any ref.

**Redeploy waives the ref check** when the source run is a succeeded run on the same lane: that SHA
already passed the gate and already ran with these credentials, and re-asserting reachability would
block rollback during an incident. Capability is still re-checked. The run and the audit entry record
`refCheck: waived_redeploy`.

### K8 — Gating: a `deploy` web feature flag plus a per-environment capability

`webAuth.features.deploy` and a `case "deploy"` in `requireFeature` (`internal/web/writes.go:23`).

**Consequence, accepted:** `featureFlag` (`internal/config/webauth.go:130`) returns false whenever
`webAuth.enabled` is false — "open LAN cannot enable writes" — so **deploys require web auth to be
on**. That is correct: a prod deploy needs an identity for the audit trail and the capability check.
It matches `startSessions`.

Per environment, `requireCapability` names a `config.Capabilities` field
(`approve` | `adminProject` | `safeOps` | `merge`) resolved through `cfg.ResolveCapabilities`. An
environment naming none defaults to `CanShip()` (builder-class), matching every other money/risk gate
(`internal/web/start.go:36`, `fix.go:789`, `reviews.go:444`).

### K9 — One detached checkout per run, outside `worktreesRoot`

`gitworktree.AddDetached(ctx, repo, path, sha)` (`internal/gitworktree/worktree.go:282`) checks out
one commit and refuses the main repo path.

Path is `data/deploys/checkouts/<project>/<runID>`. Two things this gets right:

- **Per-run, not per-lane.** A flattened `service-env` directory collides (`web-app`+`prod` vs
  `web`+`app-prod`) and lanes run concurrently. It also sidesteps a real trap: `AddDetached` reuses an
  existing worktree whenever `rev-parse HEAD` matches (`worktree.go:304-317`) with **no cleanliness
  check**, so a shared checkout kept after a failure would let stale build output ship on the retry.
  Cost: no cross-run file reuse. Accepted — Docker layer cache lives in the daemon, not the worktree.
- **Not under `config.WorktreesRoot()`.** `gitworktree.ListOnDisk` (`internal/gitworktree/idle.go:24`)
  treats every `<root>/<project>/<dir>` as a thread worktree, so a deploy checkout there would appear
  in the worktrees UI and be deleted by `pruneIdleWorktrees` (`internal/bot/idle_cleanup.go:57`).
  Detached means no branch, so `IsManagedBranch` deletion never applies either way.

Removed on terminal status; orphans swept at startup.

### K10 — Own the kill path: `grokrun.KillProcessGroup` misses a SIGTERM-trapping grandchild

Verified in `internal/grokrun/process_unix.go:19-35`, and pinned by a mutation test.

It *does* send a group SIGTERM, so well-behaved grandchildren die there — the gap is narrower than
"it does not reap grandchildren". After that SIGTERM the 2 s loop polls `Kill(pid, 0)` — **the leader
only** — and returns the instant the leader is gone, so the `Kill(-pid, SIGKILL)` on line 32 is
unreachable whenever the leader exits promptly, which is the normal case for `sh -c`. A grandchild
that traps or ignores SIGTERM therefore survives the run. Build tools trap TERM to clean up, so this
is a real case, not a theoretical one. Separately, `exec.CommandContext`'s default `Cancel` kills only
`cmd.Process`.

`TestRunStepTimeoutKillsGrandchild` uses a grandchild with `trap '' TERM` precisely so it fails when
the group poll is swapped for a leader poll; an earlier version used a plain `sleep`, which the
group SIGTERM alone already killed, and so passed against both implementations.

The step runner therefore:

- neutralises exec's own cancel (`cmd.Cancel` returning nil, plus `cmd.WaitDelay`) so the leader-only
  kill cannot preempt the group kill;
- calls `grokrun.SetProcessGroup` — the unexported `setProcessGroup` (`process_unix.go:11`) renamed
  and exported. This is the only export added to an existing package purely for reuse here;
- on timeout/cancel: `Kill(-pgid, SIGTERM)` → poll **`Kill(-pgid, 0)` (the group)** → unconditional
  `Kill(-pgid, SIGKILL)`;
- uses `os.Pipe()` and hands the write end to `cmd.Stdout`/`cmd.Stderr` rather than an `io.Writer`,
  so `exec` passes the fd directly and `cmd.Wait()` cannot block forever on a copy goroutine a
  daemonized grandchild holds open;
- **always joins the reader goroutine** before closing the log file and recording `LogBytes`. The
  grace timer decides *when* to force the read end closed, never whether to join.

### K11 — A deploy is never auto-resumed after a restart

`recover.go`'s agent resolution — re-drive with a "continue without duplicating completed steps" note
(`internal/bot/journal.go:21`) — is valid for an agent turn and invalid for `kubectl apply`.

At startup: `running` → `interrupted` (recording the step index it died on), `pending` → `cancelled`
("process restarted"). Both render with a Redeploy button, so recovery is one informed human click.

PID handling probes the **group** with `Kill(-pid, 0)`, not `ProcessAlive(pid)`: a crashed deploy
whose `sh` leader exited but whose children escaped would otherwise read as dead and let a new
deploy race a live orphan. Never signal a PID that cannot be verified — mark the lane blocked instead
(the `blocked_orphan` precedent, `internal/runjournal/journal.go:19`).
`runjournal.LooksLikeAgentCLI` is **not** reusable: its signature is `(pid, agent, bin)` and it
hardcodes substring `"grok"` (`internal/runjournal/lock.go:107-113`), so it is meaningless for a
`docker` process.

### K12 — Store: one atomic file per run, modeled on `runjournal`

`internal/runjournal` is the only store in the repo with atomic `.tmp`+`Rename` writes, a status
enum, an `Update(id, fn)` RMW API with `ErrSkipUpdate`, a `SchemaVersion`, and a path-sanitizing id
guard (`internal/runjournal/journal.go:19,30,135,178,211,242`). Mirror all of it.

`data/deploys/runs/<runID>.json` is the single source of truth including per-step records. There is
no separate queue file: active and queued runs are those with status `running`/`pending`, and the
in-memory per-lane index is rebuilt by a `List()` scan at startup.

Rejected `sessionstore` (one mutable record per key, ~130 fields, no history, non-atomic
`os.WriteFile`). Rejected `internal/timeline` for logs: its package doc excludes the live tail by
design, and `maxEventsPerUnit = 5000` (`internal/timeline/timeline.go:96`) would `ErrFull` in minutes
of build output.

### K13 — Logs are plain files, and the log file *is* the live transport

`data/deploys/runs/<runID>/steps/<NN>-<slug>.log`, appended as the step runs. The page tails it by
byte offset; a viewer arriving mid-run passes `after=0` and gets everything, so catch-up is free.

Rejected `sseStream` + `GrokStream` (`internal/web/sse_stream.go:21`, `layout.tmpl:3803`): one-shot
and per-request, it dies when the operator closes the tab and has no catch-up. A deploy must outlive
the browser.

Three details that are easy to get wrong:

- **Overflow is head+tail, not silent truncation.** 2 MiB per step as head 512 KiB + an
  `… N bytes elided …` marker + a 1.5 MiB tail ring, so both the invocation context and the failure
  survive.
- **Split on `\r` *or* `\n`**, plus a ~500 ms time-based flush. `kubectl rollout status` and
  `docker build --progress` emit carriage-return progress with no newline for minutes; a plain
  `bufio.Scanner` shows nothing at all.
- **The raw-log link needs `hx-boost="false"`.** The shell boosts every anchor with
  `hx-target="#live-root"` (`layout.tmpl:3619`), so a `text/plain` response renders blank. Precedent:
  `layout.tmpl:3734`, `login.tmpl:16`.

### K14 — Concurrency: lane key `project/repo/service/env`

One active run per lane, FIFO queue behind it, cap 5 (mirroring `maxFollowupQueue`,
`internal/bot/bot.go:31`), plus a global `maxConcurrentDeploys` (tri-state `*int`, nil = 4). Different
services deploy in parallel, and the same service to dev and prod in parallel — different lanes,
intentional.

Two patterns copied from `claimOrEnqueueInternal` (`internal/bot/bot.go:316`): the durable write
happens **inside** the RAM lock, and a failed write **rolls back** the RAM change so a claim fails
rather than diverging. One pattern deliberately **not** copied: its "replace the last queued item by
the same author" semantic — two deploys of one service at different SHAs are not interchangeable.

Read-only paths use `sync.Map.Load`, never a `LoadOrStore` helper, or the map leaks an entry per
viewed lane (`internal/bot/queue_cmd.go:191`).

Duplicate suppression keys on **SHA only, not (SHA, actor)** — the motivating case is two operators
clicking the same prod cell within seconds.

### K15 — Engine lifecycle: a `stopping` gate and shutdown ordering that works

`Bot.Stop` sets `stopping` as its *first* statement (`internal/bot/stop.go:20`) and
`claimOrEnqueueInternal` refuses with `ErrShuttingDown`. The engine needs the same: an
`atomic.Bool stopping` checked inside the lane lock before any claim.

`main.go:93-98` is `stopCtx, cancel := …; b.Stop(stopCtx); cancel(); _ = webSrv.Shutdown()`. Anything
inserted "before `Shutdown()`" lands *after* `cancel()` and is handed a dead context, force-killing
every in-flight step. Correct order:

```go
_ = webSrv.Shutdown()                 // stop accepting triggers (GraceTimeout is 1ms)
depCtx, depCancel := context.WithTimeout(context.Background(), deploy.StopTimeout)
webSrv.Deploys().Stop(depCtx)         // cancel steps, group-kill, mark interrupted
depCancel()
stopCtx, cancel := context.WithTimeout(context.Background(), b.ShutdownTimeout())
b.Stop(stopCtx); cancel()
```

The engine is constructed in `web.New`, which already builds three collaborators and panics on
failure (`internal/web/web.go:69-80`) — that avoids churning `web.New`'s six call sites. Exposed as
`func (s *Server) Deploys() *deploy.Engine`, mirroring `bot.Reviews()`.

### K16 — The SSE fingerprint needs a monotonic rev

`sse` compares fingerprints once per 2 s tick and emits nothing when equal
(`internal/web/live.go:236-266`). A lane claimed, run, finished, and reaped inside one tick produces
an identical RAM-derived fingerprint before and after, so passive viewers would never see the deploy
at all. `fpDeploys()` includes an `Engine.rev uint64` bumped on every durable transition and reap.

---

## Manifest schema

```yaml
version: 1
environments: [dev, stag, prod]     # names only; policy and secrets live in config.json

# Reserved: anchor definitions only. Never executed, never validated as steps.
x-anchors:
  build:   &s_build   { name: build,   run: "docker build -t $IMAGE_REPO/api:$GW_SHA -f services/api/Dockerfile .", timeout: 20m }
  push:    &s_push    { name: push,    run: "docker push $IMAGE_REPO/api:$GW_SHA" }
  migrate: &s_migrate { name: migrate, run: "./scripts/migrate.sh --url \"$DB_MIGRATE_URL\"", timeout: 10m }
  smoke:   &s_smoke   { name: smoke,   run: "./scripts/smoke.sh https://$API_HOST/healthz" }

services:
  api:
    dir: services/api               # relative, must stay inside the repo
    steps:                          # default pipeline — any env with no `pipelines` entry
      - *s_build
      - *s_push
      - { name: apply, run: "kubectl -n $K8S_NAMESPACE set image deploy/api api=$IMAGE_REPO/api:$GW_SHA" }
    pipelines:
      stag: [*s_build, *s_push, *s_migrate,
             { name: rollout, run: "kubectl -n $K8S_NAMESPACE set image deploy/api api=$IMAGE_REPO/api:$GW_SHA" },
             { name: wait,    run: "kubectl -n $K8S_NAMESPACE rollout status deploy/api --timeout=5m" },
             *s_smoke]
      prod:                         # never builds: promotes the image stag verified
        - { name: verify-image, run: "docker manifest inspect $IMAGE_REPO/api:$GW_SHA > /dev/null" }
        - { name: backup-db,    run: "./scripts/backup.sh --tag pre-$GW_SHORT_SHA", timeout: 30m }
        - *s_migrate
        - { name: canary,       run: "./scripts/canary.sh api $GW_SHA 10" }
        - { name: canary-wait,  run: "./scripts/canary_check.sh api --for 5m", timeout: 8m }
        - { name: rollout,      run: "kubectl -n $K8S_NAMESPACE set image deploy/api api=$IMAGE_REPO/api:$GW_SHA" }
        - { name: wait,         run: "kubectl -n $K8S_NAMESPACE rollout status deploy/api --timeout=10m" }
        - *s_smoke

  web:
    dir: apps/web
    envs: [dev, prod]               # optional narrowing; default = all environments
    steps:
      - { name: build,      run: "npm ci && npm run build", timeout: 15m }
      - { name: sync,       run: "gsutil -m rsync -d -r dist gs://$WEB_BUCKET" }
      - { name: invalidate, run: "gcloud compute url-maps invalidate-cdn-cache $CDN_URL_MAP --path '/*'",
          envs: [prod] }            # dev has no CDN in front of it
```

`api` → dev runs 3 steps, → stag 6, → prod 8 with no build at all. `web` → dev runs 2, → prod 3,
→ stag is not offered.

Missing manifest → the deploys page shows a "not configured" state naming the expected path; triggers
refused. Malformed → the strict-decoding error with its line number; triggers refused.

## Config shape

```jsonc
"projects": { "shop": { "path": "/srv/repos/shop",
  "deploy": {
    "enabled": true,
    "environments": {
      "dev":  { "allowedRefs": ["*"],
                "env": { "IMAGE_REPO": "…", "K8S_NAMESPACE": "shop-dev", "KUBECONFIG": "…" } },
      "prod": { "requireCapability": "approve",
                "allowedRefs": ["main"],
                "stepTimeoutMaxMs": 1800000,
                "env": { "IMAGE_REPO": "…", "K8S_NAMESPACE": "shop-prod", "DB_MIGRATE_URL": "…" },
                "secretKeys": ["DB_MIGRATE_URL"] }
    }
  }
}}
```

A new nested per-project field must be registered in **five** places or the next web config save
wipes it: the type; the `ProjectConfig` field (pointer + `omitempty`, nil'd when empty per
`SetProjectGitHubRepos`, `internal/config/github.go:320`); normalization in
`ProjectsMap.UnmarshalJSON` (`internal/config/project.go:102-116`); the `outObj` whitelist in
`ProjectsMap.MarshalJSON` (`:128-167`); and the copy in `cloneProjectsMap` (`:193-211`).

No `saveLocked` change is needed for those — `Projects` and `WebAuth` are whitelisted as wholes. The
**root** fields `maxConcurrentDeploys` and `deployRunRetention` **do** need adding to the anonymous
whitelist in `saveLocked` (`internal/config/config.go:607-693`), with a pin test following
`internal/config/save_wave1_fields_test.go:11`.

## Web surface

```go
GET  /projects/{project}/deploys                  // board + composer
GET  /projects/{project}/deploys/{runID}          // run detail
GET  /projects/{project}/deploys/{runID}/log      // byte-offset tail fragment
GET  /projects/{project}/deploys/{runID}/log.txt  // raw, hx-boost="false"
POST /projects/{project}/deploys                  // trigger
POST /projects/{project}/deploys/{runID}/cancel
POST /projects/{project}/deploys/{runID}/redeploy
GET  /config/projects/{name}/deploy               // settings tab
POST /config/projects/deploy/environment
```

All under `/projects/{project}/…`, so `navScopeFromURL` (`internal/web/project_home.go:29`) and its
`layout.tmpl` JS twin need no change; a global `/deploys` would need both.

**Every deploy trigger form lives outside the live region** — not only the composer (the
`session.tmpl` typed-input rule) but the per-lane Deploy buttons. The confirm handler bails on a
detached form (`isConnected` guard) and the swap-pause edit check only covers an editable control
**inside** the live-region being swapped (`isEditingInside`), which an open dialog is not. A
confirmed prod deploy would otherwise silently do nothing. Belt and braces: the pause hook also
gains `if (dlg.open) { e.preventDefault(); return; }`.

The nav anchor goes in `.nav-links` only, not the phone `.tabbar`. The tab bar is a curated set of
five primary sections; Issues, Linear, Commits, Worktrees, and Settings are all absent from it too.

## Discord notification

One message to the project's `discordChannelId` on terminal status, reusing the delivery seam from
`internal/bot/notify_done.go:110` (factored to take an outcome rather than a `grokrun.Result`),
clamped by `clampDiscord` (`internal/bot/verify.go:245`). Never a local filesystem path.

When no channel is configured (`config.ErrNoDiscordChannel`, `internal/config/github.go:233`), fall
back to `inbox.Append` for the triggering actor on any non-success outcome
(`internal/inbox/inbox.go:102`) — otherwise a web-only project's failed prod deploy notifies nobody.
A notification failure never fails the run.

---

## PR slices

| # | Slice | Acceptance |
|---|---|---|
| 1 | Manifest package + read-only deploys page + this doc | `TestParseMonorepoManifest`, `TestParseRejects` (hostile table), `TestParseRejectsAliasBombWithoutExpanding`, `TestLoadAtRefusesOversizeBlobWithoutReading`, `TestDeploysPageRendersManifest`, `TestDeploysPageNotConfigured` |
| 2 | Config: policy, environments, secrets UI | `TestSaveLockedPreservesDeployFields`, `TestProjectWhitelistPreservesDeploy`, `TestSnapshotNeverExposesDeploySecret` |
| 3 | Store + step runner, no trigger | `TestRunStepTimeoutKillsGrandchild`, `TestStepReaderJoinedBeforeLogClose` (`-race`), `TestRedactCoversDecodedBase64`, `TestStepLogKeepsHeadAndTailOnOverflow`, `TestEnvAllowlistDropsHostCredentials` |
| 4 | Trigger + run page + live log | `TestPostDeployRequiresCapability`, `TestDeployLogTailOffset`, `TestLiveRevsChangesForRunWithinOneTick`, `TestDeployPartialHasNoChrome`, `TestRawLogLinkOptsOutOfBoost` |
| 5 | Queue + concurrency + restart recovery | `TestSecondTriggerQueues`, `TestQueueCapRejects`, `TestDoubleTriggerSameShaDifferentActorDedupes`, `TestRunningBecomesInterruptedOnRestart`, `TestRecoverBlocksLaneWhenGroupAliveButLeaderGone`, `TestTriggerRefusedWhileStopping` |
| 6 | Redeploy + Discord notification | `TestRedeployReplaysFrozenSteps`, `TestRedeployWaivesRefCheckOnSameLane`, `TestDeployNoticeHasNoLocalPath`, `TestDeployNoticeFallsBackToInboxWithoutChannel` |
| 7 | Docs: README, CLAUDE.md, TODO.md, `config.example.json` | PATH poison list extended with `docker`, `kubectl`, `helm`, `gcloud` |

Slice 1 delivers observable value on its own: you can see the pipeline your repo declares, and
nothing executes.

## Tests

Seams are the repo's existing ones. Fake `sh` scripts written into `t.TempDir()` at 0755
(`internal/bot/task_start_test.go:219`); `sleep 60` binaries for cancel/timeout
(`internal/bot/resume_test.go:269`); a step that spawns a grandchild (`sh -c 'sleep 60 & wait'`)
proving the K10 group kill reaps it; a step that `echo "$SECRET"` proving the log file holds `••••`;
`runtime.GOOS == "windows"` skips on script tests; an injected `deploy.Runner` for git plumbing.

`TestMain`'s PATH poison (`internal/bot/main_test.go:21`) gains `docker`, `kubectl`, `helm`, `gcloud`
so a test that forgets a fake cannot deploy for real.

Visual review through the existing preview harness, with a `deploy.SeedRunForTest` mirroring
`bot.SeedActiveRunForTest` (`internal/bot/test_helpers.go:71`) seeding one in-flight, one failed, and
one queued run:

```
GROKWORK_WEB_PREVIEW=1 GROKWORK_WEB_PREVIEW_DELAY_MS=800 \
  go test ./internal/web -run TestPreviewServer -timeout 0
```

## Rejected alternatives

| Alternative | Why not |
|---|---|
| Steps in `config.json`, edited in the web UI | The pipeline would not be versioned with the code it deploys, and every change would bypass review. `verifyCommands` gets away with it because a verify command is a local check, not a production mutation. |
| A third `grokrun` driver | 7 of 11 interface methods are meaningless for a shell step; the interface is unexported and its switch closed (`internal/grokrun/agent.go:164`). |
| Reuse `grokrun.Run` for steps | No argv field, mandatory prompt-file/stdin dance, newline-delimited-JSON stream decoding, token/session-shaped `Result`, and a session-retry wrapper (`run.go:187`) that would re-run a deploy step on the wrong stderr match. |
| Model a deploy as a web-native unit (`startWebNativeUnit`) | Allocates a managed branch, a session entry, and an agent session — none of which a deploy has. Contradicts the "one thread = one worktree = one branch = one agent session" invariant. |
| Log lines as `timeline` events | The package doc forbids it and `maxEventsPerUnit = 5000` would `ErrFull` in minutes. |
| `sseStream`/`GrokStream` modal for the live log | Per-request and one-shot: dies with the tab, no catch-up for a viewer arriving mid-run. |
| Inherit the host env for steps | Rejected by the user; also gives every step every cloud credential on the box regardless of environment, defeating dev/stag/prod separation. |
| Length-based secret detection | Redacts non-secret config (making failure logs unreadable) and misses short secrets. |
| Shared per-lane checkout | `AddDetached` reuses a matching-HEAD worktree with no cleanliness check, so a retry after a failure would build on stale output. |

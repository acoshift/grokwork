# Agent sandbox: OS-native confinement for Grok child runs

| | |
|---|---|
| Status | Draft (rev. 2 — post adversarial review) |
| Date | 2026-07-22 |
| Repo | `github.com/acoshift/grokwork` |
| Audience | Operator / maintainer |
| Related | TODO.md:176 "Network/command egress allowlist or OS sandbox (container/bubblewrap)"; Layer A env filter (`internal/grokrun/env.go`); TODO.md "Dual-control for blast-radius config changes", "`requirePushApproval`"; `docs/design-no-pr-mode.md` (K4 stickiness precedent, deliberately diverged from here) |

**Abstract.** Every ship-capable run today execs `grok --yolo` as the bot's own OS user with no confinement below the process boundary: worktree scope, "don't read `$HOME`", "never push to main" are prompt text. This design makes the `grok` child an OS-enforced prisoner of `{its worktree, the shared repo `.git`, a bot-provisioned agent home, a per-run scratch dir}` with deny-by-default network, using only mechanisms native to the host OS — Seatbelt (`sandbox-exec`) on macOS, bubblewrap on Linux, Landlock as the Linux fallback — plus an in-process egress proxy. No Docker, no VMs, no new daemons. The bot's own `git`/`gh`/`ps` execs stay unsandboxed; the core invariant ("bot owns deterministic git, Grok owns judgment") is unchanged — we are sandboxing *judgment*, not *plumbing*. A **basic** tier ships default-on immediately as a *non-breaking* floor (resource caps + scratch tmp; it makes no read/write/egress claim); the security-critical properties arrive with the opt-in **fs** tier and the **net-observe → net-enforce** ladder. In particular, the CRITICAL token-exfil path (S2) is **not** closed by the default configuration — it is closed only at `net-enforce`, and this is stated wherever S2 appears. Prior art we deliberately copy: anthropics/sandbox-runtime ("srt") ships exactly this shape (generated Seatbelt profiles, bubblewrap, deny-all network with a host-side filtering proxy) in production under Claude Code.

---

## Motivation: problem today

The single authorization boundary in the system is the per-project Discord allowlist (`internal/bot/bot.go` `onMessage` gates, fail-closed via `project_members.go`). Passing it grants arbitrary code execution as the bot user, because the child process is a full-privilege sibling of the bot itself:

- `grokrun.Run` (`internal/grokrun/run.go:249-256`) execs `grok` with `cmd.Dir` set to the worktree and either a filtered env or — for `SummarizeTitle` (`run.go:801`, called from `internal/bot/bot.go:1251`) — **unfiltered `os.Environ()`**.
- Layer A (`internal/grokrun/env.go:10-49`) filters *environment variables* only. The child's expected tools re-authenticate from disk: `gh` reads `~/.config/gh/hosts.yml`, `git+ssh` reads `~/.ssh`, `grok` reads `~/.grok`. Stripping `XAI_`/`GH_TOKEN` from the env is largely moot while those files are readable.
- Worktrees live at `<DataDir>/worktrees/<project>/<threadId>` where `DataDir = <configDir>/data` **inside the bot's own repo**, so the bot repo root — containing `config.json` with the live Discord token — is **four directories up** (`../../../../config.json`) from every run's cwd.
- Network egress is unrestricted, and `DisableWebSearch` is never set for real task runs (the zero value `false` is left in place in `executeTask`, `bot.go` ~1543-1606). `curl` reaches the LAN, the admin UI on `:8787`, Tailscale peers, and cloud metadata IPs.
- One injected attachment (`attachments.go:127`), poisoned replied-to message (context assembled ~`bot.go:1470-1524`), or hostile repo comment is prompt-concatenated and acted on under `--yolo`: injection ≡ RCE.

TODO.md:176 has carried the fix as an open item. This document is that design.

> **Citation note.** Line numbers below are against current `main` (post-Wave-3). Where a symbol may drift, the symbol name is authoritative.

---

## Threat model

### Assets reachable by the child today

| Asset | Location | Why reachable |
|---|---|---|
| Live Discord token, Linear keys, project paths | `config.json` in bot repo root | worktree cwd is inside the bot repo; `../../../../config.json` |
| `gh` auth token | `~/.config/gh/hosts.yml` | disk auth ignores env filtering |
| SSH private keys | `~/.ssh/id_*` | plain read; usable for pushes to any remote |
| xAI / Grok credentials + all session transcripts | `~/.grok/` | child's own home; cross-project transcripts included |
| Cloud creds, kubeconfig, npmrc, browser profiles | `~/.aws`, `~/.config/gcloud`, `~/.kube`, `~/Library/…` | arbitrary `$HOME` read |
| Other projects' checkouts | every `projects.*.path` | absolute paths, same UID |
| Other threads' worktrees, bot runtime state | `data/worktrees/*/*`, `data/{sessions.json,history,audit,runs,attachments}` | same repo tree |
| Main checkout `.git` incl. `hooks/`, `config`, the worktree gitlink + per-worktree gitdir | shared via `git worktree` gitlink | write a hook or redirect the gitlink → the **bot's own unsandboxed** git executes attacker config/hooks |
| LAN + admin UI | `:8787`, Tailscale peers, metadata IPs | unrestricted sockets |

### Attack scenarios (ranked)

- **S1 — secret exfil by file read (CRITICAL).** Any run — including investigate mode, whose `Tools:"read_file,grep"` allowlist (`run_policy.go:70,133`) limits *tools, not paths* — reads `config.json` or `~/.ssh/id_rsa` and streams it into the Discord reply. The thread *is* the exfil channel.
- **S2 — credential theft → external abuse (CRITICAL).** Read `~/.ssh` / `gh auth token`, then `curl -d @-` to an attacker or push with stolen keys. **Also reachable in the default posture** via the ambient `GH_TOKEN` a ship run legitimately holds while egress is unconstrained (see K7 token gating).
- **S3 — prompt injection (CRITICAL).** Attachments, replied-to messages, repo content, fetched web pages — all untrusted, all concatenated, all executed under yolo.
- **S4 — destructive git/remote actions (HIGH).** `git push --force origin HEAD:main`, remote branch deletion, `gh pr merge` — prompt-forbidden only; `IsManagedBranch` (`gitworktree/worktree.go:145`) constrains the bot's git, not the child's.
- **S5 — writes outside the worktree (HIGH).** `rm -rf ~/…`, overwrite `config.json`, plant `.git/hooks/pre-commit`, or **rewrite the worktree gitlink / per-worktree `commondir` to redirect the bot's git at attacker config** (see K2).
- **S6 — SSRF / LAN pivot (HIGH).** `curl http://localhost:8787` against the open-LAN-mode admin UI; metadata IPs.
- **S7 — exfil-by-staging (MEDIUM).** `cp ~/.ssh/id_rsa ./x.txt` then `DISCORD_UPLOAD: x.txt` — the upload guard (`upload.go:82`) validates location, not provenance.
- **S8 — resource exhaustion (MEDIUM).** No CPU/mem/PID/disk caps within the 30-minute timeout.
- **S9 — malicious allowlisted insider (MEDIUM).** Membership grants RCE-equivalent power; no dual-control (separate TODO).

### Required security properties

**P0 (must have — each closes a CRITICAL/HIGH path):**

- **P0.1 Read confinement** — child reads only its worktree, the shared repo gitdir, its agent home, scratch. Test: `cat ~/.ssh/id_rsa` and `cat <botrepo>/config.json` fail with EPERM.
- **P0.2 On-disk credential isolation** — `~/.ssh`, `~/.config/gh`, `~/.aws`, the operator's `~/.grok`, browser profiles unreadable; GitHub capability delivered as a scoped env token, not ambient disk auth.
- **P0.3 Write confinement** — writes limited to worktree + gitdir (minus the exec-config surface in K2) + agent home + scratch. Test: writing `config.json`, `.git/hooks/pre-commit`, or the worktree gitlink fails.
- **P0.4 Network egress allowlist** — only approved hosts (xAI API, GitHub, configured registries); LAN, `:8787`, metadata IPs, arbitrary internet, and DNS tunnels denied. **Requires the netns (bwrap) backend** — the Landlock fallback cannot deliver P0.4 (see K3/K9).
- **P0.5 Cross-tenant isolation** — thread A cannot reach thread B's *worktree working files*, nor project Y's checkout. **Scoped:** it does *not* cover the shared per-project `.git` object store or local refs, nor same-project transcript reads (see residual row below and Non-goals).

**P1 (should have):** P1.1 resource limits (PID/mem/fd caps); P1.2 git blast-radius limits (no force-push/merge from the child); P1.3 credential scoping (per-project fine-grained token); P1.4 admin-UI auth hardening (separate TODO, out of scope here).

**P2 (defence in depth):** P2.1 untrusted-content provenance fencing; P2.2 exfil minimization (staging bypass dies at the read step once P0.1 holds); P2.3 auditability of egress **and sensitive-path access** (both, per K12).

---

## Goal

Deliver P0.1–P0.3, P0.5 (scoped as above), and P0.4 **opt-in per project** (see rollout decision point), plus partial P1.1/P1.3 and P2.2/P2.3, by wrapping the single exec choke point (`run.go:249`) in an OS-native sandbox with deny-by-default filesystem and (where the backend permits) network policy, phased in as: a non-breaking **basic** tier default-on immediately, an **fs** tier (OS filesystem confinement), a **net-observe** tier (egress audited), and a **net-enforce** tier (egress allowlisted). P0.4's default-on flip is a scheduled decision, not part of the initial rollout (see Rollout). Seatbelt's `sandbox-exec` has carried a deprecation notice since macOS 10.11, yet Apple ships hundreds of `.sb` profiles and Chrome and Bazel depend on it; we treat "deprecated" as *unsupported API surface, stable in practice*, and hedge with a backend interface (K3) so a container backend can slot in later without touching callers.

## Non-goals (v1) and known bypasses

This section is deliberately blunt. The sandbox shrinks the child's reach; it does not make injected instructions safe.

- **Prompt injection itself (S3) is not stopped.** Injected instructions still execute — *inside* the cage. Garbage commits, misleading PR text, and hostile comments via the legitimately held token remain possible.
- **S2 is open in the default posture.** Because ship runs need a token to push and the default mode is below `net-enforce`, the token-exfil leg of S2 stays open until an operator raises a project to `net-enforce` (or the v2 broker lands). The default provides no egress confinement; do not read "default-on" as "S2 closed." See K7 token gating for the loud runtime warning and audit line.
- **Credential broker / push interception (P1.2, P1.3 full).** In ship mode at `net-enforce` the child still legitimately holds a GitHub token and can force-push or open gists over *allowed* GitHub egress. Blocking that requires the bot to own pushes end-to-end (child commits locally; bot pushes and opens PRs host-side). That is an explicit **v2 roadmap item** (Open questions), not v1. Interim mitigations: per-project fine-grained PAT (K7), branch protection on project primaries (runbook), `requirePushApproval` (separate TODO).
- **Exfil to allowed hosts.** With net-enforce on, secrets-shaped data can still be pushed to GitHub. The allowlist is host:port; there is no TLS content inspection.
- **Shared per-project `.git` residual (P0.5 scope limit).** Within one project, the shared gitdir is rw to every thread's child. A compromised thread-A child can therefore (a) read every object in the shared store — including thread B's committed-but-unpushed content — and (b) rewrite local ref files under `refs/heads/` (including other managed branches and the primary), which `packed-refs` makes impossible to carve out per-path. Compensating controls: server-side branch protection on project primaries, the bot's push discipline (`IsManagedBranch`), and the fact that no local ref tamper reaches the remote without the bot's own push. Cross-project isolation is unaffected.
- **Same-project transcript reads.** With per-project agent homes, a thread can *read* sibling threads' transcripts. K7 mounts sibling `sessions/<sid>` and the CLI credential file **read-only** to stop *tampering*; reads remain possible (Open-Q3).
- **Reads inside the worktree/repo.** A `.env` committed into the project repo is readable by design.
- **Hook / repo-config changes landed as commits.** The exec-config read-only split (K2) stops the child *writing* hooks/gitlink/config directly; a malicious commit a human later merges can still add repo-config mischief — human review remains required.
- **Landlock-only hosts do not get egress enforcement at all.** The Landlock fallback path is capped at `fs` (K3/K9): it cannot deliver P0.4 (TCP is gated by port, not host; UDP/DNS entirely uncovered). Net modes require the bwrap+netns backend.
- **Admin-UI hardening (P1.4)** and **dual-control (S9)** — separate TODO items, orthogonal.
- **cgroup-grade resource caps.** Linux gets `setrlimit` in the bridge helper; macOS keeps wall-clock timeout + `maxTurns` only.
- **Windows / `sandbox_other.go` platforms** — sandbox unsupported; behaviour is precisely defined in K9 (basic-shim env/tmp isolation only, no rlimits, `Probe` refuses fs and above).

---

## Key decisions

### K1 — One wrap point: `internal/sandbox`, plugged into `grokrun.Run`

New package `internal/sandbox`. `grokrun.Options` gains three fields; `run.go:249` becomes the only place a sandbox is applied, covering **both** exec sites — task runs (`executeTask`, `bot.go:1581`) and `SummarizeTitle` (`run.go:801`), which today also leaks unfiltered `os.Environ()`; that gap closes here too (K7).

```go
// internal/sandbox/sandbox.go
type Mode int // ModeOff, ModeBasic, ModeFS, ModeNetObserve, ModeNetEnforce

type Spec struct {
    Mode         Mode
    WorktreeDir  string     // rw
    RepoGitDir   string     // main checkout .git — rw minus the exec-config surface (K2)
    WorktreeGit  string     // the .git gitlink FILE + per-worktree gitdir metadata — ro (K2)
    AgentHome    string     // rw own sessions/<sid> only; sibling sids + cred file ro (K7)
    ScratchDir   string     // rw: profile, tmp, gh config, per-run gitconfig, prompt file
    ReadPaths    []string   // attachments dirs, operator extraReadPaths
    WritePaths   []string   // operator extraWritePaths
    Proxy        *ProxySpec // nil unless Mode >= ModeNetObserve (never on Landlock backend)
}

// Probe reports whether mode is enforceable on this host ("", true) or why not.
// On the Landlock backend it caps at ModeFS and returns a reason for any net mode.
func Probe(m Mode) (reason string, ok bool)
func (s *Spec) Command(bin string, args []string) (wbin string, wargs []string, err error)
func (s *Spec) Env(base []string, includeGHToken bool) []string
func (s *Spec) Cleanup()
```

```go
// internal/grokrun/run.go — Options additions
Sandbox    *sandbox.Spec // nil → exec directly (current behaviour)
ScratchDir string        // when set, writePromptFile creates the prompt file here, not os.TempDir()
GrokHome   string        // when set: exported to the child AND used by the updates.jsonl/signals.json tap (run.go:750-759)
```

At `run.go:249`: `if opt.Sandbox != nil { bin, args, err = opt.Sandbox.Command(bin, args); env = opt.Sandbox.Env(env, …) }`. `setProcessGroup` (`process_unix.go:11-16`) still applies to the wrapper, so the wrapper is the pgid leader and `KillProcessGroup` keeps working (K8). Relocating the prompt file into scratch lets the profile pin one directory instead of allowing all of world `$TMPDIR`.

### K2 — Filesystem policy: deny-by-default with enumerated carve-outs + a real git boundary

Per-run generated profile — no static profile files; every path is absolute and thread-specific:

| Path | Access | Why |
|---|---|---|
| system toolchain roots (`/usr /bin /sbin /System /Library /opt/homebrew /etc /private/var/db` darwin; `/usr /bin /lib* /etc /opt` linux) | ro + exec | toolchain (`git`, `gh`, node, python) |
| worktree `data/worktrees/<proj>/<threadId>` | rw **+ exec** | the unit of work; `go test` compiles into TMPDIR/worktree, node runs `node_modules/.bin/*`, repos run `./scripts` |
| worktree `.git` **gitlink file** + per-worktree gitdir `main/.git/worktrees/<threadId>/{commondir,gitdir,config.worktree}` | **ro** | child never needs to write these; git writes `HEAD`/`index`/`logs`/`refs` which stay rw. Blocks gitlink/commondir redirection (S5) |
| shared main checkout `.git` | rw **except** `hooks/`, `config`, `worktrees/<otherThread>/` → ro | linked-worktree git ops write `objects/`, `refs/`, `logs/` |
| agent home `data/agenthome/<project>` | rw **only** the active thread's `sessions/<encoded-cwd>/<sid>/`; sibling `sessions/<otherSid>/` and the CLI credential file → **ro** | grok CLI creds + own updates.jsonl tap; stops cross-thread transcript tamper (K7) |
| scratch `data/sandbox/<threadId>/` (`tmp/`, `gh/`, `gitconfig`, `profile.sb`, prompt file) | rw **+ exec** | replaces world `$TMPDIR`; test binaries exec from here |
| `data/attachments/<msgID>/` (when present) | ro | prompt embeds absolute paths (`attachments.go:127`) |
| everything else — `$HOME`, `config.json`, other `projects.*.path`, other worktrees, `data/{sessions.json,history,audit,runs}` | **deny** | S1/S2/S5, P0.1/P0.3/P0.5 |

**The git exec-config boundary is load-bearing and now defended on both sides.** The child writes the shared `.git`. Two vectors let an escaped write turn the *bot's own unsandboxed* git into RCE outside the cage: (1) writing `.git/hooks/*` or `core.hooksPath`; (2) rewriting the worktree gitlink file or the per-worktree `commondir`/`gitdir` to point git at an attacker-controlled dir containing a malicious `config`. Every bot-side git op runs `git -C <worktree>` (`completion.go:141`, `ghpr/ghpr.go:36-37`, `gitworktree/worktree.go:757/771`) and would follow either redirection.

- **Profile side:** the gitlink file and per-worktree metadata (`commondir`/`gitdir`/`config.worktree`) are mounted **read-only** in *every* backend profile, and `hooks/`/`config` in the shared gitdir stay ro (Seatbelt: later `deny` overrides the `allow (subpath …)`; bwrap: `--bind <git> <git>` then `--ro-bind` the carve-outs).
- **Bot side (defence in depth):** every bot-run git invocation in `internal/gitworktree` (runners `worktree.go:756-775`), `internal/ghpr`, and `internal/bot/completion.go` runs with a **bot-computed pinned `--git-dir`**, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM` pointed at trusted files, `--no-ext-diff` on **every** `diff`/`show` (notably `ghpr.WorktreeDiffWith`/`WorktreeDiffFileWith`, `diff.go:117` / `diffindex.go` ~298, which an operator triggers merely by opening the session-diff or commit-review page in the `:8787` UI), and `-c` neutralization of the **full exec-config set**, not just hooks: `core.hooksPath=/dev/null core.fsmonitor=false core.pager=cat core.sshCommand=false diff.external= sequence.editor=false credential.helper=` plus disabling `filter.*.process/clean/smudge`. An enforcement/golden test asserts that a redirected gitlink + planted `config` with `diff.external=touch pwned` causes **no** external command to run in any bot git exec.

**Escaping of interpolated paths.** The `.sb` generator interpolates absolute paths into TinyScheme string literals. Worktree/agent-home segments are already sanitized (`sanitizePathSegment`, `[A-Za-z0-9._-]`), but operator `extraReadPaths`/`extraWritePaths` and the main-checkout/data-dir paths are not. Every interpolated path is now backslash-escaped for `"`/`\` and **rejected** if it contains newlines, control chars, or unbalanced parens — regardless of source. A golden test feeds a hostile path (embedded quote/paren/newline) and asserts the generated profile neither parses extra rules nor loads. (bwrap/Landlock take argv/API calls and are unaffected, but the same validation runs on all path inputs.)

**Structural fix.** New global config `worktreesRoot` (string) relocates worktrees outside the bot repo, killing the `../../../../config.json` adjacency independently of profile correctness. **It defaults to a path *outside* the bot repo tree** (resolved under the config dir's parent, e.g. `<configDir>/../grokwork-worktrees`, `""` meaning that computed default); the bot **refuses to start** if worktrees resolve inside the repo tree while any resolved sandbox mode is `< fs`. `gitworktree` does not import `internal/config`; relocation is caller-side plumbing: add `cfg.WorktreesRoot()` and thread it through every `WorktreePath`/`ListOnDisk`/`ResolveSessionWorktreePath` call site (`bot.go` ~845/856/865, idle cleanup, web). `WorktreePath(dataDir, project, unitID)` (`worktree.go:149`) stays a pure function.

**worktreesRoot migration.** Existing threads resolve via their **stored** sessionstore cwd (never re-derived), so live git-worktree gitlinks keep working; only newly created worktrees use the new root. `ResolveSessionWorktreePath`'s stale-path healing (`worktree.go:161-174`) already repairs old absolute cwds. All three cleanup triggers operate on stored paths, so a mixed-root deployment is swept correctly. PR2 acceptance covers a mixed-root deployment.

### K3 — Backends behind one interface

`internal/sandbox/{seatbelt_darwin.go, bwrap_linux.go, landlock_linux.go, sandbox_other.go}` — mirrors the `process_unix.go`/`process_other.go` split.

**darwin — Seatbelt.** `Command` writes `profile.sb` to scratch and returns `/usr/bin/sandbox-exec -f <profile> <bin> <args…>`. Skeleton (paths interpolated + escaped/validated):

```scheme
(version 1)
(deny default)
(allow process-fork)
(allow process-exec (subpath "/usr") (subpath "/bin") (subpath "/opt/homebrew")
                    (subpath "WORKTREE") (subpath "SCRATCH") (subpath "AGENT_HOME") ...) ;; exec from writable paths — deliberate no-W^X tradeoff
(allow sysctl-read) (allow mach-lookup ...) (allow signal (target same-sandbox))
(allow file-read* (subpath "/usr") (subpath "/System") (subpath "/etc") (subpath "/private/var/db") ...)
(allow file-read* file-write* (subpath "WORKTREE") (subpath "REPO_GIT")
                              (subpath "AGENT_HOME") (subpath "SCRATCH"))
(deny file-write* (subpath "REPO_GIT/hooks") (literal "REPO_GIT/config")
                  (literal "WORKTREE_GITLINK") (subpath "WORKTREE_GITDIR_META")
                  (subpath "AGENT_HOME_SIBLING_SESSIONS") (literal "AGENT_HOME_CRED"))
;; net modes (Seatbelt/bwrap only): only the bot's proxy port; no DNS (proxy resolves via CONNECT)
(deny network*)
(allow network-outbound (remote ip "localhost:PROXY_PORT"))
```

`process-exec` must include scratch, the worktree, and the agent home, because ship runs routinely exec binaries from writable paths (`go test` test binaries in TMPDIR/scratch, `node_modules/.bin/*`, `./scripts`). This is a deliberate abandonment of W^X inside the cage; a golden test asserts backend parity so bwrap (exec-by-default binds) and Seatbelt do not silently diverge.

**linux — bubblewrap (primary).** `bwrap --die-with-parent --unshare-pid --unshare-ipc --unshare-uts [--unshare-net] --proc /proc --dev /dev --tmpfs /tmp <ro-binds> <binds> -- <grokwork> sandbox-bridge …`. No `--new-session`: stdout is a pipe, TIOCSTI is moot, keeping the pgid preserves kill semantics. `--unshare-pid` means killing bwrap reaps the whole namespace.

**linux fallback — Landlock (kernel ≥ 5.13)** when bwrap is missing or user namespaces are disabled. The bridge applies a Landlock ruleset pre-exec (`golang.org/x/sys/unix`): fs rules mirror K2. **This backend is capped at `fs`.** Landlock ABI4 network rules gate TCP by *port*, not host, and do not cover UDP — so it can neither deny SSRF to arbitrary LAN/metadata hosts on the proxy port nor stop DNS/UDP exfil. Therefore `Probe` returns not-ok for any mode `> fs` on a Landlock-only host, `Proxy` is never attached, and the K9 table and threat-property mapping reflect that egress enforcement (P0.4) requires the netns (bwrap) backend. (An optional mandatory seccomp `socket(AF_INET, SOCK_DGRAM)` block — PR5 — would be a prerequisite before net-enforce could ever be claimed here; until then it simply is not offered.)

**`grokwork sandbox-bridge`** — a subcommand of the bot's binary (ro-bound into the sandbox), dispatched in `main.go` before config load. Used on Linux in both backends: it (a) on the bwrap backend, listens on `127.0.0.1:3128` *inside* the netns and forwards to the host proxy over a bind-mounted unix socket (srt's bridge pattern), (b) applies Landlock and `setrlimit` (`RLIMIT_NPROC`, `RLIMIT_AS`, `RLIMIT_NOFILE`), (c) exec()s grok in place, preserving pid identity.

**`sandbox_other.go` (Windows / unsupported OS) — defined precisely.** `Probe` returns ok **only** for `basic` and `off`; it returns not-ok (with reason) for `fs` and above. The `basic` implementation on other OS = env allowlist + `GH_CONFIG_DIR` + scratch `TMPDIR` + per-run gitconfig, with **no bridge and no rlimits** (`Spec.Command` is identity — it returns `bin, args` unchanged). Non-goals and the K9 table state exactly this.

### K4 — Network: proxy-based deny-by-default, phased through observe

`internal/sandbox/netproxy`: a stdlib HTTP CONNECT proxy owned by the bot process — one listener, no extra daemon. Per-run credential `HTTPS_PROXY=http://run-<id>:<token>@127.0.0.1:<port>` maps every connection to a thread/user for audit. **Net modes are available only on the netns (bwrap/Seatbelt) backends** (K3).

- **`net-observe`** — direct egress denied by the OS profile; the proxy allows everything and writes `{thread, user, host, port}` lines via `internal/audit`. This is how the operator *discovers* the real allowlist empirically.
- **`net-enforce`** — the proxy refuses CONNECT to hosts not matching `networkAllow` globs. DNS is denied at the OS layer (mDNSResponder socket not allowed on darwin; empty netns on linux) — the proxy resolves names itself, killing DNS-tunnel exfil.

**Hardened vetting, beyond host globs:** deny CONNECT to raw IP literals; after resolving an allowed hostname, deny loopback, RFC1918, link-local, and metadata ranges, and dial the vetted IP directly (no re-resolve) for DNS-rebinding safety.

Consequences we accept: SSH remotes do not traverse an HTTP proxy → **net modes require HTTPS git remotes**. (Note: the SSH-remote *warning* itself is not net-specific — see K7/K9, allowlist env drops `SSH_AUTH_SOCK` and remaps `HOME` at `fs` and above.) Tools that ignore proxy env simply fail to connect; that is the feature.

### K5 — Config schema

```jsonc
{
  "worktreesRoot": "",                 // "" → computed path OUTSIDE the bot repo (K2)
  "sandbox": {                         // *SandboxConfig — nil → all defaults
    "mode": "fs",                      // "" inherit-default | "off" | "basic" | "fs" | "net-observe" | "net-enforce"
    "fallback": "block",               // "" → "block" | "degrade" (K9 ladder, split at fs) | "warn-off"
    "allowUnconfinedDegrade": false,   // K9: permit degrade below fs (loses read/write confinement)
    "networkAllow": ["api.x.ai:443", "github.com:443", "api.github.com:443", "*.githubusercontent.com:443"],
    "extraReadPaths": [], "extraWritePaths": []
  },
  "projects": {
    "myproj": {
      "sandbox": { "mode": "net-enforce" },  // nil field → inherit global
      "webSearch": false                     // K13
    }
  }
}
```

`config.Config.Sandbox *SandboxConfig`; `ProjectConfig.Sandbox *SandboxConfig`. Accessor follows the RepoFetchInterval pattern (`project.go:199-213`): `cfg.SandboxFor(project) ResolvedSandbox` resolves project → global → built-in defaults. Built-in default mode is `basic` from PR1 (non-breaking, K9 basic definition), flipping to `fs` in PR4 via **platform-aware resolution** (K9). All four persistence sites updated or the field silently drops on save: `Config` struct, `saveLocked()` anonymous struct (`config.go:475+`), `ProjectsMap.MarshalJSON` `outObj` (`project.go:113-158`), `cloneProjectsMap` (`project.go:168-194`) — pinned by extending `save_wave1_fields_test.go`. `networkAllow` uses the RiskyPathGlobs three-way slice semantics (`config.go:280-302`): nil = defaults, `[]` = explicitly none.

The `networkAllow` value here is **illustrative**; the shipped built-in default is seeded from PR3 observe data. The illustrative list is kept aligned with the child `gh` needs of K4/PR4 acceptance — `api.github.com:443` (every `gh` API call, `gh pr create`, `gh issue create`) and `*.githubusercontent.com:443` are included; omitting them would fail PR4's own acceptance.

**Kill switch:** env `GROK_WORK_SANDBOX_MODE=off` (read via `config.EnvWork`, wins over file config, auto-denylisted from children by the `GROK_WORK_` prefix in `env.go:12`), logged loudly at startup; plus the audited web toggle (K11).

### K6 — Per-run resolution + flow coverage + in-flight config changes

Sandbox mode is **not** thread-sticky (unlike ShipMode): the worktree mechanism is identical in every mode and nothing persists between runs except the worktree. `executeTask` re-resolves `cfg.SandboxFor(project)` + `sandbox.Probe` **every run**, so a tightening applies to the very next run of every in-flight thread. Mixed-mode threads are made *legible* (effective mode recorded per run in `internal/history`, on the completion card, in the audit log — K12) rather than prevented.

`RunPolicy` (`run_policy.go:30-49`) gains `SandboxMode string` and `AllowWebSearch bool`; `BuildRunPolicy` stays pure. `Spec` construction happens beside it in `executeTask` from `resolveRunCwd`'s worktree, the main-checkout gitdir, attachment paths, and scratch. **Investigate/explain runs get the same `fs` floor as ship runs** — their tools allowlist limits *tools*, not *paths*.

**Worktree prerequisite (rule).** Modes `≥ fs` require `WorktreeIsolationEnabled()` (`config.go:74`) **and** `IsRepo(proj.Cwd)`. When `worktreeIsolation` is off or the project path is not a git repo, `resolveRunCwd` (`bot.go:828-836`) returns the shared **main checkout** with no per-thread worktree — there is no unit to confine without granting rw on the shared checkout (destroying P0.5). In that case fs+ is treated as **unenforceable** and the `fallback` ladder runs (block/degrade/warn-off) with a message naming `worktreeIsolation`. `basic` and `off` are unaffected. This interaction is called out in the config section because `worktreeIsolation` is an existing user-settable field.

**Flow coverage.** Every grok exec path goes through the same choke point; each gets an explicit floor, token rule, network need, and failure sink:

| Flow | Entry | FS floor | GH token | Network | Failure sink |
|---|---|---|---|---|---|
| User task | `executeTask` (`bot.go:1581`) | fs | ship only, gated at net-enforce (K7) | as configured | thread reply |
| Queued follow-up | `drainTaskQueue` | fs | as ship | as configured | thread reply; re-resolved at claim (below) |
| `SummarizeTitle` | `run.go:801` | fs, **ro, cwd = scratch** (K7) | none | none | silent (falls back to heuristic title) |
| `/fix-ci` auto-fix | `ci_triage.go:311,417-428` via `queueSystemTask` | fs | as ship | as configured | thread reply; if system-initiated with no thread, **audit line + ops thread** |
| Commit review (Discord + web-native) | `commit_review_start.go:216,252` | fs | **IncludeGHToken granted** (needs `gh issue create`); gated at net-enforce like ship | as configured | Discord thread reply, else **web unit timeline**; block-refusal → web timeline + audit |

**Config changes and in-flight runs.** (a) The **OS profile is frozen per run** — filesystem/mode changes apply to the *next* run only. (b) The **proxy is live shared state**: `networkAllow` edits and the audited web `off` toggle are evaluated **per CONNECT**, so a *tightening* cuts off in-flight egress immediately and a *loosening* takes effect immediately too — stated deliberately. (c) Per-run proxy credentials remain valid until `finishRun`; a sandbox-off toggle does not revoke an in-flight run's token (its OS cage is already frozen). (d) Queued follow-ups (FIFO max 5) **re-resolve at claim time**; if a tightening makes `fallback:"block"` trip, each is refused individually with the K9 message (not flushed silently). (e) The incident runbook says: to apply an fs-level tightening *now*, `/cancel` in-flight runs — a 30-minute in-flight run keeps its frozen cage until it ends.

### K7 — Credentials and the child environment

- **Agent home is an `fs`-tier feature, not `basic`.** At `basic` the child keeps the operator's `HOME`/`GROK_HOME`, so `--resume` (`run.go:219-226`) and existing transcripts under `~/.grok/sessions/<cwd>/<sid>` (`run.go:751-758`) keep working, and no per-project credential provisioning is required — this is what makes `basic` non-breaking (K9, Migration). At `fs` and above, `Spec.AgentHome = data/agenthome/<project>`, provisioned once per project (runbook: `GROK_HOME=data/agenthome/<proj> grok login` with a **dedicated xAI API key**). This closes cross-project transcript reads and operator-credential exposure. `Options.GrokHome` keeps the updates.jsonl/signals.json tap pointed at the same dir; the tap's absolute-cwd derivation is unaffected (paths are identical inside and outside the sandbox — no mount remapping).
- **Same-project cross-thread tamper closed (K2 mount).** Within the per-project home, only the active thread's `sessions/<sid>/` is rw; sibling `sessions/<otherSid>/` and the CLI credential file are **read-only**, so a compromised thread cannot corrupt another thread's live activity/context or overwrite the shared credential. Transcript *reads* remain possible (documented residual, Open-Q3). The cross-tenant enforcement test is extended to cover same-project thread A → thread B **writes and reads**.
- **Allowlist env construction at `fs` and above.** `Spec.Env` builds the child env by explicit allowlist: `PATH`, `HOME=<AgentHome>`, `GROK_HOME=<AgentHome>`, `TMPDIR=<scratch>/tmp`, `TERM`, `LANG`/`LC_*`, proxy vars, `GIT_CONFIG_GLOBAL`, `GH_CONFIG_DIR`, plus `GH_TOKEN` **only under the gating rule below**. `FilterChildEnv` (`env.go:20`) remains for `off`/`basic` runs.
- **Token gating on egress enforcement (S2).** `GH_TOKEN` is injected **only when the resolved mode is `net-enforce`** (where egress is allowlisted, so the token cannot be curled out). At `fs`/`net-observe`, a *ship* run that needs to push still receives the token — but with a **mandatory loud run-time warning + audit line** ("GH token issued to a run with unconstrained egress — S2 open; raise this project to net-enforce or adopt the v2 broker"), and S2 is documented as open in that posture. At `basic`/`off`, token delivery is unchanged from today (also S2-open, documented). The clean fix is the v2 host-side push broker (Open questions), which removes ambient tokens entirely.
- `~/.config/gh` **denied** at `fs`+; child gets `GH_CONFIG_DIR=<scratch>/gh`. Non-ship runs: no token, no disk auth → `gh` inert (P0.2 for GitHub). Commit review is the exception (flow table): it *is* granted the token.
- `~/.ssh` **denied** at `fs`+, always. Combined with K4 this forces HTTPS pushes via `GIT_CONFIG_GLOBAL=<scratch>/gitconfig` (`credential.helper=!gh auth git-credential`, `user.name`/`user.email`). Hardening env: `GIT_SSH_COMMAND=/usr/bin/false`, `GIT_TERMINAL_PROMPT=0`. **Because allowlist env drops `SSH_AUTH_SOCK` and remaps `HOME`, SSH pushes break at any mode `≥ fs`, not only net modes** — the SSH-remote warning is a general allowlist-env warning (K9 preflight), not a net-mode warning.
- **Per-project fine-grained PAT, PR5:** replace the ambient token as the `IncludeGHToken` value with a per-project fine-grained PAT (Contents/PRs/Issues RW, expiring); runbook adds `gh auth logout` on the host and branch protection.
- **`SummarizeTitle` fixed:** gets an explicit filtered env plus an **fs-mode read-only `Spec` with `cwd = a per-run scratch dir` — never the main checkout** (naming a thread needs no repo access). This closes the unfiltered-env exec site (`run.go:827-840`) and removes the config.json-adjacency risk even if a future change enables a tool on the title path.

### K8 — Kill, orphan, and identity semantics

The journaled `GrokPID` (`bot.go:1606-1614`) becomes the **wrapper** pid (`sandbox-exec`/`bwrap`/bridge), the pgid leader — `KillProcessGroup`'s SIGTERM → 2s → SIGKILL to `-pid` (`process_unix.go:19-35`) is unchanged, and on Linux now reaps the whole pid namespace. On darwin and in `basic` the wrapper exec()s in place, so the pid *is* grok's pid. `runjournal.LooksLikeGrokCLI` (`lock.go:100-117`) additionally matches `ps` output containing `sandbox-exec` + `data/sandbox/`, or `bwrap`/`sandbox-bridge` markers, so `RecoverActiveRuns` (`recover.go:93-129`) still verifies identity before killing.

### K9 — Degradation ladder, platform-aware defaults, unsupported platforms

Unenforceable modes degrade down a ladder governed by `fallback`, **split at the fs boundary**:

```
net-enforce → net-observe → fs      (free degrade — only egress enforcement is lost)
        fs → basic → off            (crosses the confinement boundary — gated)
```

`fallback:"degrade"` degrades **freely among net-enforce/net-observe/fs**, but must **not** cross below `fs` (losing read/write confinement) unless `allowUnconfinedDegrade:true` is explicitly set; otherwise it **fails closed like `block`** at the fs floor. Losing filesystem confinement raises a **distinct, louder** signal than merely losing egress enforcement (different Discord prefix, distinct audit event) — because the fs→basic step drops config.json/`~/.ssh`/cross-tenant protection precisely during an incident.

The **basic** tier requires no OS sandbox facility and (revised, K7) does **not** switch `GROK_HOME` or isolate `GH_CONFIG_DIR`: it is scratch `TMPDIR` + per-run scratch + `setrlimit` (linux) + the existing env filter, retaining operator `HOME`/`~/.grok`/`~/.ssh`/gh disk auth. It ships default-on in PR1 because it therefore cannot break auth, `--resume`, or a toolchain.

| Host | fs | net-observe / net-enforce | Notes |
|---|---|---|---|
| macOS | Seatbelt | Seatbelt + loopback proxy | `Probe`: `sandbox-exec` exists + self-test at startup |
| Linux, bwrap + userns | bubblewrap | bwrap `--unshare-net` + unix-socket bridge | preferred; **only path that delivers P0.4** |
| Linux, no bwrap/userns | Landlock (≥ 5.13) | **not offered** | Probe caps at fs; net modes refused (TCP-by-port, UDP uncovered) |
| Linux < 5.13 / other OS | **not offered** (basic/off only) | not offered | `sandbox_other.go` is identity `Command` |

**Platform-aware default resolution.** When mode comes from the **built-in default** (not explicit operator config), it resolves to the **highest tier `Probe` reports enforceable** (fs where possible, `basic` otherwise) — `fallback:"block"` is **never** applied to a default nobody set. So the PR4 `fs` default flip keeps non-fs-capable hosts (Windows, Linux <5.13 without bwrap) running at `basic` with a capability line on `/config`, rather than refusing every run. An *explicit* operator `mode:"fs"` still honours `fallback`.

`fallback:"block"` refuses with an actionable Discord message; `"degrade"` per the split ladder; `"warn-off"` runs unsandboxed with a loud prefix. `Probe` results are cached at startup and surfaced on `/config`.

**Startup / preflight probes.** (a) On PR1, if a project's origin remote is **SSH** or the bot env has **no `GH_TOKEN`** and relies on gh disk auth, the resolved `fs`+ mode would break that project's pushes — this is surfaced as a loud `/config` warning and the project stays at `basic` (which preserves disk auth) until the operator sets `GH_TOKEN` / switches remotes; it is pinned by a PR1 acceptance test. (b) Before trusting net modes, verify the installed grok CLI honours `HTTPS_PROXY` (canary) and that raw egress is truly blocked from inside the sandbox (probe `curl` to a non-proxy address must fail), failing closed with the reason on `/config`.

### K10 — Lifecycle and teardown

Scratch `data/sandbox/<threadId>/` is created in `executeTask` per run — `tmp/` wiped between runs, profile regenerated every run (per-run resolution, K6). Removal joins the three worktree-teardown triggers: `removeWorktreeCandidate` (`idle_cleanup.go:206`), PR-terminal cleanup (`CleanupIfPRDoneWith`, `worktree.go:227`), and manual web prune (`PruneWorktree`, `idle_cleanup.go:146`) all also `os.RemoveAll` the sibling sandbox dir. No persistent per-worktree sandbox state exists. Agent homes (`data/agenthome/<project>`) persist across runs and are excluded from sweeps.

### K11 — Web UI

- `config.tmpl`: new `<div class="section card" id="sandbox">` posting `POST /config/settings` with `section "sandbox"` (`updateSettings` switch, `web.go:1028+`): mode select (`data-autosubmit`), fallback select, `allowUnconfinedDegrade` toggle, `networkAllow` textarea, `worktreesRoot` input — plus a read-only "host capability" line from cached `Probe`/preflight results.
- `project_config.tmpl`: override section modelled on "Ship workflow" (`project_config.tmpl:26-43`), posting `POST /config/projects/sandbox` → `setProjectSandbox` → `projectConfigRedirect` (`web.go:799`), audited via `internal/audit`.
- `config.Snapshot` (`config.go:844+`) gains `SandboxMode`, `SandboxUseDefault`, `SandboxHostOK`, `SandboxHostReason`; `ProjectItem` gains effective `SandboxMode` and `WebSearch`. `web_test.go` markup pins updated (byte-for-byte convention).

### K12 — Visibility: chips, egress/read-denial audit, and the tripwire

You cannot operate a rollout you cannot see.

- **Per-run record:** effective sandbox mode + backend + grok CLI version written to `internal/history` and the audit log.
- **Completion card** (`completion.go`) and `/status` chips: `sandbox: fs` / `sandbox: net-enforce`; when N > 0, `network: N blocked connections (evil.example, …)`; and — symmetric — `sandbox: N blocked file accesses (~/.ssh, …)`.
- **Filesystem-denial audit (not just network).** Egress attempts already audit; fs denials must too, or S1 stays a silent EPERM (contradicting P2.3). The bot scans the **out-of-scope-path signal derived from its own in-memory live-tap buffer captured during the run** (bot-side, trusted) — tool-call **inputs** (not just result paths) matching watchlist substrings (`~/.ssh`, `~/.config/gh`, `config.json`, …) or denial-shaped failures — and emits `internal/audit` lines plus the completion-card chip above. On darwin it may additionally collect per-run Seatbelt violation reports from the unified log. A test row asserts a seeded denied read produces an audit line.
- **Post-run watchlist tripwire (load-bearing = writes only).** lstat/sha256 snapshot of fixed sensitive paths (`config.json`, `data/sessions.json`, main checkout `.git/hooks` + `.git/config`, the worktree gitlink, `~/.ssh`, `~/.config/gh`) **before/after** each run. This detects **modifications, not reads**. Any hit → audit line + ops warning in the thread.
- **The on-disk `updates.jsonl` is untrusted and is NOT re-read for tripwire purposes.** It lives in the rw agent home (`activity.go:78`); a compromised child can forge or omit tool-call entries. The out-of-scope-path signal above comes from the bot's in-memory live tap, not from re-reading that file. The disk file's out-of-scope scan, if surfaced at all, is documented as **best-effort only**.

### K13 — Web search off by default for task runs

Real task runs never set `DisableWebSearch` today. This design flips the default: per-project **`webSearch bool`** on `ProjectConfig` (a plain bool — nil and false would be indistinguishable, so a pointer earns nothing here; the sandbox `mode` inherit pattern is used only where a global default differs from the zero value). Default `false`, carried as `RunPolicy.AllowWebSearch`, wired to `Options.DisableWebSearch`. Shrinks the S3/S6 injection surface. Explain mode and `SummarizeTitle` already disable it; no change there.

---

## Threat-property mapping

| Mechanism | Enforces | Explicitly does NOT stop |
|---|---|---|
| K2 fs profile (Seatbelt/bwrap/Landlock) | **P0.1** read (S1), **P0.3** write (S5), **P0.5** cross-tenant *worktree files*, **P2.2** staging (S7) | reads inside the worktree/repo; committed `.env`; **shared per-project `.git` object reads + local ref tamper** (residual, Non-goals); same-project transcript reads |
| K2 exec-config ro split (gitlink/commondir/config/hooks) + bot-side pinned `--git-dir`/`GIT_CONFIG_NOSYSTEM`/`--no-ext-diff`/full `-c` neutralization | out-of-sandbox RCE via the bot's git execs (incl. `diff.external` on UI diff view) | malicious hook/config changes landed as reviewed commits |
| K7 credential design | **P0.2** at `fs`+ (`~/.ssh`, gh disk auth, `~/.aws`, operator `~/.grok`); **P1.3 partial** (env-token-only, PAT in PR5); cross-thread transcript **tamper** closed | at `basic` no fs isolation; S2 token-exfil open below **net-enforce** (default posture); grok's dedicated xAI cred visible to grok |
| K4 net-enforce proxy + OS net deny + IP vetting (**bwrap backend only**) | **P0.4** on the netns backend: no LAN/`:8787`/metadata/attacker egress (S2 exfil leg, S6); DNS tunnels + rebinding closed | exfil to allowed hosts; TLS content inspection; **not available on Landlock — no P0.4 there** |
| bridge `setrlimit` (linux) | **P1.1 partial** (S8) | macOS caps; disk-fill in allowed rw paths |
| K12 in-memory read/write-denial audit + tripwire (writes) | **P2.3** auditability of egress **and** sensitive-path access; write regression detection | read prevention (detection only); tripwire does not detect reads |
| K13 web-search default off | shrinks S3/S6 surface | injection via attachments/replies/repo content |
| whole design | — | prompt injection itself (S3): injected instructions still run, inside the cage |

---

## Failure and UX

Literal text per class:

- **Run refused (`fallback:"block"`, or `degrade` blocked at the fs floor):**
  `Sandbox is required for this project (mode: net-enforce) but not enforceable on this host: bubblewrap not found. Run refused. An operator can install bubblewrap, lower sandbox.mode, or change sandbox.fallback at /config.`
- **Missing worktree isolation (K6 rule):**
  `Sandbox mode fs requires worktreeIsolation and a git-repo project path; this project has worktreeIsolation disabled. Run refused. Enable worktreeIsolation at /config or lower sandbox.mode.`
- **Degraded within the confinement band (`degrade`, net→fs):** first reply prefixed
  `⚠ Sandbox egress enforcement unavailable — degraded to "fs" (host: Landlock backend). Filesystem confinement intact. Configured mode: net-enforce.`
- **Degraded ACROSS the fs floor (only with `allowUnconfinedDegrade:true`):** distinct, louder prefix
  `‼ Sandbox LOST FILESYSTEM CONFINEMENT — degraded to "basic" (host: user namespaces disabled). config.json / ~/.ssh / cross-tenant no longer confined. Configured mode: fs.`
- **warn-off:** `⚠ Running WITHOUT the OS sandbox (host: sandbox-exec self-test failed). Configured mode: fs.`
- **Token issued with unconstrained egress (K7):** `⚠ GitHub token issued to a run below net-enforce — egress is unconstrained (S2 open). Raise this project to net-enforce to close it.`
- **Mid-run egress denial:** `CONNECT denied by grokwork sandbox policy: evil.example:443 not in networkAllow`
- **System/web flows:** a `/fix-ci` refusal with no Discord thread lands as an audit line + ops-thread message; a web-native commit-review refusal lands on the web unit timeline + audit (K6 flow table).
- **Operator kill switch:** audited web toggle; `GROK_WORK_SANDBOX_MODE=off` env (visible in startup logs). Per-run resolution means the next run reflects the change.

---

## Migration / upgrade path

Because PR1 ships default-on and later PRs flip the default and switch credential/home wiring, deployment ordering is explicit.

1. **PR1 (`basic` default-on) is non-breaking by construction.** `basic` does **not** switch `GROK_HOME`, does **not** isolate `GH_CONFIG_DIR`, and retains the operator's `HOME`/`~/.grok`/`~/.ssh`/gh disk auth (K7/K9). Therefore: (a) every in-flight thread's `--resume` context under `~/.grok/sessions/<cwd>/<sid>` keeps resolving — **no transcript orphaning at PR1**; (b) gh disk auth and SSH remotes keep working — **no auth breakage at PR1**. No pre-deploy provisioning is required. The only PR1 obligations are the startup preflight warnings (K9) for SSH remotes / missing `GH_TOKEN`, pinned by a PR1 acceptance test.
2. **Before enabling `fs` on a project** (opt-in in PR1, default in PR4): the operator must (a) provision `data/agenthome/<project>` (`GROK_HOME=… grok login` with a dedicated xAI key) — `Probe` **refuses** an `fs` run for an unprovisioned home with an actionable message, pinned by a PR1 acceptance test; (b) ensure the project's git remote is **HTTPS** (or accept that pushes break, per the SSH warning); (c) ensure a `GH_TOKEN` is available for ship runs.
3. **Transcript story at the `fs` switch.** Switching to the per-project agent home orphans `--resume` for pre-upgrade in-flight threads (their transcripts live under the operator `~/.grok`). The runbook offers a documented one-time copy of `~/.grok/sessions/<worktree-cwd>/*` into `data/agenthome/<project>/sessions/…`; where that is skipped, the next run of an affected thread posts a user-visible **"session restarted (sandbox agent home changed)"** notice rather than silently losing context.
4. **PR4 default flip (`basic` → `fs`).** Applies only to hosts where `Probe` reports `fs` enforceable (platform-aware resolution, K9); non-fs hosts stay at `basic` with a `/config` capability line. Operators who have not completed step 2 for a project see that project stay at `basic` (or refuse, if they set an explicit `fs`), never a broken run.

---

## Rollout

### PR1 — `internal/sandbox` core, non-breaking `basic` default-on, darwin `fs` opt-in

Package + `Probe` + profile generation with path escaping/validation (golden-tested); `Options.Sandbox/ScratchDir/GrokHome`; prompt-file relocation into scratch; `sandbox-bridge` subcommand (rlimits + exec-in-place); `basic` tier (scratch/tmp/rlimits only, no HOME/gh changes); agent-home provisioning + `Probe`-refuses-unprovisioned (fs only) + runbook; allowlist env (fs+); `SummarizeTitle` env + ro scratch-cwd spec fix; config schema + accessors + persistence tests; kill-switch env; `LooksLikeGrokCLI` extension; scratch teardown in all three triggers; SSH-remote / missing-token startup preflight. Default mode: **basic**; `fs` opt-in on darwin.

*Acceptance:* basic runs a normal fix→commit→PR task unchanged **including with gh disk auth and an SSH remote** (no breakage), and `--resume` on a pre-existing thread still resumes; `Probe` refuses an `fs` run whose agent home is unprovisioned with the actionable message; with `mode:"fs"` on darwin, `cat ~/.ssh/id_rsa`, `cat <botrepo>/config.json`, write outside worktree, write `.git/hooks/x`, and **rewrite the worktree gitlink** all fail with EPERM while `go test` (test-binary exec from scratch) and the same task complete; a redirected gitlink + planted `diff.external` config runs **no** external command in any bot git exec; `/cancel` and restart-recovery behave as today.

### PR2 — Linux backends + bot-side git hardening + worktreesRoot

bubblewrap backend; Landlock fallback **capped at fs**; netns unix-socket bridge (dormant until PR3); `setrlimit`; the full bot-side git neutralization (`--git-dir` pin, `GIT_CONFIG_NOSYSTEM=1`, `--no-ext-diff`, exec-config `-c` set) on all bot git runners incl. `ghpr` diff paths; `worktreesRoot` (default outside repo, refuse-on-start if inside repo while mode < fs) + call-site plumbing; K6 `worktreeIsolation` prerequisite rule.

*Acceptance:* fs probe matrix passes under bwrap and under bridge+Landlock; **Landlock `Probe` refuses net modes**; killing the wrapper reaps the namespace; bot git ignores a planted hook **and** a redirected gitlink; mixed-root deployment round-trips create/remove and both roots sweep; fs run with `worktreeIsolation` disabled hits the ladder with the naming message.

### PR3 — netproxy + net-observe + visibility

CONNECT proxy (bwrap backend only) with per-run credential; observe audit lines; preflight probes (proxy honouring, raw-egress-blocked, SSH warning); web UI (K11); completion/`/status` chips incl. **fs-denial chip**; in-memory read/write-denial audit; write-tripwire; live-per-CONNECT config evaluation (K6); `webSearch` default-off; grok-CLI drift probe.

*Acceptance:* observe logs every egress attempt attributed to a thread; a seeded denied read produces both an audit line and a completion chip; a `networkAllow` tightening cuts in-flight egress at the next CONNECT; a week of dogfooding yields a candidate allowlist; no legitimate run breaks.

### PR4 — net-enforce + platform-aware `fs` default

Enforce mode with hardened vetting (IP literals, private-range post-resolve deny, no re-resolve); default `networkAllow` seeded from observe data (incl. `api.github.com`, `*.githubusercontent.com`); flip built-in default `basic` → `fs` **via platform-aware resolution** (non-fs hosts stay at basic).

*Acceptance:* success criteria 1–3 pass end-to-end from real Discord runs; `git push` + `gh pr create` succeed through the proxy on an HTTPS-remote project; **upgrade on a non-fs-capable host keeps running at basic with a capability line on `/config`** (no refusals from an unset default).

### PR5 — hardening and close-out

Per-project fine-grained PAT via the `GH_CONFIG_DIR`/env seam + `gh auth logout` runbook + branch-protection docs; **mandatory** seccomp UDP block as the prerequisite spike for any future Landlock net-enforce; credential-broker (v2) design spike; **P0.4 default-on decision point** (below); CLAUDE.md invariant blockquote:

> grok children run inside an OS sandbox; only the bot execs unsandboxed `git`/`gh`.

Close TODO.md:176.

### P0.4 default decision point (dated review, PR5+)

P0.4 is delivered **opt-in per project** in the initial rollout, because allowlist-breakage risk varies per project and the default posture cannot close S2 without it. A scheduled review (after `fs` has baked as the default) evaluates flipping the built-in default to `net-observe`, and — per project, once its allowlist is empirically stable from observe data — to `net-enforce`. Until an operator raises a project to `net-enforce`, the S2 exfil leg stays open for that project (documented, not silently claimed closed).

---

## Test plan (stdlib only)

| Area | Test |
|---|---|
| Profile generation | Golden: `Spec` → Seatbelt/bwrap/Landlock; **hostile path (quote/paren/newline) rejected or escaped, no extra rules**; backend exec-from-writable-path parity |
| Git boundary | Golden/enforcement: redirected gitlink + planted `config` (`diff.external`, `core.pager`, `credential.helper`, `filter.*.process`) causes **no** external exec in any bot git path; gitlink/commondir ro |
| Config | Round-trip save/load `sandbox` + `worktreesRoot` + `webSearch` (extend `save_wave1_fields_test.go`); tri-state nil/`[]`/values; `SandboxFor` precedence; refuse-on-start when worktrees inside repo & mode<fs |
| Env | Allowlist table (ship vs non-ship, **GH-token gating: injected only at net-enforce; commit review always**); `SummarizeTitle` filtered env + scratch cwd |
| Enforcement (darwin) | outside read/write EPERM; inside worktree ok; `<git>/hooks/x` and gitlink write fail while `<git>/objects/x` ok; `go test` exec from scratch ok |
| Enforcement (linux) | bwrap + bridge+Landlock matrix; `--unshare-net` bridge dial ok, direct dial fails; **Landlock Probe refuses net modes** |
| netproxy | allowlist match; denied host 403 format; IP-literal + post-resolve private-range denial; per-run token attribution; **live per-CONNECT tightening** |
| Cross-tenant | project A cannot read B's path/worktree/agent home; **same-project thread A → B write blocked, read documented** |
| Kill semantics | wrapper kill reaps within 2s; `LooksLikeGrokCLI` markers |
| Run policy | investigate gets fs floor; `AllowWebSearch` false wiring; `worktreeIsolation`-off ladder |
| Audit/visibility | seeded denied **read** → audit line + fs-denial chip; write-tripwire on canary write |
| CLI drift | build-tagged probe: grok honours `HTTPS_PROXY`; fails loudly on drift |
| Web | `web_test.go` pins for new sections; POST handlers + audit |
| Migration | PR1: basic run with gh disk auth + SSH remote unchanged, `--resume` resumes; fs run with unprovisioned agent home refused |
| Manual acceptance | scripted Discord: `cat ~/.ssh/id_rsa`→EPERM; `cat <botrepo>/config.json`→EPERM (absolute path is the primary probe; relative is `../../../../config.json`); `curl localhost:8787`→denied; `git push`→succeeds via proxy |

---

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Apple removes `sandbox-exec` | startup `Probe` self-test fails loudly; ladder governs; backend interface leaves a container swap-in |
| Seatbelt baseline too tight → tool breakage | basic non-breaking floor; profile seeded from srt baseline; `extraReadPaths`/`extraWritePaths`; fs dogfooded before default-on; exec-from-writable golden test |
| Proxy single point of failure | in-process; observe first; kill switch; distinctive refusal line |
| HTTPS-remote / allowlist-env surprises SSH projects | startup + `resolveProject` preflight warns; project stays at `basic` (disk auth intact) until remotes switch |
| Wrapper pid confuses recovery | exec-in-place on darwin/basic; `LooksLikeGrokCLI` markers; pid-ns reaping tested |
| Agent-home provisioning friction | one-time runbook; `Probe` refuses unprovisioned fs runs with an actionable message (not a confusing failure) |
| S2 open in default posture | documented everywhere; loud token-issuance warning + audit; net-enforce closes it; v2 broker removes ambient tokens |
| `degrade` silently crossing the fs floor | ladder split at fs; below-fs degrade gated behind `allowUnconfinedDegrade`; distinct louder signal |
| grok CLI update ignores proxy | drift probe + preflight; net modes fail closed |

---

## Open questions

1. **Credential broker (v2).** Child commits locally with no GitHub token; the bot pushes and opens PRs host-side. Closes the S2 default-posture residual *and* the force-push/gist residual, and pairs with `requirePushApproval`. Separate doc; PR5 spikes it.
2. **Agent-home credential materialization.** v1 has the operator `grok login` into each home. Should the bot materialize a shared credential file from one configured key? Depends on grok CLI credential-file stability — deliberately not depended on in v1.
3. **Per-project vs per-thread agent homes.** Per-project (v1) still lets same-project threads *read* each other's transcripts (writes/tamper are closed via ro sibling mounts, K7). Per-thread homes close reads too (same mechanism, more login provisioning); revisit after the broker.
4. **Landlock UDP / net on Landlock.** The mandatory seccomp `AF_INET/SOCK_DGRAM` block is a PR5 prerequisite for ever offering net-enforce on Landlock; until then Landlock is fs-capped. Ship the seccomp block, or leave Landlock permanently fs-only?
5. **grok CLI native permission modes.** If the CLI grows `--sandbox`/`[permission]` deny rules, layer them as cheap defence-in-depth once pinned by the drift probe — never as sole enforcement.

---

## Alternatives considered

Four proposals were judged against security, feasibility, and operability. This design is the winner (OS-native, no containers) with grafts absorbed.

**Container-based isolation (OrbStack/Docker, `git clone --shared`, internal no-NAT network).** Strongest cage when running: non-proxy traffic has no route, the writable shared-`.git` surface disappears, real cgroup caps, dedicated home from day one. Rejected as primary: a container runtime becomes a hard security dependency on the macOS-laptop deployment, binary failure posture, per-run startup latency, a second checkout mode to maintain, virtiofs timing risk for the 400 ms activity tap, image-pin treadmill. Best ideas absorbed: dedicated agent `GROK_HOME` (K7), allowlist env (K7), broker roadmap (Open Qs), proxy IP vetting (K4), blocked-connection surfacing (K12). A container backend remains the K3 swap-in.

**Cooperative controls (grok CLI `[permission]` deny rules, `gh auth logout` + PATs, tripwire).** Honest that it is not a sandbox — and therefore not one: P0.1/P0.3/P0.5 declined, S1 works verbatim, direct sockets bypass a cooperative proxy, same-UID-writable deny rules crossed in one shell command. Real mechanisms absorbed: fine-grained PATs (K7/PR5), branch protection, watchlist tripwire (K12), web-search default-off (K13), CLI drift probe.

**Layered rollout (cheap wins first, allow-default+denylist Seatbelt, opt-in containers later).** Best sequencing discipline, but its darwin OS layer is allow-default-plus-denylist (loses to any unenumerated path) and its egress is opt-in with degrade-to-open (P0.4 open longest, fail-open). Absorbed: the basic tier and degradation ladder (K9), per-run non-sticky resolution (K6), `worktreesRoot` relocation (K2), bot-side git neutralization (K2), degradation-visibility-first operability (K12).

---

## Docs touchpoints

- `README.md` — sandbox modes, host requirements (bubblewrap / kernel), agent-home provisioning runbook, HTTPS-remote requirement, migration/upgrade ordering, kill switch.
- `CLAUDE.md` — invariant amendment (PR5 blockquote); `internal/sandbox` is the only wrap point.
- `config.example.json` — `sandbox` block (incl. `allowUnconfinedDegrade`), `worktreesRoot`, `webSearch`.
- `TODO.md` — close line 176; cross-reference the broker design and `requirePushApproval`.

---

## Success criteria

1. On macOS and Linux (bwrap) with `mode:"fs"`, the S1/S5 probes (`cat ~/.ssh/id_rsa`, `cat <botrepo>/config.json`, write outside the worktree, write `.git/hooks`, rewrite the worktree gitlink) all fail with EPERM from inside a real Discord-triggered run, while a normal fix→commit→PR task (including `go test`) completes unchanged.
2. With `mode:"net-enforce"` (bwrap backend), `curl` to an arbitrary host, `localhost:8787`, and `169.254.169.254` are refused; `git push` + `gh pr create` to the configured HTTPS GitHub remote succeed through the proxy; every egress attempt appears in `data/audit/` attributed to a thread, and blocked connections **and blocked sensitive-file reads** surface on the completion card.
3. Cross-tenant probe: a run in project A cannot read project B's path, another thread's worktree, or another project's agent home; and a same-project thread A cannot **write** into thread B's `sessions/<sid>` or overwrite the shared credential file (read of B's transcript is a documented residual).
4. `/cancel`, timeout, and restart recovery behave identically to today (wrapper-pid kill and `LooksLikeGrokCLI` verified on both platforms).
5. A config tightening within the confinement band applies to the very next run of every in-flight thread without restarts, and a `networkAllow` tightening cuts in-flight egress at the next CONNECT; the effective mode of every run is visible on its completion card and in history.
6. `GROK_WORK_SANDBOX_MODE=off` restores pre-sandbox behaviour in one restart; the flag's use is visible in logs and the `/config` capability line.
7. Zero sandbox residue after worktree teardown (`data/sandbox/<threadId>` removed by all three triggers); agent homes persist.
8. **Read-denial** of the operator's `~/.grok`, `~/.ssh`, and `~/.config/gh` is confirmed on enforcing hosts by the P0.1 EPERM probes (criterion 1) plus the in-memory out-of-scope-path audit signal — **not** by the tripwire, which detects writes only. On `warn-off`/degraded runs, read-denial is not guaranteed; the fs-denial audit chip (K12) is best-effort visibility, and the write-tripwire covers modification only.

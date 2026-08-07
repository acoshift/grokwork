# Design: project-scoped file storage on Google Cloud Storage

## Goal

Let a project link one GCS bucket (plus optional object-name prefix) and give
project members a **Files** page in the workspace: browse the bucket one
"folder" level at a time, upload files, download them, and delete them. The
bucket is where a team parks customer files, artifacts and hand-off material
that does not belong in git — today those files live in Discord messages or on
someone's laptop.

Web-only. No Discord surface: local paths and customer files must never leak
into Discord messages, and the web UI is the private-network admin surface.

## Non-goals

- No signed URLs (`gcloud storage sign-url` needs a service-account key file;
  downloads proxy through the server instead).
- No bucket creation, lifecycle, or IAM management — the bucket is provisioned
  out of band; grokwork only reads and writes objects.
- No background sync, no mirroring of run artifacts. (A later feature may copy
  `DISCORD_UPLOAD` artifacts here; this design only lays the storage link.)
- No object versioning/metadata editing.

## Approach: wrap the `gcloud storage` CLI

A new package `internal/gcs` wraps the `gcloud` binary exactly the way
`internal/ghpr` wraps `gh`:

- Auth comes from the host's gcloud config (`~/.config/gcloud`, ADC or a
  logged-in account). No credentials in `config.json` — consistent with `gh`,
  and the deploy env allowlist already documents that HOME is inherited for
  exactly this reason.
- No new Go dependency. `cloud.google.com/go/storage` would pull a large
  module tree into a repo whose entire dependency policy is "stdlib + a
  handful"; the CLI is already the house pattern for external systems.
- The seam is a `Runner` func type identical to `ghpr.Runner` (tests inject
  fakes; `gcloud`/`gsutil` are already PATH-poisoned in the test packages, so
  the real binary can never be exec'd from tests).

## Config (`internal/config`)

New pointer sub-config on `ProjectConfig` (tri-state discipline: nil = feature
off, normalize returns nil when empty so `config.json` never grows an empty
object):

```json
"projects": {
  "app": {
    "storage": { "gcsBucket": "acme-app-files", "prefix": "grokwork" }
  }
}
```

```go
// internal/config/storage.go
type ProjectStorageConfig struct {
    GCSBucket string `json:"gcsBucket"`
    Prefix    string `json:"prefix,omitempty"` // optional object-name prefix, no trailing slash
}
```

- Follow `internal/config/deploy.go` for the full idiom: clone func,
  `normalizeProjectStorage` (trim; empty bucket ⇒ nil), read accessor
  `ProjectStorage(name)` returning a clone, and a `mutateProjectStorage`
  helper (validate before locking, mutate under the write lock, persist inside
  it, nil an empty sub-config).
- Setter `SetProjectStorageGCS(project, bucket, prefix string) error`:
  - Bucket: empty clears the link. Otherwise validate against GCS bucket-name
    rules, conservatively: lowercase letters, digits, `-`, `_`, `.`; 3–222
    chars; must start and end with a letter or digit. Reject anything else —
    the bucket name is spliced into a `gs://` URL handed to a CLI that
    expands wildcards.
  - Prefix: optional; reject a leading `/`, any `..` segment, wildcard chars
    (`*`, `?`, `[`, `]`), control chars/newlines; strip a trailing `/`.
- Touch all five round-trip points: field on `ProjectConfig`, normalize in
  `ProjectsMap.UnmarshalJSON`, marshal in the `outObj` shadow struct, clone in
  `cloneProjectsMap`, and the `Snapshot()` projection (`ProjectItem` gains
  `StorageBucket string` + `StoragePrefix string` — the bucket name is not a
  secret, so it may be shown verbatim).
- A bad `storage` block (invalid bucket name) is a **load error**, matching
  `normalizeSLA`: a typo silently disabling storage looks identical to storage
  being off.
- `config.example.json`: add a commented example under one project.

## CLI wrapper (`internal/gcs`)

```go
type Runner func(ctx context.Context, dir, name string, args ...string) ([]byte, error)
```

Same nil-tolerant `Foo`/`FooWith` pairs as ghpr; binary name is the literal
`"gcloud"`; errors wrap as `fmt.Errorf("gcloud %s: %s", strings.Join(args," "), stderrOrErr)`.
All operations take `bucket, prefix` and an object name or sub-path and build
the URL themselves — callers never concatenate `gs://` strings.

**Object-name containment is the security core.** One validator used by every
operation *and* by the web layer before names reach a URL:

```go
// ValidateObjectPath: non-empty, no leading '/', no empty or '.'/'..'
// segments, no wildcard chars (*, ?, [, ]) — gcloud expands wildcards, so an
// object name containing one becomes a glob over the whole bucket — no
// control chars or newline, max 1024 bytes.
func ValidateObjectPath(p string) error
```

Operations (each validates inputs first, then runs):

- `List(ctx, run, bucket, prefix, subPath) ([]Entry, error)` —
  `gcloud storage ls --json gs://<bucket>/<joined>/`. One level only (the
  non-recursive default): entries are files plus "folder" pseudo-entries
  (URLs ending in `/`). Parse defensively into
  `Entry{Name, IsDir bool, Size int64, Updated time.Time, ContentType string}`
  — the JSON API returns size as a string; tolerate string or number, and
  tolerate missing metadata (fall back to zero values rather than failing the
  listing). Names returned are relative to `prefix/subPath`.
- `Describe(ctx, run, bucket, object) (Entry, bool, error)` —
  `gcloud storage objects describe gs://… --format=json`; the bool is
  "exists" (a not-found stderr is not an error).
- `Upload(ctx, run, localPath, bucket, object) error` —
  `gcloud storage cp <local> gs://…`. Caller stages the local file.
- `Download(ctx, run, bucket, object, destPath) error` —
  `gcloud storage cp gs://… <dest>`.
- `Delete(ctx, run, bucket, object) error` —
  `gcloud storage rm gs://…`. Belt-and-braces guard before exec: refuse if the
  final URL contains a wildcard char or ends in `/` (never delete a "folder"),
  and never pass `-r`/`--recursive`.

## Web UI

### Files page — `GET /projects/{project}/files[?path=<subPath>]`

Follows the `deploysPage` shape: `requireAuth`, `ensureProjectAccess`,
fill affordances before every early return so the page renders its own chrome
when the bucket is unset, `gcloud` is missing, or the listing fails (inline
error, never a 500 — `TestPagesRender` runs against a fixture with no bucket).

- No bucket configured → explainer card linking to
  `/config/projects/{name}/integrations` (visible path only for admins).
- Listing: breadcrumb from `?path=` (each crumb a link), folder rows navigate
  (`?path=sub/`), file rows show name, size (`formatBytes`), updated time,
  Download link, and (capability-gated) a Delete button with `data-confirm`.
- `?path=` is validated with `ValidateObjectPath` (empty allowed = root);
  invalid → treat as root with an inline error, never echoed into a URL.
- Rows are capped at 200 with the pre-cap count printed when clipped
  ("bounded and printed" — same rule as /search).
- Upload form (capability-gated): multipart, single file input, optional
  "Overwrite if exists" checkbox.
- Page marker `id="page-project-files"`; sidebar workspace nav gains a
  **Files** entry (`data-icon="files"` + icon CSS rule) between Commits and
  Deploys; `IsFiles` flag on `pageData`. `/projects/{p}/…` already derives the
  nav scope — no `navScopeFromURL` change.

### Mutations

Feature flag **`storage`** added to `webAuth.features` (touch:
`WebAuthFeatures`, `FeatureStorage()`, the `requireFeature` switch,
`config.example.json`) — like every feature flag it is off when webAuth is
off, because a write to a shared bucket needs an identity for the audit trail.

- `POST /projects/{project}/files/upload` —
  `requireFeature("storage", requireMember(…))`; handler additionally checks
  `ResolveCapabilities(project, uid).CanStorageWrite()` — a derived predicate
  (`FileEscalation || SafeOps || CanShip()`, the `CanInvestigateShell`
  composition) because no builtin template grants bare `SafeOps`, so gating on
  it alone would refuse every default deployment including admins. Builtin
  `operator` stays read-only on purpose. Multipart handling mirrors
  `formImageUploads`' ordering contract (parse before any `PostFormValue`)
  but accepts any content type; cap 50 MiB via `http.MaxBytesReader` (under
  `memberMutationBodyLimit`). Filename sanitized with the attachments-style
  sanitizer; object = `<path>/<sanitized>`; unless overwrite was ticked,
  `Describe` first and refuse with an inline error if it exists (racy, but
  legible). Stage to `os.MkdirTemp` then `gcs.Upload`, cleanup deferred.
- `POST /projects/{project}/files/delete` — same gates; body carries
  `object`; validated, then `gcs.Delete`.
- `GET /projects/{project}/files/download?object=…` — `requireAuth` +
  `ensureProjectAccess` (read, like the page). Validate name; `Describe` and
  refuse > 100 MiB; download to `os.MkdirTemp`, `http.ServeContent` with a
  sanitized `Content-Disposition` filename and the described content type,
  cleanup deferred. Not found → 404.

Redirects: upload/delete redirect back to the files page preserving `?path=`
plus `ok=`/`err=`, following the `projectConfigTabRedirect` shape.

### Settings — Integrations tab

One "File storage (GCS)" card on the existing Integrations tab (not a new
tab): bucket + prefix inputs, Save posts to a flat named route
(`config.setProjectStorage` → `/config/projects/storage`) with the project in
a hidden `name` field, handled by `setProjectStorage` → `s.cfg.SetProjectStorageGCS`
→ `auditAction` → `projectConfigTabRedirect(ctx, name, "integrations", …)`.
Admin-only like every config write (`requireAdmin`). Clearing the bucket
unlinks. The card states the host requirement: the `gcloud` CLI must be
installed and authenticated (ADC) with access to the bucket.

### Server seam

`Server.gcsRunner gcs.Runner` in the test-injectables block, exposed via
`s.gcsRun()` (nil → production default), mirroring `ghRunner`/`ghRun`.

## Audit

Constants in `internal/audit`: `ActionStorageUpload = "storage.upload"`,
`ActionStorageDelete = "storage.delete"`, `ActionConfigSetProjectStorage =
"config.set_project_storage"`. Upload/delete details carry
`{project, bucket, object, size}` — object names identify the acted-on thing
(like PR URLs); **local staging paths never enter details** (ScrubPaths covers
regressions). Config-set detail carries `{name, bucket, prefix}`. Failures
audit too (`auditAction` is called with the error unconditionally).

## Tests

- `internal/config`: round-trip (unmarshal → marshal → clone → Snapshot) for
  the storage block; setter validation table (bad bucket names, wildcard
  prefix, clearing); load error on an invalid stored bucket.
- `internal/gcs`: fake-Runner tests asserting exact argv per operation;
  `ValidateObjectPath` table (traversal, wildcards, leading slash, control
  chars); ls JSON parsing (string sizes, folder entries, missing metadata);
  delete guard refusals.
- `internal/web`: `TestPagesRender` row for `/projects/proj/files` (renders
  with no bucket configured); nav anchor pin; upload/delete/download handler
  tests with an injected `gcsRunner` (success, capability refusal 403,
  feature-flag 404, invalid `object` refusal, oversize download refusal);
  settings POST redirect + persistence pin in `TestProjectConfigPage` style;
  download sets `Content-Disposition`.

## Sharp edges (why the design is shaped this way)

- **Wildcard injection**: `gcloud storage` expands `*`, `?`, `[` in URLs. An
  unvalidated object name turns "delete this file" into "delete matching
  files". Validation therefore lives in `internal/gcs` (every operation), not
  only in handlers — the same containment-at-the-boundary rule as
  `gitworktree.ResolveLocalRepo`.
- **No shell**: the Runner execs `gcloud` directly (`exec.CommandContext`),
  never `sh -c`, so quoting is not a defense that can be forgotten.
- **Proxy, don't redirect**: downloads stream through the server because the
  bucket is private; nothing about bucket layout or object URLs reaches the
  browser beyond relative names the viewer may already list.
- **Fail visible**: a missing `gcloud` binary or unauthenticated host renders
  as an inline error on the page (with stderr), not a 500 and not an empty
  listing that reads as "no files".

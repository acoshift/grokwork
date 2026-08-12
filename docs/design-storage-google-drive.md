# Design: Google Drive as a second Files storage backend

| Field | Value |
|-------|--------|
| **Author** | — |
| **Date** | 2026-08-12 |
| **Status** | Implemented (see PR plan; ship as filestore + config+gdrive+web unit) |
| **Related** | [`design-project-storage-gcs.md`](./design-project-storage-gcs.md), [`design-global-project-storage.md`](./design-global-project-storage.md) |
| **Revision** | 2026-08-12 — address design review (isolation race, REST contract, path parity, JWT checklist, clear/confirm predicates, PR ship unit, timeouts, delete/overwrite) |

## Overview

Project **Files** today is GCS-only: `config.StorageConfig` carries `gcsBucket` / `prefix` / `credentialsFile`, `Config.EffectiveStorage` resolves global→project inheritance with prefix isolation, and `internal/web/files.go` talks exclusively to `internal/gcs` (gcloud CLI). Operators who already park team files in Google Drive want the same browse / upload / download / delete surface without standing up a GCS bucket.

This design adds **Google Drive as a second backend** behind the same Files UX and the same global → project whole-block inheritance model. A thin `internal/filestore` package owns shared types and a `Backend` interface; GCS stays the gcloud adapter; Drive is a new thin HTTP + service-account JWT client (`internal/gdrive`). Existing configs with no `backend` field and a non-empty `gcsBucket` continue to work with **zero rewrite**.

## Background & Motivation

### Current state (verified in code)

| Piece | Location | Behavior |
|-------|----------|----------|
| Config type | `internal/config/storage.go` → `StorageConfig` | `{gcsBucket, prefix, credentialsFile, disabled}` |
| Global default | `Config.Storage` + `GlobalStorage` / `SetGlobalStorageGCS` | Host-wide; `disabled` forbidden |
| Project override | `ProjectConfig.Storage` + `ProjectStorage` / `SetProjectStorageGCS` / `ClearProjectStorage` / `SetProjectStorageDisabled` | Whole-block; empty bucket on save is an **error** |
| Effective target | `EffectiveStorage(name)` | disabled → nil; override with bucket → clone as-is; else join global prefix + `storageProjectSegment(name)` (pure, no I/O) |
| Files I/O | `internal/web/files.go` | `EffectiveStorage` → `storageTarget` → `gcs.List\|Describe\|Upload\|Download\|Delete` |
| Configured guards (today) | `postFileUpload` / `postFileDelete` / `fileDownload` | `st == nil \|\| st.GCSBucket == ""` — must become identity-aware for Drive |
| Global clear audit (today) | `setGlobalStorage` | `cleared: bucket == ""` — must become `!storageHasIdentity` |
| Global clear form (today) | `config_storage.tmpl` | Hidden `gcsBucket=""` only |
| Snapshot configured (today) | `Snapshot.StorageConfigured` | `GCSBucket != ""` — design changes to `storageHasIdentity` |
| GCS package | `internal/gcs` | `Runner`, `Target`, `Entry`, `ValidateObjectPath`, `SanitizeFilename` |
| Settings UI | `/config/storage`, Integrations tab | GCS-only forms; strict `action=save\|clear\|disable` |
| Feature flag | `webAuth.features.storage` + `FeatureStorage()` | Gates upload/delete only (list/download stay membership-only) |
| Capability | `Capabilities.CanStorageWrite()` | `FileEscalation \|\| SafeOps \|\| CanShip()` |
| Dep policy | `go.mod` | stdlib + discordgo/hime/goldmark/yaml — **no** Google client libs |
| Google OAuth | `internal/web/oauth_google.go` | Login identity only (`openid email profile`); **not** Drive scopes |

### Pain points

1. **Drive-native teams** — many orgs already use Shared Drives for customer hand-offs; forcing GCS is a second store and second credential path.
2. **Web is hard-wired to gcs** — four call sites import `internal/gcs` directly; adding Drive without a seam would fork every handler.
3. **Identity is bucket-shaped** — `EffectiveStorage` and `storageSourceLocked` treat “configured” as `GCSBucket != ""`. A second backend needs a backend-aware identity check without breaking the omit+bucket migration.

### Load-bearing invariants (must not regress)

From [`design-global-project-storage.md`](./design-global-project-storage.md) and live code:

- **Whole-block** override (backend + identity + credentials are coupled).
- **Strict mutators**: set requires identity of the target; clear vs disable are separate; empty identity on project save is never “re-inherit”.
- **Inherit isolation**: shared global root must not list company-wide content on every project’s Files page.
- **Files I/O** uses only `EffectiveStorage`; settings use raw `ProjectStorage` / `GlobalStorage`.
- **`EffectiveStorage` is pure** (no network, safe under `RLock`) — child-folder *ID* resolution for Drive happens at the filestore boundary, not inside config.
- Credentials path only in config; audit gets `credentialsFileSet` bool, never path/material (`audit.ScrubPaths` is belt-and-braces).

## Goals & Non-Goals

### Goals

1. One Files UX regardless of backend (one folder level list, upload, download, delete; same caps: 50 MiB up, 100 MiB down, 200-row list). Nested upload paths behave like GCS: missing intermediate folders are auto-created on Upload (K13).
2. Config chooses backend per storage block (global and/or project override), whole-block as today.
3. Global inherit isolates projects (Drive: child folder under shared parent; GCS: path prefix under shared bucket).
4. No silent cross-backend field merge; no data rewrite of existing `storage` blocks.
5. Stay inside house constraints: few dependencies, credentials as **absolute path on host**, no secrets in audit, web-only.
6. Invalid combos are load/setter errors (typo does not silently disable storage).

### Non-Goals

- Syncing or mirroring objects between GCS and Drive.
- Migrating existing GCS objects into Drive (or the reverse).
- Per-user Drive (“each member’s My Drive”).
- Google Docs / Sheets native conversion / export on download (v1 refuses; list+delete still work).
- Signed browser URLs (keep server-proxy download like GCS).
- Shared-drive *management* (create shared drives, IAM UI) — operators provision out of band.
- Changing `webAuth.features.storage` or `CanStorageWrite`.
- Multi-backend tabs on one project (still one effective target).
- User OAuth refresh tokens for Drive (v1 is service-account only).
- Resumable Drive uploads (v1 is multipart only; 50 MiB web cap makes this sufficient).
- Domain-wide delegation / JWT `sub` claim (Shared Drive membership only).

## Key Decisions

| # | Decision | Choice | Rationale |
|---|----------|--------|-----------|
| K1 | Backend model | Single `storage` block with `backend` discriminator (`gcs` \| `gdrive`) | Same inherit/disable/override machinery; field-level “bucket or folder” is ambiguous. |
| K2 | Default / migration | Omit `backend` + non-empty `gcsBucket` → treat as `gcs`; strip foreign fields on normalize (even when backend is explicit) | Zero rewrite for every existing `config.json`. Switching backend in the UI does not leave the old identity on disk. |
| K3 | Package seam | New `internal/filestore` owns `Entry`, path helpers, `Target`, `Backend` interface | Prevents `if backend ==` in every handler; GCS becomes an adapter. |
| K4 | Drive client | Thin `net/http` + SA JWT in `internal/gdrive` (PKCS#8 parse, RS256, multipart upload only) | Matches “stdlib + handful”; `google.golang.org/api` pulls a large tree (same reason GCS uses gcloud). |
| K5 | Auth | Service-account JSON **path** (`credentialsFile`); Shared Drive + SA membership is the production setup | Same pattern as GCS; no refresh-token store; login OAuth stays identity-only. |
| K6 | Path vs id | Client and web use **relative paths** under the storage root (same `?path=` / `object` as GCS). Folder/file **ids** are internal to the Drive adapter | Raw Drive ids from the query string would open any folder the SA can see. |
| K7 | `EffectiveStorage` purity | Stays I/O-free. Drive inherit stamps `IsolationSegment` (`json:"-"`) = `storageProjectSegment(name)`; adapter find-or-creates that child under `DriveFolderID` | Config must not call Drive under `RLock`; fail-closed isolation still holds. |
| K8 | Child folder timing | **Any** Files op resolves the effective root first (isolation child when segment set). **Create-if-missing applies only to `IsolationSegment`**, not to arbitrary user path segments (user intermediates are K13). | List of a new project is empty under *its* folder, not the shared parent. No create-on-config-save. |
| K8b | Isolation create race | After create, re-list by name ordered by `createdTime`; if count > 1, keep the **oldest** folder and permanently delete younger duplicates that this request can identify; **never** return “ambiguous” for the isolation segment itself | Prevents concurrent first-open from permanently bricking a project root (Drive allows duplicate names). |
| K9 | Google-native files | List + delete allowed; download returns a clear error | Export is a product feature later; silent PDF conversion would surprise operators. |
| K10 | Mutators | Unified `SetGlobalStorage` / `SetProjectStorage` taking structured input; keep GCS-named wrappers as thin callers | Backend-aware identity without three parallel save paths. |
| K11 | Empty credentialsFile for Drive | **Error** at normalize/set when `backend=gdrive` | Drive has no host “gcloud login” fallback that is safe/default; SA path is required. GCS empty still means host ADC. |
| K12 | Folder URL paste | Normalize accepts bare id **or** Drive folder URL / `folders/ID` and strips to id | Operators paste URLs from the browser bar. |
| K13 | Nested path parity | **(A) Auto-create intermediate folders on Upload** when a parent segment is missing (folder mime only). List/Describe/Download/Delete of a missing intermediate still fail closed with “parent folder does not exist” / not found | Matches GCS “object key always writable” mental model for the same UI path; avoids silent UX fork. |
| K14 | Delete folders | Drive `Delete` **refuses** `application/vnd.google-apps.folder` (mirror GCS refusing trailing `/` and never `-r`) | No recursive delete surface; operators clean folders in Drive UI. |
| K15 | Upload overwrite | `overwrite=false`: single non-folder leaf exists → error; zero → create. `overwrite=true`: single non-folder leaf → `files.update` media on that id; zero → create; multiple non-folder leaves → ambiguous error. **Never** silently create a second file with the same name | Drive’s default create always duplicates names; GCS overwrite replaces one object. |
| K16 | Ship unit | PR 1 ships alone. **PR 2+3+4 ship as one scrutinized unit** for user-visible Drive (no “configured Drive, Files dead” window from partial land). PR 5 can follow | Avoids Snapshot showing Drive while handlers still check `GCSBucket`. |
| K17 | Global cleared predicate | `cleared` / flash “Cleared” ⇔ normalized result is **nil** (`!storageHasIdentity`), never `gcsBucket == ""` | Saving Drive leaves bucket empty by design; bucket-empty ⇒ cleared is wrong and dangerous in audit. |

## Proposed Design

### Architecture

```mermaid
flowchart TD
  subgraph cfg ["config.json"]
    G["storage: StorageConfig\nbackend + identity"]
    P["projects.*.storage\noverride / disabled / nil"]
  end

  subgraph resolve ["Config.EffectiveStorage"]
    R1{disabled?}
    R2{raw has identity?}
    R3{global has identity?}
    R4["clone override as-is"]
    R5["nil"]
    R6["clone global +\nisolation for inherit"]
  end

  subgraph web ["internal/web/files.go"]
    ST["storageTarget(eff)\n→ filestore.Target"]
    OPS["List / Upload / Download / Delete"]
  end

  subgraph fs ["internal/filestore"]
    IF["Backend interface"]
    GCS["gcs adapter\n→ internal/gcs / gcloud"]
    GD["gdrive adapter\n→ internal/gdrive / Drive REST"]
  end

  G --> R3
  P --> R1
  R1 -->|yes| R5
  R1 -->|no| R2
  R2 -->|yes| R4
  R2 -->|no| R3
  R3 -->|no| R5
  R3 -->|yes| R6
  R4 --> ST
  R6 --> ST
  R5 --> web
  ST --> OPS
  OPS --> IF
  IF --> GCS
  IF --> GD
```

```mermaid
sequenceDiagram
  participant U as Browser
  participant W as filesPage
  participant C as EffectiveStorage
  participant F as filestore
  participant D as gdrive adapter
  participant API as Drive REST v3

  U->>W: GET /projects/api/files?path=docs
  W->>C: EffectiveStorage("api")
  Note over C: pure: backend=gdrive,<br/>DriveFolderID=global root,<br/>IsolationSegment="api"
  C-->>W: *StorageConfig
  W->>F: List(ctx, Target, "docs")
  F->>D: List
  D->>API: ensureIsolation (list/create child "api")
  API-->>D: isolation folder id
  D->>API: files.list parent=isolation path resolve "docs"
  API-->>D: entries
  D-->>W: []Entry
  W-->>U: table rows
```

### Config shape

```json
{
  "storage": {
    "backend": "gdrive",
    "driveFolderId": "0ABcdEfghIjKlMnOp",
    "credentialsFile": "/etc/grokwork/gdrive-sa.json"
  },
  "projects": {
    "app": {
      "storage": {
        "backend": "gcs",
        "gcsBucket": "acme-app-private",
        "prefix": "prod",
        "credentialsFile": "/etc/grokwork/gcs-key.json"
      }
    },
    "api": {
      // nil → inherit global Drive under isolated child folder "api"
    },
    "legacy": {
      "storage": { "disabled": true }
    }
  }
}
```

Legacy (still valid after this change — no rewrite):

```json
{
  "storage": {
    "gcsBucket": "acme-company-files",
    "prefix": "grokwork",
    "credentialsFile": "/etc/grokwork/gcs-key.json"
  }
}
```

| Field | Role |
|-------|------|
| `backend` | `"gcs"` or `"gdrive"`. Empty + `gcsBucket` set → `gcs`. Empty + no identity → nil block. |
| `gcsBucket` / `prefix` | GCS only; stripped when `backend=gdrive`. |
| `driveFolderId` | Drive root folder id for this block. Required for gdrive. Stripped when `backend=gcs`. |
| `credentialsFile` | Absolute path to SA JSON. GCS: empty = host ADC. Drive: **required** non-empty. |
| `disabled` | Unchanged (project-only). |

### Type changes (`internal/config/storage.go`)

```go
// Backend identifiers persisted in config and projected to the UI.
const (
	StorageBackendGCS    = "gcs"
	StorageBackendGDrive = "gdrive"
)

// StorageConfig is file storage: global host default and/or per-project override.
type StorageConfig struct {
	// Backend is "gcs" or "gdrive". Empty is normalized from identity fields
	// (see normalizeStorage). Persisted once set so Snapshot/forms stay stable.
	Backend string `json:"backend,omitempty"`

	GCSBucket string `json:"gcsBucket,omitempty"`
	// Prefix is an optional object-name prefix (GCS only; no leading/trailing slash).
	Prefix string `json:"prefix,omitempty"`

	// DriveFolderID is the Drive folder id for this block (not a path).
	// Required when Backend is gdrive.
	DriveFolderID string `json:"driveFolderId,omitempty"`

	// CredentialsFile is an absolute path to a service-account JSON key.
	// GCS: empty = host gcloud ADC. Drive: required.
	CredentialsFile string `json:"credentialsFile,omitempty"`

	// Disabled is project-only (unchanged).
	Disabled bool `json:"disabled,omitempty"`

	// IsolationSegment is set only by EffectiveStorage when inheriting Drive.
	// Never persisted (json:"-"). The Drive adapter find-or-creates a child
	// folder with this name under DriveFolderID. Empty on override / GCS.
	IsolationSegment string `json:"-"`
}
```

### Normalize rules (authoritative)

`normalizeStorage(s *StorageConfig, projectContext bool) (*StorageConfig, error)`:

1. If `s == nil` → `nil, nil`.
2. Trim all string fields. Lowercase `backend` after trim.
3. If `Disabled`:
   - `!projectContext` → error (`disabled is only valid on projects.*.storage`).
   - Return `&StorageConfig{Disabled: true}` (strip everything else).
4. **Parse folder URL** into id when `DriveFolderID` looks like a URL or contains `/folders/` (see `normalizeDriveFolderID` below).
5. **Infer backend** when empty:
   - `GCSBucket != ""` && `DriveFolderID == ""` → `gcs`.
   - `DriveFolderID != ""` && `GCSBucket == ""` → `gdrive`.
   - Both empty → return `nil` (empty block).
   - Both non-empty → error (`storage: set backend explicitly when both gcsBucket and driveFolderId are set`).
6. Switch on backend (explicit **or** inferred):
   - **`gcs`**: require `GCSBucket`; validate bucket/prefix/credentials (credentials may be empty); **clear** `DriveFolderID` (strip foreign field even if pasted); set `Backend = "gcs"`.
   - **`gdrive`**: require `DriveFolderID` after normalize; validate id charset/length; require non-empty absolute `CredentialsFile`; **clear** `GCSBucket` and `Prefix`; set `Backend = "gdrive"`.
   - Anything else → error (`storage: unknown backend %q`).
7. Clear `IsolationSegment` on normalize (never load from disk).

**Settings help (operators):** switching backend in the form and saving strips the other backend’s identity from `config.json`. The old bucket or folder id is not kept “just in case.”

```go
// normalizeDriveFolderID accepts:
//   - bare id: 0ABcd… / 1AbC…
//   - https://drive.google.com/drive/folders/<id>[?…]
//   - https://drive.google.com/drive/u/0/folders/<id>
//   - folders/<id>
// Rejects empty, path traversal, spaces, control chars.
// Id charset: [A-Za-z0-9_-], length 10–128 (conservative).
func normalizeDriveFolderID(raw string) (string, error)
```

### Identity helpers

`storageHasIdentity` is **only valid on post-normalize** configs (every Load / setter path runs `normalizeStorage` first). EffectiveStorage, Snapshot, and handlers must not invent partial blocks that skip normalize.

```go
// storageHasIdentity reports whether s is a usable storage identity after normalize.
// Precondition: s is nil, disabled-only, or fully normalized (Backend always set when non-nil non-disabled).
// Empty Backend is fail-closed except the legacy GCS defense: treat as gcs only when
// bucket is set AND folder id is empty. Folder-only with empty backend returns false
// (caller should have normalized; do not silently treat as gdrive).
func storageHasIdentity(s *StorageConfig) bool {
	if s == nil || s.Disabled {
		return false
	}
	switch strings.TrimSpace(strings.ToLower(s.Backend)) {
	case StorageBackendGCS:
		return strings.TrimSpace(s.GCSBucket) != ""
	case StorageBackendGDrive:
		return strings.TrimSpace(s.DriveFolderID) != ""
	case "":
		// Defense only: pre-normalize or hand-built test fixtures.
		// Fail closed on folder-only (empty backend + driveFolderId).
		if strings.TrimSpace(s.DriveFolderID) != "" {
			return false
		}
		return strings.TrimSpace(s.GCSBucket) != ""
	default:
		return false
	}
}
```

After normalize, `Backend` is always set when the block is non-nil and not disabled, so production callers never hit the empty-backend branch.

### Effective resolution (updated)

```
EffectiveStorage(name):
  1. if c == nil or name unknown → nil
  2. raw := Projects[name].Storage   // already normalized at load/mutate
  3. if raw != nil && raw.Disabled → nil
  4. if storageHasIdentity(raw) → return cloneStorage(raw)
       // override as-is; IsolationSegment stays empty
  5. if !storageHasIdentity(c.Storage) → nil
  6. out := cloneStorage(c.Storage)
  7. switch out.Backend:
       gcs:
         prefix, err := JoinStoragePrefix(out.Prefix, name)
         if err != nil → log stderr; return nil
         out.Prefix = prefix
       gdrive:
         seg := storageProjectSegment(name)  // never empty
         out.IsolationSegment = seg
         // DriveFolderID stays the global parent root
         // Prefix stays empty (GCS field)
  8. out.Disabled = false
  9. return out
```

```mermaid
flowchart TD
  A[EffectiveStorage name] --> B{raw.Disabled?}
  B -->|yes| N[nil]
  B -->|no| C{raw has identity?}
  C -->|yes| O[clone raw override]
  C -->|no| D{global has identity?}
  D -->|no| N
  D -->|yes| E{backend}
  E -->|gcs| F[JoinStoragePrefix]
  F -->|err| N
  F -->|ok| G[clone global + joined Prefix]
  E -->|gdrive| H[clone global + IsolationSegment]
```

| Project stored | Global | Effective |
|----------------|--------|-----------|
| nil | gdrive root `R` | gdrive, FolderID=`R`, IsolationSegment=`api` |
| `{backend:gdrive, driveFolderId:X}` | anything | gdrive, FolderID=`X`, IsolationSegment empty |
| `{backend:gcs, gcsBucket:B, prefix:p}` | anything | gcs as today |
| `{disabled:true}` | anything | nil |
| nil | gcs | gcs + joined prefix as today |

**Cross-backend inherit is impossible by construction:** inherit is the whole global block. A project cannot inherit “GCS bucket but Drive folder.”

### Mutators

```go
// StorageInput is the structured form for save (global or project).
type StorageInput struct {
	Backend         string // "gcs" | "gdrive" | "" (infer)
	GCSBucket       string
	Prefix          string
	DriveFolderID   string
	CredentialsFile string
}

// SetGlobalStorage sets or clears the host default.
// After normalizeStorage(next, false):
//   - storageHasIdentity(next) → set c.Storage = next
//   - else → nil c.Storage (clear). This is the only global clear path.
// Disabled never accepted (normalize with projectContext=false).
// Return value for callers that need it: cleared := !storageHasIdentity(result).
func (c *Config) SetGlobalStorage(in StorageInput) error

// SetProjectStorage sets a full override. Requires identity after normalize.
// Empty identity → error naming ClearProjectStorage / SetProjectStorageDisabled.
func (c *Config) SetProjectStorage(project string, in StorageInput) error

// Keep thin wrappers so existing tests/callers compile during the PR:
func (c *Config) SetGlobalStorageGCS(bucket, prefix, credentialsFile string) error {
	return c.SetGlobalStorage(StorageInput{
		Backend: StorageBackendGCS, GCSBucket: bucket,
		Prefix: prefix, CredentialsFile: credentialsFile,
	})
}
func (c *Config) SetProjectStorageGCS(project, bucket, prefix, credentialsFile string) error {
	return c.SetProjectStorage(project, StorageInput{
		Backend: StorageBackendGCS, GCSBucket: bucket,
		Prefix: prefix, CredentialsFile: credentialsFile,
	})
}

// Optional Drive wrappers for tests/clarity:
func (c *Config) SetGlobalStorageDrive(folderID, credentialsFile string) error
func (c *Config) SetProjectStorageDrive(project, folderID, credentialsFile string) error
```

`ClearProjectStorage` / `SetProjectStorageDisabled` / `CountInheritingStorageProjects` unchanged in meaning (`CountInheritingStorageProjects` still counts raw `pc.Storage == nil` only). `storageSourceLocked`: override when `storageHasIdentity(raw)` (not only GCS bucket).

### Snapshot / pageData fields

Extend existing projections (no secrets):

```go
// Snapshot (global raw)
StorageBackend         string // "gcs" | "gdrive" | ""
StorageBucket          string // existing
StoragePrefix          string
StorageDriveFolderID   string // new
StorageCredentialsFile string
StorageConfigured      bool   // storageHasIdentity(global) — NOT gcsBucket != ""

// ProjectItem
StorageBackend              string // raw override backend (empty if nil/disabled)
StorageBucket, StoragePrefix, StorageCredentialsFile // existing raw
StorageDriveFolderID        string // raw
StorageEffectiveBackend     string
StorageEffectiveBucket      string
StorageEffectivePrefix      string
StorageEffectiveDriveFolderID string // parent root when inherited; override folder when override
StorageEffectiveIsolation   string // IsolationSegment when Drive inherit (for admin chrome)
StorageSource               string // unchanged labels
```

Files `pageData` (handler-set):

```go
d.StorageBackend = eff.Backend          // "gcs" | "gdrive"
d.StorageBucket = eff.GCSBucket         // GCS chrome
d.StoragePrefix = eff.Prefix
d.StorageDriveFolderID = eff.DriveFolderID
d.StorageInherited = ...
// Header copy switches on backend (see UI).
```

Configured checks in handlers that currently require `GCSBucket != ""` must use `eff != nil` only:

```go
// before
if st == nil || strings.TrimSpace(st.GCSBucket) == "" { ... }
// after
if st == nil { ... } // EffectiveStorage already encodes identity
```

### Filestore package (`internal/filestore`)

Owns types and validation **moved** from `internal/gcs` (prefer **same-PR move** and update all call sites).

```go
package filestore

const (
	BackendGCS    = "gcs"
	BackendGDrive = "gdrive"
)

// Entry is one listing / describe row (backend-agnostic).
type Entry struct {
	Name        string
	IsDir       bool
	Size        int64
	Updated     time.Time
	ContentType string
}

// Target is the resolved identity for one Files operation.
// Built only from EffectiveStorage (or test fixtures).
type Target struct {
	Backend string // "gcs" | "gdrive"

	// GCS
	Bucket string
	Prefix string // object-name prefix (already joined when inherited)

	// Drive
	FolderID          string // configured root (parent when IsolationSegment set)
	IsolationSegment  string // inherit child name; empty on override

	CredentialsFile string
}

// Backend is the storage operations surface used by web/files.go.
// Upload may take an overwrite bool (web already knows it); either extend the
// signature or pass overwrite via a small UploadOptions — implementers pick
// one shape and use it for both adapters.
type Backend interface {
	List(ctx context.Context, t Target, subPath string) ([]Entry, error)
	Describe(ctx context.Context, t Target, object string) (Entry, bool, error)
	Upload(ctx context.Context, localPath string, t Target, object string, overwrite bool) error
	Download(ctx context.Context, t Target, object, destPath string) error
	Delete(ctx context.Context, t Target, object string) error
}

func ValidateObjectPath(p string) error   // move from gcs (same rules)
func SanitizeFilename(name string) string // move from gcs
```

**Recommendation:** prefer a small `filestore.Store` held on `web.Server`:

```go
// web.Server fields (test injectables)
gcsRunner   gcs.Runner           // existing
driveHTTP   *http.Client         // optional; nil → default with timeout
driveAuth   gdrive.TokenSource   // optional fake for tests

func (s *Server) filesBackend(t filestore.Target) (filestore.Backend, error)
```

GCS adapter maps `filestore.Target` → `gcs.Target{Bucket, Prefix, CredentialsFile}` and delegates to existing `gcs.List` etc. Keep `internal/gcs` as the gcloud CLI package (do not rename). GCS `Upload` ignores folder auto-create (object keys are virtual); overwrite already uses Describe + cp.

### Drive adapter (`internal/gdrive`)

#### HTTP client defaults

| Setting | Value |
|---------|--------|
| Default `http.Client.Timeout` for API (list/meta/delete/token) | **30s** |
| Media upload/download client timeout | **120s** (or context from web request; web already has body limits) |
| Token + API may share a client with 60s if one client is simpler — **minimum: never infinite** |
| Size caps | **Stay in `files.go`** (`maxFileUploadBytes` 50 MiB, `maxFileDownloadBytes` 100 MiB). Adapter streams; web still `Describe` then refuse oversize before Download (same as GCS path). |
| List pageSize | **200** |
| List stop | Collect until `len >= 201` **or** `nextPageToken` exhausted; return first 200 to UI with clipped=true when more existed (parity with `filesListCap`) |

#### Auth (SA JWT, stdlib) — implementer checklist

```go
// TokenSource mints Bearer tokens for Drive API calls.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// JWTBearer reads a Google service-account JSON key file and exchanges a
// signed JWT for an access token (OAuth 2.0 service account flow).
type JWTBearer struct {
	CredentialsFile string
	// Scope default: https://www.googleapis.com/auth/drive
	Scope string
	HTTP  *http.Client
	// token cache (mu + accessToken + expiry) — refresh ~60s early
}
```

**`JWTBearer.Token` algorithm (authoritative):**

1. If cached `accessToken` valid until `expiry - 60s`, return it.
2. Read credentials file; `json.Unmarshal` into struct with at least:
   - `client_email` (string)
   - `private_key` (PEM string)
   - `private_key_id` (string, optional → JWT `kid`)
   - `token_uri` (string; **default** `https://oauth2.googleapis.com/token` if empty)
3. Parse PEM block from `private_key`. Google SA keys are **PKCS#8** (`-----BEGIN PRIVATE KEY-----`):
   - `x509.ParsePKCS8PrivateKey(der)` then type-assert `*rsa.PrivateKey`
   - Do **not** use `ParsePKCS1PrivateKey` (that is PKCS#1 `BEGIN RSA PRIVATE KEY`).
4. Build JWT header JSON: `{"alg":"RS256","typ":"JWT"}` and if `private_key_id != ""` add `"kid":"<id>"`.
5. Build claims JSON:
   - `iss` = `client_email`
   - `scope` = `https://www.googleapis.com/auth/drive` (exact)
   - `aud` = `token_uri`
   - `iat` = now Unix; `exp` = now+3600
   - **No `sub` claim** — domain-wide delegation is out of scope (Shared Drive membership only).
6. Signing input: `base64.RawURLEncoding(header) + "." + base64.RawURLEncoding(claims)`.
7. Sign with `rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sha256Sum(input))`; append `"." + base64.RawURLEncoding(sig)`.
8. `POST` `token_uri` with `Content-Type: application/x-www-form-urlencoded` body:
   - `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer`
   - `assertion=<jwt>`
9. Parse JSON response:
   - Success: `access_token` (string), `expires_in` (number seconds) → `expiry = now + expires_in`
   - Error: `error`, `error_description` → return formatted error; **never log the assertion JWT or private key**
10. Cache token under mutex; return `access_token`.

**Why not `google.golang.org/api`:** the module tree is large relative to house policy (same rationale as rejecting `cloud.google.com/go/storage` for GCS). Drive needs a handful of endpoints. Stdlib already has `crypto/rsa`, `crypto/x509`, `encoding/pem`, `encoding/json`, `net/http`.

**Why not reuse login OAuth:** `oauth_google.go` scopes are `openid email profile`. Drive scopes would expand every Google login to a high-privilege consent and would bind storage to a human’s My Drive. Files storage is a **host integration**, like GCS SA keys.

#### Folder model

| Concept | Drive mapping |
|---------|----------------|
| Configured root | `Target.FolderID` |
| Inherit isolation | Child folder named `Target.IsolationSegment` under root; create if missing with race recovery (below) |
| Override | `IsolationSegment == ""` → root is `FolderID` as-is |
| Effective root | Resolved once per op before any user path walk |
| `subPath` / `object` | Chain of folder/file **names** under effective root |
| File | non-folder mime type |
| Folder | `application/vnd.google-apps.folder` |
| Upload | **multipart only** (`uploadType=multipart`); see REST contract |
| Download | `files.get?alt=media` |
| Delete | `files.delete` on **files only**; refuse folders (K14) |
| List | one level under resolved parent; pageSize 200; stop at 201 for clip |

**Create-if-missing scopes:**

| Segment | Create if missing? |
|---------|-------------------|
| `IsolationSegment` (project isolation child) | **Yes** — every op that needs the effective root |
| Intermediate folders on **Upload** path (`docs/a/b/file` → create `docs`, `a`, `b` as folders) | **Yes** (K13) |
| Intermediate folders on List / Describe / Download / Delete | **No** — fail with not found / parent missing |
| Leaf file on Upload | Create or update per K15; never duplicate name silently |

#### Isolation child race recovery (K8b)

```
ensureIsolationFolder(ctx, parentID, segment) → folderID:

1. matches := listChildrenByName(parentID, segment, foldersOnly=true)
   // q includes mimeType = folder; orderBy=createdTime; fields include id,name,createdTime,mimeType
2. if len(matches) == 1 → return matches[0].id
3. if len(matches) == 0:
     a. files.create folder {name: segment, mimeType: folder, parents: [parentID]}
     b. re-list by name (same as step 1)  // concurrent creators may have raced
     c. fall through to step 4 with the new list
4. if len(matches) > 1:
     a. sort by createdTime ascending (stable; missing createdTime last)
     b. winner := matches[0]  // oldest
     c. for each other match: attempt files.delete(id); log soft-fail if delete denied
     d. return winner.id
5. Never return error "ambiguous name" for IsolationSegment itself.
```

**Test (required in PR 3):** concurrent `ensureIsolationFolder` against a fake transport that, after two creates, returns two children with the same name — assert single winner id and that delete was attempted on the younger id; subsequent resolve is unambiguous.

#### Path resolution algorithm

```
// Every public op starts with:
rootID := ensureEffectiveRoot(ctx, t)
//   if t.IsolationSegment != "" → ensureIsolationFolder(t.FolderID, t.IsolationSegment)
//   else → t.FolderID

resolvePath(ctx, rootID, relPath, mode) → ...
// mode: walk | walkCreateParents (upload only)

1. ValidateObjectPath(relPath)
2. cur := rootID
3. parts := split relPath by /
4. for each part except last:
     child := findChildByName(cur, part, foldersOnly=true)
     if missing:
       if mode == walkCreateParents → create folder part under cur; cur = new id
       else → return error "parent folder does not exist"
     if multiple folders same name → error "ambiguous name %q"  // user paths only
     cur = child.id
5. leaf handling depends on op (list under cur, describe leaf, upload leaf, …)
```

`findChildByName` (user paths — ambiguous is an error):

```
q = "'<parent>' in parents and name = '<escaped>' and trashed = false"
// Escape name for Drive query: \ → \\, ' → \'
// Optional mimeType filter when foldersOnly
// If count == 0 → missing
// If count > 1 → error "drive: ambiguous name %q under parent" (user paths)
// If count == 1 → that file
```

**Containment:** never accept a client-supplied Drive file id as the sole handle. Download/delete always take path/`object` relative to the storage root and resolve via name walk from the effective root.

#### Upload / overwrite semantics (K15)

```
Upload(localPath, object, overwrite):
  root := ensureEffectiveRoot(...)
  parentID, leaf := resolve parents with walkCreateParents; leaf = basename(object)
  candidates := listChildrenByName(parentID, leaf, foldersOnly=false)
  files := filter non-folder candidates
  folders := filter folder candidates

  if len(files) > 1 → error ambiguous
  if len(files) == 1:
    if !overwrite → error "object %q already exists (tick Overwrite to replace)"
    else → media update existing file id (multipart uploadType=media or multipart update)
  if len(files) == 0:
    if len(folders) > 0 && leaf matches a folder only → error "name is a folder"
    → files.create multipart with parents=[parentID], name=leaf
```

Describe (exists check for web overwrite gate): returns exists=true only for a **single non-folder** leaf; ambiguous → error (not exists=false).

#### Delete semantics (K14)

```
Delete(object):
  resolve leaf; if missing → not found error
  if mimeType == folderMIME → error "refusing delete: path is a folder (not a single file)"
  if isGoogleNativeMIME → still allow delete (metadata-only object)
  files.delete(id) permanent
```

Mirror of GCS: never recursive, never folder.

#### Google-native mime policy

```go
const folderMIME = "application/vnd.google-apps.folder"

func isGoogleNativeMIME(m string) bool {
	return strings.HasPrefix(m, "application/vnd.google-apps.") &&
		m != folderMIME
}

// Download:
if isGoogleNativeMIME(meta.mimeType) {
	return fmt.Errorf("download of Google-native file %q is not supported (export later); mime=%s", name, mime)
}
```

List rows show them as normal files (size often 0 from API). Upload always creates binary files; never creates Docs.

#### Drive REST contract (authoritative for PR 3 fakes)

Base hosts:

| Purpose | Host + path prefix |
|---------|-------------------|
| Metadata | `https://www.googleapis.com/drive/v3` |
| Upload | `https://www.googleapis.com/upload/drive/v3` |
| Token | `https://oauth2.googleapis.com/token` (or `token_uri` from SA JSON) |

Common headers: `Authorization: Bearer <access_token>`, `Accept: application/json`.

All metadata mutating/list calls that touch shared content include query `supportsAllDrives=true`. List additionally: `includeItemsFromAllDrives=true`. Parent-scoped `q=` is enough; do **not** require `corpora=` for v1.

**Response fields used** (ignore the rest): `id`, `name`, `mimeType`, `size` (string), `modifiedTime`, `createdTime`, `nextPageToken`, `files` (array). Error body: `error.message` / `error.code` when present.

##### 1. List children (one page)

```
GET https://www.googleapis.com/drive/v3/files
  ?q='{parentId}'+in+parents+and+trashed%3Dfalse
  &pageSize=200
  &fields=nextPageToken,files(id,name,mimeType,size,modifiedTime,createdTime)
  &supportsAllDrives=true
  &includeItemsFromAllDrives=true
  &orderBy=folder,name
  [&pageToken=...]

Response 200:
{"files":[{"id":"…","name":"readme.txt","mimeType":"text/plain","size":"12","modifiedTime":"2026-08-01T12:00:00.000Z"}],"nextPageToken":""}
```

Name filter (findChildByName): append `and name = '{escaped}'` and optionally `and mimeType = 'application/vnd.google-apps.folder'`. Isolation re-list: `orderBy=createdTime`.

##### 2. Describe (metadata get)

```
GET https://www.googleapis.com/drive/v3/files/{fileId}
  ?fields=id,name,mimeType,size,modifiedTime,createdTime
  &supportsAllDrives=true

Response 200: single file object (same fields as list item)
Response 404: not found → exists=false at adapter boundary
```

##### 3. Download (media)

```
GET https://www.googleapis.com/drive/v3/files/{fileId}
  ?alt=media
  &supportsAllDrives=true

Response 200: raw bytes (Content-Type from object or application/octet-stream)
```

Web layer still caps size via Describe before this call.

##### 4. Create folder

```
POST https://www.googleapis.com/drive/v3/files
  ?supportsAllDrives=true
  &fields=id,name,mimeType,createdTime
Content-Type: application/json

{"name":"api","mimeType":"application/vnd.google-apps.folder","parents":["{parentId}"]}

Response 200: {"id":"…","name":"api","mimeType":"application/vnd.google-apps.folder","createdTime":"…"}
```

##### 5. Multipart upload (create file) — v1 only; **no resumable**

```
POST https://www.googleapis.com/upload/drive/v3/files
  ?uploadType=multipart
  &supportsAllDrives=true
  &fields=id,name,mimeType,size,modifiedTime
Content-Type: multipart/related; boundary=grokwork_boundary

--grokwork_boundary
Content-Type: application/json; charset=UTF-8

{"name":"readme.txt","parents":["{parentId}"],"mimeType":"text/plain"}
--grokwork_boundary
Content-Type: text/plain

<raw file bytes>
--grokwork_boundary--
```

##### 6. Multipart media update (overwrite existing file)

```
PATCH https://www.googleapis.com/upload/drive/v3/files/{fileId}
  ?uploadType=multipart
  &supportsAllDrives=true
  &fields=id,name,mimeType,size,modifiedTime
Content-Type: multipart/related; boundary=grokwork_boundary

--grokwork_boundary
Content-Type: application/json; charset=UTF-8

{"mimeType":"text/plain"}
--grokwork_boundary
Content-Type: text/plain

<raw file bytes>
--grokwork_boundary--
```

(Alternatively `uploadType=media` with body = raw bytes only is acceptable for overwrite if metadata need not change; fakes should accept either.)

##### 7. Delete

```
DELETE https://www.googleapis.com/drive/v3/files/{fileId}
  ?supportsAllDrives=true

Response 204 No Content
```

Secondary reference only: Google Drive API v3 documentation. The table above is the primary contract for tests.

#### Errors

| Status | Message shape |
|--------|----------------|
| 401/403 | `drive: permission denied (check SA has access to the Shared Drive / folder)` |
| 404 | `drive: not found` |
| 429 | `drive: rate limited, retry later` |
| other | `drive: <method>: <status> <reason>` — **never** include credentials path, token, or JWT assertion |

### Web layer changes

#### Files I/O (`internal/web/files.go`)

```go
func storageTarget(st *config.StorageConfig) filestore.Target {
	if st == nil {
		return filestore.Target{}
	}
	return filestore.Target{
		Backend:          st.Backend,
		Bucket:           st.GCSBucket,
		Prefix:           st.Prefix,
		FolderID:         st.DriveFolderID,
		IsolationSegment: st.IsolationSegment,
		CredentialsFile:  st.CredentialsFile,
	}
}

// filesPage, postFileUpload, postFileDelete, fileDownload:
//   eff := s.cfg.EffectiveStorage(project)
//   if eff == nil { empty / error states — no backend call }
//   be, err := s.filesBackend(storageTarget(eff))
//   be.List / Describe / Upload(..., overwrite) / Download / Delete
// path validation: filestore.ValidateObjectPath
// sanitize: filestore.SanitizeFilename
// size caps remain here (Describe before Download)
```

Audit details for upload/delete:

```go
detail := map[string]any{
	"project": project,
	"backend": st.Backend,
	"object":  object,
	"size":    fh.Size, // upload only
}
if st.Backend == config.StorageBackendGCS {
	detail["bucket"] = st.GCSBucket
} else {
	detail["folderId"] = st.DriveFolderID
	if st.IsolationSegment != "" {
		detail["isolation"] = st.IsolationSegment
	}
}
// never credentials path
```

#### Config handlers (must-change from live bucket-centric code)

`setGlobalStorage` / `setProjectStorage` read:

| Form field | Maps to |
|------------|---------|
| `backend` | `gcs` \| `gdrive` |
| `gcsBucket`, `prefix` | GCS |
| `driveFolderId` | Drive |
| `credentialsFile` | both |
| `action` | project: `save` \| `clear` \| `disable` (unchanged) |

**Global clear predicate (K17) — replace live `bucket == ""`:**

```go
// After SetGlobalStorage:
cleared := s.cfg.GlobalStorage() == nil // or capture from setter: !storageHasIdentity(normalized)
// Audit:
"cleared": cleared  // NEVER: bucket == ""
// Flash:
if cleared { msg = "Cleared global file storage default" } else { msg = "Updated global file storage default" }
```

**Clear form** (`config_storage.tmpl`) must not post only `gcsBucket=""` while leaving `driveFolderId` / `backend` / credentials in other fields. Required shape:

- Prefer a dedicated clear form with **empty** `backend`, `gcsBucket`, `driveFolderId`, `credentialsFile` (all hidden empty), **or**
- `action=clear` on global POST handled as `SetGlobalStorage(StorageInput{})` without reading residual fields.

Leftover filled fields from a dual-fieldset form must not resurrect identity on clear.

**Inherit confirm (dynamic on backend):**

- Keep attaching confirm only when `StorageInheritCount > 0` (`CountInheritingStorageProjects` = raw `nil` only — unchanged).
- **GCS string:** `Set global file storage? N project(s) without an override will inherit gs://…/{prefix}/{project}. Projects with storage disabled stay off. Projects with an override are unchanged.`
- **Drive string:** `Set global file storage? N project(s) without an override will inherit a child folder named after each project under the configured Drive folder. Projects with storage disabled stay off. Projects with an override are unchanged.`
- Implementation: JS on backend `<select>` updates `data-confirm` on the save form (or two server-rendered confirm strings swapped). **Must not** leave GCS-only confirm when saving Drive.

**Project isolation warning (PR 4 checklist):**

- Keep existing GCS same-bucket / prefix check (`files.go` ~441).
- **Add:** when `action=save` and backend is gdrive and `driveFolderId == GlobalStorage().DriveFolderID`, append warning flash that the override shares the global root (sibling isolation folders of inheriting projects are visible).

#### Templates

| Template | Change |
|----------|--------|
| `config_storage.tmpl` | Backend `<select>`; show GCS fields or Drive fields via fieldsets (`data-backend-fields`); dynamic `data-confirm`; clear form empties all identity fields; intro copy “GCS or Google Drive”; note that switching backend strips the other identity on save |
| `project_config_integrations.tmpl` | Same backend picker; same-folder isolation warning path; effective chrome `Drive · folder {short id}` or `GCS · bucket/prefix` |
| `files.tmpl` | Header: GCS line as today **or** `Drive · folder {id}` (+ `via global default`); empty-state copy mentions either backend |
| Hub drill `config.tmpl` | Value shows backend + short identity |

Strict actions preserved: no “clear by emptying the folder id and Save.”

### Operator setup (document in example + settings help)

**Recommended production:**

1. Create a Google Cloud service account; download JSON key to the host (e.g. `/etc/grokwork/gdrive-sa.json`, mode `0600`, owned by the grokwork user).
2. Create a **Shared Drive** (Workspace); add the SA as **Content manager** (or higher) on that Shared Drive.
3. Create a root folder for grokwork (optional) inside the Shared Drive; copy its folder id from the URL.
4. Set global storage: backend Google Drive, paste folder id or URL, credentials path.
5. Projects without overrides get a child folder named after the project segment on first Files use.

**My Drive + SA share** works for small deploys (share folder with SA email) but is easy to misconfigure (SA default visibility, domain restrictions). Settings copy states Shared Drive as supported production setup.

## API / Interface Changes

| Symbol | Package | Change |
|--------|---------|--------|
| `StorageConfig` | config | +`Backend`, +`DriveFolderID`, +`IsolationSegment` |
| `StorageInput` | config | **new** structured save input |
| `SetGlobalStorage` / `SetProjectStorage` | config | **new** primary mutators; clear ⇔ no identity |
| `SetGlobalStorageGCS` / `SetProjectStorageGCS` | config | thin wrappers (keep) |
| `storageHasIdentity` | config | **new**; post-normalize; fail-closed empty backend + folder-only |
| `normalizeDriveFolderID` | config | **new** |
| `EffectiveStorage` | config | Drive inherit stamps `IsolationSegment` |
| `filestore.Backend` | filestore | **new**; Upload includes `overwrite bool` |
| `gcs` path helpers | gcs | move or re-export from filestore |
| `internal/gdrive` | gdrive | **new** JWT + REST (multipart only) |
| `storageTarget` | web | returns `filestore.Target` |
| `Server.filesBackend` | web | **new** |
| Form fields | web | +`backend`, +`driveFolderId`; clear form empties all identity fields |
| Audit `cleared` | web | `GlobalStorage() == nil` after set, not `bucket == ""` |

Routes unchanged: `GET/POST /config/storage`, `POST /config/projects/storage`, Files CRUD paths.

## Data Model Changes

- **config.json only** — no new runtime stores under `data/`.
- **Migration:** load-time normalize only; omit `backend` + `gcsBucket` → gcs. No rewrite pass. Prefer **always persist backend when block non-nil** after normalize so the file is self-describing.
- **IsolationSegment** never written to disk.
- Child folders in Drive are **external state** created on first use; deleting a project in grokwork does **not** delete the Drive child (document; no orphan GC in v1).
- **Downgrade to pre-Drive binary:** typed JSON ignores unknown `backend` / `driveFolderId`. A Drive-only block has empty `gcsBucket` → current `normalizeStorage` returns **nil** → **Files silently off** (not a load crash). GCS omit-backend blocks keep working. Operators should clear or convert Drive blocks before binary downgrade, or keep binary ≥ Drive ship unit.

## Alternatives Considered

| Option | Pros | Cons | Verdict |
|--------|------|------|---------|
| A. GCS only + external sync to Drive | No code | Operators asked for in-product Files | Reject for this goal |
| B. rclone CLI backend | Many remotes, one binary | Extra host dep; argv injection; poor errors | Reject for v1 |
| C. Full `google.golang.org/api/drive` | Familiar SDK | Large module tree vs house policy | Prefer thin HTTP; SDK only if blocked |
| D. Separate top-level `driveStorage` config | Clear fields | Duplicates inherit/disable/override | Reject — one `storage` block |
| E. Field-level bucket-or-folder without backend | Fewer keys | Ambiguous empties; dual auth rules | Reject |
| F. User OAuth Drive (login scopes) | No SA | Couples login to storage; My Drive; consent creep | Reject for v1 |
| G. Resolve child folder id inside `EffectiveStorage` | Effective target is “final” | Network under RLock; needs cache; impure | Reject — stamp segment only |
| H. Create child folders at config Save for every project | Eager isolation | Creates N folders on global save; races with rename | Reject — create-on-first-I/O |
| I. Fail closed on missing intermediate Upload parents | Simpler adapter | Breaks GCS path parity for same UI | Reject — choose K13 auto-create on Upload |
| J. Resumable upload protocol | Better for huge files | Extra state machine; web already caps 50 MiB | Reject for v1 — multipart only |

## Security & Privacy Considerations

| Threat | Severity | Mitigation |
|--------|----------|------------|
| SA can see whole Drive / other folders | High | Scope root to one Shared Drive folder; path walk only under effective root; never take raw file ids from the client |
| Inherit re-opens shared Drive root | High | Auto child folder per project (`IsolationSegment`); List never lists the parent root for inheriting projects |
| Concurrent isolation create → permanent ambiguous root | High | K8b re-list + oldest-wins + delete extras; never “ambiguous” on isolation segment |
| Empty save re-inherits shared storage | High | Strict mutators (unchanged): save requires identity |
| Audit false “cleared” while Drive still set | High | K17 cleared ⇔ no identity after normalize |
| Object-name injection / path traversal | High | Reuse `ValidateObjectPath`; Drive name escape in `q=` |
| Credentials path / key material leak | High | Audit `credentialsFileSet` only; ScrubPaths; never log JWT assertion |
| Silent duplicate files on “overwrite” | High | K15 update-by-id; never create second same name when overwrite false |
| Google Workspace native file exfil via broken export | Medium | Refuse download of `vnd.google-apps.*` (non-folder) |
| Ambiguous duplicate names → wrong file | Medium | Adapter errors on ambiguous name for **user** paths; isolation uses oldest-wins |
| Login OAuth confused with Drive auth | Medium | Separate packages; Drive never uses session cookies or login tokens |
| SSRF via credentials path | Low | Absolute path only; host-local file read for SA JSON (same as GCS) |
| Folder URL with extra query junk | Low | Strict id extract; reject unexpected chars |

**AuthZ for Files** unchanged: project membership for read; `FeatureStorage` + `CanStorageWrite` for write; admin for config.

## Observability

| Event | Detail keys | Notes |
|-------|-------------|--------|
| `storage.upload` / `storage.delete` | `project`, `backend`, `object`, `size?`, `bucket?` or `folderId?`, `isolation?` | Never credentials path; never local staging path |
| `config.set_global_storage` | `backend`, `bucket` / `folderId`, `prefix?`, `credentialsFileSet`, **`cleared`** (= no identity after save), `inheritProjectCount` | |
| `config.set_project_storage` | `name`, `action`, `backend`, identity fields, `credentialsFileSet` | |

Drive API failures surface as inline `err=` flash on Files (no 500 for list). Stderr on isolation create: `[info] drive: created isolation folder %q under %s for project %q`; on race cleanup: `[warn] drive: removed duplicate isolation folder id=%s name=%q`.

No new metrics subsystem; latency target: list p95 &lt; 3s for ≤200 rows on a warm token.

## Rollout Plan

| Step | Content | Ship rule |
|------|---------|-----------|
| 1 | PR 1 filestore + GCS adapter | Independently shippable; behavior-neutral |
| 2–4 | Config + gdrive + web UI | **One ship unit** (K16): land on `main` only when all three are green and scrutinized together. No partial “Drive in config, Files still bucket-only” release. |
| 5 | Docs / example | Can follow immediately after |

**Feature flag:** none beyond existing `storage` for writes. Backend choice is config, not a flag.

**Rollback / downgrade:**

- Binary **before** Drive ship unit: Drive-only blocks (`driveFolderId` set, no `gcsBucket`) **normalize to nil** (unknown fields ignored by old types) → Files off, no crash. GCS configs keep working.
- Binary **with** Drive after operator sets Drive: rolling binary back without clearing config → Files off until config restored to GCS or binary upgraded.
- Prefer revert the whole 2–4 unit rather than half.

## Testing matrix

| Area | Cases |
|------|--------|
| config normalize | omit backend + bucket → gcs; drive folder only → gdrive; both set no backend → error; gdrive empty credentials → error; global disabled → error; folder URL → id; foreign fields stripped on explicit backend switch |
| storageHasIdentity | post-normalize gcs/gdrive; empty backend + folder-only → false; empty backend + bucket → true (defense) |
| EffectiveStorage | gcs inherit join; gdrive inherit IsolationSegment; gdrive override no segment; disabled; no global |
| Set* mutators | save gcs/gdrive; empty project save error; clear re-inherit; disable; global clear when only folder was set |
| Snapshot | backend + effective fields; StorageConfigured via identity |
| filestore / gcs adapter | existing gcs tests still pass via adapter; Upload overwrite flag wired |
| gdrive JWT | PKCS#8 parse; sign RS256; exchange against httptest; cache reuse; error body without logging assertion |
| gdrive REST fakes | assert exact method/path/query per contract section; multipart boundary shape |
| gdrive isolation race | concurrent ensureIsolation; dual create → oldest wins + delete younger |
| gdrive path | list one level; nested upload auto-creates parents; list missing parent fails; ambiguous user name; native mime download refuse; delete refuses folder; upload overwrite updates id not duplicate |
| web files | inject Drive backend fake; inherit uses child; GCS regression; audit backend field; configured without bucket |
| web settings | backend picker; clear empties all fields; cleared audit true only when nil; dynamic confirm string; same-folder isolation warning |

No live Drive or live GCS in CI (fake Runner / fake HTTP only).

## Risks

| Risk | Severity | Mitigation |
|------|----------|------------|
| Isolation create race bricks project | High | K8b oldest-wins + delete extras; concurrent fake test |
| Silent duplicate files on upload | High | K15 overwrite updates by id |
| Drive path resolution N+1 API calls | Medium | Per-request folder-id cache; one-level UI; auto-create only on upload path |
| SA without Shared Drive access | Medium | Visible Files error; settings copy documents requirement |
| Pressure to add `google.golang.org` deps | Medium | Hard cap: ship HTTP first; multipart-only keeps surface small |
| Partial PR land “Drive configured, Files dead” | Medium | K16 ship unit PR 2–4 together |
| Inherit creates many empty folders | Low | Create-on-first-use only; no global-save fan-out |
| Operators paste folder URL not id | Low | `normalizeDriveFolderID` accepts URLs |
| Child folder left after project delete | Low | Document; optional later GC tool |
| Token cache races | Low | Mutex on JWTBearer; immutable token string after mint |
| Downgrade silent disable of Drive blocks | Low | Documented; GCS unaffected |

## Open Questions

Residual items that need operator/product input only if the recommendation is rejected:

1. **Permanent delete vs trash** — **Locked:** `files.delete` (permanent), matching GCS `rm` and Files confirm (“cannot be undone”).
2. **Drive scope breadth** — **Locked:** full `https://www.googleapis.com/auth/drive` for Shared Drive SA.
3. **Persist explicit `"backend":"gcs"` after normalize** — **Locked:** yes when block non-nil.
4. **Display name for Drive root on Files header** — **v1:** short folder id (+ “via global”). Defer friendly title / “Open in Drive.”

## PR Plan

Do not skip scrutinize before ship. **PR 1** may land alone. **PR 2, 3, and 4 are one ship unit** (merge only when the unit is complete and green). PR 5 may follow.

### PR 1 — `filestore` seam + GCS adapter (behavior-neutral)

**Scope**

- Add `internal/filestore` with `Entry`, `Target`, `Backend` (Upload with `overwrite bool`), `ValidateObjectPath`, `SanitizeFilename`.
- Move validation/sanitize; update `internal/web/files.go` and tests.
- GCS adapter wraps existing `gcs.List|…`.
- Web: `storageTarget` → `filestore.Target`; still GCS-only identity.
- Tests: existing gcs + files tests pass.

**Out of scope:** Drive, backend field, UI.

### PR 2 — Config: backend + Drive fields + normalize/setters

**Scope** (not user-visible alone — lands with 3–4)

- `StorageConfig.Backend`, `DriveFolderID`, `IsolationSegment`; `StorageInput`; full normalize matrix; `normalizeDriveFolderID`.
- `EffectiveStorage` / `storageSourceLocked` / `storageHasIdentity` (post-normalize rules).
- `SetGlobalStorage` / `SetProjectStorage` + GCS/Drive wrappers; clear ⇔ no identity.
- Snapshot / `ProjectItem` new fields; `StorageConfigured` via identity.
- Unit matrix including foreign-field strip on backend switch.

**Out of scope alone:** enabling Drive in the form before PR 4. Do not advertise Drive in UI until ship unit complete.

### PR 3 — Drive backend implementation

**Scope** (ship with 2+4)

- `internal/gdrive`: JWTBearer checklist, REST contract above, isolation race recovery, path resolve, K13 upload parents, K14 delete, K15 overwrite, native-mime refuse, timeouts, list pagination.
- `filestore` gdrive adapter.
- Fake HTTP tests asserting contract URLs/queries/multipart; concurrent isolation test.
- Wire `Server.driveHTTP` / token source injectables.

### PR 4 — Web: Files switch + settings UI

**Scope** (ship with 2+3) — **must include:**

- [ ] Files handlers use `filesBackend` for both backends
- [ ] Remove all `GCSBucket != ""` configured guards; use `eff == nil` only
- [ ] Audit upload/delete: `backend`, `folderId` / `bucket`, `isolation?`
- [ ] Global audit `cleared` = no identity after save (**not** `bucket == ""`)
- [ ] Global clear form empties all identity fields (or `action=clear`)
- [ ] Dynamic inherit confirm string by selected backend (GCS vs Drive)
- [ ] Project same-`driveFolderId` isolation warning (mirror same-bucket)
- [ ] Backend picker + field show/hide on `/config/storage` + Integrations
- [ ] Settings help: switching backend strips the other identity
- [ ] `files.tmpl` / empty states / hub drill
- [ ] Tests: Drive fake; inherit isolation; clear/cleared; confirm; GCS regression

### PR 5 — Docs + operator notes

- Cross-links; Shared Drive + SA checklist; `config.example.json` Drive sample.
- Downgrade note (Drive-only → nil on old binary).
- Update this design Status → Implemented when done.

## Acceptance Criteria

1. Global or project storage can be configured as `backend=gdrive` with `driveFolderId` + required `credentialsFile`; invalid combos are load/setter errors.
2. Project with no override inherits global Drive under an isolated child folder (segment name) with race-safe ensure; override Drive uses the configured folder as-is; disable still yields no Files I/O.
3. Existing GCS configs and Files behavior unchanged when `backend` omitted.
4. Files list/upload/download/delete work against a test double of the Drive API matching the REST contract; download refuses Google-native mime types; delete refuses folders; overwrite updates existing file id.
5. Nested Upload path auto-creates intermediate Drive folders; List of missing parent fails closed.
6. Global clear / audit `cleared` is identity-based; clear form cannot leave a Drive identity half-set.
7. Automated tests cover config matrix + isolation race + web handlers; `go test` green for touched packages.
8. No new Google client library modules in `go.mod` for v1.
9. PR 2–4 do not ship user-visible Drive support partially.

## Follow-ups (explicitly out of v1)

- OAuth user credentials for Drive.
- Export Google Docs/Sheets to PDF/DOCX on download.
- “Open in Drive” deep-link column on Files rows.
- Friendly folder display name on the Files header.
- Multi-backend at once (GCS **and** Drive tabs).
- Orphan child-folder cleanup when projects are removed.
- Folder create / rename / recursive delete in the UI.
- Resumable uploads for objects &gt; multipart comfort zone (not needed under 50 MiB cap).

## References

- [`docs/design-project-storage-gcs.md`](./design-project-storage-gcs.md) — original GCS Files design
- [`docs/design-global-project-storage.md`](./design-global-project-storage.md) — global default + inherit isolation
- Live code: `internal/config/storage.go`, `internal/gcs/gcs.go`, `internal/web/files.go`, `internal/web/templates/files.tmpl`, `config_storage.tmpl`, `project_config_integrations.tmpl`
- Drive REST contract in this document (primary); Google Drive API v3 (secondary)
- House dep policy: `go.mod` (stdlib + discordgo/hime/goldmark/yaml)
- Login Google OAuth (identity only): `internal/web/oauth_google.go`

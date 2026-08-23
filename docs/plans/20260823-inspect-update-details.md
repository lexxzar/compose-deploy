# Inspect screen: update details (`update id`, `update built`)

## Overview

The `i` inspect screen's IMAGE section currently shows `image`, `digest`, `command` and
`entrypoint`. When the `⇧` glyph says an update exists, the screen cannot say **which**
image is waiting or **when it was built**.

This plan adds three rows to the IMAGE section, drawn only when the update verdict is
`true`:

```
IMAGE
  image         postgres:16-alpine
  image id      sha256:7e7dbab8…
  built         2026-07-07 17:47:22  (47d ago)
  update        available  (checked 3m ago)
  update id     sha256:c05eced0…
  update built  2026-08-19 19:14:43  (4d ago)
  command       postgres
```

The detail fetch rides the **existing `⇧` cache, TTL, session and trigger rules** — no new
key, no separate schedule. It is a strict extension of the update-detection subsystem, and
the `Composer` interface does not change.

### Verified feasibility

Measured on this machine (Docker 29.1.3 / buildx v0.30.1-desktop.1) before the plan was
written:

- A registry **config digest equals the local docker image ID**. Resolving the config digest
  at the local image's own RepoDigest returned `sha256:7e7dbab8…`, matching
  `docker image inspect --format '{{.Id}}'` exactly. So `update id` is comparable with
  `image id`.
- `--format '{{json .Manifest}}'` and `--format '{{json .Image}}'` both work, even though
  the documented `--format '{{.Manifest.Digest}}'` still silently falls through to the
  human block. The format flag is selectively broken, not uniformly broken.
- `{{json .Image}}` on a **bare tag** returns a platform-keyed map (8 entries for nginx) and
  costs 4.3 s. On a **pinned platform ref** it returns a bare object and costs ~1.2 s.
  Always pin.
- A multi-arch index interleaves **attestation manifests** with `platform.architecture ==
  "unknown"` — 8 of nginx's 16 entries. They must be filtered.
- `created` is `1970-01-01T00:00:00Z` for reproducible builds (verified on
  `gcr.io/distroless/static`). It is a sentinel, not data.

### Cost, and the risk it carries

The fetch adds **3 registry round-trips per updated image** (steps 2, 3 and 4 below), on top
of the 1 the check already makes — **4 in total per updated image**. During research this
hit Docker Hub's anonymous quota:

```
ERROR: unexpected status from HEAD request to
https://registry-1.docker.io/v2/library/postgres/manifests/16-alpine:
429 Too Many Requests
```

~15 anonymous manifest requests in ten minutes was enough. Three mitigations are built into
the design, and all three are load-bearing:

1. Details are fetched **only for services whose verdict is `true`** — usually a small
   minority.
2. The fetch is memoised **by image reference**, mirroring `scanImageUpdates`.
3. It inherits `autoUpdatesAllowed()`, so it never fires by itself on the read-only
   unmanaged screen.

A quota hit already matches `looksLikeNetworkErr` (`too many requests`), which would trip
the registry cascade and blank the whole `⇧` column. **A detail-fetch failure must therefore
never propagate into the verdict path** — see Task 7.

**Considered and rejected — reusing the index `CheckUpdates` already fetched.**
`fetchRemoteDigestVia` (`internal/compose/updates.go:697`) runs `imagetools inspect <ref>`
moments earlier, and its human output carries a `Manifests:` block with a `Name:` and
`Platform:` per entry. Reusing it would remove step 2 and cut the cost by a quarter. It is
rejected because harvesting per-platform digests from the human block is exactly the fragile
text parse that produced the `--format` regression this subsystem already carries a pin
against, and because plumbing the raw bytes out of `scanImageUpdates` would change a loop
shared by all three composers. Revisit only if the quota proves binding in practice.

## Context (from discovery)

- **Files involved**: `internal/compose/updates.go`, `internal/compose/remote.go`,
  `internal/compose/hostcontainers.go`, `internal/compose/snapshot.go` (read-only, for
  `StripTag` reuse), `internal/tui/app.go`, `internal/tui/inspect.go`, `README.md`.
- **Patterns reused**:
  - The `dockerRunner` three-method seam (`run` / `stream` / `tty`) — every top-level
    `docker` call already routes through it, and it gives remote SSH escaping plus
    `errSSHTransport` classification for free.
  - `scanImageUpdates`'s memoise-by-image-ref loop shape (`updates.go:394`).
  - The `Inspector` / `ConfigProvider` / `ExecProvider` precedent: a TUI-declared interface,
    type-asserted on the concrete composer, with `var _ …` compile-time pins in
    `internal/tui/app_test.go:14339-14351`.
  - `StripTag` (`updates.go:241`), already exported as the single source of truth for
    building a `repo@digest` ref — see `rollbackImageRef` (`snapshot.go:198`).
  - `formatInspectTime` (`internal/tui/inspect.go:409`) already renders
    `2006-01-02 15:04:05`; `humanizeAge` (`internal/tui/app.go:2893`) already renders
    `47d ago`.
- **Dependencies**: none new. No `Composer` change, so **no mock churn** in
  `internal/runner/`, `internal/tui/app_test.go`, `cmd/deploy_test.go`, `cmd/list_test.go`.
- **No help-table changes**: no new key is bound, so the `?` overlay drift pins, the
  container footer and `TestHelpGroups_NamesEveryBoundKey` are all untouched.

### Departure-site audit (done — no cleanup needed)

The repo's most fragile invariant is back-navigation state cleanup, so this was checked
explicitly rather than assumed. **No new Model field is added.** `details` lives only in
two places, both already handled:

- `updateCache`, which is context-keyed by `updatesCacheKey()` (`app.go:3869`, including the
  `unmanaged|` prefix) and already invalidated after a successful Deploy (`app.go:2654`).
- the transient `updatesMsg`, already session-gated.

Therefore `clearInspect()` (`app.go:3185`), the `esc` chain, `entryLocal` and the
`connectResultMsg` error path need **no** change, and `inspectViewportSize()`
(`app.go:3068`) needs none either — the new rows live inside the viewport, not the chrome
budget. The longest new label is `update built`: `"  update built"` is 14 cells against
`inspectValueCol = 18`, so no layout constant moves.

### Fixtures

Two real `{{json .Manifest}}` captures were taken **before** the 429 and are preserved at:

```
/Users/zavulon/.claude/jobs/5f71c92b/tmp/fixtures/
  imagetools_manifest_index_nginx.json      # 16 manifests: 8 real platforms + 8 attestation
  imagetools_manifest_index_postgres.json   # same shape, different platform set
```

These are genuine registry output, which hand-authored fixtures cannot fully imitate — the
attestation interleave and the variant fields are exactly the shapes the parser must
survive. **Copy them in Task 3 before anything else**, because that directory is scratch
space and is removed when the job is deleted. If they are already gone, hand-author both
from the shapes recorded under "Shape forks the parsers must survive" rather than re-fetching
from the registry.

## Development Approach

- **testing approach**: Regular (code first, then tests)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting the next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `go test ./...` after each change
- maintain backward compatibility

## Testing Strategy

- **unit tests**: required for every task.
- **no e2e tests**: this project has none. The TUI is tested by calling `Update()` with
  `tea.KeyMsg` directly, and the compose layer by driving fake `dockerRunner`s — no TTY and
  no Docker needed.
- **no live registry calls in tests.** Every parser test reads a captured fixture from
  `internal/compose/testdata/`. This is not only for speed: a test suite that hits Docker Hub
  would reproduce the 429 in CI.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

A new TUI-declared interface, implemented by all three composers over the shared
`dockerRunner` seam:

```go
// internal/tui/app.go
type UpdateDetailer interface {
    UpdateDetails(ctx context.Context, services []string) (map[string]compose.UpdateDetail, error)
}
```

`refreshUpdates()` — the single Cmd that already owns the `⇧` schedule — calls it in the
same goroutine, right after `CheckUpdates`, for the services that came back `true`. One
Cmd, one message, one cache entry, one session counter. The "same timings" requirement is
satisfied by construction rather than by a parallel mechanism that could drift.

The inspect screen is a **consumer** of that cache: it never fetches. When an `updatesMsg`
lands while the user is already on `screenInspect`, the handler rebuilds the summary so the
rows appear without a re-entry (Task 7).

### Key design decisions

1. **TUI-only interface, not a `Composer` change.** Keeps `runner.Composer` and its five
   mocks untouched, and keeps `cdeploy list --updates` at its current cost. The CLI has no
   detail view to render, so it would pay for data it cannot show.
2. **Details ride `refreshUpdates`, not a lazy key press.** Per the explicit requirement
   that the update info use the same rules and timings as the `⇧` glyph.
3. **A detail failure is non-fatal and never reaches `updatesMsg.err`.** The glyph is the
   load-bearing signal; the detail rows are a bonus. This mirrors the existing
   `svcErr > statsErr > updatesErr` soft-failure priority.
4. **Rows appear together, only on `true`.** `built` is fetched by the same call, so it
   appears alongside the update rows rather than always. See Residuals.
5. **Strict validation, omit on doubt.** Every parser returns "absent" rather than a guess,
   the same discipline `parseImagetoolsDigest` follows after the `--format` regression.
6. **Only fields a row draws.** `UpdateDetail` carries exactly three values. An earlier
   draft also carried `Platform` and `LocalID`; both were dropped because nothing renders
   them, and `docs/architecture/tui-inspect-screen.md` states the rule directly — "a
   parsed-but-unrendered field is a promise the screen does not keep".

## Technical Details

### The new type

```go
// internal/compose/updatedetails.go
type UpdateDetail struct {
    LocalCreated time.Time // build time of the running image; zero = unknown/sentinel
    NewID        string    // config digest of the registry image = its docker image ID
    NewCreated   time.Time // build time of the registry image; zero = unknown/sentinel
}
```

### Per-image fetch sequence

| # | Command | Yields |
|---|---|---|
| 1 | `docker image inspect --format '{{.Created}}\|{{.Os}}\|{{.Architecture}}' <ref>` | `LocalCreated` + the platform pair. **Local only.** |
| 2 | `docker buildx imagetools inspect --format '{{json .Manifest}}' <ref>` | the index; select the entry matching the platform pair |
| 3 | `docker buildx imagetools inspect --raw <pinnedRef>` | `config.digest` → `NewID` |
| 4 | `docker buildx imagetools inspect --format '{{json .Image}}' <pinnedRef>` | `created` → `NewCreated` |

`<pinnedRef>` is built with `StripTag(ref) + "@" + platformDigest`, reusing the exported
helper rather than re-deriving the repo portion.

Steps 2–4 are registry calls. Step 1 is local. All four bypass `command()` /
`remoteCommand()` — they are top-level docker commands, so they go through the
`dockerRunner` seam, exactly as `compareImageDigestVia` does.

**`{{.Variant}}` is deliberately absent from step 1.** A Go template referencing a field the
docker struct lacks is a hard execution error, not an empty string — the policy documented at
`internal/compose/hostcontainers.go:284-295`. On an older docker host that would kill step 1
for every image and silently disable the whole feature. Variant is only ever a tie-breaker in
the matching rule below, so the local side treats it as always-empty.

### Shape forks the parsers must survive

- **Step 2, index vs single manifest.** A multi-arch tag returns `{"manifests": [...]}`. A
  single-arch tag returns a manifest with no `manifests` key — in that case use the original
  ref for steps 3–4. **This must be distinguishable from "platform not found in the index"**;
  see the three-state return below.
- **Step 2, attestation entries.** Skip any entry with `platform.architecture == "unknown"`
  or `platform.os == "unknown"`.
- **Step 2, variant matching.** Match on `os` + `architecture`; treat variant as a
  tie-breaker only, never as a hard requirement.
- **Step 4, map vs object.** A pinned ref returns a bare config object. A bare tag returns a
  platform-keyed map. The parser must accept both, since a single-arch ref reaches step 4
  unpinned.
- **Epoch sentinel.** Any `created` with `Unix() <= 0` is treated as unknown and the row is
  omitted.

### The three-state index result

`parseIndexPlatformDigest` must distinguish two failures that a `(string, bool)` return
conflates:

| Result | Meaning | Caller action |
|---|---|---|
| `hasIndex=false` | no `manifests` array — a single-manifest ref | use the original ref for steps 3–4 |
| `hasIndex=true, found=false` | an index exists but the host's platform is absent | **abort this image**; omit the rows |
| `hasIndex=true, found=true` | matched | pin and continue |

Conflating the middle row with the first is the silent failure mode: step 3 would run
`--raw` against the index (no `config.digest`, so `NewID` is empty) and step 4 would return
the platform-keyed map, from which the code could pick a platform that is not the host's.

### Rendering rules

| Row | Shown when |
|---|---|
| `image`, `image id`, `command`, `entrypoint` | unchanged from today |
| `built` | detail present and `LocalCreated` non-zero |
| `update` | verdict is non-nil. `available` on `true`, `up to date` on `false`. Omitted on `nil`. |
| `update` age suffix | cache entry `fetchedAt` is non-zero → `(checked 3m ago)` |
| `update id` | detail present and `NewID` non-empty |
| `update built` | detail present and `NewCreated` non-zero |

Raw mode (`r`) stays byte-identical — `m.inspectRaw` is untouched, and these rows exist only
in the summary.

## What Goes Where

- **Implementation Steps**: all code, tests and documentation changes in this repo.
- **Post-Completion**: manual verification against a real remote host and a rate-limit
  observation that cannot be automated.

## Implementation Steps

### Task 1: Split the time formatter and add the epoch guard

Independent of the compose work — sequenced first so the two halves can proceed in parallel.

**Files:**
- Modify: `internal/tui/inspect.go`
- Modify: `internal/tui/inspect_test.go`

- [x] split `formatInspectTime(s string)` into `formatInspectTimeValue(t time.Time) string`
      plus the existing string wrapper that parses then delegates — `UpdateDetail` carries
      `time.Time`, so without the split the new rows would round-trip through RFC3339
- [x] add the epoch guard to `formatInspectTimeValue`: treat `t.Unix() <= 0` as absent,
      covering the 1970 reproducible-build sentinel alongside the existing `Year() <= 1` check
- [x] add `formatTimeWithAge(t, now time.Time) string` producing
      `2026-07-07 17:47:22  (47d ago)`, reusing `formatInspectTimeValue` and `humanizeAge`,
      and returning `""` whenever `formatInspectTimeValue` does
- [x] **do not** extend `humanizeAge` with week/month tiers — `rollbackAgeSuffix` is its other
      caller, the change is unrelated to this feature, and `47d ago` carries more information
      than `1mo ago` when choosing a rollback target
- [x] write tests asserting `formatInspectTimeValue` returns `""` for `1970-01-01T00:00:00Z`
      and for the Go zero time, and a real value for a normal timestamp
- [x] write tests for `formatTimeWithAge` across the minute/hour/day boundaries with a fixed `now`
- [x] write a test asserting the existing `started` row is unchanged for a real timestamp
- [x] run `go test ./internal/tui/` — must pass before task 2

### Task 2: Add `UpdateDetail` and the local image probe

**Files:**
- Create: `internal/compose/updatedetails.go`
- Create: `internal/compose/updatedetails_test.go`

- [x] create `internal/compose/updatedetails.go` with the three-field `UpdateDetail` struct
- [x] add `localProbeArgs(image string) []string` producing
      `image inspect --format '{{.Created}}|{{.Os}}|{{.Architecture}}' <image>` — no `.Variant`,
      no `.Id`
- [x] add `parseLocalProbe(out []byte) (localProbe, error)` — splits on `|`, parses `Created`
      as RFC3339Nano, returns a zero time for an unparseable or `Unix() <= 0` value
- [x] write tests for `parseLocalProbe`: full line, malformed field count, unparseable
      timestamp, epoch timestamp
- [x] write a test for `localProbeArgs` asserting the argv carries no `compose` element
- [x] run `go test ./internal/compose/` — must pass before task 3

➕ `parseLocalProbe` also errors on an EMPTY os/arch, not only on a wrong field count: without
a platform pair the Task 3 index match can only fail, so erroring here saves the three registry
round-trips that would follow. The timestamp guard lives in a small `parseImageTimestamp` helper
so Task 4's `parseImageCreated` reuses the same epoch rule.

### Task 3: Add the index platform-selection parser

**Files:**
- Modify: `internal/compose/updatedetails.go`
- Modify: `internal/compose/updatedetails_test.go`
- Create: `internal/compose/testdata/imagetools_manifest_index_nginx.json`
- Create: `internal/compose/testdata/imagetools_manifest_index_postgres.json`

- [ ] **first**, copy the two real captures from
      `/Users/zavulon/.claude/jobs/5f71c92b/tmp/fixtures/` into `internal/compose/testdata/`
      (scratch space — do this before anything else; if already removed, hand-author from
      "Shape forks the parsers must survive" rather than re-fetching)
- [ ] add `manifestIndexArgs(image string) []string` → `buildx imagetools inspect --format
      '{{json .Manifest}}' <image>`
- [ ] add `parseIndexPlatformDigest(data []byte, os, arch string) (digest string, hasIndex bool, found bool)`
      per the three-state table: skips `unknown` platform entries, matches os+arch with variant
      as a tie-breaker, and validates the digest against `imagetoolsDigestRE`
- [ ] write tests against the nginx fixture: selects `linux/arm64` correctly, never returns an
      attestation digest, and returns `hasIndex=true, found=false` for an absent platform
- [ ] write a test asserting the single-manifest shape yields `hasIndex=false`
- [ ] write tests for the variant tie-breaker and for malformed JSON
- [ ] run `go test ./internal/compose/` — must pass before task 4

### Task 4: Add the config-digest and created-timestamp parsers

**Files:**
- Modify: `internal/compose/updatedetails.go`
- Modify: `internal/compose/updatedetails_test.go`
- Create: `internal/compose/testdata/imagetools_raw_manifest.json`
- Create: `internal/compose/testdata/imagetools_image_config_map.json`
- Create: `internal/compose/testdata/imagetools_image_config_object.json`

- [ ] add `rawManifestArgs` and `imageConfigArgs` for steps 3 and 4
- [ ] add `parseConfigDigest(raw []byte) string` reading `config.digest`, validated against
      `imagetoolsDigestRE`, returning `""` on anything else
- [ ] add `parseImageCreated(data []byte, os, arch string) (time.Time, bool)` accepting BOTH
      the platform-keyed map and the bare object, and rejecting `Unix() <= 0`
- [ ] hand-author the three fixtures from the shapes in Technical Details
- [ ] write tests for `parseConfigDigest`: valid, missing `config`, non-sha256 value, malformed JSON
- [ ] write tests for `parseImageCreated`: map form, object form, epoch rejection, absent platform
- [ ] run `go test ./internal/compose/` — must pass before task 5

### Task 5: Add the shared `scanUpdateDetails` loop

**Files:**
- Modify: `internal/compose/updatedetails.go`
- Modify: `internal/compose/updatedetails_test.go`

- [ ] add `scanUpdateDetails(ctx, wanted map[string]string, d dockerRunner) (map[string]UpdateDetail, error)`
      mirroring `scanImageUpdates`: memoise by image ref, one entry per distinct image
- [ ] chain steps 1→4; build the pinned ref with `StripTag(ref) + "@" + digest`
- [ ] branch on the three-state index result: `hasIndex=false` → use the original ref;
      `hasIndex=true, found=false` → **abort this image**
- [ ] on any per-image failure, omit that service and continue — a partial result is valid
- [ ] short-circuit the whole scan on `errSSHTransport`, matching `scanImageUpdates`, and
      return the partial map alongside the error per the untrusted-partial-map contract
- [ ] write tests with a fake `dockerRunner`: happy path, single-manifest path, platform-absent
      abort, step-3 failure, step-4 failure, transport abort
- [ ] write a memoisation test — one image across three services issues one call set
- [ ] run `go test ./internal/compose/` — must pass before task 6

### Task 6: Bind `UpdateDetails` on all three composers

**Files:**
- Modify: `internal/compose/updates.go`
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/hostcontainers.go`
- Modify: `internal/compose/updatedetails_test.go`
- Modify: `internal/tui/app_test.go`

- [ ] add `Compose.UpdateDetails` using `fetchServiceImages` + `filterServices` + `localDockerRunner`
- [ ] add `RemoteCompose.UpdateDetails` using the remote runner, so `SSHExtraArgs` splicing and
      `classifySSHError` are inherited unchanged
- [ ] add `HostContainers.UpdateDetails` using `hostImageMap`, dropping empty refs and bare
      image IDs exactly as `CheckUpdates` does
- [ ] add the three compile-time pins beside the existing `Inspector` triple at
      `internal/tui/app_test.go:14348` — `var _ UpdateDetailer = (*compose.Compose)(nil)` and
      the same for `*compose.RemoteCompose` and `*compose.HostContainers`. Without them a
      rename or signature drift leaves the suite green while the rows silently vanish, which is
      exactly what that comment block already warns about for `i`
- [ ] extend `TestHostContainers_CapabilityInterfaces` (`app_test.go:14360`) with the runtime
      assertion for `UpdateDetailer`
- [ ] write tests asserting all three build argv with no `compose` element
- [ ] write a test asserting the remote argv splices `SSHExtraArgs` immediately before the host arg
- [ ] run `go test ./...` — must pass before task 7

### Task 7: Thread details through the update cache

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] declare the `UpdateDetailer` interface next to `Inspector` in `internal/tui/app.go`
- [ ] add `details map[string]compose.UpdateDetail` to `updatesMsg` and to `updateEntry`
      (both are keyed struct literals in tests, so this costs no test churn)
- [ ] extend `refreshUpdates()` to call `UpdateDetails` for the services whose verdict is
      `true`, guarded by the type assertion and skipped entirely when no verdict is `true` —
      note `filterServices` (`updates.go:612`) treats an empty slice as **all** services, so
      the skip is load-bearing, not an optimisation
- [ ] **discard a detail-fetch error**: never assign it to `updatesMsg.err`, so a 429 during the
      detail phase cannot blank the `⇧` column or shorten the cache entry to the error TTL
- [ ] store `details` on the cache entry beside `results`, under the same TTL and the same
      session gate; leave `hydrateUpdates` untouched
- [ ] add a `screenInspect` branch to the `updatesMsg` handler that calls
      `rebuildInspectSummary()` then `setInspectContent()` — without it, entering `i` on a cold
      cache leaves the rows permanently absent until the user backs out and re-enters
- [ ] write a test asserting a detail-fetch error still yields verdicts and the 10 m success TTL
- [ ] write a test asserting `UpdateDetails` is NOT called when every verdict is `false`
- [ ] write a test asserting a composer without the interface still produces verdicts
- [ ] write a test asserting a stale-session `updatesMsg` does not write details to the cache
- [ ] write a test asserting an `updatesMsg` arriving on `screenInspect` refreshes the summary
- [ ] run `go test ./internal/tui/` — must pass before task 8

### Task 8: Relabel `digest` → `image id`

Isolated because it is a user-visible rename with its own broken assertions.

**Files:**
- Modify: `internal/tui/inspect.go`
- Modify: `internal/tui/inspect_test.go`

- [ ] relabel the `digest` row in `inspectImageSection` (`inspect.go:292`) to `image id` — it is
      the local image ID, and an `update id` row beside a row labelled `digest` invites exactly
      the false comparison this feature must avoid
- [ ] update the four assertions that pin the old label (`inspect_test.go:578, 617, 625, 638`)
- [ ] run `go test ./internal/tui/` — must pass before task 9

### Task 9: Widen the `buildInspectSummary` signature

**Files:**
- Modify: `internal/tui/inspect.go`
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/inspect_test.go`

- [ ] add an `inspectUpdateInfo` struct carrying `now time.Time`, `detail *compose.UpdateDetail`,
      `verdict *bool` and `checkedAt time.Time`, and change the signature to
      `buildInspectSummary(doc, width int, upd inspectUpdateInfo)`. **Its zero value must draw
      no new rows**, so the migration below is mechanical and every existing assertion holds
- [ ] keep the function pure — the clock arrives inside `upd`, never via `time.Now()` internally
- [ ] update the **single** production call site: `app.go:3178`, inside `rebuildInspectSummary()`.
      `app.go:730` (the `WindowSizeMsg` branch) and `app.go:1240` (`inspectDataMsg`) both call
      `rebuildInspectSummary()`, so they inherit the change untouched
- [ ] have `rebuildInspectSummary` build `inspectUpdateInfo` itself by reading
      `m.updateCache[m.updatesCacheKey()]` and `m.inspectService` — one lookup, one site, no
      chance of three copies drifting
- [ ] migrate the 25 existing calls in `inspect_test.go` by appending `inspectUpdateInfo{}`
- [ ] write a test asserting the zero value renders a byte-identical summary to before
- [ ] run `go test ./internal/tui/` — must pass before task 10

### Task 10: Render the four new rows

**Files:**
- Modify: `internal/tui/inspect.go`
- Modify: `internal/tui/inspect_test.go`

- [ ] extend `inspectImageSection` with the `built`, `update`, `update id` and `update built`
      rows per the Rendering rules table, each omitted independently
- [ ] render the `update` row's `(checked 3m ago)` suffix from `upd.checkedAt`, omitting the
      suffix when that is zero
- [ ] verify every new line goes through `inspectBuilder.push` so the sanitiser and `wrapCells`
      soft-wrap apply, and that `viewInspect()` still does not end in a newline
- [ ] write table tests with a fixed `now`: update available (all rows), up to date (one
      `update` line, no detail rows), verdict nil (no rows at all)
- [ ] write tests for the epoch sentinel omitting `built` and `update built` independently
- [ ] write a narrow-width test asserting the new rows wrap rather than truncate
- [ ] run `go test ./internal/tui/` — must pass before task 11

### Task 11: Verify acceptance criteria

- [ ] verify the rows appear only when the verdict is `true`, and that `nil` renders nothing
- [ ] verify the detail fetch fires on the same triggers as `⇧`: screen entry on cache miss,
      `U` force refresh, and post-Deploy cache invalidation
- [ ] verify no automatic fetch occurs on the read-only unmanaged screen, and that `U` still works there
- [ ] verify raw mode (`r`) output is byte-identical to before this change
- [ ] verify no help-table or footer drift: `go test ./internal/tui/ -run TestHelp` passes untouched
- [ ] run the full suite uncached: `go test ./... -count=1`
- [ ] run `go build -o cdeploy .` and confirm the binary builds

### Task 12: Update documentation

- [ ] add the rule digest to `CLAUDE.md` under the update-detection paragraphs — the fetch
      sequence, the three-state index result, the `.Variant` prohibition, the non-fatal-detail
      rule and the epoch sentinel
- [ ] add the full rationale to `docs/architecture/update-detection.md`, including the measured
      429, the three mitigations, and the rejected index-reuse option
- [ ] add the new IMAGE rows and the `buildInspectSummary` signature change to
      `docs/architecture/tui-inspect-screen.md`
- [ ] update `README.md` in four places: line 67 and line 110 (both say "image digest"), line
      440 (the IMAGE section description) and line 195 (which enumerates the exact docker
      commands the update check runs and would otherwise undercount them)
- [ ] confirm `skills/cdeploy/SKILL.md` needs no edit — no CLI behaviour changed — and that its
      content pins still pass
- [ ] move this plan to `docs/plans/completed/`

## Residuals

Deliberately out of scope; record rather than fix:

- **`built` describes the local tag, not strictly the running container.** Step 1 inspects the
  compose image reference, while the `image id` row above it comes from the container's
  `doc.Image`. If a `docker pull` moved the tag without a redeploy, the two describe different
  images. Narrow in practice — a fully-pulled tag makes the verdict `false`, so the rows do not
  render at all — but real. Deriving step 1 from `doc.Image` would fix it and requires passing
  the container's image ID down into the compose layer.
- **`built` only shows when an update exists.** It arrives from the same call as the update
  rows. Showing it for every service needs one local `docker image inspect` per service, which
  is cheap (no registry) but changes the fetch shape.
- **One duplicated round-trip per refresh.** `Compose.UpdateDetails` calls `fetchServiceImages`
  (`docker compose config`), which `CheckUpdates` just ran in the same goroutine;
  `HostContainers.UpdateDetails` re-runs `docker ps` the same way. On the remote path both are
  full SSH round-trips. Passing the image map down from `refreshUpdates` would remove it.
- **The relative age freezes.** The summary is rebuilt on fetch, on resize and now on
  `updatesMsg`, so `(checked 3m ago)` still drifts while the user sits on the screen. Tolerable
  against a 10-minute TTL; a ticker would be the fix.
- **`created` is unreliable for reproducible builds.** distroless, ko, Bazel and nix images
  report `1970-01-01`. The epoch guard omits the row, so those images show no build date at
  all. There is no upstream fix.
- **The `--format` flag is not trustworthy across buildx versions.** `{{json .Manifest}}` and
  `{{json .Image}}` work on 0.30.1 and `{{.Manifest.Digest}}` does not. Strict validation plus
  row omission is the only defence; a future buildx could break the working forms too.
- **`update id` costs a whole round-trip on its own.** Step 3 exists only to draw that one row.
  If the 429 proves binding, dropping `update id` and keeping `update built` removes 25% of the
  registry cost and one fixture.

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification:**

- Run against a **real remote arm64 host from an amd64 machine** and confirm the resolved
  platform is the host's, not the laptop's. This is the failure mode unit tests cannot catch,
  because both sides are faked. The `hasIndex=true, found=false` abort is what should fire if
  the match is wrong — verify it does rather than silently pinning the wrong platform.
- Watch for HTTP 429 on a project with many updated services. If it appears, the per-image
  memoisation or the `true`-only gate is not working as designed.
- Confirm `docker login` on the docker host raises the quota enough for routine use, and
  document that as the recommended mitigation for teams.

**External system updates:**

- None. No CLI flag, no JSON wire-shape change, no config-file change, and no `Composer`
  contract change, so nothing downstream needs updating.

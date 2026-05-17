# Update-Available Indicators

## Overview

Surface a per-service "update available" indicator in both the cdeploy TUI and CLI `list` output by detecting whether a newer image exists in the registry for each compose service. Indicator only — no automatic pulls, no notifications, no daemon. The user keeps existing keys/commands (`d` deploy in TUI, `cdeploy deploy` in CLI) to act on what they see.

The feature converts the routine guess "is a pull worth doing right now?" into a glance. It integrates with existing service status surfaces (TUI container screen, CLI `list`) and respects the same patterns the codebase already uses for `ContainerStats`: separate Composer method, opt-in CLI flag, soft failure that never blocks the primary view, session-filtered async messages in the TUI, and TTL caching to avoid hammering the registry.

## Context (from discovery)

- **Composer interface** lives in `internal/runner/runner.go`; `ServiceStatus` is the shared row-shape between status reads and downstream rendering.
- **Local Composer** is `internal/compose/compose.go` (`Compose`); has `Detect`/`Standalone`/`SetTestHooks` patterns to mirror, and `command()` helper. `internal/compose/stats.go` is the precedent for **bypassing `command()`** when calling top-level docker commands (`docker stats`, `docker manifest inspect`).
- **Remote Composer** is `internal/compose/remote.go` (`RemoteCompose`); SSH argv is built by `remoteCommand()`, but `EditCommand`/`ExecCommand`/`AllContainerStatsRemote` bypass it. `SSHExtraArgs` must be spliced immediately before the host arg in every site.
- **TUI Model** in `internal/tui/app.go` has the established pattern for periodic refreshes: `statsSession`/`statusSession`/`projectsSession` counters bumped at 7 context-change sites; `refreshInFlight` guards against fetch stacking; `tickCmdOverride` is the test seam.
- **CLI list** in `cmd/list.go` already has the `--stats` opt-in flag pattern, multi-project discovery, JSON serialization, and `formatDots`/`formatDotsGrouped` text formatters.
- **Tests** use stdlib `testing` only — no testify. Pure parsers extracted to standalone functions; concrete types tested via `SetTestHooks` for command-construction verification.

## Development Approach

- **Testing approach**: Regular (code first, then tests in same task before closing it).
- complete each task fully before moving to the next.
- make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task — tests are not optional, they are a required deliverable.
- **CRITICAL: all tests must pass before starting the next task** — no exceptions. Run `go test ./...` (and `go build ./...`) after each task.
- **CRITICAL: update this plan file when scope changes during implementation.**
- maintain backward compatibility — `UpdateAvailable` is nil-when-unset; existing call sites remain unchanged.

## Testing Strategy

- **unit tests**: required for every task (see Development Approach).
- **TUI tests**: exercise `Update()` with `tea.KeyMsg`/custom msgs directly — no TTY needed; use the `installFakeTick` seam.
- **No e2e tests**: this repo has none; CLI behaviour is verified via `cmd/*_test.go` against the cobra `*Command` tree.
- after each task: `go build ./...` and `go test ./...` must both pass.

## Progress Tracking

- mark completed items with `[x]` immediately when done.
- add newly discovered tasks with ➕ prefix.
- document issues/blockers with ⚠️ prefix.
- update plan if implementation deviates from original scope.
- keep plan in sync with actual work done.

## Solution Overview

A new Composer method `CheckUpdates(ctx, services) (map[string]bool, error)` returns the set of services whose image-in-registry differs from the locally pulled image. The map only contains services that *were checked* — absent entries mean "unknown" (build-only services, errors, etc.). The implementation tries `docker compose pull --dry-run --quiet` first (Compose v2.22+, single round-trip per project) and falls back to per-image `docker manifest inspect` digest comparison when dry-run isn't supported. Capability is probed once via `docker compose pull --help` and cached on the Composer struct, mirroring `Standalone`/`detected`.

`ServiceStatus` gains a tri-state `UpdateAvailable *bool` field (nil/false/true). The TUI hydrates from a 10-minute TTL cache on screen entry and exposes a `U` force-refresh key. Stale-response protection uses a new `updatesSession` counter (bumped at the same 7 sites as `statsSession`/`statusSession`) and a dedicated `updateInFlight` flag. Rendering is an inline `⇧` (U+21E7, yellow) immediately after the service name; column alignment is preserved by reserving 2 cells in `maxName` whenever any service has an update.

The CLI mirrors the TUI: single-project mode (`-C` specified) always checks; multi-project mode gates the check behind a new `--updates` flag (same opt-in pattern as `--stats`). Errors are soft — stderr warning, exit 0, blank cells — so the primary status view never breaks. JSON output gains `update_available *bool` with `omitempty`.

## Technical Details

### Data structures

- `runner.ServiceStatus.UpdateAvailable *bool` — tri-state. `nil` = unknown / not checked / error (blank cell), `true` = update available (glyph), `false` = current (no glyph).
- `runner.Composer` gains `CheckUpdates(ctx context.Context, services []string) (map[string]bool, error)`. Empty `services` = all services in project.
- `compose.Compose` and `compose.RemoteCompose` each grow:
  - `dryRunSupported bool` (probe result)
  - `dryRunDetected bool` (probe done flag)
  - `SetTestHooks` extended to expose the probe (allow tests to force either branch).

### Detection flow

1. If `!dryRunDetected`, run `docker compose pull --help` (local) or via SSH (remote) and grep for `--dry-run`. Cache result.
2. If supported: `docker compose pull --dry-run --quiet` for the project. Parse stderr for service-name lines containing one of: `"Pull required"`, `"Pulling"` → update available; `"Skipped"`, `"already present"` → current. Plus `docker compose config --format json` to expand service-name → image when multiple services share an image (dry-run dedupes).
3. If not supported: for each service's image, run `docker image inspect <image> --format '{{index .RepoDigests 0}}'` for the local digest and `docker manifest inspect <image>` for the remote digest (extract `Descriptor.digest`). Compare. `manifest inspect` is a top-level docker command — bypass `command()`/`remoteCommand()`.

### Caching & refresh (TUI)

- `updateCache map[string]updateEntry` on Model, keyed by `projectDir + "|" + serverName`. `updateEntry{fetchedAt time.Time, results map[string]bool, err error}`. 10-minute TTL.
- `refreshUpdates(session)` `tea.Cmd` fires `updatesMsg{results map[string]bool, err error, session uint64}`.
- Triggered on entry to `screenSelectContainers` (alongside `refreshStatus`/`refreshStats`) when cache is missing or stale. Force-refresh: `U` key bypasses TTL.
- Stale-response: `updatesSession uint64` bumped at the same 7 sites as `statsSession`/`statusSession`; `updateInFlight bool` cleared unconditionally on session-matching arrival (before screen check), same reason as `refreshInFlight`.
- NOT wired into `refreshTickMsg` — cadence is TTL + explicit user action.

### Rendering

- TUI: `⇧` (U+21E7) in yellow (`lipgloss.Color("3")`) appended after service name with one leading space. `maxName += 2` when any service in the rendered list has `UpdateAvailable != nil && *UpdateAvailable == true`, so column alignment is stable. Footer adds `U updates` token. Error rendered in soft-warning slot, prefixed `updates: `, priority below `statsErr`.
- CLI text: same glyph, same alignment treatment, in `formatDots`/`formatDotsGrouped`. `--updates` flag gates multi-project mode.
- CLI JSON: `UpdateAvailable *bool` serialized as `update_available` with `omitempty`.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, doc updates inside this repo.
- **Post-Completion** (no checkboxes): manual verification with real registries (Docker Hub, private registries) — out of automated scope.

## Implementation Steps

### Task 1: Parse helpers and dry-run capability probe

**Files:**
- Create: `internal/compose/updates.go`
- Create: `internal/compose/updates_test.go`
- Create: `internal/compose/testdata/dryrun_*.txt` (captured fixtures)

- [x] **Pre-flight**: capture actual stderr output from `docker compose pull --dry-run --quiet` against a known Compose v2 version (e.g. v2.27+) into `internal/compose/testdata/dryrun_*.txt` fixtures (clean update, all-current, mixed, build-only). Use these fixtures as the source of truth for parser strings instead of inventing them. (Captured against Compose v2.40.3; fixtures: `dryrun_mixed.txt`, `dryrun_all_current.txt`, `dryrun_all_update.txt`, `dryrun_build_only.txt`. Confirmed: output is on stderr; the default `--policy=always` does NOT distinguish current images (everything → "Pulling"); only `--policy=missing` emits the "Skipped - Image is already present locally" verdict. Parser handles both; Tasks 2/3 will need to decide which policy to invoke for meaningful results.)
- [x] create `parseDryRunOutput(stderr string) map[string]bool` in `internal/compose/updates.go` — pure function. Unknown/unrecognised lines result in the service being **absent** from the map (tri-state preserved). Pin the Compose version targeted in a header comment so future changes are explicit.
- [x] create `detectDryRunFromHelp(help string) bool` — pure function, returns true when help text contains `--dry-run` flag mention
- [x] table-test `parseDryRunOutput` against the captured fixtures + synthetic edge cases (empty input, malformed lines, mixed-case)
- [x] table-test `detectDryRunFromHelp` against help-text samples with and without the flag
- [x] run `go test ./internal/compose/...` — must pass before Task 2

### Task 2: Local `Compose.CheckUpdates`

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/compose_test.go`

- [x] **Pre-flight**: verify whether `docker compose pull --dry-run --quiet` deduplicates output by image (i.e. omits services that share an image with another service already listed). If it does NOT dedupe, **skip** the `docker compose config --format json` expansion path entirely — it's dead weight. Document the observation in a code comment. (Observation: dry-run does NOT dedupe by image — each service produces its own verdict line even when sharing an image with another service in the same project. Therefore the dry-run path does NOT need a `compose config` expansion pass; the fallback path consumes `compose config --format json` independently for service→image mapping, which is a different concern. Documented in the `dryRunArgs` doc comment in `internal/compose/updates.go`.)
- [x] add `dryRunSupported bool` and `dryRunDetected bool` fields to `Compose`; add `SetTestHooks` extension for forcing either branch (added as `SetDryRunSupport(bool)` mirroring `SetStandalone` rather than overloading the existing two-argument `SetTestHooks` signature; the existing test hooks already expose `outputCmd` for argv capture, so no broader extension was needed)
- [x] implement `CheckUpdates(ctx, services) (map[string]bool, error)` on `Compose`: probe-if-needed; on dry-run path, parse output (and only run `compose config` expansion if pre-flight showed dedupe); fallback path bypasses `command()` and calls `docker image inspect` / `docker manifest inspect` directly (mirror `AllContainerStats` precedent). Dry-run argv is a package-internal `var` slice — do NOT export a helper. (Implemented in `internal/compose/updates.go`. Per pre-flight: `compose config` expansion is only used in the fallback path for service→image lookup; the dry-run path emits per-service lines directly. `--policy=missing` was chosen for the dry-run argv to extract any verdict signal — see code comment for the trade-off.)
- [x] write tests via `SetTestHooks` for: dry-run-supported path (capture argv, return mocked stderr, assert parsed map); fallback path (capture argv for `inspect`, return mocked digests, assert comparison); probe path (assert `docker compose pull --help` is called once, cached); error propagation (registry timeout returns error, partial maps still surfaced)
- [x] run `go test ./internal/compose/...` — must pass before Task 3

### Task 3: Remote `RemoteCompose.CheckUpdates`

**Files:**
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/remote_test.go`

- [ ] add `dryRunSupported`/`dryRunDetected` to `RemoteCompose`; extend `SetTestHooks` symmetrically
- [ ] implement `CheckUpdates` on `RemoteCompose`: dry-run path goes through `remoteCommand()` (regular compose subcommand); fallback path builds the SSH argv directly for `docker image inspect` and `docker manifest inspect` (top-level docker commands) — splice `SSHExtraArgs` immediately before the host arg, matching `AllContainerStatsRemote`
- [ ] assert in tests that probe runs `docker compose pull --help` over SSH and caches
- [ ] write tests covering: SSH argv shape for both branches; `SSHExtraArgs` spliced in the right slot for fallback; `ServerHost`/port handling matches existing tests; error propagation
- [ ] run `go test ./internal/compose/...` — must pass before Task 4

### Task 4: Runner interface + `ServiceStatus.UpdateAvailable`

**Files:**
- Modify: `internal/runner/runner.go`
- Modify: `internal/runner/runner_test.go`
- Modify: `internal/tui/app_test.go` (extend `mockComposer`)
- Modify: `cmd/deploy_test.go` (extend `opMockComposer`)

- [ ] add `UpdateAvailable *bool` to `runner.ServiceStatus`
- [ ] add `CheckUpdates(ctx context.Context, services []string) (map[string]bool, error)` to the `Composer` interface
- [ ] **update ALL existing mock `Composer` implementations** with a no-op `CheckUpdates` (returns `nil, nil`): the in-package runner mock, `mockComposer` in `internal/tui/app_test.go`, and `opMockComposer` in `cmd/deploy_test.go`. Search for other implementers with `grep -rn "func.*Composer.*ListServices" ./...` to be exhaustive — adding to the interface without updating all mocks breaks every dependent test package.
- [ ] add a compile-time interface assertion `var _ runner.Composer = (*compose.Compose)(nil)` (and the remote variant) in a test file if not already present
- [ ] run `go build ./... && go test ./...` — interface must compile cleanly across all packages before Task 5

### Task 5: TUI state, refresh wiring, and message handling

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] add Model fields: `updateCache map[string]updateEntry`, `updatesSession uint64`, `updateInFlight bool`, `updatesErr string`; define `updateEntry{fetchedAt time.Time, results map[string]bool, err error}` and message type `updatesMsg{results map[string]bool, err error, session uint64}`. Cache key is `projectDir + "|" + serverName` (empty `serverName` means local — document in a comment).
- [ ] implement `refreshUpdates(session) tea.Cmd` that calls `m.composer.CheckUpdates(...)`; implement `updatesMsg` handler that hydrates `UpdateAvailable` onto `m.services`, writes to cache, clears `updateInFlight` *before* the screen-gate check (rejection happens after the clear)
- [ ] trigger `refreshUpdates` on entry to `screenSelectContainers` (alongside `refreshStatus`/`refreshStats`) when cache is missing or stale; bump `updatesSession` at the **exact same 7 sites that bump `statsSession`** (verify against `grep -n "statsSession++" internal/tui/app.go`): project pick, esc container→proj, esc proj→server, entryLocal, execDone, return from `screenProgress`, return from `screenLogs`. **Do NOT** bump at `connectResultMsg` error path — that site bumps `projectsSession`, not stats/status.
- [ ] also reset `updateInFlight = false` at the 2 leave-screen transitions (esc container→proj, esc proj→server) alongside the session bumps, mirroring `refreshInFlight` cleanup — prevents the flag getting stuck across context changes that bump the session without an in-flight response.
- [ ] add `U` (uppercase) keypress handler on `screenSelectContainers`: bypass TTL, set `updateInFlight=true`, fire `refreshUpdates`. (No transient `"checking updates…"` indicator — matches `refreshStats` UX where the stats cells just blank until results arrive.)
- [ ] write TUI tests: cache hit hydrates without fetch; cache miss/stale triggers fetch; stale session is rejected; `updateInFlight` clears on success and error including off-screen arrivals; `updateInFlight` resets at the 2 leave-screen transitions; `U` keypress forces refresh; `updatesSession` bumps at every documented site; use `installFakeTick` seam where needed
- [ ] run `go build ./... && go test ./...` — must pass before Task 6

### Task 6: TUI rendering

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/styles.go`
- Modify: `internal/tui/app_test.go`

- [ ] add a yellow-glyph style in `styles.go` (`updateGlyphStyle` with `lipgloss.Color("3")`); export the glyph rune as a const (`updateGlyph = "⇧"`) for test-side reuse
- [ ] in the container-screen renderer, append `" ⇧"` after the service name when `*s.UpdateAvailable == true`; extend `maxName` computation to add 2 cells when *any* service in the rendered list has the flag (preserves alignment whether updates land mid-poll or not); show on stopped services too
- [ ] add `U updates` token to the help footer on `screenSelectContainers`; render `updatesErr` in the soft-warning slot prefixed `"updates: "`, below `statsErr`, only shown when `svcErr` is empty (priority: `svcErr > statsErr > updatesErr`)
- [ ] write tests asserting: glyph appears in `View()` output when flag is true; `maxName` reservation kicks in (column alignment preserved on a 3-service list where one has updates); footer contains `U updates` on the right screen; soft-warning priority order
- [ ] run `go test ./internal/tui/...` — must pass before Task 7

### Task 7: CLI list rendering (text + JSON + flag)

**Files:**
- Modify: `cmd/list.go`
- Modify: `cmd/list_test.go`

- [ ] add `--updates` bool flag to `listCmd`; in `runList`, gate multi-project update checks behind it. Single-project mode (`-C` specified) calls `composer.CheckUpdates(...)` unconditionally
- [ ] extend `formatDots` / `formatDotsGrouped` to render the glyph inline after the service name with the same +2-cell alignment reservation as the TUI; treat empty/absent flag as blank
- [ ] add `UpdateAvailable *bool` (json tag `update_available,omitempty`) to the per-service JSON output struct
- [ ] soft-fail: if `CheckUpdates` errors, write `cdeploy: updates unavailable: <err>` to stderr, exit 0, leave cells blank — do not abort the listing. (This phrasing intentionally mirrors the existing `cdeploy: stats unavailable: <err>` precedent in `cmd/list.go`; do not deviate.)
- [ ] write tests for: `--updates` flag registration on `listCmd`; JSON output includes `update_available` when set (and omits when nil); text output contains the glyph when set; soft-failure path produces stderr warning but exits 0
- [ ] run `go test ./cmd/...` — must pass before Task 8

### Task 8: Verify acceptance criteria

- [ ] verify Overview goals: TUI shows `⇧` next to services with available updates; CLI `list` shows the same glyph in text and `update_available` in JSON; `U` force-refreshes in TUI; `--updates` opts in for CLI multi-project
- [ ] verify edge cases: build-only services show no glyph; stopped services with updates still show glyph; soft-failure preserves status view; cache TTL behavior is correct; session-rejection drops stale messages without panic
- [ ] run full test suite: `go test ./... -count=1`
- [ ] run `go vet ./...` and `go build ./...`
- [ ] verify backward compatibility: every existing test still passes; existing CLI invocations behave identically when `--updates` is omitted in multi-project mode

### Task 9: Documentation and plan completion

**Files:**
- Modify: `CLAUDE.md`
- Move: this plan to `docs/plans/completed/`

- [ ] update CLAUDE.md: add a paragraph under the existing pattern documentation summarising the update-detection mechanism (where `CheckUpdates` lives, why it bypasses `command()` for `manifest inspect`, cache TTL, session counter)
- [ ] update CLAUDE.md "Adding New Operations" / "Status refresh" sections if any new conventions emerged
- [ ] `mkdir -p docs/plans/completed && mv docs/plans/20260517-update-available-indicators.md docs/plans/completed/`
- [ ] final `go test ./... -count=1` and `go vet ./...` to confirm green
- [ ] commit when user requests

## Post-Completion

*Items requiring manual intervention — no checkboxes, informational only.*

**Manual verification with real registries:**
- Docker Hub public image (pull a tag, retag it to look stale, verify glyph appears).
- Private registry with authenticated daemon (verify auth flows through `pull --dry-run` and `manifest inspect`).
- Multi-arch image with platform mismatch (verify behavior is sane — likely shows update; document limitation).
- Build-only service (compose file with `build:` only, no `image:`) — verify glyph stays absent.
- Rate-limit pressure: 20+ services, run with `--updates` against Docker Hub anonymous; verify soft-failure path.

**Remote (SSH) verification:**
- Run TUI against a remote host; verify `U` triggers a refresh that completes; verify cache TTL behavior across server switches.
- Verify `SSHExtraArgs` (e.g., `-i identity`) is respected in the `manifest inspect` fallback path.

**Compose version coverage:**
- Pre-v2.22 daemon (fallback path) — verify behavior matches dry-run path output.
- v2.22+ daemon (dry-run path) — primary path.

**Future considerations (deferred, not in scope for this plan):**
- Persistent cache across cdeploy invocations.
- Notification daemon / background polling.
- "Update all" runner operation that deploys only services with updates.
- Per-image last-pushed timestamp display.

# Container CPU and Memory Stats

## Overview

Add per-service CPU and memory usage to `cdeploy list` (CLI) and the TUI container select screen. Surfaces the two questions users actually ask when triaging a host: "is this hot?" and "did I size it right?".

- **CLI**: opt-in via `--stats` flag (default off) so scripts and CI pay no latency penalty.
- **TUI**: auto-fetched in parallel with status on screen entry — screen renders immediately and stats fill in asynchronously.
- **Single host-wide `docker stats --no-stream --format json` call**, joined to compose results by container ID. One ~1.5s cost regardless of project count.
- **Soft-fail**: if stats fetch errors, render blank cells and surface a small warning. Status remains the load-bearing primary view.

## Context (from discovery)

Files involved:
- `internal/runner/runner.go` — `Composer` interface, `ServiceStatus` struct; add `ServiceStats` struct and `ContainerStats` method.
- `internal/compose/compose.go` — local `Compose`, `command()`, `SetTestHooks`; add `ContainerStats()` method.
- `internal/compose/remote.go` — `RemoteCompose`, `remoteCommand()`, `SSHExtraArgs` splice points; add `ContainerStats()` method.
- `internal/compose/stats.go` (new) — `AllContainerStats`, `AllContainerStatsRemote`, `parseStatsOutput`, `parseSize`, `parseCPUPercent`.
- `internal/compose/uptime.go`, `internal/compose/ports.go` — pattern references for compact-string parsing helpers.
- `cmd/list.go` — `list` subcommand, multi-project discovery, JSON output, `formatDots`/`formatDotsGrouped`; add `--stats` flag and stats column wiring.
- `internal/tui/app.go` — Model, `refreshStatus()`, `statusMsg`/`servicesMsg`, `hasStatusColumns()`; add `stats` field, `statsMsg`, `refreshStats()`.
- `CLAUDE.md` — architecture docs; update Docker Compose method list and add Resource stats subsection.

Related patterns:
- `parseContainerStatus()` in `internal/compose/compose.go` — JSON-array-vs-NDJSON tolerant parser; `parseStatsOutput` follows the same shape.
- `formatUptime()` / `parseUptimeDuration()` in `internal/compose/uptime.go` — compact-string round-trip; same approach for `parseSize`.
- `ContainerStatus`/`statusMsg` flow in TUI — the new stats path mirrors it (fetch → message → field on Model → render).
- `maxPorts` width tracking in `formatDots`/`formatDotsGrouped` — same pattern for `maxCPU`/`maxMem`.
- `SetTestHooks(run, output)` on both `Compose` and `RemoteCompose` — every new command call goes through these.

Dependencies: stdlib only (existing convention). No new third-party packages.

## Development Approach

- **Testing approach**: Regular (write code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **Every task includes new/updated tests** — tests are a required deliverable, not optional
- **All tests must pass before starting the next task** — no exceptions
- Run `go test ./...` after each change
- Maintain backward compatibility: `Composer` interface gains a method (mock additions needed), but no existing signatures change; CLI without `--stats` produces byte-identical output to today; JSON wire shape unchanged when stats fields are absent (`omitempty`).
- Update this plan file when scope changes during implementation

## Testing Strategy

- **Unit tests**: required for every task — `internal/compose/stats_test.go` (parsing, aggregation), `internal/compose/remote_test.go` (SSH argv), `internal/runner/runner_test.go` (mock no-op), `internal/tui/app_test.go` (message handling, rendering, stale guard), `cmd/list_test.go` (flag, JSON shape).
- **No E2E tests**: cdeploy uses stdlib `testing` only; no Playwright/Cypress. Existing TUI tests work by calling `Update()` with `tea.KeyMsg`/`tea.Msg` directly — the new tests follow that pattern.
- **No docker invocation in tests**: existing convention — test command construction & parsing, not Docker itself. `SetTestHooks` injects canned output.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## Solution Overview

Two-layer architecture in `internal/compose`:

1. **Host-wide bulk helper** (`AllContainerStats` / `AllContainerStatsRemote`): single `docker stats --no-stream --format json` call returning `map[containerID]ServiceStats`. Used directly by the multi-project CLI path so N projects share one stats call.
2. **Per-project `ContainerStats()`** on `Compose` and `RemoteCompose`: fetches this project's container IDs via the existing `docker compose ps`, calls the bulk helper, joins by container ID, sum-aggregates by service name. Used by the TUI and the single-project CLI path.

Runner interface gains `ContainerStats(ctx) (map[string]ServiceStats, error)` and a new `ServiceStats` struct (`CPUPercent float64`, `MemoryUsed int64`, `MemoryLimit int64`).

TUI fires `refreshStatus()` and `refreshStats()` in parallel via `tea.Batch`; the two messages arrive independently and the render path uses whatever is currently in the model. Stale-message guard checks `m.screen == screenSelectContainers` before applying.

CLI `list --stats` is the only entry point that pays the stats cost. Without `--stats`, every existing code path runs exactly as today.

### Key design decisions

1. **Separate method, not a parameter on `ContainerStatus`** — stats are slow and optional; status is fast and load-bearing. Keeping them split means deploy/restart pipelines and `cdeploy list` (without `--stats`) pay nothing.
2. **Bulk host-wide call** — `docker stats` cost is dominated by its sampling window, not per-container work. One call services every project on the host.
3. **TUI auto-on / CLI opt-in** — interactive sessions absorb the latency invisibly; scripts and CI cannot.
4. **Sum aggregation for scaled services** — total resource use is what users budget against. Matches "how much is this service costing me" intuition.
5. **Soft failure** — blank cells + small note. Stats are a secondary view; a flaky `docker stats` should not lose the rest.

## Technical Details

### New types (`internal/runner/runner.go`)

```go
type ServiceStats struct {
    CPUPercent  float64 // 100.0 = 1 full core; can exceed 100 for scaled services
    MemoryUsed  int64   // bytes
    MemoryLimit int64   // bytes; whatever Docker reports (often host memory if no explicit limit)
}
```

### New interface method (`runner.Composer`)

```go
// ContainerStats returns a map of service name to ServiceStats.
// Only running containers are included; stopped services are absent.
// Scaled services: CPU%, MemoryUsed, and MemoryLimit are all summed across replicas.
ContainerStats(ctx context.Context) (map[string]ServiceStats, error)
```

### `docker stats` output parsing

`docker stats --no-stream --format json` emits NDJSON, one line per container, with fields including `ID` (short), `Name`, `CPUPerc` (`"4.20%"`), `MemUsage` (`"124MiB / 512MiB"`). `parseStatsOutput` accepts both NDJSON and the rarer JSON-array form (mirrors `parseContainerStatus` tolerance).

`parseSize` handles `B`, `KiB`/`MB`, `MiB`/`MB`, `GiB`/`GB`, `TiB`/`TB`; both binary (1024-based, IEC) and decimal (1000-based, SI) suffixes — Docker switches between them depending on engine settings.

`parseCPUPercent` strips the trailing `%` and parses as float; empty string returns 0; malformed returns error.

### Column placement

Both TUI container screen and CLI `list` table: `● name  health  Created  Uptime  CPU  Mem  Ports`. The CPU/Mem block sits between time and network, grouping "resource" columns together.

### CLI flag and JSON shape

```
cdeploy list --stats
cdeploy list --stats --json
```

JSON additive fields (all `omitempty`):

```json
{"service":"api","running":true,"cpu_percent":4.2,"memory_used":130023424,"memory_limit":536870912}
```

When `--stats` is absent, the three fields are omitted entirely — wire shape unchanged.

### TUI message flow

```
enterContainerScreen() → tea.Batch(refreshStatus(), refreshStats())
                                          ↓                ↓
                                    statusMsg         statsMsg
                                          ↓                ↓
                                  m.servicesStatus    m.stats
                                          ↓                ↓
                                       View() renders both columns
```

Stats latency (~1.5s) is hidden by status (which arrives first); the screen feels responsive and metrics fill in as a second wave.

### Aggregation

`ContainerStats()` aggregates per service name by **summing** all three fields across replicas. A 3-replica service maxing each core shows `~300.0%` CPU. Same source data (`docker stats`) but joined via `docker compose ps`'s ID → service mapping.

### Multi-project CLI path

In `cmd/list.go`, when `--stats` is set and no `-C` is given, the bulk `AllContainerStats` call happens **once** before the project discovery loop. Each per-project row-emit joins against the pre-fetched map. Per-project errors stay non-fatal (existing convention).

## What Goes Where

- **Implementation Steps** (checkboxes): code, tests, doc updates in this repo.
- **Post-Completion** (no checkboxes): manual verification on a real Docker host with scaled services.

## Implementation Steps

### Task 1: Add `ServiceStats` type and `ContainerStats` interface method

**Files:**
- Modify: `internal/runner/runner.go`
- Modify: `internal/runner/runner_test.go`

- [x] add `ServiceStats` struct to `internal/runner/runner.go` (fields: `CPUPercent float64`, `MemoryUsed int64`, `MemoryLimit int64`) with doc comment covering scaled-service summation contract
- [x] add `ContainerStats(ctx context.Context) (map[string]ServiceStats, error)` to `Composer` interface with doc comment
- [x] update the test mock `Composer` in `internal/runner/runner_test.go` to include a no-op `ContainerStats` returning `(nil, nil)`
- [x] write a test asserting the mock's `ContainerStats` returns nil-nil (sanity check the interface is satisfied)
- [x] run `go test ./internal/runner/...` — must pass before next task

### Task 2: Implement `parseStatsOutput`, `parseSize`, `parseCPUPercent`, `formatBytes`

**Files:**
- Create: `internal/compose/stats.go`
- Create: `internal/compose/stats_test.go`

- [x] create `internal/compose/stats.go` with `parseStatsOutput([]byte) (map[string]runner.ServiceStats, error)` — keyed by container ID (short form); tolerant of both NDJSON and JSON array
- [x] implement `parseSize(string) (int64, error)` handling `B`, `KiB`/`KB`/`kB`, `MiB`/`MB`, `GiB`/`GB`, `TiB`/`TB` (binary + decimal suffixes); case-insensitive on the unit letter
- [x] implement `parseCPUPercent(string) (float64, error)` — strip `%`, parse float, empty → 0, malformed → error
- [x] implement `formatBytes(int64) string` — inverse of `parseSize`, produces compact `"124M"` / `"1.5G"` / `"512K"` / `"0B"` (single-letter suffix, no `i`, rounded). Exported because both `cmd/list.go` and `internal/tui/app.go` import it; placing it here (next to `parseSize`) prevents duplicate helpers in both packages.
- [x] write `TestParseSize` table-driven: `"124MiB"`→130023424, `"1.5GiB"`→1610612736, `"512B"`→512, `"0B"`→0, `"1.5GB"`→1500000000, `"100kB"`→100000, `"100KB"`→100000, malformed → error
- [x] write `TestParseCPUPercent` table-driven: `"4.20%"`→4.2, `"0.00%"`→0, `""`→0, garbage → error
- [x] write `TestFormatBytes` table-driven: 0→`"0B"`, 512→`"512B"`, 130023424→`"124M"`, 1610612736→`"1.5G"`, boundary checks at 1024 and 1024² and 1024³
- [x] write `TestParseStatsOutput` with canned NDJSON containing 3 containers (mixed units, varied CPU%) — verify map shape, ID keys, and parsed values
- [x] write `TestParseStatsOutput_JSONArray` with array-form input — verify same result as NDJSON form
- [x] write `TestParseStatsOutput_Empty` — empty input returns empty map, no error
- [x] write `TestParseStatsOutput_Malformed` — malformed line in NDJSON returns error
- [x] run `go test ./internal/compose/...` — must pass before next task
- ➕ added stub `ContainerStats` methods on `Compose` and `RemoteCompose` (returning empty map) to keep the `runner.Composer` compile-time check passing; replaced with full implementation in Task 5

### Task 3: Implement `AllContainerStats` (local) — bypassing `command()`

**Files:**
- Modify: `internal/compose/stats.go`
- Modify: `internal/compose/compose.go` (only if needed for test hook exposure)
- Modify: `internal/compose/stats_test.go`

**Important — bypass `command()`:** `docker stats` is a top-level Docker CLI command, **not** a compose subcommand. `c.command(...)` prepends `compose` (or switches to `docker-compose` in standalone mode) which would produce a malformed argv. Build the `exec.Cmd` directly via `exec.CommandContext("docker", "stats", "--no-stream", "--format", "json")`. This is the first method on `Compose` to bypass `command()` — `EditCommand`/`ExecCommand` already bypass `remoteCommand()` for related reasons (they also need terminal access). Document this exception in the `AllContainerStats` doc comment.

- [x] add `AllContainerStats(ctx context.Context, c *Compose) (map[string]runner.ServiceStats, error)` in `internal/compose/stats.go`
- [x] build the command directly: `exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "json")` — do NOT call `c.command(...)`
- [x] execute via the existing `runOutput` test hook on `Compose` (the hook is consulted before exec, so this still honors `SetTestHooks`)
- [x] add doc comment explicitly noting "bypasses `command()` because `docker stats` is not a compose subcommand"
- [x] parse the result via `parseStatsOutput`
- [x] write `TestAllContainerStats_local_argConstruction` — inject test hook, capture argv, assert exact shape: `["docker", "stats", "--no-stream", "--format", "json"]` (no `compose` element)
- [x] write `TestAllContainerStats_local_standaloneMode_unchanged` — set `c.Standalone = true`, verify argv is still the same (does NOT become `docker-compose stats`)
- [x] write `TestAllContainerStats_local_parsing` — inject test hook returning canned NDJSON, assert returned map matches expected
- [x] write `TestAllContainerStats_local_error` — inject test hook returning error, assert error propagated
- [x] run `go test ./internal/compose/...` — must pass before next task

### Task 4: Implement `AllContainerStatsRemote` — bypassing `remoteCommand()`, splicing `SSHExtraArgs`

**Files:**
- Modify: `internal/compose/stats.go`
- Modify: `internal/compose/remote_test.go`

**Important — bypass `remoteCommand()`:** Same reason as Task 3 — `remoteCommand()` builds compose-flavored argv (`docker compose ...`). Build the SSH argv directly, mirroring the pattern that `EditCommand` and `ExecCommand` already use in `remote.go`. Splice `SSHExtraArgs` **immediately before the host argument**, which is the convention documented in CLAUDE.md for every SSH argv site (`Connect`, `Close`, `Detect` probes, `remoteCommand`, `findRemoteComposeFile`, `ConfigFile`, `EditCommand`, `ExecCommand`).

- [x] add `AllContainerStatsRemote(ctx context.Context, rc *RemoteCompose) (map[string]runner.ServiceStats, error)`
- [x] build the SSH argv directly: `ssh` + ControlMaster args + port args (if set) + `SSHExtraArgs` + host + remote command `docker stats --no-stream --format json` — do NOT call `rc.remoteCommand(...)`
- [x] route through the existing test hook on `RemoteCompose`
- [x] add doc comment noting "bypasses `remoteCommand()` because `docker stats` is not a compose subcommand; follows the `EditCommand`/`ExecCommand` precedent"
- [x] parse the result via `parseStatsOutput`
- [x] write `TestAllContainerStatsRemote_argConstruction` — verify SSH argv shape ends with `..., "user@host", "docker stats --no-stream --format json"`; assert `compose` does not appear anywhere in argv
- [x] write `TestAllContainerStatsRemote_extraArgsSplice` — set `rc.SSHExtraArgs = []string{"-i", "/tmp/key"}`, verify they appear immediately before the host arg (not after, not at the end)
- [x] write `TestAllContainerStatsRemote_portArgs` — set port on the underlying SSH target, verify `-p NNNN` precedes `SSHExtraArgs`, both precede host
- [x] write `TestAllContainerStatsRemote_parsing` — inject test hook with canned NDJSON, assert correct map
- [x] run `go test ./internal/compose/...` — must pass before next task

### Task 5: Implement `ContainerStats()` method on `Compose` and `RemoteCompose`

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/stats_test.go`

- [x] add `(c *Compose) ContainerStats(ctx context.Context) (map[string]runner.ServiceStats, error)` — calls `docker compose ps --format json` for this project's container IDs, calls `AllContainerStats(ctx, c)`, joins by container ID, sum-aggregates per service name, returns the result
- [x] add identical-shape `(rc *RemoteCompose) ContainerStats(...)` method using `AllContainerStatsRemote`
- [x] aggregation logic: build `map[serviceName]ServiceStats` by iterating compose `ps` entries; for each entry, look up by container ID in the stats map; if present, add CPU%/MemoryUsed/MemoryLimit to that service's running totals
- [x] write `TestContainerStats_local_singleReplica` — one service, one container, verify pass-through values
- [x] write `TestContainerStats_local_scaledService` — one service, 3 containers, verify all three fields summed (e.g. 3× 50% CPU, 100MiB/512MiB → 150% CPU, 300MiB/1536MiB)
- [x] write `TestContainerStats_local_stoppedServicesAbsent` — service in `ps` but absent from stats (stopped) → not in returned map
- [x] write `TestContainerStats_local_psFailureReturnsError` — `ps` fails → method returns error (do not silently swallow)
- [x] write `TestContainerStats_local_statsFailureReturnsError` — `ps` succeeds but stats fails → method returns error (caller decides soft-fail)
- [x] write `TestContainerStats_local_psIDAbsentFromStats` — `ps` returns a container ID that does not appear in the stats map (race window: container stopped between calls) → that service is simply absent from the result map, no error
- [x] write `TestContainerStats_remote_passthrough` — verify remote variant joins the same way (one test sufficient; SSH argv already covered in Task 4)
- [x] run `go test ./internal/compose/...` — must pass before next task
- ➕ added `ID` field to `psEntry` struct (required for container-ID join against `docker stats`); added helpers `parsePsIDToService`, `aggregateStatsByService`, and `shortContainerID` in `internal/compose/stats.go`; added bonus `TestContainerStats_local_shortIDJoin` to verify 64-char full IDs from `ps` correctly truncate for join against 12-char short IDs from `docker stats`. The pre-existing `mockComposer` build failures in `internal/tui` and `cmd` packages (introduced by the interface-method addition in Task 1, deferred to Task 6) remain unchanged — `go test ./internal/compose/...` passes (Task 5's required gate); the wider `go test ./...` continues to fail until Task 6 stubs the mocks.

### Task 6: Add no-op `ContainerStats` to all other mock `Composer` implementations

**Files:**
- Modify: every file containing a `Composer` mock (enumerated below)

- [x] grep the repo for `Composer` implementers: `grep -rn "ListServices(ctx" --include="*_test.go"` and `grep -rn "ContainerStatus(ctx" --include="*_test.go"`
- [x] **enumerate every mock site discovered as a `➕` discovered item appended to this task** before editing — this makes completeness auditable in the diff. Example: `➕ Mock found at internal/tui/app_test.go:42 (mockComposer)`. List all sites this way before adding any code.
- [x] add a no-op `ContainerStats(ctx context.Context) (map[string]runner.ServiceStats, error) { return nil, nil }` method to each enumerated mock
- [x] ensure `go build ./...` succeeds — every implementer must satisfy the interface
- [x] run `go test ./...` — full suite must pass before next task (this confirms no existing test breaks from the interface addition)
- ➕ Mock found at `internal/tui/app_test.go:27` (`mockComposer`) — `mockConfigComposer` and `mockExecComposer` in the same file embed it, so the method is inherited
- ➕ Mock found at `cmd/deploy_test.go:239` (`opMockComposer`)
- ➕ Mock found at `cmd/list_test.go:588` (`mockComposer`) — `mockLogsComposer` in `cmd/logs_test.go:39` embeds it, so the method is inherited
- ➕ Mock found at `cmd/list_test.go:883` (`mockComposerStatusErr`)
- ➕ Mock at `internal/runner/runner_test.go:11` (`mockComposer`) already had `ContainerStats` added in Task 1 — no change required

### Task 7: Wire `--stats` flag and JSON fields into CLI `list`

**Files:**
- Modify: `cmd/list.go`
- Modify: `cmd/list_test.go`

- [ ] add `--stats` bool flag (default false) on `listCmd`, registered alongside existing flags
- [ ] in the multi-project code path: when `--stats` is set, call `AllContainerStats(ctx, c)` or `AllContainerStatsRemote(ctx, rc)` **once** before the project loop, passing the resulting map into each project's row-emit
- [ ] in the single-project (`-C`) code path: when `--stats` is set, call `ContainerStats()` on the active composer
- [ ] **soft-fail in both paths**: on stats fetch error (multi-project bulk fetch OR single-project `ContainerStats`), print `cdeploy: stats unavailable: <err>` to stderr, render blank stats cells, exit code 0. The `list` command itself never fails because of stats — status is the load-bearing primary view.
- [ ] add CPU/Mem to the JSON output struct with `omitempty` tags: `cpu_percent`, `memory_used`, `memory_limit` — these stay zero/absent unless `--stats` was passed
- [ ] update `formatDots` / `formatDotsGrouped` to compute `maxCPU` and `maxMem` widths alongside `maxPorts`, and render the two columns between Uptime and Ports when any service has non-zero stats data
- [ ] format CPU as `"4.2%"` (one decimal) and memory as `"124M/512M"` using `compose.FormatBytes` (defined in Task 2 in `internal/compose/stats.go`) — single source of truth; do NOT define a duplicate helper in `cmd/list.go` or `internal/tui/`
- [ ] stopped containers render blank CPU/Mem cells (padded whitespace) — matches existing Uptime/Ports convention
- [ ] write `TestListCmd_statsFlagRegistration` — flag registered, default false, accepts `--stats`
- [ ] write `TestListJSON_omitsStatsFieldsWithoutFlag` — JSON output without `--stats` contains no `cpu_percent`/`memory_used`/`memory_limit` keys
- [ ] write `TestListJSON_includesStatsFieldsWithFlag` — with `--stats`, JSON output contains all three keys (use injected stats map)
- [ ] write `TestListCmd_singleProjectStatsFailure` — `-C` path with `--stats`, inject `ContainerStats` failure; verify stderr warning, exit 0, blank cells
- [ ] write `TestListCmd_multiProjectStatsFailure` — multi-project with `--stats`, inject bulk-fetch failure; verify stderr warning, exit 0, blank cells, projects still listed
- [ ] run `go test ./cmd/...` — must pass before next task

### Task 8: Wire `stats` field, `statsMsg`, and `refreshStats()` into TUI

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] add `stats map[string]runner.ServiceStats` and `statsErr error` fields to `Model`
- [ ] add `statsMsg{stats map[string]runner.ServiceStats; err error}` type
- [ ] add `refreshStats() tea.Cmd` that calls the active composer's `ContainerStats(ctx)` and returns a `statsMsg`
- [ ] update every site that currently invokes `refreshStatus()` to invoke `tea.Batch(refreshStatus(), refreshStats())` — entry to `screenSelectContainers`, return from progress/logs/config/exec screens
- [ ] handle `statsMsg` in `Update`: if `m.screen != screenSelectContainers`, ignore (stale guard); on error, store `statsErr`, keep `m.stats` as-is; on success, replace `m.stats` and clear `m.statsErr`
- [ ] clear `m.stats` and `m.statsErr` in the `esc` cleanup paths that already clear `servicesStatus` (matches existing cleanup discipline)
- [ ] write `TestStatsMsg_populates` — send `statsMsg` with a map, assert `m.stats` populated, `m.statsErr` nil
- [ ] write `TestStatsMsg_storesError` — send `statsMsg` with error, assert `m.statsErr` set, `m.stats` unchanged
- [ ] write `TestStatsMsg_staleIgnored` — set `m.screen = screenSelectServer`, send `statsMsg`, assert no state mutation
- [ ] write `TestEsc_clearsStats` — populate `m.stats`, send `esc`, assert cleared alongside other state
- [ ] run `go test ./internal/tui/...` — must pass before next task

### Task 9: Render CPU/Mem columns on the TUI container screen

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] update `hasStatusColumns()` to return true when any service has stats data, alongside the existing Created/Uptime checks
- [ ] update the column captions row to include `CPU` and `Mem` headers when stats are present (preserving lipgloss alignment with the existing 10+maxName left-pad)
- [ ] update the per-row render to emit CPU and Mem cells between Uptime and Ports — use `compose.FormatBytes` (single source of truth, defined in Task 2). Import from `internal/compose`; do not duplicate in `internal/tui/`.
- [ ] stopped or stats-missing services render blank padded cells (same convention as Ports for stopped containers)
- [ ] `statsErr` rendered in the same slot as `svcErr`; if both are set, prefer `svcErr` (the more important failure)
- [ ] write `TestContainerScreen_rendersStatsColumns` — populate `m.stats` with two services, call `View()`, assert the output contains the CPU% and Mem strings in the expected order
- [ ] write `TestContainerScreen_blankCellsForMissingStats` — `m.stats` empty, `View()` renders without panic, captions row absent (or columns blank)
- [ ] write `TestContainerScreen_statsErrFallback` — set `m.statsErr`, `m.svcErr` nil, `View()` includes the stats error string
- [ ] write `TestContainerScreen_svcErrPreferred` — both errors set, `View()` shows `svcErr` not `statsErr`
- [ ] write `TestSvcVisibleCount_withStatsColumns` — verify `svcVisibleCount()` math is unchanged when the captions row contains the new CPU/Mem headers (captions row presence is binary, so the header line count remains the same as when only Created/Uptime were present)
- [ ] run `go test ./internal/tui/...` — must pass before next task

### Task 10: Verify acceptance criteria

- [ ] verify `cdeploy list` without `--stats` is byte-identical to today (text output diff + JSON diff)
- [ ] verify `cdeploy list --stats` shows CPU/Mem columns and includes the three JSON fields
- [ ] verify the TUI container screen displays CPU/Mem and they refresh on screen entry / return-from-progress
- [ ] verify scaled services aggregate as sum (manual: scale a service to 3, observe CPU% ~= 3× single replica)
- [ ] verify soft-failure: simulate stats error (e.g. invalid hook), confirm blank cells + stderr warning in CLI, `statsErr` rendered in TUI
- [ ] verify SSH path: `AllContainerStatsRemote` works against a real or mocked SSH session
- [ ] run full test suite: `go test ./...`
- [ ] run `go test ./... -count=1` (uncached) to confirm
- [ ] run `go build -o cdeploy .` — must succeed
- [ ] run `go mod tidy` to confirm no stray imports

### Task 11: Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md` (only if it documents the `list` command's flags)

- [ ] update `CLAUDE.md` "Docker Compose" section: add `ContainerStats` to the method list, add `stats` to the subcommand list, note `docker stats --no-stream --format json` is the data source, and note that `ContainerStats` is the **first method to bypass `command()` / `remoteCommand()`** because `docker stats` is a top-level Docker CLI command rather than a compose subcommand (along with `EditCommand`/`ExecCommand` which already bypass `remoteCommand()` for SSH/TTY reasons)
- [ ] add a new "Resource stats" subsection under TUI architecture: parallel fetch via `tea.Batch(refreshStatus(), refreshStats())`, stale guard checks `m.screen == screenSelectContainers`, blank cells for stopped/missing, error rendering precedence (svcErr wins over statsErr)
- [ ] update "Multi-project discovery" section: mention bulk `AllContainerStats` join via container ID — one ~1.5s cost regardless of project count when `--stats` is set
- [ ] update "Testing Approach" `internal/compose/` line: mention `parseStatsOutput`/`parseSize`/`parseCPUPercent`/`FormatBytes` are extracted for testability
- [ ] if `README.md` documents `list` flags, add a one-line description of `--stats`
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification on a real Docker host:**
- Test against a project with a scaled service (`deploy.replicas: 3` or `docker compose up --scale svc=3`) to confirm sum aggregation visibly works.
- Test on a host with 4+ compose projects to confirm the bulk-stats path keeps `cdeploy list --stats` under ~2s rather than ~6s.
- Test over SSH to a remote host, including with `--ssh user@host -i /path/to/key` to confirm `SSHExtraArgs` splicing works for stats.
- Test with Docker engine versions emitting both binary (`MiB`) and decimal (`MB`) memory suffixes — both should parse.
- Test against a service with no memory limit set — `MemoryLimit` will equal host memory; confirm display is acceptable. If ugly, revisit the punted no-limit heuristic.

**External / future considerations** (intentional YAGNI, do not implement now):
- Periodic refresh / live ticker in TUI
- Manual `R` refresh key
- Net I/O, Block I/O, PIDs columns
- "No-limit" memory display heuristic
- Per-replica drill-down view
- Configurable stats columns
- Caching stats across screen entries within the same session

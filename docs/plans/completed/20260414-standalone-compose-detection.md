# Auto-detect standalone docker-compose binary

## Overview
- Add auto-detection for standalone `docker-compose` binary vs `docker compose` plugin subcommand
- Solves: servers with Docker installed but Compose only available as standalone binary fail with cryptic errors (`exit status 125: unknown shorthand flag`)
- Both local and remote usage supported — detection probes once and caches the result
- Zero configuration required — just works regardless of which compose variant is installed

## Context (from discovery)
- Files/components involved:
  - `internal/compose/compose.go` — local `Compose` struct, `command()` choke point, package-level `ListProjects`
  - `internal/compose/remote.go` — `RemoteCompose` struct, `remoteCommand()` choke point
  - `cmd/root.go` — TUI wiring (factory, loader, connectCb)
  - `cmd/deploy.go`, `cmd/list.go`, `cmd/logs.go` — CLI subcommands creating composers
- Related patterns: `SetTestHooks(runCmd, outputCmd)` for test injection, `var opNewRemote = compose.NewRemote` for test overrides
- Dependencies: both `Compose` and `RemoteCompose` funnel all commands through `command()` / `remoteCommand()` — single choke point per variant

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility

## Testing Strategy
- **Unit tests**: required for every task
- Detection logic tested via `outputCmd` hook (local `Compose`) and `SetTestHooks` (remote `RemoteCompose`) to mock `version` command responses
- Command building tested by inspecting `exec.Cmd.Args` (local) and SSH remote command string (remote)
- Existing tests continue to pass without changes — detection is explicit, never lazy, so tests that never call `Detect()` default to `Standalone=false` (plugin mode, current behavior)
- Tests that need to exercise standalone mode use `SetStandalone(true)` helper

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with + prefix
- Document issues/blockers with warning prefix
- Update plan if implementation deviates from original scope

## Solution Overview
- Add `Standalone bool` (exported, consistent with `ProjectDir`/`UID`) and `detected bool` (unexported) fields to both `Compose` and `RemoteCompose`
- Add `Detect(ctx) error` method on both structs — probes `docker compose version`, falls back to `docker-compose version`, no-ops if already detected
- **Detection is explicit, not lazy.** `command()` and `remoteCommand()` only read `Standalone` — they never trigger detection. Callers must call `Detect()` before first use. This avoids breaking existing tests.
- `command()` and `remoteCommand()` branch on `Standalone` to build the correct command
- Move package-level `ListProjects` to a method on `Compose` (symmetric with `RemoteCompose.ListProjects()`) — avoids parameter-threading through `ProjectLoader` type and `listLocalProjects` var
- CLI subcommands call `Detect()` explicitly after construction (remote: after `Connect()`); TUI loader calls `Detect()` before first `ListProjects()`; all factory closures inherit `Standalone` via `SetStandalone(rc.Standalone)` on new instances
- `SetStandalone(bool)` method on both structs sets `Standalone` and `detected = true`
- Error message when neither variant found: `"neither 'docker compose' nor 'docker-compose' found on host"`
- `Detect()` for `RemoteCompose` builds its own SSH probe command directly (not via `remoteCommand()`) to avoid unnecessary `CURRENT_UID` and `cd` prefix

## Implementation Steps

### Task 1: Add Standalone field and Detect method to Compose

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/compose_test.go`

- [x] Add `Standalone bool` and `detected bool` fields to `Compose` struct
- [x] Add `Detect(ctx context.Context) error` method: try `docker compose version` via `outputCmd` hook (or `exec.CommandContext` if nil), if fails try `docker-compose version`, set `Standalone` accordingly, skip if `detected` is true, return error `"neither 'docker compose' nor 'docker-compose' found"` if both fail
- [x] Add `SetStandalone(standalone bool)` method that sets `Standalone` and `detected = true`
- [x] Modify `command()` to branch: `exec.Command("docker-compose", args...)` when `Standalone`, else `exec.Command("docker", "compose", args...)` as before
- [x] Write tests for `Detect()`: plugin found, standalone found, neither found (using `outputCmd` hook)
- [x] Write tests for `command()`: verify `Cmd.Args` with `Standalone = false` and `Standalone = true`
- [x] Run tests: `go test ./internal/compose/ -v` — must pass before task 2

### Task 2: Add Standalone field and Detect method to RemoteCompose

**Files:**
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/remote_test.go`

- [x] Add `Standalone bool` and `detected bool` fields to `RemoteCompose` struct
- [x] Add `Detect(ctx context.Context) error` method: builds its own SSH command directly (not via `remoteCommand()`) to probe `docker compose version` then `docker-compose version` on the remote host, set `Standalone`, skip if `detected`, return clear error if both fail
- [x] Add `SetStandalone(standalone bool)` method that sets both fields
- [x] Modify `remoteCommand()` to use `"docker-compose %s"` when `Standalone`, else `"docker compose %s"` as before
- [x] Write tests for `Detect()` using `outputCmd` hook: plugin found, standalone found, neither found
- [x] Write tests for `remoteCommand()`: verify SSH command string with both standalone modes
- [x] Run tests: `go test ./internal/compose/ -v` — must pass before task 3

### Task 3: Move package-level ListProjects to method on Compose

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/compose_test.go`
- Modify: `cmd/list.go`

- [x] Add `ListProjects(ctx context.Context) ([]Project, error)` method on `Compose` that reads `c.Standalone` internally to build the correct command (`docker compose ls` vs `docker-compose ls`)
- [x] Keep the package-level `ListProjects` function as a thin wrapper (calls `Compose{}.ListProjects(ctx)`) for backward compat, or remove it and update callers directly
- [x] Update `cmd/list.go`: change `listLocalProjects` var to use a `Compose` instance with detection, so `ListProjects` reads `Standalone` from the instance
- [x] Write tests for `Compose.ListProjects()` with both standalone modes (verify command construction)
- [x] Run tests: `go test ./...` — must pass before task 4

### Task 4: Wire detection into CLI subcommands (deploy, list, logs)

**Files:**
- Modify: `cmd/deploy.go`
- Modify: `cmd/list.go`
- Modify: `cmd/logs.go`
- Modify: `cmd/deploy_test.go`
- Modify: `cmd/list_test.go`
- Modify: `cmd/logs_test.go`

- [x] `cmd/deploy.go`: add `rc.Detect(ctx)` after `rc.Connect(ctx)` for remote path; add `c.Detect(ctx)` for local path (where `c` is created via `opNewLocal`)
- [x] `cmd/list.go` remote path: add `rc.Detect(ctx)` after `rc.Connect(ctx)`; update multi-project remote factory to `rc2 := newRemote(server.Host, d); rc2.SetStandalone(rc.Standalone); return rc2`
- [x] `cmd/list.go` local path: create `Compose` instance, call `Detect(ctx)`, use it for both single-project and multi-project (via `ListProjects` method); update factory to inherit standalone: `lc := newLocalComposer(d); lc.SetStandalone(c.Standalone); return lc`
- [x] `cmd/logs.go`: add `rc.Detect(ctx)` after `rc.Connect(ctx)` for remote path; add detection for local path
- [x] Update `cmd/deploy_test.go`: updated test hooks to handle Detect probe
- [x] Update `cmd/list_test.go`: same pattern
- [x] Update `cmd/logs_test.go`: same pattern
- [x] Run tests: `go test ./cmd/ -v` — must pass before task 5

### Task 5: Wire detection into TUI (root.go connectCb and local flow)

**Files:**
- Modify: `cmd/root.go`

- [x] Local flow: add nil guard (`if c != nil`), call `c.Detect(cmd.Context())` before `tui.Run()`; update `factory` closure to `lc := compose.New(d); lc.SetStandalone(c.Standalone); return lc`
- [x] Remote flow (`connectCb`): update `loader` closure to call `rc.Detect(ctx)` before `rc.ListProjects(ctx)` — this runs after SSH is established via `tea.ExecProcess` since the loader is only called on `connectResultMsg` success; update `remoteFactory` to `newRC := compose.NewRemote(server.Host, d); newRC.SetStandalone(rc.Standalone); return newRC`
- [x] Run tests: `go test ./cmd/ -v` and `go test ./internal/tui/ -v` — must pass before task 6

### Task 6: Verify acceptance criteria

No new test code; this task verifies end-to-end behavior with the full existing test suite.

- [x] Verify: `docker compose` plugin mode works (default behavior unchanged)
- [x] Verify: `docker-compose` standalone mode works when plugin is unavailable
- [x] Verify: clear error message when neither variant is found
- [x] Verify: detection is cached (only one probe per session)
- [x] Run full test suite: `go test ./... -count=1`
- [x] Build binary: `go build -o cdeploy .`

### Task 7: [Final] Update documentation

- [x] Update CLAUDE.md Docker Compose section to mention standalone `docker-compose` support and auto-detection
- [x] Move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Test against a server with only standalone `docker-compose` (the user's original failing server)
- Test TUI flow: server picker -> project picker -> container list with standalone compose
- Test CLI: `cdeploy list -s <server>` with standalone compose server

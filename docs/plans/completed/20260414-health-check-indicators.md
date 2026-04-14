# Health Check Status Indicators in TUI Service List

## Overview
- Add health check status indicators to the TUI container list, showing Docker healthcheck state alongside the existing running/stopped dot
- Containers with a configured healthcheck display a colored icon (♥/✗/⏳) between the checkbox and the running dot; containers without a healthcheck show a space for alignment
- The `Health` field is already present in `docker compose ps --format json` output — no extra Docker API calls needed

## Context (from discovery)
- Files/components involved:
  - `internal/runner/runner.go` — `Composer` interface, `ContainerStatus()` signature
  - `internal/compose/compose.go` — `psEntry`, `parseContainerStatus()`, `Compose.ContainerStatus()`
  - `internal/compose/remote.go` — `RemoteCompose.ContainerStatus()`
  - `internal/tui/app.go` — `svcRunning` field, `servicesMsg`/`statusMsg` types, rendering in `viewContainerSelect()`
  - `internal/tui/styles.go` — lipgloss styles for dots
  - `cmd/list.go` — CLI `list` command uses `ContainerStatus()` via `mergeStatus()`
  - Test files: `internal/compose/compose_test.go`, `internal/tui/app_test.go`, `internal/runner/runner_test.go`, `cmd/list_test.go`
- `docker compose ps --format json` returns `"Health": "healthy"|"unhealthy"|"starting"|""` per container
- Current `parseContainerStatus()` returns `map[string]bool`; needs to become `map[string]ServiceStatus`
- The `Composer` interface is the seam — changing the return type ripples to both implementations, the runner test mock, and the TUI mock

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility for the CLI `list` command output

## Testing Strategy
- **Unit tests**: required for every task
  - `compose_test.go`: update `TestParseContainerStatus` with health field variations
  - `app_test.go`: update mock, `servicesMsg`/`statusMsg` assertions, rendering checks
  - `runner_test.go`: update mock to match new interface
  - `cmd/list_test.go`: update mock and assertions for new return type
- No e2e tests in this project

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix

## Solution Overview
- Introduce `ServiceStatus` struct in `internal/runner/runner.go` with `Running bool` and `Health string`
- Change `Composer.ContainerStatus()` return type from `map[string]bool` to `map[string]ServiceStatus`
- Parse the `Health` JSON field in `psEntry` and propagate it through `parseContainerStatus()`
- In the TUI, replace `svcRunning map[string]bool` with `svcStatus map[string]ServiceStatus`
- Render a fixed-width health column: `♥` (green), `✗` (red), `⏳` (yellow), or space (no healthcheck)
- Layout: `> [x] ♥  ● service-name` with aligned columns

## Technical Details
- **ServiceStatus struct**: `type ServiceStatus struct { Running bool; Health string }`
- **Health values from Docker**: `"healthy"`, `"unhealthy"`, `"starting"`, `""` (no healthcheck)
- **psEntry change**: add `Health string \`json:"Health"\``
- **parseContainerStatus**: returns `map[string]ServiceStatus`; for scaled services, Running uses OR logic (any running = running), Health uses worst-case priority (`unhealthy` > `starting` > `healthy` > `""`) so the most critical state is always shown
- **TUI health column**: fixed 2-char width slot placed between checkbox and dot to keep service names aligned. Use single-width Unicode characters for all states to avoid terminal width inconsistencies (⏳ is double-width in many terminals)
- **New styles**: `healthHealthy` (green/color 2), `healthUnhealthy` (red/color 1), `healthStarting` (yellow/color 3)

## What Goes Where
- **Implementation Steps**: all code changes, tests, and documentation
- **Post-Completion**: manual verification with a real Docker Compose project that has healthchecks

## Implementation Steps

### Task 1: Add ServiceStatus, update interface, compose package, and all mocks

**Files:**
- Modify: `internal/runner/runner.go`
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/remote.go`
- Modify: `internal/runner/runner_test.go`
- Modify: `internal/compose/compose_test.go`

- [x] Add `ServiceStatus` struct with `Running bool` and `Health string` fields in `internal/runner/runner.go`
- [x] Change `ContainerStatus()` return type in `Composer` interface from `map[string]bool` to `map[string]ServiceStatus`
- [x] Add `Health string \`json:"Health"\`` field to `psEntry` struct in `internal/compose/compose.go`
- [x] Change `parseContainerStatus()` return type to `map[string]runner.ServiceStatus`
- [x] Update parsing logic: set `Running` from State, set `Health` from the new field; for scaled services, OR the Running flag and use worst-case health priority (`unhealthy` > `starting` > `healthy`)
- [x] Update `Compose.ContainerStatus()` return type to match
- [x] Update `RemoteCompose.ContainerStatus()` return type in `internal/compose/remote.go` (signature only — delegates to shared `parseContainerStatus`)
- [x] Update `mockComposer.ContainerStatus()` return type in `internal/runner/runner_test.go` to return `map[string]runner.ServiceStatus` with non-empty Health values
- [x] Update `TestParseContainerStatus`: change `want` type to `map[string]runner.ServiceStatus`, update all existing cases
- [x] Add test cases: container with `"Health":"healthy"`, container with `"Health":"unhealthy"`, container with `"Health":"starting"`, container with no Health field
- [x] Add test case: scaled service with mixed health values (verify worst-case priority wins)
- [x] Run tests: `go test ./internal/compose/ ./internal/runner/ -v` — must pass before next task

### Task 2: Update CLI list command

**Files:**
- Modify: `cmd/list.go`
- Modify: `cmd/list_test.go` (if exists, otherwise `cmd/deploy_test.go`)

- [x] Update `serviceStatus` struct to include `Health string \`json:"health,omitempty"\``
- [x] Update `mergeStatus()` to accept `map[string]runner.ServiceStatus` and populate Health field
- [x] Update `formatDots()` to show health icon when Health is non-empty (same icons as TUI)
- [x] Update tests for `mergeStatus` and `formatDots` with health variations
- [x] Add test: `formatJSON` includes `health` field in JSON output when Health is non-empty
- [x] Run tests: `go test ./cmd/ -v` — must pass before next task

### Task 3: Update TUI model and messages

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] Replace `svcRunning map[string]bool` with `svcStatus map[string]runner.ServiceStatus` in `Model`
- [x] Update `servicesMsg` field from `running map[string]bool` to `status map[string]runner.ServiceStatus`
- [x] Update `statusMsg` field from `running map[string]bool` to `status map[string]runner.ServiceStatus`
- [x] Update all `servicesMsg` and `statusMsg` handlers in `Update()` to use new field names
- [x] Update `refreshStatus()` and `loadServices()` to construct messages with the new type
- [x] Update all reads of `m.svcRunning[svc]` to `m.svcStatus[svc].Running` throughout the file
- [x] Update backward-navigation cleanup: change `m.svcRunning = nil` to `m.svcStatus = nil` in esc handler, `entryLocal` handler, and `connectResultMsg` error path
- [x] Update `mockComposer` in `app_test.go`: change `running map[string]bool` to `status map[string]runner.ServiceStatus`, update `ContainerStatus()` to return it
- [x] Update all test cases that construct `servicesMsg` or `statusMsg` or check `svcRunning`
- [x] Run tests: `go test ./internal/tui/ -v` — must pass before next task

### Task 4: Add health icon rendering and styles

**Files:**
- Modify: `internal/tui/styles.go`
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] Add three new styles in `styles.go`: `healthHealthy` (color 2/green), `healthUnhealthy` (color 1/red), `healthStarting` (color 3/yellow)
- [x] In `viewContainerSelect()`, add a fixed-width health column between checkbox and dot using single-width Unicode characters for all states to avoid terminal width issues
- [x] Ensure rows align regardless of health icon presence (fixed-width slot with trailing space)
- [x] Add test: verify View() output contains health icon for service with health status
- [x] Add test: verify View() output alignment for mixed health/no-health services
- [x] Run tests: `go test ./internal/tui/ -v` — must pass before next task

### Task 5: Verify acceptance criteria

- [x] Run full test suite: `go test ./... -count=1`
- [x] Verify edge cases are covered in tests: no healthcheck, all healthy, all unhealthy, mixed, scaled services with worst-case priority

### Task 6: [Final] Update documentation

- [x] Update CLAUDE.md if new patterns discovered (e.g., ServiceStatus struct, health column)
- [x] Move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Test with a real Docker Compose project that has a healthcheck configured (e.g., `local-doc-mcp_v2`)
- Verify the TUI shows ♥ for healthy containers, no icon for containers without healthcheck
- Verify the CLI `list` command shows health status in both dot and JSON formats
- Test with a remote server via SSH to verify RemoteCompose works identically

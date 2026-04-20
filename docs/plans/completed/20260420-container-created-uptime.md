# Add container creation time and uptime to service list

## Overview
- Add two new columns to the container/service list: **Created** (absolute timestamp) and **Uptime** (relative duration)
- Displayed in both CLI (`list` command) and TUI (container select screen)
- Replaces the `"running"/"stopped"` text labels — the colored dot already indicates state
- Data is already available in Docker's `docker compose ps --format json` output but currently ignored

## Context (from discovery)
- Files/components involved:
  - `internal/runner/runner.go` — `ServiceStatus` struct (lines 8-12)
  - `internal/compose/compose.go` — `psEntry` struct (lines 333-338), `parseContainerStatus()` (lines 371-414)
  - `cmd/list.go` — `serviceStatus` struct (lines 24-29), `mergeStatus()`, `formatDots()`, `formatDotsGrouped()`, `formatJSON()`
  - `internal/tui/app.go` — `viewSelectContainers()` (lines 1784-1892)
  - `internal/compose/compose_test.go` — `parseContainerStatus` tests (line 463+)
  - `cmd/list_test.go` — format and integration tests
- Related patterns: column alignment via `fmt.Sprintf("%-*s", maxLen, ...)`, health icon rendering
- Dependencies: none new — only existing `time` stdlib package

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
- `formatUptime()` — table-driven pure function tests covering all Docker status string variants
- `parseContainerStatus()` — add Created/Status fields to JSON fixtures, test formatting and scaled aggregation
- `formatDots()`/`formatDotsGrouped()` — verify alignment with new columns, blank uptime for stopped
- `formatJSON()` — verify new fields in JSON output
- TUI rendering — at least one test verifying `viewSelectContainers()` renders Created/Uptime columns

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope

## Solution Overview

### Data flow
```
docker compose ps --format json
  → psEntry {Service, State, Health, CreatedAt (string), Status (string)}
  → parseContainerStatus() parses CreatedAt + formats Uptime
  → ServiceStatus {Running, Health, Created (string), Uptime (string)}
  → CLI: formatDots() / formatDotsGrouped() / formatJSON()
  → TUI: viewSelectContainers()
```

### Uptime parsing from Docker's Status field
- `"Up 3 hours"` → `"3h"`
- `"Up 2 days"` → `"2d"`
- `"Up About a minute"` → `"~1m"`
- `"Up 3 hours (healthy)"` → `"3h"` (strip health suffix)
- `"Restarting (1) 5 seconds ago"` → `"restarting"`
- `"Exited (0) 5 minutes ago"` → `""`
- Unrecognized `"Up ..."` → raw remainder as fallback

### Scaled services aggregation
- `Created` → oldest replica (min of parsed `time.Time` values from `CreatedAt` strings)
- `Uptime` → from the longest-running replica (paired with oldest Created); if none running, empty

### Column alignment
Both CLI and TUI use three independent max-width passes:
```go
fmt.Sprintf("%-*s  %-*s  %-*s", maxName, name, maxCreated, created, maxUptime, uptime)
```

## Implementation Steps

### Task 1: Add formatUptime() pure function

**Files:**
- Create: `internal/compose/uptime.go`
- Create: `internal/compose/uptime_test.go`

- [x] Create `internal/compose/uptime.go` with `formatUptime(status string) string`
- [x] Handle `"Up X"` prefix stripping and health suffix removal `"(healthy)"`, `"(unhealthy)"`, `"(health: starting)"`
- [x] Compact time units: `"X hours"` → `"Xh"`, `"X minutes"` → `"Xm"`, `"X days"` → `"Xd"`, `"X seconds"` → `"Xs"`, `"X weeks"` → `"Xw"`, `"X months"` → `"Xmo"`, multi-unit like `"3 hours 15 minutes"` → `"3h 15m"`
- [x] Handle singular forms: `"1 hour"` → `"1h"`, `"1 day"` → `"1d"`, `"1 month"` → `"1mo"`
- [x] Handle special cases: `"About a minute"` → `"~1m"`, `"Less than a second"` → `"<1s"`, `"About an hour"` → `"~1h"`
- [x] Handle `"Restarting ..."` → `"restarting"`
- [x] Handle non-"Up" statuses (`"Exited ..."`, `"Created"`, etc.) → `""`
- [x] Fallback: unrecognized `"Up ..."` → raw remainder trimmed
- [x] Write table-driven tests in `uptime_test.go` covering all variants above
- [x] Write tests for edge cases: empty string, whitespace-only, unknown format
- [x] Run tests: `go test ./internal/compose/ -run TestFormatUptime -v` — must pass

### Task 2: Extend ServiceStatus and parseContainerStatus()

**Files:**
- Modify: `internal/runner/runner.go`
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/compose_test.go`

- [x] Add `Created string` and `Uptime string` fields to `ServiceStatus` in `internal/runner/runner.go`
- [x] Add `CreatedAt string` (json:"CreatedAt") and `Status string` (json:"Status") to `psEntry` in `internal/compose/compose.go`
- [x] In `parseContainerStatus()` aggregation loop: parse `CreatedAt` string via `time.Parse("2006-01-02 15:04:05 -0700 MST", entry.CreatedAt)`, format as `"2006-01-02 15:04"` (empty if CreatedAt is empty or unparseable)
- [x] In aggregation loop: call `formatUptime(entry.Status)` for `Uptime`
- [x] Scaled services: parse `CreatedAt` into `time.Time` for min-comparison, track oldest and pair `Uptime` from the longest-running replica
- [x] Update existing `parseContainerStatus` test fixtures to include `"CreatedAt"` and `"Status"` JSON fields
- [x] Add new test cases: CreatedAt parsing and formatting, Uptime through parseContainerStatus, empty/missing CreatedAt → empty
- [x] Add test case: scaled services pick oldest Created and longest-running Uptime
- [x] Run tests: `go test ./internal/compose/ -v` — must pass

### Task 3: Update CLI rendering

**Files:**
- Modify: `cmd/list.go`
- Modify: `cmd/list_test.go`

- [x] Add `Created string` and `Uptime string` (json tags with omitempty) to `serviceStatus` in `cmd/list.go`
- [x] Update `mergeStatus()` to copy `Created` and `Uptime` from `runner.ServiceStatus`
- [x] Update `formatDots()`: add max-width passes for Created and Uptime columns, replace `"running"/"stopped"` with the two time columns
- [x] Update `formatDotsGrouped()`: same changes, maintaining 2-space indent
- [x] Update broken tests: `TestFormatDots_Alignment`, `TestFormatDots_MixedStates`, `TestListSingleProject_Dots` — they currently assert `"running"`/`"stopped"` strings which are being removed
- [x] Add test: `formatDots` alignment with varying Created/Uptime widths
- [x] Add test: stopped service has empty Uptime column
- [x] Add test: `formatJSON` includes Created and Uptime fields
- [x] Update `mergeStatus` tests to verify new fields are copied
- [x] Run tests: `go test ./cmd/ -v` — must pass

### Task 4: Update TUI rendering

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] Update `viewSelectContainers()`: add max-width calculation for Created and Uptime over all services (global alignment)
- [x] Update per-row rendering to append Created and Uptime columns after service name
- [x] Verify `svcVisibleCount()` is unaffected (line count per row unchanged — still 1 line per service)
- [x] Add test: verify `viewSelectContainers()` output includes Created/Uptime values when `svcStatus` has them
- [x] Update existing TUI tests that assert on container screen output if they break
- [x] Run full test suite: `go test ./... -count=1` — must pass

### Task 5: Verify acceptance criteria

- [x] Verify all requirements from Overview are implemented
- [x] Verify edge cases: Created=0, empty Status, Restarting, scaled services
- [x] Run full test suite: `go test ./... -count=1`
- [x] Build binary: `go build -o cdeploy .`
- [x] Run `go mod tidy` if any imports changed

### Task 6: [Final] Update documentation

- [x] Update CLAUDE.md: add Created/Uptime to ServiceStatus description, document formatUptime() in Docker Compose section
- [x] Run tests: `go test ./... -count=1` — must pass
- [x] Move this plan to `docs/plans/completed/`

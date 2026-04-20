# Add Column Captions to TUI Container List

## Overview
- Add a dim/gray header row showing "Created" and "Uptime" labels above their respective columns on the container select screen
- Solves first-time user confusion ("what do these numbers mean?") and improves power-user scannability across 40+ services
- Header row only appears when status data exists (conditional), uses existing `descStyle`, costs 1 visible service line

## Context (from discovery)
- Files involved: `internal/tui/app.go` (rendering + viewport math), `internal/tui/app_test.go` (tests)
- Styles: reuses existing `descStyle` (color "8", gray/dim) from `internal/tui/styles.go` — no new styles needed
- Column alignment uses `maxName`, `maxCreated`, `maxUptime` computed across all services in `viewSelectContainers()`
- Viewport math in `svcVisibleCount()` uses `headerLines = 3` currently; needs conditional +1
- Existing tests cover `svcVisibleCount()` with specific expected values that will need updating

## Development Approach
- **testing approach**: Regular (code first, then tests)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- run tests after each change

## Testing Strategy
- **unit tests**: update `svcVisibleCount` tests for new headerLines values, add header row content tests
- TUI tests use `Update()` with `tea.KeyMsg` directly — no TTY needed

## Solution Overview

### Header row format
```
                                          Created          Uptime
> [ ] ♥ ● account-settlement-service  2025-12-30 17:13    5d
  [ ] ♥ ● alias-directory-service     2025-12-30 17:06    5d
```

### Alignment calculation
Left padding = 10 display chars (cursor `2` + checkbox `3` + space `1` + health `1` + space `1` + dot `1` + space `1`) + `maxName` width + `2` spaces before Created column. Uptime label offset follows same `maxCreated + 2` spacing.

### Conditional display
- Header shown only when `maxCreated > 0 || maxUptime > 0`
- `svcVisibleCount()` needs a helper `hasStatusColumns()` to check if any service has Created/Uptime data
- When no status data exists (services loading, or compose output lacks it), header is hidden and no line is consumed

### Viewport impact
- `headerLines` increases from 3 to 4 when header is shown
- `fixSvcOffset()` unchanged — it calls `svcVisibleCount()` which handles it
- Net impact: 1 fewer visible service on screen (negligible with 40+ services)

## Implementation Steps

### Task 1: Add `hasStatusColumns()` helper and update `svcVisibleCount()`

**Files:**
- Modify: `internal/tui/app.go`

- [ ] Add `hasStatusColumns()` method on Model that iterates `m.svcStatus` and returns true if any service has non-empty `Created` or `Uptime`
- [ ] Update `svcVisibleCount()` to add 1 to `headerLines` when `hasStatusColumns()` returns true
- [ ] Update the comment on `svcVisibleCount()` to document the conditional header line
- [ ] Update existing `svcVisibleCount` tests — all current tests use empty `svcStatus`, so expected values stay the same; add new test case with status data expecting headerLines=4
- [ ] Run tests: `go test ./internal/tui/ -run TestSvcVisibleCount -v` — must pass before next task

### Task 2: Render the header row in `viewSelectContainers()`

**Files:**
- Modify: `internal/tui/app.go`

- [ ] After the gap/scroll-indicator block (line ~1843) and before the service loop (line ~1845), insert header row rendering
- [ ] Build header string: `strings.Repeat(" ", 10+maxName)` + conditional `fmt.Sprintf("  %-*s", maxCreated, "Created")` + conditional `fmt.Sprintf("  %-*s", maxUptime, "Uptime")`, styled with `descStyle`
- [ ] Only emit the header line when `maxCreated > 0 || maxUptime > 0`
- [ ] Add test: verify header row appears in `viewSelectContainers()` output when services have Created/Uptime data
- [ ] Add test: verify header row is absent when no Created/Uptime data exists
- [ ] Add test: verify header labels align with data columns (check that "Created" and actual created value start at same string offset)
- [ ] Run tests: `go test ./internal/tui/ -v` — must pass before next task

### Task 3: Verify acceptance criteria
- [ ] Verify header shows "Created" and "Uptime" labels aligned above data columns
- [ ] Verify header is hidden when no status data exists
- [ ] Verify scroll indicators still work correctly with the extra header line
- [ ] Run full test suite: `go test ./... -count=1`

### Task 4: [Final] Update documentation
- [ ] Update CLAUDE.md — add note about header row in the service list scrolling section
- [ ] Move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Test with a real remote server (40+ services) to verify alignment and scrolling
- Test with a local compose project with few services to verify header conditionally appears
- Resize terminal to verify viewport math adjusts correctly

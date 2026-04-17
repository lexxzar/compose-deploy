# Scrollable Service List on Container Select Screen

## Overview
- The container select screen (`screenSelectContainers`) renders all services in a plain loop with no scrolling
- On small terminals the list overflows and clips the top (breadcrumb + first N services invisible)
- Add a scroll window that follows the cursor, showing only services that fit the terminal height
- When all services fit, behavior is identical to current (no indicators, no offset)

## Context (from discovery)
- Files involved: `internal/tui/app.go` (Model, Update, View), `internal/tui/app_test.go`
- Model already has `svcCursor int` for cursor tracking, no scroll offset
- Other screens (logs, config) use `viewport.Model` — not suitable here (interactive list with checkboxes)
- `m.width` and `m.height` are already tracked via `tea.WindowSizeMsg`
- `svcCursor` resets to 0 in: `servicesMsg` handler, `esc` back to project, return from progress/logs screens
- Footer help bar is 1 line when wide enough, 2 lines when narrow

## Development Approach
- **Testing approach**: Regular (code first, then tests per task)
- Complete each task fully before moving to the next
- **CRITICAL: every task MUST include new/updated tests**
- **CRITICAL: all tests must pass before starting next task**
- Run tests after each change with `go test ./internal/tui/ -v`

## Testing Strategy
- **Unit tests**: test `svcVisibleCount()` and `fixSvcOffset()` as pure logic
- **Integration tests**: test cursor movement + offset via `Update()` with `tea.KeyMsg`
- **View tests**: verify `viewSelectContainers()` output contains/omits scroll indicators

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with + prefix
- Document issues/blockers with warning prefix

## Solution Overview
- Add `svcOffset int` to `Model` — index of first visible service
- `svcVisibleCount()` calculates how many services fit: `m.height - headerLines - footerLines`, min 1
- `fixSvcOffset()` ensures cursor is always within the visible window by adjusting `svcOffset`
- `viewSelectContainers()` renders only `services[svcOffset:svcOffset+visible]`
- Scroll indicators (`▲ N more` / `▼ N more`) shown as decoration in existing blank-line gaps

## Technical Details
- Header: breadcrumb + blank line = 2 lines
- Footer: blank + help (1 or 2 lines) = 2 or 3 lines; confirmation = 2 lines; warning adds 1 extra line
- Visible count: `m.height - 2 - footerLines`, accounting for confirming and warning states
- **Design invariant**: `▲`/`▼` indicators replace the content of existing blank lines (breadcrumb gap, help gap) — they do not add or remove lines. Therefore `svcVisibleCount()` is independent of whether indicators are shown. This avoids off-by-one oscillation.
- When `m.height == 0` (no WindowSizeMsg yet), `svcVisibleCount()` returns `len(m.services)` so `fixSvcOffset()` is a no-op and all items render (backward compatible)

## Implementation Steps

### Task 1: Add scroll state and offset calculation methods

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] Add `svcOffset int` field to `Model` struct (in the "Screen 1: service select" section, after `svcCursor`)
- [ ] Add `svcVisibleCount()` method that calculates available lines for services based on `m.height`, `m.width`, `m.confirming`, and `m.warning`; returns `len(m.services)` when `m.height == 0`
- [ ] Add `fixSvcOffset()` method (pointer receiver `*Model`) that adjusts `svcOffset` so cursor is within `[svcOffset, svcOffset+visible)`
- [ ] Add `svcOffset = 0` alongside `m.svcCursor = 0` in `servicesMsg` handler
- [ ] Add `svcOffset = 0` alongside `m.svcCursor = 0` in `esc` back to project screen
- [ ] Write unit tests for `svcVisibleCount()` with various heights, widths, confirming, and warning states
- [ ] Write unit tests for `fixSvcOffset()`: cursor below window, cursor above window, all items fit, height == 0
- [ ] Run `go test ./internal/tui/ -v` — must pass

### Task 2: Wire fixSvcOffset into key handlers and WindowSizeMsg

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] Call `m.fixSvcOffset()` after the `up/k` key handler block
- [ ] Call `m.fixSvcOffset()` after the `down/j` key handler block
- [ ] Call `m.fixSvcOffset()` after setting `m.confirming = true` (in `r`, `d`, `s` key handlers)
- [ ] Call `m.fixSvcOffset()` after setting `m.warning` (in `r`, `d`, `s` key handlers, no-selection path)
- [ ] Call `m.fixSvcOffset()` when confirming is cleared via `esc` (footer shrinks)
- [ ] Add `screenSelectContainers` case in `WindowSizeMsg` handler that calls `m.fixSvcOffset()`
- [ ] Call `fixSvcOffset()` in `statusMsg` handler (runs after `refreshStatus()` on return from progress/logs)
- [ ] Write integration tests: cursor down past visible window scrolls (send `WindowSizeMsg` + multiple down keys, verify `svcOffset`)
- [ ] Write integration tests: cursor up past top scrolls back, verify `svcOffset` decreases
- [ ] Write test: entering confirmation mode on small terminal calls `fixSvcOffset()`
- [ ] Write test: `a` (select all) does not change `svcOffset`
- [ ] Run `go test ./internal/tui/ -v` — must pass

### Task 3: Update viewSelectContainers to render windowed list

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] Replace the full `for i, svc := range m.services` loop with a windowed render: calculate `visible`, `start = m.svcOffset`, `end = min(start+visible, len(services))`
- [ ] When `svcOffset > 0`, replace the `\n\n` after breadcrumb with `\n` + `▲ N more` + `\n`
- [ ] When `end < len(services)`, append `▼ N more` line before the help bar (in the existing `\n` gap)
- [ ] When height is 0 (no WindowSizeMsg received), render all items (backward compatible)
- [ ] Write view test: `viewSelectContainers()` contains `▲` indicator when offset > 0
- [ ] Write view test: `viewSelectContainers()` contains `▼` indicator when items below window
- [ ] Write view test: no indicators when all items fit
- [ ] Run `go test ./internal/tui/ -v` — must pass

### Task 4: Verify acceptance criteria

- [ ] Verify all requirements from Overview are implemented
- [ ] Verify edge cases: height == 0, single-item visible, resize shrinks window
- [ ] Run full test suite: `go test ./... -count=1`

### Task 5: Update CLAUDE.md documentation

**Files:**
- Modify: `CLAUDE.md`

- [ ] Add scroll behavior note to the TUI state machine section: `svcOffset` tracks the first visible service; `fixSvcOffset()` ensures cursor visibility after any state change that affects visible count (cursor move, window resize, confirming/warning state change); scroll indicators replace existing blank-line content
- [ ] Run full test suite: `go test ./... -count=1`
- [ ] Move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Test with a real server that has 40+ services on a small terminal
- Verify cursor stays visible when navigating up/down through the full list
- Verify no indicators appear when all services fit
- Verify terminal resize mid-list keeps cursor visible
- Verify `a` (select all) doesn't break scroll position
- Verify entering confirmation mode on a small terminal doesn't hide the cursor

# Log Viewer Wrap and Pretty-Print Modes

## Overview
- Add two independent toggles to the TUI log viewer (`screenLogs`): `w` for soft-wrap and `p` for pretty JSON
- Solves the problem of long JSON log lines being truncated at the terminal edge, making them unreadable
- Integrates with the existing `viewport.Model` and `screenLogs` screen without changing the streaming architecture

## Context (from discovery)
- Files involved: `internal/tui/app.go` (Model, key handlers, view, enterLogs), `internal/tui/app_test.go` (existing log tests)
- Viewport (`charmbracelet/bubbles v1.0.0`) has horizontal scrolling (`SetHorizontalStep`, `ScrollLeft`/`ScrollRight`) but no built-in soft-wrap
- Horizontal scrolling is disabled by default (`horizontalStep = 0`), which is why lines currently just get cut off
- Log lines follow docker compose format: `<service_name> | <log body>` where body may be JSON or plain text
- `logsContent` stores raw accumulated text; `SetContent()` is called on each `logChunkMsg`

## Development Approach
- **testing approach**: Regular (code first, then tests)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change
- maintain backward compatibility

## Testing Strategy
- **unit tests**: required for every task
- `formatLogContent` is a pure function — easy to test with table-driven cases
- Key toggle tests verify Model state transitions (same pattern as existing TUI tests)
- No e2e tests in this project

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with + prefix
- document issues/blockers with !! prefix
- update plan if implementation deviates from original scope

## Solution Overview

Two independent boolean toggles on the Model:

| State | Behavior |
|---|---|
| wrap off, pretty off | Raw lines, horizontal scroll enabled (`SetHorizontalStep(4)`) |
| wrap on, pretty off | Soft-wrap long lines at viewport width |
| wrap off, pretty on | JSON indented, horizontal scroll for overflow |
| wrap on, pretty on | JSON indented AND wrapped — maximum readability |

Defaults: `logsWrap=true`, `logsPretty=false`

A pure helper function `formatLogContent(raw, width, wrap, pretty)` applies transformations in order:
1. If `pretty` — split each line on first ` | `, try `json.Unmarshal` on body, if valid use `json.MarshalIndent` with 2-space indent, prepend prefix to first line and pad continuation lines
2. If `wrap` — soft-wrap all resulting lines to viewport width

This function is called whenever content changes (new chunk), toggles flip, or the window resizes.

## Technical Details
- `formatLogContent` splits on `\n`, processes each line independently
- JSON detection: `json.Valid([]byte(body))` for speed, then `json.MarshalIndent` only for valid JSON
- Prefix alignment: continuation lines padded with `strings.Repeat(" ", len(prefix)+3)` (3 for ` | `)
- Soft-wrap: break line at `width` boundary, indent continuation lines for readability
- `SetHorizontalStep(4)` when wrap is off; `SetHorizontalStep(0)` when wrap is on

## Implementation Steps

### Task 1: Add `formatLogContent` helper function

**Files:**
- Create: `internal/tui/format.go`
- Create: `internal/tui/format_test.go`

- [ ] create `internal/tui/format.go` with `formatLogContent(raw string, width int, wrap bool, pretty bool) string`
- [ ] implement JSON pretty-print: split line on first ` | `, try `json.Valid`, `json.MarshalIndent` with 2-space indent, pad continuation lines with prefix-width spaces
- [ ] implement soft-wrap: break lines exceeding `width`, indent continuation lines
- [ ] handle edge cases: empty input, lines without ` | ` prefix, non-JSON bodies, mixed content
- [ ] write tests for JSON line formatting (single JSON object, nested JSON)
- [ ] write tests for non-JSON line pass-through
- [ ] write tests for mixed content (some JSON, some plain text)
- [ ] write tests for soft-wrap (long line, short line, exact width)
- [ ] write tests for combined wrap+pretty mode
- [ ] write tests for edge cases (empty string, no prefix, empty JSON `{}`)
- [ ] run tests — must pass before task 2

### Task 2: Integrate toggles into TUI Model and key handlers

**Files:**
- Modify: `internal/tui/app.go`

- [ ] add `logsWrap bool` and `logsPretty bool` fields to Model struct
- [ ] set `logsWrap = true` in `enterLogs()`
- [ ] add helper method `applyLogFormat()` that calls `formatLogContent` and `SetContent` on the viewport
- [ ] update `logChunkMsg` handler: append to `logsContent`, call `applyLogFormat()`
- [ ] update `logDoneMsg` handler: same pattern for error append
- [ ] add `w` key handler in `screenLogs`: toggle `logsWrap`, adjust `SetHorizontalStep`, call `applyLogFormat()`, preserve scroll position
- [ ] add `p` key handler in `screenLogs`: toggle `logsPretty`, call `applyLogFormat()`, preserve scroll position
- [ ] update `WindowSizeMsg` for `screenLogs`: call `applyLogFormat()` after resizing viewport (re-wrap at new width)
- [ ] write test: `w` key toggles `logsWrap` and re-renders content
- [ ] write test: `p` key toggles `logsPretty` and re-renders content
- [ ] write test: window resize triggers re-format
- [ ] run tests — must pass before task 3

### Task 3: Update help bar and cleanup

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] update `viewLogs()` help bar to show `w wrap` and `p pretty` hints
- [ ] show `<-/-> scroll` hint only when wrap is off
- [ ] update `esc` handler cleanup to reset `logsWrap` and `logsPretty`
- [ ] write test: help bar content changes based on wrap state
- [ ] write test: `esc` clears wrap/pretty state
- [ ] run tests — must pass before task 4

### Task 4: Verify acceptance criteria

- [ ] verify all four mode combinations work: raw, wrap-only, pretty-only, wrap+pretty
- [ ] verify edge cases are handled (empty logs, non-JSON, mixed content)
- [ ] run full test suite: `go test ./... -count=1`
- [ ] verify test coverage for new code

### Task 5: [Final] Update documentation

- [ ] update CLAUDE.md with log viewer wrap/pretty documentation
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- test with real docker compose logs containing JSON output
- test with non-JSON logs (plain text, stack traces)
- test window resize while viewing logs
- test toggling modes during live log streaming
- verify scroll position behavior when toggling modes

# Server Status Bar with Configurable Color

## Overview
- Add a persistent colored status bar to the TUI showing the currently selected remote server
- Prevents accidentally operating on the wrong server (stop/deploy are destructive)
- Optional `color` field in `servers.yml` lets users visually distinguish servers (e.g., red for production)
- Bar appears on all post-selection screens; absent for Local mode

## Context (from discovery)
- Files/components involved:
  - `internal/config/config.go` — `Server` struct, `Validate()`
  - `internal/config/config_test.go` — config tests
  - `internal/tui/app.go` — `Model` struct, `breadcrumb()`, 5 view functions, server selection handlers
  - `internal/tui/styles.go` — lipgloss styles
  - `internal/tui/app_test.go` — TUI model tests
- Related patterns: `serverName` is already set/cleared at lines 415, 553, 568, 590 in `app.go`; breadcrumb renders on 5 screens
- Dependencies: `lipgloss` (already in use), `config.Server` (adding one field)

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility (empty `color` = gray default, no breaking YAML changes)

## Testing Strategy
- **Unit tests**: required for every task
- Config: parse color, validate invalid/empty color
- TUI: `statusBar()` output for remote/local/no-color cases, status bar appears in view output

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with + prefix
- Document issues/blockers with warning prefix
- Update plan if implementation deviates from original scope

## Solution Overview (Option B — inline badge)
- Add optional `color` field to `config.Server` struct
- Validate against fixed set of ANSI color names: red, green, yellow, blue, magenta, cyan, white, gray
- Add `serverHost` and `serverColor` fields to TUI `Model`, set/cleared alongside existing `serverName`
- `serverBadge()` renders server name as a colored inline badge within the breadcrumb (e.g. `cdeploy > [prod-server] > services`)
- Color map in `styles.go` maps color names to lipgloss background colors; default is gray
- No separate bar line — badge is part of the breadcrumb, zero extra screen real estate
- No badge rendered when `serverName` is empty (Local mode or no servers configured)

## Technical Details
- Color map: `{"red":"1", "green":"2", "yellow":"3", "blue":"4", "magenta":"5", "cyan":"6", "white":"7", "gray":"8"}`
- Badge format: `serverBadgeStyle(color).Render(" " + serverName + " ")` — background color with black bold text
- `serverColor` resolution: `Server.Color` if non-empty, else `"gray"`
- YAML example:
  ```yaml
  servers:
    - name: prod-web
      host: user@10.0.1.50
      group: Production
      color: red
    - name: staging
      host: user@10.0.2.10
      group: Dev
      color: green
  ```

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): all changes are within this codebase
- **Post-Completion**: manual visual testing in a real terminal with color support

## Implementation Steps

### Task 1: Add `color` field to config and validate

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [x] Add `Color string` field with `yaml:"color,omitempty"` tag to `Server` struct
- [x] Add `validColors` set (map or slice) with allowed values: red, green, yellow, blue, magenta, cyan, white, gray
- [x] Update `Validate()` to check `Color` against allowed set when non-empty
- [x] Write test: parse server with `color: red` — verify `Color` field populated
- [x] Write test: validate server with `color: purple` — expect error containing "unknown color"
- [x] Write test: validate server with empty color — expect no error
- [x] Run tests: `go test ./internal/config/ -v` — must pass before next task

### Task 2: Add color map, status bar style, and Model fields

**Files:**
- Modify: `internal/tui/styles.go`
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] Add `colorMap` variable: `map[string]lipgloss.Color` mapping color names to ANSI codes
- [x] Add `statusBarAccent` function: takes color name string, returns `lipgloss.Style` with foreground from `colorMap` (falls back to `"8"` for unknown/empty)
- [x] Add `serverHost string` and `serverColor string` fields to `Model` struct
- [x] Set `serverHost = server.Host` and `serverColor = server.Color` alongside `serverName` at the two assignment locations (lines ~415 and ~568)
- [x] Clear `serverHost` and `serverColor` to `""` at the two clearing locations (lines ~553 and ~590)
- [x] Clear `serverHost` and `serverColor` in the `connectResultMsg` error path (line ~425-430) — follows CLAUDE.md cleanup discipline
- [x] Implement `statusBar()` method on Model: returns formatted status bar string (with trailing `\n`) or empty string when `serverName` is empty
- [x] Write test: `TestStatusBarAccent_KnownColor` — verify known color names return expected lipgloss style
- [x] Write test: `TestStatusBarAccent_UnknownAndEmpty` — verify empty and unknown color strings fall back to gray
- [x] Write test: `TestStatusBar_RemoteServer` — set serverName/serverHost/serverColor, assert output contains server name and host with accent
- [x] Write test: `TestStatusBar_Local` — empty serverName, assert returns `""`
- [x] Write test: `TestStatusBar_DefaultColor` — serverName set but serverColor empty, assert bar renders with gray default
- [x] Run tests: `go test ./internal/tui/ -v` — must pass before next task

### Task 3: Integrate status bar into view functions

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] Prepend `m.statusBar()` in `viewSelectProject()` (before breadcrumb line)
- [x] Prepend `m.statusBar()` in `viewSelectContainers()` (before breadcrumb line)
- [x] Prepend `m.statusBar()` in progress view (before breadcrumb line)
- [x] Prepend `m.statusBar()` in `viewLogs()` (before breadcrumb line)
- [x] Prepend `m.statusBar()` in `viewConfig()` (before breadcrumb line)
- [x] Verify viewport height calculations in logs/config screens account for the extra status bar line when present
- [x] Write test: render `viewSelectContainers()` with server set, assert output contains status bar content before breadcrumb
- [x] Write test: render `viewSelectContainers()` without server (Local mode), assert no status bar in output
- [x] Run tests: `go test ./internal/tui/ -v` — must pass before next task

### Task 4: Verify acceptance criteria

- [x] Verify: status bar appears on all 5 post-selection screens when a remote server is selected
- [x] Verify: no status bar when Local mode or no servers configured
- [x] Verify: unknown/empty color defaults to gray
- [x] Verify: invalid color in YAML produces validation error
- [x] Verify: existing tests still pass with no regressions
- [x] Run full test suite: `go test ./... -count=1`

### Task 5: [Final] Update documentation

- [x] Update CLAUDE.md — add status bar to Config section and TUI state machine description
- [x] Move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Run `cdeploy` TUI with a `servers.yml` containing servers with different `color` values
- Verify color rendering in terminal (ensure colors display correctly on dark/light backgrounds)
- Test with no `color` field set — confirm gray default renders
- Navigate through all screens (project, containers, progress, logs, config) — confirm bar persists
- Select Local — confirm no bar appears

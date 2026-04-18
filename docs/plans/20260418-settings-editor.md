# Settings Editor TUI — Server CRUD

## Overview
- Add an in-TUI settings editor for managing remote servers (add, edit, delete) with all fields: name, host, project_dir, group, color
- Eliminates the need to manually edit `~/.cdeploy/servers.yml` — users can manage servers entirely from the TUI
- Two new screens (`screenSettingsList` + `screenSettingsForm`) accessible via `s` key on the server select screen
- Uses `bubbles/textinput` for text fields (the `bubbles` module is already a dependency; `textinput` is a new import from it) and a cycle picker for the color field

## Context (from discovery)
- Config: `internal/config/config.go` — `Server` struct with 5 fields, `Load()`, `Validate()`, `FindServer()`. No `Save()` yet.
- TUI: `internal/tui/app.go` (1657 lines) — 6 screens, flat Model struct, `handleKey()` dispatches by screen
- TUI entry: `cmd/root.go` calls `tui.Run()` passing `cfg.Servers` (slice) and a `ConnectCallback`. Currently no config path passed.
- TUI constructor: `NewModel()` accepts servers via `[]config.Server`, uses functional options pattern
- Tests: `app_test.go` (3832 lines) tests model transitions via `Update()` with `tea.KeyMsg`; `config_test.go` tests parsing/validation with temp files
- Styles: `styles.go` has `colorMap` and `serverBadgeStyle()` already mapping color names to lipgloss colors

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
- Config package: test `Save()` with temp files — round-trip (save + load), atomic write, directory creation, error cases
- TUI: test screen transitions via `Update()` with `tea.KeyMsg` — navigation, form field focus, validation, save/delete flows
- No e2e tests in this project

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with + prefix
- Document issues/blockers with ! prefix
- Update plan if implementation deviates from original scope

## Solution Overview

### Navigation flow
```
screenSelectServer
    |  s key
    v
screenSettingsList          (browse servers: add/edit/delete)
    |  a / enter            |  esc
    v                       v
screenSettingsForm    back to screenSelectServer
    |  enter (save)
    |  esc (discard)
    v
back to screenSettingsList
```

### Key bindings
**Settings List**: up/down navigate, `a` add, `enter`/`e` edit, `d` delete (inline y/n confirmation), `esc` back, `q` quit
**Settings Form**: tab/shift-tab/up/down cycle fields, left/right on color field cycle values, `enter` validate+save, `esc` discard

### Config path threading (via functional options)
`NewModel()` already has 5+ positional params and 144 test call sites. To avoid breaking them, use the existing functional options pattern:
- `WithConfigPath(path string) Option` — sets `configPath` on Model
- `WithConfig(cfg *config.Config) Option` — sets `config` on Model
Only `cmd/root.go` and new settings tests need these options. Existing test call sites remain untouched.

### Server list data consistency
`m.servers` (used throughout for navigation/display) and `m.config.Servers` (used for save) must stay in sync. **`m.config.Servers` is the source of truth.** After any mutation (add/edit/delete), always assign `m.servers = m.config.Servers` and rebuild `m.serverEntries`. Settings is only reachable from `screenSelectServer` where no server is actively connected, so no stale connection cleanup is needed.

## Technical Details

### Config.Save()
- Marshals `Config` to YAML via `yaml.Marshal()`
- Atomic write: `os.CreateTemp()` in same directory, write, `os.Rename()` over target
- Creates parent directory with `os.MkdirAll()` if needed
- Returns wrapped errors for each failure mode

### Form fields
- 4x `textinput.Model` stored as `[4]textinput.Model` (indices: 0=name, 1=host, 2=project_dir, 3=group)
- Each has `Placeholder` text and `CharLimit`
- `settingsField int` tracks focus (0-3 = text inputs, 4 = color picker)
- Color cycles through: `""` (none), `"red"`, `"green"`, `"yellow"`, `"blue"`, `"magenta"`, `"cyan"`, `"white"`, `"gray"`

### Validation on save
- Build a temporary `Config` with the modified server list and call `config.Validate()` — reuses existing validation (name required, host required, name uniqueness, color validity) rather than reimplementing in the TUI
- Map `Validate()` errors to user-friendly `settingsErr` messages
- Color guaranteed valid by cycle picker, but `Validate()` double-checks

### State cleanup
- `esc` from settings list: return to server select, `m.servers` and `m.serverEntries` already up to date
- `esc` from settings form: clear form fields, return to settings list, no data changes
- On save: mutate `m.config.Servers`, assign `m.servers = m.config.Servers`, call `buildServerEntries()`, fix `settingsCursor` bounds, write file via `m.config.Save(m.configPath)`
- On delete: remove from `m.config.Servers`, sync `m.servers`, rebuild entries, fix cursors, save file

## What Goes Where
- **Implementation Steps**: all code changes, tests, docs — achievable in this codebase
- **Post-Completion**: manual testing of TUI flows

## Implementation Steps

### Task 1: Add `Save()` to config package

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] Add `Save(path string) error` method to `*Config` — creates parent dir, writes temp file, renames atomically
- [ ] Replace unexported `validColors` map with exported `ValidColors` slice (ordered, for cycle picker); update `Validate()` to use `slices.Contains(ValidColors, s.Color)` instead of map lookup — single source of truth
- [ ] Write test for Save round-trip (save then load, compare)
- [ ] Write test for Save creating parent directory
- [ ] Write test for Save with empty config (no servers)
- [ ] Run tests: `go test ./internal/config/ -v` — must pass before task 2

### Task 2: Thread config path into TUI via functional options

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `cmd/root.go`

- [ ] Add `configPath string` and `config *config.Config` fields to `Model`
- [ ] Add `WithConfigPath(path string) Option` functional option
- [ ] Add `WithConfig(cfg *config.Config) Option` functional option
- [ ] Update `cmd/root.go` to pass `tui.WithConfigPath(config.DefaultPath())` and `tui.WithConfig(cfg)` as options — no changes to `NewModel()`/`Run()` signatures
- [ ] Existing test call sites remain untouched (no new positional params)
- [ ] Write test verifying WithConfigPath/WithConfig options set fields correctly
- [ ] Run tests: `go test ./internal/tui/ ./cmd/ -v` — must pass before task 3

### Task 3: Settings list screen — navigation and display

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/styles.go`

- [ ] Add `screenSettingsList` and `screenSettingsForm` to the `screen` enum
- [ ] Add settings model fields: `settingsCursor int`, `settingsDelete bool`
- [ ] Add `s` key handler in `screenSelectServer` — transitions to `screenSettingsList`, resets `settingsCursor` to 0
- [ ] Update `viewSelectServer()` help line to include `s settings` hint (so users can discover the feature)
- [ ] Add `handleKey` case for `screenSettingsList`: up/down navigation, `esc` back to server select, `q` quit
- [ ] Add `viewSettingsList()` — tabular display with Name, Host, Group, Color columns; cursor indicator; empty state message
- [ ] Add any new styles needed (table header, color swatch rendering)
- [ ] Add help line: `a add  •  enter edit  •  d delete  •  esc back`
- [ ] Wire into `View()` switch and `handleKey()` switch
- [ ] Write tests for `s` key → settings list transition
- [ ] Write tests for up/down navigation in settings list
- [ ] Write tests for `esc` back to server select
- [ ] Run tests: `go test ./internal/tui/ -v` — must pass before task 4

### Task 4: Settings form screen — field navigation and display

**Files:**
- Modify: `internal/tui/app.go`

- [ ] Add form model fields: `settingsEditing int`, `settingsField int`, `settingsInputs [4]textinput.Model`, `settingsColor string`, `settingsErr string`
- [ ] Add helper `initSettingsInputs()` to create the 4 `textinput.Model` instances with placeholders (Name: "server-name", Host: "user@hostname", Project Dir: "/path/to/project", Group: "group-name")
- [ ] Add `a` key handler in settings list — sets `settingsEditing = -1`, initializes blank form, transitions to `screenSettingsForm`
- [ ] Add `enter`/`e` key handler in settings list — sets `settingsEditing` to server index, pre-fills form fields, transitions to `screenSettingsForm`
- [ ] Add `handleKey` for `screenSettingsForm`: tab/shift-tab/up/down cycle `settingsField`, left/right on color field cycle through `ValidColors`, `esc` discard and return to list
- [ ] Forward key events to focused `textinput.Model` via its `Update()` when `settingsField` is 0-3
- [ ] Add `viewSettingsForm()` — labels + inputs, color picker with `< value >` rendered in color, error display, help line
- [ ] Wire into `View()` switch and `handleKey()` switch
- [ ] Write tests for `a` key → blank form transition
- [ ] Write tests for `enter` on server → pre-filled form
- [ ] Write tests for tab cycling through fields
- [ ] Write tests for color cycling (left/right)
- [ ] Write tests for `esc` discard back to list
- [ ] Run tests: `go test ./internal/tui/ -v` — must pass before task 5

### Task 5: Save, delete, and validation logic

**Files:**
- Modify: `internal/tui/app.go`

- [ ] Add `enter` handler in settings form — build temporary config with modified servers, call `config.Validate()`, map errors to user-friendly `settingsErr`, stay on form if invalid
- [ ] On valid save (add): append new `Server` to `m.config.Servers`, call `m.config.Save(m.configPath)`, sync `m.servers = m.config.Servers`, rebuild `m.serverEntries`, return to settings list
- [ ] On valid save (edit): update `m.config.Servers[settingsEditing]` in place, save, sync `m.servers`, rebuild, return to list
- [ ] Add `d` key handler in settings list — set `settingsDelete = true` (inline confirmation)
- [ ] Add `y`/`n` handlers when `settingsDelete` is true — `y` removes server from config, saves, rebuilds entries, fixes cursor bounds; `n` cancels
- [ ] Handle save errors — display via `settingsErr` on form or `svcErr`-like field on list
- [ ] Update `viewSettingsList()` to show delete confirmation line when `settingsDelete` is true
- [ ] Write tests for add server flow (form → save → list updated)
- [ ] Write tests for edit server flow (form → save → list updated)
- [ ] Write tests for delete server flow (d → y → removed)
- [ ] Write tests for delete cancel (d → n → unchanged)
- [ ] Write tests for validation errors (empty name, empty host, duplicate name)
- [ ] Write test for save error handling
- [ ] Run tests: `go test ./internal/tui/ -v` — must pass before task 6

### Task 6: Verify acceptance criteria

- [ ] Verify all server CRUD operations work end-to-end in tests
- [ ] Verify backward compatibility — existing tests still pass with new parameters
- [ ] Verify empty server list shows proper empty state
- [ ] Verify server select screen rebuilds correctly after settings changes
- [ ] Run full test suite: `go test ./... -count=1`

### Task 7: [Final] Update documentation

**Files:**
- Modify: `CLAUDE.md`

- [ ] Add settings editor section to CLAUDE.md documenting: new screens, model fields, navigation flow, state cleanup rules
- [ ] Document the `Save()` method and config path threading
- [ ] Move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Launch TUI with existing `servers.yml` — verify `s` key opens settings, servers display correctly
- Add a new server via the form — verify it appears in the server list and in `servers.yml`
- Edit an existing server — verify changes persist
- Delete a server — verify removal from list and file
- Test with no servers configured — verify empty state and add flow
- Test color picker — verify cycling and visual preview
- Test validation — try saving with empty name/host, duplicate name

# Move Color from Server to Group Level

## Overview
- Move the `color` parameter from per-server to per-group configuration in `servers.yml`
- Introduces a first-class `Group` struct so groups are real entities, not just implicit string tags
- Ungrouped servers retain per-server `color` for standalone badge coloring
- Transparent migration: old configs auto-migrate on load, saved in new format on next write

## Context (from discovery)
- Files involved: `internal/config/config.go`, `internal/config/config_test.go`, `internal/tui/app.go`, `internal/tui/app_test.go`
- Current `Server` struct has `Color` and `Group` as independent string fields
- `buildServerEntries()` groups servers by `Group` string; no `Group` object exists
- Settings editor form has 5 fields: name, host, project_dir, group (text inputs) + color (cycle picker)
- Color resolution: `m.serverColor = server.Color` at 2 set locations (lines ~464, ~633) and 3 clear locations (lines ~480, ~616, ~664) in `app.go`; `serverBadge()` reads at line ~1602
- `serverBadge()` / `serverBadgeStyle()` in `styles.go` consume the resolved color string

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility via transparent migration

## Testing Strategy
- **Unit tests**: required for every task
- Config tests: validation rules, migration, `GroupColor()`, round-trip save/load
- TUI tests: color resolution from group vs server, settings form behavior with groups

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with + prefix
- Document issues/blockers with ! prefix
- Update plan if implementation deviates from original scope

## Solution Overview

### New YAML format
```yaml
groups:
  - name: production
    color: red
  - name: staging
    color: yellow

servers:
  - name: dev-box
    host: dev.example.com
    color: cyan              # ungrouped, own color

  - name: prod-1
    host: prod1.example.com
    group: production        # color comes from group

  - name: staging-1
    host: stg1.example.com
    group: staging
```

### Color resolution logic
1. Server has `group` set -> look up group's color via `Config.GroupColor()`
2. Server has no `group` -> use `server.Color` directly
3. Fallback: empty string (plain breadcrumb, no badge)

### Migration (on Load)
1. Parse YAML normally (old format still valid)
2. Run `migrate()`: collect colors from grouped servers (first-server-wins), create `Group` entries, strip `color` from grouped servers. Also auto-creates groups for any referenced group names missing from `Groups`.
3. Validate as usual (no "server has group AND color" rule needed since migration handles it)
4. Next `Save()` writes new format with `groups:` key

**Known trade-off**: if two servers in the same group had different colors in the old format, the first server's color wins silently. This is acceptable since conflicting per-server colors within a group was already semantically inconsistent, and migration is a one-time event.

### Settings form behavior
- Ungrouped server: color picker works as today (field 4)
- Grouped server: color picker shows group color with "(group)" label, normal styling (editable); editing changes the group's color. Note: changing a group's color affects all servers in that group.
- Tab/shift-tab evaluates the current group input value at press time: if group is non-empty, field 4 is skipped (tab from field 3 wraps to field 0)
- New group name on save: auto-create `Group` entry in `config.Groups`
- Last server removed from group (via form save or `d` delete): auto-clean orphaned `Group` entry

## Implementation Steps

### Task 1: Add Group struct and Config.Groups field

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [x] Add `Group` struct with `Name` and `Color` fields (yaml tags)
- [x] Add `Groups []Group` field to `Config` struct with `yaml:"groups,omitempty"`
- [x] Add `GroupColor(groupName string) string` method on `Config`
- [x] Write tests for `GroupColor` (found, not found, empty color)
- [x] Run tests: `go test ./internal/config/ -v` - must pass before task 2

### Task 2: Update Validate() for group rules

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [x] Add group name uniqueness check in `Validate()`
- [x] Add group color validation (must be in `ValidColors` or empty)
- [x] Add check: if server has `group`, the group must exist in `config.Groups`
- [x] Write tests for each new validation rule (unique group names, invalid group color, missing group)
- [x] Verify existing validation tests still pass
- [x] Run tests: `go test ./internal/config/ -v` - must pass before task 3

### Task 3: Implement transparent migration in Load()

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [x] Add `migrate()` method on `Config` that: collects colors from grouped servers (first-server-wins), creates missing `Group` entries, strips `color` from grouped servers
- [x] Call `migrate()` in `Load()` after YAML parse, before returning (validation is caller's responsibility)
- [x] Write test: old-format config with grouped servers having colors -> loads correctly, groups created, server colors stripped
- [x] Write test: old-format config with conflicting colors in same group -> first-server-wins
- [x] Write test: config already in new format (has `groups:` key) -> migration is a no-op
- [x] Write test: ungrouped servers with color -> color preserved (not migrated)
- [x] Update `TestSave_RoundTrip` to use new format (grouped server color on group, ungrouped server color on server)
- [x] Run tests: `go test ./internal/config/ -v` - must pass before task 4

### Task 4: Update TUI color resolution

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] Add color resolution helper: if `m.config != nil` and server has group, use `m.config.GroupColor(server.Group)`, else use `server.Color` (nil-guard prevents panic in tests and non-config paths)
- [x] Update `preselectedConnectMsg` handler (line ~464) to use resolved color
- [x] Update `entryServer` selection handler (line ~633) to use resolved color
- [x] Update `connectResultMsg` error path (line ~480) - already clears, no change needed
- [x] Update `esc` back-navigation paths (lines ~616, ~664) - already clear, no change needed
- [x] Write tests: selecting a grouped server sets `serverColor` from group config
- [x] Write tests: selecting an ungrouped server sets `serverColor` from server field
- [x] Write test: color resolution when `m.config == nil` falls back to `server.Color`
- [x] Run tests: `go test ./internal/tui/ -v` - must pass before task 5

### Task 5: Update settings editor form for group-aware color

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] When editing a grouped server: set `settingsColor` from `config.GroupColor(srv.Group)` instead of `srv.Color`
- [x] When adding new server: `settingsColor` starts empty (no change)
- [x] Tab/shift-tab navigation: evaluate group input value at press time; skip field 4 when non-empty (tab from field 3 wraps to field 0)
- [x] Left/right on field 4 when grouped: cycle changes group's color (applied to group on save); show "(group)" label, normal styling
- [x] Update `enter` (save) handler: if server has group, apply `settingsColor` to the group's color instead of the server's color; auto-create group if new group name
- [x] Auto-cleanup: on save, remove `Group` entries that have no servers referencing them
- [x] Clear `server.Color` on save when server has a group
- [x] Update `tmpCfg` construction to include `Groups` (currently only copies `Servers`)
- [x] Update `d` (delete) handler in `screenSettingsList` to include `Groups` in `tmpCfg` and perform orphaned group cleanup before saving
- [x] Write tests for settings form save with grouped server (color goes to group, not server)
- [x] Write tests for auto-create group on new group name
- [x] Write tests for orphaned group cleanup (both form save and delete paths)
- [x] Write test for dynamic tab-skip: group field changes mid-form affects color picker availability
- [x] Run tests: `go test ./internal/tui/ -v` - must pass before task 6

### Task 6: Update settings list view display

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] In settings list rendering: resolve color display from group when server is grouped
- [x] Show "(group)" suffix or dimmed indicator for group-inherited colors in the list
- [x] Write test: settings list view shows group color for grouped servers
- [x] Write test: settings list view shows server color for ungrouped servers
- [x] Run tests: `go test ./internal/tui/ -v` - must pass before task 7

### Task 7: Verify acceptance criteria

- [x] Verify: ungrouped servers can set color directly
- [x] Verify: grouped servers get color from group, not server
- [x] Verify: old config format auto-migrates on load
- [x] Verify: settings editor correctly handles group color editing
- [x] Verify: new groups auto-created, orphaned groups auto-cleaned
- [x] Run full test suite: `go test ./... -count=1`

### Task 8: [Final] Update documentation

- [x] Update CLAUDE.md with new group color resolution, Group struct, migration behavior
- [x] Run tests: `go test ./... -count=1` - final verification
- [x] Move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Test with existing `servers.yml` that has per-server colors on grouped servers -> verify auto-migration
- Test settings editor: add grouped server, edit group color, verify all servers in group reflect change
- Test settings editor: remove last server from group, verify group is cleaned up
- Test badge rendering with group colors vs ungrouped server colors

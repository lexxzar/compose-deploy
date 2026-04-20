# Exit Confirmation for Remote SSH Connections

## Overview
- When connected to a remote server via password-based SSH, accidentally pressing `q` quits the TUI and forces the user to re-enter credentials
- Add an exit confirmation prompt that activates only when there's an active remote connection (`disconnectFunc != nil`)
- Local sessions and the server select screen quit immediately as before — no friction added

## Context (from discovery)
- Files involved: `internal/tui/app.go` (Model struct, `handleKey`, `View`, ~8 quit points), `internal/tui/app_test.go`
- Existing pattern: `confirming` bool + `pendingOp` for operation confirmations on the container screen
- `disconnectFunc` field on Model is non-nil when a remote SSH connection is active
- `serverName` field holds the display name for the connected server
- Settings form screen only uses `ctrl+c` (no `q`) since it has text inputs

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- Run tests after each change
- Maintain backward compatibility

## Testing Strategy
- **Unit tests**: TUI state transition tests using `Update()` with `tea.KeyMsg` — no TTY needed
- Tests cover: remote quit → confirmation, local quit → immediate, y/n/esc responses, key swallowing, server select bypass

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix

## Solution Overview
- Add `quitting bool` field to `Model`
- Add `tryQuit() (Model, tea.Cmd)` helper: if `disconnectFunc != nil`, set `quitting = true`; otherwise return `tea.Quit`
- Global intercept at top of `handleKey`: when `quitting == true`, handle `y` → quit, `n`/`esc` → cancel, swallow other keys
- In `View()`: when `quitting == true`, render a standalone confirmation screen (title + `"Disconnect from {serverName}? (y/n)"`) instead of the underlying screen — avoids fragile footer replacement across different screen layouts
- Replace all `return m, tea.Quit` calls (except server select screen) with `return m.tryQuit()`
- Note: settings screens (`screenSettingsList`, `screenSettingsForm`) are only reachable from server select (no remote connection), so `tryQuit()` there is a no-op safety guard — `disconnectFunc` is always nil

## Implementation Steps

### Task 1: Add `quitting` state, `tryQuit()` helper, and key intercept

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] Add `quitting bool` field to `Model` struct (near the `confirming` field at line ~181)
- [ ] Add `tryQuit()` method: check `disconnectFunc != nil` → set `quitting = true` and return, else return `tea.Quit`
- [ ] Add `quitting` intercept at the top of `handleKey()` (before `switch m.screen`): `y` → `tea.Quit`, `n`/`esc` → `quitting = false`, swallow other keys
- [ ] Replace `return m, tea.Quit` with `return m.tryQuit()` in 8 locations (all screens except `screenSelectServer`): `screenSelectProject`, `screenSelectContainers` normal, `screenSelectContainers` confirming, `screenProgress`, `screenLogs`, `screenConfig`, `screenSettingsList`, `screenSettingsForm`
- [ ] Reset `quitting = false` in `esc` back-navigation handlers that clear `disconnectFunc` (going back to server select)
- [ ] Write tests: `q` on remote connection → `quitting == true`, screen unchanged
- [ ] Write tests: `q` on local (no disconnectFunc) → `tea.Quit` returned
- [ ] Write tests: `quitting` + `y` → `tea.Quit`, `n` → cancel, `esc` → cancel, other key → swallowed
- [ ] Write test: `q` on server select screen → always `tea.Quit` directly
- [ ] Run tests: `go test ./internal/tui/ -count=1`

### Task 2: Add quit confirmation rendering in `View()`

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] Add `viewQuitConfirm()` method that renders a standalone confirmation screen: title + `"Disconnect from {serverName}? (y/n)"`
- [ ] In `View()`, add `if m.quitting` check at the top that returns `viewQuitConfirm()`
- [ ] Write test: `View()` output contains "Disconnect from" when `quitting == true` and `serverName` is set
- [ ] Run tests: `go test ./internal/tui/ -count=1`

### Task 3: Final verification

- [ ] Run full test suite: `go test ./... -count=1`

### Task 5: [Final] Update documentation

- [ ] Update CLAUDE.md with quit confirmation pattern description (in the TUI state machine section)
- [ ] Move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Test with an actual remote server connection (password-based SSH)
- Verify `q` shows prompt, `y` disconnects, `n` returns to normal
- Verify `ctrl+c` behaves the same as `q` on remote
- Test on each screen: project select, container list, progress, logs, config, settings

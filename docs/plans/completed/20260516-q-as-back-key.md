# `q` as back key on nested TUI screens

## Overview

Today the `q` key always quits the TUI. When you press `q` deep inside the app (logs viewer, config viewer, container picker, settings list), the app exits — on a remote SSH session, it first prompts to confirm the disconnect, but the destination is still "fully out of the app." This forces users to use `esc` for back-navigation only.

This change makes `q` a back-navigation key inside the app, aliased to `esc` on every nested screen. `q` continues to quit only at root screens (server-select, or the project/containers screens when running standalone without a `~/.cdeploy/servers.yml`). `ctrl+c` is untouched — it remains the unambiguous quit-from-anywhere shortcut and still triggers the remote-disconnect confirmation prompt.

The result is the familiar lazygit/k9s convention: `q` always does something useful (back, or quit if you're already at the top), `ctrl+c` is the hard exit.

## Context (from discovery)

Files involved:
- `internal/tui/app.go` — `handleKey()` and per-screen `case "q", "ctrl+c"` lines; help-footer strings rendered in `view*()` functions
- `internal/tui/app_test.go` — existing `TestQuitConfirmation_*` tests, plus new tests for back-nav behavior
- `CLAUDE.md` — "Exit confirmation for remote connections" section describes current `q`/`ctrl+c` behavior
- `README.md` — key-binding reference

Related patterns:
- Bubble Tea `Update()`/`handleKey()` dispatch keyed on `m.screen`
- Existing `esc` handlers already perform full state cleanup (logs ctx cancel, `disconnectFunc()`, `refreshStatus()`, confirmation reset, etc.) — the new `q` behavior delegates to them via a key-rewrite, no duplication
- Tests use stdlib `testing` and call `Update()` with synthetic `tea.KeyMsg` — no TTY required

Existing tests to update:
- `TestQuitConfirmation_AllRemoteScreens` (app_test.go:5393) — currently asserts `q` on every nested screen triggers `m.quitting = true` on a remote connection. Under the new behavior, only `ctrl+c` does that; `q` does back-nav. The test must drop the `q` cases for nested screens (they move to a new back-nav test) and keep the `ctrl+c` cases.
- `TestQuitConfirmation_ServerSelectAlwaysQuitsDirectly` (app_test.go:5379) — `q` on server-select still quits; unchanged.
- `TestQuitConfirmation_ProgressInProgressIgnoresQ` (app_test.go:5573) — `q` on `screenProgress` while running is still a no-op; unchanged **provided** the pre-dispatch has the `screenProgress` carve-out (see Technical Details). Without that carve-out, `q` would be rewritten to `esc`, which cancels the operation — breaking this test and silently aborting deploys.
- `TestQuitConfirmation_LocalQuitReturnsQuitMsg` (app_test.go:5535) — `q` on standalone containers (`servers=nil`, composer set, `showPicker=false`) still produces `tea.QuitMsg` via the `!showPicker && !confirming` carve-out; passes unchanged.

Help-footer strings rendered in `view*()` functions that say "q quit" on nested screens (need to read "q back" instead, except on root screens): app.go lines ~1536, ~1810, ~1812, ~1820, ~1822, ~1849, ~1851, ~1890, ~1892, ~2039, ~2082, ~2119, ~2151. The `screenSelectServer` help line (~1763) keeps "q quit". Settings form (~2281) is unchanged (no `q` mention).

## Development Approach

- **testing approach**: Regular (code change + tests in the same task — pure-function `Update()` testing, no TDD ceremony needed)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every code task MUST include new/updated tests** for the behavior changed in that task
- **CRITICAL: all tests must pass before starting next task** — run `go test ./internal/tui/ -count=1 -v` between tasks
- **CRITICAL: update this plan file when scope changes during implementation**
- maintain backward compatibility for `esc` and `ctrl+c` (no changes to either)

## Testing Strategy

- **unit tests**: required in Task 1 (covers all q-key behavior changes). Pure `Update()` calls with `tea.KeyMsg` — no Docker, no TTY, no goroutines.
- **e2e tests**: project has none (TUI is tested via `Update()` directly). Manual verification noted in Post-Completion.
- **regression check**: run full `go test ./... -count=1` in Task 3 to confirm no other package is affected.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

A single pre-dispatch block at the top of `handleKey()` rewrites the key `q` to `esc` for nested screens, with two explicit "I'm the root" carve-outs that return `tea.Quit` directly. Existing `esc` handlers are left intact — they already do the right cleanup. Each per-screen `case "q", "ctrl+c": return m.tryQuit()` line is reduced to `case "ctrl+c": return m.tryQuit()`. The `q` literal disappears from those case labels because it's already been rewritten to `esc` (or returned-from) by the pre-dispatch.

Key design choices:
- **Rewrite at the top, not per-screen**: one place to read the rule, zero duplication, new screens get the behavior for free (just write an `esc` handler).
- **Carve-outs are explicit, not inferred**: `screenSelectProject` with no servers and `screenSelectContainers` with `!m.showPicker` (and `!m.confirming`) are the only two "root" scenarios where `esc` would no-op. Both explicitly return `tea.Quit` for `q`.
- **Settings form excluded**: `q` must remain typeable into textinput fields (server names like `qa-prod`). The pre-dispatch leaves `screenSettingsForm` alone.
- **Disconnect confirmation reachable via `ctrl+c`**: no functional regression for users who want the safety prompt.

Side-effect bug fixes (intentional):
- `q` while `confirming`/`pendingExec` on the container screen now cancels the confirmation (matches `esc`). Today it triggers `tryQuit`, which is jarring mid-action.
- `q` during the settings-list delete-confirm now cancels the delete prompt (matches `esc`). Today it's swallowed.

## Technical Details

**Pre-dispatch block** (inserted in `handleKey()` after the existing `quitting` intercept at app.go:637-648; uses the local `key` variable from `msg.String()` at line 635):

```go
// q acts as a back key inside the app. It quits only when there is
// no parent screen to navigate to (server-select, or the project /
// containers screens when standalone). The settings form is excluded
// so q can be typed into text inputs. screenProgress while running is
// also excluded so q cannot cancel an in-flight operation.
if key == "q" {
    switch m.screen {
    case screenSelectServer:
        return m, tea.Quit
    case screenSelectProject:
        if len(m.servers) == 0 {
            return m, tea.Quit
        }
        key = "esc"
    case screenSelectContainers:
        if !m.showPicker && !m.confirming {
            return m, tea.Quit
        }
        key = "esc"
    case screenProgress:
        if !m.done && !m.failed {
            return m, nil // no-op while running — esc cancels here, q must not
        }
        key = "esc"
    case screenSettingsForm:
        // fall through — textinput consumes it
    default:
        key = "esc"
    }
}
```

The `screenProgress` carve-out matters: the existing `esc` handler at app.go:1153-1168 cancels the operation when not done/failed (via `m.cancel()`). Without this carve-out, rewriting `q → esc` would silently abort deploys. Today both `q` and `ctrl+c` are no-ops mid-operation (app.go:1149-1152 only calls `tryQuit` when done/failed); preserving that for `q` keeps consistency with `ctrl+c`.

**Per-screen edits** — drop `"q"` from these case labels (turning each into `case "ctrl+c":`):
- `screenSelectProject` (line 705)
- `screenSelectContainers` confirming branch (line 755)
- `screenSelectContainers` idle branch (line 775)
- `screenProgress` (line 1149)
- `screenLogs` (line 865)
- `screenConfig` (line 908)
- `screenSettingsList` (line 988)

**Untouched**:
- `screenSelectServer` (~line 653) — `case "q", "ctrl+c": return m, tea.Quit` is correct as-is.
- `screenSettingsForm` (~line 1041) — `case "ctrl+c"` only; `q` already passes through to the focused textinput via the `default:` branch.
- Every `esc` handler — they do the cleanup work for the new `q` path too.
- `tryQuit()` and the `quitting` state machine — `ctrl+c` still uses them.

**Help-footer string updates** in `view*()` functions: on nested screens that previously rendered `"q quit"`, change to `"q back"`. On root screens (`screenSelectServer`, and the standalone fall-through cases), keep `"q quit"`. Specific lines:

| Line | Current | Change to |
|---|---|---|
| 1536 | `back := "q quit"` (quit-confirm view) | leave — this is the quit-confirm prompt itself |
| 1763, 1765 | `"q quit"` on server-select | leave (root) |
| 1810, 1820 | `"q quit"` (some flows) | `"q back"` when the surrounding flow has a parent; leave if it's a root flow — inspect each `if` to decide |
| 1812, 1822 | `"esc back  q quit"` | `"q back"` (q is now esc; "esc back" becomes redundant — collapse to one entry) |
| 1849, 1851 | project picker help | `"q back"` if servers exist, else `"q quit"` |
| 1890, 1892 | container help | `"q back"` if `showPicker` (parent exists), else `"q quit"` (standalone root) |
| 2039 | `back := "q quit"` (standalone-containers idle help, selected when `!m.showPicker`) | **leave** — this is the standalone-containers root per the `!showPicker && !confirming` carve-out; `q` continues to quit here |
| 2082 | logs help (`"q quit"` appended) | `"q back"` |
| 2119 | config help (`"q quit"` appended) | `"q back"` |
| 2151 | progress done/failed help: `"esc back  •  q quit"` | `"q back"` (collapse — q is alias for esc; mid-progress this line isn't rendered) |
| 2233 | settings list `"esc back"` | optionally `"esc/q back"`; leaving as-is is also acceptable |
| 2281 | settings form `"esc discard"` | unchanged (q is typed into inputs, not a binding) |

Exact text choices to be finalised when reading the surrounding code in Task 2 — the principle is "back" on nested, "quit" on root.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code change + tests in `internal/tui/`, help-footer updates in `internal/tui/app.go`, docs updates to `CLAUDE.md` and `README.md`, plan move to completed.
- **Post-Completion** (no checkboxes): manual smoke test in a real terminal (TUI behaviour can't be fully validated via unit tests — viewport rendering, exec-process suspension, etc., need a TTY).

## Implementation Steps

### Task 1: Add q→esc pre-dispatch and update screen handlers

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] add the q→esc pre-dispatch block in `handleKey()` immediately after the existing `quitting` intercept (the `if m.quitting { ... }` block), with carve-outs for `screenSelectServer` (quit), `screenSelectProject` with no servers (quit), `screenSelectContainers` with `!showPicker && !confirming` (quit), `screenProgress` while `!done && !failed` (no-op — must not cancel), and `screenSettingsForm` (pass through to textinput)
- [x] in `screenSelectProject` handler, change `case "q", "ctrl+c": return m.tryQuit()` to `case "ctrl+c": return m.tryQuit()`
- [x] in `screenSelectContainers` handler (both confirming sub-block and idle branch), change both `case "q", "ctrl+c": return m.tryQuit()` lines to `case "ctrl+c": return m.tryQuit()`
- [x] in `screenProgress` handler, change `case "q", "ctrl+c":` (which conditionally calls `m.tryQuit()`) to `case "ctrl+c":` with the same conditional body
- [x] in `screenLogs` handler, change `case "q", "ctrl+c": return m.tryQuit()` to `case "ctrl+c": return m.tryQuit()`
- [x] in `screenConfig` handler, change `case "q", "ctrl+c": return m.tryQuit()` to `case "ctrl+c": return m.tryQuit()`
- [x] in `screenSettingsList` (idle branch, not the delete-confirm sub-block), change `case "q", "ctrl+c": return m.tryQuit()` to `case "ctrl+c": return m.tryQuit()`
- [x] update existing `TestQuitConfirmation_AllRemoteScreens` (app_test.go:5393): remove every test case whose key is `"q"` on a nested screen — those scenarios are now back-nav, not disconnect-prompt — and keep only the `ctrl+c` cases (rename test if helpful, e.g. `TestCtrlCConfirmation_AllRemoteScreens`)
- [x] add `TestQBackNavigation_ContainerScreen`: `screenSelectContainers` with `showPicker = true` and services loaded; press `q`; assert `screen == screenSelectProject`, `composer == nil`, `services == nil`
- [x] add `TestQBackNavigation_ContainerScreenCancelsConfirming`: `screenSelectContainers` with `confirming = true`, `pendingOp = runner.Deploy`; press `q`; assert `confirming == false`, `screen == screenSelectContainers`, no `tea.Quit`
- [x] add `TestQBackNavigation_LogsScreen`: `screenLogs` with `logsService = "nginx"`; press `q`; assert `screen == screenSelectContainers`, `logsService == ""`
- [x] add `TestQBackNavigation_ConfigScreen`: `screenConfig` with `configContent` set; press `q`; assert `screen == screenSelectContainers`, `configContent == nil`
- [x] add `TestQBackNavigation_SettingsList`: `screenSettingsList` with config loaded; press `q`; assert `screen == screenSelectServer`
- [x] add `TestQBackNavigation_SettingsListCancelsDelete`: `screenSettingsList` with `settingsDelete = true`; press `q`; assert `settingsDelete == false` and `screen == screenSettingsList` (the existing `esc` handler at app.go:981-983 only resets the delete flag — it does not navigate)
- [x] add `TestQBackNavigation_ProjectScreen`: `screenSelectProject` with at least one server in `m.servers` and a `disconnectFunc` that increments a captured counter; press `q`; assert `screen == screenSelectServer`, `disconnectFunc == nil` after; invoke the returned `tea.Cmd` and assert (a) it returns a `disconnectDoneMsg{}` and (b) the counter incremented to 1 (matches the existing `esc` behavior at app.go:725-730)
- [x] add `TestQBackNavigation_ProgressDoneReturnsToContainers`: `screenProgress` with `done = true`; press `q`; assert `screen == screenSelectContainers`
- [x] add `TestQOnProgressWhileRunningIsNoop`: `screenProgress` with `done = false`, `failed = false`, a `cancel` function spy; press `q`; assert `screen == screenProgress` (unchanged), `cancel` was **not** called, no `tea.Cmd` returned. This guards the `screenProgress` carve-out — without it, `q→esc` rewrite would cancel the deploy.
- [x] add `TestQQuitsAtRoot_ProjectScreenNoServers`: `screenSelectProject` with `servers = nil`; press `q`; assert the returned `tea.Cmd` produces `tea.QuitMsg`
- [x] add `TestQQuitsAtRoot_ContainerScreenStandalone`: `screenSelectContainers` with `showPicker = false`, `confirming = false`; press `q`; assert the returned `tea.Cmd` produces `tea.QuitMsg`
- [x] add `TestQTypedIntoSettingsFormInput`: `screenSettingsForm`, focus on the name input (`settingsField = 0`), input initially empty; send `tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}`; assert `settingsInputs[0].Value()` contains `"q"` and `screen == screenSettingsForm`
- [x] add `TestCtrlCStillTriggersDisconnectPrompt`: on `screenSelectContainers` with `disconnectFunc != nil`; press `ctrl+c`; assert `quitting == true`, no `tea.Quit` yet (confirms the safety prompt path still works)
- [x] add `TestQDuringQuittingPromptStillSwallowed`: `m.quitting = true` (any screen), press `q`; assert `quitting` stays true, no `tea.Cmd` returned (the existing intercept at app.go:639-648 runs before the new pre-dispatch and swallows `q`)
- [x] update help-footer strings in `view*()` functions per the table in Technical Details: nested screens render `"q back"`, root screens keep `"q quit"`. Read the surrounding `if` conditions to choose the right wording for each location.
- [x] run `go test ./internal/tui/ -count=1 -v` — must pass before next task

### Task 2: Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`

- [x] in `CLAUDE.md`, update the "Exit confirmation for remote connections" section: clarify that only `ctrl+c` (not `q`) shows the disconnect prompt on nested screens, because `q` now does back-navigation. Note the new pre-dispatch rule and the two root carve-outs.
- [x] in `CLAUDE.md`, optionally add a one-line note in the "TUI state machine" section: `q` is a back key on nested screens (alias for `esc`); quits only on root screens.
- [x] in `README.md`, update the key-bindings reference: change the description of `q` from "quit" to "back inside the app; quit at root screens (server picker / standalone)"; add an explicit `ctrl+c` line for "quit the app (prompts to confirm disconnect when connected to a remote server)" if the README doesn't already mention `ctrl+c`
- [x] no test changes — documentation only

### Task 3: Verify acceptance criteria

- [x] verify the new behavior table from the plan Overview matches actual behaviour: `q` is back on all nested screens, `q` quits on root screens, `q` is typed in settings form, `ctrl+c` still prompts to disconnect
- [x] run full test suite: `go test ./... -count=1`
- [x] verify no regressions in non-TUI packages (cmd, compose, runner, config)
- [x] manually inspect `git diff internal/tui/app.go` to confirm no stray `case "q"` remains where it shouldn't, and no `tryQuit()` call was missed

### Task 4: Final — move plan to completed

- [x] `mkdir -p docs/plans/completed`
- [x] `mv docs/plans/20260516-q-as-back-key.md docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only.*

**Manual verification** (TTY-only behaviour not covered by `Update()` unit tests):
- Open the TUI with at least one configured server. Navigate `server → project → containers → logs`. Press `q` four times — should walk back to server-select (with the SSH disconnect happening silently on the `project → server` transition), then quit from server-select.
- On the container screen, press `r` to enter the restart confirmation, then press `q` — should cancel the confirmation, *not* trigger any quit/disconnect prompt.
- On a remote session, press `ctrl+c` from the container screen — should show the "Disconnect from {server}? (y/n)" confirmation prompt.
- On the settings form, type a server name containing `q` (e.g. `qa-prod`) — letter should appear in the input, not navigate back.
- Run standalone (no `~/.cdeploy/servers.yml`) from a directory with a `compose.yaml`. The TUI starts on the container screen. Press `q` — should quit (since containers is the root in this mode).

**Changelog note**:
- Users who currently mash `q` to fully exit from deep inside the app will now need to press it 2–3 times (or use `ctrl+c`). Worth mentioning in release notes / next CHANGELOG entry.

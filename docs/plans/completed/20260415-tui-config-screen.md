# TUI Config Screen — View and Edit Compose Config

## Overview
- Add a `screenConfig` to the TUI that lets users view and edit docker-compose configuration files on both local and remote servers
- Accessible via `c` key on the container screen (no confirmation, read-only by default)
- Toggle between raw file content and resolved/interpolated output (`docker compose config`)
- Edit by shelling out to `$EDITOR` via `tea.ExecProcess`, with post-edit validation
- TUI only — no CLI subcommand (CLI users can already `docker compose config` or `ssh host cat file` directly)

## Context (from discovery)
- Files/components involved:
  - `internal/tui/app.go` — new `ConfigProvider` interface, screen, model fields, key handlers, enter/exit logic
  - `internal/compose/compose.go` — local implementation of `ConfigProvider`
  - `internal/compose/remote.go` — remote SSH implementation of `ConfigProvider`
  - `internal/tui/styles.go` — any new styles if needed
  - `cmd/root.go` — wire `ConfigProvider` into TUI model
- Related patterns: `screenLogs` (viewport + action keys + data fetch), `tea.ExecProcess` (SSH password prompts), `ConnectCallback` (injecting remote capabilities)
- Dependencies: `charmbracelet/bubbles/viewport`, existing `compose.composeFiles` candidates list

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility — existing `Composer` interface is untouched

## Testing Strategy
- **unit tests**: required for every task
  - `internal/compose/`: test command construction for ConfigFile, ConfigResolved, EditCommand, ValidateConfig without executing commands (use `SetTestHooks`)
  - `internal/tui/`: test screenConfig state transitions via `Update()` with `tea.KeyMsg` — entering, toggling, esc cleanup
  - `cmd/`: verify wiring if needed

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with + prefix
- Document issues/blockers with !! prefix
- Update plan if implementation deviates from original scope

## Solution Overview

### ConfigProvider Interface (in `tui` package)
```go
type ConfigProvider interface {
    ConfigFile(ctx context.Context) ([]byte, error)
    ConfigResolved(ctx context.Context) ([]byte, error)
    EditCommand(ctx context.Context) (*exec.Cmd, error)
    ValidateConfig(ctx context.Context) error
}
```
Defined in `tui` package (not `runner`) because it imports `os/exec` via `*exec.Cmd` return type and is TUI-only — the `runner` package currently only imports `context` and `io` and should stay clean. Both `Compose` and `RemoteCompose` implement it. Separate from `Composer` — no mock updates needed in runner tests.

### File Discovery
- Local: probe `composeFiles` candidates (`compose.yml`, `compose.yaml`, `docker-compose.yml`, `docker-compose.yaml`) via `os.Stat` in `ProjectDir`. Reuse existing `composeFiles` var; `findComposeFile()` is similar to `HasComposeFile()` but returns the path
- Remote: single SSH command that tests all candidates at once: `ssh host 'for f in compose.yml compose.yaml docker-compose.yml docker-compose.yaml; do test -f "$projDir/$f" && echo "$f" && break; done'` — avoids 4 sequential SSH round-trips

### Editor Construction
- Local: `exec.Command($EDITOR || $VISUAL || "vi", filePath)`
- Remote: `ssh -t -S <socket> -o ControlMaster=no <host> 'cd <projDir> && ${EDITOR:-vi} <filename>'`

### Validation Error Capture
- `ValidateConfig` must capture stderr (not just exit code) so users see *why* validation failed (YAML syntax error, undefined variable, etc.)
- Local: use `cmd.CombinedOutput()` or capture stderr separately via `outputCmd` hook
- Remote: same approach through `remoteCommand()` — stderr flows through SSH

### TUI Screen
- New `screenConfig` constant
- Model fields: `configContent`, `configResolved`, `configViewport`, `configShowRes`, `configErr`, `configValid *bool`, `configValidMsg`, `configSession uint64` (monotonic counter for stale message rejection, matching `logsSession` pattern per CLAUDE.md)
- Keys: `r` toggle raw/resolved, `e` edit, `esc` back, `q` quit, viewport scroll keys
- Lazy-fetch resolved content on first `r` press, cache it
- Post-edit: refresh raw content + validate, show status line
- Error handling: if `EditCommand()` returns error (no compose file, etc.), show in `configErr` and stay on screen

### Wiring (type-assertion approach)
- No parallel `ConfigProviderFactory` — since both `Compose` and `RemoteCompose` implement `ConfigProvider`, the TUI type-asserts `composer.(ConfigProvider)` when entering the config screen
- If assertion fails (e.g., test mock), the `c` key is disabled
- This avoids maintaining parallel factories in lockstep and is simpler than the `ComposerFactory`/`ConfigProviderFactory` dual pattern

### Backward-Navigation Cleanup
Per CLAUDE.md, all four cleanup sites must clear config-related state:
1. `esc` from project screen — clear config fields
2. `esc` from container screen (back to project) — clear config fields
3. `entryLocal` handler in server picker — clear config fields
4. `connectResultMsg` error path — clear config fields

Since we use type-assertion from `composer` (no separate `configProvider` field to track), cleanup is simpler — config state is only populated when on `screenConfig` and cleared on `esc` from that screen. No stale closures to worry about.

## Implementation Steps

### Task 1: Add ConfigProvider interface and local Compose implementation

**Files:**
- Modify: `internal/tui/app.go` (interface definition only)
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/compose_test.go`

- [x] Define `ConfigProvider` interface in `internal/tui/app.go` with `ConfigFile`, `ConfigResolved`, `EditCommand`, `ValidateConfig`
- [x] Add `findComposeFile()` helper on `Compose` that probes `composeFiles` candidates and returns the path (related to existing `HasComposeFile()` but returns the path instead of bool)
- [x] Implement `ConfigFile` on `Compose` — call `findComposeFile()`, then `os.ReadFile`
- [x] Implement `ConfigResolved` on `Compose` — run `docker compose config` via `command()`, capture output with `outputCmd` hook
- [x] Implement `EditCommand` on `Compose` — resolve editor (`$EDITOR`/`$VISUAL`/`vi`), return `exec.Command(editor, filePath)`
- [x] Implement `ValidateConfig` on `Compose` — run `docker compose config --quiet`, capture stderr for error messages (not just exit code)
- [x] Write tests for `findComposeFile` (file exists, file missing, multiple candidates — first match wins)
- [x] Write tests for `ConfigFile` (success, no compose file error)
- [x] Write tests for `ConfigResolved` (verify command args)
- [x] Write tests for `EditCommand` (verify editor resolution: EDITOR set, VISUAL fallback, vi default; verify command args)
- [x] Write tests for `ValidateConfig` (success, validation error with stderr message)
- [x] Run tests: `go test ./internal/compose/ -v`

### Task 2: Add ConfigProvider implementation on RemoteCompose

**Files:**
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/remote_test.go`

- [x] Add `findRemoteComposeFile()` on `RemoteCompose` — single SSH command that tests all candidates at once (`for f in ...; do test -f && echo && break; done`) to avoid multiple round-trips
- [x] Implement `ConfigFile` on `RemoteCompose` — call `findRemoteComposeFile()`, then `ssh cat <path>` via ControlMaster
- [x] Implement `ConfigResolved` on `RemoteCompose` — run `docker compose config` via `remoteCommand()`
- [x] Implement `EditCommand` on `RemoteCompose` — build `ssh -t -S socket -o ControlMaster=no host 'cd projDir && ${EDITOR:-vi} filename'`
- [x] Implement `ValidateConfig` on `RemoteCompose` — run `docker compose config --quiet` via `remoteCommand()`, capture stderr for error messages
- [x] Write tests for `findRemoteComposeFile` (verify SSH command construction)
- [x] Write tests for `ConfigFile` (verify SSH cat command args)
- [x] Write tests for `ConfigResolved` (verify remote command args)
- [x] Write tests for `EditCommand` (verify SSH -t command with shell escaping)
- [x] Write tests for `ValidateConfig` (verify remote command args)
- [x] Run tests: `go test ./internal/compose/ -v`

### Task 3: Add screenConfig to TUI — model, enter/exit, key handling

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] Add `screenConfig` constant to `screen` enum
- [x] Add config fields to `Model`: `configContent`, `configResolved`, `configViewport`, `configShowRes`, `configErr`, `configValid *bool`, `configValidMsg`, `configSession uint64`
- [x] Add `configFileMsg`, `configResolvedMsg`, `configEditDoneMsg`, `configValidateMsg` message types (all carry `session uint64` for stale message rejection)
- [x] Add `enterConfig()` method — type-assert `m.composer.(ConfigProvider)`, increment `configSession`, set `m.screen = screenConfig`, spawn tea.Cmd to fetch `ConfigFile()`
- [x] Handle `c` key on `screenSelectContainers` — if type-assertion succeeds, call `enterConfig()`; otherwise ignore (no config support for this composer)
- [x] Handle `configFileMsg` in `Update()` — check `m.screen == screenConfig && msg.session == m.configSession`, populate viewport with content, handle error
- [x] Handle `configResolvedMsg` in `Update()` — same session guard, cache resolved content, swap viewport if `configShowRes`
- [x] Add key handling for `screenConfig`: `esc` (back + cleanup), `q`/`ctrl+c` (quit), `r` (toggle), `e` (edit), viewport scroll delegation
- [x] Implement `r` toggle: if resolved not cached, spawn fetch cmd; if cached, swap viewport content
- [x] Implement `e` edit: call `EditCommand()` — if error, show in `configErr`; if success, return `tea.ExecProcess`; on return, re-fetch content + validate
- [x] Handle `configEditDoneMsg` — re-fetch `ConfigFile()` and spawn `ValidateConfig()` concurrently
- [x] Handle `configValidateMsg` — set `configValid`/`configValidMsg` from result (including stderr message on failure)
- [x] Implement `esc` cleanup: clear all `config*` fields, return to `screenSelectContainers`
- [x] Guard ALL async config message handlers with `m.screen == screenConfig && msg.session == m.configSession`
- [x] Write tests for `c` key entering config screen (type-assertion success path)
- [x] Write tests for `c` key when composer doesn't implement ConfigProvider (ignored)
- [x] Write tests for `esc` cleanup (all config fields cleared)
- [x] Write tests for `r` toggle state transitions
- [x] Write tests for stale message guard (message arrives after leaving screen — discarded)
- [x] Run tests: `go test ./internal/tui/ -v`

### Task 4: Add config screen rendering (View)

**Files:**
- Modify: `internal/tui/app.go`
- Possibly modify: `internal/tui/styles.go`

- [x] Add `viewConfig()` method following `viewLogs()` pattern
- [x] Show breadcrumb title: `cdeploy > [server] > [project] > config`
- [x] Show viewport with config content
- [x] Show loading state when content is being fetched
- [x] Show error state when `configErr` is set
- [x] Show validation status line at bottom: green "Config valid" / red "Config error: ..." / nothing if not checked
- [x] Show help bar with keys: `esc back  .  r raw/resolved  .  e edit  .  up/down scroll  .  q quit`
- [x] Dynamically adjust help text based on `configShowRes` state (show "r raw" vs "r resolved")
- [x] Add `screenConfig` case to `View()` switch
- [x] Write tests: `viewConfig()` output contains breadcrumb
- [x] Write tests: `viewConfig()` shows "Loading..." when content is nil and no error
- [x] Write tests: `viewConfig()` shows error when `configErr` is set
- [x] Write tests: help bar reflects `configShowRes` state ("r raw" vs "r resolved")
- [x] Run tests: `go test ./internal/tui/ -v`

### Task 5: Verify wiring works end-to-end

Since we use type-assertion (`composer.(ConfigProvider)`) rather than a separate factory, no explicit wiring in `cmd/root.go` is needed — both `Compose` and `RemoteCompose` already implement the interface. This task verifies the integration.

**Files:**
- Possibly modify: `cmd/root.go` (only if integration issues found)

- [x] Verify that `compose.Compose` satisfies `tui.ConfigProvider` (compile-time check via `var _ tui.ConfigProvider = (*compose.Compose)(nil)`)
- [x] Verify that `compose.RemoteCompose` satisfies `tui.ConfigProvider` (same compile-time check)
- [x] Verify the type-assertion in TUI works when composer is created via the existing `ComposerFactory` flow
- [x] Test that config screen works after project selection (composer changes → new type-assertion)
- [x] Run tests: `go test ./... -v`

### Task 6: Verify acceptance criteria

- [x] Verify `c` key opens config screen from container screen
- [x] Verify raw compose file content is displayed correctly
- [x] Verify `r` toggles to resolved config and back
- [x] Verify `e` opens editor (local) and returns to config screen with refreshed content
- [x] Verify `e` opens editor (remote) via SSH -t and returns correctly
- [x] Verify post-edit validation message appears
- [x] Verify `esc` returns to container screen cleanly
- [x] Verify edge cases: no compose file found, validation failure, editor not found
- [x] Run full test suite: `go test ./... -count=1`
- [x] Verify test coverage: `go test ./internal/compose/ ./internal/tui/ -coverprofile=cover.out && go tool cover -func=cover.out`

### Task 7: [Final] Update documentation

- [x] Update `CLAUDE.md` with new `screenConfig` documentation (TUI state machine section, key bindings)
- [x] Update `CLAUDE.md` with `ConfigProvider` interface documentation
- [x] Move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems*

**Manual verification:**
- Test config view/edit on a real local docker-compose project
- Test config view/edit on a real remote server via SSH
- Verify editor behavior with various `$EDITOR` values (vim, nano, code --wait)
- Verify validation catches intentionally broken YAML
- Test with multi-file compose configs (compose.yml + compose.override.yml) — resolved view should merge them, raw view shows the primary file

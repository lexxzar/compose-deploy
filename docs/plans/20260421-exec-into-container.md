# Exec into Container

## Overview
- Add ability to exec into a running container interactively (shell access)
- TUI: `x` key on container screen with confirmation, shells out via `tea.ExecProcess`
- CLI: `cdeploy exec [service] [-- command]` with `--server`/`-s` support
- Shell default: try `/bin/bash`, fall back to `/bin/sh`
- Works for both local and remote (SSH with `-t` for TTY)

## Context (from discovery)
- Files/components involved: `internal/compose/compose.go`, `internal/compose/remote.go`, `internal/tui/app.go`, `cmd/` (new `exec.go`)
- Related patterns: `ConfigProvider` interface (type-asserted at runtime), `tea.ExecProcess` for shelling out, `cmd/logs.go` for CLI subcommand pattern
- Dependencies: `runner.Composer` interface NOT modified (exec is not a pipeline op); new `ExecProvider` interface in `tui` package

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
- **unit tests**: command construction tests (verify args, env vars, flags without executing)
- `compose` package: test `ExecCommand` builds correct docker compose exec args
- `RemoteCompose`: test SSH wrapping includes `-t` and correct shell escaping
- TUI: test `x` key triggers confirmation, confirm builds exec command, stopped service shows warning
- CLI: test flag registration, arg parsing, `--` separator handling

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with + prefix
- document issues/blockers with warning prefix
- update plan if implementation deviates from original scope

## Solution Overview
- `ExecProvider` interface defined in `internal/tui/app.go` (like `ConfigProvider`)
- Both `Compose` and `RemoteCompose` implement `ExecCommand(ctx, service, command) (*exec.Cmd, error)`
- TUI type-asserts `composer.(ExecProvider)` when `x` is pressed; ignored if assertion fails
- CLI constructs the command directly (has concrete type)
- Default shell command: `[]string{"/bin/sh", "-c", "exec bash 2>/dev/null || exec sh"}` (use `/bin/sh` as outer binary so it works on Alpine/minimal images; tries bash first for better UX)
- Remote builds SSH command directly (like `EditCommand` pattern) with `-t` for TTY allocation

## Technical Details
- `ExecCommand` returns `(*exec.Cmd, error)` (not executing it) so caller controls execution; matches `EditCommand` signature
- Local: `docker compose exec <service> <command...>` with `CURRENT_UID` env and project dir
- Remote: `ssh -t -S <socket> -o ControlMaster=no <host> "cd <dir> && CURRENT_UID=... docker compose exec <service> <command...>"`
- CLI uses `cmd.Run()` with `Stdin`/`Stdout`/`Stderr` attached to `os.Stdin`/`os.Stdout`/`os.Stderr`
- TUI uses `tea.ExecProcess` which suspends Bubble Tea and gives full terminal to the process

## Implementation Steps

### Task 1: Add ExecCommand to local Compose

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/compose_test.go`

- [ ] Add `ExecCommand(ctx context.Context, service string, command []string) (*exec.Cmd, error)` method to `Compose`
- [ ] When `command` is empty, use default: `[]string{"/bin/sh", "-c", "exec bash 2>/dev/null || exec sh"}`
- [ ] Build command via `c.command(ctx, append([]string{"exec", service}, command...)...)`
- [ ] Write tests verifying command args for: default shell, custom command, standalone mode
- [ ] Run tests - must pass before next task

### Task 2: Add ExecCommand to RemoteCompose

**Files:**
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/remote_test.go`

- [ ] Add `ExecCommand(ctx context.Context, service string, command []string) (*exec.Cmd, error)` method to `RemoteCompose`
- [ ] When `command` is empty, use same default as local
- [ ] Build SSH command directly (like `EditCommand` pattern) — construct remote shell string with `cd`, `CURRENT_UID`, compose binary, `exec` subcommand, then `exec.CommandContext(ctx, "ssh", "-t", "-S", socketPath, "-o", "ControlMaster=no", host, remoteCmd)`
- [ ] Do NOT use `remoteCommand()` — it doesn't include `-t` and retrofitting is fragile
- [ ] Write tests verifying SSH args include `-t`, shell escaping is correct, default shell works
- [ ] Run tests - must pass before next task

### Task 3: Add ExecProvider interface and TUI handler

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] Define `ExecProvider` interface in `app.go`: `ExecCommand(ctx context.Context, service string, command []string) (*exec.Cmd, error)`
- [ ] Add `x` key handler in `screenSelectContainers`: type-assert `m.composer.(ExecProvider)` first (no-op if fails), then check `st, ok := m.svcStatus[m.services[m.svcCursor]]; ok && st.Running` — if not running, set `m.warning = "Container is not running"` and return
- [ ] If running, set `m.confirming = true` and `m.pendingExec = true`
- [ ] Modify the `enter` case in the `if m.confirming` block: if `m.pendingExec`, type-assert `ExecProvider`, call `ExecCommand(ctx, cursorService, nil)`, return `tea.ExecProcess`; otherwise fall through to existing `enterProgress`
- [ ] Define `execDoneMsg{err error}` type; use it as the `tea.ExecProcess` callback message
- [ ] Handle `execDoneMsg` in Update: reset `pendingExec`, call `refreshStatus()` to update container state
- [ ] Update confirmation view rendering: when `pendingExec`, show "Exec into <service>? enter confirm / esc cancel" instead of the operation + selected containers format
- [ ] Add `x exec` to help text on container screen (verify it fits in layout or split to second line)
- [ ] Write tests: `x` on running service triggers confirm, `x` on stopped service shows warning, confirm dispatches ExecProcess-like command, `x` when composer doesn't implement ExecProvider is no-op
- [ ] Run tests - must pass before next task

### Task 4: Add CLI exec subcommand

**Files:**
- Create: `cmd/exec.go`
- Create: `cmd/exec_test.go`
- Modify: `cmd/root.go`

- [ ] Create `newExecCmd()` following `cmd/logs.go` pattern
- [ ] Use: `exec <service> [-- command...]`, Args: `cobra.MinimumNArgs(1)`
- [ ] Parse args: first arg is service, everything after `--` (via `cmd.ArgsLenAtDash()`) is the command
- [ ] Implement `runExec(ctx, service, command)`: local/remote setup identical to `runLogs` pattern
- [ ] Call `ExecCommand`, attach stdin/stdout/stderr, run with `cmd.Run()`
- [ ] Register in `root.go` via `rootCmd.AddCommand(newExecCmd())`
- [ ] Write tests: flag registration, arg parsing with and without `--`, service name required, `--server` flag exists
- [ ] Run full test suite: `go test ./... -count=1`
- [ ] Run tests - must pass before next task

### Task 5: [Final] Update documentation

- [ ] Update CLAUDE.md with exec feature documentation (ExecProvider pattern, key binding, CLI usage)
- [ ] Move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Test TUI exec on a real running container (local)
- Test TUI exec on a remote server via SSH
- Test bash fallback (Alpine container with no bash)
- Test custom command: `cdeploy exec web -- rails console`
- Verify Ctrl+D / `exit` returns cleanly to TUI/shell

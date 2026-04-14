# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
go build -o cdeploy .          # Build binary
go test ./...                   # Run all tests
go test ./internal/runner/ -v   # Run tests for a single package
go test ./... -count=1          # Run all tests uncached
go mod tidy                     # After adding/removing imports
```

The root command launches a Bubble Tea TUI requiring a TTY, so `cmd/root_test.go` tests flag registration and subcommand structure rather than executing the root command directly.

## Architecture

cdeploy is a dual-mode tool: interactive TUI (no args) or non-interactive CLI (`deploy`/`restart`/`stop`/`list`/`logs` subcommands). Both modes share the same core through a three-layer architecture:

```
cmd/ (Cobra)  →  runner (pipeline orchestrator)  →  compose (docker CLI wrapper)
tui/ (Bubble Tea) ↗
```

**Key abstraction**: `runner.Composer` interface (defined in `internal/runner/runner.go`, implemented by `internal/compose/compose.go` for local and `internal/compose/remote.go` for SSH). This is the seam between the orchestrator and Docker — the runner is testable with mock implementations. Methods: `Stop`, `Remove`, `Pull`, `Create`, `Start`, `ListServices`, `ContainerStatus`, `Logs`.

**Event system**: `runner.Run()` sends `StepEvent` structs through a channel. The CLI consumer (`cmd/deploy.go:runOperation`) prints colored status lines. The TUI consumer (`internal/tui/app.go`) drives a Bubble Tea model through `tea.Cmd` that reads from the channel.

**TUI state machine**: Five screens: `screenSelectServer` → `screenSelectProject` → `screenSelectContainers` → `screenProgress` (for operations) or `screenLogs` (for log viewing). The server screen is shown whenever servers are configured in `~/.cdeploy/servers.yml`; selecting "Local" fast-tracks to containers if a local compose file exists. The model is a flat struct with a `screen` field determining which view/update logic runs. Operations are triggered via action keys on the container screen (`r` restart, `d` deploy, `s` stop) which enter a confirmation sub-state (`confirming` bool + `pendingOp`) before proceeding. The `l` key opens the log viewer for the cursor service without confirmation (read-only operation). Back-navigation (`esc`) resets downstream state; during confirmation, `esc` cancels without navigating back. The project picker is auto-skipped when the current directory contains a compose file and no servers are configured (backward compatible). `compose.ListProjects()` discovers projects via `docker compose ls -a --format json`.

**Backward-navigation state cleanup**: When navigating backward (`esc`) or handling errors (e.g. `connectResultMsg` failure), all mutable callbacks (`composerFactory`, `projectLoader`, `disconnectFunc`) must be explicitly cleared or restored to their local defaults (`localFactory`, `localComposer`). Bubble Tea's value-type model means stale closures silently persist across screen transitions. Every new callback field added to Model needs corresponding cleanup in: (1) `esc` from the project screen, (2) `esc` from the container screen, (3) `entryLocal` handler, (4) `connectResultMsg` error path.

**Stale async message guard**: Goroutines spawned by a screen (e.g., `readLogChunk` for `screenLogs`) may deliver messages after the user has navigated away. Message handlers for screen-specific async messages (`logChunkMsg`, `logDoneMsg`) must check `m.screen` and discard stale messages — otherwise they will read nil fields cleared by `esc` cleanup and panic. A monotonic `logsSession` counter provides additional stale-message rejection. This pattern applies to any future screen that spawns background goroutines.

**Status refresh**: When returning from the progress screen or logs screen, `refreshStatus()` re-fetches `ContainerStatus()` via a `statusMsg` to update the running/stopped dots on the container screen. `ContainerStatus()` errors are propagated through `statusMsg.err` and `servicesMsg.err` and displayed via `svcErr`; a successful refresh clears any prior error. Follow this pattern whenever an operation changes container state.

**Log streaming**: The `screenLogs` screen uses `io.Pipe` to bridge `Composer.Logs()` (blocking, writes to `io.Writer`) with Bubble Tea's message-driven architecture. A goroutine calls `composer.Logs(ctx, service, true, 50, pipeWriter)` and the `readLogChunk` tea.Cmd reads 4096-byte chunks from the pipe reader, sending `logChunkMsg` or `logDoneMsg`. The log context is derived from `m.ctx` so quitting the TUI cascades cancellation. Pressing `esc` cancels the log context, clears all `logs*` fields, and returns to the container screen.

**Log formatting**: The log viewer has two independent toggles: `w` for soft-wrap (default: on) and `p` for JSON pretty-print (default: off). These are implemented via `formatLogContent()` and `formatLogLines()` in `internal/tui/format.go` — pure functions that process raw log content through optional pretty-print and soft-wrap stages. `applyLogFormat()` incrementally formats only new complete lines (tracking `logsRawOff` into `logsContent`), appending to the cached `logsFormatted`. `fullReformat()` resets and reprocesses everything — called when toggles flip or the window resizes. When wrap is off, horizontal scrolling is enabled (`SetHorizontalStep(4)`). Docker compose log lines with format `<service> | <json>` have their JSON body indented with continuation lines padded to align under the body start.

**Docker Compose**: All docker interaction goes through `compose.Compose.command()` (local) or `compose.RemoteCompose.remoteCommand()` (SSH) which builds the correct command based on the `Standalone` field — `exec.CommandContext("docker", "compose", ...)` for plugin mode or `exec.CommandContext("docker-compose", ...)` for standalone mode. Both set `CURRENT_UID` env var (format `uid:gid` locally, `$(id -u):$(id -g)` on remote). Auto-detection via `Detect(ctx)` probes `docker compose version` first, then falls back to `docker-compose version`; the result is cached in the `Standalone` and `detected` fields. Detection is explicit — callers must call `Detect()` before first use; `command()`/`remoteCommand()` only read `Standalone`. `SetStandalone(bool)` allows inheriting the detection result into new instances. `SetTestHooks(run, output)` on both `Compose` and `RemoteCompose` injects test doubles. Exact subcommands: `stop`, `rm -f`, `pull`, `up --no-start`, `start`, `config --services`, `ps -a --format json`, `logs`, `ls -a --format json`. Empty container slice means operate on all services. `parseContainerStatus()` handles both JSON array output (Docker Compose v2.21+) and NDJSON (older versions). CLI subcommands reject `-a` combined with explicit container names.

**Remote SSH**: `RemoteCompose` wraps docker compose commands in SSH calls via a ControlMaster persistent socket (`/tmp/cdeploy-ctrl-{hash}-{pid}`). `ConnectCmd()` returns the SSH connect command for TUI's `tea.ExecProcess` (full terminal access for password prompts), `Connect()` runs it directly for CLI use, and `Close()` tears down the socket. All remote args are shell-escaped via `shellEscape()`. `CURRENT_UID` is evaluated on the remote host via `$(id -u):$(id -g)` to get the correct server-side UID/GID. The `Run()` function in `tui/app.go` calls `disconnectFunc` on exit to ensure socket cleanup. CLI uses `--server`/`-s` flag to select a server; TUI shows a server picker when servers are configured.

**Config**: `internal/config` loads `~/.cdeploy/servers.yml` defining remote servers with `name`, `host`, optional `project_dir`, and optional `group` fields. Servers with the same `group` are displayed together under a shared header in the TUI. `Load()` returns an empty config (not error) if the file doesn't exist. SSH-specific options (keys, jump hosts, tunnels) belong in `~/.ssh/config`.

**Operations**: Three operations — `Restart` (stop → rm → create → start), `Deploy` (stop → rm → pull → create → start), `StopOnly` (stop). All share the same `runner.Run()` pipeline; the step list varies by operation type.

**Health checks**: `ServiceStatus` has `Running bool` and `Health string` (values: `"healthy"`, `"unhealthy"`, `"starting"`, or `""` for no healthcheck). For scaled services, `Running` uses OR (any running = running) and `Health` uses worst-case priority (`unhealthy` > `starting` > `healthy`). The TUI shows health icons (♥/✗/~) next to status dots. The CLI `list` command shows the same icons.

**Multi-project discovery**: The `list` subcommand auto-discovers all compose projects when no `-C` is specified (both local and remote). Services are grouped by project with headers. Per-project errors are non-fatal — a warning is printed to stderr and the project is skipped. When `-C` is specified, only that project's services are shown in a flat list.

## Adding New Operations

1. Add constant to `Operation` enum in `internal/runner/runner.go`
2. Update `String()`, `Steps()`, and `buildSteps()` in the same file
3. Add action key case in `screenSelectContainers` handleKey in `internal/tui/app.go`
4. Add CLI subcommand in `cmd/deploy.go` and register in `cmd/root.go`
5. Tests: runner sequence test, cmd subcommand/flag tests, TUI action key + confirmation tests

## Adding New CLI Subcommands

See `cmd/list.go` and `cmd/logs.go` for the pattern. Each subcommand:
1. Creates a `*cobra.Command` in its own `new*Cmd()` function
2. Handles `--server` by loading config, calling `FindServer()`, creating `RemoteCompose`, connecting, and deferring `Close()`
3. Falls back to local `compose.New(dir)` when no server is specified
4. Is registered in `cmd/root.go` via `rootCmd.AddCommand()`

## Package Coupling

The TUI package depends on `runner.Composer` (interface), not `compose.Compose` or `compose.RemoteCompose` (concrete). For operations that need to create a `Composer` at runtime (e.g., project picker selecting a directory), use a `ComposerFactory` callback injected from `cmd/` — this keeps `tui` decoupled from `compose`. Similarly, `ProjectLoader` and `ConnectCallback` are injected by `cmd/` for remote server support. The `compose` package has separate package-level discovery functions (`ListProjects`, `HasComposeFile`) that don't require a `Compose` instance.

## Testing Approach

Tests use stdlib `testing` only — no testify or other test frameworks.

- `internal/runner/`: Mock `Composer` interface to verify step sequences and failure behavior without Docker
- `internal/compose/`: Test command construction (args, env vars, flags) without executing commands; `parseProjects()` and `parseContainerStatus()` extracted for testability; `RemoteCompose` tests verify SSH arg construction and shell escaping
- `internal/config/`: Test YAML parsing, validation, and lookup with temp files
- `internal/tui/`: Test model state transitions by calling `Update()` with `tea.KeyMsg` directly — no TTY needed; `format_test.go` tests `formatLogContent()`, `softWrapLine()`, and `prettyPrintLine()` as pure functions
- `internal/logging/`: Use `t.TempDir()` for file creation tests
- `cmd/`: Test flag registration, subcommand existence, and validation via `NewRootCmd()`

# SSH Connection String CLI Flag (`-S`/`--ssh`)

## Overview

Add a `-S`/`--ssh` flag to the cdeploy CLI that accepts an ad-hoc SSH connection string in the form `[user@]host[:port]`. This lets users (especially CI/automation) connect to a remote Docker host without pre-configuring it in `~/.cdeploy/servers.yml`.

**Problem solved:** today, every remote operation requires a named entry in `~/.cdeploy/servers.yml`. In CI environments the config file rarely exists, and creating it just to invoke a one-off deploy is friction. With `-S`, scripts can pass everything on the command line.

**Integration:** `-S` is mutually exclusive with the existing `-s`/`--server`. When `-S` is set, the subcommand bypasses config lookup, parses the string, builds a `RemoteCompose` directly, and runs the operation through the same `runner.Run()` pipeline as today. No TUI changes — strictly CLI subcommands (`deploy`, `restart`, `stop`, `list`, `logs`, `exec`).

## Context (from discovery)

- **Project:** Go CLI tool (`cdeploy`) wrapping `docker compose` for local + remote SSH targets. Cobra for CLI, Bubble Tea for TUI.
- **Existing `--server` wiring:** registered as a persistent flag in `cmd/root.go:171`; consumed by `cmd/deploy.go:139`, `cmd/exec.go:80`, `cmd/logs.go:72`, `cmd/list.go:328`. Each subcommand uses an injectable factory variable (`opNewRemote`, `execNewRemote`, `logsNewRemote`, `newRemote` — declared at `cmd/deploy.go:25`, `cmd/exec.go:18`, `cmd/logs.go:18`, `cmd/list.go:19`) as a test seam. Tests swap them out (e.g., `cmd/deploy_test.go:528`, `cmd/list_test.go:1333`). Any new helper MUST preserve these seams.
- **`RemoteCompose`:** `internal/compose/remote.go`. Constructor `NewRemote(host, projectDir)`. `Close()` takes no arguments (`remote.go:146`). 9 distinct `exec.CommandContext(ctx, "ssh", ...)` sites (lines 70, 89, 125, 149, 180, 294, 328, 378, 416). High-level methods like `ConfigResolved`, `ValidateConfig`, `ListProjects` go through `remoteCommand` (line 162) rather than building their own ssh argv. Has `SetTestHooks(run, output)` for capturing argv in tests.
- **Connect/Detect pairing:** every existing remote site calls `rc.Connect(ctx)` then `rc.Detect(ctx)` (e.g., `deploy.go:153/157`, `list.go:338/342`, `logs.go:86/90`). `resolveSSHRemote` must do the same.
- **Config:** `internal/config/config.go`. `Server` struct has `name`, `host`, optional `project_dir`, `group`, `color`. `Validate()` enforces uniqueness and color validity. **No changes** to config validation in this plan (originally proposed `@`-rejection in server names was dropped — backward-incompatible for marginal benefit since `--ssh` and `--server` are now separate flags).
- **Test posture:** stdlib `testing` only, no testify. No real SSH/Docker calls. Table-driven tests are the norm.
- **Patterns:** persistent flags on `rootCmd`; subcommands use `RunE`; errors bubble up; existing `--server` requires `--project-dir` or config `project_dir` and that pattern is mirrored here.

## Development Approach

- **Testing approach**: Regular (code first, then tests) — matches existing project style.
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional — required deliverable per task
  - cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change
- maintain backward compatibility — existing `-s`/`--server` users must see zero behavior change

## Testing Strategy

- **unit tests**: required for every task. Stdlib `testing`, table-driven where appropriate.
- **e2e tests**: project has none (per AGENTS/CLAUDE conventions, real SSH/Docker isn't exercised in tests). Skip.
- **regression guard**: every change to `RemoteCompose` argv construction MUST include a test asserting the existing argv shape is unchanged when `SSHExtraArgs` is nil.

Project test commands:
```bash
go test ./...
go test ./... -count=1
```

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

Three layers of change, each isolated:

1. **Parser** (`internal/config/sshtarget.go`) — pure function `ParseSSHTarget(s) (SSHTarget, error)` and helpers `SSHHost()` / `PortArgs()`. Zero dependencies, fully unit-testable.
2. **Composer extension** (`internal/compose/remote.go`) — add `SSHExtraArgs []string` field. Splice into every SSH argv build site (Connect, remoteCommand, Detect, exec/edit/config-find). Default nil = no behavior change.
3. **CLI wiring** (`cmd/remote.go` + `cmd/root.go` + 4 subcommand edits) — register the flag once on root; introduce `resolveRemote(cmd)` helper that consolidates `--ssh` vs `--server` resolution; replace duplicated blocks in `deploy/exec/logs/list` with one helper call.

This keeps the abstraction surface tiny (one new helper, one new field, one new flag) and avoids touching `runner` or `tui`.

## Technical Details

### `SSHTarget` struct

```go
type SSHTarget struct {
    User string // optional
    Host string // required
    Port int    // 0 if not specified
}

func ParseSSHTarget(s string) (SSHTarget, error)
func (t SSHTarget) SSHHost() string  // "user@host" or "host"
func (t SSHTarget) PortArgs() []string // []string{"-p","2222"} or nil
```

### Parser algorithm

1. Trim whitespace
2. Reject empty
3. Reject any input containing whitespace
4. Reject input starting with `[` ("IPv6 not supported")
5. Split on first `@` → optional user + remainder
6. Split remainder on **first** `:` → host + optional port (since IPv6 is rejected, host cannot contain `:`)
7. Validate user non-empty if `@` present; validate host non-empty
8. If port present: must be all digits, parse to int, must be in `1..65535`

### Parser test matrix

Happy paths:
- `user@host` → `{User:"user", Host:"host", Port:0}`
- `host` → `{Host:"host"}`
- `user@host:2222` → `{User:"user", Host:"host", Port:2222}`
- `host:2222` → `{Host:"host", Port:2222}`
- `deploy@10.0.0.1` → `{User:"deploy", Host:"10.0.0.1"}`

Errors (specific messages):
- `""` → `"ssh target is empty"`
- `"  "` / `"a b"` → `"ssh target must not contain whitespace"`
- `"@host"` → `"user is empty"`
- `"user@"` → `"host is empty"`
- `"user@host:abc"` → `"port \"abc\" is not a number"`
- `"user@host:0"` / `"user@host:99999"` → `"port N out of range (1-65535)"`
- `"[::1]:22"` → `"IPv6 not supported"`

### `SSHExtraArgs` splicing

In each SSH argv build site in `internal/compose/remote.go`, the args today look like:

```
ssh -S {socket} [-o ControlMaster=no] {host} {remote-cmd}
```

After change, when `SSHExtraArgs` is non-nil:

```
ssh -S {socket} [-o ControlMaster=no] {SSHExtraArgs...} {host} {remote-cmd}
```

Splice position: **immediately before the host argument** in every code path (Connect, Detect, remoteCommand, exec, edit, find-compose-file). This matches `ssh`'s expectation that options precede the destination.

### `resolveSSHRemote` contract

Scope-narrowed: this helper handles ONLY the `--ssh` path. The existing `--server` blocks in each subcommand stay untouched (zero regression risk on the well-tested config-based path). The mutex check happens in each subcommand at the top of `RunE`, before deciding which path to take.

Signature (factory injected to preserve existing test seams):
```go
func resolveSSHRemote(
    ctx context.Context,
    sshTarget, projectDir string,
    newRemote func(host, projDir string) *compose.RemoteCompose,
) (rc *compose.RemoteCompose, cleanup func(), err error)
```

Why a factory parameter: `cmd/deploy.go:25`, `cmd/exec.go:18`, `cmd/logs.go:18`, `cmd/list.go:19` each declare their own package-level factory variable (`opNewRemote`, `execNewRemote`, `logsNewRemote`, `newRemote`) that tests swap out (e.g., `deploy_test.go:528`, `exec_test.go:317`, `list_test.go:1333/1395/1457`). The helper takes the factory as a parameter so each caller passes its own. Calling `compose.NewRemote` directly would break those existing test seams.

Behavior:
- if `projectDir` empty → error `"--ssh requires --project-dir"`
- parse with `config.ParseSSHTarget(sshTarget)`; on error: `fmt.Errorf("invalid --ssh value %q: %w", sshTarget, err)`
- build `rc := newRemote(target.SSHHost(), projectDir)`
- set `rc.SSHExtraArgs = target.PortArgs()`
- call `rc.Connect(ctx)`; on error wrap as `fmt.Errorf("connecting to %s: %w", target.SSHHost(), err)`
- call `rc.Detect(ctx)`; on error return wrapped (matches existing pattern at `deploy.go:157`, `list.go:342`, `logs.go:90`)
- cleanup = `func() { rc.Close() }` (note: `Close()` takes no arguments, see `remote.go:146`)

### Mutex check (lives in each subcommand)

At the top of each subcommand's `RunE`, before any branching:
```go
if sshTarget != "" && serverName != "" {
    return fmt.Errorf("--ssh and --server are mutually exclusive")
}
```
A tiny helper `checkRemoteMutex(serverName, sshTarget string) error` can hold this — DRY across 4 subcommands without coupling to anything else.

### Subcommand RunE shape after the change

```go
if err := checkRemoteMutex(serverName, sshTarget); err != nil { return err }

var c runner.Composer
switch {
case sshTarget != "":
    rc, cleanup, err := resolveSSHRemote(ctx, sshTarget, projectDir, opNewRemote)
    if err != nil { return err }
    defer cleanup()
    c = rc
case serverName != "":
    // existing block — unchanged
    ...
default:
    // existing local fallback — unchanged
    ...
}
```

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): all parser, composer, CLI, test, and doc work — entirely within this codebase.
- **Post-Completion** (no checkboxes): manual smoke test against a real SSH host (one-off verification only — not automatable per project convention).

## Implementation Steps

### Task 1: Add `SSHTarget` parser

**Files:**
- Create: `internal/config/sshtarget.go`
- Create: `internal/config/sshtarget_test.go`

- [x] create `internal/config/sshtarget.go` with `SSHTarget` struct and `ParseSSHTarget`, `SSHHost`, `PortArgs` per Technical Details
- [x] implement parser per algorithm above (trim → empty → whitespace → IPv6 reject → split `@` → split last `:` → validate)
- [x] write table-driven happy-path tests for `ParseSSHTarget` covering all 5 cases listed in Parser test matrix
- [x] write table-driven error-path tests for `ParseSSHTarget` covering all 8 error cases (verify exact error message substrings)
- [x] write tests for `SSHHost()`: `{User:"u",Host:"h"}` → `"u@h"`; `{Host:"h"}` → `"h"`
- [x] write tests for `PortArgs()`: `{Port:0}` → nil; `{Port:2222}` → `["-p","2222"]`
- [x] run `go test ./internal/config/ -count=1` — must pass before next task

### Task 2: Add `SSHExtraArgs` field to `RemoteCompose` and splice into argv

**Files:**
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/remote_test.go`

- [x] add `SSHExtraArgs []string` field to `RemoteCompose` struct (with comment: extra ssh CLI args spliced immediately before host)
- [x] splice `r.SSHExtraArgs...` immediately before the host argument in EVERY SSH argv build site. Confirmed sites in `internal/compose/remote.go` (verified by grep for `exec.CommandContext(ctx, "ssh"`):
  - line 70: `Detect` plugin probe
  - line 89: `Detect` standalone probe
  - line 125: `ConnectCmd`
  - line 149: `Close`
  - line 180: `remoteCommand` (covers `Stop`, `Remove`, `Pull`, `Create`, `Start`, `Logs`, `ListServices`, `ContainerStatus`, `ConfigResolved`, `ValidateConfig`, `ListProjects` — all downstream)
  - line 294: `findRemoteComposeFile`
  - line 328: `ConfigFile`
  - line 378: `EditCommand`
  - line 416: `ExecCommand`
- [x] regression test: with nil `SSHExtraArgs`, existing argv shape is unchanged for each direct site (Detect plugin, Detect standalone, ConnectCmd, Close, remoteCommand, findRemoteComposeFile, ConfigFile, EditCommand, ExecCommand). Use existing `SetTestHooks` capture pattern; assert exact argv slices.
- [x] new test: `SSHExtraArgs = []string{"-p","2222"}` → argv contains `-p 2222` immediately before host in each of the 9 sites above
- [x] run `go test ./internal/compose/ -count=1` — must pass before next task

### Task 3: Add `resolveSSHRemote` and `checkRemoteMutex` helpers

**Files:**
- Create: `cmd/remote.go`
- Create: `cmd/remote_test.go`

- [x] create `cmd/remote.go` with `checkRemoteMutex(serverName, sshTarget string) error` returning `"--ssh and --server are mutually exclusive"` when both non-empty, else nil
- [x] add `resolveSSHRemote(ctx, sshTarget, projectDir string, newRemote func(host, projDir string) *compose.RemoteCompose) (*compose.RemoteCompose, func(), error)` per contract in Technical Details
- [x] order of operations in `resolveSSHRemote`: validate projectDir non-empty → parse target → call factory → set `SSHExtraArgs` → `Connect` → `Detect` → return `(rc, func(){ rc.Close() }, nil)`
- [x] on `Detect` failure, the returned cleanup must still close the connection — return cleanup AND error together, OR call `Close()` internally before returning the error (pick the latter — simpler, matches existing pattern)
- [x] test for `checkRemoteMutex`: both empty → nil; only ssh → nil; only server → nil; both set → error containing `"mutually exclusive"`
- [x] test for `resolveSSHRemote`: empty `projectDir` → error containing `"requires --project-dir"`
- [x] test for `resolveSSHRemote`: malformed ssh target → error wraps parser error, contains `"invalid --ssh value"`
- [x] test for `resolveSSHRemote` happy path: pass a stub `newRemote` factory that returns a `RemoteCompose` with `SetTestHooks` configured to no-op `runCmd` (so `Connect`/`Detect` succeed without real ssh). Assert returned `rc.Host == "user@host"`, `rc.ProjectDir == "/srv/app"`, `rc.SSHExtraArgs == ["-p","2222"]`. Assert cleanup is non-nil and calling it doesn't panic.
- [x] test for `resolveSSHRemote`: factory stub where `runCmd` returns error on `Connect` → helper returns error containing `"connecting to user@host"` and (since `Connect` failed) cleanup may be nil — pick a contract and document in code comment
- [x] run `go test ./cmd/ -count=1 -run "TestCheckRemoteMutex|TestResolveSSHRemote"` — must pass before next task

### Task 4: Register `--ssh` persistent flag on root

**Files:**
- Modify: `cmd/root.go`
- Modify: `cmd/deploy_test.go` (or new `cmd/root_test.go` cases)

- [x] in `cmd/root.go` (near line 171 where `--server` is registered), add `rootCmd.PersistentFlags().StringVarP(&sshTarget, "ssh", "S", "", "ad-hoc SSH connection string [user@]host[:port] (mutually exclusive with --server)")`
- [x] declare package-level `var sshTarget string` near existing `var serverName string`
- [x] add test case to existing flag-registration table in `cmd/root_test.go`: `{"ssh flag exists", "ssh", "S"}`
- [x] run `go test ./cmd/ -count=1` — must pass before next task

### Task 5: Wire `--ssh` into `deploy`, `restart`, `stop` (in `cmd/deploy.go`)

**Files:**
- Modify: `cmd/deploy.go`
- Modify: `cmd/deploy_test.go`

- [x] in `cmd/deploy.go` `RunE`, at the top: `if err := checkRemoteMutex(serverName, sshTarget); err != nil { return err }`
- [x] add a new branch ABOVE the existing `if serverName != ""` block: `if sshTarget != "" { rc, cleanup, err := resolveSSHRemote(ctx, sshTarget, projectDir, opNewRemote); if err != nil { return err }; defer cleanup(); c = rc }` and skip the `serverName`/local branches when this fires (use `switch`/`else if` chain)
- [x] verify `restart` and `stop` subcommands (which share `runOperation` in deploy.go) inherit the same code path automatically (they call into the same `RunE` body via shared logic) — confirm by reading the file structure
- [x] add test: deploy with `--ssh` and `--server` together returns mutex error containing `"mutually exclusive"`
- [x] add test: deploy with `--ssh` but no `-C` returns error containing `"requires --project-dir"`
- [x] add test: restart with `--ssh foo@bar` and `--server prod` together returns mutex error
- [x] add test: stop with `--ssh foo@bar` and `--server prod` together returns mutex error
- [x] add test: persistent `--ssh` flag is inherited by deploy/restart/stop subcommands (mirrors existing `--server` inheritance test at `deploy_test.go:172`)
- [x] run `go test ./cmd/ -count=1` — must pass before next task

### Task 6: Wire `--ssh` into `cmd/exec.go`

**Files:**
- Modify: `cmd/exec.go`
- Modify: `cmd/exec_test.go`

- [ ] mirror Task 5: add `checkRemoteMutex` at top, add `sshTarget != ""` branch above existing `serverName != ""` block, pass `execNewRemote` as the factory
- [ ] add test: exec with `--ssh` and `--server` together returns mutex error
- [ ] add test: exec with `--ssh` but no `-C` returns "requires --project-dir" error
- [ ] add test: persistent `--ssh` flag is inherited by exec subcommand
- [ ] run `go test ./cmd/ -count=1 -run TestExec` — must pass before next task

### Task 7: Wire `--ssh` into `cmd/logs.go`

**Files:**
- Modify: `cmd/logs.go`
- Modify: `cmd/logs_test.go` (create if absent, follow `exec_test.go` pattern)

- [ ] mirror Task 5: add `checkRemoteMutex` at top, add `sshTarget != ""` branch above existing `serverName != ""` block, pass `logsNewRemote` as the factory
- [ ] add test: logs with `--ssh` and `--server` together returns mutex error
- [ ] add test: logs with `--ssh` but no `-C` returns "requires --project-dir" error
- [ ] add test: persistent `--ssh` flag is inherited by logs subcommand
- [ ] run `go test ./cmd/ -count=1 -run TestLogs` — must pass before next task

### Task 8: Wire `--ssh` into `cmd/list.go`

**Files:**
- Modify: `cmd/list.go`
- Modify: `cmd/list_test.go` (create if absent)

- [ ] mirror Task 5: add `checkRemoteMutex` at top, add `sshTarget != ""` branch above existing `serverName != ""` block, pass `newRemote` as the factory. Note that `list` allows `projectDir` to be empty for multi-project discovery in the `--server` path — but `--ssh` requires `--project-dir`, which `resolveSSHRemote` enforces. After the helper returns, follow the same single-project routing as `list.go:347` since `--ssh` always implies a single project.
- [ ] add test: list with `--ssh` and `--server` together returns mutex error
- [ ] add test: list with `--ssh` but no `-C` returns "requires --project-dir" error
- [ ] add test: persistent `--ssh` flag is inherited by list subcommand
- [ ] run `go test ./cmd/ -count=1 -run TestList` — must pass before next task

### Task 9: Update documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`

- [ ] in README.md, document the `--ssh` flag in the CLI reference section: format `[user@]host[:port]`, mutually exclusive with `--server`, requires `--project-dir`, examples for each subcommand
- [ ] in README.md, add a brief "CI usage" note: requires passwordless SSH auth (configure via `~/.ssh/config` or `ssh-agent`); host-key verification still applies
- [ ] in CLAUDE.md, add a paragraph under the Remote SSH section describing `-S` as the ad-hoc complement to config-based `-s`, including the `SSHExtraArgs` mechanism in `RemoteCompose` and the `resolveSSHRemote`/`checkRemoteMutex` helpers in `cmd/remote.go`
- [ ] run `go test ./... -count=1` — full suite must pass

### Task 10: Verify acceptance criteria

- [ ] verify `cdeploy deploy -S deploy@host -C /srv/app` builds correct argv (manual `go build` + `--help` check)
- [ ] verify `cdeploy deploy -S host -C /srv/app` works without explicit user
- [ ] verify `cdeploy deploy -S host:2222 -C /srv/app` includes `-p 2222` in SSH argv
- [ ] verify `cdeploy deploy -s prod -S deploy@host -C /srv/app` errors with mutex message
- [ ] verify `cdeploy deploy -S deploy@host` (no `-C`) errors with "requires --project-dir"
- [ ] verify `cdeploy deploy` (no flags) still works in local mode (regression)
- [ ] verify `cdeploy deploy -s prod -C /srv/app` still works (regression — config-based path untouched)
- [ ] run `go test ./... -count=1`
- [ ] `go build -o cdeploy .` succeeds with no warnings

### Task 11: Move plan to completed

- [ ] move this plan to `docs/plans/completed/20260426-ssh-connection-string-cli.md`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual smoke test** (one-off, not automated per project convention):
- against a real SSH host with `~/.ssh/config` set up, run `cdeploy list -S user@host -C /srv/app` and confirm services list returns
- against the same host with a non-default port, run `cdeploy list -S user@host:2222 -C /srv/app` and confirm port is honored
- run an actual `cdeploy deploy -S user@host -C /srv/app` against a non-production target and confirm full deploy lifecycle (stop → rm → pull → create → start) succeeds

**External system updates:** none — this is a self-contained CLI change with no consuming projects.

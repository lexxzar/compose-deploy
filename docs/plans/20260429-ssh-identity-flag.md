# SSH Identity Flag (`-i/--identity`)

## Overview

Add `-i/--identity <path>` as the ad-hoc companion to `-S/--ssh` for supplying an SSH private key inline. Targets CI/automation users who write a key file from a secret and don't have (or don't want) a `~/.ssh/config` entry.

The flag is strict-narrow:
- `-i` is only valid alongside `-S/--ssh`; mutually exclusive with `--server` (named-server users belong in `~/.ssh/config`).
- CLI-only (rejected in TUI `RunE`).
- Validates path at parse time (exists, regular file, readable, with `~/` expansion); skips permission/mode checks (`ssh(1)` already enforces those).
- Implementation reuses the existing `SSHExtraArgs` splice on `RemoteCompose` — every argv build site (`Connect`, `Close`, `Detect`, `remoteCommand`, `findRemoteComposeFile`, `ConfigFile`, `EditCommand`, `ExecCommand`) absorbs `-i` for free without touching the splice sites.

## Context (from discovery)

Files/components involved:
- `cmd/root.go:177-180` — persistent flag registration block (next to `-S/--ssh`)
- `cmd/root.go:42-48` — TUI `RunE` rejection of `--ssh` (mirror for `--identity`)
- `cmd/remote.go:18-23` — `checkRemoteMutex` (extend signature)
- `cmd/remote.go:37-63` — `resolveSSHRemote` (extend signature, append to `SSHExtraArgs`)
- `cmd/deploy.go` (shared by deploy/restart/stop via `runOperation()`), `cmd/list.go`, `cmd/logs.go`, `cmd/exec.go` — four call-site pairs at `deploy.go:108,131`, `list.go:331,340`, `logs.go:53,60`, `exec.go:68,75`
- `internal/config/sshtarget.go` — sibling style reference for new `identity.go`
- `internal/config/sshtarget_test.go` — table-driven test pattern reference
- `internal/compose/remote.go` — `SSHExtraArgs` splice sites (no changes needed)

Related patterns found:
- `SSHTarget`/`ParseSSHTarget` pattern: bare error wording, callers wrap with context (`invalid --ssh value %q: %w`). Mirror for `ParseIdentity`.
- `cmd/remote.go` helper style: `noopCleanup` for unconditional `defer cleanup()`; factory-injection for test seams via per-subcommand variables (`opNewRemote`, `execNewRemote`, `logsNewRemote`, `listNewRemote`).
- `cmd/remote_test.go` test style: stdlib `testing`, table-driven with substring error matching, `stubRemoteFactory` test seam built on `RemoteCompose.SetTestHooks`.

Dependencies identified:
- Standard library only (`os`, `path/filepath`, `strings`). No cgo (matches the project's "no `~user`" decision).
- No new fields on `RemoteCompose`. No changes to `internal/compose/remote.go` argv build sites.

## Development Approach

- **testing approach**: Regular (code first, then tests in same task) — matches existing project pattern in `internal/config/`, `cmd/`, and `internal/compose/`.
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change: `go test ./...`
- maintain backward compatibility — all existing CLI behavior unchanged when `-i` is not set

## Testing Strategy

- **unit tests**: required for every task. Stdlib `testing` only, table-driven where helpful, substring-match on error wording.
- **e2e tests**: project has no UI-based e2e suite. Manual smoke test in Post-Completion (run `cdeploy -S user@host -i /tmp/key list` against a real host).
- **build check**: `go build -o cdeploy .` after the final task to confirm no compile regressions.
- **vet**: `go vet ./...` after the final task.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

**One new public function** (`config.ParseIdentity`) handles all path validation and `~/` expansion. Returns the cleaned absolute path on success, bare-worded errors on failure.

**Two helper signature extensions** in `cmd/remote.go`:
- `checkRemoteMutex` gains an `identityFile` parameter and a second rule: `--identity` requires `--ssh`. Existing `--server`/`--ssh` mutex unchanged.
- `resolveSSHRemote` gains an `identityFile` parameter; when non-empty it calls `ParseIdentity` and appends `["-i", cleanPath]` to the same `SSHExtraArgs` slice that already carries port args.

**One new persistent flag** in `cmd/root.go` (`-i/--identity`), plus a TUI-mode rejection mirroring the existing `--ssh` rejection.

**Four mechanical call-site edits** across the subcommand files (`deploy.go` covers deploy/restart/stop because they share `runOperation()`; `list.go`, `logs.go`, `exec.go` each have their own helper invocation) to thread the new parameter through.

The implementation **does not touch** `internal/compose/remote.go` — the splice points already absorb arbitrary `SSHExtraArgs` content. This is the key payoff of the existing design.

## Technical Details

**`ParseIdentity(s string) (string, error)`** — `internal/config/identity.go`:
1. `s = strings.TrimSpace(s)` — reject empty with `"path is empty"`.
2. If `strings.HasPrefix(s, "~/")` or `s == "~"`: expand via `os.UserHomeDir()`. Replace `~` with home; preserve trailing path.
3. If `strings.HasPrefix(s, "~")` (any non-`~/` form like `~root`): reject with `"only ~/ is supported (no ~user)"`.
4. `os.Stat(path)`:
   - error → wrap as `"not found: <err>"` (or pass through if the OS message is already clear).
   - `info.Mode().IsRegular() == false` → reject with `"not a regular file"`.
5. `f, err := os.Open(path); if err != nil { return "", fmt.Errorf("not readable: %w", err) }; f.Close()` — confirms ACL/perm readability beyond mode bits.
6. Return cleaned path. Use `filepath.Clean` after expansion. Don't absolutize relative paths — let `ssh(1)` resolve them against cwd, matching `ssh -i` behavior.

**`checkRemoteMutex(serverName, sshTarget, identityFile string) error`**:
```
if serverName != "" && sshTarget != "" → existing error
if identityFile != "" && sshTarget == "" → "--identity requires --ssh"
return nil
```
Order matters: the `--server`+`--ssh` rule fires first, so `--server -i` reaches the second rule only after `--ssh` is already empty (which is correct — that combination errors out via the second rule).

**`resolveSSHRemote(ctx, sshTarget, projectDir, identityFile string, newRemote ...) (...)`**:
After `target.PortArgs()` is computed:
```go
extraArgs := target.PortArgs()
if identityFile != "" {
    cleanPath, err := config.ParseIdentity(identityFile)
    if err != nil {
        return nil, noopCleanup, fmt.Errorf("invalid --identity value %q: %w", identityFile, err)
    }
    extraArgs = append(extraArgs, "-i", cleanPath)
}
rc.SSHExtraArgs = extraArgs
```
Connect/Detect path unchanged.

**Argv ordering** (informational, no code change): final SSH argv contains `[..., "-p", "2222", "-i", "/path/to/key", "user@host", ...]`. `ssh(1)` doesn't care about flag order, and both come before the host as required.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): all code, tests, and doc changes within this repo.
- **Post-Completion** (no checkboxes): manual smoke test against a real SSH host.

## Implementation Steps

### Task 1: Add `ParseIdentity` validator

**Files:**
- Create: `internal/config/identity.go`
- Create: `internal/config/identity_test.go`

- [ ] create `internal/config/identity.go` with `ParseIdentity(s string) (string, error)`:
      trim → reject empty → `~/` and bare `~` expansion via `os.UserHomeDir()` →
      reject `~user` → `os.Stat` (regular file check) → `os.Open` + immediate close
      (readability check) → return `filepath.Clean` of the resolved path
- [ ] write tests covering: empty input, `~/foo` expansion (use `t.Setenv("HOME", t.TempDir())`),
      bare `~` expansion, `~user` rejected with clear message, non-existent path,
      directory (not a regular file), valid regular file (returns cleaned path),
      relative path passes through unchanged, unreadable file via `chmod 0000`
      (skip on `runtime.GOOS == "windows"`)
- [ ] run tests: `go test ./internal/config/ -run TestParseIdentity -v` — must pass before task 2

### Task 2: Extend `checkRemoteMutex` for `--identity` rule

**Files:**
- Modify: `cmd/remote.go`
- Modify: `cmd/remote_test.go`

- [ ] change `checkRemoteMutex` signature to `(serverName, sshTarget, identityFile string) error`
      and add the new rule: `identityFile != "" && sshTarget == ""` →
      `fmt.Errorf("--identity requires --ssh")`. Update the doc comment.
- [ ] extend `TestCheckRemoteMutex` table with: `("","","/k")` errors with
      `"--identity requires --ssh"`; `("prod","","/k")` also errors with
      `"--identity requires --ssh"` (the existing `--server`/`--ssh` mutex requires
      both non-empty, so it does not fire when `sshTarget` is empty — the new
      `--identity requires --ssh` rule catches the misuse instead, which is the
      strict-narrow scoping we want); `("","host","/k")` is ok; `("prod","host","/k")`
      still errors via the existing `--server`/`--ssh` mutex. Update all existing
      cases to pass the new third argument as `""`.
- [ ] run tests: `go test ./cmd/ -run TestCheckRemoteMutex -v` — must pass before task 3

### Task 3: Extend `resolveSSHRemote` to thread identity into `SSHExtraArgs`

**Files:**
- Modify: `cmd/remote.go`
- Modify: `cmd/remote_test.go`

- [ ] change `resolveSSHRemote` signature to add `identityFile string` between
      `projectDir` and `newRemote`. Inside, after computing `target.PortArgs()`,
      branch on `identityFile != ""`: call `config.ParseIdentity`, on error wrap as
      `"invalid --identity value %q: %w"` and return with `noopCleanup`; on success
      append `"-i", cleanPath` to the local `extraArgs` slice. Assign
      `rc.SSHExtraArgs = extraArgs`.
- [ ] update existing tests in `cmd/remote_test.go` to pass `""` as the new
      `identityFile` argument (mechanical edit to all `resolveSSHRemote(...)` calls)
- [ ] add `TestResolveSSHRemote_WithIdentity_HappyPath`: pass a real temp file path,
      expect `rc.SSHExtraArgs == ["-p","2222","-i",<tempPath>]` (or `["-i",<tempPath>]`
      with no port). Use `t.TempDir()` and `os.WriteFile` to create the key file.
- [ ] add `TestResolveSSHRemote_WithIdentity_InvalidPath`: pass `/nonexistent/key`,
      expect error wrapping `"invalid --identity value"` and the underlying `not found`
      message. Verify `cleanup` is the no-op (safe to call).
- [ ] add `TestResolveSSHRemote_WithIdentity_TildeExpansion`: set `HOME` via
      `t.Setenv`, create `$HOME/.ssh/id_test`, pass `~/.ssh/id_test`, expect
      `rc.SSHExtraArgs[len-1]` to be the absolute path under the temp HOME.
- [ ] run tests: `go test ./cmd/ -run TestResolveSSHRemote -v` — must pass before task 4

### Task 4: Wire `-i/--identity` into root command + TUI rejection

**Files:**
- Modify: `cmd/root.go`
- Modify: `cmd/root_test.go`

- [ ] add `var identityFile string` next to the existing `sshTarget` var
- [ ] register the persistent flag immediately after the `--ssh` registration:
      `rootCmd.PersistentFlags().StringVarP(&identityFile, "identity", "i", "", "path to SSH private key (requires --ssh)")`
- [ ] in the root `RunE` (right after the existing `--ssh` rejection at line 46-48),
      add an analogous rejection: `if identityFile != "" { return fmt.Errorf("--identity is not valid for the interactive TUI; use it with a subcommand") }`
- [ ] extend `cmd/root_test.go` flag-registration assertions to verify `-i`/`--identity`
      is registered as a persistent string flag with the correct help text. Follow
      the existing pattern used for `-S`/`--ssh`.
- [ ] add a test asserting that running root with `--identity /tmp/x` (without a
      subcommand) returns the TUI rejection error. Mirror the existing `--ssh` TUI test.
- [ ] run tests: `go test ./cmd/ -run TestRoot -v` — must pass before task 5

### Task 5: Thread `identityFile` through four subcommand call sites

**Files:**
- Modify: `cmd/deploy.go` (covers deploy/restart/stop — they share `runOperation()`; pair at lines 108, 131)
- Modify: `cmd/list.go` (pair at lines 331, 340)
- Modify: `cmd/logs.go` (pair at lines 53, 60)
- Modify: `cmd/exec.go` (pair at lines 68, 75)
- Modify: `cmd/deploy_test.go`
- Modify: `cmd/list_test.go`
- Modify: `cmd/logs_test.go`
- Modify: `cmd/exec_test.go`

- [ ] in each of `cmd/deploy.go`, `cmd/list.go`, `cmd/logs.go`, `cmd/exec.go`:
      update `checkRemoteMutex(serverName, sshTarget)` →
      `checkRemoteMutex(serverName, sshTarget, identityFile)`, and
      `resolveSSHRemote(ctx, sshTarget, projectDir, factory)` →
      `resolveSSHRemote(ctx, sshTarget, projectDir, identityFile, factory)`.
      Mechanical edits — the global `identityFile` var from `cmd/root.go` is in scope.
- [ ] add `TestRunOperation_IdentityWithoutSSH` in `cmd/deploy_test.go` (covers
      deploy/restart/stop because they all flow through `runOperation()`; follow the
      existing `runOperation(...)` test pattern around lines 350/385/417). Add
      analogous `TestList_IdentityWithoutSSH`, `TestLogs_IdentityWithoutSSH`,
      `TestExec_IdentityWithoutSSH` in their respective `_test.go` files. Each test:
      set `identityFile = "/tmp/k"`, leave `sshTarget = ""`, invoke the subcommand's
      `RunE` (or `runOperation` for deploy_test), assert the error contains
      `"--identity requires --ssh"`. Reset globals (`identityFile = ""`,
      `sshTarget = ""`) in `t.Cleanup` to avoid state leakage between tests.
- [ ] run tests: `go test ./cmd/ -v` — must pass before task 6

### Task 6: Verify acceptance criteria

**Files:**
- (no file edits — verification only)

- [ ] verify all requirements from Overview are implemented:
      `-i` flag exists, requires `--ssh`, mutex with `--server`, CLI-only,
      `~/` expansion works, validates existence/regular-file/readability,
      reuses `SSHExtraArgs`, no changes to `internal/compose/remote.go`
- [ ] verify edge cases: empty `--identity`, `~user` rejected, directory rejected,
      unreadable file rejected, valid path with non-default port works
- [ ] run full test suite: `go test ./... -count=1`
- [ ] run `go vet ./...`
- [ ] run `go build -o cdeploy .` — must succeed with no warnings
- [ ] verify test coverage of `ParseIdentity` (the only new public function):
      `go test ./internal/config/ -cover`. Expect ≥ 90% line coverage.

### Task 7: Update documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`
- Move: this plan file → `docs/plans/completed/`

- [ ] in `README.md`, locate the `--ssh` section/example and add an `-i` example:
      `cdeploy -S deploy@1.2.3.4 -i ~/.ssh/ci.pem -C /opt/app deploy`. Add a one-line
      note: `-i` is only valid with `-S` and is intended for CI/ephemeral use; for
      configured servers, use `IdentityFile` in `~/.ssh/config`.
- [ ] in `CLAUDE.md`, update the "**Ad-hoc SSH (`-S`/`--ssh`)**" paragraph: note
      that `SSHExtraArgs` content is now port + optional identity. Amend the sentence
      "SSH-specific options (keys, jump hosts, tunnels) belong in `~/.ssh/config`"
      with the carve-out: *except `-i` for ad-hoc CI/automation use via `--ssh`*.
- [ ] move plan: `mkdir -p docs/plans/completed && mv docs/plans/20260429-ssh-identity-flag.md docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only.*

**Manual verification:**
- Smoke test against a real SSH host: write a key file, run
  `./cdeploy -S user@host -i /tmp/key -C /opt/app list`, confirm it connects and lists services.
- Verify error UX: run with non-existent key path; confirm error message is clear
  and arrives before any ControlMaster setup attempt.
- Verify `~/` expansion in a real shell context (the `~` is shell-expanded by the
  shell before reaching cdeploy in interactive use; the cdeploy expansion only
  matters when the path is passed verbatim, e.g. from a CI YAML or `--identity="~/key"`).

**External system updates:**
- None. `cdeploy` is a single binary with no API consumers.

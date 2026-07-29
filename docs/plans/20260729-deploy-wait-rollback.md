# Health-Gated Deploy (`--wait`) + Digest-Pinned Rollback (`cdeploy rollback`)

## Overview

Two coupled features forming a "safe deploy" story — deploy with a health gate, undo with a
digest-pinned rollback — compose-native and agentless (no daemon, no Swarm, no proxy):

- **`cdeploy deploy --wait` / `restart --wait` / `rollback --wait`**: after the pipeline
  finishes, poll container health until every targeted service is healthy (or running past a
  grace window when it has no healthcheck). On failure exit with a dedicated code so CI can
  distinguish "deploy broke midway" from "deployed but unhealthy".
- **`cdeploy rollback [services...|-a]`**: every deploy snapshots the *currently running*
  image digests to a state file on the docker host before touching anything; rollback
  re-creates services pinned to those digests via a generated compose override file.
- **TUI**: deploys/restarts/rollbacks always run the health-wait phase on the progress screen
  (esc skips); a new `R` key on the services screen rolls back with confirmation.

Explicitly **not** in v1: `--rollback-on-unhealthy` auto-coupling (a failed `--wait` prints
the hint `run 'cdeploy rollback' to restore the previous images` instead), history deeper
than one snapshot, config-drift detection (rollback pins **images only** against the
*current* compose file — documented caveat), and one-shot service detection
(`ServiceStatus` carries no exit code, so `--wait` assumes targeted services are
long-running: a run-to-completion service (migrations, seeds) in the target set will fail
the wait — documented caveat).

Competitive context (validated 2026-07-29): no tool does health-gated deploy + rollback for
plain compose agentlessly — PaaS tools (Coolify/Dokploy) exclude compose services from theirs,
Kamal abandons the compose format, web UIs (Dockge/Portainer/Komodo) have neither.

### Acceptance criteria

1. `cdeploy deploy -a --wait` on a project with healthchecks exits 0 when all services go
   healthy, 2 when any goes unhealthy/exits/loops/times out (verdict table printed either way).
2. Services without healthchecks pass after running continuously past a 10s grace window,
   labeled `running (no healthcheck)` in the verdict table.
3. `cdeploy deploy` (local and `--server`/`--ssh`) records a per-service digest snapshot on
   the docker host before stopping anything; snapshot failure warns but never blocks deploy.
4. `cdeploy rollback web` re-creates `web` pinned to the snapshot digest; works with the
   registry unreachable when the old blob is still on the host; refuses clearly when no
   snapshot exists for a targeted service.
5. TUI: progress screen shows live per-service wait verdicts after deploy/restart/rollback
   (esc skips, op stays "done"); `R` on the services screen rolls back with confirmation.
6. Zero behavior change for existing invocations: nil `ExtraComposeFiles` produces
   byte-identical argv (regression-pinned); no `runner.Composer` interface change — the 4
   mock sets (`internal/runner/`, `internal/tui/app_test.go`, `cmd/deploy_test.go`,
   `cmd/list_test.go`) are untouched.

## Context (from discovery)

- `internal/runner/runner.go` — `Operation` enum + `String()`/`Steps()`/`buildSteps()`
  (5-step "Adding New Operations" recipe in CLAUDE.md applies); `Composer` interface already
  exposes the poll primitive: `ContainerStatus()` returns `Running` + tri-value `Health`
  (worst-case aggregated across replicas) + `Uptime` (`"restarting"` signal exists).
- `cmd/deploy.go:103` `runOperation()` — shared by deploy/restart/stop; events loop ends at
  the natural insertion point for the wait phase. `main.go` is a bare `os.Exit(1)` — exit-2
  mapping hooks in there via `errors.As`.
- `internal/compose/compose.go:338 command()` / `remote.go:287 remoteCommand()` — single
  choke points where `-f` splicing lands. `composeFiles` candidates at `compose.go:47`;
  `findComposeFile` (local) / `findRemoteComposeFile` (`remote.go:439`) already discover the
  main compose file (used by the config screen).
- `internal/compose/updates.go` — reusable digest machinery: service→image mapping via
  `docker compose config --format json`, `parseLocalDigest()` (`:209`), top-level docker
  runners `runDockerCmd` (`:662`) / `runRemoteDockerCmd` (`remote.go:808`, splices
  `SSHExtraArgs` before the host arg, shell-escapes args).
- `internal/config/config.go` `Save()` — atomic temp+rename pattern to copy for state writes.
- `internal/tui/app.go` — action keys set `pendingOp` (`:1438-1454`); progress-screen esc
  handler invalidates the update cache gated on `m.done && (Deploy || Restart)` (`:2049`) —
  rollback extends this line; `ConfigProvider`/`ExecProvider` optional-interface pattern for
  concrete-composer capabilities; session-counter + in-flight-guard invariants documented in
  CLAUDE.md apply to the new `waitTickMsg`/`snapshotMsg`.
- House convention: stdlib `testing` only; argv-construction tests without executing docker;
  TUI tested by driving `Update()` with `tea.KeyMsg`.

## Development Approach

- **Testing approach**: Regular (code first, then tests, within each task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional — they are a required part of the checklist
  - cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Run `go test ./...` after each change; stdlib `testing` only (repo convention)
- Backward compatible: additive flags/commands only; nil `ExtraComposeFiles` argv pinned

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach)
- Wait rules tested as a pure reducer (elapsed time is an input — no clock mocking); the
  blocking wrapper tested with a mock `Composer` and millisecond durations
- Compose changes tested via argv construction + `SetTestHooks`/`SetOutputErrHook` (no docker)
- State-file tests use `t.TempDir()` (+ `t.Setenv("HOME", ...)` where path derivation is
  exercised; no `t.Parallel` in those)
- TUI tests drive `Update()` with `tea.KeyMsg`/custom msgs directly — no TTY
- No e2e framework; final verification is a manual smoke pass (Post-Completion)

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Keep plan in sync with actual work done

## Solution Overview

**Wait engine** — one tested implementation of the rules, two drivers:

- `runner.EvaluateWait(prev WaitState, status map[string]ServiceStatus, opts WaitOptions, elapsed time.Duration) (WaitState, bool)`
  — pure step function holding ALL pass/fail rules.
- `runner.WaitHealthy(ctx, c Composer, services []string, opts WaitOptions) (WaitReport, error)`
  — thin blocking wrapper (poll `ContainerStatus` every `Poll`, feed the reducer) for the CLI.
- The TUI drives the same reducer from its own `waitTickMsg` loop (the `refreshTickMsg`
  idiom) — Bubble Tea keeps its message-driven loop, the rules can never diverge.
- No `Composer` widening, no `runner.Run` signature change, mocks untouched.

**Snapshot** — `Deploy` only (never Restart — restarting after a bad deploy must not
overwrite the good snapshot with the broken state; never Rollback — rollback is idempotent:
running it twice lands on the same state). Taken BEFORE the pipeline (pre-Stop). Captures the
digest of the image each *running container* actually uses (container ID → image ID →
`RepoDigests`), NOT the local tag's digest (a pulled-but-not-deployed newer image must not
poison the snapshot). Depth 1, per-service entries, **merge-not-replace** so partial deploys
don't destroy the rest of the project's safety net. Lives ON THE DOCKER HOST at
`~/.cdeploy/state/<sha256(projectDir)[:12]>.json` — local file for local, over SSH for remote
— so CI deploys + laptop rollbacks share one authoritative history. Write failure:
warn-and-proceed.

**Rollback** — new `runner.Operation` `Rollback` with Restart-shaped steps
(Stop→Remove→Create→Start; NO Pull step — registry outages are a classic rollback trigger, so
offline rollback with local blobs must work). A prep phase before the pipeline pulls missing
digests by digest-ref, generates a minimal override YAML, delivers it to the host, and sets
`ExtraComposeFiles = [mainFile, overrideFile]` on the concrete composer; deferred cleanup.
Pinning via override + `-f` stacking is project-scoped (rejected alternative: host-side
`docker tag` retag — tags are host-global, rolling back one project would poison other
projects sharing the image:tag).

**TUI** — no new screens. Progress screen gains a `waiting` sub-state (live verdicts +
countdown, esc = skip). `R` on services screen: async snapshot fetch → warning if absent /
confirmation with snapshot age if present → Rollback via existing `pendingOp` machinery.
Concrete-composer capabilities exposed through new optional interfaces (`Snapshotter`,
`RollbackPreparer`) type-asserted like `ConfigProvider`/`ExecProvider`.

## Technical Details

**State file schema** (JSON, written atomically):

```json
{
  "schema": 1,
  "project_dir": "/opt/myapp",
  "services": {
    "web": {"image": "nginx:latest", "digest": "sha256:ab12...", "recorded_at": "2026-07-29T14:03:00Z"},
    "db":  {"image": "postgres:16",  "digest": "sha256:cd34...", "recorded_at": "2026-07-22T09:11:00Z"}
  }
}
```

Key = `sha256(projectDir)` first 12 hex chars. Per-service `recorded_at` because merge keeps
older entries alive. Unknown `schema` → rollback refuses (never guesses).

**Snapshot capture** (per project, both composers): `compose ps --format json` → container
IDs of running containers per service → `docker inspect --format '{{.Image}}'` (batched) →
`docker image inspect` those IDs → `RepoDigests` filtered against the compose-config image
ref (existing `parseLocalDigest`). Not-running service → no entry + warning; no repo digest
(locally built) → skipped + warning.

**Override YAML** (generated, minimal):

```yaml
services:
  web:
    image: nginx@sha256:ab12...
```

Delivery: local → file in `os.TempDir()`; remote → piped stdin over the ControlMaster socket
to `/tmp/cdeploy-rollback-<pid>.yml`. Remote state write: `mkdir -p ~/.cdeploy/state && cat >
<tmp> && mv <tmp> <final>` (same piped-stdin primitive). Remote state read: `cat <file>`
tolerating missing-file exit status.

**`-f` splicing**: `ExtraComposeFiles []string` on `Compose` and `RemoteCompose`; when
non-nil, `command()`/`remoteCommand()` emit `-f <f0> -f <f1> ...` immediately after
`docker compose` / `docker-compose` (both plugin and standalone modes), shell-escaped on
remote. `-f` disables compose's file auto-discovery → the discovered main file MUST be first.
`cmd.Dir`/project-dir handling unchanged so project-name derivation stays stable (argv test).

**Wait rules** (all in `EvaluateWait`; the precedence order below IS the spec):

- Service target set = deploy's `containers` arg; empty → `ListServices()`.
- **Per-service evaluation order** — a restarting container reports `Running == false` AND
  `Uptime == "restarting"` *simultaneously* (`compose.go:813`, `uptime.go:28`), so restarting
  MUST be checked before any exited rule or the restart counter never reaches 3:
  1. `Uptime == "restarting"` ⇒ bump restart counter; 3 consecutive ⇒ fail fast
     (`restarting`). A non-restarting observation resets the counter.
  2. `Health == "unhealthy"` ⇒ fail fast (docker already debounced through its own retries).
  3. `Running == false` after the service was observed running at least once this wait
     (`everRunning`) ⇒ fail fast (`exited`).
  4. `Running == false`, never observed running, for 5 consecutive polls (~10s) ⇒ fail fast
     (`exited (never started)`). The ≥2-poll debounce absorbs the first-poll race where a
     container is caught mid-transition right after Start returns.
  5. `Health != ""` ⇒ has healthcheck ⇒ passes on `healthy`.
  6. `Health == ""` && `Running` ⇒ grace timer: passes after running continuously ≥ `Grace`
     (10s); a not-running observation resets `firstRunningAt`.
- Timeout (default 2m): remaining non-passed services get `timed out (still starting)`.
- Verdicts: `healthy` / `running (no healthcheck)` / `unhealthy` / `exited` /
  `exited (never started)` / `restarting` / `timed out (still starting)`.
- **Caveat (README + Overview)**: no exit code in `ServiceStatus` ⇒ one-shot
  run-to-completion services are indistinguishable from crashes; `--wait` assumes targeted
  services are long-running.
- `WaitOptions{Timeout: 2m, Grace: 10s, Poll: 2s}` — Grace/Poll are internal constants in v1
  (no flags); only `--wait-timeout` is exposed. No OnPoll callback in v1 (YAGNI: the CLI
  prints a start line + final table; the TUI drives the reducer directly).

**Exit codes**: 0 = success; 1 = pipeline step failed (existing); **2 = wait failed** — new
exported `cmd.WaitError` (wraps `runner.WaitReport`), `main.go` maps via `errors.As` →
`os.Exit(2)`. Hint `run 'cdeploy rollback' to restore the previous images` printed on deploy
wait-failure only.

**Rollback refusal rules** (checked before anything stops): state file missing / unreadable /
`schema != 1` / any targeted service absent from snapshot → error naming exactly what's
missing. Current digest == snapshot digest → warn `already at snapshot`, proceed (recreate is
still meaningful). Prep pull failure (blob pruned AND registry down) → per-image error, abort
before the pipeline runs.

**New TUI state** (all cleared at the documented departure sites — esc back-nav, entryLocal,
connectResultMsg error path): `waiting bool`, `waitState runner.WaitState`, `waitSession
uint64`, `waitDeadline`, snapshot-fetch fields for the `R` flow (`rollbackSnapshot`,
`rollbackFetchSession`), and `rollbackCleanup func()`. `waitTickMsg{session}` gated on
`m.screen == screenProgress && msg.session == m.waitSession`; singleton reschedule like
`refreshTickMsg`.

**Rollback cleanup timing (TUI race rule)**: `PrepareRollback`'s cleanup is stored on the
Model and invoked when LEAVING `screenProgress` (after the wait phase) — NEVER deferred
inside the pipeline goroutine. `runner.Run`'s `defer close(events)` is what triggers the
wait-tick loop, whose `ContainerStatus` tea.Cmd would read `ExtraComposeFiles` concurrently
with a goroutine-deferred reset (`go test -race` would flag it). The CLI path keeps a plain
function-scoped `defer cleanup()` — it runs after `WaitHealthy` returns, so no race.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs in this repo
- **Post-Completion** (no checkboxes): manual smoke verification, release

## Implementation Steps

### Task 1: Wait reducer — `runner.EvaluateWait` + types

**Files:**
- Create: `internal/runner/wait.go`
- Create: `internal/runner/wait_test.go`

- [x] define `WaitOptions` (Timeout/Grace/Poll + defaults via `normalize()`), `WaitVerdict`
      (string enum: the 7 verdicts), `WaitState` (per-service verdict map, `firstRunningAt`,
      `everRunning`, consecutive-restarting and consecutive-never-running counters),
      `WaitReport` (Verdicts, Elapsed, OK)
- [x] implement `EvaluateWait(prev, status, opts, elapsed)` — pure, no clock/IO, in the EXACT
      precedence order from Technical Details (restarting BEFORE exited: a restarting
      container reports `Running=false` + `Uptime=="restarting"` simultaneously)
- [x] write table tests: all-healthy; grace pass; grace reset after flap; unhealthy fail-fast;
      exited-after-running fail-fast; 3× restarting fail-fast (2× must NOT fail — and must
      NOT be misread as exited); never-running debounce (1 not-running poll must NOT fail;
      5 consecutive must → `exited (never started)`); timeout-while-starting; mixed
      checked/unchecked services; service absent from status map (treated as not-running,
      feeds the same counters)
- [x] write tests for verdict labels (exact strings — the CLI table and TUI render them)
- [x] run tests — must pass before task 2

### Task 2: Blocking wrapper — `runner.WaitHealthy`

**Files:**
- Modify: `internal/runner/wait.go`
- Modify: `internal/runner/wait_test.go`

- [x] implement `WaitHealthy(ctx, c Composer, services, opts)` — resolve empty target set via
      `ListServices()`, poll `ContainerStatus()` every `Poll`, feed `EvaluateWait`, honor ctx
      cancellation (return partial report + ctx err)
- [x] `ContainerStatus` transient error handling: a failed poll is skipped (state carried
      forward), N consecutive poll errors (3) fail the wait with that error
- [x] write tests with a scripted mock Composer (sequence of status maps) + millisecond
      durations: success path, fail-fast path, timeout path, ctx-cancel partial report,
      poll-error tolerance + 3-strike failure, empty-services → ListServices resolution
- [x] run tests — must pass before task 3

### Task 3: `Rollback` operation in the runner

**Files:**
- Modify: `internal/runner/runner.go`
- Modify: `internal/runner/runner_test.go`

- [x] add `Rollback` to the `Operation` enum; `String()` → "Rollback"; `Steps()` and
      `buildSteps()` → Stop→Remove→Create→Start (Restart shape, NO Pull)
- [x] write sequence test: Rollback emits exactly Stopping/Removing/Creating/Starting events
      in order via the existing mock
- [x] write failure-propagation test for Rollback (step fails → StatusFailed + channel closed)
- [x] run tests — must pass before task 4

### Task 4: `ExtraComposeFiles` splice (local + remote)

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/compose_test.go`
- Modify: `internal/compose/remote_test.go`

- [x] add `ExtraComposeFiles []string` to `Compose`; `command()` emits `-f <file>` pairs
      immediately after the compose invocation (both plugin `docker compose` and standalone
      `docker-compose` argv shapes), before the subcommand
- [x] add the same field to `RemoteCompose`; `remoteCommand()` splices shell-escaped `-f`
      pairs into the remote command string (both modes)
- [x] write argv tests local: with 2 files (order preserved, `-f` before subcommand), plugin +
      standalone; **regression pin: nil field ⇒ argv byte-identical to today** (all
      subcommands exercised via existing test helpers)
- [x] write argv tests remote: shell-escaping of paths with spaces/quotes; nil ⇒ unchanged
      remote command string; `CURRENT_UID` prefix unaffected
- [x] run tests — must pass before task 5

### Task 5: Snapshot schema + pure helpers

**Files:**
- Create: `internal/compose/snapshot.go`
- Create: `internal/compose/snapshot_test.go`

- [x] define `Snapshot` / `SnapshotEntry` structs matching the schema; `snapshotKey(projectDir)`
      — normalize BEFORE hashing (local: `filepath.Abs` + `filepath.Clean`; remote:
      `path.Clean` + trailing-slash trim, otherwise verbatim — `-C ./myapp` and
      `-C /abs/myapp` MUST produce the same key), then sha256 → 12 hex;
      `stateFileRelPath(projectDir)` (`.cdeploy/state/<key>.json`)
- [x] implement `mergeSnapshot(existing, fresh *Snapshot) *Snapshot` — per-service
      merge-not-replace, fresh entries win, `schema`/`project_dir` refreshed
- [x] implement `parseSnapshot([]byte)` — strict: unknown schema or malformed JSON → typed
      errors (`errSnapshotSchema`, distinguishable from not-found)
- [x] implement `buildOverrideYAML(entries map[string]SnapshotEntry, services []string) []byte`
      — minimal `services:` YAML with `image: <repo>@<digest>` refs (repo derived from the
      recorded image ref via existing `stripTag`); emit services in SORTED order (Go map
      iteration is random — build from a sorted slice, never Marshal a map)
- [x] write table tests: key derivation stability (rel vs abs path of the same dir ⇒ same
      key; trailing slash ⇒ same key); merge (partial deploy keeps other services,
      per-service recorded_at preserved); parse (valid / bad JSON / schema 2); override YAML
      (single + multi service, subset selection, deterministic ordering)
- [x] run tests — must pass before task 6

### Task 6: Local snapshot capture + state IO (`Compose`)

**Files:**
- Modify: `internal/compose/snapshot.go`
- Modify: `internal/compose/compose.go` (only if capture needs unexported access)
- Modify: `internal/compose/snapshot_test.go`

- [x] implement `(*Compose) SnapshotServices(ctx, services []string) (SnapshotResult, error)`:
      reuse the updates.go service→image mapping; `compose ps --format json` for running
      container IDs; batched `docker inspect --format '{{.Image}}'` + `docker image inspect`
      via `runDockerCmd`; `parseLocalDigest` filtering; collect per-service warnings
      (not-running, no-digest) into `SnapshotResult.Warnings`
- [x] implement `(*Compose) ReadSnapshot(ctx)` / `writeSnapshotFile(path, snap)` — read
      existing, merge, atomic temp+rename write under `$HOME/.cdeploy/state/` (MkdirAll,
      `config.Save` pattern); distinguish not-found from parse errors
      (`WriteSnapshot(ctx, fresh)` ties read+merge+write; both composers will satisfy it)
- [x] write capture tests via `SetTestHooks` (scripted ps/inspect outputs): happy path,
      service not running (warning + absent entry), build-only image (warning + absent),
      scaled service (one entry, running replica's digest)
- [x] write state IO tests with `t.TempDir()` + `t.Setenv("HOME", ...)`: round-trip, merge on
      second write, corrupt file → typed error, atomicity (no partial file on simulated
      failure)
- [x] run tests — must pass before task 7

### Task 7: Remote snapshot capture + state IO (`RemoteCompose`)

**Files:**
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/snapshot.go` (shared helpers)
- Modify: `internal/compose/remote_test.go`

- [x] implement `(*RemoteCompose) SnapshotServices(ctx, services)` mirroring Task 6 — split
      per the CLAUDE.md invariant: `compose ps`/`compose config` go through `remoteCommand`,
      but `docker inspect` / `docker image inspect` are TOP-LEVEL docker commands and MUST
      go through `runRemoteDockerCmd` (image names shell-escaped)
- [x] add the remote stdin-pipe write primitive: `writeRemoteFile(ctx, path, data)` building
      `ssh [extra] host 'mkdir -p <dir> && cat > <tmp> && mv <tmp> <path>'` with data on
      stdin; `SSHExtraArgs` spliced immediately before the host arg (existing convention);
      reuse the `-S <SocketPath> -o ControlMaster=no` prefix (as `remoteCommand` does) so
      the write rides the existing ControlMaster socket instead of a fresh connection
- [x] implement `(*RemoteCompose) ReadSnapshot(ctx)` via `cat` with missing-file tolerance
      (exit-status → not-found, not error)
- [x] write argv-construction tests: snapshot capture commands; writeRemoteFile command string
      (escaping, SSHExtraArgs position, tmp+mv shape); ReadSnapshot cat command; test hooks
      drive outputs for capture parse paths
- [x] run tests — must pass before task 8

### Task 8: Rollback prep (`PrepareRollback` on both composers)

**Files:**
- Modify: `internal/compose/snapshot.go`
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/snapshot_test.go`, `internal/compose/remote_test.go`

- [ ] add streaming top-level docker runners — the existing `runDockerCmd`/
      `runRemoteDockerCmd` CAPTURE output; no primitive today streams a top-level command to
      an `io.Writer`: local `runDockerCmdStream(ctx, args, w)` (`cmd.Stdout/Stderr = w`) and
      remote `runRemoteDockerCmdStream(ctx, args, w)` (direct SSH argv, SocketPath prefix,
      `SSHExtraArgs` before host arg); used for `docker pull <repo>@<digest>` live progress
- [ ] implement `(*Compose) PrepareRollback(ctx, entries, services, w io.Writer) (cleanup func(), err error)`:
      per-digest presence check (`docker image inspect <repo>@<digest>`), pull-by-digest for
      missing (via the streaming runners), generate override YAML, write to `os.TempDir()`,
      discover main file via `findComposeFile`, set `ExtraComposeFiles = [main, override]`;
      cleanup removes the temp file and resets the field; abort with per-image error before
      any pipeline step if a required digest is unobtainable
- [ ] implement `(*RemoteCompose) PrepareRollback(...)` mirroring via remote primitives
      (override delivered with `writeRemoteFile` to `/tmp/cdeploy-rollback-<pid>.yml`;
      `findRemoteComposeFile`; cleanup `rm -f` over SSH)
- [ ] detect current == snapshot digest → append `already at snapshot` warning, proceed
- [ ] write tests: presence-check/pull argv (incl. the streaming runners' argv local +
      remote); abort-before-pipeline on failed pull; override content and `-f` ordering
      (main first); cleanup resets field + removes file (local real-FS, remote argv);
      warning on same-digest
- [ ] run tests — must pass before task 9

### Task 9: CLI `--wait` on deploy/restart + exit-code contract

**Files:**
- Modify: `cmd/deploy.go`
- Modify: `main.go`
- Modify: `cmd/deploy_test.go`

- [ ] add `--wait` + `--wait-timeout` (default `2m`) flags to `deploy` and `restart`; thread
      into `runOperation` (options struct or params — keep `stop` unaffected)
- [ ] after the events loop completes cleanly and `--wait` is set: print
      `Waiting for health (timeout 2m)…`, call `runner.WaitHealthy`, print per-service verdict
      table (aligned, colored via existing styles)
- [ ] define exported `WaitError{Report}` in `cmd`; on wait failure return it wrapped; print
      rollback hint (deploy only); `main.go`: `errors.As` → `os.Exit(2)` (stderr message
      preserved)
- [ ] snapshot hook: for `Deploy`, call `SnapshotServices` + merge-write BEFORE `runner.Run`
      (via a small unexported interface both composers satisfy); warnings to stderr;
      write failure → prominent warning, deploy proceeds
- [ ] write tests: flag registration on both cmds (absent on `stop`); `--wait-timeout` parse;
      WaitError → exit-2 mapping (unit-test the errors.As branch); snapshot-failure
      warn-and-proceed (mock via seam); verdict table rendering (golden-ish string check)
- [ ] run tests — must pass before task 10

### Task 10: `cmd/rollback.go` subcommand

**Files:**
- Create: `cmd/rollback.go`
- Create: `cmd/rollback_test.go`
- Modify: `cmd/root.go`

- [ ] `newRollbackCmd()`: `rollback [containers...]` + `-a`, `--wait`/`--wait-timeout`;
      `checkRemoteMutex` first; same local/`--server`/`--ssh` composer branches as
      `runOperation` (reuse it — extend with an op-specific pre-run hook rather than forking
      the function)
- [ ] pre-run: `ReadSnapshot` → refusal rules (missing file / schema / targeted service
      absent — name what's missing); print plan lines
      `web  nginx@sha256:ab12… (recorded 2026-07-28 14:03)`; `PrepareRollback` with deferred
      cleanup; then `runner.Run(Rollback)`; `--wait` phase identical to task 9
- [ ] register in `cmd/root.go`; help/examples text (incl. images-only caveat one-liner)
- [ ] write tests: arg validation (names xor `-a`); refusal paths (no state, schema mismatch,
      missing service — exact error text names the service); plan-line formatting; flag
      registration; subcommand registered on root
- [ ] run tests — must pass before task 11

### Task 11: TUI — snapshot-on-deploy + wait phase on progress screen

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/styles.go`
- Modify: `internal/tui/app_test.go`

- [ ] define `Snapshotter` optional interface in tui (`SnapshotServices` + write, matching the
      cmd seam); in the tea.Cmd that launches Deploy, run snapshot FIRST inside the same
      goroutine (pre-Stop ordering; warnings into the op log writer; warn-and-proceed)
- [ ] add `waiting`/`waitState`/`waitSession`/`waitDeadline` Model fields; after
      Deploy/Restart/Rollback pipeline success (`m.done`), enter waiting sub-state and start
      the `waitTickMsg` loop: each tick = one `ContainerStatus` tea.Cmd → `EvaluateWait` →
      re-render → reschedule (singleton, gated on `screen == screenProgress && session ==
      waitSession`); StopOnly never waits
- [ ] render: verdict line per service under the step list (reuse ♥/✗/~ icons + new
      wait styles), countdown from `waitDeadline`; footer `esc skip` while waiting
- [ ] esc during waiting: bump `waitSession`, clear wait fields, op remains "done" (existing
      esc-from-progress flow continues to work); wait failure renders red verdicts + hint
      `press R on the services screen to roll back` (deploy only)
- [ ] clear new fields at the documented departure sites (esc back-nav chain, entryLocal,
      connectResultMsg error path)
- [ ] write tests: wait starts only after done (not on failed); stale-session tick rejected;
      esc-skip leaves done and clears state; fail-fast tick renders failure + hint gating
      (deploy vs restart); mock without `Snapshotter` → deploy runs exactly as today
- [ ] run tests — must pass before task 12

### Task 12: TUI — `R` rollback key on services screen

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] define `RollbackPreparer` optional interface (ReadSnapshot + PrepareRollback); `R` key
      handler: guard empty list; type-assert composer — not supported → ignore; async
      snapshot fetch (`rollbackSnapshotMsg{session}`, new session counter bumped at the same
      7 context-change sites)
- [ ] fetch result: absent/targeted-missing → inline warning (x-not-running pattern);
      present → existing `confirming` flow with `pendingOp = runner.Rollback`, prompt shows
      service list + snapshot age; respects multi-select like `r`/`d`/`s`
- [ ] confirm → progress screen: prep (inside the op tea.Cmd, before `runner.Run`; prep
      failure → op failed with error), then Rollback pipeline, then wait phase (task 11
      covers it via op == Rollback); cleanup stored as `m.rollbackCleanup` and invoked on
      LEAVING screenProgress — NOT goroutine-deferred (see Technical Details: rollback
      cleanup timing race rule)
- [ ] extend post-op update-cache invalidation at `app.go:2049` to include `runner.Rollback`;
      footer gains `R rollback`; new fields cleared at departure sites
- [ ] write tests: `R` with non-Preparer mock ignored; no-snapshot warning; confirm y/n flow
      sets/clears `pendingOp`; multi-select target set; stale snapshot-fetch msg rejected;
      cache invalidation on Rollback success; `rollbackCleanup` invoked exactly once on
      leaving progress; footer text
- [ ] run tests — must pass before task 13

### Task 13: Verify acceptance criteria

- [ ] walk the 6 acceptance criteria from Overview against the implementation
- [ ] verify edge cases: same-digest rollback warns + proceeds; prep aborts before Stop on
      unobtainable digest; ctx cancel mid-wait → partial report, exit 2; standalone
      `docker-compose` gets the `-f` splice; scaled services single entry
- [ ] grep-audit: nil `ExtraComposeFiles` pins present; no `runner.Composer` change
      (`git diff` on the interface block); 4 mock sets untouched
- [ ] run full test suite: `go test ./... -count=1`
- [ ] `go build -o cdeploy .` + `go vet ./...` clean

### Task 14: [Final] Update documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`
- Modify: `skills/cdeploy/SKILL.md`

- [ ] README.md: `--wait`/`--wait-timeout` on deploy/restart/rollback, `rollback` command
      section (snapshot model, host-side state path, images-only caveat, long-running-
      services caveat for `--wait`, exit codes 0/1/2), TUI key table (`R`, esc-skip),
      progress-screen wait description
- [ ] CLAUDE.md: new architecture paragraphs — wait engine (reducer + two drivers), snapshot
      ownership rules (Deploy-only, merge, host-side state), rollback pipeline + prep,
      `ExtraComposeFiles` contract (nil-pinned), new TUI sessions/cleanup sites; extend the
      "Adding New Operations" note if the recipe grew a prep-hook step
- [ ] `cdeploy skill` SKILL.md (`skills/cdeploy/SKILL.md`): add rollback + --wait usage for
      agents (respect the content pins: no `metadata:` key, <500 lines, description limits)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual smoke verification** (needs a docker host + a compose project with healthchecks):
- Local: `deploy -a --wait` (healthy exit 0; break a healthcheck → exit 2 + hint), `rollback
  -a` restores previous digests (`docker inspect` confirms), `rollback --wait` exits 0
- Remote (`--server` and `--ssh`): same pass; verify state file lands at
  `~/.cdeploy/state/` on the HOST; rollback from a second machine sees the snapshot
- Registry-offline rollback: `docker pull` a new tag, deploy, block registry (hosts file),
  rollback succeeds from local blobs
- TUI walkthrough: deploy → wait phase renders/esc skips; `R` → confirm → rollback → wait;
  `U` glyph shows update-available after rollback (expected: old image running)

**Release**: tag next version (GoReleaser handles artifacts); mention exit-code 2 contract in
release notes — automation that treats any non-zero as "pipeline failed" should learn code 2.

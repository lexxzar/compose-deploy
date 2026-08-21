# Read-only "unmanaged containers" view in the TUI

> Revision 2. Revised after a plan review found seven critical defects in revision 1:
> a non-compiling task order, an under-specified `dockerRunner`, an `updatesCacheKey()`
> collision, and a missing renderer file. Each fix is marked **[R2]** where it changed.

## Overview

cdeploy only shows containers that belong to a Docker Compose project. A container that
somebody started by hand — a `docker run` postgres, a watchtower, a monitoring agent — is
invisible. The user must leave the TUI and open a manual SSH session to see it.

This plan adds a read-only view of those containers. The differentiator is remote reach:
cdeploy already talks to the host over a ControlMaster socket, so this works against a
production server with **no software installed on that server**. lazydocker and ctop cannot
do that.

The view is deliberately read-only. A container with no compose file cannot deploy and cannot
roll back, so key parity is impossible. A partial `d`/`r`/`s` that works on some rows and warns
on others breaks the uniform-row contract that the footer and the `?` overlay depend on.

### Acceptance criteria

- **AC1** — The project picker shows one extra row when the host has containers with no
  `com.docker.compose.project` label. The row reports the count **as of the last project-list
  load**. `m.projects` is cached across an `esc` back-navigation (`app.go:1791-1793`), so the
  count does not refresh inside a session. **[R2: was "a live count", which the cache makes false.]**
- **AC2** — The row is absent when the host has no such containers.
- **AC3a** — The screen shows name, status dot, health icon, Created, Uptime, Ports, CPU and
  Mem, in the same columns as a compose project.
- **AC3b** — The screen shows the `⇧` update glyph. **[R2: split from AC3 — this is the only
  criterion that depends on tasks 10-12, the one droppable block.]**
- **AC4** — These keys work: `l` logs, `x` exec, `enter` (confirm the exec prompt), `/` `n` `N`
  search, `U` force update check, `esc`/`q` back, `?` overlay, arrows.
- **AC5** — These keys are inert AND absent from both the footer and the `?` overlay:
  `d`, `r`, `s`, `R`, `c`, `space`, `a`. No key and no widget advertises a no-op — the per-row
  selection checkbox goes too, not just the title counter.
- **AC6** — Local and remote (SSH) both work.
- **AC7** — The existing compose path is unchanged. `runner.Composer` is NOT widened, so the
  four mock sets named in CLAUDE.md stay untouched, and the existing tests in
  `updates_test.go` / `remote_test.go` / `help_test.go` pass **unmodified**.
- **AC8** — The unmanaged view does not share an update-cache slot with any compose project.
  **[R2: new — see design decision 7.]**

## Context (from discovery)

Files and components involved:

- `internal/compose/compose.go` — `Project{Name, Status, ConfigDir}` (:37), `sortProjects` (:118),
  `parseCreatedAt` (:780), `parsePortsString`, `psEntry` (:379).
- `internal/compose/stats.go` — `AllContainerStats` (:31), `AllContainerStatsRemote` (:62),
  `parseStatsOutput`, `aggregateStatsByService`.
- `internal/compose/updates.go` — `Compose.CheckUpdates` (:351), the daemon + registry cascades
  (:373-423), `compareImageDigest`, `runDockerCmd` (:668).
- `internal/compose/remote.go` — `RemoteCompose.ExecCommand` (:569, splices `-t`),
  `ListProjects` (:621), `CheckUpdates` (:661, `errSSHTransport` early abort, no daemon cascade),
  `runRemoteDockerCmd` (:829), `classifySSHError`, `sshArgs`.
- `internal/tui/app.go` — `ComposerFactory` (:94), `ProjectLoader` (:97), `m.projDir` assignment
  sites (:1543, :1628, :1657, :1777), project-select handler (:1657-1674), `x` key (:1910-1922),
  `U` key (:1923), action keys `d`/`r`/`s` (:1816-1836), `updatesCacheKey` (:3541),
  `viewSelectProject` description column (:4213), caption pad (:4466), row checkbox (:4495),
  `containerHelpLines` (:4259), `containerFooterLines` (:4278), `containerFooter` (:4295).
- `internal/tui/help.go` — `helpGroupsFor` (:116), `leaveGroup` (:93), OPERATE group (:169-178,
  which is the ONLY place `enter` is named), `singleColumnOrder` (:366).
- `internal/tui/footer_reservation_test.go` — `TestContainerFooter_AdvertisesOnlyWorkingKeys` (:525).
- `cmd/root.go` — local factory literal (:101), remote factory literal (:135), `tui.Run` (:186).

Related patterns found:

- **Optional capability interfaces.** `ConfigProvider`, `ExecProvider`, `Snapshotter` and
  `RollbackPreparer` are declared in `tui` and type-asserted on the concrete composer. A
  composer that omits one silently disables its key. This plan reuses the pattern.
- **Top-level docker commands bypass `command()`.** `AllContainerStats` (`stats.go:31`),
  `ExecCommand` and `EditCommand` build their argv directly, and the remote forms use
  `rc.sshArgs(...)` so `SSHExtraArgs` land immediately before the host argument. `docker ps`
  and `docker stats` at host level follow the same rule.
- **Conditional help groups.** `leaveGroup(canGoBack)` already renders a different table per
  context, and `progressGroups(phase)` renders one of three. A read-only variant is the same
  shape, not a new mechanism.

Dependencies identified:

- `docker ps -a --format '{{json .}}'` emits NDJSON, one object per line. Verified against a
  live daemon. **[R2: revision 1 specified `--format json`, which the bare `json` keyword only
  supports on Docker CLI >= 23.0. The `{{json .}}` template produces identical output and works
  on older CLIs too. The repo deliberately supports legacy standalone `docker-compose` hosts via
  `Detect()`, so old CLIs are in scope and the version constraint is avoidable — take the
  template form and add no fallback.]**
- Fields used: `ID` (already 12-char short form), `Names`, `Image`, `State`, `Status`, `Ports`,
  `Labels`, `CreatedAt`.
- `Labels` is a flat comma-joined `k=v` string, not an object.
- Health is not a separate field at host level. It sits inside `Status`, e.g. `Up 2 hours (healthy)`.
- `AllContainerStats` keys by the first 12 characters of the container ID, which matches the
  `ID` field above directly.
- `runner.Composer` has exactly **10** methods (`runner.go:55-97`): five writes
  (`Stop`, `Remove`, `Pull`, `Create`, `Start`) and five reads (`ListServices`,
  `ContainerStatus`, `ContainerStats`, `CheckUpdates`, `Logs`). `ExecCommand` is NOT one of
  them — it belongs to `tui.ExecProvider`. **[R2: revision 1 said "five and six", which is 11.]**

## Development Approach

- **testing approach**: Regular — code first, then tests, within each task.
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `go test ./...` after each change
- maintain backward compatibility

## Testing Strategy

- **unit tests**: required for every task, stdlib `testing` only, no testify.
- **e2e tests**: the project has none. The TUI is tested by calling `Update()` with
  `tea.KeyMsg` directly, with no TTY. Follow that.
- **the compose-path regression net**: every task that touches `internal/compose/updates.go`,
  `internal/compose/remote.go` or `internal/tui/help.go` must leave the existing tests passing
  **unchanged**. Those tests are the proof that AC7 holds. Do not relax an existing assertion
  to make a new path fit.
- **the `cmd/root.go` seam problem**: `cmd/root_test.go` tests flag registration only — the root
  command needs a TTY and is never executed. Any logic added to `cmd/root.go` must therefore be
  a one-line call to a package-level helper that IS testable. **[R2: revision 1 asked for tests
  on closures that have no seam.]**

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

One new composer type, one new picker row, one new gate.

```
project picker
  my-app        /srv/my-app          <- compose project, unchanged
  other-stack   /srv/other           <- compose project, unchanged
  (unmanaged)   3 containers         <- NEW synthetic row
                                        Project{Unmanaged: true, Status: "3 containers"}
        |
        v  m.composerFactory(proj)
  *compose.HostContainers            <- NEW, implements runner.Composer
        |                               read methods work, write methods refuse
        +-- docker dockerRunner      <- the ONLY local/remote variation point
              |
              +-- NewLocalHostContainers(*Compose)
              +-- NewRemoteHostContainers(*RemoteCompose)
                  (reuses runRemoteDockerCmd -> classifySSHError)
```

### Key design decisions

1. **One type, one seam — not two parallel types.** The ten `runner.Composer` methods split
   into five trivial refusals and five reads that differ only in how the `*exec.Cmd` is built.
   A `dockerRunner` field is that single variation point. This reuses `runRemoteDockerCmd`, so
   SSH transport classification and stderr capture come for free rather than being written a
   second time.

2. **The seam needs three methods, not two.** **[R2]** `Logs` streams to an `io.Writer` and must
   NOT allocate a TTY; `ExecCommand` must (`remote.go:596-600` splices `-t`); everything else
   captures and classifies. One `cmd(ctx, args...)` builder cannot express all three:

   ```go
   type dockerRunner interface {
       run(ctx context.Context, args ...string) ([]byte, error) // capture + classify
       stream(ctx context.Context, w io.Writer, args ...string) error
       tty(ctx context.Context, args ...string) (*exec.Cmd, error)
   }
   ```

3. **Constructors are exported; the interface is not.** **[R2]** `cmd/root.go` must build these,
   and it cannot name an unexported interface or an unexported adapter type. Export
   `NewLocalHostContainers(*Compose) *HostContainers` and
   `NewRemoteHostContainers(*RemoteCompose) *HostContainers`; keep `dockerRunner` and both
   adapters unexported.

4. **`ContainerStats` does not use `AllContainerStats`.** **[R2]** Those helpers take concrete
   `*Compose` / `*RemoteCompose` (`stats.go:31,62`), which a seam-held `HostContainers` cannot
   supply without a type switch that would defeat the seam. Their bodies are only "build argv →
   run → `parseStatsOutput`", so go through the seam directly:
   `run(ctx, "stats", "--no-stream", "--format", "json")` then `parseStatsOutput`. The pure
   parser is reused; only the two-line argv build is not.

5. **`ComposerFactory` takes the whole `Project`, not a directory string.** An unmanaged row
   has no config directory, so a sentinel directory string would be the alternative. The
   codebase avoids magic values, and a sentinel in `projDir` would deepen decision 7 rather
   than avoid it. Blast radius is verified small: the type, one call site (`app.go:1658`), two
   literals in `cmd/root.go` (:101, :135), five literals in `app_test.go`. `cmd/list.go`
   declares its own `func(dir string) runner.Composer` (`list.go:513`) and is NOT affected.

6. **Write methods refuse rather than being absent.** `runner.Composer` requires all ten. The
   five write methods return `ErrReadOnly`. They are unreachable in practice because the TUI
   gates the keys, so the sentinel is a backstop, not a user-facing path.

7. **The unmanaged view needs its own update-cache slot.** **[R2, and this is the subtlest
   defect the review caught.]** `updatesCacheKey()` is `m.projDir + "|" + m.serverName`
   (`app.go:3541`), and its own doc comment notes the local-fast-track key is bare `"|"`.
   The unmanaged row has an empty `ConfigDir`, so a local unmanaged view would key `"|"` too —
   a direct collision. Two failures follow: a fresh entry from the fast-track context
   suppresses the unmanaged `CheckUpdates` for the full 10-minute TTL, and `hydrateUpdates`
   writes the other context's verdicts onto any colliding service name (the phantom guard only
   drops *unknown* names, not colliding ones). Fix with an explicit `projUnmanaged bool` on
   Model folded into the key. Its cleanup rule is crisp: it is set or cleared at exactly the
   four sites that already assign `m.projDir` — `app.go:1543`, `:1628`, `:1657`, `:1777`.

8. **`c` and `R` gate themselves for free — but still get explicit gates.** `HostContainers`
   does not implement `ConfigProvider` or `RollbackPreparer`, so those keys already no-op. An
   inert key that the help table still names is exactly the failure mode CLAUDE.md warns
   about, so both get an explicit early return AND leave the read-only help table.

9. **`enter` stays in the read-only help table.** **[R2]** `x` arms `m.confirming` with
   `pendingExec = true` (`app.go:1920-1921`) and `enter` confirms it (`app.go:1843`). `enter`
   is therefore still bound on a read-only screen, and today the ONLY group naming it is
   OPERATE (`help.go:177`) — which this plan deletes. Deleting it wholesale breaks the
   "names nothing unbound" *and* "names every bound key" halves of
   `TestHelpGroups_NamesEveryBoundKey`. Move `enter` into the read-only INSPECT group with a
   sub-state-naming description, matching the existing convention: `{"enter", "confirm the exec prompt"}`.

10. **The row checkbox goes, not just the title counter.** **[R2]** Multi-select exists to feed
    `d`/`r`/`s`/`R`. With those gone, `space` and `a` are disabled — so a `[ ]` on every row
    (`app.go:4495-4498`) advertises a dead key just as loudly as the title's `(n/m selected)`.
    Both go. This couples to the caption row: the pad at `app.go:4466` is
    `strings.Repeat(" ", 10)` and its comment budgets `cursor(2) + checkbox(3) + space(1) + …`.
    Dropping the checkbox means the read-only pad is **7**, in lockstep, or every caption
    misaligns against its data column.

11. **Label matching is a token scan, not a split-and-map.** `Labels` is comma-joined, and a
    label VALUE may legally contain a comma. Splitting on `,` and taking `k=v` pairs can
    therefore mis-slice. The filter only asks one yes/no question — does a
    `com.docker.compose.project=` key exist — so scan tokens for that prefix and stop. A false
    verdict would need a label value containing the literal `,com.docker.compose.project=`.
    Pin this reasoning with a test rather than leaving it implicit.

## Technical Details

### Discovery

```
docker ps -a --format '{{json .}}'                       # local
ssh <ctrl-socket> <host> docker ps -a --format '{{json .}}'   # remote, SSHExtraArgs before host
```

Emits NDJSON, one object per line. Tolerate a JSON-array form too, mirroring
`parseContainerStatus` and `parseStatsOutput`.

### New types

```go
// internal/compose/hostcontainers.go

// hostPsEntry matches `docker ps --format '{{json .}}'` — NOT psEntry, which matches
// `docker compose ps --format json`. Different field names, different shape.
type hostPsEntry struct {
    ID        string `json:"ID"`      // already 12-char short form
    Names     string `json:"Names"`   // comma-joined; take the first
    Image     string `json:"Image"`   // may be an image ID for untagged images
    State     string `json:"State"`   // "running", "exited", ...
    Status    string `json:"Status"`  // "Up 2 hours (healthy)" — health lives here
    Ports     string `json:"Ports"`   // text form -> reuse parsePortsString
    Labels    string `json:"Labels"`  // comma-joined k=v
    CreatedAt string `json:"CreatedAt"`
}

type dockerRunner interface {
    run(ctx context.Context, args ...string) ([]byte, error)
    stream(ctx context.Context, w io.Writer, args ...string) error
    tty(ctx context.Context, args ...string) (*exec.Cmd, error)
}

type HostContainers struct{ docker dockerRunner }

func NewLocalHostContainers(c *Compose) *HostContainers
func NewRemoteHostContainers(rc *RemoteCompose) *HostContainers

var ErrReadOnly = errors.New("read-only: container is not managed by docker compose")
```

### Field mapping

| `runner.ServiceStatus` | source |
|---|---|
| map key ("service" name) | `Names`, first comma-separated element |
| `Running` | `State == "running"` |
| `Health` | parsed out of the `(...)` suffix of `Status` |
| `Created` | `parseCreatedAt(CreatedAt)`, formatted `2006-01-02 15:04` |
| `Uptime` | `formatUptime(Status)` — existing, already strips health suffixes |
| `Ports` | `parsePortsString(Ports)` — existing |
| `UpdateAvailable` | tasks 10-12; nil until then |

### Processing flow

1. Picker load calls `CountUnmanaged`. A non-zero count appends the synthetic `Project` with
   `Unmanaged: true` and `Status: "N containers"`. The append happens **after** `ListProjects`
   returns, deliberately bypassing `sortProjects` — that sorts case-insensitively by name
   (`compose.go:113,118`) and `(` sorts before every letter, so a sorted `(unmanaged)` would
   land first instead of last.
2. `enter` builds `HostContainers` through the widened factory and sets `m.projUnmanaged`.
3. `screenSelectContainers` runs its normal fetch batch — `loadServices`, `refreshStats`,
   `maybeRefreshUpdatesCmd`. No new message types, no new session counters.
4. `m.readOnly()` gates keys, selection widgets, footer and `?` overlay.

**No new goroutine, no new tea.Msg, no new session counter.** Every async path this screen
uses already exists and is already session-gated. Confirmed against the CLAUDE.md checklists:
`clearSearch()` gains no tenth departure site, `TestAllScreens_Complete` is unaffected (no new
screen), and `hasStatusColumns()` needs no change.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs inside this repo.
- **Post-Completion** (no checkboxes): manual verification against a real remote host.

## Implementation Steps

### Task 1: Parse host-level `docker ps` output

**Files:**
- Create: `internal/compose/hostcontainers.go`
- Create: `internal/compose/hostcontainers_test.go`
- Create: `internal/compose/testdata/docker_ps_host.json`

- [x] add `hostPsEntry` and `parseHostContainers(data []byte) ([]hostPsEntry, error)`, tolerant of both NDJSON and JSON-array forms
- [x] add `isComposeManaged(labels string) bool` — token scan for the `com.docker.compose.project=` prefix, no split-and-map
- [x] add `parseHealthFromStatus(status string) string` returning `healthy` / `unhealthy` / `starting` / `""`
- [x] add `hostContainerName(names string) string` returning the first comma-separated element
- [x] capture a real `docker ps -a --format '{{json .}}'` sample into the testdata file
- [x] write tests for `parseHostContainers` (NDJSON, array form, empty input, malformed line)
- [x] write tests for `isComposeManaged` (managed, unmanaged, and a label value containing a comma)
- [x] write tests for `parseHealthFromStatus` (all four outcomes, plus `Exited (0) 2 hours ago`)
- [x] run tests - must pass before task 2

### Task 2: Add the three-method `dockerRunner` seam and the `HostContainers` core

**[R2: no `runner.Composer` compile-time assertion in this task — it lands in task 4, once all
ten methods exist. `CheckUpdates` is a `(nil, nil)` stub here so the type is usable; tasks
10-12 replace it. `U` yields blank cells until then, which is the documented tri-state nil.]**

**Files:**
- Modify: `internal/compose/hostcontainers.go`
- Modify: `internal/compose/hostcontainers_test.go`

- [x] declare `dockerRunner` with `run` / `stream` / `tty`, plus `ErrReadOnly` and `HostContainers`
- [x] add the unexported `localDockerRunner` (over `*Compose`) and `remoteDockerRunner` (over `*RemoteCompose`) adapters, reusing `runDockerCmd`, `runDockerCmdStream`, `runRemoteDockerCmd`, `runRemoteDockerCmdStream` and `sshArgs`
- [x] add exported `NewLocalHostContainers` and `NewRemoteHostContainers` constructors
- [x] implement `Stop`, `Remove`, `Pull`, `Create`, `Start` as `return ErrReadOnly`
- [x] add `CheckUpdates` as a `(nil, nil)` stub with a TODO naming task 12
- [x] implement `ListServices` — unmanaged container names, sorted
- [x] implement `ContainerStatus` using the task 1 helpers plus existing `parseCreatedAt`, `formatUptime`, `parsePortsString`
- [x] write tests for `ListServices` and `ContainerStatus` via an injected fake `dockerRunner`
- [x] write tests asserting all five write methods return `ErrReadOnly`
- [x] write a test asserting the remote runner splices `SSHExtraArgs` immediately before the host argument
- [x] run tests - must pass before task 3

### Task 3: Implement `ContainerStats` through the seam

**[R2: no `ContainerStatsFromBulk`.** Its only consumer is `cmd/list.go:532` via
`bulkStatsAggregator`, and list integration is explicitly deferred. It would be dead code.**]**

**Files:**
- Modify: `internal/compose/hostcontainers.go`
- Modify: `internal/compose/hostcontainers_test.go`

- [x] implement `ContainerStats` as `run(ctx, "stats", "--no-stream", "--format", "json")` then `parseStatsOutput`, per design decision 4
- [x] join the parsed map against unmanaged container IDs by 12-char ID and aggregate
- [x] confirm a container present in `ps` but absent from `stats` is skipped, not an error
- [x] write tests for the ID join, including the present-in-ps-absent-from-stats case
- [x] write tests for a failed `docker stats` surfacing as an error, not a panic
- [x] run tests - must pass before task 4

### Task 4: Implement `Logs` and `ExecCommand`, and assert the interface

**Files:**
- Modify: `internal/compose/hostcontainers.go`
- Modify: `internal/compose/hostcontainers_test.go`
- Modify: `internal/tui/app_test.go`

- [x] implement `Logs(ctx, name, follow, tail, w)` over `stream`, building `docker logs [--follow] [--tail N] <name>`
- [x] wire **both** Stdout and Stderr to `w` — `docker logs` writes container stderr to process stderr, where most application logs land (matches `compose.go:934-936`) **[R2]**
- [x] implement `ExecCommand(ctx, name, command)` over `tty`, building `docker exec -it <name> <command...>`, defaulting to `DefaultExecCommand`
- [x] add `var _ runner.Composer = (*HostContainers)(nil)` now that all ten methods exist **[R2]**
- [x] add `var _ tui.ExecProvider = (*compose.HostContainers)(nil)` in `internal/tui/app_test.go` — `internal/tui` imports `internal/compose` (`app.go:20`), so the assertion cannot live in the `compose` package **[R2]**
- [x] add negative assertions in the same test file that `HostContainers` does NOT satisfy `ConfigProvider` or `RollbackPreparer`
- [x] write tests for `Logs` argv (follow on/off, tail value, remote escaping, no `-t`)
- [x] write tests for `ExecCommand` argv (default command, explicit command, remote `-t` present)
- [x] run tests - must pass before task 5

### Task 5: Surface the unmanaged row in the project picker

**[R2: `internal/tui/app.go` added — `viewSelectProject` renders `shortenPath(proj.ConfigDir)`
in the description column (`app.go:4213`), which is blank for this row. The count is carried in
`Project.Status`, which is already free-text (`"running(3)"`), so no new field is needed.
The append is extracted to a package-level helper because `cmd/root.go` has no test seam.]**

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/hostcontainers.go`
- Modify: `internal/compose/hostcontainers_test.go`
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`
- Modify: `cmd/root.go`

- [x] add `Unmanaged bool` to `compose.Project`, documenting that `ConfigDir` is empty and `Status` holds the count when it is set
- [x] add `CountUnmanaged(ctx) (int, error)` on `HostContainers`
- [x] add `WithUnmanagedRow(ctx, hc *HostContainers, projects []Project) []Project` — appends after `ListProjects` (bypassing `sortProjects`, per Processing flow step 1) and swallows a count error as zero
- [x] call the helper from both `ProjectLoader` literals in `cmd/root.go` as a one-line call
- [x] branch `viewSelectProject`'s description column on `proj.Unmanaged` to render `proj.Status` instead of `shortenPath(proj.ConfigDir)`
- [x] write tests for `CountUnmanaged` (some unmanaged, none, all managed)
- [x] write tests for `WithUnmanagedRow` (appended when non-zero, absent when zero, absent on error, always last)
- [x] write a render test asserting the row shows `(unmanaged)` and `N containers`
- [x] run tests - must pass before task 6

### Task 6: Widen `ComposerFactory` to take the whole `Project`

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `cmd/root.go`
- Modify: `internal/tui/app_test.go`

- [x] change `ComposerFactory` to `func(proj compose.Project) runner.Composer` and update its doc comment
- [x] update the `ConnectCallback` doc comment (`app.go:100-102`), which names the factory
- [x] update the call site at `app.go:1658` to pass `proj`
- [x] update the local factory literal (`cmd/root.go:101`) and the remote factory literal (`cmd/root.go:135`) to branch on `proj.Unmanaged` and call the matching exported constructor
- [x] update the five factory literals in `internal/tui/app_test.go`
- [x] write a test asserting the factory receives `Unmanaged: true` for the synthetic row and a real `ConfigDir` otherwise
- [x] run tests - must pass before task 7

### Task 7: Gate the write keys and isolate the update cache

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/compose/hostcontainers.go`
- Modify: `internal/tui/app_test.go`

- [ ] declare `ReadOnlyComposer` in `app.go` beside `ConfigProvider` and `ExecProvider`, with a **named** method — `ReadOnlyComposer() bool`. A method-less marker is `interface{}`, which every composer satisfies and would make `m.readOnly()` always true **[R2]**
- [ ] implement that method on `HostContainers`
- [ ] add `Model.readOnly() bool` type-asserting `m.composer`, nil-safe for `Model{}` test literals
- [ ] early-return from the `d`, `r`, `s`, `R`, `c`, `space` and `a` cases when `m.readOnly()`
- [ ] add `projUnmanaged bool` to Model, fold it into `updatesCacheKey()`, and set or clear it at exactly the four `m.projDir` assignment sites — `app.go:1543`, `:1628`, `:1657`, `:1777` **[R2, design decision 7]**
- [ ] write tests asserting each gated key produces no state change and no `pendingOp`
- [ ] write tests asserting `l`, `x`, `enter`-after-`x`, `/`, `U` still work on a read-only composer
- [ ] write a test asserting `m.readOnly()` is false for a normal composer and for a zero-value Model
- [ ] write a test asserting the unmanaged and local-fast-track contexts produce **different** `updatesCacheKey()` values, and that navigating between them does not replay one's verdicts onto the other
- [ ] run tests - must pass before task 8

### Task 8: Remove the selection affordances

**[R2: this was one checkbox inside revision 1's task 7. It is its own task because the caption
pad is silently coupled to the row checkbox and a mismatch misaligns every column.]**

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] drop the `(n/m selected)` counter from the container title when `m.readOnly()`
- [ ] drop the per-row `[ ]`/`[x]` checkbox (`app.go:4495-4498`) when `m.readOnly()`
- [ ] change the caption pad (`app.go:4466`) from 10 to 7 when `m.readOnly()`, and update its arithmetic comment to show both budgets
- [ ] write a render test asserting the read-only caption row aligns with its data columns
- [ ] write a render test asserting no `[ ]` appears on any read-only row
- [ ] run tests - must pass before task 9

### Task 9: Read-only footer and `?` overlay

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/help_test.go`
- Modify: `internal/tui/footer_reservation_test.go`

- [ ] make `containerHelpLines()` read-only aware — `line1` drops `space toggle`, `line2` becomes `l logs` and `x exec`
- [ ] confirm `containerFooterLines()` and `containerFooter()` stay correct by construction, since both read `containerHelpLines()`
- [ ] add a `readOnly bool` parameter to `helpGroupsFor` and supply it from `Model.helpGroups()` via `m.readOnly()`
- [ ] drop the SELECT and OPERATE groups in the read-only table, and drop `c` from INSPECT; keep MOVE, FIND, LEAVE
- [ ] move `enter` into the read-only INSPECT group as `{"enter", "confirm the exec prompt"}` — it is still bound via the `x` prompt **[R2, design decision 9]**
- [ ] preserve the load-bearing group ORDER so LEAVE still lands in the left column, and keep the `actions` flags that drive `singleColumnOrder()`
- [ ] union over both `readOnly` values in `helpKeyTokensFor`, matching how it unions over `allProgressPhases`
- [ ] extend `TestHelpGroups_NamesEveryBoundKey` with a read-only expected set, running BOTH directions
- [ ] extend `TestContainerFooter_AdvertisesOnlyWorkingKeys` (`footer_reservation_test.go:525`) with the read-only state **[R2]**
- [ ] extend `TestContainerFooterReservation` to sweep widths 40-180 for the read-only variant too
- [ ] extend `TestHelpGroups_LeaveGroupMatchesFooter` so the overlay cannot contradict the read-only footer
- [ ] write a test asserting no gated key (`d`, `r`, `s`, `R`, `c`, `space`, `a`) appears in the read-only footer or overlay
- [ ] run tests - must pass before task 10

---

*Tasks 10-12 deliver AC3b only. Everything above ships without them; the cost of stopping at
task 9 is the `⇧` column, and `CheckUpdates` keeps its task-2 stub. They are split three ways
because `internal/compose/updates.go` is the most invariant-dense file in the repo — five
CLAUDE.md paragraphs — and a single task doing a local refactor, a remote refactor and a new
feature cannot be bisected.* **[R2: revision 1 had this as one task and called it droppable
while `CheckUpdates` was still a mandatory interface method.]**

### Task 10: Extract the local `CheckUpdates` loop

**Files:**
- Modify: `internal/compose/updates.go`
- Modify: `internal/compose/updates_test.go`

- [ ] extract the loop at `updates.go:373-423` into a package-level helper taking a compare function and explicit knobs for the daemon cascade and the registry cascade
- [ ] extract `compareImageDigest` into a package-level form parameterised by a `dockerRunner`, and parameterise the error classification with it — the two runners emit different error shapes **[R2]**
- [ ] reduce `Compose.CheckUpdates` to a delegation
- [ ] verify every existing test in `updates_test.go` passes **unchanged** — this is the AC7 proof
- [ ] write tests for the extracted helper (daemon cascade, registry cascade, partial success)
- [ ] run tests - must pass before task 11

### Task 11: Extract the remote `CheckUpdates` loop

**Files:**
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/remote_test.go`

- [ ] extend the task 10 helper with the `errSSHTransport` early-abort knob, which the remote loop has and the local one does not (`remote.go:680-713`)
- [ ] confirm the remote path has **no** daemon cascade and that the knob defaults keep it that way
- [ ] reduce `RemoteCompose.CheckUpdates` to a delegation
- [ ] verify every existing test in `remote_test.go` passes **unchanged**
- [ ] write tests for the transport-abort knob (abort on first transport error, per-image failure absorbed)
- [ ] run tests - must pass before task 12

### Task 12: Implement `HostContainers.CheckUpdates`

**Files:**
- Modify: `internal/compose/hostcontainers.go`
- Modify: `internal/compose/hostcontainers_test.go`

- [ ] replace the task-2 stub, sourcing the name-to-image map from the `Image` field already returned by `docker ps` — no second call
- [ ] confirm an untagged image (where `Image` is an image ID, so `stripTag` and `imagetools inspect` fail) is absorbed as the tri-state absent, not an error **[R2]**
- [ ] write tests for a present verdict, a per-image failure absorbed as absent, and the registry cascade
- [ ] write a test for the untagged-image case
- [ ] write a test asserting the `⇧` glyph hydrates onto the read-only screen
- [ ] run tests - must pass before task 13

### Task 13: Verify acceptance criteria

- [ ] verify AC1-AC8 from the Overview, one by one
- [ ] verify the empty case: a host with zero unmanaged containers shows no extra row
- [ ] verify a failed `CountUnmanaged` is swallowed in the picker path and does not surface or crash **[R2: split from the next item]**
- [ ] verify a failed `ListServices`/`ContainerStatus` on the container screen surfaces in `svcErr`
- [ ] verify no gated key and no selection widget advertises a no-op anywhere in the UI
- [ ] run the full suite uncached: `go test ./... -count=1`
- [ ] run `go test ./... -race` — the plan adds no goroutine, so this must stay clean
- [ ] run `go vet ./...` and `go mod tidy`

### Task 14: [Final] Update documentation

- [ ] update `README.md` with the unmanaged-container view and its read-only limits
- [ ] add a CLAUDE.md architecture paragraph covering `HostContainers`, the three-method `dockerRunner` seam, the widened `ComposerFactory`, the `projUnmanaged` cache-key component, and the read-only footer/overlay/checkbox contract
- [ ] update the `Adding New Operations` checklist in CLAUDE.md to mention the read-only help variant
- [ ] update `skills/cdeploy/SKILL.md` if the view changes user-facing guidance, keeping the `skills/skills_test.go` pins satisfied
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**

- Run against a real remote host that has both a compose project and at least one hand-started
  container. Confirm the row count matches `docker ps` on that host.
- Confirm `x` exec works against an Alpine container with no bash, exercising the
  `command -v bash || exec sh` fallback.
- Confirm `l` logs streams container **stderr** as well as stdout, and that `esc` cancels the
  log context cleanly.
- Check a host with 50+ containers for picker and list responsiveness.
- Confirm the footer renders on one line at 80 columns in read-only mode.
- Confirm the `⇧` glyph does not leak between a compose project and the unmanaged view when
  navigating back and forth inside the 10-minute cache TTL.

**Deferred by design — record as follow-ups, do not build now:**

- Lifecycle keys (start / stop / restart) for unmanaged containers. This needs a separate
  non-compose operation path, since `runner.Run()` builds every pipeline from compose verbs.
  It also removes the safety property that every actionable row belongs to a project the user
  explicitly chose.
- Exposing unmanaged containers in `cdeploy list`. The `Project.Unmanaged` field added in
  task 5 travels there for free, but `cmd/list.go` uses its own factory type and would need
  `ContainerStatsFromBulk` on `HostContainers` (deliberately not built in task 3).
- Refreshing the picker row count on `esc` back-navigation. `m.projects` is cached, so the
  count is a load-time snapshot. See AC1.

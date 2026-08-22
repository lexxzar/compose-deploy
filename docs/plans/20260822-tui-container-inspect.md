# TUI Container Inspect Screen

> Revised after plan review. See "Review corrections" below for what changed and why.

## Overview

Add an `i` key on the container screen that opens a read-only inspect view for the
container under the cursor. The view has two modes: a **curated summary** (default)
and the **raw `docker inspect` JSON** behind an `r` toggle.

**Problem it solves.** When a container misbehaves the user needs the *running*
configuration, not the declared one. Today `c` shows `docker compose config` — what
the compose file declares. That answer is wrong in exactly the moment it matters:
the user changed an env var and pressed `r` restart instead of `d` deploy, so the
container still holds the old value. Inspect shows what the container actually has.

**The wedge is the health probe output.** `.State.Health.Log[last].Output` is the
last failing healthcheck's stdout. It is absent from `docker compose config`
entirely. The TUI already renders a `✗` unhealthy icon and the wait engine already
returns a `VerdictUnhealthy` — so the product creates the question "why is this
unhealthy?" and currently offers no way to answer it. Inspect closes that gap.

**The unmanaged screen is the primary target.** `compose.HostContainers` implements
neither `ConfigProvider` nor `RollbackPreparer`, so the read-only screen offers `l`
logs and `x` exec and nothing else. A hand-started container has no compose file, so
inspect is the *only* way to see its configuration. There, inspect has no competitor.

**Key benefit.** cdeploy's differentiator is remote reach with nothing installed on
the target host. `docker inspect` over the existing ControlMaster socket keeps that
promise — the alternative is `ssh host docker inspect x | jq`, which needs a shell
and `jq` on the far side.

## Review corrections

The first draft of this plan was reviewed against the codebase. Five substantive
corrections, recorded so the reasoning is not lost:

1. **The screen tax is NOT test-forced.** The draft claimed `TestAllScreens_Complete`
   fails until `allScreens` gains the new constant. It does not — see "No test forces
   the screen tax" below. This was the draft's single most misleading claim.
2. **Help-table wiring moved earlier.** Tax items now land in Task 7 with the screen
   constant, not in Task 10, so the drift pins guard Tasks 8-9 as they are written.
3. **Four more hand-maintained tables** were missing from the tax checklist.
4. **Parsing moved to `internal/compose`.** Every docker-output parser and every
   fixture in this repo lives there; `internal/tui/` has no `testdata/` at all.
5. **Scope cut from 10 summary sections to 5.** The draft drifted back toward the
   JSON dump it argues against.

Minor corrections are folded into the tasks: the `HostContainers` field is `docker`
not `runner`; `hostContainerName` already exists and must be reused;
`pickHostInspectContainer` needs no longest-running rule (docker enforces unique
container names per host, and CLAUDE.md states "there is no replica aggregation
because a host container is its own row").

## Context (from discovery)

**Files/components involved:**
- `internal/tui/app.go` (5,303 lines) — screen constants, Model fields, key dispatch,
  `View()`, `WindowSizeMsg`, the optional-interface declarations
- `internal/tui/help.go` (515 lines) — `screenName`, `helpGroupsFor`, `inspectGroup`
- `internal/tui/help_test.go` (1,594 lines) and `internal/tui/app_test.go` (15,232
  lines) — five hand-maintained per-screen tables, enumerated in the tax below
- `internal/compose/compose.go` / `remote.go` / `hostcontainers.go` — the three
  `runner.Composer` implementations that need an `Inspect` method
- `internal/compose/uptime.go` — `parseUptimeDuration` for the scaled-replica pick

**Related patterns found:**
- `screenConfig` is the structural template: viewport + lazy fetch + `configSession`
  stale-message counter + an `r` two-way toggle. `enterConfig()` at `app.go:2929` is
  the shape to copy (session bump, field reset, viewport sizing at `m.height - 6`,
  `m.clearSearch()`, return a fetch `tea.Cmd`).
- `ConfigProvider` / `ExecProvider` / `ReadOnlyComposer` (`app.go:54-101`) establish
  the optional-interface pattern: declared in `tui`, type-asserted on the concrete
  composer, absent implementation = the key silently no-ops.
- `docker inspect` is a **top-level docker command, not a compose subcommand**, so it
  must bypass `command()` / `remoteCommand()` — the same rule `docker image inspect`,
  `docker stats` and `docker buildx imagetools inspect` already follow. Use
  `Compose.runDockerCmd` (`updates.go:737`) and
  `RemoteCompose.runRemoteDockerCmd` (`remote.go:735`), which already splices
  `SSHExtraArgs` immediately before the host arg.
- `HostContainers` gets it nearly free: the `dockerRunner` seam's
  `run(ctx, args...)` — reached through the field named **`docker`**
  (`hostcontainers.go:130`) — captures and classifies (`errSSHTransport` included)
  on both local and remote with no new plumbing.
- **All docker-output parsing lives in `internal/compose`**: `parseContainerStatus`,
  `parseStatsOutput`, `parseHostContainers`, `parsePortsString`, `parseLocalDigest`,
  `parseImagetoolsDigest`, `parseHealthFromStatus`. Both existing fixtures live in
  `internal/compose/testdata/`. `internal/tui/` has no `testdata/` directory.

**Dependencies identified:**
- `psEntry.ID` and `hostPsEntry.ID` both already exist — container-ID resolution
  needs no new parse work.
- `runner.ServiceStatus` has **no** ID field and this plan does not add one. Widening
  that struct would push a docker implementation detail into the runner contract for
  no gain; the ID lookup stays inside `compose`, behind `Inspect`.
- No `Composer` interface change, so the four mock sets (`internal/runner/`,
  `internal/tui/app_test.go`, `cmd/deploy_test.go`, `cmd/list_test.go`) are untouched.

## Development Approach

- **testing approach**: Regular (code first, then tests)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `go test ./...` after each change
- maintain backward compatibility

## Testing Strategy

- **unit tests**: required for every task (see Development Approach above)
- **e2e tests**: this project has none (the root command needs a TTY, so
  `cmd/root_test.go` tests flag registration rather than execution). TUI behaviour is
  tested by calling `Update()` with `tea.KeyMsg` directly — no TTY needed.
- **fixtures**: capture real `docker inspect` output into
  `internal/compose/testdata/`, joining `buildx_imagetools_default_output.txt` and
  `docker_ps_host.json`. A synthetic fixture would not catch a field-shape surprise.
- **stdlib `testing` only** — no testify.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

A 9th screen, `screenInspect`, reached by `i` from the container screen. Chosen over
two alternatives:

- **Rejected — a third mode of `screenConfig`.** Config is *project*-scoped and
  inspect is *container*-scoped. The `r` toggle would become a 3-way cycle across two
  different subjects, `e` (edit in `$EDITOR`) would be dead in the third mode, and the
  read-only screen would open a screen whose first two modes do not exist at all
  (`HostContainers` is not a `ConfigProvider`).
- **Rejected — an overlay flag like `helpOpen`.** The `?` overlay avoids a screen
  constant precisely because it is static and does no IO. Inspect needs an async fetch
  (so a session counter), a scrollable viewport and its own key dispatch — most of the
  screen tax anyway, without the back-navigation clarity.

### Design decisions

**No secret masking — deliberate.** `docker inspect` prints `POSTGRES_PASSWORD` and
`DATABASE_URL=postgres://user:pass@host/db` in cleartext, and both modes here show it
verbatim. This matches lazydocker, k9s and Docker Desktop. The accepted consequence:
a user who screen-shares or records a terminal exposes credentials the moment they
press `i`. **Note this in the README so the behaviour is stated, not discovered.** If
masking is added later it must cover **both** modes — masking only the summary makes
`r` a one-key bypass, which protects nothing.

**Curate, do not dump — five sections, not ten.** `docker inspect` returns ~200 lines
of JSON per container. A viewport over that is strictly worse than
`ssh host docker inspect | jq`, because jq can query and a viewport cannot. The
summary is the feature; raw mode is the escape hatch. v1 renders **STATE** (including
restart policy and count), **HEALTH**, **IMAGE**, **MOUNTS**, **ENV**. NETWORKS,
PORTS, RESOURCES and LABELS are deferred: each is a question raw mode already answers,
and PORTS duplicates a column the container screen renders already.

**`i` is not gated on `m.readOnly()`.** Every other key added to the container screen
recently was gated, because every operation is a compose pipeline and an unmanaged
container has no compose file. Inspect inverts that: it is read-only by nature and
works identically on both screens. It therefore appears in **both** help variants.

**`i` stays out of the container footer.** The footer carries six tokens and the
one-line form already measures 74 cells at idle. Adding a seventh would force a
re-verification of the 80-column guarantee and the 40-180 width sweep in
`TestContainerFooterReservation`. `i` lives in the `?` overlay only, exactly like `c`,
`x`, `U` and `R`. **Consequence to verify in Task 10:** because `i` is a nowhere-else
key, it is subject to the single-column truncation order, and its insert position in
`inspectGroup` decides which key is sacrificed on a short narrow pane.

**No `refreshStatus()` on return.** `screenInspect` is read-only and changes no
container state, so `esc` returns without a status refresh — matching `screenConfig`,
not `screenLogs` / `screenProgress`.

**No CLI subcommand.** TUI-only, like the container search and the log filter.

## Technical Details

### No test forces the screen tax

**This is the most important thing to know before starting.** The draft of this plan
claimed `TestAllScreens_Complete` fails until `allScreens` gains the new constant.
**It does not.** The bound at `internal/tui/help_test.go:53` reads:

```go
if want := int(screenSettingsForm) + 1; len(allScreens) != want {
```

It is anchored to `screenSettingsForm`, not to the last constant. Appending
`screenInspect` after `screenSettingsForm` leaves `int(screenSettingsForm)` at 7, so
`want` stays 8 and `len(allScreens)` stays 8. **Verified: adding the constant alone
leaves the entire suite green.**

Because six tests iterate `allScreens` (`TestHelpGroups_EveryScreen`,
`TestHelpGroups_NamesEveryBoundKey`, `TestViewHelp_FitsShortTerminal`,
`TestViewHelp_MinimumHeightFloor`, `TestViewHelp_NoTrailingNewline`,
`TestViewHelp_NeverExceedsWidth`), a screen missing from that slice **silently skips
all six**. Nothing goes red. The tax is developer discipline, not a test.

➕ **Repo residual to record in Task 12:** the comment at `help_test.go:17` claims "a
9th screen that is not listed here fails that test", which is false. The bound should
anchor to the last constant. Fixing that test is out of scope here (it is a
pre-existing issue, not caused by this change) but must be written down.

### The `Inspector` optional interface

Declared in `internal/tui/app.go` beside `ConfigProvider` / `ExecProvider`:

```go
// Inspector returns the raw `docker inspect` JSON for one service's container.
type Inspector interface {
    Inspect(ctx context.Context, service string) ([]byte, error)
}
```

Implemented by `Compose`, `RemoteCompose` and `HostContainers`. A composer without it
(a test mock) makes `i` a silent no-op, matching the `c` / `x` guards.

### Container-ID resolution

| Composer | list call | inspect call |
|---|---|---|
| `Compose` | `docker compose ps -a --format json` via `command()` | `runDockerCmd(ctx, []string{"inspect", id})` |
| `RemoteCompose` | same via `remoteCommand()` | `runRemoteDockerCmd(ctx, []string{"inspect", id})` |
| `HostContainers` | `unmanagedEntries(ctx)` (exists) | `h.docker.run(ctx, "inspect", id)` |

**Scaled compose services — mirror the Uptime gate exactly.** `pickInspectContainer`
must reproduce the rule at `internal/compose/compose.go:877-887`, not a simplified
max-duration:

```go
if entry.State == "running" && entryUptime != "" {
    // longest duration wins, but ANY running replica beats a restarting one
} else if entryUptime == "restarting" && a.longestUpStr == "" {
    // only when nothing running has been seen
}
```

A naive max-duration picker disagrees with the rendered Uptime column on a
running/restarting mix, which is precisely AC3.

**Host containers need no such rule.** Docker enforces unique container names per
host and `HostContainers.ContainerStatus` keys its map one-to-one
(`hostcontainers.go:346-369`). `pickHostInspectContainer` is a first-match on
`hostContainerName(e.Names)` — **reuse that existing helper**
(`hostcontainers.go:98-103`), do not re-implement the comma split.

Both pickers return `("", false)` on no match; callers turn that into a
`no container found for %q` error, never a panic or a blank viewport.

### Parse layer — in `internal/compose`, not `internal/tui`

`inspectDoc` and its JSON parsing live in `internal/compose/inspect.go` beside the
pickers, matching every other docker-output parser in this repo. Only the renderer
lives in `tui`. `Inspect` still returns raw `[]byte` so raw mode stays byte-identical
to `docker inspect`.

```go
type InspectDoc struct {
    Name         string
    Image        string        // resolved sha256 digest
    RestartCount int
    State        inspectState  // Status, ExitCode, OOMKilled, StartedAt, Health
    Config       inspectConfig // Image, Cmd, Entrypoint, Env, Healthcheck
    HostConfig   inspectHost   // RestartPolicy
    Mounts       []InspectMount
}
```

Narrow by design — only the fields rendered, not a full Docker API type.

### Summary renderer

```go
func buildInspectSummary(doc compose.InspectDoc, width int) string
```

Pure, golden-testable, no TTY and no Docker. Sections, in order (a section with no
data is omitted; STATE always renders):

1. **STATE** — status, exit code, `OOMKilled`, started at, restart policy, restart count
2. **HEALTH** — status, failing streak, healthcheck test/interval/timeout/retries,
   and **the last probe's `Output`** ← the wedge
3. **IMAGE** — configured ref, resolved digest, command, entrypoint
4. **MOUNTS** — type, `source → destination`, rw flag
5. **ENV** — `KEY=VALUE`, verbatim

**Probe output must soft-wrap, not truncate.** Health probe output is routinely long
and multi-line (curl bodies, stack traces) and it is the feature's whole
justification. `viewConfig` puts content in a viewport with no wrap and no
`SetHorizontalStep`, so a truncating summary would cut the wedge at the terminal edge.
Reuse `softWrapLine` from `internal/tui/format.go`.

### Model fields, message and session

```go
// Screen: inspect
inspectService  string
inspectRaw      []byte          // raw `docker inspect` JSON (raw mode, verbatim)
inspectSummary  string          // rendered summary (cached; rebuilt on resize)
inspectShowRaw  bool            // false = summary (default), true = raw JSON
inspectViewport viewport.Model
inspectErr      error
inspectSession  uint64          // monotonic counter for stale message rejection
```

```go
type inspectDataMsg struct {
    data    []byte
    err     error
    session uint64
}
```

Handler gate, matching `configFileMsg`:
`if m.screen != screenInspect || msg.session != m.inspectSession { return m, nil }`

### The screen tax — the full list

Items 1-5 land in **Task 7** (with the constant). Items 6-11 land in **Task 10**.
Nothing here is forced by a failing test; see "No test forces the screen tax".

| # | Item | Location |
|---|---|---|
| 1 | `screenInspect` appended to the `screen` iota after `screenSettingsForm` | `app.go:202-209` |
| 2 | `allScreens` gains the constant — **unblocks the six iterating tests** | `help_test.go:19` |
| 3 | `TestAllScreens_Complete` bound → `int(screenInspect) + 1` | `help_test.go:53` |
| 4 | `screenName()` returns `"inspect"` | `help.go:331` |
| 5 | `helpGroupsFor()` gains `case screenInspect` — MOVE / VIEW / LEAVE | `help.go:187` |
| 6 | `bound` map gains a `screenInspect` row (see exact set below) | `help_test.go:128` |
| 7 | `inspectGroup()` gains `{"i", "inspect"}` — one shared append, position matters | `help.go:144` |
| 8 | `TestCtrlCConfirmation_AllRemoteScreens` gains a row | `app_test.go:8043` |
| 9 | `TestHelpOverlay_OpensFromEveryScreen` gains a row | `help_test.go:1011` |
| 10 | `TestHelpOverlay_SwallowsEveryActionKey`'s `containerKeys` gains `i` | `help_test.go:1454` |
| 11 | `TestViewHelp_NarrowTerminalKeepsActionKeys` want-list gains `"inspect"` | `help_test.go:666` |

Items 6, 8, 9 and 10 are **hand-maintained tables that do not iterate `allScreens`** —
they are invisible to item 2 and stay silent when forgotten.

Exact `bound` row for item 6 — the dispatch must bind precisely this set, including
`ctrl+c` → `tryQuit()`, or the reverse direction of the drift pin fires:

```go
screenInspect: {"q", "ctrl+c", "esc", "r", "up", "down", "pgup", "pgdown"},
```

Plus, outside the tables:
- `View()` gains `case screenInspect: return m.viewInspect()`
- `WindowSizeMsg` gains a `screenInspect` branch (`m.height - 6`, the config sizing —
  **not** the logs `- 7`, which reserves a row for the log bar)
- `clearSearch()` departure site #10: `enterInspect()`
- `q` → `esc` rewrite: `screenInspect` falls into the existing `default: key = "esc"`
  case. **No `typingInInput()` case is needed** — the screen opens no text input.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs inside this repo
- **Post-Completion** (no checkboxes): manual verification against real containers

## Implementation Steps

### Task 1: Add pure container-target pickers in compose

**Files:**
- Create: `internal/compose/inspect.go`
- Create: `internal/compose/inspect_test.go`

- [x] create `internal/compose/inspect.go` with `pickInspectContainer(entries []psEntry, service string) (string, bool)` **mirroring the `entry.State == "running"` gate at `compose.go:877-887` exactly** — any running replica beats a restarting one, longest duration wins among running
- [x] add `pickHostInspectContainer(entries []hostPsEntry, name string) (string, bool)` — first match on the existing `hostContainerName(e.Names)` helper, **no longest-running rule** (host container names are unique)
- [x] both return `("", false)` on no match — never a panic, never an arbitrary container
- [x] write table tests for `pickInspectContainer`: single match, scaled service picks longest-running, **running+restarting mix picks the running one**, all-restarting falls back, no match, empty slice
- [x] write table tests for `pickHostInspectContainer`: match, comma-joined `Names`, no match, empty slice
- [x] run `go test ./internal/compose/` — must pass before task 2

### Task 2: Add Compose.Inspect and RemoteCompose.Inspect

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/remote.go`
- Modify: `internal/compose/inspect_test.go`

- [x] add `Compose.Inspect(ctx, service)`: `docker compose ps -a --format json` through `command()`, parse to `[]psEntry`, `pickInspectContainer`, then `runDockerCmd(ctx, []string{"inspect", id})` — **bypassing `command()`**, because `docker inspect` is a top-level docker command
- [x] add `RemoteCompose.Inspect(ctx, service)`: same shape through `remoteCommand()` for ps and `runRemoteDockerCmd` for inspect, so `SSHExtraArgs` splices before the host arg for free
- [x] return a clear `no container found for %q` error when the picker misses
- [x] **test-seam note:** both the ps call and the inspect call route through the single `outputCmd` hook (`RemoteCompose` also has `outputErrCmd`, which takes precedence) — the test double must **dispatch on argv** to serve two different responses
- [x] write tests asserting exact argv for both (the inspect argv must NOT contain `compose`), remote shell-escaping of the container ID, and the `SSHExtraArgs`-before-host splice
- [x] write error-path tests: ps failure propagates, no-match produces the named error
- [x] run `go test ./internal/compose/` — must pass before task 3

### Task 3: Add HostContainers.Inspect through the dockerRunner seam

**Files:**
- Modify: `internal/compose/hostcontainers.go`
- Modify: `internal/compose/hostcontainers_test.go`

- [x] add `HostContainers.Inspect(ctx, name)`: `unmanagedEntries(ctx)` → `pickHostInspectContainer` → `h.docker.run(ctx, "inspect", id)` — the field is **`docker`**, not `runner`
- [x] confirm no new plumbing is needed — `run` already captures stderr and classifies `errSSHTransport` on the remote adapter
- [x] write tests through the existing fake `dockerRunner`: correct argv, local and remote adapters, no-match error, transport-error propagation
- [x] run `go test ./internal/compose/` — must pass before task 4

### Task 4: Parse docker inspect JSON in compose

**Files:**
- Modify: `internal/compose/inspect.go`
- Modify: `internal/compose/inspect_test.go`
- Create: `internal/compose/testdata/docker_inspect_healthy.json`
- Create: `internal/compose/testdata/docker_inspect_unhealthy.json`
- Create: `internal/compose/testdata/docker_inspect_stopped.json`

- [x] capture three **real** `docker inspect` outputs into `internal/compose/testdata/` (healthy with a healthcheck, unhealthy with a failing probe, stopped with a non-zero exit code)
- [x] add the exported `InspectDoc` struct family to `internal/compose/inspect.go` — narrow, only the fields the five sections render
- [x] add `ParseInspect(raw []byte) (InspectDoc, error)` handling the single-element JSON array
- [x] write tests against all three fixtures: fields populate correctly, an empty array errors, malformed JSON errors, a multi-element array takes the first
- [x] run `go test ./internal/compose/` — must pass before task 5

### Task 5: Render the STATE and HEALTH sections

**Files:**
- Create: `internal/tui/inspect.go`
- Create: `internal/tui/inspect_test.go`

- [x] create `internal/tui/inspect.go` with `buildInspectSummary(doc compose.InspectDoc, width int) string`
- [x] render STATE: status, exit code, `OOMKilled`, started at, restart policy, restart count — always rendered
- [x] render HEALTH: status, failing streak, healthcheck test/interval/timeout/retries, and the **last probe `Output`**; omit the whole section when the container has no healthcheck
- [x] soft-wrap the probe output via the existing `softWrapLine` in `internal/tui/format.go` — it must never truncate
- [x] write tests against all three fixtures: the unhealthy probe Output appears verbatim, HEALTH is absent without a healthcheck, STATE renders for a stopped container with its exit code
- [x] write a test asserting no rendered line exceeds the supplied width
- [x] run `go test ./internal/tui/` — must pass before task 6

### Task 6: Render the IMAGE, MOUNTS and ENV sections

**Files:**
- Modify: `internal/tui/inspect.go`
- Modify: `internal/tui/inspect_test.go`
- Modify: `internal/tui/styles.go` (only if reuse fails)

- [x] add IMAGE (configured ref, resolved digest, command, entrypoint)
- [x] add MOUNTS (type, `source → destination`, rw) and ENV (`KEY=VALUE`, verbatim — see Design decisions)
- [x] **check `groupHeaderStyle` (`styles.go:75`) and `helpGroupTitleStyle` (:130) for reuse before adding a new section style** — `helpGroupTitleStyle` was already reused by `inspectBuilder.section()` in Task 5, so no new style was added
- [x] write tests for each section: present, absent, and empty-collection cases
- [x] re-run the width test with all five sections — the synthetic "long values" doc in `TestBuildInspectSummary_NeverExceedsWidth` gained a long image ref, command, entrypoint, mount and env entry
- [x] run `go test ./internal/tui/` — must pass before task 7

### Task 7: Add the screen constant, tax items 1-5, Model fields and fetch command

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/help_test.go`

- [x] **read "No test forces the screen tax" first** — nothing below goes red if skipped
- [x] declare the `Inspector` interface beside `ConfigProvider` / `ExecProvider`
- [x] tax 1: append `screenInspect` to the `screen` iota after `screenSettingsForm`
- [x] tax 2-3: add it to `allScreens` and change `TestAllScreens_Complete`'s bound to `int(screenInspect) + 1` — the `allScreens` doc comment was reworded from "a 9th screen" to "a new screen", which the fixed bound now makes true
- [x] tax 4-5: add `"inspect"` to `screenName()` and `case screenInspect` to `helpGroupsFor()` — MOVE (`↑ ↓`, `pgup pgdown`), VIEW (`r`, `actions: true`), LEAVE 3rd of 3
- [x] tax 6: add the `screenInspect` row to the `bound` map with the exact key set from Technical Details
- [x] add the seven `inspect*` Model fields, `inspectDataMsg`, its session-gated handler, and `fetchInspect(session uint64) tea.Cmd`
- [x] write tests: a current-session message populates the fields, a stale session is discarded, an off-screen message is discarded
- [x] run `go test ./internal/tui/` — must pass before task 8
- ➕ [x] two helpers landed with the handler, for Tasks 8-9 to reuse: `rebuildInspectSummary()` (parse + render at the viewport's current width; a parse failure sets `inspectErr`, empties the summary and **keeps** `inspectRaw` so `r` stays a working escape hatch) and `setInspectContent()` (the single `SetContent` chokepoint, so the fetch handler, the `r` toggle and the resize branch cannot disagree about which buffer is on screen)
- ➕ [x] `mockInspectComposer` added to `app_test.go` for the `fetchInspect` tests — Task 8's mock-composer checkbox can reuse it rather than adding a second one

### Task 8: Add enterInspect and the `i` key on the container screen

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] add `enterInspect()` modelled on `enterConfig()`: bump `inspectSession`, reset the fields, size the viewport at `m.height - 6`, set `m.screen`, call `m.clearSearch()` (departure site #10), return the fetch command
- [x] add `case "i"` to the container dispatch: early-return when `len(m.services) == 0`, then type-assert `m.composer.(Inspector)` and no-op if absent — **no `m.readOnly()` gate**
- [x] **document in a comment** that the `i` no-op paths do not call `fixSvcOffset()`, matching the existing `l` / `x` guards rather than the read-only gates — an inherited hole, adopted knowingly
- [x] add a mock composer implementing `Inspector`, following the `TestReadOnly_GatesWriteKeys_WithCapableComposer` precedent — Task 7's `mockInspectComposer` covers the writable case; `readOnlyInspectComposer` (new) covers the read-only one, which is the case the not-gated-on-`readOnly` asymmetry actually rests on
- [x] write tests: `i` enters on a writable composer, `i` enters on a read-only composer, `i` no-ops on an empty list, `i` no-ops when the composer is not an `Inspector`
- [x] write a test asserting a committed container search is cleared by `enterInspect()`
- [x] run `go test ./internal/tui/` — must pass before task 9
- ➕ [x] `inspectTestModel(t, composer, services)` test helper added, so the four dispatch tests build the container screen the same way regardless of which composer double they drive

### Task 9: Add viewInspect, the `r` toggle, esc cleanup and resize

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] add `viewInspect()` rendering the breadcrumb (`cdeploy > … > inspect > <service>`), the viewport and a footer; reuse the existing error slot for `inspectErr`
- [x] add `View()`'s `case screenInspect`
- [x] add the `screenInspect` key case binding **exactly** the `bound` set from Task 7: `r` toggles `inspectShowRaw` and re-runs `SetContent`, arrows/pgup/pgdown reach the viewport, `esc` clears every `inspect*` field and returns to `screenSelectContainers` **without** a status refresh, `ctrl+c` goes through `tryQuit()`
- [x] add the `WindowSizeMsg` `screenInspect` branch (`m.height - 6`), rebuilding the summary at the new width without losing `inspectRaw`
- [x] write tests: `r` round-trips summary → raw → summary, `esc` clears and returns, `q` reaches the same path via the rewrite, resize rebuilds and preserves the raw bytes
- [x] run `go test ./internal/tui/` — must pass before task 10
- ➕ [x] `clearInspect()` extracted as the single departure-cleanup helper (the `esc` handler calls it), so a future departure site cannot reset a partial field set. It deliberately does **not** bump `inspectSession` — `enterInspect()` bumps on the way in, which is what invalidates an in-flight fetch, and the handler's screen check discards one that lands after departure. Same discipline as `configSession`.
- ➕ [x] the `r` toggle calls `inspectViewport.GotoTop()` after the chokepoint: `SetContent` preserves `YOffset`, so a toggle from a scrolled summary into the much longer raw JSON would land the reader mid-document. Pinned by `TestInspectScreen_RTogglePutsTheReaderAtTheTop`.
- ➕ [x] `viewInspect()` keeps the viewport on screen **beside** the error line when `inspectRaw` is non-empty (the parse-failure case), so `r` stays a working escape hatch; it reads as "Loading..." only when there are neither bytes nor an error. Pinned by `TestViewInspect_ParseErrorKeepsViewportOnScreen`.
- ⚠️ pre-existing, untouched: `gofmt -l .` reports `cmd/list_test.go`. Not caused by this task and out of scope per the focus rule.

### Task 10: Pay the remaining screen tax — container key and the four silent tables

**Files:**
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/help_test.go`
- Modify: `internal/tui/app_test.go`

- [x] tax 7: add `{"i", "inspect"}` to `inspectGroup()` — **one shared append, not two branches**; it lands in both the writable and read-only variants
- [x] add `i` to the `screenSelectContainers` row of the `bound` map, and confirm both drift-pin directions pass for the writable and read-only variants — the read-only `bound` list in `TestHelpGroups_ReadOnlyNamesEveryBoundKey` needed it too, since `i` is the one container key added without a `readOnly` gate
- [x] tax 8: add a `screenInspect` row to `TestCtrlCConfirmation_AllRemoteScreens` (`app_test.go:8043`) — the screen is reachable on a remote session by design, so this is the exact configuration that would otherwise be unpinned
- [x] tax 9: add a `screenInspect` row to `TestHelpOverlay_OpensFromEveryScreen` (`help_test.go:1011`) — want `summary / raw JSON`, notWant `$EDITOR` (screenConfig is the table inspect could be confused with)
- [x] tax 10: add `i` to `containerKeys` in `TestHelpOverlay_SwallowsEveryActionKey` (`help_test.go:1454`)
- [x] tax 11: add `"inspect"` to the want-list in `TestViewHelp_NarrowTerminalKeepsActionKeys` (`help_test.go:666-678`)
- [x] **re-measure the single-column truncation table** at width 50 for heights 21-24 and choose `i`'s insert position in `inspectGroup` deliberately — a 19th action row shifts what falls off, and `i` must not be the first key sacrificed given it is absent from the footer
- [x] run `go test ./internal/tui/` — must pass before task 11

➕ **Re-measured single-column truncation table — the numbers Task 12 must copy into
CLAUDE.md.** Measured by rendering `Model{screen: screenSelectContainers, showPicker:
true, helpOpen: true}` and diffing the full nowhere-else action-key set out of the
stripped view. Verified **identical at widths 30, 40, 50, 59 and 64** (below the
65-column two-column threshold, width does not change the row count):

| pane height | what the overlay still names |
|---|---|
| >= 24 | everything (all 19 action rows fit the 20 a 24-line pane keeps) |
| 23 | loses `U check updates` |
| 22 | loses `x exec` too |
| 21 | loses `c config` too |
| 20 | loses `i inspect` too |
| 19 | loses `l logs` too |

Every threshold moved up by exactly one from the pre-`i` table (which read: >= 23
keeps everything, 22 loses `U`, 21 loses `x`, 20 loses `c`). Row counts changed from
**18 action rows / 24 stacked rows** to **19 action rows / 25 stacked rows**; the
read-only variant went from 15 to 16 stacked rows and still fits every action row at
every height >= 19, so it never truncates an action key.

**CLAUDE.md sentences that are now stale** (Task 12 replaces them):
- "Truncation table at width < 65 (below the two-column threshold): height >= 23
  keeps everything, 22 loses `U check updates`, 21 loses `x exec` too."
- "that takes the container table from 29 rows to 24, and 24 is what makes all 18
  action rows fit the 19 a 24-line pane keeps" — now 30 → 25, 19 action rows, and a
  24-line pane keeps 20 rows (19 real + the `▼ N more` marker).
- the "17-row container key table" and "six tokens … `a all`, `/ search`, `n N`,
  `R rollback`, `c config`, `x exec` and `U check updates`" nowhere-else list must
  gain `i inspect` (18 rows, eight nowhere-else keys).

➕ [x] **`i`'s position is now test-pinned, not just commented.**
`TestViewHelp_NarrowTerminalKeepsActionKeys` samples height 24, where all 19 action
rows still fit — so appending `i` last would have passed it and left the position
unguarded. `TestViewHelp_InspectSurvivesTheFirstTruncation` (new, `help_test.go`)
samples height **23**, the first notch where something must go, and asserts `inspect`
survives while `check updates` is the key sacrificed; it also re-asserts the
full-fit height so a future 20th action row surfaces as a moved threshold rather than
a silent loss. Verified mutation-sensitive by moving `{"i", "inspect"}` to the end of
`inspectGroup` — the new test goes red at all four widths while every other test
stays green.

### Task 11: Verify acceptance criteria

Every AC below is pinned by a named automated test that exists and passes. No AC is
marked done by inspection.

- [x] AC1 — an unhealthy container's last probe `Output` is visible and **soft-wrapped, not truncated** (the wedge) — pinned by `TestBuildInspectSummary_UnhealthyProbeOutput` (the probe string reaches the summary verbatim, with the HEALTH rows around it), `TestBuildInspectSummary_ProbeOutputWrapsNotTruncates` (at widths 30/40/60 the probe no longer fits one line yet every rune survives) and `TestBuildInspectSummary_MultiLineProbeOutput`
- [x] AC2 — a stopped container renders STATE with its exit code and omits HEALTH — pinned by `TestBuildInspectSummary_StoppedFixture` (asserts `status exited` + `exit code 3` and that `HEALTH` is absent) and by the absent-case row of `TestBuildInspectSummary_HealthSectionPresence`
- [x] AC3 — a scaled service inspects the replica the Uptime column shows, **including on a running+restarting mix** — pinned by `TestPickInspectContainer_MatchesUptimeColumn`, which drives the SAME entries through `pickInspectContainer` and `parseContainerStatus` and fails if the two disagree; plus the picker table in `TestPickInspectContainer` and the end-to-end `TestComposeInspect_PicksTheUptimeReplica` (asserts the inspect argv carries the running replica's ID)
- [x] AC4 — a service with no container yields a named error, not a panic or a blank viewport — pinned at the composer layer by `TestComposeInspect_NoContainerFound`, `TestRemoteInspect_NoContainerFound` and `TestHostContainers_Inspect_NoMatch` (all assert the `no container found for %q` text), and at the TUI layer by `TestInspectDataMsg_FetchError` plus `TestViewInspect_ShowsFetchError`. ➕ Added `TestInspectKey_MissingContainerSurfacesNamedError`, which drives the whole path (`i` → fetch cmd → `inspectDataMsg` → `viewInspect()`) and asserts the rendered screen names the service and does not read as loading — no test covered that end to end before
- [x] AC5 — `i` works on the read-only unmanaged screen, where `c` does not exist — pinned by `TestInspectKey_EntersOnReadOnlyComposer`, which asserts `m.readOnly()` as a precondition and then that `i` still reaches `screenInspect` with a fetch command; the `c` half is already pinned by `TestReadOnly_GatesWriteKeys`
- [x] AC6 — `r` toggles to raw JSON and back; `esc` and `q` both return with the fields cleared — pinned by `TestInspectScreen_RToggleRoundTrips` (summary → raw → summary, footer label follows, raw bytes undisturbed), `TestInspectScreen_EscClearsAndReturns` (every `inspect*` field cleared, viewport reset, no status refresh), `TestInspectScreen_EscClearsErrorSlot` and `TestInspectScreen_QTakesTheSamePath`
- [x] AC7 — the `?` overlay names `i` on both container variants and lists the inspect screen's own keys — pinned by `TestHelpGroups_NamesEveryBoundKey` (writable `bound[screenSelectContainers]` contains `i`, and `bound[screenInspect]` is the screen's own 8-key set, both directions) and `TestHelpGroups_ReadOnlyNamesEveryBoundKey` (read-only `bound` contains `i`); `TestHelpOverlay_OpensFromEveryScreen` covers the inspect screen's overlay content (`summary / raw JSON`)
- [x] AC8 — `ctrl+c` on a remote session shows the disconnect prompt from `screenInspect` — pinned by the `inspect` row of `TestCtrlCConfirmation_AllRemoteScreens`
- [x] run the full suite: `go test ./... -count=1` — all 7 packages pass
- [x] build: `go build -o cdeploy .` and run `go vet ./...` — both clean

### Task 12: [Final] Update documentation

- [ ] update `README.md` — the `i` key, the `r` toggle, and **state plainly that env values are shown verbatim, including secrets**
- [ ] update `CLAUDE.md` with an "Inspect screen" paragraph: the `Inspector` optional interface, the top-level-docker-command bypass rule, the Uptime-gate mirror in the picker, the not-gated-on-`readOnly` asymmetry versus every other recent container key, and the no-masking decision
- [ ] update `CLAUDE.md`'s documented truncation table with the re-measured heights from Task 10 (the current text — "height >= 23 keeps everything, 22 loses `U check updates`, 21 loses `x exec`" and "24 is what makes all 18 action rows fit" — is stale once a 19th row exists)
- [ ] ➕ record the repo residual: `help_test.go:17`'s comment and the `TestAllScreens_Complete` bound are anchored to `screenSettingsForm` rather than the last constant, so the "9th screen fails that test" promise is false
- [ ] record the residual that `i` is deliberately absent from the container footer
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification:**
- inspect a genuinely unhealthy container (a deliberately failing healthcheck) and
  confirm the probe output is the thing you actually needed — this is the feature's
  whole justification, and a fixture cannot confirm it
- inspect over a real SSH hop and confirm the latency is acceptable; the ps + inspect
  pair is two round-trips, returning ~10-50KB
- inspect an unmanaged container on a remote host with nothing installed there — the
  primary use case
- confirm a legacy standalone-compose host still resolves container IDs

**Follow-ups deliberately out of scope:**
- the five deferred summary sections (NETWORKS, PORTS, RESOURCES, LABELS, and a
  standalone RESTART POLICY block) — raw mode answers each today
- adding `i` to the read-only container footer (touches the reservation math)
- a `cdeploy inspect` CLI subcommand
- fixing the `TestAllScreens_Complete` bound so a 9th screen genuinely does fail it
- secret masking — if ever added it must cover **both** modes

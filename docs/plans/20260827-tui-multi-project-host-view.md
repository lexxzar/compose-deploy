# TUI Multi-Project Host View

## Overview
- Replace the one-project container screen flow with a grouped host view: after server select, the container screen shows every compose project on the host as a foldable group, plus the unmanaged group.
- Operations (`d`/`r`/`s`) accept a selection that spans projects and run one full pipeline per project, sequentially. A failed batch stops the sequence.
- `enter` on a group header drills into single-project mode (today's exact screen). The project picker screen (`screenSelectProject`) is deleted.
- Solves: no way to see or operate on more than one compose project without esc-esc-repick; no host-wide triage view.

## Context (from discovery)
- Files involved: `internal/tui/app.go` (entry model, loader, ops, sessions), `internal/tui/help.go` (+ `help_test.go` pins), `internal/tui/footer_reservation_test.go`, `internal/compose/hostcontainers.go` (label grouping over `hostPsArgs`), `cmd/root.go` (`ProjectLoader`/`ComposerFactory` wiring), `internal/tui/app_test.go` (fixture migration).
- Pattern to reuse: `serverEntry` header/selectable row slice with `nextSelectable`/`prevSelectable` (`app.go:168-238`).
- Hard constraints from code: `runner.Run` defer-closes its events channel (batches cannot share one); `enterProgress` takes flat service names (needs batches); selection is `map[int]bool` over `m.services` (index-keyed, breaks on fold rebuild); `svcStatus` is keyed by bare service name (collides across projects); `handleStepEvent` resolves events by step NAME (`app.go:2883`), which cannot survive prefixed multi-batch step lists; `pipelineDoneMsg` is `struct{}` with no identity.
- Fixture inventory (drives phase-1 ordering): `app_test.go` holds ~193 `services: []string` literals, 16 `selected: map[int]bool` literals, 41 `m.svcStatus["name"]` reads; `footer_reservation_test.go` sets `m.services`/`m.selected` directly and calls `computeMatches` 7 times.
- Load-bearing subsystem rules: read `docs/architecture/tui-help-overlay.md`, `update-detection.md`, `unmanaged-containers.md`, `wait-snapshots-rollback.md` before touching those subsystems.

## Development Approach
- **Testing approach**: Regular (code first, then tests, per task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods and modified code paths
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- **CRITICAL: every task that adds a Model field must clear it at every departure site** (esc chain, `entryLocal`, `connectResultMsg` error path) and pin that with a test — CLAUDE.md names back-navigation cleanup as the most fragile invariant
- Run `go test ./...` after each change
- Phase 1 must produce ZERO visible change: drilled/single-group mode renders byte-identical to today, pinned by the existing test suite

## Testing Strategy
- **Unit tests**: required for every task (stdlib `testing` only — no testify)
- No e2e framework in this project; TUI is tested by driving `Update()` with `tea.KeyMsg` directly
- No test may hit Docker or a registry; compose-side tests go through the `dockerRunner` seam and `SetTestHooks`
- The existing 18k-line `internal/tui/app_test.go` is the regression pin for the degenerate (single-group) mode

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope

## Solution Overview
Confirmed design decisions (brainstormed and validated; do not re-litigate):
- **Entry model**: `svcGroup{proj compose.Project, services []string, folded bool}` is the source of truth; `svcEntry{kind: entryGroupHeader|entrySvc, groupIdx int, name string}` is derived by `rebuildSvcEntries()`. A single group emits NO header entries — that is the degenerate mode.
- **Cursor lands on headers** (unlike the server picker): `space` on a header folds; `space` on a service selects; `enter` on a header drills in.
- **Selection keys on identity**: `m.selected` becomes `map[string]bool` keyed `proj.Name + "/" + service` (`svcKey`). `svcStatus`, `svcStats`, and update verdicts move to the same qualified key. Qualified keys live ONLY inside the tui Model; every message boundary converts (see Task 3).
- **Grouped loading is two host-wide calls**: `ListProjects` (`docker compose ls -a`) + `hostPsArgs` (`docker ps -a --format {{json .}}`), grouped by `com.docker.compose.project` / `com.docker.compose.service` labels. Stats stay one `AllContainerStats` call joined by container ID. Accepted gap: never-created services are absent as ROWS in grouped mode; drill-in (`ListServices`) shows them. Whole-group ops do NOT inherit the gap (empty batch slice = all services, resolved by compose itself).
- **The grouped host-status seam is `HostGrouper`**: a tui-declared interface type-asserted on the concrete composer (`Inspector` pattern), implemented by `compose.HostContainers` for local and remote. No existing signature (`ProjectLoader`, `ConnectCallback`, `ComposerFactory`) changes.
- **Ops partition into `[]opBatch{proj, services}`** ordered by screen position. Empty selection = cursor's group with an EMPTY services slice (runner reads empty as ALL — includes never-created services). Unmanaged rows are unselectable and never enter a batch; `l`/`x`/`i` (and `c` on compose groups) bind the composer at action time.
- **Action-time composer binding**: grouped mode keeps `m.composer == nil`. An action key (`l`/`x`/`i`/`c`) sets `m.composer = composerFactory(group.proj)` before calling the existing `enter*` helper. Every return-to-grouped site clears it to nil and dispatches `loadGroups()` — those sites already bump sessions.
- **Sequential execution is message-driven**: `enterProgress(batches)` builds ALL steps upfront with project-prefixed display names ("web: pull"); batch i = factory → per-batch Deploy snapshot → `runner.Run` into its OWN channel; step events resolve within the CURRENT batch's step range, never by global name scan; `pipelineDoneMsg`/`batchDoneMsg` carry batch index + a sequence session. Failure stops the sequence; the rest render skipped. `esc` cancels the current batch ctx.
- **`R` rollback stays single-group**; a cross-group capture is refused.
- **Flow**: server select (or Local without cwd compose file) lands on the grouped screen; local fast-track to drilled mode unchanged; `esc`: drilled→grouped (reload), grouped→server screen. Drill-in/out are composer swaps: bump `statusSession`/`statsSession`/`updatesSession`. The `connectResultMsg` SUCCESS path becomes a live-data landing site and now bumps the three counters too (this inverts a documented CLAUDE.md rule — update it in Task 15).
- **Updates**: NO automatic scan in grouped mode; `U` scans the cursor's group only, one cache entry under the existing per-project `updatesCacheKey` (shared with drilled mode). `updatesMsg` gains a `forKey` captured at dispatch — the cursor may move before arrival. Folded headers show `⇧ n` from cached verdicts only.
- **Errors**: existing ladder `svcErr > statsErr > updatesErr` absorbs all new failures.

## Technical Details
- Header row render: `▼ web   ● 3 up  ✗ 1  ⇧ 2` (`▶` when folded); aggregates derive from the group's status entries. Service rows indent 2 cells in grouped mode only. `svcVisibleCount` math is unchanged — headers are ordinary entries. Captions row and scroll indicators get the indent offset.
- Grouped-mode breadcrumb: `cdeploy > server > host`; drilled: `cdeploy > server > proj`. The title's `(n/m selected)` denominator in grouped mode counts selectable (compose) service rows only.
- `computeMatches` matches service entries only, returns entry indices — `/`, `n`, `N` unchanged.
- `m.readOnly()` stays composer-based and nil-safe; grouped mode is not globally read-only. Unmanaged rows render NO checkbox column cell; `a` and `allSelected()` skip them.
- `svcKey` uses `/` as separator — safe because compose project and service names cannot contain `/` (pin with a test note).
- Deleting `screenSelectProject` renumbers the screen iota; the full sweep list lives in Task 13.
- New keys must land in `helpGroupsFor` AND `TestHelpGroups_NamesEveryBoundKey` (both directions), per the help-overlay rules. Group ORDER is load-bearing — additions must not reorder LEAVE.

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs in this repo
- **Post-Completion** (no checkboxes): manual TUI verification against a real multi-project host

## Implementation Steps

### Task 1: Entry model types and rebuildSvcEntries (phase 1)

**Files:**
- Create: `internal/tui/entries.go`
- Create: `internal/tui/entries_test.go`

- [x] add `svcEntryKind` (`entrySvcGroupHeader`, `entrySvcService`), `svcEntry`, `svcGroup` types
- [x] implement `rebuildSvcEntries(groups []svcGroup) []svcEntry`: one header per group then its services; a folded group emits only its header; a single group emits NO header
- [x] implement `svcKey(projName, service string) string` as the single key producer, with the `/`-separator safety note
- [x] write tests: single group no headers, multi-group headers, fold hides services, empty group renders bare header, svcKey distinctness
- [x] run tests - must pass before task 2

### Task 2: Fixture helper and mechanical migration (phase 1)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`
- Modify: `internal/tui/footer_reservation_test.go`

- [x] add `svcGroups []svcGroup` and `svcEntries []svcEntry` Model fields; a single-group seam keeps `m.services`-driven behavior working unchanged for now
- [x] add `singleGroupModel(services []string)` test helper that populates BOTH the old fields (`services`, `selected`) and the new ones
- [x] migrate the `Model{services: ...}` / `selected: map[int]bool` literals in both test files to the helper, mechanically
- [x] write a pin test: one-group `viewSelectContainers()` output has no header rows and no indent
- [x] run FULL suite `go test ./... -count=1` - must pass before task 3 (zero visible change)

### Task 3: Qualified keys and message-boundary conversion (phase 1)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/entries.go`
- Modify: `internal/tui/app_test.go`
- Modify: `internal/tui/entries_test.go`
- Modify: `internal/tui/footer_reservation_test.go`
- Modify: `internal/tui/help_test.go` (➕ Task 2 left `services:`/`selected:` literals here unmigrated)

- [x] change `m.selected` to `map[string]bool` keyed by `svcKey`; rewrite the `space` handler, the `a` toggle, and `allSelected()` over service entries; update `singleGroupModel`
- [x] key `svcStatus`/`svcStats` through `svcKey`; `servicesMsg`/`statusMsg`/`statsMsg` handlers QUALIFY the incoming bare-name maps with the owning group at arrival
- [x] `selectedContainers()`, `opContainers`, `rollbackTargets`, and the wait seed keep returning BARE names — qualified keys never cross into `runner`
- [x] update `hydrateUpdates` to write verdicts under qualified keys and keep its skip-unknown-name rule
- [x] write tests: selection survives a fold rebuild; duplicate service names in two groups stay distinct; a qualified key never reaches `runner.Run`/`NewWaitState` (pin at the boundary)
- [x] run tests - must pass before task 4

### Task 4: Cursor, offset, and render over entries (phase 1)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/entries.go` (➕ the row-model helpers `svcRefs`/`cursorEntry`/`cursorService` live beside `svcKeyAt`, not in `app.go`)
- Modify: `internal/tui/app_test.go`
- Modify: `internal/tui/footer_reservation_test.go`

- [x] move `svcCursor`/`svcOffset`/`fixSvcOffset`/`svcVisibleCount` to entry indices; cursor may land on headers; remove the task-2 single-group seam
- [x] `viewSelectContainers()` renders entries; with one group the output is byte-identical to today (no headers, no indent)
- [x] `computeMatches` matches service entries only and returns ascending entry indices; migrate its call sites in both test files; `/`, `n`, `N`, two-stage `esc` unchanged
- [x] write tests: cursor motion over mixed header/service entries; search jump skips headers; offset math with headers present
- [x] run FULL suite `go test ./... -count=1` - must pass before task 5 (phase-1 exit gate)

### Task 5: HostGrouper — host-wide label grouping in compose (phase 2)

**Files:**
- Modify: `internal/compose/hostcontainers.go`
- Modify: `internal/compose/hostcontainers_test.go`

- [x] add `GroupedStatus(ctx) (map[string]map[string]runner.ServiceStatus, error)` on `HostContainers` over `hostPsArgs` output: extract `com.docker.compose.project`/`.service` label VALUES (token-start match per the `isComposeManaged` rule; value read to the next comma — safe because both label values are name-constrained, record that beside the code); no-label containers form the `(unmanaged)` group
- [x] reuse `parseHealthFromStatus`, `formatUptime`, `parsePortsString`; scaled services aggregate with the existing rules (Running OR, worst-case Health, oldest Created, longest Uptime)
- [x] both local and remote runners get it through the existing three-method `dockerRunner` seam — no new SSH plumbing
- [x] write tests through a fake `dockerRunner`: grouping, unmanaged bucket, scaled replicas, label-value-with-comma safety, remote splice unchanged
- [x] run tests - must pass before task 6

### Task 6: Grouped loader, landing flow, and refresh dispatch (phase 2)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `cmd/root_test.go` (➕ `cmd/root.go` needed NO change — `localComposerFor`/`remoteComposerFor` already return `*compose.HostContainers` for `Unmanaged: true`, which now satisfies `HostGrouper`; the test pins that wiring in both factories)
- Modify: `internal/compose/hostcontainers.go` + `_test.go` (➕ `GroupedStats`, the stats half of the seam — Task 5 added only `GroupedStatus`, and `HostContainers.ContainerStats` filters to unmanaged containers, so grouped stats had nowhere to come from)
- Modify: `internal/tui/entries.go` + `_test.go` (➕ the pure `buildSvcGroups`/`flattenQualified` merge helpers live beside the other row-model code)

⚠️ Deviations taken to keep the landing flow shippable on its own:
- The `d`/`r`/`s`/`R`/`c`/`l` refusal gate (a Task 7 checkbox) landed here: grouped mode holds `m.composer == nil`, and `d`/`r`/`s` arm a confirm whose `enter` calls `enterProgress`, while `l` calls `enterLogs` — both would dereference nil.
- `autoUpdatesAllowed()` gained the `!m.grouped` term and `maybeRefreshUpdatesCmd` an early return (Task 12's first checkbox), for the same reason: `refreshUpdates`/`refillUpdateDetailsCmd` read `m.composer`.
- `esc` grouped → server screen (a Task 8 checkbox) landed here because `canGoBack()` had to be decided now, and a footer that advertises `back` on a key that does nothing is the failure the shared predicate exists to prevent. Both callers go through the new `backToServerScreen()` helper.
- `screenSelectProject` stays REACHABLE, via `esc` from the `entryLocal` drilled fast track. Task 8 rewires that esc to drill-out; Task 13 then deletes the screen.

- [x] declare `HostGrouper` in `tui` and type-assert it on `composerFactory(compose.Project{Unmanaged: true})`; add `loadGroups()` Cmd: `ProjectLoader` + `GroupedStatus` merged into a widened grouped `servicesMsg` (same `session` discipline)
- [x] landing flow: server select and `entryLocal` (no cwd compose file) land on the grouped container screen; local fast-track to drilled single-group mode unchanged; update `NewModel`'s start-screen decision table and `Init()`'s `showPicker` dispatch to `loadGroups()`
- [x] `connectResultMsg` SUCCESS path lands on grouped data: bump `statusSession`/`statsSession`/`updatesSession`, reset `updateInFlight`/`refreshInFlight`; error path unchanged
- [x] decide `canGoBack()`, the `showPicker` reads, and the `q`-quits-vs-rewrite rule for the grouped screen NOW (Task 13 stays pure deletion); grouped screen is non-root when servers exist
- [x] enumerate and convert ALL 7 refresh call sites plus the `refreshTickMsg` gate: grouped mode dispatches `loadGroups()`+`refreshStats()`; drilled mode keeps `refreshStatus()`; the tick gate's `m.composer == nil` check gains the grouped branch
- [x] stats: one `AllContainerStats` call joined by container ID per group via qualified keys; `refreshInFlight` guard unchanged; `ListProjects`/host-ps failure → `svcErr`, stats failure → `statsErr`
- [x] clear `svcGroups`/`svcEntries` at every departure site (esc chain, `entryLocal`, `connectResultMsg` error path)
- [x] write tests: grouped `servicesMsg` hydration, session rejection of stale grouped payloads, landing decision table, tick dispatch per mode, departure cleanup, error paths
- [x] run tests - must pass before task 7

### Task 7: Fold, header aggregates, footer, and grouped rendering (phase 2)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/help_test.go`
- Modify: `internal/tui/footer_reservation_test.go`

- [ ] `space` on a header toggles `folded`, calls `rebuildSvcEntries` + `fixSvcOffset`; `space` on a service selects as today; `space` on an unmanaged row is a no-op with `fixSvcOffset()` first
- [ ] header renders `▼/▶ name  ● n up  ✗ n` aggregates; service rows indent 2 cells; captions pad and scroll indicators get the offset; unmanaged rows render NO checkbox cell; `a`/`allSelected()` skip unmanaged rows; title counts selectable rows only
- [ ] footer: decide the grouped idle pair in `containerHelpLines()`; the line COUNT stays state-independent; every line clamped with `clampToWidth` and measured with `ansi.StringWidth`
- [ ] grouped mode refuses `d`/`r`/`s`/`R`/`c` for now (⚠️ scaffolding until Task 9 — the writable help table names keys the gate refuses for two tasks; accepted, remove in Task 9)
- [ ] name the fold binding in `helpGroupsFor` with a description distinguishing header vs service `space`; group ORDER unchanged (LEAVE stays 4th of 6)
- [ ] write tests: fold/unfold rebuild, aggregate counts, refusal gates, unmanaged row rendering/selection skips, footer reservation; extend `TestHelpGroups_NamesEveryBoundKey` both directions
- [ ] run tests - must pass before task 8

### Task 8: Drill-in, drill-out, and action-time composer (phase 2)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/help_test.go`

- [ ] `enter` on a header drills in: `m.svcGroups` = that one group, `m.composer = composerFactory(group.proj)`, reload via `loadServices` (full fidelity), breadcrumb `cdeploy > server > proj`; bump the three session counters
- [ ] `esc` from drilled mode returns to grouped mode: `m.composer = nil`, reload groups, bump sessions, follow the callback-cleanup discipline; `esc` from grouped mode returns to the server screen
- [ ] `l`/`x`/`i`/`c` in grouped mode: set `m.composer = composerFactory(cursor group.proj)` at the action key, then call the existing `enter*` helper; every return-to-grouped site clears `m.composer` to nil and dispatches `loadGroups()`; `c` refused on the unmanaged group
- [ ] `m.clearSearch()` runs on drill-in and drill-out (ephemeral-on-departure list grows by 2)
- [ ] add `enter  drill into project` to the NAV/SELECT group in `helpGroupsFor` — NOT a second row in OPERATE (which already names `enter confirm the prompt`); group order unchanged; description names the header sub-state
- [ ] write tests: drill-in/out state and session bumps, action-time bind + return cleanup, search cleared, help pins both directions
- [ ] run tests - must pass before task 9

### Task 9: Batch partitioning and confirmation (phase 3)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] add `opBatch{proj compose.Project, services []string}` and `partitionSelection()` ordered by screen position; empty selection → ONE batch: cursor's group with an EMPTY services slice (= all services, compose-resolved, never-created included); unmanaged never enters a batch
- [ ] remove the task-7 temporary gate; confirmation prompt names the batches (`deploy: web (nginx, api) → db (all)? (y/n)`) and clamps to width
- [ ] `R` refuses a capture that spans groups (existing warning slot); single-group `R` path unchanged
- [ ] write tests: partitioning order, empty-selection → empty slice, unmanaged exclusion, cross-group `R` refusal, prompt clamping
- [ ] run tests - must pass before task 10

### Task 10: Sequential progress pipeline (phase 3)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] generalize `enterProgress(batches []opBatch)`: build `m.steps` for ALL batches upfront with project-prefixed DISPLAY names; add `m.batches`, `m.batchIdx`, and a `batchSession` sequence counter
- [ ] `handleStepEvent` resolves an event within the CURRENT batch's step range (`batchIdx` + per-batch offset) — never by global name scan
- [ ] `pipelineDoneMsg` gains batch index + `batchSession`; handler gated on screen + session + index; interim rule: advance directly to batch i+1 on `pipelineDoneMsg` (Task 11 intercepts with the wait phase and introduces `batchDoneMsg`)
- [ ] batch i: `composerFactory(proj)` → Deploy snapshot (Snapshotter assert, per batch) → `runner.Run` into a fresh events channel; single-batch path stays byte-equivalent to today
- [ ] batch failure marks remaining steps skipped and stops; `esc` cancels the current batch ctx and marks the rest skipped — a late `pipelineDoneMsg` from the cancelled batch must NOT advance (session gate); mid-op `q`/`ctrl+c` no-op rules unchanged
- [ ] update-cache invalidation on success deletes the cache entry of EVERY batch's key; clear `batches`/`batchIdx` at every progress departure site
- [ ] write tests: two-batch happy path with SHARED step names (pins the range lookup), failure-stops-sequence, esc mid-sequence does not advance, single-batch equivalence, departure cleanup
- [ ] run tests - must pass before task 11

### Task 11: Per-batch wait phase (phase 3)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] on batch i's `pipelineDoneMsg`, run the existing wait sub-state seeded with `NewWaitState(batch services — bare names, empty resolved via ListServices)` against batch i's composer; wait success emits `batchDoneMsg{batchIdx, batchSession}` which starts batch i+1
- [ ] `batchDoneMsg` handler gated on screen + session + index; wait failure/timeout of batch i stops the sequence (same skipped rendering); StopOnly never waits (unchanged guard)
- [ ] `runRollbackCleanup` and departure-site cleanup (`clearWaitState`, `waiting`, `waitDeadline`, batch fields) run when LEAVING `screenProgress`, per the wait-snapshots-rollback rules — never goroutine-deferred
- [ ] write tests: per-batch wait seeding, wait failure stops sequence, stale `batchDoneMsg` rejected, cleanup on esc-from-progress
- [ ] run tests - must pass before task 12

### Task 12: Updates in grouped mode (phase 4)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] gate ALL THREE auto-scan entry points on drilled mode: `maybeRefreshUpdatesCmd()`, the `statusMsg` self-heal, and `Init()`'s fast path; `NewModel` keeps `updateInFlight = m.autoUpdatesAllowed()` semantics with the grouped branch
- [ ] `U` in grouped mode scans the cursor row's group only via that group's composer, writing one cache entry under the group's `updatesCacheKey` (projDir + server, unmanaged prefix rule kept) — shared with drilled mode; `updateInFlight` guard kept
- [ ] `updatesMsg` gains `forKey` captured at DISPATCH; the cache write and `errMsg` restore key off `forKey`, never a handler-time re-derive (the cursor may have moved)
- [ ] grouped-mode hydration iterates the cache entries of ALL visible groups (raw map reads), not the single `updatesCacheLookup()`; headers render `⇧ n` aggregated from cached verdicts only
- [ ] `currentUpdateInfo()` resolves the cursor row's group key so `i` (inspect) from a grouped row reads the right entry; `redrawInspectFromCache()` gating unchanged
- [ ] write tests: `U` scopes to cursor group, `forKey` write survives a cursor move, cache replay after drill-in, header count aggregation, no auto-scan in grouped mode, inspect-from-grouped-row reads the right entry
- [ ] run tests - must pass before task 13

### Task 13: Delete screenSelectProject (phase 4)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/help_test.go`
- Modify: `internal/tui/app_test.go`
- Modify: `cmd/root.go`

- [ ] remove the screen constant (iota renumbers), its `handleKey`/`View` cases, `viewSelectProject()`, `projectsMsg`/`projectsSession`, and the `projects`/`projCursor`/`projErr`/`showPicker` Model fields (loader logic lives in `loadGroups` since Task 6)
- [ ] `ProjectLoader` wiring in `cmd/root.go` feeds the grouped loader; delete `WithUnmanagedRow`/picker-row plumbing if no caller remains
- [ ] sweep: `allScreens` literal, `screenName()`, `helpGroupsFor` (+ its doc comment), `leaveGroup()` doc comment, the `screenSelectProject` row in `TestHelpGroups_NamesEveryBoundKey`'s bound map, `TestHelpGroups_LeaveGroupMatchesFooter`, `TestCtrlCConfirmation_AllRemoteScreens`, `TestHelpOverlay_OpensFromEveryScreen`, `containerKeys` (`TestAllScreens_Complete`'s bound self-adjusts off the last constant — verify only)
- [ ] remove the now-dead project-screen esc site from the backward-navigation cleanup chain; grouped-screen esc carries its duties (already wired in Task 8)
- [ ] write/adjust tests for the final esc chain and root-screen `q` semantics
- [ ] run FULL suite `go test ./... -count=1` - must pass before task 14

### Task 14: Verify acceptance criteria
- [ ] verify all Overview requirements: grouped landing, fold, drill, sequential ops, stop-on-failure, single-group `R`, `U` per group, picker gone
- [ ] verify edge cases: duplicate service names across projects, empty group, unmanaged-only host, single-project host (degenerate render), esc mid-sequence, cursor move during a `U` scan
- [ ] verify: no automatic update scan fires in grouped mode; the update cache entry is shared between grouped and drilled mode
- [ ] run full test suite: `go test ./... -count=1`
- [ ] `go build -o cdeploy .` and `go vet ./...` clean

### Task 15: [Final] Update documentation
- [ ] create `docs/architecture/tui-multi-project.md` with the rationale and test pins (entry model, qualified keys + boundary rule, batch sequencing + message identity, grouped-mode update rules, action-time composer binding)
- [ ] update `CLAUDE.md`: TUI state machine (screen count, deleted picker), container-screen paragraphs, session-counter site lists (including the inverted `connectResultMsg` success-path rule), ephemeral-on-departure count, "Adding a New TUI Screen" counts
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention - no checkboxes, informational only*

**Manual verification:**
- run the TUI against a real multi-project host (local + one SSH server): grouped landing, fold, drill, a two-project deploy, a mid-sequence failure (stop a registry), `U` on one group
- verify a host with only unmanaged containers and a host with a single project
- narrow-terminal pass: header rows, indent, confirmation prompt clamping

**External:**
- none — TUI-only change; CLI subcommands and JSON output are untouched

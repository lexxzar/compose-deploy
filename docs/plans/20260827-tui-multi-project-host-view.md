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
- Modify: `internal/tui/entries.go` + `_test.go` (➕ the pure row-model rules — `groupsHaveHeaders`, `groupCounts`, `groupUnmanaged`, `selectableRefs` — live beside the other row-model code, not in `app.go`)
- Modify: `internal/tui/app_test.go` (➕ the fold/render/selection pins)

⚠️ Deviations:
- The indent and the header rows key off `hasGroupHeaders()` (`len(svcGroups) > 1`), NOT `m.grouped`. `rebuildSvcEntries` already emits no header for a single group, so a grouped host that happens to hold ONE project would otherwise indent rows under a header that does not exist. One predicate now feeds both the entry model and the renderer.
- The grouped idle footer pair names the END-STATE keys (`enter drill in`, `d deploy`, `r restart`), which Tasks 8-9 make live. This is the same accepted scaffolding the checkbox below grants the writable help table, kept consistent so the footer is decided once rather than rewritten in each of the next two tasks.
- `selectedCount`/`selectedContainers` moved to `selectableRefs()` alongside `allSelected()`/`a`. An unmanaged key can never enter `m.selected`, so this changes no behaviour — it keeps every selection read on one predicate.

- [x] `space` on a header toggles `folded`, calls `rebuildSvcEntries` + `fixSvcOffset`; `space` on a service selects as today; `space` on an unmanaged row is a no-op with `fixSvcOffset()` first
- [x] header renders `▼/▶ name  ● n up  ✗ n` aggregates; service rows indent 2 cells; captions pad and scroll indicators get the offset; unmanaged rows render NO checkbox cell; `a`/`allSelected()` skip unmanaged rows; title counts selectable rows only
- [x] footer: decide the grouped idle pair in `containerHelpLines()`; the line COUNT stays state-independent; every line clamped with `clampToWidth` and measured with `ansi.StringWidth`
- [x] grouped mode refuses `d`/`r`/`s`/`R`/`c` for now (⚠️ scaffolding until Task 9 — the writable help table names keys the gate refuses for two tasks; accepted, remove in Task 9) — landed in Task 6; pinned by `TestGroupedScreen_RefusesComposerBoundKeys`
- [x] name the fold binding in `helpGroupsFor` with a description distinguishing header vs service `space`; group ORDER unchanged (LEAVE stays 4th of 6)
- [x] write tests: fold/unfold rebuild, aggregate counts, refusal gates, unmanaged row rendering/selection skips, footer reservation; extend `TestHelpGroups_NamesEveryBoundKey` both directions (➕ done via `TestHelpGroups_GroupedNamesTheSameKeys`, which compares the grouped token set against the already-pinned writable one in both directions — no second hand-maintained key list to drift)
- [x] run tests - must pass before task 8

### Task 8: Drill-in, drill-out, and action-time composer (phase 2)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/help_test.go`
- Modify: `internal/tui/entries.go` + `_test.go` (➕ `cursorGroup()` — the header-accepting twin of `cursorService()` — lives beside the other row-model helpers)
- Modify: `internal/tui/app_test.go` (➕ the drill/bind pins, and the esc-to-picker fixtures Task 8 rewires)

⚠️ Deviations:
- `readOnly()` gained a `m.grouped` short-circuit. `x` binds the cursor group's composer and KEEPS it across the confirmation prompt, so an unmanaged row would otherwise flip the whole screen to the read-only variant (no checkboxes, 7-cell caption pad, a different footer pair and therefore a different `svcVisibleCount`) for the length of the prompt. This is the plan's own "grouped mode is not globally read-only" rule made load-bearing.
- `screenSelectProject` is now UNREACHABLE in production — Task 6 predicted exactly this ("Task 8 rewires that esc to drill-out"). `loadProjects`/`projectsMsg`/`viewSelectProject` stay wired and tested until Task 13's deletion sweep; the screen constant carries a comment saying so.
- Drill-in sets `m.showPicker = true` because that is the predicate `canGoBack()` already reads on the drilled screen. Task 13 removes the field, and the drilled back-rule moves with it.
- Drill-out reuses `enterGroupedContainers()` whole rather than a second cleanup body: it already owns the composer, project identity, rows, selection, search, wait state and the four session counters, which is exactly what the site owes.
- `enter` drills in from a group HEADER only, per the plan. On a grouped host with exactly ONE project no header is emitted, so there is no drill-in there — the accepted never-created-services gap in the Solution Overview covers that case.

- [x] `enter` on a header drills in: `m.svcGroups` = that one group, `m.composer = composerFactory(group.proj)`, reload via `loadServices` (full fidelity), breadcrumb `cdeploy > server > proj`; bump the three session counters
- [x] `esc` from drilled mode returns to grouped mode: `m.composer = nil`, reload groups, bump sessions, follow the callback-cleanup discipline; `esc` from grouped mode returns to the server screen
- [x] `l`/`x`/`i`/`c` in grouped mode: set `m.composer = composerFactory(cursor group.proj)` at the action key, then call the existing `enter*` helper; every return-to-grouped site clears `m.composer` to nil and dispatches `loadGroups()`; `c` refused on the unmanaged group
- [x] `m.clearSearch()` runs on drill-in and drill-out (ephemeral-on-departure list grows by 2)
- [x] add `enter  drill into project` to the NAV/SELECT group in `helpGroupsFor` — NOT a second row in OPERATE (which already names `enter confirm the prompt`); group order unchanged; description names the header sub-state
- [x] write tests: drill-in/out state and session bumps, action-time bind + return cleanup, search cleared, help pins both directions
- [x] run tests - must pass before task 9

### Task 9: Batch partitioning and confirmation (phase 3)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`
- Modify: `internal/tui/entries.go` + `_test.go` (➕ `opBatch`, `partitionSelection` and `formatBatchTargets` are pure row-model rules, so they live beside the other row-model code rather than in `app.go`)

⚠️ Deviations:
- **The empty-selection rule applies in GROUPED mode only.** `partitionSelection()` implements it unconditionally (it is a pure row-model rule and is unit-tested both ways), but `armOperation` keeps the drilled screen's long-standing `warnNoSelection` guard. Reinterpreting an unselected `d` on the single-project screen as "deploy everything" is a semantic change to a destructive key that no checkbox asks for, and `TestWarning_ShownWhenNoSelection` pins it. Task 10 does not need to revisit this.
- **A selection that spans projects is still refused** (`warnCrossProject`), because the sequential runner is Task 10. This is the same accepted scaffolding shape as Task 7's gate, narrowed from "all of `d`/`r`/`s`/`R`" to "more than one batch"; removing it is the one-line change at the top of `armOperation`. Without it, confirming a multi-project op would call `enterProgress` with one composer and silently drop the other batch.
- **The composer binds at the confirming `enter`, not at the `d`/`r`/`s` press** (unlike `x`, which must keep it across the prompt). Nothing that changes the partition can happen while the prompt is armed — every key but `enter`/`esc`/`ctrl+c` is swallowed — so the prompt recomputes rather than captures, which is what keeps `opBatch` off the Model and out of the departure-site cleanup list until Task 10 needs it there.
- **`R` binds through the new `bindProjComposer`, addressed by the batch's project rather than the cursor's** — the selection may sit in a group the cursor has since left. Its three existing early-returns gained an `unbindGroupedComposer()` so a probe that finds no `RollbackPreparer` does not leave grouped mode holding one project's composer. All three are no-ops in drilled mode, so that path stays byte-identical.
- ➕ `viewProgress`'s title read `selectedContainers()`, which is empty for a whole-group op — the new grouped path would have rendered a blank target during a real deploy. Grouped mode now reads `m.opContainers` there and falls back to `all services`; drilled mode is untouched.

- [x] add `opBatch{proj compose.Project, services []string}` and `partitionSelection()` ordered by screen position; empty selection → ONE batch: cursor's group with an EMPTY services slice (= all services, compose-resolved, never-created included); unmanaged never enters a batch
- [x] remove the task-7 temporary gate; confirmation prompt names the batches (`deploy: web (nginx, api) → db (all)? (y/n)`) and clamps to width
- [x] `R` refuses a capture that spans groups (existing warning slot); single-group `R` path unchanged
- [x] write tests: partitioning order, empty-selection → empty slice, unmanaged exclusion, cross-group `R` refusal, prompt clamping
- [x] run tests - must pass before task 10

### Task 10: Sequential progress pipeline (phase 3)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

⚠️ Deviations:
- **`batchSession` is bumped at INVALIDATION sites only — never in `enterProgress`.** It starts at 0, so a zero-valued `pipelineDoneMsg{}` still resolves against a fresh sequence, which is what keeps the existing `Update(pipelineDoneMsg{})` pins (and `TestQualifiedKeys_NeverCrossIntoRunner`) driving the handler instead of being dropped by their own gate. The two bump sites are the `esc`-cancel and `clearBatchSequence()` at the progress departure.
- **`stepState` gained `label`, not a renamed `name`.** `name` stays the RUNNER step name (what a `StepEvent` matches); `label` is what the screen draws and carries the `"web: "` prefix only when `len(batches) > 1`. A hand-built `stepState{name: …}` therefore still renders and matches exactly as before.
- **`markBatchesSkipped(from)` starts at batch `from`, so a single-batch failure skips NOTHING** — the failed batch's own unreached steps stay `○`, which is byte-identical to today's rendering. Only the batches behind the failure are marked, because they are the rows a reader would otherwise take for "still to come".
- **The `--wait` seed does NOT fall back to `m.services` in grouped mode.** `m.services` is host-wide there, so a whole-project batch (empty target set) would seed the wait with other projects' services against one project's composer — a guaranteed timeout. The interim skips the wait for that case; Task 11's per-batch phase resolves it via `ListServices`.
- **`startBatch` does not rebind the composer for `Rollback`.** `R` binds it at press time (the snapshot fetch already needed one) and the prep validated the captured targets against it; rebinding from the batch's project would risk swapping it under a capture the selection has since drifted from.
- ➕ `warnCrossProject` and `TestGroupedScreen_RefusesCrossProjectSelection` are DELETED — Task 9 named this task as the one that removes them. The replacement pin (`TestGroupedScreen_ArmsCrossProjectSelection`) asserts the same keys now arm a two-batch prompt.
- ➕ `updatesCacheKey()` gained the sibling `projUpdatesCacheKey(proj)`, since a sequence invalidates projects the container screen has since left.
- ⚠️ Pre-existing, NOT introduced here: `go test -race ./internal/tui/` reports a data race in `TestQualifiedKeys_NeverCrossIntoRunner` (the test polls `recordingComposer.gotContainers` while the pipeline goroutine writes it). It reproduces on the parent commit. The project's documented command is `go test ./...`, which is clean.

- [x] generalize `enterProgress(batches []opBatch)`: build `m.steps` for ALL batches upfront with project-prefixed DISPLAY names; add `m.batches`, `m.batchIdx`, and a `batchSession` sequence counter
- [x] `handleStepEvent` resolves an event within the CURRENT batch's step range (`batchIdx` + per-batch offset) — never by global name scan
- [x] `pipelineDoneMsg` gains batch index + `batchSession`; handler gated on screen + session + index; interim rule: advance directly to batch i+1 on `pipelineDoneMsg` (Task 11 intercepts with the wait phase and introduces `batchDoneMsg`)
- [x] batch i: `composerFactory(proj)` → Deploy snapshot (Snapshotter assert, per batch) → `runner.Run` into a fresh events channel; single-batch path stays byte-equivalent to today
- [x] batch failure marks remaining steps skipped and stops; `esc` cancels the current batch ctx and marks the rest skipped — a late `pipelineDoneMsg` from the cancelled batch must NOT advance (session gate); mid-op `q`/`ctrl+c` no-op rules unchanged
- [x] update-cache invalidation on success deletes the cache entry of EVERY batch's key; clear `batches`/`batchIdx` at every progress departure site
- [x] write tests: two-batch happy path with SHARED step names (pins the range lookup), failure-stops-sequence, esc mid-sequence does not advance, single-batch equivalence, departure cleanup
- [x] run tests - must pass before task 11

### Task 11: Per-batch wait phase (phase 3)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`
- Modify: `internal/tui/help.go` (➕ one doc comment only — `progressPhase`'s "waiting implies done" rationale stopped being true, see the deviations)

⚠️ Deviations:
- **A failing gate stops the sequence only when a batch is still to come.** `waitPhaseResolved()` sets `m.failed` + `markBatchesSkipped` for a mid-sequence batch, but the LAST batch takes neither branch — today's single-project screen renders a failed wait as red verdicts plus the rollback hint over a `done` operation, not as a failed op, and `TestBatchWait_LastBatchFailureKeepsDoneState` pins that it still does.
- **`esc` on a mid-sequence gate RELEASES the next batch.** The three-phase esc contract already says the gate's `esc` means "skip the wait" while the pipeline's `esc` means "cancel" — so a skip must not strand the sequence with no terminal state and no way out. `batchFinished()` is the shared release path for every no-gate case (`StopOnly`, an unresolvable target set, the skip).
- **An unresolvable target set skips the gate rather than failing it.** A `ListServices` error names no health verdict, so it is not a health failure; the gate closes and the sequence continues. In the TUI the wait is advisory (esc skips it by hand for the same reason).
- **`waitTargetsMsg` is a second message, not a widened `waitStatusMsg`.** The reducer needs the names seeded BEFORE the first poll is evaluated, and `m.waitState` stays empty (header, no rows) for the one frame in between.
- **`batchSession` still bumps at invalidation sites only**, so a zero-valued `batchDoneMsg{}` resolves against a fresh sequence exactly as `pipelineDoneMsg{}` does — same rule Task 10 set.
- ➕ `drainBatch` split into `drainPipeline` + `resolveWaitPhase`: a batch's pipeline closing its channel no longer ends the batch, so the existing Task-10 sequence tests drive the gate too.

- [x] on batch i's `pipelineDoneMsg`, run the existing wait sub-state seeded with `NewWaitState(batch services — bare names, empty resolved via ListServices)` against batch i's composer; wait success emits `batchDoneMsg{batchIdx, batchSession}` which starts batch i+1
- [x] `batchDoneMsg` handler gated on screen + session + index; wait failure/timeout of batch i stops the sequence (same skipped rendering); StopOnly never waits (unchanged guard)
- [x] `runRollbackCleanup` and departure-site cleanup (`clearWaitState`, `waiting`, `waitDeadline`, batch fields) run when LEAVING `screenProgress`, per the wait-snapshots-rollback rules — never goroutine-deferred
- [x] write tests: per-batch wait seeding, wait failure stops sequence, stale `batchDoneMsg` rejected, cleanup on esc-from-progress
- [x] run tests - must pass before task 12

### Task 12: Updates in grouped mode (phase 4)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`
- Modify: `internal/tui/entries.go` + `_test.go` (➕ `groupCounts` gained the header's `⇧ n` total; it is a pure row-model rule, so it lives beside the other row-model code)

⚠️ Deviations:
- **The three auto-scan gates were already in place** — Task 6 landed them (its own deviation note says so), so this task pinned them instead of writing them: `TestGroupedMode_AllAutoScanEntryPointsRefuse` covers `maybeRefreshUpdatesCmd` + the `statusMsg` self-heal + `NewModel`'s seed, and `TestGroupedInit_FiresNoUpdateScan` covers `Init()`'s fast path.
- **Grouped mode fetches NO detail rows**, and the gate is the MODE, not the nil composer. `updateDetailsCmd` reads `m.composer`, which an armed `x` prompt may have left bound to a DIFFERENT project than the scan covered — a batch built from it would resolve the wrong project's images. The entry keeps `details == nil`, which is exactly the state `refillUpdateDetailsCmd` fills on drill-in, so the rows are deferred rather than lost.
- **`reapplyUpdateVerdicts` replaces the two clear-then-hydrate blocks in the `updatesMsg` handler.** A grouped scan covers ONE group, so the drilled rule ("the message is the whole truth") would blank every group the scan did not cover — including on the error path, where the contract binds only the failed project.
- **`groupCounts` gained a third return rather than a second helper**: the header's `⇧ n` reads the same tri-state the row glyph draws, so a nil verdict counts as zero and nothing on the header path can trigger a scan.
- **`inspectUpdateKey()` is read at render time, not captured on entry.** The cursor cannot move while the inspect screen is up, so it is equivalent to a captured field — without adding one more Model field to the departure-site cleanup list.

- [x] gate ALL THREE auto-scan entry points on drilled mode: `maybeRefreshUpdatesCmd()`, the `statusMsg` self-heal, and `Init()`'s fast path; `NewModel` keeps `updateInFlight = m.autoUpdatesAllowed()` semantics with the grouped branch
- [x] `U` in grouped mode scans the cursor row's group only via that group's composer, writing one cache entry under the group's `updatesCacheKey` (projDir + server, unmanaged prefix rule kept) — shared with drilled mode; `updateInFlight` guard kept
- [x] `updatesMsg` gains `forKey` captured at DISPATCH; the cache write and `errMsg` restore key off `forKey`, never a handler-time re-derive (the cursor may have moved)
- [x] grouped-mode hydration iterates the cache entries of ALL visible groups (raw map reads), not the single `updatesCacheLookup()`; headers render `⇧ n` aggregated from cached verdicts only
- [x] `currentUpdateInfo()` resolves the cursor row's group key so `i` (inspect) from a grouped row reads the right entry; `redrawInspectFromCache()` gating unchanged
- [x] write tests: `U` scopes to cursor group, `forKey` write survives a cursor move, cache replay after drill-in, header count aggregation, no auto-scan in grouped mode, inspect-from-grouped-row reads the right entry
- [x] run tests - must pass before task 13

### Task 13: Delete screenSelectProject (phase 4)

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/help_test.go`
- Modify: `internal/tui/app_test.go`
- Modify: `internal/tui/footer_reservation_test.go` (➕ the `showPicker` → `drilledFromHost` rename)
- Modify: `internal/compose/compose.go` + `unmanagedrow.go` + `unmanagedrow_test.go` (➕ the dead `Project.Desc` field)
- Modify: `cmd/root_test.go` (➕ `cmd/root.go` needed NO change — see the deviations)

⚠️ Deviations:
- **`showPicker` was RENAMED to `drilledFromHost`, not deleted.** It is the drilled screen's `canGoBack()` rule, and no predicate derivable from the composer, the factory or the group list can separate the two drilled shapes: one reached by drill-in (parent = the grouped host view) and one launched standalone in a directory holding a compose file (a ROOT, where `q` quits and `esc` does nothing). Deriving it from `len(servers) > 0 || config != nil` breaks the drill-in on a standalone host; making the root non-root turns `q` from "quit" into "drill out" on the primary local flow. The field is now named for what it actually decides and carries that rationale on it.
- **`WithUnmanagedRow` STAYS** — `projectsWithUnmanaged` still calls it, because the grouped loader gets the synthetic `(unmanaged)` row and its ORDER from the `ProjectLoader`. `cmd/root.go` therefore needed NO change; only `cmd/root_test.go` moved.
- **`compose.Project.Desc` was deleted** instead: `viewSelectProject` was its only reader, so it became write-only picker-row plumbing. `CountUnmanaged` is still needed (the row is appended only when the count is non-zero).
- **`m.projectsSession` was deleted with `projectsMsg`.** The counter guarded exactly one message type, and the loader it tracked now feeds `loadGroups`, which is already gated on `statusSession` at the same four swap sites. `TestBackToServerScreen_RestoresLocalCallbacks` moved its assertion to `statusSession`.
- The `viewSelectContainers` error slot read `showPicker` for its `q back`/`q quit` label; it now reads `canGoBack()`, the shared predicate the footer and the `?` overlay's LEAVE group already use — grouped mode with servers configured previously mislabelled that slot `q quit`.
- ⚠️ Pre-existing, NOT introduced here: `gofmt -l .` reports `cmd/list_test.go` (comment alignment). It is untouched by this task.

- [x] remove the screen constant (iota renumbers), its `handleKey`/`View` cases, `viewSelectProject()`, `projectsMsg`/`projectsSession`, and the `projects`/`projCursor`/`projErr`/`showPicker` Model fields (loader logic lives in `loadGroups` since Task 6)
- [x] `ProjectLoader` wiring in `cmd/root.go` feeds the grouped loader; delete `WithUnmanagedRow`/picker-row plumbing if no caller remains
- [x] sweep: `allScreens` literal, `screenName()`, `helpGroupsFor` (+ its doc comment), `leaveGroup()` doc comment, the `screenSelectProject` row in `TestHelpGroups_NamesEveryBoundKey`'s bound map, `TestHelpGroups_LeaveGroupMatchesFooter`, `TestCtrlCConfirmation_AllRemoteScreens`, `TestHelpOverlay_OpensFromEveryScreen`, `containerKeys` (`TestAllScreens_Complete`'s bound self-adjusts off the last constant — verify only)
- [x] remove the now-dead project-screen esc site from the backward-navigation cleanup chain; grouped-screen esc carries its duties (already wired in Task 8)
- [x] write/adjust tests for the final esc chain and root-screen `q` semantics (`TestEscChain_DrilledToGroupedToServer`, `TestRootScreenQ_ContainerModes`)
- [x] run FULL suite `go test ./... -count=1` - must pass before task 14

### Task 14: Verify acceptance criteria

**Files:**
- Modify: `internal/tui/app_test.go` (➕ `TestGroupedScreen_UnmanagedOnlyHost` — the one edge case on the list with no end-to-end pin)

⚠️ Deviations / findings:
- ➕ The unmanaged-only host was covered only by `TestAllSelected_UnmanagedOnlyHostIsFalse` (one predicate). It is now pinned end to end: a lone unmanaged group emits no header, draws no checkbox on any row, `a` selects nothing, and `d`/`r`/`s`/`R`/`c` all refuse without navigating.
- ⚠️ **Breadcrumb wording differs from Technical Details, deliberately kept.** The plan wrote `cdeploy > server > host` for grouped and `cdeploy > server > proj` for drilled. `breadcrumb()` appends `projName` only when drilled and `viewSelectContainers` adds a fixed `> services` tail, so the two modes ARE distinguishable (`cdeploy > prod > services` vs `cdeploy > prod > shop > services`) — but the grouped tail reads `services`, not `host`. Changing the word now would churn title assertions across the suite for no behavioural gain. `TestGroupedScreen_EnterDrillsIntoProject` pins the drilled half.
- ⚠️ Pre-existing, NOT introduced here: `go test -race ./internal/tui/` still reports the Task-10 data race in `TestQualifiedKeys_NeverCrossIntoRunner`. The project's documented command, `go test ./...`, is clean.

- [x] verify all Overview requirements: grouped landing, fold, drill, sequential ops, stop-on-failure, single-group `R`, `U` per group, picker gone
  - grouped landing: `TestNewModel_LandsOnGroupedWhenNoComposer`, `TestInit_LoadsGroupsWhenNoComposer`, `TestConnectSuccess_BumpsTheThreeCountersAndLandsGrouped`
  - fold: `TestGroupedSpace_OnHeaderFoldsAndUnfolds`, `TestGroupedSpace_OnUnmanagedHeaderFolds`
  - drill: `TestGroupedScreen_EnterDrillsIntoProject`, `TestGroupedScreen_DrillRoundTrip`, `TestEscChain_DrilledToGroupedToServer`
  - sequential ops + stop-on-failure: `TestBatchSequence_TwoBatchesRunInOrder`, `TestBatchSequence_FailureStopsSequence`, `TestBatchWait_FailureStopsSequence`; verified in `handleStepEvent` (per-batch step range, never a global name scan) and `markBatchesSkipped`
  - single-group `R`: `TestGroupedScreen_RollbackRefusesCrossProject`, `TestGroupedScreen_RollbackNonPreparerUnbinds`
  - `U` per group: `TestGroupedU_ScansCursorGroupOnly`; the `U` handler binds, reads and unbinds the cursor group's composer in one keystroke
  - picker gone: `grep -rn "screenSelectProject\|viewSelectProject\|projectsMsg\|projectsSession\|projCursor\|showPicker" --include="*.go" .` returns nothing; the screen iota is 8 constants
- [x] verify edge cases: duplicate service names across projects, empty group, unmanaged-only host, single-project host (degenerate render), esc mid-sequence, cursor move during a `U` scan
  - duplicate names: `TestQualifiedKeys_DuplicateServiceNamesStayDistinct`
  - empty group: `TestBuildSvcGroups_EmptyProjectKeepsItsGroup`, `TestGroupHeaderLine_EmptyGroupIsBare`
  - unmanaged-only host: ➕ `TestGroupedScreen_UnmanagedOnlyHost` (added here), `TestAllSelected_UnmanagedOnlyHostIsFalse`
  - single-project host: `TestViewSelectContainers_GroupedSingleProjectHasNoIndent`, `TestViewSelectContainers_SingleGroupHasNoHeaderOrIndent`
  - esc mid-sequence: `TestBatchSequence_EscDoesNotAdvance`, `TestBatchWait_EscSkipReleasesTheNextBatch`, `TestBatchWait_DepartureRunsCleanupAndClearsWait`
  - cursor move during a `U` scan: `TestGroupedU_ForKeySurvivesCursorMove`
- [x] verify: no automatic update scan fires in grouped mode; the update cache entry is shared between grouped and drilled mode
  - `autoUpdatesAllowed()` = `!readOnly() && !grouped`; `maybeRefreshUpdatesCmd` early-returns on `m.grouped`. Pinned by `TestGroupedMode_AllAutoScanEntryPointsRefuse`, `TestGroupedInit_FiresNoUpdateScan`, `TestGroupedMode_NoAutomaticUpdateScan`, `TestGroupedU_FetchesNoDetailBatch`
  - shared cache: `projUpdatesCacheKey(proj)` composes `proj.ConfigDir + "|" + serverName` — the same string `updatesCacheKey()` produces once drilled into that project. `TestGroupedUpdates_DrillInReplaysCachedEntry`, `TestGroupedU_UnmanagedGroupKeepsCachePrefix`
- [x] run full test suite: `go test ./... -count=1`
- [x] `go build -o cdeploy .` and `go vet ./...` clean (`gofmt -l internal/tui/` clean too)

### Task 15: [Final] Update documentation

⚠️ Deviations:
- ➕ **`README.md` was updated too**, though no checkbox names it. The task is "Update documentation" and the README's TUI section still listed seven screens with "Project select" as #2 and described reaching the `(unmanaged)` row through a picker that Task 13 deleted — a user-facing doc describing a screen that no longer exists. The edit is scoped to the screen list, the progress-screen paragraph (multi-project sequencing), and the "Unmanaged containers" narrative (group instead of picker row, the host view's self-refresh replacing the old snapshot caveat, `U` scoping).
- ➕ **`docs/architecture/unmanaged-containers.md` got a supersession note** on its picker-row section plus two factual patches (`autoUpdatesAllowed()` now `!readOnly() && !grouped`; the view is reached through the host view, not the picker). The section is otherwise still accurate and was left verbatim rather than rewritten — `WithUnmanagedRow`, the `ComposerFactory` widening and the cache-key collision rules all survive unchanged.
- The plan asked for five `Multi-project host view — …` digest paragraphs in `CLAUDE.md`; SIX landed. The per-batch wait gate went into the existing **TUI wait phase & `R` rollback** paragraph instead of a seventh, since that is where the reader of `wait-snapshots-rollback.md` looks.
- [x] create `docs/architecture/tui-multi-project.md` with the rationale and test pins (entry model, qualified keys + boundary rule, batch sequencing + message identity, grouped-mode update rules, action-time composer binding)
- [x] update `CLAUDE.md`: TUI state machine (screen count, deleted picker), container-screen paragraphs, session-counter site lists (including the inverted `connectResultMsg` success-path rule), ephemeral-on-departure count, "Adding a New TUI Screen" counts
  - ➕ the `?`-overlay paragraph states `progressPhase()` resolves `waiting` first "since it implies `done`" — Task 11 made a mid-sequence gate open with `done` FALSE, so the ordering rule stays but its reason must be restated (a gate can be open with `done` set, with `failed` set, or with neither)
  - ➕ the wait-phase paragraph must say the gate is PER BATCH: `pipelineDoneMsg` opens it, `batchDoneMsg` (not `pipelineDoneMsg`) releases the next batch, a whole-project batch resolves its targets through `waitTargetsMsg`/`ListServices`, and a failing gate stops the sequence only when a batch is still to come
- [x] moved by the orchestrator at completion

## Post-Completion
*Items requiring manual intervention - no checkboxes, informational only*

**Manual verification:**
- run the TUI against a real multi-project host (local + one SSH server): grouped landing, fold, drill, a two-project deploy, a mid-sequence failure (stop a registry), `U` on one group
- verify a host with only unmanaged containers and a host with a single project
- narrow-terminal pass: header rows, indent, confirmation prompt clamping

**External:**
- none — TUI-only change; CLI subcommands and JSON output are untouched

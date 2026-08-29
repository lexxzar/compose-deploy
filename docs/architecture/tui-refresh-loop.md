# The container screen's scroll window, the 5-second refresh loop, and session filters

> Extracted verbatim from `CLAUDE.md`, which is auto-loaded and has a size limit.
> `CLAUDE.md` carries the rule digest; this file carries the full rationale, the
> failure modes it prevents, and the tests that pin them. Read it before you touch `svcVisibleCount`, `fixSvcOffset`, `refreshTickMsg`, `refreshInFlight` or any session counter in `internal/tui/app.go`.

Two threads run through this cluster, and every section below sits on one of them.

**The viewport thread**: `m.height` → `svcVisibleCount()`, the ROW budget → `svcOffset`, the WINDOW, and `svcCursor`, the CURSOR → the `[start, end)` slice of `m.svcEntries` the renderer draws. The budget is not a constant and never was: a soft warning spends a footer row, a captions row spends a header row, and both appear without a keystroke. Every consumer therefore reads it at the moment it acts, and the two positions move through their own helpers — `fixSvcOffset` never moves the cursor, `clampSvcCursor` never moves the window.

**The staleness thread**: one `tea.Tick` → `refreshTickMsg` → the mode-aware fetch fan-out → `refreshInFlight` for as long as the fetch is out → a `session uint64` stamped at DISPATCH → the arrival handler's screen-and-session gate. Every container-screen fetch rides it, and every context change invalidates the whole set through one helper. Break the thread at the guard and the 5-second refresh dies silently for the rest of the session; break it at a counter and one project's data lands in another project's view.

## The row budget: `svcVisibleCount()`

The container select screen renders a WINDOW into `m.svcEntries`, not the whole list. `svcOffset` is the index of the first visible ROW — a row, not a service, so a group header consumes one exactly like a service does (`TestFixSvcOffset_CountsHeaderRows`).

`svcVisibleCount()` is `m.height - headerLines - footerLines`, and it applies **two clamps in a fixed order**:

1. `if visible < 1 { visible = 1 }` — a terminal too short for the chrome still shows one row rather than none.
2. `if visible > len(m.svcEntries) { visible = len(m.svcEntries) }` — the budget never exceeds the rows that exist.

The order matters and is not interchangeable. The floor applies to the height arithmetic; the cap applies to the result. On an EMPTY list the function therefore answers **0, not 1** — the degenerate case `TestPageKeys_EmptyListStaysInRange` pins, where the page step moves nothing and `clampSvcCursor` holds the cursor at 0 instead of letting it sit at -1. Pins: `TestSvcVisibleCount_MinOne` (height 5: `5-3-3 = -1` → 1), `TestSvcVisibleCount_AllFit` (capped to the two services that exist).

**`m.height == 0` — before the first `WindowSizeMsg` — returns `len(m.svcEntries)` outright**, an early return above the arithmetic. Backward compatibility: the pre-windowing screen drew every row, and an unsized first frame must not decide to draw one (`TestSvcVisibleCount_HeightZero`, `TestFixSvcOffset_HeightZeroNoOp`).

### `headerLines` — 3, or 4 with captions

`breadcrumb (1) + titleStyle MarginBottom space-line (1) + gap-or-indicator (1) = 3`. The margin line is the part that gets forgotten: lipgloss's `MarginBottom(1)` on `titleStyle` emits a full-width space-padded line, so it is a physical row the budget must pay for.

**+1 when the column captions row is drawn.** `hasStatusColumns()` is the predicate, and it covers more than the two columns the original rule named: it returns true when any service in any group has non-empty `Created`, non-empty `Uptime` or a non-empty `Ports` slice, when a stats entry exists for a RUNNING service, or — short-circuiting all of that — when `m.statsRequested` is set. The `statsRequested` short-circuit exists so the CPU/Mem captions render from the first frame instead of popping in a call later, when the host-wide `docker stats` returns; its stats branch must keep matching the render predicate in `viewSelectContainers` (map presence AND running), or the budget and the drawn captions disagree by a row. An available update deliberately does NOT count — the `⇧` glyph renders inline beside the name, not as a column. Pins: `TestSvcVisibleCount_WithStatusColumns` (height 10, `10-4-3 = 3`), `TestSvcVisibleCount_withStatsColumns`, `TestHasStatusColumns`, `TestColumnCaptions_ShownWithStatusData`, `TestColumnCaptions_HiddenWithoutStatusData`, `TestContainerScreen_StatsCaptionsReservedBeforeData`, `TestContainerScreen_NoStatsCaptionsAbsentWithoutRequest`.

The captions use `descStyle` and are left-padded so `Service` sits exactly above the service names. The pad is `cursor(2) + checkbox(3) + space(1) + health(1) + space(1) + dot(1) + space(1) = 10` on the writable screen and **7 on the read-only one** (the checkbox is dropped but the space after it is kept), plus the grouped view's 2-cell service indent, and then `%-*s` over `maxName`. Each active column is widened to at least its own caption width first (`Service`, `Created`, `Uptime`, `CPU` right-aligned, `Mem`, `Ports`), so a caption can never overflow and shift the columns to its right. Pins: `TestColumnCaptions_Alignment`, `TestContainerScreen_serviceColumnHasCaption`, `TestContainerScreen_serviceCaptionWidensShortNames`, `TestReadOnly_CaptionAlignment`.

### `footerLines` — 3 or more, never 2

`gap-or-indicator (1) + helpStyle MarginTop space-line (1) + containerFooterLines() (1 or 2)`. `helpStyle`'s `MarginTop(1)` is the second lipgloss margin that costs a real row.

**The reserved search-bar line does NOT add a physical row.** It is written as `gap + searchBarLine()` with no trailing newline immediately before `helpStyle.Render(...)`, and `helpStyle`'s `MarginTop(1)` prepends a full-width blank — the bar content and that blank land on the SAME physical line. Counting the bar separately over-reserved one row against the pre-search baseline and left a blank line of slack at the bottom of the terminal (`TestSvcVisibleCount_GainsRowVersusOldFooter`, `internal/tui/footer_reservation_test.go`).

The count is state-INDEPENDENT across search-idle / searching / committed / confirming, for two reasons: the merged bar+margin line is present in every state (blank while confirming, with the confirm prompt taking the footer-text slot), and the footer-text slot is always `containerFooterLines()` — derived from the idle footer alone and padded out in the other states by `containerFooter()`. Reading that count in the confirming branch rather than hard-coding 3 is what keeps the confirm state the same height as the idle one at every width where the footer wraps (`TestSvcVisibleCount_ConstantAcrossConfirming`, `TestSvcVisibleCount_Confirming`). The footer's own contract — advertise only the keys the state binds while the line COUNT stays state-independent — lives in `docs/architecture/tui-help-overlay.md`.

Two conditional rows sit on top, in the non-confirming branch only:

- `m.warning != ""` adds 1 (`TestSvcVisibleCount_Warning`).
- `m.statsErr != nil || m.updatesErr != ""` adds 1. **The soft-warning slot priority is `svcErr > statsErr > updatesErr`**; `svcErr` early-returns from the renderer entirely (see below) so it never appears in this slot, and `statsErr` and `updatesErr` are mutually exclusive in the renderer too — so the footer reserves at most ONE line for whichever is active.

### Scroll indicators replace blank lines; they never add one

`▲ N more` and `▼ N more` are written INTO the blank-line content that already sits between breadcrumb and services (top) and between services and the help bar (bottom). The top gap emits either `"\n" + indicator + "\n"` or `"\n\n"`; the bottom emits the indicator plus a newline and then suppresses the gap newline it replaced. The total line count `svcVisibleCount()` budgets is therefore identical with and without them — an indicator that ADDED a row would push the last service off the pane exactly when the list is long enough to need the indicator.

In grouped mode both indicators carry the service rows' 2-cell indent (`"  " + indent + "▲ N more"`), the same `indentW` the caption pad adds, so the whole column stack stays aligned under the group headers. The indent comes from `groupsHaveHeaders(m.svcGroups)` — not from `m.grouped` — so a grouped host holding exactly one project draws neither headers nor indent (`TestViewSelectContainers_GroupedScrollIndicatorsIndent`, `TestViewSelectContainers_WindowedOnlyShowsVisibleServices`).

## `fixSvcOffset` moves the WINDOW; `clampSvcCursor` moves the CURSOR

They are two helpers on purpose, and neither can do the other's job.

`fixSvcOffset()` pulls the WINDOW so the cursor is inside it: scroll down if `svcCursor >= svcOffset+visible`, scroll up if `svcCursor < svcOffset`, then clamp `svcOffset` to `len(svcEntries) - visible` (floored at 0). It never touches `svcCursor`.

`clampSvcCursor()` pulls the CURSOR back inside `svcEntries`: down to `len-1`, up to 0. It exists because the grouped reload is also the periodic refresh, so the row count can shrink under a cursor that was valid a moment ago, and `fixSvcOffset` cannot fix that (`TestGroupedServicesMsg_ShrinkClampsCursor`).

**`fixSvcOffset()` runs after any state change that affects the visible count**: cursor movement, window resize, entering or leaving the confirming state, a warning appearing or clearing. There are ~69 call sites and the density is deliberate — **every gated container key calls it before its early return**, because the dispatch clears `m.warning` above the switch, which frees a footer row and grows the budget by one; a scrolled list would otherwise keep a too-large `svcOffset` and render a blank row at the bottom of the pane. Pins: `TestFixSvcOffset_CursorBelowWindow`, `TestFixSvcOffset_CursorAboveWindow`, `TestFixSvcOffset_AllItemsFit`, `TestFixSvcOffset_ClampsMaxOffset`, `TestScrollDown_PastVisibleWindow`, `TestScrollUp_PastTopOfWindow`, `TestWindowResize_FixesOffset`, `TestConfirming_CallsFixSvcOffset`, `TestSelectAll_DoesNotChangeOffset`, `TestReadOnly_GatedKeyReclampsOffset`, `TestEnterExec_RefusalReclampsOffset`, `TestContainerKeys_UnsupportedCapabilityReclampsOffset`, `TestGroupedFold_ReclampsTheScrollWindow`, `TestGroupedSelectAll_FoldedRefusalFixesTheOffset`, `TestUpdatesMsg_OffScreenErrorFixesOffset`.

The search jump and the drill-out cursor target both end in `fixSvcOffset()` for the same reason — a match or a project below the fold of a tall host has to be scrolled into view (`TestSearchLiveJumpScrollsOffScreenMatchIntoView`, `TestSearchCycleScrollsOffScreenMatchIntoView`, `TestGroupedDrillOut_ScrollsTheVisitedProjectIntoView`).

## `pgup`/`pgdown` overshoot by design

**`svcVisibleCount()` IS the page step**, read at press time:

```go
page := m.svcVisibleCount()
if key == "pgup" { m.svcCursor -= page } else { m.svcCursor += page }
m.clampSvcCursor()
m.fixSvcOffset()
```

Three rules are packed into those four lines.

**The step is the WHOLE window, so the keys overshoot — that is the feature.** A `pgdown` from the TOP of the window lands one row PAST its bottom edge, `fixSvcOffset()` then scrolls the window by one, and the net effect is that a page lands on the row AFTER the one the user just finished reading. A step of `visible - 1` would re-show a row every press.

**`clampSvcCursor()` must come BEFORE `fixSvcOffset()`.** The cursor is the thing that just went out of range; `fixSvcOffset` moves only the window and would happily chase a cursor sitting at index 12 on a 10-row list.

**The step must be READ at press time, never cached or hard-coded.** The budget changes without a keystroke: a failed stats fetch spends a footer row, an arriving status refresh spends a header row on captions. `TestPageKeys_PageSizeFollowsTheVisibleCount` pins exactly this with three fixtures (idle → 3 rows, `statsErr` set → 2, `Uptime` present → 2) and asserts the cursor travelled that many rows.

**It degenerates to end/home whenever the list fits the pane.** The `visible > len(svcEntries)` cap makes a page the whole list, so one press goes to the last row and one to the first. Both shapes that produce it are covered by `TestPageKeys_PageIsTheWholeListWhenItFits`: a list shorter than the pane, and the unsized `m.height == 0` first frame where `svcVisibleCount()` returns the whole row count outright. In both, `svcOffset` must stay 0 — there is no window to scroll.

Other pins: `TestPageKeys_MoveAFullPageAndClamp` (cursor 0→3→6→9→9, `svcOffset` 7 at the bottom, 0 at the top), `TestPageKeys_InertWhereTheOtherMovementKeysAre`, `TestPageKeys_WorkOnTheReadOnlyScreen` (paging writes nothing to docker, so it is NOT gated on `readOnly()`).

## Where `svcOffset` is reset, and the one branch that must never reset it

`svcOffset` is reset to 0 alongside `svcCursor`:

- in the **DRILLED** `servicesMsg` branch — a full reload replaces the list wholesale;
- at every landing and drill site, through `clearContainerScreen()` (`enterGroupedContainers`, the `entryLocal` fast track, the `connectResultMsg` failure path) or by hand (`drillIntoGroup`, the one site with no `clearContainerScreen()` behind it).

**Never in the grouped `servicesMsg` branch.** That branch is BOTH the initial load and the 5-second reload, so resetting there would fight the user every tick: cursor, selection, fold state and an active search all have to survive it. It calls `restoreCursorRow(cursorRowID())` followed by `fixSvcOffset()` instead — capture the cursor's IDENTITY before `setGroups` renumbers every row, look it up after, and let `restoreCursorRow`'s trailing `clampSvcCursor()` handle a row count that shrank. `fixSvcOffset` alone is not enough there, because it moves only the WINDOW.

**The cursor survives by IDENTITY, not by index** — `svcRowID{proj, service, header}`, the same rule the fold state (project name) and the selection (`svcKey`) already follow. The full rationale for that, the drill-out cursor target and the one-shot lifecycle around it live in `docs/architecture/tui-multi-project.md`; do not restate them here.

## One tick, always rescheduled

`statsRefreshInterval = 5 * time.Second` (`internal/tui/app.go`) drives `refreshTickMsg`. `Init()` starts the tick on EVERY screen — including the server picker — because the handler gates on screen anyway, and starting it once from `Init` means no screen transition has to remember to kick it off. **The handler always reschedules another tick, on every path**, so the loop is a singleton: exactly one tick is pending at any moment, no matter how many times the user enters and leaves the container screen (`TestInit_ServerScreen_StartsTickOnly`).

The fetch gate is:

```go
live := m.composer != nil
if m.grouped { live = m.composerFactory != nil }
if m.screen != screenSelectContainers || m.confirming || !live || m.refreshInFlight {
    return m, m.refreshTick()   // pure reschedule, no docker calls
}
m.refreshInFlight = true
return m, tea.Batch(m.statusRefreshCmd(), m.statsRefreshCmd(), m.refreshTick())
```

Four ways to be a no-op, and each has a reason: off-screen (nothing to repaint), mid-confirmation (a reload would move the rows a prompt already named), mid-disconnect or pre-connect (no composer / no factory), and previous fetch still pending (see `refreshInFlight`).

**The liveness check is `m.composerFactory != nil` in GROUPED mode, not `m.composer != nil`.** Grouped mode holds no composer by design — that nil is what keeps every capability assertion inert until an action key binds one — so a flat composer test would silence the periodic refresh on exactly the screen that most needs it, the live host view. What grouped mode needs instead is a factory to reach the host-wide seam through. Pins: `TestRefreshTickMsg_fetchesOnContainerScreen`, `TestRefreshTickMsg_skipsOffScreen`, `TestRefreshTickMsg_skipsDuringConfirm`, `TestRefreshTickMsg_skipsWhenComposerNil`, `TestRefreshTickMsg_skipsWhenInFlight`, `TestRefreshTick_GroupedDispatchesHostWideFetches`, `TestRefreshTick_GroupedWithoutFactoryIsRescheduleOnly`, `TestRefreshTick_DrilledStillUsesTheComposer`.

### The mode-aware fan-out

`statusRefreshCmd()` and `statsRefreshCmd()` are the pair every container-screen ENTRY and the tick dispatch. They hide the mode from their callers:

| | `statusRefreshCmd()` | `statsRefreshCmd()` |
| --- | --- | --- |
| drilled | `refreshStatus()` (per-project `ContainerStatus`) | `refreshStats()` (per-project `ContainerStats`) |
| grouped | `loadGroups()` (project list + host-wide `GroupHostStatus`) | **nil** |

`tea.Batch` drops a nil, so no call site needs to know which mode it is in. **Grouped mode contributes no stats command at all** because the stats half consumes the container listing the status half returns: `groupedStatsCmd` is CHAINED off the grouped `servicesMsg` arrival, taking both the seam and the listing from THAT message rather than resolving a second `m.hostGrouper()`. The rationale for the chain — one `docker ps` per refresh, rows painted before the slow `docker stats --no-stream` returns — is in `docs/architecture/tui-multi-project.md`.

Screen ENTRIES go through `containerFetchBatch(initial bool)`, which adds the update refresh when the cache says so and picks `loadServices` over `refreshStatus` for a drilled entry that must also DISCOVER the service list (or settle a `svcReloadPending` debt). Its five call sites are `execDoneMsg`, the `entryLocal` fast track, `esc` from logs, `esc` from progress, and `drillIntoGroup`. `enterGroupedContainers` returns `loadGroups()` directly — equivalent, since grouped mode contributes no stats command and never auto-scans for updates.

## `refreshInFlight` prevents fetch stacking

The tick fan-out schedules the NEXT 5s tick *concurrently* with the current refresh — **Bubble Tea batches execute concurrently, not sequentially**. Without a guard, if `docker stats --no-stream` takes longer than 5s on a slow remote SSH hop, the next tick stacks another pair of fetches on top, and then another, until the connection is saturated with duplicate work.

**Set true** at every site that dispatches a container-screen fetch: the tick handler; the five transition sites that fan out through `containerFetchBatch` (`execDoneMsg`, `entryLocal` fast track, `esc` from logs, `esc` from progress, `drillIntoGroup`); `enterGroupedContainers`, which dispatches `loadGroups()` directly; both non-server branches of `NewModel`, because `Init()` will fire (`loadServices` + `statsRefreshCmd` on the local fast track, `loadGroups` on the grouped landing); and the grouped `servicesMsg` branch that CHAINS the stats half.

**Cleared** in the `statsMsg` handler on any CURRENT-SESSION arrival, BEFORE the screen check, on success and on error alike. A stale-session `statsMsg` returns above the clear and does not touch the flag at all.

**The grouped `servicesMsg` branch clears it the same way.** The clear is the first statement of the handler, gated only on `msg.groupedPayload && msg.session == m.statusSession`, and therefore runs before the screen check, before the `m.confirming` drop and before the `msg.err` branch. Then the ONE branch that accepts the payload and chains the stats half RE-ARMS the flag beside the `tea.Cmd` whose `statsMsg` clears it again. That is the only site in the codebase that sets `refreshInFlight` outside a fetch dispatch, and it is safe for exactly two reasons: it is inseparable from the Cmd that clears it, and that Cmd runs under `groupedStatsTimeout` so the clearing message cannot fail to arrive.

**Never write `if !chaining { clear }`.** That form skips the clear on the stale-session, off-screen, `m.confirming` and `msg.err` paths — which is the precise latch this whole rule exists to prevent. A single `docker compose ls` hiccup would freeze the host view on its error line with no self-heal path, because the guard that refuses the next tick would never come down again. `TestGroupedArrivals_ClearRefreshInFlight` is the table that pins it, with a case per path: no loader, loader failure, host-ps failure, no grouper behind the factory, dropped under an armed prompt, arrives off-screen, and success-chains-the-stats-half (the only case where the flag stays set, and the subtest then delivers the chained message and asserts it comes back down).

**Clearing has to happen before the screen check** because if the user opens the config / logs / progress screen while a fetch is pending, the response arrives off-screen and would otherwise be discarded silently — leaving `refreshInFlight` stuck at true and permanently silencing the periodic loop when the user returns to the container screen (`TestStatsMsg_offScreenClearsRefreshInFlight`).

**The session-match condition is what makes the unconditional clear safe.** A stale response means a context change already happened, and the context-change handler already reset the flag; a stale arrival that cleared it would let the tick stack a second fetch on top of the live one (`TestStatsMsg_staleSessionLeavesRefreshInFlight`, `TestGroupedArrival_StaleSessionLeavesTheGuardAlone`).

**`backToServerScreen()` resets the flag to false explicitly.** It is the one transition that navigates AWAY without firing a fetch batch, so the new context has to start clean rather than inherit a guard with no fetch behind it.

When the next tick fires while the guard is set, it reschedules without fetching — pinned at the consequence level by `TestGroupedChainWindow_TickMakesNoSecondFetch`, which keeps the tick firing during the window between the two grouped messages and asserts no second host-wide `docker ps` goes on the wire. Other pins: `TestStatsMsg_clearsRefreshInFlight`, `TestStatsMsg_errorClearsRefreshInFlight`, `TestGroupedFetchError_ClearsRefreshInFlightAndKeepsTicking`.

## Session filters for stale responses

`statsMsg`, `statusMsg` AND `servicesMsg` all carry a `session uint64` captured at fetch DISPATCH time; their handlers reject any message whose session — and screen — do not match. Two counters carry the container screen's own fetches.

**`statsSession`** covers `refreshStats` AND the chained grouped `groupedStatsCmd`; the two are the same fetch in the two modes, so they share one counter. `statsMsg` stamps it at fetch time and the handler rejects `msg.session != m.statsSession` in addition to the screen check, mirroring the `logsSession` / `configSession` pattern. Without it, a periodic-tick fetch from project A could land after the user has navigated to project B and silently overwrite the new map. Pins: `TestStatsMsg_staleSessionIgnored`, `TestStatsMsg_currentSessionAccepted`, `TestRefreshStats_capturesCurrentSession`, `TestGroupedStatsChain_RidesStatsSession`.

**`statusSession`** covers `refreshStatus`, `loadServices` AND `loadGroups`. `loadServices` and `loadGroups` are the initial-load siblings of `refreshStatus` — they fetch the list and the initial status in one shot — so they share the lifecycle and the staleness rule rather than owning counters of their own. `loadGroups` in particular is `loadServices`' grouped counterpart: same context changes, same rule. A stale `servicesMsg` overwrites BOTH the row list and `svcStatus`, so the filter matters more there than anywhere else. Pins: `TestStatusMsg_currentSessionAccepted`, `TestStatusMsg_staleSessionIgnored`, `TestRefreshStatus_capturesCurrentSession`, `TestLoadServices_capturesCurrentSession`, `TestServicesMsg_currentSessionAccepted`, `TestServicesMsg_staleSessionIgnored`, `TestGroupedServicesMsg_RejectsStaleSession`.

### `bumpFetchSessions()` — one helper, seven sites

Both counters are bumped at every context change that swaps `m.composer` or the grouped host context. There are exactly SEVEN such sites:

1. `execDoneMsg` (return from an interactive exec)
2. the `entryLocal` fast track
3. `esc` from the logs screen
4. `esc` from the progress screen
5. `enterGroupedContainers()` — connect success, the `entryLocal` grouped landing, and drill-OUT
6. `drillIntoGroup()` — drill-IN
7. `backToServerScreen()`

**All seven go through ONE helper, `bumpFetchSessions()`, which bumps `statsSession`, `statusSession`, `updatesSession` and `rollbackFetchSession` together.** Never write four hand-written `++` lines at a new site: the pairing has to be structural, because the failure it prevents is invisible. `TestUpdatesSession_BumpsAtAllSites` enumerates all seven as subtests (`drill_in`, `esc_container_to_proj`, `esc_grouped_to_server`, `entryLocal_fasttrack`, `execDoneMsg`, `esc_progress_to_container`, `esc_logs_to_container`); `TestUpdatesSession_BumpSitesFireRefresh`, `TestEnterGroupedContainers_ResetsAndBumpsSessions`, `TestSelectContainers_EscDrillOutBumpsSessions`, `TestEsc_bumpsStatsSession`, `TestExecDone_bumpsStatsSession`, `TestLogsEsc_bumpsStatsSession` and `TestProgressEsc_bumpsStatsSession` cover the same ground from the other direction.

**The grouped cycle CROSSES the first two counters, and that is why a partial bump latches the guard.** The grouped arrival passes its `statusSession` gate, and only then chains a stats fetch stamped `statsSession`. A site that bumped only `statusSession` would let the arrival through, chain a fetch under a `statsSession` the arrival never saw, and have the resulting `statsMsg` dropped at a gate that does NOT clear `refreshInFlight` — because the stale-session branch deliberately leaves the guard alone. That is the exact latch: a re-armed guard with no message left that can clear it, and a dead 5-second refresh for the rest of the visit.

**The two LONE `rollbackFetchSession++` sites are not this.** The `R` keypress and the `connectResultMsg` FAILURE path each invalidate one fetch without changing context, so they keep their single bump and must not be folded into the helper.

### `projectsSession` is gone

`projectsSession` and `projectsMsg` went with the project-select screen. The `ProjectLoader` they guarded now feeds `loadGroups`, which is already gated on `statusSession` at the same swap sites — so nothing was lost, and nothing new needs guarding.

### The `connectResultMsg` SUCCESS path DOES bump

It bumps all four counters, via `enterGroupedContainers()`. This is the INVERSE of the rule that held while the screen after a connect was the static project picker: the connect now lands on LIVE data, and **the site that starts fetching a resource is the site that must invalidate whatever a previous server left in flight**. The FAILURE path is different — it stays on the server screen and bumps only `rollbackFetchSession` alongside its own state cleanup (`TestUpdatesSession_NotBumpedAtConnectResultError`).

### The recipe for a new async message

Missing the session filter on any of these is the same class of bug; codex's adversarial review caught each one in successive rounds. **When adding a NEW async message type tied to a screen context, give it a session and gate the handler on `m.screen != <its-screen> || msg.session != m.<sessionField>`.** The existing examples to copy are `logsSession`, `configSession`, `inspectSession`, `waitSession` and `rollbackFetchSession`.

## Soft failure: a broken stats fetch never costs you the status

Status is the load-bearing primary view. Both front ends keep it intact when stats fail.

**CLI**: `cdeploy: stats unavailable: <err>` to stderr (or `cdeploy: stats unavailable for %q: <err>` in the per-project loop), exit 0, blank CPU/Mem cells, every other column unchanged.

**TUI**: `statsErr` renders as a soft warning line in the footer while the rows keep painting. `m.stats` is cleared alongside setting `statsErr`, so the screen never shows stale CPU/Mem cells next to a "Stats unavailable" warning — the contradiction would invite users to trust the stale numbers.

**The soft-warning slot priority is `svcErr > statsErr > updatesErr`.** `svcErr` does not share the slot: it early-returns from `viewSelectContainers` and replaces the WHOLE list with an error screen, which is also why the key dispatch goes inert while it is set (see `docs/architecture/tui-multi-project.md`). `statsErr` and `updatesErr` are mutually exclusive in the renderer, so the footer reserves at most one row for the pair.

The grouped status arrival deliberately leaves `m.stats` and `m.statsErr` alone — it is the 5-second refresh as well as the first load, and the chained half it starts is the slow half of the cycle. Clearing either would blank the CPU/Mem column, or drop the "stats unavailable" warning, for that whole window, on every tick (`TestGroupedStatusArrival_KeepsTheStatsCells`).

## Status refresh after an operation

Returning from the progress, logs or exec screen re-fetches `ContainerStatus()` via a `statusMsg` so the running/stopped dots reflect what the operation did. `ContainerStatus()` errors propagate through `statusMsg.err` and `servicesMsg.err` into `svcErr`; a successful refresh clears any prior error. Follow this pattern whenever an operation changes container state.

One narrowing: **a `statusMsg` ERROR is dropped while a container sub-state is active** (`m.containerSubStateActive()`). The error screen draws neither the confirm prompt nor the search bar, and the key swallow that makes row actions inert sits BELOW both sub-state intercepts in `handleKey` — so an already-armed prompt would vanish behind the error text while `enter` still ran the operation against a selection the user can no longer see. Nothing is lost: the next tick after the sub-state closes refetches. A SUCCESSFUL status refresh is not dropped, because it only repaints the dots.

## Test seam for the tick

`Model.tickCmdOverride func() tea.Cmd` is nil in production — `refreshTick()` falls through to `tea.Tick`. Tests install a replacement with `installFakeTick(&m)` (`internal/tui/app_test.go`), which swaps in a non-blocking Cmd returning `fakeTickMsg`.

Without the override, every test exercising a gated path (off-screen, in-flight, confirming) leaves a real 5-second `tea.Tick` goroutine running until the process exits. With it installed, tests assert directly on the returned command: a `tea.BatchMsg` means the fetch batch fired, anything else means the tick was a pure reschedule.

**`NewModel` sets `m.refreshInFlight = true` in both of its non-server branches** — the local fast track (`Init()` will fire `loadServices` + `statsRefreshCmd`) and the grouped landing (`Init()` will fire `loadGroups`). A test that exercises the UNBLOCKED tick path must clear it explicitly, or the tick it is testing is refused by the guard.

## Adding a screen, or a context change

**A new screen that should NOT refresh periodically needs no change**: the gate already excludes every screen other than `screenSelectContainers`.

**A new screen that DOES need its own periodic refresh gets its own tick** — a separate message type, not an overload of `refreshTickMsg`. Overloading would couple two refresh policies to one guard and one interval.

**A new context-change handler — anything that swaps `m.composer` or the grouped host context — calls `bumpFetchSessions()`**, never individual `++` lines, and then sets `refreshInFlight`: true if it fires a refresh batch, false if it only navigates away. It also resets `updateInFlight` before calling `maybeRefreshUpdatesCmd()`, for the reason `docs/architecture/update-detection.md` gives: the session bump invalidated any pending `updatesMsg`, and a leaked in-flight flag would refuse the next fetch forever.

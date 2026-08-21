# TUI `?` key-list overlay and trimmed container footer

## Overview

The container-select screen advertises 12 keys on one footer line. That line measures 150 display cells, so it never fits an 80-column terminal and rarely fits a 120-column one. Below 152 columns the renderer splits it into two lines, and the second line costs the service list one row — exactly where the list is already short.

This change moves the full key reference behind a `?` overlay and trims the footer to 6 tokens (74 cells). The overlay replaces the screen, like the existing remote-disconnect prompt, and lists the keys for whichever screen the user came from. The footer keeps the four verbs people press most and points at `?` for the rest.

Two smaller results come with it. The footer fits an 80-column terminal at every state, so the two-line fallback stops firing on normal terminals and the list keeps that row. And the width guard that decides one-line-versus-two is corrected: it currently measures bytes, not display cells.

This is deliberately step 1 of the menu idea, not a substitute for it. Section "Future work" records the upgrade path.

> **Revision note.** This plan was revised after an automated review. Five substantive corrections were folded in: the footer split (the first draft dropped `q back` and `? keys` during a search), the shared-constant regression, overlay width handling, the `?`-during-confirmation decision, and the `ctrl+c` rationale. Findings are marked ⚠️ where they changed a decision.

## Context (from discovery)

Files involved:
- `internal/tui/app.go`
  - `:19` — `github.com/charmbracelet/x/ansi` is **already imported**; the width fix adds no dependency
  - `:51-54` — the comment on `containerHelpLine2` explaining why it is shared
  - `:55` — `containerHelpLine2`, the 99-cell second half of the footer
  - `:195-202` — the screen constants; there are **8**, not 7 (`screenSettingsList` and `screenSettingsForm` are separate)
  - `:341`, `:349`, `:373`, `:379` — the four `textinput.Model` fields on Model
  - `:1020` — the `connectResultMsg` error path, one of only two non-key-driven `m.screen` assignments
  - `:1383` — `handleKey()`; `:1386-1403` the `quitting` intercept; `:1405` its comment and `:1412` the `q`→`esc` rewrite
  - `:1618` — the container-screen `m.confirming` sub-state
  - `:2377` — `ctrl+c` on `screenProgress`, guarded by `m.done || m.failed`
  - `:3836-3853` — footer line math inside `svcVisibleCount()` (`back :=` at 3836, guard at 3848)
  - `:3985` — `View()`; `:3986-3988` the `quitting` early check
  - `:4011` — `viewQuitConfirm()`, the screen-replacing-view precedent
  - `:4499-4517` — footer render inside `viewSelectContainers()` (`back :=` at 4499, `line1` at 4503, guard at 4513)
- `internal/tui/app_test.go` — four tests assert footer tokens that move to the overlay
- `internal/tui/footer_reservation_test.go:30` — `TestContainerFooterReservation` pins the footer line math
- `CLAUDE.md:42`, `CLAUDE.md:104` — two sentences that become false
- `README.md:63`, `README.md:70-75` — the container-key prose and the key table

Related patterns found:
- `quitting bool` is the exact precedent: a Model flag, a global intercept at the top of `handleKey()`, and an early check in `View()` that returns a standalone view. `helpOpen` copies this shape.
- The `q`→`esc` rewrite already carries three typing exceptions (`m.searching`, `m.logFiltering || m.logSearching`, settings form fields below 4). The `?` intercept needs the same three.
- `docs/plans/completed/20260516-q-as-back-key.md` is the direct structural precedent — same class of change, same file, same test style. It also documents its interaction with the confirmation sub-states, which this plan now matches.

Measurements (computed against the live strings, not estimated):

| footer | tokens | cells | bytes | fits 80 | fits 100 |
|---|---|---|---|---|---|
| today | 12 | 150 | 172 | no | no |
| after (variant A) | 6 | 74 | 84 | yes | yes |

Verified empirically by the review, through a `go test -overlay` that applied variant A plus the width fix without modifying the repo:
- `svcVisibleCount()` at height 24 goes 17 → 18 at widths 76, 80, 100, 120 and 160, and is unchanged at 40, 60, 174 and 200. The "gains one row between 76 and 173" criterion is exact.
- Exactly four tests fail, and nothing else in the repository.

Existing tests to update:
- `TestViewSelectContainers_ShowsConfigKey` (app_test.go:5736) — asserts the footer contains `c config`
- `TestViewSelectContainers_ShowsExecKey` (app_test.go:8905) — asserts `x exec`
- `TestViewSelectContainers_HelpFooterIncludesUpdates` (app_test.go:11205) — asserts `U updates`
- `TestViewSelectContainers_FooterIncludesRollback` (app_test.go:14194) — asserts `R rollback`

All four test the same intent: "this key is discoverable." All four tokens leave the footer under variant A. Redirect each assertion to the `?` overlay instead of deleting it — the intent survives, only the location moves.

Verified unaffected, so nobody re-checks them:
- `TestViewSelectContainers_HelpIncludesLogs` (app_test.go:2937) — passes, because `l logs` stays in the footer
- `TestContainerFooterReservation`, `TestSearchBarLineNeverWraps`, `TestLogBarLineNeverWraps` — all pass unchanged
- `skills/cdeploy/SKILL.md` — needs no change; line 11 says the TUI "requires a real TTY — never invoke", and it lists no TUI keys
- `cmd/root.go:38-51` — the CLI help text lists no TUI keys

## Development Approach

- **testing approach**: Regular (code and tests in the same task, tests written before the task closes) — matches the q-as-back-key precedent; TUI tests are pure `Update()` and `View()` calls, so TDD ceremony buys little here
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for the code changed in that task
  - tests are a required deliverable, not optional
  - cover both success and error/edge scenarios
- **CRITICAL: all tests must pass before starting the next task** — run `go test ./internal/tui/ -count=1` between tasks
- **CRITICAL: update this plan file when scope changes during implementation**
- maintain backward compatibility: `esc`, `q`, and `ctrl+c` behavior is unchanged on every screen

## Testing Strategy

- **unit tests**: required in every task. Pure `Update()` and `View()` calls with synthetic `tea.KeyMsg` — no Docker, no TTY, no goroutines.
- **e2e tests**: the project has none. The TUI is tested through `Update()` directly. Manual checks are listed under Post-Completion.
- **regression pins**: three tests prove `?` reaches an open text input (container search, log filter, settings form). These are the highest-value tests in the plan — see Technical Details for why.
- **drift pin**: a table test asserts each screen's help table names every key that screen's `handleKey` case binds. A static help table that silently drifts from the dispatch is the standard failure mode of this feature.
- **regression check**: run `go test ./... -count=1` in the verification task to confirm no other package is affected.

The typing-exception list is confirmed complete. Model has exactly four `textinput.Model` fields — `logFilterInput` (`app.go:341`), `logSearchInput` (`:349`), `settingsInputs [4]` (`:373`), `searchInput` (`:379`) — and every `Focus()` site (`:1837`, `:2092`, `:2118`, `:2237`, `:2258`, `:2281`, `:2289`) falls under one of the three conditions. No other handler consumes a raw rune, and neither `bubbles/viewport` nor `bubbles/textinput` binds `?`.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

One new Model flag, one new global intercept, one new view function, and a shorter footer built by a shared helper.

`helpOpen bool` sits beside `quitting`. When it is true, `View()` returns `viewHelp()` instead of the normal screen, and `handleKey()` swallows every key except the ones that close the overlay. `viewHelp()` reads `m.screen` to choose which key table to print, so the overlay is context-aware without any per-screen wiring.

Key design decisions:

1. **A flag, not a screen.** Adding a ninth screen constant would drag the change into the back-navigation chain, the `clearSearch()` departure checklist (9 sites), and the session-counter discipline. A flag that no navigation key can escape avoids all of it. *Verified:* `Update()` has one `case tea.KeyMsg` (`app.go:713`), so `handleKey` is the sole key entry point, and every `m.screen = screen*` assignment is key-driven except two — `NewModel` (where `helpOpen` is false) and the `connectResultMsg` error path (`app.go:1020`, handled below). So the overlay can never leave the UI stuck.
2. **A screen-replacing view, not an inline panel.** An inline panel would have to fight `svcVisibleCount()` and the one-physical-line invariants. A full-screen view is invisible to all of that math — but it still needs its own width clamp, because this package clamps every rendered bar (⚠️ finding 3).
3. **No composer call, no goroutine.** The overlay renders from static data. So it needs no session counter and no stale-message guard.
4. **Only the container footer changes.** `?` works on every screen, but only the container screen advertises it. The other footers are already short, and the logs footer would start to wrap if a token were appended. Users learn `?` once, which is how k9s and lazygit behave.
5. **A new `typingInInput()` helper, not a refactor of the `q` rewrite.** The `q` rewrite mixes typing exceptions with unrelated root-screen and mid-progress carve-outs. Extracting a shared helper would touch working code with its own test suite for no behavior gain. Accept the duplication and document the coupling.
6. **⚠️ `?` is swallowed at a confirmation prompt.** While `m.confirming` (container, `app.go:1618`) or `m.settingsDelete` (settings list) is true, `?` does nothing. Opening the overlay over a live destructive prompt would hide it, and the single `esc` that closes the overlay would leave the prompt armed underneath — a two-stage `esc` nobody expects. The precedent plan made the same call for `q`.

## Technical Details

### The typing exception is the load-bearing detail

`?` is a regex metacharacter. `ctrl+r` puts the log filter into Go RE2 mode. Without an exception, a user who types `(?:web|db)` into their own log filter would get a help overlay instead of a filter.

The same applies to the container search bar and the settings-form text fields. So the `?` intercept must skip all three, exactly as the `q` rewrite already does:

```go
func (m Model) typingInInput() bool {
	switch m.screen {
	case screenSelectContainers:
		return m.searching
	case screenLogs:
		return m.logFiltering || m.logSearching
	case screenSettingsForm:
		return m.settingsField < 4 // 0-3 are textinputs; 4 is the color picker
	}
	return false
}
```

The `< 4` form matches the surrounding code (`app.go:2367`, `:2280`, `:2288`) and is safe for out-of-range values, unlike `!= 4`.

**Coupling note for CLAUDE.md:** any future screen that opens a text input must be added to BOTH `typingInInput()` and the `q`→`esc` rewrite. The two lists are duplicated on purpose (decision 5 above).

### Intercept order

Order matters. The swallow block must precede the open block, or `?` while the overlay is open would re-open it and never close it.

```
1. quitting intercept                                    (existing, app.go:1386-1403)
2. if m.helpOpen { ...close or swallow... }              NEW, inserted at app.go:1404
3. if key == "?" && !typing && !confirming { open }       NEW
4. q -> esc rewrite                                      (existing, app.go:1405 comment / :1412)
```

Close keys are `?`, `esc`, and `q`. `ctrl+c` is a special case: it clears `helpOpen` and then **falls through** rather than returning.

⚠️ The reason is *not* "ctrl+c is a hard exit from any screen" — that rule has an exception. On `screenProgress` while `!m.done && !m.failed`, `ctrl+c` is a no-op (`app.go:2377`, and `CLAUDE.md:34`). The correct and stronger statement is: **the fall-through makes each screen's existing `ctrl+c` semantics apply unchanged** — the mid-progress no-op, the remote-disconnect prompt, and the plain local quit.

The open block is additionally gated on `!m.confirming && !m.settingsDelete` (decision 6).

### `connectResultMsg` and the open overlay

⚠️ `app.go:1020` changes `m.screen` from a message handler, not a key. If the overlay is open when a connection attempt fails, the key table silently switches from server keys to project keys. Set `m.helpOpen = false` in that error path, alongside the state it already clears.

### Overlay layout

Two columns, so the densest screen fits a 24-line terminal:

```
  cdeploy · keys                                        services


  MOVE                            OPERATE
    ↑ k      up                     d      deploy
    ↓ j      down                   r      restart
    /        search                 s      stop
    n N      next / prev match      R      rollback

  SELECT                          INSPECT
    space    toggle                 l      logs
    a        all                    c      config
                                    x      exec
  LEAVE                             U      check updates
    q esc    back
    ctrl+c   quit


  ?  or  esc   close
```

⚠️ `q` is listed alongside `esc` in LEAVE. The first draft omitted it, but `q` is a first-class back key (`CLAUDE.md:34`, `README.md:73`) and the footer renders `q back`. The container screen binds 17 keys and the table must name all 17.

The container screen is the densest at 13 left-column lines and 11 right-column lines, so the block is about 18 lines including the title and the close hint. That fits the 24-line minimum. A single column would need roughly 30 lines, which is why the layout is two columns.

⚠️ **Width handling is required.** The widest row is about 56 cells. Below roughly 60 columns the two columns wrap, which breaks the 18-line claim that justifies the layout. `TestContainerFooterReservation` already drives width 40, so narrow terminals are a real case here. Two mechanisms, matching how this package guards `searchBarLine()` and `logBarLine()`:
1. fall back to a single column below a threshold width
2. clamp every rendered row with `clampToWidth` as a backstop

⚠️ Backing data splits into pure data plus layout, so the width fallback is cheap:

```go
type helpEntry struct{ keys, desc string }
type helpGroup struct {
	title   string
	entries []helpEntry
}

func helpGroups(s screen) []helpGroup                       // pure data, table-testable
func layoutHelpColumns(groups []helpGroup, width int) string // one or two columns
```

The first draft returned `(left, right []helpGroup)` from one function, which baked the column split into the data and made the narrow fallback impossible without a signature change.

### Footer change

⚠️ **This is the correction that matters most.** `line2` is wholly replaced by the two search-state branches at `app.go:4505-4509` and `:3842-3846`:

```go
line2 := containerHelpLine2
if m.searching {
	line2 = "  enter jump  •  esc cancel"
} else if m.searchQuery != "" {
	line2 = "  n/N cycle  •  esc clear"
}
```

So **anything that must always be visible has to live in `line1`.** The first draft moved `back` into `line2` and added `? keys` there, which dropped both whenever a search was open or committed — including the exact state where the overlay matters most, since `/ search` has left the footer. The split below is therefore forced, not a style choice:

```go
line1 := fmt.Sprintf("  space toggle  •  %s  •  ? keys", back)  // 36 cells, always shown
line2 := "  d deploy  •  r restart  •  l logs"                  // 35 cells, search may replace
```

Measured: idle one-line is 74 cells, search-typing 66, search-committed 64. All fit 80 columns. The one-line reading order becomes meta-then-verbs (`space toggle • q back • ? keys • d deploy • r restart • l logs`). That is a consequence of the constraint above — do not "fix" it by moving `back` back into `line2`.

⚠️ **Keep the sharing.** `containerHelpLine2` is a `const` precisely so the render and the height math cannot drift (`app.go:51-54`: "a divergence would miscount visible rows and let the list overflow the terminal"). Variant A needs `back` interpolated, so a `const` no longer works — but writing `fmt.Sprintf` independently at both call sites recreates the duplication the const was added to prevent. Replace it with a shared helper:

```go
func containerHelpLines(back string) (line1, line2 string)
```

called from both `app.go:3840` and `app.go:4503`. The search-state overrides stay at each call site, since only the render needs them for display and the height math needs them for width.

### Width guard fix

Both footer sites build the same probe string and compare it to `m.width`:

```go
oneLine := line1 + "  •  " + line2[2:]
if m.width >= len(oneLine)+2 {
```

`len()` counts bytes. Each `•` is 3 bytes but one display cell, so the guard over-counts by 2 per separator. Today that makes the footer split into two lines anywhere from 152 to 173 columns, where it would in fact fit. Replace `len` with `ansi.StringWidth` (already imported at `:19`, already used by `clampToWidth` in this package) at BOTH sites:

```go
if m.width >= ansi.StringWidth(oneLine)+2 {
```

⚠️ Side effect worth knowing: the comment `160 /* one-line help */` at `footer_reservation_test.go:92` is **wrong today** — the byte guard is 174, so width 160 currently renders two lines. The test passes anyway because it asserts `rows == svcVisibleCount()` and constant height, not the line count. This change makes the label true for the first time. Correct the comment deliberately; do not read it as a behavior regression.

### Accepted trade-off

`/ search` leaves the footer. That feature shipped recently (`docs/plans/completed/20260711-tui-container-search.md`), so this costs some discovery. It was an explicit choice: variant B kept `/ search` but measured 98 cells and still wrapped at 80 columns. The mitigation is that `/` appears in the overlay under MOVE, and that `q back`/`? keys` now survive the search states. Listed under Post-Completion as something to watch.

## Future work (NOT in this plan)

The overlay upgrades into the lazygit-style action menu with a cursor field and one dispatch line:

```go
m.helpOpen = false
return m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
```

Because `handleKey` is a value method returning `(tea.Model, tea.Cmd)` (`app.go:1383`), re-entry is legal. The menu would then own **zero** duplicate action logic — it inherits the `R` rollback-target capture, the `confirming` flow, and the `x` exec guard for free. Note that lazygit's `x` binding is unavailable here; `x` is already exec-into-container.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, and documentation inside this repository
- **Post-Completion** (no checkboxes): manual terminal checks and follow-up observations

## Implementation Steps

### Task 1: Add `helpOpen` state and the `viewHelp()` renderer

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] add `helpOpen bool` to Model, beside `quitting`
- [ ] add `helpEntry` / `helpGroup` types and the pure `helpGroups(s screen) []helpGroup` covering all 8 screens, including `q esc back` in the container screen's LEAVE group
- [ ] add `layoutHelpColumns(groups []helpGroup, width int) string` with a single-column fallback below the two-column threshold, clamping each row with `clampToWidth`
- [ ] add `viewHelp()` composing title, layout, and close hint, mirroring `viewQuitConfirm()` at `app.go:4011`
- [ ] add the `helpOpen` early check in `View()` at `app.go:3985`, directly after the existing `quitting` check
- [ ] write a table test asserting `helpGroups()` returns a non-empty table for every one of the 8 screen constants
- [ ] write the drift pin: for each screen, assert the help table names every key that screen's `handleKey` case binds (hand-maintained expected set per screen)
- [ ] write a test that `helpOpen = true` makes `View()` render the overlay (contains "OPERATE") instead of the container list
- [ ] write a test that the container-screen overlay is at most 24 lines tall
- [ ] write a width test asserting no rendered row exceeds `m.width` at widths 120, 80, 60 and 40
- [ ] run `go test ./internal/tui/ -count=1` — must pass before Task 2

### Task 2: Add the `?` open/close intercept with typing and confirmation exceptions

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] add `typingInInput()` using `m.settingsField < 4`, covering `screenSelectContainers`, `screenLogs` and `screenSettingsForm`
- [ ] add the `helpOpen` swallow block in `handleKey()` at `app.go:1404`: `?`/`esc`/`q` close and return; `ctrl+c` clears the flag and falls through; every other key is swallowed
- [ ] add the `?` open block immediately after it, gated on `!m.typingInInput() && !m.confirming && !m.settingsDelete`
- [ ] clear `m.helpOpen` in the `connectResultMsg` error path at `app.go:1020`
- [ ] write a test that `?` on the container screen sets `helpOpen`
- [ ] write a table test that `?`, `esc` and `q` each close the overlay
- [ ] write a test that `d` while the overlay is open does NOT set `pendingOp` (the swallow works)
- [ ] write a test that `ctrl+c` while the overlay is open still produces `tea.QuitMsg` on a local session
- [ ] write a test that `ctrl+c` while the overlay is open mid-progress stays a no-op (the fall-through preserves per-screen semantics)
- [ ] write the three typing regression pins: `?` reaches the open log-filter input, the container search input and settings-form field 0 — asserting `helpOpen` stays false AND the input value contains `?`
- [ ] write two confirmation tests: `?` while `m.confirming` and while `m.settingsDelete` leaves the prompt intact and `helpOpen` false
- [ ] run `go test ./internal/tui/ -count=1` — must pass before Task 3

### Task 3: Trim the container footer and fix the width guard

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`
- Modify: `internal/tui/footer_reservation_test.go`

- [ ] replace the `containerHelpLine2` const at `app.go:51-55` with `containerHelpLines(back string) (line1, line2 string)`, carrying over the anti-drift comment
- [ ] call the helper from both `viewSelectContainers()` (`app.go:4503`) and `svcVisibleCount()` (`app.go:3840`), leaving the search-state `line2` overrides at each site
- [ ] replace `len(oneLine)` with `ansi.StringWidth(oneLine)` at both guards (`app.go:3848`, `:4513`)
- [ ] redirect the four token assertions (app_test.go:5736, :8905, :11205, :14194) from the footer to the `?` overlay, keeping each test's original intent and name
- [ ] write the three-state footer test: `? keys` and the `back` token are present when idle, while searching, and with a committed search
- [ ] write a test that the one-line footer's `ansi.StringWidth` plus 2 is at most 80 in all three states
- [ ] add an 80-column case to `TestContainerFooterReservation` and correct the stale `160 /* one-line help */` comment at `footer_reservation_test.go:92`
- [ ] run `go test ./internal/tui/ -count=1` — must pass before Task 4

### Task 4: Verify acceptance criteria

- [ ] `?` opens the overlay from all 8 screens, and the content matches the screen it was opened from
- [ ] `?` typed into an open log filter, container search or settings text field inserts a literal `?`
- [ ] `?` at a deploy/stop/rollback confirm prompt and at a server-delete confirm prompt does nothing
- [ ] `ctrl+c` from the overlay reproduces each screen's existing behavior: quits locally, prompts on remote, no-ops mid-progress
- [ ] the container footer renders on one line at width 80 in all three search states
- [ ] the service list gains one row at widths between 76 and 173 versus the previous build
- [ ] run the full suite: `go test ./... -count=1`
- [ ] run `go vet ./...` and `gofmt -l .`

### Task 5: [Final] Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`

- [ ] add a CLAUDE.md section for the `?` overlay next to "Exit confirmation for remote connections", covering the flag-not-a-screen decision, the intercept order, the confirmation exception, and the `ctrl+c` fall-through
- [ ] state the `ctrl+c` rationale correctly: the fall-through makes each screen's existing semantics apply unchanged, including the mid-progress no-op — NOT "ctrl+c is a hard exit from any screen"
- [ ] document the `typingInInput()` duplication rule: a new text-input screen must be added to BOTH that helper and the `q`→`esc` rewrite
- [ ] document why `line1` carries `back` and `? keys` — the search-state override of `line2` makes the split forced
- [ ] record the byte-versus-cell width fix so the `len()` form is not reintroduced
- [ ] fix `CLAUDE.md:42` ("Help footer adds `/ search`") — `/ search` now lives in the overlay
- [ ] fix `CLAUDE.md:104` ("Footer adds `U updates` token") — `U updates` now lives in the overlay
- [ ] update `README.md:63` — the container-screen key prose still enumerates the old footer tokens
- [ ] add a `?` row to the key table at `README.md:70-75`
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification:**
- resize a real terminal through the 76-to-173 column range and confirm the footer stays on one line and the list keeps its extra row
- open a log filter, press `ctrl+r` for regex mode, and type `(?:web|db)` — confirm it filters rather than opening help
- open the overlay on a 24-line terminal and on a 50-column terminal; confirm nothing is clipped and the narrow case falls back to one column
- open the overlay over a remote SSH session and press `ctrl+c` — confirm the disconnect prompt still appears

**Observations to watch:**
- `/ search` no longer appears in the footer. Watch for reports that the search feature became hard to find. If it happens, the cheapest fix is to swap `r restart` for `/ search` rather than to widen the footer.

**Follow-up work:**
- the menu upgrade described under "Future work" is a separate plan

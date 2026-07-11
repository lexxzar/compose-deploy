# TUI Container-List Search & Jump

## Overview
Add an incremental **search & jump** to the container select screen (`screenSelectContainers`) of the cdeploy TUI, triggered by the `/` key. When a compose project has many services (~20+), stepping through the list with `j`/`k` is slow. Search lets the user type a substring, jump the cursor straight to the matching service, and cycle between matches with `n`/`N`.

Key property: this is **search-and-jump, not a filter**. The service list (`m.services`) is never narrowed and the position-keyed multi-select map (`m.selected`) is never touched — only the cursor moves and matching rows are highlighted. This keeps the existing multi-select / deploy / restart / stop semantics completely intact.

Benefits:
- Fast navigation in large lists without disturbing selection or list order.
- Fully synchronous — no goroutine, channel, or session-counter machinery (unlike stats/status/updates).
- Small, contained blast radius: TUI package only, no CLI/runner/compose changes.

## Context (from discovery)
- **Files/components involved:**
  - `internal/tui/app.go` (~3124 lines) — Model struct, `handleKey`, `View`, container-screen render loop, cleanup sites.
  - `internal/tui/styles.go` — lipgloss styles (has `updateGlyphStyle` yellow to reuse).
  - `internal/tui/app_test.go` — no-TTY tests driving `Update()` with `tea.KeyMsg`; `installFakeTick` helper exists.
- **Key anchor points verified:**
  - Container-screen key handling: `case screenSelectContainers:` at `app.go:1208`; `confirming` intercept at 1209-1226; bare-letter switch at 1230-1344; `esc` container→project reset block at 1233-1256.
  - `q`→`esc` rewrite block at `app.go:1027-1053`; `screenSelectContainers` sub-case quits when `!m.showPicker && !m.confirming` (line ~1035) — **must also skip rewrite/quit when `m.searching`**. Settings-form textinput exception at 1046-1050 is the model to mirror.
  - Render loop iterating `m.services` and checking `m.selected[i]` around `app.go:2764`.
  - Selection helpers `allSelected`/`selectedContainers`/`selectedCount` at 2159-2188.
  - Scroll helpers: `hasStatusColumns()` at 2197, `svcVisibleCount()` at 2225, plus `fixSvcOffset()`.
  - `servicesMsg` handler at 574 (resets `svcCursor`/`svcOffset`); `connectResultMsg` at 825 & 1122; `enterProgress` at 1730; `enterExec` at 1801; `entryLocal` handler around 1068; `esc` proj→server reset around 1140-1200.
  - `textinput` already imported (`app.go:15`); `initSettingsInputs()` at 2334 shows the init pattern; `settingsInputs [4]textinput.Model` field at 285.
- **Conventions observed:** heavy state-cleanup discipline on every back-nav / context-change (CLAUDE.md's "Backward-navigation state cleanup" + "always reserve the column" width discipline); stdlib `testing` only, no testify.

## Development Approach
- **Testing approach**: Regular (code first, then tests) — but every task ships its own tests before the next task starts.
- Complete each task fully before moving to the next; small, focused changes.
- **Every task includes new/updated tests** as separate checklist items (success + edge/error cases).
- **All tests must pass before starting the next task.**
- Run `go build -o cdeploy .` and `go test ./internal/tui/ -v` after each change.
- Maintain backward compatibility — no behavior change when search is unused.

## Testing Strategy
- **Unit tests**: required per task. `computeMatches` gets a pure table-driven test. State transitions are tested by constructing a `Model`, feeding `tea.KeyMsg` through `Update()`/`handleKey`, and asserting field values (no TTY needed — the established `app_test.go` pattern).
- **Rendering assertions**: `View()` output substring checks (highlighted name present, bottom-bar text, counter, reserved line) — no golden files.
- **No e2e**: this project has no browser/UI e2e harness; TUI is validated via `Update()`-driven unit tests.
- Regression guards are explicit test cases (see Tasks 3 & 6): `q` typed while searching lands in the input; `selectedContainers()` is unchanged by an active search.

## Progress Tracking
- Mark completed items `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Keep this file in sync if scope changes during implementation.

## Solution Overview
Search is a synchronous sub-state of `screenSelectContainers`, layered like the existing `confirming` sub-state:
- **Typing mode** (`m.searching == true`): the bottom bar is an open `textinput`; keystrokes feed it; on every change matches are recomputed and the cursor live-jumps to the first match.
- **Committed/live mode** (`m.searchQuery != "" && !m.searching`): bar closed, matching rows stay highlighted, `n`/`N` cycle matches, `j`/`k` move but keep the highlight, and the first `esc` clears the search (second `esc` back-navigates).

Highlighting and `n`/`N` are driven purely by `m.searchQuery != ""`. All matching is a pure `computeMatches(services, query) []int` (case-insensitive substring on service name). No async messages or session counters are introduced.

## Technical Details

### New Model fields (`internal/tui/app.go`)
```go
searching     bool            // search bar open, capturing text
searchInput   textinput.Model // reuse settings-form init pattern
searchQuery   string          // current query; != "" ⇒ highlights + n/N live
searchMatches []int           // indices into m.services that match (cached)
searchReturn  int             // svcCursor to restore on esc-while-typing
```

> **Init note (from plan review):** `searchInput` is (re)constructed via `textinput.New()` inside the `case "/"` open handler, **not** only in `NewModel`. ~18 tests build `Model{}` literals directly (bypassing `NewModel`), which would leave a zero-value `textinput.Model` that doesn't echo runes. Constructing on open makes the feature and its tests correct regardless of how the `Model` was built. `searchInput.View()` is only ever rendered while `searching` (i.e. after `/`), so a zero-value input is never displayed. Adding a `textinput.New()` in `NewModel` too is harmless but not required.

### Pure matcher
```go
func computeMatches(services []string, query string) []int
// lowercase substring; empty query → nil; preserves list order.
```

### clearSearch helper
```go
func (m *Model) clearSearch() {
    m.searching = false
    m.searchQuery = ""
    m.searchMatches = nil
    m.searchReturn = 0
    m.searchInput.SetValue("")
    m.searchInput.Blur()
}
```

### Key-handling precedence (`handleKey`, `case screenSelectContainers`)
1. Existing `if m.confirming { … }` block stays first.
2. **New** `if m.searching { … }` intercept immediately after it, before the bare-letter switch:
   - `enter` → commit: `searching=false`; keep `searchQuery`/`searchMatches`; if `len(searchMatches)==0` also `clearSearch()`.
   - `esc` → cancel: `searching=false`; clear query/matches; `svcCursor = searchReturn`; `fixSvcOffset()`; consume (no back-nav).
   - `ctrl+c` → `tryQuit()` (unchanged).
   - default → route the key to `searchInput`, then `searchQuery = searchInput.Value()`, `searchMatches = computeMatches(...)`, and if matches, `svcCursor = searchMatches[0]; fixSvcOffset()`.
3. Non-searching switch additions:
   - `case "/"` → open: **guard `if len(m.services) == 0 { return m, nil }`** (mirror `l`/`x`), then `m.searchInput = textinput.New()` (fresh input on every open — see note below), `searching=true`, `searchReturn=svcCursor`, `searchInput.Focus()`. No `CharLimit` (service names/queries are short but unbounded); no placeholder.
   - `case "n"` / `case "N"` → cycle (no-op when `searchQuery==""`).
   - `case "esc"` gains a two-stage guard **at its top**: `if m.searchQuery != "" { m.clearSearch(); m.fixSvcOffset(); return m, nil }` then fall through to the existing back-nav.

### `q`→`esc` rewrite exception (`app.go:1027-1053`)
In the `screenSelectContainers` sub-case, when `m.searching` is true, leave `key == "q"` untouched and fall through (do **not** rewrite to `esc`, do **not** quit) so `q` reaches the search intercept and lands in the textinput. Mirror the existing settings-form field-4 exception.

### Cycle logic (`n`/`N`, committed)
Find the current cursor's position within `searchMatches`; step ±1 with **wrap-around**; set `svcCursor`; `fixSvcOffset()`. If the cursor is off all matches (after a manual `j`/`k`), `n` jumps to the first match at/after the cursor, `N` to the first match at/before it.

### Rendering (`View`, container render loop + bottom bar)
- In the row loop: if `searchQuery != "" && i ∈ searchMatches`, wrap the name in `searchMatchStyle`; if additionally `i == svcCursor`, use `searchCurrentStyle` (bold). Non-matches unchanged. Display-width math is unaffected (ANSI escapes don't count — same rationale as the update glyph / ports column).
- Bottom bar line, rendered just above the help footer:
  - `searching` → `/ ` + `searchInput.View()` + `  (i/N)` (or `(no match)` when `searchMatches` empty).
  - committed → compact `↳ <name>  (i/N)` + hint `n next · N prev · esc clear`.
  - idle → **blank line always reserved** so list height never jumps (mirrors the "always reserve the update-glyph column" discipline).
  - **confirming interaction:** search and `confirming` can coexist (a committed search is live, user presses `d`). The confirm prompt and the search bar share the **same** reserved footer line — the confirm prompt takes precedence and replaces the bar text while `m.confirming`. The line is reserved either way.
- `svcVisibleCount()` reserves the extra footer line in **both** the `confirming` early-return branch (`app.go:2237`) and the normal branch — otherwise the list height jumps by one line when entering/leaving confirmation with a search active.
- Help footer: add `/ search`; while searching show `enter jump · esc cancel`; committed show `n/N cycle · esc clear`.

### Styles (`internal/tui/styles.go`)
- `searchMatchStyle` — same yellow as `updateGlyphStyle`.
- `searchCurrentStyle` — yellow + `Bold(true)`.

### Cleanup wiring

> **Correction from plan review:** the earlier "every site that resets `svcCursor`/`svcOffset`/`selected`" rule was wrong — only **two** sites actually reset those fields (`servicesMsg` at `app.go:588-592` and `esc` container→project at `1249-1251`). The real invariant is index validity: `searchMatches` holds indices into `m.services`, so a committed search is only valid while `m.services` order/membership is unchanged.

**Design decision — search is ephemeral and scoped to the container screen.** Call `m.clearSearch()` on every transition that *leaves* `screenSelectContainers` (to a nested screen or another context) or *reloads* `m.services`. Consequently every one of the six paths back into `screenSelectContainers` (`app.go:486`, `1101`, `1200`, `1365` logs, `1415` config, `1673` progress) lands on a clean list, `searchMatches` can never carry stale indices, and **no return path needs its own `clearSearch()`**. This is the most robust reading of the codebase's cleanup discipline and avoids stale-highlight bugs.

`clearSearch()` call sites (all "leaving the screen" or "reloading the list"):
1. `servicesMsg` handler (`app.go:588`) — full reload / initial load; indices invalid.
2. `esc` container→project (`app.go:1233`).
3. `esc` project→server (`app.go:1140`).
4. `entryLocal` handler (`app.go:1068`).
5. `connectResultMsg` error path (`app.go:825`).
6. `enterLogs()` (the `l` action) — leaving to `screenLogs`.
7. `enterConfig()` (the `c` action) — leaving to `screenConfig`.
8. `enterProgress()` (`app.go:1730`) — operation start.
9. `enterExec()` (`app.go:1801`) — operation start.

Return paths (`esc` from logs `1365`, config `1415`, progress `1673`; `execDoneMsg` `950`) get **no** `clearSearch()` — departure already cleared it. A committed search does **not** survive a logs/config peek or an operation; the user re-opens `/` if needed (rationale: once you've found and acted on a service the search has served its purpose, and this keeps the invariant a single checkable rule instead of per-path "is the list still valid?" reasoning).

## What Goes Where
- **Implementation Steps** (checkboxes): all code, tests, and the CLAUDE.md doc note — everything is in-repo.
- **Post-Completion** (no checkboxes): manual TTY smoke test of the feel (live jump, cycle, highlight, two-stage esc) in a real terminal.

## Implementation Steps

### Task 1: Add `computeMatches` pure matcher

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] add `func computeMatches(services []string, query string) []int` — lowercase both sides, `strings.Contains` substring, preserve list order, return `nil` for empty query
- [x] write table test: substring match, case-insensitivity (`Web` matches `web-worker`), multiple matches in order, empty query → `nil`, no-match → empty/nil, empty services → nil
- [x] run `go test ./internal/tui/ -run ComputeMatches -v` — must pass before next task

### Task 2: Add search state fields, `clearSearch` helper, and styles

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/styles.go`
- Modify: `internal/tui/app_test.go`

- [x] add the five fields (`searching`, `searchInput`, `searchQuery`, `searchMatches`, `searchReturn`) to the `Model` struct (a zero-value `searchInput` is fine — it is (re)constructed in the `/` handler in Task 3, not here)
- [x] add `func (m *Model) clearSearch()` resetting all five fields (SetValue("")/Blur() on the input)
- [x] add `searchMatchStyle` (yellow, same as `updateGlyphStyle`) and `searchCurrentStyle` (yellow + Bold) to `styles.go`
- [x] write test: set the five fields to non-zero, call `clearSearch()`, assert all reset (searching=false, query="", matches=nil, return=0, input value empty)
- [x] run `go build -o cdeploy .` and `go test ./internal/tui/ -v` — must pass before next task

### Task 3: Open / type / commit / cancel (typing mode) + `q` exception

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] add the `if m.searching { … }` intercept right after the `confirming` block in `case screenSelectContainers`: handle `enter` (commit; clear if no matches), `esc` (cancel + restore `searchReturn`), `ctrl+c` (`tryQuit`), default (route to `searchInput`, recompute matches, live-jump to `searchMatches[0]`, `fixSvcOffset`)
- [x] add `case "/"` to the non-searching switch: guard `if len(m.services)==0 { return m, nil }` (mirror `l`/`x`), then `searchInput = textinput.New()`, `searching=true`, `searchReturn=svcCursor`, `searchInput.Focus()`
- [x] add the `m.searching` exception to the `q`→`esc` rewrite block so `q` is not rewritten/does not quit while searching (mirror the settings-form field-4 exception at `app.go:1046`); note the `screenSelectContainers` sub-case quits when `!showPicker && !confirming`, so the exception must short-circuit *before* that quit
- [x] write tests: `/` sets `searching=true`; typing `w` sets `searchQuery="w"`, populates `searchMatches`, moves `svcCursor` to first match; `enter` commits (`searching=false`, query kept); `enter` with a no-match query clears `searchQuery`; `esc` while typing restores `svcCursor` to `searchReturn` and clears query
- [x] write test: `/` on an empty list (`len(m.services)==0`) is a no-op (`searching` stays false)
- [x] write regression test: with `searching=true`, a `q` KeyMsg lands in `searchInput` (value becomes `"q"`) and does **not** quit or navigate back (template: `TestQTypedIntoSettingsFormInput` at `app_test.go:6131`)
- [x] run `go test ./internal/tui/ -v` — must pass before next task

### Task 4: Committed cycle (`n`/`N`) + two-stage `esc`

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] add `case "n"` / `case "N"` to the non-searching switch: no-op when `searchQuery==""`; else step through `searchMatches` with wrap-around (and the off-match "first at/after cursor" rule), move `svcCursor`, `fixSvcOffset()`
- [x] add the two-stage guard at the top of the existing `case "esc"`: when `searchQuery != ""`, `clearSearch()` + `fixSvcOffset()` + return (consume); otherwise fall through to existing back-nav
- [x] write tests (on-match): after commit, `n` advances to next match and wraps from last→first; `N` goes previous and wraps first→last; counter position (index within `searchMatches`) is correct; `n` no-op when no query
- [x] write tests (off-match — the off-by-one-prone branch): after commit then a manual `j`/`k` that lands the cursor *between* two matches, `n` jumps to the first match strictly **after** the cursor and `N` to the first strictly **before**; cursor above all matches → `n` first match, `N` wraps to last; cursor below all matches → `n` wraps to first, `N` last
- [x] write test: two-stage `esc` — first `esc` clears search and keeps screen (`searchQuery==""`, still `screenSelectContainers`), second `esc` navigates back to project picker
- [x] run `go test ./internal/tui/ -v` — must pass before next task

### Task 5: Rendering — row highlight, bottom bar, footer, visible-count

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] in the container render loop, apply `searchMatchStyle` to matching rows' names and `searchCurrentStyle` to the current match (`i==svcCursor`)
- [x] render the bottom bar above the help footer: typing → `/ ` + input view + `(i/N)`/`(no match)`; committed → `↳ <name> (i/N)` + `n next · N prev · esc clear`; idle → reserved blank line; while `confirming`, the confirm prompt occupies the shared reserved line (takes precedence over the bar)
- [x] add `/ search` to the container-screen help footer; show `enter jump · esc cancel` while searching and `n/N cycle · esc clear` when committed
- [x] reserve the bar line in `svcVisibleCount()` in **both** the `confirming` early-return branch (`app.go:2237`) and the normal branch, so list height is constant across search-idle / searching / confirming
- [x] write tests on `View()` output: committed search shows the counter text and the matched name is styled (contains ANSI escape / differs from plain); idle screen still renders (reserved line present)
- [x] write test: a highlighted (matched) row and a non-highlighted row in the same frame produce **equal-width** name cells (styling wraps only the raw name; `nameWidth`/`pad` alignment preserved — cross-check the `utf8.RuneCountInString(svc)` width math at `app.go:2782`)
- [x] write test: `svcVisibleCount()` returns the same reduced count with a fixed height whether or not `m.confirming` (height constant when a search is active and confirmation toggles)
- [x] run `go test ./internal/tui/ -v` — must pass before next task

### Task 6: Cleanup wiring — clear search on every departure/reload

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [x] call `m.clearSearch()` at all 9 "leave the screen / reload the list" sites: `servicesMsg`, `esc` container→project, `esc` project→server, `entryLocal`, `connectResultMsg` error, `enterLogs()`, `enterConfig()`, `enterProgress()`, `enterExec()`
- [x] confirm the 6 return-to-container paths (`esc` from logs / config / progress, `execDoneMsg`, plus init and pre-select) get **no** `clearSearch()` — departure already cleared it (verified by inspection, no code change)
- [x] write tests: with an active committed search, a reload/departure leaves `searchQuery==""`/`searchMatches==nil`/`searching==false` — cover an invalid-index reload (`servicesMsg`), a context switch (`esc`→project), a read-only departure (`enterLogs`), and an operation start (`enterProgress`)
- [x] write test: entering logs then `esc` back returns to `screenSelectContainers` with search already clear (no stale highlight)
- [x] write regression test: `selectedContainers()` returns the same result with and without an active search (multi-select integrity — search never touches `m.selected`)
- [x] run `go test ./internal/tui/ -v` — must pass before next task

### Task 7: Verify acceptance criteria
- [x] verify all Overview requirements: `/` opens search, live jump to first match, `n`/`N` cycle with wrap + counter, matches highlighted (current brightest), two-stage esc, list/selection untouched — confirmed by inspection (app.go: `/` open 1385, live-jump 1278-1279, cycle+wrap `cycleMatch` 2314, highlight `searchMatchStyle`/`searchCurrentStyle` 3010-3015, two-stage esc 1295-1296, selection untouched `TestSelectedContainersUnaffectedBySearch`) and passing tests
- [x] verify edge cases: `/` on empty list is a no-op; empty query; no-match query; single match; match off-screen scrolls into view; cursor off-match `n`/`N`; `q` typeable in query; list height constant across searching / confirming-with-search; search cleared on every departure and reload — all covered; added `TestSearchLiveJumpScrollsOffScreenMatchIntoView` + `TestSearchCycleScrollsOffScreenMatchIntoView` to fill the previously-uncovered off-screen-scroll case (all other cases already had tests)
- [x] run full suite uncached: `go test ./... -count=1` — all 6 packages pass
- [x] run `go build -o cdeploy .` — OK
- [x] `go mod tidy` (no new deps expected — `textinput` already present) and confirm clean — no churn (`git diff --exit-code go.mod go.sum` clean)

### Task 8: Update documentation
- [x] add a `**Container search (search & jump)**:` architecture note to `CLAUDE.md` documenting: search-not-filter (selection untouched), the `searching`/`searchQuery` two-mode model, `computeMatches`, the **ephemeral-on-departure** cleanup invariant (`clearSearch()` on every leave/reload of `screenSelectContainers`; return paths need none), `n`/`N` wrap + counter, two-stage esc, `/`-on-empty-list guard, `searchInput` (re)constructed in the `/` handler, reserved bar line (unconditional in `svcVisibleCount`), and the `q`-rewrite exception — added after the "Service list scrolling" note
- [x] update the `README.md` keybindings/usage if it lists container-screen keys — README documents container-screen keys in the "Service select" list (screen 3); added `/` (search) and `n`/`N` (cycle matches)
- [x] move this plan to `docs/plans/completed/` (deferred to exec finalize step — review phases still read it)

## Post-Completion
*Manual, outside the automated test loop — informational only.*

**Manual verification (real TTY):**
- Launch `./cdeploy` against a project with many services; press `/`, type a substring, confirm the cursor live-jumps and matches highlight.
- Confirm `n`/`N` cycle with wrap and the `(i/N)` counter tracks correctly; confirm a match below the fold scrolls into view.
- Confirm two-stage `esc` (first clears highlight, second leaves the screen) and that a service name containing `q` is typeable in the search box.
- Confirm multi-select still works: select a few services, run a deploy/restart, and confirm search never altered the selection.

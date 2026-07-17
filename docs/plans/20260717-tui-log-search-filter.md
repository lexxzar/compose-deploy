# TUI Log View: Search + Filter

## Overview

Add two coordinated navigation features to the log viewer (`screenLogs`):

- **Filter (`f`)** — a live grep that hides non-matching lines from the streaming buffer. New matching lines keep arriving; clearing the filter instantly reveals everything, including lines that streamed in while it was active.
- **Search (`/`)** — highlight-and-jump *within* the (possibly filtered) survivors, mirroring the existing container-screen search-and-jump: highlight matches, `n`/`N` cycle with wrap-around, live-jump while typing.

The two coexist (lnav/less model): the filter narrows the buffer, search highlights within it. Both use "smart" matching — case-insensitive substring by default, `ctrl+r` inside the input toggles to Go `regexp` (RE2). The filter additionally supports a leading `!` to negate/exclude (RE2 has no negative lookahead).

**This is a completed-brainstorm design — all decisions below are locked.** TUI-only; no CLI/runner/compose changes.

## Context (from discovery)

- **Files involved:** `internal/tui/app.go` (Model + `screenLogs` handler + `viewLogs` + streaming pipeline), `internal/tui/format.go` (`formatLogLines`), `internal/tui/styles.go` (add match styles). New: `internal/tui/logfilter.go` (+ test).
- **Reference implementation to mirror:** the container-screen search-and-jump, shipped in `docs/plans/completed/20260711-tui-container-search.md`. Concrete seams to copy: `clearSearch()` (app.go:2335), `cycleMatch()` (2356), `computeMatches()` (2433), `searchBarLine()` (3202), `clampToWidth()` (3312, wraps `ansi.Truncate`), and the regression pin `TestSearchBarLineNeverWraps` (`footer_reservation_test.go:134`).
- **Current log pipeline:** `logChunkMsg` appends raw bytes to `m.logsContent` (flat string); `applyLogFormat()` (app.go:2046) formats incrementally via `m.logsRawOff`; `fullReformat()` (2078) reprocesses on `w`/`p`/resize. Follow-awareness is `m.logsViewport.AtBottom()` — no pause field (see `20260713-tui-log-pause.md`).
- **Height math today:** viewport height is `m.height - 6` in **both** `enterLogs` (app.go:2012) and the `WindowSizeMsg` `screenLogs` branch (app.go:~561). Width is `m.width - 4`.
- **Imports:** `regexp` is **not** yet imported in `app.go` — must add. `strings`, `unicode/utf8`, and `github.com/charmbracelet/x/ansi` already are.
- **Styles present:** `searchMatchStyle`/`searchCurrentStyle` (yellow / yellow-bold, container search), `logFollowStyle`/`logPauseStyle` (log header). Reuse the yellow palette for log search styles.

## Development Approach

- **Testing approach:** Regular (implementation first, then tests within the same task), matching the repo's existing TUI test discipline. Tests use stdlib `testing` only — no testify, no TTY (drive `Update()` with `tea.KeyMsg`).
- Complete each task fully before moving to the next; small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** — success and error/edge scenarios both.
- **CRITICAL: all tests must pass before starting the next task.**
- Run `go build ./... && go test ./internal/tui/ -count=1` after each task.
- Maintain backward compatibility: with no filter and no search, the **derived viewport content** (`logsFormatted`) must be identical to today across the four `w`×`p` combinations (pinned by regression tests in Task 2). Scope the assertion to the derived content, **not** full `View()` — Task 6 intentionally adds the reserved bar line and shrinks the viewport by one row, which changes `View()` by design.

## Testing Strategy

- **Unit tests:** required every task. Pure helpers (`buildMatcher`, `deriveFiltered`, `logComputeMatches`, `logBarLine`) are table-tested in isolation; state transitions are driven through `Update()` with synthetic `tea.KeyMsg`.
- **e2e tests:** none — this project has no UI e2e harness (the root TUI needs a TTY and is not exercised end-to-end). Manual TUI verification is listed under Post-Completion.
- **Invariant pins:** `TestLogBarLineNeverWraps` (one-physical-line invariant, mirrors `TestSearchBarLineNeverWraps`); "filter operates on RAW not pretty-expanded lines"; "clearing filter reveals lines that streamed in while filtered".

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Keep this file in sync if scope shifts during implementation.

## Solution Overview

**Architecture (approved):** replace the flat `logsContent` string as the source of truth with a **raw logical-line buffer** `logsRawLines []string` plus a trailing partial `logsPartial string`. The viewport content becomes a pure derivation:

```
logsRawLines ──filter(pred)──▶ survivors ──prettyExpand+wrap──▶ physical lines ──▶ viewport
                                                     │
                                       search highlights + n/N indices (physical lines)
```

Key invariants:

1. **Raw buffer is always complete/unfiltered.** The docker stream keeps buffering in the background regardless of an active filter. Clearing the filter re-derives from the full buffer.
2. **Steady state stays incremental — O(new lines).** Per new complete raw line: test the filter predicate; if it passes, format and append to the viewport content. A full re-derivation (analogous to today's `fullReformat`) runs **only** on filter-query change, `w`/`p` toggle, or resize — never per 4 KB chunk.
3. **Filter runs at the raw-line level, before pretty-expansion** — a matched `svc | {json}` line pretty-prints as a whole block. Match against the whole raw line (the single-service `svc |` prefix is constant).
4. **Follow-awareness unchanged** — `logsViewport.AtBottom()` now means "pinned to the filtered tail". Capture `AtBottom()` **before** every re-derive and `GotoBottom()` after if it was true (same trick the `w`/`p` toggles already use).
5. **Synchronous, no new goroutine/session** — filter and search are pure like the container search. Existing `logsSession` + `m.screen == screenLogs` guards suffice; no new session counter.

**Match semantics:** `buildMatcher(query, isRegex, allowNegate)` returns `(pred func(string) bool, re *regexp.Regexp, valid bool)`. Regex compiled once (not per line). On invalid regex mid-type, the caller keeps the last-good predicate and shows `(bad regex)`. Negation (`allowNegate` true, filter only) strips a leading `!` and inverts the predicate.

## Technical Details

**New Model fields** (`internal/tui/app.go`, near the existing `logs*` block ~256):

```go
// Screen: logs — raw-line buffer (source of truth for derivation)
logsRawLines []string // complete logical lines, unfiltered
logsPartial  string   // trailing incomplete line (no newline yet)
logsScanned  int      // count of logsRawLines ALREADY folded into logsFormatted — a RAW-line
                      //   scan cursor (resume point), NOT a survivor count

// Screen: logs — filter (grep)
logFiltering   bool
logFilterInput textinput.Model // built lazily on 'f'
logFilterQuery string
logFilterIsRegex bool
logFilterRe    *regexp.Regexp  // compiled matcher when regex+valid; nil otherwise

// Screen: logs — search-within-highlight
logSearching   bool
logSearchInput textinput.Model // built lazily on '/'
logSearchQuery string
logSearchIsRegex bool
logSearchRe    *regexp.Regexp
logSearchMatches []int         // PHYSICAL line indices into rendered content
logSearchCur   int             // index into logSearchMatches
```

**New pure helpers** (`internal/tui/logfilter.go`):

- `buildMatcher(query string, isRegex, allowNegate bool) (pred func(string) bool, re *regexp.Regexp, valid bool)`
- `deriveFiltered(raw []string, pred func(string) bool) []string` — `pred == nil` passes all.
- `logComputeMatches(physical []string, pred func(string) bool) []int` — indices of matching physical lines, ascending (mirrors `computeMatches` but over physical lines and pred-driven).
- `highlightMatches(physical []string, matches []int, cur int) []string` — wraps matched spans with `logSearchMatchStyle` (current line with `logSearchCurrentStyle`); styling wraps only the matched substring so `ansi.StringWidth` is unaffected.

**Incremental-cursor semantics (do not skip — this is the Task 2 correctness trap).** The resume cursor `logsScanned` counts **raw lines already processed**, *not* survivors folded. With a filter active, survivor-count ≠ raw-lines-scanned: if L0(pass), L1(fail), L2(pass) arrive, a survivor cursor would advance to 1 after L0 and then re-scan the already-filtered L1, duplicating output. The steady-state loop is: for each `logsRawLines[logsScanned:]`, test the predicate; if it passes, append its formatted physical lines to `logsFormatted`; always advance `logsScanned`. A full re-derivation (`w`/`p` toggle, filter change, resize) resets `logsScanned = 0`, `logsFormatted = ""`, and re-scans from the top.

**`logsPartial` rendering.** The trailing incomplete line must still render (today's `applyLogFormat` does this). Append `logsPartial` **unfiltered** to the derived content on the way to `SetContent` (simplest correct choice — an in-flight line has no newline yet and shouldn't vanish behind a filter). It is *not* counted by `logsScanned` and is re-evaluated each frame.

**Highlight is a SetContent-time pass, not part of the cache.** `logsFormatted` stays **unstyled** physical text; `highlightMatches` is applied only when building the string handed to `logsViewport.SetContent`. Note the cost: while a committed search is active during live streaming, each chunk forces a full re-highlight of all physical lines (bounded by streamed content — acceptable, but the incremental-append win is lost for the highlight step specifically).

**Physical-slice handoff.** `formatLogLines` returns a joined `string` (format.go:42). The search stage (`logComputeMatches`/`highlightMatches`) consumes `[]string` of physical lines — split the formatted output on `\n` (or add a slice-returning variant) before the search stage.

**Bottom bar precedence** (`logBarLine() string`, always exactly one physical line, clamped via `clampToWidth`):

```
typing search  → "/ <q> [rx] (2/7)"          (or "(no match)")
typing filter  → "f <q> [rx] · 12/241 shown"  (or "(bad regex)")
committed      → "filter: !x [rx] · 12/241 · search: y (2/7)"
idle           → "" (blank, but the line is still reserved)
```

**Layered `esc`** (peel inner → outer): typing-search → typing-filter → committed-search → committed-filter → leave screen. **`q`** rewrites to `esc` on this screen **except** while an input is open (types literally), matching the container-search / settings-color-picker exceptions.

**Layout:** viewport height becomes `m.height - 7` (one line for the reserved bar) in **both** `enterLogs` and the `WindowSizeMsg` branch. Follow indicator stays on the title line (no new line there).

## What Goes Where

- **Implementation Steps** (checkboxes): all code, tests, and the CLAUDE.md paragraph — achievable in-repo.
- **Post-Completion** (no checkboxes): interactive TUI smoke test (needs a TTY), which cannot be automated here.

## Implementation Steps

### Task 1: Pure matching + derivation helpers

**Files:**
- Create: `internal/tui/logfilter.go`
- Create: `internal/tui/logfilter_test.go`

- [x] add `buildMatcher(query, isRegex, allowNegate)` — case-insensitive substring by default; compile `regexp` once when `isRegex`; strip leading `!` and invert when `allowNegate`; return `valid=false` (and `pred=nil`, `re=nil`) on empty query or bad regex
- [x] add `deriveFiltered(raw []string, pred func(string) bool) []string` (nil pred ⇒ return `raw` as-is)
- [x] add `logComputeMatches(physical []string, pred func(string) bool) []int` (ascending physical-line indices; nil/`valid=false` pred ⇒ `nil`)
- [x] write tests for `buildMatcher`: literal match, case-insensitivity, regex `ERROR|WARN`, bad-regex ⇒ `valid=false`, negation `!healthcheck`, empty query
- [x] write tests for `deriveFiltered` (nil pred pass-through, subset selection) and `logComputeMatches` (order, empty, no-match)
- [x] run tests — must pass before Task 2

### Task 2: Raw-line buffer + derivation pipeline (no user-facing filter yet)

**Files:**
- Modify: `internal/tui/app.go` (Model fields; `logChunkMsg` handler @886; **`logDoneMsg` handler @898-909**; `enterLogs` @2000; `applyLogFormat`/`fullReformat` @2046; `esc` cleanup @1473)
- Modify: `internal/tui/app_test.go`

- [x] add `logsRawLines`, `logsPartial`, `logsScanned` Model fields; add `"regexp"` import _(regexp import deferred to Task 3 — Go forbids unused imports and nothing in Task 2 references regexp; it lands with its first user in Task 3)_
- [x] rewrite the `logChunkMsg` handler to split incoming bytes into complete raw lines (append to `logsRawLines`) + trailing `logsPartial`, capturing `AtBottom()` before and `GotoBottom()` after when following (preserve today's behavior)
- [x] **rewire the `logDoneMsg` handler (@898-909):** it currently appends the terminal error to `logsContent` and calls `applyLogFormat()`. Instead, flush `logsPartial` and append the `Error: %v` text as a raw line into `logsRawLines` — **filter-exempt** (a terminal error must always render regardless of an active filter); preserve the force-`GotoBottom()` so the error scrolls into view _(exemption realized via a dedicated `logsErrLine` slot appended outside the predicate path, rather than mixing it into the filterable `logsRawLines`)_
- [x] rewrite `applyLogFormat`/`fullReformat` to derive viewport content from `logsRawLines` via `deriveFiltered(nil-pred)` → `formatLogLines` (wrap/pretty), using `logsScanned` as the **raw-line** resume cursor (see "Incremental-cursor semantics" above — advance per raw line scanned, NOT per survivor); append `logsPartial` unfiltered on the way to `SetContent`; reset the raw-buffer fields in `enterLogs` and the `esc`-to-containers cleanup
- [x] write a regression test: with no filter, the derived content (`logsFormatted`, **not** full `View()`) is identical to the pre-refactor output across the four `w`×`p` combinations; the fixture must include a **no-trailing-newline** payload (partial-line parity) and a JSON line (pretty-print parity)
- [x] write tests for chunk-boundary correctness (a raw line split across two `logChunkMsg` chunks folds into exactly one logical line), the `logDoneMsg` error line rendering even with a (later-added) filter active, and incremental append (`logsScanned` advances correctly when lines are filtered out — pin against the survivor-cursor duplication trap)
- [x] run tests — must pass before Task 3

### Task 3: Filter — fields, `f` key, live narrowing, `ctrl+r`, commit/cancel

**Files:**
- Modify: `internal/tui/app.go` (filter Model fields; **typing intercept + `q`-exception**; `screenLogs` handler @1469; `q`→`esc` rewrite block @1055-1088; add `clearLogFilter()`)
- Modify: `internal/tui/styles.go` (mode/label styles if needed)
- Modify: `internal/tui/app_test.go`

- [x] add filter Model fields; build `logFilterInput` lazily inside the `f` handler (so `Model{}` test literals stay valid); early-return `f` when the raw buffer is empty
- [x] **insert the typing intercept as the FIRST statement in `case screenLogs` (@1469), before the `ctrl+c`/`esc`/`w`/`p`/`G`/`default` cases:** while `m.logFiltering` (Task 4 extends to `|| m.logSearching`), route the key to the active input and `return` — otherwise typing `w`/`p`/`G`/`warn`/`log` would fire wrap/pretty/gotobottom mid-query. Mirrors the container `if m.searching` intercept (@1264)
- [x] **add the `screenLogs` sub-case to the `q`→`esc` rewrite block (@1055-1088) NOW, not in Task 5:** while `m.logFiltering` (Task 4 adds `|| m.logSearching`) leave `q` untouched so it types literally; else rewrite `q`→`esc`. Without this, the upstream rewrite (@1085 `default:`) turns every `q` into `esc` before the screen switch, so Task 3 could not test a query containing `q` and its "all tests pass" gate would be unmet
- [x] wire typing mode: each keystroke rebuilds the predicate via `buildMatcher(..., allowNegate=true)`, re-derives live (capture `AtBottom()` → `GotoBottom()` if following); on bad regex keep the last-good `logFilterRe`/predicate; `ctrl+r` flips `logFilterIsRegex`; `enter` commits (bar closes, filter stays); `esc` cancels (clears query, restores full view). NOTE: this `esc` handling is **provisional** — Task 5 folds it into the 5-rung ladder
- [x] add `clearLogFilter()` (resets all filter fields, `SetValue("")`/`Blur()`), called from `enterLogs` init and the `esc`-to-containers cleanup
- [x] write tests: `f` opens the input; typing `q`/`w`/`p` lands in the input (not an action); live narrowing as characters are typed; `ctrl+r` toggle; bad regex keeps last-good result (no thrash); `enter` commit keeps filter; `esc` cancel restores full view; `!`-negation excludes matching lines
- [x] write regression tests: **filter matches RAW lines, not pretty-expanded** (a JSON line whose body-only would fail still shows when the raw line matches); **clearing the filter reveals lines that streamed in while filtered**
- [x] run tests — must pass before Task 4

### Task 4: Search — fields, `/` key, highlight, `n`/`N`, coexistence

**Files:**
- Modify: `internal/tui/app.go` (search Model fields; `screenLogs` handler; highlight in the content-build path; add `clearLogSearch()`)
- Modify: `internal/tui/styles.go` (add `logSearchMatchStyle`, `logSearchCurrentStyle`)
- Modify: `internal/tui/app_test.go`

- [x] add `logSearchMatchStyle` (yellow) and `logSearchCurrentStyle` (yellow + bold) to styles.go
- [x] add search Model fields; build `logSearchInput` lazily on `/`; early-return `/` when the rendered buffer is empty
- [x] **extend the Task 3 typing intercept and `q`-exception to `|| m.logSearching`** so `/`-mode keystrokes route to the search input and `q`/`n`/`N` type literally while searching
- [x] wire typing mode: split the formatted content on `\n` to get physical lines, recompute `logSearchMatches` (physical indices via `logComputeMatches` over the filtered physical lines) on each keystroke, live-jump to first match; `n`/`N` cycle with wrap-around (mirror `cycleMatch`); `ctrl+r` toggles `logSearchIsRegex`; bar shows `(i/N)` / `(no match)`; apply `highlightMatches` as a **SetContent-time pass** (keep `logsFormatted` unstyled) _(routed through the single `setLogViewportContent()` chokepoint; `logSearchCounter()` helper returns the `(i/N)`/`(no match)` string for Task 6's bar)_
- [x] recompute matches whenever the filter changes or new matching lines stream in (append-only in steady state; full recompute on filter/toggle/resize); jumping scrolls up and auto-pauses follow (existing behavior), `G` re-resumes. NOTE: `esc` handling here is **provisional** pending Task 5 _(match recompute rides `applyLogFormat`/`rederiveLogs`/`fullReformat` via `setLogViewportContent`; live-jump/cycle via `scrollLogMatchIntoView`)_
- [x] add `clearLogSearch()` (resets all search fields), called from `enterLogs` init and the `esc`-to-containers cleanup
- [x] write tests: `/` opens; highlight applied to matches (assert style bytes present, `ansi.StringWidth` unchanged); `n`/`N` wrap-around; `ctrl+r` toggle; matches recompute after a filter change; search operates over filtered survivors (a line hidden by the filter is not a search match); **per-physical-line match semantics** — two occurrences on two wrapped rows both highlight/count, but an occurrence split *across* a soft-wrap boundary is not matched (accepted limitation, pinned so it's intentional)
- [x] run tests — must pass before Task 5

### Task 5: Consolidate the layered `esc` ladder

**Files:**
- Modify: `internal/tui/app.go` (`screenLogs` `esc` case — replace the provisional Task 3/4 handling)
- Modify: `internal/tui/app_test.go`

- [x] replace the provisional filter-cancel / search-cancel `esc` from Tasks 3–4 with the single 5-step precedence in the `screenLogs` handler: (1) typing-search cancel, (2) typing-filter cancel, (3) committed-search clear-only (stay), (4) committed-filter clear-only (stay), (5) existing leave-screen (cancel ctx, `clearLogFilter()`/`clearLogSearch()`, back to `screenSelectContainers`)
- [x] verify the `q`-exception already added in Tasks 3–4 (`while logFiltering || logSearching`, `q` types literally) is consistent with the finalized ladder — no change expected, just confirm
- [x] write tests for each rung of the ladder (search-typing, filter-typing, committed-search, committed-filter, and the final leave-screen), asserting screen + field state after each `esc`; include the peel-order case: committed filter **and** search both active → first `esc` clears search only, second clears filter only, third leaves the screen
- [x] write tests: `q` types literally into an open filter/search input; `q` acts as back/`esc` when no input is open
- [x] run tests — must pass before Task 6

### Task 6: Bottom bar + layout height + never-wrap invariant

**Files:**
- Modify: `internal/tui/app.go` (`viewLogs` ~3355; `enterLogs` height ~2012; `WindowSizeMsg` `screenLogs` branch ~561; add `logBarLine()`)
- Create: `internal/tui/log_bar_test.go`
- Modify: `internal/tui/app_test.go`

- [ ] add `logBarLine() string` with the content precedence (typing-search / typing-filter / committed / blank), always exactly one physical line, final `clampToWidth(line, m.width)`
- [ ] render the reserved bar above the help footer in `viewLogs`; extend the help footer with `/ search` and `f filter` tokens (and contextual `enter commit · esc cancel` / `n/N cycle` hints while active); keep the follow indicator on the title line
- [ ] change viewport height `- 6` → `- 7` at EXACTLY the two log sites: `enterLogs` `m.height - 6` (@2012) and the `WindowSizeMsg` `screenLogs` branch `msg.Height - 6` (@561). **Do NOT touch** the two config sites — `enterConfig` `m.height - 6` (@1920) and the `WindowSizeMsg` config branch `msg.Height - 6` (@541) — so a blind find/replace of `- 6` is wrong; edit by matching the surrounding `logsViewport` context
- [ ] render the zero-match placeholder `(no lines match filter)` in the viewport when the filter yields no survivors
- [ ] write `TestLogBarLineNeverWraps` (assert `ansi.StringWidth(logBarLine()) <= m.width` at widths 40 and 20 with a long query, long service name, and dual committed summary)
- [ ] write tests: idle bar is blank but the line is reserved (viewport height accounting drops by exactly 1 vs a no-bar baseline); typing/committed bar content; zero-match placeholder render
- [ ] run tests — must pass before Task 7

### Task 7: Verify acceptance criteria

- [ ] verify all Overview requirements are implemented (filter live-narrow, search highlight/jump, coexistence, smart matching + `ctrl+r`, `!`-negation, layered `esc`, reserved bar)
- [ ] verify edge cases: zero matches, stream-ended (`logsDone`) still searchable/filterable, terminal-error line renders even under an active filter, per-physical-line search semantics (occurrence fully within a row highlights; occurrence split by a soft-wrap boundary is not matched — accepted), clearing filter reveals lines streamed while filtered
- [ ] run `gofmt -l internal/tui` (expect no output) and `go vet ./...`
- [ ] run the full suite: `go build -o cdeploy . && go test ./... -count=1`
- [ ] confirm no CLI/runner/compose files changed (`git diff --stat` scoped to `internal/tui/` + docs only)

### Task 8: Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/plans/20260717-tui-log-search-filter.md` (move to completed)

- [ ] add a "Log search & filter" paragraph to CLAUDE.md's log-view section, mirroring the container-search paragraph: keys (`/` search, `f` filter), smart matching + `ctrl+r`, `!`-negation, the raw-line derivation pipeline, the layered `esc`, the reserved one-line bar + never-wrap invariant, and the "filter on raw lines before pretty-expand" rule
- [ ] update CLAUDE.md if any new pattern emerged during implementation
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention — no checkboxes, informational only.*

**Manual verification** (needs a real TTY; cannot be automated here):
- Run `./cdeploy`, open logs for a live, chatty service. Confirm: `f error` narrows live and keeps streaming; `ctrl+r` switches to `ERROR|WARN`; `!healthcheck` excludes; `/` highlights within the filtered view and `n`/`N` cycle; scrolling up shows `⏸ paused`, `G` resumes following the filtered tail; the `esc` ladder peels search → filter → back; the bottom bar never wraps at a narrow terminal width.
- Sanity-check against a service emitting JSON logs with `p` (pretty) on, to confirm filter matches raw lines while pretty-print still expands whole blocks.

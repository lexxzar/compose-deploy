# TUI Log View — Auto-Pause on Scroll

## Overview
Add an implicit pause capability to the container log view (`screenLogs`, opened via `l`). Today every incoming `logChunkMsg` calls `m.logsViewport.GotoBottom()` unconditionally, so the view yanks the user to the bottom even when they have scrolled up to read — making a busy stream effectively unreadable.

This change makes the tail *follow-aware*: scrolling up pauses auto-scroll (you can read/scroll freely while the stream keeps flowing underneath), and scrolling back to the bottom resumes following. A small header indicator shows `● following` when live or `⏸ paused ▲ N below` when scrolled up, where **N** is the distance (in display rows) to the live bottom. There is **no new key** — the existing `G` (GotoBottom) naturally becomes "jump to live **and** resume follow".

**Key benefit:** fixes the "logs jump to bottom while I'm reading" annoyance, which is exactly what a pause is for, with a minimal, stateless implementation.

## Context (from discovery)
- **Primary file:** `internal/tui/app.go` (Bubble Tea model — behavior *and* view live here).
- **Styles:** `internal/tui/styles.go`.
- **Tests:** `internal/tui/app_test.go` (Update-driven, no TTY; stdlib `testing` only — no testify).
- **Relevant existing code:**
  - `logChunkMsg` handler (~`app.go:882`) — appends chunk, `applyLogFormat()`, unconditional `GotoBottom()`, re-issues `readLogChunk()`.
  - `logDoneMsg` handler (~`app.go:891`) — sets `logsDone`, on error appends error text + `GotoBottom()`.
  - `WindowSizeMsg` resize block (~`app.go:558-565`) — sets viewport `Width`/`Height`, calls `fullReformat()`.
  - `w` wrap toggle (~`app.go:1497`), `p` pretty toggle (~`app.go:1506`), `G` bottom (~`app.go:1510`) in the `screenLogs` key handler.
  - `viewLogs()` (~`app.go:3320`) — renders title `%s > logs > %s`, viewport, help footer.
- **Verified upstream behavior** (`github.com/charmbracelet/bubbles v1.0.0`, `viewport/viewport.go`):
  - `SetContent` (line 130) preserves `YOffset`, snapping only if `YOffset > len(lines)-1` — impossible while appending (line count only grows). So a paused/scrolled-up view stays put on every chunk.
  - `AtBottom()` == `YOffset >= maxYOffset()`; `TotalLineCount()`, `GotoBottom()`, and exported `YOffset`/`Height` fields all exist.
- **Style palette:** yellow = `lipgloss.Color("3")` (used by `updateGlyphStyle`, `searchMatchStyle`); green = `lipgloss.Color("2")`.

## Development Approach
- **Testing approach:** Regular (implement, then add/update tests within the same task).
- Complete each task fully before moving to the next; small, focused changes.
- **CRITICAL: every task includes new/updated tests** covering both success and edge cases; all tests must pass before starting the next task.
- Run `go test ./internal/tui/ -v` after each change; run `go build -o cdeploy .` and `go test ./...` at the end.
- Maintain backward compatibility — no CLI/runner/compose changes; no behavior change on any other screen.

## Testing Strategy
- **Unit tests:** required for every task (see above). All Update-driven via `Model.Update(tea.KeyMsg{...})` / message structs; no TTY.
- **E2E tests:** none — this project has no UI e2e harness; the TUI is exercised purely through `Update()` in `internal/tui/app_test.go`.
- Content must exceed viewport height for `AtBottom()`/scroll to be meaningful — tests set `m.height` and feed enough lines so `TotalLineCount() > Height`.

## Progress Tracking
- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; document blockers with ⚠️ prefix.
- Keep this plan in sync with actual work; update if scope changes.

## Solution Overview
**Headline: ZERO new Model fields.** The viewport's `YOffset` *is* the pause state; "following" ≡ `m.logsViewport.AtBottom()`, recomputed wherever needed.

1. **Conditional snap:** in `logChunkMsg`, capture `following := m.logsViewport.AtBottom()` *before* appending, then only `GotoBottom()` if `following`. Scroll up → not at bottom → chunks stop snapping (SetContent preserves offset). Scroll to bottom → at bottom → next chunk resumes the tail.
2. **Preserve follow-intent across reformat:** `w`/`p` toggles and resize call `fullReformat()`, which changes line counts and can drop a *live* tail off the bottom. Once snapping is conditional, that would read as an accidental pause. Each of the 3 sites captures `following` before the reformat and re-`GotoBottom()`s if it was following.
3. **Indicator:** a pure helper `logTailStatus(vp, done)` returns `("following", 0)`, `("paused", N)`, or `("", 0)` when the stream has ended. `viewLogs()` renders it on the header line (green / yellow); a new style pair lives in `styles.go`.
4. **`logDoneMsg`:** unchanged — the existing force-`GotoBottom()` on error is deliberate (a terminal error should be forced into view).

### Design decisions & rationale
- **Auto-pause on scroll (implicit), not an explicit pause key** — chosen approach ("Option C"). Zero new key bindings; the viewport scroll position carries all state.
- **Distance-to-bottom count** ("Option A") rather than a "new since pause" counter — computed live from viewport geometry (`TotalLineCount - YOffset - Height`), so it needs no Model field, cannot drift, and stays correct through resize / wrap / pretty reformats. It answers "how far `G` will jump."
- **Indicator only while streaming** — on `logsDone` the following/paused indicator disappears (nothing left to follow).

## Technical Details
- **Conditional snap (logChunkMsg):**
  ```go
  case logChunkMsg:
      if m.screen != screenLogs || msg.session != m.logsSession {
          return m, nil
      }
      following := m.logsViewport.AtBottom() // capture BEFORE appending
      m.logsContent += string(msg.data)
      m.applyLogFormat()                     // SetContent preserves YOffset
      if following {
          m.logsViewport.GotoBottom()
      }
      return m, m.readLogChunk()
  ```
- **Follow-intent preservation (each of `w`, `p`, resize):**
  ```go
  following := m.logsViewport.AtBottom()
  // ... flip the flag / set Width+Height ...
  m.fullReformat()
  if following {
      m.logsViewport.GotoBottom()
  }
  ```
- **Indicator helper (pure, package-level in `app.go`):**
  ```go
  func logTailStatus(vp viewport.Model, done bool) (label string, below int) {
      if done {
          return "", 0
      }
      if vp.AtBottom() {
          return "following", 0
      }
      below = vp.TotalLineCount() - vp.YOffset - vp.Height
      if below < 0 {
          below = 0
      }
      return "paused", below
  }
  ```
- **Styles (`styles.go`):** `logFollowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))` (green), `logPauseStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))` (yellow, same as `updateGlyphStyle`).
- **`viewLogs()` rendering:** after the title, when `label != ""`, append `  ` + styled segment: `● following` (green) or `⏸ paused ▲ N below` (yellow). Rendered on the same header line as the breadcrumb/title; no extra viewport-height math needed.

## What Goes Where
- **Implementation Steps** (`[ ]`): all code + tests below — self-contained in `internal/tui`.
- **Post-Completion** (no checkboxes): manual TTY smoke test of the live pause/resume feel against a real streaming container.

## Implementation Steps

### Task 1: Conditional snap-to-bottom in the chunk handler

**Files:**
- Modify: `internal/tui/app.go` (`logChunkMsg` handler, ~line 882)
- Modify: `internal/tui/app_test.go`

- [x] In the `logChunkMsg` handler, capture `following := m.logsViewport.AtBottom()` **before** `m.logsContent += string(msg.data)`.
- [x] After `m.applyLogFormat()`, replace the unconditional `m.logsViewport.GotoBottom()` with `if following { m.logsViewport.GotoBottom() }`. Keep the top screen+session guard and the trailing `return m, m.readLogChunk()` unchanged.
- [x] Write test: build a `Model` in `screenLogs` with `m.height` set and enough content that `TotalLineCount() > Height`; scroll up (feed `tea.KeyMsg` "up" a few times, or `SetYOffset`), feed a `logChunkMsg`, assert `YOffset` unchanged and `!m.logsViewport.AtBottom()` (did NOT snap).
- [x] Write test: with the viewport at the bottom, feed a `logChunkMsg`, assert `m.logsViewport.AtBottom()` is still true (followed the tail).
- [x] Run `go test ./internal/tui/ -v` — must pass before Task 2.

> Note (backward compat): a fresh `viewport.New(w, h)` and any content shorter than the height report `AtBottom() == true`, so the first chunk still snaps and existing tests (`TestLogChunkMsg_AppendsContent`, `TestLogsScreen_LogChunkAppliesFormat`, which use 80×20 viewports with tiny content) stay green **with no edits**. Do not expect to touch them.

### Task 2: Preserve follow-intent across reformat (wrap / pretty / resize)

**Files:**
- Modify: `internal/tui/app.go` (`w` handler ~1497, `p` handler ~1506, `WindowSizeMsg` block ~558-565)
- Modify: `internal/tui/app_test.go`

- [x] `w` handler: capture `following := m.logsViewport.AtBottom()` before flipping `m.logsWrap`; after `m.fullReformat()`, add `if following { m.logsViewport.GotoBottom() }`.
- [x] `p` handler: same capture-before / re-snap-after around `m.fullReformat()`.
- [x] `WindowSizeMsg` `screenLogs` branch: capture `following` before setting `Width`/`Height`; after `m.fullReformat()`, add `if following { m.logsViewport.GotoBottom() }`.
- [x] Write test: viewport at bottom (following) → send a `w` (wrap toggle) `tea.KeyMsg`, then a `WindowSizeMsg` resize; assert `m.logsViewport.AtBottom()` remains true (did NOT accidentally pause).
- [x] Write test: viewport scrolled up (paused) → send `w` toggle + resize; assert it stays paused (`!AtBottom()`), i.e. re-snap only fires when previously following.
- [x] Run `go test ./internal/tui/ -v` — must pass before Task 3.

### Task 3: Follow/paused indicator (helper + styles + view)

**Files:**
- Modify: `internal/tui/app.go` (add `logTailStatus`, update `viewLogs()` ~3320)
- Modify: `internal/tui/styles.go` (add `logFollowStyle`, `logPauseStyle`)
- Modify: `internal/tui/app_test.go`

- [x] Add the pure `logTailStatus(vp viewport.Model, done bool) (label string, below int)` helper (per Technical Details).
- [x] Add `logFollowStyle` (green `Color("2")`) and `logPauseStyle` (yellow `Color("3")`) to `styles.go`.
- [x] In `viewLogs()`, call `logTailStatus(m.logsViewport, m.logsDone)` and, when `label != ""`, append the styled indicator (`● following` / `⏸ paused ▲ N below`) to the header line.
- [x] Write table test for `logTailStatus`: `{done:false, at bottom}` → `("following", 0)`; `{done:false, scrolled up N}` → `("paused", N)`; `{done:true}` → `("", 0)`.
- [x] Write test: `viewLogs()` output contains `following` when live at bottom, `paused` + a `▲` count when scrolled up, and neither token when `logsDone` is true. (The done-case must set `m.logsDone = true` on the Model before calling `viewLogs()` — that field is otherwise only set by the `logDoneMsg` handler.)
- [x] Run `go test ./internal/tui/ -v` — must pass before Task 4.

### Task 4: Verify acceptance criteria
- [ ] Verify all Overview requirements: scroll-up pauses tail, scroll-to-bottom resumes, `G` jumps-and-resumes, indicator reflects state, no indicator when done.
- [ ] Verify edge cases: reformat/resize while following keeps the tail pinned; while paused stays paused; `logDoneMsg` error path still forces the error into view (unchanged).
- [ ] Confirm zero new Model fields were added and no CLI/runner/compose/`esc`-cleanup/session code changed.
- [ ] Run full suite: `go build -o cdeploy .` and `go test ./... -count=1`.

### Task 5: Update documentation
- [ ] Update the **Log streaming** section of `CLAUDE.md` to note follow-aware auto-scroll: chunks only `GotoBottom()` when `AtBottom()`; the 3 reformat sites (`w`/`p`/resize) re-pin when following; `logTailStatus` drives the `following`/`paused ▲ N below` header indicator; `G` resumes follow; zero new Model fields.
- [ ] Update the `viewLogs`/footer note if the `G` help hint wording changes (optional).
- [ ] Move this plan to `docs/plans/completed/`.

## Post-Completion
*Items requiring manual intervention — no checkboxes, informational only.*

**Manual verification:**
- Smoke-test the live feel against a real streaming container (`l` on a busy service): scroll up mid-stream and confirm the view holds while `⏸ paused ▲ N below` climbs; scroll back to the bottom (or press `G`) and confirm `● following` returns and the tail resumes. Try during a `w`/`p` toggle and a terminal resize while following to confirm the tail stays pinned. Repeat once over a remote SSH server to confirm parity.

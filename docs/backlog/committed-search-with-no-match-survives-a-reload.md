---
worth: later
where: internal/tui/app.go:1406
added: 2026-11-16
---
# a committed search that matches nothing after a reload keeps its bar

Both `servicesMsg` branches re-derive `searchMatches` with `computeMatches` and never clear the search
(the rule in `CLAUDE.md`). When the new rows no longer match the committed query, the bar keeps
showing `search "<query>"  (no match)` with the `n next · N prev · esc clear` hints, and `n`/`N` do
nothing until the user presses `esc`.

That is consistent with the grouped reload and with a fold that hides every match, so it is not a
regression from PR #33. It only became reachable in the drilled view when PR #33 stopped the drilled
reload from calling `clearSearch()`.

**Unresolved:** is a query with no match after a reload a state worth keeping (the service may come
back on the next tick), or noise the reload should clear? Clearing it reverses the "re-derive, never
clear" rule for one case, so decide that before any code.

Raised in the review of PR #33.

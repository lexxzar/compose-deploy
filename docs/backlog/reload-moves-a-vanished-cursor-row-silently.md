---
worth: later
where: internal/tui/app.go:6956
added: 2026-11-16
---
# a reload moves the cursor off a vanished row with no notice

When a `servicesMsg` payload no longer carries the row under the cursor, `restoreCursorRow` finds no
match and `clampSvcCursor()` puts the cursor on a neighbour row. Nothing tells the user. The next `d`,
`r`, `s`, `R` or `x` then arms against that neighbour.

Both branches do this: the grouped one since the identity restore landed, the drilled one since PR #33
gave it the same restore. Before PR #33 the drilled reload reset the cursor to row 0, which was worse.
`TestDrilledReload_MissingCursorRowClamps` pins the clamp as intended.

The confirm prompt names its targets (`app.go:7934`), and a payload that arrives while `m.confirming`
is dropped, so the user sees the real target before anything runs.

**Unresolved:** is the confirm prompt enough, or does a vanished cursor row need a `m.warning` line
(and a footer row) of its own? Settle that before any code.

Raised in the review of PR #33.

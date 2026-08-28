---
worth: later
where: internal/tui/app.go:7426
added: 2026-08-28
---
# the container screen's warning line is the one single-line write that is not clamped

`viewSelectContainers` emits the warning as `b.WriteString("\n  " + warningStyle.Render(m.warning))`.
`warningStyle` sets a foreground colour and nothing else — no `Width` — so the string leaves the renderer at
its full length and the TERMINAL wraps it. Every other single-line write on this screen already goes through
`clampToWidth`: the group header (`app.go:7270`), the confirm prompt (`:7414`), `searchBarLine()` and every
line of `containerFooter()`.

`svcVisibleCount()` reserves exactly ONE row for the warning (`app.go:6052`). A wrapped warning occupies two,
so the frame is one physical row taller than `m.height`. bubbletea splits `View()` on `\n` and keeps the last
`m.height` ELEMENTS, so it cannot see the wrap: nothing is dropped from the string, the terminal scrolls
instead, and the breadcrumb goes off the top — the same failure mode the `viewHelp()` trailing-newline rule
exists to prevent, reached from the other direction.

The widths, with the two-space indent the site adds:

| warning | cells | wraps below |
| --- | --- | --- |
| `warnNoComposeDir` | 78 | 78 columns |
| `warnAllSelectableFolded` | 65 | 65 columns |
| `No rollback snapshot found — deploy first to record one` | 57 | 57 columns |
| `no rollback snapshot for: <names>` | unbounded | any width, on enough services |
| `no longer in the compose file: <names>` | unbounded | any width, on enough services |
| `exec failed: <err>` | unbounded | any width, on a long docker error |

The three constants need a narrow pane. The three formatted ones do not: they interpolate a
`strings.Join(…, ", ")` of service names or a raw docker error, so there is no width at which they are safe.

The fix is one `clampToWidth(…, m.width)` around the rendered string at that one site, which closes every row
above at once. `clampToWidth` is already a no-op at `m.width <= 0`, so the unsized first frame and the tests
that build a `Model{}` literal are unaffected.

**Pre-existing, and not introduced by the fold-aware select-all change.** `warnNoComposeDir` has been the
widest warning since the grouped host view landed; `warnAllSelectableFolded` is 13 cells narrower and only
made the class easier to hit, because the grouped view lands folded and `a` is a plausible first keystroke.
Flagged in review of that change and deliberately left out of scope: a warning-string addition should not
carry a renderer fix, and the fix wants its own test.

**Unresolved — decide the scope before the edit.** The two soft-warning slots immediately below
(`Stats unavailable: %v` at `:7435` and `updates: %s` at `:7437`) have exactly the same shape, the same
one-row reservation, and unbounded interpolated content, so they almost certainly belong in the same commit.
The service rows (`:7349`) and the column-captions row (`:7256`) are unclamped too, but they are a different
question: their width is data-driven — name, ports, CPU and Mem columns — so clamping them is a column-layout
decision rather than a one-line fix, and it should not ride along.

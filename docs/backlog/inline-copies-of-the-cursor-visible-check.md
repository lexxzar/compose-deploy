---
worth: later
where: internal/tui/grouped_test.go:205
added: 2026-08-28
---
# six inline copies of the cursor-visible check outlived the shared helper

`assertCursorVisible(t, m)` (`app_test.go:72`) is the shared form of the invariant `fixSvcOffset()` exists to
keep: the cursor sits inside `[svcOffset, svcOffset+svcVisibleCount())`. The page-key tests call it. Six
verbatim copies of the same three lines still stand beside it — `grouped_test.go:205`, `:2140`, `:4151` and
`app_test.go:6582`, `:13772`, `:13801` — each recomputing `svcVisibleCount()` into a local `visible` and
spelling the comparison out with a message of its own.

They were left alone on purpose: the paging change had to ADD the helper, and rewriting six unrelated tests
would have put the review on the wrong diff.

The cost is drift, not duplication. `svcVisibleCount()` changes shape whenever the header or the footer gains
a line — the column captions row, the reserved search bar, the grouped footer pair — and the invariant is
currently stated in seven places that nothing compares. The six messages already disagree on format, so a
failure names the site rather than the rule.

Mechanical, with three details worth carrying into the edit:

1. **Two sites keep a second assertion.** `grouped_test.go:202` and `:2137` assert `svcOffset` itself; the
   helper covers the cursor only, so the offset check stays and its `visible` local stays used.
2. **`grouped_test.go:2140` is inside a `for _, key := range []string{"z", "left"}` loop** and prefixes its
   message with `%q`. The helper drops that, so wrap the body in `t.Run(key, …)` or accept a failure that does
   not name the key.
3. **The `visible` locals differ.** `app_test.go:6582` and `:13801` declare one for the check alone, so the
   declaration goes with it; `app_test.go:13772` reuses a `visible` an earlier precondition already reads, so
   only the reassignment goes.

Raised while adding `pgup`/`pgdown` to the container list — the change that promoted the check to a helper in
the first place.

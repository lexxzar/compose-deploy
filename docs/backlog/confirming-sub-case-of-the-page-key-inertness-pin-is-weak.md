---
worth: later
where: internal/tui/app_test.go:6435
added: 2026-08-28
---
# the confirming sub-case of the page-key inertness pin cannot fail

`TestPageKeys_InertWhereTheOtherMovementKeysAre` (`app_test.go:6400`) sweeps the three states the container
dispatch already refuses to move the cursor in — a typing search bar, an armed confirmation, and the `svcErr`
screen that hides the rows — and asserts `svcCursor` stayed on its starting row. Under a deliberately
introduced key leak the `searching` and `svcErr` sub-cases both failed. `confirming` passed.

The reason is structural, not incidental. The `confirming` intercept (`app.go:2362`) handles `ctrl+c`, `enter`
and `esc` and returns `m, nil` for every other key, so control never reaches the movement switch below.
`got.svcCursor == start` therefore holds whatever that switch contains — including nothing at all. The
sub-case pins the intercept ORDER (a `pgup`/`pgdown` case hoisted above the prompt would move the cursor) and
nothing more: it stays green if the two keys are deleted, renamed, or given a different step. The `searching`
sibling avoids this by asserting state only a routed key could have changed — `searchQuery`,
`searchInput.Value()` and the recomputed `searchMatches`.

**Unresolved — settle the shape before any edit.** Asserting that the prompt survived (`confirming` still
true, `pendingOp` unchanged) is no stronger; it holds for the same early-return reason. The candidate with
teeth is a positive control inside the sub-case: send the same key to an otherwise identical model with the
prompt NOT armed and assert the cursor DID move, so a build where paging went inert everywhere fails here
instead of passing three times. That duplicates coverage `TestPageKeys_MoveAFullPageAndClamp` already owns,
and that overlap is the trade to decide.

Raised in review of the `pgup`/`pgdown` change, from a mutation run against the container dispatch. Every
other page-key test caught the mutation; this sub-case is the single hole.

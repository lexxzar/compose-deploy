---
worth: later
where: internal/tui/app.go:7002
added: 2026-08-28
---
# a blank ⇧ on a grouped header does not separate "not scanned" from "no updates"

`groupHeaderLine` draws `⇧ n` only when `updates > 0`, so a header with no glyph carries two different
meanings. In grouped mode `autoUpdatesAllowed()` is false and `U` scans one group per keypress, so a host
normally holds a mix of scanned and unscanned groups. The session that raised this showed `apps ⇧2`,
`docker ⇧6` and `kum ⇧1` beside four bare headers, with no way to tell which of the four were clean.

The data to separate the two states is already on the Model. Each group owns a cache entry under
`projUpdatesCacheKey(g.proj)`, and `hydrateGroupedUpdates` (`app.go:5500`) already reads it: a cache hit
with no `true` verdict is "scanned, clean", and a miss is "not scanned". The fix needs no registry call and
no new fetch, so its cost is a render decision only.

**Unresolved — settle this before any code.** Whether a third marker earns its cells. The header already
reads `▼ name   ● n up  ✗ n  ⇧ n` and `clampToWidth` cuts it at `m.width`. Every group is unscanned on
landing, so a "not scanned" marker draws on every row at once and may read as noise rather than as
information. Marking only the scanned-and-clean case is the quieter alternative and inverts that ratio.

Raised while deciding whether grouped mode should check for updates automatically. That auto-scan was
decided against on registry cost — see `autoUpdatesAllowed()` (`app.go:5691`) and its comment. This item is
the display half only and does not reopen it.

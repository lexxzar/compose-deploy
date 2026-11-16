---
worth: yes
where: internal/tui/app.go:1291
added: 2026-11-16
---
# two stale words in the servicesMsg handler comments

1. `app.go:1291` says a stale `loadServices` response "can overwrite both `services` and `svcStatus`".
   The Model has no `services` field; the payload rebuilds `svcGroups` through `setSingleGroup`.
2. `app.go:1408` spells "message" as "messsage".

Comment-only, no behaviour change. Left out of PR #33 to keep its diff on the fix.

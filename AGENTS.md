# AGENTS.md

**Read [`CLAUDE.md`](CLAUDE.md) — it is the single source of truth for this repository.**

This file used to carry its own copy of the architecture notes. That copy drifted
behind `CLAUDE.md` feature by feature (a deleted screen, a corrected key binding,
a widened interface) until it was actively misleading, so the duplicate is gone.
Everything an agent needs — the layer diagram, the `runner.Composer` seam, the
TUI state machine, the per-subsystem invariants and the recipes for adding an
operation, a screen or a subcommand — lives in `CLAUDE.md` and in the files it
points to under `docs/architecture/`.

## Build & test

```bash
go build -o cdeploy .          # Build binary
go test ./...                   # Run all tests
go test ./internal/runner/ -v   # Run tests for a single package
go test ./... -count=1          # Run all tests uncached
go mod tidy                     # After adding/removing imports
```

No linter or formatter config — use standard `go fmt` and `go vet`. No Makefile
or task runner. CI runs `go test ./... -covermode=atomic -coverprofile=coverage.out`
via GitHub Actions (`.github/workflows/coveralls.yml`).

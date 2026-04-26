# Ports Column for `cdeploy list` and TUI

## Overview

Add a Ports column to `cdeploy list` (CLI flat + grouped views) and the TUI `screenSelectContainers` row layout, surfacing published port mappings sourced from `docker compose ps --format json`. Read-only display only; no behavioral changes to deploy/restart/stop/exec/logs.

**Why**: triage of running stacks routinely asks "what's exposed on this host?" — the answer is in `ps` output but not in `cdeploy`'s view today, forcing users to drop to raw docker.

**Design pre-decided in brainstorm** (see "Solution Overview" for details — do not re-debate during implementation).

## Context (from discovery)

- **Project**: cdeploy — Go CLI/TUI wrapper around Docker Compose, supports local + remote (SSH) operation. `runner.Composer` interface seam between orchestrator and Docker.
- **Files involved**:
  - `internal/runner/runner.go` — `ServiceStatus` struct (extended)
  - `internal/compose/compose.go` — `psEntry`, `parseContainerStatus` (extended)
  - `internal/compose/ports.go` — new file, render helpers
  - `cmd/list.go` — `serviceStatus`, `formatDots`, `formatDotsGrouped`, `mergeStatus` (extended)
  - `internal/tui/app.go` — `hasStatusColumns`, `viewSelectContainers` row rendering at ~1900-1975 (extended)
- **Patterns observed**:
  - `psEntry` already aggregates per-replica into per-service via `svcAgg` (see compose.go:419-485). Ports aggregation slots in alongside.
  - `formatDots` uses width-tracking + `%-*s` padding for optional columns; same pattern for Ports.
  - TUI captions row at app.go:1932-1942 conditionally renders "Created"/"Uptime" labels — extend with "Ports".
  - Tests use stdlib `testing` only (no testify).
- **Remote zero-cost**: `RemoteCompose.ContainerStatus` already calls `parseContainerStatus` on the remote `ps` JSON output. No remote-specific work.

## Development Approach

- **Testing approach**: Regular (code first, tests immediately after, before next task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- run `go test ./...` after each task
- maintain backward compatibility (additive changes only — `ServiceStatus.Ports` is a new field; CLI JSON uses `omitempty`)

## Testing Strategy

- **Unit tests**: required for every task
- **No e2e**: project has no Playwright/Cypress; TUI tests drive `Update()`/`View()` directly without TTY
- **No live Docker**: parsing tests use JSON fixtures; rendering tests use synthetic `runner.ServiceStatus`/`runner.Port` slices
- All tests stdlib only

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

**Strategy**: Option A — published mappings only. Skip Compose `Publishers` entries with `PublishedPort == 0` (those are `expose:`-only, internal).

**Format**: `host:hp→cp` arrow form with full bind interface always shown (`0.0.0.0:8080→80`, `127.0.0.1:9000→9000`). `/tcp` omitted; `/udp`/`/sctp` shown as suffix. No truncation — comma-join all replica ports. Empty/missing `URL` from Compose is normalized to `0.0.0.0` before formatting (Compose's default for unspecified bind).

**Data source**:
1. Primary: `Publishers []` field from `docker compose ps --format json` (Compose v2).
2. Fallback: `Ports` text string (e.g. `0.0.0.0:8080->80/tcp, :::8080->80/tcp`) for older Compose versions where `Publishers` is empty/missing.

**Dedup**:
- IPv4/IPv6 mirrors of the same `(HostPort, ContainerPort, Protocol)` collapse to a single entry preferring `Host = "0.0.0.0"`.
- Scaled-service ports deduped by `(Host, HostPort, ContainerPort, Protocol)` tuple, sorted ascending by `HostPort` for stable output.

**Edge cases**:
- Stopped containers: empty `Ports` slice → blank cell (Compose doesn't reliably emit publishers for stopped containers).
- Service with no published ports: empty `Ports` slice → blank cell.
- Older Compose without `Publishers`: regex parse `Ports` text, dedup IPv6 mirrors.

**No responsive layout**: column always shown when any service has ports. Narrow terminal → row wraps. Defer responsive hiding until it actually bites.

## Technical Details

### `runner.Port` struct (new)

```go
type Port struct {
    Host          string // bind interface, e.g. "0.0.0.0", "127.0.0.1"
    HostPort      int
    ContainerPort int
    Protocol      string // "tcp", "udp", "sctp"
}
```

### `runner.ServiceStatus` (extended)

```go
type ServiceStatus struct {
    Running bool
    Health  string
    Created string
    Uptime  string
    Ports   []Port // new — aggregated across replicas, deduped, sorted by HostPort
}
```

### `psEntry` (extended)

```go
type psEntry struct {
    Service    string        `json:"Service"`
    State      string        `json:"State"`
    Health     string        `json:"Health"`
    CreatedAt  string        `json:"CreatedAt"`
    Status     string        `json:"Status"`
    Publishers []psPublisher `json:"Publishers"` // new
    Ports      string        `json:"Ports"`      // new — fallback
}

type psPublisher struct {
    URL           string `json:"URL"`
    TargetPort    int    `json:"TargetPort"`
    PublishedPort int    `json:"PublishedPort"`
    Protocol      string `json:"Protocol"`
}
```

### Render functions (`internal/compose/ports.go`)

```go
func FormatPort(p runner.Port) string
// returns "0.0.0.0:8080→80" or "0.0.0.0:1812→1812/udp"

func FormatPorts(ports []runner.Port) string
// comma-joined; empty slice → ""
```

### CLI JSON shape (`cmd/list.go`)

```json
{
  "service": "nginx",
  "running": true,
  "ports": [
    {"host": "0.0.0.0", "host_port": 8080, "container_port": 80, "protocol": "tcp"}
  ]
}
```

`omitempty` on `Ports` keeps existing JSON consumers compatible.

### TUI row (after Uptime, before end-of-line)

```
● ♥ nginx     2026-04-25 14:30  1d   0.0.0.0:80→80, 0.0.0.0:443→443
```

Captions row gains "Ports" label when any service has non-empty `Ports`.

## What Goes Where

- **Implementation Steps**: all code/test/doc changes — single repo, single PR scope.
- **Post-Completion**: manual smoke test against a real local compose stack with mixed services (published, internal-only, scaled, UDP, localhost-bound) since no live integration tests cover this.

## Implementation Steps

### Task 1: Add `Port` type and extend `ServiceStatus`

**Files:**
- Modify: `internal/runner/runner.go`

- [ ] add `Port` struct with `Host`, `HostPort`, `ContainerPort`, `Protocol` fields and JSON tags (`host`, `host_port`, `container_port`, `protocol`)
- [ ] append `Ports []Port` field to `ServiceStatus`
- [ ] update `ServiceStatus` doc comment to describe `Ports` aggregation rule (deduped, sorted by HostPort)
- [ ] verify `runner` package still compiles (no Composer interface change required)
- [ ] run `go test ./internal/runner/...` — must pass

### Task 2: Extend `psEntry` and add `extractPorts` helper

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/compose_test.go`

- [ ] add `psPublisher` struct with `URL`, `TargetPort`, `PublishedPort`, `Protocol` JSON tags
- [ ] extend `psEntry` with `Publishers []psPublisher` and `Ports string` fields
- [ ] add `extractPorts(entry psEntry) []runner.Port` — converts `Publishers`, skips `PublishedPort == 0`, dedupes IPv4/IPv6 mirrors (collapse `::` mirror to its `0.0.0.0` sibling when `(HostPort, ContainerPort, Protocol)` matches), preserves order
- [ ] write tests for `extractPorts`: single publisher; `PublishedPort == 0` skipped; IPv4/IPv6 mirror dedup; multiple distinct publishers preserved
- [ ] run `go test ./internal/compose/...` — must pass

### Task 3: Add `parsePortsString` fallback for older Compose

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/compose_test.go`

- [ ] add `parsePortsString(text string) []runner.Port` — parses comma-separated entries like `0.0.0.0:8080->80/tcp, :::8080->80/tcp, [::1]:8443->443/tcp`
- [ ] handle IPv6 bracket syntax (`[::]:8080->80/tcp`); strip brackets in parsed `Host`
- [ ] dedupe IPv4/IPv6 mirrors same as `extractPorts`
- [ ] skip malformed entries silently (do not error — text format is best-effort fallback)
- [ ] write tests: empty string; single entry; multi-entry comma-split; IPv4/IPv6 mirror dedup; bracketed IPv6; malformed entry skipped; all-malformed input → empty slice (no panic); UDP suffix preserved
- [ ] run `go test ./internal/compose/...` — must pass

### Task 4: Wire ports into `parseContainerStatus` aggregation

**Files:**
- Modify: `internal/compose/compose.go`
- Modify: `internal/compose/compose_test.go`

- [ ] extend `svcAgg` with `ports []runner.Port` field
- [ ] in the aggregation loop, call `extractPorts(entry)` (fallback to `parsePortsString(entry.Ports)` when `Publishers` is empty/nil) and append to `a.ports`
- [ ] after aggregation loop, dedupe `a.ports` by `(Host, HostPort, ContainerPort, Protocol)` tuple and sort ascending by `HostPort`, then assign to `status[svc].Ports`
- [ ] write tests: single replica with one publisher; scaled service with 3 ephemeral host ports → 3 distinct Ports sorted; scaled service with identical publishers → deduped to 1; stopped container with no `Publishers` → empty Ports; stopped replica that still has non-empty `Publishers` → ports are still surfaced (Compose-driven, not filtered by state); older-Compose fallback (no `Publishers`, `Ports` text only) parses correctly
- [ ] run `go test ./internal/compose/...` — must pass

### Task 5: Add `FormatPort` / `FormatPorts` render helpers

**Files:**
- Create: `internal/compose/ports.go`
- Create: `internal/compose/ports_test.go`

- [ ] create `internal/compose/ports.go` with `FormatPort(p runner.Port) string` returning `host:hp→cp` plus `/proto` suffix when protocol is non-empty and not `tcp`
- [ ] add `FormatPorts(ports []runner.Port) string` that returns `""` for empty slice and comma-joined `FormatPort` values otherwise
- [ ] write tests: TCP omits suffix; UDP shows `/udp`; SCTP shows `/sctp`; empty protocol omits suffix; localhost bind preserved (`127.0.0.1:9000→9000`); arrow rune is exactly `→` (U+2192) — not `->`; empty slice → empty string; multi-port join
- [ ] run `go test ./internal/compose/...` — must pass

### Task 6: Surface ports in CLI `list` output

**Files:**
- Modify: `cmd/list.go`
- Modify: `cmd/list_test.go`

- [ ] extend `serviceStatus` struct with `Ports []runner.Port \`json:"ports,omitempty"\``
- [ ] update `mergeStatus` to copy `st.Ports` through into `serviceStatus`
- [ ] in `formatDots`, track `maxPorts` width (computed as `len(compose.FormatPorts(item.Ports))`), append column after Uptime gated on `maxPorts > 0`
- [ ] in `formatDotsGrouped`, replicate the same `maxPorts` per-project tracking and rendering
- [ ] write tests: `formatDots` with mixed services (some with ports, some without) — column padded; `formatDots` with no services having ports — no Ports column rendered; `formatDots` over a `flattenProjectServices`-style fixture (multiple projects in flat mode, mixed port presence) — column aligns correctly; `formatDotsGrouped` with per-project max width recalculated; `formatJSON` round-trip preserves structured Port array (assert ports field present + correct field names + omitempty when empty)
- [ ] run `go test ./cmd/...` — must pass

### Task 7: Surface ports in TUI container-select screen

**Files:**
- Modify: `internal/tui/app.go`
- Modify: `internal/tui/app_test.go`

- [ ] extend `hasStatusColumns()` to return true also when any service has non-empty `Ports`
- [ ] **no changes to `svcVisibleCount()`** — Ports column shares the same captions row that already triggers the existing `+1` to `headerLines`. Do not add a second bump.
- [ ] in `viewSelectContainers` row rendering (around app.go:1900-1975), add `maxPorts` width tracking alongside `maxCreated`/`maxUptime`
- [ ] append captions cell `"Ports"` after `"Uptime"` when `maxPorts > 0`
- [ ] append per-row `compose.FormatPorts(st.Ports)` cell after Uptime gated on `maxPorts > 0`, padded with `%-*s` to `maxPorts` width — empty Ports cells still render as padded whitespace, matching the existing Created/Uptime pattern
- [ ] write tests: row rendering with ports — formatted string appears in `View()` output; captions row includes "Ports" when any service has ports; `hasStatusColumns()` returns true with only Ports populated (existing test extended); row with empty Ports for a service in a list where another service has ports — alignment preserved (padded whitespace)
- [ ] run `go test ./internal/tui/...` — must pass

### Task 8: Verify acceptance criteria

- [ ] verify `cdeploy list` shows Ports column for services with published mappings; blank cell for services without
- [ ] verify `cdeploy list --json` includes structured `ports` array per service with correct field names
- [ ] verify TUI container-select shows Ports column matching CLI format (visually align with Created/Uptime)
- [ ] verify scaled services list all replica host ports comma-joined
- [ ] verify IPv4/IPv6 dual-stack services show one row, not two
- [ ] verify older Compose versions still render ports via text fallback (synthetic test fixture with `Ports` text only, `Publishers` nil)
- [ ] run full test suite: `go test ./... -count=1` — all pass
- [ ] run `go build -o cdeploy .` — builds without errors
- [ ] run `go mod tidy` — no changes needed (no new deps)

### Task 9: Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md` (if it documents `list` output format)

- [ ] add a paragraph to CLAUDE.md under the existing "Health checks" / "Multi-project discovery" cluster describing the Ports column: data source (`Publishers` primary, `Ports` text fallback), format (`host:hp→cp`, `/tcp` omitted, `/udp` shown), aggregation (deduped, sorted by HostPort), and edge cases (stopped containers blank, internal-only skipped)
- [ ] grep README.md for `cdeploy list` examples — if found, add ports to one example; if absent, skip (do not invent new sections)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification** (no automation covers this):
- Run `cdeploy list` against a local stack with mixed services: nginx (multi-port), api (single port), postgres/redis (no published ports), admin (127.0.0.1:9000), syslog (UDP), scaled service with 3 replicas.
- Verify visual alignment in TUI with various terminal widths.
- Run against a remote server via `-s prod` and via `--ssh user@host` to confirm zero-additional-work claim holds.
- Run against a host with older standalone `docker-compose` to confirm `Ports` text fallback works.

**External system updates**: none. This is a single-binary tool with no consuming projects.

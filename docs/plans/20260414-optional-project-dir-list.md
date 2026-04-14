# Optional project-dir for list

## Overview
- Make `-C` / `project_dir` optional for `cdeploy list -s <server>` 
- When omitted, discover all compose projects on the remote server and display all services grouped by project
- When `-C` is provided, behavior stays exactly the same (single project, flat output)
- CLI `list` command only — TUI already has its own project picker flow

**Acceptance criteria:**
- `cdeploy list -s prod` (no `-C`) lists all compose projects with grouped per-service status
- `cdeploy list -s prod -C /path` produces identical flat output as before
- `cdeploy list` (local, no `-s`) works unchanged
- `--json` in multi-project mode produces flat array with `"project"` field; in single-project mode omits it
- If a single project fails to list (broken compose file), others still display with a warning on stderr

## Context (from discovery)
- Files involved: `cmd/list.go` (main change), `cmd/list_test.go` (tests)
- `RemoteCompose.ListProjects()` already works without a `ProjectDir` — runs global `docker compose ls -a`
- `NewRemote()` derives socket path from host+PID only (not ProjectDir), so all instances for the same host share the ControlMaster socket — no extra SSH connections needed
- Current error at `cmd/list.go:159`: `--server %q requires --project-dir or project_dir in config`
- `serviceStatus` struct has `Name/Running/Health` — needs a `Project` field for multi-project JSON
- Formatters `formatDots()` and `formatJSON()` expect flat `[]serviceStatus`

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility

## Testing Strategy
- **Unit tests**: required for every task
- New formatter functions get dedicated test cases
- Multi-project orchestration logic extracted into a testable function that accepts a `composerFactory` callback (matching the `ComposerFactory` pattern from `cmd/root.go`), enabling tests with mock composers
- Existing tests must continue to pass (backward compat)

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope

## Solution Overview

**Multi-project discovery flow** (when `-s` given without `-C`):
1. Connect SSH with empty `ProjectDir` (ControlMaster established)
2. `RemoteCompose.ListProjects()` → discovers all compose projects. If this fails, return error immediately (fatal — Docker may not be running)
3. For each project: create `RemoteCompose` with project's `ConfigDir`, call `ListServices()` + `ContainerStatus()`. Per-project errors are non-fatal (warn on stderr, skip project)
4. Collect results into `[]projectServices` (project name + `[]serviceStatus`)
5. Display using grouped dot formatter or flatten to `[]serviceStatus` with `Project` set for JSON

**CRITICAL — ControlMaster socket lifecycle:**
- `NewRemote()` derives socket path from host+PID only (not ProjectDir), so all instances for the same host share one ControlMaster socket
- Only the initial `RemoteCompose` (empty ProjectDir) calls `Connect()` and defers `Close()`
- Per-project `RemoteCompose` instances piggyback on the existing socket — they must NOT call `Connect()` or `Close()`

**Note on `Project.ConfigDir`:**
- `ConfigDir` is a **remote-side** absolute path (parsed from `docker compose ls` output on the remote host)
- It is passed directly to `NewRemote(host, configDir)` as the `ProjectDir` — this works correctly because `filepath.Dir()` does string manipulation and Linux/macOS path separators match

**Output formats:**

Dots (grouped by project):
```
myapp
  ● ♥ nginx     running
  ●   postgres  running

monitoring
  ●   grafana   running
  ○   loki      stopped
```

JSON (flat array with project field, omitted in single-project mode via `omitempty`):
```json
[
  {"project":"myapp","service":"nginx","running":true,"health":"healthy"},
  {"project":"myapp","service":"postgres","running":true},
  {"project":"monitoring","service":"grafana","running":true},
  {"project":"monitoring","service":"loki","running":false}
]
```

**Key design decisions:**
- `serviceStatus.Project` field uses `json:"project,omitempty"` — single-project output stays unchanged
- New `projectServices` struct groups results for the grouped dots formatter
- For JSON, multi-project results are flattened to `[]serviceStatus` with `Project` set, then passed to existing `formatJSON()` — no separate JSON formatter needed
- Single-project path (`-C` provided) is completely untouched

## Technical Details
- `projectServices` struct: `{ Name string, Services []serviceStatus }` — used internally for grouped formatting
- `serviceStatus` gains `Project string json:"project,omitempty"` — backward compatible, empty = omitted in JSON
- `formatDotsGrouped([]projectServices) string` — renders project headers + indented service lines
- For multi-project JSON: flatten `[]projectServices` to `[]serviceStatus` (setting `Project` field), reuse existing `formatJSON()` — no separate formatter needed
- `runList` branching: if `serverName != ""` and no project dir → multi-project path; otherwise → existing single-project path
- Error handling: `ListProjects()` failure is fatal (return error). Per-project `ListServices()`/`ContainerStatus()` failures are non-fatal (warn on stderr, skip project)
- Sort order: projects alphabetical (by `parseProjects`), services alphabetical within each project (by `mergeStatus`)

## Implementation Steps

### Task 1: Add project field and grouped dot formatter

**Files:**
- Modify: `cmd/list.go`
- Modify: `cmd/list_test.go`

- [x] Add `Project string` field with `json:"project,omitempty"` to `serviceStatus` struct
- [x] Add `projectServices` struct with `Name string` and `Services []serviceStatus`
- [x] Implement `formatDotsGrouped([]projectServices) string` — project name header, indented service lines (reuse dot/health rendering from `formatDots`), blank line between projects
- [x] Write tests for `formatDotsGrouped` (single project, multiple projects, empty)
- [x] Write test verifying `formatJSON` still omits `project` when field is empty (omitempty backward compat)
- [x] Verify existing formatter tests still pass: `go test ./cmd/ -run TestFormat -v`
- [x] Run tests — must pass before Task 2

### Task 2: Implement multi-project listing in `runList`

**Files:**
- Modify: `cmd/list.go`
- Modify: `cmd/list_test.go`

- [x] Extract a helper `listSingleProject(ctx, composer, jsonOutput) error` from the existing single-project tail of `runList` (services → status → merge → format → print)
- [x] Verify existing tests still pass after extraction (no behavior change): `go test ./cmd/ -v`
- [x] In `runList`, when `serverName != ""` and no project dir: connect with empty `ProjectDir`, call `ListProjects` (fatal on failure), iterate each project creating scoped `RemoteCompose` (do NOT call `Connect()`/`Close()` on these — they piggyback on the ControlMaster socket), collect `projectServices`
- [x] Handle per-project `ListServices`/`ContainerStatus` errors: print warning to stderr, skip project, continue
- [x] For dots output call `formatDotsGrouped`; for JSON flatten `[]projectServices` to `[]serviceStatus` (setting `Project` field) and reuse existing `formatJSON()`
- [x] Remove the error `--server %q requires --project-dir or project_dir in config` — replace with multi-project flow
- [x] Extract multi-project orchestration into a testable function that accepts a factory callback (`func(dir string) runner.Composer`) for mock injection
- [x] Write tests for orchestration function using mock composers (multiple projects, empty project list, per-project error skipping)
- [x] Run tests — must pass before Task 3

### Task 3: Update command help text

**Files:**
- Modify: `cmd/list.go`

- [x] Update `Long` description to mention multi-project discovery when `-C` is omitted
- [x] Update `Example` block with `cdeploy list -s prod` (no -C) showing it lists all projects
- [x] Run tests — must pass before Task 4

### Task 4: Verify acceptance criteria

- [x] Verify: `cdeploy list -s server` without `-C` discovers and lists all projects (grouped output) — verified via `collectMultiProject` + `formatDotsGrouped` tests
- [x] Verify: `cdeploy list -s server -C /path` still shows single-project flat output (backward compat) — code path unchanged, existing tests pass
- [x] Verify: `cdeploy list` locally without `-s` still works as before — existing tests pass
- [x] Verify: `--json` works in both single and multi-project modes — `TestFormatJSON_IncludesProject` + `flattenProjectServices` tests
- [x] Verify: single-project JSON has no `project` field (omitempty) — `TestFormatJSON_OmitsEmptyProject`
- [x] Run full test suite: `go test ./... -count=1` — all packages pass

### Task 5: [Final] Update documentation

- [ ] Update CLAUDE.md if new patterns discovered
- [ ] Move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Test against a real remote server with multiple compose projects
- Verify ControlMaster socket reuse (no extra SSH password prompts)
- Verify graceful handling when a project's compose file is broken/missing
- Test with `--json | jq` to confirm filtering works as expected

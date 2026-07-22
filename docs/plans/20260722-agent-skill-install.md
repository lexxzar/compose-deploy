# Agent Skill Distribution (`cdeploy skill`)

## Overview

Ship an embedded Agent Skill (SKILL.md per the agentskills.io open standard) that teaches AI
coding agents how to install, configure, and drive the cdeploy CLI — including setting up
`~/.cdeploy/servers.yml` from scratch — and a new `cdeploy skill` subcommand that installs it
into agent skill directories.

- Problem: agents already can run cdeploy's CLI, but nothing teaches them the workflows
  (stale-image sweep, deploy-with-confirmation loop, servers.yml setup) or the safety rules.
- Benefit: `cdeploy skill install claude` gives Claude Code (plus Cursor/OpenCode, which read
  Claude's dirs) full cdeploy fluency; `install codex` covers Codex/Gemini/Amp via the shared
  `.agents/skills` convention. Skill content is version-locked to the binary (go:embed).
- The repo-root `skills/cdeploy/SKILL.md` layout additionally makes external channels work
  with zero extra code: `npx skills add lexxzar/compose-deploy`, `gh skill install lexxzar/compose-deploy`.

## Context (from discovery)

- `cmd/root.go:27-31` — `version`/`commit`/`date` ldflags vars already exist (GoReleaser wired);
  `version` is the stamp source. Subcommands register at `cmd/root.go:198` via `rootCmd.AddCommand()`.
- Pattern to follow: `cmd/list.go` — subcommand + pure helpers + tests all in package `cmd`.
- `internal/config/config.go` — `config.ValidColors` is the color list quoted in the skill body.
- Related pending plan: `docs/plans/20260717-homebrew-distribution.md` (formula caveats can
  later print the install hint — out of scope here).
- Research (verified 2026-07-22): Codex skill-path migration `~/.codex/skills` → `~/.agents/skills`
  is live and messy (openai/skills#420, openai/codex#28505) — hence dual-write; re-verify at ship time.
- No `runner.Composer` changes → the 5 pinned mock implementations are untouched.

## Development Approach

- **Testing approach**: Regular (code first, then tests, within each task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional — they are a required part of the checklist
  - cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Run `go test ./...` after each change; stdlib `testing` only (repo convention)
- Backward compatible: purely additive — no existing command or interface changes

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach)
- All filesystem tests use `t.TempDir()` + `t.Setenv("HOME", ...)` / `t.Setenv("CODEX_HOME", ...)`;
  no Docker, no network, no permission-bit games (CI-as-root safe). `os.UserHomeDir()` reads
  `$HOME` on unix (precedent: `internal/config/identity_test.go:165-181`)
- `t.Setenv` is incompatible with `t.Parallel()` — do not add `t.Parallel` to these tests
- No e2e framework in this repo; final task does a manual smoke install into a temp HOME

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Keep plan in sync with actual work done

## Solution Overview

Locked decisions from the 2026-07-22 brainstorm:

1. **One skill** named `cdeploy` — safety protocol lives in the body text (skill gating is not
   an enforcement boundary; agent Bash permissions are).
2. **Targets**: `install <claude|codex|all>`. claude → `~/.claude/skills/cdeploy/`;
   codex → dual-write `~/.agents/skills/cdeploy/` **and** `$CODEX_HOME/skills/cdeploy/`
   (default `~/.codex/skills/`); all → union deduped via symlink resolution.
3. **Verbs**: `install` / `show` / `uninstall`. No `status` — idempotent install reports
   created/updated/unchanged. `show` prints the raw embedded (unstamped) SKILL.md to stdout.
4. **Ownership**: hash guard + `--force`. Stamp injected into frontmatter `metadata:` map at
   install time; repo/embedded file stays clean. Modified or unstamped files are never
   overwritten or deleted without `--force`.
5. **Scope**: user-level only. Help text documents
   `cdeploy skill show > .claude/skills/cdeploy/SKILL.md` for manual project-level placement.
6. **Repo layout**: repo-root `skills/` is both the convention directory for external
   installers and the Go package holding the `//go:embed` directive.

## Technical Details

**Rendering & stamp.** Rendered file = embedded SKILL.md with two lines inserted into the
frontmatter `metadata:` map (before the closing `---`; frontmatter must remain the first bytes
of the file — Gemini silently skips otherwise):

```yaml
metadata:
  cdeploy-version: v0.7.0
  cdeploy-content-hash: sha256:<64 hex>
```

**Owned block & hash.** `stripOwned()` removes the ENTIRE injected metadata block — the
`metadata:` header line plus both `cdeploy-*` lines — so
`stripOwned(renderSkill(canonical, v)) == canonical` holds as literal byte-equality, and the
stamped hash is simply `sha256(canonical)`. Verify = read file → `stripOwned` → sha256 →
compare to stamp. Deterministic, no self-reference, detects user edits across versions.
The canonical embedded file must NOT contain its own `metadata:` key (pinned by test) so
block insertion/removal stays trivial. Insertion targets the frontmatter's CLOSING `---`
(the second `---` in the file) — never a bare `---` that appears in the markdown body.

**Two distinct checks (do not conflate).** `verifySkillFile(onDisk)` answers only "is this
file consistent with its OWN stamp?" → intact / modified / unstamped. The install decision
additionally compares `stripOwned(onDisk)` against the CURRENT binary's canonical bytes
(equivalently: stamped hash vs `sha256(canonical)`) to pick unchanged / stamp-refresh /
updated. Without both checks the `unchanged` state is unreachable.

**Install decision (per destination).** `MkdirAll`, then:
- absent → write; `installed <path> (vNew)`
- stamp valid, stripped content == new canonical, version equal → `unchanged <path>`
- stamp valid, stripped content == new canonical, version differs → rewrite (stamp refresh); `updated <path> (vOld → vNew)`
- stamp valid, content differs → overwrite; `updated <path> (vOld → vNew)`
- stamp missing or hash mismatch → refuse this path (user-owned/edited, e.g. installed by
  `npx skills`), continue other paths, exit non-zero, message suggests `--force`
- `--force` → overwrite unconditionally

**Uninstall (per destination).** Absent → `not installed` (not an error); stamp valid →
delete `SKILL.md` only, then remove the `cdeploy/` dir only if empty (never `rm -rf`);
modified/unstamped → refuse without `--force`.

**Path resolution & dedupe.** `os.UserHomeDir()`; `$CODEX_HOME` overrides `~/.codex`, with
an EMPTY value treated as unset (`t.Setenv` can set but never unset an env var, so the
fallback must trigger on empty). Dedupe candidates by resolving symlinks on the deepest *existing* ancestor of each target dir
(target may not exist yet; `filepath.EvalSymlinks` errors on missing paths), falling back to
the cleaned absolute path. One resolved path = one write.

**CLI shape.** `Args: cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs)`,
`ValidArgs: ["claude","codex","all"]` for install/uninstall; `show` uses `Args: cobra.NoArgs`
and writes via `cmd.OutOrStdout()` (NOT `os.Stdout` directly — unlike list/logs — so tests can
capture output with `cmd.SetOut(&buf)`).
Primary output → stdout; failures → stderr; exit non-zero if any path failed.
Per-agent pickup notes after install (restart Claude Code / verify with `/skills`;
Codex scans at startup). **No install nudges/banners anywhere else in the CLI.**
Dev builds stamp `cdeploy-version: dev` — update detection keys on content hash, not version.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): Go code, SKILL.md content, tests, docs — all in this repo
- **Post-Completion** (no checkboxes): live-agent verification, path re-verification at release time, README badge/announcement

## Implementation Steps

### Task 1: skills package — embedded SKILL.md + content pins

**Files:**
- Create: `skills/cdeploy/SKILL.md`
- Create: `skills/skills.go`
- Create: `skills/skills_test.go`

- [x] write `skills/cdeploy/SKILL.md` (<500 lines, ~5k tokens): frontmatter `name: cdeploy`,
      `description` ≤1024 chars naming BOTH ops triggers (deploy/status/logs/updates) AND setup
      triggers (install cdeploy, configure servers.yml, SSH setup)
- [x] body §1 What cdeploy is (3 lines; agents use CLI subcommands, never the TTY-requiring TUI)
- [x] body §2 Setup & configuration: `cdeploy --version` check, install paths (brew/go install/
      releases), full servers.yml schema + worked example (servers: name/host/project_dir/group/
      color; groups: name/color; valid colors red green yellow blue magenta cyan white gray),
      direct YAML editing (validated by cdeploy on startup — `Load()` then `Validate()`, see
      `cmd/root.go:76-82`), SSH prerequisites (key-based auth; `~/.ssh/config`
      for advanced), ad-hoc/CI form `--ssh [user@]host[:port] --project-dir … --identity …`
- [x] body §3 Read-only ops: `list --json` with `--stats`/`--updates`/`-s`/`-C`, JSON parsing
      guide (tri-state `update_available`, health values, ports array), `logs` with tail,
      stale-image sweep workflow; §4 Mutating ops SAFETY PROTOCOL: restate services+server and
      get user confirmation first, never `-a` unless user explicitly said all, verify with `list`
      after, tail logs on failure; §5 Troubleshooting: `registry unreachable`, `remote update
      check transport failure`, `updates unavailable: …`, detect failures — meaning + next action
- [x] create `skills/skills.go`: `package skills`, `//go:embed cdeploy`, export `FS embed.FS`
      (+ helper returning the canonical SKILL.md bytes)
- [x] write `skills/skills_test.go` content pins: FS contains `cdeploy/SKILL.md`; file starts
      with `---`; `name: cdeploy` matches dir + regex `^[a-z0-9]+(-[a-z0-9]+)*$`; description
      non-empty ≤1024 chars; <500 lines; no `metadata:` key in canonical frontmatter
- [x] run tests — must pass before task 2

### Task 2: cmd/skill.go pure helpers — paths, render/stamp, verify

**Files:**
- Create: `cmd/skill.go`
- Create: `cmd/skill_test.go`

- [x] implement `skillTargetDirs(target string) ([]string, error)`: claude/codex/all matrix,
      `os.UserHomeDir()`, `$CODEX_HOME` override (empty value = unset → fall back to
      `~/.codex`), symlink-resolve dedupe (deepest existing ancestor, fallback cleaned path)
- [x] implement `renderSkill(canonical []byte, version string) []byte`: inject the metadata
      block (`metadata:` header + `cdeploy-version` + `cdeploy-content-hash: sha256(canonical)`)
      before the frontmatter's CLOSING `---` (the second `---` — never a bare `---` in the body)
- [x] implement `stripOwned(onDisk []byte) []byte` (removes the entire injected block) and
      `verifySkillFile(onDisk []byte) (stampVersion string, state int)` returning unstamped /
      modified / intact (stripOwned → sha256 → compare to own stamp)
- [x] write table tests: path matrix with `t.Setenv` HOME/CODEX_HOME (incl. empty CODEX_HOME
      ⇒ `~/.codex` fallback); symlink dedupe (`~/.codex/skills` → symlink to
      `~/.agents/skills` in temp home ⇒ one path)
- [x] write render/verify round-trip tests: `stripOwned(renderSkill(canonical, v))` byte-equals
      canonical; metadata lands inside frontmatter, frontmatter stays first bytes; a canonical
      whose BODY contains a bare `---` line still gets the injection at the frontmatter close;
      tampered body ⇒ modified; no stamp ⇒ unstamped
- [x] run tests — must pass before task 3

### Task 3: `skill install` verb + command registration

**Files:**
- Modify: `cmd/skill.go`
- Modify: `cmd/root.go`
- Modify: `cmd/skill_test.go`
- Modify: `cmd/root_test.go`

- [ ] implement `newSkillCmd()` with `install <claude|codex|all>` (`--force` flag), wire the
      per-destination decision matrix (absent/unchanged/stamp-refresh/updated/refused) with
      aggregated errors — one failed path doesn't abort others; exit non-zero on any failure
- [ ] print exact resolved paths + per-agent pickup notes to stdout after install
- [ ] register `newSkillCmd()` in `cmd/root.go` `rootCmd.AddCommand(…)`
- [ ] write install decision-matrix tests in temp HOME: fresh ⇒ installed; identical ⇒ unchanged;
      valid stamp + old content ⇒ updated; valid stamp + EQUAL content + older version ⇒ stamp
      refresh reported as `updated` (on-disk file differs only in the version line afterwards);
      tampered ⇒ refused without `--force`, overwritten with; pre-existing unstamped file ⇒ refused
- [ ] write aggregation test: plain FILE at `~/.claude/skills/cdeploy` obstructs MkdirAll ⇒
      claude path fails, codex paths still written, non-zero exit
- [ ] add registration assertions to `cmd/root_test.go` (skill command + 3 verbs exist)
- [ ] run tests — must pass before task 4

### Task 4: `skill show` + `skill uninstall` verbs

**Files:**
- Modify: `cmd/skill.go`
- Modify: `cmd/skill_test.go`

- [ ] implement `show` (`Args: cobra.NoArgs`): raw embedded SKILL.md written via
      `cmd.OutOrStdout()` (testable with `cmd.SetOut(&buf)`); help text documents
      `cdeploy skill show > .claude/skills/cdeploy/SKILL.md` for project-level placement
- [ ] implement `uninstall <claude|codex|all>` (`--force`): absent ⇒ "not installed" (no error);
      intact stamp ⇒ delete SKILL.md only, remove `cdeploy/` dir only if empty; modified/
      unstamped ⇒ refuse without `--force`; aggregate multi-path results
- [ ] write `show` test: capture via `cmd.SetOut(&buf)`, output byte-equals embedded canonical
- [ ] write uninstall matrix tests: absent ok / stamped removes file + empty dir / dir with
      extra user file retained / modified refused without `--force`, removed with / UNSTAMPED
      file (e.g. dropped by `npx skills`) refused without `--force`, removed with
- [ ] run tests — must pass before task 5

### Task 5: Verify acceptance criteria

- [ ] verify all Overview requirements implemented (install/show/uninstall, all targets,
      hash guard, dedupe, aggregation, pickup notes)
- [ ] verify edge cases: `$CODEX_HOME` set/unset, symlinked dirs, dev-version stamp, unstamped
      pre-existing files, obstructed destination
- [ ] run full suite: `go test ./... -count=1` and `go build -o cdeploy .`
- [ ] smoke test in temp HOME: `HOME=$(mktemp -d) ./cdeploy skill install all` then re-run
      (⇒ unchanged), edit file, re-run (⇒ refusal), `--force` (⇒ updated), `uninstall all`
- [ ] `go mod tidy` (no new deps expected — stdlib only)

### Task 6: [Final] Update documentation

- [ ] README.md: "AI agent integration" section — `cdeploy skill install claude|codex|all`,
      external channels (`npx skills add lexxzar/compose-deploy`, `gh skill install`), what the
      skill teaches
- [ ] CLAUDE.md: document the `skills/` package, stamp/hash ownership model, and the
      SKILL.md-content test pins (so future SKILL.md edits keep frontmatter constraints)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Install into a real Claude Code session; confirm the skill appears in `/skills` and activates
  on "check my servers for updates" and "help me set up cdeploy"
- Same check in Codex CLI (`/skills` listing; implicit activation)
- Re-verify Codex skill paths at release time — the `~/.codex/skills` → `~/.agents/skills`
  migration (openai/skills#420) may settle and allow dropping the dual-write

**External system updates:**
- After the Homebrew plan lands: add formula caveats printing `cdeploy skill install …` hint
- Optional later: submit the repo to skills.sh / gh-skill discovery (works as-is once
  `skills/cdeploy/SKILL.md` is on the default branch)

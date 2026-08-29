# Agent Skill distribution (`cdeploy skill`)

> Extracted verbatim from `CLAUDE.md`, which is auto-loaded and has a size limit.
> `CLAUDE.md` carries the rule digest; this file carries the full rationale, the
> failure modes it prevents, and the tests that pin them. Read it before you touch `cmd/skill.go`, the `skills/` package, or `skills/cdeploy/SKILL.md`.

`cdeploy skill install|show|uninstall <claude|codex|all>` writes one markdown file into a user-level directory that some other program reads. That is the whole feature, and every rule below follows from the two things it does not control: the directory may already hold a file cdeploy did not write, and the program that reads it silently drops a file whose frontmatter is not the first bytes.

## The `skills/` package: one directory, two consumers

The repo-root `skills/` directory is BOTH the external-installer convention directory AND a Go package. `npx skills add lexxzar/compose-deploy` and `gh skill install lexxzar/compose-deploy` look for `skills/<name>/SKILL.md` on the default branch, so the file has to sit at exactly that path in the repo; `skills/skills.go` then holds `//go:embed cdeploy` over the same tree, so the binary carries a byte-identical copy. There is no build step and no generated file between them — the checked-in `skills/cdeploy/SKILL.md` is the single source of truth for the CLI, for external installers, and for verification, and `go:embed` version-locks it to the binary.

`skills.FS` exposes the embedded tree. `skills.CanonicalSkill()` returns the raw, unstamped bytes and **panics on a missing embed** rather than returning an error: the `go:embed` directive guarantees the file exists at compile time, so a read failure is a build/packaging fault, not a runtime condition worth threading through every caller. `TestFSContainsCanonicalSkill` and `TestCanonicalSkillMatchesFS` pin both halves.

## Target directories and the dedupe key

`skillDests(target)` is the resolver; `skillTargetDirs(target)` is a thin wrapper that discards the agent labels for callers (uninstall) that only need paths.

| target   | candidates |
| -------- | ---------- |
| `claude` | `~/.claude/skills/cdeploy` (also read by Cursor and OpenCode) |
| `codex`  | `~/.agents/skills/cdeploy` AND `$CODEX_HOME/skills/cdeploy` (covers Codex/Gemini/Amp) |
| `all`    | the union of both, deduped |

`$CODEX_HOME` defaults to `~/.codex`, and **an EMPTY `$CODEX_HOME` is treated as unset** so the fallback fires. That is not defensive coding for its own sake: `t.Setenv` can set a variable but can never unset one, so the empty string is the only value a test can use to express "the user has no `CODEX_HOME`". Treating empty as unset is what makes the default path testable at all (`TestSkillTargetDirs_CodexEmptyCodexHome`; every other test in `cmd/skill_test.go` sets `CODEX_HOME` to `""` for the same reason). The corollary is that `cmd/skill_test.go` must never call `t.Parallel` — it drives `HOME` and `CODEX_HOME` through `t.Setenv`, which is process-global.

`resolveDedupeKey(path)` canonicalises a candidate so two spellings of one physical directory collapse to a single write. It **resolves symlinks on the deepest EXISTING ancestor and then re-appends the peeled suffix**, because `filepath.EvalSymlinks` errors on a missing path and the target directory usually does not exist yet — this is an installer, the first run creates it. Peel one component at a time toward the root, resolve the first ancestor that exists, rejoin. When no ancestor resolves (a filesystem root that itself fails), fall back to the cleaned absolute path. `TestSkillTargetDirs_SymlinkDedupe` drives the real case: with `~/.codex/skills` a symlink to `~/.agents/skills`, the two codex candidates collapse to one destination and the skill is written once. `TestSkillDests_MergesAgentsOnCollision` does the same across agents, through a `~/.claude/skills` → `~/.agents/skills` symlink.

**On a collision the agent labels are MERGED, first-seen order** (`appendUnique`), which is why `skillDest` carries `agents []string` and not one label. Two candidates from different agents can land on one physical directory, and the pickup notes are keyed on agents rather than paths — a merged destination must fire both notes (`TestSkillDests_MergesAgentsOnCollision`, `TestSkillInstall_CollapsedDirEmitsBothPickupNotes`).

Multi-path install and uninstall **aggregate per-path outcomes**: successes print to stdout, refusals and I/O errors to stderr, one failed destination never aborts the others, and any failure yields a non-zero exit (`TestSkillInstall_AggregatesFailures` plants a plain FILE where the claude `cdeploy` dir belongs and checks the codex paths still get written). **The pickup notes are keyed on the agents whose destinations actually SUCCEEDED, never on the CLI target**, so a partially failed `install all` never tells the user to restart an agent whose only path failed.

## The stamp/hash ownership model

**Install never trusts a file it did not write.** Everything else here is machinery for answering "did we write this?" against a file whose only record is itself.

`renderSkill(canonical, version)` injects exactly three lines:

```yaml
metadata:
  cdeploy-version: <version>
  cdeploy-content-hash: sha256:<64 hex>
```

**They go immediately before the frontmatter's CLOSING `---`, located by `frontmatterCloseOffset`, never by scanning for a bare `---`.** The file must open with a `---\n` fence as its first bytes; the close is the first subsequent line starting with `---`, so a horizontal rule in the markdown body can never be mistaken for the fence. Appending the block after the body, or opening a second frontmatter block, would push the real frontmatter off the front of the file, and **Gemini silently skips a skill whose frontmatter is not the first bytes** — a failure with no error message anywhere. `TestRenderMetadataInsideFrontmatter` pins the placement.

`stripOwned(onDisk)` reverses that exactly: it removes the `metadata:` header line plus the consecutive `cdeploy-version:` / `cdeploy-content-hash:` child lines under it, and leaves everything else byte-for-byte. A lone `metadata:` with no cdeploy children is preserved. The invariant is literal byte-equality:

```
stripOwned(renderSkill(canonical, v)) == canonical
```

`TestRenderStripRoundTrip` is that line as a test. Because it holds, the stamped hash is simply `sha256(canonical)` — no self-reference, no hash-of-a-file-containing-its-own-hash problem.

**`stripOwned` and `parseStamp` both scan the FRONTMATTER REGION ONLY** (`onDisk[:closeStart]`); the closing fence and the entire body are appended back verbatim. That constraint is an integrity property, not tidiness. A `metadata:` / `cdeploy-*` block a user plants in the BODY must be left untouched — stripping it too would let a tampered file hash back to the canonical bytes and verify as `intact`, defeating the whole ownership guard. Symmetrically, `parseStamp` must not read a body line as a stamp, or a forged block in the body would make an unstamped file look installed. `TestVerifyForgedStampInBodyIgnored` and `TestVerifyStampedFileWithBodyInjectedStampModified` pin the two directions.

## Two distinct checks, and why one is not enough

`verifySkillFile(onDisk)` answers **ONLY** "is this file consistent with its OWN stamp?" — `stripOwned` → `sha256` → compare against the hash the file records for itself — and returns one of `intact` / `modified` / `unstamped` alongside the recorded version.

That question cannot decide an install, because it never mentions the binary doing the installing. The install path therefore computes a **SECOND, distinct** check:

```go
contentEqual := bytes.Equal(stripOwned(onDisk), canonical)
```

against THIS binary's canonical bytes. **Without both checks the `unchanged` state is unreachable**: self-consistency alone cannot distinguish an intact install of an OLD skill (must be updated) from an intact install of the CURRENT one (must be left alone), and content equality alone cannot distinguish our file from a user's hand-made copy that happens to match.

The two combine into the decision matrix `installSkillPath` applies per destination:

| on disk | `contentEqual` | version | outcome |
| ------- | -------------- | ------- | ------- |
| absent | — | — | `installed` (write) |
| `intact` | yes | == binary's | `unchanged` (no write) |
| `intact` | yes | older | `updated` — stamp refresh only |
| `intact` | no | any | `updated` — content predates this binary |
| `modified` | any | any | `refused` without `--force` |
| `unstamped` | any | any | `refused` without `--force` |

`TestSkillInstall_Fresh`, `_IdenticalRerun`, `_StampRefresh`, `_OldContentUpdated`, `_TamperedRefusedThenForced` and `_UnstampedRefusedThenForced` are the six rows. **Freshness keys on the content hash, not the version string** — dev builds stamp `cdeploy-version: dev` (`TestVerifyDevVersionStamp`), so a version comparison alone would either never update or always update during development.

`--force` overwrites unconditionally with ONE short-circuit: an `intact` + `contentEqual` + same-version file is still reported `unchanged` rather than rewritten, because the bytes on disk already equal what would be written (`TestSkillInstall_ForceUnchangedSkipsRewrite`). Refusal reasons name themselves — `modified since install` or `not installed by cdeploy (unstamped)` — and both tell the user about `--force`. The unstamped case is the common one: a copy dropped by `npx skills` or `gh skill install` carries no stamp, and silently overwriting it would discard whatever the user chose to install.

**Writes go through `atomicWriteFile` — temp file in the same directory, `Chmod` to 0644, rename** (`os.CreateTemp` makes 0600). An interrupted plain `os.WriteFile` could leave a truncated SKILL.md whose closing frontmatter fence is gone; the next install would read that as `unstamped` and refuse the path without `--force`. The temp+rename removes that failure mode, and mirrors `config.Save`.

## Uninstall

`uninstallSkillPath` removes only what install wrote. An absent file is a SUCCESS (`not installed`, `TestSkillUninstall_Absent`) — uninstalling something that is not there is not an error. A file that is not `intact` is refused without `--force`, using the same two reasons (`TestSkillUninstall_ModifiedRefusedThenForced`, `_UnstampedRefusedThenForced`).

After removing `SKILL.md` it calls `os.Remove(dir)` on the enclosing `cdeploy/` directory — **which succeeds only when the directory is now empty, and is a safe no-op otherwise. Never a recursive delete.** A user who dropped extra files beside the skill keeps them and keeps the directory (`TestSkillUninstall_StampedRemovesFileAndEmptyDir`, `TestSkillUninstall_DirWithExtraFileRetained`).

## `show`

`skill show` writes the raw, UNSTAMPED canonical bytes through **`cmd.OutOrStdout()`, not `os.Stdout`** — that is what lets `cmd.SetOut(&buf)` capture it in a test, and `TestSkillShow_ByteEqualsCanonical` asserts byte equality against `skills.CanonicalSkill()`. It is unstamped on purpose: the output is meant to be redirected into a project-level directory (`cdeploy skill show > .claude/skills/cdeploy/SKILL.md`), which cdeploy neither owns nor manages, so a stamp there would be a claim it cannot honour.

## Flag scope

The `skill` parent carries a `PersistentPreRunE` running `checkSkillNoRemoteFlags()`, so all three verbs reject the inherited root globals `--server`, `--ssh`, `--identity` and `--project-dir` with one named error. The verbs install into LOCAL, user-level agent directories only. `cdeploy --ssh user@host skill install claude` READS as "install on the remote host" but would write locally, so a silent no-op-of-intent is worse than an error (`TestSkillRejectsRemoteFlags`, `TestSkillNoGlobalFlagsSucceeds`). This is also why `cmd/skill.go` is a documented exception to the subcommand skeleton in `CLAUDE.md`: it has no remote branch to write.

## SKILL.md content pins

`skills/skills_test.go` pins the canonical file. Future edits to `skills/cdeploy/SKILL.md` must keep all of these satisfied:

- **Frontmatter is the very first bytes** — a `---\n` fence with a closing `---` (`TestSkillStartsWithFrontmatterFence`). Both `frontmatterCloseOffset` and Gemini's loader depend on it.
- **`name: cdeploy`** — must match the containing directory AND the agentskills.io regex `^[a-z0-9]+(-[a-z0-9]+)*$` (`TestSkillName`).
- **`description` non-empty and ≤1024 chars**, naming both operational and setup triggers so an agent activates the skill in either context. The asserted keywords are `restart`, `logs`, `servers.yml` and `ssh` — **deliberately NOT `deploy`**, which is a substring of the product name `cdeploy` and so could never fail the check (`TestSkillDescription`).
- **Under 500 lines** (`TestSkillUnderLineLimit`).
- **NO `metadata:` key in the canonical frontmatter** (`TestCanonicalHasNoMetadataKey`). This is the load-bearing one: the install-time renderer OWNS the `metadata:` block, and `stripOwned` must round-trip back to exactly the canonical bytes. A hand-authored `metadata:` key carrying `cdeploy-version:` or `cdeploy-content-hash:` children would be stripped as ours, breaking the byte-equality invariant and making every fresh install verify as `modified`. One without those children survives `stripOwned`, but `renderSkill` still injects its own block at the closing fence, so the shipped file would carry two `metadata:` keys in one YAML document. Keep the canonical frontmatter free of the key entirely and neither case can arise.

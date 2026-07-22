package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Skill-file verification states. verifySkillFile answers only "is this file
// consistent with its OWN stamp?" — it does NOT compare against the current
// binary's canonical bytes (that is the install decision's job).
const (
	// skillStateUnstamped means the file carries no cdeploy content-hash stamp
	// (never installed by us, or the stamp was removed — e.g. a file dropped by
	// `npx skills`).
	skillStateUnstamped = iota
	// skillStateModified means the file has a stamp but its stripped content no
	// longer hashes to the recorded value — the user edited it after install.
	skillStateModified
	// skillStateIntact means the file's stripped content matches its own stamp.
	skillStateIntact
)

// skillTargetDirs resolves the on-disk SKILL.md parent directories for an
// install/uninstall target. Returns the deduplicated, symlink-resolved write
// targets — one entry per distinct physical location.
//
//   - "claude" → ~/.claude/skills/cdeploy
//   - "codex"  → ~/.agents/skills/cdeploy AND $CODEX_HOME/skills/cdeploy
//     ($CODEX_HOME defaults to ~/.codex; an EMPTY value is treated as unset so
//     the fallback triggers — t.Setenv can set but never unset an env var)
//   - "all"    → the union of both, deduped
//
// Dedupe resolves symlinks on the deepest EXISTING ancestor of each candidate
// (the target dir itself usually does not exist yet, and filepath.EvalSymlinks
// errors on missing paths), so two candidates that point at the same physical
// directory through a symlink collapse to a single write.
func skillTargetDirs(target string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	var candidates []string
	switch target {
	case "claude":
		candidates = append(candidates, claudeSkillDir(home))
	case "codex":
		candidates = append(candidates, codexSkillDirs(home)...)
	case "all":
		candidates = append(candidates, claudeSkillDir(home))
		candidates = append(candidates, codexSkillDirs(home)...)
	default:
		return nil, fmt.Errorf("unknown skill target %q (want claude, codex, or all)", target)
	}

	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		key := resolveDedupeKey(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out, nil
}

// claudeSkillDir is the Claude Code (and Cursor/OpenCode) skill directory.
func claudeSkillDir(home string) string {
	return filepath.Join(home, ".claude", "skills", "cdeploy")
}

// codexSkillDirs are the two Codex/Gemini/Amp skill directories: the shared
// ~/.agents convention plus the legacy/current $CODEX_HOME location. An empty
// $CODEX_HOME is treated as unset and falls back to ~/.codex.
func codexSkillDirs(home string) []string {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	return []string{
		filepath.Join(home, ".agents", "skills", "cdeploy"),
		filepath.Join(codexHome, "skills", "cdeploy"),
	}
}

// resolveDedupeKey returns a canonical key for path by resolving symlinks on the
// deepest existing ancestor and re-appending the not-yet-existing suffix. Falls
// back to the cleaned absolute path when no ancestor can be resolved.
func resolveDedupeKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}

	cur := abs
	var stripped []string // components peeled off, deepest-first
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(stripped) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, stripped[i])
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without an existing ancestor.
			return abs
		}
		stripped = append(stripped, filepath.Base(cur))
		cur = parent
	}
}

// renderSkill returns the canonical SKILL.md with a metadata stamp injected into
// the frontmatter's CLOSING fence. The injected block is exactly:
//
//	metadata:
//	  cdeploy-version: <version>
//	  cdeploy-content-hash: sha256:<64 hex of sha256(canonical)>
//
// It is inserted immediately before the frontmatter's closing `---` (the second
// `---` in the file) — never a bare `---` that may appear in the markdown body —
// so the frontmatter stays the first bytes of the file. stripOwned reverses this
// exactly, giving stripOwned(renderSkill(canonical, v)) == canonical.
func renderSkill(canonical []byte, version string) []byte {
	closeStart, ok := frontmatterCloseOffset(canonical)
	if !ok {
		// Not frontmatter-shaped; nothing to inject. The embedded canonical is
		// pinned well-formed by skills_test, so this is defensive only.
		return canonical
	}

	sum := sha256.Sum256(canonical)
	block := fmt.Sprintf("metadata:\n  cdeploy-version: %s\n  cdeploy-content-hash: sha256:%s\n",
		version, hex.EncodeToString(sum[:]))

	out := make([]byte, 0, len(canonical)+len(block))
	out = append(out, canonical[:closeStart]...)
	out = append(out, block...)
	out = append(out, canonical[closeStart:]...)
	return out
}

// frontmatterCloseOffset returns the byte offset where the frontmatter's closing
// `---` line begins. The file must open with a `---\n` fence as its first bytes;
// the close is the FIRST subsequent line starting with `---`, so a bare `---`
// later in the body is never mistaken for the fence.
func frontmatterCloseOffset(data []byte) (int, bool) {
	open := []byte("---\n")
	if !bytes.HasPrefix(data, open) {
		return 0, false
	}
	rest := data[len(open):]
	idx := bytes.Index(rest, []byte("\n---"))
	if idx < 0 {
		return 0, false
	}
	// rest[idx] is the '\n'; the fence line itself starts one byte later.
	return len(open) + idx + 1, true
}

// stripOwned removes the entire injected metadata block — the `metadata:` header
// line plus both `cdeploy-*` lines — so the result byte-equals the canonical
// bytes the stamp was computed from. Lines that are not our injected block are
// left untouched; a lone `metadata:` with no cdeploy child lines is preserved.
func stripOwned(onDisk []byte) []byte {
	lines := strings.SplitAfter(string(onDisk), "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\n") == "metadata:" {
			// Peel consecutive cdeploy-owned child lines under this header.
			j := i + 1
			owned := false
			for j < len(lines) {
				t := strings.TrimRight(lines[j], "\n")
				if strings.HasPrefix(t, "  cdeploy-version:") || strings.HasPrefix(t, "  cdeploy-content-hash:") {
					owned = true
					j++
					continue
				}
				break
			}
			if owned {
				// Drop the `metadata:` header and its cdeploy child lines.
				i = j - 1
				continue
			}
		}
		out = append(out, lines[i])
	}
	return []byte(strings.Join(out, ""))
}

// verifySkillFile answers only "is this file consistent with its OWN stamp?" —
// stripOwned → sha256 → compare to the hash the file records for itself. It
// returns the recorded version (empty when unstamped) and one of the
// skillState* constants.
func verifySkillFile(onDisk []byte) (stampVersion string, state int) {
	version, contentHash, ok := parseStamp(onDisk)
	if !ok {
		return "", skillStateUnstamped
	}
	sum := sha256.Sum256(stripOwned(onDisk))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if contentHash == want {
		return version, skillStateIntact
	}
	return version, skillStateModified
}

// parseStamp extracts the injected cdeploy-version and cdeploy-content-hash from
// the file's frontmatter. ok is true only when a content-hash line is present
// (the presence of the hash is what makes a file "stamped").
func parseStamp(onDisk []byte) (version, contentHash string, ok bool) {
	closeStart, found := frontmatterCloseOffset(onDisk)
	if !found {
		return "", "", false
	}
	// Only scan the frontmatter region so a stray body line can't be read as a
	// stamp.
	fm := string(onDisk[:closeStart])
	for _, line := range strings.Split(fm, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "cdeploy-version:"):
			version = strings.TrimSpace(strings.TrimPrefix(t, "cdeploy-version:"))
		case strings.HasPrefix(t, "cdeploy-content-hash:"):
			contentHash = strings.TrimSpace(strings.TrimPrefix(t, "cdeploy-content-hash:"))
		}
	}
	return version, contentHash, contentHash != ""
}

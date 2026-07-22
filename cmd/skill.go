package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lexxzar/compose-deploy/skills"
	"github.com/spf13/cobra"
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

// newSkillCmd builds the `skill` parent command and attaches the install, show,
// and uninstall subcommands.
func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Install the cdeploy Agent Skill for AI coding agents",
		Long: `Install the embedded cdeploy Agent Skill (SKILL.md) into AI coding-agent
skill directories so agents learn how to install, configure, and drive cdeploy.

The skill content is version-locked to this binary. Files installed by cdeploy
carry a content-hash stamp; user-edited or externally-installed files are never
overwritten or removed without --force.

For project-level placement, write the raw skill to your repo instead:

  cdeploy skill show > .claude/skills/cdeploy/SKILL.md`,
	}
	cmd.AddCommand(newSkillInstallCmd())
	cmd.AddCommand(newSkillShowCmd())
	cmd.AddCommand(newSkillUninstallCmd())
	return cmd
}

// newSkillInstallCmd builds `skill install <claude|codex|all>`.
func newSkillInstallCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "install <claude|codex|all>",
		Short: "Install the cdeploy skill into agent skill directories",
		Long: `Install the cdeploy Agent Skill into the target agent's skill directory.

  claude → ~/.claude/skills/cdeploy/          (also read by Cursor/OpenCode)
  codex  → ~/.agents/skills/cdeploy/ and $CODEX_HOME/skills/cdeploy/
           ($CODEX_HOME defaults to ~/.codex; covers Codex/Gemini/Amp)
  all    → the union of both, deduplicated

Install is idempotent: an unchanged file is reported as such, an older version is
refreshed, and outdated content is updated. A file that cdeploy did not install,
or that was edited after install, is refused unless --force is given.`,
		Example: `  # Install for Claude Code
  cdeploy skill install claude

  # Install for Codex/Gemini/Amp
  cdeploy skill install codex

  # Install everywhere, overwriting user-modified files
  cdeploy skill install all --force`,
		ValidArgs: []string{"claude", "codex", "all"},
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillInstall(cmd, args[0], force)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite user-modified or externally-installed (unstamped) files")

	return cmd
}

// runSkillInstall renders the stamped skill and writes it to every resolved
// destination for target. Per-path outcomes are printed as they happen —
// successes to stdout, refusals/errors to stderr — and one failed destination
// never aborts the others. It returns a non-nil error (non-zero exit) when any
// destination failed.
func runSkillInstall(cmd *cobra.Command, target string, force bool) error {
	dirs, err := skillTargetDirs(target)
	if err != nil {
		return err
	}

	canonical := skills.CanonicalSkill()
	rendered := renderSkill(canonical, version)

	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	var (
		failures  int
		succeeded []string
	)
	for _, dir := range dirs {
		skillPath := filepath.Join(dir, "SKILL.md")
		msg, perr := installSkillPath(dir, skillPath, canonical, rendered, version, force)
		if perr != nil {
			fmt.Fprintln(errOut, msg)
			failures++
			continue
		}
		fmt.Fprintln(out, msg)
		succeeded = append(succeeded, skillPath)
	}

	if len(succeeded) > 0 {
		printSkillPickupNotes(out, target, succeeded)
	}

	if failures > 0 {
		return fmt.Errorf("%d of %d skill destination(s) failed (see messages above)", failures, len(dirs))
	}
	return nil
}

// installSkillPath applies the per-destination install decision matrix for a
// single SKILL.md path. It returns a human-readable outcome line and a non-nil
// error only when the destination failed (refused or I/O error).
func installSkillPath(dir, skillPath string, canonical, rendered []byte, newVer string, force bool) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Sprintf("error %s: %v", skillPath, err), err
	}

	onDisk, err := os.ReadFile(skillPath)
	if err != nil {
		if os.IsNotExist(err) {
			if werr := os.WriteFile(skillPath, rendered, 0o644); werr != nil {
				return fmt.Sprintf("error %s: %v", skillPath, werr), werr
			}
			return fmt.Sprintf("installed %s (%s)", skillPath, newVer), nil
		}
		return fmt.Sprintf("error %s: %v", skillPath, err), err
	}

	stampVer, state := verifySkillFile(onDisk)
	// contentEqual asks the second, distinct question: does the file's stripped
	// content match THIS binary's canonical bytes? (verifySkillFile only checks
	// self-consistency of the stamp.) Both are required for "unchanged".
	contentEqual := bytes.Equal(stripOwned(onDisk), canonical)

	write := func() (string, error) {
		if werr := os.WriteFile(skillPath, rendered, 0o644); werr != nil {
			return fmt.Sprintf("error %s: %v", skillPath, werr), werr
		}
		return fmt.Sprintf("updated %s (%s → %s)", skillPath, skillVerDisplay(stampVer), newVer), nil
	}

	if force {
		// Overwrite unconditionally, but skip a pointless rewrite when the bytes
		// on disk already equal what we would write.
		if state == skillStateIntact && contentEqual && stampVer == newVer {
			return fmt.Sprintf("unchanged %s", skillPath), nil
		}
		return write()
	}

	switch state {
	case skillStateIntact:
		if contentEqual {
			if stampVer == newVer {
				return fmt.Sprintf("unchanged %s", skillPath), nil
			}
			// Same content, older stamp → refresh the stamp only.
			return write()
		}
		// Stamp is self-consistent but the content predates this binary → update.
		return write()
	default: // skillStateModified or skillStateUnstamped
		reason := "modified since install"
		if state == skillStateUnstamped {
			reason = "not installed by cdeploy (unstamped)"
		}
		return fmt.Sprintf("refused %s: %s; re-run with --force to overwrite", skillPath, reason),
			fmt.Errorf("%s: %s", skillPath, reason)
	}
}

// skillVerDisplay renders a stamp version for outcome messages, labelling the
// empty (unstamped) case explicitly.
func skillVerDisplay(v string) string {
	if v == "" {
		return "unstamped"
	}
	return v
}

const (
	claudePickupNote = "Claude Code: restart Claude Code (or run /skills) to pick up the cdeploy skill."
	codexPickupNote  = "Codex: scans skills at startup — restart Codex to pick up the cdeploy skill."
)

// printSkillPickupNotes lists the resolved SKILL.md paths and the per-agent
// pickup instructions after an install.
func printSkillPickupNotes(w io.Writer, target string, paths []string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Skill file(s):")
	for _, p := range paths {
		fmt.Fprintf(w, "  %s\n", p)
	}
	fmt.Fprintln(w)
	switch target {
	case "claude":
		fmt.Fprintln(w, claudePickupNote)
	case "codex":
		fmt.Fprintln(w, codexPickupNote)
	case "all":
		fmt.Fprintln(w, claudePickupNote)
		fmt.Fprintln(w, codexPickupNote)
	}
}

// newSkillShowCmd builds `skill show`, which writes the raw embedded (unstamped)
// canonical SKILL.md to stdout so it can be redirected into a project-level
// skill directory.
func newSkillShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the embedded cdeploy SKILL.md to stdout",
		Long: `Print the raw embedded cdeploy SKILL.md (without the install-time
content-hash stamp) to stdout.

Use this for project-level placement — redirect it into your repo's skill
directory so the skill travels with the project:

  cdeploy skill show > .claude/skills/cdeploy/SKILL.md`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := cmd.OutOrStdout().Write(skills.CanonicalSkill())
			return err
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	return cmd
}

// newSkillUninstallCmd builds `skill uninstall <claude|codex|all>`.
func newSkillUninstallCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "uninstall <claude|codex|all>",
		Short: "Remove the cdeploy skill from agent skill directories",
		Long: `Remove the cdeploy Agent Skill from the target agent's skill directory.

  claude → ~/.claude/skills/cdeploy/
  codex  → ~/.agents/skills/cdeploy/ and $CODEX_HOME/skills/cdeploy/
           ($CODEX_HOME defaults to ~/.codex)
  all    → the union of both, deduplicated

A destination with no installed skill is reported and treated as success. A
cdeploy-stamped file is removed (and its cdeploy/ directory too, but only when
it becomes empty). A file that cdeploy did not install, or that was edited after
install, is refused unless --force is given.`,
		Example: `  # Remove the skill for Claude Code
  cdeploy skill uninstall claude

  # Remove everywhere, including user-modified files
  cdeploy skill uninstall all --force`,
		ValidArgs: []string{"claude", "codex", "all"},
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillUninstall(cmd, args[0], force)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.Flags().BoolVar(&force, "force", false, "remove user-modified or externally-installed (unstamped) files")

	return cmd
}

// runSkillUninstall removes the SKILL.md from every resolved destination for
// target. Per-path outcomes are printed as they happen — successes to stdout,
// refusals/errors to stderr — and one failed destination never aborts the
// others. It returns a non-nil error (non-zero exit) when any destination was
// refused or failed.
func runSkillUninstall(cmd *cobra.Command, target string, force bool) error {
	dirs, err := skillTargetDirs(target)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	var failures int
	for _, dir := range dirs {
		skillPath := filepath.Join(dir, "SKILL.md")
		msg, perr := uninstallSkillPath(dir, skillPath, force)
		if perr != nil {
			fmt.Fprintln(errOut, msg)
			failures++
			continue
		}
		fmt.Fprintln(out, msg)
	}

	if failures > 0 {
		return fmt.Errorf("%d of %d skill destination(s) failed (see messages above)", failures, len(dirs))
	}
	return nil
}

// uninstallSkillPath applies the per-destination uninstall decision for a single
// SKILL.md path. It returns a human-readable outcome line and a non-nil error
// only when the destination was refused (user-owned without --force) or an I/O
// operation failed. An absent file is a success ("not installed").
func uninstallSkillPath(dir, skillPath string, force bool) (string, error) {
	onDisk, err := os.ReadFile(skillPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("not installed %s", skillPath), nil
		}
		return fmt.Sprintf("error %s: %v", skillPath, err), err
	}

	_, state := verifySkillFile(onDisk)
	if state != skillStateIntact && !force {
		reason := "modified since install"
		if state == skillStateUnstamped {
			reason = "not installed by cdeploy (unstamped)"
		}
		return fmt.Sprintf("refused %s: %s; re-run with --force to remove", skillPath, reason),
			fmt.Errorf("%s: %s", skillPath, reason)
	}

	if rerr := os.Remove(skillPath); rerr != nil {
		return fmt.Sprintf("error %s: %v", skillPath, rerr), rerr
	}
	removeSkillDirIfEmpty(dir)
	return fmt.Sprintf("removed %s", skillPath), nil
}

// removeSkillDirIfEmpty removes the enclosing cdeploy/ directory, but only when
// it is empty — a cdeploy/ dir holding extra user files is retained. Never a
// recursive delete; any error (non-empty, permission) is silently ignored since
// leaving the directory in place is a safe no-op.
func removeSkillDirIfEmpty(dir string) {
	_ = os.Remove(dir)
}

package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/skills"
)

// resolvedJoin resolves symlinks on base (which must exist) and appends the
// not-yet-existing suffix components. This mirrors what skillTargetDirs returns
// when base is the deepest existing ancestor of the target dir, computed
// independently of the helper under test.
func resolvedJoin(t *testing.T, base string, parts ...string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", base, err)
	}
	return filepath.Join(append([]string{r}, parts...)...)
}

func TestSkillTargetDirs_Claude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := skillTargetDirs("claude")
	if err != nil {
		t.Fatalf("skillTargetDirs: %v", err)
	}
	want := []string{resolvedJoin(t, home, ".claude", "skills", "cdeploy")}
	if !equalStrings(got, want) {
		t.Fatalf("claude dirs = %v, want %v", got, want)
	}
}

func TestSkillTargetDirs_CodexWithCodexHome(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)

	got, err := skillTargetDirs("codex")
	if err != nil {
		t.Fatalf("skillTargetDirs: %v", err)
	}
	want := []string{
		resolvedJoin(t, home, ".agents", "skills", "cdeploy"),
		resolvedJoin(t, codexHome, "skills", "cdeploy"),
	}
	if !equalStrings(got, want) {
		t.Fatalf("codex dirs = %v, want %v", got, want)
	}
}

// TestSkillTargetDirs_CodexEmptyCodexHome pins that an EMPTY CODEX_HOME is
// treated as unset and falls back to ~/.codex — t.Setenv can set but never unset
// an env var, so the empty-value branch must be handled.
func TestSkillTargetDirs_CodexEmptyCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	got, err := skillTargetDirs("codex")
	if err != nil {
		t.Fatalf("skillTargetDirs: %v", err)
	}
	want := []string{
		resolvedJoin(t, home, ".agents", "skills", "cdeploy"),
		resolvedJoin(t, home, ".codex", "skills", "cdeploy"),
	}
	if !equalStrings(got, want) {
		t.Fatalf("codex dirs (empty CODEX_HOME) = %v, want %v", got, want)
	}
}

func TestSkillTargetDirs_All(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	got, err := skillTargetDirs("all")
	if err != nil {
		t.Fatalf("skillTargetDirs: %v", err)
	}
	want := []string{
		resolvedJoin(t, home, ".claude", "skills", "cdeploy"),
		resolvedJoin(t, home, ".agents", "skills", "cdeploy"),
		resolvedJoin(t, home, ".codex", "skills", "cdeploy"),
	}
	if !equalStrings(got, want) {
		t.Fatalf("all dirs = %v, want %v", got, want)
	}
}

func TestSkillTargetDirs_UnknownTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := skillTargetDirs("bogus"); err == nil {
		t.Fatal("expected error for unknown target, got nil")
	}
}

// TestSkillTargetDirs_SymlinkDedupe pins that when ~/.codex/skills is a symlink
// to ~/.agents/skills, the two codex candidates collapse to a single physical
// write target.
func TestSkillTargetDirs_SymlinkDedupe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	agentsSkills := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(agentsSkills, 0o755); err != nil {
		t.Fatalf("mkdir agents skills: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir codex home: %v", err)
	}
	// ~/.codex/skills -> ~/.agents/skills
	if err := os.Symlink(agentsSkills, filepath.Join(home, ".codex", "skills")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := skillTargetDirs("codex")
	if err != nil {
		t.Fatalf("skillTargetDirs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 deduped path, got %d: %v", len(got), got)
	}
	want := resolvedJoin(t, agentsSkills, "cdeploy")
	if got[0] != want {
		t.Fatalf("deduped path = %q, want %q", got[0], want)
	}
}

// TestSkillDests_MergesAgentsOnCollision pins that when a claude candidate and a
// codex candidate resolve to the SAME physical directory (here via a
// ~/.claude/skills → ~/.agents/skills symlink), they collapse to a single write
// target whose agents slice records BOTH labels — so both pickup notes fire for
// the one write.
func TestSkillDests_MergesAgentsOnCollision(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	agentsSkills := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(agentsSkills, 0o755); err != nil {
		t.Fatalf("mkdir agents skills: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir claude home: %v", err)
	}
	// ~/.claude/skills -> ~/.agents/skills, so the claude cdeploy dir and the
	// codex ~/.agents cdeploy dir are the same physical path.
	if err := os.Symlink(agentsSkills, filepath.Join(home, ".claude", "skills")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	dests, err := skillDests("all")
	if err != nil {
		t.Fatalf("skillDests: %v", err)
	}

	sharedDir := resolvedJoin(t, agentsSkills, "cdeploy")
	var shared *skillDest
	for i := range dests {
		if dests[i].dir == sharedDir {
			shared = &dests[i]
		}
	}
	if shared == nil {
		t.Fatalf("shared physical dir %q not among dests: %v", sharedDir, dests)
	}
	if !containsStr(shared.agents, "claude") || !containsStr(shared.agents, "codex") {
		t.Fatalf("collapsed dest must record both agents, got %v", shared.agents)
	}
	// The dedupe still holds: agents dir appears exactly once.
	count := 0
	for _, d := range dests {
		if d.dir == sharedDir {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared physical dir appears %d times, want 1: %v", count, dests)
	}
}

// TestRenderStripRoundTrip pins the core invariant:
// stripOwned(renderSkill(canonical, v)) byte-equals canonical.
func TestRenderStripRoundTrip(t *testing.T) {
	canonical := skills.CanonicalSkill()
	rendered := renderSkill(canonical, "v0.7.0")
	stripped := stripOwned(rendered)
	if !bytes.Equal(stripped, canonical) {
		t.Fatalf("stripOwned(renderSkill(canonical)) != canonical\n--- stripped ---\n%s\n--- canonical ---\n%s",
			stripped, canonical)
	}
}

// TestRenderStampAndVerifyIntact checks the stamped hash equals sha256(canonical)
// and that a freshly rendered file verifies as intact with the injected version.
func TestRenderStampAndVerifyIntact(t *testing.T) {
	canonical := skills.CanonicalSkill()
	rendered := renderSkill(canonical, "v1.2.3")

	sum := sha256.Sum256(canonical)
	wantHashLine := "cdeploy-content-hash: sha256:" + hex.EncodeToString(sum[:])
	if !bytes.Contains(rendered, []byte(wantHashLine)) {
		t.Fatalf("rendered file missing expected hash line %q", wantHashLine)
	}

	ver, state := verifySkillFile(rendered)
	if state != skillStateIntact {
		t.Fatalf("state = %d, want intact (%d)", state, skillStateIntact)
	}
	if ver != "v1.2.3" {
		t.Fatalf("version = %q, want v1.2.3", ver)
	}
}

// TestRenderMetadataInsideFrontmatter pins that the metadata block lands inside
// the frontmatter, the frontmatter stays the first bytes of the file, and the
// injection targets the frontmatter's CLOSING fence even when the BODY contains
// a bare `---` line.
func TestRenderMetadataInsideFrontmatter(t *testing.T) {
	canonical := []byte("---\n" +
		"name: cdeploy\n" +
		"description: test skill\n" +
		"---\n" +
		"\n" +
		"# cdeploy\n" +
		"\n" +
		"Some prose.\n" +
		"\n" +
		"---\n" +
		"A horizontal rule in the body.\n")

	rendered := renderSkill(canonical, "vX")

	// Frontmatter must remain the first bytes.
	if !bytes.HasPrefix(rendered, []byte("---\n")) {
		t.Fatalf("rendered file must still start with a `---` fence")
	}

	// Split at the FIRST closing fence — the injected block must be within it,
	// and the body's bare `---` must survive untouched afterward.
	off, ok := frontmatterCloseOffset(rendered)
	if !ok {
		t.Fatal("rendered file has no frontmatter close")
	}
	front := string(rendered[:off])
	if !strings.Contains(front, "metadata:") ||
		!strings.Contains(front, "cdeploy-version: vX") ||
		!strings.Contains(front, "cdeploy-content-hash: sha256:") {
		t.Fatalf("metadata block not inside frontmatter; frontmatter was:\n%s", front)
	}
	// The name/description frontmatter keys and the body rule must be intact.
	if !strings.Contains(front, "name: cdeploy") || !strings.Contains(front, "description: test skill") {
		t.Fatalf("original frontmatter keys lost:\n%s", front)
	}
	if !bytes.Contains(rendered, []byte("A horizontal rule in the body.")) {
		t.Fatal("body content lost during render")
	}

	// Round-trip must still hold for a body-with-bare-fence input.
	if !bytes.Equal(stripOwned(rendered), canonical) {
		t.Fatalf("round-trip failed for body-with-bare-fence input\n--- got ---\n%s", stripOwned(rendered))
	}
}

// TestVerifyModifiedBody pins that editing the body after install is detected as
// modified (the stamp no longer matches the stripped content).
func TestVerifyModifiedBody(t *testing.T) {
	canonical := skills.CanonicalSkill()
	rendered := renderSkill(canonical, "v0.7.0")

	// Tamper: append a line to the body.
	tampered := append(append([]byte{}, rendered...), []byte("\nUSER EDIT\n")...)

	ver, state := verifySkillFile(tampered)
	if state != skillStateModified {
		t.Fatalf("state = %d, want modified (%d)", state, skillStateModified)
	}
	// The recorded version is still readable even when modified.
	if ver != "v0.7.0" {
		t.Fatalf("version = %q, want v0.7.0", ver)
	}
}

// TestVerifyUnstamped pins that a file with no stamp (e.g. dropped by `npx
// skills`) is reported as unstamped with an empty version.
func TestVerifyUnstamped(t *testing.T) {
	canonical := skills.CanonicalSkill()

	ver, state := verifySkillFile(canonical)
	if state != skillStateUnstamped {
		t.Fatalf("state = %d, want unstamped (%d)", state, skillStateUnstamped)
	}
	if ver != "" {
		t.Fatalf("version = %q, want empty for unstamped file", ver)
	}
}

// TestVerifyDevVersionStamp pins that the dev-build version stamp round-trips and
// verifies intact (update detection keys on content hash, not version).
func TestVerifyDevVersionStamp(t *testing.T) {
	canonical := skills.CanonicalSkill()
	rendered := renderSkill(canonical, "dev")

	ver, state := verifySkillFile(rendered)
	if state != skillStateIntact {
		t.Fatalf("state = %d, want intact (%d)", state, skillStateIntact)
	}
	if ver != "dev" {
		t.Fatalf("version = %q, want dev", ver)
	}
}

// TestVerifyForgedStampInBodyIgnored pins the integrity property that parseStamp
// scans ONLY the frontmatter region — a forged metadata / cdeploy-content-hash
// block planted in the BODY must not be read as a stamp, so an otherwise
// unstamped file stays unstamped (never mistaken for intact).
func TestVerifyForgedStampInBodyIgnored(t *testing.T) {
	canonical := skills.CanonicalSkill() // canonical frontmatter carries no stamp
	forged := append(append([]byte{}, canonical...),
		[]byte("\nmetadata:\n  cdeploy-version: v9.9.9\n  cdeploy-content-hash: sha256:"+
			strings.Repeat("a", 64)+"\n")...)

	ver, state := verifySkillFile(forged)
	if state != skillStateUnstamped {
		t.Fatalf("forged body stamp must not be read; state = %d, want unstamped (%d)", state, skillStateUnstamped)
	}
	if ver != "" {
		t.Fatalf("version = %q, want empty (body stamp must be ignored)", ver)
	}
}

// TestVerifyStampedFileWithBodyInjectedStampModified pins the integrity property
// that stripOwned scans ONLY the frontmatter region. A genuinely stamped file
// with a forged `metadata:`/`cdeploy-content-hash:` block appended to the BODY
// must be reported as MODIFIED — not intact. Were stripOwned to remove the
// body-injected block (as a whole-file scan would), the stripped content would
// hash straight back to the canonical and the tampered file would verify as
// intact, letting install/uninstall overwrite or delete it without --force.
func TestVerifyStampedFileWithBodyInjectedStampModified(t *testing.T) {
	canonical := skills.CanonicalSkill()
	stamped := renderSkill(canonical, version) // legit frontmatter stamp = sha256(canonical)

	// The canonical ends with a trailing newline, so this block lands as fresh
	// lines in the BODY, after the frontmatter's closing `---`. A whole-file
	// stripOwned would peel it back to the canonical and mis-verify as intact.
	bodyBlock := "metadata:\n  cdeploy-version: v9.9.9\n  cdeploy-content-hash: sha256:" +
		strings.Repeat("a", 64) + "\n"
	tampered := append(append([]byte{}, stamped...), []byte(bodyBlock)...)

	_, state := verifySkillFile(tampered)
	if state != skillStateModified {
		t.Fatalf("body-injected metadata block must be treated as a modification; state = %d, want modified (%d)", state, skillStateModified)
	}

	// The body block itself must survive stripOwned byte-for-byte (only the
	// frontmatter stamp is removed) — the body is never touched.
	stripped := stripOwned(tampered)
	if !bytes.Contains(stripped, []byte(bodyBlock)) {
		t.Fatalf("stripOwned removed a body-injected metadata block; the body must be left untouched")
	}
	// And parseStamp still reads only the real frontmatter stamp.
	ver, hash, ok := parseStamp(tampered)
	if !ok || ver != version {
		t.Fatalf("parseStamp should read the real frontmatter stamp: ver=%q hash=%q ok=%v", ver, hash, ok)
	}
}

// runSkillCLI executes `skill <verb> <args...>` through the real root command,
// capturing stdout/stderr so tests can assert on the routed output.
func runSkillCLI(t *testing.T, verb string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCmd()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"skill", verb}, args...))
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// runRootArgs executes an arbitrary root argv (so tests can place persistent
// flags BEFORE the subcommand, e.g. `--ssh user@host skill show`), capturing
// stdout/stderr.
func runRootArgs(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCmd()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// TestSkillRejectsRemoteFlags pins that every skill verb rejects the inherited
// root remote/project globals (--server/--ssh/--identity/--project-dir) with an
// error naming the offending flag — skill installs into LOCAL user-level dirs
// only. Both flag placements are covered (post-subcommand via runSkillCLI and
// pre-subcommand via runRootArgs); the PersistentPreRunE reads the shared package
// global, which is set the same way regardless of position.
func TestSkillRejectsRemoteFlags(t *testing.T) {
	flags := []struct {
		flag string
		val  string
	}{
		{"--server", "prod"},
		{"--ssh", "user@host"},
		{"--identity", "/tmp/some-key"},
		{"--project-dir", "/tmp/proj"},
	}
	// Each verb with its own required positional args.
	verbs := [][]string{
		{"show"},
		{"install", "claude"},
		{"uninstall", "claude"},
	}

	for _, f := range flags {
		for _, verb := range verbs {
			t.Run(strings.TrimLeft(f.flag, "-")+"_"+verb[0]+"_post", func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				t.Setenv("CODEX_HOME", "")

				// `skill <verb> [pos...] <flag> <val>`
				extra := append(append([]string{}, verb[1:]...), f.flag, f.val)
				_, _, err := runSkillCLI(t, verb[0], extra...)
				if err == nil {
					t.Fatalf("expected error for `skill %s %s`, got nil", strings.Join(verb, " "), f.flag)
				}
				if !strings.Contains(err.Error(), f.flag) {
					t.Fatalf("error %q must name the offending flag %q", err, f.flag)
				}
				if !strings.Contains(err.Error(), "local") {
					t.Fatalf("error %q should explain skill is local-only", err)
				}
			})

			t.Run(strings.TrimLeft(f.flag, "-")+"_"+verb[0]+"_pre", func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				t.Setenv("CODEX_HOME", "")

				// `<flag> <val> skill <verb> [pos...]`
				args := append([]string{f.flag, f.val, "skill"}, verb...)
				_, _, err := runRootArgs(t, args...)
				if err == nil {
					t.Fatalf("expected error for `%s skill %s`, got nil", f.flag, strings.Join(verb, " "))
				}
				if !strings.Contains(err.Error(), f.flag) {
					t.Fatalf("error %q must name the offending flag %q", err, f.flag)
				}
			})
		}
	}
}

// TestSkillNoGlobalFlagsSucceeds pins the positive path: with none of the
// remote/project globals set, each skill verb runs to a zero exit — the guard
// must not fire on a plain invocation.
func TestSkillNoGlobalFlagsSucceeds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	if _, stderr, err := runSkillCLI(t, "show"); err != nil {
		t.Fatalf("skill show: %v (stderr=%s)", err, stderr)
	}
	if _, stderr, err := runSkillCLI(t, "install", "claude"); err != nil {
		t.Fatalf("skill install claude: %v (stderr=%s)", err, stderr)
	}
	if _, stderr, err := runSkillCLI(t, "uninstall", "claude"); err != nil {
		t.Fatalf("skill uninstall claude: %v (stderr=%s)", err, stderr)
	}
}

// claudeSkillFile resolves the on-disk SKILL.md path for the claude target,
// matching what the install command writes (symlink-resolved).
func claudeSkillFile(t *testing.T) string {
	t.Helper()
	dirs, err := skillTargetDirs("claude")
	if err != nil {
		t.Fatalf("skillTargetDirs: %v", err)
	}
	return filepath.Join(dirs[0], "SKILL.md")
}

// TestSkillInstall_Fresh: a fresh HOME yields "installed" and writes the stamped
// canonical file.
func TestSkillInstall_Fresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	stdout, stderr, err := runSkillCLI(t, "install", "claude")
	if err != nil {
		t.Fatalf("install: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "installed ") {
		t.Fatalf("stdout missing 'installed': %q", stdout)
	}

	path := claudeSkillFile(t)
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read installed file: %v", rerr)
	}
	want := renderSkill(skills.CanonicalSkill(), version)
	if !bytes.Equal(got, want) {
		t.Fatalf("installed file != rendered skill")
	}
	if !strings.Contains(stdout, path) {
		t.Fatalf("stdout missing resolved path %q:\n%s", path, stdout)
	}
	if !strings.Contains(stdout, "Claude Code") {
		t.Fatalf("stdout missing claude pickup note:\n%s", stdout)
	}
}

// TestSkillInstall_IdenticalRerun: a second identical install reports unchanged.
func TestSkillInstall_IdenticalRerun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	if _, _, err := runSkillCLI(t, "install", "claude"); err != nil {
		t.Fatalf("first install: %v", err)
	}
	stdout, stderr, err := runSkillCLI(t, "install", "claude")
	if err != nil {
		t.Fatalf("second install: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "unchanged ") {
		t.Fatalf("stdout missing 'unchanged': %q", stdout)
	}
}

// TestSkillInstall_OldContentUpdated: a valid stamp over OLDER content is
// overwritten and reported as updated.
func TestSkillInstall_OldContentUpdated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	path := claudeSkillFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A self-consistent stamp over DIFFERENT (older) canonical content.
	oldCanonical := []byte("---\nname: cdeploy\ndescription: old\n---\n\nOld body.\n")
	if err := os.WriteFile(path, renderSkill(oldCanonical, version), 0o644); err != nil {
		t.Fatalf("write old: %v", err)
	}

	stdout, stderr, err := runSkillCLI(t, "install", "claude")
	if err != nil {
		t.Fatalf("install: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "updated ") {
		t.Fatalf("stdout missing 'updated': %q", stdout)
	}
	got, _ := os.ReadFile(path)
	want := renderSkill(skills.CanonicalSkill(), version)
	if !bytes.Equal(got, want) {
		t.Fatalf("file not overwritten with current canonical")
	}
}

// TestSkillInstall_StampRefresh: EQUAL content but an OLDER version is reported
// as updated, and the on-disk file afterward differs from the pre-install file
// only in the cdeploy-version line.
func TestSkillInstall_StampRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	canonical := skills.CanonicalSkill()
	path := claudeSkillFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Same canonical content, older version stamp.
	before := renderSkill(canonical, "v0.0.0-old")
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	stdout, stderr, err := runSkillCLI(t, "install", "claude")
	if err != nil {
		t.Fatalf("install: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "updated ") {
		t.Fatalf("stamp refresh should report 'updated': %q", stdout)
	}

	after, _ := os.ReadFile(path)
	if bytes.Equal(after, before) {
		t.Fatal("file unchanged; expected a stamp refresh")
	}
	if !bytes.Equal(after, renderSkill(canonical, version)) {
		t.Fatal("file not rewritten with current version stamp")
	}
	// The only differing line must be cdeploy-version.
	assertOnlyVersionLineDiffers(t, before, after)
}

// TestSkillInstall_TamperedRefusedThenForced: a modified file is refused without
// --force and overwritten with it.
func TestSkillInstall_TamperedRefusedThenForced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	canonical := skills.CanonicalSkill()
	path := claudeSkillFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tampered := append(append([]byte{}, renderSkill(canonical, version)...), []byte("\nUSER EDIT\n")...)
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Without --force: refused, non-zero exit, file untouched.
	stdout, stderr, err := runSkillCLI(t, "install", "claude")
	if err == nil {
		t.Fatal("expected error when refusing a modified file")
	}
	if !strings.Contains(stderr, "refused ") {
		t.Fatalf("stderr missing 'refused': %q", stderr)
	}
	if strings.Contains(stdout, "updated ") {
		t.Fatalf("modified file must not be updated without --force: %q", stdout)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, tampered) {
		t.Fatal("modified file changed despite refusal")
	}

	// With --force: overwritten.
	stdout, stderr, err = runSkillCLI(t, "install", "claude", "--force")
	if err != nil {
		t.Fatalf("forced install: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "updated ") {
		t.Fatalf("forced install should report 'updated': %q", stdout)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, renderSkill(canonical, version)) {
		t.Fatal("forced install did not overwrite with current canonical")
	}
}

// TestSkillInstall_UnstampedRefusedThenForced: a pre-existing unstamped file
// (e.g. dropped by `npx skills`) is refused without --force.
func TestSkillInstall_UnstampedRefusedThenForced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	canonical := skills.CanonicalSkill()
	path := claudeSkillFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Raw canonical: no stamp at all.
	if err := os.WriteFile(path, canonical, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	stdout, stderr, err := runSkillCLI(t, "install", "claude")
	if err == nil {
		t.Fatal("expected error when refusing an unstamped file")
	}
	if !strings.Contains(stderr, "refused ") || !strings.Contains(stderr, "unstamped") {
		t.Fatalf("stderr missing unstamped refusal: %q", stderr)
	}
	if strings.Contains(stdout, "updated ") {
		t.Fatalf("unstamped file must not be updated without --force: %q", stdout)
	}

	// With --force: overwritten with the stamped canonical.
	if _, stderr, err = runSkillCLI(t, "install", "claude", "--force"); err != nil {
		t.Fatalf("forced install: %v (stderr=%s)", err, stderr)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, renderSkill(canonical, version)) {
		t.Fatal("forced install did not stamp the unstamped file")
	}
}

// TestSkillInstall_AggregatesFailures: a plain FILE where the claude cdeploy dir
// should be obstructs MkdirAll, so the claude path fails while the codex paths
// still get written and the command returns a non-zero result.
func TestSkillInstall_AggregatesFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	// Obstruct the claude destination with a regular file at .../skills/cdeploy.
	claudeSkills := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(claudeSkills, 0o755); err != nil {
		t.Fatalf("mkdir claude skills: %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeSkills, "cdeploy"), []byte("obstruction"), 0o644); err != nil {
		t.Fatalf("write obstruction: %v", err)
	}

	stdout, stderr, err := runSkillCLI(t, "install", "all")
	if err == nil {
		t.Fatal("expected non-zero result when a destination fails")
	}
	if !strings.Contains(stderr, "error ") {
		t.Fatalf("stderr missing claude error line: %q", stderr)
	}

	// Codex paths must still be written despite the claude failure.
	codexDirs, derr := skillTargetDirs("codex")
	if derr != nil {
		t.Fatalf("skillTargetDirs codex: %v", derr)
	}
	want := renderSkill(skills.CanonicalSkill(), version)
	for _, dir := range codexDirs {
		p := filepath.Join(dir, "SKILL.md")
		got, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatalf("codex path %q not written: %v", p, rerr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("codex path %q content mismatch", p)
		}
		if !strings.Contains(stdout, p) {
			t.Fatalf("stdout missing successful codex path %q:\n%s", p, stdout)
		}
	}

	// Pickup notes must reflect the destinations that actually succeeded: the
	// codex note prints (its paths were written) but the claude note must NOT,
	// since the claude destination failed.
	if !strings.Contains(stdout, "Codex") {
		t.Fatalf("stdout missing codex pickup note for the succeeded codex paths:\n%s", stdout)
	}
	if strings.Contains(stdout, "Claude Code") {
		t.Fatalf("claude pickup note printed despite the claude destination failing:\n%s", stdout)
	}
}

// TestSkillInstall_Codex drives the standalone codex dual-write happy path: both
// resolved SKILL.md paths are written, only the codex pickup note is printed, and
// an identical rerun reports unchanged for both.
func TestSkillInstall_Codex(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	stdout, stderr, err := runSkillCLI(t, "install", "codex")
	if err != nil {
		t.Fatalf("install codex: %v (stderr=%s)", err, stderr)
	}

	dirs, derr := skillTargetDirs("codex")
	if derr != nil {
		t.Fatalf("skillTargetDirs codex: %v", derr)
	}
	if len(dirs) != 2 {
		t.Fatalf("expected 2 codex dirs (agents + codex home), got %d: %v", len(dirs), dirs)
	}
	if n := strings.Count(stdout, "installed "); n != 2 {
		t.Fatalf("expected 2 'installed' lines, got %d:\n%s", n, stdout)
	}

	want := renderSkill(skills.CanonicalSkill(), version)
	for _, dir := range dirs {
		p := filepath.Join(dir, "SKILL.md")
		got, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatalf("codex path %q not written: %v", p, rerr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("codex path %q content != rendered skill", p)
		}
		if !strings.Contains(stdout, p) {
			t.Fatalf("stdout missing resolved codex path %q:\n%s", p, stdout)
		}
	}

	// Only the codex pickup note — never the claude one.
	if !strings.Contains(stdout, "Codex") {
		t.Fatalf("stdout missing codex pickup note:\n%s", stdout)
	}
	if strings.Contains(stdout, "Claude Code") {
		t.Fatalf("codex install must not print the claude pickup note:\n%s", stdout)
	}

	// Idempotent rerun: both destinations report unchanged.
	stdout2, stderr2, err := runSkillCLI(t, "install", "codex")
	if err != nil {
		t.Fatalf("codex rerun: %v (stderr=%s)", err, stderr2)
	}
	if n := strings.Count(stdout2, "unchanged "); n != 2 {
		t.Fatalf("expected 2 'unchanged' lines on rerun, got %d:\n%s", n, stdout2)
	}
}

// TestSkillInstall_CollapsedDirEmitsBothPickupNotes pins the merged-agent
// behaviour end-to-end: when ALL candidate dirs (claude, codex-agents, and
// codex-home) collapse to one physical path via symlinks, that path is written
// exactly ONCE but BOTH pickup notes still print, because both agents read it.
// The all-collapse setup makes this a true regression pin — the codex note can
// ONLY come from the merged dest (there is no separate codex path to leak it), so
// a dedupe that dropped the secondary agent label would omit the Codex note.
func TestSkillInstall_CollapsedDirEmitsBothPickupNotes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	agentsSkills := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(agentsSkills, 0o755); err != nil {
		t.Fatalf("mkdir agents skills: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir claude home: %v", err)
	}
	// ~/.claude/skills -> ~/.agents/skills  (collapses the claude candidate)
	if err := os.Symlink(agentsSkills, filepath.Join(home, ".claude", "skills")); err != nil {
		t.Fatalf("symlink claude skills: %v", err)
	}
	// ~/.codex -> ~/.agents  (so ~/.codex/skills -> ~/.agents/skills, collapsing
	// the codex-home candidate too)
	if err := os.Symlink(filepath.Join(home, ".agents"), filepath.Join(home, ".codex")); err != nil {
		t.Fatalf("symlink codex home: %v", err)
	}

	stdout, stderr, err := runSkillCLI(t, "install", "all")
	if err != nil {
		t.Fatalf("install all: %v (stderr=%s)", err, stderr)
	}

	// All three candidates collapse to one physical dir → exactly one write.
	if n := strings.Count(stdout, "installed "); n != 1 {
		t.Fatalf("expected 1 'installed' line (all candidates collapse), got %d:\n%s", n, stdout)
	}
	sharedFile := resolvedJoin(t, agentsSkills, "cdeploy", "SKILL.md")
	if !strings.Contains(stdout, "installed "+sharedFile) {
		t.Fatalf("shared dir not installed at %q, stdout:\n%s", sharedFile, stdout)
	}

	// Both pickup notes must fire even though the shared dir was written once.
	if !strings.Contains(stdout, "Claude Code") {
		t.Fatalf("stdout missing claude pickup note for the collapsed dir:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Codex") {
		t.Fatalf("stdout missing codex pickup note for the collapsed dir:\n%s", stdout)
	}
}

// TestSkillUninstall_Codex drives standalone codex uninstall: both dual-write
// SKILL.md paths (and their now-empty cdeploy/ dirs) are removed.
func TestSkillUninstall_Codex(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	if _, _, err := runSkillCLI(t, "install", "codex"); err != nil {
		t.Fatalf("install codex: %v", err)
	}
	dirs, derr := skillTargetDirs("codex")
	if derr != nil {
		t.Fatalf("skillTargetDirs codex: %v", derr)
	}

	stdout, stderr, err := runSkillCLI(t, "uninstall", "codex")
	if err != nil {
		t.Fatalf("uninstall codex: %v (stderr=%s)", err, stderr)
	}
	if n := strings.Count(stdout, "removed "); n != 2 {
		t.Fatalf("expected 2 'removed' lines, got %d:\n%s", n, stdout)
	}
	for _, dir := range dirs {
		p := filepath.Join(dir, "SKILL.md")
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Fatalf("codex SKILL.md %q still present after uninstall: %v", p, serr)
		}
		if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
			t.Fatalf("empty codex cdeploy/ dir %q not removed: %v", dir, serr)
		}
	}
}

// TestSkillInstall_ForceUnchangedSkipsRewrite pins the --force short-circuit: when
// the on-disk file is already the current stamped canonical, --force reports
// unchanged (not updated) and leaves the bytes untouched.
func TestSkillInstall_ForceUnchangedSkipsRewrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	if _, _, err := runSkillCLI(t, "install", "claude"); err != nil {
		t.Fatalf("install: %v", err)
	}
	path := claudeSkillFile(t)
	before, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read installed file: %v", rerr)
	}

	stdout, stderr, err := runSkillCLI(t, "install", "claude", "--force")
	if err != nil {
		t.Fatalf("forced install over current file: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "unchanged ") {
		t.Fatalf("--force over the current file should report 'unchanged': %q", stdout)
	}
	if strings.Contains(stdout, "updated ") {
		t.Fatalf("--force over an identical current file must not rewrite: %q", stdout)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("file bytes changed despite the --force unchanged short-circuit")
	}
}

// assertOnlyVersionLineDiffers fails unless a and b differ on exactly one line,
// and that line is the cdeploy-version stamp.
func assertOnlyVersionLineDiffers(t *testing.T, a, b []byte) {
	t.Helper()
	la := strings.Split(string(a), "\n")
	lb := strings.Split(string(b), "\n")
	if len(la) != len(lb) {
		t.Fatalf("line count differs: %d vs %d", len(la), len(lb))
	}
	diffs := 0
	for i := range la {
		if la[i] != lb[i] {
			diffs++
			if !strings.Contains(la[i], "cdeploy-version:") {
				t.Fatalf("unexpected differing line %d: %q vs %q", i, la[i], lb[i])
			}
		}
	}
	if diffs != 1 {
		t.Fatalf("expected exactly 1 differing line, got %d", diffs)
	}
}

// TestSkillShow_ByteEqualsCanonical: `skill show` writes the raw embedded
// (unstamped) SKILL.md — captured via cmd.SetOut — byte-for-byte.
func TestSkillShow_ByteEqualsCanonical(t *testing.T) {
	stdout, stderr, err := runSkillCLI(t, "show")
	if err != nil {
		t.Fatalf("show: %v (stderr=%s)", err, stderr)
	}
	if stdout != string(skills.CanonicalSkill()) {
		t.Fatalf("show output != canonical SKILL.md")
	}
	// The stamp must NOT be present in `show` output (raw canonical only).
	if strings.Contains(stdout, "cdeploy-content-hash:") {
		t.Fatalf("show output must be unstamped, found content-hash line:\n%s", stdout)
	}
}

// TestSkillUninstall_Absent: uninstalling a target that was never installed is a
// success and reports "not installed".
func TestSkillUninstall_Absent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	stdout, stderr, err := runSkillCLI(t, "uninstall", "claude")
	if err != nil {
		t.Fatalf("uninstall absent: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "not installed ") {
		t.Fatalf("stdout missing 'not installed': %q", stdout)
	}
}

// TestSkillUninstall_StampedRemovesFileAndEmptyDir: a properly stamped install is
// removed, and the now-empty cdeploy/ directory is removed too.
func TestSkillUninstall_StampedRemovesFileAndEmptyDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	// Install first so the file is properly stamped.
	if _, _, err := runSkillCLI(t, "install", "claude"); err != nil {
		t.Fatalf("install: %v", err)
	}
	path := claudeSkillFile(t)
	dir := filepath.Dir(path)

	stdout, stderr, err := runSkillCLI(t, "uninstall", "claude")
	if err != nil {
		t.Fatalf("uninstall: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "removed ") {
		t.Fatalf("stdout missing 'removed': %q", stdout)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("SKILL.md still present after uninstall: %v", serr)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Fatalf("empty cdeploy/ dir not removed: %v", serr)
	}
}

// TestSkillUninstall_DirWithExtraFileRetained: when the cdeploy/ dir holds an
// extra user file, the SKILL.md is removed but the directory is retained (never
// rm -rf).
func TestSkillUninstall_DirWithExtraFileRetained(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	if _, _, err := runSkillCLI(t, "install", "claude"); err != nil {
		t.Fatalf("install: %v", err)
	}
	path := claudeSkillFile(t)
	dir := filepath.Dir(path)
	extra := filepath.Join(dir, "NOTES.md")
	if err := os.WriteFile(extra, []byte("user notes"), 0o644); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	stdout, stderr, err := runSkillCLI(t, "uninstall", "claude")
	if err != nil {
		t.Fatalf("uninstall: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "removed ") {
		t.Fatalf("stdout missing 'removed': %q", stdout)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("SKILL.md still present after uninstall: %v", serr)
	}
	// The directory and the extra user file must survive.
	if _, serr := os.Stat(dir); serr != nil {
		t.Fatalf("cdeploy/ dir with extra file was removed: %v", serr)
	}
	if _, serr := os.Stat(extra); serr != nil {
		t.Fatalf("extra user file was removed: %v", serr)
	}
}

// TestSkillUninstall_ModifiedRefusedThenForced: a modified (tampered) file is
// refused without --force and removed with it.
func TestSkillUninstall_ModifiedRefusedThenForced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	path := claudeSkillFile(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tampered := append(append([]byte{}, renderSkill(skills.CanonicalSkill(), version)...), []byte("\nUSER EDIT\n")...)
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	// Without --force: refused, non-zero exit, file untouched.
	stdout, stderr, err := runSkillCLI(t, "uninstall", "claude")
	if err == nil {
		t.Fatal("expected error when refusing a modified file")
	}
	if !strings.Contains(stderr, "refused ") {
		t.Fatalf("stderr missing 'refused': %q", stderr)
	}
	if strings.Contains(stdout, "removed ") {
		t.Fatalf("modified file must not be removed without --force: %q", stdout)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("modified file removed despite refusal: %v", serr)
	}

	// With --force: removed (and the now-empty dir too).
	stdout, stderr, err = runSkillCLI(t, "uninstall", "claude", "--force")
	if err != nil {
		t.Fatalf("forced uninstall: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "removed ") {
		t.Fatalf("forced uninstall should report 'removed': %q", stdout)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("modified file not removed with --force: %v", serr)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Fatalf("empty cdeploy/ dir not removed with --force: %v", serr)
	}
}

// TestSkillUninstall_UnstampedRefusedThenForced: an unstamped file (e.g. dropped
// by `npx skills`) is refused without --force and removed with it.
func TestSkillUninstall_UnstampedRefusedThenForced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	path := claudeSkillFile(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Raw canonical: no stamp at all.
	if err := os.WriteFile(path, skills.CanonicalSkill(), 0o644); err != nil {
		t.Fatalf("write unstamped: %v", err)
	}

	stdout, stderr, err := runSkillCLI(t, "uninstall", "claude")
	if err == nil {
		t.Fatal("expected error when refusing an unstamped file")
	}
	if !strings.Contains(stderr, "refused ") || !strings.Contains(stderr, "unstamped") {
		t.Fatalf("stderr missing unstamped refusal: %q", stderr)
	}
	if strings.Contains(stdout, "removed ") {
		t.Fatalf("unstamped file must not be removed without --force: %q", stdout)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("unstamped file removed despite refusal: %v", serr)
	}

	// With --force: removed.
	if _, stderr, err = runSkillCLI(t, "uninstall", "claude", "--force"); err != nil {
		t.Fatalf("forced uninstall: %v (stderr=%s)", err, stderr)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("unstamped file not removed with --force: %v", serr)
	}
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

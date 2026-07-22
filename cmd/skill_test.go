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

// runSkillInstall executes `skill install <args...>` through the real root
// command, capturing stdout/stderr so tests can assert on the routed output.
func runSkillInstallCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCmd()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"skill", "install"}, args...))
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
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

	stdout, stderr, err := runSkillInstallCLI(t, "claude")
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

	if _, _, err := runSkillInstallCLI(t, "claude"); err != nil {
		t.Fatalf("first install: %v", err)
	}
	stdout, stderr, err := runSkillInstallCLI(t, "claude")
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

	stdout, stderr, err := runSkillInstallCLI(t, "claude")
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

	stdout, stderr, err := runSkillInstallCLI(t, "claude")
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
	stdout, stderr, err := runSkillInstallCLI(t, "claude")
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
	stdout, stderr, err = runSkillInstallCLI(t, "claude", "--force")
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

	stdout, stderr, err := runSkillInstallCLI(t, "claude")
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
	if _, stderr, err = runSkillInstallCLI(t, "claude", "--force"); err != nil {
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

	stdout, stderr, err := runSkillInstallCLI(t, "all")
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

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

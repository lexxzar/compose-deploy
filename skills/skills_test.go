package skills

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// nameRE is the agentskills.io constraint on a skill's `name`: lowercase
// alphanumeric words joined by single hyphens.
var nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// splitFrontmatter returns the YAML frontmatter block and the body of a SKILL.md.
// It requires the file to start with a `---` fence (frontmatter must be the very
// first bytes) and be terminated by a closing `---` on its own line.
func splitFrontmatter(t *testing.T, data []byte) (frontmatter, body string) {
	t.Helper()
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		t.Fatalf("SKILL.md must start with a `---` frontmatter fence as its first bytes")
	}
	rest := s[len("---\n"):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		t.Fatalf("SKILL.md frontmatter has no closing `---` fence")
	}
	return rest[:idx], rest[idx:]
}

// parseScalar pulls a single-line `key: value` scalar out of the frontmatter.
func parseScalar(frontmatter, key string) (string, bool) {
	for _, line := range strings.Split(frontmatter, "\n") {
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":")), true
		}
	}
	return "", false
}

func TestFSContainsCanonicalSkill(t *testing.T) {
	data, err := FS.ReadFile("cdeploy/SKILL.md")
	if err != nil {
		t.Fatalf("embedded FS missing cdeploy/SKILL.md: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("cdeploy/SKILL.md is empty")
	}
}

func TestCanonicalSkillMatchesFS(t *testing.T) {
	got := CanonicalSkill()
	want, err := FS.ReadFile("cdeploy/SKILL.md")
	if err != nil {
		t.Fatalf("reading embedded SKILL.md: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("CanonicalSkill() bytes differ from FS cdeploy/SKILL.md")
	}
}

func TestSkillStartsWithFrontmatterFence(t *testing.T) {
	data := CanonicalSkill()
	if !bytes.HasPrefix(data, []byte("---\n")) {
		t.Fatalf("SKILL.md must begin with `---` frontmatter as its first bytes; got %q", firstBytes(data, 8))
	}
}

func TestSkillName(t *testing.T) {
	fm, _ := splitFrontmatter(t, CanonicalSkill())
	name, ok := parseScalar(fm, "name")
	if !ok {
		t.Fatal("frontmatter missing `name` key")
	}
	if name != "cdeploy" {
		t.Fatalf("name = %q, want %q (must match the skill directory)", name, "cdeploy")
	}
	if !nameRE.MatchString(name) {
		t.Fatalf("name %q does not match %s", name, nameRE)
	}
}

func TestSkillDescription(t *testing.T) {
	fm, _ := splitFrontmatter(t, CanonicalSkill())
	desc, ok := parseScalar(fm, "description")
	if !ok {
		t.Fatal("frontmatter missing `description` key")
	}
	if desc == "" {
		t.Fatal("description must be non-empty")
	}
	if len(desc) > 1024 {
		t.Fatalf("description length %d exceeds 1024", len(desc))
	}
	// Description must name both operational and setup triggers so agents
	// activate the skill in either context. Avoid "deploy" as a keyword — it is a
	// substring of the product name "cdeploy", so it can never fail; assert on
	// operational verbs ("restart", "logs") that are NOT substrings of the name.
	lower := strings.ToLower(desc)
	for _, kw := range []string{"restart", "logs", "servers.yml", "ssh"} {
		if !strings.Contains(lower, kw) {
			t.Errorf("description should mention %q (ops+setup triggers)", kw)
		}
	}
}

func TestSkillUnderLineLimit(t *testing.T) {
	lines := bytes.Count(CanonicalSkill(), []byte("\n")) + 1
	if lines >= 500 {
		t.Fatalf("SKILL.md has %d lines, must stay under 500", lines)
	}
}

// TestCanonicalHasNoMetadataKey pins the invariant that the canonical embedded
// SKILL.md carries NO `metadata:` key of its own — the install-time renderer
// injects one, and stripping it must round-trip back to exactly these bytes.
func TestCanonicalHasNoMetadataKey(t *testing.T) {
	fm, _ := splitFrontmatter(t, CanonicalSkill())
	for _, line := range strings.Split(fm, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "metadata:") {
			t.Fatalf("canonical frontmatter must not contain a `metadata:` key (injected at install time): %q", line)
		}
	}
}

func firstBytes(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

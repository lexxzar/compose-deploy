package compose

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var hex12RE = regexp.MustCompile(`^[0-9a-f]{12}$`)

func TestSnapshotKeyFormat(t *testing.T) {
	key := snapshotKey("/opt/myapp")
	if !hex12RE.MatchString(key) {
		t.Fatalf("snapshotKey = %q, want 12 lowercase hex chars", key)
	}
	// Deterministic: same input -> same key.
	if got := snapshotKey("/opt/myapp"); got != key {
		t.Fatalf("snapshotKey not deterministic: %q vs %q", got, key)
	}
	// Different input -> different key (sanity).
	if snapshotKey("/opt/other") == key {
		t.Fatalf("distinct dirs produced the same key")
	}
}

func TestLocalProjectDirRelAbsSameKey(t *testing.T) {
	// A relative `-C ./myapp` and the equivalent absolute path must key the
	// same. Resolve the abs path from the SAME cwd localProjectDir uses so the
	// comparison is symlink-safe (macOS TempDir is often a /var -> /private/var
	// symlink and filepath.Abs does not resolve symlinks).
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	relKey := snapshotKey(localProjectDir("./myapp"))
	absKey := snapshotKey(localProjectDir(filepath.Join(cwd, "myapp")))
	if relKey != absKey {
		t.Fatalf("rel vs abs keys differ: rel=%q abs=%q", relKey, absKey)
	}
}

func TestLocalProjectDirTrailingSlashSameKey(t *testing.T) {
	if snapshotKey(localProjectDir("/opt/app/")) != snapshotKey(localProjectDir("/opt/app")) {
		t.Fatalf("trailing slash changed the local key")
	}
	if snapshotKey(localProjectDir("/opt//app")) != snapshotKey(localProjectDir("/opt/app")) {
		t.Fatalf("redundant separators changed the local key")
	}
}

func TestRemoteProjectDirNormalization(t *testing.T) {
	base := snapshotKey(remoteProjectDir("/opt/app"))
	cases := []string{"/opt/app/", "/opt//app", "/opt/./app", "/opt/app/."}
	for _, in := range cases {
		if got := snapshotKey(remoteProjectDir(in)); got != base {
			t.Errorf("remoteProjectDir(%q) keyed differently: got %q want %q", in, got, base)
		}
	}
	// Empty stays empty (caller decides meaning) and does not panic.
	if remoteProjectDir("") != "" {
		t.Errorf("remoteProjectDir(\"\") = %q, want empty", remoteProjectDir(""))
	}
}

func TestStateFileRelPath(t *testing.T) {
	got := stateFileRelPath("/opt/myapp")
	want := ".cdeploy/state/" + snapshotKey("/opt/myapp") + ".json"
	if got != want {
		t.Fatalf("stateFileRelPath = %q, want %q", got, want)
	}
}

func TestMergeSnapshotKeepsOtherServices(t *testing.T) {
	existing := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: "/opt/app",
		Services: map[string]SnapshotEntry{
			"web": {Image: "nginx:1.24", Digest: "sha256:old-web", RecordedAt: "2026-07-01T00:00:00Z"},
			"db":  {Image: "postgres:16", Digest: "sha256:old-db", RecordedAt: "2026-06-01T00:00:00Z"},
		},
	}
	fresh := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: "/opt/app",
		Services: map[string]SnapshotEntry{
			"web": {Image: "nginx:1.25", Digest: "sha256:new-web", RecordedAt: "2026-07-29T00:00:00Z"},
		},
	}

	merged := mergeSnapshot(existing, fresh)

	// web is refreshed from fresh.
	if got := merged.Services["web"]; got.Digest != "sha256:new-web" || got.RecordedAt != "2026-07-29T00:00:00Z" {
		t.Errorf("web not refreshed from fresh: %+v", got)
	}
	// db is untouched, its own recorded_at preserved (merge-not-replace).
	if got := merged.Services["db"]; got.Digest != "sha256:old-db" || got.RecordedAt != "2026-06-01T00:00:00Z" {
		t.Errorf("db not preserved: %+v", got)
	}
	if merged.Schema != snapshotSchemaVersion {
		t.Errorf("schema = %d, want %d", merged.Schema, snapshotSchemaVersion)
	}
	if merged.ProjectDir != "/opt/app" {
		t.Errorf("project_dir = %q, want /opt/app", merged.ProjectDir)
	}
	// Merge must not mutate the inputs.
	if existing.Services["web"].Digest != "sha256:old-web" {
		t.Errorf("mergeSnapshot mutated existing input")
	}
}

func TestMergeSnapshotNilExisting(t *testing.T) {
	fresh := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: "/opt/app",
		Services:   map[string]SnapshotEntry{"web": {Image: "nginx", Digest: "sha256:w"}},
	}
	merged := mergeSnapshot(nil, fresh)
	if merged.Schema != snapshotSchemaVersion || merged.ProjectDir != "/opt/app" {
		t.Fatalf("first-write merge wrong header: %+v", merged)
	}
	if _, ok := merged.Services["web"]; !ok {
		t.Fatalf("first-write merge dropped fresh service")
	}
}

func TestMergeSnapshotNilFreshDefensive(t *testing.T) {
	existing := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: "/opt/app",
		Services:   map[string]SnapshotEntry{"web": {Digest: "sha256:w"}},
	}
	merged := mergeSnapshot(existing, nil)
	if _, ok := merged.Services["web"]; !ok {
		t.Fatalf("nil-fresh merge dropped existing service")
	}
	if merged.ProjectDir != "/opt/app" {
		t.Fatalf("nil-fresh merge lost project_dir")
	}
}

func TestParseSnapshotValid(t *testing.T) {
	data := []byte(`{
	  "schema": 1,
	  "project_dir": "/opt/app",
	  "services": {
	    "web": {"image": "nginx:latest", "digest": "sha256:ab12", "recorded_at": "2026-07-29T14:03:00Z"}
	  }
	}`)
	snap, err := parseSnapshot(data)
	if err != nil {
		t.Fatalf("parseSnapshot valid: unexpected error %v", err)
	}
	if snap.ProjectDir != "/opt/app" {
		t.Errorf("project_dir = %q", snap.ProjectDir)
	}
	entry := snap.Services["web"]
	if entry.Image != "nginx:latest" || entry.Digest != "sha256:ab12" || entry.RecordedAt != "2026-07-29T14:03:00Z" {
		t.Errorf("web entry mismatch: %+v", entry)
	}
}

func TestParseSnapshotBadJSON(t *testing.T) {
	_, err := parseSnapshot([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	// Malformed JSON is distinguishable from a schema error.
	if errors.Is(err, errSnapshotSchema) {
		t.Fatalf("malformed JSON misclassified as schema error: %v", err)
	}
}

func TestParseSnapshotUnknownSchema(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"schema": 2, "project_dir": "/opt/app", "services": {}}`),
		[]byte(`{"project_dir": "/opt/app", "services": {}}`), // missing schema => 0
	} {
		_, err := parseSnapshot(data)
		if !errors.Is(err, errSnapshotSchema) {
			t.Errorf("parseSnapshot(%s) err = %v, want errSnapshotSchema", data, err)
		}
	}
}

func TestParseSnapshotNilServicesNormalized(t *testing.T) {
	snap, err := parseSnapshot([]byte(`{"schema": 1, "project_dir": "/opt/app"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.Services == nil {
		t.Fatal("Services should be normalized to an empty map, got nil")
	}
	if len(snap.Services) != 0 {
		t.Fatalf("Services should be empty, got %d entries", len(snap.Services))
	}
}

func TestBuildOverrideYAMLSingle(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:ab12"},
	}
	got := string(buildOverrideYAML(entries, []string{"web"}))
	want := "services:\n  web:\n    image: nginx@sha256:ab12\n"
	if got != want {
		t.Fatalf("override YAML mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestBuildOverrideYAMLSortedDeterministic(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web":   {Image: "nginx:latest", Digest: "sha256:w"},
		"db":    {Image: "postgres:16", Digest: "sha256:d"},
		"cache": {Image: "redis:7", Digest: "sha256:c"},
	}
	// Pass service names in a non-sorted order; output must be sorted.
	got := string(buildOverrideYAML(entries, []string{"web", "db", "cache"}))
	want := "services:\n" +
		"  cache:\n    image: redis@sha256:c\n" +
		"  db:\n    image: postgres@sha256:d\n" +
		"  web:\n    image: nginx@sha256:w\n"
	if got != want {
		t.Fatalf("override YAML not sorted:\n got %q\nwant %q", got, want)
	}
	// Deterministic across runs (guards against accidental map iteration).
	for i := 0; i < 20; i++ {
		if again := string(buildOverrideYAML(entries, []string{"web", "db", "cache"})); again != got {
			t.Fatalf("override YAML non-deterministic on run %d:\n got %q\nfirst %q", i, again, got)
		}
	}
}

func TestBuildOverrideYAMLSubsetSelection(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:w"},
		"db":  {Image: "postgres:16", Digest: "sha256:d"},
	}
	// Only ask for web; db must not appear.
	got := string(buildOverrideYAML(entries, []string{"web"}))
	want := "services:\n  web:\n    image: nginx@sha256:w\n"
	if got != want {
		t.Fatalf("subset selection wrong:\n got %q\nwant %q", got, want)
	}
}

func TestBuildOverrideYAMLMissingEntrySkipped(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:w"},
	}
	// "api" is requested but absent from entries -> skipped silently.
	got := string(buildOverrideYAML(entries, []string{"api", "web"}))
	want := "services:\n  web:\n    image: nginx@sha256:w\n"
	if got != want {
		t.Fatalf("missing entry not skipped:\n got %q\nwant %q", got, want)
	}
}

func TestBuildOverrideYAMLRepoDerivation(t *testing.T) {
	// stripTag drops the tag but preserves a registry host:port so the digest
	// ref stays valid (localhost:5000/web@sha256:...).
	entries := map[string]SnapshotEntry{
		"web": {Image: "localhost:5000/web:v1", Digest: "sha256:cd34"},
	}
	got := string(buildOverrideYAML(entries, []string{"web"}))
	want := "services:\n  web:\n    image: localhost:5000/web@sha256:cd34\n"
	if got != want {
		t.Fatalf("repo derivation wrong:\n got %q\nwant %q", got, want)
	}
}

func TestBuildOverrideYAMLEmptySelection(t *testing.T) {
	got := string(buildOverrideYAML(map[string]SnapshotEntry{"web": {Image: "nginx", Digest: "sha256:w"}}, nil))
	if got != "services:\n" {
		t.Fatalf("empty selection = %q, want just the services header", got)
	}
}

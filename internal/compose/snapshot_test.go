package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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

// pinSnapshotClock overrides snapshotClock for a deterministic recorded_at and
// restores it via t.Cleanup.
func pinSnapshotClock(t *testing.T, ts time.Time) {
	t.Helper()
	prev := snapshotClock
	snapshotClock = func() time.Time { return ts }
	t.Cleanup(func() { snapshotClock = prev })
}

// snapshotHooks builds a Compose whose outputCmd hook scripts the compose /
// docker calls SnapshotServices makes. configJSON drives `compose config`,
// psJSON drives `compose ps`; containerImageID maps a container ID to the image
// ID that `docker inspect --format '{{.Image}}'` reports; imageRepoDigests maps
// an image ID to the multi-line RepoDigests output of `docker image inspect`.
// A missing containerImageID entry or an imageRepoDigests value of "" models the
// respective docker call returning empty / erroring.
func snapshotComposer(configJSON, psJSON string, containerImageID map[string]string, imageRepoDigests map[string]string) *Compose {
	return &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			switch {
			case len(args) >= 3 && args[1] == "compose" && args[2] == "config":
				return []byte(configJSON), nil
			case len(args) >= 3 && args[1] == "compose" && args[2] == "ps":
				return []byte(psJSON), nil
			case len(args) >= 3 && args[1] == "inspect" && args[2] == "--format":
				// batched `docker inspect --format {{.Image}} <ids...>`
				var b []byte
				for _, id := range args[4:] {
					iid, ok := containerImageID[id]
					if !ok {
						return nil, fmt.Errorf("no such container: %s", id)
					}
					b = append(b, []byte(iid+"\n")...)
				}
				return b, nil
			case len(args) >= 3 && args[1] == "image" && args[2] == "inspect":
				imageID := args[len(args)-1]
				return []byte(imageRepoDigests[imageID]), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", args)
		},
	}
}

func TestSnapshotServicesHappyPath(t *testing.T) {
	pinSnapshotClock(t, time.Date(2026, 7, 29, 14, 3, 0, 0, time.UTC))
	c := snapshotComposer(
		`{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`,
		`[{"ID":"cid-web","Service":"web","State":"running"},{"ID":"cid-db","Service":"db","State":"running"}]`,
		map[string]string{"cid-web": "sha256:img-web", "cid-db": "sha256:img-db"},
		map[string]string{
			"sha256:img-web": "nginx@sha256:ab12\n",
			"sha256:img-db":  "postgres@sha256:cd34\n",
		},
	)
	res, err := c.SnapshotServices(context.Background(), nil)
	if err != nil {
		t.Fatalf("SnapshotServices: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	if res.Snapshot.Schema != snapshotSchemaVersion {
		t.Errorf("schema = %d, want %d", res.Snapshot.Schema, snapshotSchemaVersion)
	}
	if res.Snapshot.ProjectDir != localProjectDir("/proj") {
		t.Errorf("project_dir = %q, want %q", res.Snapshot.ProjectDir, localProjectDir("/proj"))
	}
	web := res.Snapshot.Services["web"]
	if web.Image != "nginx:latest" || web.Digest != "sha256:ab12" || web.RecordedAt != "2026-07-29T14:03:00Z" {
		t.Errorf("web entry wrong: %+v", web)
	}
	db := res.Snapshot.Services["db"]
	if db.Image != "postgres:16" || db.Digest != "sha256:cd34" {
		t.Errorf("db entry wrong: %+v", db)
	}
}

func TestSnapshotServicesNotRunning(t *testing.T) {
	// db is requested but has no running container → warning + absent entry.
	c := snapshotComposer(
		`{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`,
		`[{"ID":"cid-web","Service":"web","State":"running"}]`,
		map[string]string{"cid-web": "sha256:img-web"},
		map[string]string{"sha256:img-web": "nginx@sha256:ab12\n"},
	)
	res, err := c.SnapshotServices(context.Background(), []string{"web", "db"})
	if err != nil {
		t.Fatalf("SnapshotServices: %v", err)
	}
	if _, ok := res.Snapshot.Services["db"]; ok {
		t.Errorf("db should be absent (not running)")
	}
	if _, ok := res.Snapshot.Services["web"]; !ok {
		t.Errorf("web should be present")
	}
	if !warningsContain(res.Warnings, "db", "not running") {
		t.Errorf("expected a 'db not running' warning, got %v", res.Warnings)
	}
}

func TestSnapshotServicesBuildOnlyNoDigest(t *testing.T) {
	// app has an image ref but no registry digest (built locally, never
	// pushed): docker image inspect returns empty RepoDigests → skipped + warn.
	c := snapshotComposer(
		`{"services":{"web":{"image":"nginx:latest"},"app":{"image":"myapp:local"}}}`,
		`[{"ID":"cid-web","Service":"web","State":"running"},{"ID":"cid-app","Service":"app","State":"running"}]`,
		map[string]string{"cid-web": "sha256:img-web", "cid-app": "sha256:img-app"},
		map[string]string{
			"sha256:img-web": "nginx@sha256:ab12\n",
			"sha256:img-app": "", // no RepoDigests
		},
	)
	res, err := c.SnapshotServices(context.Background(), nil)
	if err != nil {
		t.Fatalf("SnapshotServices: %v", err)
	}
	if _, ok := res.Snapshot.Services["app"]; ok {
		t.Errorf("app should be absent (no registry digest)")
	}
	if _, ok := res.Snapshot.Services["web"]; !ok {
		t.Errorf("web should be present")
	}
	if !warningsContain(res.Warnings, "app", "no repository digest") {
		t.Errorf("expected a 'no repository digest' warning for app, got %v", res.Warnings)
	}
}

func TestSnapshotServicesBuildOnlyNoImage(t *testing.T) {
	// builder is build-only (no image in compose config) → absent from the
	// images map → skipped with a "no image in compose config" warning.
	c := snapshotComposer(
		`{"services":{"web":{"image":"nginx:latest"},"builder":{"image":""}}}`,
		`[{"ID":"cid-web","Service":"web","State":"running"}]`,
		map[string]string{"cid-web": "sha256:img-web"},
		map[string]string{"sha256:img-web": "nginx@sha256:ab12\n"},
	)
	res, err := c.SnapshotServices(context.Background(), []string{"web", "builder"})
	if err != nil {
		t.Fatalf("SnapshotServices: %v", err)
	}
	if _, ok := res.Snapshot.Services["builder"]; ok {
		t.Errorf("builder should be absent (build-only)")
	}
	if !warningsContain(res.Warnings, "builder", "no image in compose config") {
		t.Errorf("expected a build-only warning for builder, got %v", res.Warnings)
	}
}

func TestSnapshotServicesScaledOneEntry(t *testing.T) {
	// web is scaled to 2 running replicas; the snapshot records exactly one
	// entry using the first running replica's digest.
	inspectContainerIDs := ""
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			switch {
			case len(args) >= 3 && args[1] == "compose" && args[2] == "config":
				return []byte(`{"services":{"web":{"image":"nginx:latest"}}}`), nil
			case len(args) >= 3 && args[1] == "compose" && args[2] == "ps":
				return []byte(`[{"ID":"cid-web-1","Service":"web","State":"running"},{"ID":"cid-web-2","Service":"web","State":"running"}]`), nil
			case len(args) >= 3 && args[1] == "inspect" && args[2] == "--format":
				// record which container IDs were inspected
				for _, id := range args[4:] {
					inspectContainerIDs += id + " "
				}
				var b []byte
				for range args[4:] {
					b = append(b, []byte("sha256:img-web\n")...)
				}
				return b, nil
			case len(args) >= 3 && args[1] == "image" && args[2] == "inspect":
				return []byte("nginx@sha256:ab12\n"), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", args)
		},
	}
	res, err := c.SnapshotServices(context.Background(), nil)
	if err != nil {
		t.Fatalf("SnapshotServices: %v", err)
	}
	if len(res.Snapshot.Services) != 1 {
		t.Fatalf("want exactly 1 service entry, got %d: %+v", len(res.Snapshot.Services), res.Snapshot.Services)
	}
	if res.Snapshot.Services["web"].Digest != "sha256:ab12" {
		t.Errorf("web digest wrong: %+v", res.Snapshot.Services["web"])
	}
	// Only the FIRST running replica should be inspected (not both).
	if inspectContainerIDs != "cid-web-1 " {
		t.Errorf("inspected containers = %q, want only the first replica", inspectContainerIDs)
	}
}

func TestSnapshotServicesInspectBypassesCompose(t *testing.T) {
	// The top-level docker inspect / image inspect calls must NOT carry the
	// `compose` subcommand (they bypass c.command()).
	var argvs [][]string
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			argvs = append(argvs, append([]string{}, cmd.Args...))
			args := cmd.Args
			switch {
			case len(args) >= 3 && args[1] == "compose" && args[2] == "config":
				return []byte(`{"services":{"web":{"image":"nginx:latest"}}}`), nil
			case len(args) >= 3 && args[1] == "compose" && args[2] == "ps":
				return []byte(`[{"ID":"cid-web","Service":"web","State":"running"}]`), nil
			case len(args) >= 3 && args[1] == "inspect":
				return []byte("sha256:img-web\n"), nil
			case len(args) >= 3 && args[1] == "image" && args[2] == "inspect":
				return []byte("nginx@sha256:ab12\n"), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", args)
		},
	}
	if _, err := c.SnapshotServices(context.Background(), nil); err != nil {
		t.Fatalf("SnapshotServices: %v", err)
	}
	for _, a := range argvs {
		// docker inspect / docker image inspect argvs must not contain compose
		if len(a) >= 2 && (a[1] == "inspect" || a[1] == "image") {
			for _, tok := range a {
				if tok == "compose" {
					t.Errorf("top-level docker argv leaked 'compose': %v", a)
				}
			}
		}
	}
}

// warningsContain reports whether any warning mentions both service and a
// substring — order-independent so tests don't pin the exact message text.
func warningsContain(warnings []string, service, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, service) && strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestReadSnapshotNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := &Compose{ProjectDir: "/proj"}
	snap, err := c.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot on missing file should be nil error, got %v", err)
	}
	if snap != nil {
		t.Fatalf("ReadSnapshot on missing file should return nil snapshot, got %+v", snap)
	}
}

func TestWriteSnapshotRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := &Compose{ProjectDir: "/proj"}

	fresh := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: localProjectDir("/proj"),
		Services: map[string]SnapshotEntry{
			"web": {Image: "nginx:latest", Digest: "sha256:ab12", RecordedAt: "2026-07-29T14:03:00Z"},
		},
	}
	if err := c.WriteSnapshot(context.Background(), fresh); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	// File lands under $HOME/.cdeploy/state/<key>.json.
	path := filepath.Join(home, ".cdeploy", "state", snapshotKey(localProjectDir("/proj"))+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written at %s: %v", path, err)
	}

	got, err := c.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if got == nil || got.Services["web"].Digest != "sha256:ab12" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestWriteSnapshotMergeOnSecondWrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := &Compose{ProjectDir: "/proj"}
	ctx := context.Background()

	first := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: localProjectDir("/proj"),
		Services:   map[string]SnapshotEntry{"web": {Image: "nginx", Digest: "sha256:w", RecordedAt: "2026-07-01T00:00:00Z"}},
	}
	if err := c.WriteSnapshot(ctx, first); err != nil {
		t.Fatalf("first WriteSnapshot: %v", err)
	}
	// Second write only touches db; merge must keep web alive with its own
	// recorded_at.
	second := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: localProjectDir("/proj"),
		Services:   map[string]SnapshotEntry{"db": {Image: "postgres", Digest: "sha256:d", RecordedAt: "2026-07-29T00:00:00Z"}},
	}
	if err := c.WriteSnapshot(ctx, second); err != nil {
		t.Fatalf("second WriteSnapshot: %v", err)
	}

	got, err := c.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if got.Services["web"].Digest != "sha256:w" || got.Services["web"].RecordedAt != "2026-07-01T00:00:00Z" {
		t.Errorf("web not preserved across merge: %+v", got.Services["web"])
	}
	if got.Services["db"].Digest != "sha256:d" {
		t.Errorf("db not written: %+v", got.Services["db"])
	}
}

func TestWriteSnapshotOverwritesCorruptExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := &Compose{ProjectDir: "/proj"}
	ctx := context.Background()

	// Seed a corrupt state file.
	path := filepath.Join(home, ".cdeploy", "state", snapshotKey(localProjectDir("/proj"))+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}

	// ReadSnapshot surfaces the corruption as a typed (non-nil) error.
	if _, err := c.ReadSnapshot(ctx); err == nil {
		t.Fatal("ReadSnapshot on corrupt file should error")
	}

	// WriteSnapshot ignores the corrupt existing file and records the fresh one.
	fresh := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: localProjectDir("/proj"),
		Services:   map[string]SnapshotEntry{"web": {Image: "nginx", Digest: "sha256:w"}},
	}
	if err := c.WriteSnapshot(ctx, fresh); err != nil {
		t.Fatalf("WriteSnapshot over corrupt existing: %v", err)
	}
	got, err := c.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot after overwrite: %v", err)
	}
	if got == nil || got.Services["web"].Digest != "sha256:w" {
		t.Fatalf("fresh snapshot not written over corrupt file: %+v", got)
	}
}

func TestReadSnapshotUnknownSchemaTyped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := &Compose{ProjectDir: "/proj"}
	path := filepath.Join(home, ".cdeploy", "state", snapshotKey(localProjectDir("/proj"))+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":2,"project_dir":"/proj","services":{}}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := c.ReadSnapshot(context.Background())
	if !errors.Is(err, errSnapshotSchema) {
		t.Fatalf("ReadSnapshot schema-2 err = %v, want errSnapshotSchema", err)
	}
}

func TestWriteSnapshotFileAtomicNoPartial(t *testing.T) {
	// Force writeSnapshotFile to fail (parent path is a regular file, so
	// MkdirAll cannot create the state dir) and assert no partial file is left
	// at the target path — the atomic temp+rename pattern must not truncate or
	// create the real file on failure.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "block")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	target := filepath.Join(blocker, "state", "key.json") // blocker is a file, not a dir
	err := writeSnapshotFile(target, &Snapshot{Schema: snapshotSchemaVersion, Services: map[string]SnapshotEntry{}})
	if err == nil {
		t.Fatal("expected writeSnapshotFile to fail when parent is a file")
	}
	// The atomic temp+rename pattern must leave NO file at the real target on
	// failure — a successful Stat here would mean a partial/truncated write.
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("a file was created at the target despite the write failing")
	}
}

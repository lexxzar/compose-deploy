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
	"sync"
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
	want := "services:\n  web:\n    image: nginx@sha256:ab12\n    pull_policy: never\n"
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
		"  cache:\n    image: redis@sha256:c\n    pull_policy: never\n" +
		"  db:\n    image: postgres@sha256:d\n    pull_policy: never\n" +
		"  web:\n    image: nginx@sha256:w\n    pull_policy: never\n"
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
	want := "services:\n  web:\n    image: nginx@sha256:w\n    pull_policy: never\n"
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
	want := "services:\n  web:\n    image: nginx@sha256:w\n    pull_policy: never\n"
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
	want := "services:\n  web:\n    image: localhost:5000/web@sha256:cd34\n    pull_policy: never\n"
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

// TestBuildOverrideYAMLPullPolicyNever pins the offline-rollback guarantee
// (AC4): every emitted service block MUST carry `pull_policy: never` alongside
// its digest-pinned image so the shared `up --no-start` Create step can't
// attempt a registry pull during a rollback (the digest blob is already cached
// by PrepareRollback). One `pull_policy: never` line per service, right after
// each `image:` line.
func TestBuildOverrideYAMLPullPolicyNever(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web":   {Image: "nginx:latest", Digest: "sha256:w"},
		"db":    {Image: "postgres:16", Digest: "sha256:d"},
		"cache": {Image: "redis:7", Digest: "sha256:c"},
	}
	out := string(buildOverrideYAML(entries, []string{"web", "db", "cache"}))

	if n := strings.Count(out, "pull_policy: never"); n != 3 {
		t.Fatalf("want exactly 3 pull_policy: never lines (one per service), got %d in:\n%s", n, out)
	}
	// Every service block: the image line is immediately followed by the
	// pull_policy line, so the override wins the compose merge as the 2nd -f.
	for _, name := range []string{"web", "db", "cache"} {
		ref := rollbackImageRef(entries[name])
		block := "  " + name + ":\n    image: " + ref + "\n    pull_policy: never\n"
		if !strings.Contains(out, block) {
			t.Errorf("service %q block missing or malformed; want %q in:\n%s", name, block, out)
		}
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

// TestParseRunningContainerIDs exercises the array, NDJSON, empty, and
// malformed branches plus the running-vs-stopped filter.
func TestParseRunningContainerIDs(t *testing.T) {
	t.Run("array", func(t *testing.T) {
		m, err := parseRunningContainerIDs([]byte(
			`[{"ID":"c1","Service":"web","State":"running"},{"ID":"c2","Service":"db","State":"exited"}]`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m["web"] != "c1" {
			t.Errorf("web = %q, want c1", m["web"])
		}
		if _, ok := m["db"]; ok {
			t.Errorf("db should be filtered out (not running)")
		}
	})
	t.Run("ndjson", func(t *testing.T) {
		m, err := parseRunningContainerIDs([]byte(
			"{\"ID\":\"c1\",\"Service\":\"web\",\"State\":\"running\"}\n{\"ID\":\"c2\",\"Service\":\"web\",\"State\":\"running\"}\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// First running replica wins for a scaled service.
		if m["web"] != "c1" {
			t.Errorf("web = %q, want the first running replica c1", m["web"])
		}
	})
	t.Run("empty and bracket-empty", func(t *testing.T) {
		for _, in := range []string{"", "  ", "[]"} {
			m, err := parseRunningContainerIDs([]byte(in))
			if err != nil {
				t.Fatalf("parse(%q): %v", in, err)
			}
			if len(m) != 0 {
				t.Errorf("parse(%q) = %v, want empty", in, m)
			}
		}
	})
	t.Run("skips empty service or id", func(t *testing.T) {
		m, err := parseRunningContainerIDs([]byte(
			`[{"ID":"","Service":"web","State":"running"},{"ID":"c2","Service":"","State":"running"}]`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(m) != 0 {
			t.Errorf("entries with empty ID/Service should be skipped, got %v", m)
		}
	})
	t.Run("malformed array errors", func(t *testing.T) {
		if _, err := parseRunningContainerIDs([]byte(`[{"ID":`)); err == nil {
			t.Fatal("expected error for malformed JSON array")
		}
	})
	t.Run("malformed ndjson line errors", func(t *testing.T) {
		if _, err := parseRunningContainerIDs([]byte("{\"ID\":\"c1\"}\n{bad line")); err == nil {
			t.Fatal("expected error for malformed NDJSON line")
		}
	})
}

// TestSnapshotServices_FetchImagesError: a `compose config` failure aborts the
// whole capture (mirrors the remote TestRemoteSnapshotServices_PSError shape).
func TestSnapshotServices_FetchImagesError(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			if argsHave(cmd.Args, "config") {
				return nil, fmt.Errorf("compose config exploded")
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	if _, err := c.SnapshotServices(context.Background(), nil); err == nil {
		t.Fatal("expected an error when compose config fails")
	}
}

// TestSnapshotServices_PSError: a `compose ps` failure aborts the whole capture.
func TestSnapshotServices_PSError(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			switch {
			case argsHave(cmd.Args, "config"):
				return []byte(`{"services":{"web":{"image":"nginx:latest"}}}`), nil
			case argsHave(cmd.Args, "ps"):
				return nil, fmt.Errorf("ps exploded")
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	_, err := c.SnapshotServices(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error when compose ps fails")
	}
	if !strings.Contains(err.Error(), "listing containers for snapshot") {
		t.Errorf("error = %q, want it to mention listing containers", err.Error())
	}
}

// TestSnapshotServices_InspectPartialTolerance (Q3): when a container vanishes
// between `ps` and the batched `docker inspect` (the batch returns fewer lines
// than IDs), the capture falls back to per-container inspects and still records
// the survivor rather than failing the whole snapshot.
func TestSnapshotServices_InspectPartialTolerance(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			switch {
			case argsHave(args, "compose") && argsHave(args, "config"):
				return []byte(`{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`), nil
			case argsHave(args, "compose") && argsHave(args, "ps"):
				return []byte(`[{"ID":"cid-web","Service":"web","State":"running"},{"ID":"cid-db","Service":"db","State":"running"}]`), nil
			case len(args) >= 3 && args[1] == "inspect" && args[2] == "--format":
				ids := args[4:]
				if len(ids) > 1 {
					// Batched call: db vanished → return only web's line (count
					// mismatch triggers the per-container fallback).
					return []byte("sha256:img-web\n"), nil
				}
				// Per-container fallback.
				switch ids[0] {
				case "cid-web":
					return []byte("sha256:img-web\n"), nil
				default:
					return nil, fmt.Errorf("Error: No such container: %s", ids[0])
				}
			case len(args) >= 3 && args[1] == "image" && args[2] == "inspect":
				if args[len(args)-1] == "sha256:img-web" {
					return []byte("nginx@sha256:ab12\n"), nil
				}
				return []byte(""), nil // db has no image id → no digest
			}
			return nil, fmt.Errorf("unexpected cmd: %v", args)
		},
	}
	res, err := c.SnapshotServices(context.Background(), nil)
	if err != nil {
		t.Fatalf("SnapshotServices must not fail the whole capture on a partial inspect: %v", err)
	}
	if res.Snapshot.Services["web"].Digest != "sha256:ab12" {
		t.Errorf("web should be captured, got %+v", res.Snapshot.Services["web"])
	}
	if _, ok := res.Snapshot.Services["db"]; ok {
		t.Errorf("db (vanished) should be absent, got %+v", res.Snapshot.Services["db"])
	}
	if len(res.Warnings) == 0 {
		t.Error("expected a warning for the vanished db container")
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

// TestWriteSnapshotRefusesFutureSchema: a future-schema existing file must NOT
// be clobbered (an older binary downgrading a newer format defeats the "never
// guess" protection). WriteSnapshot aborts and leaves the file intact.
func TestWriteSnapshotRefusesFutureSchema(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := &Compose{ProjectDir: "/proj"}
	ctx := context.Background()

	path := filepath.Join(home, ".cdeploy", "state", snapshotKey(localProjectDir("/proj"))+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	future := []byte(`{"schema":2,"project_dir":"/proj","services":{"db":{"digest":"sha256:keepme"}}}`)
	if err := os.WriteFile(path, future, 0o644); err != nil {
		t.Fatalf("seed future schema: %v", err)
	}

	fresh := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: localProjectDir("/proj"),
		Services:   map[string]SnapshotEntry{"web": {Image: "nginx", Digest: "sha256:w"}},
	}
	if err := c.WriteSnapshot(ctx, fresh); err == nil {
		t.Fatal("WriteSnapshot must refuse to overwrite a future-schema file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading state file: %v", err)
	}
	if string(got) != string(future) {
		t.Errorf("future-schema file was modified:\n got %q\nwant %q", got, future)
	}
}

// TestWriteSnapshotRefusesUnreadable: a transient read failure (here a state path
// that is a directory, standing in for an IO/transport hiccup) must abort the
// write so a flaky read never wipes the existing merge history.
func TestWriteSnapshotRefusesUnreadable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := &Compose{ProjectDir: "/proj"}

	path := filepath.Join(home, ".cdeploy", "state", snapshotKey(localProjectDir("/proj"))+".json")
	// Make the state path a directory: os.ReadFile returns a non-ErrNotExist
	// error, which models a transient/IO read failure (not corrupt JSON).
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir state-as-dir: %v", err)
	}

	fresh := &Snapshot{Schema: snapshotSchemaVersion, Services: map[string]SnapshotEntry{"web": {Digest: "sha256:w"}}}
	if err := c.WriteSnapshot(context.Background(), fresh); err == nil {
		t.Fatal("WriteSnapshot must abort on an unreadable existing state file")
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

// argsHave reports whether any element of args contains sub — a small helper
// for discriminating docker/compose subcommands in PrepareRollback test hooks.
func argsHave(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

func TestRunDockerCmdStream_Argv(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			// Verify the writer is wired to both streams and reaches the caller.
			fmt.Fprint(cmd.Stdout, "pull-progress")
			return nil
		},
	}
	var buf strings.Builder
	if err := c.runDockerCmdStream(context.Background(), []string{"pull", "nginx@sha256:ab12"}, &buf); err != nil {
		t.Fatalf("runDockerCmdStream: %v", err)
	}
	if captured == nil {
		t.Fatal("runCmd not called")
	}
	want := []string{"docker", "pull", "nginx@sha256:ab12"}
	if len(captured.Args) != len(want) {
		t.Fatalf("argv = %v, want %v", captured.Args, want)
	}
	for i, w := range want {
		if captured.Args[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, captured.Args[i], w)
		}
	}
	// Top-level docker command must NOT carry the compose subcommand.
	if argsHave(captured.Args, "compose") {
		t.Errorf("streaming docker argv leaked 'compose': %v", captured.Args)
	}
	if captured.Stdout == nil || captured.Stderr == nil {
		t.Error("stdout/stderr not wired to the writer")
	}
	if buf.String() != "pull-progress" {
		t.Errorf("writer got %q, want %q", buf.String(), "pull-progress")
	}
}

// prepComposer builds a Compose whose ProjectDir contains a real compose.yml
// (so findComposeFile succeeds) and whose outputCmd hook scripts the
// PrepareRollback data-plane calls. presentRefs lists digest refs whose
// `docker image inspect` presence check succeeds; any other ref errors (→ pull).
// The advisory same-digest capture is neutralized by an empty compose config +
// empty ps (no running services → no warning) unless the caller overrides.
func prepComposer(t *testing.T, presentRefs map[string]bool, runCmd func(*exec.Cmd) error) (*Compose, string) {
	t.Helper()
	dir := t.TempDir()
	mainFile := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(mainFile, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("seed compose file: %v", err)
	}
	c := &Compose{
		ProjectDir: dir,
		UID:        "1000:1000",
		runCmd:     runCmd,
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			switch {
			case argsHave(args, "{{.Id}}"):
				// presence check: last arg is the digest ref.
				ref := args[len(args)-1]
				if presentRefs[ref] {
					return []byte("sha256:present\n"), nil
				}
				return nil, fmt.Errorf("Error: No such image: %s", ref)
			case argsHave(args, "config"):
				return []byte(`{"services":{}}`), nil // advisory: no services
			case argsHave(args, "ps"):
				return []byte(`[]`), nil // advisory: nothing running
			}
			return nil, fmt.Errorf("unexpected cmd: %v", args)
		},
	}
	return c, mainFile
}

func TestPrepareRollback_OverrideAndFieldOrdering(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:ab12"},
		"db":  {Image: "postgres:16", Digest: "sha256:cd34"},
	}
	// Both digests already cached → no pull.
	present := map[string]bool{"nginx@sha256:ab12": true, "postgres@sha256:cd34": true}
	c, mainFile := prepComposer(t, present, func(cmd *exec.Cmd) error {
		t.Errorf("no pull expected, but a command ran: %v", cmd.Args)
		return nil
	})

	var w strings.Builder
	cleanup, err := c.PrepareRollback(context.Background(), entries, nil, &w)
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup is nil on success")
	}
	if len(c.ExtraComposeFiles) != 2 {
		t.Fatalf("ExtraComposeFiles = %v, want [main, override]", c.ExtraComposeFiles)
	}
	// Main compose file MUST be first (-f disables auto-discovery).
	if c.ExtraComposeFiles[0] != mainFile {
		t.Errorf("main file = %q, want %q (must be first)", c.ExtraComposeFiles[0], mainFile)
	}
	overridePath := c.ExtraComposeFiles[1]
	got, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatalf("reading override file: %v", err)
	}
	// Deterministic, sorted (db before web), digest-pinned, pull_policy: never.
	want := "services:\n" +
		"  db:\n    image: postgres@sha256:cd34\n    pull_policy: never\n" +
		"  web:\n    image: nginx@sha256:ab12\n    pull_policy: never\n"
	if string(got) != want {
		t.Errorf("override content mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestPrepareRollback_RelativeProjectDirAbsolutizesMain (I1): with a RELATIVE
// -C project dir, the discovered main compose file must be stored ABSOLUTE in
// ExtraComposeFiles. command() sets cmd.Dir = ProjectDir and docker resolves -f
// against that cwd, so a relative `-f app/compose.yml` would be re-prefixed to
// ./app/app/compose.yml and the rollback would fail with "no configuration file".
func TestPrepareRollback_RelativeProjectDirAbsolutizesMain(t *testing.T) {
	parent := t.TempDir()
	t.Chdir(parent)
	if err := os.MkdirAll("app", 0o755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}
	if err := os.WriteFile(filepath.Join("app", "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("seed compose file: %v", err)
	}
	c := &Compose{
		ProjectDir: "./app", // relative — the interaction the abs fix guards
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			switch {
			case argsHave(cmd.Args, "{{.Id}}"):
				return []byte("sha256:present\n"), nil // already cached → no pull
			case argsHave(cmd.Args, "config"):
				return []byte(`{"services":{}}`), nil // advisory: no services
			case argsHave(cmd.Args, "ps"):
				return []byte(`[]`), nil // advisory: nothing running
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	entries := map[string]SnapshotEntry{"web": {Image: "nginx:latest", Digest: "sha256:ab12"}}
	var w strings.Builder
	cleanup, err := c.PrepareRollback(context.Background(), entries, []string{"web"}, &w)
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	defer cleanup()
	if len(c.ExtraComposeFiles) != 2 {
		t.Fatalf("ExtraComposeFiles = %v, want [main, override]", c.ExtraComposeFiles)
	}
	main := c.ExtraComposeFiles[0]
	if !filepath.IsAbs(main) {
		t.Errorf("main compose file %q is not absolute; a relative -f is re-prefixed against cmd.Dir", main)
	}
	if _, err := os.Stat(main); err != nil {
		t.Errorf("main compose file %q does not resolve to the real file: %v", main, err)
	}
}

func TestPrepareRollback_PullsMissingDigest(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:ab12"},
	}
	var pullCmd *exec.Cmd
	// Digest NOT present → presence check errors → pull.
	c, _ := prepComposer(t, map[string]bool{}, func(cmd *exec.Cmd) error {
		pullCmd = cmd
		return nil
	})

	var w strings.Builder
	cleanup, err := c.PrepareRollback(context.Background(), entries, []string{"web"}, &w)
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	defer cleanup()
	if pullCmd == nil {
		t.Fatal("expected a pull command for the missing digest")
	}
	want := []string{"docker", "pull", "nginx@sha256:ab12"}
	if len(pullCmd.Args) != len(want) {
		t.Fatalf("pull argv = %v, want %v", pullCmd.Args, want)
	}
	for i, ww := range want {
		if pullCmd.Args[i] != ww {
			t.Errorf("pull argv[%d] = %q, want %q", i, pullCmd.Args[i], ww)
		}
	}
	if !strings.Contains(w.String(), "pulling nginx@sha256:ab12") {
		t.Errorf("expected a 'pulling' progress line, got %q", w.String())
	}
	if len(c.ExtraComposeFiles) != 2 {
		t.Errorf("ExtraComposeFiles not set after successful pull: %v", c.ExtraComposeFiles)
	}
}

func TestPrepareRollback_AbortsOnFailedPull(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:ab12"},
	}
	// Digest missing AND pull fails (blob pruned + registry down).
	c, _ := prepComposer(t, map[string]bool{}, func(cmd *exec.Cmd) error {
		return fmt.Errorf("manifest for nginx@sha256:ab12 not found")
	})

	var w strings.Builder
	cleanup, err := c.PrepareRollback(context.Background(), entries, []string{"web"}, &w)
	if err == nil {
		t.Fatal("expected an abort error on failed pull")
	}
	if cleanup != nil {
		t.Error("cleanup must be nil when PrepareRollback aborts")
	}
	// Aborts BEFORE any pipeline configuration: field untouched.
	if c.ExtraComposeFiles != nil {
		t.Errorf("ExtraComposeFiles mutated despite abort: %v", c.ExtraComposeFiles)
	}
	if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("abort error = %q, want it to name the service and 'unavailable'", err.Error())
	}
}

func TestPrepareRollback_CleanupResetsFieldAndRemovesFile(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:ab12"},
	}
	c, _ := prepComposer(t, map[string]bool{"nginx@sha256:ab12": true}, nil)

	var w strings.Builder
	cleanup, err := c.PrepareRollback(context.Background(), entries, nil, &w)
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	overridePath := c.ExtraComposeFiles[1]
	if _, statErr := os.Stat(overridePath); statErr != nil {
		t.Fatalf("override file missing before cleanup: %v", statErr)
	}

	cleanup()

	if c.ExtraComposeFiles != nil {
		t.Errorf("cleanup did not reset ExtraComposeFiles: %v", c.ExtraComposeFiles)
	}
	if _, statErr := os.Stat(overridePath); statErr == nil {
		t.Errorf("cleanup did not remove the override file %q", overridePath)
	}
}

func TestPrepareRollback_SameDigestWarning(t *testing.T) {
	// The currently-running web container uses sha256:ab12 — exactly the
	// snapshot digest → an "already at snapshot" advisory, prep still proceeds.
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:ab12"},
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("seed compose file: %v", err)
	}
	c := &Compose{
		ProjectDir: dir,
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			switch {
			case argsHave(args, "{{.Image}}"):
				return []byte("sha256:img-web\n"), nil
			case argsHave(args, "RepoDigests"):
				return []byte("nginx@sha256:ab12\n"), nil // current running digest
			case argsHave(args, "{{.Id}}"):
				return []byte("sha256:present\n"), nil // presence: cached, no pull
			case argsHave(args, "config"):
				return []byte(`{"services":{"web":{"image":"nginx:latest"}}}`), nil
			case argsHave(args, "ps"):
				return []byte(`[{"ID":"cid-web","Service":"web","State":"running"}]`), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", args)
		},
	}
	var w strings.Builder
	cleanup, err := c.PrepareRollback(context.Background(), entries, nil, &w)
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	defer cleanup()
	if !strings.Contains(w.String(), "already at snapshot") || !strings.Contains(w.String(), "sha256:ab12") {
		t.Errorf("expected an 'already at snapshot' advisory naming the digest, got %q", w.String())
	}
	// Advisory is non-fatal: prep still configured the pipeline.
	if len(c.ExtraComposeFiles) != 2 {
		t.Errorf("ExtraComposeFiles not set despite same-digest advisory: %v", c.ExtraComposeFiles)
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

// --- C4: local state-write locking (concurrent-deploy safety) ---

// TestWriteSnapshot_ConcurrentDeploysAllMerge exercises the flock: many
// concurrent WriteSnapshot calls for the SAME project, each merging a distinct
// single service, must all survive in the final merged state. Without the
// advisory lock the read-modify-rename would interleave and later renames would
// clobber earlier writers' fresh entries, dropping services.
func TestWriteSnapshot_ConcurrentDeploysAllMerge(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	c := New("/opt/myapp")
	ctx := context.Background()

	const n = 24
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			svc := fmt.Sprintf("svc%02d", i)
			fresh := &Snapshot{
				Schema:     snapshotSchemaVersion,
				ProjectDir: localProjectDir("/opt/myapp"),
				Services: map[string]SnapshotEntry{
					svc: {Image: svc + ":latest", Digest: "sha256:" + svc, RecordedAt: "2026-07-29T00:00:00Z"},
				},
			}
			if err := c.WriteSnapshot(ctx, fresh); err != nil {
				t.Errorf("WriteSnapshot(%s): %v", svc, err)
			}
		}(i)
	}
	wg.Wait()

	snap, err := c.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("snapshot missing after concurrent writes")
	}
	if len(snap.Services) != n {
		t.Errorf("merged services = %d, want %d — a concurrent write clobbered another's entry", len(snap.Services), n)
	}
	for i := 0; i < n; i++ {
		svc := fmt.Sprintf("svc%02d", i)
		if _, ok := snap.Services[svc]; !ok {
			t.Errorf("service %s missing from the merged snapshot", svc)
		}
	}
}

// TestLockStateFile_Serializes pins the helper directly: while one lock is held,
// a second lock on the same path must block, then acquire once the first is
// released.
func TestLockStateFile_Serializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "k.json")

	unlock1, err := lockStateFile(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		unlock2, err := lockStateFile(path)
		if err != nil {
			return
		}
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
		t.Fatal("second lock acquired while the first was still held")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	unlock1()
	select {
	case <-acquired:
		// expected: acquired after release
	case <-time.After(2 * time.Second):
		t.Fatal("second lock not acquired after the first was released")
	}
}

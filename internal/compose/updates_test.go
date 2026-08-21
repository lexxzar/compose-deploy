package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestParseConfigImages(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "two services with images",
			in:   `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`,
			want: map[string]string{"web": "nginx:latest", "db": "postgres:16"},
		},
		{
			name: "build only service omitted",
			in:   `{"services":{"web":{"image":"nginx:latest"},"app":{"image":""}}}`,
			want: map[string]string{"web": "nginx:latest"},
		},
		{
			name: "empty input",
			in:   "",
			want: map[string]string{},
		},
		{
			name: "no services key",
			in:   `{}`,
			want: map[string]string{},
		},
		{
			name:    "malformed json",
			in:      `{"services":`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConfigImages([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseConfigImages(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseLocalDigest(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		imageRef string
		want     string
	}{
		{"single-line basic", "nginx@sha256:abc123", "nginx:latest", "sha256:abc123"},
		{"registry path", "registry.example.com/foo/bar@sha256:deadbeef", "registry.example.com/foo/bar:v1", "sha256:deadbeef"},
		{"trims whitespace", "  nginx@sha256:abc123  \n", "nginx:latest", "sha256:abc123"},
		{"empty", "", "nginx:latest", ""},
		{"no at sign falls through to first", "no-at-sign", "nginx:latest", ""},
		{"trailing at", "nginx@", "nginx:latest", ""},
		// Multi-line input with multiple repo digests: prefer the one matching
		// the compose-file image reference (the fix for arbitrary RepoDigests[0]
		// selection when an image is tagged under multiple repositories).
		{
			name:     "multi-line picks matching repo",
			in:       "other/web@sha256:other-digest\nrepo.example.com/web@sha256:wanted-digest\nmirror.io/web@sha256:mirror-digest\n",
			imageRef: "repo.example.com/web:v3",
			want:     "sha256:wanted-digest",
		},
		{
			name:     "multi-line no match falls back to first",
			in:       "alt-repo/web@sha256:alt-digest\nother-repo/web@sha256:other-digest\n",
			imageRef: "repo.example.com/web:v3",
			want:     "sha256:alt-digest",
		},
		{
			name:     "matches with explicit registry port",
			in:       "localhost:5000/web@sha256:port-digest\nother/web@sha256:other-digest\n",
			imageRef: "localhost:5000/web:v1",
			want:     "sha256:port-digest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLocalDigest(tt.in, tt.imageRef)
			if got != tt.want {
				t.Fatalf("parseLocalDigest(%q, %q) = %q, want %q", tt.in, tt.imageRef, got, tt.want)
			}
		})
	}
}

func TestStripTag(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"nginx", "nginx"},
		{"nginx:latest", "nginx"},
		{"repo.example.com/web:v3", "repo.example.com/web"},
		{"localhost:5000/web", "localhost:5000/web"}, // port preserved (no tag)
		{"localhost:5000/web:v1", "localhost:5000/web"},
		{"nginx@sha256:abc", "nginx"},
		{"repo.example.com:5000/team/web@sha256:abc", "repo.example.com:5000/team/web"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := stripTag(tt.in); got != tt.want {
				t.Errorf("stripTag(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseManifestDigest(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "object form",
			in:   `{"Descriptor":{"digest":"sha256:object"}}`,
			want: "sha256:object",
		},
		{
			name: "array form takes first",
			in:   `[{"Descriptor":{"digest":"sha256:first"}},{"Descriptor":{"digest":"sha256:second"}}]`,
			want: "sha256:first",
		},
		{
			name: "empty array",
			in:   `[]`,
			want: "",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "malformed",
			in:   `{not-json`,
			want: "",
		},
		{
			name: "missing descriptor",
			in:   `{"foo":"bar"}`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseManifestDigest([]byte(tt.in))
			if got != tt.want {
				t.Fatalf("parseManifestDigest = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFilterServices(t *testing.T) {
	all := map[string]string{"a": "img-a", "b": "img-b", "c": "img-c"}
	tests := []struct {
		name   string
		wanted []string
		want   map[string]string
	}{
		{"empty returns all", nil, all},
		{"empty slice returns all", []string{}, all},
		{"subset filters", []string{"a", "c"}, map[string]string{"a": "img-a", "c": "img-c"}},
		{"missing names omitted", []string{"a", "missing"}, map[string]string{"a": "img-a"}},
		{"all missing returns empty", []string{"x", "y"}, map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterServices(all, tt.wanted)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("filterServices(%v) = %#v, want %#v", tt.wanted, got, tt.want)
			}
		})
	}
}

func TestCheckUpdates_FetchesConfigFirst(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"},"app":{"image":""}}}`
	var argvs [][]string
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			argvs = append(argvs, append([]string{}, cmd.Args...))
			args := cmd.Args
			// `docker compose config --format json`
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			// `docker image inspect ...`
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				image := args[len(args)-1]
				return []byte(image + "@sha256:local-" + image + "\n"), nil
			}
			// `docker manifest inspect ...`
			if len(args) >= 3 && args[1] == "manifest" && args[2] == "inspect" {
				image := args[len(args)-1]
				switch image {
				case "nginx:latest":
					// Differs from local → update available.
					return []byte(`{"Descriptor":{"digest":"sha256:remote-nginx:latest"}}`), nil
				case "postgres:16":
					// Matches local digest format.
					return []byte(`{"Descriptor":{"digest":"sha256:local-postgres:16"}}`), nil
				}
			}
			return nil, fmt.Errorf("unexpected cmd: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	wantByService := map[string]bool{"web": true, "db": false}
	if !reflect.DeepEqual(got, wantByService) {
		t.Fatalf("results = %#v, want %#v", got, wantByService)
	}
	// Verify the config call was first.
	if len(argvs) < 1 || argvs[0][1] != "compose" || argvs[0][2] != "config" {
		t.Errorf("first argv = %v, want docker compose config", argvs[0])
	}
	// Verify NONE of the image/manifest inspect argvs contain `compose` —
	// they must bypass c.command().
	for _, a := range argvs[1:] {
		for _, tok := range a {
			if tok == "compose" {
				t.Errorf("inspect argv contains 'compose': %v", a)
			}
		}
	}
}

func TestCheckUpdates_SubsetFilter(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	inspectCalls := 0
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				inspectCalls++
				image := args[len(args)-1]
				return []byte(image + "@sha256:same"), nil
			}
			if len(args) >= 3 && args[1] == "manifest" && args[2] == "inspect" {
				return []byte(`{"Descriptor":{"digest":"sha256:same"}}`), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), []string{"web"})
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result for subset, got %d: %#v", len(got), got)
	}
	if v, ok := got["web"]; !ok || v {
		t.Errorf("web = %v (ok=%v), want false (matching digests)", v, ok)
	}
	if inspectCalls != 1 {
		t.Errorf("image inspect called %d times, want 1 (subset filter)", inspectCalls)
	}
}

func TestCheckUpdates_InspectFailureLeavesAbsent(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				image := args[len(args)-1]
				if image == "nginx:latest" {
					return nil, fmt.Errorf("no such image")
				}
				return []byte(image + "@sha256:same"), nil
			}
			if len(args) >= 3 && args[1] == "manifest" && args[2] == "inspect" {
				return []byte(`{"Descriptor":{"digest":"sha256:same"}}`), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	// web failed inspect → absent (tri-state); db succeeded → false.
	if _, present := got["web"]; present {
		t.Errorf("web should be absent on inspect failure, got %v", got["web"])
	}
	if v, ok := got["db"]; !ok || v {
		t.Errorf("db = %v (ok=%v), want false", v, ok)
	}
}

func TestCheckUpdates_ConfigFailureReturnsError(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("compose binary missing")
		},
	}
	_, err := c.CheckUpdates(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when config fetch fails")
	}
	if !strings.Contains(err.Error(), "compose config") {
		t.Errorf("error = %q, want it to mention compose config", err.Error())
	}
}

// TestCheckUpdates_SystemicNetworkFailureSurfaces is the iteration-3
// parity with RemoteCompose: when EVERY image's remote fetch fails with
// a network-looking error AND no service got a verdict, the cascade
// fires and CheckUpdates returns a wrapped error so the CLI/TUI can
// surface "updates unavailable" instead of silently showing no glyphs.
func TestCheckUpdates_SystemicNetworkFailureSurfaces(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				image := args[len(args)-1]
				return []byte(image + "@sha256:local"), nil
			}
			if len(args) >= 4 && args[1] == "buildx" && args[2] == "imagetools" {
				return nil, fmt.Errorf("connection refused")
			}
			if len(args) >= 3 && args[1] == "manifest" && args[2] == "inspect" {
				// Network-looking stderr matched by looksLikeNetworkErr.
				return nil, fmt.Errorf("dial tcp: lookup registry-1.docker.io: no such host")
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when every image hits a network failure")
	}
	if !strings.Contains(err.Error(), "registry unreachable") {
		t.Errorf("error = %q, want it to mention registry unreachable", err.Error())
	}
	if len(got) != 0 {
		t.Errorf("got = %#v, want empty map (every service unknown)", got)
	}
}

// TestCheckUpdates_SystemicLocalDockerFailureSurfaces is the iteration-4
// parity with the registry cascade: when EVERY local `docker image inspect`
// fails (daemon stopped, socket missing, images never pulled) AND no
// service got a verdict, the cascade fires and CheckUpdates returns a
// wrapped error so the CLI/TUI can surface "updates unavailable" instead
// of silently showing no glyphs. Without this, a stopped local daemon
// would produce an empty map with nil error — feature-broken silently.
func TestCheckUpdates_SystemicLocalDockerFailureSurfaces(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				// Every local inspect fails — same as "docker daemon stopped".
				return nil, fmt.Errorf("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when every local image inspect fails")
	}
	if !strings.Contains(err.Error(), "local docker unavailable") {
		t.Errorf("error = %q, want it to mention 'local docker unavailable'", err.Error())
	}
	if !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Errorf("error = %q, want it to wrap the original daemon error", err.Error())
	}
	if len(got) != 0 {
		t.Errorf("got = %#v, want empty map (every service unknown)", got)
	}
}

// TestCheckUpdates_PartialLocalDockerFailureDoesNotCascade documents the
// negative case: when ONE image's local inspect succeeds and the OTHER
// fails, the local-docker cascade MUST NOT fire — partial local failures
// (one image not pulled while others are) shouldn't blank the screen.
// The succeeded service still gets a verdict; the failed one stays
// absent (per-image unknown).
func TestCheckUpdates_PartialLocalDockerFailureDoesNotCascade(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				image := args[len(args)-1]
				if image == "nginx:latest" {
					return nil, fmt.Errorf("Error: No such image: nginx:latest")
				}
				return []byte(image + "@sha256:local"), nil
			}
			if len(args) >= 4 && args[1] == "buildx" && args[2] == "imagetools" {
				return nil, fmt.Errorf("exit status 1: buildx unavailable")
			}
			if len(args) >= 3 && args[1] == "manifest" && args[2] == "inspect" {
				return []byte(`{"Descriptor":{"digest":"sha256:local"}}`), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error on partial local failure: %v", err)
	}
	if v, ok := got["db"]; !ok || v {
		t.Errorf("db = %v (ok=%v), want false (succeeded with matching digest)", v, ok)
	}
	if _, present := got["web"]; present {
		t.Errorf("web should be absent on local inspect failure, got %v", got["web"])
	}
}

// TestCheckUpdates_PartialNetworkFailureDoesNotCascade documents the
// negative case: when ONE image succeeds and the OTHER fails with a
// network error, the cascade MUST NOT fire — partial registry hiccups
// shouldn't blank a correct screen. The succeeded service still gets a
// verdict; the failed one stays absent.
func TestCheckUpdates_PartialNetworkFailureDoesNotCascade(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				image := args[len(args)-1]
				return []byte(image + "@sha256:local"), nil
			}
			if len(args) >= 4 && args[1] == "buildx" && args[2] == "imagetools" {
				return nil, fmt.Errorf("exit status 1: 'buildx' is not a docker command")
			}
			if len(args) >= 3 && args[1] == "manifest" && args[2] == "inspect" {
				image := args[len(args)-1]
				if image == "nginx:latest" {
					return nil, fmt.Errorf("dial tcp: connection refused")
				}
				// postgres succeeds with matching digest.
				return []byte(`{"Descriptor":{"digest":"sha256:local"}}`), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error on partial failure: %v", err)
	}
	if v, ok := got["db"]; !ok || v {
		t.Errorf("db = %v (ok=%v), want false (succeeded with matching digest)", v, ok)
	}
	if _, present := got["web"]; present {
		t.Errorf("web should be absent on per-image failure, got %v", got["web"])
	}
}

// TestLooksLikeNetworkErr exercises the heuristic with representative
// network-failure and per-image-error strings — the boundary that
// determines whether systemic-failure detection fires.
func TestLooksLikeNetworkErr(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"dial tcp lookup nxdomain", "dial tcp: lookup registry-1.docker.io: no such host", true},
		{"connection refused", "connection refused", true},
		{"i/o timeout", "Get \"https://registry-1.docker.io/v2/\": i/o timeout", true},
		{"x509 cert", "x509: certificate signed by unknown authority", true},
		{"too many requests (429)", "toomanyrequests: too many requests", true},
		{"per-image no such image", "Error response from daemon: No such image: nginx:latest", false},
		{"per-image manifest unknown", "manifest unknown", false},
		{"per-image auth required", "authentication required", false},
		{"empty", "", false},
		{"plain", "registry unreachable", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := looksLikeNetworkErr(fmt.Errorf("%s", tt.in))
			if got != tt.want {
				t.Errorf("looksLikeNetworkErr(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
	if looksLikeNetworkErr(nil) {
		t.Error("looksLikeNetworkErr(nil) should be false")
	}
}

// TestLooksLikeLocalDaemonErr exercises the iteration-5 daemon-down
// classifier — the boundary that gates the local-docker cascade so that
// fresh-deploy "No such image" failures aren't conflated with daemon-down.
func TestLooksLikeLocalDaemonErr(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"cannot connect to daemon", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", true},
		{"docker daemon is not running", "error during connect: docker daemon is not running", true},
		{"docker.sock no such file", "Cannot connect: dial unix /var/run/docker.sock: connect: no such file or directory", true},
		{"docker.sock connection refused", "dial unix /var/run/docker.sock: connect: connection refused", true},
		{"docker.sock permission denied", "dial unix /var/run/docker.sock: connect: permission denied", true},
		{"docker command not found", "docker: command not found", true},
		{"docker executable not found", `exec: "docker": executable file not found in $PATH`, true},
		// Negative cases: per-image failures during a fresh deploy.
		{"no such image", "Error: No such image: nginx:latest", false},
		{"no such image lower", "error response from daemon: no such image: foo:latest", false},
		{"manifest unknown", "manifest unknown", false},
		{"empty", "", false},
		{"plain", "image inspect failed", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := looksLikeLocalDaemonErr(fmt.Errorf("%s", tt.in))
			if got != tt.want {
				t.Errorf("looksLikeLocalDaemonErr(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
	if looksLikeLocalDaemonErr(nil) {
		t.Error("looksLikeLocalDaemonErr(nil) should be false")
	}
}

// TestCheckUpdates_FreshDeployMultipleImagesDoesNotCascade is the iteration-5
// regression for the local-cascade misfire: a fresh-deploy scenario where
// every image is referenced in compose.yml but none have been pulled to the
// local docker host yet would previously trigger the misleading
// "local docker unavailable" cascade, even though the user's local daemon
// is perfectly fine. With the iteration-5 looksLikeLocalDaemonErr gate,
// per-image "No such image" failures are absorbed as absent (matching
// RemoteCompose semantics), and the function returns (empty map, nil err).
func TestCheckUpdates_FreshDeployMultipleImagesDoesNotCascade(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"},"cache":{"image":"redis:7"}}}`
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				image := args[len(args)-1]
				// Fresh deploy: NONE of these images exist locally.
				return nil, fmt.Errorf("exit status 1: Error: No such image: %s", image)
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("fresh deploy should NOT cascade, got error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %#v, want empty map (every service unknown, no false cascade)", got)
	}
}

func TestCheckUpdates_ManifestFailureLeavesAbsent(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"}}}`
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				image := args[len(args)-1]
				return []byte(image + "@sha256:local"), nil
			}
			if len(args) >= 3 && args[1] == "manifest" && args[2] == "inspect" {
				return nil, fmt.Errorf("registry unreachable")
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	// Manifest failure → service absent (registry error is per-image soft).
	if len(got) != 0 {
		t.Errorf("expected empty map on manifest failure, got %#v", got)
	}
}

// TestCheckUpdates_RepoDigestsMatchesImageRef verifies the parseLocalDigest
// fix for finding #8: when `image inspect` returns multiple RepoDigests (image
// tagged under several repos), the one matching the compose-file image
// reference is selected — not an arbitrary first entry. Without the fix,
// a stale digest from an unrelated tag would silently win.
func TestCheckUpdates_RepoDigestsMatchesImageRef(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"repo.example.com/web:v3"}}}`
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				// Multi-line: arbitrary order, wanted repo in the middle.
				return []byte("mirror.io/web@sha256:mirror-digest\nrepo.example.com/web@sha256:wanted-digest\nother/web@sha256:other-digest\n"), nil
			}
			if len(args) >= 3 && args[1] == "manifest" && args[2] == "inspect" {
				return []byte(`{"Descriptor":{"digest":"sha256:wanted-digest"}}`), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	// Wanted digest matches manifest → false (current). If parseLocalDigest had
	// picked the first entry (sha256:mirror-digest), the result would be true
	// (false positive update).
	if v, ok := got["web"]; !ok || v {
		t.Errorf("web = %v (ok=%v), want false (matching wanted digest)", v, ok)
	}
}

// TestCheckUpdates_BuildxImagetoolsHappyPath verifies the multi-arch-correct
// path: when `docker buildx imagetools inspect` succeeds, its
// manifest-list digest is compared against the local RepoDigest directly,
// bypassing `docker manifest inspect --verbose` (which would return a
// per-platform descriptor digest for multi-arch images and falsely report
// "update available"). This is the iteration-2 fix for the popular Docker
// Hub multi-arch false positive class (nginx, postgres, alpine, etc.).
//
// Uses the same default-format human-readable output that production sees;
// see testdata/buildx_imagetools_default_output.txt for the captured
// fixture from Docker 29.1.3 / buildx v0.30.1-desktop.1.
func TestCheckUpdates_BuildxImagetoolsHappyPath(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"}}}`
	// Realistic default-format output: top-level Digest: line is the
	// manifest-list digest; per-platform Manifests: entries are indented.
	imagetoolsOut := "Name:      nginx:latest\n" +
		"MediaType: application/vnd.oci.image.index.v1+json\n" +
		"Digest:    sha256:1111111111111111111111111111111111111111111111111111111111111111\n" +
		"\n" +
		"Manifests:\n" +
		"  Name:        nginx:latest@sha256:2222222222222222222222222222222222222222222222222222222222222222\n" +
		"  MediaType:   application/vnd.oci.image.manifest.v1+json\n" +
		"  Platform:    linux/amd64\n"
	manifestInspectCalls := 0
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				// Local RepoDigest matches the manifest-list digest (multi-arch).
				return []byte("nginx:latest@sha256:1111111111111111111111111111111111111111111111111111111111111111\n"), nil
			}
			if len(args) >= 4 && args[1] == "buildx" && args[2] == "imagetools" && args[3] == "inspect" {
				return []byte(imagetoolsOut), nil
			}
			if len(args) >= 3 && args[1] == "manifest" && args[2] == "inspect" {
				manifestInspectCalls++
				// Would return a per-platform descriptor digest that would
				// MISMATCH and falsely report "update available". The fix
				// short-circuits at imagetools so this call is never made.
				return []byte(`{"Descriptor":{"digest":"sha256:per-platform"}}`), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if v, ok := got["web"]; !ok || v {
		t.Errorf("web = %v (ok=%v), want false (matching manifest-list digest)", v, ok)
	}
	if manifestInspectCalls != 0 {
		t.Errorf("manifest inspect called %d times, want 0 (imagetools should short-circuit)", manifestInspectCalls)
	}
}

// TestCheckUpdates_BuildxImagetoolsFailsFallsBackToManifest covers the
// recoverable case where buildx imagetools is unavailable (older Docker
// without the buildx plugin) — the manifest inspect path takes over so the
// feature degrades gracefully rather than going dark.
func TestCheckUpdates_BuildxImagetoolsFailsFallsBackToManifest(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"}}}`
	manifestInspectCalls := 0
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 4 && args[1] == "compose" && args[2] == "config" {
				return []byte(configJSON), nil
			}
			if len(args) >= 3 && args[1] == "image" && args[2] == "inspect" {
				return []byte("nginx:latest@sha256:abc\n"), nil
			}
			if len(args) >= 4 && args[1] == "buildx" && args[2] == "imagetools" && args[3] == "inspect" {
				return nil, fmt.Errorf("docker: 'buildx' is not a docker command")
			}
			if len(args) >= 3 && args[1] == "manifest" && args[2] == "inspect" {
				manifestInspectCalls++
				return []byte(`{"Descriptor":{"digest":"sha256:abc"}}`), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if v, ok := got["web"]; !ok || v {
		t.Errorf("web = %v (ok=%v), want false (matching digest via manifest fallback)", v, ok)
	}
	if manifestInspectCalls != 1 {
		t.Errorf("manifest inspect called %d times, want 1 (fallback after buildx failure)", manifestInspectCalls)
	}
}

func TestParseImagetoolsDigest(t *testing.T) {
	digest64 := "1111111111111111111111111111111111111111111111111111111111111111"
	other64 := "2222222222222222222222222222222222222222222222222222222222222222"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "default format extracts top-level Digest line",
			in: "Name:      nginx:latest\n" +
				"MediaType: application/vnd.oci.image.index.v1+json\n" +
				"Digest:    sha256:" + digest64 + "\n" +
				"\n" +
				"Manifests:\n" +
				"  Name:        nginx:latest@sha256:" + other64 + "\n" +
				"  Platform:    linux/amd64\n",
			want: "sha256:" + digest64,
		},
		{
			name: "indented per-platform Digest is skipped",
			in: "Manifests:\n" +
				"  Digest:    sha256:" + other64 + "\n" +
				"Digest:    sha256:" + digest64 + "\n",
			want: "sha256:" + digest64,
		},
		{
			name: "bare sha256 line WITHOUT Digest: prefix rejected (no fallback)",
			in:   "sha256:" + digest64 + "\n",
			want: "",
		},
		{
			name: "uppercase hex normalised to lower",
			in:   "Digest:    sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n",
			want: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		{
			name: "regression: Name: line must NOT be accepted as digest",
			in: "Name:      docker.io/library/nginx:latest\n" +
				"MediaType: application/vnd.oci.image.index.v1+json\n",
			want: "",
		},
		{
			name: "regression: short non-hex bare line rejected",
			in:   "abcdef\n",
			want: "",
		},
		{
			name: "regression: sha256 with wrong hex length rejected",
			in:   "sha256:abc\n",
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "blank",
			in:   "   \n\n   ",
			want: "",
		},
		{
			name: "rate-limit stderr-on-stdout treated as no data",
			in:   "ERROR: failed to copy: 429 Too Many Requests\n",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseImagetoolsDigest([]byte(tt.in)); got != tt.want {
				t.Errorf("parseImagetoolsDigest(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestParseImagetoolsDigest_RealFixture is the iteration-3 regression pin.
// Reads the actual production output captured from Docker 29.1.3 /
// buildx v0.30.1-desktop.1 (the same environment that the broken
// --format '{{.Manifest.Digest}}' silently fell through on, producing the
// universal false-positive) and asserts the parser correctly extracts the
// top-level manifest-list digest — the SAME digest that production stores
// in the local RepoDigests, so update detection compares apples to apples.
func TestParseImagetoolsDigest_RealFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/buildx_imagetools_default_output.txt")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	got := parseImagetoolsDigest(data)
	want := "sha256:0e760fdfbc48ba8041e7c6db999bb40bfca508b4be580ac75d32c4e29d202ce1"
	if got != want {
		t.Errorf("parseImagetoolsDigest(fixture) = %q, want %q", got, want)
	}
}

// scanOutcome is one canned imageComparer answer.
type scanOutcome struct {
	updated bool
	ok      bool
	err     error
}

// scanFunc builds an imageComparer from a per-image table. A missing entry is
// a programming error in the test, not a silent absent verdict.
func scanFunc(t *testing.T, table map[string]scanOutcome) imageComparer {
	t.Helper()
	return func(_ context.Context, image string) (bool, bool, error) {
		e, found := table[image]
		if !found {
			t.Fatalf("comparer called with unexpected image %q", image)
		}
		return e.updated, e.ok, e.err
	}
}

func TestScanImageUpdates_PartialSuccess(t *testing.T) {
	wanted := map[string]string{
		"web":   "nginx:latest",
		"db":    "postgres:16",
		"cache": "redis:7",
		"queue": "rabbitmq:3",
	}
	compare := scanFunc(t, map[string]scanOutcome{
		"nginx:latest": {updated: true, ok: true},
		"postgres:16":  {ok: true},
		// A network-shaped failure and a daemon-shaped one, both absorbed
		// as absent because at least one service got a verdict.
		"redis:7":    {err: fmt.Errorf("dial tcp: connection refused")},
		"rabbitmq:3": {err: fmt.Errorf("%w: cannot connect to the docker daemon", errLocalImageInspect)},
	})
	got, err := scanImageUpdates(context.Background(), wanted, compare, updateCascades{registry: true, daemon: true})
	if err != nil {
		t.Fatalf("scanImageUpdates: %v", err)
	}
	want := map[string]bool{"web": true, "db": false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("results = %#v, want %#v", got, want)
	}
}

func TestScanImageUpdates_OkFalseStaysAbsent(t *testing.T) {
	wanted := map[string]string{"web": "nginx:latest"}
	compare := scanFunc(t, map[string]scanOutcome{"nginx:latest": {updated: true}})
	got, err := scanImageUpdates(context.Background(), wanted, compare, updateCascades{registry: true, daemon: true})
	if err != nil {
		t.Fatalf("scanImageUpdates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("results = %#v, want empty (ok=false is not a verdict)", got)
	}
}

func TestScanImageUpdates_DaemonCascade(t *testing.T) {
	wanted := map[string]string{"web": "nginx:latest", "db": "postgres:16"}
	compare := scanFunc(t, map[string]scanOutcome{
		"nginx:latest": {err: fmt.Errorf("%w: cannot connect to the docker daemon", errLocalImageInspect)},
		"postgres:16":  {err: fmt.Errorf("%w: is the docker daemon running?", errLocalImageInspect)},
	})

	_, err := scanImageUpdates(context.Background(), wanted, compare, updateCascades{registry: true, daemon: true})
	if err == nil || !strings.Contains(err.Error(), "local docker unavailable") {
		t.Fatalf("err = %v, want local docker unavailable", err)
	}

	// The remote path leaves the daemon knob off, and the zero value must
	// keep it that way: the same failures become plain absent verdicts.
	got, err := scanImageUpdates(context.Background(), wanted, compare, updateCascades{registry: true})
	if err != nil {
		t.Fatalf("daemon cascade fired with the knob off: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("results = %#v, want empty", got)
	}
}

func TestScanImageUpdates_DaemonCascadeNeedsEveryFailureDaemonShaped(t *testing.T) {
	wanted := map[string]string{"web": "nginx:latest", "db": "postgres:16"}
	compare := scanFunc(t, map[string]scanOutcome{
		"nginx:latest": {err: fmt.Errorf("%w: cannot connect to the docker daemon", errLocalImageInspect)},
		// A benign per-image failure — the fresh-deploy case.
		"postgres:16": {err: fmt.Errorf("%w: No such image: postgres:16", errLocalImageInspect)},
	})
	got, err := scanImageUpdates(context.Background(), wanted, compare, updateCascades{registry: true, daemon: true})
	if err != nil {
		t.Fatalf("cascade fired on a mixed failure set: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("results = %#v, want empty", got)
	}
}

func TestScanImageUpdates_RegistryCascade(t *testing.T) {
	wanted := map[string]string{"web": "nginx:latest", "db": "postgres:16"}
	compare := scanFunc(t, map[string]scanOutcome{
		"nginx:latest": {err: fmt.Errorf("dial tcp 1.2.3.4:443: connect: connection refused")},
		"postgres:16":  {err: fmt.Errorf("lookup registry-1.docker.io: no such host")},
	})

	_, err := scanImageUpdates(context.Background(), wanted, compare, updateCascades{registry: true, daemon: true})
	if err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("err = %v, want registry unreachable", err)
	}

	got, err := scanImageUpdates(context.Background(), wanted, compare, updateCascades{})
	if err != nil {
		t.Fatalf("registry cascade fired with the knob off: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("results = %#v, want empty", got)
	}
}

func TestScanImageUpdates_NoCascadeWhenAnyVerdict(t *testing.T) {
	wanted := map[string]string{"web": "nginx:latest", "db": "postgres:16"}
	compare := scanFunc(t, map[string]scanOutcome{
		"nginx:latest": {updated: true, ok: true},
		"postgres:16":  {err: fmt.Errorf("dial tcp: connection refused")},
	})
	got, err := scanImageUpdates(context.Background(), wanted, compare, updateCascades{registry: true, daemon: true})
	if err != nil {
		t.Fatalf("cascade fired while a verdict existed: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]bool{"web": true}) {
		t.Fatalf("results = %#v, want {web:true}", got)
	}
}

func TestScanImageUpdates_EmptySetNeverCascades(t *testing.T) {
	got, err := scanImageUpdates(context.Background(), nil, scanFunc(t, nil), updateCascades{registry: true, daemon: true})
	if err != nil {
		t.Fatalf("scanImageUpdates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("results = %#v, want empty", got)
	}
}

func TestCompareImageDigestVia_LocalErrWrapKnob(t *testing.T) {
	boom := errors.New("cannot connect to the docker daemon")
	f := &fakeDockerRunner{runErr: boom}

	_, _, err := compareImageDigestVia(context.Background(), f, "nginx:latest", wrapLocalImageInspectErr)
	if !errors.Is(err, errLocalImageInspect) {
		t.Fatalf("err = %v, want errLocalImageInspect", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the cause preserved", err)
	}

	// The remote binding passes nil: the error reaches the loop unwrapped,
	// so it feeds the registry counters instead of the daemon ones.
	_, _, err = compareImageDigestVia(context.Background(), f, "nginx:latest", nil)
	if errors.Is(err, errLocalImageInspect) {
		t.Fatalf("err = %v, want no sentinel with a nil wrapper", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the raw cause", err)
	}
}

func TestCompareImageDigestVia_VerdictThroughSeam(t *testing.T) {
	f := &fakeDockerRunner{runFunc: func(args []string) ([]byte, error) {
		switch {
		case args[0] == "image":
			return []byte("nginx@sha256:local\n"), nil
		case args[0] == "buildx":
			return []byte("Name:      docker.io/library/nginx:latest\nDigest:    sha256:0000000000000000000000000000000000000000000000000000000000000001\n"), nil
		}
		return nil, fmt.Errorf("unexpected argv: %v", args)
	}}
	updated, ok, err := compareImageDigestVia(context.Background(), f, "nginx:latest", wrapLocalImageInspectErr)
	if err != nil || !ok || !updated {
		t.Fatalf("compareImageDigestVia = (%v, %v, %v), want (true, true, nil)", updated, ok, err)
	}
	if len(f.runCalls) != 2 {
		t.Fatalf("run calls = %d (%v), want 2 — imagetools must satisfy the fetch", len(f.runCalls), f.runCalls)
	}
}

func TestFetchRemoteDigestVia_FallsBackToManifest(t *testing.T) {
	f := &fakeDockerRunner{runFunc: func(args []string) ([]byte, error) {
		if args[0] == "buildx" {
			return nil, errors.New("unknown command: docker buildx")
		}
		return []byte(`{"Descriptor":{"digest":"sha256:fallback"}}`), nil
	}}
	dg, ok, err := fetchRemoteDigestVia(context.Background(), f, "nginx:latest")
	if err != nil || !ok || dg != "sha256:fallback" {
		t.Fatalf("fetchRemoteDigestVia = (%q, %v, %v), want the manifest fallback digest", dg, ok, err)
	}
	if len(f.runCalls) != 2 {
		t.Fatalf("run calls = %d (%v), want 2", len(f.runCalls), f.runCalls)
	}
}

func TestFetchRemoteDigestVia_TransportErrorSkipsFallback(t *testing.T) {
	f := &fakeDockerRunner{runErr: fmt.Errorf("%w: ssh: connect to host db1 port 22: no route", errSSHTransport)}
	_, ok, err := fetchRemoteDigestVia(context.Background(), f, "nginx:latest")
	if ok {
		t.Fatal("ok = true, want false on a transport failure")
	}
	if !errors.Is(err, errSSHTransport) {
		t.Fatalf("err = %v, want errSSHTransport preserved for the batch abort", err)
	}
	if len(f.runCalls) != 1 {
		t.Fatalf("run calls = %d (%v), want 1 — a dead hop must not be retried", len(f.runCalls), f.runCalls)
	}
}

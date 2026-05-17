package compose

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// readFixture loads a captured `docker compose` output sample from testdata/.
// Failing here is a setup error (fixture file missing), so use t.Fatalf.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return string(b)
}

func TestParseDryRunOutput_Fixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    map[string]bool
	}{
		{
			name:    "mixed",
			fixture: "dryrun_mixed.txt",
			// fakebuild is build-only (absent); alpine current (false);
			// needupdate would be pulled (true). The trailing "Pulled" line
			// for needupdate is a no-op.
			want: map[string]bool{
				"alpine":     false,
				"needupdate": true,
			},
		},
		{
			name:    "all_current",
			fixture: "dryrun_all_current.txt",
			want: map[string]bool{
				"needupdate": false,
				"alpine":     false,
			},
		},
		{
			name:    "all_update",
			fixture: "dryrun_all_update.txt",
			// Two services would be pulled; the trailing "Pulled" lines are
			// completion markers and don't change the verdict.
			want: map[string]bool{
				"needupdate": true,
				"alpine":     true,
			},
		},
		{
			name:    "build_only",
			fixture: "dryrun_build_only.txt",
			// Build-only services produce an empty map (absent = unknown).
			want: map[string]bool{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDryRunOutput(readFixture(t, tt.fixture))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseDryRunOutput(%s) = %#v, want %#v", tt.fixture, got, tt.want)
			}
		})
	}
}

func TestParseDryRunOutput_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]bool
	}{
		{
			name: "empty",
			in:   "",
			want: map[string]bool{},
		},
		{
			name: "only_whitespace",
			in:   "   \n\t\n",
			want: map[string]bool{},
		},
		{
			name: "missing_prefix",
			in:   "nginx Pulling\n",
			want: map[string]bool{},
		},
		{
			name: "prefix_no_payload",
			in:   "DRY-RUN MODE -   \n",
			want: map[string]bool{},
		},
		{
			name: "service_no_verdict",
			in:   "DRY-RUN MODE -  web\n",
			want: map[string]bool{},
		},
		{
			name: "explicit_pull_required",
			// "Pull required" is reserved per the plan even though current
			// Compose doesn't emit it; the parser must still recognise it.
			in:   "DRY-RUN MODE -  api Pull required\n",
			want: map[string]bool{"api": true},
		},
		{
			name: "noise_lines_between",
			in:   "warning: deprecated key\n DRY-RUN MODE -  web Pulling \nrandom message\n DRY-RUN MODE -  db Skipped - Image is already present locally \n",
			want: map[string]bool{"web": true, "db": false},
		},
		{
			name: "multiple_replicas_same_service_last_wins",
			// Compose dedupes per service in dry-run, so duplicates shouldn't
			// occur, but if they did the last verdict wins (map semantics).
			in:   "DRY-RUN MODE -  web Pulling \nDRY-RUN MODE -  web Skipped - Image is already present locally\n",
			want: map[string]bool{"web": false},
		},
		{
			name: "alternative_already_present_phrasing",
			// Defensive: any "already present" substring counts as current.
			in:   "DRY-RUN MODE -  cache Image already present\n",
			want: map[string]bool{"cache": false},
		},
		{
			name: "unrecognised_verdict_skipped",
			// "Error: foo" style lines are not classified — service absent.
			in:   "DRY-RUN MODE -  web Error: connection refused\n",
			want: map[string]bool{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDryRunOutput(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseDryRunOutput(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDetectDryRunFromHelp_Fixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    bool
	}{
		{"with_dryrun", "pull_help_with_dryrun.txt", true},
		{"without_dryrun", "pull_help_without_dryrun.txt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectDryRunFromHelp(readFixture(t, tt.fixture))
			if got != tt.want {
				t.Fatalf("detectDryRunFromHelp(%s) = %v, want %v", tt.fixture, got, tt.want)
			}
		})
	}
}

func TestDetectDryRunFromHelp_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"plain_mention", "Some option --dry-run does X", true},
		{"different_flag_only", "Some option --dryrun (no dash) does X", false},
		{"case_sensitive", "Use --DRY-RUN flag", false},
		{"flag_in_description_text", "Pre-flight check (no --dry-run available)", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectDryRunFromHelp(tt.in)
			if got != tt.want {
				t.Fatalf("detectDryRunFromHelp(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

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
		in   string
		want string
	}{
		{"nginx@sha256:abc123", "sha256:abc123"},
		{"registry.example.com/foo/bar@sha256:deadbeef", "sha256:deadbeef"},
		{"  nginx@sha256:abc123  ", "sha256:abc123"},
		{"", ""},
		{"no-at-sign", ""},
		{"trailing-at@", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseLocalDigest(tt.in)
			if got != tt.want {
				t.Fatalf("parseLocalDigest(%q) = %q, want %q", tt.in, got, tt.want)
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

func TestSetDryRunSupport(t *testing.T) {
	c := &Compose{ProjectDir: "/proj", UID: "1000:1000"}
	if c.dryRunDetected {
		t.Fatal("dryRunDetected should be false initially")
	}
	c.SetDryRunSupport(true)
	if !c.dryRunSupported || !c.dryRunDetected {
		t.Fatalf("after SetDryRunSupport(true): supported=%v detected=%v",
			c.dryRunSupported, c.dryRunDetected)
	}
	// Should not invoke probe after SetDryRunSupport.
	calls := 0
	c.outputCmd = func(cmd *exec.Cmd) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("probe should not run")
	}
	c.detectDryRunSupport(context.Background())
	if calls != 0 {
		t.Errorf("probe ran after SetDryRunSupport (calls=%d)", calls)
	}
}

func TestDetectDryRunSupport_ProbeCached(t *testing.T) {
	calls := 0
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			calls++
			// Verify the argv shape: must be `docker compose pull --help`.
			args := cmd.Args[1:]
			want := []string{"compose", "pull", "--help"}
			if !reflect.DeepEqual(args, want) {
				t.Errorf("probe argv = %v, want %v", args, want)
			}
			return []byte("Options:\n  --dry-run    Execute command in dry run mode\n"), nil
		},
	}
	c.detectDryRunSupport(context.Background())
	if !c.dryRunSupported {
		t.Fatal("dryRunSupported = false, want true (help mentions --dry-run)")
	}
	c.detectDryRunSupport(context.Background())
	if calls != 1 {
		t.Errorf("probe ran %d times, want 1 (cached)", calls)
	}
}

func TestDetectDryRunSupport_ProbeFailureMarksUnsupported(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("docker not found")
		},
	}
	c.detectDryRunSupport(context.Background())
	if !c.dryRunDetected {
		t.Fatal("detected should be true after probe attempt")
	}
	if c.dryRunSupported {
		t.Fatal("dryRunSupported should be false when probe fails")
	}
}

func TestCheckUpdates_DryRunPath(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = cmd
			// Return the fixture directly; CombinedOutput would also include
			// stdout, but parser only needs the verdict lines.
			return []byte(" DRY-RUN MODE -  alpine Skipped - Image is already present locally\n DRY-RUN MODE -  needupdate Pulling\n"), nil
		},
	}
	c.SetDryRunSupport(true)
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	want := map[string]bool{"alpine": false, "needupdate": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("results = %#v, want %#v", got, want)
	}
	if captured == nil {
		t.Fatal("outputCmd was not invoked")
	}
	wantArgs := []string{"compose", "pull", "--dry-run", "--quiet", "--policy=missing"}
	if !reflect.DeepEqual(captured.Args[1:], wantArgs) {
		t.Errorf("argv tail = %v, want %v", captured.Args[1:], wantArgs)
	}
}

func TestCheckUpdates_DryRunPath_AppendsServices(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = cmd
			return []byte(""), nil
		},
	}
	c.SetDryRunSupport(true)
	if _, err := c.CheckUpdates(context.Background(), []string{"web", "db"}); err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	wantArgs := []string{"compose", "pull", "--dry-run", "--quiet", "--policy=missing", "web", "db"}
	if !reflect.DeepEqual(captured.Args[1:], wantArgs) {
		t.Errorf("argv tail = %v, want %v", captured.Args[1:], wantArgs)
	}
}

func TestCheckUpdates_DryRunPath_PartialOnError(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			// Partial verdict on stderr + non-zero exit.
			return []byte(" DRY-RUN MODE -  web Pulling \n"), fmt.Errorf("manifest fetch failed")
		},
	}
	c.SetDryRunSupport(true)
	got, err := c.CheckUpdates(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error from dry-run failure")
	}
	if !strings.Contains(err.Error(), "compose pull --dry-run") {
		t.Errorf("error = %q, want it to mention dry-run", err.Error())
	}
	// Partial map must survive — caller can still surface what was parsed.
	want := map[string]bool{"web": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("partial results = %#v, want %#v", got, want)
	}
}

func TestCheckUpdates_FallbackPath(t *testing.T) {
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
	c.SetDryRunSupport(false)
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	want := map[string]bool{"nginx:latest": true, "postgres:16": false}
	// Result is keyed by service name, not image — fix expectation.
	wantByService := map[string]bool{"web": true, "db": false}
	_ = want
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

func TestCheckUpdates_FallbackPath_SubsetFilter(t *testing.T) {
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
	c.SetDryRunSupport(false)
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

func TestCheckUpdates_FallbackPath_InspectFailureLeavesAbsent(t *testing.T) {
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
	c.SetDryRunSupport(false)
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

func TestCheckUpdates_FallbackPath_ConfigFailureReturnsError(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("compose binary missing")
		},
	}
	c.SetDryRunSupport(false)
	_, err := c.CheckUpdates(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when config fetch fails")
	}
	if !strings.Contains(err.Error(), "compose config") {
		t.Errorf("error = %q, want it to mention compose config", err.Error())
	}
}

func TestCheckUpdates_FallbackPath_ManifestFailureLeavesAbsent(t *testing.T) {
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
	c.SetDryRunSupport(false)
	got, err := c.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	// Manifest failure → service absent (registry error is per-image soft).
	if len(got) != 0 {
		t.Errorf("expected empty map on manifest failure, got %#v", got)
	}
}

func TestCheckUpdates_ProbeRouting(t *testing.T) {
	// Verifies that when dryRunDetected is false, CheckUpdates probes once,
	// caches the result, and routes subsequent calls accordingly.
	var argvs [][]string
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			argvs = append(argvs, append([]string{}, cmd.Args...))
			args := cmd.Args
			// Probe: `docker compose pull --help`
			if len(args) >= 4 && args[1] == "compose" && args[2] == "pull" && args[3] == "--help" {
				return []byte("Options:\n  --dry-run   dry run\n"), nil
			}
			// Dry-run path call.
			if len(args) >= 4 && args[1] == "compose" && args[2] == "pull" && args[3] == "--dry-run" {
				return []byte(" DRY-RUN MODE -  web Pulling\n"), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}
	if _, err := c.CheckUpdates(context.Background(), nil); err != nil {
		t.Fatalf("first CheckUpdates: %v", err)
	}
	if _, err := c.CheckUpdates(context.Background(), nil); err != nil {
		t.Fatalf("second CheckUpdates: %v", err)
	}
	// Expect: 1 probe + 2 dry-run calls = 3 total.
	if len(argvs) != 3 {
		t.Fatalf("argv calls = %d, want 3 (1 probe + 2 dry-run): %v", len(argvs), argvs)
	}
	if argvs[0][len(argvs[0])-1] != "--help" {
		t.Errorf("first call should be the probe, got %v", argvs[0])
	}
}


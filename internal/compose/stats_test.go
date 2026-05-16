package compose

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0B", 0, false},
		{"512B", 512, false},
		{"124MiB", 130023424, false},
		{"1.5GiB", 1610612736, false},
		{"100kB", 100000, false},
		{"100KB", 100000, false},
		{"1.5GB", 1500000000, false},
		{"2GiB", 2 * 1024 * 1024 * 1024, false},
		{"1TiB", 1024 * 1024 * 1024 * 1024, false},
		// Case-insensitive unit
		{"512mib", 512 * 1024 * 1024, false},
		// Whitespace tolerance
		{"  124MiB  ", 130023424, false},
		// No unit → assume bytes
		{"1024", 1024, false},
		// Errors
		{"abc", 0, true},
		{"MiB", 0, true},
		{"1.5XX", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseSize(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSize(%q) expected error, got nil (value=%d)", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSize(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseCPUPercent(t *testing.T) {
	tests := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"4.20%", 4.2, false},
		{"0.00%", 0, false},
		{"", 0, false},
		{"100%", 100, false},
		{"250.5%", 250.5, false},
		// Whitespace tolerance
		{"  4.20%  ", 4.2, false},
		// No percent suffix is still accepted (some output forms omit it)
		{"4.2", 4.2, false},
		// Errors
		{"abc", 0, true},
		{"4.2.0%", 0, true},
		{"%", 0, false}, // empty after stripping → 0
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseCPUPercent(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseCPUPercent(%q) expected error, got nil (value=%v)", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCPUPercent(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseCPUPercent(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1K"},
		{1536, "1.5K"}, // 1.5 KiB
		{2048, "2K"},
		{1024 * 1024, "1M"},
		{130023424, "124M"}, // 124 MiB exact
		{1610612736, "1.5G"},
		{1024 * 1024 * 1024, "1G"},
		{int64(1024) * 1024 * 1024 * 1024, "1T"},
		// Negative values clamp to 0
		{-1, "0B"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatBytes(tt.in)
			if got != tt.want {
				t.Errorf("FormatBytes(%d) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseStatsOutput(t *testing.T) {
	// Canned NDJSON output, 3 containers, mixed units and CPU percentages.
	in := strings.Join([]string{
		`{"ID":"abc123","Name":"proj-api-1","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}`,
		`{"ID":"def456","Name":"proj-db-1","CPUPerc":"0.50%","MemUsage":"50MB / 1GiB"}`,
		`{"ID":"ghi789","Name":"proj-worker-1","CPUPerc":"100.00%","MemUsage":"1.5GiB / 2GiB"}`,
	}, "\n")

	got, err := parseStatsOutput([]byte(in))
	if err != nil {
		t.Fatalf("parseStatsOutput error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}

	api, ok := got["abc123"]
	if !ok {
		t.Fatalf("missing abc123 in result")
	}
	if api.CPUPercent != 4.2 {
		t.Errorf("abc123 CPU = %v, want 4.2", api.CPUPercent)
	}
	if api.MemoryUsed != 130023424 {
		t.Errorf("abc123 MemoryUsed = %d, want 130023424", api.MemoryUsed)
	}
	if api.MemoryLimit != 536870912 {
		t.Errorf("abc123 MemoryLimit = %d, want 536870912", api.MemoryLimit)
	}

	db, ok := got["def456"]
	if !ok {
		t.Fatalf("missing def456 in result")
	}
	if db.CPUPercent != 0.5 {
		t.Errorf("def456 CPU = %v, want 0.5", db.CPUPercent)
	}
	if db.MemoryUsed != 50_000_000 {
		t.Errorf("def456 MemoryUsed = %d, want 50000000", db.MemoryUsed)
	}
	if db.MemoryLimit != 1024*1024*1024 {
		t.Errorf("def456 MemoryLimit = %d, want %d", db.MemoryLimit, 1024*1024*1024)
	}

	worker, ok := got["ghi789"]
	if !ok {
		t.Fatalf("missing ghi789 in result")
	}
	if worker.CPUPercent != 100 {
		t.Errorf("ghi789 CPU = %v, want 100", worker.CPUPercent)
	}
	if worker.MemoryUsed != 1610612736 {
		t.Errorf("ghi789 MemoryUsed = %d, want 1610612736", worker.MemoryUsed)
	}
}

func TestParseStatsOutput_JSONArray(t *testing.T) {
	// Same data as TestParseStatsOutput but in JSON-array form.
	in := `[
		{"ID":"abc123","Name":"proj-api-1","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"},
		{"ID":"def456","Name":"proj-db-1","CPUPerc":"0.50%","MemUsage":"50MB / 1GiB"},
		{"ID":"ghi789","Name":"proj-worker-1","CPUPerc":"100.00%","MemUsage":"1.5GiB / 2GiB"}
	]`

	got, err := parseStatsOutput([]byte(in))
	if err != nil {
		t.Fatalf("parseStatsOutput (array) error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got["abc123"].CPUPercent != 4.2 {
		t.Errorf("abc123 CPU = %v, want 4.2", got["abc123"].CPUPercent)
	}
	if got["def456"].MemoryUsed != 50_000_000 {
		t.Errorf("def456 MemoryUsed = %d, want 50000000", got["def456"].MemoryUsed)
	}
	if got["ghi789"].MemoryLimit != 2*1024*1024*1024 {
		t.Errorf("ghi789 MemoryLimit = %d, want %d", got["ghi789"].MemoryLimit, 2*1024*1024*1024)
	}
}

func TestParseStatsOutput_Empty(t *testing.T) {
	cases := []string{"", "   ", "[]", "\n\n"}
	for _, in := range cases {
		got, err := parseStatsOutput([]byte(in))
		if err != nil {
			t.Errorf("parseStatsOutput(%q) error: %v", in, err)
			continue
		}
		if len(got) != 0 {
			t.Errorf("parseStatsOutput(%q) = %v, want empty map", in, got)
		}
	}
}

func TestParseStatsOutput_Malformed(t *testing.T) {
	// Second line is malformed JSON.
	in := strings.Join([]string{
		`{"ID":"abc123","Name":"proj-api-1","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}`,
		`{not valid json`,
	}, "\n")
	_, err := parseStatsOutput([]byte(in))
	if err == nil {
		t.Fatal("expected error for malformed NDJSON, got nil")
	}
}

func TestParseStatsOutput_MalformedArray(t *testing.T) {
	in := `[{"ID":"abc"}, not valid]`
	_, err := parseStatsOutput([]byte(in))
	if err == nil {
		t.Fatal("expected error for malformed JSON array, got nil")
	}
}

func TestParseStatsOutput_SkipsEmptyID(t *testing.T) {
	in := strings.Join([]string{
		`{"ID":"","Name":"weird","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}`,
		`{"ID":"abc123","Name":"proj-api-1","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}`,
	}, "\n")
	got, err := parseStatsOutput([]byte(in))
	if err != nil {
		t.Fatalf("parseStatsOutput error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 entry (empty-ID dropped), got %d", len(got))
	}
	if _, ok := got["abc123"]; !ok {
		t.Errorf("missing abc123 in result")
	}
}

func TestParseStatsOutput_PropagatesCPUError(t *testing.T) {
	in := `{"ID":"abc","Name":"x","CPUPerc":"not-a-number","MemUsage":"124MiB / 512MiB"}`
	_, err := parseStatsOutput([]byte(in))
	if err == nil {
		t.Fatal("expected error from malformed CPU percent, got nil")
	}
}

func TestParseStatsOutput_PropagatesMemError(t *testing.T) {
	in := `{"ID":"abc","Name":"x","CPUPerc":"4.2%","MemUsage":"oops"}`
	_, err := parseStatsOutput([]byte(in))
	if err == nil {
		t.Fatal("expected error from malformed memory usage, got nil")
	}
}

func TestAllContainerStats_local_argConstruction(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = cmd
			return []byte("[]"), nil
		},
	}
	if _, err := AllContainerStats(context.Background(), c); err != nil {
		t.Fatalf("AllContainerStats error: %v", err)
	}
	if captured == nil {
		t.Fatal("outputCmd hook was not called")
	}
	wantArgs := []string{"docker", "stats", "--no-stream", "--format", "json"}
	if !reflect.DeepEqual(captured.Args, wantArgs) {
		t.Errorf("argv = %v, want %v", captured.Args, wantArgs)
	}
	// Sanity: 'compose' must not appear anywhere in argv (docker stats is not
	// a compose subcommand).
	for _, a := range captured.Args {
		if a == "compose" {
			t.Errorf("argv contains 'compose' element: %v", captured.Args)
		}
	}
}

func TestAllContainerStats_local_standaloneMode_unchanged(t *testing.T) {
	// Even in standalone mode (docker-compose binary), the docker stats argv
	// must remain unchanged — docker stats is a top-level docker CLI command,
	// not a compose subcommand.
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		Standalone: true,
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = cmd
			return []byte("[]"), nil
		},
	}
	c.SetStandalone(true)
	if _, err := AllContainerStats(context.Background(), c); err != nil {
		t.Fatalf("AllContainerStats error: %v", err)
	}
	if captured == nil {
		t.Fatal("outputCmd hook was not called")
	}
	wantArgs := []string{"docker", "stats", "--no-stream", "--format", "json"}
	if !reflect.DeepEqual(captured.Args, wantArgs) {
		t.Errorf("argv (standalone) = %v, want %v (unchanged)", captured.Args, wantArgs)
	}
	if captured.Args[0] == "docker-compose" {
		t.Errorf("argv started with docker-compose: %v", captured.Args)
	}
}

func TestAllContainerStats_local_parsing(t *testing.T) {
	canned := strings.Join([]string{
		`{"ID":"abc123","Name":"proj-api-1","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}`,
		`{"ID":"def456","Name":"proj-db-1","CPUPerc":"0.50%","MemUsage":"50MB / 1GiB"}`,
	}, "\n")
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(canned), nil
		},
	}
	got, err := AllContainerStats(context.Background(), c)
	if err != nil {
		t.Fatalf("AllContainerStats error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got["abc123"].CPUPercent != 4.2 {
		t.Errorf("abc123 CPU = %v, want 4.2", got["abc123"].CPUPercent)
	}
	if got["abc123"].MemoryUsed != 130023424 {
		t.Errorf("abc123 MemoryUsed = %d, want 130023424", got["abc123"].MemoryUsed)
	}
	if got["def456"].MemoryLimit != 1024*1024*1024 {
		t.Errorf("def456 MemoryLimit = %d, want %d", got["def456"].MemoryLimit, 1024*1024*1024)
	}
}

func TestAllContainerStats_local_error(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("docker daemon unavailable")
		},
	}
	_, err := AllContainerStats(context.Background(), c)
	if err == nil {
		t.Fatal("expected error from outputCmd failure, got nil")
	}
	if !strings.Contains(err.Error(), "container stats") {
		t.Errorf("error = %q, want it to mention container stats", err.Error())
	}
}

func TestAllContainerStatsRemote_argConstruction(t *testing.T) {
	var captured *exec.Cmd
	rc := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = cmd
			return []byte("[]"), nil
		},
	}
	if _, err := AllContainerStatsRemote(context.Background(), rc); err != nil {
		t.Fatalf("AllContainerStatsRemote error: %v", err)
	}
	if captured == nil {
		t.Fatal("outputCmd hook was not called")
	}
	wantArgs := []string{
		"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no",
		"--", "user@example.com",
		"docker stats --no-stream --format json",
	}
	if !reflect.DeepEqual(captured.Args, wantArgs) {
		t.Errorf("argv = %v, want %v", captured.Args, wantArgs)
	}
	// Sanity: 'compose' must not appear anywhere in the argv — docker stats is
	// a top-level Docker CLI command, not a compose subcommand.
	for _, a := range captured.Args {
		if strings.Contains(a, "compose") {
			t.Errorf("argv contains 'compose': %v", captured.Args)
			break
		}
	}
}

func TestAllContainerStatsRemote_extraArgsSplice(t *testing.T) {
	// SSHExtraArgs (e.g. -i /tmp/key) must appear immediately before the
	// `--` separator (which itself precedes the host argument).
	var captured *exec.Cmd
	extras := []string{"-i", "/tmp/key"}
	rc := &RemoteCompose{
		Host:         "user@example.com",
		ProjectDir:   "/app",
		SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
		SSHExtraArgs: extras,
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = cmd
			return []byte("[]"), nil
		},
	}
	if _, err := AllContainerStatsRemote(context.Background(), rc); err != nil {
		t.Fatalf("AllContainerStatsRemote error: %v", err)
	}
	if captured == nil {
		t.Fatal("outputCmd hook was not called")
	}
	// Locate the host and verify extras immediately precede the `--` separator.
	hostIdx := -1
	for i, a := range captured.Args {
		if a == rc.Host {
			hostIdx = i
			break
		}
	}
	if hostIdx < 0 {
		t.Fatalf("host %q not found in argv %v", rc.Host, captured.Args)
	}
	if hostIdx < 1 || captured.Args[hostIdx-1] != "--" {
		t.Fatalf("expected '--' immediately before host, got argv %v", captured.Args)
	}
	sepIdx := hostIdx - 1
	if sepIdx < len(extras) {
		t.Fatalf("separator index %d too small to fit extras %v in argv %v", sepIdx, extras, captured.Args)
	}
	for i, e := range extras {
		got := captured.Args[sepIdx-len(extras)+i]
		if got != e {
			t.Errorf("extras arg[%d] = %q, want %q (argv: %v)", sepIdx-len(extras)+i, got, e, captured.Args)
		}
	}
}

func TestAllContainerStatsRemote_portArgs(t *testing.T) {
	// Port args (-p NNNN) are folded into SSHExtraArgs by the cmd layer
	// (resolveSSHRemote). Verify that when SSHExtraArgs starts with -p NNNN
	// followed by -i <key>, both appear in order immediately before the host.
	var captured *exec.Cmd
	extras := []string{"-p", "2222", "-i", "/tmp/key"}
	rc := &RemoteCompose{
		Host:         "user@example.com",
		ProjectDir:   "/app",
		SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
		SSHExtraArgs: extras,
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = cmd
			return []byte("[]"), nil
		},
	}
	if _, err := AllContainerStatsRemote(context.Background(), rc); err != nil {
		t.Fatalf("AllContainerStatsRemote error: %v", err)
	}
	if captured == nil {
		t.Fatal("outputCmd hook was not called")
	}
	wantArgs := []string{
		"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no",
		"-p", "2222", "-i", "/tmp/key",
		"--", "user@example.com",
		"docker stats --no-stream --format json",
	}
	if !reflect.DeepEqual(captured.Args, wantArgs) {
		t.Errorf("argv = %v, want %v", captured.Args, wantArgs)
	}
}

func TestAllContainerStatsRemote_parsing(t *testing.T) {
	canned := strings.Join([]string{
		`{"ID":"abc123","Name":"proj-api-1","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}`,
		`{"ID":"def456","Name":"proj-db-1","CPUPerc":"0.50%","MemUsage":"50MB / 1GiB"}`,
	}, "\n")
	rc := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(canned), nil
		},
	}
	got, err := AllContainerStatsRemote(context.Background(), rc)
	if err != nil {
		t.Fatalf("AllContainerStatsRemote error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got["abc123"].CPUPercent != 4.2 {
		t.Errorf("abc123 CPU = %v, want 4.2", got["abc123"].CPUPercent)
	}
	if got["abc123"].MemoryUsed != 130023424 {
		t.Errorf("abc123 MemoryUsed = %d, want 130023424", got["abc123"].MemoryUsed)
	}
	if got["def456"].MemoryLimit != 1024*1024*1024 {
		t.Errorf("def456 MemoryLimit = %d, want %d", got["def456"].MemoryLimit, 1024*1024*1024)
	}
}

func TestAllContainerStatsRemote_error(t *testing.T) {
	rc := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("ssh: connect refused")
		},
	}
	_, err := AllContainerStatsRemote(context.Background(), rc)
	if err == nil {
		t.Fatal("expected error from outputCmd failure, got nil")
	}
	if !strings.Contains(err.Error(), "remote container stats") {
		t.Errorf("error = %q, want it to mention remote container stats", err.Error())
	}
}

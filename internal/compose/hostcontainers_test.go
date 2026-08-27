package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

func TestParseHostContainers_NDJSON(t *testing.T) {
	data := []byte(`{"ID":"aaa111222333","Names":"web","Image":"nginx:latest","State":"running","Status":"Up 2 hours (healthy)","Ports":"0.0.0.0:8080->80/tcp","Labels":"foo=bar","CreatedAt":"2026-08-21 13:15:17 +0300 EEST"}
{"ID":"bbb444555666","Names":"db","Image":"postgres:16","State":"exited","Status":"Exited (0) 3 hours ago","Ports":"","Labels":"","CreatedAt":"2026-08-20 09:00:00 +0300 EEST"}`)

	got, err := parseHostContainers(data)
	if err != nil {
		t.Fatalf("parseHostContainers() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "aaa111222333" || got[0].Names != "web" || got[0].Image != "nginx:latest" {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[0].State != "running" || got[0].Status != "Up 2 hours (healthy)" {
		t.Errorf("entry 0 state/status = %q/%q", got[0].State, got[0].Status)
	}
	if got[0].Ports != "0.0.0.0:8080->80/tcp" {
		t.Errorf("entry 0 ports = %q", got[0].Ports)
	}
	if got[1].Names != "db" || got[1].State != "exited" {
		t.Errorf("entry 1 = %+v", got[1])
	}
}

func TestParseHostContainers_Empty(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   \n  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHostContainers([]byte(tt.in))
			if err != nil {
				t.Fatalf("parseHostContainers() error = %v", err)
			}
			if got != nil {
				t.Errorf("got %v, want nil", got)
			}
		})
	}
}

func TestParseHostContainers_BlankLinesSkipped(t *testing.T) {
	data := []byte("\n{\"ID\":\"aaa111222333\",\"Names\":\"web\"}\n\n{\"ID\":\"bbb444555666\",\"Names\":\"db\"}\n")
	got, err := parseHostContainers(data)
	if err != nil {
		t.Fatalf("parseHostContainers() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestParseHostContainers_MalformedLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"ndjson", "{\"ID\":\"aaa111222333\"}\nnot json\n"},
		{"array", `[{"ID":"aaa"},`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHostContainers([]byte(tt.in))
			if err == nil {
				t.Fatalf("expected error, got %v", got)
			}
			if !strings.Contains(err.Error(), "parsing host containers") {
				t.Errorf("error = %v, want it to mention parsing host containers", err)
			}
		})
	}
}

// TestParseHostContainers_RealFixture parses a capture from a live daemon
// (`docker ps -a --format '{{json .}}'`) so the field names stay pinned to the
// real output shape rather than a hand-written approximation.
func TestParseHostContainers_RealFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_ps_host.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	entries, err := parseHostContainers(data)
	if err != nil {
		t.Fatalf("parseHostContainers() error = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("fixture parsed to zero entries")
	}

	var managed, unmanaged int
	for _, e := range entries {
		if e.ID == "" {
			t.Errorf("entry with empty ID: %+v", e)
		}
		if len(e.ID) != 12 {
			t.Errorf("ID %q is not the 12-char short form", e.ID)
		}
		if e.Names == "" {
			t.Errorf("entry %s has empty Names", e.ID)
		}
		if e.State == "" {
			t.Errorf("entry %s has empty State", e.ID)
		}
		if isComposeManaged(e.Labels) {
			managed++
			continue
		}
		unmanaged++
	}
	if managed == 0 {
		t.Error("fixture has no compose-managed containers; it cannot pin the filter")
	}
	if unmanaged == 0 {
		t.Error("fixture has no unmanaged containers; it cannot pin the filter")
	}
}

func TestIsComposeManaged(t *testing.T) {
	tests := []struct {
		name   string
		labels string
		want   bool
	}{
		{"empty", "", false},
		{"managed, only label", "com.docker.compose.project=my-app", true},
		{"managed, first of many", "com.docker.compose.project=my-app,com.docker.compose.service=web", true},
		{"managed, last of many", "org.opencontainers.image.vendor=acme,com.docker.compose.project=my-app", true},
		{"managed, middle of many", "a=1,com.docker.compose.project=my-app,b=2", true},
		{"unmanaged", "org.opencontainers.image.vendor=acme,maintainer=nobody", false},
		// The sibling keys must NOT count: they exist on every compose container
		// but also stand alone on nothing, and the "=" in the prefix excludes them.
		{"config_files sibling only", "com.docker.compose.project.config_files=/srv/app/compose.yml", false},
		{"working_dir sibling only", "com.docker.compose.project.working_dir=/srv/app", false},
		// A label VALUE containing a comma must not be mis-sliced into a fake token.
		{"comma inside a value", `description=one,two,three,maintainer=nobody`, false},
		{"value that mentions the key mid-token", "description=set com.docker.compose.project=x manually", false},
		{"value ending in the key with no comma boundary", "description=xcom.docker.compose.project=x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isComposeManaged(tt.labels); got != tt.want {
				t.Errorf("isComposeManaged(%q) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestParseHealthFromStatus(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Up 2 hours (healthy)", "healthy"},
		{"Up 2 hours (unhealthy)", "unhealthy"},
		{"Up 5 seconds (health: starting)", "starting"},
		{"Up 2 hours", ""},
		{"", ""},
		{"   ", ""},
		{"Created", ""},
		{"Exited (0) 2 hours ago", ""},
		{"Exited (255) 3 months ago", ""},
		{"Restarting (1) 3 seconds ago", ""},
		{"Up 2 hours (Paused)", ""},
		{"Up 2 hours (HEALTHY)", "healthy"},
		{"Up 2 hours (healthy)  ", "healthy"},
		// The end anchor is what makes the reading TRAILING rather than
		// first-match. Without it this reads "healthy" from the leading
		// annotation and reports a paused container as healthy; the Exited /
		// Restarting rows above cannot catch that, because their first group
		// is a numeric exit code the switch rejects anyway.
		{"Up 2 hours (healthy) (paused)", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseHealthFromStatus(tt.in); got != tt.want {
				t.Errorf("parseHealthFromStatus(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHostContainerName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"web", "web"},
		{"web,web-alias", "web"},
		{" web , alias ", "web"},
		{"", ""},
		{",alias", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := hostContainerName(tt.in); got != tt.want {
				t.Errorf("hostContainerName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// fakeDockerRunner is the dockerRunner seam double. It records every call and
// replays canned output, so HostContainers can be exercised without Docker.
type fakeDockerRunner struct {
	runCalls    [][]string
	runOut      []byte
	runErr      error
	runFunc     func(args []string) ([]byte, error)
	streamCalls [][]string
	streamOut   string
	streamErr   error
	ttyCalls    [][]string
}

func (f *fakeDockerRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	f.runCalls = append(f.runCalls, append([]string(nil), args...))
	if f.runFunc != nil {
		return f.runFunc(args)
	}
	return f.runOut, f.runErr
}

func (f *fakeDockerRunner) stream(ctx context.Context, w io.Writer, args ...string) error {
	f.streamCalls = append(f.streamCalls, append([]string(nil), args...))
	if f.streamOut != "" {
		if _, err := io.WriteString(w, f.streamOut); err != nil {
			return err
		}
	}
	return f.streamErr
}

func (f *fakeDockerRunner) tty(ctx context.Context, args ...string) *exec.Cmd {
	f.ttyCalls = append(f.ttyCalls, append([]string(nil), args...))
	return exec.CommandContext(ctx, "docker", args...)
}

const hostPsMixed = `{"ID":"aaa111222333","Names":"web","Image":"nginx:1.27","State":"running","Status":"Up 3 hours (healthy)","Ports":"0.0.0.0:8080->80/tcp, :::8080->80/tcp","Labels":"com.docker.compose.project=my-app,com.docker.compose.service=web","CreatedAt":"2026-08-19 10:00:00 +0300 EEST"}
{"ID":"bbb444555666","Names":"watchtower","Image":"containrrr/watchtower","State":"running","Status":"Up 2 days","Ports":"","Labels":"org.opencontainers.image.title=watchtower","CreatedAt":"2026-08-20 09:30:00 +0300 EEST"}
{"ID":"ccc777888999","Names":"pg-scratch,pg-alias","Image":"postgres:16","State":"exited","Status":"Exited (0) 4 hours ago","Ports":"","Labels":"","CreatedAt":"2026-08-18 08:15:00 +0300 EEST"}
{"ID":"ddd000111222","Names":"agent","Image":"datadog/agent","State":"running","Status":"Up 5 seconds (health: starting)","Ports":"127.0.0.1:8126->8126/tcp","Labels":"maintainer=nobody","CreatedAt":"2026-08-22 07:00:00 +0300 EEST"}`

func TestHostContainers_ListServices(t *testing.T) {
	f := &fakeDockerRunner{runOut: []byte(hostPsMixed)}
	h := &HostContainers{docker: f}

	got, err := h.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices() error = %v", err)
	}
	want := []string{"agent", "pg-scratch", "watchtower"}
	if len(got) != len(want) {
		t.Fatalf("ListServices() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListServices() = %v, want %v", got, want)
		}
	}
	if len(f.runCalls) != 1 {
		t.Fatalf("run calls = %d, want 1", len(f.runCalls))
	}
	wantArgs := []string{"ps", "-a", "--size=false", "--format", "{{json .}}"}
	if strings.Join(f.runCalls[0], " ") != strings.Join(wantArgs, " ") {
		t.Errorf("run args = %v, want %v", f.runCalls[0], wantArgs)
	}
}

func TestHostContainers_ListServices_AllManaged(t *testing.T) {
	f := &fakeDockerRunner{runOut: []byte(`{"ID":"aaa111222333","Names":"web","State":"running","Labels":"com.docker.compose.project=my-app"}`)}
	h := &HostContainers{docker: f}

	got, err := h.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListServices() = %v, want empty", got)
	}
}

func TestHostContainers_ListServices_SkipsUnnamed(t *testing.T) {
	f := &fakeDockerRunner{runOut: []byte(`{"ID":"aaa111222333","Names":"","State":"created","Labels":""}
{"ID":"bbb444555666","Names":"keeper","State":"running","Labels":""}`)}
	h := &HostContainers{docker: f}

	got, err := h.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices() error = %v", err)
	}
	if len(got) != 1 || got[0] != "keeper" {
		t.Errorf("ListServices() = %v, want [keeper]", got)
	}
}

func TestHostContainers_ListServices_RunError(t *testing.T) {
	f := &fakeDockerRunner{runErr: errors.New("docker daemon not running")}
	h := &HostContainers{docker: f}

	if _, err := h.ListServices(context.Background()); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "listing host containers") ||
		!strings.Contains(err.Error(), "docker daemon not running") {
		t.Errorf("error = %v, want it to wrap the run failure", err)
	}
}

func TestHostContainers_ListServices_ParseError(t *testing.T) {
	f := &fakeDockerRunner{runOut: []byte("not json")}
	h := &HostContainers{docker: f}

	if _, err := h.ListServices(context.Background()); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "parsing host containers") {
		t.Errorf("error = %v, want a parse error", err)
	}
}

func TestHostContainers_ContainerStatus(t *testing.T) {
	f := &fakeDockerRunner{runOut: []byte(hostPsMixed)}
	h := &HostContainers{docker: f}

	got, err := h.ContainerStatus(context.Background())
	if err != nil {
		t.Fatalf("ContainerStatus() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (the compose-managed row must be filtered out)", len(got))
	}
	if _, ok := got["web"]; ok {
		t.Error("compose-managed container leaked into the status map")
	}

	wt := got["watchtower"]
	if !wt.Running {
		t.Error("watchtower Running = false, want true")
	}
	if wt.Health != "" {
		t.Errorf("watchtower Health = %q, want empty", wt.Health)
	}
	if wt.Uptime != "2d" {
		t.Errorf("watchtower Uptime = %q, want 2d", wt.Uptime)
	}
	if wt.Created != "2026-08-20 09:30" {
		t.Errorf("watchtower Created = %q, want 2026-08-20 09:30", wt.Created)
	}
	if len(wt.Ports) != 0 {
		t.Errorf("watchtower Ports = %v, want none", wt.Ports)
	}

	pg := got["pg-scratch"]
	if pg.Running {
		t.Error("pg-scratch Running = true, want false")
	}
	if pg.Uptime != "" {
		t.Errorf("pg-scratch Uptime = %q, want empty", pg.Uptime)
	}
	if pg.Created != "2026-08-18 08:15" {
		t.Errorf("pg-scratch Created = %q, want 2026-08-18 08:15", pg.Created)
	}

	ag := got["agent"]
	if ag.Health != "starting" {
		t.Errorf("agent Health = %q, want starting", ag.Health)
	}
	if len(ag.Ports) != 1 {
		t.Fatalf("agent Ports = %v, want 1 entry", ag.Ports)
	}
	if ag.Ports[0].Host != "127.0.0.1" || ag.Ports[0].HostPort != 8126 ||
		ag.Ports[0].ContainerPort != 8126 || ag.Ports[0].Protocol != "tcp" {
		t.Errorf("agent Ports[0] = %+v", ag.Ports[0])
	}
}

// TestHostContainers_ContainerStatus_CollapsesIPv6Mirror pins that the status
// path reuses dedupAndSortPorts, so the 0.0.0.0 / :: wildcard mirror pair that
// `docker ps` prints for every published port renders as one entry.
func TestHostContainers_ContainerStatus_CollapsesIPv6Mirror(t *testing.T) {
	f := &fakeDockerRunner{runOut: []byte(`{"ID":"aaa111222333","Names":"solo","State":"running","Status":"Up 3 hours","Ports":"0.0.0.0:8080->80/tcp, :::8080->80/tcp","Labels":"","CreatedAt":"2026-08-19 10:00:00 +0300 EEST"}`)}
	h := &HostContainers{docker: f}

	got, err := h.ContainerStatus(context.Background())
	if err != nil {
		t.Fatalf("ContainerStatus() error = %v", err)
	}
	if len(got["solo"].Ports) != 1 {
		t.Fatalf("Ports = %+v, want 1 entry after the wildcard mirror collapse", got["solo"].Ports)
	}
}

// TestHostContainers_ContainerStatus_RealFixture runs the fixture through the
// whole ContainerStatus pipeline rather than only the field-name parse, so
// parseCreatedAt, formatUptime, parseHealthFromStatus and parsePortsString are
// all validated against real daemon output instead of the hand-written
// hostPsMixed constant. The redis-scratch row is the one row appended by hand
// to the capture: every real unmanaged container in it happened to publish no
// ports, so without it the bracketed-IPv6 form never reaches the unmanaged
// port pipeline in any test.
func TestHostContainers_ContainerStatus_RealFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_ps_host.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	h := &HostContainers{docker: &fakeDockerRunner{runOut: data}}

	status, err := h.ContainerStatus(context.Background())
	if err != nil {
		t.Fatalf("ContainerStatus() error = %v", err)
	}
	for _, managed := range []string{"local-doc-mcp", "context7", "pgsql_local"} {
		if _, ok := status[managed]; ok {
			t.Errorf("compose-managed container %q leaked into the unmanaged status map", managed)
		}
	}

	running, ok := status["elastic_dhawan"]
	if !ok {
		t.Fatalf("status has no elastic_dhawan: %+v", status)
	}
	if !running.Running {
		t.Error("elastic_dhawan State is running; want Running true")
	}
	if running.Created != "2026-08-21 22:09" {
		t.Errorf("Created = %q, want %q", running.Created, "2026-08-21 22:09")
	}
	if running.Uptime != "2h" {
		t.Errorf("Uptime = %q, want %q", running.Uptime, "2h")
	}

	exited, ok := status["lucid_goodall"]
	if !ok {
		t.Fatalf("status has no lucid_goodall: %+v", status)
	}
	if exited.Running {
		t.Error("lucid_goodall State is exited; want Running false")
	}
	if exited.Uptime != "" {
		t.Errorf("Uptime = %q, want empty for an exited container", exited.Uptime)
	}

	ported, ok := status["redis-scratch"]
	if !ok {
		t.Fatalf("status has no redis-scratch: %+v", status)
	}
	if ported.Health != "healthy" {
		t.Errorf("Health = %q, want %q", ported.Health, "healthy")
	}
	// The IPv4/IPv6 wildcard mirror collapses to one entry, exactly as the
	// compose path does.
	if got := FormatPorts(ported.Ports); got != "6379→6379" {
		t.Errorf("Ports = %q, want %q", got, "6379→6379")
	}
}

func TestHostContainers_ContainerStatus_Empty(t *testing.T) {
	f := &fakeDockerRunner{runOut: []byte("")}
	h := &HostContainers{docker: f}

	got, err := h.ContainerStatus(context.Background())
	if err != nil {
		t.Fatalf("ContainerStatus() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want an empty map", got)
	}
}

func TestHostContainers_ContainerStatus_RunError(t *testing.T) {
	f := &fakeDockerRunner{runErr: errors.New("boom")}
	h := &HostContainers{docker: f}

	if _, err := h.ContainerStatus(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

// TestHostContainerRunning pins the State fallback. .State only exists on
// Docker CLI >= 20.10 — the same legacy hosts hostPsArgs takes the
// `{{json .}}` template form for — and without the fallback every unmanaged
// row on such a host renders a stopped dot beside a live Uptime, and x exec
// refuses on a container that is plainly up.
func TestHostContainerRunning(t *testing.T) {
	tests := []struct {
		name string
		e    hostPsEntry
		want bool
	}{
		{"state running", hostPsEntry{State: "running", Status: "Up 3 hours"}, true},
		{"state exited", hostPsEntry{State: "exited", Status: "Exited (0) 2 hours ago"}, false},
		{"state restarting", hostPsEntry{State: "restarting", Status: "Restarting (1) 3 seconds ago"}, false},
		{"no state, status up", hostPsEntry{Status: "Up 3 hours (healthy)"}, true},
		{"no state, status up padded", hostPsEntry{Status: "  Up 2 days"}, true},
		{"no state, status exited", hostPsEntry{Status: "Exited (0) 2 hours ago"}, false},
		{"no state, status created", hostPsEntry{Status: "Created"}, false},
		{"no state, no status", hostPsEntry{}, false},
		// State wins when present, even when Status disagrees — a container
		// caught mid-transition must not be reported twice-over.
		{"state beats status", hostPsEntry{State: "exited", Status: "Up 3 hours"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostContainerRunning(tt.e); got != tt.want {
				t.Errorf("hostContainerRunning(%+v) = %v, want %v", tt.e, got, tt.want)
			}
		})
	}
}

// TestHostContainers_ContainerStatus_NoStateField is the end-to-end half of the
// fallback: a legacy `docker ps` payload with no State key still renders the
// running dot.
func TestHostContainers_ContainerStatus_NoStateField(t *testing.T) {
	legacy := `{"ID":"bbb444555666","Names":"watchtower","Image":"containrrr/watchtower","Status":"Up 2 days","Labels":""}`
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(legacy)}}

	status, err := h.ContainerStatus(context.Background())
	if err != nil {
		t.Fatalf("ContainerStatus() error = %v", err)
	}
	if !status["watchtower"].Running {
		t.Errorf("status = %+v; a legacy ps with no State must fall back to Status", status)
	}
}

func TestHostContainers_WriteMethodsAreReadOnly(t *testing.T) {
	h := &HostContainers{docker: &fakeDockerRunner{}}
	ctx := context.Background()

	tests := []struct {
		name string
		fn   func() error
	}{
		{"Stop", func() error { return h.Stop(ctx, nil, io.Discard) }},
		{"Remove", func() error { return h.Remove(ctx, nil, io.Discard) }},
		{"Pull", func() error { return h.Pull(ctx, nil, io.Discard) }},
		{"Create", func() error { return h.Create(ctx, nil, io.Discard) }},
		{"Start", func() error { return h.Start(ctx, []string{"web"}, io.Discard) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.fn(); !errors.Is(err, errReadOnly) {
				t.Errorf("%s() error = %v, want errReadOnly", tt.name, err)
			}
		})
	}
	if n := len(h.docker.(*fakeDockerRunner).runCalls); n != 0 {
		t.Errorf("a write method reached the docker seam %d time(s)", n)
	}
}

// --- CheckUpdates ---

// hostPsUpdates is a two-container discovery fixture: one compose-managed row
// (which must never reach a registry) and two unmanaged ones.
const hostPsUpdates = `{"ID":"aaa111222333","Names":"web","Image":"nginx:1.27","State":"running","Status":"Up 3 hours","Labels":"com.docker.compose.project=my-app"}
{"ID":"bbb444555666","Names":"watchtower","Image":"containrrr/watchtower:1.7","State":"running","Status":"Up 2 days","Labels":""}
{"ID":"ccc777888999","Names":"pg-scratch","Image":"postgres:16","State":"running","Status":"Up 4 hours","Labels":"maintainer=nobody"}`

var (
	digestOld = "sha256:" + strings.Repeat("a", 64)
	digestNew = "sha256:" + strings.Repeat("b", 64)
)

// hostUpdatesRunner replays a discovery output plus per-image local and remote
// digests. A local or remote entry may be an error, which the seam returns
// verbatim so the classification in scanImageUpdates sees a real error shape.
type hostDigests struct {
	local    string
	localErr error
	remote   string
	remErr   error
}

func hostUpdatesRunner(ps string, imgs map[string]hostDigests) *fakeDockerRunner {
	return &fakeDockerRunner{runFunc: func(args []string) ([]byte, error) {
		switch args[0] {
		case "ps":
			return []byte(ps), nil
		case "image":
			d, ok := imgs[args[len(args)-1]]
			if !ok {
				return nil, fmt.Errorf("unexpected image inspect: %v", args)
			}
			if d.localErr != nil {
				return nil, d.localErr
			}
			return []byte(d.local), nil
		case "buildx", "manifest":
			d, ok := imgs[args[len(args)-1]]
			if !ok {
				return nil, fmt.Errorf("unexpected registry inspect: %v", args)
			}
			if d.remErr != nil {
				return nil, d.remErr
			}
			return []byte("Name:      docker.io/" + args[len(args)-1] + "\nDigest:    " + d.remote + "\n"), nil
		}
		return nil, fmt.Errorf("unexpected argv: %v", args)
	}}
}

// TestHostContainers_CheckUpdates_Verdicts pins the happy path: the image map
// comes from the Image field docker ps already returned, so there is exactly
// one discovery call and no compose config call, and a managed container is
// never inspected.
func TestHostContainers_CheckUpdates_Verdicts(t *testing.T) {
	f := hostUpdatesRunner(hostPsUpdates, map[string]hostDigests{
		"containrrr/watchtower:1.7": {local: "containrrr/watchtower@" + digestOld, remote: digestNew},
		"postgres:16":               {local: "postgres@" + digestOld, remote: digestOld},
	})
	h := &HostContainers{docker: f}

	got, err := h.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates() error = %v", err)
	}
	want := map[string]bool{"watchtower": true, "pg-scratch": false}
	if len(got) != len(want) {
		t.Fatalf("CheckUpdates() = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("CheckUpdates() = %v, want %v", got, want)
		}
	}

	var psCalls int
	for _, call := range f.runCalls {
		if call[0] == "ps" {
			psCalls++
		}
		if call[0] == "compose" || call[0] == "config" {
			t.Errorf("CheckUpdates reached compose config: %v", call)
		}
		if slices.Contains(call, "nginx:1.27") {
			t.Errorf("a compose-managed image was inspected: %v", call)
		}
	}
	if psCalls != 1 {
		t.Errorf("discovery calls = %d, want 1", psCalls)
	}
}

// TestHostContainers_CheckUpdates_FiltersServices pins the "no filter = all"
// contract and its complement: a named subset inspects only that image.
func TestHostContainers_CheckUpdates_FiltersServices(t *testing.T) {
	f := hostUpdatesRunner(hostPsUpdates, map[string]hostDigests{
		"containrrr/watchtower:1.7": {local: "containrrr/watchtower@" + digestOld, remote: digestNew},
	})
	h := &HostContainers{docker: f}

	got, err := h.CheckUpdates(context.Background(), []string{"watchtower"})
	if err != nil {
		t.Fatalf("CheckUpdates() error = %v", err)
	}
	if len(got) != 1 || !got["watchtower"] {
		t.Fatalf("CheckUpdates() = %v, want {watchtower:true}", got)
	}
	for _, call := range f.runCalls {
		if slices.Contains(call, "postgres:16") {
			t.Errorf("an unrequested image was inspected: %v", call)
		}
	}
}

// TestHostContainers_CheckUpdates_PerImageFailureAbsorbed pins the tri-state:
// one image failing must not blank the container that did resolve, and must
// not surface as an error.
func TestHostContainers_CheckUpdates_PerImageFailureAbsorbed(t *testing.T) {
	f := hostUpdatesRunner(hostPsUpdates, map[string]hostDigests{
		"containrrr/watchtower:1.7": {local: "containrrr/watchtower@" + digestOld, remote: digestNew},
		"postgres:16":               {localErr: errors.New("Error: No such image: postgres:16")},
	})
	h := &HostContainers{docker: f}

	got, err := h.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates() error = %v, want the failure absorbed", err)
	}
	if len(got) != 1 || !got["watchtower"] {
		t.Fatalf("CheckUpdates() = %v, want only the resolved verdict", got)
	}
	if _, ok := got["pg-scratch"]; ok {
		t.Error("a failed comparison must stay ABSENT, not be a false verdict")
	}
}

// TestHostContainers_CheckUpdates_RegistryCascade pins the one diagnostic this
// path enables: every registry fetch failing with no verdict at all must name
// the cause instead of blanking the glyph column silently.
func TestHostContainers_CheckUpdates_RegistryCascade(t *testing.T) {
	f := hostUpdatesRunner(hostPsUpdates, map[string]hostDigests{
		"containrrr/watchtower:1.7": {
			local:  "containrrr/watchtower@" + digestOld,
			remErr: errors.New("dial tcp 1.2.3.4:443: connect: connection refused"),
		},
		"postgres:16": {
			local:  "postgres@" + digestOld,
			remErr: errors.New("lookup registry-1.docker.io: no such host"),
		},
	})
	h := &HostContainers{docker: f}

	got, err := h.CheckUpdates(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("err = %v, want registry unreachable", err)
	}
	if len(got) != 0 {
		t.Errorf("results = %v, want empty alongside the cascade", got)
	}
}

// TestHostContainers_CheckUpdates_NoDaemonCascade pins the design decision:
// the daemon cascade cannot fire on this path. A daemon-shaped local failure
// is absorbed as absent rather than surfacing "local docker unavailable" — a
// dead local daemon fails the docker ps discovery call first, so the cascade
// would only ever mislabel a remote host's failure.
func TestHostContainers_CheckUpdates_NoDaemonCascade(t *testing.T) {
	daemonDown := errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	f := hostUpdatesRunner(hostPsUpdates, map[string]hostDigests{
		"containrrr/watchtower:1.7": {localErr: daemonDown},
		"postgres:16":               {localErr: daemonDown},
	})
	h := &HostContainers{docker: f}

	got, err := h.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("err = %v, want the daemon cascade to stay off", err)
	}
	if len(got) != 0 {
		t.Errorf("results = %v, want empty", got)
	}
}

// TestHostContainers_CheckUpdates_TransportAbort pins the remote abort: a dead
// SSH hop fails every remaining image the same way, so the scan returns on the
// first errSSHTransport rather than burning the rest of the round-trips.
func TestHostContainers_CheckUpdates_TransportAbort(t *testing.T) {
	f := &fakeDockerRunner{runFunc: func(args []string) ([]byte, error) {
		if args[0] == "ps" {
			return []byte(hostPsUpdates), nil
		}
		return nil, fmt.Errorf("%w: ssh: connect to host db1 port 22: no route to host", errSSHTransport)
	}}
	h := &HostContainers{docker: f}

	_, err := h.CheckUpdates(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "transport failure") {
		t.Fatalf("err = %v, want the transport abort", err)
	}
	var inspects int
	for _, call := range f.runCalls {
		if call[0] != "ps" {
			inspects++
		}
	}
	if inspects != 1 {
		t.Errorf("inspect calls = %d, want 1 — the scan must abort on the first transport failure", inspects)
	}
}

// TestHostContainers_CheckUpdates_UntaggedImage pins the untagged case both
// ways. docker ps reports a bare image ID for an untagged image, which either
// yields no RepoDigests (nothing to compare) or fails the registry inspect
// with a reference error. Both must be absorbed as the tri-state absent, and
// neither may trip the registry cascade.
func TestHostContainers_CheckUpdates_UntaggedImage(t *testing.T) {
	const ps = `{"ID":"aaa111222333","Names":"scratch-build","Image":"9f1c2b3d4e5f","State":"running","Status":"Up 1 hour","Labels":""}`

	t.Run("no repo digests", func(t *testing.T) {
		f := hostUpdatesRunner(ps, map[string]hostDigests{"9f1c2b3d4e5f": {local: ""}})
		got, err := (&HostContainers{docker: f}).CheckUpdates(context.Background(), nil)
		if err != nil {
			t.Fatalf("err = %v, want the untagged image absorbed", err)
		}
		if len(got) != 0 {
			t.Errorf("results = %v, want empty", got)
		}
	})

	t.Run("registry rejects the reference", func(t *testing.T) {
		f := hostUpdatesRunner(ps, map[string]hostDigests{"9f1c2b3d4e5f": {
			local:  "some/repo@" + digestOld,
			remErr: errors.New("invalid reference format"),
		}})
		got, err := (&HostContainers{docker: f}).CheckUpdates(context.Background(), nil)
		if err != nil {
			t.Fatalf("err = %v, want the untagged image absorbed", err)
		}
		if len(got) != 0 {
			t.Errorf("results = %v, want empty", got)
		}
	})
}

// TestHostContainers_CheckUpdates_DiscoveryError pins the fail-closed path: a
// failed docker ps returns the error and reaches no registry.
func TestHostContainers_CheckUpdates_DiscoveryError(t *testing.T) {
	f := &fakeDockerRunner{runErr: errors.New("Cannot connect to the Docker daemon")}
	h := &HostContainers{docker: f}

	if _, err := h.CheckUpdates(context.Background(), nil); err == nil {
		t.Fatal("CheckUpdates() error = nil, want the discovery failure")
	}
	if len(f.runCalls) != 1 {
		t.Errorf("run calls = %d, want 1 — no image may be inspected without a discovery", len(f.runCalls))
	}
}

// TestHostContainers_CheckUpdates_MemoizesRepeatedImages pins the per-image
// memo. A compose project usually gives every service its own image, but host
// containers repeat one image constantly (the real capture has three
// sequentialthinking sidecars), and each comparison costs a local inspect plus
// a registry inspect — every one a separate SSH round-trip on a remote host.
func TestHostContainers_CheckUpdates_MemoizesRepeatedImages(t *testing.T) {
	ps := `{"ID":"aaa111222333","Names":"mcp-a","Image":"mcp/sequentialthinking","Labels":""}
{"ID":"bbb444555666","Names":"mcp-b","Image":"mcp/sequentialthinking","Labels":""}
{"ID":"ccc777888999","Names":"mcp-c","Image":"mcp/sequentialthinking","Labels":""}`
	f := hostUpdatesRunner(ps, map[string]hostDigests{
		"mcp/sequentialthinking": {local: "mcp/sequentialthinking@" + digestOld, remote: digestNew},
	})
	h := &HostContainers{docker: f}

	got, err := h.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates() error = %v", err)
	}
	for _, svc := range []string{"mcp-a", "mcp-b", "mcp-c"} {
		if !got[svc] {
			t.Errorf("results[%q] = %v, want true for every container on the shared image", svc, got[svc])
		}
	}
	var inspects int
	for _, call := range f.runCalls {
		if call[0] != "ps" {
			inspects++
		}
	}
	// One `docker image inspect` plus one `docker buildx imagetools inspect`
	// for the ONE distinct image — not three of each.
	if inspects != 2 {
		t.Errorf("inspect calls = %d (%v), want 2 — the distinct image must be compared once", inspects, f.runCalls)
	}
}

// TestCompareImageDigest_SentinelBinding pins each composer's local-error
// wrapper directly. The daemon cascade dispatches on the errLocalImageInspect
// sentinel alone, so which composer applies it IS the design decision — a test
// that only drives CheckUpdates cannot tell a missing wrapper from a failure
// the cascade classifier rejected.
func TestCompareImageDigest_SentinelBinding(t *testing.T) {
	daemonDown := errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")

	t.Run("Compose carries the sentinel", func(t *testing.T) {
		c := New(t.TempDir())
		c.SetTestHooks(nil, func(*exec.Cmd) ([]byte, error) { return nil, daemonDown })
		_, _, err := c.compareImageDigest(context.Background(), "nginx:1.27")
		if !errors.Is(err, errLocalImageInspect) {
			t.Fatalf("err = %v, want the errLocalImageInspect sentinel — the local daemon cascade dispatches on it", err)
		}
	})

	t.Run("RemoteCompose does not", func(t *testing.T) {
		rc := NewRemote("prod.example.com", "/srv/app")
		rc.SetTestHooks(nil, func(*exec.Cmd) ([]byte, error) { return nil, daemonDown })
		_, _, err := rc.compareImageDigest(context.Background(), "nginx:1.27")
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, errLocalImageInspect) {
			t.Error("the remote path must NOT carry errLocalImageInspect: the docker CLI runs on the far side of the SSH hop, so 'local docker unavailable' would name the wrong machine")
		}
	})

	t.Run("HostContainers does not", func(t *testing.T) {
		f := &fakeDockerRunner{runErr: daemonDown}
		h := &HostContainers{docker: f}
		_, _, err := h.compareImageDigest(context.Background(), "nginx:1.27")
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, errLocalImageInspect) {
			t.Error("HostContainers must NOT carry errLocalImageInspect: the same binding serves the remote runner, where the failure is on the far side of the SSH hop")
		}
	})
}

// TestHostImageMap_DropsEmptyImage pins the guard: an entry with no image
// reference would turn `docker image inspect` into a malformed call whose
// failure would pollute the cascade counters.
func TestHostImageMap_DropsEmptyImage(t *testing.T) {
	got := hostImageMap([]hostPsEntry{
		{Names: "keeper", Image: "nginx:1.27"},
		{Names: "ghost", Image: ""},
	})
	if len(got) != 1 || got["keeper"] != "nginx:1.27" {
		t.Fatalf("hostImageMap() = %v, want only the entry with an image", got)
	}
}

// TestHostImageMap_SkipsBareImageIDs pins the update-scan input filter. A
// container whose image carries no repository tag reports a bare image ID, and
// no registry can be asked about a ref with no repository — so it can only ever
// produce the tri-state absent, while costing a wasted round-trip per
// hand-started container and skewing the cascade ratios.
func TestHostImageMap_SkipsBareImageIDs(t *testing.T) {
	entries := []hostPsEntry{
		{Names: "watchtower", Image: "containrrr/watchtower:1.7"},
		{Names: "short-id", Image: "9c2ddb0e3cca"},
		{Names: "long-id", Image: strings.Repeat("a", 64)},
		{Names: "no-image", Image: ""},
		{Names: "", Image: "nginx:1.27"},
		// A repository that merely LOOKS hex-ish still has a tag, so it stays.
		{Names: "tagged-hex", Image: "9c2ddb0e3cca:latest"},
		{Names: "registry-hex", Image: "registry.example.com/9c2ddb0e3cca"},
	}

	got := hostImageMap(entries)
	want := map[string]string{
		"watchtower":   "containrrr/watchtower:1.7",
		"tagged-hex":   "9c2ddb0e3cca:latest",
		"registry-hex": "registry.example.com/9c2ddb0e3cca",
	}
	if len(got) != len(want) {
		t.Fatalf("hostImageMap() = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("hostImageMap()[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestNewLocalHostContainers_Argv(t *testing.T) {
	var captured []string
	c := New(t.TempDir())
	c.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		captured = append([]string(nil), cmd.Args...)
		return []byte(""), nil
	})

	if _, err := NewLocalHostContainers(c).ListServices(context.Background()); err != nil {
		t.Fatalf("ListServices() error = %v", err)
	}
	want := []string{"docker", "ps", "-a", "--size=false", "--format", "{{json .}}"}
	if len(captured) != len(want) {
		t.Fatalf("argv = %v, want %v", captured, want)
	}
	for i := range want {
		if captured[i] != want[i] {
			t.Fatalf("argv = %v, want %v", captured, want)
		}
	}
}

// TestNewRemoteHostContainers_SplicesSSHExtraArgs pins the remote seam against
// the repo-wide convention: SSHExtraArgs land immediately before the `--`
// separator that precedes the host argument, on every one of the three
// dockerRunner methods.
func TestNewRemoteHostContainers_SplicesSSHExtraArgs(t *testing.T) {
	extras := []string{"-p", "2222"}
	host := "user@example.com"

	t.Run("run", func(t *testing.T) {
		var captured []string
		r := &RemoteCompose{
			Host:         host,
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
		}
		r.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
			captured = append([]string(nil), cmd.Args...)
			return []byte(""), nil
		})
		if _, err := NewRemoteHostContainers(r).ListServices(context.Background()); err != nil {
			t.Fatalf("ListServices() error = %v", err)
		}
		assertExtraBeforeHost(t, "HostContainers run", captured, host, extras)
		remoteCmd := captured[len(captured)-1]
		if remoteCmd != `docker 'ps' '-a' '--size=false' '--format' '{{json .}}'` {
			t.Errorf("remote command = %q", remoteCmd)
		}
	})

	t.Run("stream", func(t *testing.T) {
		var captured []string
		r := &RemoteCompose{
			Host:         host,
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
		}
		r.SetTestHooks(func(cmd *exec.Cmd) error {
			captured = append([]string(nil), cmd.Args...)
			return nil
		}, nil)
		rd := remoteDockerRunner{r: r}
		if err := rd.stream(context.Background(), io.Discard, "logs", "web"); err != nil {
			t.Fatalf("stream() error = %v", err)
		}
		assertExtraBeforeHost(t, "HostContainers stream", captured, host, extras)
	})

	t.Run("tty", func(t *testing.T) {
		r := &RemoteCompose{
			Host:         host,
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
		}
		rd := remoteDockerRunner{r: r}
		cmd := rd.tty(context.Background(), "exec", "-it", "web", "sh")
		assertExtraBeforeHost(t, "HostContainers tty", cmd.Args, host, extras)
		if !slices.Contains(cmd.Args, "-t") {
			t.Errorf("tty argv %v has no -t; the remote exec path must allocate a TTY", cmd.Args)
		}
	})
}

// TestRemoteDockerRunner_StreamHasNoTTY pins the split that motivates a
// three-method seam: log streaming must NOT allocate a TTY, while tty must.
func TestRemoteDockerRunner_StreamHasNoTTY(t *testing.T) {
	var captured []string
	r := &RemoteCompose{Host: "example.com", SocketPath: "/tmp/cdeploy-ctrl-abc-99"}
	r.SetTestHooks(func(cmd *exec.Cmd) error {
		captured = append([]string(nil), cmd.Args...)
		return nil
	}, nil)

	rd := remoteDockerRunner{r: r}
	if err := rd.stream(context.Background(), io.Discard, "logs", "web"); err != nil {
		t.Fatalf("stream() error = %v", err)
	}
	if slices.Contains(captured, "-t") {
		t.Errorf("stream argv %v allocates a TTY", captured)
	}
}

// hostRunFunc dispatches a fakeDockerRunner by subcommand so a single fake can
// serve both the `ps` and the `stats` call that ContainerStats makes.
func hostRunFunc(ps, stats string, statsErr error) func(args []string) ([]byte, error) {
	return func(args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "stats" {
			return []byte(stats), statsErr
		}
		return []byte(ps), nil
	}
}

const hostStatsMixed = `{"ID":"bbb444555666","Name":"watchtower","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}
{"ID":"ddd000111222","Name":"agent","CPUPerc":"1.00%","MemUsage":"64MiB / 512MiB"}
{"ID":"aaa111222333","Name":"web","CPUPerc":"9.00%","MemUsage":"200MiB / 512MiB"}`

func TestHostContainers_ContainerStats(t *testing.T) {
	f := &fakeDockerRunner{runFunc: hostRunFunc(hostPsMixed, hostStatsMixed, nil)}
	h := &HostContainers{docker: f}

	got, err := h.ContainerStats(context.Background())
	if err != nil {
		t.Fatalf("ContainerStats() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ContainerStats() = %+v, want 2 entries", got)
	}
	if _, ok := got["web"]; ok {
		t.Error("compose-managed container leaked into the stats map")
	}
	// pg-scratch is in `ps` but not in `stats` (it is exited): skipped, not an error.
	if _, ok := got["pg-scratch"]; ok {
		t.Error("a container absent from docker stats must be skipped, not zero-filled")
	}

	wt := got["watchtower"]
	if wt.CPUPercent != 4.2 {
		t.Errorf("watchtower CPUPercent = %v, want 4.2", wt.CPUPercent)
	}
	if wt.MemoryUsed != 124*1024*1024 {
		t.Errorf("watchtower MemoryUsed = %d, want %d", wt.MemoryUsed, 124*1024*1024)
	}
	if wt.MemoryLimit != 512*1024*1024 {
		t.Errorf("watchtower MemoryLimit = %d, want %d", wt.MemoryLimit, 512*1024*1024)
	}
	if ag := got["agent"]; ag.CPUPercent != 1 || ag.MemoryUsed != 64*1024*1024 {
		t.Errorf("agent stats = %+v", ag)
	}

	if len(f.runCalls) != 2 {
		t.Fatalf("run calls = %v, want a ps call and a stats call", f.runCalls)
	}
	wantStats := "stats --no-stream --format {{json .}}"
	if got := strings.Join(f.runCalls[1], " "); got != wantStats {
		t.Errorf("stats argv = %q, want %q", got, wantStats)
	}
}

// TestHostContainers_ContainerStats_JoinsOnShortID pins the ID normalization:
// `docker stats` keys by the 12-char short form, so a full-length ID from `ps`
// must be truncated before the join.
func TestHostContainers_ContainerStats_JoinsOnShortID(t *testing.T) {
	longID := "bbb444555666" + strings.Repeat("f", 52)
	ps := `{"ID":"` + longID + `","Names":"watchtower","State":"running","Labels":""}`
	stats := `{"ID":"bbb444555666","Name":"watchtower","CPUPerc":"2.50%","MemUsage":"10MiB / 20MiB"}`

	h := &HostContainers{docker: &fakeDockerRunner{runFunc: hostRunFunc(ps, stats, nil)}}
	got, err := h.ContainerStats(context.Background())
	if err != nil {
		t.Fatalf("ContainerStats() error = %v", err)
	}
	if got["watchtower"].CPUPercent != 2.5 {
		t.Errorf("ContainerStats() = %+v, want the long ID joined on its short form", got)
	}
}

func TestHostContainers_ContainerStats_NoneRunning(t *testing.T) {
	ps := `{"ID":"ccc777888999","Names":"pg-scratch","State":"exited","Labels":""}`
	h := &HostContainers{docker: &fakeDockerRunner{runFunc: hostRunFunc(ps, "", nil)}}

	got, err := h.ContainerStats(context.Background())
	if err != nil {
		t.Fatalf("ContainerStats() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ContainerStats() = %+v, want empty", got)
	}
}

// TestHostContainers_ContainerStats_SkipsStatsCallWhenEmpty pins the guard that
// keeps the ~1.5s host-wide `docker stats` (a full SSH round-trip remotely) off
// the 5s refresh tick when the host has no unmanaged containers at all: the
// join against an empty pair list is guaranteed empty.
func TestHostContainers_ContainerStats_SkipsStatsCallWhenEmpty(t *testing.T) {
	allManaged := `{"ID":"aaa111222333","Names":"web","Labels":"com.docker.compose.project=my-app"}`
	f := &fakeDockerRunner{runOut: []byte(allManaged)}
	h := &HostContainers{docker: f}

	got, err := h.ContainerStats(context.Background())
	if err != nil {
		t.Fatalf("ContainerStats() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("stats = %v, want empty", got)
	}
	for _, call := range f.runCalls {
		if call[0] == "stats" {
			t.Errorf("docker stats ran with zero unmanaged containers: %v", f.runCalls)
		}
	}
}

func TestHostContainers_ContainerStats_SkipsUnnamedAndUnidentified(t *testing.T) {
	ps := `{"ID":"","Names":"ghost","State":"running","Labels":""}
{"ID":"bbb444555666","Names":"","State":"running","Labels":""}
{"ID":"ddd000111222","Names":"agent","State":"running","Labels":""}`
	stats := `{"ID":"bbb444555666","Name":"nameless","CPUPerc":"5.00%","MemUsage":"1MiB / 2MiB"}
{"ID":"ddd000111222","Name":"agent","CPUPerc":"1.00%","MemUsage":"1MiB / 2MiB"}`

	h := &HostContainers{docker: &fakeDockerRunner{runFunc: hostRunFunc(ps, stats, nil)}}
	got, err := h.ContainerStats(context.Background())
	if err != nil {
		t.Fatalf("ContainerStats() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ContainerStats() = %+v, want only the named, identified container", got)
	}
	if _, ok := got["agent"]; !ok {
		t.Errorf("ContainerStats() = %+v, want agent", got)
	}
}

func TestHostContainers_ContainerStats_StatsError(t *testing.T) {
	f := &fakeDockerRunner{runFunc: hostRunFunc(hostPsMixed, "", errors.New("cannot connect to the docker daemon"))}
	h := &HostContainers{docker: f}

	got, err := h.ContainerStats(context.Background())
	if err == nil {
		t.Fatalf("expected an error, got %+v", got)
	}
	if !strings.Contains(err.Error(), "fetching host container stats") ||
		!strings.Contains(err.Error(), "cannot connect to the docker daemon") {
		t.Errorf("error = %v, want it to wrap the stats failure", err)
	}
}

func TestHostContainers_ContainerStats_PsError(t *testing.T) {
	f := &fakeDockerRunner{runErr: errors.New("boom")}
	h := &HostContainers{docker: f}

	if _, err := h.ContainerStats(context.Background()); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "listing host containers") {
		t.Errorf("error = %v, want it to wrap the ps failure", err)
	}
	if len(f.runCalls) != 1 {
		t.Errorf("run calls = %v, want the stats call skipped after a failed ps", f.runCalls)
	}
}

func TestHostContainers_ContainerStats_StatsParseError(t *testing.T) {
	f := &fakeDockerRunner{runFunc: hostRunFunc(hostPsMixed, "not json", nil)}
	h := &HostContainers{docker: f}

	if _, err := h.ContainerStats(context.Background()); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "parsing stats output") {
		t.Errorf("error = %v, want a stats parse error", err)
	}
}

func TestHostContainers_Logs_Args(t *testing.T) {
	tests := []struct {
		name   string
		follow bool
		tail   int
		want   string
	}{
		{"plain", false, 0, "logs web"},
		{"follow", true, 0, "logs --follow web"},
		{"tail", false, 50, "logs --tail 50 web"},
		{"follow and tail", true, 50, "logs --follow --tail 50 web"},
		{"negative tail is dropped", false, -1, "logs web"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeDockerRunner{streamOut: "hello\n"}
			h := &HostContainers{docker: f}
			var buf strings.Builder
			if err := h.Logs(context.Background(), "web", tt.follow, tt.tail, &buf); err != nil {
				t.Fatalf("Logs() error = %v", err)
			}
			if len(f.streamCalls) != 1 {
				t.Fatalf("stream calls = %d, want 1", len(f.streamCalls))
			}
			if got := strings.Join(f.streamCalls[0], " "); got != tt.want {
				t.Errorf("stream args = %q, want %q", got, tt.want)
			}
			if buf.String() != "hello\n" {
				t.Errorf("writer got %q, want the streamed output", buf.String())
			}
			if len(f.runCalls) != 0 {
				t.Errorf("Logs() must not capture through run: %v", f.runCalls)
			}
		})
	}
}

func TestHostContainers_Logs_StreamError(t *testing.T) {
	f := &fakeDockerRunner{streamErr: errors.New("no such container")}
	h := &HostContainers{docker: f}

	if err := h.Logs(context.Background(), "ghost", false, 0, io.Discard); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "no such container") {
		t.Errorf("error = %v, want the stream failure", err)
	}
}

// TestHostContainers_Logs_LocalWiresStdoutAndStderr pins the R2 requirement:
// `docker logs` writes the container's stderr to its OWN stderr, and that is
// where most application logs land. Both streams must reach w or the log
// viewer silently drops half the output.
func TestHostContainers_Logs_LocalWiresStdoutAndStderr(t *testing.T) {
	var stdout, stderr io.Writer
	var captured []string
	c := New(t.TempDir())
	c.SetTestHooks(func(cmd *exec.Cmd) error {
		captured = append([]string(nil), cmd.Args...)
		stdout, stderr = cmd.Stdout, cmd.Stderr
		return nil
	}, nil)

	var buf strings.Builder
	if err := NewLocalHostContainers(c).Logs(context.Background(), "web", true, 50, &buf); err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	want := []string{"docker", "logs", "--follow", "--tail", "50", "web"}
	if strings.Join(captured, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %v, want %v", captured, want)
	}
	if stdout != io.Writer(&buf) {
		t.Errorf("Stdout = %v, want the caller's writer", stdout)
	}
	if stderr != io.Writer(&buf) {
		t.Errorf("Stderr = %v, want the caller's writer", stderr)
	}
}

// TestHostContainers_Logs_RemoteEscapesAndHasNoTTY pins that the remote log
// stream shell-escapes the container name and allocates no pseudo-terminal.
func TestHostContainers_Logs_RemoteEscapesAndHasNoTTY(t *testing.T) {
	var captured []string
	var stdout, stderr io.Writer
	extras := []string{"-p", "2222"}
	r := &RemoteCompose{Host: "user@example.com", SocketPath: "/tmp/cdeploy-ctrl-abc-99", SSHExtraArgs: extras}
	r.SetTestHooks(func(cmd *exec.Cmd) error {
		captured = append([]string(nil), cmd.Args...)
		stdout, stderr = cmd.Stdout, cmd.Stderr
		return nil
	}, nil)

	var buf strings.Builder
	if err := NewRemoteHostContainers(r).Logs(context.Background(), "we;rm -rf /", true, 50, &buf); err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	assertExtraBeforeHost(t, "HostContainers Logs", captured, "user@example.com", extras)
	if slices.Contains(captured, "-t") {
		t.Errorf("log stream argv %v allocates a TTY", captured)
	}
	remoteCmd := captured[len(captured)-1]
	want := `docker 'logs' '--follow' '--tail' '50' 'we;rm -rf /'`
	if remoteCmd != want {
		t.Errorf("remote command = %q, want %q", remoteCmd, want)
	}
	if stdout != io.Writer(&buf) || stderr != io.Writer(&buf) {
		t.Errorf("Stdout/Stderr = %v/%v, want the caller's writer", stdout, stderr)
	}
}

func TestHostContainers_ExecCommand_DefaultCommand(t *testing.T) {
	f := &fakeDockerRunner{}
	h := &HostContainers{docker: f}

	if _, err := h.ExecCommand(context.Background(), "web", nil); err != nil {
		t.Fatalf("ExecCommand() error = %v", err)
	}
	if len(f.ttyCalls) != 1 {
		t.Fatalf("tty calls = %d, want 1", len(f.ttyCalls))
	}
	want := append([]string{"exec", "-it", "web"}, DefaultExecCommand...)
	if strings.Join(f.ttyCalls[0], "\x00") != strings.Join(want, "\x00") {
		t.Errorf("tty args = %v, want %v", f.ttyCalls[0], want)
	}
	if len(f.runCalls) != 0 || len(f.streamCalls) != 0 {
		t.Error("ExecCommand() must go through the tty seam only")
	}
}

func TestHostContainers_ExecCommand_ExplicitCommand(t *testing.T) {
	f := &fakeDockerRunner{}
	h := &HostContainers{docker: f}

	if _, err := h.ExecCommand(context.Background(), "web", []string{"ls", "-la"}); err != nil {
		t.Fatalf("ExecCommand() error = %v", err)
	}
	want := []string{"exec", "-it", "web", "ls", "-la"}
	if strings.Join(f.ttyCalls[0], " ") != strings.Join(want, " ") {
		t.Errorf("tty args = %v, want %v", f.ttyCalls[0], want)
	}
}

func TestHostContainers_ExecCommand_LocalArgv(t *testing.T) {
	c := New(t.TempDir())
	cmd, err := NewLocalHostContainers(c).ExecCommand(context.Background(), "web", []string{"sh"})
	if err != nil {
		t.Fatalf("ExecCommand() error = %v", err)
	}
	want := []string{"docker", "exec", "-it", "web", "sh"}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", cmd.Args, want)
	}
}

// TestHostContainers_ExecCommand_RemoteAllocatesTTY pins the other half of the
// three-method seam: the interactive exec path MUST allocate a TTY, or the
// remote shell has no job control, prompt, or line editing.
func TestHostContainers_ExecCommand_RemoteAllocatesTTY(t *testing.T) {
	extras := []string{"-p", "2222"}
	r := &RemoteCompose{Host: "user@example.com", SocketPath: "/tmp/cdeploy-ctrl-abc-99", SSHExtraArgs: extras}

	cmd, err := NewRemoteHostContainers(r).ExecCommand(context.Background(), "web", nil)
	if err != nil {
		t.Fatalf("ExecCommand() error = %v", err)
	}
	assertExtraBeforeHost(t, "HostContainers ExecCommand", cmd.Args, "user@example.com", extras)
	if !slices.Contains(cmd.Args, "-t") {
		t.Errorf("exec argv %v has no -t; the remote exec path must allocate a TTY", cmd.Args)
	}
	remoteCmd := cmd.Args[len(cmd.Args)-1]
	if !strings.HasPrefix(remoteCmd, `docker 'exec' '-it' 'web' `) {
		t.Errorf("remote command = %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "exec bash") {
		t.Errorf("remote command = %q, want the DefaultExecCommand fallback shell", remoteCmd)
	}
}

// --- Inspect ---

// hostInspectRunFunc dispatches a fakeDockerRunner by subcommand so a single
// fake serves both calls Inspect makes: the `ps` discovery and the `inspect`
// itself.
func hostInspectRunFunc(ps, inspect string, inspectErr error) func(args []string) ([]byte, error) {
	return func(args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "inspect" {
			return []byte(inspect), inspectErr
		}
		return []byte(ps), nil
	}
}

func TestHostContainers_Inspect(t *testing.T) {
	f := &fakeDockerRunner{runFunc: hostInspectRunFunc(hostPsMixed, `[{"Name":"/watchtower"}]`, nil)}
	h := &HostContainers{docker: f}

	raw, err := h.Inspect(context.Background(), "watchtower")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if got, want := string(raw), `[{"Name":"/watchtower"}]`; got != want {
		t.Errorf("raw = %q, want %q (bytes must be verbatim)", got, want)
	}
	if len(f.runCalls) != 2 {
		t.Fatalf("run calls = %v, want exactly 2 (ps + inspect)", f.runCalls)
	}
	if got, want := strings.Join(f.runCalls[0], " "), strings.Join(hostPsArgs, " "); got != want {
		t.Errorf("discovery argv = %q, want %q", got, want)
	}
	if got, want := strings.Join(f.runCalls[1], " "), "inspect bbb444555666"; got != want {
		t.Errorf("inspect argv = %q, want %q", got, want)
	}
}

// TestHostContainers_Inspect_CommaJoinedNames pins that resolution reuses
// hostContainerName, so a container with aliases is addressed by its first name
// — the same name ListServices and ContainerStatus key on.
func TestHostContainers_Inspect_CommaJoinedNames(t *testing.T) {
	f := &fakeDockerRunner{runFunc: hostInspectRunFunc(hostPsMixed, `[{"Name":"/pg-scratch"}]`, nil)}
	h := &HostContainers{docker: f}

	if _, err := h.Inspect(context.Background(), "pg-scratch"); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if got, want := strings.Join(f.runCalls[1], " "), "inspect ccc777888999"; got != want {
		t.Errorf("inspect argv = %q, want %q", got, want)
	}
}

func TestHostContainers_Inspect_NoMatch(t *testing.T) {
	f := &fakeDockerRunner{runFunc: hostInspectRunFunc(hostPsMixed, `[{}]`, nil)}
	h := &HostContainers{docker: f}

	_, err := h.Inspect(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected an error for a name with no container")
	}
	if want := `no container found for "nope"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	if len(f.runCalls) != 1 {
		t.Errorf("run calls = %v, want only the ps call", f.runCalls)
	}
}

// TestHostContainers_Inspect_ComposeManagedNotAddressable pins that resolution
// goes through the unmanaged set: a compose-managed container is owned by the
// compose composers, so it must not be reachable from this view.
func TestHostContainers_Inspect_ComposeManagedNotAddressable(t *testing.T) {
	f := &fakeDockerRunner{runFunc: hostInspectRunFunc(hostPsMixed, `[{"Name":"/web"}]`, nil)}
	h := &HostContainers{docker: f}

	if _, err := h.Inspect(context.Background(), "web"); err == nil {
		t.Fatal("expected the compose-managed container to be unreachable")
	} else if want := `no container found for "web"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func TestHostContainers_Inspect_PsFailurePropagates(t *testing.T) {
	f := &fakeDockerRunner{runErr: errors.New("docker daemon not running")}
	h := &HostContainers{docker: f}

	_, err := h.Inspect(context.Background(), "watchtower")
	if err == nil {
		t.Fatal("expected the ps failure to propagate")
	}
	if !strings.Contains(err.Error(), "listing host containers") ||
		!strings.Contains(err.Error(), "docker daemon not running") {
		t.Errorf("error = %v, want it to wrap the discovery failure", err)
	}
}

func TestHostContainers_Inspect_InspectFailurePropagates(t *testing.T) {
	f := &fakeDockerRunner{
		runFunc: hostInspectRunFunc(hostPsMixed, "", errors.New("No such object: bbb444555666")),
	}
	h := &HostContainers{docker: f}

	_, err := h.Inspect(context.Background(), "watchtower")
	if err == nil {
		t.Fatal("expected the inspect failure to propagate")
	}
	if !strings.Contains(err.Error(), "inspecting container bbb444555666") ||
		!strings.Contains(err.Error(), "No such object") {
		t.Errorf("error = %v, want it to name the container and carry the docker failure", err)
	}
}

// TestHostContainers_Inspect_TransportErrorPropagates pins the "no new
// plumbing" claim: the run seam already classifies an SSH failure, so the
// sentinel must survive Inspect's wrapping and reach the caller intact.
func TestHostContainers_Inspect_TransportErrorPropagates(t *testing.T) {
	f := &fakeDockerRunner{runFunc: func(args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "inspect" {
			return nil, fmt.Errorf("%w: ssh: connect to host db1 port 22: no route to host", errSSHTransport)
		}
		return []byte(hostPsMixed), nil
	}}
	h := &HostContainers{docker: f}

	_, err := h.Inspect(context.Background(), "watchtower")
	if err == nil {
		t.Fatal("expected the transport failure to propagate")
	}
	if !errors.Is(err, errSSHTransport) {
		t.Errorf("error = %v, want it to wrap errSSHTransport", err)
	}
}

func TestNewLocalHostContainers_InspectArgv(t *testing.T) {
	var captured [][]string
	c := New(t.TempDir())
	c.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		captured = append(captured, append([]string(nil), cmd.Args...))
		if slices.Contains(cmd.Args, "inspect") {
			return []byte(`[{"Name":"/watchtower"}]`), nil
		}
		return []byte(hostPsMixed), nil
	})

	if _, err := NewLocalHostContainers(c).Inspect(context.Background(), "watchtower"); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("argv calls = %v, want 2 (ps + inspect)", captured)
	}
	want := []string{"docker", "inspect", "bbb444555666"}
	if !slices.Equal(captured[1], want) {
		t.Errorf("inspect argv = %v, want %v", captured[1], want)
	}
	if slices.Contains(captured[1], "compose") {
		t.Errorf("inspect argv %v must not go through compose", captured[1])
	}
}

func TestNewRemoteHostContainers_InspectSplicesSSHExtraArgs(t *testing.T) {
	extras := []string{"-p", "2222"}
	host := "user@example.com"
	var captured [][]string

	r := &RemoteCompose{Host: host, SocketPath: "/tmp/cdeploy-ctrl-abc-99", SSHExtraArgs: extras}
	r.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		captured = append(captured, append([]string(nil), cmd.Args...))
		if strings.Contains(cmd.Args[len(cmd.Args)-1], "'inspect'") {
			return []byte(`[{"Name":"/watchtower"}]`), nil
		}
		return []byte(hostPsMixed), nil
	})

	raw, err := NewRemoteHostContainers(r).Inspect(context.Background(), "watchtower")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if got, want := string(raw), `[{"Name":"/watchtower"}]`; got != want {
		t.Errorf("raw = %q, want %q", got, want)
	}
	if len(captured) != 2 {
		t.Fatalf("argv calls = %v, want 2 (ps + inspect)", captured)
	}
	assertExtraBeforeHost(t, "HostContainers inspect ps", captured[0], host, extras)
	assertExtraBeforeHost(t, "HostContainers inspect", captured[1], host, extras)

	remoteCmd := captured[1][len(captured[1])-1]
	if want := `docker 'inspect' 'bbb444555666'`; remoteCmd != want {
		t.Errorf("remote inspect command = %q, want %q (container ID shell-escaped)", remoteCmd, want)
	}
	if strings.Contains(remoteCmd, "compose") {
		t.Errorf("remote inspect command must NOT go through compose: %q", remoteCmd)
	}
}

// --- GroupHost ---

func TestLabelValue(t *testing.T) {
	tests := []struct {
		name   string
		labels string
		key    string
		want   string
		wantOK bool
	}{
		{"at start", "com.docker.compose.project=shop,other=x", composeProjectLabel, "shop", true},
		{"mid string", "a=1,com.docker.compose.project=shop,b=2", composeProjectLabel, "shop", true},
		{"at end", "a=1,com.docker.compose.project=shop", composeProjectLabel, "shop", true},
		{"absent", "a=1,b=2", composeProjectLabel, "", false},
		{"empty labels", "", composeProjectLabel, "", false},
		{"empty value", "com.docker.compose.project=,a=1", composeProjectLabel, "", true},
		{"sibling key does not match", "com.docker.compose.project.working_dir=/srv", composeProjectLabel, "", false},
		{"value carrying a comma before the key", "desc=a,b,com.docker.compose.project=shop", composeProjectLabel, "shop", true},
		{"service key", "com.docker.compose.project=shop,com.docker.compose.service=api", composeServiceLabel, "api", true},
		{"not matched inside another value", "note=xcom.docker.compose.project=fake", composeProjectLabel, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := labelValue(tt.labels, tt.key)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("labelValue(%q, %q) = (%q, %v), want (%q, %v)", tt.labels, tt.key, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// hostPsGrouped carries two compose projects (one of them with a scaled
// service) plus a hand-started container, so one fixture exercises grouping,
// replica aggregation and the unmanaged bucket at once.
const hostPsGrouped = `{"ID":"aaa111222333","Names":"shop-api-1","Image":"shop/api:1","State":"running","Status":"Up 3 hours (healthy)","Ports":"0.0.0.0:8080->80/tcp, :::8080->80/tcp","Labels":"com.docker.compose.project=shop,com.docker.compose.service=api,com.docker.compose.project.working_dir=/srv/shop","CreatedAt":"2026-08-19 10:00:00 +0300 EEST"}
{"ID":"bbb444555666","Names":"shop-api-2","Image":"shop/api:1","State":"running","Status":"Up 5 hours (unhealthy)","Ports":"0.0.0.0:8081->80/tcp","Labels":"com.docker.compose.project=shop,com.docker.compose.service=api","CreatedAt":"2026-08-18 09:00:00 +0300 EEST"}
{"ID":"ccc777888999","Names":"shop-db-1","Image":"postgres:16","State":"exited","Status":"Exited (0) 4 hours ago","Ports":"","Labels":"com.docker.compose.project=shop,com.docker.compose.service=db","CreatedAt":"2026-08-18 08:15:00 +0300 EEST"}
{"ID":"ddd000111222","Names":"blog-web-1","Image":"nginx:1.27","State":"running","Status":"Up 2 days","Ports":"","Labels":"com.docker.compose.project=blog,com.docker.compose.service=web","CreatedAt":"2026-08-20 09:30:00 +0300 EEST"}
{"ID":"eee333444555","Names":"watchtower","Image":"containrrr/watchtower","State":"running","Status":"Up 10 minutes","Ports":"","Labels":"org.opencontainers.image.title=watchtower","CreatedAt":"2026-08-22 07:00:00 +0300 EEST"}`

func TestHostContainers_GroupedStatus(t *testing.T) {
	f := &fakeDockerRunner{runOut: []byte(hostPsGrouped)}
	h := &HostContainers{docker: f}

	got, err := groupedStatusOf(h)
	if err != nil {
		t.Fatalf("GroupHost() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("groups = %v, want shop, blog and %s", groupNames(got), UnmanagedProjectName)
	}

	shop := got["shop"]
	if len(shop) != 2 {
		t.Fatalf("shop services = %v, want api and db", shop)
	}
	if _, ok := shop["shop-api-1"]; ok {
		t.Error("shop keyed a container name; the service label must win")
	}
	if len(got["blog"]) != 1 || !got["blog"]["web"].Running {
		t.Errorf("blog = %+v, want a running web service", got["blog"])
	}
	un := got[UnmanagedProjectName]
	if len(un) != 1 {
		t.Fatalf("unmanaged = %+v, want the watchtower container only", un)
	}
	if wt := un["watchtower"]; !wt.Running || wt.Uptime != "10m" {
		t.Errorf("watchtower = %+v, want running with a 10m uptime", wt)
	}

	// Two host-wide calls in total — one ps, one stats — regardless of how
	// many projects the host runs. Splitting them across two seam methods
	// listed the containers twice per refresh.
	if len(f.runCalls) != 2 {
		t.Fatalf("run calls = %d, want 2 (ps + stats)", len(f.runCalls))
	}
	if strings.Join(f.runCalls[0], " ") != strings.Join(hostPsArgs, " ") {
		t.Errorf("run args = %v, want %v", f.runCalls[0], hostPsArgs)
	}
	if strings.Join(f.runCalls[1], " ") != strings.Join(hostStatsArgs, " ") {
		t.Errorf("second call = %v, want %v", f.runCalls[1], hostStatsArgs)
	}
}

// TestHostContainers_GroupedStatus_ScaledReplicas pins that the aggregation
// rules match parseContainerStatus: Running is an OR, Health takes the worst
// case, Created the oldest replica, Uptime the longest-running one, and ports
// merge through dedupAndSortPorts.
func TestHostContainers_GroupedStatus_ScaledReplicas(t *testing.T) {
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(hostPsGrouped)}}

	got, err := groupedStatusOf(h)
	if err != nil {
		t.Fatalf("GroupHost() error = %v", err)
	}
	api := got["shop"]["api"]
	if !api.Running {
		t.Error("api Running = false, want true (any replica running)")
	}
	if api.Health != "unhealthy" {
		t.Errorf("api Health = %q, want unhealthy (worst case wins)", api.Health)
	}
	if api.Created != "2026-08-18 09:00" {
		t.Errorf("api Created = %q, want the oldest replica", api.Created)
	}
	if api.Uptime != "5h" {
		t.Errorf("api Uptime = %q, want the longest-running replica", api.Uptime)
	}
	if len(api.Ports) != 2 {
		t.Fatalf("api Ports = %+v, want 2 after the wildcard mirror collapse", api.Ports)
	}
	if api.Ports[0].HostPort != 8080 || api.Ports[1].HostPort != 8081 {
		t.Errorf("api Ports = %+v, want them sorted by host port", api.Ports)
	}

	db := got["shop"]["db"]
	if db.Running || db.Uptime != "" {
		t.Errorf("db = %+v, want stopped with no uptime", db)
	}
	if db.Created != "2026-08-18 08:15" {
		t.Errorf("db Created = %q", db.Created)
	}
}

// TestHostContainers_GroupedStatus_RestartingReplica pins the restarting rule:
// a replica that is neither up nor exited reports "restarting" only while no
// running replica has been seen.
func TestHostContainers_GroupedStatus_RestartingReplica(t *testing.T) {
	ps := `{"ID":"aaa111222333","Names":"shop-api-1","State":"restarting","Status":"Restarting (1) 5 seconds ago","Labels":"com.docker.compose.project=shop,com.docker.compose.service=api","CreatedAt":"2026-08-19 10:00:00 +0300 EEST"}`
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(ps)}}

	got, err := groupedStatusOf(h)
	if err != nil {
		t.Fatalf("GroupHost() error = %v", err)
	}
	api := got["shop"]["api"]
	if api.Running {
		t.Error("api Running = true, want false while restarting")
	}
	if api.Uptime != "restarting" {
		t.Errorf("api Uptime = %q, want restarting", api.Uptime)
	}

	ps += "\n" + `{"ID":"bbb444555666","Names":"shop-api-2","State":"running","Status":"Up 4 hours","Labels":"com.docker.compose.project=shop,com.docker.compose.service=api","CreatedAt":"2026-08-19 11:00:00 +0300 EEST"}`
	h = &HostContainers{docker: &fakeDockerRunner{runOut: []byte(ps)}}
	got, err = groupedStatusOf(h)
	if err != nil {
		t.Fatalf("GroupHost() error = %v", err)
	}
	if api = got["shop"]["api"]; api.Uptime != "4h" || !api.Running {
		t.Errorf("api = %+v, want the running replica to win over the restarting one", api)
	}
}

// TestHostContainers_GroupedStatus_LabelValueWithComma pins the token-start
// read: a preceding label whose value carries a comma must not shift the
// project or service reading, and neither value may swallow the label after it.
func TestHostContainers_GroupedStatus_LabelValueWithComma(t *testing.T) {
	ps := `{"ID":"aaa111222333","Names":"shop-api-1","State":"running","Status":"Up 1 hour","Labels":"org.opencontainers.image.authors=alice,bob,com.docker.compose.project=shop,com.docker.compose.service=api,com.docker.compose.project.config_files=/srv/shop/compose.yml","CreatedAt":"2026-08-19 10:00:00 +0300 EEST"}`
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(ps)}}

	got, err := groupedStatusOf(h)
	if err != nil {
		t.Fatalf("GroupHost() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("groups = %v, want shop only", groupNames(got))
	}
	if _, ok := got["shop"]["api"]; !ok {
		t.Fatalf("shop = %+v, want an api service", got["shop"])
	}
}

// TestHostContainers_GroupedStatus_SiblingKeysAreNotAProject pins the trailing
// "=" rule: a container carrying only com.docker.compose.project.working_dir is
// not compose-managed, so it belongs in the unmanaged bucket.
func TestHostContainers_GroupedStatus_SiblingKeysAreNotAProject(t *testing.T) {
	ps := `{"ID":"aaa111222333","Names":"odd","State":"running","Status":"Up 1 hour","Labels":"com.docker.compose.project.working_dir=/srv/shop","CreatedAt":"2026-08-19 10:00:00 +0300 EEST"}`
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(ps)}}

	got, err := groupedStatusOf(h)
	if err != nil {
		t.Fatalf("GroupHost() error = %v", err)
	}
	if _, ok := got[UnmanagedProjectName]["odd"]; !ok {
		t.Errorf("groups = %v, want the container in the unmanaged bucket", got)
	}
}

// TestHostContainers_GroupedStatus_NameFallbacks pins the two degenerate label
// shapes: an empty project value reads as unmanaged, and a managed container
// with no service label falls back to its container name so the row stays
// addressable.
func TestHostContainers_GroupedStatus_NameFallbacks(t *testing.T) {
	ps := `{"ID":"aaa111222333","Names":"blank-proj","State":"running","Status":"Up 1 hour","Labels":"com.docker.compose.project=","CreatedAt":"2026-08-19 10:00:00 +0300 EEST"}
{"ID":"bbb444555666","Names":"no-svc-label","State":"running","Status":"Up 1 hour","Labels":"com.docker.compose.project=shop","CreatedAt":"2026-08-19 10:00:00 +0300 EEST"}
{"ID":"ccc777888999","Names":"","State":"created","Status":"Created","Labels":"","CreatedAt":"2026-08-19 10:00:00 +0300 EEST"}`
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(ps)}}

	got, err := groupedStatusOf(h)
	if err != nil {
		t.Fatalf("GroupHost() error = %v", err)
	}
	if _, ok := got[UnmanagedProjectName]["blank-proj"]; !ok {
		t.Errorf("unmanaged = %+v, want the empty-project container", got[UnmanagedProjectName])
	}
	if len(got[UnmanagedProjectName]) != 1 {
		t.Errorf("unmanaged = %+v, want the unnamed container dropped", got[UnmanagedProjectName])
	}
	if _, ok := got["shop"]["no-svc-label"]; !ok {
		t.Errorf("shop = %+v, want the container name as the service key", got["shop"])
	}
}

func TestHostContainers_GroupedStatus_EmptyHost(t *testing.T) {
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte("")}}

	got, err := groupedStatusOf(h)
	if err != nil {
		t.Fatalf("GroupHost() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("GroupHost() status = %v, want no groups", got)
	}
}

func TestHostContainers_GroupedStatus_RunError(t *testing.T) {
	h := &HostContainers{docker: &fakeDockerRunner{runErr: errors.New("docker daemon not running")}}

	if _, err := groupedStatusOf(h); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "listing host containers") ||
		!strings.Contains(err.Error(), "docker daemon not running") {
		t.Errorf("error = %v, want it to wrap the run failure", err)
	}
}

func TestHostContainers_GroupedStatus_ParseError(t *testing.T) {
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte("not json")}}

	if _, err := groupedStatusOf(h); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "parsing host containers") {
		t.Errorf("error = %v, want a parse error", err)
	}
}

// TestHostContainers_GroupedStatus_RealFixture runs the live-daemon capture
// through the grouping path, so the label reading is validated against real
// output rather than only the hand-written constants above.
func TestHostContainers_GroupedStatus_RealFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_ps_host.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	h := &HostContainers{docker: &fakeDockerRunner{runOut: data}}

	got, err := groupedStatusOf(h)
	if err != nil {
		t.Fatalf("GroupHost() error = %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("groups = %v, want at least one project plus the unmanaged bucket", groupNames(got))
	}

	// Every grouped row must also be reachable through the read paths the
	// screen already uses: the unmanaged bucket must match ListServices.
	names, err := h.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices() error = %v", err)
	}
	if len(got[UnmanagedProjectName]) != len(names) {
		t.Errorf("unmanaged group = %d rows, ListServices = %d", len(got[UnmanagedProjectName]), len(names))
	}
	for _, name := range names {
		if _, ok := got[UnmanagedProjectName][name]; !ok {
			t.Errorf("unmanaged container %q missing from the grouped map", name)
		}
	}
	for proj, svcs := range got {
		if proj == "" {
			t.Error("a group has an empty project name")
		}
		for svc := range svcs {
			if svc == "" {
				t.Errorf("project %q has a service with an empty name", proj)
			}
		}
	}
}

func groupedStatusOf(h *HostContainers) (map[string]map[string]runner.ServiceStatus, error) {
	snap, err := h.GroupHost(context.Background())
	return snap.Status, err
}

func groupedStatsOf(h *HostContainers) (map[string]map[string]runner.ServiceStats, error) {
	snap, err := h.GroupHost(context.Background())
	if err != nil {
		return nil, err
	}
	return snap.Stats, snap.StatsErr
}

func groupNames(groups map[string]map[string]runner.ServiceStatus) []string {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// hostStatsGrouped pairs with hostPsGrouped by container ID. shop-db-1 is
// deliberately absent: it is exited, so `docker stats` never reports it.
const hostStatsGrouped = `{"ID":"aaa111222333","Name":"shop-api-1","CPUPerc":"10.00%","MemUsage":"100MiB / 512MiB"}
{"ID":"bbb444555666","Name":"shop-api-2","CPUPerc":"5.50%","MemUsage":"50MiB / 512MiB"}
{"ID":"ddd000111222","Name":"blog-web-1","CPUPerc":"1.25%","MemUsage":"32MiB / 256MiB"}
{"ID":"eee333444555","Name":"watchtower","CPUPerc":"0.50%","MemUsage":"16MiB / 256MiB"}`

func TestHostContainers_GroupedStats(t *testing.T) {
	f := &fakeDockerRunner{runFunc: hostRunFunc(hostPsGrouped, hostStatsGrouped, nil)}
	h := &HostContainers{docker: f}

	got, err := groupedStatsOf(h)
	if err != nil {
		t.Fatalf("GroupHost() stats error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("groups = %d, want shop, blog and %s", len(got), UnmanagedProjectName)
	}
	// Replicas sum, exactly as the per-project ContainerStats aggregates them.
	api := got["shop"]["api"]
	if api.CPUPercent != 15.5 {
		t.Errorf("shop/api CPUPercent = %v, want 15.5 (both replicas summed)", api.CPUPercent)
	}
	if api.MemoryUsed != 150*1024*1024 {
		t.Errorf("shop/api MemoryUsed = %d, want %d", api.MemoryUsed, 150*1024*1024)
	}
	// The exited db is in ps but not in stats: skipped, never zero-filled.
	if _, ok := got["shop"]["db"]; ok {
		t.Error("a container absent from docker stats must be skipped, not zero-filled")
	}
	if got["blog"]["web"].CPUPercent != 1.25 {
		t.Errorf("blog/web = %+v", got["blog"]["web"])
	}
	if got[UnmanagedProjectName]["watchtower"].MemoryUsed != 16*1024*1024 {
		t.Errorf("unmanaged watchtower = %+v", got[UnmanagedProjectName]["watchtower"])
	}
	// Two host-wide calls total, regardless of project count.
	if len(f.runCalls) != 2 {
		t.Fatalf("run calls = %d, want 2 (ps + stats)", len(f.runCalls))
	}
	if strings.Join(f.runCalls[0], " ") != strings.Join(hostPsArgs, " ") {
		t.Errorf("first call = %v, want %v", f.runCalls[0], hostPsArgs)
	}
	if strings.Join(f.runCalls[1], " ") != strings.Join(hostStatsArgs, " ") {
		t.Errorf("second call = %v, want %v", f.runCalls[1], hostStatsArgs)
	}
}

// TestHostContainers_GroupedStats_EmptyHostSkipsStatsCall mirrors the guard
// ContainerStats carries: `docker stats --no-stream` is a ~1.5s host-wide call
// that the 5s grouped refresh would otherwise pay forever on an empty host.
func TestHostContainers_GroupedStats_EmptyHostSkipsStatsCall(t *testing.T) {
	f := &fakeDockerRunner{runOut: []byte("")}
	h := &HostContainers{docker: f}

	got, err := groupedStatsOf(h)
	if err != nil {
		t.Fatalf("GroupHost() stats error = %v", err)
	}
	if got != nil {
		t.Errorf("GroupHost() stats = %v, want nil", got)
	}
	if len(f.runCalls) != 1 {
		t.Errorf("docker stats ran with zero containers: %v", f.runCalls)
	}
}

func TestHostContainers_GroupedStats_Errors(t *testing.T) {
	t.Run("ps failure", func(t *testing.T) {
		h := &HostContainers{docker: &fakeDockerRunner{runErr: errors.New("boom")}}
		if _, err := groupedStatsOf(h); err == nil {
			t.Fatal("GroupHost() stats error = nil, want the ps failure")
		}
	})
	t.Run("stats failure", func(t *testing.T) {
		f := &fakeDockerRunner{runFunc: hostRunFunc(hostPsGrouped, "", errors.New("boom"))}
		h := &HostContainers{docker: f}
		_, err := groupedStatsOf(h)
		if err == nil {
			t.Fatal("GroupHost() stats error = nil, want the stats failure")
		}
		// The wrap names WHICH host-wide call failed; its ps sibling is
		// asserted the same way, so the two cannot be told apart by accident.
		if !strings.Contains(err.Error(), "fetching host container stats") || !strings.Contains(err.Error(), "boom") {
			t.Errorf("error = %v, want it to wrap the stats failure", err)
		}
	})
	t.Run("ps failure names the listing", func(t *testing.T) {
		h := &HostContainers{docker: &fakeDockerRunner{runErr: errors.New("boom")}}
		_, err := groupedStatsOf(h)
		if err == nil || !strings.Contains(err.Error(), "listing host containers") {
			t.Errorf("error = %v, want it to wrap the ps failure", err)
		}
	})
	t.Run("unparseable stats", func(t *testing.T) {
		f := &fakeDockerRunner{runFunc: hostRunFunc(hostPsGrouped, "{not json", nil)}
		h := &HostContainers{docker: f}
		if _, err := groupedStatsOf(h); err == nil {
			t.Fatal("GroupHost() stats error = nil, want a stats parse failure")
		}
	})
	t.Run("unparseable ps", func(t *testing.T) {
		h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte("{not json")}}
		if _, err := groupedStatsOf(h); err == nil {
			t.Fatal("GroupHost() stats error = nil, want a parse failure")
		}
	})
}

// TestHostContainers_GroupedStats_SkipsUnjoinableEntries pins the two rows the
// ID→service join cannot use: a ps entry with no container ID (nothing to match
// the stats output against) and one that resolves to no service name at all.
// Both are dropped rather than joined against an empty key, which would collect
// every unnamed container under one bogus service.
func TestHostContainers_GroupedStats_SkipsUnjoinableEntries(t *testing.T) {
	const ps = `{"ID":"","Names":"no-id","Image":"nginx","State":"running","Status":"Up 1 hour","Labels":""}
{"ID":"aaa111222333","Names":"","Image":"nginx","State":"running","Status":"Up 1 hour","Labels":""}
{"ID":"bbb444555666","Names":"keeper","Image":"nginx","State":"running","Status":"Up 1 hour","Labels":""}`
	const stats = `{"ID":"aaa111222333","Name":"anon","CPUPerc":"9.00%","MemUsage":"9MiB / 90MiB"}
{"ID":"bbb444555666","Name":"keeper","CPUPerc":"1.00%","MemUsage":"10MiB / 100MiB"}`
	h := &HostContainers{docker: &fakeDockerRunner{runFunc: hostRunFunc(ps, stats, nil)}}

	got, err := groupedStatsOf(h)
	if err != nil {
		t.Fatalf("GroupHost() stats error = %v", err)
	}
	un := got[UnmanagedProjectName]
	if len(un) != 1 {
		t.Fatalf("unmanaged stats = %+v, want only the addressable container", un)
	}
	if _, ok := un["keeper"]; !ok {
		t.Errorf("unmanaged stats = %+v, want keeper", un)
	}
}

// TestHostContainers_GroupedStats_AllStoppedIsNil pins the whole-host case of
// the "only running containers appear" rule: every container is in ps but none
// is in stats, so there is no group to report and the map is nil rather than a
// set of empty ones.
func TestHostContainers_GroupedStats_AllStoppedIsNil(t *testing.T) {
	const ps = `{"ID":"aaa111222333","Names":"shop-api-1","Image":"nginx","State":"exited","Status":"Exited (0) 4 hours ago","Labels":"com.docker.compose.project=shop,com.docker.compose.service=api"}`
	h := &HostContainers{docker: &fakeDockerRunner{runFunc: hostRunFunc(ps, "", nil)}}

	got, err := groupedStatsOf(h)
	if err != nil {
		t.Fatalf("GroupHost() stats error = %v", err)
	}
	if got != nil {
		t.Errorf("GroupHost() stats = %v, want nil when nothing is running", got)
	}
}

// TestHostContainers_EmptyProjectLabelIsUnmanagedOnBothSides pins that
// isComposeManaged and hostGroupKey agree about a degenerate
// `com.docker.compose.project=` value. They disagreed once: the grouper filed
// such a row under (unmanaged) while unmanagedEntries excluded it, so the row
// rendered but `i` could not inspect it.
func TestHostContainers_EmptyProjectLabelIsUnmanagedOnBothSides(t *testing.T) {
	const ps = `{"ID":"aaa111222333","Names":"orphan","Image":"nginx","State":"running","Status":"Up 1 hour","Labels":"com.docker.compose.project=,com.docker.compose.service=api"}`
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(ps)}}

	got, err := groupedStatusOf(h)
	if err != nil {
		t.Fatalf("GroupHost() error = %v", err)
	}
	if _, ok := got[UnmanagedProjectName]["orphan"]; !ok {
		t.Fatalf("grouped = %v, want orphan in the unmanaged bucket", got)
	}
	names, err := h.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices() error = %v", err)
	}
	if len(names) != 1 || names[0] != "orphan" {
		t.Errorf("ListServices() = %v, want the same row the grouper reported", names)
	}
	status, err := h.ContainerStatus(context.Background())
	if err != nil {
		t.Fatalf("ContainerStatus() error = %v", err)
	}
	if _, ok := status["orphan"]; !ok {
		t.Errorf("ContainerStatus() = %v, want orphan", status)
	}
}

// TestHostContainers_GroupHost_RemoteSplice pins that the grouped read needs
// no new SSH plumbing: both halves go through the same run seam, so
// SSHExtraArgs still land immediately before the host argument and the two argv
// strings are unchanged.
func TestHostContainers_GroupHost_RemoteSplice(t *testing.T) {
	extras := []string{"-p", "2222"}
	host := "user@example.com"
	var captured [][]string
	r := &RemoteCompose{Host: host, SocketPath: "/tmp/cdeploy-ctrl-abc-99", SSHExtraArgs: extras}
	r.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		captured = append(captured, append([]string(nil), cmd.Args...))
		if strings.Contains(cmd.Args[len(cmd.Args)-1], "'stats'") {
			return []byte(hostStatsGrouped), nil
		}
		return []byte(hostPsGrouped), nil
	})

	got, err := groupedStatsOf(NewRemoteHostContainers(r))
	if err != nil {
		t.Fatalf("GroupHost() stats error = %v", err)
	}
	if len(got) != 3 {
		t.Errorf("groups = %d, want 3", len(got))
	}
	if len(captured) != 2 {
		t.Fatalf("ssh invocations = %d, want 2", len(captured))
	}
	for _, args := range captured {
		assertExtraBeforeHost(t, "HostContainers GroupHost", args, host, extras)
	}
	if remoteCmd := captured[0][len(captured[0])-1]; remoteCmd != `docker 'ps' '-a' '--size=false' '--format' '{{json .}}'` {
		t.Errorf("remote ps command = %q", remoteCmd)
	}
	if remoteCmd := captured[1][len(captured[1])-1]; remoteCmd != `docker 'stats' '--no-stream' '--format' '{{json .}}'` {
		t.Errorf("remote stats command = %q", remoteCmd)
	}
}

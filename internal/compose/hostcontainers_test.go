package compose

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
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

func TestParseHostContainers_ArrayForm(t *testing.T) {
	data := []byte(`[{"ID":"aaa111222333","Names":"web","State":"running"},{"ID":"bbb444555666","Names":"db","State":"exited"}]`)

	got, err := parseHostContainers(data)
	if err != nil {
		t.Fatalf("parseHostContainers() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Names != "web" || got[1].Names != "db" {
		t.Errorf("names = %q, %q", got[0].Names, got[1].Names)
	}
}

func TestParseHostContainers_Empty(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   \n  "},
		{"empty array", "[]"},
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
	ttyErr      error
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

func (f *fakeDockerRunner) tty(ctx context.Context, args ...string) (*exec.Cmd, error) {
	f.ttyCalls = append(f.ttyCalls, append([]string(nil), args...))
	if f.ttyErr != nil {
		return nil, f.ttyErr
	}
	return exec.CommandContext(ctx, "docker", args...), nil
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
	wantArgs := []string{"ps", "-a", "--format", "{{json .}}"}
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

func TestHostContainers_ContainerStatus_Empty(t *testing.T) {
	f := &fakeDockerRunner{runOut: []byte("")}
	h := &HostContainers{docker: f}

	got, err := h.ContainerStatus(context.Background())
	if err != nil {
		t.Fatalf("ContainerStatus() error = %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestHostContainers_ContainerStatus_RunError(t *testing.T) {
	f := &fakeDockerRunner{runErr: errors.New("boom")}
	h := &HostContainers{docker: f}

	if _, err := h.ContainerStatus(context.Background()); err == nil {
		t.Fatal("expected an error")
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
			if err := tt.fn(); !errors.Is(err, ErrReadOnly) {
				t.Errorf("%s() error = %v, want ErrReadOnly", tt.name, err)
			}
		})
	}
	if n := len(h.docker.(*fakeDockerRunner).runCalls); n != 0 {
		t.Errorf("a write method reached the docker seam %d time(s)", n)
	}
}

func TestHostContainers_CheckUpdatesStub(t *testing.T) {
	h := &HostContainers{docker: &fakeDockerRunner{}}
	got, err := h.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates() error = %v", err)
	}
	if got != nil {
		t.Errorf("CheckUpdates() = %v, want nil until task 12", got)
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
	want := []string{"docker", "ps", "-a", "--format", "{{json .}}"}
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
		if remoteCmd != `docker 'ps' '-a' '--format' '{{json .}}'` {
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
		cmd, err := rd.tty(context.Background(), "exec", "-it", "web", "sh")
		if err != nil {
			t.Fatalf("tty() error = %v", err)
		}
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

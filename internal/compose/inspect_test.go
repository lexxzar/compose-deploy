package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestPickInspectContainer(t *testing.T) {
	tests := []struct {
		name    string
		entries []psEntry
		service string
		wantID  string
		wantOK  bool
	}{
		{
			name:    "empty slice",
			entries: nil,
			service: "web",
			wantOK:  false,
		},
		{
			name:    "empty service name",
			entries: []psEntry{{ID: "abc", Service: "web", State: "running", Status: "Up 2 hours"}},
			service: "",
			wantOK:  false,
		},
		{
			name:    "no match",
			entries: []psEntry{{ID: "abc", Service: "web", State: "running", Status: "Up 2 hours"}},
			service: "db",
			wantOK:  false,
		},
		{
			name:    "single running match",
			entries: []psEntry{{ID: "abc", Service: "web", State: "running", Status: "Up 2 hours"}},
			service: "web",
			wantID:  "abc",
			wantOK:  true,
		},
		{
			name: "single stopped match falls back to the entry",
			entries: []psEntry{
				{ID: "dead", Service: "web", State: "exited", Status: "Exited (1) 5 minutes ago"},
			},
			service: "web",
			wantID:  "dead",
			wantOK:  true,
		},
		{
			name: "scaled service picks the longest running replica",
			entries: []psEntry{
				{ID: "short", Service: "web", State: "running", Status: "Up 5 minutes"},
				{ID: "long", Service: "web", State: "running", Status: "Up 3 days"},
				{ID: "mid", Service: "web", State: "running", Status: "Up 2 hours"},
			},
			service: "web",
			wantID:  "long",
			wantOK:  true,
		},
		{
			name: "running beats restarting even when restarting comes first",
			entries: []psEntry{
				{ID: "flapping", Service: "web", State: "restarting", Status: "Restarting (1) 2 seconds ago"},
				{ID: "healthy", Service: "web", State: "running", Status: "Up 10 seconds"},
			},
			service: "web",
			wantID:  "healthy",
			wantOK:  true,
		},
		{
			name: "running beats a longer-looking restarting entry",
			entries: []psEntry{
				{ID: "healthy", Service: "web", State: "running", Status: "Up 10 seconds"},
				{ID: "flapping", Service: "web", State: "restarting", Status: "Restarting (1) 2 seconds ago"},
			},
			service: "web",
			wantID:  "healthy",
			wantOK:  true,
		},
		{
			name: "all restarting falls back to the first restarting replica",
			entries: []psEntry{
				{ID: "flap1", Service: "web", State: "restarting", Status: "Restarting (1) 2 seconds ago"},
				{ID: "flap2", Service: "web", State: "restarting", Status: "Restarting (1) 1 second ago"},
			},
			service: "web",
			wantID:  "flap1",
			wantOK:  true,
		},
		{
			name: "other services are ignored",
			entries: []psEntry{
				{ID: "dbid", Service: "db", State: "running", Status: "Up 9 days"},
				{ID: "webid", Service: "web", State: "running", Status: "Up 1 hour"},
			},
			service: "web",
			wantID:  "webid",
			wantOK:  true,
		},
		{
			name: "entry without an ID is skipped",
			entries: []psEntry{
				{ID: "", Service: "web", State: "running", Status: "Up 3 days"},
				{ID: "real", Service: "web", State: "running", Status: "Up 1 hour"},
			},
			service: "web",
			wantID:  "real",
			wantOK:  true,
		},
		{
			name: "only entries without IDs yields no match",
			entries: []psEntry{
				{ID: "", Service: "web", State: "running", Status: "Up 3 days"},
			},
			service: "web",
			wantOK:  false,
		},
		{
			name: "health suffix does not confuse the uptime gate",
			entries: []psEntry{
				{ID: "sick", Service: "web", State: "running", Status: "Up 2 minutes (unhealthy)"},
				{ID: "fine", Service: "web", State: "running", Status: "Up 4 hours (healthy)"},
			},
			service: "web",
			wantID:  "fine",
			wantOK:  true,
		},
		{
			name: "stopped replica loses to a running one",
			entries: []psEntry{
				{ID: "gone", Service: "web", State: "exited", Status: "Exited (0) 1 hour ago"},
				{ID: "live", Service: "web", State: "running", Status: "Up 30 seconds"},
			},
			service: "web",
			wantID:  "live",
			wantOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotOK := pickInspectContainer(tt.entries, tt.service)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotID != tt.wantID {
				t.Errorf("id = %q, want %q", gotID, tt.wantID)
			}
		})
	}
}

// TestPickInspectContainer_MatchesUptimeColumn pins the picker against the Uptime
// column parseContainerStatus renders: on a running+restarting mix both must land
// on the running replica.
func TestPickInspectContainer_MatchesUptimeColumn(t *testing.T) {
	entries := []psEntry{
		{ID: "flapping", Service: "web", State: "restarting", Status: "Restarting (1) 2 seconds ago"},
		{ID: "healthy", Service: "web", State: "running", Status: "Up 45 minutes"},
	}

	id, ok := pickInspectContainer(entries, "web")
	if !ok || id != "healthy" {
		t.Fatalf("picker = (%q, %v), want (\"healthy\", true)", id, ok)
	}

	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal entries: %v", err)
	}
	status, err := parseContainerStatus(raw)
	if err != nil {
		t.Fatalf("parseContainerStatus: %v", err)
	}
	if got := status["web"].Uptime; got != "45m" {
		t.Fatalf("uptime column = %q, want %q — picker and column disagree", got, "45m")
	}
}

func TestPickHostInspectContainer(t *testing.T) {
	tests := []struct {
		name    string
		entries []hostPsEntry
		target  string
		wantID  string
		wantOK  bool
	}{
		{
			name:    "empty slice",
			entries: nil,
			target:  "watchtower",
			wantOK:  false,
		},
		{
			name:    "empty name",
			entries: []hostPsEntry{{ID: "abc", Names: "watchtower"}},
			target:  "",
			wantOK:  false,
		},
		{
			name:    "match",
			entries: []hostPsEntry{{ID: "abc", Names: "watchtower"}},
			target:  "watchtower",
			wantID:  "abc",
			wantOK:  true,
		},
		{
			name:    "comma joined names takes the first",
			entries: []hostPsEntry{{ID: "abc", Names: "watchtower,wt-alias"}},
			target:  "watchtower",
			wantID:  "abc",
			wantOK:  true,
		},
		{
			name:    "alias in a comma joined name does not match",
			entries: []hostPsEntry{{ID: "abc", Names: "watchtower,wt-alias"}},
			target:  "wt-alias",
			wantOK:  false,
		},
		{
			name: "first match wins",
			entries: []hostPsEntry{
				{ID: "one", Names: "pg"},
				{ID: "two", Names: "pg"},
			},
			target: "pg",
			wantID: "one",
			wantOK: true,
		},
		{
			name:    "no match",
			entries: []hostPsEntry{{ID: "abc", Names: "watchtower"}},
			target:  "redis",
			wantOK:  false,
		},
		{
			name:    "entry without an ID is skipped",
			entries: []hostPsEntry{{ID: "", Names: "redis"}, {ID: "real", Names: "redis"}},
			target:  "redis",
			wantID:  "real",
			wantOK:  true,
		},
		{
			name:    "whitespace around the name is trimmed",
			entries: []hostPsEntry{{ID: "abc", Names: " redis , alias"}},
			target:  "redis",
			wantID:  "abc",
			wantOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotOK := pickHostInspectContainer(tt.entries, tt.target)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotID != tt.wantID {
				t.Errorf("id = %q, want %q", gotID, tt.wantID)
			}
		})
	}
}

// --- Inspect: local ---

// inspectComposer builds a Compose whose single outputCmd hook DISPATCHES ON
// ARGV: both the `docker compose ps` call and the top-level `docker inspect`
// call route through that one seam, so a test double must tell them apart.
// Every argv seen is appended to *seen so callers can pin the exact commands.
func inspectComposer(psJSON, inspectJSON string, seen *[][]string) *Compose {
	return &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			*seen = append(*seen, append([]string{}, cmd.Args...))
			args := cmd.Args
			switch {
			case len(args) >= 3 && args[1] == "compose" && args[2] == "ps":
				return []byte(psJSON), nil
			case len(args) >= 2 && args[1] == "inspect":
				return []byte(inspectJSON), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", args)
		},
	}
}

func TestComposeInspect_ArgvAndOutput(t *testing.T) {
	var seen [][]string
	c := inspectComposer(
		`[{"ID":"cid-web","Service":"web","State":"running","Status":"Up 2 hours"}]`,
		`[{"Name":"/proj-web-1"}]`,
		&seen,
	)

	raw, err := c.Inspect(context.Background(), "web")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got, want := string(raw), `[{"Name":"/proj-web-1"}]`; got != want {
		t.Errorf("raw = %q, want %q (must be verbatim docker inspect output)", got, want)
	}
	if len(seen) != 2 {
		t.Fatalf("expected exactly 2 commands (ps + inspect), got %d: %v", len(seen), seen)
	}

	wantPs := []string{"docker", "compose", "ps", "-a", "--format", "json"}
	if !reflect.DeepEqual(seen[0], wantPs) {
		t.Errorf("ps argv = %v, want %v", seen[0], wantPs)
	}
	wantInspect := []string{"docker", "inspect", "cid-web"}
	if !reflect.DeepEqual(seen[1], wantInspect) {
		t.Errorf("inspect argv = %v, want %v", seen[1], wantInspect)
	}
	// `docker inspect` is a top-level docker command, never a compose subcommand.
	for _, a := range seen[1] {
		if a == "compose" {
			t.Errorf("inspect argv must NOT contain \"compose\": %v", seen[1])
		}
	}
}

func TestComposeInspect_PicksTheUptimeReplica(t *testing.T) {
	var seen [][]string
	c := inspectComposer(
		`[{"ID":"flapping","Service":"web","State":"restarting","Status":"Restarting (1) 2 seconds ago"},`+
			`{"ID":"healthy","Service":"web","State":"running","Status":"Up 45 minutes"}]`,
		`[{"Name":"/proj-web-2"}]`,
		&seen,
	)
	if _, err := c.Inspect(context.Background(), "web"); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 commands, got %v", seen)
	}
	if got := seen[1][len(seen[1])-1]; got != "healthy" {
		t.Errorf("inspected container = %q, want %q (the replica the Uptime column shows)", got, "healthy")
	}
}

func TestComposeInspect_NDJSONPsOutput(t *testing.T) {
	var seen [][]string
	c := inspectComposer(
		"{\"ID\":\"cid-a\",\"Service\":\"db\",\"State\":\"running\",\"Status\":\"Up 1 hour\"}\n"+
			"{\"ID\":\"cid-b\",\"Service\":\"web\",\"State\":\"running\",\"Status\":\"Up 2 hours\"}\n",
		`[{"Name":"/proj-web-1"}]`,
		&seen,
	)
	if _, err := c.Inspect(context.Background(), "web"); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got := seen[1][len(seen[1])-1]; got != "cid-b" {
		t.Errorf("inspected container = %q, want %q", got, "cid-b")
	}
}

func TestComposeInspect_NoContainerFound(t *testing.T) {
	var seen [][]string
	c := inspectComposer(
		`[{"ID":"cid-web","Service":"web","State":"running","Status":"Up 2 hours"}]`,
		`[{"Name":"/proj-web-1"}]`,
		&seen,
	)
	_, err := c.Inspect(context.Background(), "db")
	if err == nil {
		t.Fatal("expected an error for a service with no container")
	}
	if want := `no container found for "db"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	if len(seen) != 1 {
		t.Errorf("expected only the ps call, got %v", seen)
	}
}

func TestComposeInspect_EmptyPsOutput(t *testing.T) {
	var seen [][]string
	c := inspectComposer("[]", `[{"Name":"/x"}]`, &seen)
	if _, err := c.Inspect(context.Background(), "web"); err == nil {
		t.Fatal("expected an error when ps returns no containers")
	}
}

func TestComposeInspect_PsFailurePropagates(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		outputCmd: func(*exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("boom: daemon not running")
		},
	}
	_, err := c.Inspect(context.Background(), "web")
	if err == nil {
		t.Fatal("expected the ps failure to propagate")
	}
	if !strings.Contains(err.Error(), "daemon not running") {
		t.Errorf("error = %q, want it to carry the underlying ps failure", err)
	}
}

func TestComposeInspect_MalformedPsOutput(t *testing.T) {
	var seen [][]string
	c := inspectComposer(`[{"ID":`, `[{"Name":"/x"}]`, &seen)
	if _, err := c.Inspect(context.Background(), "web"); err == nil {
		t.Fatal("expected a parse error for malformed ps JSON")
	}
}

func TestComposeInspect_InspectFailurePropagates(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			if len(args) >= 3 && args[1] == "compose" && args[2] == "ps" {
				return []byte(`[{"ID":"cid-web","Service":"web","State":"running","Status":"Up 2 hours"}]`), nil
			}
			return nil, fmt.Errorf("No such object: cid-web")
		},
	}
	_, err := c.Inspect(context.Background(), "web")
	if err == nil {
		t.Fatal("expected the inspect failure to propagate")
	}
	if !strings.Contains(err.Error(), "No such object") {
		t.Errorf("error = %q, want it to carry the docker inspect failure", err)
	}
}

// --- Inspect: remote ---

// remoteInspectComposer builds a RemoteCompose whose single outputCmd hook
// dispatches on the remote shell command (the last argv element). The ps call
// goes through remoteCommand and the inspect call through runRemoteDockerCmd,
// but BOTH land on this one seam.
func remoteInspectComposer(psJSON, inspectJSON string, seen *[][]string, extras []string) *RemoteCompose {
	return &RemoteCompose{
		Host:         "user@example.com",
		ProjectDir:   "/app",
		SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
		SSHExtraArgs: extras,
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			*seen = append(*seen, append([]string{}, cmd.Args...))
			switch {
			case isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "ps"):
				return []byte(psJSON), nil
			case isRemoteShellCmd(cmd.Args, "docker 'inspect'"):
				return []byte(inspectJSON), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
}

func TestRemoteInspect_ArgvAndEscaping(t *testing.T) {
	var seen [][]string
	r := remoteInspectComposer(
		`[{"ID":"cid-web","Service":"web","State":"running","Status":"Up 2 hours"}]`,
		`[{"Name":"/app-web-1"}]`,
		&seen, nil,
	)

	raw, err := r.Inspect(context.Background(), "web")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got, want := string(raw), `[{"Name":"/app-web-1"}]`; got != want {
		t.Errorf("raw = %q, want %q", got, want)
	}
	if len(seen) != 2 {
		t.Fatalf("expected exactly 2 commands (ps + inspect), got %d: %v", len(seen), seen)
	}

	remoteCmd := seen[1][len(seen[1])-1]
	if want := "docker 'inspect' 'cid-web'"; remoteCmd != want {
		t.Errorf("remote inspect command = %q, want %q (container ID shell-escaped)", remoteCmd, want)
	}
	if strings.Contains(remoteCmd, "compose") {
		t.Errorf("remote inspect command must NOT go through compose: %q", remoteCmd)
	}
	// The ps call is a compose subcommand and keeps the cd + CURRENT_UID prefix.
	if psCmd := seen[0][len(seen[0])-1]; !strings.Contains(psCmd, "docker compose 'ps' '-a'") {
		t.Errorf("remote ps command = %q, want a `docker compose ps -a` shape", psCmd)
	}
}

func TestRemoteInspect_ShellEscapesContainerID(t *testing.T) {
	var seen [][]string
	// A container ID can never contain a quote in practice, but the escaping is
	// what keeps the remote command string safe regardless of the ps output.
	r := remoteInspectComposer(
		`[{"ID":"cid'; rm -rf /","Service":"web","State":"running","Status":"Up 2 hours"}]`,
		`[{"Name":"/app-web-1"}]`,
		&seen, nil,
	)
	if _, err := r.Inspect(context.Background(), "web"); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	remoteCmd := seen[1][len(seen[1])-1]
	if want := `docker 'inspect' 'cid'\''; rm -rf /'`; remoteCmd != want {
		t.Errorf("remote inspect command = %q, want %q", remoteCmd, want)
	}
}

func TestRemoteInspect_SSHExtraArgsSplicedBeforeHost(t *testing.T) {
	var seen [][]string
	extras := []string{"-p", "2222"}
	r := remoteInspectComposer(
		`[{"ID":"cid-web","Service":"web","State":"running","Status":"Up 2 hours"}]`,
		`[{"Name":"/app-web-1"}]`,
		&seen, extras,
	)
	if _, err := r.Inspect(context.Background(), "web"); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 commands, got %v", seen)
	}
	assertExtraBeforeHost(t, "Inspect ps", seen[0], "user@example.com", extras)
	assertExtraBeforeHost(t, "Inspect inspect", seen[1], "user@example.com", extras)
}

func TestRemoteInspect_NoContainerFound(t *testing.T) {
	var seen [][]string
	r := remoteInspectComposer(
		`[{"ID":"cid-web","Service":"web","State":"running","Status":"Up 2 hours"}]`,
		`[{"Name":"/app-web-1"}]`,
		&seen, nil,
	)
	_, err := r.Inspect(context.Background(), "db")
	if err == nil {
		t.Fatal("expected an error for a service with no container")
	}
	if want := `no container found for "db"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	if len(seen) != 1 {
		t.Errorf("expected only the ps call, got %v", seen)
	}
}

func TestRemoteInspect_PsFailurePropagates(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(*exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("ssh: connect to host example.com port 22: Connection refused")
		},
	}
	_, err := r.Inspect(context.Background(), "web")
	if err == nil {
		t.Fatal("expected the ps failure to propagate")
	}
	if !strings.Contains(err.Error(), "Connection refused") {
		t.Errorf("error = %q, want it to carry the underlying ps failure", err)
	}
}

func TestRemoteInspect_TransportFailurePropagates(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputErrCmd: func(cmd *exec.Cmd) ([]byte, string, error) {
			return nil, "ssh: connect to host example.com port 22: Connection refused",
				fmt.Errorf("exit status 255")
		},
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			// Only the ps call reaches outputCmd — runRemoteDockerCmd prefers
			// outputErrCmd, which is what classifies the transport failure.
			return []byte(`[{"ID":"cid-web","Service":"web","State":"running","Status":"Up 2 hours"}]`), nil
		},
	}
	_, err := r.Inspect(context.Background(), "web")
	if err == nil {
		t.Fatal("expected the transport failure to propagate")
	}
	if !errors.Is(err, errSSHTransport) {
		t.Errorf("error = %v, want it to wrap errSSHTransport", err)
	}
}

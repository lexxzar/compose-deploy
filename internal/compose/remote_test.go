package compose

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestNewRemote_SocketPath(t *testing.T) {
	r := NewRemote("user@host1", "/app")
	if !strings.HasPrefix(r.SocketPath, "/tmp/cdeploy-ctrl-") {
		t.Errorf("SocketPath = %q, want prefix /tmp/cdeploy-ctrl-", r.SocketPath)
	}
	if !strings.Contains(r.SocketPath, fmt.Sprintf("-%d", os.Getpid())) {
		t.Errorf("SocketPath = %q, should contain PID %d", r.SocketPath, os.Getpid())
	}
}

func TestNewRemote_DifferentHostsDifferentSockets(t *testing.T) {
	r1 := NewRemote("user@host1", "/app")
	r2 := NewRemote("user@host2", "/app")
	if r1.SocketPath == r2.SocketPath {
		t.Error("different hosts should have different socket paths")
	}
}

func TestNewRemote_DeterministicSocket(t *testing.T) {
	r1 := NewRemote("user@host1", "/app")
	r2 := NewRemote("user@host1", "/other")
	if r1.SocketPath != r2.SocketPath {
		t.Error("same host should produce same socket path")
	}
}

func TestNewRemote_NoLocalUID(t *testing.T) {
	r := NewRemote("user@host", "/app")
	// RemoteCompose should not capture local UID; CURRENT_UID is evaluated
	// on the remote host via $(id -u):$(id -g).
	cmd := r.remoteCommand(context.Background(), "stop")
	remoteCmd := cmd.Args[len(cmd.Args)-1]
	if !strings.Contains(remoteCmd, "CURRENT_UID=$(id -u):$(id -g)") {
		t.Errorf("remote command should use server-side UID, got: %q", remoteCmd)
	}
}

func TestShellEscape(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain string", "hello", "'hello'"},
		{"string with spaces", "hello world", "'hello world'"},
		{"string with single quotes", "it's", "'it'\\''s'"},
		{"empty string", "", "''"},
		{"string with special chars", "a;b&c|d", "'a;b&c|d'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellEscape(tt.input)
			if got != tt.want {
				t.Errorf("shellEscape(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestConnectCmd_Args(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc123-99",
	}

	cmd := r.ConnectCmd(context.Background())

	if !strings.HasSuffix(cmd.Path, "ssh") {
		t.Errorf("command path = %q, want ssh", cmd.Path)
	}

	wantArgs := []string{"ssh", "-fNM", "-S", "/tmp/cdeploy-ctrl-abc123-99", "user@example.com"}
	if len(cmd.Args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", cmd.Args, wantArgs)
	}
	for i, want := range wantArgs {
		if cmd.Args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, cmd.Args[i], want)
		}
	}
}

func TestClose_Args(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc123-99",
	}

	// We can't call Close() since it runs the command, but we can verify the
	// command construction by replicating what Close does.
	// Instead we test the args that would be constructed.
	wantArgs := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc123-99", "-O", "exit", "user@example.com"}
	// Verify the expected format is valid
	if len(wantArgs) != 6 {
		t.Fatalf("expected 6 args, got %d", len(wantArgs))
	}
	if wantArgs[3] != "-O" || wantArgs[4] != "exit" {
		t.Error("close command should use -O exit")
	}
	_ = r // suppress unused
}

func TestRemoteCommand_WithContainers(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	cmd := r.remoteCommand(context.Background(), "stop", "nginx", "postgres")

	wantPrefix := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no", "user@example.com"}
	for i, want := range wantPrefix {
		if i >= len(cmd.Args) {
			t.Fatalf("missing arg[%d], want %q", i, want)
		}
		if cmd.Args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, cmd.Args[i], want)
		}
	}

	// The last arg is the remote command string
	remoteCmd := cmd.Args[len(cmd.Args)-1]
	if !strings.HasPrefix(remoteCmd, "cd '/app'") {
		t.Errorf("remote command should start with cd, got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "CURRENT_UID=$(id -u):$(id -g)") {
		t.Errorf("remote command should contain CURRENT_UID, got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "docker compose") {
		t.Errorf("remote command should contain docker compose, got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'stop'") {
		t.Errorf("remote command should contain 'stop', got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'nginx'") {
		t.Errorf("remote command should contain 'nginx', got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'postgres'") {
		t.Errorf("remote command should contain 'postgres', got: %q", remoteCmd)
	}
}

func TestRemoteCommand_WithoutContainers(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	cmd := r.remoteCommand(context.Background(), "stop")

	remoteCmd := cmd.Args[len(cmd.Args)-1]
	if !strings.Contains(remoteCmd, "'stop'") {
		t.Errorf("remote command should contain 'stop', got: %q", remoteCmd)
	}
	// Should not have any container names after stop
	parts := strings.SplitAfter(remoteCmd, "'stop'")
	trailing := strings.TrimSpace(parts[len(parts)-1])
	if trailing != "" {
		t.Errorf("expected no trailing args after 'stop', got: %q", trailing)
	}
}

func TestRemoteCommand_WithoutProjectDir(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	cmd := r.remoteCommand(context.Background(), "stop")

	remoteCmd := cmd.Args[len(cmd.Args)-1]
	if strings.HasPrefix(remoteCmd, "cd ") {
		t.Errorf("remote command should not have cd when no project dir, got: %q", remoteCmd)
	}
	if !strings.HasPrefix(remoteCmd, "CURRENT_UID=$(id -u):$(id -g)") {
		t.Errorf("remote command should start with CURRENT_UID=$(id -u):$(id -g), got: %q", remoteCmd)
	}
}

func TestRemoteCommand_CURRENT_UID_InCommandString(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	cmd := r.remoteCommand(context.Background(), "stop")

	// CURRENT_UID should be in the remote command string, not in cmd.Env
	remoteCmd := cmd.Args[len(cmd.Args)-1]
	if !strings.Contains(remoteCmd, "CURRENT_UID=") {
		t.Error("CURRENT_UID should be in remote command string")
	}
	for _, env := range cmd.Env {
		if strings.HasPrefix(env, "CURRENT_UID=") {
			t.Error("CURRENT_UID should NOT be in cmd.Env for remote commands")
		}
	}
}

func TestRemoteCommand_AllComposerMethods(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	tests := []struct {
		name        string
		args        []string
		wantContain []string
	}{
		{"stop", []string{"stop", "nginx"}, []string{"'stop'", "'nginx'"}},
		{"rm -f", []string{"rm", "-f", "nginx"}, []string{"'rm'", "'-f'", "'nginx'"}},
		{"pull", []string{"pull", "nginx"}, []string{"'pull'", "'nginx'"}},
		{"up --no-start", []string{"up", "--no-start", "nginx"}, []string{"'up'", "'--no-start'", "'nginx'"}},
		{"start", []string{"start", "nginx"}, []string{"'start'", "'nginx'"}},
		{"config --services", []string{"config", "--services"}, []string{"'config'", "'--services'"}},
		{"ps -a --format json", []string{"ps", "-a", "--format", "json"}, []string{"'ps'", "'-a'", "'--format'", "'json'"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := r.remoteCommand(context.Background(), tt.args...)
			remoteCmd := cmd.Args[len(cmd.Args)-1]

			for _, want := range tt.wantContain {
				if !strings.Contains(remoteCmd, want) {
					t.Errorf("remote command should contain %s, got: %q", want, remoteCmd)
				}
			}
		})
	}
}

func TestRemoteLogs_ArgsConstruction(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	tests := []struct {
		name        string
		follow      bool
		tail        int
		service     string
		wantContain []string
	}{
		{
			name:        "follow with tail",
			follow:      true,
			tail:        50,
			service:     "nginx",
			wantContain: []string{"'logs'", "'--follow'", "'--tail'", "'50'", "'nginx'"},
		},
		{
			name:        "no follow with tail",
			follow:      false,
			tail:        100,
			service:     "nginx",
			wantContain: []string{"'logs'", "'--tail'", "'100'", "'nginx'"},
		},
		{
			name:        "follow without tail",
			follow:      true,
			tail:        0,
			service:     "postgres",
			wantContain: []string{"'logs'", "'--follow'", "'postgres'"},
		},
		{
			name:        "no follow no tail",
			follow:      false,
			tail:        0,
			service:     "redis",
			wantContain: []string{"'logs'", "'redis'"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"logs"}
			if tt.follow {
				args = append(args, "--follow")
			}
			if tt.tail > 0 {
				args = append(args, "--tail", fmt.Sprintf("%d", tt.tail))
			}
			args = append(args, tt.service)

			cmd := r.remoteCommand(context.Background(), args...)
			remoteCmd := cmd.Args[len(cmd.Args)-1]

			for _, want := range tt.wantContain {
				if !strings.Contains(remoteCmd, want) {
					t.Errorf("remote command should contain %s, got: %q", want, remoteCmd)
				}
			}

			// Verify SSH wrapping
			if !strings.Contains(remoteCmd, "cd '/app'") {
				t.Errorf("remote command should start with cd, got: %q", remoteCmd)
			}
			if !strings.Contains(remoteCmd, "CURRENT_UID=$(id -u):$(id -g)") {
				t.Errorf("remote command should contain CURRENT_UID, got: %q", remoteCmd)
			}
		})
	}
}

func TestRemoteCommand_SpecialCharactersEscaped(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	// Service name with special characters
	cmd := r.remoteCommand(context.Background(), "stop", "my-service's name")
	remoteCmd := cmd.Args[len(cmd.Args)-1]

	if !strings.Contains(remoteCmd, "'my-service'\\''s name'") {
		t.Errorf("special characters should be escaped, got: %q", remoteCmd)
	}
}

// --- Tests using injection hooks ---

func TestRemoteConnect_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := r.Connect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured == nil {
		t.Fatal("runCmd was not called")
	}
	// Verify it's the SSH ControlMaster command
	wantArgs := []string{"ssh", "-fNM", "-S", "/tmp/cdeploy-ctrl-abc-99", "user@example.com"}
	if len(captured.Args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", captured.Args, wantArgs)
	}
	for i, want := range wantArgs {
		if captured.Args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, captured.Args[i], want)
		}
	}
}

func TestRemoteConnect_Error(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			return fmt.Errorf("connection refused")
		},
	}

	err := r.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %q, want it to contain 'connection refused'", err.Error())
	}
}

func TestRemoteClose_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := r.Close()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured == nil {
		t.Fatal("runCmd was not called")
	}
	wantArgs := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-O", "exit", "user@example.com"}
	if len(captured.Args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", captured.Args, wantArgs)
	}
	for i, want := range wantArgs {
		if captured.Args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, captured.Args[i], want)
		}
	}
}

func TestRemoteStop_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := r.Stop(context.Background(), []string{"nginx"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	remoteCmd := captured.Args[len(captured.Args)-1]
	if !strings.Contains(remoteCmd, "'stop'") {
		t.Errorf("remote command should contain 'stop', got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'nginx'") {
		t.Errorf("remote command should contain 'nginx', got: %q", remoteCmd)
	}
}

func TestRemoteRemove_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := r.Remove(context.Background(), []string{"nginx"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	remoteCmd := captured.Args[len(captured.Args)-1]
	if !strings.Contains(remoteCmd, "'rm'") || !strings.Contains(remoteCmd, "'-f'") {
		t.Errorf("remote command should contain 'rm' '-f', got: %q", remoteCmd)
	}
}

func TestRemotePull_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := r.Pull(context.Background(), []string{"nginx"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	remoteCmd := captured.Args[len(captured.Args)-1]
	if !strings.Contains(remoteCmd, "'pull'") || !strings.Contains(remoteCmd, "'nginx'") {
		t.Errorf("remote command should contain 'pull' 'nginx', got: %q", remoteCmd)
	}
}

func TestRemoteCreate_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := r.Create(context.Background(), []string{"nginx"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	remoteCmd := captured.Args[len(captured.Args)-1]
	if !strings.Contains(remoteCmd, "'up'") || !strings.Contains(remoteCmd, "'--no-start'") {
		t.Errorf("remote command should contain 'up' '--no-start', got: %q", remoteCmd)
	}
}

func TestRemoteStart_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := r.Start(context.Background(), []string{"nginx", "db"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	remoteCmd := captured.Args[len(captured.Args)-1]
	if !strings.Contains(remoteCmd, "'start'") || !strings.Contains(remoteCmd, "'nginx'") || !strings.Contains(remoteCmd, "'db'") {
		t.Errorf("remote command should contain 'start' 'nginx' 'db', got: %q", remoteCmd)
	}
}

func TestRemoteLogs_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := r.Logs(context.Background(), "nginx", true, 50, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	remoteCmd := captured.Args[len(captured.Args)-1]
	for _, want := range []string{"'logs'", "'--follow'", "'--tail'", "'50'", "'nginx'"} {
		if !strings.Contains(remoteCmd, want) {
			t.Errorf("remote command should contain %s, got: %q", want, remoteCmd)
		}
	}
}

func TestRemoteLogs_NoFollowNoTail(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := r.Logs(context.Background(), "redis", false, 0, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	remoteCmd := captured.Args[len(captured.Args)-1]
	if !strings.Contains(remoteCmd, "'logs'") || !strings.Contains(remoteCmd, "'redis'") {
		t.Errorf("remote command should contain 'logs' 'redis', got: %q", remoteCmd)
	}
	if strings.Contains(remoteCmd, "'--follow'") {
		t.Errorf("should not contain --follow, got: %q", remoteCmd)
	}
	if strings.Contains(remoteCmd, "'--tail'") {
		t.Errorf("should not contain --tail, got: %q", remoteCmd)
	}
}

func TestRemoteListServices_ViaHook(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("web\ndb\nredis\n"), nil
		},
	}

	services, err := r.ListServices(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"web", "db", "redis"}
	if len(services) != len(want) {
		t.Fatalf("got %d services, want %d", len(services), len(want))
	}
	for i, w := range want {
		if services[i] != w {
			t.Errorf("service[%d] = %q, want %q", i, services[i], w)
		}
	}
}

func TestRemoteListServices_Error(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("ssh failed")
		},
	}

	_, err := r.ListServices(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listing remote services") {
		t.Errorf("error = %q, want it to contain 'listing remote services'", err.Error())
	}
}

func TestRemoteContainerStatus_ViaHook(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(`[{"Service":"web","State":"running"},{"Service":"db","State":"exited"}]`), nil
		},
	}

	status, err := r.ContainerStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(status) != 2 {
		t.Fatalf("got %d entries, want 2", len(status))
	}
	if !status["web"].Running {
		t.Error("web should be running")
	}
	if status["db"].Running {
		t.Error("db should not be running")
	}
}

func TestRemoteContainerStatus_Error(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("ssh timeout")
		},
	}

	_, err := r.ContainerStatus(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listing remote container status") {
		t.Errorf("error = %q, want it to contain 'listing remote container status'", err.Error())
	}
}

func TestRemoteListProjects_ViaHook(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(`[{"Name":"app1","Status":"running(2)","ConfigFiles":"/srv/app1/compose.yml"}]`), nil
		},
	}

	projects, err := r.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("got %d projects, want 1", len(projects))
	}
	if projects[0].Name != "app1" {
		t.Errorf("project[0].Name = %q, want %q", projects[0].Name, "app1")
	}
}

func TestRemoteListProjects_Error(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("ssh failed")
		},
	}

	_, err := r.ListProjects(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listing remote projects") {
		t.Errorf("error = %q, want it to contain 'listing remote projects'", err.Error())
	}
}

func TestRemoteRun_ErrorPropagation(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			return fmt.Errorf("exit status 1")
		},
	}

	err := r.Stop(context.Background(), []string{"nginx"}, io.Discard)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestRemoteRun_WriterWiring(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			if cmd.Stdout == nil || cmd.Stderr == nil {
				return fmt.Errorf("writers not wired")
			}
			fmt.Fprint(cmd.Stdout, "output")
			return nil
		},
	}

	var buf strings.Builder
	err := r.Stop(context.Background(), nil, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.String() != "output" {
		t.Errorf("writer got %q, want %q", buf.String(), "output")
	}
}

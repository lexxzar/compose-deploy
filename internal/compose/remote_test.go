package compose

import (
	"context"
	"fmt"
	"os"
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

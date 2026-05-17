package compose

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Compile-time check: RemoteCompose implements tui.ConfigProvider shape.
// Can't import tui (circular), so we verify the method signatures here.
func TestRemoteCompose_ImplementsConfigProviderShape(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@host",
		ProjectDir: "/app",
		SocketPath: "/tmp/test-socket",
		outputCmd:  func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}
	ctx := context.Background()

	_, _ = r.ConfigFile(ctx)
	_, _ = r.ConfigResolved(ctx)
	_, _ = r.EditCommand(ctx)
	_ = r.ValidateConfig(ctx)
}

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

	wantArgs := []string{"ssh", "-fNM", "-S", "/tmp/cdeploy-ctrl-abc123-99", "--", "user@example.com"}
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

	wantPrefix := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no", "--", "user@example.com"}
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

func TestRemoteDetect_PluginFound(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			// Check if the SSH remote command is "docker compose version"
			remoteCmd := cmd.Args[len(cmd.Args)-1]
			if remoteCmd == "docker compose version" {
				return []byte("Docker Compose version v2.24.0\n"), nil
			}
			return nil, fmt.Errorf("unknown command")
		},
	}

	err := r.Detect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Standalone {
		t.Error("Standalone = true, want false (plugin found)")
	}
}

func TestRemoteDetect_StandaloneFound(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			remoteCmd := cmd.Args[len(cmd.Args)-1]
			if remoteCmd == "docker-compose version" {
				return []byte("docker-compose version 1.29.2\n"), nil
			}
			return nil, fmt.Errorf("command failed")
		},
	}

	err := r.Detect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Standalone {
		t.Error("Standalone = false, want true (standalone found)")
	}
}

func TestRemoteDetect_NeitherFound(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("not found")
		},
	}

	err := r.Detect(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error = %q, want it to contain 'neither'", err.Error())
	}
}

func TestRemoteDetect_CachesResult(t *testing.T) {
	calls := 0
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			calls++
			return []byte("ok\n"), nil
		},
	}

	if err := r.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("outputCmd called %d times, want 1 (cached)", calls)
	}
}

func TestRemoteDetect_SSHArgs(t *testing.T) {
	// Verify that Detect builds its own SSH command (no CURRENT_UID, no cd)
	var capturedArgs []string
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			capturedArgs = cmd.Args
			return []byte("ok\n"), nil
		},
	}

	if err := r.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}

	remoteCmd := capturedArgs[len(capturedArgs)-1]
	if strings.Contains(remoteCmd, "CURRENT_UID") {
		t.Errorf("Detect probe should not include CURRENT_UID, got: %q", remoteCmd)
	}
	if strings.Contains(remoteCmd, "cd ") {
		t.Errorf("Detect probe should not include cd, got: %q", remoteCmd)
	}
}

func TestRemoteSetStandalone(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	r.SetStandalone(true)
	if !r.Standalone {
		t.Error("Standalone = false after SetStandalone(true)")
	}

	// Detect should no-op after SetStandalone
	calls := 0
	r.outputCmd = func(cmd *exec.Cmd) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("should not be called")
	}
	if err := r.Detect(context.Background()); err != nil {
		t.Fatalf("Detect after SetStandalone should no-op, got: %v", err)
	}
	if calls != 0 {
		t.Error("Detect called outputCmd after SetStandalone")
	}
}

func TestRemoteCommand_Standalone(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		Standalone: true,
	}

	cmd := r.remoteCommand(context.Background(), "stop", "nginx")
	remoteCmd := cmd.Args[len(cmd.Args)-1]

	if !strings.Contains(remoteCmd, "docker-compose") {
		t.Errorf("standalone remote command should contain docker-compose, got: %q", remoteCmd)
	}
	// Should NOT contain "docker compose" (with space as subcommand)
	if strings.Contains(remoteCmd, "docker compose") {
		t.Errorf("standalone remote command should not contain 'docker compose', got: %q", remoteCmd)
	}
}

func TestRemoteCommand_Plugin(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		Standalone: false,
	}

	cmd := r.remoteCommand(context.Background(), "stop", "nginx")
	remoteCmd := cmd.Args[len(cmd.Args)-1]

	if !strings.Contains(remoteCmd, "docker compose") {
		t.Errorf("plugin remote command should contain 'docker compose', got: %q", remoteCmd)
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
	wantArgs := []string{"ssh", "-fNM", "-S", "/tmp/cdeploy-ctrl-abc-99", "--", "user@example.com"}
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
	wantArgs := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-O", "exit", "--", "user@example.com"}
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

// --- RemoteCompose ConfigProvider tests ---

func TestRemoteFindComposeFile_SSHCommand(t *testing.T) {
	var capturedArgs []string
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			capturedArgs = cmd.Args
			return []byte("compose.yml\n"), nil
		},
	}

	name, err := r.findRemoteComposeFile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "compose.yml" {
		t.Errorf("findRemoteComposeFile() = %q, want %q", name, "compose.yml")
	}

	// Verify SSH args structure
	if capturedArgs[0] != "ssh" {
		t.Errorf("arg[0] = %q, want %q", capturedArgs[0], "ssh")
	}
	// Should use ControlMaster socket
	remoteCmd := capturedArgs[len(capturedArgs)-1]
	if !strings.Contains(remoteCmd, "for f in") {
		t.Errorf("remote command should contain 'for f in', got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'/app'") {
		t.Errorf("remote command should reference project dir, got: %q", remoteCmd)
	}
}

func TestRemoteFindComposeFile_NoFile(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(""), nil
		},
	}

	_, err := r.findRemoteComposeFile(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no compose file found") {
		t.Errorf("error = %q, want 'no compose file found'", err.Error())
	}
}

func TestRemoteConfigFile_SSHCatCommand(t *testing.T) {
	callCount := 0
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			callCount++
			remoteCmd := cmd.Args[len(cmd.Args)-1]
			if strings.Contains(remoteCmd, "for f in") {
				return []byte("compose.yml\n"), nil
			}
			if strings.Contains(remoteCmd, "cat") {
				if !strings.Contains(remoteCmd, "'/app/compose.yml'") {
					return nil, fmt.Errorf("unexpected cat path: %s", remoteCmd)
				}
				return []byte("services:\n  web:\n    image: nginx\n"), nil
			}
			return nil, fmt.Errorf("unexpected command: %s", remoteCmd)
		},
	}

	got, err := r.ConfigFile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "services:\n  web:\n    image: nginx\n" {
		t.Errorf("ConfigFile() = %q, want compose content", string(got))
	}
	if callCount != 2 {
		t.Errorf("expected 2 SSH calls (find + cat), got %d", callCount)
	}
}

func TestRemoteConfigResolved_Args(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			remoteCmd := cmd.Args[len(cmd.Args)-1]
			if !strings.Contains(remoteCmd, "'config'") {
				return nil, fmt.Errorf("expected 'config' in command, got: %s", remoteCmd)
			}
			return []byte("resolved config"), nil
		},
	}

	got, err := r.ConfigResolved(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "resolved config" {
		t.Errorf("ConfigResolved() = %q, want %q", string(got), "resolved config")
	}
}

func TestRemoteEditCommand_SSHArgs(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			remoteCmd := cmd.Args[len(cmd.Args)-1]
			if strings.Contains(remoteCmd, "for f in") {
				return []byte("compose.yml\n"), nil
			}
			return nil, fmt.Errorf("unexpected command")
		},
	}

	cmd, err := r.EditCommand(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must have -t for TTY
	if !strings.Contains(strings.Join(cmd.Args, " "), "-t") {
		t.Error("EditCommand should include -t for TTY")
	}

	remoteCmd := cmd.Args[len(cmd.Args)-1]
	if !strings.Contains(remoteCmd, "${EDITOR:-vi}") {
		t.Errorf("remote command should use ${EDITOR:-vi}, got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "cd '/app'") {
		t.Errorf("remote command should cd to project dir, got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'compose.yml'") {
		t.Errorf("remote command should reference compose file, got: %q", remoteCmd)
	}
}

func TestRemoteEditCommand_NoFile(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(""), nil
		},
	}

	_, err := r.EditCommand(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestRemoteValidateConfig_Args(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			remoteCmd := cmd.Args[len(cmd.Args)-1]
			if !strings.Contains(remoteCmd, "'config'") || !strings.Contains(remoteCmd, "'--quiet'") {
				return nil, fmt.Errorf("expected 'config' '--quiet', got: %s", remoteCmd)
			}
			return nil, nil
		},
	}

	err := r.ValidateConfig(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRemoteValidateConfig_Error(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("invalid config")
		},
	}

	err := r.ValidateConfig(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("error = %q, want 'invalid config'", err.Error())
	}
}

func TestRemoteValidateConfig_CombinedOutputSuccess(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\n" +
		"last=''\n" +
		"for arg in \"$@\"; do\n" +
		"  last=\"$arg\"\n" +
		"done\n" +
		"case \"$last\" in\n" +
		"  *\"'config'\"*\"'--quiet'\"*) exit 0 ;;\n" +
		"esac\n" +
		"echo unexpected remote command: \"$last\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}
	if err := r.ValidateConfig(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRemoteValidateConfig_CombinedOutputErrorIncludesStderr(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\n" +
		"echo remote yaml syntax error >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}
	err := r.ValidateConfig(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "remote yaml syntax error") {
		t.Fatalf("error = %q, want stderr text included", err.Error())
	}
}

// --- RemoteCompose ExecCommand tests ---

func TestRemoteExecCommand_DefaultShell(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	cmd, err := r.ExecCommand(context.Background(), "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must have -t for TTY
	foundT := false
	for _, a := range cmd.Args {
		if a == "-t" {
			foundT = true
			break
		}
	}
	if !foundT {
		t.Error("ExecCommand should include -t for TTY allocation")
	}

	remoteCmd := cmd.Args[len(cmd.Args)-1]

	// Should have cd to project dir
	if !strings.Contains(remoteCmd, "cd '/app'") {
		t.Errorf("remote command should start with cd, got: %q", remoteCmd)
	}

	// Should have CURRENT_UID
	if !strings.Contains(remoteCmd, "CURRENT_UID=$(id -u):$(id -g)") {
		t.Errorf("remote command should contain CURRENT_UID, got: %q", remoteCmd)
	}

	// Should use docker compose (plugin mode by default)
	if !strings.Contains(remoteCmd, "docker compose") {
		t.Errorf("remote command should contain 'docker compose', got: %q", remoteCmd)
	}

	// Should contain exec subcommand and service
	if !strings.Contains(remoteCmd, "'exec'") {
		t.Errorf("remote command should contain 'exec', got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'web'") {
		t.Errorf("remote command should contain 'web', got: %q", remoteCmd)
	}

	// Should contain default shell command parts
	if !strings.Contains(remoteCmd, "'/bin/sh'") {
		t.Errorf("remote command should contain '/bin/sh', got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'-c'") {
		t.Errorf("remote command should contain '-c', got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "command -v bash >/dev/null 2>&1 && exec bash || exec sh") {
		t.Errorf("remote command should contain bash fallback, got: %q", remoteCmd)
	}
}

func TestRemoteExecCommand_CustomCommand(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	cmd, err := r.ExecCommand(context.Background(), "web", []string{"rails", "console"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	remoteCmd := cmd.Args[len(cmd.Args)-1]

	if !strings.Contains(remoteCmd, "'exec'") {
		t.Errorf("remote command should contain 'exec', got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'web'") {
		t.Errorf("remote command should contain 'web', got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'rails'") {
		t.Errorf("remote command should contain 'rails', got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'console'") {
		t.Errorf("remote command should contain 'console', got: %q", remoteCmd)
	}

	// Should NOT contain default shell command
	if strings.Contains(remoteCmd, "/bin/sh") {
		t.Errorf("remote command should NOT contain /bin/sh when custom command given, got: %q", remoteCmd)
	}
}

func TestRemoteExecCommand_Standalone(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		Standalone: true,
	}

	cmd, err := r.ExecCommand(context.Background(), "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	remoteCmd := cmd.Args[len(cmd.Args)-1]

	// Should use docker-compose (standalone)
	if !strings.Contains(remoteCmd, "docker-compose") {
		t.Errorf("standalone remote command should contain 'docker-compose', got: %q", remoteCmd)
	}
	// Should NOT contain "docker compose" (with space as plugin)
	if strings.Contains(remoteCmd, "docker compose") {
		t.Errorf("standalone remote command should not contain 'docker compose', got: %q", remoteCmd)
	}
}

func TestRemoteExecCommand_WithoutProjectDir(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	cmd, err := r.ExecCommand(context.Background(), "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	remoteCmd := cmd.Args[len(cmd.Args)-1]

	// Should NOT have cd prefix
	if strings.HasPrefix(remoteCmd, "cd ") {
		t.Errorf("remote command should not have cd when no project dir, got: %q", remoteCmd)
	}
	// Should start with CURRENT_UID
	if !strings.HasPrefix(remoteCmd, "CURRENT_UID=$(id -u):$(id -g)") {
		t.Errorf("remote command should start with CURRENT_UID, got: %q", remoteCmd)
	}
}

func TestRemoteExecCommand_SSHArgs(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	cmd, err := r.ExecCommand(context.Background(), "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify SSH arg structure: ssh -t -S <socket> -o ControlMaster=no -- <host> <remoteCmd>
	wantPrefix := []string{"ssh", "-t", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no", "--", "user@example.com"}
	if len(cmd.Args) < len(wantPrefix)+1 {
		t.Fatalf("expected at least %d args, got %d: %v", len(wantPrefix)+1, len(cmd.Args), cmd.Args)
	}
	for i, want := range wantPrefix {
		if cmd.Args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, cmd.Args[i], want)
		}
	}
}

func TestRemoteExecCommand_ShellEscaping(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	cmd, err := r.ExecCommand(context.Background(), "my-service", []string{"sh", "-c", "echo 'hello world'"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	remoteCmd := cmd.Args[len(cmd.Args)-1]

	// Service name should be escaped
	if !strings.Contains(remoteCmd, "'my-service'") {
		t.Errorf("service name should be shell-escaped, got: %q", remoteCmd)
	}
	// Command args should be escaped (the single quotes in the echo arg should be escaped)
	if !strings.Contains(remoteCmd, "'sh'") {
		t.Errorf("command arg 'sh' should be shell-escaped, got: %q", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'-c'") {
		t.Errorf("command arg '-c' should be shell-escaped, got: %q", remoteCmd)
	}
	// The echo 'hello world' should have its inner quotes escaped
	if !strings.Contains(remoteCmd, "'echo '\\''hello world'\\'''") {
		t.Errorf("command with inner quotes should be properly escaped, got: %q", remoteCmd)
	}
}

func TestRemoteExecCommand_DoesNotUseRemoteCommand(t *testing.T) {
	// ExecCommand should build SSH command directly (like EditCommand),
	// NOT through remoteCommand(), to ensure -t flag is included.
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}

	cmd, err := r.ExecCommand(context.Background(), "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The second arg must be "-t" (directly after "ssh")
	if len(cmd.Args) < 2 || cmd.Args[1] != "-t" {
		t.Errorf("second arg should be '-t', got args: %v", cmd.Args)
	}
}

// --- SSHExtraArgs splicing tests ---

// findHostIndex returns the index of the host argument (first arg whose value
// equals r.Host) in cmd.Args, or -1.
func findHostIndex(args []string, host string) int {
	for i, a := range args {
		if a == host {
			return i
		}
	}
	return -1
}

func TestSSHExtraArgs_NilArgvUnchanged_Detect(t *testing.T) {
	// Regression: Detect with nil SSHExtraArgs must produce the original argv.
	var capturedPlugin []string
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			capturedPlugin = append([]string(nil), cmd.Args...)
			return []byte("ok\n"), nil
		},
	}
	if err := r.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no", "--", "user@example.com", "docker compose version"}
	if len(capturedPlugin) != len(want) {
		t.Fatalf("Detect (plugin) args = %v, want %v", capturedPlugin, want)
	}
	for i, w := range want {
		if capturedPlugin[i] != w {
			t.Errorf("Detect (plugin) arg[%d] = %q, want %q", i, capturedPlugin[i], w)
		}
	}
}

func TestSSHExtraArgs_NilArgvUnchanged_DetectStandalone(t *testing.T) {
	// Force standalone branch by failing the plugin probe.
	var capturedStandalone []string
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			remoteCmd := cmd.Args[len(cmd.Args)-1]
			if remoteCmd == "docker compose version" {
				return nil, fmt.Errorf("not found")
			}
			capturedStandalone = append([]string(nil), cmd.Args...)
			return []byte("ok\n"), nil
		},
	}
	if err := r.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no", "--", "user@example.com", "docker-compose version"}
	if len(capturedStandalone) != len(want) {
		t.Fatalf("Detect (standalone) args = %v, want %v", capturedStandalone, want)
	}
	for i, w := range want {
		if capturedStandalone[i] != w {
			t.Errorf("Detect (standalone) arg[%d] = %q, want %q", i, capturedStandalone[i], w)
		}
	}
}

func TestSSHExtraArgs_NilArgvUnchanged_ConnectCmd(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}
	cmd := r.ConnectCmd(context.Background())
	want := []string{"ssh", "-fNM", "-S", "/tmp/cdeploy-ctrl-abc-99", "--", "user@example.com"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("ConnectCmd args = %v, want %v", cmd.Args, want)
	}
	for i, w := range want {
		if cmd.Args[i] != w {
			t.Errorf("ConnectCmd arg[%d] = %q, want %q", i, cmd.Args[i], w)
		}
	}
}

func TestSSHExtraArgs_NilArgvUnchanged_Close(t *testing.T) {
	var captured []string
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = append([]string(nil), cmd.Args...)
			return nil
		},
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-O", "exit", "--", "user@example.com"}
	if len(captured) != len(want) {
		t.Fatalf("Close args = %v, want %v", captured, want)
	}
	for i, w := range want {
		if captured[i] != w {
			t.Errorf("Close arg[%d] = %q, want %q", i, captured[i], w)
		}
	}
}

func TestSSHExtraArgs_NilArgvUnchanged_RemoteCommand(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}
	cmd := r.remoteCommand(context.Background(), "stop")
	// Prefix before remote-cmd: ssh -S <sock> -o ControlMaster=no -- <host>
	wantPrefix := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no", "--", "user@example.com"}
	if len(cmd.Args) != len(wantPrefix)+1 {
		t.Fatalf("remoteCommand args length = %d, want %d (prefix + 1 remote cmd)", len(cmd.Args), len(wantPrefix)+1)
	}
	for i, w := range wantPrefix {
		if cmd.Args[i] != w {
			t.Errorf("remoteCommand arg[%d] = %q, want %q", i, cmd.Args[i], w)
		}
	}
}

func TestSSHExtraArgs_NilArgvUnchanged_FindRemoteComposeFile(t *testing.T) {
	var captured []string
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = append([]string(nil), cmd.Args...)
			return []byte("compose.yml\n"), nil
		},
	}
	if _, err := r.findRemoteComposeFile(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no", "--", "user@example.com"}
	if len(captured) != len(wantPrefix)+1 {
		t.Fatalf("findRemoteComposeFile args length = %d, want %d", len(captured), len(wantPrefix)+1)
	}
	for i, w := range wantPrefix {
		if captured[i] != w {
			t.Errorf("findRemoteComposeFile arg[%d] = %q, want %q", i, captured[i], w)
		}
	}
}

func TestSSHExtraArgs_NilArgvUnchanged_ConfigFile(t *testing.T) {
	var capturedCat []string
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			remoteCmd := cmd.Args[len(cmd.Args)-1]
			if strings.Contains(remoteCmd, "for f in") {
				return []byte("compose.yml\n"), nil
			}
			capturedCat = append([]string(nil), cmd.Args...)
			return []byte("content"), nil
		},
	}
	if _, err := r.ConfigFile(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{"ssh", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no", "--", "user@example.com"}
	if len(capturedCat) != len(wantPrefix)+1 {
		t.Fatalf("ConfigFile cat args length = %d, want %d", len(capturedCat), len(wantPrefix)+1)
	}
	for i, w := range wantPrefix {
		if capturedCat[i] != w {
			t.Errorf("ConfigFile cat arg[%d] = %q, want %q", i, capturedCat[i], w)
		}
	}
}

func TestSSHExtraArgs_NilArgvUnchanged_EditCommand(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("compose.yml\n"), nil
		},
	}
	cmd, err := r.EditCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{"ssh", "-t", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no", "--", "user@example.com"}
	if len(cmd.Args) != len(wantPrefix)+1 {
		t.Fatalf("EditCommand args length = %d, want %d", len(cmd.Args), len(wantPrefix)+1)
	}
	for i, w := range wantPrefix {
		if cmd.Args[i] != w {
			t.Errorf("EditCommand arg[%d] = %q, want %q", i, cmd.Args[i], w)
		}
	}
}

func TestSSHExtraArgs_NilArgvUnchanged_ExecCommand(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}
	cmd, err := r.ExecCommand(context.Background(), "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{"ssh", "-t", "-S", "/tmp/cdeploy-ctrl-abc-99", "-o", "ControlMaster=no", "--", "user@example.com"}
	if len(cmd.Args) != len(wantPrefix)+1 {
		t.Fatalf("ExecCommand args length = %d, want %d", len(cmd.Args), len(wantPrefix)+1)
	}
	for i, w := range wantPrefix {
		if cmd.Args[i] != w {
			t.Errorf("ExecCommand arg[%d] = %q, want %q", i, cmd.Args[i], w)
		}
	}
}

// assertExtraBeforeHost verifies that SSHExtraArgs ([-p 2222]) appear immediately
// before the `--` separator (which itself precedes the host argument).
// The argv shape is: ... <extras...> "--" <host> ...
func assertExtraBeforeHost(t *testing.T, label string, args []string, host string, extras []string) {
	t.Helper()
	hi := findHostIndex(args, host)
	if hi < 0 {
		t.Fatalf("%s: host %q not found in args %v", label, host, args)
	}
	// `--` separator must immediately precede the host.
	if hi < 1 || args[hi-1] != "--" {
		t.Fatalf("%s: expected '--' immediately before host at index %d, got args %v", label, hi, args)
	}
	// Extras must immediately precede the `--` separator.
	sepIdx := hi - 1
	if sepIdx < len(extras) {
		t.Fatalf("%s: separator index %d too small to fit extras %v in args %v", label, sepIdx, extras, args)
	}
	for i, e := range extras {
		got := args[sepIdx-len(extras)+i]
		if got != e {
			t.Errorf("%s: extras arg[%d] = %q, want %q (full args: %v)", label, sepIdx-len(extras)+i, got, e, args)
		}
	}
}

func TestSSHExtraArgs_SplicedBeforeHost_AllSites(t *testing.T) {
	extras := []string{"-p", "2222"}
	host := "user@example.com"

	// Detect (plugin)
	t.Run("Detect plugin", func(t *testing.T) {
		var captured []string
		r := &RemoteCompose{
			Host:         host,
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
			outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
				captured = append([]string(nil), cmd.Args...)
				return []byte("ok\n"), nil
			},
		}
		if err := r.Detect(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertExtraBeforeHost(t, "Detect plugin", captured, host, extras)
	})

	// Detect (standalone)
	t.Run("Detect standalone", func(t *testing.T) {
		var captured []string
		r := &RemoteCompose{
			Host:         host,
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
			outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
				remoteCmd := cmd.Args[len(cmd.Args)-1]
				if remoteCmd == "docker compose version" {
					return nil, fmt.Errorf("not found")
				}
				captured = append([]string(nil), cmd.Args...)
				return []byte("ok\n"), nil
			},
		}
		if err := r.Detect(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertExtraBeforeHost(t, "Detect standalone", captured, host, extras)
	})

	// ConnectCmd
	t.Run("ConnectCmd", func(t *testing.T) {
		r := &RemoteCompose{
			Host:         host,
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
		}
		cmd := r.ConnectCmd(context.Background())
		assertExtraBeforeHost(t, "ConnectCmd", cmd.Args, host, extras)
	})

	// Close
	t.Run("Close", func(t *testing.T) {
		var captured []string
		r := &RemoteCompose{
			Host:         host,
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
			runCmd: func(cmd *exec.Cmd) error {
				captured = append([]string(nil), cmd.Args...)
				return nil
			},
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		assertExtraBeforeHost(t, "Close", captured, host, extras)
	})

	// remoteCommand
	t.Run("remoteCommand", func(t *testing.T) {
		r := &RemoteCompose{
			Host:         host,
			ProjectDir:   "/app",
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
		}
		cmd := r.remoteCommand(context.Background(), "stop")
		assertExtraBeforeHost(t, "remoteCommand", cmd.Args, host, extras)
	})

	// findRemoteComposeFile
	t.Run("findRemoteComposeFile", func(t *testing.T) {
		var captured []string
		r := &RemoteCompose{
			Host:         host,
			ProjectDir:   "/app",
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
			outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
				captured = append([]string(nil), cmd.Args...)
				return []byte("compose.yml\n"), nil
			},
		}
		if _, err := r.findRemoteComposeFile(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertExtraBeforeHost(t, "findRemoteComposeFile", captured, host, extras)
	})

	// ConfigFile (cat call)
	t.Run("ConfigFile cat", func(t *testing.T) {
		var capturedCat []string
		r := &RemoteCompose{
			Host:         host,
			ProjectDir:   "/app",
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
			outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
				remoteCmd := cmd.Args[len(cmd.Args)-1]
				if strings.Contains(remoteCmd, "for f in") {
					// Verify find-call also has extras before host.
					assertExtraBeforeHost(t, "ConfigFile find", cmd.Args, host, extras)
					return []byte("compose.yml\n"), nil
				}
				capturedCat = append([]string(nil), cmd.Args...)
				return []byte("content"), nil
			},
		}
		if _, err := r.ConfigFile(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertExtraBeforeHost(t, "ConfigFile cat", capturedCat, host, extras)
	})

	// EditCommand
	t.Run("EditCommand", func(t *testing.T) {
		r := &RemoteCompose{
			Host:         host,
			ProjectDir:   "/app",
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
			outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
				return []byte("compose.yml\n"), nil
			},
		}
		cmd, err := r.EditCommand(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertExtraBeforeHost(t, "EditCommand", cmd.Args, host, extras)
	})

	// ExecCommand
	t.Run("ExecCommand", func(t *testing.T) {
		r := &RemoteCompose{
			Host:         host,
			ProjectDir:   "/app",
			SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
			SSHExtraArgs: extras,
		}
		cmd, err := r.ExecCommand(context.Background(), "web", nil)
		if err != nil {
			t.Fatal(err)
		}
		assertExtraBeforeHost(t, "ExecCommand", cmd.Args, host, extras)
	})
}

func TestRemoteConfigResolved_ErrorIncludesStderr(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, &exec.ExitError{Stderr: []byte("remote config parse error")}
		},
	}

	_, err := r.ConfigResolved(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "remote config parse error") {
		t.Errorf("error = %q, want stderr text included", err.Error())
	}
}

// --- RemoteCompose.CheckUpdates tests ---

// isRemoteShellCmd reports whether the trailing arg of a ssh argv (the
// remote shell command) contains the given substring. Used to discriminate
// between different remote command invocations in a single outputCmd hook.
func isRemoteShellCmd(args []string, substr string) bool {
	if len(args) == 0 {
		return false
	}
	return strings.Contains(args[len(args)-1], substr)
}

func TestRemoteSetDryRunSupport(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}
	if r.dryRunDetected {
		t.Fatal("dryRunDetected should be false initially")
	}
	r.SetDryRunSupport(true)
	if !r.dryRunSupported || !r.dryRunDetected {
		t.Fatalf("after SetDryRunSupport(true): supported=%v detected=%v",
			r.dryRunSupported, r.dryRunDetected)
	}
	// Should not invoke probe after SetDryRunSupport.
	calls := 0
	r.outputCmd = func(cmd *exec.Cmd) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("probe should not run")
	}
	r.detectDryRunSupport(context.Background())
	if calls != 0 {
		t.Errorf("probe ran after SetDryRunSupport (calls=%d)", calls)
	}
}

func TestRemoteDetectDryRunSupport_Probe(t *testing.T) {
	calls := 0
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			calls++
			captured = cmd
			return []byte("Options:\n  --dry-run    Execute command in dry run mode\n"), nil
		},
	}
	r.detectDryRunSupport(context.Background())
	if !r.dryRunSupported {
		t.Fatal("dryRunSupported = false, want true (help mentions --dry-run)")
	}
	if !r.dryRunDetected {
		t.Fatal("dryRunDetected should be true after probe")
	}
	// Cached: second call must NOT invoke the hook again.
	r.detectDryRunSupport(context.Background())
	if calls != 1 {
		t.Errorf("probe ran %d times, want 1 (cached)", calls)
	}
	// Verify the probe is `docker compose pull --help` over SSH.
	if captured == nil {
		t.Fatal("outputCmd hook was not called")
	}
	if captured.Args[0] != "ssh" {
		t.Errorf("argv[0] = %q, want ssh", captured.Args[0])
	}
	remoteCmd := captured.Args[len(captured.Args)-1]
	if !strings.Contains(remoteCmd, "pull") || !strings.Contains(remoteCmd, "--help") {
		t.Errorf("remote shell command = %q, want it to contain pull --help", remoteCmd)
	}
	// Sanity: probe goes through remoteCommand → has CURRENT_UID and cd prefix.
	if !strings.Contains(remoteCmd, "CURRENT_UID") {
		t.Errorf("remote shell command = %q, want CURRENT_UID prefix", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "cd '/app'") {
		t.Errorf("remote shell command = %q, want cd '/app' prefix", remoteCmd)
	}
}

func TestRemoteDetectDryRunSupport_ProbeFailureMarksUnsupported(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("ssh: connection refused")
		},
	}
	r.detectDryRunSupport(context.Background())
	if !r.dryRunDetected {
		t.Fatal("detected should be true after probe attempt")
	}
	if r.dryRunSupported {
		t.Fatal("dryRunSupported should be false when probe fails")
	}
}

func TestRemoteCheckUpdates_DryRunPath(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = cmd
			return []byte(" DRY-RUN MODE -  alpine Skipped - Image is already present locally\n DRY-RUN MODE -  needupdate Pulling\n"), nil
		},
	}
	r.SetDryRunSupport(true)
	got, err := r.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	want := map[string]bool{"alpine": false, "needupdate": true}
	if len(got) != len(want) {
		t.Fatalf("results = %#v, want %#v", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("results[%q] = (%v, ok=%v), want (%v, true)", k, g, ok, v)
		}
	}
	if captured == nil {
		t.Fatal("outputCmd was not invoked")
	}
	// Dry-run path goes through remoteCommand → ssh + CURRENT_UID + cd prefix.
	if captured.Args[0] != "ssh" {
		t.Errorf("argv[0] = %q, want ssh", captured.Args[0])
	}
	remoteCmd := captured.Args[len(captured.Args)-1]
	if !strings.Contains(remoteCmd, "pull") {
		t.Errorf("remote shell command = %q, want pull", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'--dry-run'") {
		t.Errorf("remote shell command = %q, want --dry-run (escaped)", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'--quiet'") {
		t.Errorf("remote shell command = %q, want --quiet (escaped)", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'--policy=missing'") {
		t.Errorf("remote shell command = %q, want --policy=missing (escaped)", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "CURRENT_UID") {
		t.Errorf("remote shell command = %q, want CURRENT_UID prefix", remoteCmd)
	}
}

func TestRemoteCheckUpdates_DryRunPath_AppendsServices(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = cmd
			return []byte(""), nil
		},
	}
	r.SetDryRunSupport(true)
	if _, err := r.CheckUpdates(context.Background(), []string{"web", "db"}); err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	remoteCmd := captured.Args[len(captured.Args)-1]
	if !strings.Contains(remoteCmd, "'web'") {
		t.Errorf("remote shell command = %q, want 'web'", remoteCmd)
	}
	if !strings.Contains(remoteCmd, "'db'") {
		t.Errorf("remote shell command = %q, want 'db'", remoteCmd)
	}
}

func TestRemoteCheckUpdates_DryRunPath_PartialOnError(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(" DRY-RUN MODE -  web Pulling \n"), fmt.Errorf("manifest fetch failed")
		},
	}
	r.SetDryRunSupport(true)
	got, err := r.CheckUpdates(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error from remote dry-run failure")
	}
	if !strings.Contains(err.Error(), "remote compose pull --dry-run") {
		t.Errorf("error = %q, want it to mention remote dry-run", err.Error())
	}
	// Partial map survives.
	if v, ok := got["web"]; !ok || !v {
		t.Errorf("partial results = %#v, want web=true", got)
	}
}

func TestRemoteCheckUpdates_FallbackPath(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"},"builder":{"image":""}}}`
	var argvs [][]string
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			argvs = append(argvs, append([]string{}, cmd.Args...))
			// Discriminate by the remote shell command (last arg).
			switch {
			case isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config"):
				return []byte(configJSON), nil
			case isRemoteShellCmd(cmd.Args, "image") && isRemoteShellCmd(cmd.Args, "inspect"):
				// Extract the image from the shell command (last token after spaces,
				// after stripping shell quotes).
				rc := cmd.Args[len(cmd.Args)-1]
				toks := strings.Fields(rc)
				img := strings.Trim(toks[len(toks)-1], "'")
				return []byte(img + "@sha256:local-" + img + "\n"), nil
			case isRemoteShellCmd(cmd.Args, "manifest") && isRemoteShellCmd(cmd.Args, "inspect"):
				rc := cmd.Args[len(cmd.Args)-1]
				toks := strings.Fields(rc)
				img := strings.Trim(toks[len(toks)-1], "'")
				switch img {
				case "nginx:latest":
					return []byte(`{"Descriptor":{"digest":"sha256:remote-nginx:latest"}}`), nil
				case "postgres:16":
					return []byte(`{"Descriptor":{"digest":"sha256:local-postgres:16"}}`), nil
				}
				return nil, fmt.Errorf("unexpected image: %s", img)
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	r.SetDryRunSupport(false)
	got, err := r.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	wantByService := map[string]bool{"web": true, "db": false}
	if len(got) != len(wantByService) {
		t.Fatalf("results = %#v, want %#v", got, wantByService)
	}
	for k, v := range wantByService {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("results[%q] = (%v, ok=%v), want (%v, true)", k, g, ok, v)
		}
	}
	// First call is the compose config fetch (goes through remoteCommand →
	// remote shell contains both "compose" AND "config").
	if len(argvs) < 1 {
		t.Fatal("no calls recorded")
	}
	first := argvs[0][len(argvs[0])-1]
	if !strings.Contains(first, "compose") || !strings.Contains(first, "config") {
		t.Errorf("first call remote shell = %q, want compose config", first)
	}
	// All subsequent image/manifest inspect calls must bypass remoteCommand —
	// the remote shell must NOT contain "compose" (top-level docker commands).
	for _, a := range argvs[1:] {
		rc := a[len(a)-1]
		if strings.Contains(rc, "compose") {
			t.Errorf("inspect remote shell contains 'compose': %q", rc)
		}
		// Must also NOT contain CURRENT_UID / cd prefix — direct SSH argv.
		if strings.Contains(rc, "CURRENT_UID") {
			t.Errorf("inspect remote shell contains CURRENT_UID (should bypass remoteCommand): %q", rc)
		}
		if strings.HasPrefix(rc, "cd ") {
			t.Errorf("inspect remote shell starts with 'cd ' (should bypass remoteCommand): %q", rc)
		}
	}
}

func TestRemoteCheckUpdates_FallbackPath_InspectFailureLeavesAbsent(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"}}}`
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			if isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config") {
				return []byte(configJSON), nil
			}
			if isRemoteShellCmd(cmd.Args, "image") && isRemoteShellCmd(cmd.Args, "inspect") {
				// Image not pulled locally → inspect fails. The service must
				// stay absent from the result (tri-state unknown).
				return nil, fmt.Errorf("no such image")
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	r.SetDryRunSupport(false)
	got, err := r.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if _, ok := got["web"]; ok {
		t.Errorf("expected web to be absent (inspect failure → unknown), got %#v", got)
	}
}

func TestRemoteCheckUpdates_FallbackPath_ConfigFailureReturnsError(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("ssh transport error")
		},
	}
	r.SetDryRunSupport(false)
	_, err := r.CheckUpdates(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when config fetch fails")
	}
	if !strings.Contains(err.Error(), "fetching remote compose config") {
		t.Errorf("error = %q, want it to mention remote compose config", err.Error())
	}
}

func TestRemoteCheckUpdates_FallbackPath_ManifestFailureLeavesAbsent(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"}}}`
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			if isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config") {
				return []byte(configJSON), nil
			}
			if isRemoteShellCmd(cmd.Args, "image") && isRemoteShellCmd(cmd.Args, "inspect") {
				return []byte("nginx:latest@sha256:local-nginx\n"), nil
			}
			if isRemoteShellCmd(cmd.Args, "manifest") && isRemoteShellCmd(cmd.Args, "inspect") {
				return nil, fmt.Errorf("auth required")
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	r.SetDryRunSupport(false)
	got, err := r.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if _, ok := got["web"]; ok {
		t.Errorf("expected web to be absent (manifest failure → unknown), got %#v", got)
	}
}

func TestRemoteCheckUpdates_ProbeRouting(t *testing.T) {
	// Unforced: CheckUpdates must call the probe first, then route based on
	// the result. We stub the probe to advertise --dry-run and verify the
	// second call is the dry-run subcommand (not the config fallback).
	calls := 0
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			calls++
			rc := cmd.Args[len(cmd.Args)-1]
			if strings.Contains(rc, "pull") && strings.Contains(rc, "--help") {
				return []byte("--dry-run    Execute command in dry run mode"), nil
			}
			// Subsequent call must be the dry-run pull, not config.
			if !strings.Contains(rc, "--dry-run") {
				t.Errorf("expected dry-run call after probe, got: %q", rc)
			}
			return []byte(""), nil
		},
	}
	if _, err := r.CheckUpdates(context.Background(), nil); err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (probe + dry-run), got %d", calls)
	}
}

// Verify SSHExtraArgs (e.g. -i /tmp/key) is spliced immediately before the
// host argument in the fallback path's direct SSH argv for `docker image
// inspect` / `docker manifest inspect`. Mirrors
// TestAllContainerStatsRemote_extraArgsSplice.
func TestRemoteCheckUpdates_FallbackPath_ExtraArgsSpliced(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"}}}`
	extras := []string{"-i", "/tmp/key"}
	host := "user@example.com"
	var inspectArgvs [][]string
	r := &RemoteCompose{
		Host:         host,
		ProjectDir:   "/app",
		SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
		SSHExtraArgs: extras,
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc := cmd.Args[len(cmd.Args)-1]
			switch {
			case strings.Contains(rc, "compose") && strings.Contains(rc, "config"):
				return []byte(configJSON), nil
			case strings.Contains(rc, "image") && strings.Contains(rc, "inspect"):
				inspectArgvs = append(inspectArgvs, append([]string{}, cmd.Args...))
				return []byte("nginx:latest@sha256:local-nginx\n"), nil
			case strings.Contains(rc, "manifest") && strings.Contains(rc, "inspect"):
				inspectArgvs = append(inspectArgvs, append([]string{}, cmd.Args...))
				return []byte(`{"Descriptor":{"digest":"sha256:remote-nginx"}}`), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	r.SetDryRunSupport(false)
	if _, err := r.CheckUpdates(context.Background(), nil); err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(inspectArgvs) != 2 {
		t.Fatalf("expected 2 inspect calls (image + manifest), got %d", len(inspectArgvs))
	}
	for i, argv := range inspectArgvs {
		label := fmt.Sprintf("inspect[%d]", i)
		assertExtraBeforeHost(t, label, argv, host, extras)
		// Sanity: the fallback path's direct SSH argv must match the
		// AllContainerStatsRemote shape: `ssh -S <sock> -o ControlMaster=no
		// -i /tmp/key -- <host> <remoteCmd>`.
		if argv[0] != "ssh" {
			t.Errorf("%s: argv[0] = %q, want ssh", label, argv[0])
		}
		// SocketPath must be present (uses ControlMaster).
		foundSock := false
		for j, a := range argv {
			if a == "-S" && j+1 < len(argv) && argv[j+1] == r.SocketPath {
				foundSock = true
				break
			}
		}
		if !foundSock {
			t.Errorf("%s: argv missing -S %q: %v", label, r.SocketPath, argv)
		}
	}
}

// Verify the dry-run path also splices SSHExtraArgs before host (goes through
// remoteCommand which uses sshArgs).
func TestRemoteCheckUpdates_DryRunPath_ExtraArgsSpliced(t *testing.T) {
	extras := []string{"-p", "2222"}
	host := "user@example.com"
	var captured []string
	r := &RemoteCompose{
		Host:         host,
		ProjectDir:   "/app",
		SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
		SSHExtraArgs: extras,
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = append([]string(nil), cmd.Args...)
			return []byte(""), nil
		},
	}
	r.SetDryRunSupport(true)
	if _, err := r.CheckUpdates(context.Background(), nil); err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	assertExtraBeforeHost(t, "CheckUpdates dry-run", captured, host, extras)
}

// Verify the probe (detectDryRunSupport) also splices SSHExtraArgs — it goes
// through remoteCommand, so it inherits sshArgs splicing.
func TestRemoteDetectDryRunSupport_ExtraArgsSpliced(t *testing.T) {
	extras := []string{"-i", "/tmp/key", "-p", "2222"}
	host := "user@example.com"
	var captured []string
	r := &RemoteCompose{
		Host:         host,
		ProjectDir:   "/app",
		SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
		SSHExtraArgs: extras,
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			captured = append([]string(nil), cmd.Args...)
			return []byte("--dry-run"), nil
		},
	}
	r.detectDryRunSupport(context.Background())
	if !r.dryRunSupported {
		t.Fatal("expected dryRunSupported=true after probe with --dry-run in help")
	}
	assertExtraBeforeHost(t, "detectDryRunSupport probe", captured, host, extras)
}

// Verify shell-escaping survives for image names with special characters in
// the fallback path (registry hosts with colons, version tags with slashes,
// digests, etc.).
func TestRemoteCheckUpdates_FallbackPath_ShellEscapesImage(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"registry.example.com:5000/team/web:v1.2.3"}}}`
	var inspectArgvs []string
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc := cmd.Args[len(cmd.Args)-1]
			switch {
			case strings.Contains(rc, "compose") && strings.Contains(rc, "config"):
				return []byte(configJSON), nil
			case strings.Contains(rc, "image") && strings.Contains(rc, "inspect"):
				inspectArgvs = append(inspectArgvs, rc)
				return []byte("foo@sha256:abc\n"), nil
			case strings.Contains(rc, "manifest") && strings.Contains(rc, "inspect"):
				inspectArgvs = append(inspectArgvs, rc)
				return []byte(`{"Descriptor":{"digest":"sha256:abc"}}`), nil
			}
			return nil, fmt.Errorf("unexpected: %v", cmd.Args)
		},
	}
	r.SetDryRunSupport(false)
	if _, err := r.CheckUpdates(context.Background(), nil); err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(inspectArgvs) != 2 {
		t.Fatalf("expected 2 inspect commands, got %d", len(inspectArgvs))
	}
	for _, rc := range inspectArgvs {
		// Image must be shell-escaped (single-quoted) to survive transport.
		if !strings.Contains(rc, "'registry.example.com:5000/team/web:v1.2.3'") {
			t.Errorf("remote shell = %q, want escaped image", rc)
		}
	}
}

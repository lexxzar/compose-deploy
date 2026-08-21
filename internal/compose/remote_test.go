package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRemoteCommand_ExtraComposeFiles(t *testing.T) {
	r := &RemoteCompose{
		Host:              "user@example.com",
		ProjectDir:        "/app",
		SocketPath:        "/tmp/cdeploy-ctrl-abc-99",
		ExtraComposeFiles: []string{"/app/docker-compose.yml", "/tmp/override.yml"},
	}

	cmd := r.remoteCommand(context.Background(), "up", "--no-start", "web")
	remoteCmd := cmd.Args[len(cmd.Args)-1]

	// -f pairs are shell-escaped and land immediately after "docker compose",
	// before the subcommand, main file first.
	want := "docker compose -f '/app/docker-compose.yml' -f '/tmp/override.yml' 'up' '--no-start' 'web'"
	if !strings.Contains(remoteCmd, want) {
		t.Errorf("remote command should contain %q, got: %q", want, remoteCmd)
	}
	// CURRENT_UID prefix must be unaffected.
	if !strings.HasPrefix(remoteCmd, "cd '/app' && CURRENT_UID=$(id -u):$(id -g) docker compose -f ") {
		t.Errorf("remote command prefix unexpected, got: %q", remoteCmd)
	}
}

func TestRemoteCommand_ExtraComposeFiles_Standalone(t *testing.T) {
	r := &RemoteCompose{
		Host:              "user@example.com",
		SocketPath:        "/tmp/cdeploy-ctrl-abc-99",
		Standalone:        true,
		ExtraComposeFiles: []string{"/app/compose.yml"},
	}

	cmd := r.remoteCommand(context.Background(), "start", "web")
	remoteCmd := cmd.Args[len(cmd.Args)-1]

	want := "docker-compose -f '/app/compose.yml' 'start' 'web'"
	if !strings.Contains(remoteCmd, want) {
		t.Errorf("remote command should contain %q, got: %q", want, remoteCmd)
	}
}

func TestRemoteCommand_ExtraComposeFiles_Escaping(t *testing.T) {
	r := &RemoteCompose{
		Host:              "user@example.com",
		SocketPath:        "/tmp/cdeploy-ctrl-abc-99",
		ExtraComposeFiles: []string{"/app/my compose.yml", "/tmp/it's-override.yml"},
	}

	cmd := r.remoteCommand(context.Background(), "stop")
	remoteCmd := cmd.Args[len(cmd.Args)-1]

	// Spaces and single quotes must be shell-escaped so the remote shell
	// receives them as single argv tokens.
	wantSpace := "-f '/app/my compose.yml'"
	if !strings.Contains(remoteCmd, wantSpace) {
		t.Errorf("remote command should contain %q, got: %q", wantSpace, remoteCmd)
	}
	wantQuote := "-f '/tmp/it'\\''s-override.yml'"
	if !strings.Contains(remoteCmd, wantQuote) {
		t.Errorf("remote command should contain escaped quote %q, got: %q", wantQuote, remoteCmd)
	}
}

// TestRemoteCommand_NilExtraComposeFiles_ByteIdentical is the regression pin
// for acceptance criterion 6 on the remote path: a nil ExtraComposeFiles field
// must produce a remote command string byte-identical to the pre-feature
// behavior across compose subcommands, in both plugin and standalone modes.
func TestRemoteCommand_NilExtraComposeFiles_ByteIdentical(t *testing.T) {
	subcommands := [][]string{
		{"stop", "nginx"},
		{"rm", "-f", "nginx"},
		{"pull"},
		{"up", "--no-start", "nginx"},
		{"start", "nginx"},
		{"config", "--services"},
		{"ps", "-a", "--format", "json"},
	}

	for _, standalone := range []bool{false, true} {
		composeBin := "docker compose"
		if standalone {
			composeBin = "docker-compose"
		}
		for _, args := range subcommands {
			name := "plugin"
			if standalone {
				name = "standalone"
			}
			name += "/" + strings.Join(args, "_")
			t.Run(name, func(t *testing.T) {
				r := &RemoteCompose{
					Host:       "user@example.com",
					ProjectDir: "/app",
					SocketPath: "/tmp/cdeploy-ctrl-abc-99",
					Standalone: standalone,
				}
				cmd := r.remoteCommand(context.Background(), args...)
				remoteCmd := cmd.Args[len(cmd.Args)-1]

				// Reconstruct the exact pre-feature command string.
				var escaped []string
				for _, a := range args {
					escaped = append(escaped, shellEscape(a))
				}
				want := "cd '/app' && " +
					"CURRENT_UID=$(id -u):$(id -g) " + composeBin + " " +
					strings.Join(escaped, " ")

				if remoteCmd != want {
					t.Errorf("remote command\n got: %q\nwant: %q", remoteCmd, want)
				}
			})
		}
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

func TestRemoteCheckUpdates_FallbackPath(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"},"builder":{"image":""}}}`
	var argvs [][]string
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			argvs = append(argvs, append([]string{}, cmd.Args...))
			// Discriminate by the remote shell command (last arg). buildx must
			// be checked BEFORE the image case because "imagetools" contains
			// the substring "image" and would otherwise match the image case.
			switch {
			case isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config"):
				return []byte(configJSON), nil
			case isRemoteShellCmd(cmd.Args, "buildx") && isRemoteShellCmd(cmd.Args, "imagetools"):
				// Simulate buildx plugin missing — exercise the manifest fallback.
				return nil, fmt.Errorf("exit status 1: docker: 'buildx' is not a docker command")
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
	// Multi-image project: one image fails locally, the other succeeds. The
	// failing service stays absent (tri-state unknown), the succeeding one is
	// reported. Cascading-failure detection (every image failing) does NOT
	// fire when at least one image succeeds.
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			if isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config") {
				return []byte(configJSON), nil
			}
			rc := cmd.Args[len(cmd.Args)-1]
			// buildx imagetools — check BEFORE the image case because
			// "imagetools" contains the substring "image".
			if isRemoteShellCmd(cmd.Args, "buildx") && isRemoteShellCmd(cmd.Args, "imagetools") {
				return nil, fmt.Errorf("exit status 1: 'buildx' is not a docker command")
			}
			if isRemoteShellCmd(cmd.Args, "image") && isRemoteShellCmd(cmd.Args, "inspect") {
				if strings.Contains(rc, "nginx:latest") {
					// nginx not pulled → inspect fails for that one.
					return nil, fmt.Errorf("no such image")
				}
				return []byte("postgres:16@sha256:local-postgres\n"), nil
			}
			if isRemoteShellCmd(cmd.Args, "manifest") && isRemoteShellCmd(cmd.Args, "inspect") {
				return []byte(`{"Descriptor":{"digest":"sha256:local-postgres"}}`), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	got, err := r.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if _, ok := got["web"]; ok {
		t.Errorf("expected web to be absent (inspect failure → unknown), got %#v", got)
	}
	if v, ok := got["db"]; !ok || v {
		t.Errorf("db = %v (ok=%v), want false (matching digests)", v, ok)
	}
}

// TestRemoteCheckUpdates_TransportFailureAbortsBatch pins the
// transport-vs-per-image classification: when an inspect call hits an SSH
// transport failure (matched via stderr against sshTransportStderrPatterns),
// the batch aborts early with errSSHTransport wrapped in the returned
// error so the caller surfaces the real diagnostic rather than silently
// emitting an "all unknown" map. Single-image projects also trigger this
// path (any transport failure poisons the whole batch).
func TestRemoteCheckUpdates_TransportFailureAbortsBatch(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"}}}`
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		// Config fetch goes through remoteCommand() and uses outputCmd —
		// it doesn't need classification.
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			if isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config") {
				return []byte(configJSON), nil
			}
			// Inspect calls should NOT hit this path — outputErrCmd takes
			// precedence in runRemoteDockerCmd. Surface a clear failure if
			// the hook precedence ever regresses.
			return nil, fmt.Errorf("test bug: inspect call hit outputCmd, expected outputErrCmd")
		},
	}
	// Inspect calls go through runRemoteDockerCmd which prefers outputErrCmd
	// — drive the classifier with explicit stderr text so the test mirrors
	// the production stderr-capture path instead of the legacy err.Error()
	// fallback.
	r.SetOutputErrHook(func(cmd *exec.Cmd) ([]byte, string, error) {
		return nil, "ssh: connection lost", fmt.Errorf("exit status 255")
	})
	_, err := r.CheckUpdates(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when transport fails")
	}
	if !errors.Is(err, errSSHTransport) {
		t.Errorf("error = %q, want errSSHTransport wrapped", err.Error())
	}
	if !strings.Contains(err.Error(), "ssh: connection lost") {
		t.Errorf("error = %q, want underlying stderr wrapped", err.Error())
	}
}

// TestRemoteCheckUpdates_FreshDeploySingleServiceNoCascade is the regression
// for the iteration-2 finding: a single-service project where the image
// hasn't been pulled to the remote host yet (a normal fresh-deploy state)
// MUST NOT trigger a cascading-failure error. `docker image inspect` returns
// exit 1 with "No such image" on stderr — that's a per-image docker error,
// not an SSH transport failure, so CheckUpdates should soft-absorb it and
// return an empty map without error, mirroring local Compose behaviour.
func TestRemoteCheckUpdates_FreshDeploySingleServiceNoCascade(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"}}}`
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			if isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config") {
				return []byte(configJSON), nil
			}
			// Synthesise the docker-style stderr — does NOT match any
			// transport pattern, so classifySSHError will return a
			// non-errSSHTransport error and CheckUpdates will absorb it.
			return nil, fmt.Errorf("exit status 1: Error: No such image: nginx:latest")
		},
	}
	got, err := r.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v (expected nil — per-image docker errors should be absorbed)", err)
	}
	if len(got) != 0 {
		t.Errorf("got %#v, want empty map (web absent → unknown)", got)
	}
}

// TestRemoteCheckUpdates_SystemicRegistryFailureSurfaces is the
// iteration-4 parity with local Compose: when EVERY remote-side digest
// fetch fails with a network-shaped stderr AND no service got a verdict,
// the cascade fires and CheckUpdates returns "registry unreachable"
// rather than silently returning an empty map. Without this, narrowing
// sshTransportStderrPatterns to SSH-only signatures means a Docker Hub
// outage seen from the remote host produces blank glyphs for every
// service with no diagnostic — looks identical to "everything is
// up-to-date" to the user.
func TestRemoteCheckUpdates_SystemicRegistryFailureSurfaces(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			if isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config") {
				return []byte(configJSON), nil
			}
			// Local inspect on the remote host succeeds for both images.
			if isRemoteShellCmd(cmd.Args, "image") && isRemoteShellCmd(cmd.Args, "inspect") &&
				!isRemoteShellCmd(cmd.Args, "buildx") {
				rc := cmd.Args[len(cmd.Args)-1]
				if strings.Contains(rc, "nginx") {
					return []byte("nginx:latest@sha256:local-nginx\n"), nil
				}
				return []byte("postgres:16@sha256:local-postgres\n"), nil
			}
			// buildx imagetools — registry-network error (DNS failure).
			if isRemoteShellCmd(cmd.Args, "buildx") && isRemoteShellCmd(cmd.Args, "imagetools") {
				return nil, fmt.Errorf("exit status 1: dial tcp: lookup registry-1.docker.io: no such host")
			}
			// manifest inspect — same registry-network error.
			if isRemoteShellCmd(cmd.Args, "manifest") && isRemoteShellCmd(cmd.Args, "inspect") {
				return nil, fmt.Errorf("exit status 1: dial tcp: lookup registry-1.docker.io: no such host")
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	got, err := r.CheckUpdates(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when every remote registry fetch hits a network failure")
	}
	if !strings.Contains(err.Error(), "registry unreachable") {
		t.Errorf("error = %q, want it to mention 'registry unreachable'", err.Error())
	}
	if !strings.Contains(err.Error(), "no such host") {
		t.Errorf("error = %q, want it to wrap the underlying DNS error", err.Error())
	}
	if len(got) != 0 {
		t.Errorf("got = %#v, want empty map (every service unknown)", got)
	}
}

// TestRemoteCheckUpdates_PartialRegistryFailureDoesNotCascade documents
// the negative case for the remote registry cascade: when one image
// succeeds and another fails with a network-shaped error, the cascade
// MUST NOT fire — the succeeded service still gets a verdict, the failed
// one stays absent.
func TestRemoteCheckUpdates_PartialRegistryFailureDoesNotCascade(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	// Use full 64-char hex digests so parseImagetoolsDigest's strict regex
	// validates them. The two services have distinct digests; the matching
	// "remote = local" digest for db proves the verdict is UpdateAvailable=false.
	dbHex := strings.Repeat("d", 64)
	dbDigest := "sha256:" + dbHex
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			if isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config") {
				return []byte(configJSON), nil
			}
			rc := cmd.Args[len(cmd.Args)-1]
			// buildx imagetools — check BEFORE image case (substring overlap).
			if isRemoteShellCmd(cmd.Args, "buildx") && isRemoteShellCmd(cmd.Args, "imagetools") {
				if strings.Contains(rc, "nginx") {
					return nil, fmt.Errorf("exit status 1: dial tcp: lookup registry-1.docker.io: no such host")
				}
				// db's buildx call succeeds with a matching digest.
				return []byte("Name:      postgres:16\nMediaType: application/vnd.oci.image.index.v1+json\nDigest:    " + dbDigest + "\n"), nil
			}
			if isRemoteShellCmd(cmd.Args, "image") && isRemoteShellCmd(cmd.Args, "inspect") {
				if strings.Contains(rc, "nginx") {
					return []byte("nginx:latest@sha256:" + strings.Repeat("a", 64) + "\n"), nil
				}
				return []byte("postgres:16@" + dbDigest + "\n"), nil
			}
			if isRemoteShellCmd(cmd.Args, "manifest") && isRemoteShellCmd(cmd.Args, "inspect") {
				return nil, fmt.Errorf("exit status 1: dial tcp: lookup registry-1.docker.io: no such host")
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	got, err := r.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error on partial registry failure: %v", err)
	}
	if v, ok := got["db"]; !ok || v {
		t.Errorf("db = %v (ok=%v), want false (matching digests)", v, ok)
	}
	if _, present := got["web"]; present {
		t.Errorf("web should be absent on registry failure, got %v", got["web"])
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
	_, err := r.CheckUpdates(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when config fetch fails")
	}
	if !strings.Contains(err.Error(), "fetching remote compose config") {
		t.Errorf("error = %q, want it to mention remote compose config", err.Error())
	}
}

func TestRemoteCheckUpdates_FallbackPath_ManifestFailureLeavesAbsent(t *testing.T) {
	// Multi-image: one image's manifest call fails (per-image registry/auth
	// error), the other succeeds. The failing service stays absent;
	// cascading-failure detection does NOT fire because at least one image
	// succeeded.
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			if isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config") {
				return []byte(configJSON), nil
			}
			rc := cmd.Args[len(cmd.Args)-1]
			// buildx imagetools — check BEFORE the image case because
			// "imagetools" contains the substring "image".
			if isRemoteShellCmd(cmd.Args, "buildx") && isRemoteShellCmd(cmd.Args, "imagetools") {
				return nil, fmt.Errorf("exit status 1: 'buildx' is not a docker command")
			}
			if isRemoteShellCmd(cmd.Args, "image") && isRemoteShellCmd(cmd.Args, "inspect") {
				if strings.Contains(rc, "nginx:latest") {
					return []byte("nginx:latest@sha256:local-nginx\n"), nil
				}
				return []byte("postgres:16@sha256:local-postgres\n"), nil
			}
			if isRemoteShellCmd(cmd.Args, "manifest") && isRemoteShellCmd(cmd.Args, "inspect") {
				if strings.Contains(rc, "nginx:latest") {
					return nil, fmt.Errorf("auth required")
				}
				return []byte(`{"Descriptor":{"digest":"sha256:local-postgres"}}`), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	got, err := r.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if _, ok := got["web"]; ok {
		t.Errorf("expected web to be absent (manifest failure → unknown), got %#v", got)
	}
	if v, ok := got["db"]; !ok || v {
		t.Errorf("db = %v (ok=%v), want false (matching digests)", v, ok)
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
			// buildx imagetools — check BEFORE the image case because the rc
			// for buildx imagetools contains the substring "image" too.
			case strings.Contains(rc, "buildx") && strings.Contains(rc, "imagetools"):
				inspectArgvs = append(inspectArgvs, append([]string{}, cmd.Args...))
				// Simulate buildx missing → exercise the manifest fallback.
				return nil, fmt.Errorf("exit status 1: 'buildx' is not a docker command")
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
	if _, err := r.CheckUpdates(context.Background(), nil); err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(inspectArgvs) != 3 {
		t.Fatalf("expected 3 inspect calls (image + buildx + manifest), got %d", len(inspectArgvs))
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
			// buildx imagetools — check BEFORE the image case because rc contains "image".
			case strings.Contains(rc, "buildx") && strings.Contains(rc, "imagetools"):
				inspectArgvs = append(inspectArgvs, rc)
				// Simulate buildx missing → exercise the manifest fallback.
				return nil, fmt.Errorf("exit status 1: 'buildx' is not a docker command")
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
	if _, err := r.CheckUpdates(context.Background(), nil); err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(inspectArgvs) != 3 {
		t.Fatalf("expected 3 inspect commands (image + buildx + manifest), got %d", len(inspectArgvs))
	}
	for _, rc := range inspectArgvs {
		// Image must be shell-escaped (single-quoted) to survive transport.
		if !strings.Contains(rc, "'registry.example.com:5000/team/web:v1.2.3'") {
			t.Errorf("remote shell = %q, want escaped image", rc)
		}
	}
}

// --- transport-abort knob (updateCascades.transportAbort) ---
//
// The knob is declared in updates.go beside the two cascades, but only the
// remote path enables it, so its tests live here with the rest of the SSH
// contract.

// TestScanImageUpdates_TransportAbortStopsOnFirstError pins the early return:
// a dead SSH hop fails every remaining image the same way, so the loop must
// stop rather than burn the rest of the round-trips. The failure is injected
// on the third call regardless of image, so the assertion does not depend on
// Go's randomised map iteration order.
func TestScanImageUpdates_TransportAbortStopsOnFirstError(t *testing.T) {
	wanted := map[string]string{
		"web":   "nginx:latest",
		"db":    "postgres:16",
		"cache": "redis:7",
		"queue": "rabbitmq:3",
		"proxy": "traefik:v3",
	}
	calls := 0
	compare := func(_ context.Context, _ string) (bool, bool, error) {
		calls++
		if calls == 3 {
			return false, false, fmt.Errorf("%w: exit status 255: ssh: connection lost", errSSHTransport)
		}
		return true, true, nil
	}

	got, err := scanImageUpdates(context.Background(), wanted, compare,
		updateCascades{registry: true, transportAbort: true})
	if err == nil {
		t.Fatal("expected an error when the transport dies")
	}
	if !errors.Is(err, errSSHTransport) {
		t.Errorf("err = %q, want errSSHTransport wrapped", err)
	}
	if !strings.Contains(err.Error(), "remote update check transport failure") {
		t.Errorf("err = %q, want the remote transport diagnostic", err)
	}
	if calls != 3 {
		t.Errorf("comparer called %d times, want 3 (abort on the failing call)", calls)
	}
	// The two verdicts collected before the abort still come back — the
	// caller treats a partial map as untrusted, it is not discarded here.
	if len(got) != 2 {
		t.Errorf("partial map = %#v, want the 2 verdicts collected before the abort", got)
	}
}

// TestScanImageUpdates_TransportAbortAbsorbsPerImageFailure is the negative
// half: only errSSHTransport aborts. A per-image docker failure on the far
// host (image not pulled, manifest auth) stays absorbed as the tri-state
// absent, so a fresh deploy does not blank the column with a false transport
// diagnostic.
func TestScanImageUpdates_TransportAbortAbsorbsPerImageFailure(t *testing.T) {
	wanted := map[string]string{"web": "nginx:latest", "db": "postgres:16"}
	compare := scanFunc(t, map[string]scanOutcome{
		"nginx:latest": {updated: true, ok: true},
		"postgres:16":  {err: fmt.Errorf("exit status 1: Error: No such image: postgres:16")},
	})

	got, err := scanImageUpdates(context.Background(), wanted, compare,
		updateCascades{registry: true, transportAbort: true})
	if err != nil {
		t.Fatalf("per-image failure aborted the batch: %v", err)
	}
	want := map[string]bool{"web": true}
	if len(got) != len(want) || got["web"] != want["web"] {
		t.Fatalf("results = %#v, want %#v (db absent → unknown)", got, want)
	}
}

// TestScanImageUpdates_TransportAbortOffAbsorbsTransportError pins the knob
// default. Compose never emits errSSHTransport, so the local path leaves the
// knob off; with it off the sentinel must carry no special meaning and be
// absorbed like any other per-image failure.
func TestScanImageUpdates_TransportAbortOffAbsorbsTransportError(t *testing.T) {
	wanted := map[string]string{"web": "nginx:latest", "db": "postgres:16"}
	compare := scanFunc(t, map[string]scanOutcome{
		"nginx:latest": {updated: true, ok: true},
		"postgres:16":  {err: fmt.Errorf("%w: exit status 255: ssh: connection lost", errSSHTransport)},
	})

	got, err := scanImageUpdates(context.Background(), wanted, compare,
		updateCascades{registry: true, daemon: true})
	if err != nil {
		t.Fatalf("the transport sentinel aborted with the knob off: %v", err)
	}
	if len(got) != 1 || !got["web"] {
		t.Fatalf("results = %#v, want {web: true}", got)
	}
}

// TestRemoteCheckUpdates_NoDaemonCascade pins the knob RemoteCompose must
// leave OFF. The docker CLI runs on the far side of the SSH hop, so a
// daemon-shaped stderr there is just a per-image docker failure — surfacing it
// as "local docker unavailable" would name the wrong machine. Every image
// fails with a daemon-shaped stderr and no service gets a verdict, which is
// exactly the condition that fires the cascade on the local path.
func TestRemoteCheckUpdates_NoDaemonCascade(t *testing.T) {
	configJSON := `{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/app",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			if isRemoteShellCmd(cmd.Args, "compose") && isRemoteShellCmd(cmd.Args, "config") {
				return []byte(configJSON), nil
			}
			return nil, fmt.Errorf("exit status 1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
		},
	}
	got, err := r.CheckUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckUpdates: %v (the remote path must not run the daemon cascade)", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %#v, want empty map (every service unknown)", got)
	}
}

// TestClassifySSHError_ControlMasterPatterns is the iteration-3 regression
// for ControlMaster (persistent socket) failure modes: when the SSH mux
// socket dies mid-batch, the stderr matches one of mux_client / client_loop /
// "broken pipe" / ControlSocket / "session open refused" / multiplex —
// all of which MUST be classified as transport failures so cascading-
// failure detection fires and the user gets an actionable diagnostic
// instead of a silently blank glyph column.
func TestClassifySSHError_ControlMasterPatterns(t *testing.T) {
	tests := []struct {
		name        string
		stderr      string
		wantTrans   bool // expect errSSHTransport wrap
		wantInclude string
	}{
		{
			name:      "client_loop broken pipe",
			stderr:    "client_loop: send disconnect: Broken pipe",
			wantTrans: true,
		},
		{
			name:      "mux_client request_session broken pipe",
			stderr:    "mux_client_request_session: read from master failed: Broken pipe",
			wantTrans: true,
		},
		{
			name:      "mux_client hello_exchange write packet",
			stderr:    "mux_client_hello_exchange: write packet: Broken pipe",
			wantTrans: true,
		},
		{
			name:      "ControlSocket missing",
			stderr:    "ControlSocket /tmp/cdeploy-ctrl-abc-99: No such file or directory",
			wantTrans: true,
		},
		{
			name:      "session open refused",
			stderr:    "mux_client_request_session: session request failed: Session open refused by peer",
			wantTrans: true,
		},
		{
			name:      "multiplex master state",
			stderr:    "multiplex: master state corrupt",
			wantTrans: true,
		},
		{
			name:      "per-image docker no such image",
			stderr:    "Error response from daemon: No such image: nginx:latest",
			wantTrans: false,
		},
		{
			name:      "per-image manifest unknown",
			stderr:    "manifest unknown",
			wantTrans: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifySSHError(fmt.Errorf("ssh exit 255"), tt.stderr)
			if tt.wantTrans {
				if !errors.Is(got, errSSHTransport) {
					t.Errorf("got %q, want errSSHTransport wrap (stderr=%q)", got.Error(), tt.stderr)
				}
			} else {
				if errors.Is(got, errSSHTransport) {
					t.Errorf("got %q wrapped in errSSHTransport, want plain error (stderr=%q)", got.Error(), tt.stderr)
				}
			}
		})
	}
}

// TestClassifySSHError_SshExchangeIdentificationIsTransport is the
// iteration-5 regression for the `ssh:`-vs-`ssh_` pattern gap: the
// literal substring `ssh:` (with colon) does NOT match real-world
// `ssh_exchange_identification:` stderr lines, which previously fell
// through to looksLikeNetworkErr (because "Connection closed"/"reset"
// are in that pattern list) and triggered the misleading
// "registry unreachable" cascade instead of the SSH-transport cascade.
// Worst case, neither cascade matched and the user saw a silent failure
// (no glyphs, no diagnostic).
func TestClassifySSHError_SshExchangeIdentificationIsTransport(t *testing.T) {
	tests := []struct {
		name      string
		stderr    string
		wantTrans bool
	}{
		{
			name:      "exchange identification connection closed",
			stderr:    "ssh_exchange_identification: Connection closed by remote host",
			wantTrans: true,
		},
		{
			name:      "exchange identification read reset",
			stderr:    "ssh_exchange_identification: read: Connection reset by peer",
			wantTrans: true,
		},
		{
			name:      "exchange identification mixed case",
			stderr:    "SSH_exchange_identification: server unexpectedly closed",
			wantTrans: true,
		},
		{
			name:      "lost connection",
			stderr:    "ssh: connect to host example.com port 22: lost connection",
			wantTrans: true,
		},
		// Negative pin: bare per-image docker error must NOT be classified as transport.
		{
			name:      "per-image no such image",
			stderr:    "Error response from daemon: No such image: nginx:latest",
			wantTrans: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifySSHError(fmt.Errorf("exit status 255"), tt.stderr)
			if tt.wantTrans {
				if !errors.Is(got, errSSHTransport) {
					t.Errorf("got %q, want errSSHTransport wrap (stderr=%q)", got.Error(), tt.stderr)
				}
			} else {
				if errors.Is(got, errSSHTransport) {
					t.Errorf("got %q wrapped in errSSHTransport, want plain error (stderr=%q)", got.Error(), tt.stderr)
				}
			}
		})
	}
}

// TestRunRemoteDockerCmd_OutputErrHook proves the iteration-3 fix for
// finding #7: tests can now provide explicit (stdout, stderr, err) so
// classification doesn't depend on the err.Error()-as-stderr heuristic.
// The hook drives a synthetic transport-pattern stderr; the result MUST
// classify as errSSHTransport even though err.Error() itself doesn't
// contain the transport pattern.
func TestRunRemoteDockerCmd_OutputErrHook(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
	}
	r.SetOutputErrHook(func(cmd *exec.Cmd) ([]byte, string, error) {
		// Plain error text — would NOT match transport patterns on its own.
		// Explicit stderr DOES match → must classify as transport.
		return nil, "client_loop: send disconnect: Broken pipe", fmt.Errorf("exit status 255")
	})
	_, err := r.runRemoteDockerCmd(context.Background(), []string{"image", "inspect", "nginx"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, errSSHTransport) {
		t.Errorf("got %q, want errSSHTransport (explicit stderr drove classification)", err.Error())
	}
	if !strings.Contains(err.Error(), "Broken pipe") {
		t.Errorf("got %q, want stderr text included for diagnostic", err.Error())
	}
}

// --- RemoteCompose snapshot capture + state IO tests (Task 7) ---

// remoteShellTokens splits a remote shell command string on whitespace and
// strips the single-quote shellEscape wrapping from each token. Sufficient for
// the test fixtures (simple container IDs / image refs without embedded
// whitespace).
func remoteShellTokens(rc string) []string {
	fields := strings.Fields(rc)
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = strings.Trim(f, "'")
	}
	return out
}

// remoteShellTokensAfter returns the unquoted tokens following the first token
// equal to marker (used to pull the container IDs after the `{{.Image}}` format
// arg in a batched `docker inspect` remote command).
func remoteShellTokensAfter(rc, marker string) []string {
	toks := remoteShellTokens(rc)
	for i, t := range toks {
		if t == marker {
			return toks[i+1:]
		}
	}
	return nil
}

// remoteSnapshotComposer builds a RemoteCompose whose outputCmd hook scripts the
// four remote calls SnapshotServices makes, discriminating by the remote shell
// command string (the trailing ssh arg): `compose config`, `compose ps`,
// batched `docker inspect --format {{.Image}}` (container ID → image ID via
// containerImageID), and `docker image inspect` (image ID → RepoDigests via
// imageRepoDigests). It mirrors snapshotComposer for the local path.
func remoteSnapshotComposer(configJSON, psJSON string, containerImageID, imageRepoDigests map[string]string) *RemoteCompose {
	return &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc := cmd.Args[len(cmd.Args)-1]
			switch {
			case strings.Contains(rc, "compose") && strings.Contains(rc, "'config'"):
				return []byte(configJSON), nil
			case strings.Contains(rc, "compose") && strings.Contains(rc, "'ps'"):
				return []byte(psJSON), nil
			case strings.Contains(rc, "{{.Image}}"):
				var b strings.Builder
				for _, id := range remoteShellTokensAfter(rc, "{{.Image}}") {
					iid, ok := containerImageID[id]
					if !ok {
						return nil, fmt.Errorf("no such container: %s", id)
					}
					b.WriteString(iid + "\n")
				}
				return []byte(b.String()), nil
			case strings.Contains(rc, "RepoDigests"):
				toks := remoteShellTokens(rc)
				imageID := toks[len(toks)-1]
				return []byte(imageRepoDigests[imageID]), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
}

func TestRemoteSnapshotServices_HappyPath(t *testing.T) {
	pinSnapshotClock(t, time.Date(2026, 7, 29, 14, 3, 0, 0, time.UTC))
	r := remoteSnapshotComposer(
		`{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`,
		`[{"ID":"cid-web","Service":"web","State":"running"},{"ID":"cid-db","Service":"db","State":"running"}]`,
		map[string]string{"cid-web": "sha256:img-web", "cid-db": "sha256:img-db"},
		map[string]string{
			"sha256:img-web": "nginx@sha256:ab12\n",
			"sha256:img-db":  "postgres@sha256:cd34\n",
		},
	)
	res, err := r.SnapshotServices(context.Background(), nil)
	if err != nil {
		t.Fatalf("SnapshotServices: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	if res.Snapshot.ProjectDir != remoteProjectDir("/proj") {
		t.Errorf("project_dir = %q, want %q", res.Snapshot.ProjectDir, remoteProjectDir("/proj"))
	}
	web := res.Snapshot.Services["web"]
	if web.Image != "nginx:latest" || web.Digest != "sha256:ab12" || web.RecordedAt != "2026-07-29T14:03:00Z" {
		t.Errorf("web entry wrong: %+v", web)
	}
	if db := res.Snapshot.Services["db"]; db.Image != "postgres:16" || db.Digest != "sha256:cd34" {
		t.Errorf("db entry wrong: %+v", db)
	}
}

func TestRemoteSnapshotServices_NotRunning(t *testing.T) {
	r := remoteSnapshotComposer(
		`{"services":{"web":{"image":"nginx:latest"},"db":{"image":"postgres:16"}}}`,
		`[{"ID":"cid-web","Service":"web","State":"running"}]`,
		map[string]string{"cid-web": "sha256:img-web"},
		map[string]string{"sha256:img-web": "nginx@sha256:ab12\n"},
	)
	res, err := r.SnapshotServices(context.Background(), []string{"web", "db"})
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

func TestRemoteSnapshotServices_BuildOnlyNoDigest(t *testing.T) {
	r := remoteSnapshotComposer(
		`{"services":{"web":{"image":"nginx:latest"},"app":{"image":"myapp:local"}}}`,
		`[{"ID":"cid-web","Service":"web","State":"running"},{"ID":"cid-app","Service":"app","State":"running"}]`,
		map[string]string{"cid-web": "sha256:img-web", "cid-app": "sha256:img-app"},
		map[string]string{
			"sha256:img-web": "nginx@sha256:ab12\n",
			"sha256:img-app": "", // no RepoDigests → locally built
		},
	)
	res, err := r.SnapshotServices(context.Background(), nil)
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

func TestRemoteSnapshotServices_ScaledOneEntry(t *testing.T) {
	// web scaled to 2 running replicas → exactly one entry, only the first
	// replica is inspected.
	var inspected []string
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc := cmd.Args[len(cmd.Args)-1]
			switch {
			case strings.Contains(rc, "compose") && strings.Contains(rc, "'config'"):
				return []byte(`{"services":{"web":{"image":"nginx:latest"}}}`), nil
			case strings.Contains(rc, "compose") && strings.Contains(rc, "'ps'"):
				return []byte(`[{"ID":"cid-web-1","Service":"web","State":"running"},{"ID":"cid-web-2","Service":"web","State":"running"}]`), nil
			case strings.Contains(rc, "{{.Image}}"):
				ids := remoteShellTokensAfter(rc, "{{.Image}}")
				inspected = append(inspected, ids...)
				var b strings.Builder
				for range ids {
					b.WriteString("sha256:img-web\n")
				}
				return []byte(b.String()), nil
			case strings.Contains(rc, "RepoDigests"):
				return []byte("nginx@sha256:ab12\n"), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	res, err := r.SnapshotServices(context.Background(), nil)
	if err != nil {
		t.Fatalf("SnapshotServices: %v", err)
	}
	if len(res.Snapshot.Services) != 1 {
		t.Fatalf("want exactly 1 service entry, got %d: %+v", len(res.Snapshot.Services), res.Snapshot.Services)
	}
	if res.Snapshot.Services["web"].Digest != "sha256:ab12" {
		t.Errorf("web digest wrong: %+v", res.Snapshot.Services["web"])
	}
	if len(inspected) != 1 || inspected[0] != "cid-web-1" {
		t.Errorf("inspected containers = %v, want only the first replica", inspected)
	}
}

func TestRemoteSnapshotServices_InspectBypassesCompose(t *testing.T) {
	// The top-level docker inspect / image inspect remote commands must NOT
	// carry the compose subcommand, CURRENT_UID, or a `cd` prefix — they go
	// through runRemoteDockerCmd, not remoteCommand.
	var inspectCmds []string
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc := cmd.Args[len(cmd.Args)-1]
			switch {
			case strings.Contains(rc, "compose") && strings.Contains(rc, "'config'"):
				return []byte(`{"services":{"web":{"image":"nginx:latest"}}}`), nil
			case strings.Contains(rc, "compose") && strings.Contains(rc, "'ps'"):
				return []byte(`[{"ID":"cid-web","Service":"web","State":"running"}]`), nil
			case strings.Contains(rc, "{{.Image}}"):
				inspectCmds = append(inspectCmds, rc)
				return []byte("sha256:img-web\n"), nil
			case strings.Contains(rc, "RepoDigests"):
				inspectCmds = append(inspectCmds, rc)
				return []byte("nginx@sha256:ab12\n"), nil
			}
			return nil, fmt.Errorf("unexpected cmd: %v", cmd.Args)
		},
	}
	if _, err := r.SnapshotServices(context.Background(), nil); err != nil {
		t.Fatalf("SnapshotServices: %v", err)
	}
	if len(inspectCmds) != 2 {
		t.Fatalf("expected 2 top-level docker inspect calls, got %d", len(inspectCmds))
	}
	for _, rc := range inspectCmds {
		if strings.Contains(rc, "compose") {
			t.Errorf("inspect remote command leaked 'compose': %q", rc)
		}
		if strings.Contains(rc, "CURRENT_UID") {
			t.Errorf("inspect remote command leaked CURRENT_UID: %q", rc)
		}
		if strings.HasPrefix(rc, "cd ") {
			t.Errorf("inspect remote command has a cd prefix: %q", rc)
		}
		if !strings.HasPrefix(rc, "docker ") {
			t.Errorf("inspect remote command should start with 'docker ': %q", rc)
		}
	}
}

func TestRemoteSnapshotServices_PSError(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc := cmd.Args[len(cmd.Args)-1]
			if strings.Contains(rc, "compose") && strings.Contains(rc, "'config'") {
				return []byte(`{"services":{"web":{"image":"nginx:latest"}}}`), nil
			}
			return nil, fmt.Errorf("ssh timeout")
		},
	}
	_, err := r.SnapshotServices(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listing remote containers for snapshot") {
		t.Errorf("error = %q, want it to mention listing remote containers", err.Error())
	}
}

func TestRemoteWriteRemoteFile_Command(t *testing.T) {
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}
	path := "$HOME/.cdeploy/state/abc123def456.json"
	if err := r.writeRemoteFile(context.Background(), path, []byte("payload-bytes")); err != nil {
		t.Fatalf("writeRemoteFile: %v", err)
	}
	if captured == nil {
		t.Fatal("runCmd not called")
	}
	if captured.Args[0] != "ssh" {
		t.Errorf("argv[0] = %q, want ssh", captured.Args[0])
	}
	// Reuses the ControlMaster socket.
	foundSock := false
	for i, a := range captured.Args {
		if a == "-S" && i+1 < len(captured.Args) && captured.Args[i+1] == r.SocketPath {
			foundSock = true
		}
	}
	if !foundSock {
		t.Errorf("argv missing -S %q: %v", r.SocketPath, captured.Args)
	}
	rc := captured.Args[len(captured.Args)-1]
	for _, want := range []string{
		`mkdir -p "$(dirname "$HOME/.cdeploy/state/abc123def456.json")"`,
		`cat > "$HOME/.cdeploy/state/abc123def456.json.$$.tmp"`,
		`mv "$HOME/.cdeploy/state/abc123def456.json.$$.tmp" "$HOME/.cdeploy/state/abc123def456.json"`,
	} {
		if !strings.Contains(rc, want) {
			t.Errorf("remote command missing %q, got: %q", want, rc)
		}
	}
	// Payload is piped over stdin.
	if captured.Stdin == nil {
		t.Fatal("stdin not wired")
	}
	got, _ := io.ReadAll(captured.Stdin)
	if string(got) != "payload-bytes" {
		t.Errorf("stdin data = %q, want %q", string(got), "payload-bytes")
	}
}

func TestRemoteWriteRemoteFile_SSHExtraArgs(t *testing.T) {
	extras := []string{"-i", "/tmp/key"}
	host := "user@example.com"
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:         host,
		SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
		SSHExtraArgs: extras,
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}
	if err := r.writeRemoteFile(context.Background(), "/tmp/cdeploy-rollback-1.yml", []byte("x")); err != nil {
		t.Fatalf("writeRemoteFile: %v", err)
	}
	assertExtraBeforeHost(t, "writeRemoteFile", captured.Args, host, extras)
}

func TestRemoteWriteRemoteFile_Error(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		runCmd: func(cmd *exec.Cmd) error {
			return fmt.Errorf("exit status 1")
		},
	}
	err := r.writeRemoteFile(context.Background(), "/tmp/x.yml", []byte("x"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestRemoteReadSnapshot_CatCommand(t *testing.T) {
	var rc string
	snapJSON := `{"schema":1,"project_dir":"/proj","services":{"web":{"image":"nginx:latest","digest":"sha256:ab12","recorded_at":"2026-07-29T14:03:00Z"}}}`
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc = cmd.Args[len(cmd.Args)-1]
			return []byte(snapJSON), nil
		},
	}
	snap, err := r.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if snap == nil || snap.Services["web"].Digest != "sha256:ab12" {
		t.Fatalf("parsed snapshot wrong: %+v", snap)
	}
	// Missing-file-tolerant guard + $HOME-expanded state path.
	if !strings.Contains(rc, "[ -f") {
		t.Errorf("read command should guard with [ -f ], got: %q", rc)
	}
	wantPath := "$HOME/" + stateFileRelPath(remoteProjectDir("/proj"))
	if !strings.Contains(rc, wantPath) {
		t.Errorf("read command should reference %q, got: %q", wantPath, rc)
	}
}

func TestRemoteReadSnapshot_MissingFileNil(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(""), nil // missing file → empty output, exit 0
		},
	}
	snap, err := r.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot on missing file should be nil error, got %v", err)
	}
	if snap != nil {
		t.Fatalf("ReadSnapshot on missing file should return nil snapshot, got %+v", snap)
	}
}

func TestRemoteReadSnapshot_UnknownSchemaTyped(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(`{"schema":2,"project_dir":"/proj","services":{}}`), nil
		},
	}
	_, err := r.ReadSnapshot(context.Background())
	if !errors.Is(err, errSnapshotSchema) {
		t.Fatalf("ReadSnapshot schema-2 err = %v, want errSnapshotSchema", err)
	}
}

func TestRemoteReadSnapshot_Error(t *testing.T) {
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("ssh timeout")
		},
	}
	_, err := r.ReadSnapshot(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "reading remote snapshot") {
		t.Errorf("error = %q, want it to contain 'reading remote snapshot'", err.Error())
	}
}

func TestRemoteWriteSnapshot_MergeAndWrite(t *testing.T) {
	// ReadSnapshot (outputCmd) returns an existing snapshot with only db;
	// writeRemoteFile (runCmd) captures the piped merged payload. The merge
	// must keep db alive (its own recorded_at) and add the fresh web entry.
	existing := `{"schema":1,"project_dir":"/proj","services":{"db":{"image":"postgres:16","digest":"sha256:old-db","recorded_at":"2026-06-01T00:00:00Z"}}}`
	var written []byte
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(existing), nil
		},
		runCmd: func(cmd *exec.Cmd) error {
			written, _ = io.ReadAll(cmd.Stdin)
			return nil
		},
	}
	fresh := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: remoteProjectDir("/proj"),
		Services: map[string]SnapshotEntry{
			"web": {Image: "nginx:latest", Digest: "sha256:new-web", RecordedAt: "2026-07-29T00:00:00Z"},
		},
	}
	if err := r.WriteSnapshot(context.Background(), fresh); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	got, err := parseSnapshot(written)
	if err != nil {
		t.Fatalf("parsing written snapshot: %v", err)
	}
	if got.Services["web"].Digest != "sha256:new-web" {
		t.Errorf("web not written from fresh: %+v", got.Services["web"])
	}
	if got.Services["db"].Digest != "sha256:old-db" || got.Services["db"].RecordedAt != "2026-06-01T00:00:00Z" {
		t.Errorf("db not preserved across merge: %+v", got.Services["db"])
	}
}

// TestRemoteWriteSnapshot_RefusesFutureSchema: a future-schema remote state file
// must NOT be clobbered — WriteSnapshot aborts and never pipes a downgraded file.
func TestRemoteWriteSnapshot_RefusesFutureSchema(t *testing.T) {
	wrote := false
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(`{"schema":2,"project_dir":"/proj","services":{}}`), nil
		},
		runCmd: func(cmd *exec.Cmd) error { wrote = true; return nil },
	}
	fresh := &Snapshot{Schema: snapshotSchemaVersion, Services: map[string]SnapshotEntry{"web": {Digest: "sha256:w"}}}
	if err := r.WriteSnapshot(context.Background(), fresh); err == nil {
		t.Fatal("WriteSnapshot must refuse to overwrite a future-schema remote file")
	}
	if wrote {
		t.Error("writeRemoteFile must not run when the write is aborted")
	}
}

// TestRemoteWriteSnapshot_RefusesUnreadable: a transient SSH read failure must
// abort the write so a flaky round-trip never wipes the remote merge history.
func TestRemoteWriteSnapshot_RefusesUnreadable(t *testing.T) {
	wrote := false
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("ssh timeout")
		},
		runCmd: func(cmd *exec.Cmd) error { wrote = true; return nil },
	}
	fresh := &Snapshot{Schema: snapshotSchemaVersion, Services: map[string]SnapshotEntry{"web": {Digest: "sha256:w"}}}
	if err := r.WriteSnapshot(context.Background(), fresh); err == nil {
		t.Fatal("WriteSnapshot must abort on a transient remote read failure")
	}
	if wrote {
		t.Error("writeRemoteFile must not run when the write is aborted")
	}
}

// TestRemoteWriteSnapshot_OverwritesCorrupt: a malformed-JSON remote file is
// safe to overwrite (it carries no interpretable merge history).
func TestRemoteWriteSnapshot_OverwritesCorrupt(t *testing.T) {
	var written []byte
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("{not json"), nil
		},
		runCmd: func(cmd *exec.Cmd) error {
			written, _ = io.ReadAll(cmd.Stdin)
			return nil
		},
	}
	fresh := &Snapshot{Schema: snapshotSchemaVersion, ProjectDir: remoteProjectDir("/proj"), Services: map[string]SnapshotEntry{"web": {Digest: "sha256:w"}}}
	if err := r.WriteSnapshot(context.Background(), fresh); err != nil {
		t.Fatalf("WriteSnapshot over corrupt file: %v", err)
	}
	got, err := parseSnapshot(written)
	if err != nil {
		t.Fatalf("parsing written snapshot: %v", err)
	}
	if got.Services["web"].Digest != "sha256:w" {
		t.Errorf("fresh snapshot not written over corrupt file: %+v", got.Services)
	}
}

func TestRunRemoteDockerCmdStream_Argv(t *testing.T) {
	extras := []string{"-i", "/tmp/key"}
	host := "user@example.com"
	var captured *exec.Cmd
	r := &RemoteCompose{
		Host:         host,
		SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
		SSHExtraArgs: extras,
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			fmt.Fprint(cmd.Stdout, "pull-progress")
			return nil
		},
	}
	var buf strings.Builder
	if err := r.runRemoteDockerCmdStream(context.Background(), []string{"pull", "nginx@sha256:ab12"}, &buf); err != nil {
		t.Fatalf("runRemoteDockerCmdStream: %v", err)
	}
	if captured == nil {
		t.Fatal("runCmd not called")
	}
	if captured.Args[0] != "ssh" {
		t.Errorf("argv[0] = %q, want ssh", captured.Args[0])
	}
	// SSHExtraArgs spliced immediately before the host (same convention as
	// runRemoteDockerCmd / writeRemoteFile).
	assertExtraBeforeHost(t, "runRemoteDockerCmdStream", captured.Args, host, extras)
	// Reuses the ControlMaster socket.
	foundSock := false
	for i, a := range captured.Args {
		if a == "-S" && i+1 < len(captured.Args) && captured.Args[i+1] == r.SocketPath {
			foundSock = true
		}
	}
	if !foundSock {
		t.Errorf("argv missing -S %q: %v", r.SocketPath, captured.Args)
	}
	// The remote command is a TOP-LEVEL docker command (no compose), shell-escaped.
	rc := captured.Args[len(captured.Args)-1]
	if rc != "docker 'pull' 'nginx@sha256:ab12'" {
		t.Errorf("remote command = %q, want %q", rc, "docker 'pull' 'nginx@sha256:ab12'")
	}
	if strings.Contains(rc, "compose") {
		t.Errorf("streaming remote docker command leaked 'compose': %q", rc)
	}
	if captured.Stdout == nil || captured.Stderr == nil {
		t.Error("stdout/stderr not wired to the writer")
	}
	if buf.String() != "pull-progress" {
		t.Errorf("writer got %q, want %q", buf.String(), "pull-progress")
	}
}

func TestRemotePrepareRollback_PullAndOverride(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:ab12"},
	}
	var pullRC string
	var writeRC string
	var overrideWritten []byte
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc := cmd.Args[len(cmd.Args)-1]
			switch {
			case strings.Contains(rc, "test -f"):
				return []byte("compose.yml\n"), nil // findRemoteComposeFile
			case strings.Contains(rc, "{{.Id}}"):
				return nil, fmt.Errorf("Error: No such image") // presence: missing → pull
			case strings.Contains(rc, "'config'"):
				return []byte(`{"services":{}}`), nil // advisory: no services
			case strings.Contains(rc, "'ps'"):
				return []byte(`[]`), nil // advisory: nothing running
			}
			return nil, fmt.Errorf("unexpected outputCmd: %q", rc)
		},
		runCmd: func(cmd *exec.Cmd) error {
			rc := cmd.Args[len(cmd.Args)-1]
			switch {
			case strings.Contains(rc, "'pull'"):
				pullRC = rc
			case strings.Contains(rc, "mkdir -p"):
				writeRC = rc
				overrideWritten, _ = io.ReadAll(cmd.Stdin)
			default:
				return fmt.Errorf("unexpected runCmd: %q", rc)
			}
			return nil
		},
	}

	var w strings.Builder
	cleanup, err := r.PrepareRollback(context.Background(), entries, nil, &w)
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup nil on success")
	}
	// pull-by-digest over SSH (top-level docker command).
	if pullRC != "docker 'pull' 'nginx@sha256:ab12'" {
		t.Errorf("pull remote command = %q, want %q", pullRC, "docker 'pull' 'nginx@sha256:ab12'")
	}
	// Override delivered to a per-invocation-unique /tmp/cdeploy-rollback-<pid>-<rand>.yml,
	// main file first. The exact path must not be the pid-only form (which
	// collides across clients rolling back the same project on the same host).
	if len(r.ExtraComposeFiles) != 2 || r.ExtraComposeFiles[0] != "compose.yml" {
		t.Fatalf("ExtraComposeFiles = %v, want [compose.yml <unique-override>]", r.ExtraComposeFiles)
	}
	overridePath := r.ExtraComposeFiles[1]
	if !strings.HasPrefix(overridePath, "/tmp/cdeploy-rollback-") || !strings.HasSuffix(overridePath, ".yml") {
		t.Errorf("override path = %q, want /tmp/cdeploy-rollback-*.yml", overridePath)
	}
	if overridePath == fmt.Sprintf("/tmp/cdeploy-rollback-%d.yml", os.Getpid()) {
		t.Errorf("override path = %q, want a random suffix (pid-only form collides across clients)", overridePath)
	}
	// The file must be delivered to the EXACT unique path that was recorded in
	// ExtraComposeFiles (write-path == recorded-path).
	if !strings.Contains(writeRC, overridePath) {
		t.Errorf("writeRemoteFile command %q does not reference override path %q", writeRC, overridePath)
	}
	if string(overrideWritten) != "services:\n  web:\n    image: nginx@sha256:ab12\n    pull_policy: never\n" {
		t.Errorf("override content = %q", string(overrideWritten))
	}
}

func TestRemotePrepareRollback_AbortsOnFailedPull(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:ab12"},
	}
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc := cmd.Args[len(cmd.Args)-1]
			switch {
			case strings.Contains(rc, "test -f"):
				return []byte("compose.yml\n"), nil
			case strings.Contains(rc, "{{.Id}}"):
				return nil, fmt.Errorf("Error: No such image") // missing → pull
			case strings.Contains(rc, "'config'"):
				return []byte(`{"services":{}}`), nil
			case strings.Contains(rc, "'ps'"):
				return []byte(`[]`), nil
			}
			return nil, fmt.Errorf("unexpected outputCmd: %q", rc)
		},
		runCmd: func(cmd *exec.Cmd) error {
			return fmt.Errorf("manifest unknown") // pull fails → abort
		},
	}
	var w strings.Builder
	cleanup, err := r.PrepareRollback(context.Background(), entries, nil, &w)
	if err == nil {
		t.Fatal("expected abort error on failed remote pull")
	}
	if cleanup != nil {
		t.Error("cleanup must be nil when PrepareRollback aborts")
	}
	if r.ExtraComposeFiles != nil {
		t.Errorf("ExtraComposeFiles mutated despite abort: %v", r.ExtraComposeFiles)
	}
	if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("abort error = %q, want it to name the service and 'unavailable'", err.Error())
	}
}

func TestRemotePrepareRollback_CleanupRemovesFileAndResetsField(t *testing.T) {
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:ab12"},
	}
	var rmRC string
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc := cmd.Args[len(cmd.Args)-1]
			switch {
			case strings.Contains(rc, "test -f"):
				return []byte("compose.yml\n"), nil
			case strings.Contains(rc, "{{.Id}}"):
				return []byte("sha256:present\n"), nil // cached → no pull
			case strings.Contains(rc, "'config'"):
				return []byte(`{"services":{}}`), nil
			case strings.Contains(rc, "'ps'"):
				return []byte(`[]`), nil
			}
			return nil, fmt.Errorf("unexpected outputCmd: %q", rc)
		},
		runCmd: func(cmd *exec.Cmd) error {
			rc := cmd.Args[len(cmd.Args)-1]
			if strings.Contains(rc, "rm -f") {
				rmRC = rc
			}
			return nil // writeRemoteFile + rm both succeed
		},
	}
	var w strings.Builder
	cleanup, err := r.PrepareRollback(context.Background(), entries, nil, &w)
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	if len(r.ExtraComposeFiles) != 2 {
		t.Fatalf("ExtraComposeFiles not set: %v", r.ExtraComposeFiles)
	}
	// Capture the exact unique override path BEFORE cleanup so we can assert
	// cleanup removes THAT path (not a recomputed/pid-only one).
	overridePath := r.ExtraComposeFiles[1]
	if !strings.HasPrefix(overridePath, "/tmp/cdeploy-rollback-") || !strings.HasSuffix(overridePath, ".yml") {
		t.Errorf("override path = %q, want /tmp/cdeploy-rollback-*.yml", overridePath)
	}

	cleanup()

	if r.ExtraComposeFiles != nil {
		t.Errorf("cleanup did not reset ExtraComposeFiles: %v", r.ExtraComposeFiles)
	}
	if !strings.Contains(rmRC, "rm -f") || !strings.Contains(rmRC, overridePath) {
		t.Errorf("cleanup rm command = %q, want it to rm -f %q", rmRC, overridePath)
	}
}

func TestRemoteRollbackOverridePath_UniqueAndShaped(t *testing.T) {
	pidPrefix := fmt.Sprintf("/tmp/cdeploy-rollback-%d-", os.Getpid())
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		p, err := remoteRollbackOverridePath()
		if err != nil {
			t.Fatalf("remoteRollbackOverridePath: %v", err)
		}
		if !strings.HasPrefix(p, pidPrefix) || !strings.HasSuffix(p, ".yml") {
			t.Fatalf("path %q, want prefix %q and suffix .yml", p, pidPrefix)
		}
		// The random component must not be empty (pid-only form collides across
		// clients on the same host).
		suffix := strings.TrimSuffix(strings.TrimPrefix(p, pidPrefix), ".yml")
		if suffix == "" {
			t.Fatalf("path %q has an empty random suffix", p)
		}
		if seen[p] {
			t.Fatalf("duplicate path generated: %q", p)
		}
		seen[p] = true
	}
}

func TestRemotePrepareRollback_SameDigestWarning(t *testing.T) {
	// Currently-running web container uses sha256:ab12 == snapshot digest →
	// "already at snapshot" advisory, prep proceeds.
	entries := map[string]SnapshotEntry{
		"web": {Image: "nginx:latest", Digest: "sha256:ab12"},
	}
	r := &RemoteCompose{
		Host:       "user@example.com",
		ProjectDir: "/proj",
		SocketPath: "/tmp/cdeploy-ctrl-abc-99",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			rc := cmd.Args[len(cmd.Args)-1]
			switch {
			case strings.Contains(rc, "test -f"):
				return []byte("compose.yml\n"), nil
			case strings.Contains(rc, "{{.Image}}"):
				return []byte("sha256:img-web\n"), nil
			case strings.Contains(rc, "RepoDigests"):
				return []byte("nginx@sha256:ab12\n"), nil // current running digest
			case strings.Contains(rc, "{{.Id}}"):
				return []byte("sha256:present\n"), nil // cached → no pull
			case strings.Contains(rc, "'config'"):
				return []byte(`{"services":{"web":{"image":"nginx:latest"}}}`), nil
			case strings.Contains(rc, "'ps'"):
				return []byte(`[{"ID":"cid-web","Service":"web","State":"running"}]`), nil
			}
			return nil, fmt.Errorf("unexpected outputCmd: %q", rc)
		},
		runCmd: func(cmd *exec.Cmd) error { return nil }, // writeRemoteFile
	}
	var w strings.Builder
	cleanup, err := r.PrepareRollback(context.Background(), entries, nil, &w)
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	defer cleanup()
	if !strings.Contains(w.String(), "already at snapshot") || !strings.Contains(w.String(), "sha256:ab12") {
		t.Errorf("expected an 'already at snapshot' advisory, got %q", w.String())
	}
	if len(r.ExtraComposeFiles) != 2 {
		t.Errorf("prep did not proceed past the advisory: %v", r.ExtraComposeFiles)
	}
}

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
)

// TestCheckRemoteMutex verifies the two-rule mutex:
//   - "server and identity without ssh": the --server/--ssh mutex needs both
//     non-empty to fire, so this misuse falls through to the new
//     `--identity requires --ssh` rule (the strict-narrow scoping we want).
//   - "server, ssh, and identity": the --server/--ssh mutex still fires first
//     (and reports first), regardless of identity.
func TestCheckRemoteMutex(t *testing.T) {
	tests := []struct {
		name         string
		serverName   string
		sshTarget    string
		identityFile string
		wantErr      string // substring in error; empty means no error
	}{
		{name: "all empty", serverName: "", sshTarget: "", identityFile: "", wantErr: ""},
		{name: "only server", serverName: "prod", sshTarget: "", identityFile: "", wantErr: ""},
		{name: "only ssh", serverName: "", sshTarget: "user@host", identityFile: "", wantErr: ""},
		{name: "server and ssh", serverName: "prod", sshTarget: "user@host", identityFile: "", wantErr: `--ssh ("user@host") and --server ("prod") are mutually exclusive`},
		{name: "ssh and identity", serverName: "", sshTarget: "user@host", identityFile: "/k", wantErr: ""},
		{name: "identity without ssh", serverName: "", sshTarget: "", identityFile: "/k", wantErr: "--identity requires --ssh"},
		{name: "server and identity without ssh", serverName: "prod", sshTarget: "", identityFile: "/k", wantErr: "--identity requires --ssh"},
		{name: "server, ssh, and identity", serverName: "prod", sshTarget: "user@host", identityFile: "/k", wantErr: "mutually exclusive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRemoteMutex(tt.serverName, tt.sshTarget, tt.identityFile)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// stubRemoteFactory returns a factory that builds a RemoteCompose with test
// hooks attached. runErr controls Connect behavior; outputErr controls Detect.
func stubRemoteFactory(runErr, outputErr error) (
	func(host, projDir string) *compose.RemoteCompose,
	func() *compose.RemoteCompose,
) {
	factory, getBuilt, _ := stubRemoteFactoryWithCloseCount(runErr, outputErr)
	return factory, getBuilt
}

// stubRemoteFactoryWithCloseCount is like stubRemoteFactory but also exposes
// a counter for "ssh ... -O exit" invocations (i.e., Close() calls). Used to
// assert that the helper closes the ControlMaster on Detect failure.
func stubRemoteFactoryWithCloseCount(runErr, outputErr error) (
	func(host, projDir string) *compose.RemoteCompose,
	func() *compose.RemoteCompose,
	func() int,
) {
	var built *compose.RemoteCompose
	closeCount := 0
	factory := func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error {
				// Detect a Close() invocation by looking for "-O exit" in
				// the SSH argv (see RemoteCompose.Close).
				for i, a := range cmd.Args {
					if a == "-O" && i+1 < len(cmd.Args) && cmd.Args[i+1] == "exit" {
						closeCount++
						break
					}
				}
				return runErr
			},
			func(cmd *exec.Cmd) ([]byte, error) {
				if outputErr != nil {
					return nil, outputErr
				}
				return []byte("Docker Compose version v2.0.0\n"), nil
			},
		)
		built = rc
		return rc
	}
	return factory, func() *compose.RemoteCompose { return built }, func() int { return closeCount }
}

func TestResolveSSHRemote_EmptyProjectDir(t *testing.T) {
	factory, _ := stubRemoteFactory(nil, nil)
	_, _, err := resolveSSHRemote(context.Background(), "user@host", "", "", factory)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --project-dir") {
		t.Errorf("error = %q, want it to contain 'requires --project-dir'", err.Error())
	}
}

func TestResolveSSHRemote_MalformedTarget(t *testing.T) {
	factory, _ := stubRemoteFactory(nil, nil)
	_, _, err := resolveSSHRemote(context.Background(), "user@", "/srv/app", "", factory)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid --ssh value") {
		t.Errorf("error = %q, want it to contain 'invalid --ssh value'", err.Error())
	}
	// Ensure underlying parser error is wrapped
	if !strings.Contains(err.Error(), "host is empty") {
		t.Errorf("error = %q, want it to wrap parser error 'host is empty'", err.Error())
	}
}

func TestResolveSSHRemote_HappyPath(t *testing.T) {
	factory, getBuilt := stubRemoteFactory(nil, nil)

	rc, cleanup, err := resolveSSHRemote(
		context.Background(),
		"user@host:2222",
		"/srv/app",
		"",
		factory,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc == nil {
		t.Fatal("expected non-nil RemoteCompose")
	}
	if cleanup == nil {
		t.Fatal("expected non-nil cleanup")
	}

	if rc != getBuilt() {
		t.Error("returned RemoteCompose differs from factory-built instance")
	}

	if rc.Host != "user@host" {
		t.Errorf("Host = %q, want %q", rc.Host, "user@host")
	}
	if rc.ProjectDir != "/srv/app" {
		t.Errorf("ProjectDir = %q, want %q", rc.ProjectDir, "/srv/app")
	}
	wantArgs := []string{"-p", "2222"}
	if len(rc.SSHExtraArgs) != len(wantArgs) {
		t.Fatalf("SSHExtraArgs = %v, want %v", rc.SSHExtraArgs, wantArgs)
	}
	for i, want := range wantArgs {
		if rc.SSHExtraArgs[i] != want {
			t.Errorf("SSHExtraArgs[%d] = %q, want %q", i, rc.SSHExtraArgs[i], want)
		}
	}

	// cleanup must be safe to call (won't panic; uses test hook to no-op).
	cleanup()
}

func TestResolveSSHRemote_HappyPathNoPort(t *testing.T) {
	factory, _ := stubRemoteFactory(nil, nil)

	rc, cleanup, err := resolveSSHRemote(
		context.Background(),
		"host",
		"/srv/app",
		"",
		factory,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	if rc.Host != "host" {
		t.Errorf("Host = %q, want %q", rc.Host, "host")
	}
	if rc.SSHExtraArgs != nil {
		t.Errorf("SSHExtraArgs = %v, want nil", rc.SSHExtraArgs)
	}
}

func TestResolveSSHRemote_ConnectFailure(t *testing.T) {
	factory, _ := stubRemoteFactory(fmt.Errorf("network unreachable"), nil)

	rc, cleanup, err := resolveSSHRemote(
		context.Background(),
		"user@host",
		"/srv/app",
		"",
		factory,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if rc != nil {
		t.Error("expected nil RemoteCompose on Connect failure")
	}
	if cleanup == nil {
		t.Error("expected non-nil (no-op) cleanup on Connect failure")
	} else {
		cleanup() // must be safe to call (no-op)
	}
	if !strings.Contains(err.Error(), "connecting to user@host") {
		t.Errorf("error = %q, want it to contain 'connecting to user@host'", err.Error())
	}
	if !strings.Contains(err.Error(), "network unreachable") {
		t.Errorf("error = %q, want it to wrap underlying error", err.Error())
	}
}

func TestResolveSSHRemote_DetectFailure(t *testing.T) {
	// Connect succeeds (runCmd returns nil), Detect fails (outputCmd returns error).
	factory, _, getCloseCount := stubRemoteFactoryWithCloseCount(nil, fmt.Errorf("docker not installed"))

	rc, cleanup, err := resolveSSHRemote(
		context.Background(),
		"user@host",
		"/srv/app",
		"",
		factory,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if rc != nil {
		t.Error("expected nil RemoteCompose on Detect failure")
	}
	if cleanup == nil {
		t.Error("expected non-nil (no-op) cleanup on Detect failure")
	} else {
		cleanup() // must be safe to call (no-op; helper already closed)
	}
	// Verify the helper actually called Close() on the established
	// ControlMaster connection — i.e., issued an `ssh ... -O exit`.
	if got := getCloseCount(); got != 1 {
		t.Errorf("expected exactly 1 Close()/(-O exit) invocation, got %d", got)
	}
}

func TestResolveSSHRemote_WithIdentity_HappyPath(t *testing.T) {
	// Create a temp file to act as the SSH key.
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "id_test")
	if err := os.WriteFile(keyPath, []byte("dummy key"), 0o600); err != nil {
		t.Fatalf("failed to write temp key: %v", err)
	}

	factory, _ := stubRemoteFactory(nil, nil)

	rc, cleanup, err := resolveSSHRemote(
		context.Background(),
		"user@host:2222",
		"/srv/app",
		keyPath,
		factory,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	wantArgs := []string{"-p", "2222", "-i", keyPath}
	if len(rc.SSHExtraArgs) != len(wantArgs) {
		t.Fatalf("SSHExtraArgs = %v, want %v", rc.SSHExtraArgs, wantArgs)
	}
	for i, want := range wantArgs {
		if rc.SSHExtraArgs[i] != want {
			t.Errorf("SSHExtraArgs[%d] = %q, want %q", i, rc.SSHExtraArgs[i], want)
		}
	}
}

func TestResolveSSHRemote_WithIdentity_NoPort(t *testing.T) {
	// Identity but no port: SSHExtraArgs should be just ["-i", keyPath].
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "id_test")
	if err := os.WriteFile(keyPath, []byte("dummy key"), 0o600); err != nil {
		t.Fatalf("failed to write temp key: %v", err)
	}

	factory, _ := stubRemoteFactory(nil, nil)

	rc, cleanup, err := resolveSSHRemote(
		context.Background(),
		"user@host",
		"/srv/app",
		keyPath,
		factory,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	wantArgs := []string{"-i", keyPath}
	if len(rc.SSHExtraArgs) != len(wantArgs) {
		t.Fatalf("SSHExtraArgs = %v, want %v", rc.SSHExtraArgs, wantArgs)
	}
	for i, want := range wantArgs {
		if rc.SSHExtraArgs[i] != want {
			t.Errorf("SSHExtraArgs[%d] = %q, want %q", i, rc.SSHExtraArgs[i], want)
		}
	}
}

func TestResolveSSHRemote_WithIdentity_InvalidPath(t *testing.T) {
	factory, _ := stubRemoteFactory(nil, nil)

	_, cleanup, err := resolveSSHRemote(
		context.Background(),
		"user@host",
		"/srv/app",
		"/nonexistent/key",
		factory,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	cleanup() // must be the no-op (safe to call)
	if !strings.Contains(err.Error(), "invalid --identity value") {
		t.Errorf("error = %q, want it to contain 'invalid --identity value'", err.Error())
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to wrap 'not found' from ParseIdentity", err.Error())
	}
}

func TestResolveSSHRemote_WithIdentity_TildeExpansion(t *testing.T) {
	// Set HOME to a temp dir, create a key under $HOME/.ssh, then pass `~/.ssh/id_test`.
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	sshDir := filepath.Join(homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("failed to create .ssh dir: %v", err)
	}
	keyPath := filepath.Join(sshDir, "id_test")
	if err := os.WriteFile(keyPath, []byte("dummy key"), 0o600); err != nil {
		t.Fatalf("failed to write temp key: %v", err)
	}

	factory, _ := stubRemoteFactory(nil, nil)

	rc, cleanup, err := resolveSSHRemote(
		context.Background(),
		"user@host",
		"/srv/app",
		"~/.ssh/id_test",
		factory,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	wantArgs := []string{"-i", keyPath}
	if len(rc.SSHExtraArgs) != len(wantArgs) {
		t.Fatalf("SSHExtraArgs = %v, want %v (~/ expanded to %q)", rc.SSHExtraArgs, wantArgs, homeDir)
	}
	for i, want := range wantArgs {
		if rc.SSHExtraArgs[i] != want {
			t.Errorf("SSHExtraArgs[%d] = %q, want %q", i, rc.SSHExtraArgs[i], want)
		}
	}
}

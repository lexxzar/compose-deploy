package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
)

func TestCheckRemoteMutex(t *testing.T) {
	tests := []struct {
		name       string
		serverName string
		sshTarget  string
		wantErr    string // substring in error; empty means no error
	}{
		{name: "both empty", serverName: "", sshTarget: "", wantErr: ""},
		{name: "only server", serverName: "prod", sshTarget: "", wantErr: ""},
		{name: "only ssh", serverName: "", sshTarget: "user@host", wantErr: ""},
		{name: "both set", serverName: "prod", sshTarget: "user@host", wantErr: "mutually exclusive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRemoteMutex(tt.serverName, tt.sshTarget)
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
	var built *compose.RemoteCompose
	factory := func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return runErr },
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
	return factory, func() *compose.RemoteCompose { return built }
}

func TestResolveSSHRemote_EmptyProjectDir(t *testing.T) {
	factory, _ := stubRemoteFactory(nil, nil)
	_, _, err := resolveSSHRemote(context.Background(), "user@host", "", factory)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --project-dir") {
		t.Errorf("error = %q, want it to contain 'requires --project-dir'", err.Error())
	}
}

func TestResolveSSHRemote_MalformedTarget(t *testing.T) {
	factory, _ := stubRemoteFactory(nil, nil)
	_, _, err := resolveSSHRemote(context.Background(), "user@", "/srv/app", factory)
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
		factory,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if rc != nil {
		t.Error("expected nil RemoteCompose on Connect failure")
	}
	if cleanup != nil {
		t.Error("expected nil cleanup on Connect failure (caller must check err first)")
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
	factory, _ := stubRemoteFactory(nil, fmt.Errorf("docker not installed"))

	rc, cleanup, err := resolveSSHRemote(
		context.Background(),
		"user@host",
		"/srv/app",
		factory,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if rc != nil {
		t.Error("expected nil RemoteCompose on Detect failure")
	}
	if cleanup != nil {
		t.Error("expected nil cleanup on Detect failure (helper closes internally)")
	}
}

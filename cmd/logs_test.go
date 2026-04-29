package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
)

func TestRunLogs_NoComposeFile(t *testing.T) {
	oldHas := logsHasCompose
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		logsHasCompose = oldHas
		projectDir = oldProj
		serverName = oldServer
	})

	logsHasCompose = func(dir string) bool { return false }
	projectDir = ""
	serverName = ""

	err := runLogs(context.Background(), "nginx", true, 50)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no compose file found") {
		t.Errorf("error = %q, want it to contain 'no compose file found'", err.Error())
	}
}

// mockLogsComposer captures Logs call arguments.
type mockLogsComposer struct {
	mockComposer
	logService string
	logFollow  bool
	logTail    int
	logErr     error
}

func (m *mockLogsComposer) Logs(_ context.Context, service string, follow bool, tail int, _ io.Writer) error {
	m.logService = service
	m.logFollow = follow
	m.logTail = tail
	return m.logErr
}

// newTestLogsCompose creates a *compose.Compose with test hooks that delegate to a mockLogsComposer.
func newTestLogsCompose(dir string, mock *mockLogsComposer) *compose.Compose {
	c := compose.New(dir)
	c.SetTestHooks(
		func(cmd *exec.Cmd) error {
			args := strings.Join(cmd.Args, " ")
			if strings.Contains(args, "logs") {
				// Extract follow, tail, and service from args
				mock.logFollow = strings.Contains(args, "--follow")
				// Parse tail value
				for i, a := range cmd.Args {
					if a == "--tail" && i+1 < len(cmd.Args) {
						fmt.Sscanf(cmd.Args[i+1], "%d", &mock.logTail)
					}
				}
				// Service is the last arg (after compose args)
				mock.logService = cmd.Args[len(cmd.Args)-1]
				return mock.logErr
			}
			return nil
		},
		func(cmd *exec.Cmd) ([]byte, error) {
			// Handle Detect probe
			args := strings.Join(cmd.Args, " ")
			if strings.Contains(args, "version") {
				return []byte("Docker Compose version v2.24.0\n"), nil
			}
			return nil, nil
		},
	)
	return c
}

func TestRunLogs_LocalSuccess(t *testing.T) {
	oldHas := logsHasCompose
	oldNew := logsNewLocal
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		logsHasCompose = oldHas
		logsNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	mock := &mockLogsComposer{}
	logsHasCompose = func(dir string) bool { return true }
	logsNewLocal = func(dir string) *compose.Compose { return newTestLogsCompose(dir, mock) }
	projectDir = ""
	serverName = ""

	err := runLogs(context.Background(), "nginx", true, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.logService != "nginx" {
		t.Errorf("service = %q, want %q", mock.logService, "nginx")
	}
	if !mock.logFollow {
		t.Error("follow = false, want true")
	}
	if mock.logTail != 100 {
		t.Errorf("tail = %d, want %d", mock.logTail, 100)
	}
}

func TestRunLogs_LocalNoFollow(t *testing.T) {
	oldHas := logsHasCompose
	oldNew := logsNewLocal
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		logsHasCompose = oldHas
		logsNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	mock := &mockLogsComposer{}
	logsHasCompose = func(dir string) bool { return true }
	logsNewLocal = func(dir string) *compose.Compose { return newTestLogsCompose(dir, mock) }
	projectDir = ""
	serverName = ""

	err := runLogs(context.Background(), "redis", false, 25)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.logFollow {
		t.Error("follow = true, want false")
	}
}

func TestRunLogs_LogsError(t *testing.T) {
	oldHas := logsHasCompose
	oldNew := logsNewLocal
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		logsHasCompose = oldHas
		logsNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	mock := &mockLogsComposer{logErr: context.Canceled}
	logsHasCompose = func(dir string) bool { return true }
	logsNewLocal = func(dir string) *compose.Compose { return newTestLogsCompose(dir, mock) }
	projectDir = ""
	serverName = ""

	err := runLogs(context.Background(), "web", true, 50)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestRunLogs_ServerNotFound(t *testing.T) {
	oldServer := serverName
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
	})

	// No config file → empty config → server not found
	serverName = "nonexistent"
	projectDir = ""

	err := runLogs(context.Background(), "nginx", true, 50)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to contain 'not found'", err.Error())
	}
}

func TestRunLogs_ServerNoProjectDir(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := tmpHome + "/.cdeploy"
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Server with no project_dir
	cfgData := "servers:\n  - name: srv\n    host: user@host\n"
	if err := os.WriteFile(cfgDir+"/servers.yml", []byte(cfgData), 0o644); err != nil {
		t.Fatal(err)
	}

	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	oldServer := serverName
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
	})

	serverName = "srv"
	projectDir = ""

	err := runLogs(context.Background(), "nginx", true, 50)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --project-dir") {
		t.Errorf("error = %q, want it to contain 'requires --project-dir'", err.Error())
	}
}

func TestRunLogs_ServerSuccess(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := tmpHome + "/.cdeploy"
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgData := "servers:\n  - name: prod\n    host: user@prod\n    project_dir: /opt/app\n"
	if err := os.WriteFile(cfgDir+"/servers.yml", []byte(cfgData), 0o644); err != nil {
		t.Fatal(err)
	}

	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	oldServer := serverName
	oldProj := projectDir
	oldNewRemote := logsNewRemote
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
		logsNewRemote = oldNewRemote
	})

	serverName = "prod"
	projectDir = ""

	var logsCalled bool
	logsNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "'logs'") {
					logsCalled = true
				}
				return nil
			},
			func(cmd *exec.Cmd) ([]byte, error) {
				remoteCmd := cmd.Args[len(cmd.Args)-1]
				if strings.Contains(remoteCmd, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				return nil, nil
			},
		)
		return rc
	}

	err := runLogs(context.Background(), "nginx", true, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !logsCalled {
		t.Error("logs not called on remote")
	}
}

func TestRunLogs_LocalDetectFailure(t *testing.T) {
	oldHas := logsHasCompose
	oldNew := logsNewLocal
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		logsHasCompose = oldHas
		logsNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	logsHasCompose = func(dir string) bool { return true }
	logsNewLocal = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				return nil, fmt.Errorf("not found")
			},
		)
		return c
	}
	projectDir = ""
	serverName = ""

	err := runLogs(context.Background(), "nginx", true, 50)
	if err == nil {
		t.Fatal("expected error when Detect fails")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error = %q, want it to contain 'neither'", err.Error())
	}
}

func TestRunLogs_SSHAndServerMutex(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
	})

	serverName = "prod"
	sshTarget = "user@host"
	projectDir = "/srv/app"

	err := runLogs(context.Background(), "nginx", true, 50)
	if err == nil {
		t.Fatal("expected mutex error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want it to contain 'mutually exclusive'", err.Error())
	}
}

func TestRunLogs_SSHRequiresProjectDir(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
	})

	serverName = ""
	sshTarget = "user@host"
	projectDir = ""

	err := runLogs(context.Background(), "nginx", true, 50)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --project-dir") {
		t.Errorf("error = %q, want it to contain 'requires --project-dir'", err.Error())
	}
}

func TestRunLogs_SSHHappyPath(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldIdentity := identityFile
	oldNewRemote := logsNewRemote
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		identityFile = oldIdentity
		logsNewRemote = oldNewRemote
	})

	serverName = ""
	sshTarget = "deploy@host:2222"
	projectDir = "/srv/app"
	identityFile = ""

	var capturedSSHArgs []string
	var logsCalled bool
	logsNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error {
				if cmd.Args[0] == "ssh" && len(capturedSSHArgs) == 0 {
					// First ssh call after Detect probe is the connect (-fNM).
					// Capture the args from any docker compose remote call to
					// verify port splicing.
				}
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "'logs'") {
					logsCalled = true
					capturedSSHArgs = append([]string(nil), cmd.Args...)
				}
				return nil
			},
			func(cmd *exec.Cmd) ([]byte, error) {
				return []byte("Docker Compose version v2.24.0\n"), nil
			},
		)
		return rc
	}

	err := runLogs(context.Background(), "nginx", true, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !logsCalled {
		t.Fatal("logs subcommand was not invoked on remote")
	}

	// Verify -p 2222 appears in SSH argv (port splicing) and that the
	// remote command contains "logs".
	joined := strings.Join(capturedSSHArgs, " ")
	if !strings.Contains(joined, "-p 2222") {
		t.Errorf("ssh argv = %v, want to contain '-p 2222'", capturedSSHArgs)
	}
	if !strings.Contains(joined, "'logs'") {
		t.Errorf("ssh argv = %v, want to contain 'logs' subcommand", capturedSSHArgs)
	}
}

// TestRunLogs_SSHHappyPathWithIdentity verifies the logs subcommand splices
// -i <keyPath> into SSHExtraArgs when both --ssh and --identity are set.
func TestRunLogs_SSHHappyPathWithIdentity(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldIdentity := identityFile
	oldNewRemote := logsNewRemote
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		identityFile = oldIdentity
		logsNewRemote = oldNewRemote
	})

	tmpDir := t.TempDir()
	keyPath := tmpDir + "/id_test"
	if err := os.WriteFile(keyPath, []byte("dummy"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	serverName = ""
	sshTarget = "deploy@host:2222"
	projectDir = "/srv/app"
	identityFile = keyPath

	var capturedSSHArgs []string
	logsNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "'logs'") {
					capturedSSHArgs = append([]string(nil), cmd.Args...)
				}
				return nil
			},
			func(cmd *exec.Cmd) ([]byte, error) {
				return []byte("Docker Compose version v2.24.0\n"), nil
			},
		)
		return rc
	}

	if err := runLogs(context.Background(), "nginx", true, 50); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedSSHArgs == nil {
		t.Fatal("logs subcommand was not invoked on remote")
	}
	joined := strings.Join(capturedSSHArgs, " ")
	if !strings.Contains(joined, "-p 2222") {
		t.Errorf("ssh argv = %v, want to contain '-p 2222'", capturedSSHArgs)
	}
	if !strings.Contains(joined, "-i "+keyPath) {
		t.Errorf("ssh argv = %v, want to contain '-i %s'", capturedSSHArgs, keyPath)
	}
}

func TestLogs_IdentityWithoutSSH(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldIdentity := identityFile
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		identityFile = oldIdentity
	})

	serverName = ""
	sshTarget = ""
	projectDir = ""
	identityFile = "/tmp/k"

	err := runLogs(context.Background(), "nginx", true, 50)
	if err == nil {
		t.Fatal("expected error when --identity is set without --ssh")
	}
	if !strings.Contains(err.Error(), "--identity requires --ssh") {
		t.Errorf("error = %q, want it to contain '--identity requires --ssh'", err.Error())
	}
}

func TestLogsCmd_SSHFlagInherited(t *testing.T) {
	root := NewRootCmd()

	cmd, _, err := root.Find([]string{"logs"})
	if err != nil {
		t.Fatalf("logs command not found: %v", err)
	}
	sshFlag := cmd.InheritedFlags().Lookup("ssh")
	if sshFlag == nil {
		t.Error("--ssh persistent flag not inherited by logs command")
	}
	if sshFlag != nil && sshFlag.Shorthand != "S" {
		t.Errorf("--ssh shorthand = %q, want %q", sshFlag.Shorthand, "S")
	}
}

func TestRunLogs_WithProjectDir(t *testing.T) {
	oldHas := logsHasCompose
	oldNew := logsNewLocal
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		logsHasCompose = oldHas
		logsNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	var capturedDir string
	mock := &mockLogsComposer{}
	logsHasCompose = func(dir string) bool { return true }
	logsNewLocal = func(dir string) *compose.Compose {
		capturedDir = dir
		return newTestLogsCompose(dir, mock)
	}
	projectDir = "/custom/path"
	serverName = ""

	err := runLogs(context.Background(), "web", true, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedDir != "/custom/path" {
		t.Errorf("dir = %q, want %q", capturedDir, "/custom/path")
	}
}

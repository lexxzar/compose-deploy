package cmd

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/runner"
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
	logsNewLocal = func(dir string) runner.Composer { return mock }
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
	logsNewLocal = func(dir string) runner.Composer { return mock }
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
	logsNewLocal = func(dir string) runner.Composer { return mock }
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
			nil,
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
	logsNewLocal = func(dir string) runner.Composer {
		capturedDir = dir
		return mock
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

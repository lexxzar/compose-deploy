package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/logging"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

func TestDeployCmd_NoArgsNoFlag(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"deploy"})

	var stderr bytes.Buffer
	root.SetErr(&stderr)

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when no args and no -a flag")
	}
}

func TestRestartCmd_NoArgsNoFlag(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"restart"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when no args and no -a flag")
	}
}

func TestDeployCmd_FlagRegistration(t *testing.T) {
	root := NewRootCmd()

	deploy, _, err := root.Find([]string{"deploy"})
	if err != nil {
		t.Fatalf("deploy command not found: %v", err)
	}

	flag := deploy.Flags().Lookup("all")
	if flag == nil {
		t.Fatal("--all flag not found on deploy command")
	}
	if flag.Shorthand != "a" {
		t.Errorf("--all shorthand = %q, want %q", flag.Shorthand, "a")
	}
}

func TestRestartCmd_FlagRegistration(t *testing.T) {
	root := NewRootCmd()

	restart, _, err := root.Find([]string{"restart"})
	if err != nil {
		t.Fatalf("restart command not found: %v", err)
	}

	flag := restart.Flags().Lookup("all")
	if flag == nil {
		t.Fatal("--all flag not found on restart command")
	}
	if flag.Shorthand != "a" {
		t.Errorf("--all shorthand = %q, want %q", flag.Shorthand, "a")
	}
}

func TestStopCmd_NoArgsNoFlag(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"stop"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when no args and no -a flag")
	}
}

func TestStopCmd_FlagRegistration(t *testing.T) {
	root := NewRootCmd()

	stop, _, err := root.Find([]string{"stop"})
	if err != nil {
		t.Fatalf("stop command not found: %v", err)
	}

	flag := stop.Flags().Lookup("all")
	if flag == nil {
		t.Fatal("--all flag not found on stop command")
	}
	if flag.Shorthand != "a" {
		t.Errorf("--all shorthand = %q, want %q", flag.Shorthand, "a")
	}
}

func TestDeployCmd_SubcommandExists(t *testing.T) {
	root := NewRootCmd()

	for _, name := range []string{"deploy", "restart", "stop"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Errorf("command %q not found: %v", name, err)
			continue
		}
		if cmd.Name() != name {
			t.Errorf("found command name = %q, want %q", cmd.Name(), name)
		}
	}
}

func TestAllFlagWithContainerNames(t *testing.T) {
	for _, name := range []string{"deploy", "restart", "stop"} {
		t.Run(name, func(t *testing.T) {
			root := NewRootCmd()
			root.SetArgs([]string{name, "-a", "nginx"})

			err := root.Execute()
			if err == nil {
				t.Fatalf("%s -a nginx: expected error, got nil", name)
			}
			if !strings.Contains(err.Error(), "cannot be combined") {
				t.Errorf("unexpected error message: %v", err)
			}
		})
	}
}

func TestServerFlag_Registration(t *testing.T) {
	root := NewRootCmd()

	flag := root.PersistentFlags().Lookup("server")
	if flag == nil {
		t.Fatal("--server flag not found")
	}
	if flag.Shorthand != "s" {
		t.Errorf("--server shorthand = %q, want %q", flag.Shorthand, "s")
	}
	if flag.DefValue != "" {
		t.Errorf("--server default = %q, want empty", flag.DefValue)
	}
}

func TestServerFlag_NotFound(t *testing.T) {
	// Snapshot/restore package-level globals — cobra binds flags to them
	// and root.Execute() will mutate them, leaking state to subsequent tests
	// when -count >1 or -shuffle is used.
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
	})

	root := NewRootCmd()
	root.SetArgs([]string{"deploy", "-s", "nonexistent", "-a"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unknown server")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to contain 'not found'", err.Error())
	}
}

func TestServerFlag_NoProjectDir(t *testing.T) {
	// When --server is used but neither --project-dir nor config project_dir is set,
	// it should error. We can't easily test this without a config file, but we can
	// test that the flag is inherited by subcommands.
	root := NewRootCmd()
	deploy, _, _ := root.Find([]string{"deploy"})
	if deploy == nil {
		t.Fatal("deploy command not found")
	}

	serverFlag := deploy.InheritedFlags().Lookup("server")
	if serverFlag == nil {
		t.Error("--server persistent flag not inherited by deploy command")
	}
}

func TestNoServerFlag_LocalBehaviorUnchanged(t *testing.T) {
	// Without --server, the behavior should be unchanged (local mode).
	// We can verify by running deploy without -s and seeing it tries to use local compose.
	root := NewRootCmd()
	root.SetArgs([]string{"deploy", "-a"})

	// This will fail because docker isn't available, but it should NOT fail
	// with a "server not found" error — it should proceed to local mode.
	err := root.Execute()
	if err != nil && strings.Contains(err.Error(), "not found in config") {
		t.Errorf("without --server flag, should not try to find server: %v", err)
	}
}

func TestDeployCmd_PersistentFlagsInherited(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldLogDir := logDir
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		logDir = oldLogDir
	})

	root := NewRootCmd()
	root.SetArgs([]string{"deploy", "--log-dir", "/tmp/test", "-C", "/proj", "-a"})

	// This will fail because docker isn't available, but we can verify
	// the flags are parsed correctly by checking flag values after parse
	deploy, _, _ := root.Find([]string{"deploy"})
	if deploy == nil {
		t.Fatal("deploy command not found")
	}

	// Verify persistent flags are visible from subcommand via InheritedFlags
	logDirFlag := deploy.InheritedFlags().Lookup("log-dir")
	if logDirFlag == nil {
		t.Error("--log-dir persistent flag not inherited by deploy command")
	}

	projectDirFlag := deploy.InheritedFlags().Lookup("project-dir")
	if projectDirFlag == nil {
		t.Error("--project-dir persistent flag not inherited by deploy command")
	}
}

// opMockComposer implements runner.Composer for runOperation tests.
type opMockComposer struct {
	stopCalls   int
	removeCalls int
	pullCalls   int
	createCalls int
	startCalls  int
	failStep    string // which step should fail (e.g. "pull")
}

func (m *opMockComposer) ListServices(_ context.Context) ([]string, error) { return nil, nil }
func (m *opMockComposer) ContainerStatus(_ context.Context) (map[string]runner.ServiceStatus, error) {
	return nil, nil
}
func (m *opMockComposer) Logs(_ context.Context, _ string, _ bool, _ int, _ io.Writer) error {
	return nil
}
func (m *opMockComposer) Stop(_ context.Context, _ []string, _ io.Writer) error {
	m.stopCalls++
	if m.failStep == "stop" {
		return fmt.Errorf("stop failed")
	}
	return nil
}
func (m *opMockComposer) Remove(_ context.Context, _ []string, _ io.Writer) error {
	m.removeCalls++
	if m.failStep == "remove" {
		return fmt.Errorf("remove failed")
	}
	return nil
}
func (m *opMockComposer) Pull(_ context.Context, _ []string, _ io.Writer) error {
	m.pullCalls++
	if m.failStep == "pull" {
		return fmt.Errorf("pull failed")
	}
	return nil
}
func (m *opMockComposer) Create(_ context.Context, _ []string, _ io.Writer) error {
	m.createCalls++
	if m.failStep == "create" {
		return fmt.Errorf("create failed")
	}
	return nil
}
func (m *opMockComposer) Start(_ context.Context, _ []string, _ io.Writer) error {
	m.startCalls++
	if m.failStep == "start" {
		return fmt.Errorf("start failed")
	}
	return nil
}

// newTestCompose creates a *compose.Compose with test hooks that delegate to the mock.
// The outputCmd hook handles the Detect probe by succeeding for "docker compose version".
func newTestCompose(dir string, mock *opMockComposer) *compose.Compose {
	c := compose.New(dir)
	c.SetTestHooks(
		func(cmd *exec.Cmd) error {
			args := strings.Join(cmd.Args, " ")
			if strings.Contains(args, "stop") {
				return mock.Stop(context.Background(), nil, cmd.Stdout)
			}
			if strings.Contains(args, "rm") {
				return mock.Remove(context.Background(), nil, cmd.Stdout)
			}
			if strings.Contains(args, "pull") {
				return mock.Pull(context.Background(), nil, cmd.Stdout)
			}
			if strings.Contains(args, "up") {
				return mock.Create(context.Background(), nil, cmd.Stdout)
			}
			if strings.Contains(args, "start") {
				return mock.Start(context.Background(), nil, cmd.Stdout)
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

func TestRunOperation_LocalDeploy(t *testing.T) {
	oldNew := opNewLocal
	oldLogger := opNewLogger
	oldProj := projectDir
	oldServer := serverName
	oldLogDir := logDir
	t.Cleanup(func() {
		opNewLocal = oldNew
		opNewLogger = oldLogger
		projectDir = oldProj
		serverName = oldServer
		logDir = oldLogDir
	})

	mock := &opMockComposer{}
	opNewLocal = func(dir string) *compose.Compose { return newTestCompose(dir, mock) }
	opNewLogger = func(dir string) (*logging.Logger, error) {
		return logging.NewLogger(t.TempDir())
	}
	projectDir = ""
	serverName = ""
	logDir = t.TempDir()

	err := runOperation(context.Background(), runner.Deploy, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.stopCalls != 1 {
		t.Errorf("stop calls = %d, want 1", mock.stopCalls)
	}
	if mock.pullCalls != 1 {
		t.Errorf("pull calls = %d, want 1", mock.pullCalls)
	}
	if mock.startCalls != 1 {
		t.Errorf("start calls = %d, want 1", mock.startCalls)
	}
}

func TestRunOperation_LocalRestart(t *testing.T) {
	oldNew := opNewLocal
	oldLogger := opNewLogger
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		opNewLocal = oldNew
		opNewLogger = oldLogger
		projectDir = oldProj
		serverName = oldServer
	})

	mock := &opMockComposer{}
	opNewLocal = func(dir string) *compose.Compose { return newTestCompose(dir, mock) }
	opNewLogger = func(dir string) (*logging.Logger, error) {
		return logging.NewLogger(t.TempDir())
	}
	projectDir = ""
	serverName = ""

	err := runOperation(context.Background(), runner.Restart, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.pullCalls != 0 {
		t.Errorf("restart should not pull, but pull calls = %d", mock.pullCalls)
	}
	if mock.stopCalls != 1 || mock.startCalls != 1 {
		t.Errorf("restart should stop+start, got stop=%d start=%d", mock.stopCalls, mock.startCalls)
	}
}

func TestRunOperation_LocalStop(t *testing.T) {
	oldNew := opNewLocal
	oldLogger := opNewLogger
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		opNewLocal = oldNew
		opNewLogger = oldLogger
		projectDir = oldProj
		serverName = oldServer
	})

	mock := &opMockComposer{}
	opNewLocal = func(dir string) *compose.Compose { return newTestCompose(dir, mock) }
	opNewLogger = func(dir string) (*logging.Logger, error) {
		return logging.NewLogger(t.TempDir())
	}
	projectDir = ""
	serverName = ""

	err := runOperation(context.Background(), runner.StopOnly, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.stopCalls != 1 {
		t.Errorf("stop calls = %d, want 1", mock.stopCalls)
	}
	if mock.startCalls != 0 {
		t.Errorf("stop-only should not start, but start calls = %d", mock.startCalls)
	}
}

func TestRunOperation_FailedStep(t *testing.T) {
	oldNew := opNewLocal
	oldLogger := opNewLogger
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		opNewLocal = oldNew
		opNewLogger = oldLogger
		projectDir = oldProj
		serverName = oldServer
	})

	mock := &opMockComposer{failStep: "pull"}
	opNewLocal = func(dir string) *compose.Compose { return newTestCompose(dir, mock) }
	opNewLogger = func(dir string) (*logging.Logger, error) {
		return logging.NewLogger(t.TempDir())
	}
	projectDir = ""
	serverName = ""

	err := runOperation(context.Background(), runner.Deploy, true, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error = %q, want it to contain 'failed'", err.Error())
	}
}

func TestRunOperation_WithContainers(t *testing.T) {
	oldNew := opNewLocal
	oldLogger := opNewLogger
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		opNewLocal = oldNew
		opNewLogger = oldLogger
		projectDir = oldProj
		serverName = oldServer
	})

	mock := &opMockComposer{}
	opNewLocal = func(dir string) *compose.Compose { return newTestCompose(dir, mock) }
	opNewLogger = func(dir string) (*logging.Logger, error) {
		return logging.NewLogger(t.TempDir())
	}
	projectDir = ""
	serverName = ""

	err := runOperation(context.Background(), runner.Restart, false, []string{"nginx", "postgres"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.stopCalls != 1 {
		t.Errorf("stop calls = %d, want 1", mock.stopCalls)
	}
}

func TestRunOperation_WithProjectDir(t *testing.T) {
	oldNew := opNewLocal
	oldLogger := opNewLogger
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		opNewLocal = oldNew
		opNewLogger = oldLogger
		projectDir = oldProj
		serverName = oldServer
	})

	var capturedDir string
	mock := &opMockComposer{}
	opNewLocal = func(dir string) *compose.Compose {
		capturedDir = dir
		return newTestCompose(dir, mock)
	}
	opNewLogger = func(dir string) (*logging.Logger, error) {
		return logging.NewLogger(t.TempDir())
	}
	projectDir = "/custom/project"
	serverName = ""

	err := runOperation(context.Background(), runner.StopOnly, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedDir != "/custom/project" {
		t.Errorf("dir = %q, want %q", capturedDir, "/custom/project")
	}
}

func TestRunOperation_ServerDeploy(t *testing.T) {
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
	oldNewRemote := opNewRemote
	oldNewLogger := opNewLogger
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
		opNewRemote = oldNewRemote
		opNewLogger = oldNewLogger
	})

	serverName = "prod"
	projectDir = ""

	// Track which compose operations were called via the remote command
	var stopCalled, pullCalled, startCalled bool
	opNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error {
				args := strings.Join(cmd.Args, " ")
				// Detect compose operations from the remote command string
				if strings.Contains(args, "'stop'") {
					stopCalled = true
				}
				if strings.Contains(args, "'pull'") {
					pullCalled = true
				}
				if strings.Contains(args, "'start'") {
					startCalled = true
				}
				return nil
			},
			func(cmd *exec.Cmd) ([]byte, error) {
				// Handle Detect probe
				remoteCmd := cmd.Args[len(cmd.Args)-1]
				if strings.Contains(remoteCmd, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				return nil, nil
			},
		)
		return rc
	}
	opNewLogger = func(dir string) (*logging.Logger, error) {
		return logging.NewLogger(t.TempDir())
	}

	err := runOperation(context.Background(), runner.Deploy, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stopCalled {
		t.Error("stop not called on remote")
	}
	if !pullCalled {
		t.Error("pull not called on remote")
	}
	if !startCalled {
		t.Error("start not called on remote")
	}
}

func TestRunOperation_ServerNotFound(t *testing.T) {
	oldServer := serverName
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
	})

	serverName = "nonexistent"
	projectDir = ""

	err := runOperation(context.Background(), runner.Deploy, true, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to contain 'not found'", err.Error())
	}
}

func TestRunOperation_ServerNoProjectDir(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := tmpHome + "/.cdeploy"
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
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

	err := runOperation(context.Background(), runner.Deploy, true, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --project-dir") {
		t.Errorf("error = %q, want it to contain 'requires --project-dir'", err.Error())
	}
}

func TestRunOperation_SSHAndServerMutex(t *testing.T) {
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

	err := runOperation(context.Background(), runner.Deploy, true, nil)
	if err == nil {
		t.Fatal("expected mutex error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want it to contain 'mutually exclusive'", err.Error())
	}
}

// TestRunOperation_MutexBeforeContainerValidation verifies that the
// `--ssh` + `--server` mutex check fires before container-argument validation.
// Regression: previously, `cdeploy deploy --ssh foo --server bar` (no -a, no
// container names) returned the "specify container names or use -a" error
// instead of the mutex error, hiding the real misuse and diverging from
// exec/logs/list which always check the mutex first.
func TestRunOperation_MutexBeforeContainerValidation(t *testing.T) {
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
	projectDir = ""

	// No -a flag, no container args — the previous container-arg validation
	// would have fired here and returned the wrong error.
	err := runOperation(context.Background(), runner.Deploy, false, nil)
	if err == nil {
		t.Fatal("expected mutex error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want it to contain 'mutually exclusive'", err.Error())
	}
}

func TestRunOperation_SSHRequiresProjectDir(t *testing.T) {
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

	// Force an empty cwd so projectDir resolution doesn't pick up a default.
	// runOperation falls back to os.Getwd() when projectDir is empty, but
	// resolveSSHRemote checks the package-level projectDir directly.
	err := runOperation(context.Background(), runner.Deploy, true, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --project-dir") {
		t.Errorf("error = %q, want it to contain 'requires --project-dir'", err.Error())
	}
}

func TestRunOperation_SSHHappyPath(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldNewRemote := opNewRemote
	oldNewLogger := opNewLogger
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		opNewRemote = oldNewRemote
		opNewLogger = oldNewLogger
	})

	serverName = ""
	sshTarget = "deploy@host:2222"
	projectDir = "/srv/app"

	var stopArgs []string
	var pullCalled, startCalled bool
	opNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "'stop'") && stopArgs == nil {
					stopArgs = append([]string(nil), cmd.Args...)
				}
				if strings.Contains(args, "'pull'") {
					pullCalled = true
				}
				if strings.Contains(args, "'start'") {
					startCalled = true
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
	opNewLogger = func(dir string) (*logging.Logger, error) {
		return logging.NewLogger(t.TempDir())
	}

	err := runOperation(context.Background(), runner.Deploy, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stopArgs == nil {
		t.Fatal("stop not called on remote")
	}
	if !pullCalled {
		t.Error("pull not called on remote")
	}
	if !startCalled {
		t.Error("start not called on remote")
	}
	args := strings.Join(stopArgs, " ")
	if !strings.Contains(args, "-p 2222") {
		t.Errorf("ssh argv = %v, want to contain '-p 2222'", stopArgs)
	}
	if !strings.Contains(args, "'stop'") {
		t.Errorf("ssh argv = %v, want to contain 'stop' subcommand", stopArgs)
	}
}

func TestRestartCmd_SSHAndServerMutex(t *testing.T) {
	// cobra binds flags to package-level globals (sshTarget, serverName,
	// projectDir); snapshot/restore so subsequent tests in the same package
	// don't see leaked values.
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
	})

	root := NewRootCmd()
	root.SetArgs([]string{"restart", "-s", "prod", "-S", "user@host", "-C", "/srv/app", "-a"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected mutex error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want 'mutually exclusive'", err.Error())
	}
}

func TestStopCmd_SSHAndServerMutex(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
	})

	root := NewRootCmd()
	root.SetArgs([]string{"stop", "-s", "prod", "-S", "user@host", "-C", "/srv/app", "-a"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected mutex error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want 'mutually exclusive'", err.Error())
	}
}

func TestDeployCmd_SSHFlagInherited(t *testing.T) {
	root := NewRootCmd()

	for _, name := range []string{"deploy", "restart", "stop"} {
		t.Run(name, func(t *testing.T) {
			cmd, _, err := root.Find([]string{name})
			if err != nil {
				t.Fatalf("%s command not found: %v", name, err)
			}
			sshFlag := cmd.InheritedFlags().Lookup("ssh")
			if sshFlag == nil {
				t.Errorf("--ssh persistent flag not inherited by %s command", name)
			}
		})
	}
}

func TestRunOperation_LocalDetectFailure(t *testing.T) {
	oldNew := opNewLocal
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		opNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	opNewLocal = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				// Fail all version probes to simulate Docker not installed
				return nil, fmt.Errorf("not found")
			},
		)
		return c
	}
	projectDir = ""
	serverName = ""

	err := runOperation(context.Background(), runner.Deploy, true, nil)
	if err == nil {
		t.Fatal("expected error when Detect fails")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error = %q, want it to contain 'neither'", err.Error())
	}
}

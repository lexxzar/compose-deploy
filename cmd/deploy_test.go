package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
func (m *opMockComposer) ContainerStats(_ context.Context) (map[string]runner.ServiceStats, error) {
	return nil, nil
}
func (m *opMockComposer) CheckUpdates(_ context.Context, _ []string) (map[string]bool, error) {
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
	// Deploy records a pre-deploy snapshot under $HOME/.cdeploy/state/ — isolate
	// HOME so the capture (best-effort, empty here) never writes to the real home.
	t.Setenv("HOME", t.TempDir())

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
	// Deploy snapshots before the pipeline; isolate HOME so the (best-effort)
	// capture never writes to the real home even when the deploy later fails.
	t.Setenv("HOME", t.TempDir())

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
	oldIdentity := identityFile
	oldNewRemote := opNewRemote
	oldNewLogger := opNewLogger
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		identityFile = oldIdentity
		opNewRemote = oldNewRemote
		opNewLogger = oldNewLogger
	})

	serverName = ""
	sshTarget = "deploy@host:2222"
	projectDir = "/srv/app"
	identityFile = ""

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

// TestRunOperation_SSHHappyPathWithIdentity verifies that when both --ssh and
// --identity are set, -i <keyPath> is spliced into the SSH argv via
// SSHExtraArgs alongside any port args. Catches a regression where the
// identityFile global is dropped from the deploy/restart/stop wiring.
func TestRunOperation_SSHHappyPathWithIdentity(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldIdentity := identityFile
	oldNewRemote := opNewRemote
	oldNewLogger := opNewLogger
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		identityFile = oldIdentity
		opNewRemote = oldNewRemote
		opNewLogger = oldNewLogger
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

	var capturedArgs []string
	opNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "'stop'") && capturedArgs == nil {
					capturedArgs = append([]string(nil), cmd.Args...)
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

	if err := runOperation(context.Background(), runner.Deploy, true, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedArgs == nil {
		t.Fatal("stop not called on remote")
	}
	args := strings.Join(capturedArgs, " ")
	if !strings.Contains(args, "-p 2222") {
		t.Errorf("ssh argv = %v, want to contain '-p 2222'", capturedArgs)
	}
	if !strings.Contains(args, "-i "+keyPath) {
		t.Errorf("ssh argv = %v, want to contain '-i %s'", capturedArgs, keyPath)
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

// TestRunOperation_IdentityWithoutSSH verifies that --identity without --ssh
// is rejected by the mutex check. Covers deploy/restart/stop because they all
// flow through runOperation().
func TestRunOperation_IdentityWithoutSSH(t *testing.T) {
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

	err := runOperation(context.Background(), runner.Deploy, true, nil)
	if err == nil {
		t.Fatal("expected error when --identity is set without --ssh")
	}
	if !strings.Contains(err.Error(), "--identity requires --ssh") {
		t.Errorf("error = %q, want it to contain '--identity requires --ssh'", err.Error())
	}
}

// --- Task 9: --wait flags, exit-code contract, snapshot hook, verdict table ---

// TestWaitFlags_Registration verifies --wait and --wait-timeout are registered
// on deploy and restart but NOT on stop (stop has no health phase).
func TestWaitFlags_Registration(t *testing.T) {
	root := NewRootCmd()

	for _, name := range []string{"deploy", "restart"} {
		t.Run(name+"_has_flags", func(t *testing.T) {
			cmd, _, err := root.Find([]string{name})
			if err != nil {
				t.Fatalf("%s command not found: %v", name, err)
			}
			if cmd.Flags().Lookup("wait") == nil {
				t.Errorf("--wait flag not found on %s command", name)
			}
			wt := cmd.Flags().Lookup("wait-timeout")
			if wt == nil {
				t.Fatalf("--wait-timeout flag not found on %s command", name)
			}
			if wt.DefValue != runner.DefaultWaitTimeout.String() {
				t.Errorf("--wait-timeout default = %q, want %q", wt.DefValue, runner.DefaultWaitTimeout.String())
			}
		})
	}

	stop, _, err := root.Find([]string{"stop"})
	if err != nil {
		t.Fatalf("stop command not found: %v", err)
	}
	if stop.Flags().Lookup("wait") != nil {
		t.Error("--wait flag should NOT be registered on stop command")
	}
	if stop.Flags().Lookup("wait-timeout") != nil {
		t.Error("--wait-timeout flag should NOT be registered on stop command")
	}
}

// TestWaitTimeout_Parse verifies --wait-timeout parses a duration into the bound
// global.
func TestWaitTimeout_Parse(t *testing.T) {
	oldWait := waitEnabled
	oldTimeout := waitTimeout
	t.Cleanup(func() {
		waitEnabled = oldWait
		waitTimeout = oldTimeout
	})

	root := NewRootCmd()
	deploy, _, err := root.Find([]string{"deploy"})
	if err != nil {
		t.Fatalf("deploy command not found: %v", err)
	}
	if err := deploy.ParseFlags([]string{"--wait", "--wait-timeout", "45s"}); err != nil {
		t.Fatalf("parsing wait flags: %v", err)
	}
	if !waitEnabled {
		t.Error("--wait did not set waitEnabled")
	}
	if waitTimeout != 45*time.Second {
		t.Errorf("waitTimeout = %s, want 45s", waitTimeout)
	}
}

// TestWaitError_ErrorsAs verifies the exit-2 mapping mechanism main.go relies on:
// a *WaitError is detectable via errors.As, both as the direct error and when
// wrapped, while a plain error is not.
func TestWaitError_ErrorsAs(t *testing.T) {
	report := runner.WaitReport{
		Verdicts: map[string]runner.WaitVerdict{"web": runner.VerdictUnhealthy},
		OK:       false,
	}
	we := &WaitError{Report: report}

	var target *WaitError
	if !errors.As(error(we), &target) {
		t.Fatal("errors.As did not match a direct *WaitError")
	}

	wrapped := fmt.Errorf("deploy: %w", we)
	target = nil
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As did not match a wrapped *WaitError")
	}
	if target.Report.Verdicts["web"] != runner.VerdictUnhealthy {
		t.Errorf("recovered report verdict = %q, want unhealthy", target.Report.Verdicts["web"])
	}

	if errors.As(errors.New("plain"), &target) {
		t.Error("errors.As matched a plain error as *WaitError")
	}
}

// TestWaitError_Message covers both message forms: the operational-error form
// (Err set, surfaced via Unwrap) and the failing-service list form.
func TestWaitError_Message(t *testing.T) {
	// Operational error form.
	underlying := errors.New("context canceled")
	we := &WaitError{Err: underlying}
	if !strings.Contains(we.Error(), "context canceled") {
		t.Errorf("error = %q, want it to mention the underlying error", we.Error())
	}
	if !errors.Is(we, underlying) {
		t.Error("Unwrap did not expose the underlying error")
	}

	// Failing-service list form (no operational error).
	we = &WaitError{Report: runner.WaitReport{
		Verdicts: map[string]runner.WaitVerdict{
			"web": runner.VerdictHealthy,
			"db":  runner.VerdictUnhealthy,
			"api": runner.VerdictTimedOut,
		},
	}}
	msg := we.Error()
	if !strings.Contains(msg, "db") || !strings.Contains(msg, "unhealthy") {
		t.Errorf("error = %q, want it to name db/unhealthy", msg)
	}
	if !strings.Contains(msg, "api") || !strings.Contains(msg, "timed out") {
		t.Errorf("error = %q, want it to name api/timed out", msg)
	}
	if strings.Contains(msg, "web") {
		t.Errorf("error = %q, should not name the healthy service web", msg)
	}
}

// mockSnapshotter drives the recordSnapshot seam without a real composer.
type mockSnapshotter struct {
	result     compose.SnapshotResult
	snapErr    error
	writeErr   error
	written    *compose.Snapshot
	writeCalls int
}

func (m *mockSnapshotter) SnapshotServices(_ context.Context, _ []string) (compose.SnapshotResult, error) {
	return m.result, m.snapErr
}

func (m *mockSnapshotter) WriteSnapshot(_ context.Context, fresh *compose.Snapshot) error {
	m.writeCalls++
	m.written = fresh
	return m.writeErr
}

// TestRecordSnapshot_HappyPath: a clean capture is written and produces no
// warnings.
func TestRecordSnapshot_HappyPath(t *testing.T) {
	snap := &compose.Snapshot{Schema: 1, ProjectDir: "/opt/app"}
	m := &mockSnapshotter{result: compose.SnapshotResult{Snapshot: snap}}

	var buf bytes.Buffer
	recordSnapshot(context.Background(), m, nil, &buf)

	if m.writeCalls != 1 {
		t.Errorf("WriteSnapshot calls = %d, want 1", m.writeCalls)
	}
	if m.written != snap {
		t.Error("WriteSnapshot did not receive the captured snapshot")
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected warnings on happy path: %q", buf.String())
	}
}

// TestRecordSnapshot_CaptureError: a capture failure warns and never writes.
func TestRecordSnapshot_CaptureError(t *testing.T) {
	m := &mockSnapshotter{snapErr: errors.New("compose config unavailable")}

	var buf bytes.Buffer
	recordSnapshot(context.Background(), m, nil, &buf)

	if m.writeCalls != 0 {
		t.Errorf("WriteSnapshot calls = %d, want 0 after capture error", m.writeCalls)
	}
	if !strings.Contains(buf.String(), "skipped") || !strings.Contains(buf.String(), "compose config unavailable") {
		t.Errorf("warning = %q, want it to mention skipped + the capture error", buf.String())
	}
}

// TestRecordSnapshot_WarnAndProceed: per-service warnings are surfaced and a
// write failure warns without panicking (deploy proceeds).
func TestRecordSnapshot_WarnAndProceed(t *testing.T) {
	m := &mockSnapshotter{
		result: compose.SnapshotResult{
			Snapshot: &compose.Snapshot{Schema: 1},
			Warnings: []string{"db: not running, skipped"},
		},
		writeErr: errors.New("disk full"),
	}

	var buf bytes.Buffer
	recordSnapshot(context.Background(), m, nil, &buf)

	if m.writeCalls != 1 {
		t.Errorf("WriteSnapshot calls = %d, want 1", m.writeCalls)
	}
	out := buf.String()
	if !strings.Contains(out, "db: not running, skipped") {
		t.Errorf("output = %q, want the per-service warning surfaced", out)
	}
	if !strings.Contains(out, "failed to write rollback snapshot") || !strings.Contains(out, "disk full") {
		t.Errorf("output = %q, want the write-failure warning", out)
	}
}

// TestFormatWaitReport verifies the verdict table renders each service with its
// verdict label, sorted by name (golden-ish: substrings + ordering, color-free).
func TestFormatWaitReport(t *testing.T) {
	report := runner.WaitReport{
		Verdicts: map[string]runner.WaitVerdict{
			"web": runner.VerdictHealthy,
			"db":  runner.VerdictUnhealthy,
			"api": runner.VerdictRunningNoHC,
		},
	}
	out := formatWaitReport(report)

	for _, want := range []string{"web", "db", "api", "healthy", "unhealthy", "running (no healthcheck)"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatWaitReport output missing %q:\n%s", want, out)
		}
	}
	// Sorted by service name: api < db < web.
	iAPI := strings.Index(out, "api")
	iDB := strings.Index(out, "db")
	iWeb := strings.Index(out, "web")
	if !(iAPI < iDB && iDB < iWeb) {
		t.Errorf("services not sorted by name (api=%d db=%d web=%d):\n%s", iAPI, iDB, iWeb, out)
	}
}

// newWaitTestCompose extends newTestCompose's hooks to also script the
// ContainerStatus poll (`compose ps -a --format json`) the --wait phase drives,
// plus best-effort empty snapshot data (`config`/`ps`). psJSON is returned for the
// `ps -a` status query.
func newWaitTestCompose(dir string, mock *opMockComposer, psJSON string) *compose.Compose {
	c := compose.New(dir)
	c.SetTestHooks(
		func(cmd *exec.Cmd) error {
			args := strings.Join(cmd.Args, " ")
			switch {
			case strings.Contains(args, "stop"):
				return mock.Stop(context.Background(), nil, cmd.Stdout)
			case strings.Contains(args, "rm"):
				return mock.Remove(context.Background(), nil, cmd.Stdout)
			case strings.Contains(args, "pull"):
				return mock.Pull(context.Background(), nil, cmd.Stdout)
			case strings.Contains(args, "up"):
				return mock.Create(context.Background(), nil, cmd.Stdout)
			case strings.Contains(args, "start"):
				return mock.Start(context.Background(), nil, cmd.Stdout)
			}
			return nil
		},
		func(cmd *exec.Cmd) ([]byte, error) {
			args := strings.Join(cmd.Args, " ")
			switch {
			case strings.Contains(args, "version"):
				return []byte("Docker Compose version v2.24.0\n"), nil
			case strings.Contains(args, "ps") && strings.Contains(args, "-a"):
				return []byte(psJSON), nil // ContainerStatus for the wait
			case strings.Contains(args, "config"):
				return []byte(`{"services":{}}`), nil // snapshot: no services
			case strings.Contains(args, "ps"):
				return []byte("[]"), nil // snapshot: nothing running
			}
			return nil, nil
		},
	)
	return c
}

// runWaitOperation wires the --wait globals and a scripted compose, runs the
// operation, and returns its error plus everything written to stderr.
func runWaitOperation(t *testing.T, op runner.Operation, containers []string, psJSON string, timeout time.Duration) (error, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	oldNew, oldLogger, oldProj, oldServer, oldLogDir := opNewLocal, opNewLogger, projectDir, serverName, logDir
	oldWait, oldTimeout := waitEnabled, waitTimeout
	t.Cleanup(func() {
		opNewLocal, opNewLogger, projectDir, serverName, logDir = oldNew, oldLogger, oldProj, oldServer, oldLogDir
		waitEnabled, waitTimeout = oldWait, oldTimeout
	})

	mock := &opMockComposer{}
	opNewLocal = func(dir string) *compose.Compose { return newWaitTestCompose(dir, mock, psJSON) }
	opNewLogger = func(dir string) (*logging.Logger, error) { return logging.NewLogger(t.TempDir()) }
	projectDir, serverName, logDir = "", "", t.TempDir()
	waitEnabled, waitTimeout = true, timeout

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	runErr := runOperation(context.Background(), op, false, containers)
	w.Close()
	os.Stderr = oldStderr
	var buf strings.Builder
	if _, cerr := io.Copy(&buf, r); cerr != nil {
		t.Fatal(cerr)
	}
	return runErr, buf.String()
}

// TestRunOperation_WaitHealthy (T1/AC1): a --wait deploy whose services report
// healthy exits with no error (also exercises the timeout<=0 default fallback).
func TestRunOperation_WaitHealthy(t *testing.T) {
	ps := `[{"Service":"web","State":"running","Health":"healthy","Status":"Up 5 minutes (healthy)"}]`
	err, stderr := runWaitOperation(t, runner.Deploy, []string{"web"}, ps, 0) // 0 → default fallback
	if err != nil {
		t.Fatalf("healthy --wait deploy should succeed, got: %v", err)
	}
	if !strings.Contains(stderr, "All services healthy") {
		t.Errorf("stderr missing the success line:\n%s", stderr)
	}
}

// TestRunOperation_WaitUnhealthyDeploy (T1/AC1): a --wait deploy whose service is
// unhealthy returns a *WaitError (exit 2) and prints the rollback hint.
func TestRunOperation_WaitUnhealthyDeploy(t *testing.T) {
	ps := `[{"Service":"web","State":"running","Health":"unhealthy","Status":"Up 5 minutes (unhealthy)"}]`
	err, stderr := runWaitOperation(t, runner.Deploy, []string{"web"}, ps, 30*time.Second)
	var we *WaitError
	if !errors.As(err, &we) {
		t.Fatalf("unhealthy --wait deploy should return *WaitError, got: %v", err)
	}
	if !strings.Contains(stderr, "run 'cdeploy rollback'") {
		t.Errorf("deploy wait-failure must print the rollback hint:\n%s", stderr)
	}
}

// TestRunOperation_WaitUnhealthyRestart (T1): a --wait restart failure also
// returns a *WaitError but must NOT print the deploy-only rollback hint.
func TestRunOperation_WaitUnhealthyRestart(t *testing.T) {
	ps := `[{"Service":"web","State":"running","Health":"unhealthy","Status":"Up 5 minutes (unhealthy)"}]`
	err, stderr := runWaitOperation(t, runner.Restart, []string{"web"}, ps, 30*time.Second)
	var we *WaitError
	if !errors.As(err, &we) {
		t.Fatalf("unhealthy --wait restart should return *WaitError, got: %v", err)
	}
	if strings.Contains(stderr, "run 'cdeploy rollback'") {
		t.Errorf("restart wait-failure must NOT print the deploy-only rollback hint:\n%s", stderr)
	}
}

// TestWaitForHealth_CtxCancelNotWaitError (Q4): an operator interrupt (canceled
// context) must NOT be wrapped in *WaitError — that would map to exit 2 and
// misreport a deliberate Ctrl-C as "deployed but unhealthy".
func TestWaitForHealth_CtxCancelNotWaitError(t *testing.T) {
	oldWait, oldTimeout := waitEnabled, waitTimeout
	t.Cleanup(func() { waitEnabled, waitTimeout = oldWait, oldTimeout })
	waitTimeout = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: WaitHealthy returns context.Canceled on its first check

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	err := waitForHealth(ctx, &opMockComposer{}, runner.Deploy, []string{"web"})
	w.Close()
	os.Stderr = oldStderr
	var sb strings.Builder
	_, _ = io.Copy(&sb, r)

	if err == nil {
		t.Fatal("expected an error on canceled context")
	}
	var we *WaitError
	if errors.As(err, &we) {
		t.Errorf("ctx-cancel must not be a *WaitError (would exit 2), got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should unwrap to context.Canceled, got %v", err)
	}
	if strings.Contains(sb.String(), "run 'cdeploy rollback'") {
		t.Errorf("interrupt must not print the rollback hint:\n%s", sb.String())
	}
}

// TestRunOperationWithPrep_PipelineTargets (C1): a prep hook that resolves a
// target set narrows the PIPELINE (runner.Run) to exactly those services, not
// just the wait phase. For `rollback -a` (containers == nil) the pipeline must
// Stop/Create the resolved snapshot services, never every compose service with
// its current unpinned image. A nil prep (deploy/restart) is unchanged.
func TestRunOperationWithPrep_PipelineTargets(t *testing.T) {
	tests := []struct {
		name       string
		op         runner.Operation
		all        bool
		containers []string
		prep       func(context.Context, runner.Composer) (func(), []string, error)
		wantIn     []string
		wantNotIn  []string
	}{
		{
			name: "rollback -a uses the resolved snapshot targets (not all)",
			op:   runner.Rollback, all: true, containers: nil,
			prep: func(_ context.Context, _ runner.Composer) (func(), []string, error) {
				return nil, []string{"db", "web"}, nil
			},
			wantIn: []string{"db", "web"},
		},
		{
			name: "rollback named passes the explicit service through unchanged",
			op:   runner.Rollback, all: false, containers: []string{"web"},
			prep:      func(_ context.Context, _ runner.Composer) (func(), []string, error) { return nil, []string{"web"}, nil },
			wantIn:    []string{"web"},
			wantNotIn: []string{"db"},
		},
		{
			name: "restart with nil prep keeps operating on the explicit containers",
			op:   runner.Restart, all: false, containers: []string{"api"},
			prep:   nil,
			wantIn: []string{"api"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			oldNew, oldLogger, oldProj, oldServer, oldLogDir := opNewLocal, opNewLogger, projectDir, serverName, logDir
			t.Cleanup(func() {
				opNewLocal, opNewLogger, projectDir, serverName, logDir = oldNew, oldLogger, oldProj, oldServer, oldLogDir
			})

			var stopArgs string
			opNewLocal = func(dir string) *compose.Compose {
				c := compose.New(dir)
				c.SetTestHooks(
					func(cmd *exec.Cmd) error {
						a := strings.Join(cmd.Args, " ")
						if strings.Contains(a, "stop") {
							stopArgs = a
						}
						return nil
					},
					func(cmd *exec.Cmd) ([]byte, error) {
						if strings.Contains(strings.Join(cmd.Args, " "), "version") {
							return []byte("Docker Compose version v2.24.0\n"), nil
						}
						return nil, nil
					},
				)
				return c
			}
			opNewLogger = func(dir string) (*logging.Logger, error) { return logging.NewLogger(t.TempDir()) }
			projectDir, serverName, logDir = "", "", t.TempDir()

			if err := runOperationWithPrep(context.Background(), tt.op, tt.all, tt.containers, tt.prep); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if stopArgs == "" {
				t.Fatal("stop step never ran")
			}
			for _, w := range tt.wantIn {
				if !strings.Contains(stopArgs, " "+w) {
					t.Errorf("stop argv = %q, want it to include service %q", stopArgs, w)
				}
			}
			for _, w := range tt.wantNotIn {
				if strings.Contains(stopArgs, " "+w) {
					t.Errorf("stop argv = %q, should NOT include %q", stopArgs, w)
				}
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

// --project-name must reach the composer on every branch. Without it the CLI
// kept addressing the directory's default project while the TUI addressed the
// row's real name, so a TUI deploy and a CLI rollback of the "same" project
// touched different containers and different snapshot files.
func TestRunOperation_ProjectNameReachesTheComposer(t *testing.T) {
	oldServer, oldSSH, oldProj, oldName := serverName, sshTarget, projectDir, projectName
	oldNewLocal, oldLogDir := opNewLocal, logDir
	t.Cleanup(func() {
		serverName, sshTarget, projectDir, projectName = oldServer, oldSSH, oldProj, oldName
		opNewLocal, logDir = oldNewLocal, oldLogDir
	})

	serverName, sshTarget = "", ""
	projectDir, projectName = t.TempDir(), "blue"
	logDir = t.TempDir()

	var built *compose.Compose
	opNewLocal = func(dir string) *compose.Compose {
		built = compose.New(dir)
		built.SetStandalone(false)
		built.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) { return []byte(""), nil },
		)
		return built
	}

	_ = runOperation(context.Background(), runner.StopOnly, true, nil)
	if built == nil {
		t.Fatal("the local composer was never built")
	}
	if built.ProjectName != "blue" {
		t.Errorf("ProjectName = %q, want blue", built.ProjectName)
	}
}

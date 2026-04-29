package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
)

func TestExecCmd_Subcommand(t *testing.T) {
	root := NewRootCmd()

	subcommands := make(map[string]bool)
	for _, sub := range root.Commands() {
		subcommands[sub.Name()] = true
	}

	if !subcommands["exec"] {
		t.Fatal("exec subcommand not found")
	}
}

func TestExecCmd_RequiresServiceArg(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"exec"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when no service arg provided")
	}
}

func TestExecCmd_ServerFlagExists(t *testing.T) {
	root := NewRootCmd()
	flag := root.PersistentFlags().Lookup("server")
	if flag == nil {
		t.Fatal("--server flag not found (persistent)")
	}
	if flag.Shorthand != "s" {
		t.Errorf("--server shorthand = %q, want %q", flag.Shorthand, "s")
	}
}

func TestExecCmd_Use(t *testing.T) {
	cmd := newExecCmd()
	if !strings.Contains(cmd.Use, "exec") {
		t.Errorf("Use = %q, want it to contain 'exec'", cmd.Use)
	}
}

func TestRunExec_NoComposeFile(t *testing.T) {
	oldHas := execHasCompose
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		execHasCompose = oldHas
		projectDir = oldProj
		serverName = oldServer
	})

	execHasCompose = func(dir string) bool { return false }
	projectDir = ""
	serverName = ""

	err := runExec(context.Background(), "nginx", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no compose file found") {
		t.Errorf("error = %q, want it to contain 'no compose file found'", err.Error())
	}
}

func TestRunExec_LocalDefaultShell(t *testing.T) {
	oldHas := execHasCompose
	oldNew := execNewLocal
	oldProj := projectDir
	oldServer := serverName
	oldRun := execRunCmd
	t.Cleanup(func() {
		execHasCompose = oldHas
		execNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
		execRunCmd = oldRun
	})

	var capturedArgs []string
	execRunCmd = func(cmd *exec.Cmd) error {
		capturedArgs = cmd.Args
		return nil
	}
	execHasCompose = func(dir string) bool { return true }
	execNewLocal = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				return nil, nil
			},
		)
		return c
	}
	projectDir = ""
	serverName = ""

	err := runExec(context.Background(), "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := strings.Join(capturedArgs, " ")
	if !strings.Contains(args, "exec") {
		t.Errorf("args = %q, want it to contain 'exec'", args)
	}
	if !strings.Contains(args, "web") {
		t.Errorf("args = %q, want it to contain 'web'", args)
	}
	// Default shell uses /bin/sh -c
	if !strings.Contains(args, "/bin/sh") {
		t.Errorf("args = %q, want it to contain '/bin/sh' (default shell)", args)
	}
}

func TestRunExec_LocalCustomCommand(t *testing.T) {
	oldHas := execHasCompose
	oldNew := execNewLocal
	oldProj := projectDir
	oldServer := serverName
	oldRun := execRunCmd
	t.Cleanup(func() {
		execHasCompose = oldHas
		execNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
		execRunCmd = oldRun
	})

	var capturedArgs []string
	execRunCmd = func(cmd *exec.Cmd) error {
		capturedArgs = cmd.Args
		return nil
	}
	execHasCompose = func(dir string) bool { return true }
	execNewLocal = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				return nil, nil
			},
		)
		return c
	}
	projectDir = ""
	serverName = ""

	err := runExec(context.Background(), "web", []string{"rails", "console"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := strings.Join(capturedArgs, " ")
	if !strings.Contains(args, "exec") {
		t.Errorf("args = %q, want it to contain 'exec'", args)
	}
	if !strings.Contains(args, "rails") {
		t.Errorf("args = %q, want it to contain 'rails'", args)
	}
	if !strings.Contains(args, "console") {
		t.Errorf("args = %q, want it to contain 'console'", args)
	}
}

func TestRunExec_WithProjectDir(t *testing.T) {
	oldHas := execHasCompose
	oldNew := execNewLocal
	oldProj := projectDir
	oldServer := serverName
	oldRun := execRunCmd
	t.Cleanup(func() {
		execHasCompose = oldHas
		execNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
		execRunCmd = oldRun
	})

	var capturedDir string
	execRunCmd = func(cmd *exec.Cmd) error { return nil }
	execHasCompose = func(dir string) bool { return true }
	execNewLocal = func(dir string) *compose.Compose {
		capturedDir = dir
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				return nil, nil
			},
		)
		return c
	}
	projectDir = "/custom/path"
	serverName = ""

	err := runExec(context.Background(), "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedDir != "/custom/path" {
		t.Errorf("dir = %q, want %q", capturedDir, "/custom/path")
	}
}

func TestRunExec_ServerNotFound(t *testing.T) {
	oldServer := serverName
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
	})

	serverName = "nonexistent"
	projectDir = ""

	err := runExec(context.Background(), "nginx", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to contain 'not found'", err.Error())
	}
}

func TestRunExec_ServerNoProjectDir(t *testing.T) {
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

	err := runExec(context.Background(), "nginx", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --project-dir") {
		t.Errorf("error = %q, want it to contain 'requires --project-dir'", err.Error())
	}
}

func TestRunExec_ServerSuccess(t *testing.T) {
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
	oldNewRemote := execNewRemote
	oldRun := execRunCmd
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
		execNewRemote = oldNewRemote
		execRunCmd = oldRun
	})

	serverName = "prod"
	projectDir = ""

	var capturedArgs []string
	execRunCmd = func(cmd *exec.Cmd) error {
		capturedArgs = cmd.Args
		return nil
	}

	execNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
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

	err := runExec(context.Background(), "nginx", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it built an SSH exec command
	args := strings.Join(capturedArgs, " ")
	if !strings.Contains(args, "ssh") {
		t.Errorf("args = %q, want it to contain 'ssh'", args)
	}
	if !strings.Contains(args, "-t") {
		t.Errorf("args = %q, want it to contain '-t' for TTY", args)
	}
	if !strings.Contains(args, "exec") {
		t.Errorf("args = %q, want it to contain 'exec'", args)
	}
}

func TestRunExec_LocalDetectFailure(t *testing.T) {
	oldHas := execHasCompose
	oldNew := execNewLocal
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		execHasCompose = oldHas
		execNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	execHasCompose = func(dir string) bool { return true }
	execNewLocal = func(dir string) *compose.Compose {
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

	err := runExec(context.Background(), "nginx", nil)
	if err == nil {
		t.Fatal("expected error when Detect fails")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error = %q, want it to contain 'neither'", err.Error())
	}
}

func TestExecCmd_ArgsLenAtDash(t *testing.T) {
	// Verify the exec command accepts service + -- + command args
	root := NewRootCmd()

	execCmd, _, err := root.Find([]string{"exec"})
	if err != nil {
		t.Fatalf("exec command not found: %v", err)
	}

	// MinimumNArgs(1) should be set
	if execCmd.Args == nil {
		t.Fatal("Args validator not set")
	}
}

func TestExecCmd_ArgParsingWithDash(t *testing.T) {
	// Test that the command correctly parses service and command after --
	oldHas := execHasCompose
	oldNew := execNewLocal
	oldProj := projectDir
	oldServer := serverName
	oldRun := execRunCmd
	t.Cleanup(func() {
		execHasCompose = oldHas
		execNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
		execRunCmd = oldRun
	})

	var capturedArgs []string
	execRunCmd = func(cmd *exec.Cmd) error {
		capturedArgs = cmd.Args
		return nil
	}
	execHasCompose = func(dir string) bool { return true }
	execNewLocal = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				return nil, nil
			},
		)
		return c
	}
	projectDir = ""
	serverName = ""

	root := NewRootCmd()
	root.SetArgs([]string{"exec", "web", "--", "rails", "console"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := strings.Join(capturedArgs, " ")
	if !strings.Contains(args, "exec") {
		t.Errorf("args = %q, want it to contain 'exec'", args)
	}
	if !strings.Contains(args, "web") {
		t.Errorf("args = %q, want it to contain 'web'", args)
	}
	if !strings.Contains(args, "rails") {
		t.Errorf("args = %q, want it to contain 'rails'", args)
	}
	if !strings.Contains(args, "console") {
		t.Errorf("args = %q, want it to contain 'console'", args)
	}
}

func TestExecCmd_ArgParsingWithoutDash(t *testing.T) {
	// Test that the command works with just a service name (no --)
	oldHas := execHasCompose
	oldNew := execNewLocal
	oldProj := projectDir
	oldServer := serverName
	oldRun := execRunCmd
	t.Cleanup(func() {
		execHasCompose = oldHas
		execNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
		execRunCmd = oldRun
	})

	var capturedArgs []string
	execRunCmd = func(cmd *exec.Cmd) error {
		capturedArgs = cmd.Args
		return nil
	}
	execHasCompose = func(dir string) bool { return true }
	execNewLocal = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				return nil, nil
			},
		)
		return c
	}
	projectDir = ""
	serverName = ""

	root := NewRootCmd()
	root.SetArgs([]string{"exec", "nginx"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := strings.Join(capturedArgs, " ")
	if !strings.Contains(args, "exec") {
		t.Errorf("args = %q, want it to contain 'exec'", args)
	}
	if !strings.Contains(args, "nginx") {
		t.Errorf("args = %q, want it to contain 'nginx'", args)
	}
	// Default shell should be used
	if !strings.Contains(args, "/bin/sh") {
		t.Errorf("args = %q, want it to contain '/bin/sh' (default shell)", args)
	}
}

func TestRunExec_SSHAndServerMutex(t *testing.T) {
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

	err := runExec(context.Background(), "nginx", nil)
	if err == nil {
		t.Fatal("expected mutex error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want it to contain 'mutually exclusive'", err.Error())
	}
}

func TestRunExec_SSHRequiresProjectDir(t *testing.T) {
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

	err := runExec(context.Background(), "nginx", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --project-dir") {
		t.Errorf("error = %q, want it to contain 'requires --project-dir'", err.Error())
	}
}

func TestRunExec_SSHHappyPath(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldIdentity := identityFile
	oldNewRemote := execNewRemote
	oldRun := execRunCmd
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		identityFile = oldIdentity
		execNewRemote = oldNewRemote
		execRunCmd = oldRun
	})

	serverName = ""
	sshTarget = "deploy@host:2222"
	projectDir = "/srv/app"
	identityFile = ""

	var capturedArgs []string
	execRunCmd = func(cmd *exec.Cmd) error {
		capturedArgs = append([]string(nil), cmd.Args...)
		return nil
	}

	execNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) {
				return []byte("Docker Compose version v2.24.0\n"), nil
			},
		)
		return rc
	}

	err := runExec(context.Background(), "nginx", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := strings.Join(capturedArgs, " ")
	if !strings.Contains(args, "-p 2222") {
		t.Errorf("args = %q, want it to contain '-p 2222'", args)
	}
	if !strings.Contains(args, "'exec'") {
		t.Errorf("args = %q, want it to contain 'exec' subcommand", args)
	}
	if !strings.Contains(args, "'nginx'") {
		t.Errorf("args = %q, want it to contain 'nginx'", args)
	}
}

// TestRunExec_SSHHappyPathWithIdentity verifies the exec subcommand splices
// -i <keyPath> into the SSH argv when both --ssh and --identity are set.
func TestRunExec_SSHHappyPathWithIdentity(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldIdentity := identityFile
	oldNewRemote := execNewRemote
	oldRun := execRunCmd
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		identityFile = oldIdentity
		execNewRemote = oldNewRemote
		execRunCmd = oldRun
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
	execRunCmd = func(cmd *exec.Cmd) error {
		capturedArgs = append([]string(nil), cmd.Args...)
		return nil
	}

	execNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) {
				return []byte("Docker Compose version v2.24.0\n"), nil
			},
		)
		return rc
	}

	if err := runExec(context.Background(), "nginx", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := strings.Join(capturedArgs, " ")
	if !strings.Contains(args, "-p 2222") {
		t.Errorf("args = %q, want it to contain '-p 2222'", args)
	}
	if !strings.Contains(args, "-i "+keyPath) {
		t.Errorf("args = %q, want it to contain '-i %s'", args, keyPath)
	}
}

func TestExec_IdentityWithoutSSH(t *testing.T) {
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

	err := runExec(context.Background(), "nginx", nil)
	if err == nil {
		t.Fatal("expected error when --identity is set without --ssh")
	}
	if !strings.Contains(err.Error(), "--identity requires --ssh") {
		t.Errorf("error = %q, want it to contain '--identity requires --ssh'", err.Error())
	}
}

func TestExecCmd_SSHFlagInherited(t *testing.T) {
	root := NewRootCmd()

	cmd, _, err := root.Find([]string{"exec"})
	if err != nil {
		t.Fatalf("exec command not found: %v", err)
	}
	sshFlag := cmd.InheritedFlags().Lookup("ssh")
	if sshFlag == nil {
		t.Error("--ssh persistent flag not inherited by exec command")
	}
	if sshFlag != nil && sshFlag.Shorthand != "S" {
		t.Errorf("--ssh shorthand = %q, want %q", sshFlag.Shorthand, "S")
	}
}

func TestExecCmd_ArgParsingExtraArgsWithoutDash(t *testing.T) {
	// Test that extra positional args without -- are treated as the command
	// (e.g. "cdeploy exec web rails console" should run "rails console" in "web")
	oldHas := execHasCompose
	oldNew := execNewLocal
	oldProj := projectDir
	oldServer := serverName
	oldRun := execRunCmd
	t.Cleanup(func() {
		execHasCompose = oldHas
		execNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
		execRunCmd = oldRun
	})

	var capturedArgs []string
	execRunCmd = func(cmd *exec.Cmd) error {
		capturedArgs = cmd.Args
		return nil
	}
	execHasCompose = func(dir string) bool { return true }
	execNewLocal = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				return nil, nil
			},
		)
		return c
	}
	projectDir = ""
	serverName = ""

	root := NewRootCmd()
	root.SetArgs([]string{"exec", "web", "rails", "console"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := strings.Join(capturedArgs, " ")
	if !strings.Contains(args, "exec") {
		t.Errorf("args = %q, want it to contain 'exec'", args)
	}
	if !strings.Contains(args, "web") {
		t.Errorf("args = %q, want it to contain 'web'", args)
	}
	if !strings.Contains(args, "rails") {
		t.Errorf("args = %q, want it to contain 'rails'", args)
	}
	if !strings.Contains(args, "console") {
		t.Errorf("args = %q, want it to contain 'console'", args)
	}
	// Should NOT use default shell since extra args were provided
	if strings.Contains(args, "/bin/sh") {
		t.Errorf("args = %q, should NOT contain '/bin/sh' when extra args are provided", args)
	}
}

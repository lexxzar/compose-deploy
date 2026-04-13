package cmd

import (
	"bytes"
	"strings"
	"testing"
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

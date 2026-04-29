package cmd

import (
	"strings"
	"testing"
)

func TestRootCmd_FlagRegistration(t *testing.T) {
	cmd := NewRootCmd()

	tests := []struct {
		name      string
		flagName  string
		shorthand string
	}{
		{"log-dir flag exists", "log-dir", ""},
		{"project-dir flag exists", "project-dir", "C"},
		{"server flag exists", "server", "s"},
		{"ssh flag exists", "ssh", "S"},
		{"identity flag exists", "identity", "i"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flag := cmd.PersistentFlags().Lookup(tt.flagName)
			if flag == nil {
				t.Fatalf("flag %q not found", tt.flagName)
			}
			if tt.shorthand != "" && flag.Shorthand != tt.shorthand {
				t.Errorf("flag %q shorthand = %q, want %q", tt.flagName, flag.Shorthand, tt.shorthand)
			}
		})
	}
}

func TestRootCmd_IdentityFlagDetails(t *testing.T) {
	cmd := NewRootCmd()
	flag := cmd.PersistentFlags().Lookup("identity")
	if flag == nil {
		t.Fatal("identity flag not found")
	}
	if flag.Shorthand != "i" {
		t.Errorf("identity shorthand = %q, want %q", flag.Shorthand, "i")
	}
	if flag.DefValue != "" {
		t.Errorf("identity default = %q, want empty", flag.DefValue)
	}
	if flag.Value.Type() != "string" {
		t.Errorf("identity flag type = %q, want %q", flag.Value.Type(), "string")
	}
	if !strings.Contains(flag.Usage, "SSH private key") {
		t.Errorf("identity usage missing 'SSH private key': %q", flag.Usage)
	}
	if !strings.Contains(flag.Usage, "--ssh") {
		t.Errorf("identity usage missing '--ssh' reference: %q", flag.Usage)
	}
}

func TestRootCmd_IdentityRejectedInTUI(t *testing.T) {
	// reset globals on exit to avoid state leakage between tests
	t.Cleanup(func() {
		identityFile = ""
		sshTarget = ""
		serverName = ""
		projectDir = ""
		logDir = ""
	})
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"--identity", "/tmp/x"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --identity is used without a subcommand")
	}
	if !strings.Contains(err.Error(), "--identity is not valid for the interactive TUI") {
		t.Errorf("error = %q, want substring %q", err.Error(), "--identity is not valid for the interactive TUI")
	}
}

func TestRootCmd_FlagDefaults(t *testing.T) {
	cmd := NewRootCmd()

	logDirFlag := cmd.PersistentFlags().Lookup("log-dir")
	if logDirFlag.DefValue != "" {
		t.Errorf("log-dir default = %q, want empty", logDirFlag.DefValue)
	}

	projectDirFlag := cmd.PersistentFlags().Lookup("project-dir")
	if projectDirFlag.DefValue != "" {
		t.Errorf("project-dir default = %q, want empty", projectDirFlag.DefValue)
	}
}

func TestRootCmd_Subcommands(t *testing.T) {
	cmd := NewRootCmd()

	subcommands := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subcommands[sub.Name()] = true
	}

	for _, name := range []string{"deploy", "restart", "stop", "list", "logs", "exec"} {
		if !subcommands[name] {
			t.Errorf("subcommand %q not found", name)
		}
	}
}

func TestLogsCmd_Flags(t *testing.T) {
	cmd := newLogsCmd()

	tailFlag := cmd.Flags().Lookup("tail")
	if tailFlag == nil {
		t.Fatal("tail flag not found")
	}
	if tailFlag.Shorthand != "n" {
		t.Errorf("tail shorthand = %q, want %q", tailFlag.Shorthand, "n")
	}
	if tailFlag.DefValue != "50" {
		t.Errorf("tail default = %q, want %q", tailFlag.DefValue, "50")
	}

	noFollowFlag := cmd.Flags().Lookup("no-follow")
	if noFollowFlag == nil {
		t.Fatal("no-follow flag not found")
	}
	if noFollowFlag.DefValue != "false" {
		t.Errorf("no-follow default = %q, want %q", noFollowFlag.DefValue, "false")
	}
}

func TestLogsCmd_RequiresServiceArg(t *testing.T) {
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"logs"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no service arg provided")
	}
}

func TestLogsCmd_RejectsMultipleArgs(t *testing.T) {
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"logs", "nginx", "postgres"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when multiple service args provided")
	}
}

package cmd

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/tui"
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

func TestRootCmd_FlagRejectedInTUI(t *testing.T) {
	// snapshot/restore globals (cobra binds flags to them) so subsequent
	// tests don't see leaked values when -count >1 or -shuffle is used.
	oldIdentity := identityFile
	oldSSH := sshTarget
	oldServer := serverName
	oldProj := projectDir
	oldLogDir := logDir
	t.Cleanup(func() {
		identityFile = oldIdentity
		sshTarget = oldSSH
		serverName = oldServer
		projectDir = oldProj
		logDir = oldLogDir
	})

	tests := []struct {
		name       string
		args       []string
		wantSubstr string
	}{
		{
			name:       "identity rejected",
			args:       []string{"--identity", "/tmp/x"},
			wantSubstr: "--identity is not valid for the interactive TUI",
		},
		{
			name:       "ssh rejected",
			args:       []string{"--ssh", "user@host"},
			wantSubstr: "--ssh is not valid for the interactive TUI",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewRootCmd()
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("expected error for args %v", tt.args)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantSubstr)
			}
		})
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

	for _, name := range []string{"deploy", "restart", "stop", "list", "logs", "exec", "skill"} {
		if !subcommands[name] {
			t.Errorf("subcommand %q not found", name)
		}
	}
}

// TestSkillCmd_Registration pins that the `skill` command is registered and that
// its `install` verb exists. (`show`/`uninstall` are added by a later task; this
// asserts only what exists now.)
func TestSkillCmd_Registration(t *testing.T) {
	root := NewRootCmd()

	var skillFound, installFound bool
	for _, sub := range root.Commands() {
		if sub.Name() != "skill" {
			continue
		}
		skillFound = true
		for _, verb := range sub.Commands() {
			if verb.Name() == "install" {
				installFound = true
			}
		}
	}
	if !skillFound {
		t.Fatal("skill command not registered")
	}
	if !installFound {
		t.Error("skill install verb not found")
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

// --- TUI wiring helpers (the closures in NewRootCmd's RunE have no test seam:
// the root command needs a TTY and is never executed, so the branch bodies live
// at package level and are pinned here). ---

// fakeProjectLister is the ListProjects half of a composer.
type fakeProjectLister struct {
	projects []compose.Project
	err      error
}

func (f fakeProjectLister) ListProjects(ctx context.Context) ([]compose.Project, error) {
	return f.projects, f.err
}

// hostPsForWiring is one compose-managed and two hand-started containers, in the
// NDJSON shape `docker ps -a --format '{{json .}}'` emits.
const hostPsForWiring = `{"ID":"aaa111222333","Names":"web","Image":"nginx:1.27","State":"running","Status":"Up 3 hours","Labels":"com.docker.compose.project=my-app"}
{"ID":"bbb444555666","Names":"watchtower","Image":"containrrr/watchtower","State":"running","Status":"Up 2 days","Labels":""}
{"ID":"ccc777888999","Names":"pg-scratch","Image":"postgres:16","State":"exited","Status":"Exited (0) 4 hours ago","Labels":""}`

func TestProjectsWithUnmanaged_AppendsRow(t *testing.T) {
	c := compose.New(t.TempDir())
	c.SetTestHooks(nil, func(*exec.Cmd) ([]byte, error) { return []byte(hostPsForWiring), nil })

	lister := fakeProjectLister{projects: []compose.Project{{Name: "my-app", ConfigDir: "/srv/my-app"}}}
	got, err := projectsWithUnmanaged(context.Background(), lister, compose.NewLocalHostContainers(c))
	if err != nil {
		t.Fatalf("projectsWithUnmanaged() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d projects, want 2: %+v", len(got), got)
	}
	if got[0].Name != "my-app" {
		t.Errorf("first row = %q, want the compose project", got[0].Name)
	}
	last := got[len(got)-1]
	if last.Name != compose.UnmanagedProjectName || !last.Unmanaged {
		t.Fatalf("last row = %+v, want the synthetic unmanaged row", last)
	}
	if last.Desc != "2 containers" {
		t.Errorf("unmanaged row desc = %q, want %q", last.Desc, "2 containers")
	}
}

func TestProjectsWithUnmanaged_NoRowWhenHostIsClean(t *testing.T) {
	c := compose.New(t.TempDir())
	c.SetTestHooks(nil, func(*exec.Cmd) ([]byte, error) {
		return []byte(`{"ID":"aaa111222333","Names":"web","Image":"nginx","State":"running","Status":"Up 1 hour","Labels":"com.docker.compose.project=my-app"}`), nil
	})

	lister := fakeProjectLister{projects: []compose.Project{{Name: "my-app"}}}
	got, err := projectsWithUnmanaged(context.Background(), lister, compose.NewLocalHostContainers(c))
	if err != nil {
		t.Fatalf("projectsWithUnmanaged() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d projects, want only the compose project: %+v", len(got), got)
	}
}

func TestProjectsWithUnmanaged_ListErrorPropagates(t *testing.T) {
	lister := fakeProjectLister{err: errors.New("docker compose ls failed")}
	if _, err := projectsWithUnmanaged(context.Background(), lister, nil); err == nil {
		t.Fatal("expected the ListProjects error to propagate")
	}
}

func TestLocalComposerFor(t *testing.T) {
	detector := compose.New(t.TempDir())
	detector.SetStandalone(true)

	t.Run("unmanaged row gets the read-only host composer", func(t *testing.T) {
		got := localComposerFor(compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true}, detector, true)
		if _, ok := got.(*compose.HostContainers); !ok {
			t.Fatalf("got %T, want *compose.HostContainers", got)
		}
		// The grouped host view addresses the host-wide seam through exactly
		// this branch — composerFactory(compose.Project{Unmanaged: true}) — so
		// the wiring is only correct if what comes back is a tui.HostGrouper.
		if _, ok := got.(tui.HostGrouper); !ok {
			t.Errorf("got %T, which is no tui.HostGrouper; the grouped screen would render no status", got)
		}
	})

	t.Run("compose row gets a Compose rooted at its config dir", func(t *testing.T) {
		got := localComposerFor(compose.Project{Name: "my-app", ConfigDir: "/srv/my-app"}, detector, true)
		lc, ok := got.(*compose.Compose)
		if !ok {
			t.Fatalf("got %T, want *compose.Compose", got)
		}
		if lc.ProjectDir != "/srv/my-app" {
			t.Errorf("ProjectDir = %q, want %q", lc.ProjectDir, "/srv/my-app")
		}
		if !lc.Standalone {
			t.Error("a detected standalone verdict must be inherited")
		}
	})

	t.Run("undetected local docker does not inherit a verdict", func(t *testing.T) {
		got := localComposerFor(compose.Project{Name: "my-app", ConfigDir: "/srv/my-app"}, detector, false)
		lc := got.(*compose.Compose)
		if lc.Standalone {
			t.Error("Standalone must stay false when detection never ran")
		}
	})
}

func TestRemoteComposerFor(t *testing.T) {
	rc := compose.NewRemote("prod.example.com", "/srv/base")
	rc.SetStandalone(true)

	t.Run("unmanaged row reuses the live RemoteCompose", func(t *testing.T) {
		got := remoteComposerFor(compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true}, rc)
		if _, ok := got.(*compose.HostContainers); !ok {
			t.Fatalf("got %T, want *compose.HostContainers", got)
		}
		if _, ok := got.(tui.HostGrouper); !ok {
			t.Errorf("got %T, which is no tui.HostGrouper; the remote grouped screen would render no status", got)
		}
	})

	t.Run("compose row gets a RemoteCompose for its dir", func(t *testing.T) {
		got := remoteComposerFor(compose.Project{Name: "my-app", ConfigDir: "/srv/my-app"}, rc)
		newRC, ok := got.(*compose.RemoteCompose)
		if !ok {
			t.Fatalf("got %T, want *compose.RemoteCompose", got)
		}
		if newRC.Host != "prod.example.com" {
			t.Errorf("Host = %q, want %q", newRC.Host, "prod.example.com")
		}
		if newRC.ProjectDir != "/srv/my-app" {
			t.Errorf("ProjectDir = %q, want %q", newRC.ProjectDir, "/srv/my-app")
		}
		if !newRC.Standalone {
			t.Error("the detected standalone verdict must be inherited")
		}
	})
}

package cmd

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestLocalComposerFor(t *testing.T) {
	detector := compose.New(t.TempDir())
	detector.SetStandalone(true)

	t.Run("unmanaged row gets the read-only host composer", func(t *testing.T) {
		got := localComposerFor(compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true}, detector, true, true)
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
		got := localComposerFor(compose.Project{Name: "my-app", ConfigDir: "/srv/my-app"}, detector, true, true)
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
		// The NAME as well as the directory. `docker compose ls` reports a
		// project launched with `-p` (or COMPOSE_PROJECT_NAME) under its real
		// name while several such projects share one ConfigDir; a composer
		// built from the directory alone would address the directory's default
		// project and stop/remove another project's containers.
		if lc.ProjectName != "my-app" {
			t.Errorf("ProjectName = %q, want %q", lc.ProjectName, "my-app")
		}
	})

	t.Run("two -p projects in one dir get two composers", func(t *testing.T) {
		blue := localComposerFor(compose.Project{Name: "blue", ConfigDir: "/srv/app"}, detector, true, true).(*compose.Compose)
		green := localComposerFor(compose.Project{Name: "green", ConfigDir: "/srv/app"}, detector, true, true).(*compose.Compose)
		if blue.ProjectName == green.ProjectName {
			t.Fatalf("both composers name project %q; one project's keys would target the other", blue.ProjectName)
		}
	})

	t.Run("undetected local docker does not inherit a verdict", func(t *testing.T) {
		got := localComposerFor(compose.Project{Name: "my-app", ConfigDir: "/srv/my-app"}, detector, true, false)
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
		got := remoteComposerFor(compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true}, rc, true)
		if _, ok := got.(*compose.HostContainers); !ok {
			t.Fatalf("got %T, want *compose.HostContainers", got)
		}
		if _, ok := got.(tui.HostGrouper); !ok {
			t.Errorf("got %T, which is no tui.HostGrouper; the remote grouped screen would render no status", got)
		}
	})

	t.Run("compose row gets a RemoteCompose for its dir", func(t *testing.T) {
		got := remoteComposerFor(compose.Project{Name: "my-app", ConfigDir: "/srv/my-app"}, rc, true)
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
		if newRC.ProjectName != "my-app" {
			t.Errorf("ProjectName = %q, want %q", newRC.ProjectName, "my-app")
		}
	})

	t.Run("two -p projects in one dir get two composers", func(t *testing.T) {
		blue := remoteComposerFor(compose.Project{Name: "blue", ConfigDir: "/srv/app"}, rc, true).(*compose.RemoteCompose)
		green := remoteComposerFor(compose.Project{Name: "green", ConfigDir: "/srv/app"}, rc, true).(*compose.RemoteCompose)
		if blue.ProjectName == green.ProjectName {
			t.Fatalf("both composers name project %q; one project's keys would target the other", blue.ProjectName)
		}
	})
}

// --- detectGuard: the plugin/standalone probe is shared between the
// ProjectLoader goroutine (which runs it) and the ComposerFactory on the UI
// goroutine (which reads its verdict). ---

// TestDetectGuard_ProbesOnceAndRecordsTheVerdict pins the once-only contract and
// the copy-out: the factory must never read the field Detect writes.
func TestDetectGuard_ProbesOnceAndRecordsTheVerdict(t *testing.T) {
	var d detectGuard
	if standalone, detected := d.verdict(); standalone || detected {
		t.Fatalf("zero value = (%v,%v), want (false,false)", standalone, detected)
	}

	probes := 0
	run := func() error { probes++; return nil }
	if err := d.resolve(run, func() bool { return true }); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := d.resolve(run, func() bool { return false }); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if probes != 1 {
		t.Errorf("probe ran %d times, want 1", probes)
	}
	standalone, detected := d.verdict()
	if !standalone || !detected {
		t.Errorf("verdict = (%v,%v), want (true,true)", standalone, detected)
	}
}

// A failed probe records nothing, so the next caller retries — that is what
// keeps a TUI started without local Docker usable if Docker appears later.
func TestDetectGuard_FailureRetries(t *testing.T) {
	var d detectGuard
	boom := errors.New("docker not found")
	if err := d.resolve(func() error { return boom }, func() bool { return true }); !errors.Is(err, boom) {
		t.Fatalf("resolve err = %v, want %v", err, boom)
	}
	if _, detected := d.verdict(); detected {
		t.Fatal("a failed probe must not record a verdict")
	}
	probes := 0
	if err := d.resolve(func() error { probes++; return nil }, func() bool { return true }); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if probes != 1 {
		t.Errorf("the retry ran the probe %d times, want 1", probes)
	}
}

// TestDetectGuard_VerdictNeverBlocksOnAProbe pins the two-mutex split. verdict()
// runs on the Bubble Tea UI goroutine, inside Update (hostGrouper,
// bindProjComposer, drillIntoGroup), while resolve() runs a subprocess exec or —
// remotely — an SSH round-trip under a context with no deadline. One mutex
// covering both would put that call inside the lock the UI takes, so a hung
// probe (a dead ControlMaster socket falling back to a direct connect) would
// freeze the whole TUI, ctrl+c included.
func TestDetectGuard_VerdictNeverBlocksOnAProbe(t *testing.T) {
	var d detectGuard
	probing := make(chan struct{})
	release := make(chan struct{})
	probed := make(chan struct{})
	go func() {
		defer close(probed)
		_ = d.resolve(func() error {
			close(probing)
			<-release
			return nil
		}, func() bool { return true })
	}()
	<-probing

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.verdict()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("verdict blocked while a probe was in flight; the probe must not run under the verdict lock")
	}
	close(release)
	<-probed

	if standalone, detected := d.verdict(); !standalone || !detected {
		t.Errorf("verdict = (%v,%v), want (true,true)", standalone, detected)
	}
}

// The probe stays serialised even though it no longer runs under the verdict
// lock: probeMu plus the re-check behind it is what keeps exactly one Detect()
// writing Compose.Standalone when several loaders race.
func TestDetectGuard_ConcurrentResolveProbesOnce(t *testing.T) {
	var d detectGuard
	var probes int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = d.resolve(func() error {
				atomic.AddInt32(&probes, 1)
				time.Sleep(10 * time.Millisecond)
				return nil
			}, func() bool { return true })
		}()
	}
	close(start)
	wg.Wait()

	if n := atomic.LoadInt32(&probes); n != 1 {
		t.Errorf("probe ran %d times, want 1", n)
	}
	if standalone, detected := d.verdict(); !standalone || !detected {
		t.Errorf("verdict = (%v,%v), want (true,true)", standalone, detected)
	}
}

// TestDetectGuard_ConcurrentResolveAndVerdict is the race pin. In the grouped
// host view the loader runs on the 5-second tick and the factory on every
// action key, so the two are live together for the whole session; without the
// mutex `go test -race` flags the unsynchronised bool pair.
func TestDetectGuard_ConcurrentResolveAndVerdict(t *testing.T) {
	var d detectGuard
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.resolve(func() error { return nil }, func() bool { return true })
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.verdict()
		}()
	}
	wg.Wait()
	if standalone, detected := d.verdict(); !standalone || !detected {
		t.Errorf("verdict = (%v,%v), want (true,true)", standalone, detected)
	}
}

// -p pins WHICH project; the reported file set pins WHAT that project is made
// of. A project created from `-f prod.yml` in a directory that also holds a
// docker-compose.yml was recreated from the wrong service definitions under the
// right label, and a stack.yml-only project failed every file read.
func TestComposerFor_CarriesTheReportedFileSet(t *testing.T) {
	proj := compose.Project{
		Name:        "prod",
		ConfigDir:   "/srv/app",
		ConfigFiles: []string{"/srv/app/prod.yml", "/srv/app/extra.yml"},
	}

	lc, ok := localComposerFor(proj, compose.New(t.TempDir()), false, false).(*compose.Compose)
	if !ok {
		t.Fatal("local factory did not return a *compose.Compose")
	}
	if !slices.Equal(lc.ComposeFiles, proj.ConfigFiles) {
		t.Errorf("local ComposeFiles = %v, want %v", lc.ComposeFiles, proj.ConfigFiles)
	}

	rc := compose.NewRemote("prod.example.com", "/srv/base")
	rc.SSHExtraArgs = []string{"-p", "2222", "-i", "/tmp/key"}
	rc.SetStandalone(false)
	newRC, ok := remoteComposerFor(proj, rc, false).(*compose.RemoteCompose)
	if !ok {
		t.Fatal("remote factory did not return a *compose.RemoteCompose")
	}
	if !slices.Equal(newRC.ComposeFiles, proj.ConfigFiles) {
		t.Errorf("remote ComposeFiles = %v, want %v", newRC.ComposeFiles, proj.ConfigFiles)
	}
	// SSHExtraArgs carries the port and the identity that reach the host at
	// all; a composer built without them would dial a different endpoint than
	// the connection it was derived from.
	if !slices.Equal(newRC.SSHExtraArgs, rc.SSHExtraArgs) {
		t.Errorf("SSHExtraArgs = %v, want %v", newRC.SSHExtraArgs, rc.SSHExtraArgs)
	}
}

// --project-name is the CLI's half of the same fix. Without it `cdeploy deploy
// -C /srv/app` still addressed the directory's default project while the TUI
// addressed `blue`, and the two wrote different rollback snapshots.
func TestProjectNameFlagRegistered(t *testing.T) {
	root := NewRootCmd()
	f := root.PersistentFlags().Lookup("project-name")
	if f == nil {
		t.Fatal("--project-name is not registered")
	}
	if f.Shorthand != "p" {
		t.Errorf("shorthand = %q, want p", f.Shorthand)
	}
}

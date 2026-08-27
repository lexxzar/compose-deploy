package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

// lsPayloadFor renders a one-project `docker compose ls -a --format json`
// answer for a directory holding a single docker-compose.yml.
func lsPayloadFor(name, dir string) []byte {
	return []byte(`[{"Name":"` + name + `","Status":"running(1)","ConfigFiles":"` +
		filepath.ToSlash(filepath.Join(dir, "docker-compose.yml")) + `"}]`)
}

// hookedLocalFactory returns a compose.New replacement that attaches test hooks
// answering every `docker` output call with ls.
func hookedLocalFactory(ls []byte, built **compose.Compose) func(string) *compose.Compose {
	return func(dir string) *compose.Compose {
		lc := compose.New(dir)
		lc.SetStandalone(false)
		lc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) { return ls, nil },
		)
		if built != nil {
			*built = lc
		}
		return lc
	}
}

// projectSubdir makes a real directory whose base name is a clean compose
// project name, so the derived default identity is predictable.
func projectSubdir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return dir
}

func writeSnapshotEntry(t *testing.T, c *compose.Compose, service string) {
	t.Helper()
	err := c.WriteSnapshot(context.Background(), &compose.Snapshot{
		ProjectDir: c.ProjectDir,
		Services: map[string]compose.SnapshotEntry{
			service: {Image: service + ":1", Digest: "sha256:" + strings.Repeat("a", 64), RecordedAt: "2026-08-27T00:00:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
}

func stateFiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, ".cdeploy", "state"))
	if err != nil {
		t.Fatalf("reading the state directory: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

// THE convergence property: the TUI fast track, a grouped drill-in and the CLI
// must address ONE project and therefore key ONE rollback state file.
//
// The fast track and every bare CLI verb name no project, so they address the
// one compose derives from the directory; a picker row names that same project
// explicitly. canonicalStateName folds the two spellings together with NO host
// lookup, which is what makes the convergence survive a pipeline that removes
// every container.
func TestProjectIdentity_EveryPathKeysOneStateFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projDir := projectSubdir(t, "myapp")
	ls := lsPayloadFor("myapp", projDir)

	// Path A — the TUI local fast track (cmd/root.go), a bare directory.
	oldRootNewLocal := rootNewLocal
	t.Cleanup(func() { rootNewLocal = oldRootNewLocal })
	rootNewLocal = hookedLocalFactory(ls, nil)
	fastTrack, ok := fastTrackComposer(projDir, false).(*compose.Compose)
	if !ok {
		t.Fatal("the fast track did not build a *compose.Compose")
	}

	// Path B — a grouped drill-in (cmd/root.go localComposerFor), from a row
	// that carries the project's real name.
	row := compose.Project{
		Name:        "myapp",
		ConfigDir:   projDir,
		ConfigFiles: []string{filepath.Join(projDir, "docker-compose.yml")},
	}
	drilled, ok := localComposerFor(row, compose.New(projDir), false, true).(*compose.Compose)
	if !ok {
		t.Fatal("the drill-in factory did not build a *compose.Compose")
	}
	if drilled.ProjectName != "myapp" {
		t.Errorf("the drill-in dropped the row's name: %q", drilled.ProjectName)
	}

	// Path C — the CLI, through the real runOperation branch, with no -p.
	oldServer, oldSSH, oldProj, oldName := serverName, sshTarget, projectDir, projectName
	oldNewLocal, oldLogDir := opNewLocal, logDir
	t.Cleanup(func() {
		serverName, sshTarget, projectDir, projectName = oldServer, oldSSH, oldProj, oldName
		opNewLocal, logDir = oldNewLocal, oldLogDir
	})
	serverName, sshTarget, projectName = "", "", ""
	projectDir, logDir = projDir, t.TempDir()
	var cli *compose.Compose
	opNewLocal = hookedLocalFactory(ls, &cli)
	if err := runOperation(context.Background(), runner.StopOnly, true, nil); err != nil {
		t.Fatalf("runOperation: %v", err)
	}
	if cli == nil {
		t.Fatal("the CLI never built a local composer")
	}

	paths := map[string]*compose.Compose{"fast track": fastTrack, "drill-in": drilled, "cli": cli}
	writeSnapshotEntry(t, fastTrack, "web")
	writeSnapshotEntry(t, drilled, "db")
	writeSnapshotEntry(t, cli, "cache")

	if files := stateFiles(t, home); len(files) != 1 {
		t.Fatalf("the three paths wrote %d state files (%v), want 1", len(files), files)
	}

	for name, c := range paths {
		snap, err := c.ReadSnapshot(context.Background())
		if err != nil {
			t.Fatalf("%s: ReadSnapshot: %v", name, err)
		}
		if snap == nil {
			t.Fatalf("%s: read no snapshot", name)
		}
		for _, svc := range []string{"web", "db", "cache"} {
			if _, found := snap.Services[svc]; !found {
				t.Errorf("%s: snapshot is missing %q (services: %v)", name, svc, snap.Services)
			}
		}
	}
}

// The other half of the same rule: a project that is NOT the directory's own
// keeps a state file of its own, so two `-p` projects sharing one directory
// never read each other's digests. mergeSnapshot merges by SERVICE name, so a
// shared file would have let one project's deploy overwrite the other's `web`
// entry and a later rollback pin the wrong image.
func TestProjectIdentity_TwoNamedProjectsInOneDirStayApart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projDir := projectSubdir(t, "srvapp")

	blue := compose.New(projDir)
	blue.ProjectName = "blue"
	green := compose.New(projDir)
	green.ProjectName = "green"
	deflt := compose.New(projDir)

	writeSnapshotEntry(t, blue, "blue-web")
	writeSnapshotEntry(t, green, "green-web")
	writeSnapshotEntry(t, deflt, "default-web")

	if files := stateFiles(t, home); len(files) != 3 {
		t.Fatalf("three identities wrote %d state files (%v), want 3", len(files), files)
	}

	for name, pair := range map[string]struct {
		c    *compose.Compose
		want string
	}{
		"blue":    {blue, "blue-web"},
		"green":   {green, "green-web"},
		"default": {deflt, "default-web"},
	} {
		snap, err := pair.c.ReadSnapshot(context.Background())
		if err != nil {
			t.Fatalf("%s: ReadSnapshot: %v", name, err)
		}
		if snap == nil || len(snap.Services) != 1 {
			t.Fatalf("%s: read %+v, want exactly its own entry", name, snap)
		}
		if _, found := snap.Services[pair.want]; !found {
			t.Errorf("%s: read another project's digests: %v", name, snap.Services)
		}
	}
}

// REQUIREMENT: a deploy that fails at `pull`, after `rm -f` has removed every
// container, must still be rollback-able by the command a user naturally
// reaches for.
//
// This is the regression a previous round shipped: it derived the identity from
// `docker compose ls -a`, which enumerates projects by scanning CONTAINER
// LABELS. With the containers gone the project was not reported, the composer
// keyed a different state file, and the rollback reported "no snapshot".
func TestRollbackFindsTheSnapshotAfterEveryContainerIsRemoved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projDir := projectSubdir(t, "myapp")

	// The deploy ran from a grouped drill-in, while the project still had
	// containers and `docker compose ls` still reported it.
	row := compose.Project{Name: "myapp", ConfigDir: projDir}
	deployComposer, ok := localComposerFor(row, compose.New(projDir), false, true).(*compose.Compose)
	if !ok {
		t.Fatal("the drill-in factory did not build a *compose.Compose")
	}
	writeSnapshotEntry(t, deployComposer, "web")

	// `rm -f` removed every container, then `pull` failed. `docker compose ls`
	// now reports NOTHING for this host.
	emptyLS := []byte(`[]`)

	// `cdeploy rollback -C <dir>` — the command a user reaches for.
	lc := hookedLocalFactory(emptyLS, nil)(projDir)
	if err := prepareLocalComposer(context.Background(), lc, ""); err != nil {
		t.Fatalf("prepareLocalComposer: %v", err)
	}
	snap, err := lc.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if snap == nil || snap.Services["web"].Digest == "" {
		t.Fatalf("the snapshot was orphaned by the pipeline that removed the containers: %+v", snap)
	}

	// `cdeploy rollback -C <dir> -p myapp` must work too, and must not be
	// refused just because `ls` no longer reports the project.
	named := hookedLocalFactory(emptyLS, nil)(projDir)
	if err := prepareLocalComposer(context.Background(), named, "myapp"); err != nil {
		t.Fatalf("prepareLocalComposer(-p myapp): %v", err)
	}
	snap, err = named.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot(-p myapp): %v", err)
	}
	if snap == nil || snap.Services["web"].Digest == "" {
		t.Fatalf("an explicitly named rollback lost the snapshot: %+v", snap)
	}
}

// The remote twin of the same requirement, over SSH: the state path a rollback
// reads must be the one the deploy wrote, with no `docker compose ls` involved
// on either side.
func TestRemoteRollbackFindsTheSnapshotAfterEveryContainerIsRemoved(t *testing.T) {
	payload := `{"schema":1,"project_dir":"/srv/myapp","project_name":"","services":` +
		`{"web":{"image":"nginx","digest":"sha256:old","recorded_at":"2026-08-27T00:00:00Z"}}}`

	var lsCalls int
	var readCmd string
	factory := func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) {
				last := cmd.Args[len(cmd.Args)-1]
				switch {
				case strings.Contains(last, "'ls' '-a'"):
					lsCalls++
					return []byte(`[]`), nil
				case strings.Contains(last, ".cdeploy/state/"):
					readCmd = last
					return []byte(payload), nil
				}
				return []byte("Docker Compose version v2.0.0\n"), nil
			},
		)
		return rc
	}

	// The deploy drilled into row "myapp"; the rollback is a bare
	// `cdeploy rollback -S host -C /srv/myapp`.
	deployRC := compose.NewRemote("user@host", "/srv/myapp")
	deployRC.ProjectName = "myapp"

	rc, cleanup, err := resolveSSHRemote(context.Background(), "user@host", "/srv/myapp", "", "", factory)
	if err != nil {
		t.Fatalf("resolveSSHRemote: %v", err)
	}
	defer cleanup()
	if lsCalls != 0 {
		t.Errorf("the remote resolve ran `docker compose ls` %d times, want 0", lsCalls)
	}

	snap, err := rc.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if snap == nil || snap.Services["web"].Digest != "sha256:old" {
		t.Fatalf("the remote snapshot was orphaned: %+v", snap)
	}
	// The bare rollback read exactly the path the named deploy would write.
	if !strings.Contains(readCmd, remoteStateKeyFor(t, deployRC)) {
		t.Errorf("rollback read %q, which is not the path the drill-in deploy writes", readCmd)
	}
}

// remoteStateKeyFor extracts the 12-hex state key a remote composer writes, by
// reading it back out of the read command it emits.
func remoteStateKeyFor(t *testing.T, rc *compose.RemoteCompose) string {
	t.Helper()
	var got string
	rc.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		got = cmd.Args[len(cmd.Args)-1]
		return nil, nil
	})
	if _, err := rc.ReadSnapshot(context.Background()); err != nil {
		t.Fatalf("probing the remote state path: %v", err)
	}
	i := strings.Index(got, ".cdeploy/state/")
	if i < 0 {
		t.Fatalf("no state path in %q", got)
	}
	rest := got[i+len(".cdeploy/state/"):]
	j := strings.Index(rest, ".json")
	if j < 0 {
		t.Fatalf("no state file in %q", got)
	}
	return rest[:j]
}

// REQUIREMENT: `cdeploy deploy -p brand-new-project -a` must be able to create a
// project that does not exist yet, locally AND remotely. A previous round
// refused any `-p` name `docker compose ls` did not report, which also refused
// every redeploy after a `down`, a `prune`, or a failed deploy.
func TestNamedProjectNeedNotExistYet(t *testing.T) {
	lc := hookedLocalFactory([]byte(`[]`), nil)("/srv/app")
	if err := prepareLocalComposer(context.Background(), lc, "myapp-pr123"); err != nil {
		t.Fatalf("a brand-new local project was refused: %v", err)
	}
	if lc.ProjectName != "myapp-pr123" {
		t.Errorf("ProjectName = %q, want myapp-pr123", lc.ProjectName)
	}

	factory := func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) {
				return []byte("Docker Compose version v2.0.0\n"), nil
			},
		)
		return rc
	}
	rc, cleanup, err := resolveSSHRemote(context.Background(), "user@host", "/srv/app", "myapp-pr123", "", factory)
	if err != nil {
		t.Fatalf("a brand-new remote project was refused: %v", err)
	}
	defer cleanup()
	if rc.ProjectName != "myapp-pr123" {
		t.Errorf("remote ProjectName = %q, want myapp-pr123", rc.ProjectName)
	}
}

// REQUIREMENT: `-C` must not be silently overridden by `-p`. A previous round
// matched the name across EVERY project on the host and then replaced the
// caller's directory with the matched row's ConfigDir, so `deploy -C /opt/appA
// -p web -a` ran the whole pipeline against /opt/appB.
func TestProjectNameDoesNotOverrideProjectDir(t *testing.T) {
	ls := []byte(`[{"Name":"web","Status":"running(1)","ConfigFiles":"/opt/appB/docker-compose.yml"}]`)
	lc := hookedLocalFactory(ls, nil)("/opt/appA")
	if err := prepareLocalComposer(context.Background(), lc, "web"); err != nil {
		t.Fatalf("prepareLocalComposer: %v", err)
	}
	if lc.ProjectDir != "/opt/appA" {
		t.Errorf("ProjectDir = %q, want the caller's /opt/appA", lc.ProjectDir)
	}
	if len(lc.ComposeFiles) != 0 {
		t.Errorf("ComposeFiles = %v, want none — the CLI must not adopt another directory's files", lc.ComposeFiles)
	}
}

// REQUIREMENT: a compose file set adopted from a stale container label must not
// silently disable auto-discovery. Both TUI factories route the row's files
// through compose.PinComposeFiles, which drops a set auto-discovery would find
// anyway — so a docker-compose.override.yml added after the containers were
// created is picked up instead of being frozen out forever.
func TestPickerRowsDoNotPinAutoDiscoverableFiles(t *testing.T) {
	discoverable := compose.Project{
		Name:        "app",
		ConfigDir:   "/srv/app",
		ConfigFiles: []string{"/srv/app/docker-compose.yml"},
	}
	handPicked := compose.Project{
		Name:        "prod",
		ConfigDir:   "/srv/app",
		ConfigFiles: []string{"/srv/app/prod.yml"},
	}

	lc, ok := localComposerFor(discoverable, compose.New("/srv/app"), false, true).(*compose.Compose)
	if !ok {
		t.Fatal("localComposerFor did not build a *compose.Compose")
	}
	if len(lc.ComposeFiles) != 0 {
		t.Errorf("ComposeFiles = %v, want auto-discovery for a default-named file", lc.ComposeFiles)
	}
	lc, ok = localComposerFor(handPicked, compose.New("/srv/app"), false, true).(*compose.Compose)
	if !ok {
		t.Fatal("localComposerFor did not build a *compose.Compose")
	}
	if !slices.Equal(lc.ComposeFiles, handPicked.ConfigFiles) {
		t.Errorf("ComposeFiles = %v, want the hand-picked %v", lc.ComposeFiles, handPicked.ConfigFiles)
	}

	live := compose.NewRemote("user@host", "/srv/app")
	rc, ok := remoteComposerFor(discoverable, live, false).(*compose.RemoteCompose)
	if !ok {
		t.Fatal("remoteComposerFor did not build a *compose.RemoteCompose")
	}
	if len(rc.ComposeFiles) != 0 {
		t.Errorf("remote ComposeFiles = %v, want auto-discovery", rc.ComposeFiles)
	}
	rc, ok = remoteComposerFor(handPicked, live, false).(*compose.RemoteCompose)
	if !ok {
		t.Fatal("remoteComposerFor did not build a *compose.RemoteCompose")
	}
	if !slices.Equal(rc.ComposeFiles, handPicked.ConfigFiles) {
		t.Errorf("remote ComposeFiles = %v, want the hand-picked %v", rc.ComposeFiles, handPicked.ConfigFiles)
	}
}

// No entry point may pay a `docker compose ls` to learn its own identity: that
// lookup is what tied the state key to container state.
func TestNoIdentityLookupOnTheWritePath(t *testing.T) {
	var calls int
	countingFactory := func(dir string) *compose.Compose {
		lc := compose.New(dir)
		lc.SetStandalone(false)
		lc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) {
				if slices.Contains(cmd.Args, "ls") {
					calls++
				}
				return []byte(`[]`), nil
			},
		)
		return lc
	}

	oldRootNewLocal := rootNewLocal
	t.Cleanup(func() { rootNewLocal = oldRootNewLocal })
	rootNewLocal = countingFactory
	fastTrackComposer("/srv/app", false)

	lc := countingFactory("/srv/app")
	if err := prepareLocalComposer(context.Background(), lc, "blue"); err != nil {
		t.Fatalf("prepareLocalComposer: %v", err)
	}
	if calls != 0 {
		t.Errorf("`docker compose ls` ran %d times while resolving identity, want 0", calls)
	}
}

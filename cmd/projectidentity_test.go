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

func writeSnapshotEntry(t *testing.T, c *compose.Compose, service string) {
	t.Helper()
	err := c.WriteSnapshot(context.Background(), &compose.Snapshot{
		ProjectDir:  c.ProjectDir,
		ProjectName: c.ProjectName,
		Services: map[string]compose.SnapshotEntry{
			service: {Image: service + ":1", Digest: "sha256:" + strings.Repeat("a", 64), RecordedAt: "2026-08-27T00:00:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
}

// THE point of canonical identity resolution: the TUI fast track, a grouped
// drill-in and the CLI must address ONE project and therefore key ONE rollback
// state file.
//
// Before resolution the key depended on how the composer was BUILT, not on the
// project: `entryLocal` and every CLI verb left ProjectName empty and keyed
// sha256(dir), while a picker row keyed sha256(dir+NUL+name). A TUI deploy then
// recorded digests the CLI rollback could not see — and the CLI restored a
// different deploy's images with no warning.
func TestProjectIdentity_EveryPathKeysOneStateFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COMPOSE_PROJECT_NAME", "")

	projDir := t.TempDir()
	ls := lsPayloadFor("blue", projDir)
	composeFile := filepath.Join(projDir, "docker-compose.yml")

	// Path A — the TUI local fast track (cmd/root.go), a bare directory.
	oldRootNewLocal := rootNewLocal
	t.Cleanup(func() { rootNewLocal = oldRootNewLocal })
	rootNewLocal = hookedLocalFactory(ls, nil)
	fastTrack, ok := fastTrackComposer(context.Background(), projDir, false).(*compose.Compose)
	if !ok {
		t.Fatal("the fast track did not build a *compose.Compose")
	}

	// Path B — a grouped drill-in (cmd/root.go localComposerFor), from a row.
	row := compose.Project{Name: "blue", ConfigDir: projDir, ConfigFiles: []string{composeFile}}
	drilled, ok := localComposerFor(row, compose.New(projDir), false, true).(*compose.Compose)
	if !ok {
		t.Fatal("the drill-in factory did not build a *compose.Compose")
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
	for name, c := range paths {
		if c.ProjectName != "blue" {
			t.Errorf("%s: ProjectName = %q, want blue", name, c.ProjectName)
		}
	}

	writeSnapshotEntry(t, fastTrack, "web")
	writeSnapshotEntry(t, drilled, "db")
	writeSnapshotEntry(t, cli, "cache")

	entries, err := os.ReadDir(filepath.Join(home, ".cdeploy", "state"))
	if err != nil {
		t.Fatalf("reading the state directory: %v", err)
	}
	var stateFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			stateFiles = append(stateFiles, e.Name())
		}
	}
	if len(stateFiles) != 1 {
		t.Fatalf("the three paths wrote %d state files (%v), want 1", len(stateFiles), stateFiles)
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

// The CLI must carry the project's own compose files, not just its name.
// `deploy -C /srv/app -p prod -a` otherwise ran stop → rm → pull → create →
// start against project prod using an auto-discovered docker-compose.yml — prod
// rebuilt from another project's definitions, destructive and silent.
func TestPrepareLocalComposer_NamedProjectCarriesItsFileSet(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	want := []string{"/srv/app/prod.yml", "/srv/app/prod.override.yml"}
	ls := []byte(`[{"Name":"app","Status":"running(1)","ConfigFiles":"/srv/app/docker-compose.yml"},` +
		`{"Name":"prod","Status":"running(2)","ConfigFiles":"` + strings.Join(want, ",") + `"}]`)

	lc := hookedLocalFactory(ls, nil)("/srv/app")
	if err := prepareLocalComposer(context.Background(), lc, "prod"); err != nil {
		t.Fatalf("prepareLocalComposer: %v", err)
	}
	if lc.ProjectName != "prod" {
		t.Errorf("ProjectName = %q, want prod", lc.ProjectName)
	}
	if !slices.Equal(lc.ComposeFiles, want) {
		t.Errorf("ComposeFiles = %v, want %v", lc.ComposeFiles, want)
	}
}

// A --project-name docker does not report is refused rather than run against
// whatever file the directory holds.
func TestPrepareLocalComposer_RefusesAnUnknownProject(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	ls := []byte(`[{"Name":"app","Status":"running(1)","ConfigFiles":"/srv/app/docker-compose.yml"}]`)
	lc := hookedLocalFactory(ls, nil)("/srv/app")
	err := prepareLocalComposer(context.Background(), lc, "prod")
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	if !strings.Contains(err.Error(), `"prod"`) {
		t.Errorf("error = %q, want it to name the project", err.Error())
	}
}

// The remote helpers resolve too, so `-s prod -p blue` addresses blue's own
// files over SSH — and the snapshot it keys matches the TUI's.
func TestResolveSSHRemote_ResolvesTheProjectFileSet(t *testing.T) {
	want := []string{"/srv/app/blue.yml", "/srv/app/blue.override.yml"}
	ls := []byte(`[{"Name":"blue","Status":"running(1)","ConfigFiles":"` + strings.Join(want, ",") + `"}]`)

	var built *compose.RemoteCompose
	factory := func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) {
				// remoteCommand shell-escapes each arg, so the subcommand
				// reads `'ls' '-a' …` on the wire.
				if strings.Contains(cmd.Args[len(cmd.Args)-1], "'ls' '-a'") {
					return ls, nil
				}
				return []byte("Docker Compose version v2.0.0\n"), nil
			},
		)
		built = rc
		return rc
	}

	rc, cleanup, err := resolveSSHRemote(context.Background(), "user@host", "/srv/app", "blue", "", factory)
	if err != nil {
		t.Fatalf("resolveSSHRemote: %v", err)
	}
	defer cleanup()
	if built == nil || rc != built {
		t.Fatal("the helper did not return the composer it built")
	}
	if !slices.Equal(rc.ComposeFiles, want) {
		t.Errorf("ComposeFiles = %v, want %v", rc.ComposeFiles, want)
	}
}

// A remote --project-name the host does not report is refused, and the refusal
// tears the ControlMaster connection down.
func TestResolveSSHRemote_RefusesAnUnknownProject(t *testing.T) {
	closes := 0
	factory := func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error {
				for i, a := range cmd.Args {
					if a == "-O" && i+1 < len(cmd.Args) && cmd.Args[i+1] == "exit" {
						closes++
					}
				}
				return nil
			},
			func(cmd *exec.Cmd) ([]byte, error) {
				// remoteCommand shell-escapes each arg, so the subcommand
				// reads `'ls' '-a' …` on the wire.
				if strings.Contains(cmd.Args[len(cmd.Args)-1], "'ls' '-a'") {
					return []byte(`[{"Name":"app","Status":"running(1)","ConfigFiles":"/srv/app/docker-compose.yml"}]`), nil
				}
				return []byte("Docker Compose version v2.0.0\n"), nil
			},
		)
		return rc
	}

	_, cleanup, err := resolveSSHRemote(context.Background(), "user@host", "/srv/app", "prod", "", factory)
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	defer cleanup()
	if closes != 1 {
		t.Errorf("Close() ran %d times, want 1", closes)
	}
}

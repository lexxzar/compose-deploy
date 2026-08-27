package compose

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// lsJSON renders a `docker compose ls -a --format json` payload.
func lsJSON(entries ...string) []byte {
	return []byte("[" + strings.Join(entries, ",") + "]")
}

func lsEntryJSON(name, files string) string {
	return `{"Name":"` + name + `","Status":"running(1)","ConfigFiles":"` + files + `"}`
}

// hookedLocal returns a Compose wired to a fake docker that answers the
// host-wide `ls` with the given payload and nothing else. It also clears
// COMPOSE_PROJECT_NAME, which ResolveProject reads on the local path — a
// developer with it exported would otherwise get a different identity than CI.
func hookedLocal(t *testing.T, dir string, ls []byte, lsErr error) (*Compose, func() []string) {
	t.Helper()
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	c := New(dir)
	c.SetStandalone(false)
	var lastArgs []string
	c.SetTestHooks(
		func(cmd *exec.Cmd) error { return nil },
		func(cmd *exec.Cmd) ([]byte, error) {
			lastArgs = cmd.Args
			if lsErr != nil {
				return nil, lsErr
			}
			return ls, nil
		},
	)
	return c, func() []string { return lastArgs }
}

// The unnamed lookup matches on the directory and adopts the row's identity.
// This is the whole point of resolution: a composer built from a bare directory
// used to address whatever project compose derived from that directory's name,
// while the TUI addressed the row's real name.
func TestResolveProject_AdoptsTheDirectorysProject(t *testing.T) {
	c, _ := hookedLocal(t, "/srv/app", lsJSON(lsEntryJSON("blue", "/srv/app/blue.yml,/srv/app/extra.yml")), nil)
	if err := c.ResolveProject(context.Background()); err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if c.ProjectName != "blue" {
		t.Errorf("ProjectName = %q, want blue", c.ProjectName)
	}
	if want := []string{"/srv/app/blue.yml", "/srv/app/extra.yml"}; !slices.Equal(c.ComposeFiles, want) {
		t.Errorf("ComposeFiles = %v, want %v", c.ComposeFiles, want)
	}
	if c.ProjectDir != "/srv/app" {
		t.Errorf("ProjectDir = %q, want /srv/app", c.ProjectDir)
	}
}

// Two projects in one directory are two identities. Picking one would address
// the wrong container set, so the composer stays directory-addressed and the
// user must say which with --project-name.
func TestResolveProject_AmbiguousDirectoryResolvesNothing(t *testing.T) {
	c, _ := hookedLocal(t, "/srv/app", lsJSON(
		lsEntryJSON("blue", "/srv/app/docker-compose.yml"),
		lsEntryJSON("green", "/srv/app/docker-compose.yml"),
	), nil)
	if err := c.ResolveProject(context.Background()); err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if c.ProjectName != "" {
		t.Errorf("ProjectName = %q, want empty", c.ProjectName)
	}
	if !c.legacyStateBlocked {
		t.Error("legacyStateBlocked = false, want true for a shared directory")
	}
}

// An explicit --project-name is looked up BY NAME and contributes the project's
// own file set. Without it `deploy -C /srv/app -p prod` recreated prod from a
// sibling project's auto-discovered docker-compose.yml.
func TestResolveProject_NamedLookupCarriesTheFileSet(t *testing.T) {
	c, _ := hookedLocal(t, "/srv/app", lsJSON(
		lsEntryJSON("app", "/srv/app/docker-compose.yml"),
		lsEntryJSON("prod", "/srv/app/prod.yml,/srv/app/prod.override.yml"),
	), nil)
	c.ProjectName = "prod"
	if err := c.ResolveProject(context.Background()); err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if want := []string{"/srv/app/prod.yml", "/srv/app/prod.override.yml"}; !slices.Equal(c.ComposeFiles, want) {
		t.Errorf("ComposeFiles = %v, want %v", c.ComposeFiles, want)
	}
}

// A caller-supplied file set is never overwritten by the lookup.
func TestResolveProject_KeepsAnExplicitFileSet(t *testing.T) {
	c, _ := hookedLocal(t, "/srv/app", lsJSON(lsEntryJSON("prod", "/srv/app/prod.yml")), nil)
	c.ProjectName = "prod"
	c.ComposeFiles = []string{"/srv/app/caller.yml"}
	if err := c.ResolveProject(context.Background()); err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if want := []string{"/srv/app/caller.yml"}; !slices.Equal(c.ComposeFiles, want) {
		t.Errorf("ComposeFiles = %v, want %v", c.ComposeFiles, want)
	}
}

// A named project docker does not report is REFUSED. Proceeding would run the
// pipeline against whatever compose file the directory holds, under the
// requested label — destructive and silent.
func TestResolveProject_RefusesAnUnknownName(t *testing.T) {
	c, _ := hookedLocal(t, "/srv/app", lsJSON(lsEntryJSON("app", "/srv/app/docker-compose.yml")), nil)
	c.ProjectName = "prod"
	err := c.ResolveProject(context.Background())
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	if !errors.Is(err, errProjectNotFound) {
		t.Errorf("error %v does not wrap errProjectNotFound", err)
	}
	if !strings.Contains(err.Error(), `"prod"`) {
		t.Errorf("error = %q, want it to name the project", err.Error())
	}
}

// A listing failure is fatal for an explicit name (we cannot know that
// project's files) but soft for an unnamed composer, which must keep working on
// a host whose `docker compose ls` is unusable.
func TestResolveProject_ListingFailure(t *testing.T) {
	named, _ := hookedLocal(t, "/srv/app", nil, errors.New("boom"))
	named.ProjectName = "prod"
	if err := named.ResolveProject(context.Background()); err == nil {
		t.Error("named composer: expected an error, got nil")
	}

	unnamed, _ := hookedLocal(t, "/srv/app", nil, errors.New("boom"))
	if err := unnamed.ResolveProject(context.Background()); err != nil {
		t.Errorf("unnamed composer: %v", err)
	}
	if unnamed.ProjectName != "" {
		t.Errorf("ProjectName = %q, want empty", unnamed.ProjectName)
	}
}

// COMPOSE_PROJECT_NAME is what the local docker CLI itself would use (the
// command inherits os.Environ), so resolution must not override it with a
// directory lookup — and must not refuse a name docker has not created yet.
func TestResolveProject_HonorsComposeProjectNameEnv(t *testing.T) {
	c, _ := hookedLocal(t, "/srv/app", lsJSON(lsEntryJSON("green", "/srv/app/docker-compose.yml")), nil)
	t.Setenv("COMPOSE_PROJECT_NAME", "blue")
	if err := c.ResolveProject(context.Background()); err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if c.ProjectName != "blue" {
		t.Errorf("ProjectName = %q, want blue (the environment's project, not the directory's)", c.ProjectName)
	}
}

// The lookup costs one `docker compose ls` per composer.
func TestResolveProject_IsIdempotent(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	c := New("/srv/app")
	c.SetStandalone(false)
	calls := 0
	c.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		calls++
		return lsJSON(lsEntryJSON("blue", "/srv/app/docker-compose.yml")), nil
	})
	for i := 0; i < 3; i++ {
		if err := c.ResolveProject(context.Background()); err != nil {
			t.Fatalf("ResolveProject: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("docker compose ls ran %d times, want 1", calls)
	}
}

// `docker compose ls` is host-wide discovery: `-p` selects nothing there and
// `-f` would point compose at a file it must parse. ResolveProject calls it on
// a composer that already carries both, which is exactly when it matters.
func TestListProjects_CarriesNoProjectOrFileFlags(t *testing.T) {
	c, args := hookedLocal(t, "/srv/app", lsJSON(), nil)
	c.ProjectName = "blue"
	c.ComposeFiles = []string{"/srv/app/blue.yml"}
	if _, err := c.ListProjects(context.Background()); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if slices.Contains(args(), "-p") || slices.Contains(args(), "-f") {
		t.Errorf("ls argv = %v, want no -p/-f flags", args())
	}

	r := NewRemote("host", "/srv/app")
	r.SetStandalone(false)
	r.ProjectName = "blue"
	r.ComposeFiles = []string{"/srv/app/blue.yml"}
	var remoteCmd string
	r.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		remoteCmd = cmd.Args[len(cmd.Args)-1]
		return lsJSON(), nil
	})
	if _, err := r.ListProjects(context.Background()); err != nil {
		t.Fatalf("remote ListProjects: %v", err)
	}
	if strings.Contains(remoteCmd, "-p ") || strings.Contains(remoteCmd, "-f ") {
		t.Errorf("remote ls command = %q, want no -p/-f flags", remoteCmd)
	}
}

// The remote twin resolves the same way, and does NOT consult the local
// COMPOSE_PROJECT_NAME: ssh does not carry the local environment, so a local
// value says nothing about which project a remote command addresses.
func TestRemoteResolveProject(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "local-only")
	r := NewRemote("host", "/srv/app/")
	r.SetStandalone(false)
	r.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		return lsJSON(lsEntryJSON("blue", "/srv/app/blue.yml")), nil
	})
	if err := r.ResolveProject(context.Background()); err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if r.ProjectName != "blue" {
		t.Errorf("ProjectName = %q, want blue", r.ProjectName)
	}
	if want := []string{"/srv/app/blue.yml"}; !slices.Equal(r.ComposeFiles, want) {
		t.Errorf("ComposeFiles = %v, want %v", r.ComposeFiles, want)
	}
}

func TestRemoteResolveProject_RefusesAnUnknownName(t *testing.T) {
	r := NewRemote("host", "/srv/app")
	r.SetStandalone(false)
	r.ProjectName = "prod"
	r.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		return lsJSON(lsEntryJSON("app", "/srv/app/docker-compose.yml")), nil
	})
	if err := r.ResolveProject(context.Background()); !errors.Is(err, errProjectNotFound) {
		t.Fatalf("error = %v, want errProjectNotFound", err)
	}
}

// resolveIdentity is the pure core both transports share.
func TestResolveIdentity(t *testing.T) {
	projects := []Project{
		{Name: "blue", ConfigDir: "/srv/app", ConfigFiles: []string{"/srv/app/blue.yml"}},
		{Name: "green", ConfigDir: "/srv/app", ConfigFiles: []string{"/srv/app/green.yml"}},
		{Name: "solo", ConfigDir: "/srv/solo", ConfigFiles: []string{"/srv/solo/docker-compose.yml"}},
		{Name: "(unmanaged)", Unmanaged: true},
		{Name: "labelonly"},
	}

	tests := []struct {
		name          string
		dir, projName string
		wantName      string
		wantOK        bool
		wantShared    bool
	}{
		{name: "unique directory", dir: "/srv/solo", wantName: "solo", wantOK: true},
		{name: "shared directory", dir: "/srv/app", wantOK: false, wantShared: true},
		{name: "unknown directory", dir: "/srv/none", wantOK: false},
		{name: "empty directory", dir: "", wantOK: false},
		{name: "by name in a shared dir", dir: "/srv/app", projName: "green", wantName: "green", wantOK: true, wantShared: true},
		{name: "by name elsewhere", dir: "/srv/none", projName: "solo", wantName: "solo", wantOK: true},
		{name: "unknown name", dir: "/srv/solo", projName: "nope", wantOK: false},
		// The synthetic row stands for containers with no compose project at
		// all; it must never be resolved as one.
		{name: "unmanaged is never a match", dir: "", projName: "(unmanaged)", wantOK: false},
		// A project docker reports with no config_files has no directory to
		// match on, but is still addressable by name.
		{name: "label-only project by name", dir: "/srv/solo", projName: "labelonly", wantName: "labelonly", wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proj, ok, shared := resolveIdentity(projects, tt.dir, tt.projName, func(s string) string { return s })
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && proj.Name != tt.wantName {
				t.Errorf("name = %q, want %q", proj.Name, tt.wantName)
			}
			if shared != tt.wantShared {
				t.Errorf("dirShared = %v, want %v", shared, tt.wantShared)
			}
		})
	}
}

// The dir-only state file was written by a build that addressed whichever
// project the DIRECTORY resolved to. With several projects living there it
// names none of them, so the read fallback must not hand its digests to one.
func TestLegacyStateFallbackBlockedForASharedDirectory(t *testing.T) {
	c := New("/srv/app")
	c.ProjectName = "blue"
	path, err := c.localLegacyStatePath()
	if err != nil {
		t.Fatalf("localLegacyStatePath: %v", err)
	}
	if path == "" {
		t.Fatal("an unshared directory must keep its dir-only fallback")
	}

	c.legacyStateBlocked = true
	path, err = c.localLegacyStatePath()
	if err != nil {
		t.Fatalf("localLegacyStatePath: %v", err)
	}
	if path != "" {
		t.Errorf("legacy path = %q, want empty for a shared directory", path)
	}

	r := NewRemote("host", "/srv/app")
	r.ProjectName = "blue"
	if r.remoteLegacyStatePath() == "" {
		t.Fatal("an unshared remote directory must keep its dir-only fallback")
	}
	r.legacyStateBlocked = true
	if got := r.remoteLegacyStatePath(); got != "" {
		t.Errorf("remote legacy path = %q, want empty for a shared directory", got)
	}
}

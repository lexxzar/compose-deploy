package compose

import (
	"context"
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

// hookedLocal returns a Compose wired to a fake docker that answers every
// output call with the given payload.
func hookedLocal(t *testing.T, dir string, ls []byte, lsErr error) (*Compose, func() []string) {
	t.Helper()
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

// composeDefaultProjectName must reproduce compose-go's NormalizeProjectName
// applied to the directory's base name. Everything else keys off it: it is the
// definition of the project an UNNAMED invocation addresses, and therefore of
// which named composer shares the dir-only state file.
func TestComposeDefaultProjectName(t *testing.T) {
	tests := []struct {
		dir  string
		want string
	}{
		{"/srv/app", "app"},
		{"/srv/MyApp", "myapp"},
		{"/srv/my.app", "myapp"},
		{"/srv/my app", "myapp"},
		{"/srv/_app", "app"},
		{"/srv/-app-", "app-"},
		{"/srv/app-2", "app-2"},
		{"/srv/app_2", "app_2"},
		{"/srv/....", ""},
		{"/", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := composeDefaultProjectName(tt.dir); got != tt.want {
			t.Errorf("composeDefaultProjectName(%q) = %q, want %q", tt.dir, got, tt.want)
		}
	}
}

// THE identity rule. A composer naming the directory's DEFAULT project and a
// composer naming nothing are the same project, so they must fold onto the same
// canonical name — that is what makes the TUI fast track, a grouped drill-in and
// a bare CLI verb key one state file with no host lookup. A project that is NOT
// the directory default keeps its own name, so two `-p` projects in one
// directory stay apart.
func TestCanonicalStateName(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		in   string
		want string
	}{
		{"unnamed stays dir-only", "/srv/app", "", ""},
		{"the directory default folds onto dir-only", "/srv/app", "app", ""},
		{"a normalized default folds too", "/srv/My.App", "myapp", ""},
		{"a different project keeps its name", "/srv/app", "blue", "blue"},
		{"a second different project keeps its name", "/srv/app", "green", "green"},
		{"an unnormalized spelling is not the default", "/srv/app", "App", "App"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalStateName(tt.dir, tt.in); got != tt.want {
				t.Errorf("canonicalStateName(%q, %q) = %q, want %q", tt.dir, tt.in, got, tt.want)
			}
		})
	}
}

// The state key must never depend on the environment, on `docker compose ls`,
// or on anything else a running operation can change — only on the directory
// and the name the caller supplied.
func TestStateName_LocalAndRemoteAgree(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "env-only")

	// A trailing slash and a doubled separator must not change the verdict.
	for _, dir := range []string{"/srv/app", "/srv/app/", "/srv//app"} {
		c := New(dir)
		c.ProjectName = "app"
		if got := c.stateName(); got != "" {
			t.Errorf("local stateName(%q, app) = %q, want the dir-only key", dir, got)
		}
		r := NewRemote("host", dir)
		r.ProjectName = "app"
		if got := r.stateName(); got != "" {
			t.Errorf("remote stateName(%q, app) = %q, want the dir-only key", dir, got)
		}
	}

	c := New("/srv/app")
	c.ProjectName = "blue"
	r := NewRemote("host", "/srv/app")
	r.ProjectName = "blue"
	if c.stateName() != "blue" || r.stateName() != "blue" {
		t.Errorf("stateName = %q/%q, want blue/blue", c.stateName(), r.stateName())
	}
}

// `docker compose ls` is host-wide discovery: `-p` selects nothing there and
// `-f` would point compose at a file it must parse. `cdeploy list` and the TUI
// project loader call it on a composer that may already carry both.
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

// A file set docker reported that auto-discovery would find anyway must NOT be
// pinned: `-f` disables discovery, and the config_files label was stamped when
// the containers were created, so a docker-compose.override.yml added later
// would be ignored for the rest of the project's life.
func TestPinComposeFiles(t *testing.T) {
	tests := []struct {
		name  string
		dir   string
		files []string
		want  []string
	}{
		{"the plain default file auto-discovers", "/srv/app", []string{"/srv/app/docker-compose.yml"}, nil},
		{"compose.yaml auto-discovers", "/srv/app", []string{"/srv/app/compose.yaml"}, nil},
		{
			"a default file plus its override auto-discovers",
			"/srv/app",
			[]string{"/srv/app/docker-compose.yml", "/srv/app/docker-compose.override.yml"},
			nil,
		},
		{
			"a trailing slash on the dir does not defeat the match",
			"/srv/app/",
			[]string{"/srv/app/docker-compose.yml"},
			nil,
		},
		{
			"a hand-picked -f name is pinned",
			"/srv/app",
			[]string{"/srv/app/prod.yml"},
			[]string{"/srv/app/prod.yml"},
		},
		{
			"one non-default file pins the whole set",
			"/srv/app",
			[]string{"/srv/app/docker-compose.yml", "/srv/app/extra.yml"},
			[]string{"/srv/app/docker-compose.yml", "/srv/app/extra.yml"},
		},
		{
			"a default name OUTSIDE the project dir is pinned",
			"/srv/app",
			[]string{"/srv/other/docker-compose.yml"},
			[]string{"/srv/other/docker-compose.yml"},
		},
		{"an empty set stays empty", "/srv/app", nil, nil},
		{
			"no config dir leaves the set alone",
			"",
			[]string{"/srv/app/docker-compose.yml"},
			[]string{"/srv/app/docker-compose.yml"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PinComposeFiles(tt.dir, tt.files)
			if !slices.Equal(got, tt.want) {
				t.Errorf("PinComposeFiles(%q, %v) = %v, want %v", tt.dir, tt.files, got, tt.want)
			}
		})
	}
}

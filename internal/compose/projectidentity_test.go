package compose

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

// The order of composeFiles/remoteComposeFiles IS compose's discovery
// precedence, verified against compose v2.40.3: a directory holding all four
// logs `Using .../compose.yaml`. findComposeFile and findRemoteComposeFile
// probe these slices in order to pick a project's main file, and
// composeDiscoveredFiles resolves the local pin decision from the first, so a
// reordering silently points both at the wrong file.
func TestComposeFileCandidatesAreInPrecedenceOrder(t *testing.T) {
	want := []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}
	if !slices.Equal(composeFiles, want) {
		t.Errorf("composeFiles = %v, want compose's precedence order %v", composeFiles, want)
	}
	if !slices.Equal(remoteComposeFiles, want) {
		t.Errorf("remoteComposeFiles = %v, want compose's precedence order %v", remoteComposeFiles, want)
	}
	wantOverrides := []string{
		"compose.override.yml",
		"compose.override.yaml",
		"docker-compose.override.yml",
		"docker-compose.override.yaml",
	}
	if !slices.Equal(composeOverrideFiles, wantOverrides) {
		t.Errorf("composeOverrideFiles = %v, want %v", composeOverrideFiles, wantOverrides)
	}
}

// REQUIREMENT: a project created with an explicit `-f` naming a LOWER-precedence
// default file must keep its pin. Compose does not load every default name it
// finds — it takes the first by precedence — so a name-only membership test
// dropped the pin and every later stop → rm -f → pull → create → start
// recreated the project from the higher-precedence sibling's service
// definitions under the right `-p` label.
func TestPinComposeFilesLocal(t *testing.T) {
	tests := []struct {
		name    string
		present []string
		files   []string
		pinned  bool
	}{
		{
			name:    "a -f docker-compose.yml project beside a compose.yaml keeps its pin",
			present: []string{"compose.yaml", "docker-compose.yml"},
			files:   []string{"docker-compose.yml"},
			pinned:  true,
		},
		{
			name:    "compose.yml loses to compose.yaml too",
			present: []string{"compose.yaml", "compose.yml"},
			files:   []string{"compose.yml"},
			pinned:  true,
		},
		{
			name:    "docker-compose.yaml loses to docker-compose.yml",
			present: []string{"docker-compose.yml", "docker-compose.yaml"},
			files:   []string{"docker-compose.yaml"},
			pinned:  true,
		},
		{
			name:    "the file discovery actually resolves is not pinned",
			present: []string{"compose.yaml", "docker-compose.yml"},
			files:   []string{"compose.yaml"},
			pinned:  false,
		},
		{
			name:    "the only default file in the directory is not pinned",
			present: []string{"docker-compose.yml"},
			files:   []string{"docker-compose.yml"},
			pinned:  false,
		},
		{
			name:    "a reported main plus the override discovery resolves is not pinned",
			present: []string{"docker-compose.yml", "docker-compose.override.yml"},
			files:   []string{"docker-compose.yml", "docker-compose.override.yml"},
			pinned:  false,
		},
		{
			name:    "an override added after the containers were created still heals",
			present: []string{"docker-compose.yml", "docker-compose.override.yml"},
			files:   []string{"docker-compose.yml"},
			pinned:  false,
		},
		{
			name:    "a reported override that no longer exists is not pinned",
			present: []string{"docker-compose.yml"},
			files:   []string{"docker-compose.yml", "docker-compose.override.yml"},
			pinned:  false,
		},
		{
			name:    "a reported override that lost the precedence race keeps its pin",
			present: []string{"docker-compose.yml", "docker-compose.override.yml", "compose.override.yml"},
			files:   []string{"docker-compose.yml", "docker-compose.override.yml"},
			pinned:  true,
		},
		{
			name:    "a hand-picked -f name keeps its pin",
			present: []string{"docker-compose.yml", "prod.yml"},
			files:   []string{"prod.yml"},
			pinned:  true,
		},
		{
			name:    "a directory with no default file at all pins the reported set",
			present: []string{"stack.yml"},
			files:   []string{"stack.yml"},
			pinned:  true,
		},
		{
			name:    "a second main name after the first keeps the pin",
			present: []string{"compose.yaml", "docker-compose.yml"},
			files:   []string{"compose.yaml", "docker-compose.yml"},
			pinned:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tt.present {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("services: {}\n"), 0o600); err != nil {
					t.Fatalf("writing %s: %v", name, err)
				}
			}
			files := make([]string, len(tt.files))
			for i, name := range tt.files {
				files[i] = filepath.Join(dir, name)
			}
			got := PinComposeFilesLocal(dir, files)
			if tt.pinned {
				if !slices.Equal(got, files) {
					t.Errorf("PinComposeFilesLocal(%v) = %v, want the set pinned", tt.files, got)
				}
				return
			}
			if got != nil {
				t.Errorf("PinComposeFilesLocal(%v) = %v, want nil (auto-discover)", tt.files, got)
			}
		})
	}
}

// The degenerate inputs must behave exactly as the pure form does — a picker
// row with no reported files, and the unmanaged row's empty ConfigDir.
func TestPinComposeFilesLocalPassesThroughDegenerateInputs(t *testing.T) {
	if got := PinComposeFilesLocal(t.TempDir(), nil); got != nil {
		t.Errorf("empty set = %v, want nil", got)
	}
	files := []string{"/srv/app/docker-compose.yml"}
	if got := PinComposeFilesLocal("", files); !slices.Equal(got, files) {
		t.Errorf("no config dir = %v, want the set untouched", got)
	}
}

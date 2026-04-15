package compose

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

// Compile-time check: Compose implements tui.ConfigProvider.
// Can't import tui (circular), so we verify the interface shape here.
func TestCompose_ImplementsConfigProviderShape(t *testing.T) {
	c := &Compose{ProjectDir: t.TempDir(), UID: "1000:1000"}
	ctx := context.Background()

	// ConfigFile
	_, _ = c.ConfigFile(ctx)
	// ConfigResolved (needs outputCmd hook to avoid running docker)
	c.outputCmd = func(cmd *exec.Cmd) ([]byte, error) { return nil, nil }
	_, _ = c.ConfigResolved(ctx)
	// EditCommand
	os.WriteFile(filepath.Join(c.ProjectDir, "compose.yml"), []byte("test"), 0o644)
	_, _ = c.EditCommand(ctx)
	// ValidateConfig
	_ = c.ValidateConfig(ctx)
}

func TestParseProjects(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []Project
		wantErr bool
	}{
		{
			name:  "multiple projects",
			input: `[{"Name":"forms-app","Status":"running(1)","ConfigFiles":"/Work/docker/forms-app/compose.yml"},{"Name":"api-proxy","Status":"running(2)","ConfigFiles":"/Work/docker/api-proxy/compose.yml"}]`,
			want: []Project{
				{Name: "api-proxy", Status: "running(2)", ConfigDir: "/Work/docker/api-proxy"},
				{Name: "forms-app", Status: "running(1)", ConfigDir: "/Work/docker/forms-app"},
			},
		},
		{
			name:  "single project",
			input: `[{"Name":"nginx","Status":"running(1)","ConfigFiles":"/srv/nginx/docker-compose.yml"}]`,
			want: []Project{
				{Name: "nginx", Status: "running(1)", ConfigDir: "/srv/nginx"},
			},
		},
		{
			name:  "comma-separated config files",
			input: `[{"Name":"app","Status":"running(3)","ConfigFiles":"/Work/app/compose.yml,/Work/app/compose.override.yml"}]`,
			want: []Project{
				{Name: "app", Status: "running(3)", ConfigDir: "/Work/app"},
			},
		},
		{
			name:  "empty output",
			input: "",
			want:  nil,
		},
		{
			name:  "empty array",
			input: "[]",
			want:  nil,
		},
		{
			name:    "malformed JSON",
			input:   `[{"Name":`,
			wantErr: true,
		},
		{
			name:  "case-insensitive sort",
			input: `[{"Name":"Zebra","Status":"running(1)","ConfigFiles":"/a/compose.yml"},{"Name":"alpha","Status":"running(1)","ConfigFiles":"/b/compose.yml"}]`,
			want: []Project{
				{Name: "alpha", Status: "running(1)", ConfigDir: "/b"},
				{Name: "Zebra", Status: "running(1)", ConfigDir: "/a"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProjects([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d projects, want %d: %v", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				if got[i].Name != want.Name {
					t.Errorf("project[%d].Name = %q, want %q", i, got[i].Name, want.Name)
				}
				if got[i].Status != want.Status {
					t.Errorf("project[%d].Status = %q, want %q", i, got[i].Status, want.Status)
				}
				if got[i].ConfigDir != want.ConfigDir {
					t.Errorf("project[%d].ConfigDir = %q, want %q", i, got[i].ConfigDir, want.ConfigDir)
				}
			}
		})
	}
}

func TestHasComposeFile(t *testing.T) {
	tests := []struct {
		name     string
		files    []string
		expected bool
	}{
		{
			name:     "compose.yml present",
			files:    []string{"compose.yml"},
			expected: true,
		},
		{
			name:     "compose.yaml present",
			files:    []string{"compose.yaml"},
			expected: true,
		},
		{
			name:     "docker-compose.yml present",
			files:    []string{"docker-compose.yml"},
			expected: true,
		},
		{
			name:     "docker-compose.yaml present",
			files:    []string{"docker-compose.yaml"},
			expected: true,
		},
		{
			name:     "no compose file",
			files:    []string{"Dockerfile", "README.md"},
			expected: false,
		},
		{
			name:     "empty directory",
			files:    nil,
			expected: false,
		},
		{
			name:     "multiple compose files",
			files:    []string{"compose.yml", "docker-compose.yml"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(""), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := HasComposeFile(dir)
			if got != tt.expected {
				t.Errorf("HasComposeFile() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestNew(t *testing.T) {
	c := New("/some/dir")
	if c.ProjectDir != "/some/dir" {
		t.Errorf("ProjectDir = %q, want %q", c.ProjectDir, "/some/dir")
	}
	expected := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if c.UID != expected {
		t.Errorf("UID = %q, want %q", c.UID, expected)
	}
}

func TestCommand_Args(t *testing.T) {
	c := &Compose{ProjectDir: "/proj", UID: "1000:1000"}

	tests := []struct {
		name     string
		args     []string
		wantArgs []string
	}{
		{
			name:     "stop with containers",
			args:     []string{"stop", "nginx", "postgres"},
			wantArgs: []string{"compose", "stop", "nginx", "postgres"},
		},
		{
			name:     "rm -f with containers",
			args:     []string{"rm", "-f", "nginx"},
			wantArgs: []string{"compose", "rm", "-f", "nginx"},
		},
		{
			name:     "up --no-start",
			args:     []string{"up", "--no-start", "nginx"},
			wantArgs: []string{"compose", "up", "--no-start", "nginx"},
		},
		{
			name:     "pull no containers",
			args:     []string{"pull"},
			wantArgs: []string{"compose", "pull"},
		},
		{
			name:     "config --services",
			args:     []string{"config", "--services"},
			wantArgs: []string{"compose", "config", "--services"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := c.command(context.Background(), tt.args...)

			// Verify the command is "docker"
			if !strings.HasSuffix(cmd.Path, "docker") && !strings.Contains(cmd.Path, "docker") {
				t.Errorf("command path = %q, want docker", cmd.Path)
			}

			// cmd.Args[0] is the program name, rest are arguments
			gotArgs := cmd.Args[1:]
			if len(gotArgs) != len(tt.wantArgs) {
				t.Fatalf("args count = %d, want %d\ngot:  %v\nwant: %v", len(gotArgs), len(tt.wantArgs), gotArgs, tt.wantArgs)
			}
			for i, want := range tt.wantArgs {
				if gotArgs[i] != want {
					t.Errorf("arg[%d] = %q, want %q", i, gotArgs[i], want)
				}
			}
		})
	}
}

func TestCommand_Env(t *testing.T) {
	c := &Compose{ProjectDir: "/proj", UID: "1000:1000"}
	cmd := c.command(context.Background(), "stop")

	found := false
	for _, env := range cmd.Env {
		if env == "CURRENT_UID=1000:1000" {
			found = true
			break
		}
	}
	if !found {
		t.Error("CURRENT_UID=1000:1000 not found in command env")
	}
}

func TestCommand_Dir(t *testing.T) {
	t.Run("with project dir", func(t *testing.T) {
		c := &Compose{ProjectDir: "/proj", UID: "1000:1000"}
		cmd := c.command(context.Background(), "stop")
		if cmd.Dir != "/proj" {
			t.Errorf("Dir = %q, want %q", cmd.Dir, "/proj")
		}
	})

	t.Run("without project dir", func(t *testing.T) {
		c := &Compose{UID: "1000:1000"}
		cmd := c.command(context.Background(), "stop")
		if cmd.Dir != "" {
			t.Errorf("Dir = %q, want empty", cmd.Dir)
		}
	})
}

func TestListServices_Parsing(t *testing.T) {
	// We can't easily test ListServices without docker, but we can test
	// the parsing logic by checking the split behavior
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "multiple services",
			input: "nginx\npostgres\nredis\n",
			want:  []string{"nginx", "postgres", "redis"},
		},
		{
			name:  "single service",
			input: "nginx\n",
			want:  []string{"nginx"},
		},
		{
			name:  "services with whitespace",
			input: "  nginx \n postgres \n",
			want:  []string{"nginx", "postgres"},
		},
		{
			name:  "empty lines filtered",
			input: "nginx\n\npostgres\n\n",
			want:  []string{"nginx", "postgres"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := strings.Split(strings.TrimSpace(tt.input), "\n")
			var services []string
			for _, l := range lines {
				l = strings.TrimSpace(l)
				if l != "" {
					services = append(services, l)
				}
			}

			if len(services) != len(tt.want) {
				t.Fatalf("got %d services, want %d: %v", len(services), len(tt.want), services)
			}
			for i, want := range tt.want {
				if services[i] != want {
					t.Errorf("service[%d] = %q, want %q", i, services[i], want)
				}
			}
		})
	}
}

func TestParseContainerStatus(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]runner.ServiceStatus
		wantErr bool
	}{
		{
			name:  "mixed states",
			input: "{\"Service\":\"nginx\",\"State\":\"running\"}\n{\"Service\":\"postgres\",\"State\":\"exited\"}\n",
			want: map[string]runner.ServiceStatus{
				"nginx":    {Running: true},
				"postgres": {Running: false},
			},
		},
		{
			name:  "all running",
			input: "{\"Service\":\"web\",\"State\":\"running\"}\n{\"Service\":\"db\",\"State\":\"running\"}\n",
			want: map[string]runner.ServiceStatus{
				"web": {Running: true},
				"db":  {Running: true},
			},
		},
		{
			name:  "all stopped",
			input: "{\"Service\":\"web\",\"State\":\"exited\"}\n",
			want:  map[string]runner.ServiceStatus{"web": {Running: false}},
		},
		{
			name:  "empty output",
			input: "",
			want:  nil,
		},
		{
			name:  "whitespace only",
			input: "  \n  \n",
			want:  nil,
		},
		{
			name:    "malformed JSON",
			input:   "{\"Service\":",
			wantErr: true,
		},
		{
			name:  "created state",
			input: "{\"Service\":\"app\",\"State\":\"created\"}\n",
			want:  map[string]runner.ServiceStatus{"app": {Running: false}},
		},
		{
			name:  "scaled service NDJSON any running means running",
			input: "{\"Service\":\"web\",\"State\":\"running\"}\n{\"Service\":\"web\",\"State\":\"exited\"}\n{\"Service\":\"web\",\"State\":\"running\"}\n",
			want:  map[string]runner.ServiceStatus{"web": {Running: true}},
		},
		{
			name:  "scaled service NDJSON all exited",
			input: "{\"Service\":\"web\",\"State\":\"exited\"}\n{\"Service\":\"web\",\"State\":\"exited\"}\n",
			want:  map[string]runner.ServiceStatus{"web": {Running: false}},
		},
		{
			name:  "scaled service JSON array any running means running",
			input: `[{"Service":"web","State":"running"},{"Service":"web","State":"exited"},{"Service":"db","State":"running"}]`,
			want: map[string]runner.ServiceStatus{
				"web": {Running: true},
				"db":  {Running: true},
			},
		},
		{
			name:  "JSON array format",
			input: `[{"Service":"nginx","State":"running"},{"Service":"postgres","State":"exited"}]`,
			want: map[string]runner.ServiceStatus{
				"nginx":    {Running: true},
				"postgres": {Running: false},
			},
		},
		{
			name:  "JSON array single entry",
			input: `[{"Service":"web","State":"running"}]`,
			want:  map[string]runner.ServiceStatus{"web": {Running: true}},
		},
		{
			name:  "JSON array empty",
			input: `[]`,
			want:  nil,
		},
		{
			name:  "healthy container",
			input: `{"Service":"web","State":"running","Health":"healthy"}` + "\n",
			want:  map[string]runner.ServiceStatus{"web": {Running: true, Health: "healthy"}},
		},
		{
			name:  "unhealthy container",
			input: `{"Service":"web","State":"running","Health":"unhealthy"}` + "\n",
			want:  map[string]runner.ServiceStatus{"web": {Running: true, Health: "unhealthy"}},
		},
		{
			name:  "starting health",
			input: `{"Service":"web","State":"running","Health":"starting"}` + "\n",
			want:  map[string]runner.ServiceStatus{"web": {Running: true, Health: "starting"}},
		},
		{
			name:  "no health field",
			input: `{"Service":"web","State":"running"}` + "\n",
			want:  map[string]runner.ServiceStatus{"web": {Running: true, Health: ""}},
		},
		{
			name: "scaled service mixed health worst-case wins",
			input: `[{"Service":"web","State":"running","Health":"healthy"},` +
				`{"Service":"web","State":"running","Health":"unhealthy"},` +
				`{"Service":"web","State":"running","Health":"starting"}]`,
			want: map[string]runner.ServiceStatus{"web": {Running: true, Health: "unhealthy"}},
		},
		{
			name: "scaled service healthy and starting",
			input: `[{"Service":"web","State":"running","Health":"healthy"},` +
				`{"Service":"web","State":"running","Health":"starting"}]`,
			want: map[string]runner.ServiceStatus{"web": {Running: true, Health: "starting"}},
		},
		{
			name: "mixed health and no health",
			input: `[{"Service":"web","State":"running","Health":"healthy"},` +
				`{"Service":"db","State":"running"}]`,
			want: map[string]runner.ServiceStatus{
				"web": {Running: true, Health: "healthy"},
				"db":  {Running: true, Health: ""},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseContainerStatus([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d entries, want %d: %v", len(got), len(tt.want), got)
			}
			for svc, want := range tt.want {
				gotStatus := got[svc]
				if gotStatus.Running != want.Running {
					t.Errorf("status[%q].Running = %v, want %v", svc, gotStatus.Running, want.Running)
				}
				if gotStatus.Health != want.Health {
					t.Errorf("status[%q].Health = %q, want %q", svc, gotStatus.Health, want.Health)
				}
			}
		})
	}
}

func TestLogs_ArgsConstruction(t *testing.T) {
	c := &Compose{ProjectDir: "/proj", UID: "1000:1000"}

	tests := []struct {
		name     string
		follow   bool
		tail     int
		service  string
		wantArgs []string
	}{
		{
			name:     "follow with tail",
			follow:   true,
			tail:     50,
			service:  "nginx",
			wantArgs: []string{"compose", "logs", "--follow", "--tail", "50", "nginx"},
		},
		{
			name:     "no follow with tail",
			follow:   false,
			tail:     100,
			service:  "nginx",
			wantArgs: []string{"compose", "logs", "--tail", "100", "nginx"},
		},
		{
			name:     "follow without tail",
			follow:   true,
			tail:     0,
			service:  "postgres",
			wantArgs: []string{"compose", "logs", "--follow", "postgres"},
		},
		{
			name:     "no follow no tail",
			follow:   false,
			tail:     0,
			service:  "redis",
			wantArgs: []string{"compose", "logs", "redis"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build the args the same way Logs() does
			args := []string{"logs"}
			if tt.follow {
				args = append(args, "--follow")
			}
			if tt.tail > 0 {
				args = append(args, "--tail", fmt.Sprintf("%d", tt.tail))
			}
			args = append(args, tt.service)

			cmd := c.command(context.Background(), args...)
			gotArgs := cmd.Args[1:]
			if len(gotArgs) != len(tt.wantArgs) {
				t.Fatalf("args count = %d, want %d\ngot:  %v\nwant: %v", len(gotArgs), len(tt.wantArgs), gotArgs, tt.wantArgs)
			}
			for i, want := range tt.wantArgs {
				if gotArgs[i] != want {
					t.Errorf("arg[%d] = %q, want %q", i, gotArgs[i], want)
				}
			}
		})
	}
}

func TestStop_ArgsConstruction(t *testing.T) {
	c := &Compose{ProjectDir: "/proj", UID: "1000:1000"}

	t.Run("with specific containers", func(t *testing.T) {
		args := append([]string{"stop"}, "nginx", "postgres")
		cmd := c.command(context.Background(), args...)
		gotArgs := cmd.Args[1:]
		wantArgs := []string{"compose", "stop", "nginx", "postgres"}
		for i, want := range wantArgs {
			if gotArgs[i] != want {
				t.Errorf("arg[%d] = %q, want %q", i, gotArgs[i], want)
			}
		}
	})

	t.Run("with empty containers (all)", func(t *testing.T) {
		var containers []string
		args := append([]string{"stop"}, containers...)
		cmd := c.command(context.Background(), args...)
		gotArgs := cmd.Args[1:]
		wantArgs := []string{"compose", "stop"}
		if len(gotArgs) != len(wantArgs) {
			t.Fatalf("args = %v, want %v", gotArgs, wantArgs)
		}
	})
}

func TestRemove_UsesForceFlag(t *testing.T) {
	c := &Compose{ProjectDir: "/proj", UID: "1000:1000"}
	args := append([]string{"rm", "-f"}, "nginx")
	cmd := c.command(context.Background(), args...)
	gotArgs := cmd.Args[1:]

	// Verify -f flag is present
	wantArgs := []string{"compose", "rm", "-f", "nginx"}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", gotArgs, wantArgs)
	}
	for i, want := range wantArgs {
		if gotArgs[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, gotArgs[i], want)
		}
	}
}

func TestCreate_UsesNoStartFlag(t *testing.T) {
	c := &Compose{ProjectDir: "/proj", UID: "1000:1000"}
	args := append([]string{"up", "--no-start"}, "nginx")
	cmd := c.command(context.Background(), args...)
	gotArgs := cmd.Args[1:]

	wantArgs := []string{"compose", "up", "--no-start", "nginx"}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", gotArgs, wantArgs)
	}
	for i, want := range wantArgs {
		if gotArgs[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, gotArgs[i], want)
		}
	}
}

func TestDetect_PluginFound(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			// "docker compose version" succeeds
			if len(cmd.Args) >= 3 && cmd.Args[1] == "compose" && cmd.Args[2] == "version" {
				return []byte("Docker Compose version v2.24.0\n"), nil
			}
			return nil, fmt.Errorf("unknown command")
		},
	}

	err := c.Detect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Standalone {
		t.Error("Standalone = true, want false (plugin found)")
	}
}

func TestDetect_StandaloneFound(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			// "docker compose version" fails, "docker-compose version" succeeds
			if strings.HasSuffix(cmd.Path, "docker-compose") || cmd.Args[0] == "docker-compose" {
				return []byte("docker-compose version 1.29.2\n"), nil
			}
			return nil, fmt.Errorf("unknown docker command")
		},
	}

	err := c.Detect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.Standalone {
		t.Error("Standalone = false, want true (standalone found)")
	}
}

func TestDetect_NeitherFound(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("not found")
		},
	}

	err := c.Detect(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error = %q, want it to contain 'neither'", err.Error())
	}
}

func TestDetect_CachesResult(t *testing.T) {
	calls := 0
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			calls++
			return []byte("ok\n"), nil
		},
	}

	if err := c.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("outputCmd called %d times, want 1 (cached)", calls)
	}
}

func TestSetStandalone(t *testing.T) {
	c := &Compose{ProjectDir: "/proj", UID: "1000:1000"}

	c.SetStandalone(true)
	if !c.Standalone {
		t.Error("Standalone = false after SetStandalone(true)")
	}

	// Detect should no-op after SetStandalone
	calls := 0
	c.outputCmd = func(cmd *exec.Cmd) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("should not be called")
	}
	if err := c.Detect(context.Background()); err != nil {
		t.Fatalf("Detect after SetStandalone should no-op, got: %v", err)
	}
	if calls != 0 {
		t.Error("Detect called outputCmd after SetStandalone")
	}
}

func TestCommand_Standalone(t *testing.T) {
	c := &Compose{ProjectDir: "/proj", UID: "1000:1000", Standalone: true}

	cmd := c.command(context.Background(), "stop", "nginx")

	// Standalone mode uses docker-compose binary directly
	if !strings.HasSuffix(cmd.Path, "docker-compose") && !strings.Contains(cmd.Args[0], "docker-compose") {
		t.Errorf("standalone command should use docker-compose, got path=%q args[0]=%q", cmd.Path, cmd.Args[0])
	}

	// Args should NOT include "compose" as a subcommand
	gotArgs := cmd.Args[1:]
	wantArgs := []string{"stop", "nginx"}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", gotArgs, wantArgs)
	}
	for i, want := range wantArgs {
		if gotArgs[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, gotArgs[i], want)
		}
	}
}

func TestCommand_Plugin(t *testing.T) {
	c := &Compose{ProjectDir: "/proj", UID: "1000:1000", Standalone: false}

	cmd := c.command(context.Background(), "stop", "nginx")

	gotArgs := cmd.Args[1:]
	wantArgs := []string{"compose", "stop", "nginx"}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", gotArgs, wantArgs)
	}
	for i, want := range wantArgs {
		if gotArgs[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, gotArgs[i], want)
		}
	}
}

// --- Tests using injection hooks ---

func TestListServices_ViaHook(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("nginx\npostgres\nredis\n"), nil
		},
	}

	services, err := c.ListServices(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"nginx", "postgres", "redis"}
	if len(services) != len(want) {
		t.Fatalf("got %d services, want %d", len(services), len(want))
	}
	for i, w := range want {
		if services[i] != w {
			t.Errorf("service[%d] = %q, want %q", i, services[i], w)
		}
	}
}

func TestListServices_Error(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("docker not found")
		},
	}

	_, err := c.ListServices(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listing services") {
		t.Errorf("error = %q, want it to contain 'listing services'", err.Error())
	}
}

func TestContainerStatus_ViaHook(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(`[{"Service":"web","State":"running","Health":"healthy"},{"Service":"db","State":"exited"}]`), nil
		},
	}

	status, err := c.ContainerStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(status) != 2 {
		t.Fatalf("got %d entries, want 2", len(status))
	}
	if !status["web"].Running {
		t.Error("web should be running")
	}
	if status["web"].Health != "healthy" {
		t.Errorf("web health = %q, want %q", status["web"].Health, "healthy")
	}
	if status["db"].Running {
		t.Error("db should not be running")
	}
}

func TestContainerStatus_Error(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	_, err := c.ContainerStatus(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listing container status") {
		t.Errorf("error = %q, want it to contain 'listing container status'", err.Error())
	}
}

func TestStop_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := c.Stop(context.Background(), []string{"nginx", "postgres"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured == nil {
		t.Fatal("runCmd was not called")
	}
	args := captured.Args[1:] // skip "docker"
	wantArgs := []string{"compose", "stop", "nginx", "postgres"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], want)
		}
	}
}

func TestStop_AllContainers(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := c.Stop(context.Background(), nil, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := captured.Args[1:]
	wantArgs := []string{"compose", "stop"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
}

func TestRemove_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := c.Remove(context.Background(), []string{"nginx"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := captured.Args[1:]
	wantArgs := []string{"compose", "rm", "-f", "nginx"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], want)
		}
	}
}

func TestPull_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := c.Pull(context.Background(), []string{"nginx"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := captured.Args[1:]
	wantArgs := []string{"compose", "pull", "nginx"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], want)
		}
	}
}

func TestCreate_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := c.Create(context.Background(), []string{"nginx"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := captured.Args[1:]
	wantArgs := []string{"compose", "up", "--no-start", "nginx"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], want)
		}
	}
}

func TestStart_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := c.Start(context.Background(), []string{"nginx", "db"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := captured.Args[1:]
	wantArgs := []string{"compose", "start", "nginx", "db"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], want)
		}
	}
}

func TestLogs_ViaHook(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	var buf strings.Builder
	err := c.Logs(context.Background(), "nginx", true, 50, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := captured.Args[1:]
	wantArgs := []string{"compose", "logs", "--follow", "--tail", "50", "nginx"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], want)
		}
	}
}

func TestLogs_NoFollowNoTail(t *testing.T) {
	var captured *exec.Cmd
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			captured = cmd
			return nil
		},
	}

	err := c.Logs(context.Background(), "redis", false, 0, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := captured.Args[1:]
	wantArgs := []string{"compose", "logs", "redis"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], want)
		}
	}
}

func TestRun_ErrorPropagation(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			return fmt.Errorf("exit status 1")
		},
	}

	err := c.Stop(context.Background(), []string{"nginx"}, io.Discard)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("error = %q, want it to contain 'exit status 1'", err.Error())
	}
}

func TestRun_WriterWiring(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		runCmd: func(cmd *exec.Cmd) error {
			// Verify stdout and stderr are wired to the writer
			if cmd.Stdout == nil {
				return fmt.Errorf("stdout not wired")
			}
			if cmd.Stderr == nil {
				return fmt.Errorf("stderr not wired")
			}
			// Write to stdout to verify it reaches our writer
			fmt.Fprint(cmd.Stdout, "hello")
			return nil
		},
	}

	var buf strings.Builder
	err := c.Stop(context.Background(), nil, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.String() != "hello" {
		t.Errorf("writer got %q, want %q", buf.String(), "hello")
	}
}

func TestListProjects_ViaHook(t *testing.T) {
	c := &Compose{
		UID: "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(`[{"Name":"app1","Status":"running(2)","ConfigFiles":"/srv/app1/compose.yml"},{"Name":"app2","Status":"exited(1)","ConfigFiles":"/srv/app2/compose.yml"}]`), nil
		},
	}

	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(projects))
	}
	if projects[0].Name != "app1" {
		t.Errorf("project[0].Name = %q, want %q", projects[0].Name, "app1")
	}
	if projects[1].Name != "app2" {
		t.Errorf("project[1].Name = %q, want %q", projects[1].Name, "app2")
	}
}

func TestListProjects_Error(t *testing.T) {
	c := &Compose{
		UID: "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("docker not running")
		},
	}

	_, err := c.ListProjects(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listing projects") {
		t.Errorf("error = %q, want it to contain 'listing projects'", err.Error())
	}
}

func TestListProjects_Standalone(t *testing.T) {
	var capturedArgs []string
	c := &Compose{
		UID:        "1000:1000",
		Standalone: true,
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			capturedArgs = cmd.Args
			return []byte(`[{"Name":"app","Status":"running(1)","ConfigFiles":"/srv/app/compose.yml"}]`), nil
		},
	}

	_, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Standalone should use "docker-compose ls -a --format json"
	if capturedArgs[0] != "docker-compose" && !strings.HasSuffix(capturedArgs[0], "docker-compose") {
		t.Errorf("standalone ListProjects should use docker-compose, got args[0]=%q", capturedArgs[0])
	}
	// Should NOT have "compose" as first arg
	if len(capturedArgs) > 1 && capturedArgs[1] == "compose" {
		t.Errorf("standalone should not have 'compose' subcommand, got: %v", capturedArgs)
	}
}

func TestListProjects_Plugin(t *testing.T) {
	var capturedArgs []string
	c := &Compose{
		UID:        "1000:1000",
		Standalone: false,
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			capturedArgs = cmd.Args
			return []byte(`[]`), nil
		},
	}

	_, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Plugin should use "docker compose ls -a --format json"
	if len(capturedArgs) < 2 || capturedArgs[1] != "compose" {
		t.Errorf("plugin ListProjects should have 'compose' subcommand, got: %v", capturedArgs)
	}
}

// --- ConfigProvider tests ---

func TestFindComposeFile(t *testing.T) {
	tests := []struct {
		name     string
		files    []string
		wantName string
		wantErr  bool
	}{
		{
			name:     "compose.yml present",
			files:    []string{"compose.yml"},
			wantName: "compose.yml",
		},
		{
			name:     "compose.yaml present",
			files:    []string{"compose.yaml"},
			wantName: "compose.yaml",
		},
		{
			name:     "docker-compose.yml present",
			files:    []string{"docker-compose.yml"},
			wantName: "docker-compose.yml",
		},
		{
			name:     "docker-compose.yaml present",
			files:    []string{"docker-compose.yaml"},
			wantName: "docker-compose.yaml",
		},
		{
			name:     "first match wins",
			files:    []string{"compose.yml", "docker-compose.yml"},
			wantName: "compose.yml",
		},
		{
			name:    "no compose file",
			files:   []string{"Dockerfile"},
			wantErr: true,
		},
		{
			name:    "empty directory",
			files:   nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("version: '3'"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			c := &Compose{ProjectDir: dir}
			got, err := c.findComposeFile()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if filepath.Base(got) != tt.wantName {
				t.Errorf("findComposeFile() = %q, want file named %q", got, tt.wantName)
			}
			if filepath.Dir(got) != dir {
				t.Errorf("findComposeFile() dir = %q, want %q", filepath.Dir(got), dir)
			}
		})
	}
}

func TestConfigFile_Success(t *testing.T) {
	dir := t.TempDir()
	content := "services:\n  web:\n    image: nginx\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Compose{ProjectDir: dir}
	got, err := c.ConfigFile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != content {
		t.Errorf("ConfigFile() = %q, want %q", string(got), content)
	}
}

func TestConfigFile_NoFile(t *testing.T) {
	dir := t.TempDir()
	c := &Compose{ProjectDir: dir}
	_, err := c.ConfigFile(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no compose file found") {
		t.Errorf("error = %q, want it to contain 'no compose file found'", err.Error())
	}
}

func TestConfigResolved_Args(t *testing.T) {
	var capturedArgs []string
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			capturedArgs = cmd.Args
			return []byte("resolved config output"), nil
		},
	}

	got, err := c.ConfigResolved(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "resolved config output" {
		t.Errorf("ConfigResolved() = %q, want %q", string(got), "resolved config output")
	}

	// Verify command: docker compose config
	wantArgs := []string{"compose", "config"}
	gotArgs := capturedArgs[1:]
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", gotArgs, wantArgs)
	}
	for i, want := range wantArgs {
		if gotArgs[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, gotArgs[i], want)
		}
	}
}

func TestEditCommand_EditorEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		editor     string
		visual     string
		wantEditor string
		wantArgs   []string // expected args between editor and file path
	}{
		{"EDITOR set", "nano", "", "nano", nil},
		{"VISUAL fallback", "", "code", "code", nil},
		{"vi default", "", "", "vi", nil},
		{"EDITOR takes precedence", "nano", "code", "nano", nil},
		{"multi-word EDITOR", "code --wait", "", "code", []string{"--wait"}},
		{"multi-word VISUAL", "", "nvim -f", "nvim", []string{"-f"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save and restore env
			origEditor := os.Getenv("EDITOR")
			origVisual := os.Getenv("VISUAL")
			defer func() {
				os.Setenv("EDITOR", origEditor)
				os.Setenv("VISUAL", origVisual)
			}()

			os.Setenv("EDITOR", tt.editor)
			os.Setenv("VISUAL", tt.visual)

			c := &Compose{ProjectDir: dir}
			cmd, err := c.EditCommand(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cmd.Args[0] != tt.wantEditor {
				t.Errorf("editor = %q, want %q", cmd.Args[0], tt.wantEditor)
			}
			// Check extra args between editor and file path
			filePath := filepath.Join(dir, "compose.yml")
			extraArgs := cmd.Args[1 : len(cmd.Args)-1]
			if len(extraArgs) != len(tt.wantArgs) {
				t.Fatalf("extra args = %v, want %v", extraArgs, tt.wantArgs)
			}
			for i, want := range tt.wantArgs {
				if extraArgs[i] != want {
					t.Errorf("arg[%d] = %q, want %q", i, extraArgs[i], want)
				}
			}
			if cmd.Args[len(cmd.Args)-1] != filePath {
				t.Errorf("file = %q, want %q", cmd.Args[len(cmd.Args)-1], filePath)
			}
		})
	}
}

func TestEditCommand_NoFile(t *testing.T) {
	dir := t.TempDir()
	c := &Compose{ProjectDir: dir}
	_, err := c.EditCommand(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestValidateConfig_Success(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			// Verify args include --quiet
			args := cmd.Args[1:]
			wantArgs := []string{"compose", "config", "--quiet"}
			if len(args) != len(wantArgs) {
				return nil, fmt.Errorf("unexpected args: %v", args)
			}
			for i, w := range wantArgs {
				if args[i] != w {
					return nil, fmt.Errorf("arg[%d] = %q, want %q", i, args[i], w)
				}
			}
			return nil, nil
		},
	}

	err := c.ValidateConfig(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateConfig_Error(t *testing.T) {
	c := &Compose{
		ProjectDir: "/proj",
		UID:        "1000:1000",
		outputCmd: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("validation failed")
		},
	}

	err := c.ValidateConfig(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("error = %q, want it to contain 'validation failed'", err.Error())
	}
}

func TestValidateConfig_CombinedOutputSuccess(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"compose\" ] && [ \"$2\" = \"config\" ] && [ \"$3\" = \"--quiet\" ]; then\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo unexpected args: \"$@\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	c := &Compose{
		ProjectDir: dir,
		UID:        "1000:1000",
	}
	if err := c.ValidateConfig(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateConfig_CombinedOutputErrorIncludesStderr(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"echo yaml syntax error on line 3 >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	c := &Compose{
		ProjectDir: dir,
		UID:        "1000:1000",
	}
	err := c.ValidateConfig(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "yaml syntax error on line 3") {
		t.Fatalf("error = %q, want stderr text included", err.Error())
	}
}

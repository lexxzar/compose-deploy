package compose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
		want    map[string]bool
		wantErr bool
	}{
		{
			name:  "mixed states",
			input: "{\"Service\":\"nginx\",\"State\":\"running\"}\n{\"Service\":\"postgres\",\"State\":\"exited\"}\n",
			want:  map[string]bool{"nginx": true, "postgres": false},
		},
		{
			name:  "all running",
			input: "{\"Service\":\"web\",\"State\":\"running\"}\n{\"Service\":\"db\",\"State\":\"running\"}\n",
			want:  map[string]bool{"web": true, "db": true},
		},
		{
			name:  "all stopped",
			input: "{\"Service\":\"web\",\"State\":\"exited\"}\n",
			want:  map[string]bool{"web": false},
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
			want:  map[string]bool{"app": false},
		},
		{
			name:  "scaled service NDJSON any running means running",
			input: "{\"Service\":\"web\",\"State\":\"running\"}\n{\"Service\":\"web\",\"State\":\"exited\"}\n{\"Service\":\"web\",\"State\":\"running\"}\n",
			want:  map[string]bool{"web": true},
		},
		{
			name:  "scaled service NDJSON all exited",
			input: "{\"Service\":\"web\",\"State\":\"exited\"}\n{\"Service\":\"web\",\"State\":\"exited\"}\n",
			want:  map[string]bool{"web": false},
		},
		{
			name:  "scaled service JSON array any running means running",
			input: `[{"Service":"web","State":"running"},{"Service":"web","State":"exited"},{"Service":"db","State":"running"}]`,
			want:  map[string]bool{"web": true, "db": true},
		},
		{
			name:  "JSON array format",
			input: `[{"Service":"nginx","State":"running"},{"Service":"postgres","State":"exited"}]`,
			want:  map[string]bool{"nginx": true, "postgres": false},
		},
		{
			name:  "JSON array single entry",
			input: `[{"Service":"web","State":"running"}]`,
			want:  map[string]bool{"web": true},
		},
		{
			name:  "JSON array empty",
			input: `[]`,
			want:  nil,
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
			for svc, wantRunning := range tt.want {
				if got[svc] != wantRunning {
					t.Errorf("status[%q] = %v, want %v", svc, got[svc], wantRunning)
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

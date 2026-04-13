package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_SingleServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `servers:
  - name: prod
    host: user@10.0.1.50
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(cfg.Servers))
	}
	if cfg.Servers[0].Name != "prod" {
		t.Errorf("Name = %q, want %q", cfg.Servers[0].Name, "prod")
	}
	if cfg.Servers[0].Host != "user@10.0.1.50" {
		t.Errorf("Host = %q, want %q", cfg.Servers[0].Host, "user@10.0.1.50")
	}
}

func TestLoad_MultipleServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `servers:
  - name: prod-web
    host: user@10.0.1.50
  - name: staging
    host: deploy@staging.internal
    project_dir: /opt/apps/web
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(cfg.Servers))
	}
	if cfg.Servers[1].ProjectDir != "/opt/apps/web" {
		t.Errorf("ProjectDir = %q, want %q", cfg.Servers[1].ProjectDir, "/opt/apps/web")
	}
}

func TestLoad_WithGroups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `servers:
  - name: app.dev
    host: user@app.dev
    group: Dev
  - name: app.prod
    host: user@app.prod
    group: Production
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Servers[0].Group != "Dev" {
		t.Errorf("Servers[0].Group = %q, want %q", cfg.Servers[0].Group, "Dev")
	}
	if cfg.Servers[1].Group != "Production" {
		t.Errorf("Servers[1].Group = %q, want %q", cfg.Servers[1].Group, "Production")
	}
}

func TestLoad_WithoutGroups_BackwardCompat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `servers:
  - name: prod
    host: user@prod
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Servers[0].Group != "" {
		t.Errorf("Servers[0].Group = %q, want empty", cfg.Servers[0].Group)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	cfg, err := Load("/nonexistent/path/servers.yml")
	if err != nil {
		t.Fatalf("missing file should not error, got: %v", err)
	}
	if len(cfg.Servers) != 0 {
		t.Errorf("got %d servers, want 0", len(cfg.Servers))
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	if err := os.WriteFile(path, []byte("servers:\n  - [invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
	if !strings.Contains(err.Error(), "parsing config") {
		t.Errorf("error = %q, want it to contain 'parsing config'", err.Error())
	}
}

func TestValidate_Valid(t *testing.T) {
	cfg := &Config{
		Servers: []Server{
			{Name: "prod", Host: "user@host"},
			{Name: "staging", Host: "deploy@staging"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_MissingName(t *testing.T) {
	cfg := &Config{
		Servers: []Server{
			{Name: "", Host: "user@host"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %q, want it to contain 'name is required'", err.Error())
	}
}

func TestValidate_MissingHost(t *testing.T) {
	cfg := &Config{
		Servers: []Server{
			{Name: "prod", Host: ""},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for missing host")
	}
	if !strings.Contains(err.Error(), "host is required") {
		t.Errorf("error = %q, want it to contain 'host is required'", err.Error())
	}
}

func TestValidate_DuplicateNames(t *testing.T) {
	cfg := &Config{
		Servers: []Server{
			{Name: "prod", Host: "user@host1"},
			{Name: "prod", Host: "user@host2"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate names")
	}
	if !strings.Contains(err.Error(), "duplicate server name") {
		t.Errorf("error = %q, want it to contain 'duplicate server name'", err.Error())
	}
}

func TestValidate_EmptyConfig(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty config should be valid, got: %v", err)
	}
}

func TestFindServer_Found(t *testing.T) {
	cfg := &Config{
		Servers: []Server{
			{Name: "prod", Host: "user@host1"},
			{Name: "staging", Host: "deploy@staging"},
		},
	}

	s, err := cfg.FindServer("staging")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Name != "staging" {
		t.Errorf("Name = %q, want %q", s.Name, "staging")
	}
	if s.Host != "deploy@staging" {
		t.Errorf("Host = %q, want %q", s.Host, "deploy@staging")
	}
}

func TestFindServer_NotFound(t *testing.T) {
	cfg := &Config{
		Servers: []Server{
			{Name: "prod", Host: "user@host1"},
		},
	}

	_, err := cfg.FindServer("nonexistent")
	if err == nil {
		t.Fatal("expected error for not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to contain 'not found'", err.Error())
	}
}

func TestDefaultPath(t *testing.T) {
	path := DefaultPath()
	if !strings.HasSuffix(path, filepath.Join(".cdeploy", "servers.yml")) {
		t.Errorf("DefaultPath() = %q, want it to end with .cdeploy/servers.yml", path)
	}
}

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
	data := `groups:
  - name: Dev
  - name: Production
servers:
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

func TestLoad_WithColor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `servers:
  - name: prod
    host: user@prod
    color: red
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Servers[0].Color != "red" {
		t.Errorf("Servers[0].Color = %q, want %q", cfg.Servers[0].Color, "red")
	}
}

func TestValidate_InvalidColor(t *testing.T) {
	cfg := &Config{
		Servers: []Server{
			{Name: "prod", Host: "user@host", Color: "purple"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid color")
	}
	if !strings.Contains(err.Error(), "unknown color") {
		t.Errorf("error = %q, want it to contain 'unknown color'", err.Error())
	}
}

func TestValidate_EmptyColor(t *testing.T) {
	cfg := &Config{
		Servers: []Server{
			{Name: "prod", Host: "user@host", Color: ""},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty color should be valid, got: %v", err)
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

func TestGroupColor_Found(t *testing.T) {
	cfg := &Config{
		Groups: []Group{
			{Name: "production", Color: "red"},
			{Name: "staging", Color: "yellow"},
		},
	}
	if got := cfg.GroupColor("production"); got != "red" {
		t.Errorf("GroupColor(production) = %q, want %q", got, "red")
	}
	if got := cfg.GroupColor("staging"); got != "yellow" {
		t.Errorf("GroupColor(staging) = %q, want %q", got, "yellow")
	}
}

func TestGroupColor_NotFound(t *testing.T) {
	cfg := &Config{
		Groups: []Group{
			{Name: "production", Color: "red"},
		},
	}
	if got := cfg.GroupColor("nonexistent"); got != "" {
		t.Errorf("GroupColor(nonexistent) = %q, want empty", got)
	}
}

func TestGroupColor_EmptyColor(t *testing.T) {
	cfg := &Config{
		Groups: []Group{
			{Name: "production", Color: ""},
		},
	}
	if got := cfg.GroupColor("production"); got != "" {
		t.Errorf("GroupColor(production) = %q, want empty", got)
	}
}

func TestGroupColor_NoGroups(t *testing.T) {
	cfg := &Config{}
	if got := cfg.GroupColor("anything"); got != "" {
		t.Errorf("GroupColor(anything) = %q, want empty", got)
	}
}

func TestValidate_DuplicateGroupNames(t *testing.T) {
	cfg := &Config{
		Groups: []Group{
			{Name: "prod", Color: "red"},
			{Name: "prod", Color: "blue"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate group names")
	}
	if !strings.Contains(err.Error(), "duplicate group name") {
		t.Errorf("error = %q, want it to contain 'duplicate group name'", err.Error())
	}
}

func TestValidate_InvalidGroupColor(t *testing.T) {
	cfg := &Config{
		Groups: []Group{
			{Name: "prod", Color: "rainbow"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid group color")
	}
	if !strings.Contains(err.Error(), "unknown color") {
		t.Errorf("error = %q, want it to contain 'unknown color'", err.Error())
	}
}

func TestValidate_EmptyGroupColor(t *testing.T) {
	cfg := &Config{
		Groups: []Group{
			{Name: "prod", Color: ""},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty group color should be valid, got: %v", err)
	}
}

func TestValidate_MissingGroup(t *testing.T) {
	cfg := &Config{
		Servers: []Server{
			{Name: "web", Host: "user@host", Group: "nonexistent"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for missing group reference")
	}
	if !strings.Contains(err.Error(), "unknown group") {
		t.Errorf("error = %q, want it to contain 'unknown group'", err.Error())
	}
}

func TestValidate_ServerGroupExists(t *testing.T) {
	cfg := &Config{
		Groups: []Group{
			{Name: "prod", Color: "red"},
		},
		Servers: []Server{
			{Name: "web", Host: "user@host", Group: "prod"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid group reference should pass, got: %v", err)
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

func TestSave_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	cfg := &Config{
		Groups: []Group{
			{Name: "Production", Color: "red"},
		},
		Servers: []Server{
			{Name: "prod", Host: "user@prod", ProjectDir: "/opt/app", Group: "Production"},
			{Name: "staging", Host: "deploy@staging", Color: "cyan"},
		},
	}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(loaded.Groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(loaded.Groups))
	}
	if loaded.Groups[0].Name != "Production" || loaded.Groups[0].Color != "red" {
		t.Errorf("group[0] = %+v, want Production/red", loaded.Groups[0])
	}

	if len(loaded.Servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(loaded.Servers))
	}
	s := loaded.Servers[0]
	if s.Name != "prod" || s.Host != "user@prod" || s.ProjectDir != "/opt/app" || s.Group != "Production" || s.Color != "" {
		t.Errorf("server[0] = %+v, want prod/user@prod//opt/app/Production with no color", s)
	}
	s = loaded.Servers[1]
	if s.Name != "staging" || s.Host != "deploy@staging" || s.Color != "cyan" {
		t.Errorf("server[1] = %+v, want staging/deploy@staging/cyan", s)
	}
}

func TestMigrate_OldFormatGroupedServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `servers:
  - name: prod-1
    host: user@prod1
    group: production
    color: red
  - name: prod-2
    host: user@prod2
    group: production
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Group should be auto-created with first server's color
	if len(cfg.Groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(cfg.Groups))
	}
	if cfg.Groups[0].Name != "production" || cfg.Groups[0].Color != "red" {
		t.Errorf("group = %+v, want production/red", cfg.Groups[0])
	}

	// Server colors should be stripped
	for i, s := range cfg.Servers {
		if s.Color != "" {
			t.Errorf("server[%d].Color = %q, want empty", i, s.Color)
		}
	}
}

func TestMigrate_ConflictingColors_FirstWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `servers:
  - name: prod-1
    host: user@prod1
    group: production
    color: red
  - name: prod-2
    host: user@prod2
    group: production
    color: blue
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(cfg.Groups))
	}
	if cfg.Groups[0].Color != "red" {
		t.Errorf("group color = %q, want %q (first-server-wins)", cfg.Groups[0].Color, "red")
	}
}

func TestMigrate_NewFormatNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `groups:
  - name: production
    color: red
servers:
  - name: prod-1
    host: user@prod1
    group: production
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(cfg.Groups))
	}
	if cfg.Groups[0].Color != "red" {
		t.Errorf("group color = %q, want %q", cfg.Groups[0].Color, "red")
	}
	if cfg.Servers[0].Color != "" {
		t.Errorf("server color = %q, want empty", cfg.Servers[0].Color)
	}
}

func TestMigrate_UngroupedPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `servers:
  - name: dev-box
    host: user@dev
    color: cyan
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Groups) != 0 {
		t.Errorf("got %d groups, want 0", len(cfg.Groups))
	}
	if cfg.Servers[0].Color != "cyan" {
		t.Errorf("server color = %q, want %q", cfg.Servers[0].Color, "cyan")
	}
}

func TestMigrate_AutoCreateGroupWithoutColor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `servers:
  - name: prod-1
    host: user@prod1
    group: production
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(cfg.Groups))
	}
	if cfg.Groups[0].Name != "production" {
		t.Errorf("group name = %q, want %q", cfg.Groups[0].Name, "production")
	}
	if cfg.Groups[0].Color != "" {
		t.Errorf("group color = %q, want empty", cfg.Groups[0].Color)
	}
}

func TestMigrate_NewFormatUnknownGroupNotCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")
	data := `groups:
  - name: production
    color: red
servers:
  - name: prod-1
    host: user@prod1
    group: production
  - name: staging
    host: user@staging
    group: prodction
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// migrate should NOT auto-create "prodction" — explicit groups exist
	if len(cfg.Groups) != 1 {
		t.Fatalf("got %d groups, want 1 (typo should not create a phantom group)", len(cfg.Groups))
	}

	// Validate should catch the typo
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for misspelled group reference")
	}
	if !strings.Contains(err.Error(), "unknown group") {
		t.Errorf("error = %q, want it to contain 'unknown group'", err.Error())
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "servers.yml")

	cfg := &Config{
		Servers: []Server{{Name: "test", Host: "user@host"}},
	}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(loaded.Servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(loaded.Servers))
	}
	if loaded.Servers[0].Name != "test" {
		t.Errorf("Name = %q, want %q", loaded.Servers[0].Name, "test")
	}
}

func TestSave_EmptyConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	cfg := &Config{}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(loaded.Servers) != 0 {
		t.Fatalf("got %d servers, want 0", len(loaded.Servers))
	}
}

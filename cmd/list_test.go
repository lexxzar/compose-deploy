package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/runner"
	"github.com/spf13/cobra"
)

func TestMergeStatus_AllRunning(t *testing.T) {
	services := []string{"nginx", "postgres"}
	status := map[string]runner.ServiceStatus{
		"nginx":    {Running: true},
		"postgres": {Running: true},
	}

	got := mergeStatus(services, status)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, s := range got {
		if !s.Running {
			t.Errorf("%s: running = false, want true", s.Name)
		}
	}
}

func TestMergeStatus_SomeStopped(t *testing.T) {
	services := []string{"nginx", "redis", "postgres"}
	status := map[string]runner.ServiceStatus{
		"nginx":    {Running: true},
		"postgres": {Running: true},
		"redis":    {Running: false},
	}

	got := mergeStatus(services, status)

	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}

	expected := map[string]bool{"nginx": true, "postgres": true, "redis": false}
	for _, s := range got {
		if s.Running != expected[s.Name] {
			t.Errorf("%s: running = %v, want %v", s.Name, s.Running, expected[s.Name])
		}
	}
}

func TestMergeStatus_AbsentFromStatus(t *testing.T) {
	services := []string{"nginx", "redis"}
	status := map[string]runner.ServiceStatus{
		"nginx": {Running: true},
	}

	got := mergeStatus(services, status)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, s := range got {
		if s.Name == "redis" && s.Running {
			t.Error("redis: running = true, want false (absent from status)")
		}
	}
}

func TestMergeStatus_Empty(t *testing.T) {
	got := mergeStatus(nil, nil)
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestMergeStatus_SortedAlphabetically(t *testing.T) {
	services := []string{"Zebra", "alpha", "middle"}
	status := map[string]runner.ServiceStatus{}

	got := mergeStatus(services, status)

	if got[0].Name != "alpha" || got[1].Name != "middle" || got[2].Name != "Zebra" {
		t.Errorf("order = [%s, %s, %s], want [alpha, middle, Zebra]", got[0].Name, got[1].Name, got[2].Name)
	}
}

func TestMergeStatus_WithHealth(t *testing.T) {
	services := []string{"web", "db"}
	status := map[string]runner.ServiceStatus{
		"web": {Running: true, Health: "healthy"},
		"db":  {Running: true},
	}

	got := mergeStatus(services, status)

	for _, s := range got {
		if s.Name == "web" && s.Health != "healthy" {
			t.Errorf("web health = %q, want %q", s.Health, "healthy")
		}
		if s.Name == "db" && s.Health != "" {
			t.Errorf("db health = %q, want empty", s.Health)
		}
	}
}

func TestFormatDots_Alignment(t *testing.T) {
	items := []serviceStatus{
		{Name: "nginx", Running: true},
		{Name: "postgres", Running: true},
		{Name: "redis", Running: false},
	}

	out := formatDots(items)
	lines := strings.Split(out, "\n")

	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}

	for _, line := range lines {
		if !strings.Contains(line, "running") && !strings.Contains(line, "stopped") {
			t.Errorf("line missing status label: %q", line)
		}
	}

	if !strings.Contains(lines[2], "redis   ") {
		t.Errorf("redis not padded: %q", lines[2])
	}
}

func TestFormatDots_MixedStates(t *testing.T) {
	items := []serviceStatus{
		{Name: "web", Running: true},
		{Name: "db", Running: false},
	}

	out := formatDots(items)

	if !strings.Contains(out, "running") {
		t.Error("missing 'running' label")
	}
	if !strings.Contains(out, "stopped") {
		t.Error("missing 'stopped' label")
	}
}

func TestFormatDots_Empty(t *testing.T) {
	out := formatDots(nil)
	if out != "" {
		t.Errorf("got %q, want empty", out)
	}
}

func TestFormatDots_HealthIcons(t *testing.T) {
	items := []serviceStatus{
		{Name: "web", Running: true, Health: "healthy"},
		{Name: "api", Running: true, Health: "unhealthy"},
		{Name: "worker", Running: true, Health: "starting"},
		{Name: "db", Running: true},
	}

	out := formatDots(items)
	lines := strings.Split(out, "\n")

	if len(lines) != 4 {
		t.Fatalf("lines = %d, want 4", len(lines))
	}

	// Healthy line should contain "♥"
	if !strings.Contains(lines[0], "♥") {
		t.Errorf("healthy line missing ♥ icon: %q", lines[0])
	}
	// Unhealthy line should contain "✗"
	if !strings.Contains(lines[1], "✗") {
		t.Errorf("unhealthy line missing ✗ icon: %q", lines[1])
	}
	// Starting line should contain "~"
	if !strings.Contains(lines[2], "~") {
		t.Errorf("starting line missing ~ icon: %q", lines[2])
	}
}

func TestFormatJSON_RoundTrip(t *testing.T) {
	items := []serviceStatus{
		{Name: "nginx", Running: true},
		{Name: "redis", Running: false},
	}

	out, err := formatJSON(items)
	if err != nil {
		t.Fatal(err)
	}

	var got []serviceStatus
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "nginx" || !got[0].Running {
		t.Errorf("got[0] = %+v, want {nginx, true}", got[0])
	}
	if got[1].Name != "redis" || got[1].Running {
		t.Errorf("got[1] = %+v, want {redis, false}", got[1])
	}
}

func TestFormatJSON_Empty(t *testing.T) {
	out, err := formatJSON([]serviceStatus{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "[]" {
		t.Errorf("got %q, want []", out)
	}
}

func TestFormatJSON_IncludesHealth(t *testing.T) {
	items := []serviceStatus{
		{Name: "web", Running: true, Health: "healthy"},
		{Name: "db", Running: true},
	}

	out, err := formatJSON(items)
	if err != nil {
		t.Fatal(err)
	}

	var got []serviceStatus
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got[0].Health != "healthy" {
		t.Errorf("got[0].Health = %q, want %q", got[0].Health, "healthy")
	}
	// db has no healthcheck, should omit health field
	if got[1].Health != "" {
		t.Errorf("got[1].Health = %q, want empty (omitempty)", got[1].Health)
	}
	// Verify omitempty: raw JSON should not contain "health" for db
	if strings.Contains(out, `"db"`) {
		// Find the db entry in raw JSON
		if strings.Count(out, `"health"`) != 1 {
			t.Errorf("expected health field exactly once (for web only), got JSON: %s", out)
		}
	}
}

func TestFormatDotsGrouped_MultipleProjects(t *testing.T) {
	projects := []projectServices{
		{
			Name: "myapp",
			Services: []serviceStatus{
				{Name: "nginx", Running: true, Health: "healthy"},
				{Name: "postgres", Running: true},
			},
		},
		{
			Name: "monitoring",
			Services: []serviceStatus{
				{Name: "grafana", Running: true},
				{Name: "loki", Running: false},
			},
		},
	}

	out := formatDotsGrouped(projects)

	if !strings.Contains(out, "myapp") {
		t.Error("missing project header 'myapp'")
	}
	if !strings.Contains(out, "monitoring") {
		t.Error("missing project header 'monitoring'")
	}
	// Services should be indented
	for _, line := range strings.Split(out, "\n") {
		if line == "myapp" || line == "monitoring" || line == "" {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("service line not indented: %q", line)
		}
	}
	// Should have blank line between projects
	if !strings.Contains(out, "\n\n") {
		t.Error("missing blank line between projects")
	}
}

func TestFormatDotsGrouped_SingleProject(t *testing.T) {
	projects := []projectServices{
		{
			Name: "myapp",
			Services: []serviceStatus{
				{Name: "web", Running: true},
			},
		},
	}

	out := formatDotsGrouped(projects)

	if !strings.Contains(out, "myapp") {
		t.Error("missing project header")
	}
	if strings.Contains(out, "\n\n") {
		t.Error("single project should not have blank line separator")
	}
}

func TestFormatDotsGrouped_Empty(t *testing.T) {
	out := formatDotsGrouped(nil)
	if out != "" {
		t.Errorf("got %q, want empty", out)
	}
}

func TestFormatDotsGrouped_HealthIcons(t *testing.T) {
	projects := []projectServices{
		{
			Name: "app",
			Services: []serviceStatus{
				{Name: "web", Running: true, Health: "healthy"},
				{Name: "api", Running: true, Health: "unhealthy"},
			},
		},
	}

	out := formatDotsGrouped(projects)

	if !strings.Contains(out, "♥") {
		t.Error("missing healthy icon ♥")
	}
	if !strings.Contains(out, "✗") {
		t.Error("missing unhealthy icon ✗")
	}
}

func TestFormatJSON_OmitsEmptyProject(t *testing.T) {
	items := []serviceStatus{
		{Name: "nginx", Running: true},
	}

	out, err := formatJSON(items)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out, "project") {
		t.Errorf("empty project should be omitted from JSON, got: %s", out)
	}
}

func TestFormatJSON_IncludesProject(t *testing.T) {
	items := []serviceStatus{
		{Project: "myapp", Name: "nginx", Running: true},
	}

	out, err := formatJSON(items)
	if err != nil {
		t.Fatal(err)
	}

	var got []serviceStatus
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got[0].Project != "myapp" {
		t.Errorf("project = %q, want %q", got[0].Project, "myapp")
	}
}

// mockComposer implements runner.Composer for testing.
type mockComposer struct {
	services []string
	status   map[string]runner.ServiceStatus
	err      error
}

func (m *mockComposer) ListServices(_ context.Context) ([]string, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.services, nil
}

func (m *mockComposer) ContainerStatus(_ context.Context) (map[string]runner.ServiceStatus, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.status, nil
}

func (m *mockComposer) Stop(_ context.Context, _ []string, _ io.Writer) error   { return nil }
func (m *mockComposer) Remove(_ context.Context, _ []string, _ io.Writer) error { return nil }
func (m *mockComposer) Pull(_ context.Context, _ []string, _ io.Writer) error   { return nil }
func (m *mockComposer) Create(_ context.Context, _ []string, _ io.Writer) error { return nil }
func (m *mockComposer) Start(_ context.Context, _ []string, _ io.Writer) error  { return nil }
func (m *mockComposer) Logs(_ context.Context, _ string, _ bool, _ int, _ io.Writer) error {
	return nil
}

func TestCollectMultiProject_Success(t *testing.T) {
	mocks := map[string]*mockComposer{
		"/app1": {
			services: []string{"web", "db"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}, "db": {Running: true}},
		},
		"/app2": {
			services: []string{"api"},
			status:   map[string]runner.ServiceStatus{"api": {Running: false}},
		},
	}

	projects := []compose.Project{
		{Name: "app1", ConfigDir: "/app1"},
		{Name: "app2", ConfigDir: "/app2"},
	}

	factory := func(dir string) runner.Composer { return mocks[dir] }
	result := collectMultiProject(context.Background(), projects, factory)

	if len(result) != 2 {
		t.Fatalf("got %d projects, want 2", len(result))
	}
	if result[0].Name != "app1" {
		t.Errorf("result[0].Name = %q, want %q", result[0].Name, "app1")
	}
	if len(result[0].Services) != 2 {
		t.Errorf("app1 services = %d, want 2", len(result[0].Services))
	}
	if len(result[1].Services) != 1 {
		t.Errorf("app2 services = %d, want 1", len(result[1].Services))
	}
}

func TestCollectMultiProject_SkipsFailedProject(t *testing.T) {
	mocks := map[string]*mockComposer{
		"/good": {
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
		"/bad": {
			err: fmt.Errorf("compose file not found"),
		},
	}

	projects := []compose.Project{
		{Name: "good", ConfigDir: "/good"},
		{Name: "bad", ConfigDir: "/bad"},
	}

	factory := func(dir string) runner.Composer { return mocks[dir] }
	result := collectMultiProject(context.Background(), projects, factory)

	if len(result) != 1 {
		t.Fatalf("got %d projects, want 1 (bad should be skipped)", len(result))
	}
	if result[0].Name != "good" {
		t.Errorf("result[0].Name = %q, want %q", result[0].Name, "good")
	}
}

func TestCollectMultiProject_EmptyProjects(t *testing.T) {
	result := collectMultiProject(context.Background(), nil, nil)
	if len(result) != 0 {
		t.Fatalf("got %d projects, want 0", len(result))
	}
}

func TestFlattenProjectServices(t *testing.T) {
	projects := []projectServices{
		{
			Name: "app1",
			Services: []serviceStatus{
				{Name: "web", Running: true},
				{Name: "db", Running: true},
			},
		},
		{
			Name: "app2",
			Services: []serviceStatus{
				{Name: "api", Running: false},
			},
		},
	}

	flat := flattenProjectServices(projects)

	if len(flat) != 3 {
		t.Fatalf("got %d items, want 3", len(flat))
	}

	for _, item := range flat[:2] {
		if item.Project != "app1" {
			t.Errorf("item %q project = %q, want %q", item.Name, item.Project, "app1")
		}
	}
	if flat[2].Project != "app2" {
		t.Errorf("item %q project = %q, want %q", flat[2].Name, flat[2].Project, "app2")
	}
}

func TestFlattenProjectServices_Empty(t *testing.T) {
	flat := flattenProjectServices(nil)
	if len(flat) != 0 {
		t.Fatalf("got %d items, want 0", len(flat))
	}
}

func TestListCmd_Exists(t *testing.T) {
	cmd := NewRootCmd()

	subcommands := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subcommands[sub.Name()] = true
	}

	if !subcommands["list"] {
		t.Error("subcommand 'list' not found")
	}
}

func TestListCmd_JSONFlag(t *testing.T) {
	cmd := NewRootCmd()

	var listCmd *cobra.Command
	for _, sub := range cmd.Commands() {
		if sub.Name() == "list" {
			listCmd = sub
			break
		}
	}
	if listCmd == nil {
		t.Fatal("list subcommand not found")
	}

	flag := listCmd.Flags().Lookup("json")
	if flag == nil {
		t.Fatal("--json flag not found")
	}
	if flag.DefValue != "false" {
		t.Errorf("--json default = %q, want 'false'", flag.DefValue)
	}
}

func TestListCmd_HasExample(t *testing.T) {
	cmd := newListCmd()
	if cmd.Example == "" {
		t.Error("list command has no Example text")
	}
}

func TestListCmd_ExplicitProjectDir_NoComposeFile(t *testing.T) {
	dir := t.TempDir()
	old := projectDir
	projectDir = dir
	t.Cleanup(func() { projectDir = old })

	err := runList(context.Background(), false)
	if err == nil {
		t.Fatal("expected error when -C points to directory without compose file")
	}
	if !strings.Contains(err.Error(), "no compose file found") {
		t.Errorf("error = %q, want it to contain 'no compose file found'", err.Error())
	}
}

// captureStdout runs fn while capturing os.Stdout, returns the captured output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })

	fn()
	w.Close()

	var buf strings.Builder
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	return buf.String()
}

func TestListSingleProject_Dots(t *testing.T) {
	mock := &mockComposer{
		services: []string{"nginx", "postgres"},
		status: map[string]runner.ServiceStatus{
			"nginx":    {Running: true},
			"postgres": {Running: false},
		},
	}

	out := captureStdout(t, func() {
		err := listSingleProject(context.Background(), mock, false)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "nginx") || !strings.Contains(out, "postgres") {
		t.Errorf("output should contain service names, got: %q", out)
	}
	if !strings.Contains(out, "running") || !strings.Contains(out, "stopped") {
		t.Errorf("output should contain status labels, got: %q", out)
	}
}

func TestListSingleProject_JSON(t *testing.T) {
	mock := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true, Health: "healthy"}},
	}

	out := captureStdout(t, func() {
		err := listSingleProject(context.Background(), mock, true)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	var got []serviceStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %q", err, out)
	}
	if len(got) != 1 || got[0].Name != "web" || !got[0].Running {
		t.Errorf("unexpected JSON result: %+v", got)
	}
}

func TestListSingleProject_ListServicesError(t *testing.T) {
	mock := &mockComposer{err: fmt.Errorf("docker down")}

	err := listSingleProject(context.Background(), mock, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listing services") {
		t.Errorf("error = %q, want it to contain 'listing services'", err.Error())
	}
}

func TestListSingleProject_ContainerStatusError(t *testing.T) {
	// mockComposerStatusErr returns services successfully but errors on ContainerStatus
	statusErr := &mockComposerStatusErr{
		services:  []string{"web"},
		statusErr: fmt.Errorf("connection lost"),
	}

	err := listSingleProject(context.Background(), statusErr, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "getting container status") {
		t.Errorf("error = %q, want it to contain 'getting container status'", err.Error())
	}
}

// mockComposerStatusErr returns services successfully but errors on ContainerStatus.
type mockComposerStatusErr struct {
	services  []string
	statusErr error
}

func (m *mockComposerStatusErr) ListServices(_ context.Context) ([]string, error) {
	return m.services, nil
}
func (m *mockComposerStatusErr) ContainerStatus(_ context.Context) (map[string]runner.ServiceStatus, error) {
	return nil, m.statusErr
}
func (m *mockComposerStatusErr) Stop(_ context.Context, _ []string, _ io.Writer) error   { return nil }
func (m *mockComposerStatusErr) Remove(_ context.Context, _ []string, _ io.Writer) error { return nil }
func (m *mockComposerStatusErr) Pull(_ context.Context, _ []string, _ io.Writer) error   { return nil }
func (m *mockComposerStatusErr) Create(_ context.Context, _ []string, _ io.Writer) error { return nil }
func (m *mockComposerStatusErr) Start(_ context.Context, _ []string, _ io.Writer) error  { return nil }
func (m *mockComposerStatusErr) Logs(_ context.Context, _ string, _ bool, _ int, _ io.Writer) error {
	return nil
}

func TestPrintMultiProject_Dots(t *testing.T) {
	grouped := []projectServices{
		{
			Name: "app1",
			Services: []serviceStatus{
				{Name: "web", Running: true},
			},
		},
		{
			Name: "app2",
			Services: []serviceStatus{
				{Name: "api", Running: false},
			},
		},
	}

	out := captureStdout(t, func() {
		err := printMultiProject(grouped, false)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "app1") || !strings.Contains(out, "app2") {
		t.Errorf("output should contain project names, got: %q", out)
	}
}

func TestPrintMultiProject_JSON(t *testing.T) {
	grouped := []projectServices{
		{
			Name: "app1",
			Services: []serviceStatus{
				{Name: "web", Running: true},
			},
		},
	}

	out := captureStdout(t, func() {
		err := printMultiProject(grouped, true)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	var got []serviceStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %q", err, out)
	}
	if len(got) != 1 || got[0].Project != "app1" {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestRunList_LocalSingleProject(t *testing.T) {
	oldHas := hasLocalCompose
	oldNew := newLocalComposer
	oldProj := projectDir
	t.Cleanup(func() {
		hasLocalCompose = oldHas
		newLocalComposer = oldNew
		projectDir = oldProj
	})

	hasLocalCompose = func(dir string) bool { return true }
	newLocalComposer = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				if strings.Contains(args, "config") && strings.Contains(args, "--services") {
					return []byte("web\ndb\n"), nil
				}
				if strings.Contains(args, "ps") {
					return []byte(`[{"Service":"web","State":"running"},{"Service":"db","State":"running"}]`), nil
				}
				return nil, nil
			},
		)
		return c
	}
	projectDir = ""

	out := captureStdout(t, func() {
		err := runList(context.Background(), false)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "web") || !strings.Contains(out, "db") {
		t.Errorf("output should contain service names, got: %q", out)
	}
}

func TestRunList_LocalMultiProject(t *testing.T) {
	oldHas := hasLocalCompose
	oldNew := newLocalComposer
	oldProj := projectDir
	t.Cleanup(func() {
		hasLocalCompose = oldHas
		newLocalComposer = oldNew
		projectDir = oldProj
	})

	hasLocalCompose = func(dir string) bool { return false }

	// Mock responses differ based on dir in the outputCmd
	newLocalComposer = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				if strings.Contains(args, "ls") && strings.Contains(args, "--format") {
					return []byte(`[{"Name":"app1","Status":"running(1)","ConfigFiles":"/app1/compose.yml"},{"Name":"app2","Status":"running(1)","ConfigFiles":"/app2/compose.yml"}]`), nil
				}
				if strings.Contains(args, "config") && strings.Contains(args, "--services") {
					switch dir {
					case "/app1":
						return []byte("web\n"), nil
					case "/app2":
						return []byte("api\n"), nil
					}
				}
				if strings.Contains(args, "ps") {
					switch dir {
					case "/app1":
						return []byte(`[{"Service":"web","State":"running"}]`), nil
					case "/app2":
						return []byte(`[{"Service":"api","State":"exited"}]`), nil
					}
				}
				return nil, nil
			},
		)
		return c
	}
	projectDir = ""

	out := captureStdout(t, func() {
		err := runList(context.Background(), false)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "app1") || !strings.Contains(out, "app2") {
		t.Errorf("output should contain project names, got: %q", out)
	}
}

func TestRunList_LocalMultiProject_JSON(t *testing.T) {
	oldHas := hasLocalCompose
	oldNew := newLocalComposer
	oldProj := projectDir
	t.Cleanup(func() {
		hasLocalCompose = oldHas
		newLocalComposer = oldNew
		projectDir = oldProj
	})

	hasLocalCompose = func(dir string) bool { return false }
	newLocalComposer = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				if strings.Contains(args, "ls") && strings.Contains(args, "--format") {
					return []byte(`[{"Name":"app1","Status":"running(1)","ConfigFiles":"/app1/compose.yml"}]`), nil
				}
				if strings.Contains(args, "config") && strings.Contains(args, "--services") {
					return []byte("web\n"), nil
				}
				if strings.Contains(args, "ps") {
					return []byte(`[{"Service":"web","State":"running"}]`), nil
				}
				return nil, nil
			},
		)
		return c
	}
	projectDir = ""

	out := captureStdout(t, func() {
		err := runList(context.Background(), true)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	var got []serviceStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %q", err, out)
	}
	if len(got) != 1 || got[0].Project != "app1" {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestRunList_LocalListProjectsError(t *testing.T) {
	oldHas := hasLocalCompose
	oldNew := newLocalComposer
	oldProj := projectDir
	t.Cleanup(func() {
		hasLocalCompose = oldHas
		newLocalComposer = oldNew
		projectDir = oldProj
	})

	hasLocalCompose = func(dir string) bool { return false }
	newLocalComposer = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				return nil, fmt.Errorf("docker not running")
			},
		)
		return c
	}
	projectDir = ""

	err := runList(context.Background(), false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listing projects") {
		t.Errorf("error = %q, want it to contain 'listing projects'", err.Error())
	}
}

func TestRunList_LocalNoProjects(t *testing.T) {
	oldHas := hasLocalCompose
	oldNew := newLocalComposer
	oldProj := projectDir
	t.Cleanup(func() {
		hasLocalCompose = oldHas
		newLocalComposer = oldNew
		projectDir = oldProj
	})

	hasLocalCompose = func(dir string) bool { return false }
	newLocalComposer = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				return []byte(`[]`), nil
			},
		)
		return c
	}
	projectDir = ""

	err := runList(context.Background(), false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no compose projects found") {
		t.Errorf("error = %q, want it to contain 'no compose projects found'", err.Error())
	}
}

func TestRunList_LocalDetectFailure(t *testing.T) {
	oldHas := hasLocalCompose
	oldNew := newLocalComposer
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		hasLocalCompose = oldHas
		newLocalComposer = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	hasLocalCompose = func(dir string) bool { return true }
	newLocalComposer = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				return nil, fmt.Errorf("not found")
			},
		)
		return c
	}
	projectDir = ""
	serverName = ""

	err := runList(context.Background(), false)
	if err == nil {
		t.Fatal("expected error when Detect fails")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error = %q, want it to contain 'neither'", err.Error())
	}
}

func TestRunList_LocalMultiProjectDetectFailure(t *testing.T) {
	oldHas := hasLocalCompose
	oldNew := newLocalComposer
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		hasLocalCompose = oldHas
		newLocalComposer = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	hasLocalCompose = func(dir string) bool { return false }
	newLocalComposer = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				return nil, fmt.Errorf("not found")
			},
		)
		return c
	}
	projectDir = ""
	serverName = ""

	err := runList(context.Background(), false)
	if err == nil {
		t.Fatal("expected error when Detect fails")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error = %q, want it to contain 'neither'", err.Error())
	}
}

func TestRunList_ServerSingleProject(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := tmpHome + "/.cdeploy"
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgData := "servers:\n  - name: test-srv\n    host: user@host\n    project_dir: /opt/apps\n"
	if err := os.WriteFile(cfgDir+"/servers.yml", []byte(cfgData), 0o644); err != nil {
		t.Fatal(err)
	}

	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	oldServer := serverName
	oldProj := projectDir
	oldNewRemote := newRemote
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
		newRemote = oldNewRemote
	})

	serverName = "test-srv"
	projectDir = "/explicit/project"

	// Create a RemoteCompose with hooks so Connect/Close/Detect succeed and ListServices/ContainerStatus work
	newRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := oldNewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil }, // runCmd
			func(cmd *exec.Cmd) ([]byte, error) { // outputCmd
				// Handle Detect probe and remote commands
				remoteCmd := cmd.Args[len(cmd.Args)-1]
				if strings.Contains(remoteCmd, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "'config'") && strings.Contains(args, "'--services'") {
					return []byte("web\ndb\n"), nil
				}
				if strings.Contains(args, "'ps'") && strings.Contains(args, "'-a'") {
					return []byte(`[{"Service":"web","State":"running"},{"Service":"db","State":"exited"}]`), nil
				}
				return nil, nil
			},
		)
		return rc
	}

	out := captureStdout(t, func() {
		err := runList(context.Background(), false)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "web") || !strings.Contains(out, "db") {
		t.Errorf("output should contain service names, got: %q", out)
	}
}

func TestRunList_ServerMultiProject(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := tmpHome + "/.cdeploy"
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgData := "servers:\n  - name: test-srv\n    host: user@host\n    project_dir: /opt/apps\n"
	if err := os.WriteFile(cfgDir+"/servers.yml", []byte(cfgData), 0o644); err != nil {
		t.Fatal(err)
	}

	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	oldServer := serverName
	oldProj := projectDir
	oldNewRemote := newRemote
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
		newRemote = oldNewRemote
	})

	serverName = "test-srv"
	projectDir = "" // no explicit -C → multi-project discovery

	newRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := oldNewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) {
				remoteCmd := cmd.Args[len(cmd.Args)-1]
				if strings.Contains(remoteCmd, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "'ls'") && strings.Contains(args, "'-a'") {
					return []byte(`[{"Name":"app1","Status":"running(1)","ConfigFiles":"/srv/app1/compose.yml"}]`), nil
				}
				if strings.Contains(args, "'config'") && strings.Contains(args, "'--services'") {
					return []byte("web\n"), nil
				}
				if strings.Contains(args, "'ps'") && strings.Contains(args, "'-a'") {
					return []byte(`[{"Service":"web","State":"running"}]`), nil
				}
				return nil, nil
			},
		)
		return rc
	}

	out := captureStdout(t, func() {
		err := runList(context.Background(), false)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "app1") {
		t.Errorf("output should contain project name 'app1', got: %q", out)
	}
}

func TestListCmd_RemoteIgnoresServerProjectDir(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := tmpHome + "/.cdeploy"
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgData := "servers:\n  - name: test-srv\n    host: user@host.invalid\n    project_dir: /opt/apps\n"
	if err := os.WriteFile(cfgDir+"/servers.yml", []byte(cfgData), 0o644); err != nil {
		t.Fatal(err)
	}

	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	oldServer := serverName
	serverName = "test-srv"
	t.Cleanup(func() { serverName = oldServer })

	oldProj := projectDir
	projectDir = ""
	t.Cleanup(func() { projectDir = oldProj })

	var capturedProjDir string
	oldNewRemote := newRemote
	newRemote = func(host, projDir string) *compose.RemoteCompose {
		capturedProjDir = projDir
		return oldNewRemote(host, projDir)
	}
	t.Cleanup(func() { newRemote = oldNewRemote })

	_ = runList(context.Background(), false)

	if capturedProjDir != "" {
		t.Errorf("newRemote received projDir = %q, want empty (server.ProjectDir should be ignored)", capturedProjDir)
	}
}

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
	"unicode/utf8"

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
		{Name: "nginx", Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		{Name: "postgres", Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		{Name: "redis", Running: false, Created: "2024-01-14 08:00"},
	}

	out := formatDots(items)
	lines := strings.Split(out, "\n")

	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}

	// redis should be padded to match "postgres" (8 chars)
	if !strings.Contains(lines[2], "redis   ") {
		t.Errorf("redis not padded: %q", lines[2])
	}

	// Running services should show uptime
	if !strings.Contains(lines[0], "3h") {
		t.Errorf("nginx line missing uptime: %q", lines[0])
	}

	// All should show Created
	for _, line := range lines {
		if !strings.Contains(line, "2024-01") {
			t.Errorf("line missing created column: %q", line)
		}
	}
}

func TestFormatDots_MixedStates(t *testing.T) {
	items := []serviceStatus{
		{Name: "web", Running: true, Created: "2024-01-15 09:30", Uptime: "2h"},
		{Name: "db", Running: false, Created: "2024-01-15 09:30"},
	}

	out := formatDots(items)

	// Running service should show uptime
	if !strings.Contains(out, "2h") {
		t.Error("missing uptime for running service")
	}
	// Stopped service should NOT show uptime
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.Contains(line, "db") && strings.Contains(line, "2h") {
			t.Error("stopped service should not have uptime")
		}
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

func TestFormatDotsGrouped_CreatedAndUptime(t *testing.T) {
	projects := []projectServices{
		{
			Name: "myapp",
			Services: []serviceStatus{
				{Name: "web", Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
				{Name: "long-worker", Running: true, Created: "2024-01-01 00:00", Uptime: "15d 3h"},
				{Name: "db", Running: false, Created: "2024-01-10 12:00"},
			},
		},
	}

	out := formatDotsGrouped(projects)
	lines := strings.Split(out, "\n")

	// First line is the project header
	if lines[0] != "myapp" {
		t.Errorf("first line = %q, want project header", lines[0])
	}

	// Service lines are indented and should contain Created values
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("service line not indented: %q", line)
		}
		if !strings.Contains(line, "2024-") {
			t.Errorf("line missing created column: %q", line)
		}
	}

	// The running services should have uptime
	if !strings.Contains(lines[1], "3h") {
		t.Errorf("web line missing uptime: %q", lines[1])
	}
	if !strings.Contains(lines[2], "15d 3h") {
		t.Errorf("long-worker line missing uptime: %q", lines[2])
	}

	// Uptime column should be padded to uniform width (maxUptime = len("15d 3h") = 6)
	// Both running lines should have equal total length after the created column
	webLine := lines[1]
	workerLine := lines[2]
	if len(webLine) != len(workerLine) {
		t.Errorf("uptime column not aligned: web line len=%d, worker line len=%d\nweb:    %q\nworker: %q",
			len(webLine), len(workerLine), webLine, workerLine)
	}

	// Stopped service should still have the uptime column space (padded empty)
	dbLine := lines[3]
	if len(dbLine) != len(webLine) {
		t.Errorf("stopped service line not aligned with running: db len=%d, web len=%d\ndb:  %q\nweb: %q",
			len(dbLine), len(webLine), dbLine, webLine)
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

func TestFormatDots_AlignmentVaryingWidths(t *testing.T) {
	items := []serviceStatus{
		{Name: "a", Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		{Name: "long-service-name", Running: true, Created: "2024-01-01 00:00", Uptime: "15d 3h"},
		{Name: "mid", Running: false, Created: "2024-12-31 23:59"},
	}

	out := formatDots(items)
	lines := strings.Split(out, "\n")

	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}

	// All lines should have aligned created columns (max created width = 16)
	// The name column should be padded to "long-service-name" width (17 chars)
	for _, line := range lines {
		if !strings.Contains(line, "2024-") {
			t.Errorf("line missing created column: %q", line)
		}
	}

	// The stopped service should not have uptime
	if strings.Contains(lines[2], "15d") || strings.Contains(lines[2], "3h") {
		t.Errorf("stopped service should not show uptime from other services: %q", lines[2])
	}
}

func TestFormatDots_StoppedEmptyUptime(t *testing.T) {
	items := []serviceStatus{
		{Name: "web", Running: true, Created: "2024-01-15 09:30", Uptime: "5h"},
		{Name: "worker", Running: false, Created: "2024-01-15 09:30"},
	}

	out := formatDots(items)
	lines := strings.Split(out, "\n")

	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}

	// First line (web) should have uptime
	if !strings.Contains(lines[0], "5h") {
		t.Errorf("running service missing uptime: %q", lines[0])
	}

	// Second line (worker) should NOT have uptime
	if strings.Contains(lines[1], "5h") {
		t.Errorf("stopped service should have empty uptime: %q", lines[1])
	}
}

func TestFormatDots_NoCreatedNoUptime(t *testing.T) {
	// When no Created or Uptime data, just show dot + health + name
	items := []serviceStatus{
		{Name: "web", Running: true},
		{Name: "db", Running: false},
	}

	out := formatDots(items)
	lines := strings.Split(out, "\n")

	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}

	// Should contain service names but no extra columns
	if !strings.Contains(out, "web") || !strings.Contains(out, "db") {
		t.Errorf("output should contain service names, got: %q", out)
	}
}

func TestFormatJSON_IncludesCreatedAndUptime(t *testing.T) {
	items := []serviceStatus{
		{Name: "web", Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		{Name: "db", Running: false, Created: "2024-01-14 08:00"},
	}

	out, err := formatJSON(items)
	if err != nil {
		t.Fatal(err)
	}

	var got []serviceStatus
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got[0].Created != "2024-01-15 09:30" {
		t.Errorf("got[0].Created = %q, want %q", got[0].Created, "2024-01-15 09:30")
	}
	if got[0].Uptime != "3h" {
		t.Errorf("got[0].Uptime = %q, want %q", got[0].Uptime, "3h")
	}
	if got[1].Created != "2024-01-14 08:00" {
		t.Errorf("got[1].Created = %q, want %q", got[1].Created, "2024-01-14 08:00")
	}
	// Stopped service: Uptime should be empty and omitted from JSON
	if got[1].Uptime != "" {
		t.Errorf("got[1].Uptime = %q, want empty", got[1].Uptime)
	}
	// Verify omitempty: uptime should not appear for db
	if strings.Count(out, `"uptime"`) != 1 {
		t.Errorf("expected uptime field exactly once (for web only), got JSON: %s", out)
	}
}

func TestMergeStatus_CopiesCreatedAndUptime(t *testing.T) {
	services := []string{"web", "db"}
	status := map[string]runner.ServiceStatus{
		"web": {Running: true, Health: "healthy", Created: "2024-01-15 09:30", Uptime: "3h"},
		"db":  {Running: false, Created: "2024-01-14 08:00"},
	}

	got := mergeStatus(services, status)

	for _, s := range got {
		switch s.Name {
		case "web":
			if s.Created != "2024-01-15 09:30" {
				t.Errorf("web Created = %q, want %q", s.Created, "2024-01-15 09:30")
			}
			if s.Uptime != "3h" {
				t.Errorf("web Uptime = %q, want %q", s.Uptime, "3h")
			}
		case "db":
			if s.Created != "2024-01-14 08:00" {
				t.Errorf("db Created = %q, want %q", s.Created, "2024-01-14 08:00")
			}
			if s.Uptime != "" {
				t.Errorf("db Uptime = %q, want empty", s.Uptime)
			}
		}
	}
}

// mockComposer implements runner.Composer for testing.
type mockComposer struct {
	services []string
	status   map[string]runner.ServiceStatus
	stats    map[string]runner.ServiceStats
	statsErr error
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

func (m *mockComposer) ContainerStats(_ context.Context) (map[string]runner.ServiceStats, error) {
	if m.statsErr != nil {
		return nil, m.statsErr
	}
	return m.stats, nil
}

func (m *mockComposer) Stop(_ context.Context, _ []string, _ io.Writer) error   { return nil }
func (m *mockComposer) Remove(_ context.Context, _ []string, _ io.Writer) error { return nil }
func (m *mockComposer) Pull(_ context.Context, _ []string, _ io.Writer) error   { return nil }
func (m *mockComposer) Create(_ context.Context, _ []string, _ io.Writer) error { return nil }
func (m *mockComposer) Start(_ context.Context, _ []string, _ io.Writer) error  { return nil }
func (m *mockComposer) Logs(_ context.Context, _ string, _ bool, _ int, _ io.Writer) error {
	return nil
}

// mockComposerBulk extends mockComposer with the bulkStatsAggregator interface
// so collectMultiProjectStats's optimized path can be exercised in tests.
// The bulkStats field is what ContainerStatsFromBulk returns; the inherited
// stats / statsErr fields cover the fallback ContainerStats() path.
// Call counters let tests assert which path was taken.
type mockComposerBulk struct {
	mockComposer
	bulkStats           map[string]runner.ServiceStats
	bulkErr             error
	bulkCalls           int
	containerStatsCalls int
}

func (m *mockComposerBulk) ContainerStats(ctx context.Context) (map[string]runner.ServiceStats, error) {
	m.containerStatsCalls++
	return m.mockComposer.ContainerStats(ctx)
}

func (m *mockComposerBulk) ContainerStatsFromBulk(_ context.Context, _ map[string]runner.ServiceStats) (map[string]runner.ServiceStats, error) {
	m.bulkCalls++
	if m.bulkErr != nil {
		return nil, m.bulkErr
	}
	return m.bulkStats, nil
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

	err := runList(context.Background(), false, false)
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
			"nginx":    {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
			"postgres": {Running: false, Created: "2024-01-14 08:00"},
		},
	}

	out := captureStdout(t, func() {
		err := listSingleProject(context.Background(), mock, false, false)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "nginx") || !strings.Contains(out, "postgres") {
		t.Errorf("output should contain service names, got: %q", out)
	}
	if !strings.Contains(out, "2024-01-15 09:30") {
		t.Errorf("output should contain created time, got: %q", out)
	}
	if !strings.Contains(out, "3h") {
		t.Errorf("output should contain uptime, got: %q", out)
	}
}

func TestListSingleProject_JSON(t *testing.T) {
	mock := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true, Health: "healthy"}},
	}

	out := captureStdout(t, func() {
		err := listSingleProject(context.Background(), mock, true, false)
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

	err := listSingleProject(context.Background(), mock, false, false)
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

	err := listSingleProject(context.Background(), statusErr, false, false)
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
func (m *mockComposerStatusErr) ContainerStats(_ context.Context) (map[string]runner.ServiceStats, error) {
	return nil, nil
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
	oldHas := listHasCompose
	oldNew := listNewLocal
	oldProj := projectDir
	t.Cleanup(func() {
		listHasCompose = oldHas
		listNewLocal = oldNew
		projectDir = oldProj
	})

	listHasCompose = func(dir string) bool { return true }
	listNewLocal = func(dir string) *compose.Compose {
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
	projectDir = "/explicit/dir"

	out := captureStdout(t, func() {
		err := runList(context.Background(), false, false)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "web") || !strings.Contains(out, "db") {
		t.Errorf("output should contain service names, got: %q", out)
	}
}

func TestRunList_LocalDiscoveryFromComposeDir(t *testing.T) {
	oldHas := listHasCompose
	oldNew := listNewLocal
	oldProj := projectDir
	t.Cleanup(func() {
		listHasCompose = oldHas
		listNewLocal = oldNew
		projectDir = oldProj
	})

	// CWD has a compose file but -C is NOT given → should discover all projects
	listHasCompose = func(dir string) bool { return true }
	listNewLocal = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				if strings.Contains(args, "version") {
					return []byte("Docker Compose version v2.24.0\n"), nil
				}
				if strings.Contains(args, "ls") && strings.Contains(args, "--format") {
					return []byte(`[{"Name":"myapp","Status":"running(1)","ConfigFiles":"/app/compose.yml"},{"Name":"other","Status":"running(1)","ConfigFiles":"/other/compose.yml"}]`), nil
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
		err := runList(context.Background(), false, false)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	// Should show multiple projects, not just a flat single-project list
	if !strings.Contains(out, "myapp") || !strings.Contains(out, "other") {
		t.Errorf("should discover all projects, got: %q", out)
	}
}

func TestRunList_LocalMultiProject(t *testing.T) {
	oldHas := listHasCompose
	oldNew := listNewLocal
	oldProj := projectDir
	t.Cleanup(func() {
		listHasCompose = oldHas
		listNewLocal = oldNew
		projectDir = oldProj
	})

	listHasCompose = func(dir string) bool { return false }

	// Mock responses differ based on dir in the outputCmd
	listNewLocal = func(dir string) *compose.Compose {
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
		err := runList(context.Background(), false, false)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "app1") || !strings.Contains(out, "app2") {
		t.Errorf("output should contain project names, got: %q", out)
	}
}

func TestRunList_LocalMultiProject_JSON(t *testing.T) {
	oldHas := listHasCompose
	oldNew := listNewLocal
	oldProj := projectDir
	t.Cleanup(func() {
		listHasCompose = oldHas
		listNewLocal = oldNew
		projectDir = oldProj
	})

	listHasCompose = func(dir string) bool { return false }
	listNewLocal = func(dir string) *compose.Compose {
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
		err := runList(context.Background(), true, false)
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
	oldHas := listHasCompose
	oldNew := listNewLocal
	oldProj := projectDir
	t.Cleanup(func() {
		listHasCompose = oldHas
		listNewLocal = oldNew
		projectDir = oldProj
	})

	listHasCompose = func(dir string) bool { return false }
	listNewLocal = func(dir string) *compose.Compose {
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

	err := runList(context.Background(), false, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "listing projects") {
		t.Errorf("error = %q, want it to contain 'listing projects'", err.Error())
	}
}

func TestRunList_LocalNoProjects(t *testing.T) {
	oldHas := listHasCompose
	oldNew := listNewLocal
	oldProj := projectDir
	t.Cleanup(func() {
		listHasCompose = oldHas
		listNewLocal = oldNew
		projectDir = oldProj
	})

	listHasCompose = func(dir string) bool { return false }
	listNewLocal = func(dir string) *compose.Compose {
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

	err := runList(context.Background(), false, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no compose projects found") {
		t.Errorf("error = %q, want it to contain 'no compose projects found'", err.Error())
	}
}

func TestRunList_LocalDetectFailure(t *testing.T) {
	oldHas := listHasCompose
	oldNew := listNewLocal
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		listHasCompose = oldHas
		listNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	listHasCompose = func(dir string) bool { return true }
	listNewLocal = func(dir string) *compose.Compose {
		c := compose.New(dir)
		c.SetTestHooks(
			nil,
			func(cmd *exec.Cmd) ([]byte, error) {
				return nil, fmt.Errorf("not found")
			},
		)
		return c
	}
	projectDir = "/explicit/dir"
	serverName = ""

	err := runList(context.Background(), false, false)
	if err == nil {
		t.Fatal("expected error when Detect fails")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error = %q, want it to contain 'neither'", err.Error())
	}
}

func TestRunList_LocalMultiProjectDetectFailure(t *testing.T) {
	oldHas := listHasCompose
	oldNew := listNewLocal
	oldProj := projectDir
	oldServer := serverName
	t.Cleanup(func() {
		listHasCompose = oldHas
		listNewLocal = oldNew
		projectDir = oldProj
		serverName = oldServer
	})

	listHasCompose = func(dir string) bool { return false }
	listNewLocal = func(dir string) *compose.Compose {
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

	err := runList(context.Background(), false, false)
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
	oldNewRemote := listNewRemote
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
		listNewRemote = oldNewRemote
	})

	serverName = "test-srv"
	projectDir = "/explicit/project"

	// Create a RemoteCompose with hooks so Connect/Close/Detect succeed and ListServices/ContainerStatus work
	listNewRemote = func(host, projDir string) *compose.RemoteCompose {
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
		err := runList(context.Background(), false, false)
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
	oldNewRemote := listNewRemote
	t.Cleanup(func() {
		serverName = oldServer
		projectDir = oldProj
		listNewRemote = oldNewRemote
	})

	serverName = "test-srv"
	projectDir = "" // no explicit -C → multi-project discovery

	listNewRemote = func(host, projDir string) *compose.RemoteCompose {
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
		err := runList(context.Background(), false, false)
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
	oldNewRemote := listNewRemote
	listNewRemote = func(host, projDir string) *compose.RemoteCompose {
		capturedProjDir = projDir
		return oldNewRemote(host, projDir)
	}
	t.Cleanup(func() { listNewRemote = oldNewRemote })

	_ = runList(context.Background(), false, false)

	if capturedProjDir != "" {
		t.Errorf("listNewRemote received projDir = %q, want empty (server.ProjectDir should be ignored)", capturedProjDir)
	}
}

func TestRunList_SSHAndServerMutex(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
	})

	serverName = "prod"
	sshTarget = "user@host"
	projectDir = "/srv/app"

	err := runList(context.Background(), false, false)
	if err == nil {
		t.Fatal("expected mutex error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want it to contain 'mutually exclusive'", err.Error())
	}
}

func TestRunList_SSHRequiresProjectDir(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
	})

	serverName = ""
	sshTarget = "user@host"
	projectDir = ""

	err := runList(context.Background(), false, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --project-dir") {
		t.Errorf("error = %q, want it to contain 'requires --project-dir'", err.Error())
	}
}

func TestRunList_SSHHappyPath(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldIdentity := identityFile
	oldNewRemote := listNewRemote
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		identityFile = oldIdentity
		listNewRemote = oldNewRemote
	})

	serverName = ""
	sshTarget = "deploy@host:2222"
	projectDir = "/srv/app"
	identityFile = ""

	var capturedConfigArgs []string
	listNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				switch {
				case strings.Contains(args, "version"):
					return []byte("Docker Compose version v2.24.0\n"), nil
				case strings.Contains(args, "'config'") && strings.Contains(args, "'--services'"):
					capturedConfigArgs = append([]string(nil), cmd.Args...)
					return []byte("nginx\nweb\n"), nil
				case strings.Contains(args, "'ps'"):
					return []byte("[]"), nil
				}
				return nil, nil
			},
		)
		return rc
	}

	if err := runList(context.Background(), false, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedConfigArgs == nil {
		t.Fatal("config --services was not invoked on remote")
	}
	args := strings.Join(capturedConfigArgs, " ")
	if !strings.Contains(args, "-p 2222") {
		t.Errorf("ssh argv = %v, want to contain '-p 2222'", capturedConfigArgs)
	}
	if !strings.Contains(args, "'config'") {
		t.Errorf("ssh argv = %v, want to contain 'config' subcommand", capturedConfigArgs)
	}
}

func TestMergeStatus_CopiesPorts(t *testing.T) {
	services := []string{"web", "db"}
	ports := []runner.Port{
		{Host: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
	}
	status := map[string]runner.ServiceStatus{
		"web": {Running: true, Ports: ports},
		"db":  {Running: true},
	}

	got := mergeStatus(services, status)

	for _, s := range got {
		switch s.Name {
		case "web":
			if len(s.Ports) != 1 {
				t.Fatalf("web Ports len = %d, want 1", len(s.Ports))
			}
			if s.Ports[0] != ports[0] {
				t.Errorf("web Ports[0] = %+v, want %+v", s.Ports[0], ports[0])
			}
		case "db":
			if len(s.Ports) != 0 {
				t.Errorf("db Ports = %+v, want empty", s.Ports)
			}
		}
	}
}

func TestFormatDots_PortsColumn_Mixed(t *testing.T) {
	items := []serviceStatus{
		{Name: "web", Running: true, Ports: []runner.Port{
			{Host: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		}},
		{Name: "db", Running: true},
	}

	out := formatDots(items)
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}

	if !strings.Contains(lines[0], "8080→80") {
		t.Errorf("web line missing ports: %q", lines[0])
	}
	// Both lines should have the same visible (rune) width: db should be padded
	w0 := utf8.RuneCountInString(lines[0])
	w1 := utf8.RuneCountInString(lines[1])
	if w0 != w1 {
		t.Errorf("ports column not aligned (rune width): web=%d, db=%d\nweb: %q\ndb:  %q",
			w0, w1, lines[0], lines[1])
	}
}

func TestFormatDots_NoPorts_NoColumn(t *testing.T) {
	items := []serviceStatus{
		{Name: "web", Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		{Name: "db", Running: false, Created: "2024-01-14 08:00"},
	}

	out := formatDots(items)
	// No service has ports → arrow rune should not appear
	if strings.Contains(out, "→") {
		t.Errorf("output should not contain ports arrow when no service has ports, got: %q", out)
	}
}

func TestFormatDots_PortsColumn_FlattenedMultiProject(t *testing.T) {
	// Simulates a flat-mode listing across multiple projects, mixed port presence.
	flat := flattenProjectServices([]projectServices{
		{
			Name: "app1",
			Services: []serviceStatus{
				{Name: "web", Running: true, Ports: []runner.Port{
					{Host: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"},
				}},
			},
		},
		{
			Name: "app2",
			Services: []serviceStatus{
				{Name: "api", Running: true, Ports: []runner.Port{
					{Host: "127.0.0.1", HostPort: 9000, ContainerPort: 9000, Protocol: "tcp"},
				}},
				{Name: "worker", Running: true},
			},
		},
	})

	out := formatDots(flat)
	if !strings.Contains(out, "80→80") {
		t.Errorf("missing web ports in output: %q", out)
	}
	if !strings.Contains(out, "127.0.0.1:9000→9000") {
		t.Errorf("missing api ports in output: %q", out)
	}

	// All lines should have the same length (uniform alignment).
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	w0 := utf8.RuneCountInString(lines[0])
	for i := 1; i < len(lines); i++ {
		wi := utf8.RuneCountInString(lines[i])
		if wi != w0 {
			t.Errorf("alignment mismatch (rune width): line[0]=%d, line[%d]=%d\n[0]: %q\n[%d]: %q",
				w0, i, wi, lines[0], i, lines[i])
		}
	}
}

func TestFormatDotsGrouped_PerProjectPortsWidth(t *testing.T) {
	projects := []projectServices{
		{
			Name: "shortports",
			Services: []serviceStatus{
				{Name: "web", Running: true, Ports: []runner.Port{
					{Host: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"},
				}},
			},
		},
		{
			Name: "noports",
			Services: []serviceStatus{
				{Name: "worker", Running: true},
			},
		},
	}

	out := formatDotsGrouped(projects)
	if !strings.Contains(out, "80→80") {
		t.Errorf("missing ports in shortports project: %q", out)
	}
	// noports project should not have an arrow rune
	noPortsIdx := strings.Index(out, "noports")
	if noPortsIdx == -1 {
		t.Fatal("noports header missing")
	}
	noPortsBlock := out[noPortsIdx:]
	if strings.Contains(noPortsBlock, "→") {
		t.Errorf("noports project should not render ports column: %q", noPortsBlock)
	}
}

func TestFormatJSON_IncludesPorts(t *testing.T) {
	items := []serviceStatus{
		{Name: "web", Running: true, Ports: []runner.Port{
			{Host: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"},
			{Host: "0.0.0.0", HostPort: 443, ContainerPort: 443, Protocol: "tcp"},
		}},
		{Name: "nilports", Running: true},                       // nil slice → omitempty
		{Name: "emptyports", Running: true, Ports: []runner.Port{}}, // explicit empty slice → omitempty too
	}

	out, err := formatJSON(items)
	if err != nil {
		t.Fatal(err)
	}

	// Full field-name set (snake_case) verification on raw output
	for _, want := range []string{`"ports"`, `"host"`, `"host_port"`, `"container_port"`, `"protocol"`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected field %s in JSON, got: %s", want, out)
		}
	}

	// omitempty: only one occurrence of "ports" (for web), since both nil and explicit
	// empty slice should be omitted by encoding/json with the omitempty tag.
	if strings.Count(out, `"ports"`) != 1 {
		t.Errorf("expected ports field exactly once (web only — nil and empty-slice both omitted), got JSON: %s", out)
	}

	// Round-trip
	var got []serviceStatus
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Multi-port-per-service shape preserved.
	if len(got[0].Ports) != 2 {
		t.Fatalf("got[0].Ports len = %d, want 2 (multi-port per service)", len(got[0].Ports))
	}
	if got[0].Ports[0].Host != "0.0.0.0" || got[0].Ports[0].HostPort != 80 ||
		got[0].Ports[0].ContainerPort != 80 || got[0].Ports[0].Protocol != "tcp" {
		t.Errorf("got[0].Ports[0] = %+v, want first port round-trip", got[0].Ports[0])
	}
	if got[0].Ports[1].HostPort != 443 || got[0].Ports[1].ContainerPort != 443 {
		t.Errorf("got[0].Ports[1] = %+v, want second port round-trip", got[0].Ports[1])
	}
	if len(got[1].Ports) != 0 {
		t.Errorf("got[1].Ports = %+v, want empty (nil → omitempty)", got[1].Ports)
	}
	if len(got[2].Ports) != 0 {
		t.Errorf("got[2].Ports = %+v, want empty (explicit empty slice → omitempty)", got[2].Ports)
	}
}

// TestRunList_SSHHappyPathWithIdentity verifies the list subcommand splices
// -i <keyPath> into SSHExtraArgs when both --ssh and --identity are set.
func TestRunList_SSHHappyPathWithIdentity(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldIdentity := identityFile
	oldNewRemote := listNewRemote
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		identityFile = oldIdentity
		listNewRemote = oldNewRemote
	})

	tmpDir := t.TempDir()
	keyPath := tmpDir + "/id_test"
	if err := os.WriteFile(keyPath, []byte("dummy"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	serverName = ""
	sshTarget = "deploy@host:2222"
	projectDir = "/srv/app"
	identityFile = keyPath

	var capturedConfigArgs []string
	listNewRemote = func(host, projDir string) *compose.RemoteCompose {
		rc := compose.NewRemote(host, projDir)
		rc.SetTestHooks(
			func(cmd *exec.Cmd) error { return nil },
			func(cmd *exec.Cmd) ([]byte, error) {
				args := strings.Join(cmd.Args, " ")
				switch {
				case strings.Contains(args, "version"):
					return []byte("Docker Compose version v2.24.0\n"), nil
				case strings.Contains(args, "'config'") && strings.Contains(args, "'--services'"):
					capturedConfigArgs = append([]string(nil), cmd.Args...)
					return []byte("nginx\nweb\n"), nil
				case strings.Contains(args, "'ps'"):
					return []byte("[]"), nil
				}
				return nil, nil
			},
		)
		return rc
	}

	if err := runList(context.Background(), false, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedConfigArgs == nil {
		t.Fatal("config --services was not invoked on remote")
	}
	args := strings.Join(capturedConfigArgs, " ")
	if !strings.Contains(args, "-p 2222") {
		t.Errorf("ssh argv = %v, want to contain '-p 2222'", capturedConfigArgs)
	}
	if !strings.Contains(args, "-i "+keyPath) {
		t.Errorf("ssh argv = %v, want to contain '-i %s'", capturedConfigArgs, keyPath)
	}
}

func TestList_IdentityWithoutSSH(t *testing.T) {
	oldServer := serverName
	oldSSH := sshTarget
	oldProj := projectDir
	oldIdentity := identityFile
	t.Cleanup(func() {
		serverName = oldServer
		sshTarget = oldSSH
		projectDir = oldProj
		identityFile = oldIdentity
	})

	serverName = ""
	sshTarget = ""
	projectDir = ""
	identityFile = "/tmp/k"

	err := runList(context.Background(), false, false)
	if err == nil {
		t.Fatal("expected error when --identity is set without --ssh")
	}
	if !strings.Contains(err.Error(), "--identity requires --ssh") {
		t.Errorf("error = %q, want it to contain '--identity requires --ssh'", err.Error())
	}
}

func TestListCmd_SSHFlagInherited(t *testing.T) {
	root := NewRootCmd()

	cmd, _, err := root.Find([]string{"list"})
	if err != nil {
		t.Fatalf("list command not found: %v", err)
	}
	sshFlag := cmd.InheritedFlags().Lookup("ssh")
	if sshFlag == nil {
		t.Error("--ssh persistent flag not inherited by list command")
	}
	if sshFlag != nil && sshFlag.Shorthand != "S" {
		t.Errorf("--ssh shorthand = %q, want %q", sshFlag.Shorthand, "S")
	}
}

// TestListCmd_statsFlagRegistration verifies the --stats flag is registered
// on `list` with a `false` default.
func TestListCmd_statsFlagRegistration(t *testing.T) {
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

	flag := listCmd.Flags().Lookup("stats")
	if flag == nil {
		t.Fatal("--stats flag not registered on list")
	}
	if flag.DefValue != "false" {
		t.Errorf("--stats default = %q, want %q", flag.DefValue, "false")
	}
	// Verify the flag actually accepts being set via Set() — sanity check it's a Bool flag.
	if err := flag.Value.Set("true"); err != nil {
		t.Errorf("setting --stats=true failed: %v", err)
	}
	if flag.Value.String() != "true" {
		t.Errorf("--stats after Set('true') = %q, want %q", flag.Value.String(), "true")
	}
}

// TestListJSON_omitsStatsFieldsWithoutFlag verifies that without --stats the
// JSON output contains none of the cpu_percent / memory_used / memory_limit
// keys, preserving wire-shape compatibility.
func TestListJSON_omitsStatsFieldsWithoutFlag(t *testing.T) {
	mock := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		// stats deliberately populated, but showStats=false should ignore them.
		stats: map[string]runner.ServiceStats{
			"web": {CPUPercent: 12.5, MemoryUsed: 100, MemoryLimit: 1000},
		},
	}

	out := captureStdout(t, func() {
		if err := listSingleProject(context.Background(), mock, true, false); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	for _, key := range []string{`"cpu_percent"`, `"memory_used"`, `"memory_limit"`} {
		if strings.Contains(out, key) {
			t.Errorf("JSON without --stats must not contain %s, got: %s", key, out)
		}
	}
}

// TestListJSON_includesStatsFieldsWithFlag verifies that with --stats the
// JSON output contains the three stats keys with values from the stats map.
func TestListJSON_includesStatsFieldsWithFlag(t *testing.T) {
	mock := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		stats: map[string]runner.ServiceStats{
			"web": {CPUPercent: 12.5, MemoryUsed: 130023424, MemoryLimit: 536870912},
		},
	}

	out := captureStdout(t, func() {
		if err := listSingleProject(context.Background(), mock, true, true); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	for _, key := range []string{`"cpu_percent"`, `"memory_used"`, `"memory_limit"`} {
		if !strings.Contains(out, key) {
			t.Errorf("JSON with --stats must contain %s, got: %s", key, out)
		}
	}

	// Round-trip the JSON and confirm the values match.
	var got []serviceStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %q", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1", len(got))
	}
	if got[0].CPUPercent == nil || *got[0].CPUPercent != 12.5 {
		t.Errorf("CPUPercent = %v, want 12.5", got[0].CPUPercent)
	}
	if got[0].MemoryUsed == nil || *got[0].MemoryUsed != 130023424 {
		t.Errorf("MemoryUsed = %v, want 130023424", got[0].MemoryUsed)
	}
	if got[0].MemoryLimit == nil || *got[0].MemoryLimit != 536870912 {
		t.Errorf("MemoryLimit = %v, want 536870912", got[0].MemoryLimit)
	}
}

// TestListJSON_includesStatsFieldsWithZeroValues verifies that --stats emits
// stats fields even when the values are zero (idle container at 0% CPU). This
// is the key reason pointer types are used on the JSON struct — value-types
// with omitempty would silently drop legitimate zeros.
func TestListJSON_includesStatsFieldsWithZeroValues(t *testing.T) {
	mock := &mockComposer{
		services: []string{"idle"},
		status:   map[string]runner.ServiceStatus{"idle": {Running: true}},
		stats: map[string]runner.ServiceStats{
			"idle": {CPUPercent: 0, MemoryUsed: 0, MemoryLimit: 0},
		},
	}

	out := captureStdout(t, func() {
		if err := listSingleProject(context.Background(), mock, true, true); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	for _, key := range []string{`"cpu_percent"`, `"memory_used"`, `"memory_limit"`} {
		if !strings.Contains(out, key) {
			t.Errorf("zero-valued stats with --stats must still appear in JSON: missing %s, got: %s", key, out)
		}
	}
}

// TestListCmd_singleProjectStatsFailure verifies that on stats failure in
// single-project mode, the command exits 0 with a stderr warning and the
// listing still renders without stats values.
func TestListCmd_singleProjectStatsFailure(t *testing.T) {
	mock := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		statsErr: fmt.Errorf("docker stats failed: connection refused"),
	}

	// Capture stderr to verify the warning is emitted.
	oldStderr := os.Stderr
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = wErr
	t.Cleanup(func() { os.Stderr = oldStderr })

	stdout := captureStdout(t, func() {
		if err := listSingleProject(context.Background(), mock, false, true); err != nil {
			t.Errorf("listSingleProject must not return error on stats fail, got: %v", err)
		}
	})
	wErr.Close()
	os.Stderr = oldStderr

	var stderrBuf strings.Builder
	if _, err := io.Copy(&stderrBuf, rErr); err != nil {
		t.Fatal(err)
	}
	stderr := stderrBuf.String()

	if !strings.Contains(stderr, "stats unavailable") {
		t.Errorf("stderr missing stats warning, got: %q", stderr)
	}
	if !strings.Contains(stderr, "connection refused") {
		t.Errorf("stderr missing underlying error, got: %q", stderr)
	}
	// Service line is still rendered.
	if !strings.Contains(stdout, "web") {
		t.Errorf("stdout missing service name on stats fail, got: %q", stdout)
	}
	// Cells must be blank — not fake "0.0%" / "0B/0B" indistinguishable from a real idle reading.
	if strings.Contains(stdout, "0.0%") {
		t.Errorf("stats-fail output must not contain fake 0.0%% CPU value, got: %q", stdout)
	}
	if strings.Contains(stdout, "0B/0B") {
		t.Errorf("stats-fail output must not contain fake 0B/0B mem value, got: %q", stdout)
	}
}

// TestListCmd_multiProjectStatsFailure verifies a per-project stats failure
// is non-fatal: the warning is emitted and other projects still render.
func TestListCmd_multiProjectStatsFailure(t *testing.T) {
	mocks := map[string]*mockComposer{
		"/app1": {
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
			statsErr: fmt.Errorf("stats fetch failed"),
		},
		"/app2": {
			services: []string{"api"},
			status:   map[string]runner.ServiceStatus{"api": {Running: true}},
			stats: map[string]runner.ServiceStats{
				"api": {CPUPercent: 5.5, MemoryUsed: 1024, MemoryLimit: 4096},
			},
		},
	}

	projects := []compose.Project{
		{Name: "app1", ConfigDir: "/app1"},
		{Name: "app2", ConfigDir: "/app2"},
	}
	factory := func(dir string) runner.Composer { return mocks[dir] }

	oldStderr := os.Stderr
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = wErr
	t.Cleanup(func() { os.Stderr = oldStderr })

	result := collectMultiProjectStats(context.Background(), projects, factory, true, nil)
	wErr.Close()
	os.Stderr = oldStderr

	var stderrBuf strings.Builder
	if _, err := io.Copy(&stderrBuf, rErr); err != nil {
		t.Fatal(err)
	}
	stderr := stderrBuf.String()

	if !strings.Contains(stderr, "stats unavailable") {
		t.Errorf("stderr missing stats warning for app1, got: %q", stderr)
	}
	if !strings.Contains(stderr, "app1") {
		t.Errorf("stderr warning should name failing project app1, got: %q", stderr)
	}
	// Both projects still in the result (app1's status fetch succeeded; only stats failed).
	if len(result) != 2 {
		t.Fatalf("got %d projects, want 2 (stats failure must not drop a project)", len(result))
	}
	// app2's stats came through.
	for _, p := range result {
		if p.Name == "app2" && len(p.Services) == 1 {
			s := p.Services[0]
			if s.CPUPercent == nil || *s.CPUPercent != 5.5 {
				t.Errorf("app2/api CPUPercent = %v, want 5.5", s.CPUPercent)
			}
		}
		if p.Name == "app1" && len(p.Services) == 1 {
			s := p.Services[0]
			// On stats failure (stats map is nil), pointers stay nil so:
			//   - tabular cells render blank (formatCPUCell/formatMemCell short-circuit on nil)
			//   - JSON consumers see fields omitted (omitempty on nil pointer)
			// rather than fake "0.0%" / "0B/0B" indistinguishable from a real idle reading.
			if s.CPUPercent != nil {
				t.Errorf("app1/web CPUPercent should be nil on stats failure (blank cell), got %v", *s.CPUPercent)
			}
			if s.MemoryUsed != nil {
				t.Errorf("app1/web MemoryUsed should be nil on stats failure (blank cell), got %v", *s.MemoryUsed)
			}
			if s.MemoryLimit != nil {
				t.Errorf("app1/web MemoryLimit should be nil on stats failure (blank cell), got %v", *s.MemoryLimit)
			}
		}
	}
}

// TestFormatDots_StatsColumns verifies CPU and Mem columns are rendered when
// any service has stats data, with proper alignment.
func TestFormatDots_StatsColumns(t *testing.T) {
	cpu1 := 4.2
	mem1 := int64(130023424) // 124M
	lim1 := int64(536870912) // 512M
	items := []serviceStatus{
		{
			Name: "web", Running: true,
			CPUPercent: &cpu1, MemoryUsed: &mem1, MemoryLimit: &lim1,
		},
		{Name: "db", Running: false}, // stopped: stats fields nil → blank cells
	}

	out := formatDots(items)
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}

	if !strings.Contains(lines[0], "4.2%") {
		t.Errorf("web line missing CPU%%, got: %q", lines[0])
	}
	if !strings.Contains(lines[0], "124M/512M") {
		t.Errorf("web line missing memory cell, got: %q", lines[0])
	}
	// Stopped db must not display web's stats.
	if strings.Contains(lines[1], "4.2%") || strings.Contains(lines[1], "124M") {
		t.Errorf("stopped db must not show running stats, got: %q", lines[1])
	}
	// Lines must align (rune width equal).
	w0 := utf8.RuneCountInString(lines[0])
	w1 := utf8.RuneCountInString(lines[1])
	if w0 != w1 {
		t.Errorf("stats columns not aligned: w0=%d, w1=%d\n[0]: %q\n[1]: %q", w0, w1, lines[0], lines[1])
	}
}

// TestFormatDots_NoStatsNoColumn verifies that when no service has stats
// data (nil pointers), no CPU/Mem column is rendered.
func TestFormatDots_NoStatsNoColumn(t *testing.T) {
	items := []serviceStatus{
		{Name: "web", Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		{Name: "db", Running: false, Created: "2024-01-14 08:00"},
	}

	out := formatDots(items)
	if strings.Contains(out, "%") {
		t.Errorf("output should not contain '%%' when no stats, got: %q", out)
	}
}

// TestFormatDotsGrouped_StatsColumns verifies grouped (multi-project) output
// renders CPU/Mem columns per project, with proper alignment.
func TestFormatDotsGrouped_StatsColumns(t *testing.T) {
	cpu := 7.5
	mem := int64(1024 * 1024 * 50)  // 50M
	lim := int64(1024 * 1024 * 256) // 256M
	projects := []projectServices{
		{
			Name: "app",
			Services: []serviceStatus{
				{
					Name: "web", Running: true,
					CPUPercent: &cpu, MemoryUsed: &mem, MemoryLimit: &lim,
				},
				{Name: "db", Running: false},
			},
		},
	}

	out := formatDotsGrouped(projects)
	if !strings.Contains(out, "7.5%") {
		t.Errorf("grouped output missing CPU%%, got: %q", out)
	}
	if !strings.Contains(out, "50M/256M") {
		t.Errorf("grouped output missing memory cell, got: %q", out)
	}
}

// TestMergeStatusStats_RequestedPopulatesPointers verifies that when
// statsRequested is true, services present in the stats map get non-nil
// pointers while services absent from the map keep nil pointers (blank cells,
// omitted JSON fields). The nil-vs-&0 distinction matters: &0 means a
// legitimate idle container observed by docker; nil means "no data" (stopped,
// or ps/stats race). Conflating them would emit fake "0.0%" / "0B/0B" cells
// that look indistinguishable from real readings.
func TestMergeStatusStats_RequestedPopulatesPointers(t *testing.T) {
	services := []string{"web", "missing"}
	status := map[string]runner.ServiceStatus{
		"web":     {Running: true},
		"missing": {Running: false},
	}
	stats := map[string]runner.ServiceStats{
		"web": {CPUPercent: 1.5, MemoryUsed: 100, MemoryLimit: 1000},
	}

	got := mergeStatusStats(services, status, stats, true)

	// alphabetical sort: "missing" < "web"
	if got[0].Name != "missing" || got[1].Name != "web" {
		t.Fatalf("order = [%s, %s], want [missing, web]", got[0].Name, got[1].Name)
	}

	// "missing" is absent from stats → pointers stay nil (blank cells, JSON omits fields).
	if got[0].CPUPercent != nil || got[0].MemoryUsed != nil || got[0].MemoryLimit != nil {
		t.Errorf("absent-from-stats service must have nil pointers, got %+v", got[0])
	}

	// "web" should have the actual values.
	if got[1].CPUPercent == nil || *got[1].CPUPercent != 1.5 {
		t.Errorf("web CPU = %v, want 1.5", got[1].CPUPercent)
	}
	if got[1].MemoryUsed == nil || *got[1].MemoryUsed != 100 {
		t.Errorf("web MemoryUsed = %v, want 100", got[1].MemoryUsed)
	}
}

// TestMergeStatusStats_LegitimateZeroPreserved verifies that a service present
// in the stats map with zero values (idle container observed by docker) still
// emits &0 pointers — JSON keys appear with value 0, text shows "0.0%" /
// "0B/0B". This is the case pointer types were chosen to distinguish from
// "absent from stats" (which renders blank).
func TestMergeStatusStats_LegitimateZeroPreserved(t *testing.T) {
	services := []string{"idle"}
	status := map[string]runner.ServiceStatus{"idle": {Running: true}}
	stats := map[string]runner.ServiceStats{
		"idle": {CPUPercent: 0, MemoryUsed: 0, MemoryLimit: 0},
	}

	got := mergeStatusStats(services, status, stats, true)
	if got[0].CPUPercent == nil || *got[0].CPUPercent != 0 {
		t.Errorf("idle CPU should be &0, got %v", got[0].CPUPercent)
	}
	if got[0].MemoryUsed == nil || *got[0].MemoryUsed != 0 {
		t.Errorf("idle MemoryUsed should be &0, got %v", got[0].MemoryUsed)
	}
	if got[0].MemoryLimit == nil || *got[0].MemoryLimit != 0 {
		t.Errorf("idle MemoryLimit should be &0, got %v", got[0].MemoryLimit)
	}
}

// TestMergeStatusStats_NotRequestedLeavesNil verifies that when statsRequested
// is false, stats pointers stay nil — preserving wire-shape compatibility for
// callers that did not opt into --stats.
func TestMergeStatusStats_NotRequestedLeavesNil(t *testing.T) {
	services := []string{"web"}
	status := map[string]runner.ServiceStatus{"web": {Running: true}}
	stats := map[string]runner.ServiceStats{
		"web": {CPUPercent: 99.9, MemoryUsed: 1, MemoryLimit: 1},
	}

	got := mergeStatusStats(services, status, stats, false)
	if got[0].CPUPercent != nil || got[0].MemoryUsed != nil || got[0].MemoryLimit != nil {
		t.Errorf("stats pointers must remain nil when not requested, got %+v", got[0])
	}
}

// TestCollectMultiProjectStats_PopulatesStats verifies the multi-project
// path threads stats through to each project's services.
func TestCollectMultiProjectStats_PopulatesStats(t *testing.T) {
	mocks := map[string]*mockComposer{
		"/a": {
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
			stats:    map[string]runner.ServiceStats{"web": {CPUPercent: 3.3, MemoryUsed: 100, MemoryLimit: 1000}},
		},
	}
	projects := []compose.Project{{Name: "a", ConfigDir: "/a"}}
	factory := func(dir string) runner.Composer { return mocks[dir] }

	result := collectMultiProjectStats(context.Background(), projects, factory, true, nil)
	if len(result) != 1 || len(result[0].Services) != 1 {
		t.Fatalf("unexpected result shape: %+v", result)
	}
	s := result[0].Services[0]
	if s.CPUPercent == nil || *s.CPUPercent != 3.3 {
		t.Errorf("CPUPercent = %v, want 3.3", s.CPUPercent)
	}
}

// TestCollectMultiProjectStats_NotRequestedSkipsStatsCall verifies that when
// showStats is false, ContainerStats is not called (saves the host-wide stats
// latency on the no-stats path).
func TestCollectMultiProjectStats_NotRequestedSkipsStatsCall(t *testing.T) {
	mock := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		statsErr: fmt.Errorf("stats should not have been called"),
	}
	projects := []compose.Project{{Name: "a", ConfigDir: "/a"}}
	factory := func(_ string) runner.Composer { return mock }

	// With showStats=false, statsErr must not surface — ContainerStats() not invoked.
	result := collectMultiProjectStats(context.Background(), projects, factory, false, nil)
	if len(result) != 1 || len(result[0].Services) != 1 {
		t.Fatalf("unexpected result shape: %+v", result)
	}
	if result[0].Services[0].CPUPercent != nil {
		t.Errorf("CPUPercent must be nil without --stats, got %v", result[0].Services[0].CPUPercent)
	}
}

// TestCollectMultiProjectStats_UsesBulkAggregator verifies that when bulkStats
// is supplied AND the composer implements bulkStatsAggregator, the bulk path
// runs and the per-project ContainerStats() fallback is NOT invoked. This is
// the optimization that makes multi-project --stats pay one host-wide
// `docker stats` cost regardless of project count.
func TestCollectMultiProjectStats_UsesBulkAggregator(t *testing.T) {
	mock := &mockComposerBulk{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
			// statsErr is what ContainerStats() returns. If the bulk path is taken
			// correctly, ContainerStats() is never called and this error never
			// surfaces — the test asserts a populated stats cell instead.
			statsErr: fmt.Errorf("ContainerStats should not have been called when bulk is used"),
		},
		bulkStats: map[string]runner.ServiceStats{"web": {CPUPercent: 7.7}},
	}
	projects := []compose.Project{{Name: "a", ConfigDir: "/a"}}
	factory := func(_ string) runner.Composer { return mock }
	bulk := map[string]runner.ServiceStats{"deadbeef0000": {CPUPercent: 0}} // contents irrelevant; presence triggers the bulk path

	result := collectMultiProjectStats(context.Background(), projects, factory, true, bulk)
	if len(result) != 1 || len(result[0].Services) != 1 {
		t.Fatalf("unexpected result shape: %+v", result)
	}
	s := result[0].Services[0]
	if s.CPUPercent == nil || *s.CPUPercent != 7.7 {
		t.Errorf("CPUPercent = %v, want 7.7 (bulk path)", s.CPUPercent)
	}
	if mock.containerStatsCalls != 0 {
		t.Errorf("ContainerStats called %d times, want 0 (bulk should bypass it)", mock.containerStatsCalls)
	}
	if mock.bulkCalls != 1 {
		t.Errorf("ContainerStatsFromBulk called %d times, want 1", mock.bulkCalls)
	}
}

// TestCollectMultiProjectStats_EmptyBulkSkipsPerProjectRetry verifies that
// when the host-wide stats fetch failed and the caller signaled this by
// passing a non-nil empty map, the bulk path still runs (only `docker
// compose ps` per project). Critically, per-project ContainerStats() is NOT
// called — that would re-trigger the failing `docker stats` call N times
// and violate the plan's "one ~1.5s cost regardless of project count"
// guarantee on the soft-fail path.
func TestCollectMultiProjectStats_EmptyBulkSkipsPerProjectRetry(t *testing.T) {
	mock := &mockComposerBulk{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
			// ContainerStats() must NOT be called — assert via call count below.
			// Set statsErr so any accidental call surfaces loudly in the result.
			statsErr: fmt.Errorf("ContainerStats must not be called on empty-bulk soft-fail"),
		},
		// bulkStats here is what ContainerStatsFromBulk returns — irrelevant for
		// this test since the function only joins against the (empty) bulk map
		// passed in, not this mock field.
		bulkStats: map[string]runner.ServiceStats{},
	}
	projects := []compose.Project{{Name: "a", ConfigDir: "/a"}}
	factory := func(_ string) runner.Composer { return mock }

	// Non-nil empty bulk map — the contract for "bulk fetch failed".
	result := collectMultiProjectStats(context.Background(), projects, factory, true, map[string]runner.ServiceStats{})
	if len(result) != 1 || len(result[0].Services) != 1 {
		t.Fatalf("unexpected result shape: %+v", result)
	}
	if mock.containerStatsCalls != 0 {
		t.Errorf("ContainerStats called %d times, want 0 (must not retry per-project on bulk failure)", mock.containerStatsCalls)
	}
	if mock.bulkCalls != 1 {
		t.Errorf("ContainerStatsFromBulk called %d times, want 1", mock.bulkCalls)
	}
}

// TestCollectMultiProjectStats_FallsBackWhenBulkNil verifies that when the
// host-wide stats fetch failed (bulkStats nil), the per-project ContainerStats
// fallback runs — behavior degrades to the legacy path rather than dropping
// stats entirely.
func TestCollectMultiProjectStats_FallsBackWhenBulkNil(t *testing.T) {
	mock := &mockComposerBulk{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
			stats:    map[string]runner.ServiceStats{"web": {CPUPercent: 3.3}},
		},
		bulkStats: map[string]runner.ServiceStats{"web": {CPUPercent: 99}}, // would be used if bulk path taken
	}
	projects := []compose.Project{{Name: "a", ConfigDir: "/a"}}
	factory := func(_ string) runner.Composer { return mock }

	result := collectMultiProjectStats(context.Background(), projects, factory, true, nil)
	if len(result) != 1 || len(result[0].Services) != 1 {
		t.Fatalf("unexpected result shape: %+v", result)
	}
	s := result[0].Services[0]
	if s.CPUPercent == nil || *s.CPUPercent != 3.3 {
		t.Errorf("CPUPercent = %v, want 3.3 (per-project fallback path)", s.CPUPercent)
	}
	if mock.containerStatsCalls != 1 {
		t.Errorf("ContainerStats called %d times, want 1 (fallback)", mock.containerStatsCalls)
	}
	if mock.bulkCalls != 0 {
		t.Errorf("ContainerStatsFromBulk called %d times, want 0", mock.bulkCalls)
	}
}

// TestFormatCPUCell_BlankWhenStopped verifies the CPU cell helper returns "" for
// stopped services even if a CPU pointer is set (defensive: prevents stale
// stats from leaking into a stopped row).
func TestFormatCPUCell_BlankWhenStopped(t *testing.T) {
	cpu := 5.5
	s := serviceStatus{Name: "x", Running: false, CPUPercent: &cpu}
	if got := formatCPUCell(s); got != "" {
		t.Errorf("formatCPUCell stopped = %q, want empty", got)
	}
}

// TestFormatMemCell_BlankWhenNil verifies the Mem cell helper returns "" when
// either MemoryUsed or MemoryLimit is nil.
func TestFormatMemCell_BlankWhenNil(t *testing.T) {
	used := int64(100)
	cases := []serviceStatus{
		{Name: "a", Running: true},                                          // both nil
		{Name: "b", Running: true, MemoryUsed: &used},                       // limit nil
		{Name: "c", Running: false, MemoryUsed: &used, MemoryLimit: &used},  // not running
	}
	for _, c := range cases {
		if got := formatMemCell(c); got != "" {
			t.Errorf("%s: formatMemCell = %q, want empty", c.Name, got)
		}
	}
}

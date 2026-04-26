package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

func mockFactory(mc *mockComposer) ComposerFactory {
	return func(string) runner.Composer { return mc }
}

type mockComposer struct {
	services  []string
	status    map[string]runner.ServiceStatus
	err       error
	statusErr error
}

func (m *mockComposer) Stop(ctx context.Context, containers []string, w io.Writer) error {
	return nil
}
func (m *mockComposer) Remove(ctx context.Context, containers []string, w io.Writer) error {
	return nil
}
func (m *mockComposer) Pull(ctx context.Context, containers []string, w io.Writer) error {
	return nil
}
func (m *mockComposer) Create(ctx context.Context, containers []string, w io.Writer) error {
	return nil
}
func (m *mockComposer) Start(ctx context.Context, containers []string, w io.Writer) error {
	return nil
}
func (m *mockComposer) ListServices(ctx context.Context) ([]string, error) {
	return m.services, m.err
}
func (m *mockComposer) ContainerStatus(ctx context.Context) (map[string]runner.ServiceStatus, error) {
	return m.status, m.statusErr
}

func (m *mockComposer) Logs(ctx context.Context, service string, follow bool, tail int, w io.Writer) error {
	return nil
}

// mockConfigComposer implements both runner.Composer and ConfigProvider.
type mockConfigComposer struct {
	mockComposer
	configFile     []byte
	configResolved []byte
	configErr      error
	validateErr    error
}

func (m *mockConfigComposer) ConfigFile(ctx context.Context) ([]byte, error) {
	return m.configFile, m.configErr
}
func (m *mockConfigComposer) ConfigResolved(ctx context.Context) ([]byte, error) {
	return m.configResolved, m.configErr
}
func (m *mockConfigComposer) EditCommand(ctx context.Context) (*exec.Cmd, error) {
	if m.configErr != nil {
		return nil, m.configErr
	}
	return exec.Command("echo", "edit"), nil
}
func (m *mockConfigComposer) ValidateConfig(ctx context.Context) error {
	return m.validateErr
}

func mockConfigFactory(mc *mockConfigComposer) ComposerFactory {
	return func(string) runner.Composer { return mc }
}

// mockExecComposer implements both runner.Composer and ExecProvider.
type mockExecComposer struct {
	mockComposer
	execErr error
}

func (m *mockExecComposer) ExecCommand(ctx context.Context, service string, command []string) (*exec.Cmd, error) {
	if m.execErr != nil {
		return nil, m.execErr
	}
	return exec.Command("echo", "exec", service), nil
}

func mockExecFactory(mc *mockExecComposer) ComposerFactory {
	return func(string) runner.Composer { return mc }
}

func TestNewModel_InitialState(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	if m.screen != screenSelectContainers {
		t.Errorf("initial screen = %d, want %d", m.screen, screenSelectContainers)
	}
}

func TestNewModel_SkipsPickerWhenComposerProvided(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (screenSelectContainers)", m.screen, screenSelectContainers)
	}
	if m.showPicker {
		t.Error("showPicker should be false when composer is provided")
	}
	if m.composer == nil {
		t.Error("composer should be set")
	}
}

func TestNewModel_ShowsPickerWhenNoComposer(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)

	if m.screen != screenSelectProject {
		t.Errorf("screen = %d, want %d (screenSelectProject)", m.screen, screenSelectProject)
	}
	if !m.showPicker {
		t.Error("showPicker should be true when no composer")
	}
}

func TestInit_LoadsProjectsWhenPickerShown(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)

	cmd := m.Init()
	if cmd == nil {
		t.Error("Init() should return a command when picker is shown")
	}
}

func TestWithLocalProjectLoader(t *testing.T) {
	mc := &mockComposer{}
	called := false
	loader := func(ctx context.Context) ([]compose.Project, error) {
		called = true
		return []compose.Project{{Name: "test", ConfigDir: "/test"}}, nil
	}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil, WithLocalProjectLoader(loader))

	if m.localProjectLoader == nil {
		t.Fatal("localProjectLoader should be set")
	}
	if m.projectLoader == nil {
		t.Fatal("projectLoader should be set by WithLocalProjectLoader")
	}

	// Execute the loader via loadProjects
	cmd := m.loadProjects()
	msg := cmd()
	pm := msg.(projectsMsg)
	if pm.err != nil {
		t.Fatalf("unexpected error: %v", pm.err)
	}
	if !called {
		t.Error("local loader should have been called")
	}
	if len(pm.projects) != 1 || pm.projects[0].Name != "test" {
		t.Errorf("projects = %v, want [{test /test}]", pm.projects)
	}
}

func TestWithConfigPath(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil, WithConfigPath("/tmp/test-servers.yml"))
	if m.configPath != "/tmp/test-servers.yml" {
		t.Errorf("configPath = %q, want %q", m.configPath, "/tmp/test-servers.yml")
	}
}

func TestWithConfig(t *testing.T) {
	mc := &mockComposer{}
	cfg := &config.Config{
		Servers: []config.Server{{Name: "test", Host: "user@host"}},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil, WithConfig(cfg))
	if m.config == nil {
		t.Fatal("config should be set")
	}
	if len(m.config.Servers) != 1 || m.config.Servers[0].Name != "test" {
		t.Errorf("config.Servers = %v, want [{test user@host}]", m.config.Servers)
	}
}

func TestLoadProjects_NilLoader_ReturnsError(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	// Ensure no loader is set
	m.projectLoader = nil

	cmd := m.loadProjects()
	msg := cmd()
	pm := msg.(projectsMsg)
	if pm.err == nil {
		t.Fatal("expected error when no loader configured")
	}
	if !strings.Contains(pm.err.Error(), "no project loader") {
		t.Errorf("error = %q, want it to contain 'no project loader'", pm.err.Error())
	}
}

func TestInit_LoadsServicesWhenPickerSkipped(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	cmd := m.Init()
	if cmd == nil {
		t.Error("Init() should return loadServices command when picker is skipped")
	}
}

func TestServicesMsg_SortsServicesCaseInsensitive(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	updated, _ := m.Update(servicesMsg{services: []string{"zebra", "Alpha", "beta"}})
	m = updated.(Model)

	want := []string{"Alpha", "beta", "zebra"}
	if len(m.services) != len(want) {
		t.Fatalf("got %d services, want %d", len(m.services), len(want))
	}
	for i, svc := range want {
		if m.services[i] != svc {
			t.Fatalf("service[%d] = %q, want %q", i, m.services[i], svc)
		}
	}
}

func TestSelectContainers_Toggle(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "postgres"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services

	// Toggle first item
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	m = updated.(Model)
	if !m.selected[0] {
		t.Error("item 0 should be selected after space")
	}

	// Toggle off
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	m = updated.(Model)
	if m.selected[0] {
		t.Error("item 0 should be deselected after second space")
	}
}

func TestSelectContainers_SelectAll(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "postgres", "redis"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services

	// Select all
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)
	for i := range m.services {
		if !m.selected[i] {
			t.Errorf("item %d should be selected after 'a'", i)
		}
	}

	// Deselect all
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)
	for i := range m.services {
		if m.selected[i] {
			t.Errorf("item %d should be deselected after second 'a'", i)
		}
	}
}

func TestSelectContainers_EnterIgnoredWhenNotConfirming(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected[0] = true

	// Enter with selection but not in confirming state should do nothing
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (should stay on container select)", m.screen, screenSelectContainers)
	}
}

func TestSelectContainers_EscGoesBackWhenPickerShown(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.showPicker = true
	m.services = mc.services
	m.selected[0] = true
	m.composer = mc
	m.projects = []compose.Project{{Name: "app", ConfigDir: "/app"}}
	m.projCursor = 0

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectProject {
		t.Errorf("screen = %d, want %d", m.screen, screenSelectProject)
	}
	if len(m.services) != 0 {
		t.Error("services should be cleared on back")
	}
	if m.svcStatus != nil {
		t.Error("svcStatus should be nil after going back")
	}
	if cmd != nil {
		t.Error("should not reload projects when already loaded")
	}
}

func TestSelectContainers_EscLoadsProjectsWhenNil(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.showPicker = true
	m.services = mc.services
	m.composer = mc
	// projects is nil (local fast-path skipped project screen)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectProject {
		t.Errorf("screen = %d, want %d", m.screen, screenSelectProject)
	}
	if cmd == nil {
		t.Error("should load projects when projects is nil")
	}
}

func TestSelectContainers_EscPreservesCursor(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.showPicker = true
	m.services = mc.services
	m.composer = mc
	m.projects = []compose.Project{
		{Name: "alpha", ConfigDir: "/a"},
		{Name: "beta", ConfigDir: "/b"},
		{Name: "gamma", ConfigDir: "/c"},
	}
	m.projCursor = 2 // user had selected "gamma"

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.projCursor != 2 {
		t.Errorf("projCursor = %d, want 2 (should preserve position)", m.projCursor)
	}
}

func TestSelectContainers_EscDoesNothingWhenPickerSkipped(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{"nginx": {Running: true}}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (should stay on container select)", m.screen, screenSelectContainers)
	}
	if m.svcStatus == nil {
		t.Error("svcStatus should be preserved when picker is skipped")
	}
}

func TestSelectContainers_QuitReturnsQuit(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
}

func TestSelectedContainers(t *testing.T) {
	m := Model{
		services: []string{"nginx", "postgres", "redis"},
		selected: map[int]bool{0: true, 2: true},
	}

	got := m.selectedContainers()
	if len(got) != 2 || got[0] != "nginx" || got[1] != "redis" {
		t.Errorf("selectedContainers() = %v, want [nginx redis]", got)
	}
}

func TestAllSelected(t *testing.T) {
	m := Model{
		services: []string{"a", "b"},
		selected: map[int]bool{0: true, 1: true},
	}
	if !m.allSelected() {
		t.Error("allSelected() = false, want true")
	}

	m.selected[1] = false
	if m.allSelected() {
		t.Error("allSelected() = true, want false")
	}
}

func TestHandleStepEvent_Done(t *testing.T) {
	m := Model{
		screen: screenProgress,
		steps: []stepState{
			{name: runner.StepStopping, status: runner.StatusRunning},
		},
		eventCh: make(chan runner.StepEvent),
	}

	updated, _ := m.handleStepEvent(runner.StepEvent{
		Step: runner.StepStopping, Status: runner.StatusDone,
	})
	m = updated.(Model)

	if m.steps[0].status != runner.StatusDone {
		t.Errorf("step status = %q, want %q", m.steps[0].status, runner.StatusDone)
	}
}

func TestHandleStepEvent_Failed(t *testing.T) {
	m := Model{
		screen: screenProgress,
		steps: []stepState{
			{name: runner.StepStopping, status: runner.StatusRunning},
		},
	}

	updated, _ := m.handleStepEvent(runner.StepEvent{
		Step: runner.StepStopping, Status: runner.StatusFailed,
	})
	m = updated.(Model)

	if !m.failed {
		t.Error("failed should be true after failed event")
	}
}

func TestView_AllScreens(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	// Container select screen (initial screen when composer provided)
	m.screen = screenSelectContainers
	m.services = []string{"nginx", "postgres"}
	m.selected[1] = true
	v := m.View()
	if v == "" {
		t.Error("viewSelectContainers returned empty")
	}
	if !strings.Contains(v, "services (1/2 selected)") {
		t.Errorf("viewSelectContainers() missing services summary: %q", v)
	}

	// Progress screen
	m.screen = screenProgress
	m.pendingOp = runner.Restart
	m.steps = []stepState{
		{name: "Stopping", status: runner.StatusDone},
		{name: "Starting", status: runner.StatusRunning},
	}
	v = m.View()
	if v == "" {
		t.Error("viewProgress returned empty")
	}
}

func TestSelectProject_Navigation(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.projects = []compose.Project{
		{Name: "alpha", ConfigDir: "/a"},
		{Name: "beta", ConfigDir: "/b"},
		{Name: "gamma", ConfigDir: "/c"},
	}

	// Move down
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.projCursor != 1 {
		t.Errorf("after j: projCursor = %d, want 1", m.projCursor)
	}

	// Move down again
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.projCursor != 2 {
		t.Errorf("after second j: projCursor = %d, want 2", m.projCursor)
	}

	// Can't go past last item
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.projCursor != 2 {
		t.Errorf("after third j: projCursor = %d, want 2", m.projCursor)
	}

	// Move back up
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = updated.(Model)
	if m.projCursor != 1 {
		t.Errorf("after k: projCursor = %d, want 1", m.projCursor)
	}
}

func TestSelectProject_EnterTransitionsToContainers(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.projects = []compose.Project{
		{Name: "my-app", ConfigDir: "/work/my-app"},
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (screenSelectContainers)", m.screen, screenSelectContainers)
	}
	if m.projName != "my-app" {
		t.Errorf("projName = %q, want %q", m.projName, "my-app")
	}
	if m.composer == nil {
		t.Error("composer should be set after project selection")
	}
}

func TestSelectProject_EnterWithNoProjects(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSelectProject {
		t.Errorf("screen = %d, want %d (should stay on project select)", m.screen, screenSelectProject)
	}
}

func TestSelectProject_QuitReturnsQuit(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
}

func TestProjectsMsg_PopulatesProjects(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)

	projects := []compose.Project{
		{Name: "alpha", ConfigDir: "/a"},
		{Name: "beta", ConfigDir: "/b"},
	}
	updated, _ := m.Update(projectsMsg{projects: projects})
	m = updated.(Model)

	if len(m.projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(m.projects))
	}
	if m.projCursor != 0 {
		t.Errorf("projCursor = %d, want 0", m.projCursor)
	}
}

func TestProjectsMsg_Error(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)

	updated, _ := m.Update(projectsMsg{err: io.ErrUnexpectedEOF})
	m = updated.(Model)

	if m.projErr == nil {
		t.Error("projErr should be set")
	}
}

func TestViewSelectProject_WithProjects(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.projects = []compose.Project{
		{Name: "api-proxy", ConfigDir: "/Work/docker/api-proxy"},
		{Name: "forms-app", ConfigDir: "/Work/docker/forms-app"},
	}

	v := m.View()
	if !strings.Contains(v, "select project") {
		t.Error("view should contain 'select project'")
	}
	if !strings.Contains(v, "api-proxy") {
		t.Error("view should contain 'api-proxy'")
	}
	if !strings.Contains(v, "forms-app") {
		t.Error("view should contain 'forms-app'")
	}
}

func TestViewSelectProject_Loading(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)

	v := m.View()
	if !strings.Contains(v, "Loading projects") {
		t.Error("view should show loading state")
	}
}

func TestViewSelectProject_Error(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.projErr = fmt.Errorf("connection refused")

	v := m.View()
	if !strings.Contains(v, "Error") {
		t.Error("view should show error state")
	}
	if !strings.Contains(v, "connection refused") {
		t.Error("view should show error message")
	}
	if strings.Contains(v, "esc back") {
		t.Error("local-only error should not show 'esc back'")
	}
}

func TestViewSelectProject_ErrorWithPicker(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, nil)
	m.screen = screenSelectProject
	m.showPicker = true
	m.projErr = fmt.Errorf("connection refused")

	v := m.View()
	if !strings.Contains(v, "esc back") {
		t.Error("error state should show 'esc back' when server picker is available")
	}
}

func TestViewSelectProject_Empty(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.projects = []compose.Project{}

	v := m.View()
	if !strings.Contains(v, "No Docker Compose projects found") {
		t.Error("view should show empty state message")
	}
	if strings.Contains(v, "esc back") {
		t.Error("local-only empty should not show 'esc back'")
	}
}

func TestViewSelectProject_EmptyWithPicker(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, nil)
	m.screen = screenSelectProject
	m.showPicker = true
	m.projects = []compose.Project{}

	v := m.View()
	if !strings.Contains(v, "esc back") {
		t.Error("empty state should show 'esc back' when server picker is available")
	}
}

func TestBreadcrumb_WithProjectName(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.projName = "api-proxy"

	// Container select screen
	m.screen = screenSelectContainers
	m.services = []string{"nginx"}
	v := m.View()
	if !strings.Contains(v, "cdeploy > api-proxy") {
		t.Errorf("container select breadcrumb should contain project name, got: %q", v)
	}

	// Progress screen
	m.screen = screenProgress
	m.selected[0] = true
	m.pendingOp = runner.Restart
	m.steps = []stepState{{name: "Stopping", status: runner.StatusRunning}}
	v = m.View()
	if !strings.Contains(v, "cdeploy > api-proxy") {
		t.Errorf("progress breadcrumb should contain project name, got: %q", v)
	}
}

func TestBreadcrumb_WithoutProjectName(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"nginx"}

	v := m.View()
	if !strings.Contains(v, "cdeploy") {
		t.Error("breadcrumb should contain 'cdeploy'")
	}
}

func TestViewSelectContainers_HealthIcons(t *testing.T) {
	mc := &mockComposer{
		services: []string{"api", "db", "web", "worker"},
		status: map[string]runner.ServiceStatus{
			"web":    {Running: true, Health: "healthy"},
			"api":    {Running: true, Health: "unhealthy"},
			"worker": {Running: true, Health: "starting"},
			"db":     {Running: true},
		},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"api", "db", "web", "worker"}
	m.svcStatus = mc.status

	v := m.View()

	// Should contain health icon "✗" for unhealthy (api)
	if !strings.Contains(v, "✗") {
		t.Error("view should contain '✗' for unhealthy service")
	}
	// Should contain "♥" for healthy (web)
	if !strings.Contains(v, "♥") {
		t.Error("view should contain '♥' for healthy service")
	}
	// Should contain "~" for starting (worker)
	if !strings.Contains(v, "~") {
		t.Error("view should contain '~' for starting service")
	}
}

func TestViewSelectContainers_HealthAlignment(t *testing.T) {
	mc := &mockComposer{
		services: []string{"web", "db"},
		status: map[string]runner.ServiceStatus{
			"web": {Running: true, Health: "healthy"},
			"db":  {Running: true},
		},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = mc.status

	v := m.View()
	lines := strings.Split(v, "\n")

	// Find lines containing service names, check they both have the dot character
	svcLines := []string{}
	for _, line := range lines {
		if strings.Contains(line, "web") || strings.Contains(line, "db") {
			svcLines = append(svcLines, line)
		}
	}
	if len(svcLines) != 2 {
		t.Fatalf("expected 2 service lines, got %d", len(svcLines))
	}

	// Both lines should contain the status dot
	for _, line := range svcLines {
		if !strings.Contains(line, "●") {
			t.Errorf("service line missing status dot: %q", line)
		}
	}
}

func TestViewSelectContainers_StatusDots(t *testing.T) {
	mc := &mockComposer{
		services: []string{"nginx", "postgres"},
		status: map[string]runner.ServiceStatus{
			"nginx":    {Running: true},
			"postgres": {Running: false},
		},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = mc.status

	v := m.View()
	if !strings.Contains(v, "●") {
		t.Error("view should contain status dot indicator")
	}
	if !strings.Contains(v, "nginx") {
		t.Error("view should contain 'nginx'")
	}
	if !strings.Contains(v, "postgres") {
		t.Error("view should contain 'postgres'")
	}
}

func TestServicesMsg_StoresRunningStatus(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	status := map[string]runner.ServiceStatus{
		"nginx":    {Running: true},
		"postgres": {Running: false},
	}
	updated, _ := m.Update(servicesMsg{
		services: []string{"nginx", "postgres"},
		status:   status,
	})
	m = updated.(Model)

	if m.svcStatus == nil {
		t.Fatal("svcStatus should be set")
	}
	if !m.svcStatus["nginx"].Running {
		t.Error("nginx should be running")
	}
	if m.svcStatus["postgres"].Running {
		t.Error("postgres should not be running")
	}
}

func TestEscFromProgress_GoesToContainers(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.done = true
	m.showPicker = true
	m.projName = "my-app"
	m.composer = mc

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (screenSelectContainers)", m.screen, screenSelectContainers)
	}
	if m.confirming {
		t.Error("confirming should be false after returning from progress")
	}
}

// Action key and confirmation tests

func TestActionKey_EntersConfirmation(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "postgres"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected[0] = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)

	if !m.confirming {
		t.Error("confirming should be true after pressing 'd' with selection")
	}
	if m.pendingOp != runner.Deploy {
		t.Errorf("pendingOp = %v, want Deploy", m.pendingOp)
	}
}

func TestActionKey_IgnoredWithNoSelection(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)

	if m.confirming {
		t.Error("confirming should be false when nothing is selected")
	}
}

func TestWarning_ShownWhenNoSelection(t *testing.T) {
	for _, key := range []rune{'r', 'd', 's'} {
		t.Run(string(key), func(t *testing.T) {
			mc := &mockComposer{services: []string{"nginx", "redis"}}
			m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
			m.screen = screenSelectContainers
			m.services = mc.services

			updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
			m = updated.(Model)

			if m.warning != warnNoSelection {
				t.Errorf("warning = %q, want %q", m.warning, warnNoSelection)
			}
		})
	}
}

func TestWarning_ClearedOnNextKeypress(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.warning = warnNoSelection

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)

	if m.warning != "" {
		t.Errorf("warning should be cleared after keypress, got %q", m.warning)
	}
}

func TestConfirmation_EnterProceeds(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected[0] = true
	m.confirming = true
	m.pendingOp = runner.Deploy
	m.width = 80
	m.height = 24

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenProgress {
		t.Errorf("screen = %d, want %d (screenProgress)", m.screen, screenProgress)
	}
}

func TestConfirmation_EscCancels(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected[0] = true
	m.confirming = true
	m.pendingOp = runner.Deploy

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.confirming {
		t.Error("confirming should be false after esc")
	}
	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (should stay on containers)", m.screen, screenSelectContainers)
	}
}

func TestConfirmation_NavigationLocked(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "postgres"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected[0] = true
	m.confirming = true
	m.pendingOp = runner.Deploy

	// j, k, space, a should all be ignored
	for _, key := range []rune{'j', 'k', ' ', 'a'} {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		m = updated.(Model)
		if !m.confirming {
			t.Errorf("confirming should remain true after pressing %q", string(key))
		}
		if m.svcCursor != 0 {
			t.Errorf("svcCursor should not change during confirmation, got %d after %q", m.svcCursor, string(key))
		}
	}
}

func TestConfirmation_QuitStillWorks(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected[0] = true
	m.confirming = true
	m.pendingOp = runner.Deploy

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("expected quit command during confirmation, got nil")
	}
}

func TestConfirmation_AllOperationKeys(t *testing.T) {
	tests := []struct {
		key rune
		op  runner.Operation
	}{
		{'r', runner.Restart},
		{'d', runner.Deploy},
		{'s', runner.StopOnly},
	}

	for _, tt := range tests {
		t.Run(string(tt.key), func(t *testing.T) {
			mc := &mockComposer{services: []string{"nginx"}}
			m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
			m.screen = screenSelectContainers
			m.services = mc.services
			m.selected[0] = true

			updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{tt.key}})
			m = updated.(Model)

			if !m.confirming {
				t.Errorf("confirming should be true after pressing %q", string(tt.key))
			}
			if m.pendingOp != tt.op {
				t.Errorf("pendingOp = %v, want %v", m.pendingOp, tt.op)
			}
		})
	}
}

func TestStatusMsg_ErrorSetsSvcErr(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services

	updated, _ := m.Update(statusMsg{err: fmt.Errorf("daemon not running")})
	m = updated.(Model)

	if m.svcErr == nil {
		t.Fatal("svcErr should be set after statusMsg with error")
	}
	if m.svcErr.Error() != "daemon not running" {
		t.Errorf("svcErr = %q, want %q", m.svcErr.Error(), "daemon not running")
	}
}

func TestStatusMsg_SuccessClearsSvcErr(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcErr = fmt.Errorf("previous error")

	updated, _ := m.Update(statusMsg{status: map[string]runner.ServiceStatus{"nginx": {Running: true}}})
	m = updated.(Model)

	if m.svcErr != nil {
		t.Errorf("svcErr should be nil after successful statusMsg, got %v", m.svcErr)
	}
	if !m.svcStatus["nginx"].Running {
		t.Error("svcStatus should be updated after successful statusMsg")
	}
}

func TestConfirmation_ViewShowsOperationAndServices(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "postgres"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected[0] = true
	m.selected[1] = true
	m.confirming = true
	m.pendingOp = runner.Deploy

	v := m.View()
	if !strings.Contains(v, "Deploy") {
		t.Error("confirmation view should contain operation name 'Deploy'")
	}
	if !strings.Contains(v, "nginx") {
		t.Error("confirmation view should contain service name 'nginx'")
	}
	if !strings.Contains(v, "postgres") {
		t.Error("confirmation view should contain service name 'postgres'")
	}
	if !strings.Contains(v, "confirm") {
		t.Error("confirmation view should contain 'confirm'")
	}
	if !strings.Contains(v, "cancel") {
		t.Error("confirmation view should contain 'cancel'")
	}
}

// --- Server picker tests ---

var testServers = []config.Server{
	{Name: "prod", Host: "user@prod.example.com"},
	{Name: "staging", Host: "deploy@staging.internal", ProjectDir: "/opt/apps"},
}

func mockConnectCb(mc *mockComposer) ConnectCallback {
	return func(server config.Server) (*exec.Cmd, ComposerFactory, ProjectLoader, func() error) {
		cmd := exec.Command("echo", "connected")
		factory := func(d string) runner.Composer { return mc }
		loader := func(ctx context.Context) ([]compose.Project, error) {
			return []compose.Project{{Name: "remote-app", ConfigDir: "/remote"}}, nil
		}
		disconnect := func() error { return nil }
		return cmd, factory, loader, disconnect
	}
}

func TestNewModel_StartScreenDecisionTable(t *testing.T) {
	mc := &mockComposer{}

	tests := []struct {
		name       string
		composer   runner.Composer
		servers    []config.Server
		wantScreen screen
	}{
		{"no servers, no composer -> project", nil, nil, screenSelectProject},
		{"no servers, composer -> containers", mc, nil, screenSelectContainers},
		{"servers, no composer -> server", nil, testServers, screenSelectServer},
		{"servers, composer -> server", mc, testServers, screenSelectServer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewModel(tt.composer, io.Discard, mockFactory(mc), tt.servers, mockConnectCb(mc))
			if m.screen != tt.wantScreen {
				t.Errorf("screen = %d, want %d", m.screen, tt.wantScreen)
			}
		})
	}
}

func TestNewModel_BackwardCompat_NilServers(t *testing.T) {
	mc := &mockComposer{}

	// With composer
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d", m.screen, screenSelectContainers)
	}

	// Without composer
	m = NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	if m.screen != screenSelectProject {
		t.Errorf("screen = %d, want %d", m.screen, screenSelectProject)
	}
}

func TestServerScreen_Navigation(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))

	// Initial cursor at 0 (Local)
	if m.serverCursor != 0 {
		t.Fatalf("initial serverCursor = %d, want 0", m.serverCursor)
	}

	// Move down
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.serverCursor != 1 {
		t.Errorf("after j: serverCursor = %d, want 1", m.serverCursor)
	}

	// Move down again
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.serverCursor != 2 {
		t.Errorf("after second j: serverCursor = %d, want 2", m.serverCursor)
	}

	// Can't go past last entry (Local + 2 servers = 3 entries, max index = 2)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.serverCursor != 2 {
		t.Errorf("after third j: serverCursor = %d, want 2 (should stay at end)", m.serverCursor)
	}

	// Move up
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = updated.(Model)
	if m.serverCursor != 1 {
		t.Errorf("after k: serverCursor = %d, want 1", m.serverCursor)
	}

	// Move up past beginning
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(Model)
	if m.serverCursor != 0 {
		t.Errorf("after up: serverCursor = %d, want 0", m.serverCursor)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(Model)
	if m.serverCursor != 0 {
		t.Errorf("after second up: serverCursor = %d, want 0 (should stay at start)", m.serverCursor)
	}
}

func TestServerScreen_LocalSelection(t *testing.T) {
	mc := &mockComposer{}
	localLoader := func(ctx context.Context) ([]compose.Project, error) { return nil, nil }
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc),
		WithLocalProjectLoader(localLoader))

	// Cursor at 0 = "Local", press enter
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSelectProject {
		t.Errorf("screen = %d, want %d (screenSelectProject)", m.screen, screenSelectProject)
	}
	if m.serverName != "" {
		t.Errorf("serverName should be empty for local, got %q", m.serverName)
	}
	if m.disconnectFunc != nil {
		t.Error("disconnectFunc should be nil for local")
	}
	if m.projectLoader == nil {
		t.Error("projectLoader should be restored to localProjectLoader for local")
	}
	if !m.showPicker {
		t.Error("showPicker should be true after local selection")
	}
	if cmd == nil {
		t.Error("should return loadProjects command")
	}
}

func TestServerScreen_LocalSelection_WithComposer(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))

	// Should start on server screen even though composer is set
	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want %d (screenSelectServer)", m.screen, screenSelectServer)
	}

	// Cursor at 0 = "Local", press enter — should skip to containers
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (screenSelectContainers)", m.screen, screenSelectContainers)
	}
	if m.composer != mc {
		t.Error("composer should be the local composer")
	}
	if !m.showPicker {
		t.Error("showPicker should be true so esc navigates back")
	}
	if cmd == nil {
		t.Error("should return loadServices command")
	}
}

func TestNewModel_ServersAlwaysShowServerScreen(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))

	if m.screen != screenSelectServer {
		t.Errorf("screen = %d, want %d (screenSelectServer)", m.screen, screenSelectServer)
	}
	if m.localComposer != mc {
		t.Error("localComposer should be preserved")
	}
}

func TestServerScreen_RemoteSelection(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))

	// Move to first remote server (index 1 = "prod")
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)

	// Press enter — should trigger connect
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.serverName != "prod" {
		t.Errorf("serverName = %q, want %q", m.serverName, "prod")
	}
	if m.disconnectFunc == nil {
		t.Error("disconnectFunc should be set after remote selection")
	}
	if m.composerFactory == nil {
		t.Error("composerFactory should be set after remote selection")
	}
	if m.projectLoader == nil {
		t.Error("projectLoader should be set after remote selection")
	}
	if cmd == nil {
		t.Error("should return tea.ExecProcess command")
	}
}

func TestServerScreen_ConnectSuccess(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.serverName = "prod"
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return []compose.Project{{Name: "app"}}, nil
	}

	// Simulate connect result success
	updated, cmd := m.Update(connectResultMsg{err: nil})
	m = updated.(Model)

	if m.screen != screenSelectProject {
		t.Errorf("screen = %d, want %d (screenSelectProject)", m.screen, screenSelectProject)
	}
	if m.serverErr != nil {
		t.Errorf("serverErr = %v, want nil", m.serverErr)
	}
	if !m.showPicker {
		t.Error("showPicker should be true after successful connect")
	}
	if cmd == nil {
		t.Error("should return loadProjects command")
	}
}

func TestServerScreen_ConnectError(t *testing.T) {
	mc := &mockComposer{}
	localLoader := func(ctx context.Context) ([]compose.Project, error) { return nil, nil }
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc),
		WithLocalProjectLoader(localLoader))
	m.serverName = "prod"
	// Simulate stale remote state set before connect attempt
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return nil, fmt.Errorf("remote loader")
	}
	m.disconnectFunc = func() error { return nil }

	// Simulate connect failure
	updated, _ := m.Update(connectResultMsg{err: fmt.Errorf("connection refused")})
	m = updated.(Model)

	if m.serverErr == nil {
		t.Fatal("serverErr should be set")
	}
	if m.serverErr.Error() != "connection refused" {
		t.Errorf("serverErr = %q, want %q", m.serverErr.Error(), "connection refused")
	}
	if m.projectLoader == nil {
		t.Error("projectLoader should be restored to localProjectLoader after connect failure")
	}
	if m.disconnectFunc != nil {
		t.Error("disconnectFunc should be cleared after connect failure")
	}
}

func TestServerScreen_QuitReturnsQuit(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
}

func TestEscFromProjectScreen_WithServers_GoesToServerScreen(t *testing.T) {
	mc := &mockComposer{}
	localLoader := func(ctx context.Context) ([]compose.Project, error) { return nil, nil }
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc),
		WithLocalProjectLoader(localLoader))
	// Simulate state after connecting to remote server and being on project screen
	m.screen = screenSelectProject
	m.serverName = "prod"
	m.showPicker = true
	disconnectCalled := false
	m.disconnectFunc = func() error { disconnectCalled = true; return nil }
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return nil, fmt.Errorf("remote loader")
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectServer {
		t.Errorf("screen = %d, want %d (screenSelectServer)", m.screen, screenSelectServer)
	}
	if m.serverName != "" {
		t.Errorf("serverName should be cleared, got %q", m.serverName)
	}
	if m.disconnectFunc != nil {
		t.Error("disconnectFunc should be nil after going back")
	}
	if m.projectLoader == nil {
		t.Error("projectLoader should be restored to localProjectLoader after going back")
	}

	// Disconnect is called async via tea.Cmd
	if cmd != nil {
		// Execute the command to trigger disconnect
		msg := cmd()
		_ = msg
		if !disconnectCalled {
			t.Error("disconnect should have been called")
		}
	}
}

func TestEscFromProjectScreen_WithoutServers_DoesNothing(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectProject
	m.showPicker = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectProject {
		t.Errorf("screen = %d, want %d (should stay on project screen)", m.screen, screenSelectProject)
	}
}

func TestViewSelectServer(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))

	v := m.View()
	if !strings.Contains(v, "select server") {
		t.Error("view should contain 'select server'")
	}
	if !strings.Contains(v, "Local") {
		t.Error("view should contain 'Local'")
	}
	if !strings.Contains(v, "prod") {
		t.Error("view should contain 'prod'")
	}
	if !strings.Contains(v, "staging") {
		t.Error("view should contain 'staging'")
	}
	if !strings.Contains(v, "user@prod.example.com") {
		t.Error("view should show host for prod")
	}
}

func TestViewSelectServer_WithError(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.serverErr = fmt.Errorf("connection timeout")

	v := m.View()
	if !strings.Contains(v, "Connection failed") {
		t.Error("view should show connection error")
	}
	if !strings.Contains(v, "connection timeout") {
		t.Error("view should show error message")
	}
}

func TestBreadcrumb_WithServerName(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.serverName = "prod"
	m.projName = "my-app"

	bc := m.breadcrumb()
	if !strings.Contains(bc, "prod") {
		t.Errorf("breadcrumb should contain server name badge, got: %q", bc)
	}
	if !strings.Contains(bc, "my-app") {
		t.Errorf("breadcrumb should contain project name, got: %q", bc)
	}
	if !strings.HasPrefix(bc, "cdeploy > ") {
		t.Errorf("breadcrumb should start with 'cdeploy > ', got: %q", bc)
	}
}

func TestBreadcrumb_ServerOnly(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.serverName = "staging"

	bc := m.breadcrumb()
	if !strings.Contains(bc, "staging") {
		t.Errorf("breadcrumb should contain server name badge, got: %q", bc)
	}
	if !strings.HasPrefix(bc, "cdeploy > ") {
		t.Errorf("breadcrumb should start with 'cdeploy > ', got: %q", bc)
	}
}

func TestInit_ServerScreen_ReturnsNil(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))

	cmd := m.Init()
	if cmd != nil {
		t.Error("Init() should return nil for server screen (static list)")
	}
}

// --- Server group tests ---

var testGroupedServers = []config.Server{
	{Name: "app.dev", Host: "user@app.dev", Group: "Dev"},
	{Name: "discovery.dev", Host: "user@discovery.dev", Group: "Dev"},
	{Name: "app.prod", Host: "user@app.prod", Group: "Production"},
	{Name: "discovery.prod", Host: "user@discovery.prod", Group: "Production"},
}

func TestBuildServerEntries_NoGroups(t *testing.T) {
	entries := buildServerEntries(testServers)
	// Should be: Local, prod, staging (no headers)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if entries[0].kind != entryLocal {
		t.Errorf("entries[0].kind = %d, want entryLocal", entries[0].kind)
	}
	if entries[1].kind != entryServer || entries[1].serverIdx != 0 {
		t.Errorf("entries[1] = %+v, want entryServer with serverIdx=0", entries[1])
	}
	if entries[2].kind != entryServer || entries[2].serverIdx != 1 {
		t.Errorf("entries[2] = %+v, want entryServer with serverIdx=1", entries[2])
	}
}

func TestBuildServerEntries_WithGroups(t *testing.T) {
	entries := buildServerEntries(testGroupedServers)
	// Should be: Local, Header-Dev, app.dev, discovery.dev, Header-Production, app.prod, discovery.prod
	if len(entries) != 7 {
		t.Fatalf("got %d entries, want 7", len(entries))
	}
	if entries[0].kind != entryLocal {
		t.Errorf("entries[0].kind = %d, want entryLocal", entries[0].kind)
	}
	if entries[1].kind != entryGroupHeader || entries[1].group != "Dev" {
		t.Errorf("entries[1] = %+v, want entryGroupHeader Dev", entries[1])
	}
	if entries[2].kind != entryServer || entries[2].serverIdx != 0 {
		t.Errorf("entries[2] = %+v, want entryServer idx=0", entries[2])
	}
	if entries[4].kind != entryGroupHeader || entries[4].group != "Production" {
		t.Errorf("entries[4] = %+v, want entryGroupHeader Production", entries[4])
	}
}

func TestBuildServerEntries_MixedGroupedAndUngrouped(t *testing.T) {
	servers := []config.Server{
		{Name: "standalone", Host: "user@standalone"},
		{Name: "app.dev", Host: "user@app.dev", Group: "Dev"},
	}
	entries := buildServerEntries(servers)
	// Should be: Local, standalone (no header), Header-Dev, app.dev
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}
	if entries[1].kind != entryServer {
		t.Errorf("entries[1].kind = %d, want entryServer (ungrouped)", entries[1].kind)
	}
	if entries[2].kind != entryGroupHeader {
		t.Errorf("entries[2].kind = %d, want entryGroupHeader", entries[2].kind)
	}
}

func TestBuildServerEntries_UngroupedAfterGrouped(t *testing.T) {
	// Bug case: grouped server appears before ungrouped in YAML.
	// Ungrouped servers must still appear right after Local.
	servers := []config.Server{
		{Name: "app.dev", Host: "user@app.dev", Group: "Dev"},
		{Name: "standalone", Host: "user@standalone"},
	}
	entries := buildServerEntries(servers)
	// Should be: Local, standalone (no header), Header-Dev, app.dev
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}
	if entries[1].kind != entryServer || servers[entries[1].serverIdx].Name != "standalone" {
		t.Errorf("entries[1] should be ungrouped 'standalone', got %+v", entries[1])
	}
	if entries[2].kind != entryGroupHeader || entries[2].group != "Dev" {
		t.Errorf("entries[2] should be group header 'Dev', got %+v", entries[2])
	}
	if entries[3].kind != entryServer || servers[entries[3].serverIdx].Name != "app.dev" {
		t.Errorf("entries[3] should be 'app.dev', got %+v", entries[3])
	}
}

func TestBuildServerEntries_Empty(t *testing.T) {
	entries := buildServerEntries(nil)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (Local only)", len(entries))
	}
	if entries[0].kind != entryLocal {
		t.Errorf("entries[0].kind = %d, want entryLocal", entries[0].kind)
	}
}

func TestServerScreen_GroupedNavigation(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testGroupedServers, mockConnectCb(mc))

	// entries: [Local(0), Header-Dev(1), app.dev(2), discovery.dev(3), Header-Prod(4), app.prod(5), discovery.prod(6)]
	if m.serverCursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.serverCursor)
	}

	// Down from Local should skip Header-Dev, land on app.dev (index 2)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.serverCursor != 2 {
		t.Errorf("after j from Local: cursor = %d, want 2 (app.dev)", m.serverCursor)
	}

	// Down to discovery.dev (index 3)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.serverCursor != 3 {
		t.Errorf("after j: cursor = %d, want 3 (discovery.dev)", m.serverCursor)
	}

	// Down should skip Header-Prod, land on app.prod (index 5)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.serverCursor != 5 {
		t.Errorf("after j from discovery.dev: cursor = %d, want 5 (app.prod)", m.serverCursor)
	}

	// Down to discovery.prod (index 6)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.serverCursor != 6 {
		t.Errorf("after j: cursor = %d, want 6 (discovery.prod)", m.serverCursor)
	}

	// Down at end stays
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.serverCursor != 6 {
		t.Errorf("after j at end: cursor = %d, want 6 (should stay)", m.serverCursor)
	}

	// Up from discovery.prod should land on app.prod (index 5)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = updated.(Model)
	if m.serverCursor != 5 {
		t.Errorf("after k: cursor = %d, want 5 (app.prod)", m.serverCursor)
	}

	// Up from app.prod should skip Header-Prod, land on discovery.dev (index 3)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = updated.(Model)
	if m.serverCursor != 3 {
		t.Errorf("after k from app.prod: cursor = %d, want 3 (discovery.dev)", m.serverCursor)
	}
}

func TestServerScreen_GroupedSelection(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testGroupedServers, mockConnectCb(mc))

	// Navigate to app.dev (index 2) and select
	m.serverCursor = 2
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.serverName != "app.dev" {
		t.Errorf("serverName = %q, want %q", m.serverName, "app.dev")
	}
}

func TestViewSelectServer_WithGroups(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testGroupedServers, mockConnectCb(mc))

	v := m.View()
	if !strings.Contains(v, "Local") {
		t.Error("view should contain 'Local'")
	}
	if !strings.Contains(v, "Dev") {
		t.Error("view should contain group header 'Dev'")
	}
	if !strings.Contains(v, "Production") {
		t.Error("view should contain group header 'Production'")
	}
	if !strings.Contains(v, "app.dev") {
		t.Error("view should contain 'app.dev'")
	}
	if !strings.Contains(v, "discovery.prod") {
		t.Error("view should contain 'discovery.prod'")
	}
}

func TestNextSelectable(t *testing.T) {
	entries := []serverEntry{
		{kind: entryLocal},
		{kind: entryGroupHeader},
		{kind: entryServer},
		{kind: entryGroupHeader},
		{kind: entryServer},
	}
	if got := nextSelectable(entries, 0); got != 2 {
		t.Errorf("nextSelectable(0) = %d, want 2", got)
	}
	if got := nextSelectable(entries, 2); got != 4 {
		t.Errorf("nextSelectable(2) = %d, want 4", got)
	}
	if got := nextSelectable(entries, 4); got != 4 {
		t.Errorf("nextSelectable(4) = %d, want 4 (at end)", got)
	}
}

func TestPrevSelectable(t *testing.T) {
	entries := []serverEntry{
		{kind: entryLocal},
		{kind: entryGroupHeader},
		{kind: entryServer},
		{kind: entryGroupHeader},
		{kind: entryServer},
	}
	if got := prevSelectable(entries, 4); got != 2 {
		t.Errorf("prevSelectable(4) = %d, want 2", got)
	}
	if got := prevSelectable(entries, 2); got != 0 {
		t.Errorf("prevSelectable(2) = %d, want 0", got)
	}
	if got := prevSelectable(entries, 0); got != 0 {
		t.Errorf("prevSelectable(0) = %d, want 0 (at start)", got)
	}
}

func TestPreselectedServer_InitReturnsCmd(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc),
		WithPreselectedServer(0))

	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want %d", m.screen, screenSelectServer)
	}

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() should return a command for preselected server")
	}
}

func TestPreselectedServer_ConnectTriggered(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc),
		WithPreselectedServer(0))

	// Simulate the preselectedConnectMsg that Init would send
	updated, cmd := m.Update(preselectedConnectMsg{})
	m = updated.(Model)

	if m.serverName != "prod" {
		t.Errorf("serverName = %q, want %q", m.serverName, "prod")
	}
	if m.disconnectFunc == nil {
		t.Error("disconnectFunc should be set")
	}
	if m.composerFactory == nil {
		t.Error("composerFactory should be set")
	}
	if m.projectLoader == nil {
		t.Error("projectLoader should be set")
	}
	if cmd == nil {
		t.Error("should return tea.ExecProcess command")
	}
}

// --- Logs screen tests ---

func TestLogsKey_TransitionsToScreenLogs(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "postgres"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcCursor = 1 // cursor on "postgres"
	m.width = 80
	m.height = 24

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)

	if m.screen != screenLogs {
		t.Errorf("screen = %d, want %d (screenLogs)", m.screen, screenLogs)
	}
	if m.logsService != "postgres" {
		t.Errorf("logsService = %q, want %q", m.logsService, "postgres")
	}
	if m.logsCancel == nil {
		t.Error("logsCancel should be set")
	}
	if cmd == nil {
		t.Error("should return readLogChunk command")
	}
}

func TestLogsKey_DoesNothingWhenServicesNil(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	// services is nil (loading)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (should stay on containers)", m.screen, screenSelectContainers)
	}
	if cmd != nil {
		t.Error("should not return a command")
	}
}

func TestLogsKey_DoesNothingWhenServicesEmpty(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{} // empty

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (should stay on containers)", m.screen, screenSelectContainers)
	}
	if cmd != nil {
		t.Error("should not return a command")
	}
}

func TestLogChunkMsg_AppendsContent(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.logsViewport = viewport.New(80, 20)
	// Set up a pipe so readLogChunk has something to read
	pr, _ := io.Pipe()
	m.logsPipeR = pr

	updated, _ := m.Update(logChunkMsg{data: []byte("line 1\n")})
	m = updated.(Model)

	if m.logsContent != "line 1\n" {
		t.Errorf("logsContent = %q, want %q", m.logsContent, "line 1\n")
	}

	updated, _ = m.Update(logChunkMsg{data: []byte("line 2\n")})
	m = updated.(Model)

	if m.logsContent != "line 1\nline 2\n" {
		t.Errorf("logsContent = %q, want %q", m.logsContent, "line 1\nline 2\n")
	}
}

func TestLogDoneMsg_WithError(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.logsViewport = viewport.New(80, 20)

	testErr := fmt.Errorf("connection lost")
	updated, _ := m.Update(logDoneMsg{err: testErr})
	m = updated.(Model)

	if !m.logsDone {
		t.Error("logsDone should be true")
	}
	if m.logsErr == nil {
		t.Fatal("logsErr should be set")
	}
	if m.logsErr.Error() != "connection lost" {
		t.Errorf("logsErr = %q, want %q", m.logsErr.Error(), "connection lost")
	}
	if !strings.Contains(m.logsContent, "Error: connection lost") {
		t.Errorf("logsContent should contain error, got %q", m.logsContent)
	}
}

func TestLogDoneMsg_WithoutError(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.logsViewport = viewport.New(80, 20)

	updated, _ := m.Update(logDoneMsg{err: nil})
	m = updated.(Model)

	if !m.logsDone {
		t.Error("logsDone should be true")
	}
	if m.logsErr != nil {
		t.Errorf("logsErr should be nil, got %v", m.logsErr)
	}
}

func TestLogsEsc_ReturnsToContainerScreen(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.logsContent = "some logs"
	m.logsCancel = func() {} // no-op cancel
	m.logsDone = false
	m.logsViewport = viewport.New(80, 20)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (screenSelectContainers)", m.screen, screenSelectContainers)
	}
	if m.logsService != "" {
		t.Errorf("logsService should be cleared, got %q", m.logsService)
	}
	if m.logsContent != "" {
		t.Errorf("logsContent should be cleared, got %q", m.logsContent)
	}
	if m.logsCancel != nil {
		t.Error("logsCancel should be nil")
	}
	if m.logsDone {
		t.Error("logsDone should be false")
	}
	if m.logsErr != nil {
		t.Error("logsErr should be nil")
	}
	if cmd == nil {
		t.Error("should return refreshStatus command")
	}
}

func TestLogsGKey_DoesNotCrash(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.logsViewport = viewport.New(80, 20)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	m = updated.(Model)

	if m.screen != screenLogs {
		t.Errorf("screen = %d, want %d (should stay on logs)", m.screen, screenLogs)
	}
}

func TestViewLogs_RendersBreadcrumb(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.logsViewport = viewport.New(80, 20)
	m.width = 80
	m.height = 24

	v := m.View()
	if !strings.Contains(v, "logs") {
		t.Error("view should contain 'logs'")
	}
	if !strings.Contains(v, "nginx") {
		t.Error("view should contain service name 'nginx'")
	}
	if !strings.Contains(v, "esc back") {
		t.Error("view should contain 'esc back' in help")
	}
	if !strings.Contains(v, "G bottom") {
		t.Error("view should contain 'G bottom' in help")
	}
}

func TestViewSelectContainers_HelpIncludesLogs(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.width = 200

	v := m.View()
	if !strings.Contains(v, "l logs") {
		t.Error("container screen help should contain 'l logs'")
	}
}

func TestLogChunkMsg_IgnoredWhenNotOnLogScreen(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers // not on log screen
	m.logsPipeR = nil                 // pipe cleared by esc

	updated, cmd := m.Update(logChunkMsg{data: []byte("stale data")})
	m = updated.(Model)

	if m.logsContent != "" {
		t.Errorf("logsContent should remain empty, got %q", m.logsContent)
	}
	if cmd != nil {
		t.Error("should not return a command for stale logChunkMsg")
	}
}

func TestLogDoneMsg_IgnoredWhenNotOnLogScreen(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers

	updated, cmd := m.Update(logDoneMsg{err: fmt.Errorf("stale error")})
	m = updated.(Model)

	if m.logsDone {
		t.Error("logsDone should remain false for stale logDoneMsg")
	}
	if m.logsErr != nil {
		t.Error("logsErr should remain nil for stale logDoneMsg")
	}
	if cmd != nil {
		t.Error("should not return a command for stale logDoneMsg")
	}
}

func TestPreselectedServer_OutOfRange(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc),
		WithPreselectedServer(99))

	cmd := m.Init()
	if cmd != nil {
		t.Error("Init() should return nil for out-of-range preselection")
	}
}

// --- Log viewer wrap/pretty toggle tests ---

func setupLogsModel() Model {
	mc := &mockComposer{services: []string{"app"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.services = mc.services
	m.composer = mc
	m.logsService = "app"
	m.logsContent = `app | {"level":"info","msg":"hello"}`
	m.logsWrap = true
	m.logsPretty = false
	m.logsViewport = viewport.New(80, 20)
	m.logsViewport.SetHorizontalStep(0)
	m.logsViewport.SetContent(m.logsContent)
	m.width = 84
	m.height = 26
	return m
}

func TestLogsScreen_WKeyTogglesWrap(t *testing.T) {
	m := setupLogsModel()
	if !m.logsWrap {
		t.Fatal("logsWrap should default to true")
	}

	// Toggle wrap off
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	m = updated.(Model)
	if m.logsWrap {
		t.Error("logsWrap should be false after pressing 'w'")
	}

	// Toggle wrap back on
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	m = updated.(Model)
	if !m.logsWrap {
		t.Error("logsWrap should be true after pressing 'w' again")
	}
}

func TestLogsScreen_PKeyTogglesPretty(t *testing.T) {
	m := setupLogsModel()
	if m.logsPretty {
		t.Fatal("logsPretty should default to false")
	}

	// Toggle pretty on
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(Model)
	if !m.logsPretty {
		t.Error("logsPretty should be true after pressing 'p'")
	}

	// Viewport content should be reformatted with pretty JSON
	content := m.logsViewport.View()
	if !strings.Contains(content, "level") {
		t.Errorf("viewport should contain formatted JSON after pretty toggle, got:\n%s", content)
	}

	// Toggle pretty off
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(Model)
	if m.logsPretty {
		t.Error("logsPretty should be false after pressing 'p' again")
	}
}

func TestLogsScreen_WrapUpdatesHorizontalStep(t *testing.T) {
	m := setupLogsModel()

	// Wrap off → horizontal scroll enabled
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	m = updated.(Model)
	if m.logsWrap {
		t.Error("logsWrap should be false")
	}
	// We can't directly read HorizontalStep, but we verify the toggle works
	// by checking the model state is consistent

	// Wrap on → horizontal scroll disabled
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	m = updated.(Model)
	if !m.logsWrap {
		t.Error("logsWrap should be true")
	}
}

func TestLogsScreen_WindowResizeReformats(t *testing.T) {
	m := setupLogsModel()
	m.logsPretty = true
	m.applyLogFormat()

	// Resize window
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m = updated.(Model)

	if m.logsViewport.Width != 56 { // 60 - 4
		t.Errorf("viewport width = %d, want 56", m.logsViewport.Width)
	}
}

func TestLogsScreen_LogChunkAppliesFormat(t *testing.T) {
	m := setupLogsModel()
	m.logsContent = ""
	m.logsPretty = true
	m.logsSession = 42

	// Simulate a pipe reader so readLogChunk doesn't panic
	pr, pw := io.Pipe()
	m.logsPipeR = pr
	go func() { pw.Close() }()

	updated, _ := m.Update(logChunkMsg{
		data:    []byte(`svc | {"key":"val"}` + "\n"),
		session: 42,
	})
	m = updated.(Model)

	content := m.logsViewport.View()
	if !strings.Contains(content, "key") {
		t.Errorf("viewport should contain formatted content after logChunkMsg, got:\n%s", content)
	}
}

func TestWaitForEvent_ReturnsStepEventMsg(t *testing.T) {
	ch := make(chan runner.StepEvent, 1)
	m := Model{eventCh: ch}
	want := runner.StepEvent{Step: "pull", Status: runner.StatusRunning}
	ch <- want

	msg := m.waitForEvent()()
	got, ok := msg.(stepEventMsg)
	if !ok {
		t.Fatalf("msg type = %T, want stepEventMsg", msg)
	}
	if runner.StepEvent(got) != want {
		t.Fatalf("step event = %+v, want %+v", runner.StepEvent(got), want)
	}
}

func TestWaitForEvent_ReturnsPipelineDoneWhenClosed(t *testing.T) {
	ch := make(chan runner.StepEvent)
	close(ch)
	m := Model{eventCh: ch}

	msg := m.waitForEvent()()
	if _, ok := msg.(pipelineDoneMsg); !ok {
		t.Fatalf("msg type = %T, want pipelineDoneMsg", msg)
	}
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestReadLogChunk_ReturnsChunk(t *testing.T) {
	m := Model{
		logsPipeR:   strings.NewReader("hello"),
		logsSession: 7,
	}

	msg := m.readLogChunk()()
	got, ok := msg.(logChunkMsg)
	if !ok {
		t.Fatalf("msg type = %T, want logChunkMsg", msg)
	}
	if string(got.data) != "hello" {
		t.Fatalf("chunk data = %q, want %q", string(got.data), "hello")
	}
	if got.session != 7 {
		t.Fatalf("chunk session = %d, want 7", got.session)
	}
}

func TestReadLogChunk_ReturnsDoneOnEOF(t *testing.T) {
	m := Model{
		logsPipeR:   strings.NewReader(""),
		logsSession: 9,
	}

	msg := m.readLogChunk()()
	got, ok := msg.(logDoneMsg)
	if !ok {
		t.Fatalf("msg type = %T, want logDoneMsg", msg)
	}
	if got.err != nil {
		t.Fatalf("done err = %v, want nil", got.err)
	}
	if got.session != 9 {
		t.Fatalf("done session = %d, want 9", got.session)
	}
}

func TestReadLogChunk_ReturnsDoneOnReadError(t *testing.T) {
	m := Model{
		logsPipeR:   errReader{err: errors.New("boom")},
		logsSession: 11,
	}

	msg := m.readLogChunk()()
	got, ok := msg.(logDoneMsg)
	if !ok {
		t.Fatalf("msg type = %T, want logDoneMsg", msg)
	}
	if got.err == nil || got.err.Error() != "boom" {
		t.Fatalf("done err = %v, want boom", got.err)
	}
	if got.session != 11 {
		t.Fatalf("done session = %d, want 11", got.session)
	}
}

func TestReadLogChunk_ReturnsDoneOnZeroReadWithoutError(t *testing.T) {
	m := Model{
		logsPipeR:   zeroReader{},
		logsSession: 13,
	}

	msg := m.readLogChunk()()
	got, ok := msg.(logDoneMsg)
	if !ok {
		t.Fatalf("msg type = %T, want logDoneMsg", msg)
	}
	if got.err != nil {
		t.Fatalf("done err = %v, want nil", got.err)
	}
	if got.session != 13 {
		t.Fatalf("done session = %d, want 13", got.session)
	}
}

func TestLogsScreen_EnterLogsDefaultState(t *testing.T) {
	mc := &mockComposer{services: []string{"app"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.composer = mc
	m.width = 84
	m.height = 26
	m.svcCursor = 0

	updated, _ := m.enterLogs()
	m = updated.(Model)

	if !m.logsWrap {
		t.Error("logsWrap should default to true on entering logs")
	}
	if m.logsPretty {
		t.Error("logsPretty should default to false on entering logs")
	}
}

func TestLogsScreen_HelpBarWrapOn(t *testing.T) {
	m := setupLogsModel()
	m.logsWrap = true

	v := m.View()
	if !strings.Contains(v, "w unwrap") {
		t.Errorf("help bar should show 'w unwrap' when wrap is on, got:\n%s", v)
	}
	if strings.Contains(v, "<-/-> scroll") {
		t.Errorf("help bar should NOT show horizontal scroll hint when wrap is on, got:\n%s", v)
	}
}

func TestLogsScreen_HelpBarWrapOff(t *testing.T) {
	m := setupLogsModel()
	m.logsWrap = false

	v := m.View()
	if !strings.Contains(v, "w wrap") {
		t.Errorf("help bar should show 'w wrap' when wrap is off, got:\n%s", v)
	}
	if !strings.Contains(v, "<-/-> scroll") {
		t.Errorf("help bar should show horizontal scroll hint when wrap is off, got:\n%s", v)
	}
}

func TestLogsScreen_HelpBarPrettyToggle(t *testing.T) {
	m := setupLogsModel()

	m.logsPretty = false
	v := m.View()
	if !strings.Contains(v, "p pretty") {
		t.Errorf("help bar should show 'p pretty' when pretty is off, got:\n%s", v)
	}

	m.logsPretty = true
	v = m.View()
	if !strings.Contains(v, "p raw") {
		t.Errorf("help bar should show 'p raw' when pretty is on, got:\n%s", v)
	}
}

func TestLogsScreen_EscClearsWrapPretty(t *testing.T) {
	m := setupLogsModel()
	m.logsWrap = true
	m.logsPretty = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d", m.screen, screenSelectContainers)
	}
	if m.logsWrap {
		t.Error("logsWrap should be cleared after esc")
	}
	if m.logsPretty {
		t.Error("logsPretty should be cleared after esc")
	}
}

// Regression: a wrapped partial line extended by the next chunk must not
// duplicate the earlier wrapped segments (P1 corruption bug).
func TestLogsScreen_IncrementalWrapNoDuplication(t *testing.T) {
	m := setupLogsModel()
	m.logsContent = ""
	m.logsFormatted = ""
	m.logsRawOff = 0
	m.logsWrap = true
	m.logsPretty = false
	m.logsViewport = viewport.New(5, 20) // width=5 to force wrapping
	m.logsViewport.SetHorizontalStep(0)
	m.logsSession = 1

	pr, pw := io.Pipe()
	m.logsPipeR = pr

	// Chunk 1: partial line, no newline — 10 chars wraps to 2 segments
	m.logsContent = strings.Repeat("a", 10)
	m.applyLogFormat()
	content1 := m.logsFormatted
	// logsFormatted should be empty (no complete lines yet)
	if content1 != "" {
		t.Errorf("no complete lines yet, logsFormatted should be empty, got %q", content1)
	}

	// Chunk 2: extend the same line and complete it
	m.logsContent = strings.Repeat("a", 10) + "bbbb\n"
	m.applyLogFormat()

	// The raw line "aaaaaaaaaabbbb" (14 chars) should wrap to: "aaaaa", "aaaaa", "aaaa", "bbbb"
	// No duplicated segments
	viewContent := m.logsFormatted
	lines := strings.Split(viewContent, "\n")
	aCount := 0
	for _, l := range lines {
		aCount += strings.Count(l, "a")
	}
	if aCount != 10 {
		t.Errorf("expected 10 'a' chars total, got %d in formatted output: %q", aCount, viewContent)
	}

	pw.Close()
	pr.Close()
}

// Verify that incremental formatting only scans new data, not the full buffer.
func TestLogsScreen_IncrementalOffsetAdvances(t *testing.T) {
	m := setupLogsModel()
	m.logsContent = ""
	m.logsFormatted = ""
	m.logsRawOff = 0
	m.logsWrap = false
	m.logsPretty = false
	m.logsViewport = viewport.New(80, 20)

	// Add two complete lines
	m.logsContent = "line1\nline2\n"
	m.applyLogFormat()

	if m.logsRawOff != 12 { // len("line1\nline2\n")
		t.Errorf("logsRawOff = %d, want 12", m.logsRawOff)
	}

	// Add a third line — offset should advance past it
	m.logsContent += "line3\n"
	m.applyLogFormat()

	if m.logsRawOff != 18 { // 12 + len("line3\n")
		t.Errorf("logsRawOff = %d, want 18", m.logsRawOff)
	}

	// logsFormatted should contain all three lines
	if !strings.Contains(m.logsFormatted, "line1") ||
		!strings.Contains(m.logsFormatted, "line2") ||
		!strings.Contains(m.logsFormatted, "line3") {
		t.Errorf("logsFormatted should contain all lines, got: %q", m.logsFormatted)
	}
}

func TestShortenPath_HomeDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home dir")
	}

	tests := []struct {
		name string
		dir  string
		want string
	}{
		{"under home", home + "/projects/app", "~/projects/app"},
		{"home itself", home, "~"},
		{"not under home", "/usr/local/bin", "/usr/local/bin"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortenPath(tt.dir)
			if got != tt.want {
				t.Errorf("shortenPath(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestSortServices_CaseInsensitive(t *testing.T) {
	input := []string{"Zebra", "alpha", "BETA", "gamma"}
	got := sortServices(input)

	want := []string{"alpha", "BETA", "gamma", "Zebra"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sorted[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Original should be unmodified
	if input[0] != "Zebra" {
		t.Error("sortServices modified original slice")
	}
}

func TestSortServices_Empty(t *testing.T) {
	got := sortServices(nil)
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestSortServices_TieBreaker(t *testing.T) {
	input := []string{"Beta", "beta", "alpha"}
	got := sortServices(input)

	// "alpha" first, then "Beta" vs "beta" — uppercase B < lowercase b
	if got[0] != "alpha" {
		t.Errorf("got[0] = %q, want %q", got[0], "alpha")
	}
	if got[1] != "Beta" {
		t.Errorf("got[1] = %q, want %q", got[1], "Beta")
	}
	if got[2] != "beta" {
		t.Errorf("got[2] = %q, want %q", got[2], "beta")
	}
}

func TestAllSelected_Empty(t *testing.T) {
	m := Model{services: nil, selected: nil}
	if m.allSelected() {
		t.Error("allSelected() = true for empty services, want false")
	}
}

func TestAllSelected_AllTrue(t *testing.T) {
	m := Model{
		services: []string{"web", "db"},
		selected: map[int]bool{0: true, 1: true},
	}
	if !m.allSelected() {
		t.Error("allSelected() = false, want true")
	}
}

func TestAllSelected_SomeFalse(t *testing.T) {
	m := Model{
		services: []string{"web", "db", "redis"},
		selected: map[int]bool{0: true, 1: false, 2: true},
	}
	if m.allSelected() {
		t.Error("allSelected() = true, want false")
	}
}

func TestViewProgress_Running(t *testing.T) {
	m := Model{
		screen:    screenProgress,
		pendingOp: runner.Deploy,
		steps: []stepState{
			{name: "Stop", status: runner.StatusDone},
			{name: "Pull", status: runner.StatusRunning},
			{name: "Create", status: ""},
		},
		width: 80,
	}

	view := m.viewProgress()
	if !strings.Contains(view, "Stop") || !strings.Contains(view, "Pull") {
		t.Errorf("viewProgress should show step names, got: %q", view)
	}
}

func TestViewProgress_AllDone(t *testing.T) {
	m := Model{
		screen:    screenProgress,
		pendingOp: runner.Restart,
		steps: []stepState{
			{name: "Stop", status: runner.StatusDone},
			{name: "Start", status: runner.StatusDone},
		},
		done:  true,
		width: 80,
	}

	view := m.viewProgress()
	if !strings.Contains(view, "esc") {
		t.Errorf("done progress should show esc hint, got: %q", view)
	}
}

func TestViewProgress_Failed(t *testing.T) {
	m := Model{
		screen:    screenProgress,
		pendingOp: runner.Deploy,
		steps: []stepState{
			{name: "Stop", status: runner.StatusDone},
			{name: "Pull", status: runner.StatusFailed},
		},
		done:       true,
		failed:     true,
		logContent: "pull failed",
		width:      80,
	}

	view := m.viewProgress()
	if !strings.Contains(view, "Pull") {
		t.Errorf("failed progress should show failed step, got: %q", view)
	}
}

func TestLoadServices_Success(t *testing.T) {
	mc := &mockComposer{
		services: []string{"web", "db"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}, "db": {Running: false}},
	}

	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.composer = mc
	m.ctx = context.Background()

	cmd := m.loadServices()
	msg := cmd()

	svcMsg, ok := msg.(servicesMsg)
	if !ok {
		t.Fatalf("expected servicesMsg, got %T", msg)
	}
	if svcMsg.err != nil {
		t.Fatalf("unexpected error: %v", svcMsg.err)
	}
	if len(svcMsg.services) != 2 {
		t.Errorf("got %d services, want 2", len(svcMsg.services))
	}
}

func TestLoadServices_ListError(t *testing.T) {
	mc := &mockComposer{err: fmt.Errorf("docker down")}

	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.composer = mc
	m.ctx = context.Background()

	cmd := m.loadServices()
	msg := cmd()

	svcMsg, ok := msg.(servicesMsg)
	if !ok {
		t.Fatalf("expected servicesMsg, got %T", msg)
	}
	if svcMsg.err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestLoadServices_StatusError(t *testing.T) {
	mc := &mockComposer{
		services:  []string{"web"},
		statusErr: fmt.Errorf("status failed"),
	}

	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.composer = mc
	m.ctx = context.Background()

	cmd := m.loadServices()
	msg := cmd()

	svcMsg, ok := msg.(servicesMsg)
	if !ok {
		t.Fatalf("expected servicesMsg, got %T", msg)
	}
	if svcMsg.err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestRefreshStatus_Success(t *testing.T) {
	mc := &mockComposer{
		status: map[string]runner.ServiceStatus{"web": {Running: true}},
	}

	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.composer = mc
	m.ctx = context.Background()

	cmd := m.refreshStatus()
	msg := cmd()

	stMsg, ok := msg.(statusMsg)
	if !ok {
		t.Fatalf("expected statusMsg, got %T", msg)
	}
	if stMsg.err != nil {
		t.Fatalf("unexpected error: %v", stMsg.err)
	}
	if !stMsg.status["web"].Running {
		t.Error("web should be running")
	}
}

func TestRefreshStatus_Error(t *testing.T) {
	mc := &mockComposer{statusErr: fmt.Errorf("timeout")}

	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.composer = mc
	m.ctx = context.Background()

	cmd := m.refreshStatus()
	msg := cmd()

	stMsg, ok := msg.(statusMsg)
	if !ok {
		t.Fatalf("expected statusMsg, got %T", msg)
	}
	if stMsg.err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestLoadProjects_Success(t *testing.T) {
	projects := []compose.Project{
		{Name: "app1", ConfigDir: "/app1"},
		{Name: "app2", ConfigDir: "/app2"},
	}

	m := NewModel(nil, io.Discard, nil, nil, nil)
	m.ctx = context.Background()
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return projects, nil
	}

	cmd := m.loadProjects()
	msg := cmd()

	projMsg, ok := msg.(projectsMsg)
	if !ok {
		t.Fatalf("expected projectsMsg, got %T", msg)
	}
	if projMsg.err != nil {
		t.Fatalf("unexpected error: %v", projMsg.err)
	}
	if len(projMsg.projects) != 2 {
		t.Errorf("got %d projects, want 2", len(projMsg.projects))
	}
}

func TestLoadProjects_Error(t *testing.T) {
	m := NewModel(nil, io.Discard, nil, nil, nil)
	m.ctx = context.Background()
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return nil, fmt.Errorf("docker not running")
	}

	cmd := m.loadProjects()
	msg := cmd()

	projMsg, ok := msg.(projectsMsg)
	if !ok {
		t.Fatalf("expected projectsMsg, got %T", msg)
	}
	if projMsg.err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestViewSelectContainers_ConfirmState(t *testing.T) {
	mc := &mockComposer{
		services: []string{"web", "db"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}, "db": {Running: true}},
	}

	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"web", "db"}
	m.selected = map[int]bool{0: true, 1: true}
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.confirming = true
	m.pendingOp = runner.Deploy
	m.svcCursor = 0
	m.width = 80
	m.height = 24

	view := m.viewSelectContainers()
	// When confirming, should show the confirmation prompt
	if !strings.Contains(view, "Deploy") {
		t.Errorf("confirming view should mention the operation, got: %q", view)
	}
}

// --- Config screen tests ---

func TestConfigScreen_CKeyEntersConfig(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
		configFile: []byte("services:\n  web:\n    image: nginx\n"),
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.width = 80
	m.height = 24

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	model := result.(Model)

	if model.screen != screenConfig {
		t.Errorf("screen = %d, want %d (screenConfig)", model.screen, screenConfig)
	}
	if cmd == nil {
		t.Error("expected a cmd to fetch config file")
	}
	if model.configSession != 1 {
		t.Errorf("configSession = %d, want 1", model.configSession)
	}
}

func TestConfigScreen_CKeyIgnoredWithoutConfigProvider(t *testing.T) {
	mc := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	model := result.(Model)

	if model.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (should stay on containers)", model.screen, screenSelectContainers)
	}
}

func TestConfigScreen_EscCleansUp(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
		configFile: []byte("test content"),
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenConfig
	m.configContent = []byte("test")
	m.configResolved = []byte("resolved")
	m.configShowRes = true
	v := true
	m.configValid = &v
	m.configValidMsg = "ok"
	m.configSession = 5

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := result.(Model)

	if model.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d", model.screen, screenSelectContainers)
	}
	if model.configContent != nil {
		t.Error("configContent should be nil after esc")
	}
	if model.configResolved != nil {
		t.Error("configResolved should be nil after esc")
	}
	if model.configShowRes {
		t.Error("configShowRes should be false after esc")
	}
	if model.configErr != nil {
		t.Error("configErr should be nil after esc")
	}
	if model.configValid != nil {
		t.Error("configValid should be nil after esc")
	}
	if model.configValidMsg != "" {
		t.Error("configValidMsg should be empty after esc")
	}
}

func TestConfigScreen_ToggleRawResolved(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
		configFile:     []byte("raw content"),
		configResolved: []byte("resolved content"),
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configContent = mc.configFile
	m.configSession = 1
	m.width = 80
	m.height = 24
	m.configViewport = viewport.New(76, 18)

	// Toggle to resolved — no cache yet, should trigger fetch
	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model := result.(Model)
	if !model.configShowRes {
		t.Error("configShowRes should be true after first r press")
	}
	if cmd == nil {
		t.Error("expected a cmd to fetch resolved config")
	}

	// Simulate resolved data arriving
	model.configResolved = mc.configResolved

	// Toggle back to raw
	result, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model = result.(Model)
	if model.configShowRes {
		t.Error("configShowRes should be false after second r press")
	}

	// Toggle to resolved again — cached this time
	result, cmd = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model = result.(Model)
	if !model.configShowRes {
		t.Error("configShowRes should be true after third r press")
	}
	if cmd != nil {
		t.Error("should not fetch again when resolved is cached")
	}
}

func TestConfigScreen_StaleMessageDiscarded(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configSession = 5

	// Message from old session
	result, _ := m.Update(configFileMsg{data: []byte("stale"), session: 3})
	model := result.(Model)
	if model.configContent != nil {
		t.Error("stale configFileMsg should be discarded")
	}

	// Message from current session
	result, _ = m.Update(configFileMsg{data: []byte("current"), session: 5})
	model = result.(Model)
	if string(model.configContent) != "current" {
		t.Errorf("configContent = %q, want 'current'", string(model.configContent))
	}
}

func TestConfigScreen_StaleMessageDiscardedWhenNotOnScreen(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.configSession = 5

	// Message arrives while on containers screen
	result, _ := m.Update(configFileMsg{data: []byte("stale"), session: 5})
	model := result.(Model)
	if model.configContent != nil {
		t.Error("configFileMsg should be discarded when not on config screen")
	}
}

func TestConfigScreen_ConfigFileMsgError(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
		},
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configSession = 1

	result, _ := m.Update(configFileMsg{err: fmt.Errorf("no compose file"), session: 1})
	model := result.(Model)
	if model.configErr == nil {
		t.Fatal("configErr should be set on error")
	}
	if !strings.Contains(model.configErr.Error(), "no compose file") {
		t.Errorf("configErr = %q, want 'no compose file'", model.configErr.Error())
	}
}

func TestConfigScreen_ValidateMsg(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{services: []string{"web"}},
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configSession = 1

	// Success
	result, _ := m.Update(configValidateMsg{err: nil, session: 1})
	model := result.(Model)
	if model.configValid == nil || !*model.configValid {
		t.Error("configValid should be true on successful validation")
	}

	// Failure
	result, _ = model.Update(configValidateMsg{err: fmt.Errorf("bad yaml"), session: 1})
	model = result.(Model)
	if model.configValid == nil || *model.configValid {
		t.Error("configValid should be false on failed validation")
	}
	if model.configValidMsg != "bad yaml" {
		t.Errorf("configValidMsg = %q, want 'bad yaml'", model.configValidMsg)
	}
}

func TestConfigScreen_EditDoneTriggersFetchAndValidate(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{services: []string{"web"}},
		configFile:   []byte("new content"),
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configSession = 1
	m.configResolved = []byte("old resolved")
	m.configShowRes = true

	result, cmd := m.Update(configEditDoneMsg{session: 1})
	model := result.(Model)

	if model.configResolved != nil {
		t.Error("configResolved should be cleared to invalidate cache after edit")
	}
	if model.configShowRes {
		t.Error("configShowRes should be reset to false after edit")
	}
	if cmd == nil {
		t.Error("expected batch cmd for re-fetch and validate")
	}
}

func TestConfigScreen_EditDoneError(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{services: []string{"web"}},
		configFile:   []byte("content"),
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configSession = 1

	editErr := fmt.Errorf("editor exited with status 1")
	result, cmd := m.Update(configEditDoneMsg{err: editErr, session: 1})
	model := result.(Model)

	if model.configErr == nil || model.configErr.Error() != editErr.Error() {
		t.Errorf("configErr = %v, want %v", model.configErr, editErr)
	}
	if cmd != nil {
		t.Error("expected no cmd when edit returns error")
	}
}

func TestFetchConfigHelpers_RequireConfigProvider(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	if cmd := m.fetchConfigFile(); cmd != nil {
		t.Fatal("fetchConfigFile should return nil when composer is not a ConfigProvider")
	}
	if cmd := m.fetchConfigResolved(); cmd != nil {
		t.Fatal("fetchConfigResolved should return nil when composer is not a ConfigProvider")
	}
	if cmd := m.fetchConfigValidate(); cmd != nil {
		t.Fatal("fetchConfigValidate should return nil when composer is not a ConfigProvider")
	}
}

func TestFetchConfigHelpers_ReturnMessages(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer:   mockComposer{services: []string{"web"}},
		configFile:     []byte("raw"),
		configResolved: []byte("resolved"),
		validateErr:    fmt.Errorf("bad yaml"),
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.configSession = 23

	fileMsg := m.fetchConfigFile()()
	gotFile, ok := fileMsg.(configFileMsg)
	if !ok {
		t.Fatalf("file msg type = %T, want configFileMsg", fileMsg)
	}
	if string(gotFile.data) != "raw" || gotFile.err != nil || gotFile.session != 23 {
		t.Fatalf("configFileMsg = %+v, want data raw, nil err, session 23", gotFile)
	}

	resolvedMsg := m.fetchConfigResolved()()
	gotResolved, ok := resolvedMsg.(configResolvedMsg)
	if !ok {
		t.Fatalf("resolved msg type = %T, want configResolvedMsg", resolvedMsg)
	}
	if string(gotResolved.data) != "resolved" || gotResolved.err != nil || gotResolved.session != 23 {
		t.Fatalf("configResolvedMsg = %+v, want data resolved, nil err, session 23", gotResolved)
	}

	validateMsg := m.fetchConfigValidate()()
	gotValidate, ok := validateMsg.(configValidateMsg)
	if !ok {
		t.Fatalf("validate msg type = %T, want configValidateMsg", validateMsg)
	}
	if gotValidate.err == nil || gotValidate.err.Error() != "bad yaml" || gotValidate.session != 23 {
		t.Fatalf("configValidateMsg = %+v, want err bad yaml, session 23", gotValidate)
	}
}

func TestViewConfig_Breadcrumb(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{services: []string{"web"}},
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configContent = []byte("test")
	m.configViewport = viewport.New(76, 18)
	m.configViewport.SetContent("test")
	m.width = 80
	m.height = 24
	m.projName = "myapp"

	view := m.viewConfig()
	if !strings.Contains(view, "config") {
		t.Errorf("view should contain 'config', got: %q", view)
	}
	if !strings.Contains(view, "myapp") {
		t.Errorf("view should contain project name, got: %q", view)
	}
}

func TestViewConfig_Loading(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{services: []string{"web"}},
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.width = 80
	m.height = 24

	view := m.viewConfig()
	if !strings.Contains(view, "Loading") {
		t.Errorf("view should show 'Loading' when no content, got: %q", view)
	}
}

func TestViewConfig_Error(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{services: []string{"web"}},
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configErr = fmt.Errorf("file not found")
	m.width = 80
	m.height = 24

	view := m.viewConfig()
	if !strings.Contains(view, "file not found") {
		t.Errorf("view should show error, got: %q", view)
	}
}

func TestViewConfig_HelpBarReflectsToggle(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{services: []string{"web"}},
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configContent = []byte("test")
	m.configViewport = viewport.New(76, 18)
	m.configViewport.SetContent("test")
	m.width = 80
	m.height = 24

	// Raw mode: help should say "r resolved"
	m.configShowRes = false
	view := m.viewConfig()
	if !strings.Contains(view, "r resolved") {
		t.Errorf("help should say 'r resolved' when showing raw, got: %q", view)
	}

	// Resolved mode: help should say "r raw"
	m.configShowRes = true
	view = m.viewConfig()
	if !strings.Contains(view, "r raw") {
		t.Errorf("help should say 'r raw' when showing resolved, got: %q", view)
	}
}

func TestViewConfig_ValidationStatus(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{services: []string{"web"}},
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configContent = []byte("test")
	m.configViewport = viewport.New(76, 18)
	m.configViewport.SetContent("test")
	m.width = 80
	m.height = 24

	// No validation yet
	view := m.viewConfig()
	if strings.Contains(view, "Config valid") || strings.Contains(view, "Config error") {
		t.Error("should not show validation status when configValid is nil")
	}

	// Valid
	v := true
	m.configValid = &v
	view = m.viewConfig()
	if !strings.Contains(view, "Config valid") {
		t.Errorf("should show 'Config valid', got: %q", view)
	}

	// Invalid
	v2 := false
	m.configValid = &v2
	m.configValidMsg = "bad yaml on line 5"
	view = m.viewConfig()
	if !strings.Contains(view, "Config error") {
		t.Errorf("should show 'Config error', got: %q", view)
	}
	if !strings.Contains(view, "bad yaml on line 5") {
		t.Errorf("should show validation message, got: %q", view)
	}
}

func TestViewSelectContainers_ShowsConfigKey(t *testing.T) {
	mc := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.width = 120
	m.height = 24

	view := m.viewSelectContainers()
	if !strings.Contains(view, "c config") {
		t.Errorf("container screen help should mention 'c config', got: %q", view)
	}
}

func TestServerBadgeStyle_KnownColor(t *testing.T) {
	for _, name := range []string{"red", "green", "yellow", "blue", "magenta", "cyan", "white", "gray"} {
		s := serverBadgeStyle(name)
		rendered := s.Render("test")
		if rendered == "" {
			t.Errorf("serverBadgeStyle(%q) rendered empty", name)
		}
	}
}

func TestServerBadgeStyle_UnknownAndEmpty(t *testing.T) {
	gray := serverBadgeStyle("gray").Render("x")
	for _, input := range []string{"", "purple", "unknown"} {
		got := serverBadgeStyle(input).Render("x")
		if got != gray {
			t.Errorf("serverBadgeStyle(%q) = %q, want gray %q", input, got, gray)
		}
	}
}

func TestServerBadge_RemoteServer(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.serverName = "prod-web"
	m.serverHost = "user@10.0.1.50"
	m.serverColor = "red"

	badge := m.serverBadge()
	if !strings.Contains(badge, "prod-web") {
		t.Errorf("server badge should contain server name, got: %q", badge)
	}
}

func TestServerBadge_NoColorUsesPlainServerName(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.serverName = "staging"
	m.serverHost = "user@staging"
	m.serverColor = ""

	badge := m.serverBadge()
	if badge != "staging" {
		t.Errorf("server badge without color should fall back to plain server name, got: %q", badge)
	}
}

func TestBreadcrumb_ServerBadgeInline(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.serverName = "prod"
	m.serverColor = "red"
	m.projName = "myapp"

	bc := m.breadcrumb()
	if !strings.Contains(bc, "prod") {
		t.Errorf("breadcrumb should contain server badge, got: %q", bc)
	}
	if !strings.Contains(bc, "myapp") {
		t.Errorf("breadcrumb should contain project name, got: %q", bc)
	}
}

func TestBreadcrumb_ServerNameWithoutColor(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.serverName = "staging"
	m.serverColor = ""
	m.projName = "myapp"

	bc := m.breadcrumb()
	if bc != "cdeploy > staging > myapp" {
		t.Errorf("breadcrumb without server color should use plain server name, got: %q", bc)
	}
}

func TestBreadcrumb_NoBadgeForLocal(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.serverName = ""

	bc := m.breadcrumb()
	if bc != "cdeploy" {
		t.Errorf("breadcrumb without server should be 'cdeploy', got: %q", bc)
	}
}

func TestResolveServerColor_GroupedServer(t *testing.T) {
	mc := &mockComposer{}
	cfg := &config.Config{
		Groups:  []config.Group{{Name: "production", Color: "red"}},
		Servers: []config.Server{{Name: "web", Host: "user@host", Group: "production"}},
	}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg))

	got := m.resolveServerColor(cfg.Servers[0])
	if got != "red" {
		t.Errorf("resolveServerColor(grouped) = %q, want %q", got, "red")
	}
}

func TestResolveServerColor_UngroupedServer(t *testing.T) {
	mc := &mockComposer{}
	cfg := &config.Config{
		Servers: []config.Server{{Name: "dev", Host: "user@host", Color: "cyan"}},
	}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg))

	got := m.resolveServerColor(cfg.Servers[0])
	if got != "cyan" {
		t.Errorf("resolveServerColor(ungrouped) = %q, want %q", got, "cyan")
	}
}

func TestResolveServerColor_NilConfig(t *testing.T) {
	mc := &mockComposer{}
	srv := config.Server{Name: "web", Host: "user@host", Group: "production", Color: "blue"}
	m := NewModel(nil, io.Discard, mockFactory(mc), []config.Server{srv}, mockConnectCb(mc))

	got := m.resolveServerColor(srv)
	if got != "blue" {
		t.Errorf("resolveServerColor(nil config) = %q, want %q (fallback to server.Color)", got, "blue")
	}
}

func TestViewSelectContainers_WithBadge(t *testing.T) {
	mc := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.width = 120
	m.height = 24
	m.serverName = "prod"
	m.serverHost = "user@prod"
	m.serverColor = "red"

	view := m.viewSelectContainers()
	if !strings.Contains(view, "prod") {
		t.Errorf("container view with server should contain badge with server name, got: %q", view)
	}
}

func TestViewSelectContainers_WithoutServerColorUsesPlainBreadcrumb(t *testing.T) {
	mc := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.width = 120
	m.height = 24
	m.serverName = "staging"
	m.serverHost = "user@staging"
	m.serverColor = ""

	view := m.viewSelectContainers()
	if !strings.Contains(view, "cdeploy > staging > services") {
		t.Errorf("container view without server color should use plain breadcrumb, got: %q", view)
	}
}

func TestViewSelectContainers_WithoutBadge(t *testing.T) {
	mc := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.width = 120
	m.height = 24
	m.serverName = ""

	view := m.viewSelectContainers()
	// With no server, breadcrumb starts with "cdeploy > services"
	if !strings.Contains(view, "cdeploy > services") {
		t.Errorf("container view without server should show plain breadcrumb, got: %q", view)
	}
}

// --- Scroll offset tests ---

func TestSvcVisibleCount_HeightZero(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c", "d", "e"}
	m.height = 0

	got := m.svcVisibleCount()
	if got != 5 {
		t.Errorf("svcVisibleCount() with height=0 = %d, want 5 (all services)", got)
	}
}

func TestSvcVisibleCount_NormalHeight(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.width = 130 // wide enough for one-line help
	m.height = 10

	// header=3, footer=3 (one-line help on wide terminal) → 10-3-3 = 4
	got := m.svcVisibleCount()
	if got != 4 {
		t.Errorf("svcVisibleCount() = %d, want 4", got)
	}
}

func TestSvcVisibleCount_NarrowWidth(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.width = 40 // too narrow for one-line help
	m.height = 10

	// header=3, footer=4 (two-line help) → 10-3-4 = 3
	got := m.svcVisibleCount()
	if got != 3 {
		t.Errorf("svcVisibleCount() narrow = %d, want 3", got)
	}
}

func TestSvcVisibleCount_Confirming(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.width = 120
	m.height = 10
	m.confirming = true

	// header=3, footer=3 (confirming) → 10-3-3 = 4
	got := m.svcVisibleCount()
	if got != 4 {
		t.Errorf("svcVisibleCount() confirming = %d, want 4", got)
	}
}

func TestSvcVisibleCount_Warning(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.width = 130
	m.height = 10
	m.warning = "something wrong"

	// header=3, footer=3+1 (warning) → 10-3-4 = 3
	got := m.svcVisibleCount()
	if got != 3 {
		t.Errorf("svcVisibleCount() warning = %d, want 3", got)
	}
}

func TestSvcVisibleCount_AllFit(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b"}
	m.width = 120
	m.height = 20

	// Plenty of room; visible capped at len(services)=2
	got := m.svcVisibleCount()
	if got != 2 {
		t.Errorf("svcVisibleCount() all fit = %d, want 2", got)
	}
}

func TestSvcVisibleCount_MinOne(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c"}
	m.width = 120
	m.height = 5 // header=3, footer=3 → 5-6=-1 → clamped to 1

	got := m.svcVisibleCount()
	if got != 1 {
		t.Errorf("svcVisibleCount() tiny height = %d, want 1", got)
	}
}

func TestSvcVisibleCount_WithStatusColumns(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.width = 130
	m.height = 10
	m.svcStatus = map[string]runner.ServiceStatus{
		"a": {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
	}

	// header=4 (3 + column captions), footer=3 → 10-4-3 = 3
	got := m.svcVisibleCount()
	if got != 3 {
		t.Errorf("svcVisibleCount() with status columns = %d, want 3", got)
	}
}

func TestHasStatusColumns(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a"}

	// No status data
	if m.hasStatusColumns() {
		t.Error("hasStatusColumns() = true, want false with no status data")
	}

	// Status with only Running (no Created/Uptime)
	m.svcStatus = map[string]runner.ServiceStatus{
		"a": {Running: true},
	}
	if m.hasStatusColumns() {
		t.Error("hasStatusColumns() = true, want false with only Running set")
	}

	// Status with Created
	m.svcStatus = map[string]runner.ServiceStatus{
		"a": {Created: "2024-01-15 09:30"},
	}
	if !m.hasStatusColumns() {
		t.Error("hasStatusColumns() = false, want true with Created set")
	}

	// Status with Uptime
	m.svcStatus = map[string]runner.ServiceStatus{
		"a": {Uptime: "3h"},
	}
	if !m.hasStatusColumns() {
		t.Error("hasStatusColumns() = false, want true with Uptime set")
	}

	// Status for service NOT in m.services should be ignored
	m.services = []string{"b"}
	m.svcStatus = map[string]runner.ServiceStatus{
		"a": {Created: "2024-01-15 09:30", Uptime: "3h"},
	}
	if m.hasStatusColumns() {
		t.Error("hasStatusColumns() = true, want false when status key not in services")
	}

	// Status with only Ports (no Created/Uptime)
	m.services = []string{"a"}
	m.svcStatus = map[string]runner.ServiceStatus{
		"a": {Running: true, Ports: []runner.Port{{Host: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"}}},
	}
	if !m.hasStatusColumns() {
		t.Error("hasStatusColumns() = false, want true with Ports set")
	}
}

func TestFixSvcOffset_CursorBelowWindow(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c", "d", "e"}
	m.width = 130
	m.height = 9 // visible = 9-3-3 = 3
	m.svcCursor = 4
	m.svcOffset = 0

	m.fixSvcOffset()
	// cursor=4, visible=3 → offset should be 4-3+1=2
	if m.svcOffset != 2 {
		t.Errorf("fixSvcOffset cursor below: svcOffset = %d, want 2", m.svcOffset)
	}
}

func TestFixSvcOffset_CursorAboveWindow(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c", "d", "e"}
	m.width = 120
	m.height = 9 // visible = 9-3-3 = 3
	m.svcCursor = 1
	m.svcOffset = 3

	m.fixSvcOffset()
	// cursor=1 < offset=3 → offset should become 1
	if m.svcOffset != 1 {
		t.Errorf("fixSvcOffset cursor above: svcOffset = %d, want 1", m.svcOffset)
	}
}

func TestFixSvcOffset_AllItemsFit(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b"}
	m.width = 120
	m.height = 20 // visible = all
	m.svcCursor = 1
	m.svcOffset = 0

	m.fixSvcOffset()
	if m.svcOffset != 0 {
		t.Errorf("fixSvcOffset all fit: svcOffset = %d, want 0", m.svcOffset)
	}
}

func TestFixSvcOffset_HeightZeroNoOp(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c"}
	m.height = 0
	m.svcCursor = 2
	m.svcOffset = 0

	m.fixSvcOffset()
	// height=0 → visible=len(services)=3 → all fit → offset stays 0
	if m.svcOffset != 0 {
		t.Errorf("fixSvcOffset height=0: svcOffset = %d, want 0", m.svcOffset)
	}
}

func TestFixSvcOffset_ClampsMaxOffset(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c", "d", "e"}
	m.width = 130
	m.height = 9 // visible = 9-3-3 = 3
	m.svcCursor = 4
	m.svcOffset = 10 // way too high

	m.fixSvcOffset()
	// maxOffset = 5-3 = 2; cursor=4 wants offset=2
	if m.svcOffset != 2 {
		t.Errorf("fixSvcOffset clamped: svcOffset = %d, want 2", m.svcOffset)
	}
}

func TestScrollDown_PastVisibleWindow(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b", "c", "d", "e"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.width = 130
	m.height = 9 // visible = 9-3-3 = 3

	// Set initial size
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 130, Height: 9})
	m = updated.(Model)

	// Press down 4 times to reach index 4
	for i := 0; i < 4; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = updated.(Model)
	}

	if m.svcCursor != 4 {
		t.Errorf("cursor = %d, want 4", m.svcCursor)
	}
	// visible=3, cursor=4 → offset should be 2
	if m.svcOffset != 2 {
		t.Errorf("svcOffset after scrolling down = %d, want 2", m.svcOffset)
	}
}

func TestScrollUp_PastTopOfWindow(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b", "c", "d", "e"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.width = 130
	m.height = 9 // visible = 9-3-3 = 3
	m.svcCursor = 4
	m.svcOffset = 2

	// Press up 3 times to reach index 1
	for i := 0; i < 3; i++ {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
		m = updated.(Model)
	}

	if m.svcCursor != 1 {
		t.Errorf("cursor = %d, want 1", m.svcCursor)
	}
	if m.svcOffset != 1 {
		t.Errorf("svcOffset after scrolling up = %d, want 1", m.svcOffset)
	}
}

func TestConfirming_CallsFixSvcOffset(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b", "c", "d", "e", "f", "g", "h"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.width = 120
	m.height = 8 // visible normal = 4, confirming = 4

	// Navigate to last item and select it
	m.svcCursor = 7
	m.svcOffset = 4 // near bottom
	m.selected[7] = true

	// Press 'r' to enter confirming
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m = updated.(Model)

	if !m.confirming {
		t.Error("expected confirming=true after 'r'")
	}
	// Cursor should still be visible
	visible := m.svcVisibleCount()
	if m.svcCursor < m.svcOffset || m.svcCursor >= m.svcOffset+visible {
		t.Errorf("cursor %d not in visible window [%d, %d)", m.svcCursor, m.svcOffset, m.svcOffset+visible)
	}
}

func TestSelectAll_DoesNotChangeOffset(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b", "c", "d", "e"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.width = 130
	m.height = 9 // visible = 3
	m.svcCursor = 3
	m.svcOffset = 1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)

	if m.svcOffset != 1 {
		t.Errorf("svcOffset changed after 'a': got %d, want 1", m.svcOffset)
	}
}

func TestWindowResize_FixesOffset(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b", "c", "d", "e"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.width = 130
	m.height = 20 // all fit
	m.svcCursor = 4
	m.svcOffset = 0

	// Shrink terminal
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 130, Height: 9}) // visible=3
	m = updated.(Model)

	// cursor=4 should force offset adjustment
	if m.svcOffset < 2 {
		t.Errorf("svcOffset after resize = %d, want >= 2", m.svcOffset)
	}
}

// --- View scroll indicator tests ---

func TestViewSelectContainers_UpIndicator(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b", "c", "d", "e"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 130
	m.height = 9 // visible = 3
	m.svcCursor = 3
	m.svcOffset = 2

	view := m.viewSelectContainers()
	if !strings.Contains(view, "▲ 2 more") {
		t.Errorf("expected up indicator '▲ 2 more' in view, got:\n%s", view)
	}
}

func TestViewSelectContainers_DownIndicator(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b", "c", "d", "e"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 130
	m.height = 9 // visible = 3
	m.svcCursor = 0
	m.svcOffset = 0

	view := m.viewSelectContainers()
	if !strings.Contains(view, "▼ 2 more") {
		t.Errorf("expected down indicator '▼ 2 more' in view, got:\n%s", view)
	}
}

func TestViewSelectContainers_NoIndicatorsWhenAllFit(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 120
	m.height = 20 // plenty of room
	m.svcCursor = 0
	m.svcOffset = 0

	view := m.viewSelectContainers()
	if strings.Contains(view, "▲") {
		t.Errorf("unexpected up indicator when all items fit:\n%s", view)
	}
	if strings.Contains(view, "▼") {
		t.Errorf("unexpected down indicator when all items fit:\n%s", view)
	}
}

func TestViewSelectContainers_BothIndicators(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b", "c", "d", "e"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 130
	m.height = 9 // visible = 3
	m.svcCursor = 2
	m.svcOffset = 1

	view := m.viewSelectContainers()
	if !strings.Contains(view, "▲ 1 more") {
		t.Errorf("expected up indicator in view, got:\n%s", view)
	}
	if !strings.Contains(view, "▼ 1 more") {
		t.Errorf("expected down indicator in view, got:\n%s", view)
	}
}

func TestViewSelectContainers_HeightZeroRendersAll(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b", "c", "d", "e"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 0
	m.height = 0

	view := m.viewSelectContainers()
	for _, svc := range mc.services {
		if !strings.Contains(view, svc) {
			t.Errorf("expected service %q in view when height=0", svc)
		}
	}
	if strings.Contains(view, "▲") || strings.Contains(view, "▼") {
		t.Errorf("unexpected indicators when height=0")
	}
}

func TestViewSelectContainers_WindowedOnlyShowsVisibleServices(t *testing.T) {
	mc := &mockComposer{services: []string{"aaa", "bbb", "ccc", "ddd", "eee"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 130
	m.height = 9 // visible = 3
	m.svcCursor = 2
	m.svcOffset = 1 // showing bbb, ccc, ddd

	view := m.viewSelectContainers()
	if strings.Contains(view, "aaa") {
		t.Error("service 'aaa' should not be visible (above window)")
	}
	if !strings.Contains(view, "bbb") {
		t.Error("service 'bbb' should be visible")
	}
	if !strings.Contains(view, "ccc") {
		t.Error("service 'ccc' should be visible")
	}
	if !strings.Contains(view, "ddd") {
		t.Error("service 'ddd' should be visible")
	}
	if strings.Contains(view, "eee") {
		t.Error("service 'eee' should not be visible (below window)")
	}
}

func TestViewSelectContainers_CreatedAndUptime(t *testing.T) {
	mc := &mockComposer{services: []string{"web", "db", "cache"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"web":   {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"db":    {Running: true, Created: "2024-01-14 08:00", Uptime: "1d 3h"},
		"cache": {Running: false, Created: "2024-01-15 10:00", Uptime: ""},
	}
	m.width = 120
	m.height = 24

	view := m.viewSelectContainers()

	// Verify Created values are shown
	if !strings.Contains(view, "2024-01-15 09:30") {
		t.Errorf("expected Created '2024-01-15 09:30' in view, got:\n%s", view)
	}
	if !strings.Contains(view, "2024-01-14 08:00") {
		t.Errorf("expected Created '2024-01-14 08:00' in view, got:\n%s", view)
	}
	if !strings.Contains(view, "2024-01-15 10:00") {
		t.Errorf("expected Created '2024-01-15 10:00' in view, got:\n%s", view)
	}

	// Verify Uptime values are shown
	if !strings.Contains(view, "3h") {
		t.Errorf("expected Uptime '3h' in view, got:\n%s", view)
	}
	if !strings.Contains(view, "1d 3h") {
		t.Errorf("expected Uptime '1d 3h' in view, got:\n%s", view)
	}
}

func TestViewSelectContainers_CreatedAndUptimeAlignment(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "postgres-db"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"nginx":       {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"postgres-db": {Running: true, Created: "2024-01-14 08:00", Uptime: "1d 3h"},
	}
	m.width = 120
	m.height = 24

	view := m.viewSelectContainers()

	// Both service names should be padded to same width (postgres-db is longer)
	// Look for "nginx" followed by spaces to align with "postgres-db"
	lines := strings.Split(view, "\n")
	var svcLines []string
	for _, line := range lines {
		if strings.Contains(line, "nginx") || strings.Contains(line, "postgres-db") {
			svcLines = append(svcLines, line)
		}
	}
	if len(svcLines) != 2 {
		t.Fatalf("expected 2 service lines, got %d:\n%s", len(svcLines), view)
	}

	// The Created column should start at the same position in both lines
	idx0 := strings.Index(svcLines[0], "2024-01-15")
	idx1 := strings.Index(svcLines[1], "2024-01-14")
	if idx0 != idx1 {
		t.Errorf("Created columns not aligned: line0 at %d, line1 at %d\nLine0: %q\nLine1: %q",
			idx0, idx1, svcLines[0], svcLines[1])
	}
}

func TestViewSelectContainers_Ports(t *testing.T) {
	mc := &mockComposer{services: []string{"web", "db"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true, Ports: []runner.Port{
			{Host: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			{Host: "0.0.0.0", HostPort: 8443, ContainerPort: 443, Protocol: "tcp"},
		}},
		"db": {Running: true},
	}
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()

	// Verify Ports caption appears
	if !strings.Contains(view, "Ports") {
		t.Errorf("expected 'Ports' caption in view, got:\n%s", view)
	}

	// Verify formatted ports appear
	if !strings.Contains(view, "0.0.0.0:8080→80") {
		t.Errorf("expected formatted port '0.0.0.0:8080→80' in view, got:\n%s", view)
	}
	if !strings.Contains(view, "0.0.0.0:8443→443") {
		t.Errorf("expected formatted port '0.0.0.0:8443→443' in view, got:\n%s", view)
	}
}

func TestViewSelectContainers_PortsAlignment(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "api"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"nginx": {Running: true, Ports: []runner.Port{
			{Host: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"},
		}},
		"api": {Running: true},
	}
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()
	lines := strings.Split(view, "\n")

	// Find caption row and both service rows
	var captionLine, nginxLine, apiLine string
	for _, line := range lines {
		if strings.Contains(line, "Ports") && !strings.Contains(line, "●") {
			captionLine = line
		}
		if strings.Contains(line, "● ") && strings.Contains(line, "nginx") {
			nginxLine = line
		}
		if strings.Contains(line, "● ") && strings.Contains(line, "api") && !strings.Contains(line, "nginx") {
			apiLine = line
		}
	}
	if captionLine == "" {
		t.Fatalf("expected captions row containing 'Ports', got:\n%s", view)
	}
	if nginxLine == "" || apiLine == "" {
		t.Fatalf("expected service rows, got:\n%s", view)
	}

	// nginx line must contain the formatted port
	if !strings.Contains(nginxLine, "0.0.0.0:80→80") {
		t.Errorf("expected formatted port in nginx line, got: %q", nginxLine)
	}

	// Strong alignment check: both rows must have the exact same visible (rune)
	// width. The empty-ports row pads the Ports column with spaces to match the
	// formatted-port row, so widths must be equal — mirrors the CLI's parallel
	// assertion in TestFormatDots_PortsColumn_Mixed.
	wNginx := utf8.RuneCountInString(nginxLine)
	wAPI := utf8.RuneCountInString(apiLine)
	if wNginx != wAPI {
		t.Errorf("ports column not aligned (rune width): nginx=%d, api=%d\nnginx: %q\napi:   %q",
			wNginx, wAPI, nginxLine, apiLine)
	}

	// Column-boundary check: locate the rune-index of "Ports" in the captions row,
	// then assert that both data rows have a rune at that column position (padding
	// or content) — i.e., both rows are at least as wide as the captions column starts.
	captionRuneIdx := utf8.RuneCountInString(captionLine[:strings.Index(captionLine, "Ports")])
	for _, line := range []string{nginxLine, apiLine} {
		if utf8.RuneCountInString(line) < captionRuneIdx {
			t.Errorf("data row shorter than 'Ports' caption start (%d runes): %q", captionRuneIdx, line)
		}
	}
}

func TestViewSelectContainers_NoPortsColumnWhenAllEmpty(t *testing.T) {
	mc := &mockComposer{services: []string{"web", "db"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"db":  {Running: false, Created: "2024-01-15 09:30"},
	}
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()
	if strings.Contains(view, "Ports") {
		t.Errorf("did not expect 'Ports' caption when no service has ports, got:\n%s", view)
	}
}

func TestViewSelectContainers_NoColumnsWhenNoStatus(t *testing.T) {
	mc := &mockComposer{services: []string{"web", "db"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true},
		"db":  {Running: false},
	}
	m.width = 120
	m.height = 24

	view := m.viewSelectContainers()

	// When no Created/Uptime data exists, no extra padding columns should appear
	// Service name should be at end of line (just trailing space from padding)
	lines := strings.Split(view, "\n")
	for _, line := range lines {
		if strings.Contains(line, "web") {
			// Should not have lots of trailing whitespace from empty columns
			trimmed := strings.TrimRight(line, " ")
			if strings.HasSuffix(trimmed, "web") {
				// Good: service name is at end
			} else if len(line)-len(trimmed) > 5 {
				t.Errorf("unexpected trailing whitespace suggesting empty columns: %q", line)
			}
		}
	}
}

func TestConfigScreen_ResolvedErrorResetsToggle(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
		configFile: []byte("raw content"),
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configSession = 1
	m.configContent = mc.configFile
	m.configViewport = viewport.New(76, 18)
	m.configViewport.SetContent(string(mc.configFile))

	// Simulate resolved fetch error
	result, _ := m.Update(configResolvedMsg{err: fmt.Errorf("config error"), session: 1})
	model := result.(Model)

	if model.configShowRes {
		t.Error("configShowRes should be false after resolved fetch error")
	}
	if model.configErr != nil {
		t.Error("configErr should be nil when raw content is available")
	}
	// Error should be surfaced via validation status
	if model.configValid == nil || *model.configValid {
		t.Error("configValid should be false after resolved fetch error")
	}
	if model.configValidMsg == "" {
		t.Error("configValidMsg should describe the error")
	}
	// Viewport should still show raw content
	if !strings.Contains(model.configViewport.View(), "raw content") {
		t.Error("viewport should show raw content after resolved fetch error")
	}

	// Press r should re-attempt the fetch (configResolved is still nil)
	result, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model = result.(Model)
	if !model.configShowRes {
		t.Error("configShowRes should be true after r press")
	}
	if cmd == nil {
		t.Error("expected a cmd to re-fetch resolved config")
	}

	// Simulate successful retry
	result, _ = model.Update(configResolvedMsg{data: []byte("resolved output"), session: 1})
	model = result.(Model)
	if model.configValid != nil {
		t.Error("configValid should be cleared after successful resolved fetch")
	}
	if model.configValidMsg != "" {
		t.Errorf("configValidMsg should be empty, got %q", model.configValidMsg)
	}
}

// --- Settings editor tests ---

func settingsModel(servers []config.Server) Model {
	mc := &mockComposer{}
	cfg := &config.Config{Servers: servers}
	return NewModel(nil, io.Discard, mockFactory(mc), servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath("/tmp/test-settings.yml"))
}

func TestSettingsList_SKeyOpensSettings(t *testing.T) {
	m := settingsModel(testServers)
	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want screenSelectServer", m.screen)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	if m.screen != screenSettingsList {
		t.Errorf("screen = %d, want screenSettingsList", m.screen)
	}
	if m.settingsCursor != 0 {
		t.Errorf("settingsCursor = %d, want 0", m.settingsCursor)
	}
}

func TestSettingsList_SKeyIgnoredWithoutConfig(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	// No WithConfig → config is nil

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	if m.screen != screenSelectServer {
		t.Errorf("screen = %d, want screenSelectServer (s should be ignored)", m.screen)
	}
}

func TestSettingsList_Navigation(t *testing.T) {
	m := settingsModel(testServers)
	// Go to settings
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	// Move down
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.settingsCursor != 1 {
		t.Errorf("settingsCursor = %d, want 1", m.settingsCursor)
	}

	// Can't go past last
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.settingsCursor != 1 {
		t.Errorf("settingsCursor = %d, want 1 (should stay at end)", m.settingsCursor)
	}

	// Move up
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(Model)
	if m.settingsCursor != 0 {
		t.Errorf("settingsCursor = %d, want 0", m.settingsCursor)
	}

	// Can't go before 0
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(Model)
	if m.settingsCursor != 0 {
		t.Errorf("settingsCursor = %d, want 0 (should stay at start)", m.settingsCursor)
	}
}

func TestSettingsList_EscBackToServerSelect(t *testing.T) {
	m := settingsModel(testServers)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectServer {
		t.Errorf("screen = %d, want screenSelectServer", m.screen)
	}
}

func TestSettingsForm_AKeyOpensBlankForm(t *testing.T) {
	m := settingsModel(testServers)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)

	if m.screen != screenSettingsForm {
		t.Errorf("screen = %d, want screenSettingsForm", m.screen)
	}
	if m.settingsEditing != -1 {
		t.Errorf("settingsEditing = %d, want -1 (add mode)", m.settingsEditing)
	}
	if m.settingsInputs[0].Value() != "" {
		t.Errorf("name input should be empty, got %q", m.settingsInputs[0].Value())
	}
}

func TestSettingsForm_EnterOpensPrefilledForm(t *testing.T) {
	m := settingsModel(testServers)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsForm {
		t.Errorf("screen = %d, want screenSettingsForm", m.screen)
	}
	if m.settingsEditing != 0 {
		t.Errorf("settingsEditing = %d, want 0 (edit first server)", m.settingsEditing)
	}
	if m.settingsInputs[0].Value() != "prod" {
		t.Errorf("name = %q, want %q", m.settingsInputs[0].Value(), "prod")
	}
	if m.settingsInputs[1].Value() != "user@prod.example.com" {
		t.Errorf("host = %q, want %q", m.settingsInputs[1].Value(), "user@prod.example.com")
	}
}

func TestSettingsForm_TabCyclesFields(t *testing.T) {
	m := settingsModel(testServers)
	// s → a → form
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)

	if m.settingsField != 0 {
		t.Fatalf("initial field = %d, want 0", m.settingsField)
	}

	// Tab through all fields
	for i := 1; i <= 4; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(Model)
		if m.settingsField != i%5 {
			t.Errorf("after tab %d: field = %d, want %d", i, m.settingsField, i%5)
		}
	}

	// Tab wraps around
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.settingsField != 0 {
		t.Errorf("after wrap tab: field = %d, want 0", m.settingsField)
	}
}

func TestSettingsForm_ShiftTabCyclesBackward(t *testing.T) {
	m := settingsModel(testServers)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)

	// Shift+tab from 0 → 4
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = updated.(Model)
	if m.settingsField != 4 {
		t.Errorf("field = %d, want 4", m.settingsField)
	}
}

func TestSettingsForm_ColorCycling(t *testing.T) {
	m := settingsModel(testServers)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)

	// Navigate to color field (field 4)
	for i := 0; i < 4; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(Model)
	}
	if m.settingsField != 4 {
		t.Fatalf("field = %d, want 4", m.settingsField)
	}

	// Initial color is ""
	if m.settingsColor != "" {
		t.Errorf("initial color = %q, want empty", m.settingsColor)
	}

	// Right → first color
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(Model)
	if m.settingsColor != "red" {
		t.Errorf("color = %q, want %q", m.settingsColor, "red")
	}

	// Left back to ""
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(Model)
	if m.settingsColor != "" {
		t.Errorf("color = %q, want empty", m.settingsColor)
	}

	// Left wraps to last color
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(Model)
	if m.settingsColor != "gray" {
		t.Errorf("color = %q, want %q", m.settingsColor, "gray")
	}
}

func TestSettingsForm_EscDiscardsBack(t *testing.T) {
	m := settingsModel(testServers)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSettingsList {
		t.Errorf("screen = %d, want screenSettingsList", m.screen)
	}
}

func TestSettingsForm_AddServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{Servers: []config.Server{
		{Name: "existing", Host: "user@existing"},
	}}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// s → settings list
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	// a → add form
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)

	// Type name
	for _, r := range "newserver" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	// Tab to host
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	// Type host
	for _, r := range "user@newhost" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	// Enter to save
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsList {
		t.Errorf("screen = %d, want screenSettingsList after save", m.screen)
	}
	if len(m.config.Servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(m.config.Servers))
	}
	if m.config.Servers[1].Name != "newserver" {
		t.Errorf("new server name = %q, want %q", m.config.Servers[1].Name, "newserver")
	}
	if m.config.Servers[1].Host != "user@newhost" {
		t.Errorf("new server host = %q, want %q", m.config.Servers[1].Host, "user@newhost")
	}
	// Verify servers synced
	if len(m.servers) != 2 {
		t.Errorf("m.servers has %d entries, want 2", len(m.servers))
	}
	// Verify cursor moved to new entry
	if m.settingsCursor != 1 {
		t.Errorf("settingsCursor = %d, want 1 (new server)", m.settingsCursor)
	}
}

func TestSettingsForm_EditServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{Servers: []config.Server{
		{Name: "prod", Host: "user@prod"},
	}}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// s → enter (edit)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.settingsInputs[0].Value() != "prod" {
		t.Fatalf("name = %q, want %q", m.settingsInputs[0].Value(), "prod")
	}

	// Tab to host, clear and type new host
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	// Select all + delete existing host text
	m.settingsInputs[1].SetValue("")
	for _, r := range "user@newprod" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}

	// Enter to save
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsList {
		t.Errorf("screen = %d, want screenSettingsList", m.screen)
	}
	if m.config.Servers[0].Host != "user@newprod" {
		t.Errorf("host = %q, want %q", m.config.Servers[0].Host, "user@newprod")
	}
}

func TestSettingsList_DeleteServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{Servers: []config.Server{
		{Name: "prod", Host: "user@prod"},
		{Name: "staging", Host: "user@staging"},
	}}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// s → d → y (delete first server)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)

	if !m.settingsDelete {
		t.Fatal("settingsDelete should be true")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = updated.(Model)

	if m.settingsDelete {
		t.Error("settingsDelete should be false after confirm")
	}
	if len(m.config.Servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(m.config.Servers))
	}
	if m.config.Servers[0].Name != "staging" {
		t.Errorf("remaining server = %q, want %q", m.config.Servers[0].Name, "staging")
	}
	if len(m.servers) != 1 {
		t.Errorf("m.servers = %d, want 1", len(m.servers))
	}
}

func TestSettingsList_DeleteCancel(t *testing.T) {
	m := settingsModel(testServers)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = updated.(Model)

	if m.settingsDelete {
		t.Error("settingsDelete should be false after cancel")
	}
	if len(m.config.Servers) != 2 {
		t.Errorf("servers should be unchanged, got %d", len(m.config.Servers))
	}
}

func TestSettingsForm_ValidationError_EmptyName(t *testing.T) {
	m := settingsModel(testServers)
	// s → a → form
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)

	// Skip name, tab to host, type host
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	for _, r := range "user@host" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}

	// Try to save — should fail validation
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsForm {
		t.Errorf("screen = %d, want screenSettingsForm (should stay on form)", m.screen)
	}
	if m.settingsErr == "" {
		t.Error("settingsErr should be set for empty name")
	}
	if !strings.Contains(m.settingsErr, "name is required") {
		t.Errorf("settingsErr = %q, want it to contain 'name is required'", m.settingsErr)
	}
}

func TestSettingsForm_ValidationError_DuplicateName(t *testing.T) {
	m := settingsModel(testServers)
	// s → a → form
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)

	// Type duplicate name "prod"
	for _, r := range "prod" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	// Tab to host, type host
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	for _, r := range "user@newhost" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}

	// Try to save
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsForm {
		t.Errorf("screen = %d, want screenSettingsForm", m.screen)
	}
	if !strings.Contains(m.settingsErr, "duplicate") {
		t.Errorf("settingsErr = %q, want it to contain 'duplicate'", m.settingsErr)
	}
}

func TestSettingsList_EmptyState(t *testing.T) {
	mc := &mockComposer{}
	cfg := &config.Config{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil,
		WithConfig(cfg), WithConfigPath("/tmp/test.yml"))

	// With config set and no servers, starts on server screen — navigate to settings
	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want screenSelectServer", m.screen)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	view := m.View()
	if !strings.Contains(view, "No servers configured") {
		t.Errorf("empty list view should show empty state message")
	}
}

func TestSettingsList_DeleteOnEmptyList(t *testing.T) {
	mc := &mockComposer{}
	cfg := &config.Config{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil,
		WithConfig(cfg), WithConfigPath("/tmp/test.yml"))
	// Navigate to settings
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	// d should do nothing on empty list
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	if m.settingsDelete {
		t.Error("settingsDelete should not activate on empty list")
	}
}

func TestSettingsList_EnterOnEmptyList(t *testing.T) {
	mc := &mockComposer{}
	cfg := &config.Config{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil,
		WithConfig(cfg), WithConfigPath("/tmp/test.yml"))
	// Navigate to settings
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	// enter should do nothing on empty list
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.screen != screenSettingsList {
		t.Errorf("screen = %d, want screenSettingsList (enter on empty should be noop)", m.screen)
	}
}

func TestViewSettingsForm_ShowsTitle(t *testing.T) {
	m := settingsModel(testServers)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)

	view := m.View()
	if !strings.Contains(view, "Add Server") {
		t.Errorf("add form should show 'Add Server' title")
	}
}

func TestViewSettingsForm_EditShowsEditTitle(t *testing.T) {
	m := settingsModel(testServers)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	view := m.View()
	if !strings.Contains(view, "Edit Server") {
		t.Errorf("edit form should show 'Edit Server' title")
	}
}

func TestSettingsForm_SaveError(t *testing.T) {
	mc := &mockComposer{}
	cfg := &config.Config{Servers: []config.Server{
		{Name: "prod", Host: "user@prod"},
	}}
	// Use invalid path to trigger save error
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath("/nonexistent/deeply/nested/readonly/servers.yml"))

	// s → a → fill → enter
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)
	for _, r := range "new" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	for _, r := range "user@host" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsForm {
		t.Errorf("screen = %d, want screenSettingsForm (should stay on form after save error)", m.screen)
	}
	if !strings.Contains(m.settingsErr, "save failed") {
		t.Errorf("settingsErr = %q, want it to contain 'save failed'", m.settingsErr)
	}
	// P2 fix: live state must NOT be mutated on save failure
	if len(m.config.Servers) != 1 {
		t.Errorf("config.Servers has %d entries after failed save, want 1 (should be unchanged)", len(m.config.Servers))
	}
	if m.config.Servers[0].Name != "prod" {
		t.Errorf("config.Servers[0].Name = %q, want %q (original should be preserved)", m.config.Servers[0].Name, "prod")
	}
}

func TestSettingsList_ServerEntryRebuildAfterAdd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{Servers: []config.Server{
		{Name: "prod", Host: "user@prod"},
	}}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// s → a → add server → save
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)
	for _, r := range "staging" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	for _, r := range "user@staging" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	// Back to server select — entries should include new server
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want screenSelectServer", m.screen)
	}
	// serverEntries should have: Local + 2 servers = 3
	if len(m.serverEntries) != 3 {
		t.Errorf("serverEntries = %d, want 3 (Local + 2 servers)", len(m.serverEntries))
	}
}

func TestViewSelectServer_ShowsSettingsHint(t *testing.T) {
	m := settingsModel(testServers)
	view := m.View()
	if !strings.Contains(view, "s settings") {
		t.Errorf("server select view should show 's settings' hint when config is set")
	}
}

func TestViewSelectServer_NoSettingsHintWithoutConfig(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	view := m.View()
	if strings.Contains(view, "s settings") {
		t.Errorf("server select view should not show 's settings' hint without config")
	}
}

func TestSettingsReachable_EmptyServerList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil,
		WithConfig(cfg), WithConfigPath(path))

	// With config set, should start on server select even with 0 servers
	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want screenSelectServer", m.screen)
	}

	// s opens settings
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	if m.screen != screenSettingsList {
		t.Fatalf("screen = %d, want screenSettingsList", m.screen)
	}

	// a → add form → fill → save
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)
	for _, r := range "myserver" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	for _, r := range "user@host" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsList {
		t.Fatalf("screen = %d, want screenSettingsList after save", m.screen)
	}
	if len(m.config.Servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(m.config.Servers))
	}

	// esc back to server select — should now show the new server
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want screenSelectServer", m.screen)
	}
	// Local + 1 server = 2 entries
	if len(m.serverEntries) != 2 {
		t.Errorf("serverEntries = %d, want 2", len(m.serverEntries))
	}
}

func TestSettingsEdit_AddGroup_ClampsServerCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{Servers: []config.Server{
		{Name: "prod", Host: "user@prod"},
		{Name: "staging", Host: "user@staging"},
	}}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// Move server picker cursor to staging (index 2: Local=0, prod=1, staging=2)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.serverCursor != 2 {
		t.Fatalf("serverCursor = %d, want 2", m.serverCursor)
	}

	// Go to settings, edit "prod" (index 0), add a group
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	// Tab to group field (field 3) and type a group name
	for i := 0; i < 3; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(Model)
	}
	for _, r := range "Production" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	// Save
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsList {
		t.Fatalf("screen = %d, want screenSettingsList", m.screen)
	}

	// Back to server select
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	// serverEntries now has a group header. serverCursor must be selectable.
	entry := m.serverEntries[m.serverCursor]
	if entry.kind == entryGroupHeader {
		t.Fatalf("serverCursor %d points to a group header — would panic on Enter", m.serverCursor)
	}

	// Pressing Enter should not panic
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = updated.(Model) // would panic without the fix
}

func TestCycleColor(t *testing.T) {
	// Forward from empty
	if c := cycleColor("", 1); c != "red" {
		t.Errorf("cycleColor(\"\", 1) = %q, want %q", c, "red")
	}
	// Backward from empty wraps to last
	if c := cycleColor("", -1); c != "gray" {
		t.Errorf("cycleColor(\"\", -1) = %q, want %q", c, "gray")
	}
	// Forward from last wraps to empty
	if c := cycleColor("gray", 1); c != "" {
		t.Errorf("cycleColor(\"gray\", 1) = %q, want empty", c)
	}
	// Forward from red → green
	if c := cycleColor("red", 1); c != "green" {
		t.Errorf("cycleColor(\"red\", 1) = %q, want %q", c, "green")
	}
}

func TestSettingsList_DeleteClampsServerCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{Servers: []config.Server{
		{Name: "prod", Host: "user@prod"},
	}}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// Move server picker cursor to the server entry (index 1, after Local)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.serverCursor != 1 {
		t.Fatalf("serverCursor = %d, want 1", m.serverCursor)
	}

	// Go to settings and delete the only server
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = updated.(Model)

	// serverEntries now has only Local (index 0)
	if len(m.serverEntries) != 1 {
		t.Fatalf("serverEntries = %d, want 1", len(m.serverEntries))
	}
	// serverCursor must be clamped
	if m.serverCursor != 0 {
		t.Errorf("serverCursor = %d, want 0 (should be clamped after delete)", m.serverCursor)
	}
}

func TestSettingsList_DeleteLastServer_FixesCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{Servers: []config.Server{
		{Name: "prod", Host: "user@prod"},
		{Name: "staging", Host: "user@staging"},
	}}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// s → navigate to last → d → y
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.settingsCursor != 1 {
		t.Fatalf("cursor should be at 1")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = updated.(Model)

	if m.settingsCursor != 0 {
		t.Errorf("cursor = %d, want 0 (should clamp after deleting last)", m.settingsCursor)
	}
}

func TestSettingsList_ShowsGroupColorForGroupedServer(t *testing.T) {
	mc := &mockComposer{}
	cfg := &config.Config{
		Groups:  []config.Group{{Name: "production", Color: "red"}},
		Servers: []config.Server{{Name: "web", Host: "user@host", Group: "production"}},
	}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg))

	// Open settings list
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	view := m.viewSettingsList()
	if !strings.Contains(view, "red") {
		t.Errorf("settings list should show group color 'red', got: %q", view)
	}
	if !strings.Contains(view, "(group)") {
		t.Errorf("settings list should show '(group)' indicator for grouped servers, got: %q", view)
	}
}

func TestSettingsList_ShowsServerColorForUngroupedServer(t *testing.T) {
	mc := &mockComposer{}
	cfg := &config.Config{
		Servers: []config.Server{{Name: "dev", Host: "user@host", Color: "cyan"}},
	}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg))

	// Open settings list
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)

	view := m.viewSettingsList()
	if !strings.Contains(view, "cyan") {
		t.Errorf("settings list should show server color 'cyan', got: %q", view)
	}
	if strings.Contains(view, "(group)") {
		t.Errorf("settings list should NOT show '(group)' for ungrouped servers, got: %q", view)
	}
}

func TestSettingsForm_GroupedServer_ColorGoesToGroup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{
		Groups:  []config.Group{{Name: "production", Color: "red"}},
		Servers: []config.Server{{Name: "web", Host: "user@host", Group: "production"}},
	}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// Open settings, edit server
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	// settingsColor should be loaded from group
	if m.settingsColor != "red" {
		t.Fatalf("settingsColor = %q, want %q (from group)", m.settingsColor, "red")
	}

	// Save without changes
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsList {
		t.Fatalf("screen = %d, want screenSettingsList", m.screen)
	}
	// Color should be on group, not server
	if m.config.Servers[0].Color != "" {
		t.Errorf("server color = %q, want empty (should be on group)", m.config.Servers[0].Color)
	}
	if m.config.Groups[0].Color != "red" {
		t.Errorf("group color = %q, want %q", m.config.Groups[0].Color, "red")
	}
}

func TestSettingsForm_AutoCreateGroup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{
		Servers: []config.Server{{Name: "web", Host: "user@host"}},
	}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// Open settings, edit server
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	// Tab to group field (field 3) and type group name
	for i := 0; i < 3; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(Model)
	}
	for _, r := range "staging" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}

	// Save
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsList {
		t.Fatalf("screen = %d, want screenSettingsList", m.screen)
	}
	if len(m.config.Groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(m.config.Groups))
	}
	if m.config.Groups[0].Name != "staging" {
		t.Errorf("group name = %q, want %q", m.config.Groups[0].Name, "staging")
	}
}

func TestSettingsForm_OrphanedGroupCleanup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{
		Groups:  []config.Group{{Name: "production", Color: "red"}},
		Servers: []config.Server{{Name: "web", Host: "user@host", Group: "production"}},
	}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// Open settings, edit server, clear the group
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	// Tab to group field (field 3) and clear it
	for i := 0; i < 3; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(Model)
	}
	// Select all and delete group text
	for i := 0; i < len("production"); i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		m = updated.(Model)
	}

	// Save
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenSettingsList {
		t.Fatalf("screen = %d, want screenSettingsList", m.screen)
	}
	// Group should be cleaned up since no server references it
	if len(m.config.Groups) != 0 {
		t.Errorf("got %d groups, want 0 (orphaned group should be cleaned)", len(m.config.Groups))
	}
}

func TestSettingsForm_OrphanedGroupCleanup_Delete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	mc := &mockComposer{}
	cfg := &config.Config{
		Groups:  []config.Group{{Name: "production", Color: "red"}},
		Servers: []config.Server{{Name: "web", Host: "user@host", Group: "production"}},
	}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg), WithConfigPath(path))

	// Open settings, delete server
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = updated.(Model)

	// Group should be cleaned up
	if len(m.config.Groups) != 0 {
		t.Errorf("got %d groups, want 0 (orphaned group should be cleaned after delete)", len(m.config.Groups))
	}
}

func TestSettingsForm_ColorAccessibleWhenGrouped(t *testing.T) {
	mc := &mockComposer{}
	cfg := &config.Config{
		Groups:  []config.Group{{Name: "prod", Color: "red"}},
		Servers: []config.Server{{Name: "web", Host: "user@host", Group: "prod"}},
	}
	m := NewModel(nil, io.Discard, mockFactory(mc), cfg.Servers, mockConnectCb(mc),
		WithConfig(cfg))

	// Open settings, edit server
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	// Tab through fields 0→1→2→3→4 (color picker must be reachable)
	for i := 0; i < 4; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(Model)
	}
	if m.settingsField != 4 {
		t.Errorf("settingsField after 4 tabs = %d, want 4 (color picker should be accessible)", m.settingsField)
	}

	// View should show "(group)" label
	view := m.viewSettingsForm()
	if !strings.Contains(view, "(group)") {
		t.Errorf("form should show '(group)' label for grouped server color, got: %q", view)
	}
}

// --- Quit confirmation tests ---

func TestQuitConfirmation_RemoteConnection_ShowsPrompt(t *testing.T) {
	// When connected to a remote server (disconnectFunc != nil),
	// pressing q should set quitting = true and NOT return tea.Quit.
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected = make(map[int]bool)
	m.disconnectFunc = func() error { return nil }
	m.serverName = "prod"

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if cmd != nil {
		t.Fatal("expected nil command (no quit), got non-nil")
	}
	if !um.quitting {
		t.Error("quitting should be true after pressing q on remote connection")
	}
	if um.screen != screenSelectContainers {
		t.Errorf("screen should remain unchanged, got %d", um.screen)
	}
}

func TestQuitConfirmation_LocalSession_QuitsImmediately(t *testing.T) {
	// Without a remote connection (disconnectFunc == nil),
	// pressing q should return tea.Quit directly.
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected = make(map[int]bool)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
	if um.quitting {
		t.Error("quitting should be false for local session")
	}
}

func TestQuitConfirmation_NoCancels(t *testing.T) {
	// When quitting is true, pressing n should cancel (set quitting = false).
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected = make(map[int]bool)
	m.quitting = true
	m.disconnectFunc = func() error { return nil }

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	um := updated.(Model)

	if cmd != nil {
		t.Fatal("expected nil command after pressing n, got non-nil")
	}
	if um.quitting {
		t.Error("quitting should be false after pressing n")
	}
	if um.screen != screenSelectContainers {
		t.Errorf("screen should remain unchanged after cancel, got %d", um.screen)
	}
}

func TestQuitConfirmation_EscCancels(t *testing.T) {
	// When quitting is true, pressing esc should cancel (set quitting = false).
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected = make(map[int]bool)
	m.quitting = true
	m.disconnectFunc = func() error { return nil }

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	um := updated.(Model)

	if cmd != nil {
		t.Fatal("expected nil command after pressing esc, got non-nil")
	}
	if um.quitting {
		t.Error("quitting should be false after pressing esc")
	}
}

func TestQuitConfirmation_OtherKeysSwallowed(t *testing.T) {
	// When quitting is true, other keys should be swallowed (no effect).
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected = make(map[int]bool)
	m.quitting = true
	m.disconnectFunc = func() error { return nil }

	for _, key := range []rune{'j', 'k', 'd', 'r', 'a', 'x'} {
		updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		um := updated.(Model)

		if cmd != nil {
			t.Errorf("key %c: expected nil command, got non-nil", key)
		}
		if !um.quitting {
			t.Errorf("key %c: quitting should remain true", key)
		}
	}

	// ctrl+c should also be swallowed when quitting prompt is active
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	um := updated.(Model)
	if cmd != nil {
		t.Error("ctrl+c: expected nil command, got non-nil")
	}
	if !um.quitting {
		t.Error("ctrl+c: quitting should remain true")
	}
}

func TestQuitConfirmation_ServerSelectAlwaysQuitsDirectly(t *testing.T) {
	// On the server select screen, q should always quit directly,
	// even if disconnectFunc is set (should not happen in practice).
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectServer

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	if cmd == nil {
		t.Fatal("expected quit command on server select, got nil")
	}
}

func TestQuitConfirmation_AllRemoteScreens(t *testing.T) {
	// Verify that tryQuit is used across all non-server-select screens
	// by testing q on each screen with a remote connection.
	tests := []struct {
		name   string
		screen screen
		key    string
		setup  func(m *Model)
	}{
		{"project select", screenSelectProject, "q", func(m *Model) {
			m.projects = []compose.Project{{Name: "app", ConfigDir: "/app"}}
		}},
		{"containers normal", screenSelectContainers, "q", func(m *Model) {
			m.services = []string{"nginx"}
			m.selected = make(map[int]bool)
		}},
		{"containers confirming", screenSelectContainers, "q", func(m *Model) {
			m.services = []string{"nginx"}
			m.selected = map[int]bool{0: true}
			m.confirming = true
			m.pendingOp = runner.Deploy
		}},
		{"logs", screenLogs, "q", func(m *Model) {
			m.logsService = "nginx"
		}},
		{"config", screenConfig, "q", func(m *Model) {
			m.configContent = []byte("version: '3'")
		}},
		{"settings list", screenSettingsList, "q", func(m *Model) {
			m.config = &config.Config{Servers: testServers}
		}},
		{"settings form ctrl+c", screenSettingsForm, "ctrl+c", func(m *Model) {
			m.config = &config.Config{Servers: testServers}
			m.settingsInputs = initSettingsInputs()
		}},
		{"progress done", screenProgress, "q", func(m *Model) {
			m.done = true
		}},
		{"progress failed", screenProgress, "q", func(m *Model) {
			m.failed = true
		}},
		{"containers ctrl+c", screenSelectContainers, "ctrl+c", func(m *Model) {
			m.services = []string{"nginx"}
			m.selected = make(map[int]bool)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc := &mockComposer{}
			m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
			m.screen = tt.screen
			m.disconnectFunc = func() error { return nil }
			m.serverName = "prod"
			if tt.setup != nil {
				tt.setup(&m)
			}

			var keyMsg tea.KeyMsg
			if tt.key == "ctrl+c" {
				keyMsg = tea.KeyMsg{Type: tea.KeyCtrlC}
			} else {
				keyMsg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tt.key)}
			}

			updated, cmd := m.Update(keyMsg)
			um := updated.(Model)

			if cmd != nil {
				t.Errorf("screen %q: expected nil command (quit intercepted), got non-nil", tt.name)
			}
			if !um.quitting {
				t.Errorf("screen %q: quitting should be true after pressing %s on remote connection", tt.name, tt.key)
			}
		})
	}
}

func TestQuitConfirmation_ViewRendersDisconnectPrompt(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectContainers
	m.services = []string{"nginx"}
	m.selected = make(map[int]bool)
	m.disconnectFunc = func() error { return nil }
	m.serverName = "prod-server"
	m.quitting = true

	output := m.View()

	if !strings.Contains(output, "Disconnect from prod-server? (y/n)") {
		t.Errorf("expected View to contain disconnect prompt, got:\n%s", output)
	}
	if !strings.Contains(output, "cdeploy") {
		t.Errorf("expected View to contain title 'cdeploy', got:\n%s", output)
	}
}

func TestQuitConfirmation_ConnectErrorResetsQuitting(t *testing.T) {
	// When a remote connection attempt fails, quitting should be reset to false.
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectServer
	m.quitting = true
	m.disconnectFunc = func() error { return nil }

	updated, _ := m.Update(connectResultMsg{err: errors.New("connection refused")})
	um := updated.(Model)

	if um.quitting {
		t.Error("quitting should be reset to false after connectResultMsg error")
	}
	if um.disconnectFunc != nil {
		t.Error("disconnectFunc should be nil after connectResultMsg error")
	}
	if um.serverErr == nil {
		t.Error("serverErr should be set after connectResultMsg error")
	}
}

func TestQuitConfirmation_YesReturnsQuitMsg(t *testing.T) {
	// Verify that pressing y during quit confirmation returns a command
	// that produces tea.QuitMsg.
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected = make(map[int]bool)
	m.quitting = true
	m.disconnectFunc = func() error { return nil }

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if cmd == nil {
		t.Fatal("expected quit command after pressing y, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestQuitConfirmation_LocalQuitReturnsQuitMsg(t *testing.T) {
	// Verify that quitting a local session returns a command
	// that produces tea.QuitMsg.
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected = make(map[int]bool)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestQuitConfirmation_ServerSelectReturnsQuitMsg(t *testing.T) {
	// Verify that quitting from server select returns a command
	// that produces tea.QuitMsg.
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectServer

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	if cmd == nil {
		t.Fatal("expected quit command on server select, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestQuitConfirmation_ProgressInProgressIgnoresQ(t *testing.T) {
	// When an operation is in progress (not done, not failed), pressing q
	// should NOT trigger quit or set quitting = true.
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.done = false
	m.failed = false
	m.disconnectFunc = func() error { return nil }
	m.serverName = "prod"

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	updated := result.(Model)

	if updated.quitting {
		t.Error("pressing q during in-progress operation should not set quitting")
	}
	if cmd != nil {
		t.Error("pressing q during in-progress operation should not return a command")
	}
}

func TestColumnCaptions_ShownWithStatusData(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"web", "api"}
	m.selected = map[int]bool{}
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"api": {Running: true, Created: "2024-01-15 09:25", Uptime: "3h"},
	}

	view := m.viewSelectContainers()
	if !strings.Contains(view, "Created") {
		t.Errorf("expected 'Created' caption in view, got:\n%s", view)
	}
	if !strings.Contains(view, "Uptime") {
		t.Errorf("expected 'Uptime' caption in view, got:\n%s", view)
	}
}

func TestColumnCaptions_HiddenWithoutStatusData(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"web", "api"}
	m.selected = map[int]bool{}
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true},
		"api": {Running: false},
	}

	view := m.viewSelectContainers()
	if strings.Contains(view, "Created") {
		t.Errorf("unexpected 'Created' caption when no Created data exists, got:\n%s", view)
	}
	if strings.Contains(view, "Uptime") {
		t.Errorf("unexpected 'Uptime' caption when no Uptime data exists, got:\n%s", view)
	}
}

func TestColumnCaptions_Alignment(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"web", "api-service"}
	m.selected = map[int]bool{}
	m.svcStatus = map[string]runner.ServiceStatus{
		"web":         {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"api-service": {Running: true, Created: "2024-01-15 09:25", Uptime: "5d"},
	}

	view := m.viewSelectContainers()
	// Strip ANSI escape sequences for reliable offset comparison
	ansiRe := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	clean := ansiRe.ReplaceAllString(view, "")
	lines := strings.Split(clean, "\n")

	// Find the header line containing "Created" and a data line
	var headerLine, dataLine string
	for _, line := range lines {
		if strings.Contains(line, "Created") && strings.Contains(line, "Uptime") && !strings.Contains(line, "●") {
			headerLine = line
		}
		if strings.Contains(line, "api-service") && strings.Contains(line, "●") {
			dataLine = line
		}
	}

	if headerLine == "" {
		t.Fatalf("could not find header line in view:\n%s", clean)
	}
	if dataLine == "" {
		t.Fatalf("could not find data line in view:\n%s", clean)
	}

	// "Created" label and actual created value should start at the same rune offset
	// (byte offsets differ due to multi-byte ● character in data line)
	headerCreatedIdx := len([]rune(headerLine[:strings.Index(headerLine, "Created")]))
	dataCreatedIdx := len([]rune(dataLine[:strings.Index(dataLine, "2024-01-15 09:25")]))
	if headerCreatedIdx != dataCreatedIdx {
		t.Errorf("Created label rune offset (%d) != data rune offset (%d)\nheader: %q\ndata:   %q",
			headerCreatedIdx, dataCreatedIdx, headerLine, dataLine)
	}
}

// --- Exec screen tests ---

func TestExec_XKeyOnRunningServiceTriggersConfirm(t *testing.T) {
	mc := &mockExecComposer{
		mockComposer: mockComposer{
			services: []string{"web", "db"},
			status: map[string]runner.ServiceStatus{
				"web": {Running: true},
				"db":  {Running: false},
			},
		},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.svcCursor = 0 // "web" is running
	m.width = 120
	m.height = 24

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	model := result.(Model)

	if !model.confirming {
		t.Error("expected confirming=true after 'x' on running service")
	}
	if !model.pendingExec {
		t.Error("expected pendingExec=true after 'x' on running service")
	}
}

func TestExec_XKeyOnStoppedServiceShowsWarning(t *testing.T) {
	mc := &mockExecComposer{
		mockComposer: mockComposer{
			services: []string{"web", "db"},
			status: map[string]runner.ServiceStatus{
				"web": {Running: true},
				"db":  {Running: false},
			},
		},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.svcCursor = 1 // "db" is stopped
	m.width = 120
	m.height = 24

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	model := result.(Model)

	if model.confirming {
		t.Error("should not enter confirming state for stopped service")
	}
	if model.pendingExec {
		t.Error("should not set pendingExec for stopped service")
	}
	if model.warning != "Container is not running" {
		t.Errorf("warning = %q, want %q", model.warning, "Container is not running")
	}
}

func TestExec_XKeyWithoutExecProviderIsNoOp(t *testing.T) {
	// Plain mockComposer does NOT implement ExecProvider
	mc := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.svcCursor = 0
	m.width = 120
	m.height = 24

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	model := result.(Model)

	if model.confirming {
		t.Error("should not enter confirming state when composer doesn't implement ExecProvider")
	}
	if model.pendingExec {
		t.Error("should not set pendingExec when composer doesn't implement ExecProvider")
	}
	if model.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (should stay on containers)", model.screen, screenSelectContainers)
	}
}

func TestExec_XKeyOnNoServicesIsNoOp(t *testing.T) {
	mc := &mockExecComposer{
		mockComposer: mockComposer{
			services: []string{},
			status:   map[string]runner.ServiceStatus{},
		},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.width = 120
	m.height = 24

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	model := result.(Model)

	if model.confirming {
		t.Error("should not enter confirming state with no services")
	}
}

func TestExec_ConfirmEnterDispatchesExecProcess(t *testing.T) {
	mc := &mockExecComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.svcCursor = 0
	m.confirming = true
	m.pendingExec = true
	m.width = 120
	m.height = 24

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if cmd == nil {
		t.Error("expected a cmd (tea.ExecProcess) when confirming exec")
	}
}

func TestExec_ConfirmEscClearsPendingExec(t *testing.T) {
	mc := &mockExecComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.svcCursor = 0
	m.confirming = true
	m.pendingExec = true
	m.width = 120
	m.height = 24

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := result.(Model)

	if model.confirming {
		t.Error("confirming should be false after esc")
	}
	if model.pendingExec {
		t.Error("pendingExec should be false after esc")
	}
}

func TestExec_ExecDoneMsgRefreshesStatus(t *testing.T) {
	mc := &mockExecComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.pendingExec = true
	m.confirming = true

	result, cmd := m.Update(execDoneMsg{err: nil})
	model := result.(Model)

	if model.pendingExec {
		t.Error("pendingExec should be false after execDoneMsg")
	}
	if model.confirming {
		t.Error("confirming should be false after execDoneMsg")
	}
	if cmd == nil {
		t.Error("expected a cmd (refreshStatus) after execDoneMsg")
	}
}

func TestExec_ExecDoneMsgWithErrorStillResetsState(t *testing.T) {
	mc := &mockExecComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.pendingExec = true
	m.confirming = true

	result, cmd := m.Update(execDoneMsg{err: fmt.Errorf("exec failed: exit status 1")})
	model := result.(Model)

	if model.pendingExec {
		t.Error("pendingExec should be false after execDoneMsg with error")
	}
	if model.confirming {
		t.Error("confirming should be false after execDoneMsg with error")
	}
	if cmd == nil {
		t.Error("expected a cmd (refreshStatus) after execDoneMsg with error")
	}
	if model.warning == "" {
		t.Error("expected warning to be set after execDoneMsg with error")
	}
	if !strings.Contains(model.warning, "exit status 1") {
		t.Errorf("warning should contain error message, got: %s", model.warning)
	}
}

func TestExec_ExecDoneMsgStaleMessageGuard(t *testing.T) {
	mc := &mockExecComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenLogs // not on container screen
	m.pendingExec = true
	m.confirming = true

	result, cmd := m.Update(execDoneMsg{err: fmt.Errorf("some error")})
	model := result.(Model)

	// Stale message should be discarded — state unchanged
	if !model.pendingExec {
		t.Error("pendingExec should remain true when message is stale")
	}
	if !model.confirming {
		t.Error("confirming should remain true when message is stale")
	}
	if cmd != nil {
		t.Error("expected nil cmd when message is stale")
	}
	if model.warning != "" {
		t.Error("warning should not be set when message is stale")
	}
}

func TestExec_ViewShowsExecConfirmation(t *testing.T) {
	mc := &mockExecComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.svcCursor = 0
	m.confirming = true
	m.pendingExec = true
	m.width = 120
	m.height = 24

	view := m.viewSelectContainers()
	if !strings.Contains(view, "Exec into web") {
		t.Errorf("exec confirmation should mention 'Exec into web', got: %q", view)
	}
	if !strings.Contains(view, "enter confirm") {
		t.Errorf("exec confirmation should mention 'enter confirm', got: %q", view)
	}
}

func TestViewSelectContainers_ShowsExecKey(t *testing.T) {
	mc := &mockComposer{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.width = 120
	m.height = 24

	view := m.viewSelectContainers()
	if !strings.Contains(view, "x exec") {
		t.Errorf("container screen help should mention 'x exec', got: %q", view)
	}
}

func TestExec_XKeyOnServiceWithNoStatus(t *testing.T) {
	// Service exists but has no status entry — treated as not running
	mc := &mockExecComposer{
		mockComposer: mockComposer{
			services: []string{"web"},
			status:   map[string]runner.ServiceStatus{},
		},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	m.services = mc.services
	m.svcStatus = mc.status
	m.screen = screenSelectContainers
	m.svcCursor = 0
	m.width = 120
	m.height = 24

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	model := result.(Model)

	if model.confirming {
		t.Error("should not enter confirming state for service with no status")
	}
	if model.warning != "Container is not running" {
		t.Errorf("warning = %q, want %q", model.warning, "Container is not running")
	}
}

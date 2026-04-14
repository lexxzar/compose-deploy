package tui

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"

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
}

func TestViewSelectProject_Empty(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.projects = []compose.Project{}

	v := m.View()
	if !strings.Contains(v, "No Docker Compose projects found") {
		t.Error("view should show empty state message")
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
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))

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
	if m.projectLoader != nil {
		t.Error("projectLoader should be nil for local")
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
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.serverName = "prod"
	// Simulate stale remote state set before connect attempt
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return nil, nil
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
	if m.projectLoader != nil {
		t.Error("projectLoader should be cleared after connect failure")
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
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	// Simulate state after connecting to remote server and being on project screen
	m.screen = screenSelectProject
	m.serverName = "prod"
	m.showPicker = true
	disconnectCalled := false
	m.disconnectFunc = func() error { disconnectCalled = true; return nil }
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return nil, nil
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
	if m.projectLoader != nil {
		t.Error("projectLoader should be nil after going back")
	}
	if !m.showPicker {
		// showPicker is reset to false since we're going back to server screen
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
	if bc != "cdeploy > prod > my-app" {
		t.Errorf("breadcrumb = %q, want %q", bc, "cdeploy > prod > my-app")
	}
}

func TestBreadcrumb_ServerOnly(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.serverName = "staging"

	bc := m.breadcrumb()
	if bc != "cdeploy > staging" {
		t.Errorf("breadcrumb = %q, want %q", bc, "cdeploy > staging")
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

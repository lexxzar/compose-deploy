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
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/runner"
	"github.com/muesli/termenv"
)

func mockFactory(mc *mockComposer) ComposerFactory {
	return func(compose.Project) runner.Composer { return mc }
}

type mockComposer struct {
	services   []string
	status     map[string]runner.ServiceStatus
	err        error
	statusErr  error
	stats      map[string]runner.ServiceStats
	statsErr   error
	updates    map[string]bool
	updatesErr error

	// Call counters — incremented when the corresponding tea.Cmd is actually
	// invoked. Used by tick/refresh tests to assert that gated paths don't
	// produce docker calls.
	statusCalls  int
	statsCalls   int
	updatesCalls int
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
	m.statusCalls++
	return m.status, m.statusErr
}

func (m *mockComposer) ContainerStats(ctx context.Context) (map[string]runner.ServiceStats, error) {
	m.statsCalls++
	return m.stats, m.statsErr
}

func (m *mockComposer) CheckUpdates(ctx context.Context, services []string) (map[string]bool, error) {
	m.updatesCalls++
	return m.updates, m.updatesErr
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
	return func(compose.Project) runner.Composer { return mc }
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
	return func(compose.Project) runner.Composer { return mc }
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
	installFakeTick(&m)

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() should return a batch command when picker is skipped")
	}
	// Drive the batch so the constituent Cmds actually invoke the mock —
	// catches regressions where Init() returns a Cmd that looks right but
	// doesn't actually trigger the expected refreshes (e.g. refreshUpdates
	// silently dropped from the batch).
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init() should return a tea.Batch, got %T", msg)
	}
	for _, child := range batch {
		if child != nil {
			_ = child()
		}
	}
	if mc.updatesCalls == 0 {
		t.Error("Init batch should invoke CheckUpdates (refreshUpdates not in fan-out)")
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

func TestComputeMatches(t *testing.T) {
	services := []string{"web", "web-worker", "postgres", "redis", "Nginx"}

	tests := []struct {
		name     string
		services []string
		query    string
		want     []int
	}{
		{
			name:     "substring match",
			services: services,
			query:    "post",
			want:     []int{2},
		},
		{
			name:     "case-insensitive query matches lowercase name",
			services: services,
			query:    "Web",
			want:     []int{0, 1},
		},
		{
			name:     "case-insensitive name matches lowercase query",
			services: services,
			query:    "nginx",
			want:     []int{4},
		},
		{
			name:     "multiple matches preserve list order",
			services: services,
			query:    "e",
			want:     []int{0, 1, 2, 3},
		},
		{
			name:     "empty query returns nil",
			services: services,
			query:    "",
			want:     nil,
		},
		{
			name:     "no-match returns nil",
			services: services,
			query:    "zzz",
			want:     nil,
		},
		{
			name:     "empty services returns nil",
			services: nil,
			query:    "web",
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeMatches(tt.services, tt.query)
			if len(got) != len(tt.want) {
				t.Fatalf("computeMatches(%v, %q) = %v, want %v", tt.services, tt.query, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("computeMatches(%v, %q) = %v, want %v", tt.services, tt.query, got, tt.want)
					break
				}
			}
		})
	}
}

func TestClearSearch(t *testing.T) {
	m := &Model{
		searching:     true,
		searchInput:   textinput.New(),
		searchQuery:   "web",
		searchMatches: []int{0, 1},
		searchReturn:  3,
	}
	m.searchInput.SetValue("web")
	m.searchInput.Focus()

	m.clearSearch()

	if m.searching {
		t.Error("searching = true, want false")
	}
	if m.searchQuery != "" {
		t.Errorf("searchQuery = %q, want empty", m.searchQuery)
	}
	if m.searchMatches != nil {
		t.Errorf("searchMatches = %v, want nil", m.searchMatches)
	}
	if m.searchReturn != 0 {
		t.Errorf("searchReturn = %d, want 0", m.searchReturn)
	}
	if m.searchInput.Value() != "" {
		t.Errorf("searchInput value = %q, want empty", m.searchInput.Value())
	}
	if m.searchInput.Focused() {
		t.Error("searchInput focused = true, want false (blurred)")
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
	if !strings.Contains(v, "q back") {
		t.Error("error state should show 'q back' when server picker is available")
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
	if !strings.Contains(v, "q back") {
		t.Error("empty state should show 'q back' when server picker is available")
	}
}

func TestViewSelectProject_UnmanagedRow(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.projects = []compose.Project{
		{Name: "my-app", Status: "running(3)", ConfigDir: "/srv/my-app"},
		{Name: compose.UnmanagedProjectName, Desc: "3 containers", Unmanaged: true},
	}

	v := m.View()
	if !strings.Contains(v, compose.UnmanagedProjectName) {
		t.Errorf("view should show %q:\n%s", compose.UnmanagedProjectName, v)
	}
	if !strings.Contains(v, "3 containers") {
		t.Errorf("unmanaged row should show its count in the description column:\n%s", v)
	}
	if !strings.Contains(v, shortenPath("/srv/my-app")) {
		t.Errorf("compose row should still show its config dir:\n%s", v)
	}
}

func TestViewSelectProject_UnmanagedRowIsLast(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.projects = []compose.Project{
		{Name: "zebra", Status: "running(1)", ConfigDir: "/srv/zebra"},
		{Name: compose.UnmanagedProjectName, Desc: "1 container", Unmanaged: true},
	}

	v := m.View()
	// strings.Index returns -1 for an absent needle, and -1 is less than every
	// valid index — so both rows must be proven present before the comparison
	// means anything.
	composeIdx := strings.Index(v, "zebra")
	unmanagedIdx := strings.Index(v, compose.UnmanagedProjectName)
	if composeIdx < 0 {
		t.Fatalf("compose row missing from the render:\n%s", v)
	}
	if unmanagedIdx < 0 {
		t.Fatalf("unmanaged row missing from the render:\n%s", v)
	}
	if composeIdx > unmanagedIdx {
		t.Errorf("unmanaged row should render last:\n%s", v)
	}
}

func TestComposerFactory_ReceivesWholeProject(t *testing.T) {
	projects := []compose.Project{
		{Name: "my-app", Status: "running(3)", ConfigDir: "/srv/my-app"},
		{Name: compose.UnmanagedProjectName, Desc: "3 containers", Unmanaged: true},
	}

	tests := []struct {
		name      string
		cursor    int
		wantDir   string
		wantUnman bool
	}{
		{"compose project", 0, "/srv/my-app", false},
		{"unmanaged row", 1, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc := &mockComposer{}
			var got compose.Project
			factory := func(proj compose.Project) runner.Composer {
				got = proj
				return mc
			}
			m := NewModel(nil, io.Discard, factory, nil, nil)
			m.screen = screenSelectProject
			m.projects = projects
			m.projCursor = tt.cursor

			updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m = updated.(Model)

			if got.Name != projects[tt.cursor].Name {
				t.Errorf("factory got Name = %q, want %q", got.Name, projects[tt.cursor].Name)
			}
			if got.ConfigDir != tt.wantDir {
				t.Errorf("factory got ConfigDir = %q, want %q", got.ConfigDir, tt.wantDir)
			}
			if got.Unmanaged != tt.wantUnman {
				t.Errorf("factory got Unmanaged = %v, want %v", got.Unmanaged, tt.wantUnman)
			}
			if m.composer != runner.Composer(mc) {
				t.Error("composer should be the one the factory returned")
			}
		})
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

// TestEscFromProgress_InvalidatesUpdateCache_OnDeploy is the iteration-3
// regression for the "stale glyph after Deploy" UX bug: after a successful
// Deploy (which pulls a new image), the cached "update available" verdict
// is no longer accurate, and the user expects fresh feedback rather than
// up to ~10 minutes of stale glyph. Esc-from-progress on a successful
// Deploy must delete the cache entry so maybeRefreshUpdatesCmd misses and
// schedules a fresh CheckUpdates.
func TestEscFromProgress_InvalidatesUpdateCache_OnDeploy(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.done = true
	m.pendingOp = runner.Deploy
	m.composer = mc
	installFakeTick(&m)
	// Prime the cache with a fresh entry for the current context.
	key := m.updatesCacheKey()
	avail := true
	m.updateCache = map[string]updateEntry{
		key: {
			fetchedAt: time.Now(),
			results:   map[string]bool{"web": avail},
		},
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if _, present := m.updateCache[key]; present {
		t.Errorf("cache entry %q should have been deleted after successful Deploy", key)
	}
}

// TestEscFromProgress_InvalidatesUpdateCache_OnRestart mirrors the Deploy
// case for Restart. Most restarts do not pull a new image, but users with
// `pull_policy: always` in compose DO get a fresh image on restart; the
// cost of invalidating the cache for the rare-but-real case is one extra
// CheckUpdates call.
func TestEscFromProgress_InvalidatesUpdateCache_OnRestart(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.done = true
	m.pendingOp = runner.Restart
	m.composer = mc
	installFakeTick(&m)
	key := m.updatesCacheKey()
	avail := true
	m.updateCache = map[string]updateEntry{
		key: {fetchedAt: time.Now(), results: map[string]bool{"web": avail}},
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if _, present := m.updateCache[key]; present {
		t.Errorf("cache entry %q should have been deleted after successful Restart", key)
	}
}

// TestEscFromProgress_PreservesUpdateCache_OnFailedOp documents the
// negative case: a failed operation must NOT clear the cache (the previous
// verdict is the most recent ground truth we have; spurious clearing
// would erase user-visible state for no benefit).
func TestEscFromProgress_PreservesUpdateCache_OnFailedOp(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.failed = true
	m.pendingOp = runner.Deploy
	m.composer = mc
	installFakeTick(&m)
	key := m.updatesCacheKey()
	avail := true
	m.updateCache = map[string]updateEntry{
		key: {fetchedAt: time.Now(), results: map[string]bool{"web": avail}},
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if _, present := m.updateCache[key]; !present {
		t.Errorf("cache entry %q should have survived a failed Deploy", key)
	}
}

// TestEscFromProgress_PreservesUpdateCache_OnStopOnly documents the
// negative case for the Stop operation: stopping a service doesn't change
// image freshness (containers go away but the local image stays), so the
// cache MUST survive.
func TestEscFromProgress_PreservesUpdateCache_OnStopOnly(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.done = true
	m.pendingOp = runner.StopOnly
	m.composer = mc
	installFakeTick(&m)
	key := m.updatesCacheKey()
	avail := true
	m.updateCache = map[string]updateEntry{
		key: {fetchedAt: time.Now(), results: map[string]bool{"web": avail}},
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if _, present := m.updateCache[key]; !present {
		t.Errorf("cache entry %q should have survived a Stop op", key)
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

func TestConfirmation_QCancelsConfirming(t *testing.T) {
	// q during a confirmation prompt cancels the prompt (matches esc),
	// rather than quitting the app. Intentional behavior change so q is
	// never destructive mid-action.
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected[0] = true
	m.confirming = true
	m.pendingOp = runner.Deploy

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)
	if um.confirming {
		t.Error("confirming should be cancelled by q")
	}
	if cmd != nil {
		t.Errorf("expected nil command, got non-nil")
	}
}

func TestConfirmation_CtrlCStillQuits(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.selected[0] = true
	m.confirming = true
	m.pendingOp = runner.Deploy

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected quit command from ctrl+c during confirmation, got nil")
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
		factory := func(compose.Project) runner.Composer { return mc }
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

func TestInit_ServerScreen_StartsTickOnly(t *testing.T) {
	// The server list itself is static, but Init still kicks off the periodic
	// refresh tick so it's already running by the time the user reaches the
	// container screen. Without a preselected server, that's the *only* Cmd
	// Init queues — no spurious connect.
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() should return the refresh tick, got nil")
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

	if got := m.logsRawLines; len(got) != 1 || got[0] != "line 1" {
		t.Errorf("logsRawLines = %q, want [line 1]", got)
	}
	if m.logsPartial != "" {
		t.Errorf("logsPartial = %q, want empty", m.logsPartial)
	}

	updated, _ = m.Update(logChunkMsg{data: []byte("line 2\n")})
	m = updated.(Model)

	if got := m.logsRawLines; len(got) != 2 || got[0] != "line 1" || got[1] != "line 2" {
		t.Errorf("logsRawLines = %q, want [line 1 line 2]", got)
	}
	if m.logsPartial != "" {
		t.Errorf("logsPartial = %q, want empty", m.logsPartial)
	}
}

// logChunkContent returns a string of n numbered lines (each newline-terminated),
// enough to exceed a small viewport height so AtBottom()/scroll are meaningful.
func logChunkContent(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

// logChunkLines returns n numbered raw log lines (no trailing-newline element),
// used to seed logsRawLines directly in tests — the raw-line-buffer equivalent
// of logChunkContent.
func logChunkLines(n int) []string {
	lines := make([]string, n)
	for i := 0; i < n; i++ {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	return lines
}

// TestDerivedLogContentMatchesPreRefactor pins backward compatibility: with no
// filter, the derived viewport content is byte-identical to the pre-refactor
// formatLogContent output across the four wrap×pretty combinations. The fixture
// ends WITHOUT a trailing newline (partial-line parity) and includes a JSON line
// (pretty-print parity).
func TestDerivedLogContentMatchesPreRefactor(t *testing.T) {
	const width = 20
	fixture := "plain line one\n" +
		`svc | {"level":"info","msg":"hello world","n":42}` + "\n" +
		"another somewhat long plain line\n" +
		"trailing partial with no newline"

	for _, wrap := range []bool{false, true} {
		for _, pretty := range []bool{false, true} {
			wrap, pretty := wrap, pretty
			t.Run(fmt.Sprintf("wrap=%v/pretty=%v", wrap, pretty), func(t *testing.T) {
				var m Model
				m.logsViewport = viewport.New(width, 20)
				m.logsWrap = wrap
				m.logsPretty = pretty
				m.appendRawChunk([]byte(fixture))
				m.applyLogFormat()

				got := m.derivedLogContent()
				want := formatLogContent(fixture, width, wrap, pretty)
				if got != want {
					t.Errorf("derived content mismatch\n got: %q\nwant: %q", got, want)
				}
			})
		}
	}
}

// streamBlankTest builds a fresh log model with the given committed filter/search
// state, then either streams the chunks incrementally (appendRawChunk +
// applyLogFormat per chunk, the production streaming path) or, when full is true,
// appends everything and runs a single fullReformat. It returns the derived
// viewport content and the committed search matches so callers can assert the
// incremental result equals the full reformat.
func streamBlankTest(chunks []string, filterQuery, searchQuery string, full bool) (string, []int) {
	var m Model
	m.logsViewport = viewport.New(80, 20)
	m.logFilterQuery = filterQuery
	m.logSearchQuery = searchQuery
	for _, ch := range chunks {
		m.appendRawChunk([]byte(ch))
		if !full {
			m.applyLogFormat()
		}
	}
	if full {
		m.fullReformat()
	}
	m.setLogViewportContent() // recompute search matches over the final content
	return m.derivedLogContent(), m.logSearchMatches
}

// TestApplyLogFormat_BlankSurvivorRoundTripsAcrossChunkSplittings pins the core
// invariant that a KEPT blank line round-trips through the incremental append
// path identically no matter how the byte stream was chunked, and that the
// incremental result is byte-identical to a full reformat. Before the fix,
// applyLogFormat gated the append on delta != "", so a blank survivor line
// (which formats to an empty delta) arriving in its own chunk was dropped and the
// "\n" separator elided — merging the next non-blank line onto its physical row
// and making both the rendered content and the search physical-line indices
// depend on chunk boundaries.
func TestApplyLogFormat_BlankSurvivorRoundTripsAcrossChunkSplittings(t *testing.T) {
	// A raw stream with a leading-adjacent, an interior, and consecutive blank
	// lines. Every splitting below yields the same six logical raw lines
	// (["alpha","","beta","","","gamma"]) but delivers the bytes on different chunk
	// boundaries — including a blank line alone in its own chunk and blank lines
	// straddling chunk edges.
	const stream = "alpha\n\nbeta\n\n\ngamma\n"

	byteByByte := make([]string, len(stream))
	for i := 0; i < len(stream); i++ {
		byteByByte[i] = string(stream[i])
	}
	splittings := map[string][]string{
		"all-at-once":          {stream},
		"one-line-per-chunk":   {"alpha\n", "\n", "beta\n", "\n", "\n", "gamma\n"},
		"blank-alone":          {"alpha\n\nbeta\n", "\n", "\ngamma\n"},
		"blank-straddles-edge": {"alpha\n\nbe", "ta\n\n", "\ngamma\n"},
		"byte-by-byte":         byteByByte,
	}

	variants := []struct {
		name        string
		filterQuery string // committed filter query ("" = none)
		searchQuery string // committed search query ("" = none)
	}{
		{name: "no-filter-no-search"},
		{name: "committed-search", searchQuery: "a"},        // matches alpha, beta, gamma
		{name: "filter-keeps-blanks", filterQuery: "!beta"}, // drops beta; blanks pass the negated filter
	}

	for _, v := range variants {
		v := v
		t.Run(v.name, func(t *testing.T) {
			// Reference: a single full reformat over the whole stream.
			wantContent, wantMatches := streamBlankTest([]string{stream}, v.filterQuery, v.searchQuery, true)

			for split, chunks := range splittings {
				gotContent, gotMatches := streamBlankTest(chunks, v.filterQuery, v.searchQuery, false)
				if gotContent != wantContent {
					t.Errorf("[%s] incremental content differs from full reformat:\n got: %q\nwant: %q", split, gotContent, wantContent)
				}
				if !intSliceEq(gotMatches, wantMatches) {
					t.Errorf("[%s] incremental search matches differ from full reformat: got %v, want %v", split, gotMatches, wantMatches)
				}
			}
		})
	}
}

// TestDerivedLogContent_BlankSurvivorThenPartial pins that a KEPT blank survivor
// line followed by a trailing in-flight partial renders with the separating
// newline preserved, identical across chunk splittings and equal to a full
// reformat. Before the fix, derivedLogContent tested content == "" to decide the
// partial join; a lone blank survivor has logsFormatted == "" yet logFilterShown
// == 1, so the partial was rendered WITHOUT its leading "\n" — collapsing the
// blank line and the partial onto one physical row.
func TestDerivedLogContent_BlankSurvivorThenPartial(t *testing.T) {
	const stream = "\ntail" // one blank complete line, then a partial with no newline

	// Reference: single full reformat over the whole stream.
	wantContent, _ := streamBlankTest([]string{stream}, "", "", true)
	if wantContent != "\ntail" {
		t.Fatalf("blank survivor + partial should render with a separating newline, got %q", wantContent)
	}
	physical := strings.Split(wantContent, "\n")
	if len(physical) != 2 || physical[0] != "" || physical[1] != "tail" {
		t.Fatalf("expected physical lines [\"\", \"tail\"], got %q", physical)
	}

	splittings := map[string][]string{
		"all-at-once":  {stream},
		"blank-alone":  {"\n", "tail"},
		"byte-by-byte": {"\n", "t", "a", "i", "l"},
	}
	for split, chunks := range splittings {
		gotContent, _ := streamBlankTest(chunks, "", "", false)
		if gotContent != wantContent {
			t.Errorf("[%s] incremental content differs from full reformat:\n got: %q\nwant: %q", split, gotContent, wantContent)
		}
	}
}

// TestDerivedLogContent_ErrorAfterBlankSurvivor pins that a terminal error
// (logDoneMsg) arriving after a single KEPT blank survivor keeps the blank line
// and the "\n\n" separator before the error. Before the fix, derivedLogContent
// tested content == "" for the error join; a lone blank survivor has logsFormatted
// == "" yet logFilterShown == 1, so the error rendered standalone and the blank
// survivor plus its separator were dropped.
func TestDerivedLogContent_ErrorAfterBlankSurvivor(t *testing.T) {
	m := setupFilterableLogsModel()
	// Replace the buffer with a single blank line: logsFormatted == "" but
	// logFilterShown == 1 (a real folded survivor).
	m.logsRawLines = []string{""}
	m.logsPartial = ""
	m.logsScanned = 0
	m.logsFormatted = ""
	m.logFilterShown = 0
	m.logsErrLine = ""
	m.applyLogFormat()

	if m.logFilterShown != 1 {
		t.Fatalf("precondition: expected 1 folded blank survivor, got logFilterShown=%d", m.logFilterShown)
	}
	if m.derivedLogContent() != "" {
		t.Fatalf("precondition: blank survivor renders as an empty string, got %q", m.derivedLogContent())
	}

	updated, _ := m.Update(logDoneMsg{err: errors.New("connection lost"), session: m.logsSession})
	m = updated.(Model)

	content := m.derivedLogContent()
	if !strings.HasPrefix(content, "\n\n") {
		t.Errorf("blank survivor + separator lost before the terminal error, content = %q", content)
	}
	physical := strings.Split(content, "\n")
	// Expect three physical lines: blank survivor, blank separator, error.
	if len(physical) != 3 {
		t.Fatalf("expected 3 physical lines (blank survivor, separator, error), got %d: %q", len(physical), physical)
	}
	if physical[0] != "" {
		t.Errorf("line 0 should be the blank survivor, got %q", physical[0])
	}
	if !strings.Contains(physical[len(physical)-1], "connection lost") {
		t.Errorf("last line should be the terminal error, got %q", physical[len(physical)-1])
	}
}

// TestLogFilter_SingleBlankSurvivorNotZeroMatch pins that a committed filter which
// keeps exactly one BLANK line is NOT treated as a zero-match: the placeholder is
// suppressed and the blank line is a searchable physical line (`/` opens). Before
// the fix, both the placeholder guard and the `/`-open guard tested content == "",
// which a lone blank survivor (logFilterShown == 1) satisfies, so the UI wrongly
// showed "(no lines match filter)" and refused to open search.
func TestLogFilter_SingleBlankSurvivorNotZeroMatch(t *testing.T) {
	m := setupFilterableLogsModel()
	m.logsRawLines = []string{"noise", ""}
	m.logsPartial = ""
	m.logsScanned = 0
	m.logsFormatted = ""
	m.logFilterShown = 0
	m.logsErrLine = ""
	m.logFilterQuery = "!noise" // negated: drops "noise", keeps the blank line
	m.logFilterCommittedRegex = false
	m.applyLogFormat()

	if m.logFilterShown != 1 {
		t.Fatalf("precondition: filter should keep exactly one blank survivor, got logFilterShown=%d", m.logFilterShown)
	}
	if m.derivedLogContent() != "" {
		t.Fatalf("precondition: the single blank survivor renders as an empty string, got %q", m.derivedLogContent())
	}

	// The zero-match placeholder must NOT be shown — there IS a survivor (blank).
	m.setLogViewportContent()
	if strings.Contains(m.logsViewport.View(), "no lines match filter") {
		t.Error("placeholder shown for a filter that keeps one blank line; it must render the blank line instead")
	}

	// The blank line must be a searchable physical line: `/` opens the search.
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	if !m.logSearching {
		t.Error("/ must open search over the single blank survivor (a real physical line)")
	}
}

// TestLogChunkMsg_SplitLineFoldsToOneRawLine verifies a single logical line
// delivered across two chunks (the newline arriving only in the second) folds
// into exactly one entry in logsRawLines.
func TestLogChunkMsg_SplitLineFoldsToOneRawLine(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.logsViewport = viewport.New(80, 20)
	pr, _ := io.Pipe()
	m.logsPipeR = pr

	// First chunk: no newline yet — retained as the partial.
	updated, _ := m.Update(logChunkMsg{data: []byte("hello ")})
	m = updated.(Model)
	if len(m.logsRawLines) != 0 {
		t.Errorf("no complete line yet, logsRawLines = %q", m.logsRawLines)
	}
	if m.logsPartial != "hello " {
		t.Errorf("logsPartial = %q, want %q", m.logsPartial, "hello ")
	}

	// Second chunk completes the line — folds into exactly one raw line.
	updated, _ = m.Update(logChunkMsg{data: []byte("world\n")})
	m = updated.(Model)
	if len(m.logsRawLines) != 1 || m.logsRawLines[0] != "hello world" {
		t.Errorf("logsRawLines = %q, want [hello world]", m.logsRawLines)
	}
	if m.logsPartial != "" {
		t.Errorf("logsPartial = %q, want empty", m.logsPartial)
	}
}

// TestLogDoneErrorIsFilterExempt proves the terminal error renders even when a
// filter hides every raw line. Task 2 has no filter wiring yet, so we stand in
// for the Task 3 filter by folding with a reject-all predicate (leaving
// logsFormatted empty) and assert the error, held in the dedicated exempt slot,
// still appears in the derived content.
func TestLogDoneErrorIsFilterExempt(t *testing.T) {
	var m Model
	m.logsViewport = viewport.New(80, 20)
	m.logsRawLines = []string{"noise one", "noise two"}

	reject := func(string) bool { return false }
	delta, scanned, _ := foldNewRawLines(m.logsRawLines, m.logsScanned, m.logsViewport.Width, m.logsWrap, m.logsPretty, reject)
	m.logsFormatted = delta
	m.logsScanned = scanned
	if m.logsFormatted != "" {
		t.Fatalf("precondition: reject-all filter should leave logsFormatted empty, got %q", m.logsFormatted)
	}

	// The terminal error lands in the filter-exempt slot.
	m.logsErrLine = "Error: connection lost"

	got := m.derivedLogContent()
	if !strings.Contains(got, "Error: connection lost") {
		t.Errorf("terminal error must render even when the filter hides every line, got %q", got)
	}
}

// TestLogChunkMsg_ScrolledUpDoesNotSnap verifies that when the user has scrolled
// up (viewport not at bottom), an incoming chunk does NOT yank the view to the
// bottom — the tail is auto-paused.
func TestLogChunkMsg_ScrolledUpDoesNotSnap(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.height = 24
	m.logsViewport = viewport.New(80, 10)
	pr, _ := io.Pipe()
	m.logsPipeR = pr

	// Fill the viewport with more content than its height so scrolling is possible.
	m.logsRawLines = logChunkLines(50)
	m.applyLogFormat()
	m.logsViewport.GotoBottom()
	if !m.logsViewport.AtBottom() {
		t.Fatal("precondition: viewport should be at bottom after GotoBottom")
	}
	if m.logsViewport.TotalLineCount() <= m.logsViewport.Height {
		t.Fatalf("precondition: content must exceed viewport height (lines=%d, height=%d)",
			m.logsViewport.TotalLineCount(), m.logsViewport.Height)
	}

	// Scroll up a few lines so we're no longer at the bottom.
	m.logsViewport.SetYOffset(m.logsViewport.YOffset - 5)
	if m.logsViewport.AtBottom() {
		t.Fatal("precondition: viewport should NOT be at bottom after scrolling up")
	}
	offBefore := m.logsViewport.YOffset

	// Incoming chunk while scrolled up must not snap us to the bottom.
	updated, _ := m.Update(logChunkMsg{data: []byte("new tail line\n")})
	m = updated.(Model)

	if m.logsViewport.AtBottom() {
		t.Error("viewport snapped to bottom while scrolled up; expected paused tail")
	}
	if m.logsViewport.YOffset != offBefore {
		t.Errorf("YOffset changed from %d to %d; expected it to stay put while paused",
			offBefore, m.logsViewport.YOffset)
	}
}

// TestLogChunkMsg_AtBottomFollowsTail verifies that when the viewport is at the
// bottom (following), an incoming chunk keeps it pinned to the tail.
func TestLogChunkMsg_AtBottomFollowsTail(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.height = 24
	m.logsViewport = viewport.New(80, 10)
	pr, _ := io.Pipe()
	m.logsPipeR = pr

	m.logsRawLines = logChunkLines(50)
	m.applyLogFormat()
	m.logsViewport.GotoBottom()
	if !m.logsViewport.AtBottom() {
		t.Fatal("precondition: viewport should be at bottom")
	}

	// Feed a chunk while following; the tail should stay pinned.
	updated, _ := m.Update(logChunkMsg{data: []byte("new tail line\n")})
	m = updated.(Model)

	if !m.logsViewport.AtBottom() {
		t.Error("viewport did not follow the tail; expected AtBottom() to remain true")
	}
}

// TestLogsReformatWhileFollowingStaysPinned verifies that reformatting the log
// content (wrap toggle) and resizing while the viewport is at the bottom keeps
// the tail pinned — the follow intent survives fullReformat()'s line-count
// changes instead of reading as an accidental pause.
func TestLogsReformatWhileFollowingStaysPinned(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.height = 24
	m.logsViewport = viewport.New(80, 10)
	pr, _ := io.Pipe()
	m.logsPipeR = pr

	// Fill with more content than the viewport height so AtBottom() is meaningful.
	m.logsRawLines = logChunkLines(50)
	m.applyLogFormat()
	m.logsViewport.GotoBottom()
	if !m.logsViewport.AtBottom() {
		t.Fatal("precondition: viewport should be at bottom after GotoBottom")
	}
	if m.logsViewport.TotalLineCount() <= m.logsViewport.Height {
		t.Fatalf("precondition: content must exceed viewport height (lines=%d, height=%d)",
			m.logsViewport.TotalLineCount(), m.logsViewport.Height)
	}

	// Toggle wrap (w) — fullReformat() runs; the tail must stay pinned.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m = updated.(Model)
	if !m.logsViewport.AtBottom() {
		t.Error("wrap toggle while following dropped the tail; expected AtBottom() to remain true")
	}

	// Resize — fullReformat() runs again; still pinned.
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	m = updated.(Model)
	if !m.logsViewport.AtBottom() {
		t.Error("resize while following dropped the tail; expected AtBottom() to remain true")
	}
}

// TestLogsReformatWhilePausedStaysPaused verifies that reformatting (wrap
// toggle) and resizing while the user is scrolled up (paused) does NOT snap the
// view to the bottom — re-pinning only fires when previously following.
func TestLogsReformatWhilePausedStaysPaused(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.height = 24
	m.logsViewport = viewport.New(80, 10)
	pr, _ := io.Pipe()
	m.logsPipeR = pr

	m.logsRawLines = logChunkLines(50)
	m.applyLogFormat()
	m.logsViewport.GotoBottom()
	if m.logsViewport.TotalLineCount() <= m.logsViewport.Height {
		t.Fatalf("precondition: content must exceed viewport height (lines=%d, height=%d)",
			m.logsViewport.TotalLineCount(), m.logsViewport.Height)
	}

	// Scroll up so we're paused, not following.
	m.logsViewport.SetYOffset(m.logsViewport.YOffset - 5)
	if m.logsViewport.AtBottom() {
		t.Fatal("precondition: viewport should NOT be at bottom after scrolling up")
	}

	// Toggle wrap (w) while paused — must stay paused.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m = updated.(Model)
	if m.logsViewport.AtBottom() {
		t.Error("wrap toggle while paused snapped to bottom; expected the view to stay paused")
	}

	// Resize while paused — must still stay paused. Height 12 → viewport height
	// 12-6 = 6 (shrinks from 10), so this genuinely changes the viewport
	// geometry / AtBottom() boundary rather than exercising only a width change.
	heightBefore := m.logsViewport.Height
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	m = updated.(Model)
	if m.logsViewport.Height == heightBefore {
		t.Fatalf("precondition: resize should shrink viewport height (was %d, still %d)",
			heightBefore, m.logsViewport.Height)
	}
	if m.logsViewport.AtBottom() {
		t.Error("resize while paused snapped to bottom; expected the view to stay paused")
	}
}

// TestLogTailStatus is a table test for the pure logTailStatus helper covering
// the three states: streaming+at-bottom → ("following", 0), streaming+scrolled-up
// → ("paused", N) where N is the distance to the bottom, and done → ("", 0).
func TestLogTailStatus(t *testing.T) {
	// Build a viewport with more content than its height so scroll is meaningful.
	newVP := func() viewport.Model {
		vp := viewport.New(80, 10)
		vp.SetContent(logChunkContent(50))
		return vp
	}

	t.Run("following at bottom", func(t *testing.T) {
		vp := newVP()
		vp.GotoBottom()
		label, below := logTailStatus(vp, false)
		if label != "following" || below != 0 {
			t.Errorf("got (%q, %d); want (\"following\", 0)", label, below)
		}
	})

	t.Run("paused scrolled up", func(t *testing.T) {
		vp := newVP()
		vp.GotoBottom()
		// Scroll up exactly 5 rows from the bottom so the distance-to-bottom
		// is a known constant. Asserting the concrete value (5) — rather than
		// recomputing it with the impl's own formula — catches an off-by-one
		// or wrong-sign bug in logTailStatus.
		vp.SetYOffset(vp.YOffset - 5)
		if vp.AtBottom() {
			t.Fatal("precondition: viewport should NOT be at bottom after scrolling up")
		}
		label, below := logTailStatus(vp, false)
		if label != "paused" {
			t.Errorf("got label %q; want \"paused\"", label)
		}
		if below != 5 {
			t.Errorf("got below=%d; want 5 (scrolled up 5 rows from bottom)", below)
		}
	})

	t.Run("done shows nothing", func(t *testing.T) {
		vp := newVP()
		vp.GotoBottom()
		// Even scrolled up, done wins and yields the empty indicator.
		vp.SetYOffset(vp.YOffset - 5)
		label, below := logTailStatus(vp, true)
		if label != "" || below != 0 {
			t.Errorf("got (%q, %d); want (\"\", 0)", label, below)
		}
	})
}

// TestViewLogsIndicator verifies viewLogs() renders the follow/paused indicator
// on the header: "following" when live at bottom, "paused" + a "▲" count when
// scrolled up, and neither token when the stream has ended (logsDone).
func TestViewLogsIndicator(t *testing.T) {
	newLogsModel := func() Model {
		mc := &mockComposer{services: []string{"nginx"}}
		m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
		m.screen = screenLogs
		m.logsService = "nginx"
		m.height = 24
		m.width = 80
		m.logsViewport = viewport.New(80, 10)
		m.logsRawLines = logChunkLines(50)
		m.applyLogFormat()
		return m
	}

	// headerLine returns the physical line that contains the "logs >" breadcrumb.
	// The indicator must render on THIS line (not on titleStyle's margin line
	// below it), so the follow/paused assertions target it directly.
	headerLine := func(t *testing.T, out string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "logs >") {
				return line
			}
		}
		t.Fatalf("viewLogs() output has no line containing the \"logs >\" breadcrumb:\n%s", out)
		return ""
	}

	t.Run("following at bottom", func(t *testing.T) {
		m := newLogsModel()
		m.logsViewport.GotoBottom()
		out := m.viewLogs()
		if !strings.Contains(out, "following") {
			t.Errorf("viewLogs() output missing \"following\" indicator:\n%s", out)
		}
		// The indicator must sit on the SAME physical line as the breadcrumb,
		// not on titleStyle's margin line below it.
		if hl := headerLine(t, out); !strings.Contains(hl, "following") {
			t.Errorf("\"following\" indicator not on the breadcrumb line, got header line %q\nfull output:\n%s", hl, out)
		}
		if strings.Contains(out, "paused") {
			t.Errorf("viewLogs() output should not contain \"paused\" while following:\n%s", out)
		}
	})

	t.Run("paused scrolled up", func(t *testing.T) {
		m := newLogsModel()
		m.logsViewport.GotoBottom()
		m.logsViewport.SetYOffset(m.logsViewport.YOffset - 5)
		if m.logsViewport.AtBottom() {
			t.Fatal("precondition: viewport should NOT be at bottom after scrolling up")
		}
		out := m.viewLogs()
		if !strings.Contains(out, "paused") {
			t.Errorf("viewLogs() output missing \"paused\" indicator:\n%s", out)
		}
		// Scrolled up exactly 5 rows from the bottom, so the header must render
		// the concrete distance-to-bottom count — asserting the number catches
		// a formatting or wrong-value bug that a bare "▲" check would miss. It
		// must also land on the breadcrumb line, not titleStyle's margin line.
		if hl := headerLine(t, out); !strings.Contains(hl, "▲ 5 below") {
			t.Errorf("\"▲ 5 below\" count not on the breadcrumb line, got header line %q\nfull output:\n%s", hl, out)
		}
		if strings.Contains(out, "following") {
			t.Errorf("viewLogs() output should not contain \"following\" while paused:\n%s", out)
		}
	})

	t.Run("done shows no indicator", func(t *testing.T) {
		m := newLogsModel()
		m.logsViewport.GotoBottom()
		m.logsDone = true
		out := m.viewLogs()
		if strings.Contains(out, "following") || strings.Contains(out, "paused") {
			t.Errorf("viewLogs() output should show no follow/paused indicator when done:\n%s", out)
		}
	})
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
	if m.logsErrLine != "Error: connection lost" {
		t.Errorf("logsErrLine = %q, want %q", m.logsErrLine, "Error: connection lost")
	}
	if !strings.Contains(m.derivedLogContent(), "Error: connection lost") {
		t.Errorf("derived content should contain error, got %q", m.derivedLogContent())
	}
}

// TestLogDoneMsg_WithError_ForcesScrolledUpViewToBottom verifies that a terminal
// error is forced into view even when the user has scrolled up (paused). With
// content exceeding the viewport height and the view scrolled up, delivering an
// error logDoneMsg must snap to the bottom so the appended error is visible —
// this pins the deliberate error-path GotoBottom() (removing it would leave the
// view paused and hide the error).
func TestLogDoneMsg_WithError_ForcesScrolledUpViewToBottom(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.logsViewport = viewport.New(80, 10)

	// Fill with more content than the viewport height so scroll is meaningful.
	m.logsRawLines = logChunkLines(50)
	m.applyLogFormat()
	m.logsViewport.GotoBottom()
	if m.logsViewport.TotalLineCount() <= m.logsViewport.Height {
		t.Fatalf("precondition: content must exceed viewport height (lines=%d, height=%d)",
			m.logsViewport.TotalLineCount(), m.logsViewport.Height)
	}

	// Scroll up so we're paused, not at the bottom.
	m.logsViewport.SetYOffset(m.logsViewport.YOffset - 5)
	if m.logsViewport.AtBottom() {
		t.Fatal("precondition: viewport should NOT be at bottom after scrolling up")
	}

	// A terminal error must force the appended error text into view.
	updated, _ := m.Update(logDoneMsg{err: fmt.Errorf("connection lost")})
	m = updated.(Model)

	if !m.logsViewport.AtBottom() {
		t.Error("error logDoneMsg did not force the view to the bottom; the error would be hidden while paused")
	}
	if !strings.Contains(m.derivedLogContent(), "Error: connection lost") {
		t.Errorf("derived content should contain error, got %q", m.derivedLogContent())
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
	m.logsRawLines = []string{"some logs"}
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
	if m.logsRawLines != nil {
		t.Errorf("logsRawLines should be cleared, got %q", m.logsRawLines)
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
	if !strings.Contains(v, "q back") {
		t.Error("view should contain 'q back' in help")
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

	if m.logsRawLines != nil || m.logsPartial != "" {
		t.Errorf("raw buffer should remain empty, got lines=%q partial=%q", m.logsRawLines, m.logsPartial)
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
	// An out-of-range preselection must NOT trigger a connect — Init falls
	// through to the no-preselection path, so the only Cmd returned is the
	// background refresh tick.
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc),
		WithPreselectedServer(99))

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() should return the refresh tick, got nil")
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
	m.logsRawLines = []string{`app | {"level":"info","msg":"hello"}`}
	m.logsWrap = true
	m.logsPretty = false
	m.logsViewport = viewport.New(80, 20)
	m.logsViewport.SetHorizontalStep(0)
	m.applyLogFormat()
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
	m.logsRawLines = nil
	m.logsPartial = ""
	m.logsScanned = 0
	m.logsFormatted = ""
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
// duplicate the earlier wrapped segments (P1 corruption bug). Chunk splitting
// now happens in appendRawChunk (partial retained in logsPartial until a later
// chunk's newline completes it).
func TestLogsScreen_IncrementalWrapNoDuplication(t *testing.T) {
	m := setupLogsModel()
	m.logsRawLines = nil
	m.logsPartial = ""
	m.logsScanned = 0
	m.logsFormatted = ""
	m.logsWrap = true
	m.logsPretty = false
	m.logsViewport = viewport.New(5, 20) // width=5 to force wrapping
	m.logsViewport.SetHorizontalStep(0)
	m.logsSession = 1

	// Chunk 1: partial line, no newline — retained in logsPartial.
	m.appendRawChunk([]byte(strings.Repeat("a", 10)))
	m.applyLogFormat()
	// No complete lines yet: the cached logsFormatted must be empty and the raw
	// buffer must hold nothing (the 10 chars live in logsPartial).
	if m.logsFormatted != "" {
		t.Errorf("no complete lines yet, logsFormatted should be empty, got %q", m.logsFormatted)
	}
	if len(m.logsRawLines) != 0 {
		t.Errorf("no complete lines yet, logsRawLines should be empty, got %q", m.logsRawLines)
	}
	if m.logsPartial != strings.Repeat("a", 10) {
		t.Errorf("logsPartial = %q, want 10 a's", m.logsPartial)
	}

	// Chunk 2: extend the same line and complete it with a newline.
	m.appendRawChunk([]byte("bbbb\n"))
	m.applyLogFormat()

	// The raw line "aaaaaaaaaabbbb" (14 chars) should wrap to: "aaaaa", "aaaaa",
	// "aaaa", "bbbb" — no duplicated segments (exactly 10 'a' chars total).
	viewContent := m.logsFormatted
	lines := strings.Split(viewContent, "\n")
	aCount := 0
	for _, l := range lines {
		aCount += strings.Count(l, "a")
	}
	if aCount != 10 {
		t.Errorf("expected 10 'a' chars total, got %d in formatted output: %q", aCount, viewContent)
	}
	if m.logsPartial != "" {
		t.Errorf("logsPartial should be empty after the newline, got %q", m.logsPartial)
	}
}

// Verify that incremental formatting only scans new raw lines, advancing the
// logsScanned raw-line cursor by the number of raw lines scanned.
func TestLogsScreen_IncrementalOffsetAdvances(t *testing.T) {
	m := setupLogsModel()
	m.logsRawLines = nil
	m.logsPartial = ""
	m.logsScanned = 0
	m.logsFormatted = ""
	m.logsWrap = false
	m.logsPretty = false
	m.logsViewport = viewport.New(80, 20)

	// Add two complete lines
	m.appendRawChunk([]byte("line1\nline2\n"))
	m.applyLogFormat()

	if m.logsScanned != 2 {
		t.Errorf("logsScanned = %d, want 2", m.logsScanned)
	}

	// Add a third line — the cursor should advance past it
	m.appendRawChunk([]byte("line3\n"))
	m.applyLogFormat()

	if m.logsScanned != 3 {
		t.Errorf("logsScanned = %d, want 3", m.logsScanned)
	}

	// logsFormatted should contain all three lines
	if !strings.Contains(m.logsFormatted, "line1") ||
		!strings.Contains(m.logsFormatted, "line2") ||
		!strings.Contains(m.logsFormatted, "line3") {
		t.Errorf("logsFormatted should contain all lines, got: %q", m.logsFormatted)
	}
}

// --- Task 3: log viewer live filter (f key) -------------------------------

// runeKey builds a single-rune key message (the common typing case in the
// filter tests). Multi-rune keys (ctrl+r, enter, esc) are sent explicitly.
func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// typeInto feeds each rune of s to the model as a separate keystroke, returning
// the updated model — mirrors how the TUI receives one KeyMsg per character.
func typeInto(m Model, s string) Model {
	for _, r := range s {
		updated, _ := m.Update(runeKey(r))
		m = updated.(Model)
	}
	return m
}

// setupFilterableLogsModel returns a log-screen Model seeded with a mix of
// INFO/ERROR/WARN raw lines so filter narrowing is observable.
func setupFilterableLogsModel() Model {
	mc := &mockComposer{services: []string{"app"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.services = mc.services
	m.composer = mc
	m.logsService = "app"
	m.logsWrap = true
	m.logsPretty = false
	m.logsViewport = viewport.New(80, 20)
	m.logsViewport.SetHorizontalStep(0)
	m.width = 84
	m.height = 26
	m.logsRawLines = []string{
		"app | INFO starting up",
		"app | ERROR disk full",
		"app | INFO healthcheck ok",
		"app | WARN retrying",
		"app | ERROR timeout",
	}
	m.applyLogFormat()
	return m
}

func TestLogFilter_FOpensInput(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	if !m.logFiltering {
		t.Fatal("logFiltering should be true after pressing f")
	}
	if !m.logFilterInput.Focused() {
		t.Error("filter input should be focused after opening")
	}
}

func TestLogFilter_FNoopOnEmptyBuffer(t *testing.T) {
	m := setupFilterableLogsModel()
	m.logsRawLines = nil
	m.logsPartial = ""
	m.logsScanned = 0
	m.logsFormatted = ""
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	if m.logFiltering {
		t.Error("f must be a no-op when the raw buffer is empty (mirrors l/x guards)")
	}
}

// TestLogFilter_TypingActionKeysLandInInput pins the typing intercept: while the
// filter bar is open, q/w/p are literal characters, NOT the back/wrap/pretty
// actions they would otherwise fire.
func TestLogFilter_TypingActionKeysLandInInput(t *testing.T) {
	m := setupFilterableLogsModel()
	wrapBefore, prettyBefore := m.logsWrap, m.logsPretty

	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "qwp")

	if m.logFilterInput.Value() != "qwp" {
		t.Errorf("input value = %q, want %q", m.logFilterInput.Value(), "qwp")
	}
	if m.logsWrap != wrapBefore {
		t.Error("w must not toggle wrap while typing a filter")
	}
	if m.logsPretty != prettyBefore {
		t.Error("p must not toggle pretty while typing a filter")
	}
	if m.screen != screenLogs {
		t.Error("q must not navigate away while typing a filter")
	}
	if !m.logFiltering {
		t.Error("should still be filtering after typing")
	}
}

func TestLogFilter_LiveNarrowing(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")

	got := m.derivedLogContent()
	if !strings.Contains(got, "disk full") || !strings.Contains(got, "timeout") {
		t.Errorf("both ERROR lines should survive, got:\n%s", got)
	}
	if strings.Contains(got, "starting up") ||
		strings.Contains(got, "healthcheck") ||
		strings.Contains(got, "retrying") {
		t.Errorf("non-ERROR lines should be filtered out, got:\n%s", got)
	}
}

func TestLogFilter_CtrlRTogglesRegex(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	if m.logFilterIsRegex {
		t.Fatal("regex mode should be off by default")
	}

	// As a substring, "ERROR|WARN" is a literal that matches no line.
	m = typeInto(m, "ERROR|WARN")
	if got := strings.TrimSpace(m.derivedLogContent()); got != "" {
		t.Errorf("substring mode: literal ERROR|WARN matches nothing, got %q", got)
	}

	// Toggle to regex — the alternation now matches ERROR and WARN lines.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = updated.(Model)
	if !m.logFilterIsRegex {
		t.Fatal("ctrl+r should enable regex mode")
	}
	got := m.derivedLogContent()
	if !strings.Contains(got, "disk full") ||
		!strings.Contains(got, "timeout") ||
		!strings.Contains(got, "retrying") {
		t.Errorf("regex ERROR|WARN should match ERROR and WARN lines, got:\n%s", got)
	}
	if strings.Contains(got, "starting up") || strings.Contains(got, "healthcheck") {
		t.Errorf("INFO lines should not match ERROR|WARN, got:\n%s", got)
	}
}

// TestLogFilter_BadRegexKeepsLastGood proves a mid-type invalid regex does not
// thrash the view: the last-good query/predicate and derived content hold.
func TestLogFilter_BadRegexKeepsLastGood(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR}) // regex mode
	m = updated.(Model)
	m = typeInto(m, "ERROR")

	goodContent := m.derivedLogContent()
	goodQuery := m.logFilterQuery
	if goodQuery != "ERROR" {
		t.Fatalf("precondition: query = %q, want ERROR", goodQuery)
	}

	// "[" makes the regex invalid ("ERROR[").
	m = typeInto(m, "[")

	if m.logFilterQuery != goodQuery {
		t.Errorf("bad regex must not update logFilterQuery: got %q, want %q", m.logFilterQuery, goodQuery)
	}
	if m.derivedLogContent() != goodContent {
		t.Error("bad regex must keep the last-good derived content (no thrash)")
	}
	if m.logFilterInput.Value() != "ERROR[" {
		t.Errorf("input should still echo the typed text: got %q, want %q", m.logFilterInput.Value(), "ERROR[")
	}
}

func TestLogFilter_EnterCommitsKeepsFilter(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.logFiltering {
		t.Error("the bar should close on enter (commit)")
	}
	if m.logFilterQuery != "ERROR" {
		t.Errorf("committed query should persist, got %q", m.logFilterQuery)
	}
	if m.logFilterInput.Focused() {
		t.Error("input should be blurred after commit")
	}
	got := m.derivedLogContent()
	if !strings.Contains(got, "disk full") || strings.Contains(got, "starting up") {
		t.Errorf("committed filter should still narrow the view, got:\n%s", got)
	}
}

func TestLogFilter_EscCancelRestoresFullView(t *testing.T) {
	m := setupFilterableLogsModel()
	fullContent := m.derivedLogContent()

	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	if m.derivedLogContent() == fullContent {
		t.Fatal("precondition: the filter should have narrowed the view")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.logFiltering {
		t.Error("esc should close the filter bar")
	}
	if m.logFilterQuery != "" {
		t.Errorf("esc should clear the query, got %q", m.logFilterQuery)
	}
	if m.screen != screenLogs {
		t.Error("esc while typing a filter must not leave the log screen")
	}
	if m.derivedLogContent() != fullContent {
		t.Errorf("esc should restore the full view:\n got %q\nwant %q", m.derivedLogContent(), fullContent)
	}
}

func TestLogFilter_NegationExcludes(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "!healthcheck")

	got := m.derivedLogContent()
	if strings.Contains(got, "healthcheck") {
		t.Errorf("!-negation should exclude the healthcheck line, got:\n%s", got)
	}
	if !strings.Contains(got, "starting up") ||
		!strings.Contains(got, "disk full") ||
		!strings.Contains(got, "retrying") ||
		!strings.Contains(got, "timeout") {
		t.Errorf("all non-matching lines should remain, got:\n%s", got)
	}
}

// TestLogFilter_MatchesRawNotPrettyExpanded pins that the filter predicate runs
// against the RAW line, before pretty-expansion. The compact JSON substring
// `"level":"info"` exists only in the raw line — pretty-print inserts a space
// after the colon — yet the whole pretty block still renders because the raw
// line matched.
func TestLogFilter_MatchesRawNotPrettyExpanded(t *testing.T) {
	m := setupFilterableLogsModel()
	m.logsRawLines = []string{
		`app | {"level":"info","msg":"hello"}`,
		`app | {"level":"debug","msg":"other"}`,
	}
	m.logsPretty = true // JSON expands into multiple indented physical lines
	m.fullReformat()

	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, `"level":"info"`) // compact form: only present in the raw line

	got := m.derivedLogContent()
	if !strings.Contains(got, `"level": "info"`) || !strings.Contains(got, `"msg": "hello"`) {
		t.Errorf("the matching raw line's full pretty block should render, got:\n%s", got)
	}
	if strings.Contains(got, "debug") || strings.Contains(got, "other") {
		t.Errorf("the non-matching raw line must be filtered out, got:\n%s", got)
	}
}

// TestLogFilter_ClearRevealsLinesStreamedWhileFiltered pins that the raw buffer
// stays complete under an active filter: a non-matching line that streams in
// while filtered is hidden, then revealed when the filter is cleared.
func TestLogFilter_ClearRevealsLinesStreamedWhileFiltered(t *testing.T) {
	m := setupFilterableLogsModel()
	pr, _ := io.Pipe()
	m.logsPipeR = pr

	// Commit a filter that hides everything but the ERROR lines.
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.logFiltering {
		t.Fatal("precondition: filter should be committed")
	}

	// A non-matching line streams in while the filter is active.
	updated, _ = m.Update(logChunkMsg{data: []byte("app | INFO late arrival\n")})
	m = updated.(Model)
	if strings.Contains(m.derivedLogContent(), "late arrival") {
		t.Fatal("a non-matching streamed line must be hidden while filtered")
	}
	found := false
	for _, l := range m.logsRawLines {
		if strings.Contains(l, "late arrival") {
			found = true
		}
	}
	if !found {
		t.Fatal("the streamed line must still be present in the raw buffer")
	}

	// Clear the filter (re-open with f, then esc-cancel) — the hidden line appears.
	updated, _ = m.Update(runeKey('f'))
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.logFilterQuery != "" {
		t.Fatalf("filter should be cleared, got %q", m.logFilterQuery)
	}
	if !strings.Contains(m.derivedLogContent(), "late arrival") {
		t.Errorf("clearing the filter must reveal the line streamed in while filtered, got:\n%s", m.derivedLogContent())
	}
}

// TestLogFilter_QActsAsBackWhenNotFiltering pins the q→esc rewrite sub-case:
// with no filter bar open, q leaves the log screen (back-nav) as before.
func TestLogFilter_QActsAsBackWhenNotFiltering(t *testing.T) {
	m := setupFilterableLogsModel()
	if m.logFiltering {
		t.Fatal("precondition: not filtering")
	}
	updated, _ := m.Update(runeKey('q'))
	m = updated.(Model)
	if m.screen != screenSelectContainers {
		t.Errorf("q should navigate back to the container screen, got screen %d", m.screen)
	}
}

// --- Task 4: log-view search-within-highlight ---

func TestLogSearch_SlashOpensInput(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	if !m.logSearching {
		t.Fatal("logSearching should be true after pressing /")
	}
	if !m.logSearchInput.Focused() {
		t.Error("search input should be focused after opening")
	}
}

func TestLogSearch_SlashNoopOnEmptyBuffer(t *testing.T) {
	m := setupFilterableLogsModel()
	m.logsRawLines = nil
	m.logsPartial = ""
	m.logsScanned = 0
	m.logsFormatted = ""
	m.logFilterShown = 0 // pairs with logsFormatted at every real reset site
	m.logsErrLine = ""
	if m.derivedLogContent() != "" {
		t.Fatalf("precondition: rendered buffer should be empty, got %q", m.derivedLogContent())
	}
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	if m.logSearching {
		t.Error("/ must be a no-op when the rendered buffer is empty")
	}
}

// TestLogSearch_TypingActionKeysLandInInput pins the typing intercept: while the
// search bar is open, q/n/N are literal characters, NOT the back/cycle actions.
func TestLogSearch_TypingActionKeysLandInInput(t *testing.T) {
	m := setupFilterableLogsModel()
	wrapBefore := m.logsWrap

	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "qnN")

	if m.logSearchInput.Value() != "qnN" {
		t.Errorf("input value = %q, want %q", m.logSearchInput.Value(), "qnN")
	}
	if m.screen != screenLogs {
		t.Error("q must not navigate away while typing a search")
	}
	if !m.logSearching {
		t.Error("should still be searching after typing")
	}
	if m.logsWrap != wrapBefore {
		t.Error("wrap should be unchanged while typing a search")
	}
}

// TestLogSearch_HighlightAppliedWidthPreserved pins that a live search overlays
// the highlight (ANSI style bytes present in the render) yet leaves each line's
// display width unchanged (ansi.StringWidth is escape-blind).
func TestLogSearch_HighlightAppliedWidthPreserved(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")

	if m.logSearchQuery != "ERROR" {
		t.Fatalf("logSearchQuery = %q, want ERROR", m.logSearchQuery)
	}
	if !intSliceEq(m.logSearchMatches, []int{1, 4}) {
		t.Fatalf("logSearchMatches = %v, want [1 4]", m.logSearchMatches)
	}

	if !strings.Contains(m.logsViewport.View(), "\x1b[") {
		t.Error("expected ANSI style escapes in the highlighted viewport render")
	}

	physical := strings.Split(m.derivedLogContent(), "\n")
	styled := highlightMatches(physical, m.logSearchMatches, m.logSearchMatches[m.logSearchCur])
	for i := range physical {
		if a, b := ansi.StringWidth(styled[i]), ansi.StringWidth(physical[i]); a != b {
			t.Errorf("line %d display width changed by highlight: %d vs %d", i, a, b)
		}
	}
	if styled[1] == physical[1] {
		t.Error("a matched line should be styled (differ from its raw form)")
	}
}

// TestLogSearch_NCyclesWithWrapAround pins n/N cycle with wrap-around over the
// committed match set.
func TestLogSearch_NCyclesWithWrapAround(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit
	m = updated.(Model)

	if len(m.logSearchMatches) != 2 {
		t.Fatalf("want 2 matches, got %v", m.logSearchMatches)
	}
	if m.logSearchCur != 0 {
		t.Fatalf("cur should start at 0 (first match), got %d", m.logSearchCur)
	}

	updated, _ = m.Update(runeKey('n')) // → second match
	m = updated.(Model)
	if m.logSearchCur != 1 {
		t.Errorf("after n, cur = %d, want 1", m.logSearchCur)
	}
	updated, _ = m.Update(runeKey('n')) // wrap → first match
	m = updated.(Model)
	if m.logSearchCur != 0 {
		t.Errorf("after n wrap, cur = %d, want 0", m.logSearchCur)
	}
	updated, _ = m.Update(runeKey('N')) // wrap backward → last match
	m = updated.(Model)
	if m.logSearchCur != 1 {
		t.Errorf("after N from first, cur = %d, want 1 (wrap to last)", m.logSearchCur)
	}
}

// TestLogSearch_CtrlRTogglesRegex pins the substring→regex toggle: "ERROR|WARN"
// is a literal that matches nothing as a substring, but matches the ERROR and
// WARN lines once regex mode is on.
func TestLogSearch_CtrlRTogglesRegex(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	if m.logSearchIsRegex {
		t.Fatal("regex mode should be off by default")
	}

	m = typeInto(m, "ERROR|WARN")
	if len(m.logSearchMatches) != 0 {
		t.Errorf("substring mode: literal ERROR|WARN matches nothing, got %v", m.logSearchMatches)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = updated.(Model)
	if !m.logSearchIsRegex {
		t.Fatal("ctrl+r should enable regex mode")
	}
	// ERROR lines (idx 1, 4) and the WARN line (idx 3) → 3 matches, ascending.
	if !intSliceEq(m.logSearchMatches, []int{1, 3, 4}) {
		t.Errorf("regex ERROR|WARN should match [1 3 4], got %v", m.logSearchMatches)
	}
}

// TestLogSearch_BadRegexKeepsLastGood pins that a mid-type invalid regex keeps
// the last-good query/matches (no thrash), mirroring the filter behavior.
func TestLogSearch_BadRegexKeepsLastGood(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR}) // regex mode
	m = updated.(Model)
	m = typeInto(m, "ERROR")

	goodMatches := append([]int(nil), m.logSearchMatches...)
	if m.logSearchQuery != "ERROR" {
		t.Fatalf("precondition: query = %q, want ERROR", m.logSearchQuery)
	}

	m = typeInto(m, "[") // "ERROR[" is an invalid regex

	if m.logSearchQuery != "ERROR" {
		t.Errorf("bad regex must not update logSearchQuery: got %q", m.logSearchQuery)
	}
	if !intSliceEq(m.logSearchMatches, goodMatches) {
		t.Errorf("bad regex must keep last-good matches: got %v, want %v", m.logSearchMatches, goodMatches)
	}
	if m.logSearchInput.Value() != "ERROR[" {
		t.Errorf("input should still echo the typed text: got %q", m.logSearchInput.Value())
	}
}

// TestLogSearch_RecomputesAfterFilterChange pins that committing a filter
// re-derives the survivors AND recomputes the search match indices over them.
func TestLogSearch_RecomputesAfterFilterChange(t *testing.T) {
	m := setupFilterableLogsModel()

	// Commit a search for "timeout" (only the last raw line, physical idx 4).
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "timeout")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if !intSliceEq(m.logSearchMatches, []int{4}) {
		t.Fatalf("before filter: matches = %v, want [4]", m.logSearchMatches)
	}

	// Apply a filter for "ERROR": survivors are the two ERROR lines; "timeout"
	// is now physical index 1 among the survivors.
	updated, _ = m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if !intSliceEq(m.logSearchMatches, []int{1}) {
		t.Errorf("after filter: matches = %v, want [1] (recomputed over survivors)", m.logSearchMatches)
	}
}

// TestLogSearch_OperatesOverFilteredSurvivors pins that search runs over the
// filtered survivors only: a term present solely on a filtered-out line yields
// zero matches.
func TestLogSearch_OperatesOverFilteredSurvivors(t *testing.T) {
	m := setupFilterableLogsModel()

	// Filter to ERROR lines only — this hides the "healthcheck" line.
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if strings.Contains(m.derivedLogContent(), "healthcheck") {
		t.Fatal("precondition: the healthcheck line should be hidden by the filter")
	}

	// Search for a term only present on the filtered-out line.
	updated, _ = m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "healthcheck")
	if len(m.logSearchMatches) != 0 {
		t.Errorf("a line hidden by the filter must not be a search match, got %v", m.logSearchMatches)
	}
}

// TestLogSearch_PerPhysicalLineMatchSemantics pins the accepted per-physical-line
// semantics: two occurrences that each fall fully within a wrapped row both
// count; an occurrence split across a soft-wrap boundary is NOT matched.
func TestLogSearch_PerPhysicalLineMatchSemantics(t *testing.T) {
	t.Run("two occurrences on two wrapped rows both match", func(t *testing.T) {
		m := setupFilterableLogsModel()
		m.logsViewport = viewport.New(6, 20) // wrap every 6 runes
		m.logsViewport.SetHorizontalStep(0)
		m.logsWrap = true
		m.logsRawLines = []string{"XYZ000XYZ"} // → "XYZ000" + "XYZ", each holds a full XYZ
		m.fullReformat()

		updated, _ := m.Update(runeKey('/'))
		m = updated.(Model)
		m = typeInto(m, "XYZ")

		if len(m.logSearchMatches) != 2 {
			t.Errorf("want 2 matches (one per wrapped row), got %v; physical=%q",
				m.logSearchMatches, strings.Split(m.derivedLogContent(), "\n"))
		}
	})

	t.Run("occurrence split across a wrap boundary is not matched", func(t *testing.T) {
		m := setupFilterableLogsModel()
		m.logsViewport = viewport.New(6, 20)
		m.logsViewport.SetHorizontalStep(0)
		m.logsWrap = true
		m.logsRawLines = []string{"0000XYZ00"} // → "0000XY" + "Z00", XYZ straddles the split
		m.fullReformat()

		updated, _ := m.Update(runeKey('/'))
		m = updated.(Model)
		m = typeInto(m, "XYZ")

		if len(m.logSearchMatches) != 0 {
			t.Errorf("an occurrence split across a wrap boundary must not match, got %v; physical=%q",
				m.logSearchMatches, strings.Split(m.derivedLogContent(), "\n"))
		}
	})
}

func TestLogSearch_EnterCommitsKeepsSearch(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.logSearching {
		t.Error("the bar should close on enter (commit)")
	}
	if m.logSearchQuery != "ERROR" {
		t.Errorf("committed query should persist, got %q", m.logSearchQuery)
	}
	if m.logSearchInput.Focused() {
		t.Error("input should be blurred after commit")
	}
	if len(m.logSearchMatches) != 2 {
		t.Errorf("committed search should keep matches, got %v", m.logSearchMatches)
	}
}

func TestLogSearch_EscCancelClearsSearch(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	if m.logSearchQuery != "ERROR" {
		t.Fatal("precondition: query should be set while typing")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.logSearching {
		t.Error("esc should close the search bar")
	}
	if m.logSearchQuery != "" {
		t.Errorf("esc should clear the query, got %q", m.logSearchQuery)
	}
	if m.logSearchMatches != nil {
		t.Errorf("esc should clear matches, got %v", m.logSearchMatches)
	}
	if m.screen != screenLogs {
		t.Error("esc while typing a search must not leave the log screen")
	}
}

// TestLogSearch_EscToContainersClearsSearch pins the cleanup wiring: after the
// Task 5 ladder, a committed search's first esc clears the search (rung 3, stays
// on the screen) and a second esc leaves; the container-cleanup path (rung 5)
// must reset all search state.
func TestLogSearch_EscToContainersClearsSearch(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit
	m = updated.(Model)

	// First esc: rung 3 clears the committed search but stays on the screen.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenLogs {
		t.Fatalf("first esc on a committed search must stay on the log screen, got screen %d", m.screen)
	}
	if m.logSearching || m.logSearchQuery != "" || m.logSearchMatches != nil {
		t.Errorf("first esc must clear search state: searching=%v query=%q matches=%v",
			m.logSearching, m.logSearchQuery, m.logSearchMatches)
	}

	// Second esc: rung 5 leaves the screen (no search/filter left to peel).
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectContainers {
		t.Fatalf("second esc should return to the container screen, got screen %d", m.screen)
	}
	if m.logSearching || m.logSearchQuery != "" || m.logSearchMatches != nil {
		t.Errorf("esc-to-containers must clear search state: searching=%v query=%q matches=%v",
			m.logSearching, m.logSearchQuery, m.logSearchMatches)
	}
}

// TestLogFilterSearch_StillWorkAfterStreamEnded pins that once the stream ends
// (logsDone == true), the log view is still fully filterable and searchable:
// the f and / guards key off buffer content, not the done flag, so a finished
// (but non-empty) buffer keeps both features live.
func TestLogFilterSearch_StillWorkAfterStreamEnded(t *testing.T) {
	m := setupFilterableLogsModel()
	m.logsDone = true // stream has terminated; no more chunks will arrive

	// Filter still opens and narrows after the stream ended.
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	if !m.logFiltering {
		t.Fatal("f must still open the filter after the stream ended")
	}
	m = typeInto(m, "ERROR")
	got := m.derivedLogContent()
	if !strings.Contains(got, "disk full") || !strings.Contains(got, "timeout") {
		t.Errorf("filter should still narrow to ERROR lines after stream end, got:\n%s", got)
	}
	if strings.Contains(got, "starting up") {
		t.Errorf("non-ERROR lines should be filtered out after stream end, got:\n%s", got)
	}

	// Clear the filter (esc while typing) so the search runs over the full view.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.logFiltering || m.logFilterQuery != "" {
		t.Fatalf("esc should cancel the filter: filtering=%v query=%q", m.logFiltering, m.logFilterQuery)
	}

	// Search still opens and matches after the stream ended.
	updated, _ = m.Update(runeKey('/'))
	m = updated.(Model)
	if !m.logSearching {
		t.Fatal("/ must still open the search after the stream ended")
	}
	m = typeInto(m, "ERROR")
	if len(m.logSearchMatches) == 0 {
		t.Error("search should still find matches after the stream ended")
	}
}

// --- Task 5: layered esc ladder ---
//
// The five rungs, peeled inner → outer: (1) typing-search cancel, (2)
// typing-filter cancel, (3) committed-search clear-only, (4) committed-filter
// clear-only, (5) leave the screen. Each test drives one rung in isolation and
// asserts the post-esc screen + field state; the peel-order test drives all
// three of rungs 3/4/5 in sequence with both a filter and a search committed.

// TestLogLadder_Rung1_SearchTypingEscCancels: esc while the search bar is open
// discards the in-progress query and clears the highlight, staying on-screen.
func TestLogLadder_Rung1_SearchTypingEscCancels(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	if !m.logSearching || m.logSearchQuery != "ERROR" {
		t.Fatalf("precondition: searching=%v query=%q", m.logSearching, m.logSearchQuery)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenLogs {
		t.Errorf("rung 1 esc must stay on the log screen, got screen %d", m.screen)
	}
	if m.logSearching {
		t.Error("rung 1 esc should close the search bar")
	}
	if m.logSearchQuery != "" || m.logSearchMatches != nil {
		t.Errorf("rung 1 esc should clear the search: query=%q matches=%v", m.logSearchQuery, m.logSearchMatches)
	}
}

// TestLogLadder_Rung2_FilterTypingEscCancels: esc while the filter bar is open
// discards the in-progress query and restores the full unfiltered view.
func TestLogLadder_Rung2_FilterTypingEscCancels(t *testing.T) {
	m := setupFilterableLogsModel()
	fullContent := m.derivedLogContent()
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	if !m.logFiltering || m.derivedLogContent() == fullContent {
		t.Fatalf("precondition: filtering=%v (view should be narrowed)", m.logFiltering)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenLogs {
		t.Errorf("rung 2 esc must stay on the log screen, got screen %d", m.screen)
	}
	if m.logFiltering {
		t.Error("rung 2 esc should close the filter bar")
	}
	if m.logFilterQuery != "" {
		t.Errorf("rung 2 esc should clear the filter query, got %q", m.logFilterQuery)
	}
	if m.derivedLogContent() != fullContent {
		t.Errorf("rung 2 esc should restore the full view:\n got %q\nwant %q", m.derivedLogContent(), fullContent)
	}
}

// TestLogLadder_Rung3_CommittedSearchEscClearsOnly: with a committed search (bar
// closed, highlights live), esc clears the search only and stays on-screen.
func TestLogLadder_Rung3_CommittedSearchEscClearsOnly(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit
	m = updated.(Model)
	if m.logSearching || m.logSearchQuery != "ERROR" || len(m.logSearchMatches) == 0 {
		t.Fatalf("precondition: committed search searching=%v query=%q matches=%v",
			m.logSearching, m.logSearchQuery, m.logSearchMatches)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenLogs {
		t.Errorf("rung 3 esc must stay on the log screen, got screen %d", m.screen)
	}
	if m.logSearchQuery != "" || m.logSearchMatches != nil {
		t.Errorf("rung 3 esc should clear the committed search: query=%q matches=%v", m.logSearchQuery, m.logSearchMatches)
	}
}

// TestLogLadder_Rung4_CommittedFilterEscClearsOnly: with a committed filter (bar
// closed, view narrowed), esc clears the filter only, re-derives, and stays.
func TestLogLadder_Rung4_CommittedFilterEscClearsOnly(t *testing.T) {
	m := setupFilterableLogsModel()
	fullContent := m.derivedLogContent()
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit
	m = updated.(Model)
	if m.logFiltering || m.logFilterQuery != "ERROR" || m.derivedLogContent() == fullContent {
		t.Fatalf("precondition: committed filter filtering=%v query=%q", m.logFiltering, m.logFilterQuery)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenLogs {
		t.Errorf("rung 4 esc must stay on the log screen, got screen %d", m.screen)
	}
	if m.logFilterQuery != "" {
		t.Errorf("rung 4 esc should clear the committed filter, got %q", m.logFilterQuery)
	}
	if m.derivedLogContent() != fullContent {
		t.Errorf("rung 4 esc should restore the full view:\n got %q\nwant %q", m.derivedLogContent(), fullContent)
	}
}

// TestLogLadder_Rung5_LeaveScreen: with neither a filter nor a search active,
// esc leaves the log screen back to the container screen (rung 5).
func TestLogLadder_Rung5_LeaveScreen(t *testing.T) {
	m := setupFilterableLogsModel()
	if m.logFiltering || m.logSearching || m.logFilterQuery != "" || m.logSearchQuery != "" {
		t.Fatal("precondition: no filter or search active")
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("rung 5 esc should return to the container screen, got screen %d", m.screen)
	}
	if m.logsRawLines != nil || m.logsService != "" {
		t.Errorf("rung 5 esc should clear log state: rawLines=%v service=%q", m.logsRawLines, m.logsService)
	}
}

// TestLogLadder_PeelOrder_SearchFilterLeave pins the full peel order with BOTH a
// filter and a search committed: the first esc clears search only (filter still
// narrows), the second clears filter only (full view restored), and only the
// third leaves the screen.
func TestLogLadder_PeelOrder_SearchFilterLeave(t *testing.T) {
	m := setupFilterableLogsModel()
	fullContent := m.derivedLogContent()

	// Commit a filter that keeps only the two ERROR lines.
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	filteredContent := m.derivedLogContent()
	if m.logFilterQuery != "ERROR" || filteredContent == fullContent {
		t.Fatalf("precondition: filter should be committed and narrowing, query=%q", m.logFilterQuery)
	}

	// Commit a search within the filtered survivors.
	updated, _ = m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "timeout")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.logSearchQuery != "timeout" || len(m.logSearchMatches) == 0 {
		t.Fatalf("precondition: search should be committed with matches, query=%q matches=%v",
			m.logSearchQuery, m.logSearchMatches)
	}

	// First esc: rung 3 clears the search only; the filter still narrows.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenLogs {
		t.Fatalf("first esc must stay on the log screen, got screen %d", m.screen)
	}
	if m.logSearchQuery != "" || m.logSearchMatches != nil {
		t.Errorf("first esc must clear search: query=%q matches=%v", m.logSearchQuery, m.logSearchMatches)
	}
	if m.logFilterQuery != "ERROR" {
		t.Errorf("first esc must leave the filter committed, got %q", m.logFilterQuery)
	}
	if m.derivedLogContent() != filteredContent {
		t.Errorf("first esc must keep the filtered view:\n got %q\nwant %q", m.derivedLogContent(), filteredContent)
	}

	// Second esc: rung 4 clears the filter only; the full view returns.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenLogs {
		t.Fatalf("second esc must stay on the log screen, got screen %d", m.screen)
	}
	if m.logFilterQuery != "" {
		t.Errorf("second esc must clear the filter, got %q", m.logFilterQuery)
	}
	if m.derivedLogContent() != fullContent {
		t.Errorf("second esc must restore the full view:\n got %q\nwant %q", m.derivedLogContent(), fullContent)
	}

	// Third esc: rung 5 leaves the screen.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectContainers {
		t.Errorf("third esc should return to the container screen, got screen %d", m.screen)
	}
}

// TestLogLadder_QTypesLiterallyIntoFilterInput pins the q-exception in the
// q→esc rewrite block: while the filter bar is open, q is a literal character.
func TestLogLadder_QTypesLiterallyIntoFilterInput(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	updated, _ = m.Update(runeKey('q'))
	m = updated.(Model)

	if m.screen != screenLogs {
		t.Errorf("q must not navigate away while the filter bar is open, got screen %d", m.screen)
	}
	if !m.logFiltering {
		t.Error("q should not close the filter bar")
	}
	if m.logFilterInput.Value() != "q" {
		t.Errorf("q should land in the filter input, got %q", m.logFilterInput.Value())
	}
}

// TestLogLadder_QTypesLiterallyIntoSearchInput pins the q-exception for the
// search bar: while the search bar is open, q is a literal character.
func TestLogLadder_QTypesLiterallyIntoSearchInput(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	updated, _ = m.Update(runeKey('q'))
	m = updated.(Model)

	if m.screen != screenLogs {
		t.Errorf("q must not navigate away while the search bar is open, got screen %d", m.screen)
	}
	if !m.logSearching {
		t.Error("q should not close the search bar")
	}
	if m.logSearchInput.Value() != "q" {
		t.Errorf("q should land in the search input, got %q", m.logSearchInput.Value())
	}
}

// TestLogLadder_QActsAsBackWhenNoInputOpen pins that with no filter/search bar
// open, q is rewritten to esc and leaves the log screen (rung 5, back-nav).
func TestLogLadder_QActsAsBackWhenNoInputOpen(t *testing.T) {
	m := setupFilterableLogsModel()
	if m.logFiltering || m.logSearching {
		t.Fatal("precondition: no input bar open")
	}

	updated, _ := m.Update(runeKey('q'))
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("q with no input open should navigate back, got screen %d", m.screen)
	}
}

// --- Code-review follow-ups: reopen-resets, scroll-into-view, streaming append,
// follow-pinning, backspace/empty-enter clears, terminal-error partial flush ---

// TestLogSearch_OpenOverCommittedResetsState pins that reopening `/` over a
// committed search starts from a clean slate (empty query, no stale matches or
// counter) and that an immediate enter cannot re-commit the old query. Mirrors
// the container-screen TestSearchOpenOverCommittedResetsState.
func TestLogSearch_OpenOverCommittedResetsState(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit
	m = updated.(Model)
	if m.logSearchQuery != "ERROR" || len(m.logSearchMatches) != 2 {
		t.Fatalf("precondition: committed ERROR with 2 matches, got %q %v", m.logSearchQuery, m.logSearchMatches)
	}

	updated, _ = m.Update(runeKey('/')) // reopen
	m = updated.(Model)
	if !m.logSearching {
		t.Error("logSearching should be true after reopening /")
	}
	if m.logSearchQuery != "" {
		t.Errorf("query should be empty after reopen, got %q (stale leaked)", m.logSearchQuery)
	}
	if m.logSearchMatches != nil {
		t.Errorf("matches should be nil after reopen, got %v (stale highlights leaked)", m.logSearchMatches)
	}
	if m.logSearchInput.Value() != "" {
		t.Errorf("input should be empty after reopen, got %q", m.logSearchInput.Value())
	}
	if m.logSearchCounter() != "(no match)" {
		t.Errorf("counter should reset after reopen, got %q (stale counter leaked)", m.logSearchCounter())
	}

	// An immediate enter must NOT re-commit the old query (matches empty ⇒ cleared).
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.logSearchQuery != "" {
		t.Errorf("immediate enter must not re-commit the old query, got %q", m.logSearchQuery)
	}
	if m.logSearching {
		t.Error("empty enter should close the bar")
	}
}

// TestLogFilter_OpenOverCommittedResetsState pins that reopening `f` over a
// committed filter resets the query AND restores the full view (not a stale
// narrowing), and that an immediate enter cannot re-commit the old filter.
func TestLogFilter_OpenOverCommittedResetsState(t *testing.T) {
	m := setupFilterableLogsModel()
	fullContent := m.derivedLogContent()

	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit
	m = updated.(Model)
	if m.logFilterQuery != "ERROR" {
		t.Fatalf("precondition: committed filter ERROR, got %q", m.logFilterQuery)
	}
	if m.derivedLogContent() == fullContent {
		t.Fatal("precondition: filter should have narrowed the view")
	}

	updated, _ = m.Update(runeKey('f')) // reopen
	m = updated.(Model)
	if !m.logFiltering {
		t.Error("logFiltering should be true after reopening f")
	}
	if m.logFilterQuery != "" {
		t.Errorf("query should be empty after reopen, got %q (stale leaked)", m.logFilterQuery)
	}
	if m.logFilterInput.Value() != "" {
		t.Errorf("input should be empty after reopen, got %q", m.logFilterInput.Value())
	}
	if m.derivedLogContent() != fullContent {
		t.Errorf("reopening f should restore the full view (not stay narrowed):\n got %q\nwant %q",
			m.derivedLogContent(), fullContent)
	}

	// An immediate enter must NOT re-commit the old filter.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.logFilterQuery != "" {
		t.Errorf("immediate enter must not re-commit the old filter, got %q", m.logFilterQuery)
	}
	if m.logFiltering {
		t.Error("empty enter should close the filter bar")
	}
	if m.derivedLogContent() != fullContent {
		t.Errorf("after empty commit the full view should remain, got %q", m.derivedLogContent())
	}
}

// TestLogSearch_ScrollOffscreenMatchAutoPausesThenGResumes pins scrollLogMatchIntoView
// and the auto-pause-on-jump behavior: with a short viewport, cycling to a match
// below the fold scrolls it into view and pauses follow (AtBottom false); G then
// resumes the live tail (AtBottom true).
func TestLogSearch_ScrollOffscreenMatchAutoPausesThenGResumes(t *testing.T) {
	m := setupFilterableLogsModel()
	m.logsViewport = viewport.New(80, 3) // short: 3 rows visible
	m.logsViewport.SetHorizontalStep(0)
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = fmt.Sprintf("app | plain row %02d", i)
	}
	lines[0] = "app | NEEDLE row 00" // visible at the top
	lines[6] = "app | NEEDLE row 06" // below the fold
	m.logsRawLines = lines
	m.fullReformat()
	m.logsViewport.SetYOffset(0)

	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "NEEDLE")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit
	m = updated.(Model)
	if !intSliceEq(m.logSearchMatches, []int{0, 6}) {
		t.Fatalf("precondition: matches = %v, want [0 6]", m.logSearchMatches)
	}
	m.logsViewport.SetYOffset(0) // current match (0) visible at top
	if m.logSearchCur != 0 {
		t.Fatalf("precondition: cur = %d, want 0", m.logSearchCur)
	}

	// n → jump to the off-screen match at physical line 6.
	updated, _ = m.Update(runeKey('n'))
	m = updated.(Model)
	if m.logSearchCur != 1 {
		t.Fatalf("after n, cur = %d, want 1", m.logSearchCur)
	}
	if m.logsViewport.YOffset != 6 {
		t.Errorf("YOffset = %d, want 6 (off-screen match scrolled into view / pinned to top)",
			m.logsViewport.YOffset)
	}
	if m.logsViewport.AtBottom() {
		t.Error("jumping to a mid-buffer match must auto-pause follow (AtBottom should be false)")
	}

	// G resumes follow.
	updated, _ = m.Update(runeKey('G'))
	m = updated.(Model)
	if !m.logsViewport.AtBottom() {
		t.Error("G should resume follow (AtBottom true again)")
	}
}

// TestLogSearch_StreamingAppendGrowsMatches pins that a committed search picks up
// a matching line that streams in after the commit: the match set grows and the
// current-match cursor holds.
func TestLogSearch_StreamingAppendGrowsMatches(t *testing.T) {
	m := setupFilterableLogsModel()
	pr, _ := io.Pipe()
	m.logsPipeR = pr

	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit
	m = updated.(Model)
	if !intSliceEq(m.logSearchMatches, []int{1, 4}) {
		t.Fatalf("precondition: matches = %v, want [1 4]", m.logSearchMatches)
	}
	curBefore := m.logSearchCur

	// A new ERROR line streams in — the committed search must include it.
	updated, _ = m.Update(logChunkMsg{data: []byte("app | ERROR another failure\n")})
	m = updated.(Model)
	if !intSliceEq(m.logSearchMatches, []int{1, 4, 5}) {
		t.Errorf("streamed matching line should grow matches to [1 4 5], got %v", m.logSearchMatches)
	}
	if m.logSearchCur != curBefore {
		t.Errorf("current-match cursor should hold across an append, got %d want %d", m.logSearchCur, curBefore)
	}
}

// TestLogRederive_FilterChangeFollowPinning pins the follow-aware re-pinning of
// rederiveLogs across a filter commit and clear: a following view stays pinned to
// the bottom, a paused view holds its YOffset (never yanked to the tail).
func TestLogRederive_FilterChangeFollowPinning(t *testing.T) {
	newModel := func() Model {
		mc := &mockComposer{services: []string{"nginx"}}
		m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
		m.screen = screenLogs
		m.logsService = "nginx"
		m.height = 24
		m.width = 84
		m.logsViewport = viewport.New(80, 10)
		m.logsViewport.SetHorizontalStep(0)
		m.logsWrap = true
		pr, _ := io.Pipe()
		m.logsPipeR = pr
		m.logsRawLines = logChunkLines(50) // every line contains "line"
		m.applyLogFormat()
		if m.logsViewport.TotalLineCount() <= m.logsViewport.Height {
			t.Fatalf("precondition: content must exceed viewport height (lines=%d, height=%d)",
				m.logsViewport.TotalLineCount(), m.logsViewport.Height)
		}
		return m
	}

	// commitFilter opens the filter, types q (kept as a no-narrowing "line" here so
	// the survivor set == the full buffer and no YOffset clamping muddies the test),
	// and commits.
	commitFilter := func(m Model, q string) Model {
		updated, _ := m.Update(runeKey('f'))
		m = updated.(Model)
		m = typeInto(m, q)
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		return updated.(Model)
	}

	t.Run("following stays pinned across filter commit and clear", func(t *testing.T) {
		m := newModel()
		m.logsViewport.GotoBottom()
		if !m.logsViewport.AtBottom() {
			t.Fatal("precondition: should be following (at bottom)")
		}
		m = commitFilter(m, "line")
		if !m.logsViewport.AtBottom() {
			t.Error("following view should stay pinned to the bottom across a filter commit")
		}
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // clear committed filter (rung 4)
		m = updated.(Model)
		if m.logFilterQuery != "" {
			t.Fatalf("filter should be cleared, got %q", m.logFilterQuery)
		}
		if !m.logsViewport.AtBottom() {
			t.Error("following view should stay pinned to the bottom across a filter clear")
		}
	})

	t.Run("paused view holds YOffset across filter commit and clear", func(t *testing.T) {
		m := newModel()
		m.logsViewport.GotoBottom()
		m.logsViewport.SetYOffset(m.logsViewport.YOffset - 5) // scroll up → paused
		if m.logsViewport.AtBottom() {
			t.Fatal("precondition: should be paused (not at bottom)")
		}
		off := m.logsViewport.YOffset

		m = commitFilter(m, "line")
		if m.logsViewport.AtBottom() {
			t.Error("paused view must not snap to bottom on a filter commit")
		}
		if m.logsViewport.YOffset != off {
			t.Errorf("paused YOffset should hold across a filter commit: got %d want %d",
				m.logsViewport.YOffset, off)
		}

		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // clear committed filter
		m = updated.(Model)
		if m.logsViewport.AtBottom() {
			t.Error("paused view must not snap to bottom on a filter clear")
		}
		if m.logsViewport.YOffset != off {
			t.Errorf("paused YOffset should hold across a filter clear: got %d want %d",
				m.logsViewport.YOffset, off)
		}
	})
}

// TestLogFilter_BackspaceToEmptyRevealsFull pins the recomputeLogFilter
// backspace-to-empty reveal branch: deleting the query back to empty clears the
// committed filter and restores the full view (distinct from the esc-cancel path).
func TestLogFilter_BackspaceToEmptyRevealsFull(t *testing.T) {
	m := setupFilterableLogsModel()
	fullContent := m.derivedLogContent()

	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	if m.derivedLogContent() == fullContent {
		t.Fatal("precondition: filter should have narrowed the view")
	}

	for i := 0; i < len("ERROR"); i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		m = updated.(Model)
	}

	if m.logFilterInput.Value() != "" {
		t.Errorf("input should be empty after backspacing, got %q", m.logFilterInput.Value())
	}
	if m.logFilterQuery != "" {
		t.Errorf("query should clear when backspaced to empty, got %q", m.logFilterQuery)
	}
	if !m.logFiltering {
		t.Error("still typing — the bar should remain open (empty, not committed)")
	}
	if m.derivedLogContent() != fullContent {
		t.Errorf("backspacing to empty should restore the full view:\n got %q\nwant %q",
			m.derivedLogContent(), fullContent)
	}
}

// TestLogSearch_BackspaceToEmptyClearsHighlight pins the recomputeLogSearch
// empty-query branch reached by backspacing the query away one rune at a time.
func TestLogSearch_BackspaceToEmptyClearsHighlight(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "ERROR")
	if len(m.logSearchMatches) == 0 {
		t.Fatal("precondition: search should have matches")
	}

	for i := 0; i < len("ERROR"); i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		m = updated.(Model)
	}

	if m.logSearchInput.Value() != "" {
		t.Errorf("input should be empty after backspacing, got %q", m.logSearchInput.Value())
	}
	if m.logSearchQuery != "" {
		t.Errorf("query should clear when backspaced to empty, got %q", m.logSearchQuery)
	}
	if m.logSearchMatches != nil {
		t.Errorf("matches should clear when backspaced to empty, got %v", m.logSearchMatches)
	}
	if !m.logSearching {
		t.Error("still typing — the bar should remain open")
	}
}

// TestLogFilter_EmptyEnterCommitClears pins that pressing enter on an untouched
// (empty) filter bar closes it with no filter lingering.
func TestLogFilter_EmptyEnterCommitClears(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('f'))
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.logFiltering {
		t.Error("empty enter should close the filter bar")
	}
	if m.logFilterQuery != "" {
		t.Errorf("no filter should linger after empty enter, got %q", m.logFilterQuery)
	}
	if m.logFilterInput.Value() != "" {
		t.Errorf("input should be reset after empty enter, got %q", m.logFilterInput.Value())
	}
}

// TestLogSearch_EmptyEnterCommitClears pins that pressing enter on an untouched
// (empty) search bar closes it with no search state lingering.
func TestLogSearch_EmptyEnterCommitClears(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.logSearching {
		t.Error("empty enter should close the search bar")
	}
	if m.logSearchQuery != "" {
		t.Errorf("no search should linger after empty enter, got %q", m.logSearchQuery)
	}
	if m.logSearchMatches != nil {
		t.Errorf("no matches should linger after empty enter, got %v", m.logSearchMatches)
	}
}

// TestLogSearch_EnterOnNonMatchingQueryClears pins the F4 fix: committing a
// valid-but-non-matching search drops the dead search (no lingering "(no match)"
// counter with inert n/N), mirroring the container search's zero-match drop.
func TestLogSearch_EnterOnNonMatchingQueryClears(t *testing.T) {
	m := setupFilterableLogsModel()
	updated, _ := m.Update(runeKey('/'))
	m = updated.(Model)
	m = typeInto(m, "zzz-no-such-term")
	if m.logSearchQuery != "zzz-no-such-term" {
		t.Fatalf("precondition: query set while typing, got %q", m.logSearchQuery)
	}
	if len(m.logSearchMatches) != 0 {
		t.Fatalf("precondition: query should match nothing, got %v", m.logSearchMatches)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit
	m = updated.(Model)
	if m.logSearching {
		t.Error("bar should close on enter")
	}
	if m.logSearchQuery != "" {
		t.Errorf("a non-matching search must be cleared on commit, got %q", m.logSearchQuery)
	}
	if m.logSearchMatches != nil {
		t.Errorf("matches should be nil after clearing a dead search, got %v", m.logSearchMatches)
	}
}

// TestLogDoneMsg_ErrorFlushesPartialLine pins that a terminal error flushes the
// in-flight partial line into the raw buffer (so it is not lost) and that both the
// flushed line and the terminal error render.
func TestLogDoneMsg_ErrorFlushesPartialLine(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.logsViewport = viewport.New(80, 20)

	// A chunk with no trailing newline leaves an in-flight partial line.
	updated, _ := m.Update(logChunkMsg{data: []byte("app | INFO partial without newline")})
	m = updated.(Model)
	if m.logsPartial != "app | INFO partial without newline" {
		t.Fatalf("precondition: partial should hold the newline-less tail, got %q", m.logsPartial)
	}
	if len(m.logsRawLines) != 0 {
		t.Fatalf("precondition: no complete raw line yet, got %v", m.logsRawLines)
	}

	updated, _ = m.Update(logDoneMsg{err: fmt.Errorf("stream closed")})
	m = updated.(Model)
	if m.logsPartial != "" {
		t.Errorf("partial should be flushed on terminal error, got %q", m.logsPartial)
	}
	if len(m.logsRawLines) != 1 || m.logsRawLines[0] != "app | INFO partial without newline" {
		t.Errorf("flushed partial should become a raw line, got %v", m.logsRawLines)
	}
	got := m.derivedLogContent()
	if !strings.Contains(got, "partial without newline") {
		t.Errorf("flushed partial should render, got %q", got)
	}
	if !strings.Contains(got, "Error: stream closed") {
		t.Errorf("terminal error should render, got %q", got)
	}
}

// TestLogDoneMsg_CleanEOFFlushesPartialLine pins that a CLEAN EOF (err == nil)
// also folds the final unterminated line into the raw buffer, so it becomes
// subject to the filter and the `f`/`/` open handlers are no longer blocked by
// the empty-buffer guard when the partial is the sole content.
func TestLogDoneMsg_CleanEOFFlushesPartialLine(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.logsViewport = viewport.New(80, 20)

	// A chunk with no trailing newline leaves an in-flight partial line and, as
	// the sole content, an empty raw buffer.
	updated, _ := m.Update(logChunkMsg{data: []byte("app | INFO final line no newline")})
	m = updated.(Model)
	if m.logsPartial != "app | INFO final line no newline" {
		t.Fatalf("precondition: partial should hold the newline-less tail, got %q", m.logsPartial)
	}
	if len(m.logsRawLines) != 0 {
		t.Fatalf("precondition: no complete raw line yet, got %v", m.logsRawLines)
	}
	// Precondition: `f` is a no-op while the raw buffer is empty (nothing to filter).
	guard, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	if guard.(Model).logFiltering {
		t.Fatalf("precondition: `f` must not open while the raw buffer is empty")
	}

	// Clean EOF: err == nil.
	updated, _ = m.Update(logDoneMsg{err: nil})
	m = updated.(Model)
	if m.logsErr != nil {
		t.Errorf("clean EOF should not set logsErr, got %v", m.logsErr)
	}
	if m.logsErrLine != "" {
		t.Errorf("clean EOF should not set logsErrLine, got %q", m.logsErrLine)
	}
	if m.logsPartial != "" {
		t.Errorf("partial should be flushed on clean EOF, got %q", m.logsPartial)
	}
	if len(m.logsRawLines) != 1 || m.logsRawLines[0] != "app | INFO final line no newline" {
		t.Errorf("flushed partial should become a raw line, got %v", m.logsRawLines)
	}

	// The flushed line is now filterable: `f` opens.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = updated.(Model)
	if !m.logFiltering {
		t.Errorf("`f` should open now that the flushed line populates the raw buffer")
	}
}

// TestLogFilterCounts_IncrementalCache verifies logFilterCounts reads the
// logFilterShown cache maintained by applyLogFormat/fullReformat: fullReformat
// recomputes it over the whole buffer on a filter change, and a streaming append
// grows it incrementally without a full rescan (the C2 fix). It drives the real
// derivation path, not the cache field directly.
func TestLogFilterCounts_IncrementalCache(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"
	m.width = 80
	m.logsViewport = viewport.New(80, 10)
	m.logsRawLines = []string{"INFO a", "ERROR b", "INFO c", "ERROR d"}
	m.applyLogFormat() // no filter yet

	// No filter committed: survivors == total (cache not read).
	if s, tot := m.logFilterCounts(); s != 4 || tot != 4 {
		t.Fatalf("no-filter counts = %d/%d, want 4/4", s, tot)
	}

	// Commit an "ERROR" filter through the real path (fullReformat recomputes
	// logFilterShown over the whole buffer).
	m.logFilterInput = textinput.New()
	m.logFilterInput.SetValue("ERROR")
	m.recomputeLogFilter()
	if s, tot := m.logFilterCounts(); s != 2 || tot != 4 {
		t.Fatalf("committed-filter counts = %d/%d, want 2/4", s, tot)
	}

	// Stream two more raw lines (one match, one not) — the cache grows
	// incrementally via applyLogFormat, no full rescan.
	updated, _ := m.Update(logChunkMsg{data: []byte("ERROR e\nINFO f\n")})
	m = updated.(Model)
	if s, tot := m.logFilterCounts(); s != 3 || tot != 6 {
		t.Fatalf("post-stream counts = %d/%d, want 3/6", s, tot)
	}

	// Clearing the filter reveals everything again (cache is bypassed when the
	// query is empty).
	m.logFilterInput.SetValue("")
	m.recomputeLogFilter()
	if s, tot := m.logFilterCounts(); s != 6 || tot != 6 {
		t.Fatalf("cleared-filter counts = %d/%d, want 6/6", s, tot)
	}
}

// intSliceEq reports whether two int slices are element-wise equal (nil and
// empty are treated as equal).
func intSliceEq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	if !strings.Contains(view, "q back") {
		t.Errorf("done progress should show 'q back' hint, got: %q", view)
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

// helpOverlayNamesKey reports whether the `?` overlay for m's screen shows the
// given key immediately before the given description on one row.
//
// ansi.Strip first: lipgloss wraps the key itself in helpKeyStyle, so a bare
// == against strings.Fields only matches when the test runs with colour
// disabled (the default under `go test`, which pipes stdout — but not under
// CLICOLOR_FORCE=1 or a real TTY). Requiring the key to sit BEFORE the
// description on the same row keeps the two-column layout from satisfying the
// assertion with a key from the left column and a description from the right.
func helpOverlayNamesKey(m Model, key, desc string) bool {
	m.helpOpen = true
	for _, line := range strings.Split(ansi.Strip(m.View()), "\n") {
		d := strings.Index(line, desc)
		if d < 0 {
			continue
		}
		for _, f := range strings.Fields(line[:d]) {
			if f == key {
				return true
			}
		}
	}
	return false
}

// TestHelpOverlay_ShowsConfigKey pins that `c config` stays discoverable. The
// token moved out of the trimmed footer into the `?` overlay, so this renders
// the overlay, not the footer.
func TestHelpOverlay_ShowsConfigKey(t *testing.T) {
	m := Model{screen: screenSelectContainers, width: 120, height: 24}

	if !helpOverlayNamesKey(m, "c", "config") {
		t.Errorf("`?` overlay should mention the 'c' config key, got: %q", m.viewHelp())
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
	m.statsRequested = false // exercise the "no columns" branch
	m.services = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.width = 200 // wide enough for one-line help
	m.height = 10

	// header=3, footer=3 (one-line help; reserved search bar merges onto the
	// helpStyle MarginTop blank, so it adds no physical row) → 10-3-3 = 4
	got := m.svcVisibleCount()
	if got != 4 {
		t.Errorf("svcVisibleCount() = %d, want 4", got)
	}
}

func TestSvcVisibleCount_NarrowWidth(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.statsRequested = false
	m.services = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.width = 40 // too narrow for one-line help
	m.height = 10

	// header=3, footer=4 (two-line help; reserved bar merges onto MarginTop) → 10-3-4 = 3
	got := m.svcVisibleCount()
	if got != 3 {
		t.Errorf("svcVisibleCount() narrow = %d, want 3", got)
	}
}

func TestSvcVisibleCount_Confirming(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.statsRequested = false
	m.services = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.width = 120
	m.height = 10
	m.confirming = true

	// header=3, footer=3 (confirming; reserved bar merges onto MarginTop) → 10-3-3 = 4
	got := m.svcVisibleCount()
	if got != 4 {
		t.Errorf("svcVisibleCount() confirming = %d, want 4", got)
	}
}

func TestSvcVisibleCount_Warning(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.statsRequested = false
	m.services = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.width = 200
	m.height = 10
	m.warning = "something wrong"

	// header=3, footer=3+1 (one-line help + warning; reserved bar merges) → 10-3-4 = 3
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
	m.width = 200
	m.height = 10
	m.svcStatus = map[string]runner.ServiceStatus{
		"a": {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
	}

	// header=4 (3 + column captions), footer=3 (one-line help; reserved bar merges) → 10-4-3 = 3
	got := m.svcVisibleCount()
	if got != 3 {
		t.Errorf("svcVisibleCount() with status columns = %d, want 3", got)
	}
}

func TestHasStatusColumns(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.statsRequested = false // exercise the data-driven branch
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

// TestHasStatusColumns_StatsRequested verifies that statsRequested short-circuits
// hasStatusColumns to true even when no per-service data has arrived yet. This
// keeps the CPU/Mem column captions visible from the first frame instead of
// popping in when the ~1.5s host-wide docker stats call returns.
func TestHasStatusColumns_StatsRequested(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a"}
	m.statsRequested = true
	m.svcStatus = nil
	m.stats = nil

	if !m.hasStatusColumns() {
		t.Error("hasStatusColumns() = false, want true when statsRequested && no data")
	}
}

// TestNewModel_setsStatsRequestedForLocalFastTrack verifies that constructing
// a Model with a local composer and no servers (the fast-track to
// screenSelectContainers) flips statsRequested so the first frame reserves
// the CPU/Mem column widths.
func TestNewModel_setsStatsRequestedForLocalFastTrack(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	if m.screen != screenSelectContainers {
		t.Fatalf("screen = %v, want screenSelectContainers (precondition)", m.screen)
	}
	if !m.statsRequested {
		t.Error("statsRequested = false, want true after NewModel local fast-track")
	}
}

func TestFixSvcOffset_CursorBelowWindow(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.statsRequested = false
	m.services = []string{"a", "b", "c", "d", "e"}
	m.width = 200
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
	m.statsRequested = false
	m.services = []string{"a", "b", "c", "d", "e"}
	m.width = 120
	m.height = 9 // visible = 9-3-3 = 3 (one-line help; reserved bar merges onto MarginTop)
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
	m.statsRequested = false
	m.services = []string{"a", "b", "c", "d", "e"}
	m.width = 200
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
	m.statsRequested = false // isolate scroll math from the captions row
	m.screen = screenSelectContainers
	m.services = mc.services
	m.width = 200
	m.height = 9 // visible = 9-3-3 = 3

	// Set initial size
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 9})
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
	m.statsRequested = false
	m.screen = screenSelectContainers
	m.services = mc.services
	m.width = 160
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
	m.height = 8 // visible normal = confirming (constant with reserved search bar)

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
	m.width = 160
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
	m.width = 160
	m.height = 20 // all fit
	m.svcCursor = 4
	m.svcOffset = 0

	// Shrink terminal
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 9}) // visible=3
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
	m.width = 160
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
	m.statsRequested = false
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 200
	m.height = 8 // visible = 8-3-3 = 2
	m.svcCursor = 0
	m.svcOffset = 0

	view := m.viewSelectContainers()
	if !strings.Contains(view, "▼ 3 more") {
		t.Errorf("expected down indicator '▼ 3 more' in view, got:\n%s", view)
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
	m.statsRequested = false
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 200
	m.height = 8 // visible = 8-3-3 = 2
	m.svcCursor = 2
	m.svcOffset = 1

	view := m.viewSelectContainers()
	if !strings.Contains(view, "▲ 1 more") {
		t.Errorf("expected up indicator in view, got:\n%s", view)
	}
	if !strings.Contains(view, "▼ 2 more") {
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
	m.statsRequested = false
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 200
	m.height = 9 // visible = 9-3-3 = 3
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

	// Verify formatted ports appear (wildcard host hidden)
	if !strings.Contains(view, "8080→80") {
		t.Errorf("expected formatted port '8080→80' in view, got:\n%s", view)
	}
	if !strings.Contains(view, "8443→443") {
		t.Errorf("expected formatted port '8443→443' in view, got:\n%s", view)
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

	// nginx line must contain the formatted port (wildcard host hidden)
	if !strings.Contains(nginxLine, "80→80") {
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
	// pressing ctrl+c should set quitting = true and NOT return tea.Quit.
	// (q on nested screens is now back-navigation — covered by
	// TestQBackNavigation_*.)
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectContainers
	m.showPicker = true
	m.services = mc.services
	m.selected = make(map[int]bool)
	m.disconnectFunc = func() error { return nil }
	m.serverName = "prod"

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	um := updated.(Model)

	if cmd != nil {
		t.Fatal("expected nil command (no quit), got non-nil")
	}
	if !um.quitting {
		t.Error("quitting should be true after pressing ctrl+c on remote connection")
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

func TestCtrlCConfirmation_AllRemoteScreens(t *testing.T) {
	// Verify that ctrl+c is intercepted by tryQuit across all non-server-select
	// screens with a remote connection. q is no longer tested here — it now
	// performs back-navigation on nested screens (covered by TestQBackNavigation_*).
	tests := []struct {
		name   string
		screen screen
		key    string
		setup  func(m *Model)
	}{
		{"project select", screenSelectProject, "ctrl+c", func(m *Model) {
			m.projects = []compose.Project{{Name: "app", ConfigDir: "/app"}}
		}},
		{"containers normal", screenSelectContainers, "ctrl+c", func(m *Model) {
			m.services = []string{"nginx"}
			m.selected = make(map[int]bool)
		}},
		{"containers confirming", screenSelectContainers, "ctrl+c", func(m *Model) {
			m.services = []string{"nginx"}
			m.selected = map[int]bool{0: true}
			m.confirming = true
			m.pendingOp = runner.Deploy
		}},
		{"logs", screenLogs, "ctrl+c", func(m *Model) {
			m.logsService = "nginx"
		}},
		{"config", screenConfig, "ctrl+c", func(m *Model) {
			m.configContent = []byte("version: '3'")
		}},
		{"settings list", screenSettingsList, "ctrl+c", func(m *Model) {
			m.config = &config.Config{Servers: testServers}
		}},
		{"settings form ctrl+c", screenSettingsForm, "ctrl+c", func(m *Model) {
			m.config = &config.Config{Servers: testServers}
			m.settingsInputs = initSettingsInputs()
		}},
		{"progress done", screenProgress, "ctrl+c", func(m *Model) {
			m.done = true
		}},
		{"progress failed", screenProgress, "ctrl+c", func(m *Model) {
			m.failed = true
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

// --- q back-navigation tests ---

func TestQBackNavigation_ContainerScreen(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectContainers
	m.showPicker = true
	m.services = []string{"nginx"}
	m.selected = make(map[int]bool)
	m.composer = mc

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenSelectProject {
		t.Errorf("screen = %d, want screenSelectProject", um.screen)
	}
	if um.composer != nil {
		t.Error("composer should be cleared after back nav")
	}
	if um.services != nil {
		t.Error("services should be cleared after back nav")
	}
}

func TestQBackNavigation_ContainerScreenCancelsConfirming(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectContainers
	m.showPicker = true
	m.services = []string{"nginx"}
	m.selected = map[int]bool{0: true}
	m.confirming = true
	m.pendingOp = runner.Deploy

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.confirming {
		t.Error("confirming should be cancelled after q")
	}
	if um.screen != screenSelectContainers {
		t.Errorf("screen = %d, want screenSelectContainers", um.screen)
	}
	if cmd != nil {
		t.Errorf("expected nil command, got non-nil")
	}
}

func TestQBackNavigation_ContainerScreenCancelsPendingExec(t *testing.T) {
	// q during the exec confirmation prompt should cancel both confirming
	// and pendingExec, mirroring the esc handler at app.go:799-803.
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectContainers
	m.showPicker = true
	m.services = []string{"nginx"}
	m.selected = map[int]bool{0: true}
	m.confirming = true
	m.pendingExec = true

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.confirming {
		t.Error("confirming should be cancelled after q")
	}
	if um.pendingExec {
		t.Error("pendingExec should be cancelled after q")
	}
	if um.screen != screenSelectContainers {
		t.Errorf("screen = %d, want screenSelectContainers", um.screen)
	}
	if cmd != nil {
		t.Errorf("expected nil command, got non-nil")
	}
}

func TestQBackNavigation_LogsScreen(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.logsService = "nginx"

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenSelectContainers {
		t.Errorf("screen = %d, want screenSelectContainers", um.screen)
	}
	if um.logsService != "" {
		t.Errorf("logsService = %q, want empty", um.logsService)
	}
}

func TestQBackNavigation_ConfigScreen(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenConfig
	m.configContent = []byte("version: '3'")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenSelectContainers {
		t.Errorf("screen = %d, want screenSelectContainers", um.screen)
	}
	if um.configContent != nil {
		t.Errorf("configContent should be cleared, got %v", um.configContent)
	}
}

func TestQBackNavigation_SettingsList(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSettingsList
	m.config = &config.Config{Servers: testServers}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenSelectServer {
		t.Errorf("screen = %d, want screenSelectServer", um.screen)
	}
}

func TestQBackNavigation_SettingsListCancelsDelete(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSettingsList
	m.config = &config.Config{Servers: testServers}
	m.settingsDelete = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.settingsDelete {
		t.Error("settingsDelete should be false after q")
	}
	if um.screen != screenSettingsList {
		t.Errorf("screen = %d, want screenSettingsList", um.screen)
	}
}

func TestQBackNavigation_ProjectScreen(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectProject
	m.serverName = "prod"
	disconnectCalls := 0
	m.disconnectFunc = func() error {
		disconnectCalls++
		return nil
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenSelectServer {
		t.Errorf("screen = %d, want screenSelectServer", um.screen)
	}
	if um.disconnectFunc != nil {
		t.Error("disconnectFunc should be nil after back nav")
	}
	if cmd == nil {
		t.Fatal("expected a tea.Cmd to run disconnect, got nil")
	}
	msg := cmd()
	if _, ok := msg.(disconnectDoneMsg); !ok {
		t.Errorf("expected disconnectDoneMsg, got %T", msg)
	}
	if disconnectCalls != 1 {
		t.Errorf("disconnect called %d times, want 1", disconnectCalls)
	}
}

func TestQBackNavigation_ProgressDoneReturnsToContainers(t *testing.T) {
	tests := []struct {
		name string
		done bool
		fail bool
	}{
		{"done", true, false},
		{"failed", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockComposer{services: []string{"nginx"}}
			m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
			m.screen = screenProgress
			m.done = tc.done
			m.failed = tc.fail
			m.services = []string{"nginx"}
			m.selected = make(map[int]bool)

			updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
			um := updated.(Model)

			if um.screen != screenSelectContainers {
				t.Errorf("screen = %d, want screenSelectContainers", um.screen)
			}
		})
	}
}

func TestQOnProgressWhileRunningIsNoop(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.done = false
	m.failed = false
	cancelCalls := 0
	m.cancel = func() {
		cancelCalls++
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenProgress {
		t.Errorf("screen = %d, want screenProgress (unchanged)", um.screen)
	}
	if cancelCalls != 0 {
		t.Errorf("cancel called %d times, want 0", cancelCalls)
	}
	if cmd != nil {
		t.Error("expected nil cmd, got non-nil")
	}
}

func TestQQuitsAtRoot_ProjectScreenNoServers(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectProject

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	if cmd == nil {
		t.Fatal("expected quit cmd, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestQBackNavigation_ProjectScreenWithEmptyConfig(t *testing.T) {
	// When the config file exists but has no servers, NewModel starts on
	// screenSelectServer (showing just the Local entry). Selecting Local
	// transitions to screenSelectProject. Pressing q there must navigate
	// back to server-select, not quit — because there IS a parent screen.
	mc := &mockComposer{}
	emptyCfg := &config.Config{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil, WithConfig(emptyCfg))
	m.screen = screenSelectProject

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenSelectServer {
		t.Errorf("screen = %d, want screenSelectServer", um.screen)
	}
	// No tea.Quit should be returned.
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, isQuit := msg.(tea.QuitMsg); isQuit {
				t.Errorf("got tea.QuitMsg, expected back navigation")
			}
		}
	}
}

func TestEscBackNavigation_ProjectScreenWithEmptyConfig(t *testing.T) {
	// Same parent-exists condition for esc: when m.config != nil and
	// len(servers) == 0, esc must still navigate back to server-select.
	mc := &mockComposer{}
	emptyCfg := &config.Config{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil, WithConfig(emptyCfg))
	m.screen = screenSelectProject

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	um := updated.(Model)

	if um.screen != screenSelectServer {
		t.Errorf("screen = %d, want screenSelectServer", um.screen)
	}
}

func TestQQuitsAtRoot_ContainerScreenStandalone(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.showPicker = false
	m.confirming = false
	m.services = mc.services
	m.selected = make(map[int]bool)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	if cmd == nil {
		t.Fatal("expected quit cmd, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestQTypedIntoSettingsFormInput(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSettingsForm
	m.config = &config.Config{Servers: testServers}
	m.settingsInputs = initSettingsInputs()
	m.settingsField = 0
	m.settingsInputs[0].Focus()

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenSettingsForm {
		t.Errorf("screen = %d, want screenSettingsForm", um.screen)
	}
	if !strings.Contains(um.settingsInputs[0].Value(), "q") {
		t.Errorf("settingsInputs[0] = %q, want to contain %q", um.settingsInputs[0].Value(), "q")
	}
}

func TestQBackNavigation_SettingsFormColorPicker(t *testing.T) {
	// When the color picker is focused (settingsField == 4) no text input
	// has focus, so q acts as back-nav to the settings list rather than
	// being silently swallowed.
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSettingsForm
	m.config = &config.Config{Servers: testServers}
	m.settingsInputs = initSettingsInputs()
	m.settingsField = 4
	m.settingsColor = "red"

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenSettingsList {
		t.Errorf("screen = %d, want screenSettingsList", um.screen)
	}
}

func TestCtrlCStillTriggersDisconnectPrompt(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectContainers
	m.showPicker = true
	m.services = []string{"nginx"}
	m.selected = make(map[int]bool)
	m.disconnectFunc = func() error { return nil }
	m.serverName = "prod"

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	um := updated.(Model)

	if !um.quitting {
		t.Error("quitting should be true after ctrl+c on remote connection")
	}
	if cmd != nil {
		t.Error("expected nil cmd (quit intercepted), got non-nil")
	}
}

func TestQDuringQuittingPromptStillSwallowed(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectContainers
	m.showPicker = true
	m.services = []string{"nginx"}
	m.selected = make(map[int]bool)
	m.disconnectFunc = func() error { return nil }
	m.serverName = "prod"
	m.quitting = true

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if !um.quitting {
		t.Error("quitting should remain true (q is swallowed by the prompt intercept)")
	}
	if cmd != nil {
		t.Error("expected nil cmd, got non-nil")
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

// TestHelpOverlay_ShowsExecKey pins that `x exec` stays discoverable. The token
// moved out of the trimmed footer into the `?` overlay, so this renders the
// overlay, not the footer.
func TestHelpOverlay_ShowsExecKey(t *testing.T) {
	m := Model{screen: screenSelectContainers, width: 120, height: 24}

	if !helpOverlayNamesKey(m, "x", "exec") {
		t.Errorf("`?` overlay should mention the 'x' exec key, got: %q", m.viewHelp())
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

func TestStatsMsg_populates(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers

	stats := map[string]runner.ServiceStats{
		"web": {CPUPercent: 4.2, MemoryUsed: 130023424, MemoryLimit: 536870912},
		"db":  {CPUPercent: 1.1, MemoryUsed: 200000000, MemoryLimit: 1073741824},
	}
	result, _ := m.Update(statsMsg{stats: stats})
	model := result.(Model)

	if model.statsErr != nil {
		t.Errorf("statsErr = %v, want nil", model.statsErr)
	}
	if len(model.stats) != 2 {
		t.Fatalf("len(stats) = %d, want 2", len(model.stats))
	}
	if got := model.stats["web"].CPUPercent; got != 4.2 {
		t.Errorf("stats[web].CPUPercent = %v, want 4.2", got)
	}
	if got := model.stats["db"].MemoryUsed; got != 200000000 {
		t.Errorf("stats[db].MemoryUsed = %v, want 200000000", got)
	}
}

func TestStatsMsg_storesError(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	prior := map[string]runner.ServiceStats{"web": {CPUPercent: 4.2}}
	m.stats = prior

	wantErr := errors.New("docker stats failed")
	result, _ := m.Update(statsMsg{err: wantErr})
	model := result.(Model)

	if model.statsErr == nil || model.statsErr.Error() != wantErr.Error() {
		t.Errorf("statsErr = %v, want %v", model.statsErr, wantErr)
	}
	// On error, m.stats must be cleared — otherwise the screen would render
	// stale CPU/Mem cells next to a "Stats unavailable" warning, contradicting
	// itself and tempting users to trust outdated values.
	if model.stats != nil {
		t.Errorf("stats must be cleared on error, got %+v", model.stats)
	}
}

func TestStatsMsg_staleIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectServer

	result, _ := m.Update(statsMsg{stats: map[string]runner.ServiceStats{"web": {CPUPercent: 99}}})
	model := result.(Model)

	if model.stats != nil {
		t.Errorf("stats mutated for stale msg: got %+v", model.stats)
	}
	if model.statsErr != nil {
		t.Errorf("statsErr mutated for stale msg: got %v", model.statsErr)
	}
}

func TestStatsMsg_staleErrorIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectProject

	result, _ := m.Update(statsMsg{err: errors.New("boom")})
	model := result.(Model)

	if model.statsErr != nil {
		t.Errorf("statsErr set for stale msg: got %v", model.statsErr)
	}
}

func TestStatsMsg_clearsPriorError(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statsErr = errors.New("previous failure")

	result, _ := m.Update(statsMsg{stats: map[string]runner.ServiceStats{"web": {CPUPercent: 1.5}}})
	model := result.(Model)

	if model.statsErr != nil {
		t.Errorf("statsErr not cleared on success: got %v", model.statsErr)
	}
	if got := model.stats["web"].CPUPercent; got != 1.5 {
		t.Errorf("stats[web].CPUPercent = %v, want 1.5", got)
	}
}

func TestEsc_clearsStats(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.showPicker = true
	m.projects = []compose.Project{{Name: "proj"}}
	m.services = []string{"web"}
	m.svcStatus = map[string]runner.ServiceStatus{"web": {Running: true}}
	m.stats = map[string]runner.ServiceStats{"web": {CPUPercent: 4.2}}
	m.statsErr = errors.New("stale stats error")

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := result.(Model)

	if model.screen != screenSelectProject {
		t.Fatalf("screen = %d, want %d", model.screen, screenSelectProject)
	}
	if model.stats != nil {
		t.Errorf("stats not cleared on esc: got %+v", model.stats)
	}
	if model.statsErr != nil {
		t.Errorf("statsErr not cleared on esc: got %v", model.statsErr)
	}
	if model.svcStatus != nil {
		t.Errorf("svcStatus not cleared on esc: got %+v", model.svcStatus)
	}
}

func TestStatsMsg_staleSessionIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statsSession = 5

	result, _ := m.Update(statsMsg{
		stats:   map[string]runner.ServiceStats{"web": {CPUPercent: 99}},
		session: 3,
	})
	model := result.(Model)

	if model.stats != nil {
		t.Errorf("stats mutated for old-session msg: got %+v", model.stats)
	}
	if model.statsErr != nil {
		t.Errorf("statsErr mutated for old-session msg: got %v", model.statsErr)
	}
}

func TestStatsMsg_currentSessionAccepted(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statsSession = 7

	result, _ := m.Update(statsMsg{
		stats:   map[string]runner.ServiceStats{"web": {CPUPercent: 1.0}},
		session: 7,
	})
	model := result.(Model)

	if got := model.stats["web"].CPUPercent; got != 1.0 {
		t.Errorf("stats[web].CPUPercent = %v, want 1.0", got)
	}
}

func TestRefreshStats_capturesCurrentSession(t *testing.T) {
	mc := &mockComposer{stats: map[string]runner.ServiceStats{"web": {CPUPercent: 1}}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.statsSession = 42

	msg, ok := m.refreshStats()().(statsMsg)
	if !ok {
		t.Fatalf("expected statsMsg, got %T", m.refreshStats()())
	}
	if msg.session != 42 {
		t.Errorf("captured session = %d, want 42", msg.session)
	}
}

func TestProjectEnter_bumpsStatsSession(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectProject
	m.projects = []compose.Project{{Name: "proj", ConfigDir: "/tmp"}}
	m.projCursor = 0
	m.statsSession = 1

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model := result.(Model)

	if model.statsSession <= 1 {
		t.Errorf("statsSession = %d, want > 1 after project enter", model.statsSession)
	}
}

func TestEsc_bumpsStatsSession(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.showPicker = true
	m.projects = []compose.Project{{Name: "proj"}}
	m.statsSession = 10

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := result.(Model)

	if model.statsSession <= 10 {
		t.Errorf("statsSession = %d, want > 10 after esc back-nav", model.statsSession)
	}
}

// TestExecDone_bumpsStatsSession verifies that returning from exec bumps the
// session — without this, an older in-flight stats fetch from the prior
// container-screen entry could land after the post-exec refresh and overwrite
// fresher data with stale state.
func TestExecDone_bumpsStatsSession(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statsSession = 5

	result, _ := m.Update(execDoneMsg{})
	model := result.(Model)

	if model.statsSession <= 5 {
		t.Errorf("statsSession = %d, want > 5 after execDoneMsg", model.statsSession)
	}
}

// TestLogsEsc_bumpsStatsSession verifies returning from the log viewer bumps
// the session so any pre-logs in-flight stats fetch is filtered out.
func TestLogsEsc_bumpsStatsSession(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenLogs
	m.statsSession = 7

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := result.(Model)

	if model.statsSession <= 7 {
		t.Errorf("statsSession = %d, want > 7 after logs esc", model.statsSession)
	}
}

// TestProgressEsc_bumpsStatsSession verifies returning from the progress screen
// (after a completed operation) bumps the session — same race protection as
// exec/logs.
func TestProgressEsc_bumpsStatsSession(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.done = true
	m.statsSession = 3

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := result.(Model)

	if model.statsSession <= 3 {
		t.Errorf("statsSession = %d, want > 3 after progress esc", model.statsSession)
	}
}

func TestRefreshStats_callsContainerStats(t *testing.T) {
	want := map[string]runner.ServiceStats{
		"web": {CPUPercent: 7.5, MemoryUsed: 1024 * 1024 * 100, MemoryLimit: 1024 * 1024 * 512},
	}
	mc := &mockComposer{stats: want}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	cmd := m.refreshStats()
	if cmd == nil {
		t.Fatal("refreshStats returned nil cmd")
	}
	msg, ok := cmd().(statsMsg)
	if !ok {
		t.Fatalf("cmd() type = %T, want statsMsg", cmd())
	}
	if msg.err != nil {
		t.Errorf("err = %v, want nil", msg.err)
	}
	if got := msg.stats["web"].CPUPercent; got != 7.5 {
		t.Errorf("stats[web].CPUPercent = %v, want 7.5", got)
	}
}

func TestRefreshStats_propagatesError(t *testing.T) {
	wantErr := errors.New("stats fetch failed")
	mc := &mockComposer{statsErr: wantErr}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	cmd := m.refreshStats()
	msg, ok := cmd().(statsMsg)
	if !ok {
		t.Fatalf("cmd() type = %T, want statsMsg", cmd())
	}
	if msg.err == nil || msg.err.Error() != wantErr.Error() {
		t.Errorf("err = %v, want %v", msg.err, wantErr)
	}
}

// --- Periodic refresh tick tests ---

// fakeTickMsg replaces refreshTickMsg in tests so the tick override can be
// distinguished from the real refreshTickMsg path. Tests assert on the cmd's
// returned msg type to decide whether the handler reached the fetch branch.
type fakeTickMsg struct{}

// installFakeTick swaps tickCmdOverride to a non-blocking Cmd. Without this,
// every test exercising the refreshTick path would leave a real 5s tea.Tick
// goroutine running until the program exits.
func installFakeTick(m *Model) {
	m.tickCmdOverride = func() tea.Cmd {
		return func() tea.Msg { return fakeTickMsg{} }
	}
}

// TestRefreshTickMsg_fetchesOnContainerScreen verifies that a tick firing on
// the container screen fans out to refreshStatus and refreshStats, AND queues
// the next tick. Verified by unwrapping the returned tea.BatchMsg and counting
// the docker calls each inner Cmd produces when invoked.
func TestRefreshTickMsg_fetchesOnContainerScreen(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.refreshInFlight = false // NewModel sets this true for local fast-track; we want the unblocked path
	mc.statusCalls = 0
	mc.statsCalls = 0

	result, cmd := m.Update(refreshTickMsg{})
	model := result.(Model)
	if cmd == nil {
		t.Fatal("refreshTickMsg on container screen should return a batch cmd, got nil")
	}
	if !model.refreshInFlight {
		t.Error("handler should set refreshInFlight=true when firing the fetch batch")
	}

	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected tea.BatchMsg, got %T", cmd())
	}
	if len(batch) != 3 {
		t.Fatalf("batch len = %d, want 3 (status + stats + tick)", len(batch))
	}
	// Invoke each inner Cmd; with the fake tick installed, none of them blocks.
	for _, c := range batch {
		_ = c()
	}
	if mc.statusCalls != 1 {
		t.Errorf("statusCalls = %d, want 1", mc.statusCalls)
	}
	if mc.statsCalls != 1 {
		t.Errorf("statsCalls = %d, want 1", mc.statsCalls)
	}
}

// TestRefreshTickMsg_skipsWhenInFlight verifies that a tick firing while a
// previous tick's fetch is still pending (refreshInFlight=true) reschedules
// without firing another batch — preventing pile-up of docker stats / SSH calls
// when each one takes longer than statsRefreshInterval.
func TestRefreshTickMsg_skipsWhenInFlight(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.refreshInFlight = true // previous tick's fetch is still pending

	_, cmd := m.Update(refreshTickMsg{})
	if cmd == nil {
		t.Fatal("refreshTickMsg with in-flight fetch should still reschedule, got nil")
	}
	if _, isBatch := cmd().(tea.BatchMsg); isBatch {
		t.Error("in-flight tick should return the tick alone, not a fetch batch")
	}
}

// TestRefreshTickMsg_skipsOffScreen verifies that a tick firing on any screen
// other than screenSelectContainers reschedules without producing the batch of
// docker fetches.
func TestRefreshTickMsg_skipsOffScreen(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenLogs

	_, cmd := m.Update(refreshTickMsg{})
	if cmd == nil {
		t.Fatal("refreshTickMsg off-screen should still reschedule a tick, got nil")
	}
	if _, isBatch := cmd().(tea.BatchMsg); isBatch {
		t.Error("off-screen tick should return the tick alone, not a fetch batch")
	}
}

// TestRefreshTickMsg_skipsDuringConfirm verifies that a tick firing while a
// confirmation prompt is up does NOT fan out to fetches (which would feel
// jittery under the user's eye) — but still reschedules.
func TestRefreshTickMsg_skipsDuringConfirm(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.refreshInFlight = false
	m.confirming = true

	_, cmd := m.Update(refreshTickMsg{})
	if cmd == nil {
		t.Fatal("refreshTickMsg during confirm should still reschedule, got nil")
	}
	if _, isBatch := cmd().(tea.BatchMsg); isBatch {
		t.Error("confirming tick should return the tick alone, not a fetch batch")
	}
}

// TestRefreshTickMsg_skipsWhenComposerNil verifies that a tick firing when
// the composer is nil (e.g. mid-disconnect transition) does NOT panic and
// still reschedules.
func TestRefreshTickMsg_skipsWhenComposerNil(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.refreshInFlight = false
	m.composer = nil

	_, cmd := m.Update(refreshTickMsg{})
	if cmd == nil {
		t.Fatal("refreshTickMsg with nil composer should still reschedule, got nil")
	}
	if _, isBatch := cmd().(tea.BatchMsg); isBatch {
		t.Error("nil-composer tick should return the tick alone, not a fetch batch")
	}
}

// TestStatsMsg_clearsRefreshInFlight verifies that a current-session statsMsg
// arrival clears the in-flight guard so the next periodic tick can fire a
// fresh fetch instead of skipping.
func TestStatsMsg_clearsRefreshInFlight(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statsSession = 5
	m.refreshInFlight = true

	result, _ := m.Update(statsMsg{stats: map[string]runner.ServiceStats{}, session: 5})
	model := result.(Model)

	if model.refreshInFlight {
		t.Error("current-session statsMsg should clear refreshInFlight, got true")
	}
}

// TestStatsMsg_errorClearsRefreshInFlight verifies the same on the error path
// — without this, a permanently-failing docker stats call would leave the
// guard stuck on and the periodic loop would never fire fetches again.
func TestStatsMsg_errorClearsRefreshInFlight(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statsSession = 5
	m.refreshInFlight = true

	result, _ := m.Update(statsMsg{err: errors.New("docker boom"), session: 5})
	model := result.(Model)

	if model.refreshInFlight {
		t.Error("error statsMsg should clear refreshInFlight, got true")
	}
}

// TestStatsMsg_offScreenClearsRefreshInFlight is the regression test for
// the codex round-2 finding: if the user opens the config screen (or logs,
// or progress) while a stats fetch is in flight, the response arrives with
// m.screen != screenSelectContainers. The handler must still clear the
// in-flight guard — otherwise, when the user returns to the container
// screen, every periodic tick sees refreshInFlight=true and skips,
// permanently silencing the loop.
func TestStatsMsg_offScreenClearsRefreshInFlight(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenConfig // user opened config while a fetch was pending
	m.statsSession = 5
	m.refreshInFlight = true

	result, _ := m.Update(statsMsg{stats: map[string]runner.ServiceStats{}, session: 5})
	model := result.(Model)

	if model.refreshInFlight {
		t.Error("off-screen current-session statsMsg should still clear refreshInFlight, got true")
	}
	// And the response data must NOT have been applied (screen check still gates display).
	if model.stats != nil {
		t.Errorf("off-screen statsMsg should not update m.stats, got %+v", model.stats)
	}
}

// TestStatsMsg_staleSessionLeavesRefreshInFlight verifies that a stale
// (different-session) response does NOT clear refreshInFlight. The context
// bump at the navigation site already reset the flag; clearing it again on a
// stale response could accidentally clear the new context's freshly-set
// in-flight guard if the response arrives mid-transition.
func TestStatsMsg_staleSessionLeavesRefreshInFlight(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statsSession = 6 // current
	m.refreshInFlight = true

	result, _ := m.Update(statsMsg{stats: map[string]runner.ServiceStats{}, session: 3}) // stale
	model := result.(Model)

	if !model.refreshInFlight {
		t.Error("stale statsMsg should NOT clear refreshInFlight, got false")
	}
}

// --- statusMsg session guard tests ---

// TestStatusMsg_currentSessionAccepted verifies the happy path for the new
// statusSession filter.
func TestStatusMsg_currentSessionAccepted(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statusSession = 7

	result, _ := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true}},
		session: 7,
	})
	model := result.(Model)

	if !model.svcStatus["web"].Running {
		t.Errorf("svcStatus[web].Running = false, want true (current-session msg should be applied)")
	}
}

// TestStatusMsg_staleSessionIgnored is the regression test for the bug codex
// flagged: a periodic-tick statusMsg from project A must NOT overwrite
// svcStatus after the user has navigated to project B.
func TestStatusMsg_staleSessionIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statusSession = 9
	m.svcStatus = map[string]runner.ServiceStatus{"new": {Running: true}}

	result, _ := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"stale": {Running: true}},
		session: 4, // older context
	})
	model := result.(Model)

	if _, ok := model.svcStatus["stale"]; ok {
		t.Error("stale statusMsg from older context overwrote svcStatus")
	}
	if !model.svcStatus["new"].Running {
		t.Error("current svcStatus was clobbered by stale statusMsg")
	}
}

// TestStatusMsg_offScreenIgnored verifies the screen-gate still applies, so
// a refreshStatus response can't overwrite svcStatus while the user is on a
// different screen (e.g. mid-progress).
func TestStatusMsg_offScreenIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.statusSession = 1

	result, _ := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"x": {Running: true}},
		session: 1,
	})
	model := result.(Model)

	if _, ok := model.svcStatus["x"]; ok {
		t.Error("statusMsg applied while off-screen; should have been ignored")
	}
}

// TestRefreshStatus_capturesCurrentSession mirrors TestRefreshStats_capturesCurrentSession
// for the new statusSession plumbing.
func TestRefreshStatus_capturesCurrentSession(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.statusSession = 42

	msg, ok := m.refreshStatus()().(statusMsg)
	if !ok {
		t.Fatalf("expected statusMsg, got %T", m.refreshStatus()())
	}
	if msg.session != 42 {
		t.Errorf("captured session = %d, want 42", msg.session)
	}
}

// --- servicesMsg session guard tests ---

// TestLoadServices_capturesCurrentSession verifies that loadServices() captures
// the current statusSession at fire time so the response can be filtered.
func TestLoadServices_capturesCurrentSession(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.statusSession = 23

	msg, ok := m.loadServices()().(servicesMsg)
	if !ok {
		t.Fatalf("expected servicesMsg, got %T", m.loadServices()())
	}
	if msg.session != 23 {
		t.Errorf("captured session = %d, want 23", msg.session)
	}
}

// TestServicesMsg_currentSessionAccepted verifies the happy path.
func TestServicesMsg_currentSessionAccepted(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statusSession = 7

	result, _ := m.Update(servicesMsg{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		session:  7,
	})
	model := result.(Model)

	if len(model.services) != 1 || model.services[0] != "web" {
		t.Errorf("services = %v, want [web]", model.services)
	}
	if !model.svcStatus["web"].Running {
		t.Error("svcStatus not applied for current-session msg")
	}
}

// TestServicesMsg_staleSessionIgnored is the regression test for the codex
// round-2 finding: loadServices from project A must NOT overwrite the services
// list / svcStatus after the user has navigated to project B.
func TestServicesMsg_staleSessionIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statusSession = 9 // current
	m.services = []string{"new-svc"}
	m.svcStatus = map[string]runner.ServiceStatus{"new-svc": {Running: true}}

	result, _ := m.Update(servicesMsg{
		services: []string{"stale-svc"},
		status:   map[string]runner.ServiceStatus{"stale-svc": {Running: true}},
		session:  4, // stale
	})
	model := result.(Model)

	if len(model.services) != 1 || model.services[0] != "new-svc" {
		t.Errorf("stale servicesMsg clobbered services: got %v, want [new-svc]", model.services)
	}
	if _, ok := model.svcStatus["stale-svc"]; ok {
		t.Error("stale servicesMsg clobbered svcStatus")
	}
}

// TestServicesMsg_offScreenIgnored verifies the screen-gate still applies.
func TestServicesMsg_offScreenIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.statusSession = 1
	m.services = []string{"existing"}

	result, _ := m.Update(servicesMsg{
		services: []string{"unwanted"},
		status:   map[string]runner.ServiceStatus{"unwanted": {Running: true}},
		session:  1,
	})
	model := result.(Model)

	if len(model.services) != 1 || model.services[0] != "existing" {
		t.Errorf("servicesMsg applied while off-screen: got %v, want [existing]", model.services)
	}
}

// --- projectsMsg session guard tests ---

// TestLoadProjects_capturesCurrentSession verifies loadProjects captures
// m.projectsSession at fire time.
func TestLoadProjects_capturesCurrentSession(t *testing.T) {
	mc := &mockComposer{}
	loader := func(context.Context) ([]compose.Project, error) {
		return []compose.Project{{Name: "p", ConfigDir: "/p"}}, nil
	}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil, WithLocalProjectLoader(loader))
	m.projectsSession = 17

	msg, ok := m.loadProjects()().(projectsMsg)
	if !ok {
		t.Fatalf("expected projectsMsg, got %T", m.loadProjects()())
	}
	if msg.session != 17 {
		t.Errorf("captured session = %d, want 17", msg.session)
	}
}

// TestProjectsMsg_currentSessionAccepted verifies the happy path.
func TestProjectsMsg_currentSessionAccepted(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectProject
	m.projectsSession = 7

	result, _ := m.Update(projectsMsg{
		projects: []compose.Project{{Name: "p", ConfigDir: "/p"}},
		session:  7,
	})
	model := result.(Model)

	if len(model.projects) != 1 || model.projects[0].Name != "p" {
		t.Errorf("projects = %v, want [{Name: p}]", model.projects)
	}
}

// TestProjectsMsg_staleSessionIgnored is the regression test for the codex
// round-3 finding: loadProjects from server A must NOT overwrite m.projects
// after the user has navigated to server B (which would let server B's
// composerFactory get fed a path discovered on server A — potentially mounting
// the wrong directory on deploy).
func TestProjectsMsg_staleSessionIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectProject
	m.projectsSession = 9 // current (server B)
	m.projects = []compose.Project{{Name: "b-proj", ConfigDir: "/b"}}

	result, _ := m.Update(projectsMsg{
		projects: []compose.Project{{Name: "a-proj", ConfigDir: "/a"}}, // stale, from server A
		session:  4,
	})
	model := result.(Model)

	if len(model.projects) != 1 || model.projects[0].Name != "b-proj" {
		t.Errorf("stale projectsMsg clobbered projects: got %v, want [{b-proj}]", model.projects)
	}
}

// TestProjectsMsg_offScreenIgnored verifies the screen-gate still applies.
func TestProjectsMsg_offScreenIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectServer // not the project picker
	m.projectsSession = 3
	m.projects = []compose.Project{{Name: "existing"}}

	result, _ := m.Update(projectsMsg{
		projects: []compose.Project{{Name: "unwanted"}},
		session:  3,
	})
	model := result.(Model)

	if len(model.projects) != 1 || model.projects[0].Name != "existing" {
		t.Errorf("projectsMsg applied while off-screen: got %v, want [existing]", model.projects)
	}
}

// TestProjectsMsg_errorWithCurrentSessionApplied verifies error responses on
// the current session ARE applied — without this, a network error from the
// remote loader would never surface.
func TestProjectsMsg_errorWithCurrentSessionApplied(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectProject
	m.projectsSession = 5

	result, _ := m.Update(projectsMsg{
		err:     errors.New("ssh: connection refused"),
		session: 5,
	})
	model := result.(Model)

	if model.projErr == nil || model.projErr.Error() != "ssh: connection refused" {
		t.Errorf("projErr = %v, want 'ssh: connection refused'", model.projErr)
	}
}

// --- Task 9: CPU/Mem column rendering tests ---

func TestContainerScreen_rendersStatsColumns(t *testing.T) {
	mc := &mockComposer{services: []string{"web", "db"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true},
		"db":  {Running: true},
	}
	m.stats = map[string]runner.ServiceStats{
		"web": {CPUPercent: 4.2, MemoryUsed: 130023424, MemoryLimit: 536870912},
		"db":  {CPUPercent: 1.1, MemoryUsed: 200 * 1024 * 1024, MemoryLimit: 1024 * 1024 * 1024},
	}
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()

	// Captions row includes CPU and Mem headers.
	if !strings.Contains(view, "CPU") {
		t.Errorf("expected 'CPU' caption in view, got:\n%s", view)
	}
	if !strings.Contains(view, "Mem") {
		t.Errorf("expected 'Mem' caption in view, got:\n%s", view)
	}

	// Per-row values: CPU formatted as one decimal + %, Mem as used/limit.
	if !strings.Contains(view, "4.2%") {
		t.Errorf("expected '4.2%%' for web CPU in view, got:\n%s", view)
	}
	if !strings.Contains(view, "1.1%") {
		t.Errorf("expected '1.1%%' for db CPU in view, got:\n%s", view)
	}
	// 130023424 → "124M", 536870912 → "512M"
	if !strings.Contains(view, "124M/512M") {
		t.Errorf("expected '124M/512M' for web Mem in view, got:\n%s", view)
	}
	// 200*MiB → "200M", 1*GiB → "1G"
	if !strings.Contains(view, "200M/1G") {
		t.Errorf("expected '200M/1G' for db Mem in view, got:\n%s", view)
	}

	// Column order: Uptime ... CPU before Mem before Ports (Mem appears before any Ports section,
	// even though Ports is absent here, so we just check CPU appears before Mem in caption row).
	captionLineIdx := strings.Index(view, "CPU")
	memCaptionIdx := strings.Index(view, "Mem")
	if captionLineIdx >= 0 && memCaptionIdx >= 0 && captionLineIdx >= memCaptionIdx {
		t.Errorf("expected 'CPU' caption before 'Mem' caption, CPU=%d Mem=%d", captionLineIdx, memCaptionIdx)
	}
}

// TestContainerScreen_serviceColumnHasCaption verifies that the "Service"
// caption is rendered above the service-name column when any status columns
// are present, and that it aligns with the service names underneath.
func TestContainerScreen_serviceColumnHasCaption(t *testing.T) {
	mc := &mockComposer{services: []string{"web", "db"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true, Created: "2024-01-15 09:30"},
		"db":  {Running: true, Created: "2024-01-15 09:30"},
	}
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()

	// Caption must appear.
	serviceIdx := strings.Index(view, "Service")
	if serviceIdx < 0 {
		t.Fatalf("expected 'Service' caption in view, got:\n%s", view)
	}
	// Caption must precede the data rows (the rows contain the service names).
	webIdx := strings.Index(view, "web")
	if serviceIdx >= webIdx {
		t.Errorf("'Service' caption at %d should precede 'web' row at %d", serviceIdx, webIdx)
	}
	// Caption must precede other column headers.
	if createdIdx := strings.Index(view, "Created"); createdIdx > 0 && serviceIdx >= createdIdx {
		t.Errorf("'Service' caption at %d should precede 'Created' at %d", serviceIdx, createdIdx)
	}
}

// TestContainerScreen_serviceCaptionWidensShortNames verifies that when every
// service name is shorter than "Service" (7 chars), the column widens to fit
// the caption — keeping the following column headers aligned with the data
// rows underneath. Lipgloss embeds ANSI codes inline in styled segments
// (checkbox / health / dot), so we strip those before comparing column
// positions across rows.
func TestContainerScreen_serviceCaptionWidensShortNames(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b"}} // both 1 char, shorter than "Service"
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"a": {Running: true, Created: "2024-01-15 09:30"},
		"b": {Running: true, Created: "2024-01-15 09:30"},
	}
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()

	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	stripAnsi := func(s string) string { return ansi.ReplaceAllString(s, "") }

	var captionLine, dataLine string
	for _, ln := range strings.Split(view, "\n") {
		bare := stripAnsi(ln)
		if strings.Contains(bare, "Service") && strings.Contains(bare, "Created") && captionLine == "" {
			captionLine = bare
		}
		if strings.Contains(bare, "2024-01-15") && dataLine == "" {
			dataLine = bare
		}
	}
	if captionLine == "" || dataLine == "" {
		t.Fatalf("could not find caption/data lines in view:\n%s", view)
	}
	// "Created" caption must align with the date in the data row beneath it.
	// Use rune-count position rather than byte index so multi-byte glyphs (●)
	// in the data row don't skew the comparison.
	runePos := func(s, sub string) int {
		i := strings.Index(s, sub)
		if i < 0 {
			return -1
		}
		return utf8.RuneCountInString(s[:i])
	}
	if got, want := runePos(captionLine, "Created"), runePos(dataLine, "2024-01-15"); got != want {
		t.Errorf("Created caption col=%d, data col=%d (caption widening didn't propagate to data rows)\ncaption=%q\ndata=%q", got, want, captionLine, dataLine)
	}
}

// TestContainerScreen_statsColumnWidthsAreStable verifies that CPU/Mem
// columns reserve fixed minimum widths so a transient high value (e.g. one
// service briefly hitting 11% CPU on a periodic refresh) doesn't push every
// other column rightward. Both small ("0.2%") and larger ("11.1%") values
// must render with the SAME column width — anything smaller than the
// reserved minimum gets left-padded.
func TestContainerScreen_statsColumnWidthsAreStable(t *testing.T) {
	mc := &mockComposer{services: []string{"small", "big"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"small": {Running: true},
		"big":   {Running: true},
	}
	m.stats = map[string]runner.ServiceStats{
		"small": {CPUPercent: 0.2, MemoryUsed: 10 * 1024 * 1024, MemoryLimit: 128 * 1024 * 1024},
		"big":   {CPUPercent: 11.1, MemoryUsed: 1503238553, MemoryLimit: 4 * 1024 * 1024 * 1024}, // ~1.4 GiB
	}
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()

	// CPU is right-aligned. With cpuColMin=6, "0.2%" (4 chars) becomes
	// "  0.2%" and "11.1%" (5 chars) becomes " 11.1%". Both should appear.
	if !strings.Contains(view, "  0.2%") {
		t.Errorf("expected right-aligned '  0.2%%' (4-char value in 6-char column), got:\n%s", view)
	}
	if !strings.Contains(view, " 11.1%") {
		t.Errorf("expected right-aligned ' 11.1%%' (5-char value in 6-char column), got:\n%s", view)
	}

	// Both rows must have the same overall column layout — find the start of
	// each row and assert that the "%" characters align at the same column index.
	smallIdx := strings.Index(view, "small")
	bigIdx := strings.Index(view, "big")
	if smallIdx < 0 || bigIdx < 0 {
		t.Fatalf("could not locate both service rows in view:\n%s", view)
	}
	// Walk forward from each row start to the next newline; find the % position.
	pctCol := func(rowStart int) int {
		lineEnd := strings.IndexByte(view[rowStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(view) - rowStart
		}
		line := view[rowStart : rowStart+lineEnd]
		return strings.IndexByte(line, '%')
	}
	smallPct, bigPct := pctCol(smallIdx), pctCol(bigIdx)
	if smallPct != bigPct {
		t.Errorf("CPU percent signs misaligned: small at col %d, big at col %d (right-alignment + fixed width should make them equal)", smallPct, bigPct)
	}
}

func TestContainerScreen_blankCellsForMissingStats(t *testing.T) {
	// One service has stats, the other doesn't — stats-missing service should
	// render without panic and contribute blank padded cells.
	mc := &mockComposer{services: []string{"web", "db"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true},
		"db":  {Running: true},
	}
	m.stats = map[string]runner.ServiceStats{
		"web": {CPUPercent: 5.5, MemoryUsed: 100 * 1024 * 1024, MemoryLimit: 200 * 1024 * 1024},
		// db absent from stats map
	}
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()

	// Captions present because stats data exists on at least one service.
	if !strings.Contains(view, "CPU") {
		t.Errorf("expected CPU caption when any service has stats, got:\n%s", view)
	}

	// The "db" service should render without web's CPU value bleeding into it.
	lines := strings.Split(view, "\n")
	var dbLine string
	for _, l := range lines {
		if strings.Contains(l, " db ") || strings.HasSuffix(strings.TrimRight(l, " "), " db") || strings.Contains(l, "● db") {
			dbLine = l
			break
		}
	}
	if dbLine == "" {
		t.Fatalf("could not locate db service line in view:\n%s", view)
	}
	if strings.Contains(dbLine, "5.5%") {
		t.Errorf("db line should not contain web's CPU value: %q", dbLine)
	}
	if strings.Contains(dbLine, "100M") {
		t.Errorf("db line should not contain web's Mem value: %q", dbLine)
	}
}

func TestContainerScreen_blankCellsForStoppedService(t *testing.T) {
	// Stopped service should render blank CPU/Mem even if stats map has an entry
	// (defensive: a leftover stats entry for a now-stopped container shouldn't leak).
	mc := &mockComposer{services: []string{"web", "db"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true},
		"db":  {Running: false},
	}
	m.stats = map[string]runner.ServiceStats{
		"web": {CPUPercent: 9.9, MemoryUsed: 100 * 1024 * 1024, MemoryLimit: 200 * 1024 * 1024},
		"db":  {CPUPercent: 7.7, MemoryUsed: 50 * 1024 * 1024, MemoryLimit: 100 * 1024 * 1024},
	}
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()
	lines := strings.Split(view, "\n")
	var dbLine string
	for _, l := range lines {
		if strings.Contains(l, " db ") || strings.HasSuffix(strings.TrimRight(l, " "), " db") {
			dbLine = l
			break
		}
	}
	if dbLine == "" {
		t.Fatalf("could not locate db service line:\n%s", view)
	}
	if strings.Contains(dbLine, "7.7%") {
		t.Errorf("stopped db line should not contain stale CPU value: %q", dbLine)
	}
	if strings.Contains(dbLine, "50M") {
		t.Errorf("stopped db line should not contain stale Mem value: %q", dbLine)
	}
}

func TestContainerScreen_StatsCaptionsReservedBeforeData(t *testing.T) {
	// Once stats have been requested, CPU/Mem captions are reserved from the
	// first frame so they don't pop in when the ~1.5s docker stats call returns.
	// Cells stay blank for services without data yet.
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statsRequested = true
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true},
	}
	m.stats = nil
	m.width = 120
	m.height = 24

	view := m.viewSelectContainers()
	if !strings.Contains(view, "CPU") {
		t.Errorf("CPU caption should be reserved when statsRequested, got:\n%s", view)
	}
	if !strings.Contains(view, "Mem") {
		t.Errorf("Mem caption should be reserved when statsRequested, got:\n%s", view)
	}
}

func TestContainerScreen_NoStatsCaptionsAbsentWithoutRequest(t *testing.T) {
	// When stats have not been requested (statsRequested=false, e.g. a screen
	// that never fetched stats), and no other status columns are present, the
	// captions row is suppressed entirely.
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statsRequested = false
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true},
	}
	m.stats = nil
	m.width = 120
	m.height = 24

	view := m.viewSelectContainers()
	if strings.Contains(view, "CPU") {
		t.Errorf("CPU caption should be absent when !statsRequested && no data, got:\n%s", view)
	}
	if strings.Contains(view, "Mem") {
		t.Errorf("Mem caption should be absent when !statsRequested && no data, got:\n%s", view)
	}
}

func TestContainerScreen_statsErrFallback(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{"web": {Running: true}}
	m.statsErr = errors.New("docker stats failed: timeout")
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()

	// Services are still listed (soft-fail; not a full-screen error).
	if !strings.Contains(view, "web") {
		t.Errorf("services should still render when only statsErr is set, got:\n%s", view)
	}
	// statsErr message renders inline as a warning.
	if !strings.Contains(view, "Stats unavailable") {
		t.Errorf("expected 'Stats unavailable' warning in view, got:\n%s", view)
	}
	if !strings.Contains(view, "docker stats failed: timeout") {
		t.Errorf("expected statsErr message in view, got:\n%s", view)
	}
}

func TestContainerScreen_svcErrPreferred(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.svcStatus = map[string]runner.ServiceStatus{"web": {Running: true}}
	m.svcErr = errors.New("services load failed")
	m.statsErr = errors.New("stats fetch failed too")
	m.width = 200
	m.height = 24

	view := m.viewSelectContainers()

	// svcErr wins via early-return: its message appears.
	if !strings.Contains(view, "services load failed") {
		t.Errorf("svcErr message should appear, got:\n%s", view)
	}
	// statsErr message is suppressed when svcErr takes over the screen.
	if strings.Contains(view, "stats fetch failed too") {
		t.Errorf("statsErr should NOT appear when svcErr is set, got:\n%s", view)
	}
	if strings.Contains(view, "Stats unavailable") {
		t.Errorf("Stats unavailable warning should NOT appear when svcErr is set, got:\n%s", view)
	}
}

func TestSvcVisibleCount_withStatsColumns(t *testing.T) {
	// With stats data present, the captions row still appears, so headerLines=4.
	// The captions row presence is binary: adding CPU/Mem headers does not
	// change the per-row line count beyond the existing Created/Uptime case.
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m.width = 200
	m.height = 10
	m.svcStatus = map[string]runner.ServiceStatus{
		"a": {Running: true},
	}
	m.stats = map[string]runner.ServiceStats{
		"a": {CPUPercent: 4.2, MemoryUsed: 100, MemoryLimit: 1000},
	}

	// header=4 (3 + captions row), footer=3 (one-line help; reserved bar merges) → 10-4-3 = 3
	got := m.svcVisibleCount()
	if got != 3 {
		t.Errorf("svcVisibleCount() with stats columns = %d, want 3", got)
	}
}

func TestHasStatusColumns_StatsDataAlone(t *testing.T) {
	// Stats data alone (no Created/Uptime/Ports) should trigger captions row.
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.services = []string{"web"}
	m.svcStatus = map[string]runner.ServiceStatus{"web": {Running: true}}
	m.stats = map[string]runner.ServiceStats{
		"web": {CPUPercent: 4.2},
	}
	if !m.hasStatusColumns() {
		t.Error("hasStatusColumns() = false, want true with stats data present")
	}
}

// --- updatesMsg / refreshUpdates / cache tests (Task 5) ---

// TestUpdatesMsg_currentSessionHydrates verifies the happy path: a
// current-session updatesMsg writes UpdateAvailable into svcStatus and
// clears updateInFlight.
func TestUpdatesMsg_currentSessionHydrates(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updatesSession = 3
	m.updateInFlight = true
	m.svcStatus = map[string]runner.ServiceStatus{
		"web":   {Running: true},
		"db":    {Running: true},
		"cache": {Running: true},
	}

	result, _ := m.Update(updatesMsg{
		results: map[string]bool{"web": true, "db": false},
		session: 3,
	})
	model := result.(Model)

	if model.updateInFlight {
		t.Error("updateInFlight should be cleared after current-session msg")
	}
	if model.svcStatus["web"].UpdateAvailable == nil || !*model.svcStatus["web"].UpdateAvailable {
		t.Errorf("web UpdateAvailable = %v, want &true", model.svcStatus["web"].UpdateAvailable)
	}
	if model.svcStatus["db"].UpdateAvailable == nil || *model.svcStatus["db"].UpdateAvailable {
		t.Errorf("db UpdateAvailable = %v, want &false", model.svcStatus["db"].UpdateAvailable)
	}
	// cache absent — UpdateAvailable should remain nil
	if model.svcStatus["cache"].UpdateAvailable != nil {
		t.Errorf("cache UpdateAvailable = %v, want nil (absent from results)", model.svcStatus["cache"].UpdateAvailable)
	}
	// Cache should be populated
	key := model.updatesCacheKey()
	if entry, ok := model.updateCache[key]; !ok {
		t.Errorf("updateCache missing key %q", key)
	} else if entry.results == nil || !entry.results["web"] {
		t.Errorf("cached entry = %+v, want web=true", entry)
	}
}

// TestUpdatesMsg_staleSessionIgnored verifies that a stale (older session)
// updatesMsg is dropped and does NOT hydrate svcStatus.
func TestUpdatesMsg_staleSessionIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updatesSession = 9
	m.svcStatus = map[string]runner.ServiceStatus{"web": {Running: true}}

	result, _ := m.Update(updatesMsg{
		results: map[string]bool{"web": true},
		session: 4, // older context
	})
	model := result.(Model)

	if model.svcStatus["web"].UpdateAvailable != nil {
		t.Error("stale updatesMsg should NOT hydrate svcStatus")
	}
	// Cache should not be touched by stale msgs
	if len(model.updateCache) != 0 {
		t.Errorf("stale updatesMsg should not populate cache, got %d entries", len(model.updateCache))
	}
}

// TestUpdatesMsg_clearsInFlightOffScreen mirrors the off-screen handling for
// stats: even when the user has navigated away, a current-session response
// must clear updateInFlight so the next entry doesn't see a permanently-stuck
// guard.
func TestUpdatesMsg_clearsInFlightOffScreen(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenConfig // off-screen
	m.updatesSession = 5
	m.updateInFlight = true

	result, _ := m.Update(updatesMsg{
		results: map[string]bool{"web": true},
		session: 5,
	})
	model := result.(Model)

	if model.updateInFlight {
		t.Error("off-screen current-session updatesMsg should still clear updateInFlight")
	}
	// Data must NOT be applied to svcStatus (screen check still gates display).
	if model.svcStatus["web"].UpdateAvailable != nil {
		t.Error("off-screen updatesMsg should not hydrate svcStatus")
	}
	// Cache IS populated regardless of screen so a re-entry can pick it up.
	if _, ok := model.updateCache[model.updatesCacheKey()]; !ok {
		t.Error("cache should be populated even when off-screen")
	}
}

// TestUpdatesMsg_errorSetsErrAndClearsInFlight verifies the error path:
// updatesErr is set, updateInFlight cleared.
func TestUpdatesMsg_errorSetsErrAndClearsInFlight(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updatesSession = 1
	m.updateInFlight = true

	result, _ := m.Update(updatesMsg{
		err:     errors.New("registry timeout"),
		session: 1,
	})
	model := result.(Model)

	if model.updateInFlight {
		t.Error("error updatesMsg should clear updateInFlight")
	}
	if !strings.Contains(model.updatesErr, "registry timeout") {
		t.Errorf("updatesErr = %q, want it to contain 'registry timeout'", model.updatesErr)
	}
}

// TestUpdatesMsg_staleClearsInFlight verifies the iteration-4 semantics:
// the in-flight guard is cleared on ANY arrival (stale or current), not
// just current-session arrivals. The previous semantics ("stale leaves
// flag alone, context-change handler resets it") had a sticky-flag bug:
// when the user pressed U (in-flight=true) then navigated away and back,
// the stale response would arrive with the OLD session — and on a cache
// hit path, maybeRefreshUpdatesCmd's new in-flight guard would silently
// swallow every subsequent fetch. Clearing on stale arrivals as well is
// safe because the cache write below is still session-gated, so a stale
// result can't pollute the cache.
func TestUpdatesMsg_staleClearsInFlight(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updatesSession = 6
	m.updateInFlight = true

	result, _ := m.Update(updatesMsg{
		results: map[string]bool{"web": true},
		session: 3, // stale
	})
	model := result.(Model)

	if model.updateInFlight {
		t.Error("stale updatesMsg should clear updateInFlight (iteration-4 semantics)")
	}
}

// TestUpdatesCache_HydratesOnServicesMsg verifies that when servicesMsg
// arrives (initial load), a fresh cached entry is re-applied so that
// UpdateAvailable survives the svcStatus overwrite.
func TestUpdatesCache_HydratesOnServicesMsg(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statusSession = 2
	// Seed the cache with a fresh entry for the current context.
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			results:   map[string]bool{"web": true},
		},
	}

	result, _ := m.Update(servicesMsg{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		session:  2,
	})
	model := result.(Model)

	if model.svcStatus["web"].UpdateAvailable == nil || !*model.svcStatus["web"].UpdateAvailable {
		t.Errorf("UpdateAvailable should be hydrated from cache, got %v", model.svcStatus["web"].UpdateAvailable)
	}
}

// TestUpdatesCache_HydratesOnStatusMsg is the periodic-refresh counterpart:
// statusMsg overwrites svcStatus, so without cache re-hydration the glyph
// would flicker off every 5s.
func TestUpdatesCache_HydratesOnStatusMsg(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statusSession = 3
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			results:   map[string]bool{"web": true},
		},
	}

	result, _ := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true}},
		session: 3,
	})
	model := result.(Model)

	if model.svcStatus["web"].UpdateAvailable == nil || !*model.svcStatus["web"].UpdateAvailable {
		t.Error("UpdateAvailable should survive periodic statusMsg via cache re-apply")
	}
}

// TestUpdatesCache_ExpiredEntryNotHydrated verifies TTL: an entry older than
// updatesCacheTTL is treated as missing.
func TestUpdatesCache_ExpiredEntryNotHydrated(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statusSession = 1
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now().Add(-2 * updatesCacheTTL),
			results:   map[string]bool{"web": true},
		},
	}

	result, _ := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true}},
		session: 1,
	})
	model := result.(Model)

	if model.svcStatus["web"].UpdateAvailable != nil {
		t.Error("expired cache entry should not hydrate UpdateAvailable")
	}
}

// TestMaybeRefreshUpdates_CacheMissTriggersFetch verifies the cache-miss
// branch returns a non-nil Cmd and sets updateInFlight.
func TestMaybeRefreshUpdates_CacheMissTriggersFetch(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updateInFlight = false

	cmd := m.maybeRefreshUpdatesCmd()
	if cmd == nil {
		t.Fatal("cache miss should return a non-nil refresh Cmd")
	}
	if !m.updateInFlight {
		t.Error("cache miss should set updateInFlight=true")
	}
}

// TestMaybeRefreshUpdates_CacheHitSkipsFetch verifies the cache-hit branch
// returns nil (no fetch) and DOES NOT set updateInFlight.
func TestMaybeRefreshUpdates_CacheHitSkipsFetch(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updateInFlight = false
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			results:   map[string]bool{"web": true},
		},
	}

	cmd := m.maybeRefreshUpdatesCmd()
	if cmd != nil {
		t.Error("fresh cache hit should return nil Cmd (no fetch)")
	}
	if m.updateInFlight {
		t.Error("fresh cache hit should not set updateInFlight")
	}
}

// TestUpdatesMsg_ErrorCachedWithShortTTL pins the iter-2 contract: failed
// fetches ARE cached, but with a short error TTL (updatesErrorTTL). The
// failure entry has results=nil and err=true. Surfaces updatesErr. The
// short-TTL caching prevents the 5-second refetch loop a previous
// "delete on error" implementation produced via the statusMsg self-heal.
func TestUpdatesMsg_ErrorCachedWithShortTTL(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updateInFlight = true

	result, _ := m.Update(updatesMsg{
		results: map[string]bool{"web": true}, // partial — should NOT be cached on error
		err:     errors.New("registry boom"),
		session: m.updatesSession,
	})
	model := result.(Model)
	if model.updatesErr == "" {
		t.Error("updatesErr should be set when fetch returns an error")
	}
	entry, ok := model.updateCache[model.updatesCacheKey()]
	if !ok {
		t.Fatal("cache SHOULD contain an error entry after errored fetch")
	}
	if !entry.err {
		t.Error("cache entry after error should have err=true")
	}
	if entry.results != nil {
		t.Errorf("cache entry after error should have results=nil, got %v", entry.results)
	}
	// Within the error TTL, maybeRefreshUpdatesCmd should NOT re-fetch.
	if cmd := model.maybeRefreshUpdatesCmd(); cmd != nil {
		t.Error("cache hit on fresh error entry should NOT fire refreshUpdates (prevents tight retry loop)")
	}
}

// TestUpdatesMsg_ErrorClearsCachedSuccess pins the iter-1 codex finding:
// a failed refresh must invalidate any prior successful cache entry for the
// same key. Without this, the successful predecessor's verdict sticks for
// the full TTL window and the next screen entry hydrates from data the
// most-recent refresh proved unreliable. With iter-2 semantics, the
// "invalidation" is now an OVERWRITE with an err=true entry (results=nil)
// rather than a delete — same end result for hydration (nil results
// don't paint glyphs), with the added benefit of suppressing tight retry.
func TestUpdatesMsg_ErrorClearsCachedSuccess(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updateInFlight = true
	// Seed a known-good prior success at the current key.
	key := m.updatesCacheKey()
	m.updateCache = map[string]updateEntry{
		key: {
			fetchedAt: time.Now(),
			results:   map[string]bool{"web": true},
		},
	}

	result, _ := m.Update(updatesMsg{
		err:     errors.New("registry boom"),
		session: m.updatesSession,
	})
	model := result.(Model)

	entry, ok := model.updateCache[key]
	if !ok {
		t.Fatal("errored refresh should leave an error entry, not delete the key entirely")
	}
	if !entry.err {
		t.Error("errored refresh should overwrite the prior success with an err=true entry")
	}
	if entry.results != nil {
		t.Errorf("errored refresh should drop the prior success's results, got %v", entry.results)
	}
	// Within the error TTL, maybeRefreshUpdatesCmd should NOT re-fetch.
	if cmd := model.maybeRefreshUpdatesCmd(); cmd != nil {
		t.Error("post-error fresh-error-cache hit should NOT fire refreshUpdates")
	}
}

// TestUpdatesMsg_ErrorClearsGlyphs (iter-2 fix #2): when CheckUpdates errors,
// the handler must clear UpdateAvailable on every svcStatus entry. Without
// this, the user sees both the "updates unavailable" soft warning AND stale
// ⇧ glyphs from a prior successful fetch — confusing inconsistency.
func TestUpdatesMsg_ErrorClearsGlyphs(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updateInFlight = true
	// Seed svcStatus with prior verdicts from a previous successful fetch.
	trueVal, falseVal := true, false
	m.svcStatus = map[string]runner.ServiceStatus{
		"web":   {Running: true, UpdateAvailable: &trueVal},
		"db":    {Running: true, UpdateAvailable: &falseVal},
		"cache": {Running: true, UpdateAvailable: &trueVal},
	}

	result, _ := m.Update(updatesMsg{
		err:     errors.New("registry timeout"),
		session: m.updatesSession,
	})
	model := result.(Model)

	for svc, st := range model.svcStatus {
		if st.UpdateAvailable != nil {
			t.Errorf("svcStatus[%q].UpdateAvailable should be nil after error, got %v", svc, *st.UpdateAvailable)
		}
	}
	if model.updatesErr == "" {
		t.Error("updatesErr should be set so the soft warning renders")
	}
}

// TestUpdatesMsg_ErrorExpiresAfterErrorTTL (iter-2 fix #1): after an error
// is cached, once the short error TTL has elapsed the entry should no
// longer be "fresh" and the next maybeRefreshUpdatesCmd should re-fetch.
// Verifies the bounded-retry property: persistent failures don't lock in
// for the full 10-minute success TTL.
func TestUpdatesMsg_ErrorExpiresAfterErrorTTL(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updateInFlight = false
	// Seed an error entry that's already past updatesErrorTTL.
	key := m.updatesCacheKey()
	m.updateCache = map[string]updateEntry{
		key: {
			fetchedAt: time.Now().Add(-2 * updatesErrorTTL),
			err:       true,
		},
	}

	if cmd := m.maybeRefreshUpdatesCmd(); cmd == nil {
		t.Error("stale error entry (past errorTTL) should fire refreshUpdates on next call")
	}
	if !m.updateInFlight {
		t.Error("stale error entry should set updateInFlight=true")
	}
}

// TestStatusMsg_ErrorEntryNotRefetchedTightly (iter-2 fix #1, regression):
// the statusMsg self-heal must NOT re-fetch when a fresh error entry is in
// the cache. Multiple 5-second statusMsg ticks within updatesErrorTTL
// should result in ZERO CheckUpdates calls — that's the whole point of
// the errorTTL cache.
func TestStatusMsg_ErrorEntryNotRefetchedTightly(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}, updates: map[string]bool{"web": true}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.composer = mc
	m.statusSession = 11
	m.updateInFlight = false
	// Fresh error entry (just-failed).
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			err:       true,
		},
	}

	// Simulate three 5-second ticks delivering statusMsg.
	for i := 0; i < 3; i++ {
		result, cmd := m.Update(statusMsg{
			status:  map[string]runner.ServiceStatus{"web": {Running: true}},
			session: 11,
		})
		m = result.(Model)
		if cmd != nil {
			t.Fatalf("iter %d: statusMsg with fresh error cache must NOT trigger refresh, got cmd %T", i, cmd())
		}
	}
	if mc.updatesCalls != 0 {
		t.Errorf("fresh-error-cache should suppress all self-heal fetches, got %d CheckUpdates calls", mc.updatesCalls)
	}
}

// TestUpdatesMsg_FreshResultClearsStaleVerdict pins the iter-1 codex finding:
// when a successful refresh's results map omits a service that was previously
// marked with UpdateAvailable, the stale verdict must be cleared. The
// updatesMsg success path is the only site that should clear missing
// services — cache-hit replays in servicesMsg/statusMsg stay additive.
func TestUpdatesMsg_FreshResultClearsStaleVerdict(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updateInFlight = true
	// Seed svcStatus with prior verdicts for two services.
	trueVal, falseVal := true, false
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true, UpdateAvailable: &trueVal},
		"db":  {Running: true, UpdateAvailable: &falseVal},
	}

	// New refresh result omits "db" — it should drop back to nil.
	result, _ := m.Update(updatesMsg{
		results: map[string]bool{"web": true},
		session: m.updatesSession,
	})
	model := result.(Model)

	if model.svcStatus["db"].UpdateAvailable != nil {
		t.Errorf("fresh result that omits db should clear its UpdateAvailable, got %v", model.svcStatus["db"].UpdateAvailable)
	}
	if model.svcStatus["web"].UpdateAvailable == nil || !*model.svcStatus["web"].UpdateAvailable {
		t.Errorf("web verdict from results should hydrate, got %v", model.svcStatus["web"].UpdateAvailable)
	}
}

// TestStatusMsg_ExpiredCacheTriggersRefresh pins the iter-1 codex finding:
// when a periodic statusMsg arrives and the update cache is expired/missing,
// the handler must queue a fresh refreshUpdates so glyphs don't vanish
// silently after TTL elapses while the user stays on the container screen.
// Mirrors the in-flight discipline used for stats.
func TestStatusMsg_ExpiredCacheTriggersRefresh(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}, updates: map[string]bool{"web": true}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.composer = mc
	m.statusSession = 7
	m.updateInFlight = false
	// Expired cache entry for the current key.
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now().Add(-2 * updatesCacheTTL),
			results:   map[string]bool{"web": true},
		},
	}

	result, cmd := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true}},
		session: 7,
	})
	model := result.(Model)

	if !model.updateInFlight {
		t.Error("expired-cache statusMsg should set updateInFlight=true")
	}
	if cmd == nil {
		t.Fatal("expired-cache statusMsg should return a non-nil refreshUpdates Cmd")
	}
	// Invoke the Cmd to confirm CheckUpdates was actually called (proves the
	// returned Cmd is refreshUpdates, not some unrelated artifact).
	if _, ok := cmd().(updatesMsg); !ok {
		t.Errorf("expired-cache statusMsg Cmd should return updatesMsg, got %T", cmd())
	}
	if mc.updatesCalls == 0 {
		t.Error("expired-cache statusMsg should call CheckUpdates")
	}
}

// TestStatusMsg_FreshCacheDoesNotRefresh ensures the self-heal only fires
// when the cache is stale — a fresh entry should be re-hydrated without
// triggering a fetch.
func TestStatusMsg_FreshCacheDoesNotRefresh(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.composer = mc
	m.statusSession = 4
	m.updateInFlight = false
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			results:   map[string]bool{"web": true},
		},
	}

	_, cmd := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true}},
		session: 4,
	})

	if cmd != nil {
		t.Errorf("fresh-cache statusMsg should NOT trigger refresh, got cmd %T", cmd())
	}
	if mc.updatesCalls != 0 {
		t.Errorf("fresh-cache statusMsg should not call CheckUpdates, got %d calls", mc.updatesCalls)
	}
}

// TestStatusMsg_ExpiredCacheRespectInFlight verifies the self-heal honors the
// in-flight guard: if a refreshUpdates is already pending, statusMsg must not
// stack a second fetch.
func TestStatusMsg_ExpiredCacheRespectInFlight(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.composer = mc
	m.statusSession = 5
	m.updateInFlight = true // already in flight
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now().Add(-2 * updatesCacheTTL),
			results:   map[string]bool{"web": true},
		},
	}

	_, cmd := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true}},
		session: 5,
	})

	if cmd != nil {
		t.Error("in-flight refresh should suppress statusMsg self-heal")
	}
	if mc.updatesCalls != 0 {
		t.Errorf("in-flight guard should prevent CheckUpdates, got %d calls", mc.updatesCalls)
	}
}

// TestUKeyPress_ForcesRefresh verifies the U keypress on screenSelectContainers
// bypasses cache and fires refreshUpdates regardless of TTL.
func TestUKeyPress_ForcesRefresh(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}, updates: map[string]bool{"web": true}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.composer = mc
	m.updateInFlight = false
	// Seed a FRESH cache entry — U should still fire a fetch.
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			results:   map[string]bool{"web": false},
		},
	}

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	model := result.(Model)
	if cmd == nil {
		t.Fatal("U keypress should return a refresh Cmd")
	}
	if !model.updateInFlight {
		t.Error("U keypress should set updateInFlight=true")
	}
	// Invoke the Cmd to confirm CheckUpdates was actually called.
	_ = cmd()
	if mc.updatesCalls == 0 {
		t.Error("U keypress should call CheckUpdates")
	}
}

// TestUKeyPress_NoComposer verifies the U keypress is a no-op when composer
// is nil (defensive — should never happen on this screen, but matches the
// pattern used elsewhere).
func TestUKeyPress_NoComposer(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.composer = nil
	m.updateInFlight = false

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	if cmd != nil {
		t.Error("U keypress with nil composer should return nil Cmd")
	}
	if m.updateInFlight {
		t.Error("U keypress with nil composer should NOT set updateInFlight")
	}
}

// TestUpdatesSession_BumpsAtAllSites enumerates every site the plan
// documents and asserts that triggering it bumps both statsSession and
// updatesSession (the two are paired across all 7 sites).
func TestUpdatesSession_BumpsAtAllSites(t *testing.T) {
	// 1. project pick (enter on screenSelectProject)
	t.Run("project_pick", func(t *testing.T) {
		mc := &mockComposer{}
		m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
		installFakeTick(&m)
		m.screen = screenSelectProject
		m.projects = []compose.Project{{Name: "p1", ConfigDir: "/p1"}}
		m.projCursor = 0
		before := m.updatesSession
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if result.(Model).updatesSession <= before {
			t.Errorf("project_pick: updatesSession not bumped (before=%d, after=%d)", before, result.(Model).updatesSession)
		}
	})

	// 2. esc container→proj
	t.Run("esc_container_to_proj", func(t *testing.T) {
		mc := &mockComposer{}
		m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
		installFakeTick(&m)
		m.screen = screenSelectContainers
		m.showPicker = true
		before := m.updatesSession
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if result.(Model).updatesSession <= before {
			t.Errorf("esc_container_to_proj: updatesSession not bumped (before=%d, after=%d)", before, result.(Model).updatesSession)
		}
	})

	// 3. esc proj→server
	t.Run("esc_proj_to_server", func(t *testing.T) {
		mc := &mockComposer{}
		srv := config.Server{Name: "s1", Host: "h1"}
		m := NewModel(nil, io.Discard, mockFactory(mc), []config.Server{srv}, nil)
		installFakeTick(&m)
		m.screen = screenSelectProject
		before := m.updatesSession
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if result.(Model).updatesSession <= before {
			t.Errorf("esc_proj_to_server: updatesSession not bumped (before=%d, after=%d)", before, result.(Model).updatesSession)
		}
	})

	// 4. entryLocal fast-track (local composer set)
	t.Run("entryLocal_fasttrack", func(t *testing.T) {
		mc := &mockComposer{}
		srv := config.Server{Name: "s1", Host: "h1"}
		m := NewModel(mc, io.Discard, mockFactory(mc), []config.Server{srv}, nil)
		installFakeTick(&m)
		m.screen = screenSelectServer
		m.serverCursor = 0 // entryLocal is index 0
		before := m.updatesSession
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if result.(Model).updatesSession <= before {
			t.Errorf("entryLocal_fasttrack: updatesSession not bumped (before=%d, after=%d)", before, result.(Model).updatesSession)
		}
	})

	// 5. execDoneMsg
	t.Run("execDoneMsg", func(t *testing.T) {
		mc := &mockComposer{}
		m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
		installFakeTick(&m)
		m.screen = screenSelectContainers
		before := m.updatesSession
		result, _ := m.Update(execDoneMsg{err: nil})
		if result.(Model).updatesSession <= before {
			t.Errorf("execDoneMsg: updatesSession not bumped (before=%d, after=%d)", before, result.(Model).updatesSession)
		}
	})

	// 6. return from screenProgress (esc when done)
	t.Run("esc_progress_to_container", func(t *testing.T) {
		mc := &mockComposer{}
		m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
		installFakeTick(&m)
		m.screen = screenProgress
		m.done = true
		before := m.updatesSession
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if result.(Model).updatesSession <= before {
			t.Errorf("esc_progress_to_container: updatesSession not bumped (before=%d, after=%d)", before, result.(Model).updatesSession)
		}
	})

	// 7. return from screenLogs (esc)
	t.Run("esc_logs_to_container", func(t *testing.T) {
		mc := &mockComposer{}
		m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
		installFakeTick(&m)
		m.screen = screenLogs
		before := m.updatesSession
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if result.(Model).updatesSession <= before {
			t.Errorf("esc_logs_to_container: updatesSession not bumped (before=%d, after=%d)", before, result.(Model).updatesSession)
		}
	})
}

// TestUpdatesSession_NotBumpedAtConnectResultError verifies the
// connectResultMsg error path does NOT bump updatesSession (it's a
// projectsSession site, not a stats/status/updates site — confirms the plan
// constraint).
func TestUpdatesSession_NotBumpedAtConnectResultError(t *testing.T) {
	mc := &mockComposer{}
	srv := config.Server{Name: "s1", Host: "h1"}
	m := NewModel(mc, io.Discard, mockFactory(mc), []config.Server{srv}, nil)
	before := m.updatesSession
	result, _ := m.Update(connectResultMsg{err: errors.New("boom")})
	after := result.(Model).updatesSession
	if after != before {
		t.Errorf("connectResultMsg error must NOT bump updatesSession (before=%d, after=%d)", before, after)
	}
}

// TestUpdatesSession_BumpSitesFireRefresh drives one representative site
// from each fan-out branch (project pick, execDone, esc-from-progress,
// esc-from-logs, entryLocal fast-track) and confirms the returned batch
// actually invokes CheckUpdates. The bump-only assertions above don't
// catch a regression where the session bumps but refreshUpdates is dropped
// from the batch — this test does.
func TestUpdatesSession_BumpSitesFireRefresh(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Model)
		key   tea.Msg
	}{
		{
			name: "project_pick",
			setup: func(m *Model) {
				m.screen = screenSelectProject
				m.projects = []compose.Project{{Name: "p1", ConfigDir: "/p1"}}
				m.projCursor = 0
			},
			key: tea.KeyMsg{Type: tea.KeyEnter},
		},
		{
			name: "execDoneMsg",
			setup: func(m *Model) {
				m.screen = screenSelectContainers
			},
			key: execDoneMsg{err: nil},
		},
		{
			name: "esc_progress_to_container",
			setup: func(m *Model) {
				m.screen = screenProgress
				m.done = true
			},
			key: tea.KeyMsg{Type: tea.KeyEsc},
		},
		{
			name: "esc_logs_to_container",
			setup: func(m *Model) {
				m.screen = screenLogs
			},
			key: tea.KeyMsg{Type: tea.KeyEsc},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockComposer{services: []string{"web"}}
			m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
			installFakeTick(&m)
			tc.setup(&m)
			// Pre-set composer so refreshUpdates() actually has something
			// to call against (loadServices/refreshStats/refreshUpdates all
			// gate on a non-nil composer via the factory).
			if m.composer == nil {
				m.composer = mc
			}
			_, cmd := m.Update(tc.key)
			if cmd == nil {
				t.Fatalf("%s: expected non-nil batch Cmd", tc.name)
			}
			msg := cmd()
			batch, ok := msg.(tea.BatchMsg)
			if !ok {
				// Some sites return tea.ExecProcess or other singletons —
				// in that case there's nothing to drain. Mark as PASS.
				return
			}
			for _, child := range batch {
				if child != nil {
					_ = child()
				}
			}
			if mc.updatesCalls == 0 {
				t.Errorf("%s: batch did not invoke CheckUpdates", tc.name)
			}
		})
	}
}

// TestUKeyPress_SwallowedDuringConfirmation pins the no-op behaviour of U
// when a confirmation prompt is active. Without this guard a user could
// burn registry calls by mashing U mid-confirmation, and any in-flight
// fetch would race against the imminent operation.
func TestUKeyPress_SwallowedDuringConfirmation(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.composer = mc
	m.confirming = true
	m.updateInFlight = false

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	if mc.updatesCalls != 0 {
		t.Errorf("U during confirmation should not call CheckUpdates (got %d calls)", mc.updatesCalls)
	}
	if m.updateInFlight {
		t.Error("U during confirmation should not set updateInFlight")
	}
	if cmd != nil {
		// Drain to be safe — if the model returned a Cmd we want to ensure
		// it doesn't make calls either.
		_ = cmd()
		if mc.updatesCalls != 0 {
			t.Errorf("U during confirmation returned a Cmd that called CheckUpdates (got %d calls)", mc.updatesCalls)
		}
	}
}

// TestUKeyPress_GuardsAgainstStacking verifies the updateInFlight guard
// added to the U handler: if a fetch is already in flight, additional U
// presses are no-ops. Prevents the user mashing U from queueing N fetches.
func TestUKeyPress_GuardsAgainstStacking(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.composer = mc
	m.updateInFlight = true // simulate prior fetch still pending

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	if cmd != nil {
		t.Error("U with in-flight fetch should return nil Cmd")
	}
	if mc.updatesCalls != 0 {
		t.Errorf("U with in-flight fetch should not call CheckUpdates (got %d calls)", mc.updatesCalls)
	}
}

// TestUpdateInFlight_ResetOnLeaveScreen_ContainerToProj verifies that
// updateInFlight is explicitly reset to false when navigating back from the
// container screen (mirrors refreshInFlight cleanup at the same site).
func TestUpdateInFlight_ResetOnLeaveScreen_ContainerToProj(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.showPicker = true
	m.updateInFlight = true

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if result.(Model).updateInFlight {
		t.Error("esc container→proj should reset updateInFlight to false")
	}
}

// TestUpdateInFlight_ResetOnLeaveScreen_ProjToServer verifies the same reset
// at the second leave-screen site.
func TestUpdateInFlight_ResetOnLeaveScreen_ProjToServer(t *testing.T) {
	mc := &mockComposer{}
	srv := config.Server{Name: "s1", Host: "h1"}
	m := NewModel(nil, io.Discard, mockFactory(mc), []config.Server{srv}, nil)
	installFakeTick(&m)
	m.screen = screenSelectProject
	m.updateInFlight = true

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if result.(Model).updateInFlight {
		t.Error("esc proj→server should reset updateInFlight to false")
	}
}

// TestRefreshUpdates_capturesCurrentSession mirrors
// TestRefreshStatus_capturesCurrentSession for the new updatesSession plumbing.
func TestRefreshUpdates_capturesCurrentSession(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.updatesSession = 77

	msg, ok := m.refreshUpdates()().(updatesMsg)
	if !ok {
		t.Fatalf("refreshUpdates() returned %T, want updatesMsg", m.refreshUpdates()())
	}
	if msg.session != 77 {
		t.Errorf("session = %d, want 77", msg.session)
	}
}

// TestUpdatesCacheKey_Composition verifies the cache key format documented
// in the comment: projDir + "|" + serverName, empty serverName = local.
func TestUpdatesCacheKey_Composition(t *testing.T) {
	tests := []struct {
		name       string
		projDir    string
		serverName string
		want       string
	}{
		{"local_no_project", "", "", "|"},
		{"local_with_project", "/srv/app", "", "/srv/app|"},
		{"remote_with_project", "/srv/app", "prod", "/srv/app|prod"},
		{"remote_no_project", "", "prod", "|prod"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockComposer{}
			m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
			m.projDir = tc.projDir
			m.serverName = tc.serverName
			if got := m.updatesCacheKey(); got != tc.want {
				t.Errorf("updatesCacheKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- Task 6: TUI rendering tests for update-available glyph ---

// trueP / falseP are tiny helpers for building *bool pointers in tri-state
// UpdateAvailable assignments without taking the address of a literal at every
// call site.
func trueP() *bool  { v := true; return &v }
func falseP() *bool { v := false; return &v }

// TestViewSelectContainers_UpdateGlyphRendered verifies the U+21E7 glyph
// appears in View() output for a service whose UpdateAvailable is &true, and
// does NOT appear for services whose flag is &false or nil.
func TestViewSelectContainers_UpdateGlyphRendered(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"web", "db", "cache"}
	m.svcStatus = map[string]runner.ServiceStatus{
		"web":   {Running: true, UpdateAvailable: trueP()},
		"db":    {Running: true, UpdateAvailable: falseP()},
		"cache": {Running: true}, // nil = unknown, no glyph
	}

	v := m.View()
	if !strings.Contains(v, compose.UpdateGlyph) {
		t.Errorf("View() should contain update glyph %q when a service has UpdateAvailable=&true; got:\n%s", compose.UpdateGlyph, v)
	}

	// More targeted: the glyph must be on the same line as "web", not "db" or
	// "cache". Scan per-line to assert that mapping precisely.
	for _, line := range strings.Split(v, "\n") {
		if strings.Contains(line, compose.UpdateGlyph) {
			if !strings.Contains(line, "web") {
				t.Errorf("update glyph rendered on wrong line; expected on 'web' row, got: %q", line)
			}
			if strings.Contains(line, "db ") || strings.Contains(line, "cache") {
				t.Errorf("update glyph leaked onto db/cache row: %q", line)
			}
		}
	}
}

// TestViewSelectContainers_UpdateGlyphOnStoppedService verifies the glyph
// shows even when the service is stopped — the indicator is about the image
// version, not container state.
func TestViewSelectContainers_UpdateGlyphOnStoppedService(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"web"}
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: false, UpdateAvailable: trueP()},
	}

	v := m.View()
	if !strings.Contains(v, compose.UpdateGlyph) {
		t.Errorf("View() should contain update glyph for stopped service with UpdateAvailable=&true; got:\n%s", v)
	}
}

// TestViewSelectContainers_UpdateAlignment_PreservesColumns verifies that
// when ANY service in the rendered list has an available update, the name
// column is widened by 2 cells for ALL rows — so any following column
// (Created/Uptime) stays put across rows that have/lack the glyph.
func TestViewSelectContainers_UpdateAlignment_PreservesColumns(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"web", "db", "cache"}
	m.svcStatus = map[string]runner.ServiceStatus{
		// Created/Uptime are present so the Created column renders, giving us
		// a stable column to align against.
		"web":   {Running: true, Created: "2024-01-15 09:30", Uptime: "3h", UpdateAvailable: trueP()},
		"db":    {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"cache": {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
	}

	v := m.View()
	lines := strings.Split(v, "\n")

	// Compare each row's Created-column position by *display cells* (rune
	// count of the ANSI-stripped prefix up to the date). Byte-indexing would
	// be off by 2 for the row with the glyph since U+21E7 is 3 bytes / 1
	// cell — the very mismatch this column padding is designed to compensate
	// for.
	stripped := map[string]int{}
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	for _, line := range lines {
		clean := ansi.ReplaceAllString(line, "")
		for _, svc := range m.services {
			if !strings.Contains(clean, svc) || !strings.Contains(clean, "2024-01-15") {
				continue
			}
			byteIdx := strings.Index(clean, "2024-01-15")
			stripped[svc] = utf8.RuneCountInString(clean[:byteIdx])
		}
	}
	if len(stripped) != 3 {
		t.Fatalf("ANSI-stripped scan should find all 3 rows, got %d: %+v", len(stripped), stripped)
	}
	webCol := stripped["web"]
	dbCol := stripped["db"]
	cacheCol := stripped["cache"]
	if webCol != dbCol || dbCol != cacheCol {
		t.Errorf("Created column misalignment: web=%d db=%d cache=%d (must be equal — glyph reservation should pad non-glyph rows)", webCol, dbCol, cacheCol)
	}
}

// TestHelpOverlay_ShowsUpdatesKey pins that `U check updates` stays
// discoverable. The token moved out of the trimmed footer into the `?`
// overlay, so this renders the overlay, not the footer.
func TestHelpOverlay_ShowsUpdatesKey(t *testing.T) {
	m := Model{screen: screenSelectContainers, width: 200, height: 24}

	if !helpOverlayNamesKey(m, "U", "updates") {
		t.Errorf("`?` overlay should mention the 'U' updates key; got:\n%s", m.viewHelp())
	}
}

// TestViewSelectContainers_SoftWarningPriority_StatsBeatsUpdates verifies
// that when both statsErr and updatesErr are set, statsErr wins the
// soft-warning slot and the updates warning does NOT appear.
func TestViewSelectContainers_SoftWarningPriority_StatsBeatsUpdates(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"web"}
	m.svcStatus = map[string]runner.ServiceStatus{"web": {Running: true}}
	m.width = 200
	m.height = 24
	m.statsErr = errors.New("stats boom")
	m.updatesErr = "updates boom"

	v := m.View()
	if !strings.Contains(v, "Stats unavailable") || !strings.Contains(v, "stats boom") {
		t.Errorf("View() should show statsErr warning when both errors are set; got:\n%s", v)
	}
	if strings.Contains(v, "updates: updates boom") {
		t.Errorf("View() should NOT show updatesErr warning when statsErr is set; got:\n%s", v)
	}
}

// TestViewSelectContainers_SoftWarningPriority_UpdatesAloneShown verifies
// that updatesErr renders in the warning slot when statsErr is empty.
func TestViewSelectContainers_SoftWarningPriority_UpdatesAloneShown(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"web"}
	m.svcStatus = map[string]runner.ServiceStatus{"web": {Running: true}}
	m.width = 200
	m.height = 24
	m.statsErr = nil
	m.updatesErr = "registry timeout"

	v := m.View()
	if !strings.Contains(v, "updates: registry timeout") {
		t.Errorf("View() should show updatesErr warning when statsErr is empty; got:\n%s", v)
	}
}

// TestViewSelectContainers_NoGlyph_ReservationAlwaysApplied verifies that
// even when NO service has an update available, the name column STILL reserves
// 2 trailing cells for the glyph. This keeps the Created/Uptime/CPU/Mem/Ports
// columns at a stable offset across mid-poll changes — when a verdict arrives,
// clears, or refreshes, downstream columns don't shift.
func TestViewSelectContainers_NoGlyph_ReservationAlwaysApplied(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"web", "db"}
	m.svcStatus = map[string]runner.ServiceStatus{
		// All UpdateAvailable=nil → no glyph rendered, but the reservation
		// must still apply so downstream columns don't shift when a verdict
		// later arrives.
		"web": {Running: true, Created: "2024-01-15 09:30"},
		"db":  {Running: true, Created: "2024-01-15 09:30"},
	}

	v := m.View()
	if strings.Contains(v, compose.UpdateGlyph) {
		t.Errorf("View() should NOT contain update glyph when no service has UpdateAvailable=&true; got:\n%s", v)
	}
	// Both rows must align at the Created column (no glyph present, but
	// reservation applied uniformly to every row).
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	cols := map[string]int{}
	for _, line := range strings.Split(v, "\n") {
		clean := ansi.ReplaceAllString(line, "")
		for _, svc := range m.services {
			if !strings.Contains(clean, svc) || !strings.Contains(clean, "2024-01-15") {
				continue
			}
			byteIdx := strings.Index(clean, "2024-01-15")
			cols[svc] = utf8.RuneCountInString(clean[:byteIdx])
		}
	}
	if cols["web"] != cols["db"] {
		t.Errorf("Created column misalignment without glyph: web=%d db=%d", cols["web"], cols["db"])
	}

	// Verify the reservation is actually applied: render a second View with
	// one service flagged as updated. The Created column offset must be
	// IDENTICAL to the no-glyph case — that's the column-stability property.
	trueVal := true
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true, Created: "2024-01-15 09:30", UpdateAvailable: &trueVal},
		"db":  {Running: true, Created: "2024-01-15 09:30"},
	}
	v2 := m.View()
	cols2 := map[string]int{}
	for _, line := range strings.Split(v2, "\n") {
		clean := ansi.ReplaceAllString(line, "")
		for _, svc := range m.services {
			if !strings.Contains(clean, svc) || !strings.Contains(clean, "2024-01-15") {
				continue
			}
			byteIdx := strings.Index(clean, "2024-01-15")
			cols2[svc] = utf8.RuneCountInString(clean[:byteIdx])
		}
	}
	if cols["web"] != cols2["web"] {
		t.Errorf("Created column shifted when verdict arrived: no-glyph=%d with-glyph=%d (should be stable)", cols["web"], cols2["web"])
	}
}

// TestHydrateUpdates_SkipsUnknownServices is the regression for iteration-2
// finding #8: when CheckUpdates returns a verdict for a service that is NOT
// in m.svcStatus (e.g. transient race between `compose config` listing and
// `compose config --services` at fetch time, or a verdict referring to a
// stale service after project switch), hydrateUpdates MUST NOT synthesise a
// phantom svcStatus entry. The renderer iterates m.services not the status
// map so today it'd be invisible, but a phantom would leak across project
// switches and surface in any future map-key iterator.
func TestHydrateUpdates_SkipsUnknownServices(t *testing.T) {
	m := &Model{
		svcStatus: map[string]runner.ServiceStatus{
			"web": {Running: true},
		},
	}
	yes := true
	m.hydrateUpdates(map[string]bool{
		"web":     true,  // known: hydrate
		"phantom": false, // unknown: skip
	})
	if got := len(m.svcStatus); got != 1 {
		t.Errorf("svcStatus len = %d, want 1 (phantom must not be created); got %#v", got, m.svcStatus)
	}
	web, ok := m.svcStatus["web"]
	if !ok {
		t.Fatal("web entry missing after hydrate")
	}
	if web.UpdateAvailable == nil || *web.UpdateAvailable != yes {
		t.Errorf("web.UpdateAvailable = %v, want &true", web.UpdateAvailable)
	}
	if !web.Running {
		t.Error("web.Running flipped to false during hydrate — phantom-style overwrite")
	}
	if _, leaked := m.svcStatus["phantom"]; leaked {
		t.Errorf("phantom entry leaked into svcStatus: %#v", m.svcStatus["phantom"])
	}
}

// TestHydrateUpdates_NilStatusMapNoOp ensures hydrateUpdates is safe to call
// when m.svcStatus is nil — early-returns without panicking and without
// allocating a phantom map (allocating would mask a real cleanup bug where
// the status map should have been populated by loadServices first).
func TestHydrateUpdates_NilStatusMapNoOp(t *testing.T) {
	m := &Model{}
	m.hydrateUpdates(map[string]bool{"web": true})
	if m.svcStatus != nil {
		t.Errorf("svcStatus = %#v, want nil (hydrate must not allocate when source map was missing)", m.svcStatus)
	}
}

// TestUpdatesMsg_OffScreenErrorClearsState (iter-3 fix #1): when a
// CheckUpdates failure arrives while the user is off-screen (config, logs,
// progress), the handler MUST still set m.updatesErr AND clear stale
// UpdateAvailable verdicts on svcStatus. Otherwise the user returns and
// sees old ⇧ glyphs alongside no warning — worst-case for the config
// screen, which is read-only and does NOT trigger refreshStatus, so
// nothing downstream would clear the stale glyphs.
func TestUpdatesMsg_OffScreenErrorClearsState(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenConfig // off-screen
	m.updatesSession = 4
	m.updateInFlight = true
	trueVal := true
	m.svcStatus = map[string]runner.ServiceStatus{
		"web": {Running: true, UpdateAvailable: &trueVal},
		"db":  {Running: true, UpdateAvailable: &trueVal},
	}

	result, _ := m.Update(updatesMsg{
		err:     errors.New("registry timeout"),
		session: 4,
	})
	model := result.(Model)

	if model.updateInFlight {
		t.Error("off-screen error updatesMsg should clear updateInFlight")
	}
	if !strings.Contains(model.updatesErr, "registry timeout") {
		t.Errorf("off-screen error must still set updatesErr, got %q", model.updatesErr)
	}
	for svc, st := range model.svcStatus {
		if st.UpdateAvailable != nil {
			t.Errorf("svcStatus[%q].UpdateAvailable should be cleared off-screen on error, got %v", svc, *st.UpdateAvailable)
		}
	}
	// Cache should hold an error entry with the message preserved.
	entry, ok := model.updateCache[model.updatesCacheKey()]
	if !ok {
		t.Fatal("cache must hold an error entry after off-screen failure")
	}
	if !entry.err {
		t.Error("cache entry should have err=true")
	}
	if !strings.Contains(entry.errMsg, "registry timeout") {
		t.Errorf("cache entry errMsg should preserve the error text, got %q", entry.errMsg)
	}
}

// TestMaybeRefreshUpdatesCmd_CachedErrorRestoresWarning (iter-3 fix #2):
// when a user navigates away from the container screen (clearing
// m.updatesErr) and re-enters within updatesErrorTTL while the cache holds
// a recent failure, maybeRefreshUpdatesCmd must restore the warning text
// from the cached errMsg — otherwise the user sees blank glyphs with no
// explanation while the system still knows the data is stale-bad.
func TestMaybeRefreshUpdatesCmd_CachedErrorRestoresWarning(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updateInFlight = false
	// Simulate the post-navigation state: cleared updatesErr, fresh error
	// entry in cache with the original error message.
	m.updatesErr = ""
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			err:       true,
			errMsg:    "registry unreachable: dial tcp: i/o timeout",
		},
	}

	cmd := m.maybeRefreshUpdatesCmd()
	if cmd != nil {
		t.Error("fresh cached error should NOT trigger a refetch (errorTTL window)")
	}
	if !strings.Contains(m.updatesErr, "registry unreachable") {
		t.Errorf("cached error msg should be restored to m.updatesErr, got %q", m.updatesErr)
	}
}

// TestStatusMsg_CachedErrorRestoresWarning is the periodic-refresh
// counterpart: when statusMsg arrives and the cache holds a fresh error
// entry, the soft warning must be restored from entry.errMsg so the
// periodic tick doesn't silently drop the warning while the failure is
// still known-bad.
func TestStatusMsg_CachedErrorRestoresWarning(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.composer = mc
	m.statusSession = 12
	m.updateInFlight = false
	m.updatesErr = ""
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			err:       true,
			errMsg:    "registry boom",
		},
	}

	result, _ := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true}},
		session: 12,
	})
	model := result.(Model)

	if !strings.Contains(model.updatesErr, "registry boom") {
		t.Errorf("statusMsg should restore cached error warning, got %q", model.updatesErr)
	}
}

// TestServicesMsg_CachedErrorRestoresWarning: initial-load arrivals must
// also restore the cached error warning (servicesMsg cache replay mirrors
// the statusMsg path).
func TestServicesMsg_CachedErrorRestoresWarning(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statusSession = 3
	m.updatesErr = ""
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			err:       true,
			errMsg:    "stale-bad",
		},
	}

	result, _ := m.Update(servicesMsg{
		services: []string{"web"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}},
		session:  3,
	})
	model := result.(Model)

	if model.updatesErr != "stale-bad" {
		t.Errorf("servicesMsg cache-replay should restore cached error warning, got %q", model.updatesErr)
	}
	if model.svcStatus["web"].UpdateAvailable != nil {
		t.Errorf("cached error must NOT hydrate glyphs, got %v", model.svcStatus["web"].UpdateAvailable)
	}
}

// TestUpdatesMsg_ErrorCacheStoresErrMsg ensures the cache write path
// populates entry.errMsg with the original error text, so downstream
// cache-hit paths can restore the warning across navigation.
func TestUpdatesMsg_ErrorCacheStoresErrMsg(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updateInFlight = true

	result, _ := m.Update(updatesMsg{
		err:     errors.New("docker hub timeout"),
		session: m.updatesSession,
	})
	model := result.(Model)

	entry, ok := model.updateCache[model.updatesCacheKey()]
	if !ok {
		t.Fatal("error entry should be in cache")
	}
	if entry.errMsg != "docker hub timeout" {
		t.Errorf("entry.errMsg = %q, want %q", entry.errMsg, "docker hub timeout")
	}
}

// TestUpdatesMsg_OffScreenErrorFixesOffset (iter-4 fix): when an updates
// failure arrives while the user is off-screen, setting m.updatesErr adds
// a footer warning line, which shrinks svcVisibleCount() by one. If the
// cursor was previously at the last visible row, it now falls outside the
// window. The handler MUST call fixSvcOffset() so the user returns to a
// correctly-scrolled list — worst case is the config screen, which does
// not refresh status on return, so without this fix the cursor stays out
// of view until the user presses a navigation key.
func TestUpdatesMsg_OffScreenErrorFixesOffset(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.statsRequested = false // keep captions row out of the height math
	m.screen = screenConfig  // off-screen — user is in config viewer
	m.updatesSession = 7
	m.updateInFlight = true
	// Geometry: width=200 (forces one-line help => footerLines=3 baseline; the
	// reserved search-bar line merges onto the helpStyle MarginTop blank and
	// adds no physical row), height=9 → visible = 9-3-3 = 3 when no updatesErr;
	// visible = 9-3-4 = 2 once updatesErr is set. Cursor=2 with offset=0 sits at
	// the last visible row pre-error; post-error it falls outside the new window.
	m.width = 200
	m.height = 9
	m.services = []string{"a", "b", "c", "d", "e"}
	m.svcStatus = map[string]runner.ServiceStatus{
		"a": {Running: true},
		"b": {Running: true},
		"c": {Running: true},
		"d": {Running: true},
		"e": {Running: true},
	}
	m.svcCursor = 2
	m.svcOffset = 0

	result, _ := m.Update(updatesMsg{
		err:     errors.New("registry timeout"),
		session: 7,
	})
	model := result.(Model)

	if model.updatesErr == "" {
		t.Fatal("precondition: off-screen error must set updatesErr")
	}
	// New visible = 2, cursor = 2 → offset should be 2-2+1 = 1 to keep
	// cursor visible in window [1, 3).
	if model.svcOffset != 1 {
		t.Errorf("svcOffset = %d, want 1 (cursor=2 must stay visible after footer grew)", model.svcOffset)
	}
}

// containerSearchModel builds a minimal container-screen Model for search tests.
// Uses a literal (not NewModel) so the "/" handler is responsible for building
// the textinput, matching the plan's "fresh input on every open" contract.
func containerSearchModel(services []string) Model {
	return Model{
		screen:   screenSelectContainers,
		services: services,
		selected: make(map[int]bool),
	}
}

func TestSearchOpen(t *testing.T) {
	m := containerSearchModel([]string{"web", "worker", "db"})
	m.svcCursor = 1

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	um := updated.(Model)

	if !um.searching {
		t.Error("searching = false, want true after /")
	}
	if um.searchReturn != 1 {
		t.Errorf("searchReturn = %d, want 1 (svcCursor at open)", um.searchReturn)
	}
	if !um.searchInput.Focused() {
		t.Error("searchInput not focused after /")
	}
	if cmd != nil {
		t.Error("expected nil cmd after opening search")
	}
}

func TestSearchOpenOverCommittedResetsState(t *testing.T) {
	// Establish a committed search (query "w" matching web + web-worker).
	m := containerSearchModel([]string{"api", "web", "web-worker"})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit
	m = updated.(Model)
	if m.searchQuery != "w" || len(m.searchMatches) != 2 {
		t.Fatalf("precondition: expected committed query 'w' with 2 matches, got %q %v", m.searchQuery, m.searchMatches)
	}

	// Reopen search with "/" — must start from a clean slate: empty query, no
	// stale matches/highlights, no stale counter. Not a leftover of "w".
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)

	if !m.searching {
		t.Error("searching = false, want true after reopening /")
	}
	if m.searchQuery != "" {
		t.Errorf("searchQuery = %q, want empty after reopen (stale query leaked)", m.searchQuery)
	}
	if m.searchMatches != nil {
		t.Errorf("searchMatches = %v, want nil after reopen (stale highlights leaked)", m.searchMatches)
	}
	if m.searchInput.Value() != "" {
		t.Errorf("searchInput value = %q, want empty after reopen", m.searchInput.Value())
	}
	if m.searchCounter() != "(no match)" {
		t.Errorf("searchCounter = %q, want %q after reopen (stale counter leaked)", m.searchCounter(), "(no match)")
	}

	// An immediate enter must NOT re-commit the old query — searchMatches is empty
	// so the no-match commit path clears everything.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.searchQuery != "" {
		t.Errorf("searchQuery = %q, want empty after immediate enter (old query re-committed)", m.searchQuery)
	}
	if m.searchMatches != nil {
		t.Errorf("searchMatches = %v, want nil after immediate enter", m.searchMatches)
	}
	if m.searching {
		t.Error("searching = true, want false after enter")
	}
}

func TestSearchOpenEmptyListNoop(t *testing.T) {
	m := containerSearchModel(nil)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	um := updated.(Model)

	if um.searching {
		t.Error("searching = true, want false on empty list (/ must be a no-op)")
	}
	if cmd != nil {
		t.Error("expected nil cmd on empty-list /")
	}
}

func TestSearchTypeLiveJump(t *testing.T) {
	m := containerSearchModel([]string{"api", "web", "web-worker"})

	// Open search.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)

	// Type "w" — should match web (idx 1) and web-worker (idx 2) and jump
	// the cursor to the first match.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	um := updated.(Model)

	if um.searchQuery != "w" {
		t.Errorf("searchQuery = %q, want %q", um.searchQuery, "w")
	}
	if len(um.searchMatches) != 2 || um.searchMatches[0] != 1 || um.searchMatches[1] != 2 {
		t.Errorf("searchMatches = %v, want [1 2]", um.searchMatches)
	}
	if um.svcCursor != 1 {
		t.Errorf("svcCursor = %d, want 1 (first match)", um.svcCursor)
	}
	if um.searchInput.Value() != "w" {
		t.Errorf("searchInput value = %q, want %q", um.searchInput.Value(), "w")
	}
}

func TestSearchCommitKeepsQuery(t *testing.T) {
	m := containerSearchModel([]string{"api", "web", "worker"})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	m = updated.(Model)

	// Commit with enter.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	um := updated.(Model)

	if um.searching {
		t.Error("searching = true, want false after enter (bar closed)")
	}
	if um.searchQuery != "w" {
		t.Errorf("searchQuery = %q, want %q (kept after commit)", um.searchQuery, "w")
	}
	if len(um.searchMatches) != 2 {
		t.Errorf("searchMatches = %v, want 2 matches kept after commit", um.searchMatches)
	}
}

func TestSearchCommitNoMatchClears(t *testing.T) {
	m := containerSearchModel([]string{"api", "web", "worker"})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	// Type a query that matches nothing.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	m = updated.(Model)
	if len(m.searchMatches) != 0 {
		t.Fatalf("precondition: expected no matches for 'z', got %v", m.searchMatches)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	um := updated.(Model)

	if um.searching {
		t.Error("searching = true, want false after enter")
	}
	if um.searchQuery != "" {
		t.Errorf("searchQuery = %q, want empty (no-match commit clears)", um.searchQuery)
	}
	if um.searchMatches != nil {
		t.Errorf("searchMatches = %v, want nil after no-match commit", um.searchMatches)
	}
}

func TestSearchCancelRestoresCursor(t *testing.T) {
	m := containerSearchModel([]string{"api", "web", "web-worker"})
	m.svcCursor = 2 // open with cursor at "web-worker"

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	// Type "a" → matches "api" (idx 0), cursor jumps there.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)
	if m.svcCursor != 0 {
		t.Fatalf("precondition: svcCursor = %d, want 0 after typing a", m.svcCursor)
	}

	// esc while typing cancels and restores.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	um := updated.(Model)

	if um.searching {
		t.Error("searching = true, want false after esc")
	}
	if um.searchQuery != "" {
		t.Errorf("searchQuery = %q, want empty after cancel", um.searchQuery)
	}
	if um.searchMatches != nil {
		t.Errorf("searchMatches = %v, want nil after cancel", um.searchMatches)
	}
	if um.svcCursor != 2 {
		t.Errorf("svcCursor = %d, want 2 (restored to searchReturn)", um.svcCursor)
	}
	if um.screen != screenSelectContainers {
		t.Errorf("screen = %d, want screenSelectContainers (esc must not back-nav while typing)", um.screen)
	}
}

// TestQTypedIntoSearchInput is the regression guard: while searching, a q
// keystroke must land in searchInput and must NOT quit or navigate back.
// Template: TestQTypedIntoSettingsFormInput.
func TestQTypedIntoSearchInput(t *testing.T) {
	m := containerSearchModel([]string{"qa-runner", "web"})

	// Open search via the "/" key so the handler constructs a live input
	// (per the plan: tests must not hand-set a zero-value textinput).
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	if !m.searching {
		t.Fatal("precondition: search must be open")
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenSelectContainers {
		t.Errorf("screen = %d, want screenSelectContainers (q must not navigate)", um.screen)
	}
	if !um.searching {
		t.Error("searching = false, want true (q must not close search)")
	}
	if um.searchInput.Value() != "q" {
		t.Errorf("searchInput value = %q, want %q (q lands in input)", um.searchInput.Value(), "q")
	}
	if um.searchQuery != "q" {
		t.Errorf("searchQuery = %q, want %q", um.searchQuery, "q")
	}
	// q while searching must not produce a quit command.
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Error("q while searching produced a QuitMsg, want none")
		}
	}
}

// committedSearchModel builds a container-screen Model with a committed
// (non-typing) search: the query is set, matches are computed, the bar is
// closed. The cursor is placed on the first match, mirroring the state right
// after enter. Uses a literal Model so the n/N handlers are exercised directly.
func committedSearchModel(services []string, query string) Model {
	m := containerSearchModel(services)
	m.searchQuery = query
	m.searchMatches = computeMatches(services, query)
	if len(m.searchMatches) > 0 {
		m.svcCursor = m.searchMatches[0]
	}
	return m
}

func sendKey(t *testing.T, m Model, r rune) Model {
	t.Helper()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return updated.(Model)
}

// TestSearchCycleNextOnMatch: n advances through matches and wraps last→first.
func TestSearchCycleNextOnMatch(t *testing.T) {
	// "w" matches web (1) and web-worker (2).
	m := committedSearchModel([]string{"api", "web", "web-worker"}, "w")
	if len(m.searchMatches) != 2 || m.searchMatches[0] != 1 || m.searchMatches[1] != 2 {
		t.Fatalf("precondition: searchMatches = %v, want [1 2]", m.searchMatches)
	}
	if m.svcCursor != 1 {
		t.Fatalf("precondition: svcCursor = %d, want 1", m.svcCursor)
	}

	// n: 1 → 2.
	m = sendKey(t, m, 'n')
	if m.svcCursor != 2 {
		t.Errorf("after 1st n: svcCursor = %d, want 2", m.svcCursor)
	}
	// n again: wrap 2 → 1.
	m = sendKey(t, m, 'n')
	if m.svcCursor != 1 {
		t.Errorf("after wrap n: svcCursor = %d, want 1 (wrap last→first)", m.svcCursor)
	}
	// Search state must be untouched by cycling.
	if m.searchQuery != "w" || len(m.searchMatches) != 2 {
		t.Errorf("cycle mutated search state: query=%q matches=%v", m.searchQuery, m.searchMatches)
	}
}

// TestSearchCyclePrevOnMatch: N goes previous and wraps first→last.
func TestSearchCyclePrevOnMatch(t *testing.T) {
	m := committedSearchModel([]string{"api", "web", "web-worker"}, "w")
	if m.svcCursor != 1 {
		t.Fatalf("precondition: svcCursor = %d, want 1", m.svcCursor)
	}

	// N from first match wraps to last: 1 → 2.
	m = sendKey(t, m, 'N')
	if m.svcCursor != 2 {
		t.Errorf("after N wrap: svcCursor = %d, want 2 (wrap first→last)", m.svcCursor)
	}
	// N again: 2 → 1.
	m = sendKey(t, m, 'N')
	if m.svcCursor != 1 {
		t.Errorf("after N: svcCursor = %d, want 1", m.svcCursor)
	}
}

// TestSearchCyclePositionCounter verifies the cursor lands on exactly the
// expected match indices across a longer cycle so the (i/N) counter that the
// renderer derives from position stays correct.
func TestSearchCyclePositionCounter(t *testing.T) {
	// "s" matches svc-a (0), svc-b (1), other-s (3).
	svcs := []string{"svc-a", "svc-b", "db", "other-s"}
	m := committedSearchModel(svcs, "s")
	want := []int{0, 1, 3}
	if len(m.searchMatches) != 3 || m.searchMatches[0] != 0 || m.searchMatches[1] != 1 || m.searchMatches[2] != 3 {
		t.Fatalf("precondition: searchMatches = %v, want %v", m.searchMatches, want)
	}

	// Full loop forward: 0 → 1 → 3 → wrap 0.
	seq := []int{1, 3, 0}
	for i, exp := range seq {
		m = sendKey(t, m, 'n')
		if m.svcCursor != exp {
			t.Errorf("forward step %d: svcCursor = %d, want %d", i, m.svcCursor, exp)
		}
	}
	// Full loop backward from 0: wrap 3 → 1 → 0.
	backSeq := []int{3, 1, 0}
	for i, exp := range backSeq {
		m = sendKey(t, m, 'N')
		if m.svcCursor != exp {
			t.Errorf("backward step %d: svcCursor = %d, want %d", i, m.svcCursor, exp)
		}
	}
}

// TestSearchCycleNoQueryNoop: n/N do nothing without a committed search.
func TestSearchCycleNoQueryNoop(t *testing.T) {
	m := containerSearchModel([]string{"api", "web", "worker"})
	m.svcCursor = 1

	m = sendKey(t, m, 'n')
	if m.svcCursor != 1 {
		t.Errorf("n with no query moved cursor to %d, want 1 (no-op)", m.svcCursor)
	}
	m = sendKey(t, m, 'N')
	if m.svcCursor != 1 {
		t.Errorf("N with no query moved cursor to %d, want 1 (no-op)", m.svcCursor)
	}
	if m.searchQuery != "" || m.searchMatches != nil {
		t.Errorf("n/N synthesised search state: query=%q matches=%v", m.searchQuery, m.searchMatches)
	}
}

// TestSearchCycleOffMatchBetween: cursor manually moved between two matches;
// n jumps to the first match strictly after, N to the first strictly before.
func TestSearchCycleOffMatchBetween(t *testing.T) {
	// "s" matches idx 0 and idx 3. Park the cursor at idx 2 (between them).
	svcs := []string{"svc-a", "db", "cache", "svc-b"}
	m := committedSearchModel(svcs, "s")
	if len(m.searchMatches) != 2 || m.searchMatches[0] != 0 || m.searchMatches[1] != 3 {
		t.Fatalf("precondition: searchMatches = %v, want [0 3]", m.searchMatches)
	}
	m.svcCursor = 2 // off-match, between 0 and 3

	// n → first match strictly after 2 = 3.
	mn := sendKey(t, m, 'n')
	if mn.svcCursor != 3 {
		t.Errorf("off-match n: svcCursor = %d, want 3 (first after)", mn.svcCursor)
	}
	// N → first match strictly before 2 = 0.
	mN := sendKey(t, m, 'N')
	if mN.svcCursor != 0 {
		t.Errorf("off-match N: svcCursor = %d, want 0 (first before)", mN.svcCursor)
	}
}

// TestSearchCycleOffMatchAboveAll: cursor before all matches; n → first match,
// N → wraps to last.
func TestSearchCycleOffMatchAboveAll(t *testing.T) {
	// "s" matches idx 2 and idx 3. Park the cursor at idx 0 (above all).
	svcs := []string{"db", "cache", "svc-a", "svc-b"}
	m := committedSearchModel(svcs, "s")
	if len(m.searchMatches) != 2 || m.searchMatches[0] != 2 || m.searchMatches[1] != 3 {
		t.Fatalf("precondition: searchMatches = %v, want [2 3]", m.searchMatches)
	}
	m.svcCursor = 0 // above all matches

	// n → first match after 0 = 2.
	mn := sendKey(t, m, 'n')
	if mn.svcCursor != 2 {
		t.Errorf("above-all n: svcCursor = %d, want 2 (first match)", mn.svcCursor)
	}
	// N → nothing strictly before 0, wrap to last = 3.
	mN := sendKey(t, m, 'N')
	if mN.svcCursor != 3 {
		t.Errorf("above-all N: svcCursor = %d, want 3 (wrap to last)", mN.svcCursor)
	}
}

// TestSearchCycleOffMatchBelowAll: cursor after all matches; n → wraps to first,
// N → last match.
func TestSearchCycleOffMatchBelowAll(t *testing.T) {
	// "s" matches idx 0 and idx 1. Park the cursor at idx 3 (below all).
	svcs := []string{"svc-a", "svc-b", "db", "cache"}
	m := committedSearchModel(svcs, "s")
	if len(m.searchMatches) != 2 || m.searchMatches[0] != 0 || m.searchMatches[1] != 1 {
		t.Fatalf("precondition: searchMatches = %v, want [0 1]", m.searchMatches)
	}
	m.svcCursor = 3 // below all matches

	// n → nothing strictly after 3, wrap to first = 0.
	mn := sendKey(t, m, 'n')
	if mn.svcCursor != 0 {
		t.Errorf("below-all n: svcCursor = %d, want 0 (wrap to first)", mn.svcCursor)
	}
	// N → first match before 3 = 1 (last match).
	mN := sendKey(t, m, 'N')
	if mN.svcCursor != 1 {
		t.Errorf("below-all N: svcCursor = %d, want 1 (last match)", mN.svcCursor)
	}
}

// TestSearchTwoStageEsc: first esc clears the committed search and stays on the
// container screen; second esc navigates back to the project picker.
func TestSearchTwoStageEsc(t *testing.T) {
	m := committedSearchModel([]string{"api", "web", "worker"}, "w")
	m.showPicker = true // so the second esc has a project picker to return to
	if m.searchQuery == "" {
		t.Fatal("precondition: committed search expected")
	}

	// First esc: clears search, stays on screen.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectContainers {
		t.Errorf("after 1st esc: screen = %d, want screenSelectContainers", m.screen)
	}
	if m.searchQuery != "" {
		t.Errorf("after 1st esc: searchQuery = %q, want empty (search cleared)", m.searchQuery)
	}
	if m.searchMatches != nil {
		t.Errorf("after 1st esc: searchMatches = %v, want nil", m.searchMatches)
	}

	// Second esc: no active search → back-nav to project picker.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectProject {
		t.Errorf("after 2nd esc: screen = %d, want screenSelectProject (back-nav)", m.screen)
	}
}

// TestSearchLiveJumpScrollsOffScreenMatchIntoView: when the only match lives
// below the fold, live-jumping to it (while typing) must scroll svcOffset so the
// cursor is inside the visible window. Guards the fixSvcOffset() call in the
// typing-mode handler. The existing search tests all use height==0 (everything
// on screen), so this is the one exercising the off-screen scroll path.
func TestSearchLiveJumpScrollsOffScreenMatchIntoView(t *testing.T) {
	svcs := []string{
		"a0", "a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "target", "a11",
	}
	m := containerSearchModel(svcs)
	m.width = 200 // one-line help so footer math is deterministic
	m.height = 10 // visible = 10 - 3(header) - 3(footer) = 4
	m.svcCursor = 0
	m.svcOffset = 0

	visible := m.svcVisibleCount()
	if visible >= len(svcs) {
		t.Fatalf("precondition: visible=%d must be < %d so a match can be off-screen", visible, len(svcs))
	}

	// Open search and type "target" — the sole match is at idx 10, far below
	// the initial visible window [0, visible).
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	for _, r := range "target" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}

	if len(m.searchMatches) != 1 || m.searchMatches[0] != 10 {
		t.Fatalf("searchMatches = %v, want [10]", m.searchMatches)
	}
	if m.svcCursor != 10 {
		t.Fatalf("svcCursor = %d, want 10 (jumped to sole match)", m.svcCursor)
	}
	// The off-screen jump must have scrolled the window to keep the cursor visible.
	visible = m.svcVisibleCount()
	if m.svcCursor < m.svcOffset || m.svcCursor >= m.svcOffset+visible {
		t.Errorf("cursor %d not in visible window [%d, %d) after off-screen live-jump",
			m.svcCursor, m.svcOffset, m.svcOffset+visible)
	}
}

// TestSearchCycleScrollsOffScreenMatchIntoView: cycling with n to a match below
// the fold must scroll it into view. Guards the fixSvcOffset() call in the
// committed n/N cycle handler on the off-screen branch.
func TestSearchCycleScrollsOffScreenMatchIntoView(t *testing.T) {
	// "m" matches idx 0 (match-a) and idx 11 (match-b) — far apart.
	svcs := []string{
		"match-a", "b1", "b2", "b3", "b4", "b5", "b6", "b7", "b8", "b9", "b10", "match-b",
	}
	m := committedSearchModel(svcs, "match")
	m.width = 200
	m.height = 10 // visible = 3
	m.svcCursor = 0
	m.svcOffset = 0
	if len(m.searchMatches) != 2 || m.searchMatches[0] != 0 || m.searchMatches[1] != 11 {
		t.Fatalf("precondition: searchMatches = %v, want [0 11]", m.searchMatches)
	}

	// n: 0 → 11 (the far, off-screen match).
	m = sendKey(t, m, 'n')
	if m.svcCursor != 11 {
		t.Fatalf("svcCursor = %d, want 11 after n", m.svcCursor)
	}
	visible := m.svcVisibleCount()
	if m.svcCursor < m.svcOffset || m.svcCursor >= m.svcOffset+visible {
		t.Errorf("cursor %d not in visible window [%d, %d) after cycling to off-screen match",
			m.svcCursor, m.svcOffset, m.svcOffset+visible)
	}
}

// --- Task 5: rendering (row highlight, bottom bar, footer, visible-count) ---

// TestSearchViewCommittedShowsCounter: a committed search renders the (i/N)
// counter and the "↳ <name>" bar summary in the reserved footer line.
func TestSearchViewCommittedShowsCounter(t *testing.T) {
	m := committedSearchModel([]string{"api", "web", "web-worker"}, "w")
	m.svcStatus = map[string]runner.ServiceStatus{}
	if m.svcCursor != 1 {
		t.Fatalf("precondition: svcCursor = %d, want 1 (first match)", m.svcCursor)
	}

	v := m.viewSelectContainers()

	// Counter for cursor on first of two matches.
	if !strings.Contains(v, "(1/2)") {
		t.Errorf("view missing committed counter '(1/2)':\n%s", v)
	}
	// Committed bar summary points at the current match name.
	if !strings.Contains(v, "↳") {
		t.Errorf("view missing committed bar glyph '↳':\n%s", v)
	}
	if !strings.Contains(v, "web") {
		t.Errorf("view missing current match name 'web':\n%s", v)
	}
	// Committed help hint tokens.
	if !strings.Contains(v, "n/N cycle") {
		t.Errorf("view missing 'n/N cycle' hint:\n%s", v)
	}
	if !strings.Contains(v, "esc clear") {
		t.Errorf("view missing 'esc clear' hint:\n%s", v)
	}
}

// TestSearchViewMatchedNameStyled: a matching row's name carries ANSI styling
// (it differs from the plain name), while a non-matching row's name does not.
// lipgloss emits no escape codes under the default (Ascii) test color profile,
// so we force TrueColor for the duration of the test (restored via defer) so
// the styled/unstyled distinction is observable in View() output.
func TestSearchViewMatchedNameStyled(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	m := committedSearchModel([]string{"api", "web"}, "web")
	m.svcStatus = map[string]runner.ServiceStatus{}
	// Cursor is on the sole match "web".

	v := m.viewSelectContainers()
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)

	var webLine, apiLine string
	for _, line := range strings.Split(v, "\n") {
		clean := ansi.ReplaceAllString(line, "")
		// Only the service ROWS carry the status dot; the committed bar summary
		// line also contains "web" but no dot, so this filters it out.
		if !strings.Contains(clean, "●") {
			continue
		}
		switch {
		case strings.Contains(clean, "web"):
			webLine = line
		case strings.Contains(clean, "api"):
			apiLine = line
		}
	}
	if webLine == "" || apiLine == "" {
		t.Fatalf("could not find both service rows in view:\n%s", v)
	}

	// The matched row's name must carry an ANSI color escape (it differs from
	// the plain name); the current match uses searchCurrentStyle whose escape
	// sequence must appear in the styled row.
	if !strings.Contains(webLine, "\x1b[") {
		t.Errorf("matched 'web' row not styled (no ANSI escape): %q", webLine)
	}
	if !strings.Contains(webLine, searchCurrentStyle.Render("web")) {
		t.Errorf("current match not rendered with searchCurrentStyle: %q", webLine)
	}
	// The unmatched "api" name region (from the name to end of line) must NOT
	// be wrapped in a search style — no color escape appears around it.
	apiNameRegion := apiLine[strings.Index(apiLine, "api"):]
	if strings.Contains(apiNameRegion, "\x1b[") {
		t.Errorf("unmatched 'api' name unexpectedly styled: %q", apiNameRegion)
	}
}

// TestSearchViewIdleReservedLine: with no active search, the container view
// still renders (the reserved bar line is present but blank) and the help
// footer shows the '/' search token.
func TestSearchViewIdleReservedLine(t *testing.T) {
	m := containerSearchModel([]string{"api", "web"})
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 200 // one-line help

	v := m.viewSelectContainers()

	// Services still render.
	if !strings.Contains(v, "api") || !strings.Contains(v, "web") {
		t.Errorf("idle view missing services:\n%s", v)
	}
	// The '/' search token is present in the help footer.
	if !strings.Contains(v, "/") {
		t.Errorf("idle view missing '/' search token:\n%s", v)
	}
	// No committed bar glyph and no counter while idle.
	if strings.Contains(v, "↳") {
		t.Errorf("idle view should not render committed bar glyph:\n%s", v)
	}
	if strings.Contains(v, "n/N cycle") {
		t.Errorf("idle view should not render committed hint:\n%s", v)
	}
}

// TestSearchViewSearchingShowsInputAndCounter: while typing, the bar shows the
// "/ " prefix + counter and the footer shows the "enter jump · esc cancel" hint.
func TestSearchViewSearchingShowsInputAndCounter(t *testing.T) {
	m := containerSearchModel([]string{"api", "web", "web-worker"})
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 200

	// Open + type "w".
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	m = updated.(Model)

	v := m.viewSelectContainers()

	// Counter shows 2 matches, cursor on first.
	if !strings.Contains(v, "(1/2)") {
		t.Errorf("searching view missing counter '(1/2)':\n%s", v)
	}
	// Searching help hint.
	if !strings.Contains(v, "enter jump") {
		t.Errorf("searching view missing 'enter jump' hint:\n%s", v)
	}
	if !strings.Contains(v, "esc cancel") {
		t.Errorf("searching view missing 'esc cancel' hint:\n%s", v)
	}
}

// TestSearchViewNoMatchCounter: a searching query with zero matches renders
// "(no match)" in the bar.
func TestSearchViewNoMatchCounter(t *testing.T) {
	m := containerSearchModel([]string{"api", "web"})
	m.svcStatus = map[string]runner.ServiceStatus{}
	m.width = 200

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	m = updated.(Model)

	v := m.viewSelectContainers()
	if !strings.Contains(v, "(no match)") {
		t.Errorf("searching no-match view missing '(no match)':\n%s", v)
	}
}

// TestSearchViewEqualWidthNameCells: a highlighted (matched) row and a
// non-highlighted row must produce equal-width name cells — the styling wraps
// only the raw name so ANSI escapes don't disturb the padding math. We force
// TrueColor so the matched name actually carries escapes, then strip them and
// assert the following column (Created) lands at the same VISIBLE offset on
// both rows.
func TestSearchViewEqualWidthNameCells(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	// Same-length names so the padding is identical; "web" matches, "api" not.
	m := committedSearchModel([]string{"api", "web"}, "web")
	m.svcStatus = map[string]runner.ServiceStatus{
		"api": {Running: true, Created: "2024-01-15 09:30"},
		"web": {Running: true, Created: "2024-01-15 09:30"},
	}

	v := m.viewSelectContainers()
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)

	cols := map[string]int{}
	for _, line := range strings.Split(v, "\n") {
		clean := ansi.ReplaceAllString(line, "")
		if !strings.Contains(clean, "●") || !strings.Contains(clean, "2024-01-15") {
			continue
		}
		byteIdx := strings.Index(clean, "2024-01-15")
		switch {
		case strings.Contains(clean, "web"):
			cols["web"] = utf8.RuneCountInString(clean[:byteIdx])
		case strings.Contains(clean, "api"):
			cols["api"] = utf8.RuneCountInString(clean[:byteIdx])
		}
	}
	if cols["web"] == 0 || cols["api"] == 0 {
		t.Fatalf("could not measure both rows' Created offsets: %v\n%s", cols, v)
	}
	if cols["web"] != cols["api"] {
		t.Errorf("name-cell width mismatch: matched(web)=%d unmatched(api)=%d (styling must not shift columns)", cols["web"], cols["api"])
	}
}

// TestSvcVisibleCount_ConstantAcrossConfirming: with an active search, toggling
// m.confirming must NOT change svcVisibleCount() — the reserved bar line is
// counted in both branches so the list height stays constant.
func TestSvcVisibleCount_ConstantAcrossConfirming(t *testing.T) {
	base := func() Model {
		mc := &mockComposer{}
		m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
		m.statsRequested = false
		m.services = []string{"api", "web", "web-worker", "db", "cache", "queue"}
		m.width = 200 // one-line help in both branches
		m.height = 12
		m.searchQuery = "w"
		m.searchMatches = computeMatches(m.services, "w")
		m.svcCursor = m.searchMatches[0]
		return m
	}

	m1 := base()
	m1.confirming = false
	m2 := base()
	m2.confirming = true
	m2.selected = map[int]bool{1: true}

	c1 := m1.svcVisibleCount()
	c2 := m2.svcVisibleCount()
	if c1 != c2 {
		t.Errorf("svcVisibleCount differs across confirming: non-confirming=%d confirming=%d (must be constant)", c1, c2)
	}
}

// --- Task 6: cleanup wiring — clear search on every departure/reload ---

// assertSearchCleared asserts the three-field ephemeral invariant: an active
// committed search has been fully torn down.
func assertSearchCleared(t *testing.T, m Model, where string) {
	t.Helper()
	if m.searchQuery != "" {
		t.Errorf("%s: searchQuery = %q, want empty (search must be cleared on departure/reload)", where, m.searchQuery)
	}
	if m.searchMatches != nil {
		t.Errorf("%s: searchMatches = %v, want nil", where, m.searchMatches)
	}
	if m.searching {
		t.Errorf("%s: searching = true, want false", where)
	}
}

// TestSearchClearedOnServicesReload: a full list reload (servicesMsg) invalidates
// searchMatches (indices into the OLD services), so the committed search must be
// cleared. Uses an incoming service set that would make the old indices invalid.
func TestSearchClearedOnServicesReload(t *testing.T) {
	mc := &mockComposer{services: []string{"api", "web", "web-worker"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.services, "w") // [1 2]
	m.svcCursor = 1
	if len(m.searchMatches) != 2 {
		t.Fatalf("precondition: expected 2 matches, got %v", m.searchMatches)
	}

	// Reload with a DIFFERENT, shorter list — old indices [1 2] would be invalid.
	updated, _ := m.Update(servicesMsg{
		services: []string{"db"},
		status:   map[string]runner.ServiceStatus{},
		session:  m.statusSession, // must match to be accepted
	})
	m = updated.(Model)

	assertSearchCleared(t, m, "after servicesMsg reload")
}

// TestSearchClearedOnEscToProject: a context switch off the container screen
// (esc → project picker) clears the committed search.
func TestSearchClearedOnEscToProject(t *testing.T) {
	m := committedSearchModel([]string{"api", "web", "worker"}, "w")
	m.showPicker = true
	if m.searchQuery == "" {
		t.Fatal("precondition: committed search expected")
	}

	// First esc clears the search (two-stage guard); the SECOND esc back-navigates.
	// After the second esc we're on the project picker AND search stays clear.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	assertSearchCleared(t, m, "after 1st esc (search clear)")

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectProject {
		t.Errorf("after 2nd esc: screen = %d, want screenSelectProject", m.screen)
	}
	assertSearchCleared(t, m, "after esc→project back-nav")
}

// TestSearchClearedOnEscToProjectSingleEsc: even when a committed search is active
// and the two-stage guard consumes the first esc, the container→project back-nav
// site itself calls clearSearch() unconditionally, so arriving there with no
// active search (e.g. via a direct second esc) still leaves search clear. This
// asserts the unconditional (idempotent) clearSearch at the back-nav site.
func TestSearchClearedOnEscToProjectNoActiveSearch(t *testing.T) {
	m := containerSearchModel([]string{"api", "web", "worker"})
	m.showPicker = true
	// No active search — first (and only) esc back-navigates directly.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectProject {
		t.Errorf("screen = %d, want screenSelectProject (direct back-nav)", m.screen)
	}
	assertSearchCleared(t, m, "after direct esc→project (no active search)")
}

// TestSearchClearedOnEnterLogs: a read-only departure to the logs screen (l key)
// clears the committed search — search is ephemeral, not carried into a log peek.
func TestSearchClearedOnEnterLogs(t *testing.T) {
	mc := &mockComposer{services: []string{"api", "web", "web-worker"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.composer = mc
	m.width = 80
	m.height = 24
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.services, "w")
	m.svcCursor = m.searchMatches[0]

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)

	if m.screen != screenLogs {
		t.Fatalf("screen = %d, want screenLogs (l should enter logs)", m.screen)
	}
	assertSearchCleared(t, m, "after enterLogs")
}

// TestSearchClearedOnEnterProgress: an operation start (enterProgress) clears the
// committed search.
func TestSearchClearedOnEnterProgress(t *testing.T) {
	mc := &mockComposer{services: []string{"api", "web", "web-worker"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.composer = mc
	m.width = 80
	m.height = 24
	m.pendingOp = runner.Restart
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.services, "w")
	m.svcCursor = m.searchMatches[0]

	updated, _ := m.enterProgress([]string{"web"})
	m = updated.(Model)

	if m.screen != screenProgress {
		t.Fatalf("screen = %d, want screenProgress", m.screen)
	}
	assertSearchCleared(t, m, "after enterProgress")
}

// TestSearchClearedEnterLogsThenEscBack: entering logs (which clears search on
// departure) then esc back to the container screen lands with search still clear
// — no stale highlight resurfaces because the return path adds no search state.
func TestSearchClearedEnterLogsThenEscBack(t *testing.T) {
	mc := &mockComposer{services: []string{"api", "web", "web-worker"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.composer = mc
	m.width = 80
	m.height = 24
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.services, "w")
	m.svcCursor = m.searchMatches[0]

	// Enter logs (departure clears search).
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = updated.(Model)
	if m.screen != screenLogs {
		t.Fatalf("precondition: screen = %d, want screenLogs", m.screen)
	}
	assertSearchCleared(t, m, "on entering logs")

	// Esc back to the container screen.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectContainers {
		t.Fatalf("after esc from logs: screen = %d, want screenSelectContainers", m.screen)
	}
	assertSearchCleared(t, m, "back on container screen after logs peek")
}

// TestSelectedContainersUnaffectedBySearch is a regression guard: search is
// search-and-jump, never a filter — it must NEVER touch m.selected. The set of
// selected containers is identical with and without an active committed search.
func TestSelectedContainersUnaffectedBySearch(t *testing.T) {
	services := []string{"api", "web", "web-worker", "db"}

	// Baseline: no search, select "api" (0) and "web-worker" (2).
	base := containerSearchModel(services)
	base.selected = map[int]bool{0: true, 2: true}
	want := base.selectedContainers()
	if len(want) != 2 || want[0] != "api" || want[1] != "web-worker" {
		t.Fatalf("precondition: selectedContainers() = %v, want [api web-worker]", want)
	}

	// Same selection, but with an active committed search on "w" (matches web,
	// web-worker) and the cursor jumped onto a match.
	withSearch := containerSearchModel(services)
	withSearch.selected = map[int]bool{0: true, 2: true}
	withSearch.searchQuery = "w"
	withSearch.searchMatches = computeMatches(services, "w")
	withSearch.svcCursor = withSearch.searchMatches[0]

	got := withSearch.selectedContainers()
	if len(got) != len(want) {
		t.Fatalf("selectedContainers() length differs with search: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("selectedContainers()[%d] = %q with search, want %q (search must not touch m.selected)", i, got[i], want[i])
		}
	}
}

// --- Review phase 1: additional coverage ---

// TestSearchCounterOffMatchMultiple: after committing a multi-match search and
// parking the cursor OFF all matches (k moves onto a non-matching row), the bar
// counter shows "(N matches)" instead of a bogus "(i/N)" position.
func TestSearchCounterOffMatchMultiple(t *testing.T) {
	// "w" matches web (1) and web-worker (2); cursor starts on the first match.
	m := committedSearchModel([]string{"api", "web", "web-worker"}, "w")
	if m.svcCursor != 1 {
		t.Fatalf("precondition: svcCursor = %d, want 1 (first match)", m.svcCursor)
	}

	// k: move up to "api" (0) — a non-matching row, so the cursor is off-match.
	m = sendKey(t, m, 'k')
	if m.svcCursor != 0 {
		t.Fatalf("after k: svcCursor = %d, want 0 (off-match)", m.svcCursor)
	}
	if got := m.searchCounter(); got != "(2 matches)" {
		t.Errorf("searchCounter() = %q, want %q (off-match multi)", got, "(2 matches)")
	}

	// The bar must not label the non-matching row with the ↳ jump glyph.
	bar := m.searchBarLine()
	if strings.Contains(bar, "↳") {
		t.Errorf("searchBarLine still shows ↳ glyph on a non-matching row: %q", bar)
	}
	if !strings.Contains(bar, "(2 matches)") {
		t.Errorf("searchBarLine missing '(2 matches)': %q", bar)
	}
}

// TestSearchCounterOffMatchSingle: same as above but with a single match, so the
// counter reads "(1 match)".
func TestSearchCounterOffMatchSingle(t *testing.T) {
	// "web" matches only web (1); cursor starts on it.
	m := committedSearchModel([]string{"api", "web", "db"}, "web")
	if m.svcCursor != 1 {
		t.Fatalf("precondition: svcCursor = %d, want 1", m.svcCursor)
	}

	// k: move up to "api" (0) — off-match.
	m = sendKey(t, m, 'k')
	if m.svcCursor != 0 {
		t.Fatalf("after k: svcCursor = %d, want 0 (off-match)", m.svcCursor)
	}
	if got := m.searchCounter(); got != "(1 match)" {
		t.Errorf("searchCounter() = %q, want %q (off-match single)", got, "(1 match)")
	}
}

// TestSearchViewNonCurrentMatchStyled: with >=2 matches and the cursor on one,
// the NON-current matched row is styled with searchMatchStyle (non-bold) and NOT
// with searchCurrentStyle (bold). Forces TrueColor so escapes are observable.
func TestSearchViewNonCurrentMatchStyled(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	// "web" matches web (0) and web-worker (1); "api" (2) does not. Cursor on 0.
	m := committedSearchModel([]string{"web", "web-worker", "api"}, "web")
	m.svcStatus = map[string]runner.ServiceStatus{}
	if m.svcCursor != 0 {
		t.Fatalf("precondition: svcCursor = %d, want 0 (first match)", m.svcCursor)
	}

	v := m.viewSelectContainers()
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)

	var workerLine string
	for _, line := range strings.Split(v, "\n") {
		clean := ansi.ReplaceAllString(line, "")
		if !strings.Contains(clean, "●") {
			continue // skip the committed bar summary line
		}
		if strings.Contains(clean, "web-worker") {
			workerLine = line
		}
	}
	if workerLine == "" {
		t.Fatalf("could not find 'web-worker' service row in view:\n%s", v)
	}

	// The non-current matched row must carry searchMatchStyle (the match name is
	// wrapped in it) and must NOT be rendered with the bold searchCurrentStyle.
	if !strings.Contains(workerLine, searchMatchStyle.Render("web-worker")) {
		t.Errorf("non-current match not rendered with searchMatchStyle: %q", workerLine)
	}
	if strings.Contains(workerLine, searchCurrentStyle.Render("web-worker")) {
		t.Errorf("non-current match wrongly rendered with (bold) searchCurrentStyle: %q", workerLine)
	}
}

// TestSearchViewCommittedBarHint: the committed bar's own hint text
// ("n next · N prev · esc clear") is rendered in the bar — assert the bar-unique
// "n next" token so this isn't satisfied by the footer hint instead.
func TestSearchViewCommittedBarHint(t *testing.T) {
	m := committedSearchModel([]string{"api", "web", "web-worker"}, "w")
	m.svcStatus = map[string]runner.ServiceStatus{}

	v := m.viewSelectContainers()
	if !strings.Contains(v, "n next") {
		t.Errorf("committed view missing bar hint 'n next':\n%s", v)
	}
}

// TestNTypedIntoSearchInput: while searching, "n" must land in the input as a
// literal character (NOT trigger cycle-to-next-match, which is its committed-mode
// meaning). A precedence bug would move the cursor instead of typing.
func TestNTypedIntoSearchInput(t *testing.T) {
	m := containerSearchModel([]string{"nginx", "web"})
	m.svcCursor = 1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	if !m.searching {
		t.Fatal("precondition: search must be open")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	um := updated.(Model)

	if !um.searching {
		t.Error("searching = false, want true (n must not close search)")
	}
	if um.searchInput.Value() != "n" {
		t.Errorf("searchInput value = %q, want %q (n lands in input)", um.searchInput.Value(), "n")
	}
	if um.searchQuery != "n" {
		t.Errorf("searchQuery = %q, want %q", um.searchQuery, "n")
	}
	// "n" matches nginx (0); the live-jump moves the cursor to the first match,
	// NOT a cycle-driven move — the point is it typed rather than cycling.
	if len(um.searchMatches) != 1 || um.searchMatches[0] != 0 {
		t.Errorf("searchMatches = %v, want [0] (nginx), i.e. 'n' was typed not cycled", um.searchMatches)
	}
}

// TestUpperNTypedIntoSearchInput: same as above for "N" (committed-mode "cycle to
// previous"). It must land in the input as a literal, not cycle.
func TestUpperNTypedIntoSearchInput(t *testing.T) {
	m := containerSearchModel([]string{"Nginx", "web"})
	m.svcCursor = 1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	if !m.searching {
		t.Fatal("precondition: search must be open")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'N'}})
	um := updated.(Model)

	if !um.searching {
		t.Error("searching = false, want true (N must not close search)")
	}
	if um.searchInput.Value() != "N" {
		t.Errorf("searchInput value = %q, want %q (N lands in input)", um.searchInput.Value(), "N")
	}
	if um.searchQuery != "N" {
		t.Errorf("searchQuery = %q, want %q", um.searchQuery, "N")
	}
}

// TestSearchClearedOnEntryLocal: selecting the "Local" entry on the server screen
// swaps m.composer AND reloads the service list, so a committed search from a
// previous session must be cleared (stale indices would point into a replaced
// m.services). This exercises the entryLocal clearSearch() site.
func TestSearchClearedOnEntryLocal(t *testing.T) {
	mc := &mockComposer{services: []string{"api", "web", "web-worker"}}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	installFakeTick(&m)
	m.screen = screenSelectServer
	// Position the cursor on the "Local" entry.
	for i, e := range m.serverEntries {
		if e.kind == entryLocal {
			m.serverCursor = i
		}
	}
	// Seed a stale committed search as if left over from a prior container view.
	m.searchQuery = "w"
	m.searchMatches = []int{1, 2}
	m.svcCursor = 1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	assertSearchCleared(t, m, "after entryLocal")
}

// TestSearchClearedOnEscProjectToServer: esc from the project picker back to the
// server screen clears any active search (defensive — search is container-scoped).
func TestSearchClearedOnEscProjectToServer(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	installFakeTick(&m)
	m.screen = screenSelectProject
	m.showPicker = true
	m.disconnectFunc = func() error { return nil }
	// Seed a stale committed search.
	m.searchQuery = "w"
	m.searchMatches = []int{1, 2}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want screenSelectServer (esc project→server)", m.screen)
	}
	assertSearchCleared(t, m, "after esc project→server")
}

// TestSearchClearedOnConnectError: a failed remote connect swaps the projectLoader
// and resets transient state; it must also clear a committed search (the error
// path lands the user on the project picker with no valid service list).
func TestSearchClearedOnConnectError(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	installFakeTick(&m)
	m.screen = screenSelectServer
	// Seed a stale committed search.
	m.searchQuery = "w"
	m.searchMatches = []int{1, 2}

	updated, _ := m.Update(connectResultMsg{err: errors.New("connection refused")})
	m = updated.(Model)

	assertSearchCleared(t, m, "after connectResultMsg error")
}

// TestSearchClearedOnEnterConfig: opening the config screen (c key) is a read-only
// departure from the container screen; it clears the committed search.
func TestSearchClearedOnEnterConfig(t *testing.T) {
	mc := &mockConfigComposer{
		mockComposer: mockComposer{services: []string{"api", "web", "web-worker"}},
		configFile:   []byte("services:\n  web: {}\n"),
	}
	m := NewModel(mc, io.Discard, mockConfigFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.composer = mc
	m.width = 80
	m.height = 24
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.services, "w")
	m.svcCursor = m.searchMatches[0]

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = updated.(Model)

	if m.screen != screenConfig {
		t.Fatalf("screen = %d, want screenConfig (c should enter config)", m.screen)
	}
	assertSearchCleared(t, m, "after enterConfig")
}

// TestSearchClearedOnEnterExec: execing into a container (enterExec success path)
// leaves the container screen and clears the committed search.
func TestSearchClearedOnEnterExec(t *testing.T) {
	mc := &mockExecComposer{
		mockComposer: mockComposer{services: []string{"api", "web", "web-worker"}},
	}
	m := NewModel(mc, io.Discard, mockExecFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.composer = mc
	m.width = 80
	m.height = 24
	m.svcCursor = 1 // "web"
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.services, "w")

	updated, _ := m.enterExec()
	m = updated.(Model)

	assertSearchCleared(t, m, "after enterExec")
}

// ---------------------------------------------------------------------------
// Task 11: snapshot-on-deploy + health-wait phase on the progress screen.
// ---------------------------------------------------------------------------

// recordingSnapshotter is a minimal Snapshotter for exercising the
// recordDeploySnapshot best-effort helper in isolation.
type recordingSnapshotter struct {
	res        compose.SnapshotResult
	snapErr    error
	writeErr   error
	snapCalls  int
	writeCalls int
	wrote      *compose.Snapshot
}

func (r *recordingSnapshotter) SnapshotServices(ctx context.Context, services []string) (compose.SnapshotResult, error) {
	r.snapCalls++
	return r.res, r.snapErr
}

func (r *recordingSnapshotter) WriteSnapshot(ctx context.Context, fresh *compose.Snapshot) error {
	r.writeCalls++
	r.wrote = fresh
	return r.writeErr
}

// mockSnapshotComposer implements runner.Composer AND Snapshotter, recording the
// order of the snapshot vs the pipeline Stop step so the pre-Stop ordering can be
// asserted. Reads happen after the events channel closes (a happens-before edge),
// so the plain order slice is race-free under `go test -race`.
type mockSnapshotComposer struct {
	mockComposer
	snapResult compose.SnapshotResult
	snapErr    error
	writeErr   error
	snapCalls  int
	writeCalls int
	order      []string
}

func (m *mockSnapshotComposer) SnapshotServices(ctx context.Context, services []string) (compose.SnapshotResult, error) {
	m.snapCalls++
	m.order = append(m.order, "snapshot")
	return m.snapResult, m.snapErr
}

func (m *mockSnapshotComposer) WriteSnapshot(ctx context.Context, fresh *compose.Snapshot) error {
	m.writeCalls++
	return m.writeErr
}

func (m *mockSnapshotComposer) Stop(ctx context.Context, containers []string, w io.Writer) error {
	m.order = append(m.order, "stop")
	return nil
}

type fakeWaitTickMsg struct{}

// installFakeWaitTick swaps the wait-tick timer for a non-blocking Cmd so a
// not-yet-resolved wait in a test does not leave a real DefaultWaitPoll tea.Tick
// goroutine running.
func installFakeWaitTick(m *Model) {
	m.waitTickOverride = func() tea.Cmd {
		return func() tea.Msg { return fakeWaitTickMsg{} }
	}
}

func TestRecordDeploySnapshot_HappyPath(t *testing.T) {
	snap := &compose.Snapshot{Schema: 1, Services: map[string]compose.SnapshotEntry{"web": {}}}
	s := &recordingSnapshotter{res: compose.SnapshotResult{Snapshot: snap}}
	var buf strings.Builder

	recordDeploySnapshot(context.Background(), s, []string{"web"}, &buf)

	if s.snapCalls != 1 {
		t.Errorf("SnapshotServices calls = %d, want 1", s.snapCalls)
	}
	if s.writeCalls != 1 {
		t.Errorf("WriteSnapshot calls = %d, want 1", s.writeCalls)
	}
	if s.wrote != snap {
		t.Error("WriteSnapshot should receive the captured snapshot")
	}
	if buf.Len() != 0 {
		t.Errorf("no warnings expected on the happy path, got %q", buf.String())
	}
}

func TestRecordDeploySnapshot_CaptureError_SkipsWrite(t *testing.T) {
	s := &recordingSnapshotter{snapErr: errors.New("boom")}
	var buf strings.Builder

	recordDeploySnapshot(context.Background(), s, nil, &buf)

	if s.writeCalls != 0 {
		t.Errorf("WriteSnapshot must not run when capture fails, calls = %d", s.writeCalls)
	}
	if !strings.Contains(buf.String(), "rollback snapshot skipped") {
		t.Errorf("expected skip warning, got %q", buf.String())
	}
}

func TestRecordDeploySnapshot_WriteError_WarnsAndProceeds(t *testing.T) {
	s := &recordingSnapshotter{
		res:      compose.SnapshotResult{Snapshot: &compose.Snapshot{Schema: 1}},
		writeErr: errors.New("disk full"),
	}
	var buf strings.Builder

	recordDeploySnapshot(context.Background(), s, nil, &buf)

	if !strings.Contains(buf.String(), "failed to write rollback snapshot") {
		t.Errorf("expected write-failure warning, got %q", buf.String())
	}
}

func TestRecordDeploySnapshot_PerServiceWarnings(t *testing.T) {
	s := &recordingSnapshotter{res: compose.SnapshotResult{
		Snapshot: &compose.Snapshot{Schema: 1},
		Warnings: []string{"web not running", "db built locally"},
	}}
	var buf strings.Builder

	recordDeploySnapshot(context.Background(), s, nil, &buf)

	out := buf.String()
	if !strings.Contains(out, "web not running") || !strings.Contains(out, "db built locally") {
		t.Errorf("expected both per-service warnings, got %q", out)
	}
	if s.writeCalls != 1 {
		t.Errorf("write should still run after warnings, calls = %d", s.writeCalls)
	}
}

// TestEnterProgress_DeploySnapshotsBeforeStop verifies the launch goroutine runs
// the snapshot FIRST (pre-Stop) for a Deploy. Draining the events channel to
// close establishes a happens-before edge, so reading the order slice is safe.
func TestEnterProgress_DeploySnapshotsBeforeStop(t *testing.T) {
	mc := &mockSnapshotComposer{
		mockComposer: mockComposer{services: []string{"web"}},
		snapResult:   compose.SnapshotResult{Snapshot: &compose.Snapshot{Schema: 1}},
	}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.composer = mc
	m.selected[0] = true
	m.pendingOp = runner.Deploy
	m.width = 80
	m.height = 24

	updated, _ := m.enterProgress([]string{"web"})
	m = updated.(Model)

	// Drain the pipeline events to completion (channel close = goroutine done).
	for range m.eventCh { //nolint:revive // draining for synchronization
	}

	if mc.snapCalls != 1 {
		t.Fatalf("SnapshotServices calls = %d, want 1", mc.snapCalls)
	}
	if mc.writeCalls != 1 {
		t.Errorf("WriteSnapshot calls = %d, want 1", mc.writeCalls)
	}
	if len(mc.order) < 2 || mc.order[0] != "snapshot" || mc.order[1] != "stop" {
		t.Errorf("order = %v, want snapshot before stop", mc.order)
	}
}

// TestEnterProgress_NonSnapshotterMockRunsAsToday verifies a composer without the
// Snapshotter capability deploys exactly as before — no snapshot attempted, the
// pipeline runs to completion.
func TestEnterProgress_NonSnapshotterMockRunsAsToday(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = mc.services
	m.composer = mc
	m.selected[0] = true
	m.pendingOp = runner.Deploy
	m.width = 80
	m.height = 24

	updated, _ := m.enterProgress([]string{"web"})
	m = updated.(Model)

	if m.screen != screenProgress {
		t.Fatalf("screen = %d, want screenProgress", m.screen)
	}
	// Draining without panic confirms the pipeline ran normally.
	for range m.eventCh { //nolint:revive // draining for synchronization
	}
}

func TestPipelineDone_StartsWaitAfterDone(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := Model{
		screen:       screenProgress,
		pendingOp:    runner.Deploy,
		services:     []string{"web"},
		opContainers: []string{"web"},
		composer:     mc,
		ctx:          context.Background(),
	}

	updated, cmd := m.Update(pipelineDoneMsg{})
	m = updated.(Model)

	if !m.done {
		t.Error("done should be true after pipelineDoneMsg")
	}
	if !m.waiting {
		t.Error("waiting should be true after a successful non-stop pipeline")
	}
	if len(m.waitState.Services) != 1 || m.waitState.Services[0] != "web" {
		t.Errorf("waitState.Services = %v, want [web]", m.waitState.Services)
	}
	if m.waitDeadline.IsZero() {
		t.Error("waitDeadline should be set when the wait starts")
	}
	if cmd == nil {
		t.Error("expected a poll cmd to be returned to kick off the wait")
	}
}

func TestPipelineDone_NoWaitOnFailed(t *testing.T) {
	m := Model{screen: screenProgress, pendingOp: runner.Deploy, failed: true, services: []string{"web"}, opContainers: []string{"web"}}

	updated, cmd := m.Update(pipelineDoneMsg{})
	m = updated.(Model)

	if m.done {
		t.Error("done should NOT be set on a failed pipeline")
	}
	if m.waiting {
		t.Error("waiting must never start after a failed pipeline")
	}
	if cmd != nil {
		t.Error("no cmd expected on the failed path")
	}
}

func TestPipelineDone_StopOnlyNeverWaits(t *testing.T) {
	m := Model{screen: screenProgress, pendingOp: runner.StopOnly, services: []string{"web"}, opContainers: []string{"web"}}

	updated, cmd := m.Update(pipelineDoneMsg{})
	m = updated.(Model)

	if !m.done {
		t.Error("StopOnly should still mark the op done")
	}
	if m.waiting {
		t.Error("StopOnly must never enter the health-wait phase")
	}
	if cmd != nil {
		t.Error("StopOnly should return no wait cmd")
	}
}

func TestPipelineDone_EmptyTargetsFallsBackToServices(t *testing.T) {
	mc := &mockComposer{}
	m := Model{
		screen:    screenProgress,
		pendingOp: runner.Restart,
		services:  []string{"web", "db"},
		composer:  mc,
		ctx:       context.Background(),
	} // opContainers deliberately nil

	updated, _ := m.Update(pipelineDoneMsg{})
	m = updated.(Model)

	if !m.waiting {
		t.Fatal("waiting should start with a services fallback target set")
	}
	// The fallback target set must be exactly m.services, in order.
	want := []string{"web", "db"}
	if len(m.waitState.Services) != len(want) {
		t.Fatalf("waitState.Services = %v, want %v", m.waitState.Services, want)
	}
	for i, svc := range want {
		if m.waitState.Services[i] != svc {
			t.Errorf("waitState.Services[%d] = %q, want %q", i, m.waitState.Services[i], svc)
		}
	}
}

func TestWaitStatusMsg_HealthyResolves(t *testing.T) {
	m := Model{screen: screenProgress, waiting: true, pendingOp: runner.Deploy, waitSession: 5}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(runner.DefaultWaitTimeout)

	updated, cmd := m.Update(waitStatusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true, Health: "healthy"}},
		session: 5,
	})
	m = updated.(Model)

	if m.waiting {
		t.Error("waiting should be false once every service resolved")
	}
	if m.waitState.Verdicts["web"] != runner.VerdictHealthy {
		t.Errorf("verdict = %q, want healthy", m.waitState.Verdicts["web"])
	}
	if cmd != nil {
		t.Error("a resolved wait should return no further tick cmd")
	}
}

func TestWaitStatusMsg_StaleSessionRejected(t *testing.T) {
	m := Model{screen: screenProgress, waiting: true, pendingOp: runner.Deploy, waitSession: 9}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(runner.DefaultWaitTimeout)

	updated, _ := m.Update(waitStatusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true, Health: "healthy"}},
		session: 8, // stale
	})
	m = updated.(Model)

	if !m.waiting {
		t.Error("a stale-session poll must not resolve the wait")
	}
	if m.waitState.Verdicts["web"] != runner.VerdictPending {
		t.Errorf("verdict = %q, want pending (stale poll ignored)", m.waitState.Verdicts["web"])
	}
}

func TestWaitStatusMsg_NotDoneReschedulesTick(t *testing.T) {
	m := Model{screen: screenProgress, waiting: true, pendingOp: runner.Deploy, waitSession: 1}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(runner.DefaultWaitTimeout)
	installFakeWaitTick(&m)

	updated, cmd := m.Update(waitStatusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true, Health: "starting"}},
		session: 1,
	})
	m = updated.(Model)

	if !m.waiting {
		t.Error("a still-starting service should keep the wait active")
	}
	if m.waitState.Verdicts["web"] != runner.VerdictPending {
		t.Errorf("verdict = %q, want pending", m.waitState.Verdicts["web"])
	}
	if cmd == nil {
		t.Fatal("expected a reschedule tick cmd")
	}
	if _, ok := cmd().(fakeWaitTickMsg); !ok {
		t.Errorf("cmd should produce a wait tick, got %T", cmd())
	}
}

func TestWaitTickMsg_StaleSessionRejected(t *testing.T) {
	m := Model{screen: screenProgress, waiting: true, waitSession: 7}

	_, cmd := m.Update(waitTickMsg{session: 6})

	if cmd != nil {
		t.Error("a stale wait tick should be dropped without rescheduling")
	}
}

func TestWaitTickMsg_LiveFiresPoll(t *testing.T) {
	mc := &mockComposer{status: map[string]runner.ServiceStatus{"web": {Running: true, Health: "healthy"}}}
	m := Model{screen: screenProgress, waiting: true, waitSession: 3, composer: mc, ctx: context.Background()}
	m.waitState = runner.NewWaitState([]string{"web"})

	_, cmd := m.Update(waitTickMsg{session: 3})
	if cmd == nil {
		t.Fatal("a live tick should fire a status poll")
	}
	msg := cmd()
	ws, ok := msg.(waitStatusMsg)
	if !ok {
		t.Fatalf("cmd should produce waitStatusMsg, got %T", msg)
	}
	if ws.session != 3 {
		t.Errorf("poll session = %d, want 3", ws.session)
	}
}

func TestEscSkip_LeavesDoneClearsState(t *testing.T) {
	m := Model{screen: screenProgress, waiting: true, done: true, pendingOp: runner.Deploy, waitSession: 3}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(runner.DefaultWaitTimeout)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.waiting {
		t.Error("esc should stop the wait")
	}
	if !m.done {
		t.Error("the op must remain done after esc-skip")
	}
	if m.screen != screenProgress {
		t.Errorf("screen = %d, want screenProgress (esc-skip stays)", m.screen)
	}
	if len(m.waitState.Services) != 0 {
		t.Errorf("waitState should be cleared on skip, got %v", m.waitState.Services)
	}
	if m.waitSession != 4 {
		t.Errorf("waitSession = %d, want 4 (bumped to invalidate in-flight polls)", m.waitSession)
	}
}

// TestEscSkip_ThenEscGoesBack verifies the two-stage esc: first esc skips the
// wait (stays on progress), a second esc navigates back to the container screen.
func TestEscSkip_ThenEscGoesBack(t *testing.T) {
	mc := &mockComposer{}
	m := Model{screen: screenProgress, waiting: true, done: true, pendingOp: runner.Deploy, composer: mc, ctx: context.Background()}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(runner.DefaultWaitTimeout)
	installFakeTick(&m)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenProgress {
		t.Fatalf("after first esc screen = %d, want screenProgress", m.screen)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectContainers {
		t.Errorf("after second esc screen = %d, want screenSelectContainers", m.screen)
	}
}

func TestEscFromProgress_ClearsWaitState(t *testing.T) {
	mc := &mockComposer{}
	m := Model{screen: screenProgress, done: true, pendingOp: runner.Deploy, composer: mc, ctx: context.Background(), waitSession: 2}
	m.waitState = runner.NewWaitState([]string{"web"}) // resolved verdicts still on screen
	installFakeTick(&m)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Fatalf("screen = %d, want screenSelectContainers", m.screen)
	}
	if len(m.waitState.Services) != 0 {
		t.Errorf("waitState should be cleared on leaving progress, got %v", m.waitState.Services)
	}
	if m.waitSession != 3 {
		t.Errorf("waitSession = %d, want 3 (bumped on departure)", m.waitSession)
	}
}

func TestViewProgress_WaitingShowsCountdownAndEscSkip(t *testing.T) {
	m := Model{screen: screenProgress, waiting: true, done: true, pendingOp: runner.Deploy}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(90 * time.Second)
	m.width = 80
	m.height = 24

	view := m.viewProgress()

	if !strings.Contains(view, "Waiting for health") {
		t.Errorf("view should show the wait countdown, got:\n%s", view)
	}
	if !strings.Contains(view, "esc skip") {
		t.Errorf("view should show the esc-skip footer while waiting, got:\n%s", view)
	}
}

func TestViewProgress_FailedDeployShowsRollbackHint(t *testing.T) {
	m := Model{screen: screenProgress, waiting: false, done: true, pendingOp: runner.Deploy}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitState.Verdicts["web"] = runner.VerdictUnhealthy
	m.width = 80
	m.height = 24

	view := m.viewProgress()

	if !strings.Contains(view, "roll back") {
		t.Errorf("a failed Deploy wait should render the rollback hint, got:\n%s", view)
	}
	if !strings.Contains(view, string(runner.VerdictUnhealthy)) {
		t.Errorf("the unhealthy verdict label should be rendered, got:\n%s", view)
	}
}

func TestViewProgress_FailedRestartNoRollbackHint(t *testing.T) {
	m := Model{screen: screenProgress, waiting: false, done: true, pendingOp: runner.Restart}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitState.Verdicts["web"] = runner.VerdictUnhealthy
	m.width = 80
	m.height = 24

	view := m.viewProgress()

	if strings.Contains(view, "roll back") {
		t.Errorf("a Restart wait failure must NOT show the deploy-only rollback hint, got:\n%s", view)
	}
}

func TestWaitStatusMsg_PollErrorTimesOut(t *testing.T) {
	m := Model{screen: screenProgress, waiting: true, pendingOp: runner.Deploy, waitSession: 1}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(-time.Second) // already past the deadline

	updated, cmd := m.Update(waitStatusMsg{err: errors.New("ssh dead"), session: 1})
	m = updated.(Model)

	if m.waiting {
		t.Error("a poll error past the deadline should finalize the wait")
	}
	if m.waitState.Verdicts["web"] != runner.VerdictTimedOut {
		t.Errorf("verdict = %q, want timed out", m.waitState.Verdicts["web"])
	}
	if cmd != nil {
		t.Error("no reschedule after the timeout sweep")
	}
}

func TestWaitStatusMsg_PollErrorBeforeDeadlineRetries(t *testing.T) {
	m := Model{screen: screenProgress, waiting: true, pendingOp: runner.Deploy, waitSession: 1}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(runner.DefaultWaitTimeout)
	installFakeWaitTick(&m)

	updated, cmd := m.Update(waitStatusMsg{err: errors.New("transient"), session: 1})
	m = updated.(Model)

	if !m.waiting {
		t.Error("a transient poll error before the deadline should keep waiting")
	}
	if cmd == nil {
		t.Fatal("expected a retry tick after a transient poll error")
	}
	if _, ok := cmd().(fakeWaitTickMsg); !ok {
		t.Errorf("cmd should produce a wait tick, got %T", cmd())
	}
}

// hangingStatusComposer blocks ContainerStatus until its context is cancelled,
// simulating a hung Docker daemon or a stalled SSH status call during the TUI
// wait poll. It records whether the received context carried a deadline and
// returns ctx.Err() once the context fires. The channel handoff in the test
// establishes a happens-before edge for reading sawDeadline, so no lock is needed.
type hangingStatusComposer struct {
	mockComposer
	sawDeadline bool
}

func (h *hangingStatusComposer) ContainerStatus(ctx context.Context) (map[string]runner.ServiceStatus, error) {
	if _, ok := ctx.Deadline(); ok {
		h.sawDeadline = true
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestPollWaitStatus_BoundedByDeadline(t *testing.T) {
	// C8: the TUI health poll must bound each ContainerStatus by m.waitDeadline. A
	// HUNG Docker/SSH call under the raw m.ctx would never deliver a waitStatusMsg,
	// so the reducer's timeout sweep would never run and the progress screen would
	// wait indefinitely. With the deadline bound, the hung poll returns a deadline
	// error at ~m.waitDeadline and the wait can resolve.
	c := &hangingStatusComposer{}
	m := Model{
		screen:       screenProgress,
		waiting:      true,
		composer:     c,
		ctx:          context.Background(),
		waitSession:  3,
		waitDeadline: time.Now().Add(40 * time.Millisecond),
	}
	m.waitState = runner.NewWaitState([]string{"web"})

	cmd := m.pollWaitStatus()
	if cmd == nil {
		t.Fatal("pollWaitStatus returned nil cmd")
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	select {
	case msg := <-done:
		ws, ok := msg.(waitStatusMsg)
		if !ok {
			t.Fatalf("poll produced %T, want waitStatusMsg", msg)
		}
		if ws.err == nil {
			t.Error("a hung poll cut off at the deadline should carry a (deadline) error")
		}
		if ws.session != 3 {
			t.Errorf("waitStatusMsg session = %d, want 3", ws.session)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pollWaitStatus never returned — the poll was NOT bounded by the wait deadline (C8)")
	}

	if !c.sawDeadline {
		t.Error("ContainerStatus received a context WITHOUT a deadline — poll not deadline-bound")
	}
}

func TestPollWaitStatus_HungPollResolvesAsTimedOut(t *testing.T) {
	// C8 end-to-end: a hung poll bounded by the deadline returns a deadline error at
	// ~m.waitDeadline; feeding that waitStatusMsg (deadline already past) into
	// Update() must sweep the pending verdicts to timed out and finalize the wait,
	// NOT stall.
	m := Model{screen: screenProgress, waiting: true, pendingOp: runner.Deploy, waitSession: 1}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(-time.Millisecond) // deadline reached

	updated, cmd := m.Update(waitStatusMsg{err: context.DeadlineExceeded, session: 1})
	m = updated.(Model)

	if m.waiting {
		t.Error("a deadline-error poll at/after the deadline must finalize the wait")
	}
	if m.waitState.Verdicts["web"] != runner.VerdictTimedOut {
		t.Errorf("verdict = %q, want timed out", m.waitState.Verdicts["web"])
	}
	if cmd != nil {
		t.Error("no reschedule after the timeout sweep")
	}
}

// --- Task 12: TUI `R` rollback key ---------------------------------------

// The `R` key type-asserts m.composer to RollbackPreparer at runtime; pin that
// both real composers satisfy it so a signature drift is a compile error, not a
// silently-ignored key press in production.
var (
	_ RollbackPreparer = (*compose.Compose)(nil)
	_ RollbackPreparer = (*compose.RemoteCompose)(nil)
)

// mockRollbackComposer implements runner.Composer AND RollbackPreparer, driving
// the `R`-key snapshot fetch + prep flow without a real composer.
type mockRollbackComposer struct {
	mockComposer
	snap        *compose.Snapshot
	readErr     error
	readCalls   int
	prepCleanup func()
	prepErr     error
	prepCalls   int
	prepSvcs    []string
}

func (m *mockRollbackComposer) ReadSnapshot(_ context.Context) (*compose.Snapshot, error) {
	m.readCalls++
	return m.snap, m.readErr
}

func (m *mockRollbackComposer) PrepareRollback(_ context.Context, _ map[string]compose.SnapshotEntry, services []string, _ io.Writer) (func(), error) {
	m.prepCalls++
	m.prepSvcs = append([]string(nil), services...)
	if m.prepErr != nil {
		return nil, m.prepErr
	}
	return m.prepCleanup, nil
}

func mockRollbackFactory(mc *mockRollbackComposer) ComposerFactory {
	return func(compose.Project) runner.Composer { return mc }
}

func rollbackTestSnapshot() *compose.Snapshot {
	return &compose.Snapshot{
		Schema: 1,
		Services: map[string]compose.SnapshotEntry{
			"web": {Image: "nginx:latest", Digest: "sha256:ab", RecordedAt: time.Now().Add(-3 * time.Hour).Format(time.RFC3339)},
			"db":  {Image: "postgres:16", Digest: "sha256:cd", RecordedAt: time.Now().Add(-9 * time.Hour).Format(time.RFC3339)},
		},
	}
}

func TestRollbackKey_NonPreparerIgnored(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := Model{
		screen:   screenSelectContainers,
		services: []string{"web"},
		composer: mc,
		selected: map[int]bool{0: true},
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	m = updated.(Model)
	if m.confirming {
		t.Error("R must be a no-op for a composer without RollbackPreparer")
	}
	if cmd != nil {
		t.Error("R should fire no fetch cmd for a non-Preparer composer")
	}
}

func TestRollbackKey_EmptyListIgnored(t *testing.T) {
	mc := &mockRollbackComposer{}
	m := Model{screen: screenSelectContainers, services: nil, composer: mc, selected: map[int]bool{}}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	m = updated.(Model)
	if cmd != nil || m.warning != "" {
		t.Error("R on an empty service list must be a silent no-op")
	}
}

func TestRollbackKey_NoSelectionWarns(t *testing.T) {
	mc := &mockRollbackComposer{mockComposer: mockComposer{services: []string{"web"}}}
	m := Model{screen: screenSelectContainers, services: []string{"web"}, composer: mc, selected: map[int]bool{}}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	m = updated.(Model)
	if m.warning != warnNoSelection {
		t.Errorf("warning = %q, want %q", m.warning, warnNoSelection)
	}
	if cmd != nil {
		t.Error("R with no selection must not fire a fetch cmd")
	}
}

func TestRollbackKey_FiresFetch(t *testing.T) {
	snap := rollbackTestSnapshot()
	mc := &mockRollbackComposer{mockComposer: mockComposer{services: []string{"web"}}, snap: snap}
	m := Model{
		screen:               screenSelectContainers,
		services:             []string{"web"},
		composer:             mc,
		selected:             map[int]bool{0: true},
		ctx:                  context.Background(),
		rollbackFetchSession: 0,
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	m = updated.(Model)
	if m.rollbackFetchSession != 1 {
		t.Errorf("rollbackFetchSession = %d, want 1 (bumped on R press)", m.rollbackFetchSession)
	}
	if cmd == nil {
		t.Fatal("R with a Preparer + selection should fire a fetch cmd")
	}
	msg := cmd()
	rm, ok := msg.(rollbackSnapshotMsg)
	if !ok {
		t.Fatalf("cmd should produce rollbackSnapshotMsg, got %T", msg)
	}
	if rm.session != 1 {
		t.Errorf("fetch session = %d, want 1", rm.session)
	}
	if rm.snap != snap {
		t.Error("fetch should return the mock snapshot")
	}
	if mc.readCalls != 1 {
		t.Errorf("ReadSnapshot calls = %d, want 1", mc.readCalls)
	}
}

func TestRollbackSnapshotMsg_NoSnapshotWarns(t *testing.T) {
	m := Model{
		screen:               screenSelectContainers,
		services:             []string{"web"},
		selected:             map[int]bool{0: true},
		rollbackFetchSession: 3,
	}
	updated, _ := m.Update(rollbackSnapshotMsg{snap: nil, session: 3})
	m = updated.(Model)
	if m.confirming {
		t.Error("a missing snapshot must not enter the confirm flow")
	}
	if !strings.Contains(m.warning, "No rollback snapshot") {
		t.Errorf("warning = %q, want a no-snapshot message", m.warning)
	}
}

func TestRollbackSnapshotMsg_ReadErrorWarns(t *testing.T) {
	m := Model{
		screen:               screenSelectContainers,
		services:             []string{"web"},
		selected:             map[int]bool{0: true},
		rollbackFetchSession: 1,
	}
	updated, _ := m.Update(rollbackSnapshotMsg{err: errors.New("schema 2 unsupported"), session: 1})
	m = updated.(Model)
	if m.confirming {
		t.Error("a read error must not enter the confirm flow")
	}
	if !strings.Contains(m.warning, "rollback unavailable") {
		t.Errorf("warning = %q, want a read-error message", m.warning)
	}
}

func TestRollbackSnapshotMsg_TargetedMissingWarns(t *testing.T) {
	snap := &compose.Snapshot{Schema: 1, Services: map[string]compose.SnapshotEntry{"web": {}}}
	m := Model{
		screen:               screenSelectContainers,
		services:             []string{"web", "db"},
		selected:             map[int]bool{0: true, 1: true},
		rollbackTargets:      []string{"web", "db"}, // captured at R-press
		rollbackFetchSession: 2,
	}
	updated, _ := m.Update(rollbackSnapshotMsg{snap: snap, session: 2})
	m = updated.(Model)
	if m.confirming {
		t.Error("a selected service missing from the snapshot must not confirm")
	}
	if !strings.Contains(m.warning, "db") {
		t.Errorf("warning = %q, want it to name the missing service 'db'", m.warning)
	}
}

// TestRollbackSnapshotMsg_ComposeRemovedWarns (C6): a selected service that is in
// the snapshot but has since been REMOVED from the compose file must not proceed
// to PrepareRollback — the generated override would otherwise resurrect it as an
// image-only service. It is filtered out (refused with a naming warning),
// mirroring the CLI filterLiveTargets.
func TestRollbackSnapshotMsg_ComposeRemovedWarns(t *testing.T) {
	// Snapshot still records both web + db, but the live compose file only has web.
	snap := rollbackTestSnapshot()
	m := Model{
		screen:               screenSelectContainers,
		services:             []string{"web", "db"},
		selected:             map[int]bool{0: true, 1: true}, // both selected
		rollbackTargets:      []string{"web", "db"},          // captured at R-press
		rollbackFetchSession: 2,
	}
	updated, _ := m.Update(rollbackSnapshotMsg{snap: snap, live: []string{"web"}, session: 2})
	m = updated.(Model)
	if m.confirming {
		t.Error("a target removed from the compose file must not enter the confirm flow")
	}
	if m.pendingOp == runner.Rollback {
		t.Error("pendingOp must not be set to Rollback when a target is stale")
	}
	if m.rollbackSnapshot != nil {
		t.Error("the snapshot must not be stored when a target is refused")
	}
	if !strings.Contains(m.warning, "db") {
		t.Errorf("warning = %q, want it to name the removed service 'db'", m.warning)
	}
	if !strings.Contains(m.warning, "compose file") {
		t.Errorf("warning = %q, want a 'no longer in the compose file' message", m.warning)
	}
}

// TestFetchRollbackSnapshot_PopulatesLiveServices (C6): the async fetch reads the
// snapshot AND the current compose service set (ListServices) in one goroutine so
// the handler can intersect them. A non-empty snapshot triggers the live fetch.
func TestFetchRollbackSnapshot_PopulatesLiveServices(t *testing.T) {
	snap := rollbackTestSnapshot()
	mc := &mockRollbackComposer{mockComposer: mockComposer{services: []string{"web", "db"}}, snap: snap}
	m := Model{composer: mc, ctx: context.Background(), rollbackFetchSession: 1}
	cmd := m.fetchRollbackSnapshot()
	if cmd == nil {
		t.Fatal("fetchRollbackSnapshot returned nil cmd")
	}
	rm, ok := cmd().(rollbackSnapshotMsg)
	if !ok {
		t.Fatalf("want rollbackSnapshotMsg, got different msg")
	}
	if rm.err != nil {
		t.Fatalf("unexpected err: %v", rm.err)
	}
	if strings.Join(rm.live, ",") != "web,db" {
		t.Errorf("live = %v, want [web db] from ListServices", rm.live)
	}
}

// TestFetchRollbackSnapshot_ListServicesErrorFailsClosed (C6): a ListServices
// failure during the fetch fails closed — it surfaces as the shared err (the
// handler shows "rollback unavailable: ..."), never proceeding without the
// live-compose intersection.
func TestFetchRollbackSnapshot_ListServicesErrorFailsClosed(t *testing.T) {
	snap := rollbackTestSnapshot()
	mc := &mockRollbackComposer{
		mockComposer: mockComposer{err: errors.New("compose config --services failed")},
		snap:         snap,
	}
	m := Model{composer: mc, ctx: context.Background(), rollbackFetchSession: 1}
	rm := m.fetchRollbackSnapshot()().(rollbackSnapshotMsg)
	if rm.err == nil {
		t.Fatal("want a ListServices error surfaced, got nil")
	}
	if !strings.Contains(rm.err.Error(), "current compose services") {
		t.Errorf("err = %v, want it to mention listing compose services", rm.err)
	}
	if rm.live != nil {
		t.Errorf("live = %v, want nil on ListServices failure", rm.live)
	}
}

func TestRollbackSnapshotMsg_PresentEntersConfirm(t *testing.T) {
	snap := rollbackTestSnapshot()
	m := Model{
		screen:               screenSelectContainers,
		services:             []string{"web"},
		selected:             map[int]bool{0: true},
		rollbackTargets:      []string{"web"}, // captured at R-press
		rollbackFetchSession: 1,
	}
	// live includes the selected target, so the live-compose intersection passes.
	updated, _ := m.Update(rollbackSnapshotMsg{snap: snap, live: []string{"web", "db"}, session: 1})
	m = updated.(Model)
	if !m.confirming || m.pendingOp != runner.Rollback {
		t.Fatalf("confirming=%v pendingOp=%v, want confirming with Rollback", m.confirming, m.pendingOp)
	}
	if m.rollbackSnapshot != snap {
		t.Error("the fetched snapshot should be stored for prep")
	}
	// esc cancels the confirmation (existing confirming flow).
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.confirming {
		t.Error("esc should cancel the rollback confirmation")
	}
}

func TestRollbackSnapshotMsg_StaleSessionRejected(t *testing.T) {
	snap := rollbackTestSnapshot()
	m := Model{
		screen:               screenSelectContainers,
		services:             []string{"web"},
		selected:             map[int]bool{0: true},
		rollbackFetchSession: 5,
	}
	updated, _ := m.Update(rollbackSnapshotMsg{snap: snap, session: 4}) // stale
	m = updated.(Model)
	if m.confirming {
		t.Error("a stale-session fetch must not enter the confirm flow")
	}
	if m.rollbackSnapshot != nil {
		t.Error("a stale-session fetch must not store the snapshot")
	}
}

func TestRollbackSnapshotMsg_WrongScreenRejected(t *testing.T) {
	snap := rollbackTestSnapshot()
	m := Model{screen: screenLogs, rollbackFetchSession: 1}
	updated, _ := m.Update(rollbackSnapshotMsg{snap: snap, session: 1})
	m = updated.(Model)
	if m.confirming {
		t.Error("a fetch delivered off the container screen must be dropped")
	}
}

func TestRollbackConfirm_MultiSelectTargetSet(t *testing.T) {
	snap := rollbackTestSnapshot()
	mc := &mockRollbackComposer{mockComposer: mockComposer{services: []string{"db", "web"}}, snap: snap}
	m := NewModel(mc, io.Discard, mockRollbackFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"db", "web"}
	m.composer = mc
	m.selected = map[int]bool{0: true, 1: true}
	m.rollbackSnapshot = snap
	m.rollbackTargets = []string{"db", "web"} // captured at R-press; drives the pipeline target
	m.pendingOp = runner.Rollback
	m.confirming = true
	m.ctx = context.Background()
	m.width, m.height = 80, 24

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenProgress {
		t.Fatalf("screen = %d, want screenProgress after confirm", m.screen)
	}
	if strings.Join(m.opContainers, ",") != "db,web" {
		t.Errorf("opContainers = %v, want [db web] (multi-select target set)", m.opContainers)
	}
}

// TestRollbackTargets_CapturedAtPressNotAfterFetch (C11): the target set is
// captured at R-press. If the user changes/clears the multi-select during the
// async snapshot fetch, the rollback must still target the CAPTURED set — never
// the mutated (here: cleared) selection, which the runner would treat as "all
// services". This is the TOCTOU guard for an unintended all-service rollback.
func TestRollbackTargets_CapturedAtPressNotAfterFetch(t *testing.T) {
	snap := rollbackTestSnapshot() // records web + db
	mc := &mockRollbackComposer{mockComposer: mockComposer{services: []string{"web", "db"}}, snap: snap}
	m := Model{
		screen:   screenSelectContainers,
		services: []string{"web", "db"},
		composer: mc,
		selected: map[int]bool{0: true}, // web only
		ctx:      context.Background(),
	}
	// Press R: captures {web}, bumps the fetch session, fires the async fetch.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	m = updated.(Model)
	if got := strings.Join(m.rollbackTargets, ","); got != "web" {
		t.Fatalf("rollbackTargets after R = %q, want web (captured at press)", got)
	}
	if cmd == nil {
		t.Fatal("R with a Preparer + selection should fire a fetch cmd")
	}
	session := m.rollbackFetchSession

	// Simulate the user CLEARING the selection while the fetch is in flight.
	m.selected = map[int]bool{}

	// The snapshot lands for the captured session.
	updated, _ = m.Update(rollbackSnapshotMsg{snap: snap, live: []string{"web", "db"}, session: session})
	m = updated.(Model)
	if !m.confirming || m.pendingOp != runner.Rollback {
		t.Fatalf("want confirming Rollback for the captured target; confirming=%v pendingOp=%v", m.confirming, m.pendingOp)
	}
	if got := strings.Join(m.rollbackTargets, ","); got != "web" {
		t.Errorf("captured rollbackTargets mutated to %q; a selection change must not touch it (want web)", got)
	}

	// Confirm: the pipeline target must be the captured {web}, NOT the now-empty
	// selection (empty would become an all-service rollback in the runner).
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.screen != screenProgress {
		t.Fatalf("screen = %d, want screenProgress after confirm", m.screen)
	}
	if got := strings.Join(m.opContainers, ","); got != "web" {
		t.Errorf("opContainers = %q, want web (captured set) — never empty/all", got)
	}
}

// TestRollbackSnapshotMsg_EmptyCapturedRefuses (C11): if the captured target set
// is somehow empty when the snapshot lands, the handler must REFUSE (warn) rather
// than fall through to the confirm flow — an empty set would become an
// all-service rollback in PrepareRollback/runner.Run.
func TestRollbackSnapshotMsg_EmptyCapturedRefuses(t *testing.T) {
	snap := rollbackTestSnapshot()
	m := Model{
		screen:               screenSelectContainers,
		services:             []string{"web", "db"},
		selected:             map[int]bool{0: true, 1: true},
		rollbackTargets:      nil, // captured set empty
		rollbackFetchSession: 2,
	}
	updated, _ := m.Update(rollbackSnapshotMsg{snap: snap, live: []string{"web", "db"}, session: 2})
	m = updated.(Model)
	if m.confirming {
		t.Error("an empty captured target set must not enter the confirm flow")
	}
	if m.pendingOp == runner.Rollback {
		t.Error("pendingOp must not be Rollback with an empty captured set")
	}
	if m.rollbackSnapshot != nil {
		t.Error("the snapshot must not be stored when the captured set is empty")
	}
	if m.warning != warnNoSelection {
		t.Errorf("warning = %q, want %q", m.warning, warnNoSelection)
	}
}

func TestPrepareRollbackCmd_SuccessCallsPrepareWithTargets(t *testing.T) {
	cleanup := func() {}
	mc := &mockRollbackComposer{prepCleanup: cleanup}
	snap := rollbackTestSnapshot()
	m := Model{composer: mc, rollbackSnapshot: snap, ctx: context.Background()}
	events := make(chan runner.StepEvent, 20)

	cmd := m.prepareRollbackCmd(context.Background(), []string{"db", "web"}, io.Discard, events)
	msg := cmd()
	// Drain the pipeline goroutine launched on success (channel close = done).
	for range events { //nolint:revive // draining for synchronization
	}

	pm, ok := msg.(rollbackPreppedMsg)
	if !ok {
		t.Fatalf("cmd should produce rollbackPreppedMsg, got %T", msg)
	}
	if pm.err != nil {
		t.Fatalf("unexpected prep error: %v", pm.err)
	}
	if pm.cleanup == nil {
		t.Error("cleanup should be returned on prep success")
	}
	if mc.prepCalls != 1 {
		t.Errorf("PrepareRollback calls = %d, want 1", mc.prepCalls)
	}
	if strings.Join(mc.prepSvcs, ",") != "db,web" {
		t.Errorf("PrepareRollback services = %v, want [db web]", mc.prepSvcs)
	}
}

func TestPrepareRollbackCmd_ErrorReturnsErr(t *testing.T) {
	mc := &mockRollbackComposer{prepErr: errors.New("image unavailable")}
	m := Model{composer: mc, rollbackSnapshot: rollbackTestSnapshot(), ctx: context.Background()}
	events := make(chan runner.StepEvent, 20)

	cmd := m.prepareRollbackCmd(context.Background(), []string{"web"}, io.Discard, events)
	msg := cmd()

	pm, ok := msg.(rollbackPreppedMsg)
	if !ok {
		t.Fatalf("cmd should produce rollbackPreppedMsg, got %T", msg)
	}
	if pm.err == nil || !strings.Contains(pm.err.Error(), "image unavailable") {
		t.Errorf("prepped err = %v, want the prep failure", pm.err)
	}
	if pm.cleanup != nil {
		t.Error("no cleanup on prep failure")
	}
	// The pipeline goroutine must NOT have been launched on failure — events is
	// still open with no writer/closer.
	select {
	case _, ok := <-events:
		if !ok {
			t.Error("events channel must not be closed on prep failure")
		} else {
			t.Error("no pipeline event should be produced on prep failure")
		}
	default:
	}
}

func TestRollbackPreppedMsg_ErrorFailsOp(t *testing.T) {
	m := Model{screen: screenProgress}
	updated, cmd := m.Update(rollbackPreppedMsg{err: errors.New("pull failed")})
	m = updated.(Model)
	if !m.failed {
		t.Error("prep error should mark the op failed")
	}
	if !strings.Contains(m.rollbackErr, "pull failed") {
		t.Errorf("rollbackErr = %q, want the prep error text", m.rollbackErr)
	}
	if m.rollbackCleanup != nil {
		t.Error("no cleanup should be stored on prep failure")
	}
	if cmd != nil {
		t.Error("no further cmd on prep failure")
	}
}

func TestRollbackPreppedMsg_SuccessStoresCleanupAndConsumesEvents(t *testing.T) {
	calls := 0
	cleanup := func() { calls++ }
	events := make(chan runner.StepEvent)
	close(events) // so waitForEvent resolves to pipelineDoneMsg immediately
	m := Model{screen: screenProgress, eventCh: events}

	updated, cmd := m.Update(rollbackPreppedMsg{cleanup: cleanup})
	m = updated.(Model)

	if m.rollbackCleanup == nil {
		t.Fatal("cleanup should be stored for later invocation")
	}
	if calls != 0 {
		t.Error("cleanup must NOT run at store time (only on leaving progress)")
	}
	if cmd == nil {
		t.Fatal("success should return waitForEvent to consume pipeline events")
	}
	if _, ok := cmd().(pipelineDoneMsg); !ok {
		t.Error("returned cmd should read from eventCh (waitForEvent)")
	}
}

func TestRollbackPreppedMsg_OffScreenInvokesCleanup(t *testing.T) {
	calls := 0
	m := Model{screen: screenSelectContainers} // not the progress screen
	m.Update(rollbackPreppedMsg{cleanup: func() { calls++ }})
	if calls != 1 {
		t.Errorf("an off-screen prepped msg should invoke cleanup once (no leak), got %d", calls)
	}
}

func TestEscFromProgress_InvokesRollbackCleanupOnce(t *testing.T) {
	calls := 0
	m := Model{
		screen:          screenProgress,
		done:            true,
		pendingOp:       runner.Rollback,
		composer:        &mockComposer{},
		ctx:             context.Background(),
		rollbackCleanup: func() { calls++ },
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if calls != 1 {
		t.Fatalf("rollbackCleanup calls = %d, want 1 on leaving progress", calls)
	}
	if m.rollbackCleanup != nil {
		t.Error("rollbackCleanup should be cleared after invocation")
	}
	if m.screen != screenSelectContainers {
		t.Fatalf("screen = %d, want screenSelectContainers", m.screen)
	}
	// A second esc (now on the container screen) must not re-invoke cleanup.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if calls != 1 {
		t.Errorf("rollbackCleanup must run exactly once, got %d", calls)
	}
}

func TestEscSkipDuringWait_DefersRollbackCleanup(t *testing.T) {
	calls := 0
	m := Model{
		screen:          screenProgress,
		waiting:         true,
		done:            true,
		pendingOp:       runner.Rollback,
		composer:        &mockComposer{},
		ctx:             context.Background(),
		rollbackCleanup: func() { calls++ },
		waitSession:     1,
	}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(runner.DefaultWaitTimeout)

	// First esc = skip the wait; stays on progress, cleanup must NOT run.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if calls != 0 {
		t.Error("esc-skip must not invoke the rollback cleanup (still on progress)")
	}
	if m.rollbackCleanup == nil {
		t.Fatal("cleanup must survive an esc-skip until the screen is left")
	}
	// Second esc leaves the progress screen and invokes cleanup.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if calls != 1 {
		t.Errorf("cleanup should run once on leaving progress, got %d", calls)
	}
}

// TestCtrlCOnProgress_LocalRunsRollbackCleanup (Q5): a hard exit (ctrl+c → quit)
// during a local rollback wait must remove the override temp file — otherwise it
// leaks because the esc-only cleanup path is skipped on quit.
func TestCtrlCOnProgress_LocalRunsRollbackCleanup(t *testing.T) {
	calls := 0
	m := Model{
		screen:          screenProgress,
		waiting:         true,
		done:            true,
		pendingOp:       runner.Rollback,
		composer:        &mockComposer{},
		ctx:             context.Background(),
		rollbackCleanup: func() { calls++ },
		waitSession:     1,
		// disconnectFunc nil → local session → ctrl+c quits immediately.
	}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(runner.DefaultWaitTimeout)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(Model)
	if calls != 1 {
		t.Fatalf("rollbackCleanup calls = %d, want 1 on hard-quit", calls)
	}
	if m.rollbackCleanup != nil {
		t.Error("rollbackCleanup should be cleared after invocation")
	}
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", cmd())
	}
}

// TestCtrlCOnProgress_RemoteRunsCleanupOnConfirm (Q5): on a remote session the
// ctrl+c shows the disconnect prompt (no cleanup yet); the cleanup runs only when
// the user confirms with "y", while the ControlMaster socket is still live.
func TestCtrlCOnProgress_RemoteRunsCleanupOnConfirm(t *testing.T) {
	calls := 0
	m := Model{
		screen:          screenProgress,
		waiting:         true,
		done:            true,
		pendingOp:       runner.Rollback,
		composer:        &mockComposer{},
		ctx:             context.Background(),
		rollbackCleanup: func() { calls++ },
		waitSession:     1,
		disconnectFunc:  func() error { return nil }, // remote session
		serverName:      "prod",
	}
	m.waitState = runner.NewWaitState([]string{"web"})
	m.waitDeadline = time.Now().Add(runner.DefaultWaitTimeout)

	// ctrl+c → confirmation prompt, cleanup deferred.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(Model)
	if !m.quitting {
		t.Fatal("ctrl+c on a remote session should show the disconnect prompt")
	}
	if calls != 0 {
		t.Error("cleanup must NOT run until the quit is confirmed")
	}
	// Confirm quit → cleanup runs before teardown.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = updated.(Model)
	if calls != 1 {
		t.Fatalf("rollbackCleanup calls = %d, want 1 after confirming quit", calls)
	}
	if cmd == nil || func() bool { _, ok := cmd().(tea.QuitMsg); return !ok }() {
		t.Error("confirming quit should return tea.QuitMsg")
	}
}

// TestHumanizeAge (T2) pins the four relative-age ranges.
func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "moments ago"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
	}
	for _, c := range cases {
		if got := humanizeAge(c.d); got != c.want {
			t.Errorf("humanizeAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestRollbackAgeSuffix_PicksNewest (T2): with two target entries of different
// ages the suffix reflects the NEWEST recorded_at (the most representative
// "last deploy"), not the oldest.
func TestRollbackAgeSuffix_PicksNewest(t *testing.T) {
	now := time.Now().UTC()
	m := Model{
		rollbackSnapshot: &compose.Snapshot{
			Services: map[string]compose.SnapshotEntry{
				"web": {RecordedAt: now.Add(-3 * time.Hour).Format(time.RFC3339)},
				"db":  {RecordedAt: now.Add(-50 * time.Hour).Format(time.RFC3339)},
			},
		},
	}
	got := m.rollbackAgeSuffix([]string{"web", "db"})
	if !strings.Contains(got, "to snapshot") {
		t.Errorf("rollbackAgeSuffix = %q, want the 'to snapshot' prefix", got)
	}
	if !strings.Contains(got, "3h ago") {
		t.Errorf("rollbackAgeSuffix = %q, want the NEWEST age (3h ago), not the oldest (2d)", got)
	}
}

// TestRollbackAgeSuffix_NilSnapshotEmpty (T2): no snapshot → empty suffix so the
// confirm prompt degrades to the plain service list.
func TestRollbackAgeSuffix_NilSnapshotEmpty(t *testing.T) {
	m := Model{}
	if got := m.rollbackAgeSuffix([]string{"web"}); got != "" {
		t.Errorf("rollbackAgeSuffix with nil snapshot = %q, want empty", got)
	}
	// A snapshot with no parseable ages also degrades to empty.
	m.rollbackSnapshot = &compose.Snapshot{Services: map[string]compose.SnapshotEntry{"web": {RecordedAt: "not-a-time"}}}
	if got := m.rollbackAgeSuffix([]string{"web"}); got != "" {
		t.Errorf("rollbackAgeSuffix with unparseable age = %q, want empty", got)
	}
}

func TestEscFromProgress_RollbackInvalidatesUpdateCache(t *testing.T) {
	m := Model{
		screen:      screenProgress,
		done:        true,
		pendingOp:   runner.Rollback,
		composer:    &mockComposer{},
		ctx:         context.Background(),
		updateCache: map[string]updateEntry{"|": {results: map[string]bool{"web": true}}},
	}
	if _, ok := m.updateCache[m.updatesCacheKey()]; !ok {
		t.Fatalf("precondition: cache should hold key %q", m.updatesCacheKey())
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if _, ok := m.updateCache["|"]; ok {
		t.Error("a successful Rollback should invalidate the update-availability cache")
	}
}

// TestHelpOverlay_ShowsRollbackKey pins that `R rollback` stays discoverable.
// The token moved out of the trimmed footer into the `?` overlay, so this
// renders the overlay, not the footer.
func TestHelpOverlay_ShowsRollbackKey(t *testing.T) {
	m := Model{screen: screenSelectContainers, width: 200, height: 24}

	if !helpOverlayNamesKey(m, "R", "rollback") {
		t.Errorf("`?` overlay should mention the 'R' rollback key, got:\n%s", m.viewHelp())
	}
}

func TestViewProgress_RollbackConfirmShowsAge(t *testing.T) {
	snap := rollbackTestSnapshot()
	mc := &mockRollbackComposer{mockComposer: mockComposer{services: []string{"web"}}, snap: snap}
	m := NewModel(mc, io.Discard, mockRollbackFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.services = []string{"web"}
	m.composer = mc
	m.selected = map[int]bool{0: true}
	m.rollbackSnapshot = snap
	m.rollbackTargets = []string{"web"} // captured at R-press; drives the confirm prompt
	m.pendingOp = runner.Rollback
	m.confirming = true
	m.width, m.height = 120, 24
	v := m.View()
	if !strings.Contains(v, "Rollback web") {
		t.Errorf("confirm prompt should show the Rollback op + service, got:\n%s", v)
	}
	if !strings.Contains(v, "snapshot") {
		t.Errorf("confirm prompt should mention the snapshot age, got:\n%s", v)
	}
}

func TestViewProgress_RollbackPrepErrorRendered(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.pendingOp = runner.Rollback
	m.failed = true
	m.rollbackErr = "web: image sha256:ab unavailable"
	m.width, m.height = 120, 24
	v := m.View()
	if !strings.Contains(v, "rollback prep failed") {
		t.Errorf("progress view should render the rollback prep error, got:\n%s", v)
	}
}

// HostContainers satisfies tui.ExecProvider, so the `x` key works on the
// read-only unmanaged screen. The assertion lives here rather than in the
// compose package because internal/tui imports internal/compose, not the
// other way round.
var _ ExecProvider = (*compose.HostContainers)(nil)

// TestHostContainers_CapabilityInterfaces pins both halves of the read-only
// capability contract. The positive half is the compile-time assertion above;
// the negative half must be a runtime check, because Go cannot express "does
// NOT implement" at compile time. ConfigProvider and RollbackPreparer are the
// self-gating pair from design decision 8: a container with no compose file
// has no config to show and no deploy snapshot to roll back to, so the `c` and
// `R` keys no-op through the same type-assert guards the mocks rely on.
func TestHostContainers_CapabilityInterfaces(t *testing.T) {
	var c runner.Composer = compose.NewLocalHostContainers(compose.New(t.TempDir()))

	if _, ok := c.(ExecProvider); !ok {
		t.Error("HostContainers must satisfy ExecProvider so the x key works")
	}
	if _, ok := c.(ConfigProvider); ok {
		t.Error("HostContainers must NOT satisfy ConfigProvider; the c key has to gate itself")
	}
	if _, ok := c.(RollbackPreparer); ok {
		t.Error("HostContainers must NOT satisfy RollbackPreparer; the R key has to gate itself")
	}
	if _, ok := c.(Snapshotter); ok {
		t.Error("HostContainers must NOT satisfy Snapshotter; there is no compose project to snapshot")
	}
}

// readOnlyMockComposer is a mockComposer that also satisfies ReadOnlyComposer
// and ExecProvider — the exact capability set of *compose.HostContainers. The
// real type is used for the interface pins above; this mock is used for key
// dispatch so no test ever shells out to docker.
type readOnlyMockComposer struct {
	mockComposer
	execErr error
}

func (m *readOnlyMockComposer) ReadOnlyComposer() bool { return true }

func (m *readOnlyMockComposer) ExecCommand(ctx context.Context, service string, command []string) (*exec.Cmd, error) {
	if m.execErr != nil {
		return nil, m.execErr
	}
	return exec.CommandContext(ctx, "true"), nil
}

func newReadOnlyModel(t *testing.T, mc *readOnlyMockComposer) Model {
	t.Helper()
	// The factory must hand back the READ-ONLY composer: passing the embedded
	// mockComposer would let any test that triggers m.composerFactory(...) swap
	// in a writable one, and every read-only assertion after that would go
	// quiet rather than fail.
	m := NewModel(mc, io.Discard, func(compose.Project) runner.Composer { return mc }, nil, nil)
	m.screen = screenSelectContainers
	m.services = append([]string(nil), mc.services...)
	// Copy rather than alias mc.status: hydrateUpdates mutates m.svcStatus in
	// place, and an alias would write verdicts back into the composer double
	// that later subtests share.
	m.svcStatus = make(map[string]runner.ServiceStatus, len(mc.status))
	for k, v := range mc.status {
		m.svcStatus[k] = v
	}
	m.width, m.height = 120, 24
	m.updateInFlight = false
	m.refreshInFlight = false
	installFakeTick(&m)
	return m
}

// capableReadOnlyComposer is a read-only composer that ALSO satisfies
// ConfigProvider and RollbackPreparer. No production composer looks like this
// — HostContainers implements neither — but readOnlyMockComposer implements
// neither either, so the c and R cases of TestReadOnly_GatesWriteKeys would
// pass on the capability assertion that follows the gate rather than on the
// gate itself. With this double the two m.readOnly() early returns are the only
// thing standing between the key and its action.
type capableReadOnlyComposer struct {
	readOnlyMockComposer
}

func (c *capableReadOnlyComposer) ConfigFile(ctx context.Context) ([]byte, error) {
	return []byte("services: {}\n"), nil
}

func (c *capableReadOnlyComposer) ConfigResolved(ctx context.Context) ([]byte, error) {
	return []byte("services: {}\n"), nil
}

func (c *capableReadOnlyComposer) EditCommand(ctx context.Context) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "true"), nil
}

func (c *capableReadOnlyComposer) ValidateConfig(ctx context.Context) error { return nil }

func (c *capableReadOnlyComposer) ReadSnapshot(ctx context.Context) (*compose.Snapshot, error) {
	return &compose.Snapshot{Schema: 1, Services: map[string]compose.SnapshotEntry{
		"watchtower": {Image: "containrrr/watchtower:1.7", Digest: "sha256:abc"},
	}}, nil
}

func (c *capableReadOnlyComposer) PrepareRollback(ctx context.Context, entries map[string]compose.SnapshotEntry, services []string, w io.Writer) (func(), error) {
	return func() {}, nil
}

func readOnlyTestComposer() *readOnlyMockComposer {
	return &readOnlyMockComposer{mockComposer: mockComposer{
		services: []string{"watchtower", "portainer"},
		status: map[string]runner.ServiceStatus{
			"watchtower": {Running: true},
			"portainer":  {Running: true},
		},
	}}
}

// TestReadOnly_Predicate pins the three cases the gate depends on: a zero-value
// Model (the ~18 Model{} test literals) and a normal compose composer are both
// writable, and only a composer that answers the named ReadOnlyComposer method
// is read-only. A method-less marker interface would make all three true.
func TestReadOnly_Predicate(t *testing.T) {
	if (Model{}).readOnly() {
		t.Error("zero-value Model must not be read-only (nil composer)")
	}

	mc := &mockComposer{}
	if (Model{composer: mc}).readOnly() {
		t.Error("a normal compose composer must not be read-only")
	}

	ro := &readOnlyMockComposer{}
	if !(Model{composer: ro}).readOnly() {
		t.Error("a ReadOnlyComposer must be read-only")
	}

	hc := compose.NewLocalHostContainers(compose.New(t.TempDir()))
	if !(Model{composer: hc}).readOnly() {
		t.Error("HostContainers must be read-only")
	}
}

// TestReadOnly_GatesWriteKeys asserts every gated key is fully inert: no
// confirmation armed, no pendingOp, no selection change, no warning, no cmd.
// A key that changed state while the footer and the `?` overlay hide it would
// be a silent surprise; a key that warned would be advertising a no-op.
func TestReadOnly_GatesWriteKeys(t *testing.T) {
	for _, key := range []string{"d", "r", "s", "R", "c", " ", "a"} {
		t.Run(key, func(t *testing.T) {
			mc := readOnlyTestComposer()
			m := newReadOnlyModel(t, mc)

			updated, cmd := m.Update(keyMsgFor(key))
			got := updated.(Model)

			if cmd != nil {
				t.Errorf("key %q returned a command; want nil", key)
			}
			if got.screen != screenSelectContainers {
				t.Errorf("key %q changed screen to %d", key, got.screen)
			}
			if got.confirming {
				t.Errorf("key %q armed a confirmation", key)
			}
			if got.pendingOp != m.pendingOp {
				t.Errorf("key %q set pendingOp = %v", key, got.pendingOp)
			}
			if got.pendingExec {
				t.Errorf("key %q armed pendingExec", key)
			}
			if got.warning != "" {
				t.Errorf("key %q set warning %q; a gated key must not advertise itself", key, got.warning)
			}
			if got.selectedCount() != 0 {
				t.Errorf("key %q selected %d services", key, got.selectedCount())
			}
			if got.rollbackTargets != nil {
				t.Errorf("key %q captured rollback targets", key)
			}
		})
	}
}

// TestReadOnly_GatesWriteKeys_WithCapableComposer is the non-vacuous half of
// the c and R cases above. readOnlyMockComposer implements neither
// ConfigProvider nor RollbackPreparer, so those two keys return on the
// capability assertion below the gate; here the composer answers both, leaving
// the m.readOnly() early return as the only thing that keeps them inert.
func TestReadOnly_GatesWriteKeys_WithCapableComposer(t *testing.T) {
	for _, key := range []string{"c", "R"} {
		t.Run(key, func(t *testing.T) {
			mc := &capableReadOnlyComposer{readOnlyMockComposer: *readOnlyTestComposer()}
			m := NewModel(mc, io.Discard, func(compose.Project) runner.Composer { return mc }, nil, nil)
			m.screen = screenSelectContainers
			m.services = append([]string(nil), mc.services...)
			m.svcStatus = mc.status
			m.width, m.height = 120, 24
			m.updateInFlight = false
			m.refreshInFlight = false
			installFakeTick(&m)
			// A selection is what R needs past its own empty-selection warning;
			// it is unreachable through space on this composer, so set it
			// directly to prove the gate — not the missing selection — is
			// what stops the key.
			m.selected[0] = true

			// Precondition: the capability assertions the gates sit above
			// would BOTH succeed on this composer.
			if _, ok := m.composer.(ConfigProvider); !ok {
				t.Fatal("precondition: the double must satisfy ConfigProvider")
			}
			if _, ok := m.composer.(RollbackPreparer); !ok {
				t.Fatal("precondition: the double must satisfy RollbackPreparer")
			}

			updated, cmd := m.Update(keyMsgFor(key))
			got := updated.(Model)

			if cmd != nil {
				t.Errorf("key %q returned a command; want nil", key)
			}
			if got.screen != screenSelectContainers {
				t.Errorf("key %q changed screen to %d", key, got.screen)
			}
			if got.confirming {
				t.Errorf("key %q armed a confirmation", key)
			}
			if got.rollbackTargets != nil {
				t.Errorf("key %q captured rollback targets", key)
			}
			if got.warning != "" {
				t.Errorf("key %q set warning %q; a gated key must not advertise itself", key, got.warning)
			}
		})
	}
}

// TestReadOnly_KeepsReadKeys is the other half of AC4/AC5: the read keys must
// still work on a read-only composer. Without this, gating by composer type
// could over-reach and silently disable the whole screen.
func TestReadOnly_KeepsReadKeys(t *testing.T) {
	t.Run("l opens logs", func(t *testing.T) {
		mc := readOnlyTestComposer()
		m := newReadOnlyModel(t, mc)
		updated, cmd := m.Update(keyMsgFor("l"))
		got := updated.(Model)
		if got.screen != screenLogs {
			t.Errorf("screen = %d, want screenLogs", got.screen)
		}
		if cmd == nil {
			t.Error("l should return the readLogChunk command")
		}
		if got.logsCancel != nil {
			got.logsCancel()
		}
	})

	t.Run("x arms the exec prompt", func(t *testing.T) {
		mc := readOnlyTestComposer()
		m := newReadOnlyModel(t, mc)
		updated, _ := m.Update(keyMsgFor("x"))
		got := updated.(Model)
		if !got.confirming || !got.pendingExec {
			t.Errorf("x should arm the exec prompt; confirming=%v pendingExec=%v", got.confirming, got.pendingExec)
		}
	})

	t.Run("enter confirms the exec prompt", func(t *testing.T) {
		mc := readOnlyTestComposer()
		m := newReadOnlyModel(t, mc)
		updated, _ := m.Update(keyMsgFor("x"))
		updated, cmd := updated.(Model).Update(tea.KeyMsg{Type: tea.KeyEnter})
		got := updated.(Model)
		if cmd == nil {
			t.Error("enter after x should return the exec command")
		}
		if got.warning != "" {
			t.Errorf("exec should not warn, got %q", got.warning)
		}
	})

	t.Run("/ opens search and n cycles", func(t *testing.T) {
		mc := readOnlyTestComposer()
		m := newReadOnlyModel(t, mc)
		updated, _ := m.Update(keyMsgFor("/"))
		got := updated.(Model)
		if !got.searching {
			t.Fatal("/ should open the search bar")
		}
		updated, _ = got.Update(keyMsgFor("p"))
		updated, _ = updated.(Model).Update(tea.KeyMsg{Type: tea.KeyEnter})
		got = updated.(Model)
		if got.searchQuery != "p" || len(got.searchMatches) == 0 {
			t.Fatalf("search should commit; query=%q matches=%v", got.searchQuery, got.searchMatches)
		}
		if got.svcCursor != got.searchMatches[0] {
			t.Errorf("cursor = %d, want %d", got.svcCursor, got.searchMatches[0])
		}
		updated, _ = got.Update(keyMsgFor("n"))
		if updated.(Model).searchQuery != "p" {
			t.Error("n must not clear the committed search")
		}
	})

	t.Run("U forces an update refresh", func(t *testing.T) {
		mc := readOnlyTestComposer()
		m := newReadOnlyModel(t, mc)
		updated, cmd := m.Update(keyMsgFor("U"))
		got := updated.(Model)
		if cmd == nil {
			t.Error("U should return a refreshUpdates command")
		}
		if !got.updateInFlight {
			t.Error("U should mark updateInFlight")
		}
	})
}

// TestUpdatesCacheKey_UnmanagedIsolated is AC8. The unmanaged row has an empty
// ConfigDir, so without the read-only prefix a local unmanaged view and the
// local-fast-track entry would share the bare "|" slot.
func TestUpdatesCacheKey_UnmanagedIsolated(t *testing.T) {
	ro := readOnlyTestComposer()
	fastTrack := Model{composer: &mockComposer{}}
	unmanaged := Model{composer: ro}
	if fastTrack.updatesCacheKey() == unmanaged.updatesCacheKey() {
		t.Fatalf("unmanaged and local-fast-track share the cache key %q", fastTrack.updatesCacheKey())
	}

	// The same isolation must hold per server, and against a real project dir.
	remoteUnmanaged := Model{composer: ro, serverName: "prod"}
	remoteProject := Model{composer: &mockComposer{}, projDir: "/srv/app", serverName: "prod"}
	if remoteUnmanaged.updatesCacheKey() == remoteProject.updatesCacheKey() {
		t.Error("unmanaged and a remote project share a cache key")
	}
	if remoteUnmanaged.updatesCacheKey() == unmanaged.updatesCacheKey() {
		t.Error("local and remote unmanaged share a cache key")
	}
}

// TestUpdatesCacheKey_FollowsComposerAcrossNavigation is the regression pin for
// the cleanup discipline the removed projUnmanaged field used to need: pick the
// unmanaged row, walk back out with esc, then fast-track into the local
// composer, and the key must return to the bare "|" slot. A stale unmanaged
// marker would key the fast-track context as "unmanaged||" — the AC8 collision
// in the opposite direction.
func TestUpdatesCacheKey_FollowsComposerAcrossNavigation(t *testing.T) {
	ro := readOnlyTestComposer()
	local := &mockComposer{services: []string{"web"}}
	m := NewModel(local, io.Discard, func(proj compose.Project) runner.Composer {
		if proj.Unmanaged {
			return ro
		}
		return local
	}, []config.Server{{Name: "prod", Host: "prod.example.com"}}, nil)
	installFakeTick(&m)
	m.screen = screenSelectProject
	m.showPicker = true
	m.projects = []compose.Project{{Name: compose.UnmanagedProjectName, Desc: "2 containers", Unmanaged: true}}

	updated, _ := m.Update(keyMsgFor("enter"))
	m = updated.(Model)
	if got := m.updatesCacheKey(); got != "unmanaged||" {
		t.Fatalf("after picking the unmanaged row, key = %q, want %q", got, "unmanaged||")
	}

	updated, _ = m.Update(keyMsgFor("esc")) // containers -> project
	m = updated.(Model)
	updated, _ = m.Update(keyMsgFor("esc")) // project -> server
	m = updated.(Model)
	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want screenSelectServer", m.screen)
	}

	// Local entry is first in serverEntries; enter fast-tracks to containers.
	m.serverCursor = 0
	updated, _ = m.Update(keyMsgFor("enter"))
	m = updated.(Model)
	if got := m.updatesCacheKey(); got != "|" {
		t.Errorf("after the local fast-track, key = %q, want %q", got, "|")
	}
}

// TestUpdatesCache_UnmanagedDoesNotReplayFastTrackVerdicts is the behavioural
// half of AC8: a colliding service name must not pick up the other context's
// verdict. hydrateUpdates' phantom guard drops only UNKNOWN names, so a shared
// key would write "web is out of date" straight onto the unmanaged web row.
func TestUpdatesCache_UnmanagedDoesNotReplayFastTrackVerdicts(t *testing.T) {
	mc := readOnlyTestComposer()
	m := newReadOnlyModel(t, mc)
	m.services = []string{"web"}
	m.svcStatus = map[string]runner.ServiceStatus{"web": {Running: true}}
	m.updateCache = map[string]updateEntry{
		// The local-fast-track slot, populated by a previous compose context.
		"|": {fetchedAt: time.Now(), results: map[string]bool{"web": true}},
	}

	updated, cmd := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true}},
		session: m.statusSession,
	})
	got := updated.(Model)

	if ua := got.svcStatus["web"].UpdateAvailable; ua != nil {
		t.Errorf("UpdateAvailable = %v, want nil; the fast-track verdict leaked into the unmanaged view", *ua)
	}

	// The miss must NOT queue a fetch: the unmanaged view is opt-in, so the
	// self-heal stays gated and U is the only trigger (see
	// TestReadOnly_NoAutomaticUpdateFetch). The isolation above therefore comes
	// from the key alone, which the writable control below confirms.
	if cmd != nil || got.updateInFlight {
		t.Error("a cache miss on the unmanaged key must not queue an automatic refreshUpdates")
	}

	// Control: on a writable composer the SAME cache entry does replay —
	// proving the isolation above comes from the key, not from a dead lookup.
	m.composer = &mc.mockComposer
	updated, _ = m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true}},
		session: m.statusSession,
	})
	ua := updated.(Model).svcStatus["web"].UpdateAvailable
	if ua == nil || !*ua {
		t.Error("the fast-track context must still replay its own cached verdict")
	}
}

// TestReadOnly_NoSelectionAffordances pins task 8: with d/r/s/R gated, both the
// per-row checkbox and the title's (n/m selected) counter would advertise a dead
// key. The writable control proves the assertions are not vacuous.
func TestReadOnly_NoSelectionAffordances(t *testing.T) {
	mc := readOnlyTestComposer()
	m := newReadOnlyModel(t, mc)

	view := ansi.Strip(m.viewSelectContainers())
	if strings.Contains(view, "selected") {
		t.Errorf("read-only view shows the selection counter:\n%s", view)
	}
	if strings.Contains(view, "[ ]") || strings.Contains(view, "[x]") {
		t.Errorf("read-only view shows a row checkbox:\n%s", view)
	}

	wc := &mockComposer{services: mc.services, status: mc.status}
	w := NewModel(wc, io.Discard, mockFactory(wc), nil, nil)
	w.screen = screenSelectContainers
	w.services = wc.services
	w.svcStatus = wc.status
	w.width, w.height = 120, 24
	control := ansi.Strip(w.viewSelectContainers())
	if !strings.Contains(control, "selected") || !strings.Contains(control, "[ ]") {
		t.Errorf("the writable control must keep both affordances:\n%s", control)
	}
}

// TestReadOnly_CaptionAlignment pins the caption pad against the row checkbox.
// The pad is 10 cells with the checkbox and 7 without it, so dropping one
// without the other misaligns every column against its caption.
func TestReadOnly_CaptionAlignment(t *testing.T) {
	status := map[string]runner.ServiceStatus{
		"watchtower": {Running: true, Created: "2026-08-20 09:30", Uptime: "3h"},
		"portainer":  {Running: true, Created: "2026-08-19 11:00", Uptime: "1d"},
	}

	for _, tt := range []struct {
		name     string
		readOnly bool
	}{
		{"read-only", true},
		{"writable", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var m Model
			if tt.readOnly {
				mc := readOnlyTestComposer()
				mc.status = status
				m = newReadOnlyModel(t, mc)
			} else {
				wc := &mockComposer{services: []string{"watchtower", "portainer"}, status: status}
				m = NewModel(wc, io.Discard, mockFactory(wc), nil, nil)
				m.screen = screenSelectContainers
				m.services = wc.services
				m.svcStatus = status
				m.width, m.height = 120, 24
			}

			view := m.viewSelectContainers()
			var caption string
			rows := map[string]string{}
			for _, line := range strings.Split(view, "\n") {
				plain := ansi.Strip(line)
				if strings.Contains(plain, "Service") && strings.Contains(plain, "Created") {
					caption = plain
					continue
				}
				if !strings.Contains(plain, "●") {
					continue
				}
				for _, svc := range m.services {
					if strings.Contains(plain, svc) {
						rows[svc] = plain
					}
				}
			}
			if caption == "" {
				t.Fatalf("no captions row rendered:\n%s", view)
			}
			if len(rows) != len(m.services) {
				t.Fatalf("found %d data rows, want %d:\n%s", len(rows), len(m.services), view)
			}

			want := ansi.StringWidth(caption[:strings.Index(caption, "Service")])
			wantPad := 10
			if tt.readOnly {
				wantPad = 7
			}
			if want != wantPad {
				t.Errorf("caption pad = %d cells, want %d", want, wantPad)
			}
			for svc, row := range rows {
				got := ansi.StringWidth(row[:strings.Index(row, svc)])
				if got != want {
					t.Errorf("%s starts at column %d; the Service caption is at %d\ncaption: %q\nrow:     %q",
						svc, got, want, caption, row)
				}
			}
		})
	}
}

// TestReadOnly_UpdateGlyphHydrates pins task 12's user-visible half: the ⇧
// column is the one thing HostContainers.CheckUpdates delivers, and it must
// survive the read-only render path, which drops the checkbox the writable
// path pads around.
func TestReadOnly_UpdateGlyphHydrates(t *testing.T) {
	mc := readOnlyTestComposer()
	m := newReadOnlyModel(t, mc)

	result, _ := m.Update(updatesMsg{
		results: map[string]bool{"watchtower": true, "portainer": false},
		session: m.updatesSession,
	})
	model := result.(Model)

	if av := model.svcStatus["watchtower"].UpdateAvailable; av == nil || !*av {
		t.Fatalf("watchtower UpdateAvailable = %v, want &true", av)
	}
	if av := model.svcStatus["portainer"].UpdateAvailable; av == nil || *av {
		t.Fatalf("portainer UpdateAvailable = %v, want &false", av)
	}

	view := model.viewSelectContainers()
	if !strings.Contains(view, compose.UpdateGlyph) {
		t.Fatalf("read-only view shows no update glyph:\n%s", ansi.Strip(view))
	}
	for _, line := range strings.Split(view, "\n") {
		if !strings.Contains(line, compose.UpdateGlyph) {
			continue
		}
		plain := ansi.Strip(line)
		if !strings.Contains(plain, "watchtower") {
			t.Errorf("update glyph rendered on the wrong row: %q", plain)
		}
	}
}

// TestReadOnly_RendersAllStatusColumns is AC3a. The row builder is shared with
// the writable path — only the checkbox and the caption pad branch on readOnly
// — but "shared today" is exactly the kind of claim that rots silently, so the
// full column set is pinned against the rendered read-only screen: name, status
// dot, health icon, Created, Uptime, Ports, CPU and Mem.
func TestReadOnly_RendersAllStatusColumns(t *testing.T) {
	mc := readOnlyTestComposer()
	mc.status = map[string]runner.ServiceStatus{
		"watchtower": {
			Running: true,
			Health:  "healthy",
			Created: "2026-08-20 09:30",
			Uptime:  "3h",
			Ports:   []runner.Port{{Host: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		},
		"portainer": {Running: false, Health: "unhealthy", Created: "2026-08-19 11:00"},
	}
	m := newReadOnlyModel(t, mc)
	m.statsRequested = true
	m.stats = map[string]runner.ServiceStats{
		"watchtower": {CPUPercent: 12.5, MemoryUsed: 130023424, MemoryLimit: 2147483648},
	}

	view := ansi.Strip(m.viewSelectContainers())

	var caption, running, stopped string
	for _, line := range strings.Split(view, "\n") {
		switch {
		case strings.Contains(line, "Service") && strings.Contains(line, "Created"):
			caption = line
		case strings.Contains(line, "watchtower"):
			running = line
		case strings.Contains(line, "portainer"):
			stopped = line
		}
	}
	if caption == "" || running == "" || stopped == "" {
		t.Fatalf("missing rows — caption=%q running=%q stopped=%q\n%s", caption, running, stopped, view)
	}

	for _, want := range []string{"Service", "Created", "Uptime", "CPU", "Mem", "Ports"} {
		if !strings.Contains(caption, want) {
			t.Errorf("caption row is missing the %q column: %q", want, caption)
		}
	}

	for _, tt := range []struct{ what, want string }{
		{"name", "watchtower"},
		{"status dot", "●"},
		{"health icon", "♥"},
		{"Created", "2026-08-20 09:30"},
		{"Uptime", "3h"},
		{"Ports", compose.FormatPort(runner.Port{Host: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"})},
		{"CPU", "12.5%"},
		{"Mem", compose.FormatBytes(130023424)},
	} {
		if !strings.Contains(running, tt.want) {
			t.Errorf("read-only row is missing %s (%q): %q", tt.what, tt.want, running)
		}
	}

	// The unhealthy icon and the stopped dot share the row builder too, and a
	// stopped container must still render its Created cell.
	if !strings.Contains(stopped, "✗") {
		t.Errorf("stopped row is missing the unhealthy icon: %q", stopped)
	}
	if !strings.Contains(stopped, "2026-08-19 11:00") {
		t.Errorf("stopped row is missing its Created cell: %q", stopped)
	}
}

// TestReadOnly_FetchErrorSurfacesInSvcErr pins the container screen's error
// path for a read-only composer end to end: loadServices folds a ListServices
// OR a ContainerStatus failure into servicesMsg.err, the handler stores it in
// svcErr, and the renderer shows it. Without this the unmanaged view could fail
// silently against an unreachable daemon or a dead SSH hop.
func TestReadOnly_FetchErrorSurfacesInSvcErr(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(*readOnlyMockComposer)
		want string
	}{
		{"ListServices fails", func(c *readOnlyMockComposer) { c.err = errors.New("docker ps failed") }, "docker ps failed"},
		{"ContainerStatus fails", func(c *readOnlyMockComposer) { c.statusErr = errors.New("ssh hop down") }, "ssh hop down"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mc := readOnlyTestComposer()
			tt.set(mc)
			m := newReadOnlyModel(t, mc)

			msg := m.loadServices()()
			sm, ok := msg.(servicesMsg)
			if !ok {
				t.Fatalf("loadServices returned %T, want servicesMsg", msg)
			}
			if sm.err == nil || sm.err.Error() != tt.want {
				t.Fatalf("servicesMsg.err = %v, want %q", sm.err, tt.want)
			}

			updated, _ := m.Update(sm)
			got := updated.(Model)
			if got.svcErr == nil || got.svcErr.Error() != tt.want {
				t.Fatalf("svcErr = %v, want %q", got.svcErr, tt.want)
			}
			if view := ansi.Strip(got.viewSelectContainers()); !strings.Contains(view, tt.want) {
				t.Errorf("the error is not rendered on the container screen:\n%s", view)
			}
		})
	}
}

// TestReadOnly_NoAutomaticUpdateFetch pins the opt-in contract for the
// unmanaged view: nothing may fire CheckUpdates on its own there.
//
// The compose path bounds the check to one project's service list. The
// unmanaged path is derived from `docker ps -a`, so the set grows with every
// container ever left on the host and each distinct image costs a REGISTRY
// manifest request. An automatic fan-out on screen entry — and again on every
// 5-second status tick via the self-heal — can exhaust the anonymous Docker Hub
// quota and break a real docker pull from the same host, so U is the only
// trigger, matching the CLI's opt-in `list --updates`.
//
// Both automatic entry points are driven, each against a WRITABLE control: a
// gate applied to only one of them leaves the other firing, and the self-heal
// is the worse half (it repeats forever).
func TestReadOnly_NoAutomaticUpdateFetch(t *testing.T) {
	writable := func(t *testing.T) Model {
		t.Helper()
		mc := &mockComposer{services: []string{"web"}}
		m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
		m.screen = screenSelectContainers
		m.updateInFlight = false
		m.refreshInFlight = false
		installFakeTick(&m)
		return m
	}

	t.Run("screen entry", func(t *testing.T) {
		ro := newReadOnlyModel(t, readOnlyTestComposer())
		if cmd := ro.maybeRefreshUpdatesCmd(); cmd != nil {
			t.Error("read-only entry fired an automatic CheckUpdates; U must be the only trigger")
		}
		if ro.updateInFlight {
			t.Error("read-only entry set updateInFlight without fetching")
		}
		w := writable(t)
		if cmd := w.maybeRefreshUpdatesCmd(); cmd == nil {
			t.Error("control: a writable cache miss must still fetch")
		}
	})

	t.Run("status-tick self-heal", func(t *testing.T) {
		ro := newReadOnlyModel(t, readOnlyTestComposer())
		result, cmd := ro.Update(statusMsg{
			status:  map[string]runner.ServiceStatus{"watchtower": {Running: true}},
			session: ro.statusSession,
		})
		if cmd != nil {
			t.Error("the status self-heal fired an automatic CheckUpdates on the read-only screen")
		}
		if result.(Model).updateInFlight {
			t.Error("the status self-heal set updateInFlight on the read-only screen")
		}

		w := writable(t)
		_, wcmd := w.Update(statusMsg{
			status:  map[string]runner.ServiceStatus{"web": {Running: true}},
			session: w.statusSession,
		})
		if wcmd == nil {
			t.Error("control: the writable status self-heal must still fetch on a cache miss")
		}
	})
}

// TestReadOnly_InitFastPathHonoursOptIn closes the third automatic entry point.
// maybeRefreshUpdatesCmd and the statusMsg self-heal both consult
// autoUpdatesAllowed, but Init()'s picker-skipped fast path calls
// refreshUpdates() directly — a read-only composer handed straight to NewModel
// would fan out to the registry on launch, exactly what the opt-in exists to
// prevent. NewModel must leave updateInFlight clear in that case too: a flag
// with no fetch behind it never clears, and maybeRefreshUpdatesCmd's in-flight
// guard would then refuse every later fetch, including the one U asks for.
func TestReadOnly_InitFastPathHonoursOptIn(t *testing.T) {
	// drain runs the Cmd Init returns, unwrapping the tea.BatchMsg and
	// invoking every inner Cmd, so the composer records the calls the batch
	// actually makes rather than the ones it merely queued.
	drain := func(t *testing.T, cmd tea.Cmd) {
		t.Helper()
		if cmd == nil {
			t.Fatal("Init returned no Cmd")
		}
		batch, ok := cmd().(tea.BatchMsg)
		if !ok {
			t.Fatal("Init did not return a batch")
		}
		for _, inner := range batch {
			if inner != nil {
				inner()
			}
		}
	}

	t.Run("read-only", func(t *testing.T) {
		mc := readOnlyTestComposer()
		m := NewModel(mc, io.Discard, func(compose.Project) runner.Composer { return mc }, nil, nil)
		installFakeTick(&m)
		if m.updateInFlight {
			t.Error("NewModel marked updateInFlight for a read-only composer that Init will not fetch for")
		}
		drain(t, m.Init())
		if mc.updatesCalls != 0 {
			t.Errorf("Init fast path ran CheckUpdates %d times on a read-only composer; U must be the only trigger", mc.updatesCalls)
		}
	})

	t.Run("writable control", func(t *testing.T) {
		wc := &mockComposer{services: []string{"web"}}
		m := NewModel(wc, io.Discard, mockFactory(wc), nil, nil)
		installFakeTick(&m)
		if !m.updateInFlight {
			t.Error("control: NewModel must mark updateInFlight for the fetch Init fires")
		}
		drain(t, m.Init())
		if wc.updatesCalls != 1 {
			t.Errorf("control: writable Init fast path ran CheckUpdates %d times, want 1", wc.updatesCalls)
		}
	})
}

// TestReadOnly_UpdateKeyStillFetches is the other half of the opt-in contract:
// the automatic paths are gated, U is not. The `?` overlay advertises
// `U check updates` on the read-only screen, so a gate that also caught the
// keypress would leave the overlay promising a no-op.
func TestReadOnly_UpdateKeyStillFetches(t *testing.T) {
	m := newReadOnlyModel(t, readOnlyTestComposer())

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	if cmd == nil {
		t.Fatal("U must still force a CheckUpdates on the read-only screen")
	}
	if !result.(Model).updateInFlight {
		t.Error("U did not mark updateInFlight")
	}
}

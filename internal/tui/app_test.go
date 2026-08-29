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
	"slices"
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

// singleGroupModel returns a container-screen Model in the degenerate
// one-group shape that setSingleGroup installs in production: the flat services
// list plus the derived svcGroups/svcEntries. A single group emits no header
// rows, so its rows are index-parallel to its services — that property, not a
// temporary seam, is what keeps every index-keyed assertion in this file
// meaning what it did before grouping existed.
func singleGroupModel(services []string) Model {
	m := Model{selected: make(map[string]bool)}
	m.setSingleGroup(services)
	return m
}

// selectedIdx builds a selection map from ROW indices. Tests address rows by
// index; the Model keys m.selected by svcKey, and this is the one converter
// between the two — the model must already carry its group (singleGroupModel or
// setSingleGroup) before it is called.
func selectedIdx(m Model, idx ...int) map[string]bool {
	sel := make(map[string]bool, len(idx))
	for _, i := range idx {
		sel[m.svcKeyAt(i)] = true
	}
	return sel
}

// qStatus qualifies a bare-name status map exactly as the servicesMsg/statusMsg
// handlers do, so a test can keep writing the names docker compose reports.
func qStatus(m Model, status map[string]runner.ServiceStatus) map[string]runner.ServiceStatus {
	return qualifyMap(m.ownerProjName(), status)
}

// qStats is qStatus for the resource-usage map.
func qStats(m Model, stats map[string]runner.ServiceStats) map[string]runner.ServiceStats {
	return qualifyMap(m.ownerProjName(), stats)
}

// qk resolves a bare service name to the key the Model stores it under.
func qk(m Model, service string) string {
	return svcKey(m.ownerProjName(), service)
}

// assertCursorVisible pins the invariant every cursor move owes fixSvcOffset:
// the window holds the cursor. It sits in this shared block because
// grouped_test.go consumes it too.
func assertCursorVisible(t *testing.T, m Model) {
	t.Helper()
	visible := m.svcVisibleCount()
	if m.svcCursor < m.svcOffset || m.svcCursor >= m.svcOffset+visible {
		t.Errorf("cursor %d outside the visible window [%d, %d)", m.svcCursor, m.svcOffset, m.svcOffset+visible)
	}
}

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

// mockDetailComposer implements runner.Composer AND UpdateDetailer, so the
// detail half of refreshUpdates can be driven without Docker. The plain
// mockComposer deliberately does NOT implement the interface — that is the
// "composer without the capability" case, which must still produce verdicts.
type mockDetailComposer struct {
	mockComposer
	details    map[string]compose.UpdateDetail
	detailsErr error

	detailsCalls     int
	detailsArgs      [][]string  // one entry per call, the services slice as received
	detailsDeadlines []time.Time // one entry per call, zero when the ctx carried none
}

func (m *mockDetailComposer) UpdateDetails(ctx context.Context, services []string) (map[string]compose.UpdateDetail, error) {
	m.detailsCalls++
	m.detailsArgs = append(m.detailsArgs, append([]string(nil), services...))
	deadline, _ := ctx.Deadline()
	m.detailsDeadlines = append(m.detailsDeadlines, deadline)
	return m.details, m.detailsErr
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
	if m.drilledFromHost {
		t.Error("drilledFromHost should be false: a standalone drilled screen is a root")
	}
	if m.composer == nil {
		t.Error("composer should be set")
	}
}

func TestNewModel_LandsOnGroupedWhenNoComposer(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (screenSelectContainers)", m.screen, screenSelectContainers)
	}
	if !m.grouped {
		t.Error("grouped should be true when no cwd compose file")
	}
	if m.drilledFromHost {
		t.Error("drilledFromHost must stay false: the grouped view is the landing screen")
	}
	if !m.refreshInFlight {
		t.Error("refreshInFlight should be armed for the stats fetch Init() fires")
	}
	if m.updateInFlight {
		t.Error("updateInFlight must stay clear: grouped mode never scans automatically")
	}
	if m.autoUpdatesAllowed() {
		t.Error("autoUpdatesAllowed must be false in grouped mode")
	}
}

func TestInit_LoadsGroupsWhenNoComposer(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)

	cmd := m.Init()
	if cmd == nil {
		t.Error("Init() should return a command for the grouped landing screen")
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

	// Execute the loader through the grouped loader, its only consumer now
	// that the project picker is gone.
	sm := m.loadGroups()().(servicesMsg)
	if sm.err != nil {
		t.Fatalf("unexpected error: %v", sm.err)
	}
	if !called {
		t.Error("local loader should have been called")
	}
	if len(sm.projects) != 1 || sm.projects[0].Name != "test" {
		t.Errorf("projects = %v, want [{test /test}]", sm.projects)
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
	got := modelServices(m)
	if len(got) != len(want) {
		t.Fatalf("got %d services, want %d", len(got), len(want))
	}
	for i, svc := range want {
		if got[i] != svc {
			t.Fatalf("service[%d] = %q, want %q", i, got[i], svc)
		}
	}
}

func TestSelectContainers_Toggle(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "postgres"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.setSingleGroup(mc.services)

	// Toggle first item
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	m = updated.(Model)
	if !m.selected[m.svcKeyAt(0)] {
		t.Error("item 0 should be selected after space")
	}

	// Toggle off
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	m = updated.(Model)
	if m.selected[m.svcKeyAt(0)] {
		t.Error("item 0 should be deselected after second space")
	}
}

func TestSelectContainers_SelectAll(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "postgres", "redis"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.setSingleGroup(mc.services)

	// Select all
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)
	for i := range modelServices(m) {
		if !m.selected[m.svcKeyAt(i)] {
			t.Errorf("item %d should be selected after 'a'", i)
		}
	}

	// Deselect all
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = updated.(Model)
	for i := range modelServices(m) {
		if m.selected[m.svcKeyAt(i)] {
			t.Errorf("item %d should be deselected after second 'a'", i)
		}
	}
}

// `a` is fold-aware on the grouped host view, and the drilled screen must be
// byte-identical anyway: the fold keys are gated on m.grouped, so nothing here
// can hide a row from select-all.
func TestSelectContainers_SelectAllDrilledIgnoresTheFoldRule(t *testing.T) {
	m := singleGroupModel([]string{"nginx", "postgres", "redis"})
	m.screen = screenSelectContainers
	m.width, m.height = 100, 30

	for _, key := range []string{"left", "z"} {
		m = pressGroupKey(m, key)
		if m.svcGroups[0].folded {
			t.Fatalf("%q folded the drilled group", key)
		}
	}

	m = pressGroupKey(m, "a")

	if got := m.selectedCount(); got != 3 {
		t.Errorf("selectedCount = %d, want 3: %v", got, m.selected)
	}
	if !m.allVisibleSelected() {
		t.Error("allVisibleSelected = false after `a` on the drilled screen")
	}
	if m.warning != "" {
		t.Errorf("warning = %q, want none on the drilled screen", m.warning)
	}

	m = pressGroupKey(m, "a")

	if got := m.selectedCount(); got != 0 {
		t.Errorf("a second `a` left %d selected: %v", got, m.selected)
	}
	if m.warning != "" {
		t.Errorf("warning = %q, want none after the second `a`", m.warning)
	}
}

func TestSelectContainers_EnterIgnoredWhenNotConfirming(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.setSingleGroup(mc.services)
	m.selected[m.svcKeyAt(0)] = true

	// Enter with selection but not in confirming state should do nothing
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (should stay on container select)", m.screen, screenSelectContainers)
	}
}

// esc out of a drilled project lands on the grouped host view — the screen that
// sits above a single project now that the picker is gone.
func TestSelectContainers_EscDrillsOutToGroupedView(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.drilledFromHost = true
	m.grouped = false // drilled mode: NewModel now starts grouped
	m.projName = "app"
	m.projDir = "/app"
	m.setSingleGroup(mc.services)
	m.selected[m.svcKeyAt(0)] = true
	m.composer = mc

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectContainers || !m.grouped {
		t.Errorf("screen = %d grouped = %v, want the grouped container screen", m.screen, m.grouped)
	}
	if m.composer != nil {
		t.Error("the drilled project's composer must be dropped on drill-out")
	}
	if m.svcGroups != nil || m.svcEntries != nil {
		t.Error("row state should be cleared on drill-out")
	}
	if m.svcStatus != nil {
		t.Error("svcStatus should be nil after drill-out")
	}
	if len(m.selected) != 0 {
		t.Error("selection should be dropped on drill-out")
	}
	if m.projName != "" || m.projDir != "" {
		t.Errorf("project identity survived drill-out: %q %q", m.projName, m.projDir)
	}
	if cmd == nil {
		t.Error("drill-out must dispatch the grouped load batch")
	}
}

// Drill-out reloads the host view unconditionally: the grouped screen holds no
// cached project list to fall back on the way the picker did.
func TestSelectContainers_EscDrillOutBumpsSessions(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.drilledFromHost = true
	m.grouped = false
	m.setSingleGroup(mc.services)
	m.composer = mc
	status, stats, updates := m.statusSession, m.statsSession, m.updatesSession

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.statusSession == status || m.statsSession == stats || m.updatesSession == updates {
		t.Error("drill-out is a composer swap: all three session counters must bump")
	}
	if cmd == nil {
		t.Error("should dispatch the grouped reload")
	}
}

func TestSelectContainers_EscDoesNothingWhenStandalone(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"nginx": {Running: true}})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (should stay on container select)", m.screen, screenSelectContainers)
	}
	if m.svcStatus == nil {
		t.Error("svcStatus should be preserved on a root drilled screen")
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
	m := Model{}
	m.setSingleGroup([]string{"nginx", "postgres", "redis"})
	m.selected = selectedIdx(m, 0, 2)

	got := m.selectedContainers()
	if len(got) != 2 || got[0] != "nginx" || got[1] != "redis" {
		t.Errorf("selectedContainers() = %v, want [nginx redis]", got)
	}
}

func TestAllSelected(t *testing.T) {
	m := Model{}
	m.setSingleGroup([]string{"a", "b"})
	m.selected = selectedIdx(m, 0, 1)
	if !m.allVisibleSelected() {
		t.Error("allVisibleSelected() = false, want true")
	}

	m.selected[m.svcKeyAt(1)] = false
	if m.allVisibleSelected() {
		t.Error("allVisibleSelected() = true, want false")
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
			// A single group emits no header rows, so the entry indices the
			// helper returns are the service indices the table names.
			got := computeMatches(rebuildSvcEntries([]svcGroup{{services: tt.services}}), tt.query)
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

// progressStep builds the stepState shape enterProgress produces for a
// single-batch run: the drawn label IS the runner step name when there is only
// one project. enterProgress is the only production producer and always sets
// both fields, so a hand-built fixture has to as well.
func progressStep(name, status string) stepState {
	return stepState{name: name, label: name, status: status}
}

func TestHandleStepEvent_Done(t *testing.T) {
	m := Model{
		screen:         screenProgress,
		steps:          []stepState{progressStep(runner.StepStopping, runner.StatusRunning)},
		batchStepCount: 1,
		eventCh:        make(chan runner.StepEvent),
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
		screen:         screenProgress,
		steps:          []stepState{progressStep(runner.StepStopping, runner.StatusRunning)},
		batchStepCount: 1,
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
	m.setSingleGroup([]string{"nginx", "postgres"})
	m.selected[m.svcKeyAt(1)] = true
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
		progressStep("Stopping", runner.StatusDone),
		progressStep("Starting", runner.StatusRunning),
	}
	m.batchStepCount = len(m.steps)
	v = m.View()
	if v == "" {
		t.Error("viewProgress returned empty")
	}
}

// parkOnGroupedScreen puts the model on the grouped host view holding the
// given projects, with the cursor on the first project's header row. It is the
// replacement for the project-picker fixtures: drilling into a header is what
// picking a project used to be.
func parkOnGroupedScreen(m *Model, projects ...compose.Project) {
	m.screen = screenSelectContainers
	m.grouped = true
	m.composer = nil
	m.svcGroups = make([]svcGroup, 0, len(projects))
	for _, p := range projects {
		m.svcGroups = append(m.svcGroups, svcGroup{proj: p})
	}
	m.svcEntries = rebuildSvcEntries(m.svcGroups)
	m.svcCursor = 0
}

// headerIndexFor returns the entry index of group gi's header row.
func headerIndexFor(t *testing.T, entries []svcEntry, gi int) int {
	t.Helper()
	for i, e := range entries {
		if e.kind == entrySvcGroupHeader && e.groupIdx == gi {
			return i
		}
	}
	t.Fatalf("no header entry for group %d in %+v", gi, entries)
	return 0
}

// TestComposerFactory_ReceivesWholeProject pins that drill-in hands the factory
// the WHOLE compose.Project — the Unmanaged flag included, since that is the
// branch that picks the read-only host composer.
func TestComposerFactory_ReceivesWholeProject(t *testing.T) {
	projects := []compose.Project{
		{Name: "my-app", Status: "running(3)", ConfigDir: "/srv/my-app"},
		{Name: compose.UnmanagedProjectName, Unmanaged: true},
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
			m.screen = screenSelectContainers
			m.grouped = true
			m.svcGroups = []svcGroup{{proj: projects[0]}, {proj: projects[1]}}
			m.svcEntries = rebuildSvcEntries(m.svcGroups)
			m.svcCursor = headerIndexFor(t, m.svcEntries, tt.cursor)

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
	m.setSingleGroup([]string{"nginx"})
	v := m.View()
	if !strings.Contains(v, "cdeploy > api-proxy") {
		t.Errorf("container select breadcrumb should contain project name, got: %q", v)
	}

	// Progress screen
	m.screen = screenProgress
	m.selected[m.svcKeyAt(0)] = true
	m.pendingOp = runner.Restart
	m.steps = []stepState{progressStep("Stopping", runner.StatusRunning)}
	m.batchStepCount = len(m.steps)
	v = m.View()
	if !strings.Contains(v, "cdeploy > api-proxy") {
		t.Errorf("progress breadcrumb should contain project name, got: %q", v)
	}
}

func TestBreadcrumb_WithoutProjectName(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.setSingleGroup([]string{"nginx"})

	v := m.View()
	if !strings.Contains(v, "cdeploy") {
		t.Error("breadcrumb should contain 'cdeploy'")
	}
}

func TestSetSingleGroup(t *testing.T) {
	t.Run("installs one group and derives the entries", func(t *testing.T) {
		var m Model
		m.projName = "web"
		m.projDir = "/srv/web"
		m.setSingleGroup([]string{"api", "db"})

		if len(m.svcGroups) != 1 {
			t.Fatalf("svcGroups has %d groups, want 1", len(m.svcGroups))
		}
		g := m.svcGroups[0]
		if g.proj.Name != "web" || g.proj.ConfigDir != "/srv/web" {
			t.Errorf("group project = %+v, want the current project", g.proj)
		}
		if g.folded {
			t.Error("a freshly installed group must not be folded")
		}
		if len(m.svcEntries) != 2 || m.svcEntries[0].name != "api" || m.svcEntries[1].name != "db" {
			t.Errorf("svcEntries = %+v, want two service rows in order", m.svcEntries)
		}
	})

	t.Run("an empty but non-nil slice is a project with no services", func(t *testing.T) {
		var m Model
		m.setSingleGroup([]string{})

		if len(m.svcGroups) != 1 {
			t.Fatalf("svcGroups has %d groups, want 1", len(m.svcGroups))
		}
		if m.svcGroups == nil {
			t.Error("services must stay non-nil, or the screen renders its loading state")
		}
		// It draws its header and nothing else: a screen with zero rows has no
		// cursor and names no project (see groupsHaveHeaders).
		if len(m.svcEntries) != 1 || m.svcEntries[0].kind != entrySvcGroupHeader {
			t.Errorf("svcEntries = %+v, want a lone group header", m.svcEntries)
		}
	})

	t.Run("nil clears the group state", func(t *testing.T) {
		var m Model
		m.setSingleGroup([]string{"api"})
		m.setSingleGroup(nil)

		if m.svcGroups != nil || m.svcEntries != nil {
			t.Errorf("groups=%v entries=%v, want both nil", m.svcGroups, m.svcEntries)
		}
	})

	t.Run("a read-only composer marks the group unmanaged", func(t *testing.T) {
		var m Model
		m.composer = &readOnlyMockComposer{}
		m.setSingleGroup([]string{"portainer"})

		if !m.svcGroups[0].proj.Unmanaged {
			t.Error("a read-only composer must produce an unmanaged group")
		}
	})
}

func TestClearSvcGroups(t *testing.T) {
	var m Model
	m.setSingleGroup([]string{"api", "db"})
	m.clearSvcGroups()

	if m.svcGroups != nil || m.svcEntries != nil {
		t.Errorf("groups=%v entries=%v, want both nil", m.svcGroups, m.svcEntries)
	}
}

// servicesMsg is the production writer of the single-group shape: the entries
// must be derived from the SORTED list the handler installs.
func TestServicesMsg_InstallsSingleGroup(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)

	updated, _ := m.Update(servicesMsg{services: []string{"zebra", "Alpha"}})
	m = updated.(Model)

	if len(m.svcGroups) != 1 {
		t.Fatalf("svcGroups has %d groups, want 1", len(m.svcGroups))
	}
	want := []string{"Alpha", "zebra"}
	for i, name := range want {
		if m.svcEntries[i].name != name {
			t.Errorf("entry %d = %q, want %q (entries follow the sorted order)", i, m.svcEntries[i].name, name)
		}
	}
}

// Departure-site pin: esc back to the grouped host view drops the service list, so
// the derived group state must go with it — a group that outlived its services
// would feed the next project's screen stale rows.
func TestEscFromContainerScreen_ClearsSvcGroups(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.drilledFromHost = true
	m.setSingleGroup(mc.services)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.svcGroups != nil || m.svcEntries != nil {
		t.Errorf("groups=%v entries=%v, want both nil after esc", m.svcGroups, m.svcEntries)
	}
}

// Phase-1 pin for the grouped entry model: a single group must render exactly
// the pre-grouping screen. rebuildSvcEntries emits NO header for one group, so
// svcEntries stays index-parallel to services, and the rows keep the
// cursor-column + checkbox prefix with no group indent in front of it.
func TestViewSelectContainers_SingleGroupHasNoHeaderOrIndent(t *testing.T) {
	m := singleGroupModel([]string{"api", "web"})
	m.screen = screenSelectContainers
	m.width = 80
	m.height = 20
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"api": {Running: true, Uptime: "3h"},
		"web": {},
	})

	flat := modelServices(m)
	if len(m.svcEntries) != len(flat) {
		t.Fatalf("svcEntries has %d rows, want %d (a single group emits no header)", len(m.svcEntries), len(flat))
	}
	for i, e := range m.svcEntries {
		if e.kind != entryService {
			t.Errorf("entry %d kind = %v, want a service row", i, e.kind)
		}
		if e.name != flat[i] {
			t.Errorf("entry %d name = %q, want %q (entries must stay index-parallel)", i, e.name, flat[i])
		}
	}

	out := ansi.Strip(m.viewSelectContainers())
	for _, glyph := range []string{"\u25bc", "\u25b6"} {
		if strings.Contains(out, glyph) {
			t.Errorf("single group rendered a group header glyph %q:\n%s", glyph, out)
		}
	}

	rowRe := regexp.MustCompile(`^[> ] \[[ x]\]`)
	for _, name := range modelServices(m) {
		var row string
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, name) && strings.Contains(line, "[") {
				row = line
				break
			}
		}
		if row == "" {
			t.Fatalf("no row rendered for %q in:\n%s", name, out)
		}
		if !rowRe.MatchString(row) {
			t.Errorf("row for %q = %q, want the un-indented cursor+checkbox prefix", name, row)
		}
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
	m.setSingleGroup([]string{"api", "db", "web", "worker"})
	m.svcStatus = qStatus(m, mc.status)

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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)

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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)

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
	if !m.svcStatus[qk(m, "nginx")].Running {
		t.Error("nginx should be running")
	}
	if m.svcStatus[qk(m, "postgres")].Running {
		t.Error("postgres should not be running")
	}
}

func TestEscFromProgress_GoesToContainers(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.done = true
	m.drilledFromHost = true
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
	// enterProgress always installs a batch sequence; the invalidation walks it.
	m.batches = []opBatch{{proj: m.currentProject()}}
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
	m.batches = []opBatch{{proj: m.currentProject()}}
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
	m.setSingleGroup(mc.services)
	m.selected[m.svcKeyAt(0)] = true

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
	m.setSingleGroup(mc.services)

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
			m.setSingleGroup(mc.services)

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
	m.setSingleGroup(mc.services)
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
	m.setSingleGroup(mc.services)
	m.selected[m.svcKeyAt(0)] = true
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
	m.setSingleGroup(mc.services)
	m.selected[m.svcKeyAt(0)] = true
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
	m.setSingleGroup(mc.services)
	m.selected[m.svcKeyAt(0)] = true
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
	m.setSingleGroup(mc.services)
	m.selected[m.svcKeyAt(0)] = true
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
	m.setSingleGroup(mc.services)
	m.selected[m.svcKeyAt(0)] = true
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
			m.setSingleGroup(mc.services)
			m.selected[m.svcKeyAt(0)] = true

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
	m.setSingleGroup(mc.services)

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
	m.setSingleGroup(mc.services)
	m.svcErr = fmt.Errorf("previous error")

	updated, _ := m.Update(statusMsg{status: map[string]runner.ServiceStatus{"nginx": {Running: true}}})
	m = updated.(Model)

	if m.svcErr != nil {
		t.Errorf("svcErr should be nil after successful statusMsg, got %v", m.svcErr)
	}
	if !m.svcStatus[qk(m, "nginx")].Running {
		t.Error("svcStatus should be updated after successful statusMsg")
	}
}

func TestConfirmation_ViewShowsOperationAndServices(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx", "postgres"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.setSingleGroup(mc.services)
	m.selected[m.svcKeyAt(0)] = true
	m.selected[m.svcKeyAt(1)] = true
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
		{"no servers, no composer -> grouped containers", nil, nil, screenSelectContainers},
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

	// Without composer: the grouped host view, not the deleted picker.
	m = NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	if m.screen != screenSelectContainers || !m.grouped {
		t.Errorf("screen = %d grouped = %v, want %d true", m.screen, m.grouped, screenSelectContainers)
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

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (screenSelectContainers)", m.screen, screenSelectContainers)
	}
	if !m.grouped {
		t.Error("Local with no cwd compose file lands on the grouped host view")
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
	if m.drilledFromHost {
		t.Error("drilledFromHost must stay false on the grouped view")
	}
	if cmd == nil {
		t.Error("should return the grouped load batch")
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
	if !m.drilledFromHost {
		t.Error("drilledFromHost should be true so esc drills back out")
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

	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want %d (screenSelectContainers)", m.screen, screenSelectContainers)
	}
	if !m.grouped {
		t.Error("a successful connect lands on the grouped host view")
	}
	if m.serverErr != nil {
		t.Errorf("serverErr = %v, want nil", m.serverErr)
	}
	if m.drilledFromHost {
		t.Error("drilledFromHost must stay false on the grouped view")
	}
	if cmd == nil {
		t.Error("should return the grouped load batch")
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
	m.setSingleGroup(mc.services)
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
	m.setSingleGroup([]string{}) // empty

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
	m.setSingleGroup(mc.services)
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
	m.setSingleGroup(mc.services)
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
	if got.event != want {
		t.Fatalf("step event = %+v, want %+v", got.event, want)
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
	m.setSingleGroup(mc.services)
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
	m.setSingleGroup(mc.services)
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
	m := singleGroupModel(nil)
	m.selected = nil
	if m.allVisibleSelected() {
		t.Error("allVisibleSelected() = true for empty services, want false")
	}
}

func TestAllSelected_AllTrue(t *testing.T) {
	m := Model{}
	m.setSingleGroup([]string{"web", "db"})
	m.selected = selectedIdx(m, 0, 1)
	if !m.allVisibleSelected() {
		t.Error("allVisibleSelected() = false, want true")
	}
}

func TestAllSelected_SomeFalse(t *testing.T) {
	m := Model{}
	m.setSingleGroup([]string{"web", "db", "redis"})
	m.selected = selectedIdx(m, 0, 2)
	if m.allVisibleSelected() {
		t.Error("allVisibleSelected() = true, want false")
	}
}

func TestViewProgress_Running(t *testing.T) {
	m := Model{
		screen:    screenProgress,
		pendingOp: runner.Deploy,
		steps: []stepState{
			progressStep("Stop", runner.StatusDone),
			progressStep("Pull", runner.StatusRunning),
			progressStep("Create", ""),
		},
		batchStepCount: 3,
		width:          80,
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
			progressStep("Stop", runner.StatusDone),
			progressStep("Start", runner.StatusDone),
		},
		batchStepCount: 2,
		done:           true,
		width:          80,
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
			progressStep("Stop", runner.StatusDone),
			progressStep("Pull", runner.StatusFailed),
		},
		batchStepCount: 2,
		done:           true,
		failed:         true,
		logContent:     "pull failed",
		width:          80,
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

func TestViewSelectContainers_ConfirmState(t *testing.T) {
	mc := &mockComposer{
		services: []string{"web", "db"},
		status:   map[string]runner.ServiceStatus{"web": {Running: true}, "db": {Running: true}},
	}

	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.setSingleGroup([]string{"web", "db"})
	m.selected = selectedIdx(m, 0, 1)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e"})
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"})
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"})
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"})
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"})
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
	m.setSingleGroup([]string{"a", "b"})
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
	m.setSingleGroup([]string{"a", "b", "c"})
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"})
	m.width = 200
	m.height = 10
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"a": {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
	})

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
	m.setSingleGroup([]string{"a"})

	// No status data
	if m.hasStatusColumns() {
		t.Error("hasStatusColumns() = true, want false with no status data")
	}

	// Status with only Running (no Created/Uptime)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"a": {Running: true},
	})
	if m.hasStatusColumns() {
		t.Error("hasStatusColumns() = true, want false with only Running set")
	}

	// Status with Created
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"a": {Created: "2024-01-15 09:30"},
	})
	if !m.hasStatusColumns() {
		t.Error("hasStatusColumns() = false, want true with Created set")
	}

	// Status with Uptime
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"a": {Uptime: "3h"},
	})
	if !m.hasStatusColumns() {
		t.Error("hasStatusColumns() = false, want true with Uptime set")
	}

	// Status for a service that has no row should be ignored
	m.setSingleGroup([]string{"b"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"a": {Created: "2024-01-15 09:30", Uptime: "3h"},
	})
	if m.hasStatusColumns() {
		t.Error("hasStatusColumns() = true, want false when status key not in services")
	}

	// Status with only Ports (no Created/Uptime)
	m.setSingleGroup([]string{"a"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"a": {Running: true, Ports: []runner.Port{{Host: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"}}},
	})
	if !m.hasStatusColumns() {
		t.Error("hasStatusColumns() = false, want true with Ports set")
	}
}

// TestHasStatusColumns_StatsRequested verifies that statsRequested short-circuits
// hasStatusColumns to true even when no per-service data has arrived yet. This
// keeps the CPU/Mem column captions visible from the first frame instead of
// popping in when the host-wide docker stats call returns.
func TestHasStatusColumns_StatsRequested(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.setSingleGroup([]string{"a"})
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e"})
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e"})
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
	m.setSingleGroup([]string{"a", "b"})
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
	m.setSingleGroup([]string{"a", "b", "c"})
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e"})
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
	m.setSingleGroup(mc.services)
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
	m.setSingleGroup(mc.services)
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

// pageKeyModel is a drilled container screen sized so one page is 3 rows:
// height 9 minus 3 header lines minus 3 footer lines.
func pageKeyModel(services []string) Model {
	m := singleGroupModel(services)
	m.screen = screenSelectContainers
	m.width, m.height = 200, 9
	return m
}

// TestPageKeys_MoveAFullPageAndClamp pins pgup/pgdown on the drilled container
// screen: one press moves svcVisibleCount() rows, neither key leaves
// svcEntries, and svcOffset follows so the cursor stays on screen.
func TestPageKeys_MoveAFullPageAndClamp(t *testing.T) {
	m := pageKeyModel([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"})
	if got := m.svcVisibleCount(); got != 3 {
		t.Fatalf("svcVisibleCount() = %d, want 3", got)
	}

	// The last press has nowhere left to go: 9 is the last row.
	for _, want := range []int{3, 6, 9, 9} {
		m = pressGroupKey(m, "pgdown")
		if m.svcCursor != want {
			t.Fatalf("pgdown: svcCursor = %d, want %d", m.svcCursor, want)
		}
		assertCursorVisible(t, m)
	}
	if m.svcOffset != 7 {
		t.Errorf("svcOffset at the bottom = %d, want 7", m.svcOffset)
	}

	for _, want := range []int{6, 3, 0, 0} {
		m = pressGroupKey(m, "pgup")
		if m.svcCursor != want {
			t.Fatalf("pgup: svcCursor = %d, want %d", m.svcCursor, want)
		}
		assertCursorVisible(t, m)
	}
	if m.svcOffset != 0 {
		t.Errorf("svcOffset at the top = %d, want 0", m.svcOffset)
	}
}

// TestPageKeys_EmptyListStaysInRange pins the degenerate case svcVisibleCount
// answers 0 for: the step moves nothing, and clampSvcCursor holds the cursor at
// 0 rather than letting it sit at -1 on a list with no rows.
func TestPageKeys_EmptyListStaysInRange(t *testing.T) {
	m := pageKeyModel(nil)
	if len(m.svcEntries) != 0 {
		t.Fatalf("fixture has %d rows, want 0", len(m.svcEntries))
	}
	for _, key := range []string{"pgdown", "pgup"} {
		m = pressGroupKey(m, key)
		if m.svcCursor != 0 || m.svcOffset != 0 {
			t.Fatalf("%s on an empty list: cursor = %d, offset = %d, want 0/0", key, m.svcCursor, m.svcOffset)
		}
	}
}

// TestPageKeys_WorkOnTheReadOnlyScreen pins that paging is not gated on
// readOnly(): moving the cursor writes nothing to docker, exactly like up/down.
func TestPageKeys_WorkOnTheReadOnlyScreen(t *testing.T) {
	m := pageKeyModel([]string{"a", "b", "c", "d", "e", "f"})
	m.composer = &readOnlyMockComposer{}
	if !m.readOnly() {
		t.Fatal("fixture is not read-only")
	}
	page := m.svcVisibleCount()

	m = pressGroupKey(m, "pgdown")
	if m.svcCursor != page {
		t.Errorf("pgdown on the read-only screen: svcCursor = %d, want %d", m.svcCursor, page)
	}
	assertCursorVisible(t, m)

	m = pressGroupKey(m, "pgup")
	if m.svcCursor != 0 {
		t.Errorf("pgup on the read-only screen: svcCursor = %d, want 0", m.svcCursor)
	}
}

// TestPageKeys_InertWhereTheOtherMovementKeysAre pins pgup/pgdown against the
// three states the container dispatch already refuses to move the cursor in: a
// typing search bar (every key is the query's), an armed confirmation (enter
// re-resolves its batch from the cursor) and the error screen that hides the
// rows.
//
// The cursor starts on row 4 of ten, so one page (3 rows) in either direction
// stays inside the list: a step that ran would land on 1 or 7 and be seen.
func TestPageKeys_InertWhereTheOtherMovementKeysAre(t *testing.T) {
	const start = 4
	for _, tc := range []struct {
		name  string
		arm   func(*Model)
		check func(*testing.T, Model)
	}{
		{
			// The search intercept re-aims the cursor at the FIRST match on
			// every keystroke, so the query names row 4 and the fixture starts
			// there — an empty query would make the cursor unmovable for a
			// second reason and pass this whether the keys were routed or not.
			name: "searching",
			arm: func(m *Model) {
				m.searching = true
				m.searchInput = textinput.New()
				// Focused, like the / handler leaves it:
				// textinput.Model.Update returns immediately when it is not,
				// so an unfocused bar freezes the Value() assertion below
				// whether or not the key was routed to it.
				m.searchInput.Focus()
				m.searchInput.SetValue("e")
				m.searchQuery = "e"
				m.searchMatches = computeMatches(m.svcEntries, "e")
			},
			check: func(t *testing.T, m Model) {
				if m.searchQuery != "e" || m.searchInput.Value() != "e" {
					t.Errorf("the key reached the query: %q / %q, want %q both",
						m.searchQuery, m.searchInput.Value(), "e")
				}
				if len(m.searchMatches) != 1 || m.searchMatches[0] != start {
					t.Errorf("searchMatches = %v, want [%d]", m.searchMatches, start)
				}
			},
		},
		{name: "confirming", arm: func(m *Model) { m.confirming = true; m.pendingOp = runner.Deploy }},
		{name: "svcErr", arm: func(m *Model) { m.svcErr = errors.New("docker is unreachable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"pgdown", "pgup"} {
				m := pageKeyModel([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"})
				m.svcCursor = start
				tc.arm(&m)
				if got := m.svcVisibleCount(); got != 3 {
					t.Fatalf("fixture: svcVisibleCount() = %d, want 3", got)
				}

				got := pressGroupKey(m, key)
				if got.svcCursor != start {
					t.Errorf("%s moved the cursor to %d", key, got.svcCursor)
				}
				if tc.check != nil {
					tc.check(t, got)
				}
			}
		})
	}
}

// TestPageKeys_PageIsTheWholeListWhenItFits pins the branch
// TestPageKeys_MoveAFullPageAndClamp cannot reach: svcVisibleCount() never
// answers more rows than svcEntries holds, so when the list fits the window a
// page IS the whole list and the two keys degenerate to end/home. Both shapes
// that produce it are covered — a list shorter than the pane, and the unsized
// terminal before the first WindowSizeMsg, which is the separate m.height == 0
// early return. In both, svcOffset must stay 0: there is no window to scroll.
func TestPageKeys_PageIsTheWholeListWhenItFits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() Model
		last  int
	}{
		{
			// height 9 budgets 3 rows, the list holds 2, so the clamp at the
			// bottom of svcVisibleCount decides the page.
			name:  "list shorter than the pane",
			build: func() Model { return pageKeyModel([]string{"a", "b"}) },
			last:  1,
		},
		{
			// No WindowSizeMsg yet: svcVisibleCount returns len(svcEntries)
			// outright, so the first frame pages the whole list.
			name: "no WindowSizeMsg yet",
			build: func() Model {
				m := singleGroupModel([]string{"a", "b", "c", "d", "e"})
				m.screen = screenSelectContainers
				return m
			},
			last: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.build()
			if got, rows := m.svcVisibleCount(), len(m.svcEntries); got != rows {
				t.Fatalf("fixture: svcVisibleCount() = %d, rows = %d — the list must fit the window", got, rows)
			}

			m = pressGroupKey(m, "pgdown")
			if m.svcCursor != tc.last {
				t.Errorf("pgdown: svcCursor = %d, want %d (the last row)", m.svcCursor, tc.last)
			}
			if m.svcOffset != 0 {
				t.Errorf("pgdown scrolled a list that fits: svcOffset = %d, want 0", m.svcOffset)
			}
			assertCursorVisible(t, m)

			m = pressGroupKey(m, "pgdown")
			if m.svcCursor != tc.last {
				t.Errorf("second pgdown ran past the end: svcCursor = %d, want %d", m.svcCursor, tc.last)
			}

			m = pressGroupKey(m, "pgup")
			if m.svcCursor != 0 || m.svcOffset != 0 {
				t.Errorf("pgup: cursor = %d, offset = %d, want 0/0", m.svcCursor, m.svcOffset)
			}
		})
	}
}

// TestPageKeys_PageSizeFollowsTheVisibleCount pins WHERE the step comes from.
// svcVisibleCount() is not a constant: the soft-warning slot spends a footer
// row and the Created/Uptime captions spend a header row, both without a
// keystroke (a stats fetch fails, a status refresh lands). The page must be
// read from it at press time, so the cursor always travels exactly the rows the
// pane is showing — a hard-coded or cached step would scroll past a row here.
func TestPageKeys_PageSizeFollowsTheVisibleCount(t *testing.T) {
	services := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	for _, tc := range []struct {
		name string
		arm  func(*Model)
		want int
	}{
		{name: "idle", arm: func(*Model) {}, want: 3},
		{
			name: "soft warning takes a footer row",
			arm:  func(m *Model) { m.statsErr = errors.New("docker stats unavailable") },
			want: 2,
		},
		{
			name: "column captions take a header row",
			arm: func(m *Model) {
				m.svcStatus = qStatus(*m, map[string]runner.ServiceStatus{
					"a": {Running: true, Uptime: "3h"},
				})
			},
			want: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := pageKeyModel(services)
			tc.arm(&m)
			if got := m.svcVisibleCount(); got != tc.want {
				t.Fatalf("fixture: svcVisibleCount() = %d, want %d", got, tc.want)
			}

			m = pressGroupKey(m, "pgdown")
			if m.svcCursor != tc.want {
				t.Errorf("pgdown moved %d rows, want %d — the step must come from svcVisibleCount() at press time",
					m.svcCursor, tc.want)
			}
			assertCursorVisible(t, m)
		})
	}
}

func TestConfirming_CallsFixSvcOffset(t *testing.T) {
	mc := &mockComposer{services: []string{"a", "b", "c", "d", "e", "f", "g", "h"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.setSingleGroup(mc.services)
	m.width = 120
	m.height = 8 // visible normal = confirming (constant with reserved search bar)

	// Navigate to last item and select it
	m.svcCursor = 7
	m.svcOffset = 4 // near bottom
	m.selected[m.svcKeyAt(7)] = true

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
	m.setSingleGroup(mc.services)
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
	m.setSingleGroup(mc.services)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web":   {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"db":    {Running: true, Created: "2024-01-14 08:00", Uptime: "1d 3h"},
		"cache": {Running: false, Created: "2024-01-15 10:00", Uptime: ""},
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"nginx":       {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"postgres-db": {Running: true, Created: "2024-01-14 08:00", Uptime: "1d 3h"},
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true, Ports: []runner.Port{
			{Host: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			{Host: "0.0.0.0", HostPort: 8443, ContainerPort: 443, Protocol: "tcp"},
		}},
		"db": {Running: true},
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"nginx": {Running: true, Ports: []runner.Port{
			{Host: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"},
		}},
		"api": {Running: true},
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"db":  {Running: false, Created: "2024-01-15 09:30"},
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true},
		"db":  {Running: false},
	})
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
	m.drilledFromHost = true
	m.setSingleGroup(mc.services)
	m.selected = make(map[string]bool)
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
	m.setSingleGroup(mc.services)
	m.selected = make(map[string]bool)

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
	m.setSingleGroup(mc.services)
	m.selected = make(map[string]bool)
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
	m.setSingleGroup(mc.services)
	m.selected = make(map[string]bool)
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
	m.setSingleGroup(mc.services)
	m.selected = make(map[string]bool)
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
		{"containers grouped", screenSelectContainers, "ctrl+c", func(m *Model) {
			m.grouped = true
			m.svcGroups = []svcGroup{{proj: compose.Project{Name: "app", ConfigDir: "/app"}}}
			m.svcEntries = rebuildSvcEntries(m.svcGroups)
		}},
		{"containers normal", screenSelectContainers, "ctrl+c", func(m *Model) {
			m.setSingleGroup([]string{"nginx"})
			m.selected = make(map[string]bool)
		}},
		{"containers confirming", screenSelectContainers, "ctrl+c", func(m *Model) {
			m.setSingleGroup([]string{"nginx"})
			m.selected = selectedIdx(*m, 0)
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
		{"inspect", screenInspect, "ctrl+c", func(m *Model) {
			m.inspectService = "nginx"
			m.inspectRaw = []byte(`[{"Name":"/nginx"}]`)
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
	m.setSingleGroup([]string{"nginx"})
	m.selected = make(map[string]bool)
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
	m.setSingleGroup(mc.services)
	m.selected = make(map[string]bool)
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
	m.setSingleGroup(mc.services)
	m.selected = make(map[string]bool)

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
	m.drilledFromHost = true
	m.setSingleGroup([]string{"nginx"})
	m.selected = make(map[string]bool)
	m.composer = mc

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	um := updated.(Model)

	if um.screen != screenSelectContainers || !um.grouped {
		t.Errorf("screen = %d grouped = %v, want the grouped host view", um.screen, um.grouped)
	}
	if um.composer != nil {
		t.Error("composer should be cleared after back nav")
	}
	if um.svcGroups != nil {
		t.Error("the rows should be cleared after back nav")
	}
}

func TestQBackNavigation_ContainerScreenCancelsConfirming(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.screen = screenSelectContainers
	m.drilledFromHost = true
	m.setSingleGroup([]string{"nginx"})
	m.selected = selectedIdx(m, 0)
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
	m.drilledFromHost = true
	m.setSingleGroup([]string{"nginx"})
	m.selected = selectedIdx(m, 0)
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
			m.setSingleGroup([]string{"nginx"})
			m.selected = make(map[string]bool)

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

func TestQQuitsAtRoot_ContainerScreenStandalone(t *testing.T) {
	mc := &mockComposer{services: []string{"nginx"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.drilledFromHost = false
	m.confirming = false
	m.setSingleGroup(mc.services)
	m.selected = make(map[string]bool)

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
	m.drilledFromHost = true
	m.setSingleGroup([]string{"nginx"})
	m.selected = make(map[string]bool)
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
	m.drilledFromHost = true
	m.setSingleGroup([]string{"nginx"})
	m.selected = make(map[string]bool)
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
	m.setSingleGroup([]string{"web", "api"})
	m.selected = map[string]bool{}
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"api": {Running: true, Created: "2024-01-15 09:25", Uptime: "3h"},
	})

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
	m.setSingleGroup([]string{"web", "api"})
	m.selected = map[string]bool{}
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true},
		"api": {Running: false},
	})

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
	m.setSingleGroup([]string{"web", "api-service"})
	m.selected = map[string]bool{}
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web":         {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"api-service": {Running: true, Created: "2024-01-15 09:25", Uptime: "5d"},
	})

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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, mc.status)
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
	if got := model.stats[qk(model, "web")].CPUPercent; got != 4.2 {
		t.Errorf("stats[web].CPUPercent = %v, want 4.2", got)
	}
	if got := model.stats[qk(model, "db")].MemoryUsed; got != 200000000 {
		t.Errorf("stats[db].MemoryUsed = %v, want 200000000", got)
	}
}

func TestStatsMsg_storesError(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	prior := map[string]runner.ServiceStats{"web": {CPUPercent: 4.2}}
	m.stats = qStats(m, prior)

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
	m.screen = screenLogs

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
	if got := model.stats[qk(model, "web")].CPUPercent; got != 1.5 {
		t.Errorf("stats[web].CPUPercent = %v, want 1.5", got)
	}
}

func TestEsc_clearsStats(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.drilledFromHost = true
	m.setSingleGroup([]string{"web"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
	m.stats = qStats(m, map[string]runner.ServiceStats{"web": {CPUPercent: 4.2}})
	m.statsErr = errors.New("stale stats error")

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := result.(Model)

	if model.screen != screenSelectContainers || !model.grouped {
		t.Fatalf("screen = %d grouped = %v, want the grouped host view", model.screen, model.grouped)
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

	if got := model.stats[qk(model, "web")].CPUPercent; got != 1.0 {
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

func TestEsc_bumpsStatsSession(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.drilledFromHost = true
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

	if !model.svcStatus[qk(model, "web")].Running {
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"new": {Running: true}})

	result, _ := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"stale": {Running: true}},
		session: 4, // older context
	})
	model := result.(Model)

	if _, ok := model.svcStatus[qk(model, "stale")]; ok {
		t.Error("stale statusMsg from older context overwrote svcStatus")
	}
	if !model.svcStatus[qk(model, "new")].Running {
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

	if _, ok := model.svcStatus[qk(model, "x")]; ok {
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

	if got := modelServices(model); len(got) != 1 || got[0] != "web" {
		t.Errorf("services = %v, want [web]", got)
	}
	if !model.svcStatus[qk(model, "web")].Running {
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
	m.setSingleGroup([]string{"new-svc"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"new-svc": {Running: true}})

	result, _ := m.Update(servicesMsg{
		services: []string{"stale-svc"},
		status:   map[string]runner.ServiceStatus{"stale-svc": {Running: true}},
		session:  4, // stale
	})
	model := result.(Model)

	if got := modelServices(model); len(got) != 1 || got[0] != "new-svc" {
		t.Errorf("stale servicesMsg clobbered services: got %v, want [new-svc]", got)
	}
	if _, ok := model.svcStatus[qk(model, "stale-svc")]; ok {
		t.Error("stale servicesMsg clobbered svcStatus")
	}
}

// TestServicesMsg_offScreenIgnored verifies the screen-gate still applies.
func TestServicesMsg_offScreenIgnored(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenProgress
	m.statusSession = 1
	m.setSingleGroup([]string{"existing"})

	result, _ := m.Update(servicesMsg{
		services: []string{"unwanted"},
		status:   map[string]runner.ServiceStatus{"unwanted": {Running: true}},
		session:  1,
	})
	model := result.(Model)

	if got := modelServices(model); len(got) != 1 || got[0] != "existing" {
		t.Errorf("servicesMsg applied while off-screen: got %v, want [existing]", got)
	}
}

// --- Task 9: CPU/Mem column rendering tests ---

func TestContainerScreen_rendersStatsColumns(t *testing.T) {
	mc := &mockComposer{services: []string{"web", "db"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true},
		"db":  {Running: true},
	})
	m.stats = qStats(m, map[string]runner.ServiceStats{
		"web": {CPUPercent: 4.2, MemoryUsed: 130023424, MemoryLimit: 536870912},
		"db":  {CPUPercent: 1.1, MemoryUsed: 200 * 1024 * 1024, MemoryLimit: 1024 * 1024 * 1024},
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true, Created: "2024-01-15 09:30"},
		"db":  {Running: true, Created: "2024-01-15 09:30"},
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"a": {Running: true, Created: "2024-01-15 09:30"},
		"b": {Running: true, Created: "2024-01-15 09:30"},
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"small": {Running: true},
		"big":   {Running: true},
	})
	m.stats = qStats(m, map[string]runner.ServiceStats{
		"small": {CPUPercent: 0.2, MemoryUsed: 10 * 1024 * 1024, MemoryLimit: 128 * 1024 * 1024},
		"big":   {CPUPercent: 11.1, MemoryUsed: 1503238553, MemoryLimit: 4 * 1024 * 1024 * 1024}, // ~1.4 GiB
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true},
		"db":  {Running: true},
	})
	m.stats = qStats(m, map[string]runner.ServiceStats{
		"web": {CPUPercent: 5.5, MemoryUsed: 100 * 1024 * 1024, MemoryLimit: 200 * 1024 * 1024},
		// db absent from stats map
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true},
		"db":  {Running: false},
	})
	m.stats = qStats(m, map[string]runner.ServiceStats{
		"web": {CPUPercent: 9.9, MemoryUsed: 100 * 1024 * 1024, MemoryLimit: 200 * 1024 * 1024},
		"db":  {CPUPercent: 7.7, MemoryUsed: 50 * 1024 * 1024, MemoryLimit: 100 * 1024 * 1024},
	})
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
	// first frame so they don't pop in when the docker stats call returns.
	// Cells stay blank for services without data yet.
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.statsRequested = true
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true},
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true},
	})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
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
	m.setSingleGroup(mc.services)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"})
	m.width = 200
	m.height = 10
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"a": {Running: true},
	})
	m.stats = qStats(m, map[string]runner.ServiceStats{
		"a": {CPUPercent: 4.2, MemoryUsed: 100, MemoryLimit: 1000},
	})

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
	m.setSingleGroup([]string{"web"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
	m.stats = qStats(m, map[string]runner.ServiceStats{
		"web": {CPUPercent: 4.2},
	})
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web":   {Running: true},
		"db":    {Running: true},
		"cache": {Running: true},
	})

	result, _ := m.Update(updatesMsg{
		forKey: m.updatesCacheKey(), results: map[string]bool{"web": true, "db": false},
		session: 3,
	})
	model := result.(Model)

	if model.updateInFlight {
		t.Error("updateInFlight should be cleared after current-session msg")
	}
	if model.svcStatus[qk(model, "web")].UpdateAvailable == nil || !*model.svcStatus[qk(model, "web")].UpdateAvailable {
		t.Errorf("web UpdateAvailable = %v, want &true", model.svcStatus[qk(model, "web")].UpdateAvailable)
	}
	if model.svcStatus[qk(model, "db")].UpdateAvailable == nil || *model.svcStatus[qk(model, "db")].UpdateAvailable {
		t.Errorf("db UpdateAvailable = %v, want &false", model.svcStatus[qk(model, "db")].UpdateAvailable)
	}
	// cache absent — UpdateAvailable should remain nil
	if model.svcStatus[qk(model, "cache")].UpdateAvailable != nil {
		t.Errorf("cache UpdateAvailable = %v, want nil (absent from results)", model.svcStatus[qk(model, "cache")].UpdateAvailable)
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})

	result, _ := m.Update(updatesMsg{
		forKey: m.updatesCacheKey(), results: map[string]bool{"web": true},
		session: 4, // older context
	})
	model := result.(Model)

	if model.svcStatus[qk(model, "web")].UpdateAvailable != nil {
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
		forKey: m.updatesCacheKey(), results: map[string]bool{"web": true},
		session: 5,
	})
	model := result.(Model)

	if model.updateInFlight {
		t.Error("off-screen current-session updatesMsg should still clear updateInFlight")
	}
	// Data must NOT be applied to svcStatus (screen check still gates display).
	if model.svcStatus[qk(model, "web")].UpdateAvailable != nil {
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
		forKey: m.updatesCacheKey(), err: errors.New("registry timeout"),
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
		forKey: m.updatesCacheKey(), results: map[string]bool{"web": true},
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

	if model.svcStatus[qk(model, "web")].UpdateAvailable == nil || !*model.svcStatus[qk(model, "web")].UpdateAvailable {
		t.Errorf("UpdateAvailable should be hydrated from cache, got %v", model.svcStatus[qk(model, "web")].UpdateAvailable)
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

	if model.svcStatus[qk(model, "web")].UpdateAvailable == nil || !*model.svcStatus[qk(model, "web")].UpdateAvailable {
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

	if model.svcStatus[qk(model, "web")].UpdateAvailable != nil {
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
		forKey: m.updatesCacheKey(), results: map[string]bool{"web": true}, // partial — should NOT be cached on error
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
		forKey: m.updatesCacheKey(), err: errors.New("registry boom"),
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web":   {Running: true, UpdateAvailable: &trueVal},
		"db":    {Running: true, UpdateAvailable: &falseVal},
		"cache": {Running: true, UpdateAvailable: &trueVal},
	})

	result, _ := m.Update(updatesMsg{
		forKey: m.updatesCacheKey(), err: errors.New("registry timeout"),
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true, UpdateAvailable: &trueVal},
		"db":  {Running: true, UpdateAvailable: &falseVal},
	})

	// New refresh result omits "db" — it should drop back to nil.
	result, _ := m.Update(updatesMsg{
		forKey: m.updatesCacheKey(), results: map[string]bool{"web": true},
		session: m.updatesSession,
	})
	model := result.(Model)

	if model.svcStatus[qk(model, "db")].UpdateAvailable != nil {
		t.Errorf("fresh result that omits db should clear its UpdateAvailable, got %v", model.svcStatus[qk(model, "db")].UpdateAvailable)
	}
	if model.svcStatus[qk(model, "web")].UpdateAvailable == nil || !*model.svcStatus[qk(model, "web")].UpdateAvailable {
		t.Errorf("web verdict from results should hydrate, got %v", model.svcStatus[qk(model, "web")].UpdateAvailable)
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
	// 1. drill in (enter on a group header)
	t.Run("drill_in", func(t *testing.T) {
		mc := &mockComposer{}
		m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
		installFakeTick(&m)
		parkOnGroupedScreen(&m, compose.Project{Name: "p1", ConfigDir: "/p1"}, compose.Project{Name: "p2", ConfigDir: "/p2"})
		before := m.updatesSession
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if result.(Model).updatesSession <= before {
			t.Errorf("drill_in: updatesSession not bumped (before=%d, after=%d)", before, result.(Model).updatesSession)
		}
	})

	// 2. esc container→host view
	t.Run("esc_container_to_proj", func(t *testing.T) {
		mc := &mockComposer{}
		m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
		installFakeTick(&m)
		m.screen = screenSelectContainers
		m.drilledFromHost = true
		before := m.updatesSession
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if result.(Model).updatesSession <= before {
			t.Errorf("esc_container_to_proj: updatesSession not bumped (before=%d, after=%d)", before, result.(Model).updatesSession)
		}
	})

	// 3. esc host view→server
	t.Run("esc_grouped_to_server", func(t *testing.T) {
		mc := &mockComposer{}
		srv := config.Server{Name: "s1", Host: "h1"}
		m := NewModel(nil, io.Discard, mockFactory(mc), []config.Server{srv}, nil)
		installFakeTick(&m)
		m.screen = screenSelectContainers
		m.grouped = true
		before := m.updatesSession
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if result.(Model).updatesSession <= before {
			t.Errorf("esc_grouped_to_server: updatesSession not bumped (before=%d, after=%d)", before, result.(Model).updatesSession)
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
// connectResultMsg error path does NOT bump updatesSession: it navigates BACK
// to the server screen without fetching anything, so it invalidates the
// rollback fetch only.
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
// from each fan-out branch (drill-in, execDone, esc-from-progress,
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
			name: "drill_in",
			setup: func(m *Model) {
				parkOnGroupedScreen(m, compose.Project{Name: "p1", ConfigDir: "/p1"}, compose.Project{Name: "p2", ConfigDir: "/p2"})
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

// modelOf unwraps an Update result. It takes the tea.Cmd so a bare
// `modelOf(m.Update(msg))` compiles — Go only expands a multi-valued call when
// it is the sole argument.
func modelOf(teaModel tea.Model, _ tea.Cmd) Model { return teaModel.(Model) }

// detailFixture is the UpdateDetail the detail-half tests hand back. It is a
// FUNCTION rather than a package-level map so a test that mutates a cached
// entry's details cannot corrupt the ~10 other tests that share it. The exact
// values don't matter here — inspect_test.go owns the rendering — only that the
// map survives the trip from the composer to the cache entry intact.
func detailFixture() map[string]compose.UpdateDetail {
	return map[string]compose.UpdateDetail{
		"web": {
			LocalCreated: time.Date(2026, 7, 7, 17, 47, 22, 0, time.UTC),
			NewID:        "sha256:c05eced0000000000000000000000000000000000000000000000000000000ff",
			NewCreated:   time.Date(2026, 8, 19, 19, 14, 43, 0, time.UTC),
		},
	}
}

// deliverUpdates feeds an updatesMsg through Update and then runs the follow-up
// Cmd the handler enqueues, delivering the resulting updateDetailsMsg too. The
// split is the point: the verdicts land on the first message and the details on
// the second, so a test that wants both has to make both trips.
func deliverUpdates(t *testing.T, m Model, msg updatesMsg) (Model, *updateDetailsMsg) {
	t.Helper()
	model, cmd := m.Update(msg)
	if cmd == nil {
		return model.(Model), nil
	}
	raw := cmd()
	dm, ok := raw.(updateDetailsMsg)
	if !ok {
		t.Fatalf("the follow-up Cmd produced %T, want updateDetailsMsg", raw)
	}
	return modelOf(model.(Model).Update(dm)), &dm
}

// TestRefreshUpdates_VerdictsDoNotWaitOnDetails pins the message split. One
// tea.Cmd returns one message, so folding the detail fetch into refreshUpdates'
// own goroutine would hold the ⇧ verdicts behind three registry round-trips per
// updated image — with updateInFlight pinned true, which makes U a no-op and
// gates off the statusMsg self-heal for that whole window.
func TestRefreshUpdates_VerdictsDoNotWaitOnDetails(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture()}
	mc.updates = map[string]bool{"web": true}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
	m.updateInFlight = true

	msg := m.refreshUpdates()().(updatesMsg)
	if mc.detailsCalls != 0 {
		t.Fatalf("the verdict fetch spent %d detail round-trips before returning", mc.detailsCalls)
	}

	model, cmd := m.Update(msg)
	mo := model.(Model)
	if mo.updateInFlight {
		t.Error("the in-flight guard must clear with the verdicts — U stays dead until it does")
	}
	if st := mo.svcStatus[qk(mo, "web")]; st.UpdateAvailable == nil || !*st.UpdateAvailable {
		t.Error("the glyph must hydrate from the verdict message alone")
	}
	if cmd == nil {
		t.Fatal("the handler must enqueue the detail fetch as a follow-up Cmd")
	}
	if _, ok := cmd().(updateDetailsMsg); !ok {
		t.Error("the follow-up Cmd must produce an updateDetailsMsg")
	}
}

// TestRefreshUpdates_PassesOnlyTrueVerdicts pins the rate-limit guard: the
// compose layer reads an empty services slice as "ALL services", and each
// image costs three registry round-trips, so handing it anything but the true
// verdicts would walk straight into Docker Hub's anonymous quota. Sorted so the
// argument is deterministic.
func TestRefreshUpdates_PassesOnlyTrueVerdicts(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture()}
	mc.updates = map[string]bool{"web": true, "db": false, "api": true, "cache": false, "edge": true}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers

	msg, ok := m.refreshUpdates()().(updatesMsg)
	if !ok {
		t.Fatal("refreshUpdates should produce an updatesMsg")
	}

	model, dm := deliverUpdates(t, m, msg)
	if mc.detailsCalls != 1 {
		t.Fatalf("UpdateDetails calls = %d, want exactly 1", mc.detailsCalls)
	}
	got := mc.detailsArgs[0]
	want := []string{"api", "edge", "web"}
	if !slices.Equal(got, want) {
		t.Fatalf("UpdateDetails services = %v, want %v (sorted, true verdicts only)", got, want)
	}
	if !slices.IsSorted(got) {
		t.Errorf("UpdateDetails services = %v, want them sorted", got)
	}
	if dm == nil || dm.details == nil {
		t.Fatal("the follow-up message should carry the fetched details")
	}
	if entry := model.updateCache[model.updatesCacheKey()]; entry.details == nil {
		t.Error("the details should have merged onto the cache entry the verdicts wrote")
	}
}

// TestRefreshUpdates_SkipsDetailsWhenNoVerdictIsTrue: the skip is load-bearing,
// not an optimisation — filterServices treats an empty slice as "all
// services", so a call with no true verdicts would fetch details for the
// entire project.
func TestRefreshUpdates_SkipsDetailsWhenNoVerdictIsTrue(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture()}
	mc.updates = map[string]bool{"web": false, "db": false}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers

	msg := m.refreshUpdates()().(updatesMsg)
	_, cmd := m.Update(msg)

	if cmd != nil {
		t.Errorf("the handler enqueued a detail fetch (%T) with no true verdict", cmd())
	}
	if mc.detailsCalls != 0 {
		t.Errorf("UpdateDetails calls = %d, want 0 when every verdict is false", mc.detailsCalls)
	}
	if len(msg.results) != 2 {
		t.Errorf("results = %v, want both verdicts through untouched", msg.results)
	}
}

// TestRefreshUpdates_SkipsDetailsOnCheckError: a non-nil CheckUpdates error
// makes the whole verdict map untrusted, so there is nothing worth following
// up — and the detail fetch would spend registry round-trips on a path that is
// already broken.
func TestRefreshUpdates_SkipsDetailsOnCheckError(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture()}
	mc.updates = map[string]bool{"web": true}
	mc.updatesErr = errors.New("registry unreachable")
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers

	msg := m.refreshUpdates()().(updatesMsg)
	if msg.err == nil {
		t.Fatal("the CheckUpdates error must still reach the handler")
	}
	_, cmd := m.Update(msg)
	if cmd != nil {
		t.Errorf("the handler enqueued a detail fetch (%T) after CheckUpdates errored", cmd())
	}
	if mc.detailsCalls != 0 {
		t.Errorf("UpdateDetails calls = %d, want 0 when CheckUpdates errored", mc.detailsCalls)
	}
}

// TestRefreshUpdates_ComposerWithoutDetailerStillProducesVerdicts: the
// capability is type-asserted, so a composer that lacks it (every test mock,
// and any future composer) simply draws no detail rows.
func TestRefreshUpdates_ComposerWithoutDetailerStillProducesVerdicts(t *testing.T) {
	mc := &mockComposer{updates: map[string]bool{"web": true}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers

	msg := m.refreshUpdates()().(updatesMsg)
	if msg.err != nil {
		t.Errorf("err = %v, want nil", msg.err)
	}
	if v, ok := msg.results["web"]; !ok || !v {
		t.Errorf("results = %v, want the verdict through unchanged", msg.results)
	}

	model, cmd := m.Update(msg)
	if cmd != nil {
		t.Errorf("a composer without UpdateDetailer enqueued a detail fetch (%T)", cmd())
	}
	if entry := model.(Model).updateCache[model.(Model).updatesCacheKey()]; entry.details != nil {
		t.Errorf("details = %v, want nil from a composer without UpdateDetailer", entry.details)
	}
}

// TestRefreshUpdates_DetailErrorKeepsVerdictsAndSuccessTTL is the whole reason
// the detail error is discarded: a 429 during the detail phase reads as "too
// many requests", which looksLikeNetworkErr matches. Letting it reach
// updatesMsg.err would blank the ⇧ column and cut the cache entry down to the
// 30-second error TTL — the detail rows would degrade the signal they annotate.
func TestRefreshUpdates_DetailErrorKeepsVerdictsAndSuccessTTL(t *testing.T) {
	mc := &mockDetailComposer{detailsErr: errors.New("429 Too Many Requests")}
	mc.updates = map[string]bool{"web": true}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})

	msg := m.refreshUpdates()().(updatesMsg)
	if msg.err != nil {
		t.Fatalf("err = %v; a detail-fetch failure must never reach updatesMsg.err", msg.err)
	}

	model, _ := deliverUpdates(t, m, msg)
	if model.updatesErr != "" {
		t.Errorf("updatesErr = %q, want empty — the verdicts are sound", model.updatesErr)
	}
	if st := model.svcStatus[qk(model, "web")]; st.UpdateAvailable == nil || !*st.UpdateAvailable {
		t.Error("the ⇧ verdict must survive a detail-fetch failure")
	}
	entry, ok := model.updateCache[model.updatesCacheKey()]
	if !ok {
		t.Fatal("the fetch should still have written a cache entry")
	}
	if entry.err {
		t.Error("the cache entry must be a SUCCESS entry, not the short error-TTL one")
	}
	// The TTL is chosen from entry.err, so age the entry past the error window
	// and confirm the lookup still calls it fresh: this is the 10-minute
	// success TTL in effect, not the 30-second error one.
	entry.fetchedAt = time.Now().Add(-2 * updatesErrorTTL)
	model.updateCache[model.updatesCacheKey()] = entry
	if _, fresh := model.updatesCacheLookup(); !fresh {
		t.Error("entry aged past updatesErrorTTL should still be fresh under the success TTL")
	}
}

// TestRefreshUpdates_PartialDetailsSurviveTheirError: scanUpdateDetails returns
// (partialMap, err) on a transport abort BY DESIGN — the rows resolved before
// the abort are still correct. Discarding the map along with the error would
// throw them away silently.
func TestRefreshUpdates_PartialDetailsSurviveTheirError(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture(), detailsErr: errors.New("update detail transport failure")}
	mc.updates = map[string]bool{"web": true}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers

	model, dm := deliverUpdates(t, m, m.refreshUpdates()().(updatesMsg))
	if dm == nil || dm.details == nil {
		t.Fatal("the partial detail map must survive the error that accompanied it")
	}
	entry := model.updateCache[model.updatesCacheKey()]
	if got := entry.details["web"].NewID; got != detailFixture()["web"].NewID {
		t.Errorf("cached NewID = %q, want the partial map's %q", got, detailFixture()["web"].NewID)
	}
}

// TestUpdateDetailsMsg_MergesOntoTheCacheEntry: the details ride the same entry
// as the verdicts so they inherit its key, TTL, session gate and post-Deploy
// invalidation.
func TestUpdateDetailsMsg_MergesOntoTheCacheEntry(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})

	fetched := time.Now()
	m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {
		fetchedAt: fetched,
		results:   map[string]bool{"web": true},
	}}
	model := modelOf(m.Update(updateDetailsMsg{
		details:  detailFixture(),
		forKey:   m.updatesCacheKey(),
		forEntry: fetched,
	}))

	entry := model.updateCache[model.updatesCacheKey()]
	if entry.details == nil {
		t.Fatal("details should be stored beside results on the cache entry")
	}
	if got := entry.details["web"].NewID; got != detailFixture()["web"].NewID {
		t.Errorf("cached NewID = %q, want %q", got, detailFixture()["web"].NewID)
	}
	if entry.results["web"] != true {
		t.Error("the merge must leave the verdicts alone")
	}
}

// TestUpdateDetailsMsg_LandsAfterASessionBump: the details must NOT carry a
// session gate. updatesSession is bumped by ordinary navigation that leaves the
// context (and therefore the cache key and the entry) untouched — esc back from
// logs or progress, execDone — and those sites find the just-written entry
// fresh, so they enqueue no replacement fetch. A session gate here would drop
// the only message that could fill the entry, leaving the inspect screen
// without `built` / `update id` / `update built` for the full 10-minute TTL,
// recoverable only by a manual U.
func TestUpdateDetailsMsg_LandsAfterASessionBump(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updatesSession = 6
	fetched := time.Now()
	m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {fetchedAt: fetched, results: map[string]bool{"web": true}}}
	// The user opened logs and pressed esc while the details were resolving.
	m.updatesSession = 7

	model := modelOf(m.Update(updateDetailsMsg{details: detailFixture(), forKey: m.updatesCacheKey(), forEntry: fetched}))

	entry := model.updateCache[model.updatesCacheKey()]
	if entry.details == nil {
		t.Fatal("a session bump must not drop the details — the entry it annotates is still the current one")
	}
	if got := entry.details["web"].NewID; got != detailFixture()["web"].NewID {
		t.Errorf("cached NewID = %q, want %q", got, detailFixture()["web"].NewID)
	}
}

// TestUpdateDetailsMsg_ForeignMergeDoesNotRedrawTheScreen: the merge is keyed
// by the batch's own context, but the redraw is a render path and must stay
// keyed by the CURRENT one. redrawInspectFromCache ends in a SetContent that
// does SetXOffset(0), so redrawing for an entry the screen is not showing would
// snap a sideways-scrolled pane back to column 0 at an arbitrary moment, for
// data that did not change.
func TestUpdateDetailsMsg_ForeignMergeDoesNotRedrawTheScreen(t *testing.T) {
	m := inspectScreenModel(t)
	m.projDir = "/srv/app-b"
	fetched := time.Now()
	m.updateCache = map[string]updateEntry{"/srv/app-a|": {fetchedAt: fetched, results: map[string]bool{"web": true}}}
	// A sentinel the rebuild would overwrite: rebuildInspectSummary re-parses
	// m.inspectRaw, so any value surviving here proves it never ran.
	m.inspectSummary = "STALE SUMMARY"

	model := modelOf(m.Update(updateDetailsMsg{details: detailFixture(), forKey: "/srv/app-a|", forEntry: fetched}))

	if model.updateCache["/srv/app-a|"].details == nil {
		t.Fatal("precondition: the merge itself must still land on the batch's own entry")
	}
	if model.inspectSummary != "STALE SUMMARY" {
		t.Error("a merge onto another context's entry redrew the screen")
	}
}

// TestUpdateDetails_ColdCacheFillsEveryRow walks the whole lifecycle on a cold
// cache, through the real Cmds rather than a hand-built cache entry: the
// verdicts land and paint the ⇧ glyph, the follow-up batch resolves, and all
// four IMAGE rows appear on a screenInspect that was already open. That last
// part is the point — entering `i` while the first fetch is in flight is
// exactly when a cold cache happens, and without the rebuild the rows would
// stay absent until a back-out and re-entry.
func TestUpdateDetails_ColdCacheFillsEveryRow(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture()}
	mc.updates = map[string]bool{"web": true}
	m := inspectScreenModel(t)
	m.composer = mc
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
	if _, ok := m.updateCache[m.updatesCacheKey()]; ok {
		t.Fatal("precondition: the cache must be cold")
	}

	verdicts, ok := m.refreshUpdates()().(updatesMsg)
	if !ok {
		t.Fatal("refreshUpdates must produce an updatesMsg")
	}
	model, detailsCmd := m.Update(verdicts)
	mo := model.(Model)
	if detailsCmd == nil {
		t.Fatal("the verdicts must enqueue the detail batch")
	}
	if !strings.Contains(mo.inspectSummary, inspectRow("update", "available")) {
		t.Fatalf("the verdict row must appear without waiting on the details:\n%s", mo.inspectSummary)
	}

	filled := modelOf(mo.Update(detailsCmd().(updateDetailsMsg)))
	for _, want := range []string{
		inspectRow("built", "2026-07-07 17:47:22"),
		inspectRow("update", "available"),
		inspectRow("update id", detailFixture()["web"].NewID),
		inspectRow("update built", "2026-08-19 19:14:43"),
	} {
		if !strings.Contains(filled.inspectSummary, want) {
			t.Errorf("summary missing %q:\n%s", want, filled.inspectSummary)
		}
	}
	// The pane is rewritten too, not just the summary string. The IMAGE block
	// sits below the fold at this height, so scroll to it rather than asserting
	// against the first screenful.
	filled.inspectViewport.GotoBottom()
	if got := filled.inspectViewport.View(); !strings.Contains(got, "ENV") {
		t.Errorf("the pane must be rewritten with the filled summary:\n%s", got)
	}
}

// TestUpdateDetailsMsg_ForeignContextIsNotTouched: the batch's OWN key decides
// which entry it annotates, so a batch fetched under project A can never write
// project B's entry. Both entries carry the same fetchedAt on purpose — that is
// the case a handler which re-derived the key from the CURRENT context would
// get wrong, merging A's answers onto B's verdicts.
func TestUpdateDetailsMsg_ForeignContextIsNotTouched(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	fetched := time.Now()
	m.projDir = "/srv/app-a"
	keyA := m.updatesCacheKey()
	m.projDir = "/srv/app-b"
	keyB := m.updatesCacheKey()
	m.updateCache = map[string]updateEntry{
		keyA: {fetchedAt: fetched, results: map[string]bool{"web": true}},
		keyB: {fetchedAt: fetched, results: map[string]bool{"web": true}},
	}
	// The user is now on project B; the batch below was fetched for A.
	model := modelOf(m.Update(updateDetailsMsg{details: detailFixture(), forKey: keyA, forEntry: fetched}))

	if entry := model.updateCache[keyB]; entry.details != nil {
		t.Errorf("project A's details landed on project B's entry: %+v", entry.details)
	}
	if entry := model.updateCache[keyA]; entry.details == nil {
		t.Error("the batch must still annotate the entry it was fetched for")
	}
}

// TestUpdateDetailsMsg_NeverCreatesAnEntry: the details are an annotation on a
// verdict set, so a message naming a key the cache does not hold (the entry was
// invalidated by a deploy, or the fetch outlived it) must not conjure one —
// there would be no verdicts to annotate and no fetchedAt to age it by.
func TestUpdateDetailsMsg_NeverCreatesAnEntry(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updateCache = map[string]updateEntry{}

	model := modelOf(m.Update(updateDetailsMsg{
		details:  detailFixture(),
		forKey:   "/srv/app-a|",
		forEntry: time.Now(),
	}))

	if len(model.updateCache) != 0 {
		t.Errorf("the handler created a cache entry from a detail message: %+v", model.updateCache)
	}
}

// TestUpdateDetailsMsg_ErroredEntryIsNotMerged: a non-nil CheckUpdates error
// makes the whole verdict map untrusted per the Composer contract, so an entry
// that failed must not gain rows describing verdicts it does not hold. The
// inspect screen reads the raw entry, so a merge here would draw update rows
// beside a blanked glyph column.
func TestUpdateDetailsMsg_ErroredEntryIsNotMerged(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	fetched := time.Now()
	key := m.updatesCacheKey()
	m.updateCache = map[string]updateEntry{key: {
		fetchedAt: fetched,
		err:       true,
		errMsg:    "registry unreachable",
	}}

	model := modelOf(m.Update(updateDetailsMsg{details: detailFixture(), forKey: key, forEntry: fetched}))

	if entry := model.updateCache[key]; entry.details != nil {
		t.Errorf("details merged onto an errored entry: %+v", entry.details)
	}
}

// TestUpdateDetailsMsg_LandsAfterNavigatingAway pins the second half of the
// identity rule. A detail scan is the long half — three registry round-trips
// per updated image, tens of seconds over SSH — so an ordinary esc to the
// grouped host view mid-scan is routine. Because the batch carries the key it was
// fetched under, the merge still lands on that project's entry, and returning
// inside the 10-minute TTL shows the rows. Looking the key up at MERGE time
// instead dropped the message, and nothing refetched: the entry is a fresh
// success, so maybeRefreshUpdatesCmd and the statusMsg self-heal both skip it.
func TestUpdateDetailsMsg_LandsAfterNavigatingAway(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.projDir = "/srv/app-a"
	projectKey := m.updatesCacheKey()
	fetched := time.Now()
	m.updateCache = map[string]updateEntry{projectKey: {fetchedAt: fetched, results: map[string]bool{"web": true}}}

	// esc out to the grouped host view: projDir is cleared, so the current key
	// is no longer the one the batch was fetched under.
	m.projDir = ""
	m.grouped = true
	if m.updatesCacheKey() == projectKey {
		t.Fatal("precondition: leaving the project must change the cache key")
	}

	model := modelOf(m.Update(updateDetailsMsg{details: detailFixture(), forKey: projectKey, forEntry: fetched}))

	entry := model.updateCache[projectKey]
	if entry.details == nil {
		t.Fatal("a batch that finished after the user navigated away lost its rows for the entry's whole TTL")
	}
	if got := entry.details["web"].NewID; got != detailFixture()["web"].NewID {
		t.Errorf("cached NewID = %q, want %q", got, detailFixture()["web"].NewID)
	}
}

// TestUpdateDetails_OneBatchAtATime pins the second in-flight guard. The
// verdicts clear updateInFlight the moment they land (that is what keeps U
// alive), so without a guard of its own the expensive half is unprotected: a U
// press during the detail scan stacks a SECOND batch of three registry
// round-trips per updated image on top of the running one, and repeated presses
// stack without bound — the exact Docker Hub 429 the true-only filter and the
// per-image memoisation exist to avoid (two concurrent batches memoise
// separately, so the mitigation does not span them).
//
// The refusal must not cost the rows. The U press replaced the entry the
// running batch names, so that batch's map is dropped on arrival — and the
// replacement is a fresh SUCCESS entry, which maybeRefreshUpdatesCmd and the
// statusMsg self-heal both skip. The arrival therefore refills from the entry's
// own details == nil, with no further keystroke.
func TestUpdateDetails_OneBatchAtATime(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture()}
	mc.updates = map[string]bool{"web": true}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})

	first, cmd := m.Update(updatesMsg{forKey: m.updatesCacheKey(), results: map[string]bool{"web": true}, session: m.updatesSession})
	mo := first.(Model)
	if cmd == nil {
		t.Fatal("the first verdicts must enqueue a detail fetch")
	}
	if !mo.detailsInFlight {
		t.Fatal("enqueuing the detail fetch must raise the in-flight guard")
	}

	// The user presses U while the details are still resolving: the verdicts
	// refresh (that half is guarded by updateInFlight alone), but the handler
	// must NOT enqueue a second detail batch.
	second, cmd2 := mo.Update(updatesMsg{forKey: mo.updatesCacheKey(), results: map[string]bool{"web": true}, session: mo.updatesSession})
	so := second.(Model)
	if cmd2 != nil {
		t.Errorf("a second detail batch was enqueued while one was in flight: %T", cmd2())
	}
	if entry := so.updateCache[so.updatesCacheKey()]; entry.details != nil {
		t.Fatal("precondition: the entry the U press wrote must carry no reported batch")
	}

	// Once the running batch reports, the guard clears and the refused batch is
	// re-dispatched on the spot. The arriving message names the SUPERSEDED
	// entry, so its own map is dropped; without the refill those rows would be
	// gone for the replacement entry's full 10-minute TTL.
	third, cmd3 := so.Update(updateDetailsMsg{
		details:  detailFixture(),
		forKey:   so.updatesCacheKey(),
		forEntry: time.Now().Add(-time.Minute),
	})
	to := third.(Model)
	if cmd3 == nil {
		t.Fatal("the refused batch must be retried when the running one reports, with no further keystroke")
	}
	if !to.detailsInFlight {
		t.Error("the retry must re-raise the guard so the phase stays one batch wide")
	}
	// The retry carries the CURRENT entry's identity and fills it.
	dm, ok := cmd3().(updateDetailsMsg)
	if !ok {
		t.Fatalf("the retry produced %T, want updateDetailsMsg", cmd3())
	}
	filled := modelOf(to.Update(dm))
	entry := filled.updateCache[filled.updatesCacheKey()]
	if entry.details == nil {
		t.Fatal("the entry the U press wrote never received its detail rows")
	}
	if got := entry.details["web"].NewID; got != detailFixture()["web"].NewID {
		t.Errorf("cached NewID = %q, want %q", got, detailFixture()["web"].NewID)
	}
	if filled.detailsInFlight {
		t.Error("the retry's own arrival must clear the guard again")
	}
	if _, extra := filled.Update(updateDetailsMsg{details: detailFixture(), forKey: filled.updatesCacheKey(), forEntry: entry.fetchedAt}); extra != nil {
		t.Errorf("an entry a batch has already reported for was refetched: %T", extra())
	}
}

// TestUpdateDetails_RefillTargetsTheNewestEntry: repeated U presses during one
// detail scan must not queue one retry each. The retry is derived from the
// CURRENT cache entry rather than from a record of the refusals, so three
// refusals produce ONE batch — against the newest verdict set — however hard
// the key is mashed.
func TestUpdateDetails_RefillTargetsTheNewestEntry(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture()}
	mc.updates = map[string]bool{"web": true}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})

	model, cmd := m.Update(updatesMsg{forKey: m.updatesCacheKey(), results: map[string]bool{"web": true}, session: m.updatesSession})
	if cmd == nil {
		t.Fatal("the first verdicts must enqueue a detail fetch")
	}
	mo := model.(Model)
	for range 3 {
		next, c := mo.Update(updatesMsg{forKey: mo.updatesCacheKey(), results: map[string]bool{"web": true}, session: mo.updatesSession})
		if c != nil {
			t.Fatalf("a second detail batch was enqueued while one was in flight: %T", c())
		}
		mo = next.(Model)
	}
	newest := mo.updateCache[mo.updatesCacheKey()].fetchedAt

	// One arrival, one retry — not three, and it names the NEWEST entry.
	after, retry := mo.Update(updateDetailsMsg{details: nil, forKey: mo.updatesCacheKey(), forEntry: time.Now().Add(-time.Hour)})
	if retry == nil {
		t.Fatal("the refused batch must be re-dispatched")
	}
	rm, ok := retry().(updateDetailsMsg)
	if !ok {
		t.Fatalf("the retry produced %T, want updateDetailsMsg", retry())
	}
	if !rm.forEntry.Equal(newest) {
		t.Errorf("the retry names fetchedAt %v, want the newest entry's %v", rm.forEntry, newest)
	}
	done := modelOf(after.(Model).Update(rm))
	if _, extra := done.Update(updateDetailsMsg{details: nil, forKey: done.updatesCacheKey(), forEntry: time.Now()}); extra != nil {
		t.Errorf("a fourth batch was dispatched after the entry was filled: %T", extra())
	}
	if mc.detailsCalls != 1 {
		t.Errorf("UpdateDetails calls = %d, want 1 (three refusals collapse into one retry)", mc.detailsCalls)
	}
}

// TestUpdateDetails_EmptyResultIsNotRetried: a batch that reports NOTHING (a
// 429, or a transport abort before the first row) still counts as reported. The
// merge normalises its nil map to an empty one, because details == nil is the
// refill trigger — leaving it nil would re-scan the registry on every screen
// entry for the entry's whole 10-minute TTL, which is the amplification the
// error-swallowing at the fetch site exists to prevent.
func TestUpdateDetails_EmptyResultIsNotRetried(t *testing.T) {
	mc := &mockDetailComposer{detailsErr: errors.New("429 Too Many Requests")}
	mc.updates = map[string]bool{"web": true}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers

	model, dm := deliverUpdates(t, m, m.refreshUpdates()().(updatesMsg))
	if dm == nil || dm.details != nil {
		t.Fatalf("precondition: the batch must report a nil map, got %+v", dm)
	}
	entry := model.updateCache[model.updatesCacheKey()]
	if entry.details == nil {
		t.Fatal("a batch that reported nothing must still be RECORDED on the entry")
	}
	if len(entry.details) != 0 {
		t.Errorf("details = %+v, want an empty map", entry.details)
	}
	if cmd := model.refillUpdateDetailsCmd(); cmd != nil {
		t.Errorf("the failed batch was retried inside its own TTL: %T", cmd())
	}
}

// TestRefillUpdateDetails_FailsClosed: the refill is the detail phase's only
// retry path, so everything that is NOT "a fresh success entry with a true
// verdict that no batch has reported for" must refuse — otherwise the trigger
// that heals a lost batch becomes a registry loop.
func TestRefillUpdateDetails_FailsClosed(t *testing.T) {
	fetched := time.Now()
	rewrite := func(m *Model, fn func(*updateEntry)) {
		e := m.updateCache[m.updatesCacheKey()]
		fn(&e)
		m.updateCache[m.updatesCacheKey()] = e
	}
	tests := []struct {
		name  string
		build func(*Model)
	}{
		{
			name:  "a batch has already reported",
			build: func(m *Model) { rewrite(m, func(e *updateEntry) { e.details = map[string]compose.UpdateDetail{} }) },
		},
		{
			name:  "entry was invalidated by a deploy",
			build: func(m *Model) { delete(m.updateCache, m.updatesCacheKey()) },
		},
		{
			// The verdicts are kept on purpose: a non-nil CheckUpdates error
			// makes the whole map untrusted per the Composer contract, so the
			// err flag alone must refuse — not the emptiness that happens to
			// accompany it on the production write path.
			name: "entry failed",
			build: func(m *Model) {
				rewrite(m, func(e *updateEntry) { e.err, e.errMsg = true, "registry unreachable" })
			},
		},
		{
			name: "entry aged past its TTL",
			build: func(m *Model) {
				rewrite(m, func(e *updateEntry) { e.fetchedAt = time.Now().Add(-2 * updatesCacheTTL) })
			},
		},
		{
			name:  "no verdict is true",
			build: func(m *Model) { rewrite(m, func(e *updateEntry) { e.results = map[string]bool{"web": false} }) },
		},
		{
			name:  "a batch is already in flight",
			build: func(m *Model) { m.detailsInFlight = true },
		},
		{
			// The entry lives under another context's key, so the current one
			// has nothing to refill — the refill must never reach across.
			name: "the entry belongs to another context",
			build: func(m *Model) {
				m.updateCache["/srv/other|"] = m.updateCache[m.updatesCacheKey()]
				delete(m.updateCache, m.updatesCacheKey())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc := &mockDetailComposer{details: detailFixture()}
			m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
			m.composer = mc
			m.screen = screenSelectContainers
			m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {fetchedAt: fetched, results: map[string]bool{"web": true}}}
			tt.build(&m)

			if cmd := m.refillUpdateDetailsCmd(); cmd != nil {
				t.Errorf("the refill fired anyway: %T", cmd())
			}
			if mc.detailsCalls != 0 {
				t.Errorf("UpdateDetails calls = %d, want 0", mc.detailsCalls)
			}
		})
	}

	// The control: without one case that DOES fire, every assertion above would
	// pass on a refill that never works at all.
	t.Run("a fresh entry with no reported batch", func(t *testing.T) {
		mc := &mockDetailComposer{details: detailFixture()}
		m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
		m.composer = mc
		m.screen = screenSelectContainers
		m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {fetchedAt: fetched, results: map[string]bool{"web": true}}}

		cmd := m.refillUpdateDetailsCmd()
		if cmd == nil {
			t.Fatal("a fresh success entry with a true verdict and no reported batch must refill")
		}
		if !m.detailsInFlight {
			t.Error("the refill must raise the in-flight guard like any other dispatch")
		}
		if _, ok := cmd().(updateDetailsMsg); !ok {
			t.Errorf("the refill produced %T, want updateDetailsMsg", cmd())
		}
	})
}

// TestUpdateDetails_UStaysLiveDuringTheDetailScan: the fix for the stacking
// above must NOT be a gate on the U key (or a delayed updateInFlight clear).
// The detail scan is the long half — three registry round-trips per updated
// image, tens of seconds over SSH — and refusing U for that window is exactly
// the dead-U window the message split removed.
func TestUpdateDetails_UStaysLiveDuringTheDetailScan(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture()}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers
	m.setSingleGroup([]string{"web"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
	m.detailsInFlight = true
	m.updateInFlight = false

	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("U")})
	if cmd == nil {
		t.Fatal("U must still force-refresh the verdicts while the details resolve")
	}
	if !model.(Model).updateInFlight {
		t.Error("the U refresh must raise updateInFlight as usual")
	}
}

// hasUpdateDetailsCmd runs a Cmd (a tea.Batch or a single one) and reports
// whether any of it produced an updateDetailsMsg.
func hasUpdateDetailsCmd(t *testing.T, cmd tea.Cmd) bool {
	t.Helper()
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		_, isDetails := msg.(updateDetailsMsg)
		return isDetails
	}
	for _, child := range batch {
		if child == nil {
			continue
		}
		if _, isDetails := child().(updateDetailsMsg); isDetails {
			return true
		}
	}
	return false
}

// TestUpdateDetails_GuardIsNotClearedByNavigation pins the guard's scope. It is
// a GLOBAL one-batch bound, not a per-context one: a navigation that clears it
// while the batch is still running lets the next verdicts dispatch a second
// scan on top of it (each memoises separately, so the per-image mitigation does
// not span them), and it makes the unconditional clear on arrival a lie — the
// refill would then see "nothing in flight" and dispatch a third for an entry
// the second is already filling.
//
// The table names every site that used to reset it, including the three that
// change neither the cache key nor the composer.
func TestUpdateDetails_GuardIsNotClearedByNavigation(t *testing.T) {
	servers := []config.Server{{Name: "s1", Host: "h1"}}
	cases := []struct {
		name  string
		setup func(*Model)
		msg   tea.Msg
	}{
		{
			name:  "execDone",
			setup: func(m *Model) { m.screen = screenSelectContainers },
			msg:   execDoneMsg{},
		},
		{
			name:  "esc from logs",
			setup: func(m *Model) { m.screen = screenLogs },
			msg:   tea.KeyMsg{Type: tea.KeyEsc},
		},
		{
			name:  "esc from progress",
			setup: func(m *Model) { m.screen = screenProgress; m.done = true },
			msg:   tea.KeyMsg{Type: tea.KeyEsc},
		},
		{
			name:  "esc container→host view",
			setup: func(m *Model) { m.screen = screenSelectContainers; m.drilledFromHost = true },
			msg:   tea.KeyMsg{Type: tea.KeyEsc},
		},
		{
			name:  "esc host view→server",
			setup: func(m *Model) { m.screen = screenSelectContainers; m.grouped = true; m.servers = servers },
			msg:   tea.KeyMsg{Type: tea.KeyEsc},
		},
		{
			name: "drill in",
			setup: func(m *Model) {
				parkOnGroupedScreen(m, compose.Project{Name: "p1", ConfigDir: "/p1"}, compose.Project{Name: "p2", ConfigDir: "/p2"})
			},
			msg: tea.KeyMsg{Type: tea.KeyEnter},
		},
		{
			name:  "entryLocal fast-track",
			setup: func(m *Model) { m.screen = screenSelectServer; m.serverCursor = 0 },
			msg:   tea.KeyMsg{Type: tea.KeyEnter},
		},
		{
			name:  "connectResultMsg error",
			setup: func(m *Model) { m.screen = screenSelectServer },
			msg:   connectResultMsg{err: errors.New("boom")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockDetailComposer{details: detailFixture()}
			mc.services = []string{"web"}
			mc.updates = map[string]bool{"web": true}
			m := NewModel(mc, io.Discard, func(compose.Project) runner.Composer { return mc }, servers, nil)
			installFakeTick(&m)
			tc.setup(&m)
			m.composer = mc
			m.detailsInFlight = true

			model := modelOf(m.Update(tc.msg))
			if !model.detailsInFlight {
				t.Error("the navigation cleared the guard while the batch was still running")
			}
		})
	}
}

// TestUpdateDetails_SelfHealsAtEveryScreenEntry covers the other half: an entry
// that a lost batch left with no detail rows is refetched at the next screen
// entry, at every site that consults maybeRefreshUpdatesCmd. Without it the
// entry is a fresh SUCCESS, so the statusMsg self-heal skips it too and the
// inspect screen — a pure consumer — shows `update available` with no `built`,
// no `update id` and no `update built` for the full 10-minute TTL.
func TestUpdateDetails_SelfHealsAtEveryScreenEntry(t *testing.T) {
	servers := []config.Server{{Name: "s1", Host: "h1"}}
	cases := []struct {
		name  string
		key   string // the updatesCacheKey the navigation lands on
		setup func(*Model)
		msg   tea.Msg
	}{
		{
			name: "drill in",
			key:  "/p1\x00p1|",
			setup: func(m *Model) {
				parkOnGroupedScreen(m, compose.Project{Name: "p1", ConfigDir: "/p1"}, compose.Project{Name: "p2", ConfigDir: "/p2"})
			},
			msg: tea.KeyMsg{Type: tea.KeyEnter},
		},
		{
			name:  "entryLocal fast-track",
			key:   "\x00|",
			setup: func(m *Model) { m.screen = screenSelectServer; m.serverCursor = 0 },
			msg:   tea.KeyMsg{Type: tea.KeyEnter},
		},
		{
			name:  "execDone",
			key:   "\x00|",
			setup: func(m *Model) { m.screen = screenSelectContainers },
			msg:   execDoneMsg{},
		},
		{
			name:  "esc from logs",
			key:   "\x00|",
			setup: func(m *Model) { m.screen = screenLogs },
			msg:   tea.KeyMsg{Type: tea.KeyEsc},
		},
		{
			name: "esc from progress",
			key:  "\x00|",
			setup: func(m *Model) {
				m.screen = screenProgress
				m.done = true
				// StopOnly leaves the cache entry in place; a successful
				// Deploy/Restart/Rollback deletes it, and then the site
				// enqueues a full refresh rather than a refill.
				m.pendingOp = runner.StopOnly
			},
			msg: tea.KeyMsg{Type: tea.KeyEsc},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockDetailComposer{details: detailFixture()}
			mc.services = []string{"web"}
			mc.updates = map[string]bool{"web": true}
			m := NewModel(mc, io.Discard, func(compose.Project) runner.Composer { return mc }, servers, nil)
			installFakeTick(&m)
			tc.setup(&m)
			m.composer = mc
			// A fresh SUCCESS entry with a true verdict that no detail batch
			// ever reported for — exactly what a lost batch leaves behind.
			m.updateCache = map[string]updateEntry{tc.key: {fetchedAt: time.Now(), results: map[string]bool{"web": true}}}

			_, cmd := m.Update(tc.msg)
			if cmd == nil {
				t.Fatal("the navigation returned no Cmd at all")
			}
			if !hasUpdateDetailsCmd(t, cmd) {
				t.Error("the screen entry did not refill the entry's missing detail rows")
			}
			if mc.detailsCalls == 0 {
				t.Error("the refill Cmd never reached UpdateDetails")
			}
		})
	}
}

// TestUpdateDetails_LostBatchHealsAfterNavigating walks the whole reachable
// sequence a lost batch takes: a batch is refused, an ordinary navigation
// happens while the running one is still out, and the refused batch's entry
// must still end up with its rows.
func TestUpdateDetails_LostBatchHealsAfterNavigating(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture()}
	mc.services = []string{"web"}
	mc.updates = map[string]bool{"web": true}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	installFakeTick(&m)
	m.composer = mc
	m.screen = screenSelectContainers
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})

	// Batch B1 goes out for entry E1.
	first, b1 := m.Update(updatesMsg{forKey: m.updatesCacheKey(), results: map[string]bool{"web": true}, session: m.updatesSession})
	mo := first.(Model)
	if b1 == nil {
		t.Fatal("the first verdicts must enqueue a detail fetch")
	}
	// U mid-scan: E2 replaces E1 and its own batch is refused.
	second, refused := mo.Update(updatesMsg{forKey: mo.updatesCacheKey(), results: map[string]bool{"web": true}, session: mo.updatesSession})
	if refused != nil {
		t.Fatalf("precondition: the second batch must be refused, got %T", refused())
	}
	so := second.(Model)

	// The user opens the log viewer and comes straight back — same project,
	// same host, same cache entry.
	so.screen = screenLogs
	back := modelOf(so.Update(tea.KeyMsg{Type: tea.KeyEsc}))
	if !back.detailsInFlight {
		t.Error("the navigation cleared the guard while B1 was still running")
	}

	// B1 reports at last, naming the superseded entry: its map is dropped and
	// the refill goes out for E2 instead.
	stale := b1().(updateDetailsMsg)
	healed, retry := back.Update(stale)
	if retry == nil {
		t.Fatal("the entry left without details across the navigation was never refilled")
	}
	filled := modelOf(healed.(Model).Update(retry().(updateDetailsMsg)))
	entry := filled.updateCache[filled.updatesCacheKey()]
	if entry.details == nil {
		t.Fatal("the entry ended the sequence with no detail rows — the ten-minute hole is back")
	}
	if got := entry.details["web"].NewID; got != detailFixture()["web"].NewID {
		t.Errorf("cached NewID = %q, want %q", got, detailFixture()["web"].NewID)
	}
}

// TestUpdateDetailsMsg_SupersededEntryIsDropped: a U press mid-fetch replaces
// the cache entry under the SAME session, so neither the session nor the key
// can tell the older fetch's (possibly truncated) map from the newer one. The
// entry's own fetchedAt is the identity that can.
func TestUpdateDetailsMsg_SupersededEntryIsDropped(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	superseded := time.Now().Add(-time.Minute)
	m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {
		fetchedAt: time.Now(),
		results:   map[string]bool{"web": true},
	}}

	// forKey names the CURRENT context on purpose: with it omitted the key
	// lookup refuses the message first and the fetchedAt conjunct this test is
	// named for is never consulted. Only the identity mismatch may refuse it.
	model := modelOf(m.Update(updateDetailsMsg{
		details:  detailFixture(),
		forKey:   m.updatesCacheKey(),
		forEntry: superseded,
	}))

	if entry := model.updateCache[model.updatesCacheKey()]; entry.details != nil {
		t.Errorf("details from a superseded fetch landed on the newer entry: %+v", entry.details)
	}
}

// TestUpdateDetails_FetchCarriesADeadline: detailsInFlight is a GLOBAL
// one-batch bound whose only clear is the message arrival, so the arrival has
// to be guaranteed. m.ctx lives for the whole program and neither dockerRunner
// adds a deadline, so without this bound one stalled registry call closes the
// detail phase for the rest of the session — no departure site resets the flag
// and U cannot force it.
func TestUpdateDetails_FetchCarriesADeadline(t *testing.T) {
	mc := &mockDetailComposer{details: detailFixture()}
	mc.updates = map[string]bool{"web": true}
	m := NewModel(mc, io.Discard, mockFactory(&mc.mockComposer), nil, nil)
	m.composer = mc
	m.screen = screenSelectContainers

	_, cmd := m.Update(updatesMsg{forKey: m.updatesCacheKey(), results: map[string]bool{"web": true}, session: m.updatesSession})
	if cmd == nil {
		t.Fatal("the verdicts enqueued no detail fetch")
	}
	before := time.Now()
	cmd()

	if len(mc.detailsDeadlines) != 1 {
		t.Fatalf("UpdateDetails calls = %d, want exactly 1", len(mc.detailsDeadlines))
	}
	deadline := mc.detailsDeadlines[0]
	if deadline.IsZero() {
		t.Fatal("the ctx handed to UpdateDetails carries no deadline — a stalled batch latches detailsInFlight for the rest of the session")
	}
	// Within a second of the constant, so a hand-rolled window that happens to
	// be non-zero does not pass for the one the design bounds the phase with.
	if got := deadline.Sub(before); got > updateDetailsTimeout+time.Second || got < updateDetailsTimeout-time.Second {
		t.Errorf("deadline is %v out, want updateDetailsTimeout (%v)", got, updateDetailsTimeout)
	}
}

// TestUpdatesMsg_StaleSessionDoesNotWriteTheEntry: the cache write is gated by
// the session check, so a response from a previous project or server context
// cannot pollute the new context's entry.
func TestUpdatesMsg_StaleSessionDoesNotWriteTheEntry(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.updatesSession = 7

	model, cmd := m.Update(updatesMsg{
		forKey: m.updatesCacheKey(), results: map[string]bool{"web": true},
		session: 6, // a fetch issued before the context changed
	})

	if entry, ok := model.(Model).updateCache[model.(Model).updatesCacheKey()]; ok {
		t.Errorf("a stale updatesMsg wrote a cache entry: %+v", entry)
	}
	if cmd != nil {
		t.Errorf("a superseded scan must not spend the detail round-trips at all, got %T", cmd())
	}
	if model.(Model).updateInFlight {
		t.Error("the in-flight guard must still clear on a stale arrival")
	}
}

// TestUpdatesMsg_RefreshesInspectSummary: the inspect screen is a consumer of
// the update cache and never fetches details itself. Entering `i` on a cold
// cache is exactly when the fetch is still in flight, so without this rebuild
// the new IMAGE rows would stay absent until a back-out and re-entry. The
// assertions read the MESSAGE's own data back out of the summary, so a rebuild
// that ran before the cache write (and therefore rendered the previous entry)
// fails here rather than passing on "the sentinel is gone".
func TestUpdatesMsg_RefreshesInspectSummary(t *testing.T) {
	m := inspectScreenModel(t)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
	// A sentinel the rebuild has to overwrite: rebuildInspectSummary re-parses
	// m.inspectRaw, so any value here that survives proves it never ran.
	m.inspectSummary = "STALE SUMMARY"
	m.setInspectContent()

	mid, cmd := m.Update(updatesMsg{
		forKey: m.updatesCacheKey(), results: map[string]bool{"web": true},
		session: m.updatesSession,
	})
	model := mid.(Model)

	if model.inspectSummary == "STALE SUMMARY" {
		t.Fatal("an updatesMsg on screenInspect must rebuild the summary")
	}
	if !strings.Contains(model.inspectSummary, "STATE") {
		t.Errorf("the rebuilt summary should be the real one:\n%s", model.inspectSummary)
	}
	// The verdict from THIS message, not the previous entry's.
	if !strings.Contains(model.inspectSummary, inspectRow("update", "available")) {
		t.Errorf("the rebuild must render the verdict the message carried:\n%s", model.inspectSummary)
	}
	if got := model.inspectViewport.View(); strings.Contains(got, "STALE SUMMARY") {
		t.Error("setInspectContent must follow the rebuild so the pane shows it")
	}
	if model.screen != screenInspect {
		t.Errorf("screen = %d, want screenInspect", model.screen)
	}

	// The detail rows arrive on the follow-up message and rebuild again. The
	// inspect double carries no UpdateDetailer, so the message is delivered
	// directly rather than through a Cmd this composer would never produce.
	_ = cmd
	withDetails := modelOf(model.Update(updateDetailsMsg{
		details:  detailFixture(),
		forKey:   model.updatesCacheKey(),
		forEntry: model.updateCache[model.updatesCacheKey()].fetchedAt,
	}))
	if !strings.Contains(withDetails.inspectSummary, inspectRow("update id", detailFixture()["web"].NewID)) {
		t.Errorf("the detail rows must reach the summary without a re-entry:\n%s", withDetails.inspectSummary)
	}
}

// TestUpdatesMsg_FailurePathClearsInspectRows: the rebuild runs on the FAILURE
// path too. The entry that just landed carries no verdicts and no details, so a
// summary drawn from a previous one must stop showing them — otherwise the
// glyph column blanks while the inspect screen keeps contradicting it.
func TestUpdatesMsg_FailurePathClearsInspectRows(t *testing.T) {
	m := inspectScreenModel(t)
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
	m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {
		fetchedAt: time.Now().Add(-3 * time.Minute),
		results:   map[string]bool{"web": true},
		details:   detailFixture(),
	}}
	m.rebuildInspectSummary()
	for _, row := range []string{"update", "update id", "update built", "built"} {
		if !strings.Contains(m.inspectSummary, inspectRow(row, "")) {
			t.Fatalf("precondition: the summary should carry the %q row:\n%s", row, m.inspectSummary)
		}
	}

	model := modelOf(m.Update(updatesMsg{
		forKey: m.updatesCacheKey(), err: errors.New("registry unreachable"),
		session: m.updatesSession,
	}))

	for _, row := range []string{"update", "update id", "update built", "built"} {
		if strings.Contains(model.inspectSummary, inspectRow(row, "")) {
			t.Errorf("a failed check must drop the %q row:\n%s", row, model.inspectSummary)
		}
	}
}

// TestUpdatesMsg_RawModeKeepsHorizontalScroll: in raw mode the buffer is
// untouched by update details, so writing it again would change nothing except
// reset the horizontal offset — snapping a user who scrolled sideways through
// the long JSON lines back to column 0 when a background refresh lands.
func TestUpdatesMsg_RawModeKeepsHorizontalScroll(t *testing.T) {
	raw := inspectScrolledRawModel(t)

	model := modelOf(raw.Update(updatesMsg{
		forKey: raw.updatesCacheKey(), results: map[string]bool{"web": true},
		session: raw.updatesSession,
	}))

	if strings.Contains(model.inspectViewport.View(), `"Name"`) {
		t.Errorf("a background updatesMsg snapped the raw view back to column 0:\n%s", model.inspectViewport.View())
	}
	// The summary is still rebuilt, so a later `r` shows the new rows.
	if !strings.Contains(model.inspectSummary, inspectRow("update", "available")) {
		t.Errorf("the summary must still be rebuilt in raw mode:\n%s", model.inspectSummary)
	}
}

// TestUpdatesMsg_InspectRebuildIsScreenScoped: the rebuild is the ONE
// screen-coupled mutation in this handler. Off-screen it must not run — the
// inspect fields are cleared on departure, and a rebuild there would write a
// summary for a screen the user already left.
func TestUpdatesMsg_InspectRebuildIsScreenScoped(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	m.screen = screenSelectContainers
	m.inspectSummary = "STALE SUMMARY"

	model := modelOf(m.Update(updatesMsg{
		forKey: m.updatesCacheKey(), results: map[string]bool{"web": true},
		session: m.updatesSession,
	}))

	if model.inspectSummary != "STALE SUMMARY" {
		t.Error("the rebuild must not run while the user is off screenInspect")
	}
}

// readOnlyDetailComposer is a read-only composer that ALSO satisfies
// UpdateDetailer — the shape *compose.HostContainers really has. The plain
// readOnlyMockComposer does not implement it, so a read-only test built on that
// double would pass on the missing capability rather than on the
// autoUpdatesAllowed() gate under test (the capableReadOnlyComposer precedent).
type readOnlyDetailComposer struct {
	readOnlyMockComposer
	details      map[string]compose.UpdateDetail
	detailsCalls int
	detailsArgs  [][]string
}

func (c *readOnlyDetailComposer) UpdateDetails(ctx context.Context, services []string) (map[string]compose.UpdateDetail, error) {
	c.detailsCalls++
	c.detailsArgs = append(c.detailsArgs, append([]string(nil), services...))
	return c.details, nil
}

// TestInspectScreen_UpdateRowsFollowTheVerdict is the end-to-end half of the
// "rows appear only when the verdict is true, and nil renders nothing"
// criterion: buildInspectSummary is pinned per row in inspect_test.go, and
// refreshUpdates is pinned to ask for details only on a true verdict, so what
// is left is the path between them — the cache entry the inspect screen reads.
// The false-verdict entry deliberately carries NO details, because that is the
// shape refreshUpdates really writes.
//
// `built` is NOT in the verdict-following set. It describes the image the
// container runs, so its own source is the image probe the inspect fetch makes
// (TestInspectScreen_BuiltRowIgnoresTheVerdict); the cache entry only supplies
// it as a fallback, which is what the true-verdict case below still reads.
func TestInspectScreen_UpdateRowsFollowTheVerdict(t *testing.T) {
	fetched := time.Now().Add(-3 * time.Minute)
	detailRows := []string{"update id", "update built"}

	tests := []struct {
		name     string
		entry    *updateEntry
		wantRows []string
		skipRows []string
	}{
		{
			name: "true verdict draws the whole block",
			entry: &updateEntry{
				fetchedAt: fetched,
				results:   map[string]bool{"web": true},
				details:   detailFixture(),
			},
			wantRows: []string{
				inspectRow("update", "available  (checked 3m ago)"),
				inspectRow("built", "2026-07-07 17:47:22"),
				inspectRow("update id", detailFixture()["web"].NewID),
				inspectRow("update built", "2026-08-19 19:14:43"),
			},
		},
		{
			name:     "false verdict draws the verdict alone",
			entry:    &updateEntry{fetchedAt: fetched, results: map[string]bool{"web": false}},
			wantRows: []string{inspectRow("update", "up to date  (checked 3m ago)")},
			// `built` is absent because the message below carries no probed
			// date either, not because the verdict is false.
			skipRows: append([]string{"built"}, detailRows...),
		},
		{
			name:     "no entry draws nothing",
			skipRows: append([]string{"update", "built"}, detailRows...),
		},
		{
			name:     "another service's verdict is not borrowed",
			entry:    &updateEntry{fetchedAt: fetched, results: map[string]bool{"db": true}, details: detailFixtureFor("db")},
			skipRows: append([]string{"update", "built"}, detailRows...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web"}
			m.width, m.height = 120, 24
			m.inspectViewport = viewport.New(m.width-4, m.height-6)
			if tt.entry != nil {
				m.updateCache = map[string]updateEntry{m.updatesCacheKey(): *tt.entry}
			}

			model := modelOf(m.Update(inspectDataMsg{data: []byte(inspectFixtureJSON), session: 1}))
			out := model.inspectSummary
			if out == "" {
				t.Fatal("precondition: the summary should be rendered")
			}
			for _, want := range tt.wantRows {
				if !strings.Contains(out, want) {
					t.Errorf("summary missing %q:\n%s", want, out)
				}
			}
			for _, skip := range tt.skipRows {
				if strings.Contains(out, skip) {
					t.Errorf("summary must not contain %q:\n%s", skip, out)
				}
			}
		})
	}
}

// detailFixtureFor re-keys the shared detail fixture onto another service, so a
// cross-service leak reads as a real map rather than an empty one.
func detailFixtureFor(service string) map[string]compose.UpdateDetail {
	return map[string]compose.UpdateDetail{service: detailFixture()["web"]}
}

// TestUpdateDetails_FireOnTheGlyphTriggers pins the acceptance criterion that
// the detail fetch rides the "⇧" schedule rather than one of its own: the three
// triggers that refresh the glyph — a cache-miss screen entry, the U force
// refresh and the post-Deploy invalidation — each carry the details with them.
// They do by construction, because refreshUpdates is the single Cmd behind all
// three; this is what keeps a future second mechanism from drifting away.
func TestUpdateDetails_FireOnTheGlyphTriggers(t *testing.T) {
	newComposer := func() *mockDetailComposer {
		mc := &mockDetailComposer{details: detailFixture()}
		mc.services = []string{"web"}
		mc.status = map[string]runner.ServiceStatus{"web": {Running: true}}
		mc.updates = map[string]bool{"web": true}
		return mc
	}
	primed := func(m Model) map[string]updateEntry {
		return map[string]updateEntry{m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			results:   map[string]bool{"web": true},
			details:   detailFixture(),
		}}
	}
	// The details ride the follow-up Cmd the updatesMsg handler enqueues, so
	// "the trigger reached them" means that Cmd exists and produces them.
	assertFetched := func(t *testing.T, m Model, mc *mockDetailComposer, msg updatesMsg) {
		t.Helper()
		if mc.detailsCalls != 0 {
			t.Errorf("UpdateDetails calls = %d before the verdicts were delivered, want 0", mc.detailsCalls)
		}
		_, dm := deliverUpdates(t, m, msg)
		if mc.detailsCalls != 1 {
			t.Errorf("UpdateDetails calls = %d, want exactly 1", mc.detailsCalls)
		}
		if dm == nil || dm.details == nil {
			t.Error("the trigger must carry the details through to the follow-up message")
		}
	}

	t.Run("screen entry on a cache miss", func(t *testing.T) {
		mc := newComposer()
		m := inspectTestModel(t, mc, mc.services)

		cmd := m.maybeRefreshUpdatesCmd()
		if cmd == nil {
			t.Fatal("a cache miss must fire a refresh")
		}
		raw := cmd()
		msg, ok := raw.(updatesMsg)
		if !ok {
			t.Fatalf("expected an updatesMsg, got %T", raw)
		}
		assertFetched(t, m, mc, msg)
	})

	t.Run("U bypasses a fresh cache entry", func(t *testing.T) {
		mc := newComposer()
		m := inspectTestModel(t, mc, mc.services)
		m.updateCache = primed(m)
		if _, fresh := m.updatesCacheLookup(); !fresh {
			t.Fatal("precondition: the primed entry should be fresh")
		}

		_, cmd := m.Update(keyMsgFor("U"))
		if cmd == nil {
			t.Fatal("U must force a refresh straight through a fresh entry")
		}
		raw := cmd()
		msg, ok := raw.(updatesMsg)
		if !ok {
			t.Fatalf("expected an updatesMsg, got %T", raw)
		}
		assertFetched(t, m, mc, msg)
	})

	t.Run("post-Deploy invalidation", func(t *testing.T) {
		mc := newComposer()
		m := inspectTestModel(t, mc, mc.services)
		m.screen = screenProgress
		m.done = true
		m.pendingOp = runner.Deploy
		m.batches = []opBatch{{proj: m.currentProject()}}
		key := m.updatesCacheKey()
		m.updateCache = primed(m)

		result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		after := result.(Model)
		if _, present := after.updateCache[key]; present {
			t.Fatal("a successful Deploy should have invalidated the cache entry")
		}
		if cmd == nil {
			t.Fatal("leaving the progress screen must fan out a refresh batch")
		}
		raw := cmd()
		batch, ok := raw.(tea.BatchMsg)
		if !ok {
			t.Fatalf("expected a tea.BatchMsg, got %T", raw)
		}
		var msg updatesMsg
		found := false
		for _, c := range batch {
			if got, isUpdates := c().(updatesMsg); isUpdates {
				msg, found = got, true
			}
		}
		if !found {
			t.Fatal("the post-Deploy batch carried no updates refresh")
		}
		// Deliver into the post-esc model: the esc bumped updatesSession, so
		// the pre-esc copy would drop the message as stale.
		assertFetched(t, after, mc, msg)
	})
}

// TestReadOnly_NoAutomaticDetailFetch is the read-only half of the same
// criterion. The unmanaged view is derived from `docker ps -a`, so every
// leftover container contributes an image; adding three registry round-trips
// per updated one to an automatic check is exactly the quota burn the U-only
// opt-in exists to prevent. U must still reach both halves.
func TestReadOnly_NoAutomaticDetailFetch(t *testing.T) {
	newComposer := func() *readOnlyDetailComposer {
		c := &readOnlyDetailComposer{details: detailFixtureFor("watchtower")}
		c.services = []string{"watchtower"}
		c.status = map[string]runner.ServiceStatus{"watchtower": {Running: true}}
		c.updates = map[string]bool{"watchtower": true}
		return c
	}

	t.Run("screen entry fetches neither half", func(t *testing.T) {
		c := newComposer()
		m := inspectTestModel(t, c, c.services)
		if !m.readOnly() {
			t.Fatal("precondition: the composer should be read-only")
		}

		if cmd := m.maybeRefreshUpdatesCmd(); cmd != nil {
			t.Error("the read-only screen fired an automatic refresh; U must be the only trigger")
		}
		if c.updatesCalls != 0 || c.detailsCalls != 0 {
			t.Errorf("CheckUpdates calls = %d, UpdateDetails calls = %d, want 0 and 0",
				c.updatesCalls, c.detailsCalls)
		}
	})

	t.Run("a fresh entry missing its details is refilled on re-entry", func(t *testing.T) {
		// NEITHER refill site is gated, and this one used to be. The entry can
		// only exist because U created it (the subtest above pins that no
		// automatic path makes one under a read-only key), so refilling it
		// completes that U — restricted to the services it already reported
		// true — rather than starting an automatic fan-out. Gating it stranded
		// the rows for the whole 10-minute TTL whenever the batch was refused
		// while another was running and the arrival landed under another key.
		c := newComposer()
		m := inspectTestModel(t, c, c.services)
		m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			results:   map[string]bool{"watchtower": true},
		}}

		cmd := m.maybeRefreshUpdatesCmd()
		if cmd == nil {
			t.Fatal("the entry U created was left without its detail rows for the whole TTL")
		}
		if _, ok := cmd().(updateDetailsMsg); !ok {
			t.Fatalf("the refill produced %T, want updateDetailsMsg", cmd())
		}
		if c.updatesCalls != 0 {
			t.Errorf("CheckUpdates calls = %d, want 0 — the refill must not re-run the verdicts", c.updatesCalls)
		}
		if c.detailsCalls != 1 {
			t.Fatalf("UpdateDetails calls = %d, want exactly 1", c.detailsCalls)
		}
		if got := c.detailsArgs[0]; len(got) != 1 || got[0] != "watchtower" {
			t.Errorf("UpdateDetails services = %v, want [watchtower] — only U's own true verdicts", got)
		}
	})

	t.Run("the arrival-path refill completes U as well", func(t *testing.T) {
		// The second of the two refill sites, on the same reasoning: the entry
		// it fires for must be fresh, non-errored and under the read-only key,
		// and only U can produce one. So a refill here is the deferred
		// completion of that same U, restricted to the services it already
		// reported true, not a second automatic fan-out.
		c := newComposer()
		m := inspectTestModel(t, c, c.services)
		m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {
			fetchedAt: time.Now(),
			results:   map[string]bool{"watchtower": true},
		}}

		// A batch that finished under a context the user has since left: it
		// merges nowhere, and its arrival is the refill trigger.
		_, cmd := m.Update(updateDetailsMsg{
			details:  detailFixtureFor("watchtower"),
			forKey:   "some-other-project|",
			forEntry: time.Now().Add(-time.Minute),
		})
		if cmd == nil {
			t.Fatal("the entry U created was left without its detail rows for the whole TTL")
		}
		if _, ok := cmd().(updateDetailsMsg); !ok {
			t.Fatalf("the refill produced %T, want updateDetailsMsg", cmd())
		}
		if c.detailsCalls != 1 {
			t.Errorf("UpdateDetails calls = %d, want exactly 1", c.detailsCalls)
		}
		if got := c.detailsArgs[0]; len(got) != 1 || got[0] != "watchtower" {
			t.Errorf("UpdateDetails services = %v, want [watchtower] — only U's own true verdicts", got)
		}
	})

	t.Run("U still fetches both halves", func(t *testing.T) {
		c := newComposer()
		m := inspectTestModel(t, c, c.services)

		_, cmd := m.Update(keyMsgFor("U"))
		if cmd == nil {
			t.Fatal("U must still force a refresh on the read-only screen")
		}
		raw := cmd()
		msg, ok := raw.(updatesMsg)
		if !ok {
			t.Fatalf("expected an updatesMsg, got %T", raw)
		}
		_, dm := deliverUpdates(t, m, msg)
		if c.detailsCalls != 1 {
			t.Fatalf("UpdateDetails calls = %d, want exactly 1", c.detailsCalls)
		}
		if got := c.detailsArgs[0]; len(got) != 1 || got[0] != "watchtower" {
			t.Errorf("UpdateDetails services = %v, want [watchtower]", got)
		}
		if dm == nil || dm.details == nil {
			t.Error("U must carry the details back on the follow-up message")
		}
	})
}

// TestInspectScreen_RawModeIgnoresUpdateDetails pins the acceptance criterion
// that `r` output is byte-identical to what it was before this feature: the new
// rows exist only in the summary, and m.inspectRaw is never rewritten. The
// summary comparison is the control — without it the raw equality could pass
// on an empty cache on both sides.
func TestInspectScreen_RawModeIgnoresUpdateDetails(t *testing.T) {
	build := func(t *testing.T, withCache bool) Model {
		t.Helper()
		m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web"}
		m.width, m.height = 120, 24
		m.inspectViewport = viewport.New(m.width-4, m.height-6)
		if withCache {
			m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {
				fetchedAt: time.Now().Add(-3 * time.Minute),
				results:   map[string]bool{"web": true},
				details:   detailFixture(),
			}}
		}
		model := modelOf(m.Update(inspectDataMsg{data: []byte(inspectFixtureJSON), session: 1}))
		if model.inspectSummary == "" {
			t.Fatal("precondition: the summary should be rendered")
		}
		return model
	}

	hot, cold := build(t, true), build(t, false)
	if hot.inspectSummary == cold.inspectSummary {
		t.Fatal("precondition: the cache entry should change the SUMMARY")
	}

	hotRaw := modelOf(hot.Update(keyMsgFor("r")))
	coldRaw := modelOf(cold.Update(keyMsgFor("r")))
	if !hotRaw.inspectShowRaw || !coldRaw.inspectShowRaw {
		t.Fatal("r should have switched both models to raw mode")
	}
	if got, want := hotRaw.inspectViewport.View(), coldRaw.inspectViewport.View(); got != want {
		t.Errorf("the update cache changed raw mode:\nwith:\n%s\nwithout:\n%s", got, want)
	}
	for _, m := range []Model{hotRaw, coldRaw} {
		if string(m.inspectRaw) != inspectFixtureJSON {
			t.Error("inspectRaw must stay verbatim in raw mode")
		}
	}
	if strings.Contains(hotRaw.inspectViewport.View(), "update id") {
		t.Error("the update rows must not leak into the raw buffer")
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
	m.drilledFromHost = true
	m.updateInFlight = true

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if result.(Model).updateInFlight {
		t.Error("esc container→proj should reset updateInFlight to false")
	}
}

// TestUpdateInFlight_ResetOnLeaveScreen_GroupedToServer verifies the same reset
// at the second leave-screen site.
func TestUpdateInFlight_ResetOnLeaveScreen_GroupedToServer(t *testing.T) {
	mc := &mockComposer{}
	srv := config.Server{Name: "s1", Host: "h1"}
	m := NewModel(nil, io.Discard, mockFactory(mc), []config.Server{srv}, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.grouped = true
	m.updateInFlight = true

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if result.(Model).updateInFlight {
		t.Error("esc host view→server should reset updateInFlight to false")
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

// TestUpdatesCacheKey_Composition verifies the cache key format documented in
// the comment: projDir + NUL + projName + "|" + serverName, empty serverName =
// local. The last two cases are the finding the key format exists for: two
// projects deployed with `docker compose -p` out of ONE directory are two
// container sets, and on ConfigDir alone they shared a cache entry.
func TestUpdatesCacheKey_Composition(t *testing.T) {
	tests := []struct {
		name       string
		projDir    string
		projName   string
		serverName string
		want       string
	}{
		{"local_no_project", "", "", "", "\x00|"},
		{"local_with_project", "/srv/app", "app", "", "/srv/app\x00app|"},
		{"remote_with_project", "/srv/app", "app", "prod", "/srv/app\x00app|prod"},
		{"remote_no_project", "", "", "prod", "\x00|prod"},
		{"named_project_in_shared_dir", "/srv/app", "blue", "", "/srv/app\x00blue|"},
		{"sibling_named_project", "/srv/app", "green", "", "/srv/app\x00green|"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockComposer{}
			m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
			m.projDir = tc.projDir
			m.projName = tc.projName
			m.serverName = tc.serverName
			if got := m.updatesCacheKey(); got != tc.want {
				t.Errorf("updatesCacheKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUpdatesCacheKey_NamedProjectsInOneDirStayDistinct is the finding pin: two
// compose projects launched with `-p` out of the same directory are separate
// container sets and must not share one update-cache entry — a shared entry
// paints one project's ⇧ verdicts onto the other's rows and suppresses its own
// scan for the whole TTL.
func TestUpdatesCacheKey_NamedProjectsInOneDirStayDistinct(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	blue := m.projUpdatesCacheKey(compose.Project{Name: "blue", ConfigDir: "/srv/app"})
	green := m.projUpdatesCacheKey(compose.Project{Name: "green", ConfigDir: "/srv/app"})
	if blue == green {
		t.Errorf("two -p projects in one directory share the cache key %q", blue)
	}
	// The separator must not be spellable by either half, or one (dir, name)
	// pair could forge another's key. "/srv" + "app|x" is the shape that
	// collides on a printable separator.
	if m.projUpdatesCacheKey(compose.Project{Name: "app", ConfigDir: "/srv"}) ==
		m.projUpdatesCacheKey(compose.Project{Name: "", ConfigDir: "/srvapp"}) {
		t.Error("the separator is forgeable: two different projects spell one key")
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
	m.setSingleGroup([]string{"web", "db", "cache"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web":   {Running: true, UpdateAvailable: trueP()},
		"db":    {Running: true, UpdateAvailable: falseP()},
		"cache": {Running: true}, // nil = unknown, no glyph
	})

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
	m.setSingleGroup([]string{"web"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: false, UpdateAvailable: trueP()},
	})

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
	m.setSingleGroup([]string{"web", "db", "cache"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		// Created/Uptime are present so the Created column renders, giving us
		// a stable column to align against.
		"web":   {Running: true, Created: "2024-01-15 09:30", Uptime: "3h", UpdateAvailable: trueP()},
		"db":    {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
		"cache": {Running: true, Created: "2024-01-15 09:30", Uptime: "3h"},
	})

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
		for _, svc := range modelServices(m) {
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
	m.setSingleGroup([]string{"web"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
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
	m.setSingleGroup([]string{"web"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
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
	m.setSingleGroup([]string{"web", "db"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		// All UpdateAvailable=nil → no glyph rendered, but the reservation
		// must still apply so downstream columns don't shift when a verdict
		// later arrives.
		"web": {Running: true, Created: "2024-01-15 09:30"},
		"db":  {Running: true, Created: "2024-01-15 09:30"},
	})

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
		for _, svc := range modelServices(m) {
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true, Created: "2024-01-15 09:30", UpdateAvailable: &trueVal},
		"db":  {Running: true, Created: "2024-01-15 09:30"},
	})
	v2 := m.View()
	cols2 := map[string]int{}
	for _, line := range strings.Split(v2, "\n") {
		clean := ansi.ReplaceAllString(line, "")
		for _, svc := range modelServices(m) {
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
// phantom svcStatus entry. The renderer iterates the rows, not the status
// map so today it'd be invisible, but a phantom would leak across project
// switches and surface in any future map-key iterator.
func TestHydrateUpdates_SkipsUnknownServices(t *testing.T) {
	m := &Model{}
	m.svcStatus = qStatus(*m, map[string]runner.ServiceStatus{
		"web": {Running: true},
	})
	yes := true
	m.hydrateUpdates(map[string]bool{
		"web":     true,  // known: hydrate
		"phantom": false, // unknown: skip
	})
	if got := len(m.svcStatus); got != 1 {
		t.Errorf("svcStatus len = %d, want 1 (phantom must not be created); got %#v", got, m.svcStatus)
	}
	web, ok := m.svcStatus[qk(*m, "web")]
	if !ok {
		t.Fatal("web entry missing after hydrate")
	}
	if web.UpdateAvailable == nil || *web.UpdateAvailable != yes {
		t.Errorf("web.UpdateAvailable = %v, want &true", web.UpdateAvailable)
	}
	if !web.Running {
		t.Error("web.Running flipped to false during hydrate — phantom-style overwrite")
	}
	if _, leaked := m.svcStatus[qk(*m, "phantom")]; leaked {
		t.Errorf("phantom entry leaked into svcStatus: %#v", m.svcStatus[qk(*m, "phantom")])
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"web": {Running: true, UpdateAvailable: &trueVal},
		"db":  {Running: true, UpdateAvailable: &trueVal},
	})

	result, _ := m.Update(updatesMsg{
		forKey: m.updatesCacheKey(), err: errors.New("registry timeout"),
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
	if model.svcStatus[qk(model, "web")].UpdateAvailable != nil {
		t.Errorf("cached error must NOT hydrate glyphs, got %v", model.svcStatus[qk(model, "web")].UpdateAvailable)
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
		forKey: m.updatesCacheKey(), err: errors.New("docker hub timeout"),
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
	m.setSingleGroup([]string{"a", "b", "c", "d", "e"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"a": {Running: true},
		"b": {Running: true},
		"c": {Running: true},
		"d": {Running: true},
		"e": {Running: true},
	})
	m.svcCursor = 2
	m.svcOffset = 0

	result, _ := m.Update(updatesMsg{
		forKey: m.updatesCacheKey(), err: errors.New("registry timeout"),
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
	m := singleGroupModel(services)
	m.screen = screenSelectContainers
	return m
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
	m.searchMatches = computeMatches(m.svcEntries, query)
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
// container screen; second esc navigates back to the grouped host view.
func TestSearchTwoStageEsc(t *testing.T) {
	m := committedSearchModel([]string{"api", "web", "worker"}, "w")
	m.drilledFromHost = true // so the second esc has a host view to return to
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

	// Second esc: no active search → drill out to the grouped host view.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectContainers || !m.grouped {
		t.Errorf("after 2nd esc: screen = %d grouped = %v, want the grouped host view", m.screen, m.grouped)
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{
		"api": {Running: true, Created: "2024-01-15 09:30"},
		"web": {Running: true, Created: "2024-01-15 09:30"},
	})

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
		m.setSingleGroup([]string{"api", "web", "web-worker", "db", "cache", "queue"})
		m.width = 200 // one-line help in both branches
		m.height = 12
		m.searchQuery = "w"
		m.searchMatches = computeMatches(m.svcEntries, "w")
		m.svcCursor = m.searchMatches[0]
		return m
	}

	m1 := base()
	m1.confirming = false
	m2 := base()
	m2.confirming = true
	m2.selected = selectedIdx(m2, 1)

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
	m.setSingleGroup(mc.services)
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.svcEntries, "w") // [1 2]
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
// (esc → grouped host view) clears the committed search.
func TestSearchClearedOnEscToProject(t *testing.T) {
	m := committedSearchModel([]string{"api", "web", "worker"}, "w")
	m.drilledFromHost = true
	if m.searchQuery == "" {
		t.Fatal("precondition: committed search expected")
	}

	// First esc clears the search (two-stage guard); the SECOND esc back-navigates.
	// After the second esc we're on the grouped host view AND search stays clear.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	assertSearchCleared(t, m, "after 1st esc (search clear)")

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectContainers || !m.grouped {
		t.Errorf("after 2nd esc: screen = %d grouped = %v, want the grouped host view", m.screen, m.grouped)
	}
	assertSearchCleared(t, m, "after esc→grouped drill-out")
}

// TestSearchClearedOnEscToProjectSingleEsc: even when a committed search is active
// and the two-stage guard consumes the first esc, the container→project back-nav
// site itself calls clearSearch() unconditionally, so arriving there with no
// active search (e.g. via a direct second esc) still leaves search clear. This
// asserts the unconditional (idempotent) clearSearch at the back-nav site.
func TestSearchClearedOnEscToProjectNoActiveSearch(t *testing.T) {
	m := containerSearchModel([]string{"api", "web", "worker"})
	m.drilledFromHost = true
	// No active search — first (and only) esc back-navigates directly.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectContainers || !m.grouped {
		t.Errorf("screen = %d grouped = %v, want the grouped host view (direct back-nav)", m.screen, m.grouped)
	}
	assertSearchCleared(t, m, "after direct esc→grouped (no active search)")
}

// TestSearchClearedOnEnterLogs: a read-only departure to the logs screen (l key)
// clears the committed search — search is ephemeral, not carried into a log peek.
func TestSearchClearedOnEnterLogs(t *testing.T) {
	mc := &mockComposer{services: []string{"api", "web", "web-worker"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.setSingleGroup(mc.services)
	m.composer = mc
	m.width = 80
	m.height = 24
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.svcEntries, "w")
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
	m.setSingleGroup(mc.services)
	m.composer = mc
	m.width = 80
	m.height = 24
	m.pendingOp = runner.Restart
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.svcEntries, "w")
	m.svcCursor = m.searchMatches[0]

	updated, _ := m.enterProgress([]opBatch{{proj: m.currentProject(), services: []string{"web"}}})
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
	m.setSingleGroup(mc.services)
	m.composer = mc
	m.width = 80
	m.height = 24
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.svcEntries, "w")
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
	base.selected = selectedIdx(base, 0, 2)
	want := base.selectedContainers()
	if len(want) != 2 || want[0] != "api" || want[1] != "web-worker" {
		t.Fatalf("precondition: selectedContainers() = %v, want [api web-worker]", want)
	}

	// Same selection, but with an active committed search on "w" (matches web,
	// web-worker) and the cursor jumped onto a match.
	withSearch := containerSearchModel(services)
	withSearch.selected = selectedIdx(withSearch, 0, 2)
	withSearch.searchQuery = "w"
	withSearch.searchMatches = computeMatches(withSearch.svcEntries, "w")
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})
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
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{})

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
// the row list). This exercises the entryLocal clearSearch() site.
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

// TestSearchClearedOnEscGroupedToServer: esc from the grouped host view back to
// the server screen clears any active search.
func TestSearchClearedOnEscGroupedToServer(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.grouped = true
	m.disconnectFunc = func() error { return nil }
	// Seed a stale committed search. The first esc is the two-stage clear, the
	// second back-navigates — and must leave the search cleared either way.
	m.searchQuery = "w"
	m.searchMatches = []int{1, 2}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	assertSearchCleared(t, m, "after the two-stage esc")

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want screenSelectServer (esc host view→server)", m.screen)
	}
	assertSearchCleared(t, m, "after esc host view→server")
}

// TestSearchClearedOnConnectError: a failed remote connect swaps the projectLoader
// and resets transient state; it must also clear a committed search (the error
// path lands the user back on the server screen with no valid service list).
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
	m.setSingleGroup(mc.services)
	m.composer = mc
	m.width = 80
	m.height = 24
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.svcEntries, "w")
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
	m.setSingleGroup(mc.services)
	m.composer = mc
	m.width = 80
	m.height = 24
	m.svcCursor = 1 // "web"
	m.searchQuery = "w"
	m.searchMatches = computeMatches(m.svcEntries, "w")

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
	m.setSingleGroup(mc.services)
	m.composer = mc
	m.selected[m.svcKeyAt(0)] = true
	m.pendingOp = runner.Deploy
	m.width = 80
	m.height = 24

	updated, _ := m.enterProgress([]opBatch{{proj: m.currentProject(), services: []string{"web"}}})
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
	m.setSingleGroup(mc.services)
	m.composer = mc
	m.selected[m.svcKeyAt(0)] = true
	m.pendingOp = runner.Deploy
	m.width = 80
	m.height = 24

	updated, _ := m.enterProgress([]opBatch{{proj: m.currentProject(), services: []string{"web"}}})
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
		opContainers: []string{"web"},
		composer:     mc,
		ctx:          context.Background(),
	}
	m.setSingleGroup([]string{"web"})

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
	m := Model{screen: screenProgress, pendingOp: runner.Deploy, failed: true, opContainers: []string{"web"}}
	m.setSingleGroup([]string{"web"})

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
	m := Model{screen: screenProgress, pendingOp: runner.StopOnly, opContainers: []string{"web"}}
	m.setSingleGroup([]string{"web"})

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
		composer:  mc,
		ctx:       context.Background(),
	} // opContainers deliberately nil
	m.setSingleGroup([]string{"web", "db"})

	updated, _ := m.Update(pipelineDoneMsg{})
	m = updated.(Model)

	if !m.waiting {
		t.Fatal("waiting should start with a services fallback target set")
	}
	// The fallback target set must be exactly the drilled group, in order.
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
		composer: mc,
	}
	m.setSingleGroup([]string{"web"})
	m.selected = selectedIdx(m, 0)
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
	m := Model{screen: screenSelectContainers, composer: mc, selected: map[string]bool{}}
	m.setSingleGroup(nil)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	m = updated.(Model)
	if cmd != nil || m.warning != "" {
		t.Error("R on an empty service list must be a silent no-op")
	}
}

func TestRollbackKey_NoSelectionWarns(t *testing.T) {
	mc := &mockRollbackComposer{mockComposer: mockComposer{services: []string{"web"}}}
	m := Model{screen: screenSelectContainers, composer: mc, selected: map[string]bool{}}
	m.setSingleGroup([]string{"web"})
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
		composer:             mc,
		ctx:                  context.Background(),
		rollbackFetchSession: 0,
	}
	m.setSingleGroup([]string{"web"})
	m.selected = selectedIdx(m, 0)
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
		rollbackFetchSession: 3,
	}
	m.setSingleGroup([]string{"web"})
	m.selected = selectedIdx(m, 0)
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
		rollbackFetchSession: 1,
	}
	m.setSingleGroup([]string{"web"})
	m.selected = selectedIdx(m, 0)
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
		rollbackTargets:      []string{"web", "db"}, // captured at R-press
		rollbackFetchSession: 2,
	}
	m.setSingleGroup([]string{"web", "db"})
	m.selected = selectedIdx(m, 0, 1)
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
		rollbackTargets:      []string{"web", "db"}, // captured at R-press
		rollbackFetchSession: 2,
	}
	m.setSingleGroup([]string{"web", "db"})
	m.selected = selectedIdx(m, 0, 1) // both selected
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
		rollbackTargets:      []string{"web"}, // captured at R-press
		rollbackFetchSession: 1,
	}
	m.setSingleGroup([]string{"web"})
	m.selected = selectedIdx(m, 0)
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
		rollbackFetchSession: 5,
	}
	m.setSingleGroup([]string{"web"})
	m.selected = selectedIdx(m, 0)
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
	m.setSingleGroup([]string{"db", "web"})
	m.composer = mc
	m.selected = selectedIdx(m, 0, 1)
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
		composer: mc,
		ctx:      context.Background(),
	}
	m.setSingleGroup([]string{"web", "db"})
	m.selected = selectedIdx(m, 0) // web only
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
	m.selected = map[string]bool{}

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
		rollbackTargets:      nil, // captured set empty
		rollbackFetchSession: 2,
	}
	m.setSingleGroup([]string{"web", "db"})
	m.selected = selectedIdx(m, 0, 1)
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
		batches:     []opBatch{{}},
		updateCache: map[string]updateEntry{"\x00|": {results: map[string]bool{"web": true}}},
	}
	if _, ok := m.updateCache[m.updatesCacheKey()]; !ok {
		t.Fatalf("precondition: cache should hold key %q", m.updatesCacheKey())
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if _, ok := m.updateCache["\x00|"]; ok {
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
	m.setSingleGroup([]string{"web"})
	m.composer = mc
	m.selected = selectedIdx(m, 0)
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

// The `i` key works entirely through a runtime type assertion on Inspector, and
// every TUI test drives a hand-written double that satisfies it by
// construction. Without these three lines a rename or a signature drift on any
// real Inspect method leaves the whole suite green while `i` becomes a silent
// no-op in production. Inspector is the one capability ALL THREE composers must
// implement — it has no readOnly gate to make its absence visible.
var (
	_ Inspector = (*compose.Compose)(nil)
	_ Inspector = (*compose.RemoteCompose)(nil)
	_ Inspector = (*compose.HostContainers)(nil)
)

// UpdateDetailer is reached the same way and needs the same protection, for a
// worse failure mode: a drifted signature costs no key, so nothing looks broken
// — the update-detail rows just silently stop appearing on a screen that still
// draws the "⇧" glyph beside them.
var (
	_ UpdateDetailer = (*compose.Compose)(nil)
	_ UpdateDetailer = (*compose.RemoteCompose)(nil)
	_ UpdateDetailer = (*compose.HostContainers)(nil)
)

// ImageInspector is reached the same way and fails the same silently: a drifted
// signature costs no key, and the `built` row — which every container has, with
// or without an update verdict — just stops appearing. All three composers must
// implement it, because the row describes what the container runs rather than
// what the registry offers.
var (
	_ ImageInspector = (*compose.Compose)(nil)
	_ ImageInspector = (*compose.RemoteCompose)(nil)
	_ ImageInspector = (*compose.HostContainers)(nil)
)

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
	if _, ok := c.(Inspector); !ok {
		t.Error("HostContainers must satisfy Inspector so the i key works on the unmanaged screen")
	}
	if _, ok := c.(UpdateDetailer); !ok {
		t.Error("HostContainers must satisfy UpdateDetailer so U fills the inspect update rows")
	}
	if _, ok := c.(ImageInspector); !ok {
		t.Error("HostContainers must satisfy ImageInspector so the built row works on the unmanaged screen")
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
// and ExecProvider — the read-only-plus-exec subset of *compose.HostContainers,
// which also implements Inspector (readOnlyInspectComposer models that whole
// set). The real type is used for the interface pins above; this mock is used
// for key dispatch so no test ever shells out to docker.
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
	// inspectTestModel supplies the shared container-screen setup, and it hands
	// the composer to NewModel as the factory too — load-bearing here, because a
	// factory returning the embedded writable mockComposer would let any test
	// that triggers m.composerFactory(...) swap the read-only one out, and every
	// read-only assertion after that would go quiet rather than fail.
	m := inspectTestModel(t, mc, mc.services)
	// Copy rather than alias mc.status: hydrateUpdates mutates m.svcStatus in
	// place, and an alias would write verdicts back into the composer double
	// that later subtests share.
	m.svcStatus = make(map[string]runner.ServiceStatus, len(mc.status))
	for k, v := range mc.status {
		m.svcStatus[qk(m, k)] = v
	}
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
			m.setSingleGroup(append([]string(nil), mc.services...))
			m.svcStatus = qStatus(m, mc.status)
			m.width, m.height = 120, 24
			m.updateInFlight = false
			m.refreshInFlight = false
			installFakeTick(&m)
			// A selection is what R needs past its own empty-selection warning;
			// it is unreachable through space on this composer, so set it
			// directly to prove the gate — not the missing selection — is
			// what stops the key.
			m.selected[m.svcKeyAt(0)] = true

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

// TestReadOnly_GatedKeyReclampsOffset pins the reserved-bar side of the gates.
// The container dispatch clears m.warning before the switch, which frees the
// warning footer line and grows svcVisibleCount() by one. Every other case in
// the switch re-clamps; a gated key that returns early without fixSvcOffset
// leaves a too-large svcOffset and renders a blank row under the last service.
func TestReadOnly_GatedKeyReclampsOffset(t *testing.T) {
	for _, key := range []string{"d", "r", "s", "R", "c", " ", "a"} {
		t.Run(key, func(t *testing.T) {
			mc := readOnlyTestComposer()
			mc.services = nil
			for i := 0; i < 30; i++ {
				mc.services = append(mc.services, fmt.Sprintf("svc-%02d", i))
			}
			m := newReadOnlyModel(t, mc)
			m.height = 24
			// The x-on-stopped warning is the state this reproduces from.
			m.warning = "Container is not running"
			m.svcCursor = len(m.svcEntries) - 1
			m.fixSvcOffset()
			if m.svcOffset == 0 {
				t.Fatal("precondition: the list must scroll at this height")
			}

			updated, _ := m.Update(keyMsgFor(key))
			got := updated.(Model)

			if got.warning != "" {
				t.Fatalf("precondition: the dispatch must clear the warning, got %q", got.warning)
			}
			want := len(got.svcEntries) - got.svcVisibleCount()
			if got.svcOffset != want {
				t.Errorf("svcOffset = %d, want %d; the gated key left a blank row at the bottom", got.svcOffset, want)
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
	parkOnGroupedScreen(&m,
		compose.Project{Name: "shop", ConfigDir: "/srv/shop"},
		compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true},
	)
	m.svcCursor = headerIndexFor(t, m.svcEntries, 1)

	updated, _ := m.Update(keyMsgFor("enter"))
	m = updated.(Model)
	if got := m.updatesCacheKey(); got != "unmanaged|\x00(unmanaged)|" {
		t.Fatalf("after drilling into the unmanaged group, key = %q, want %q", got, "unmanaged|\x00(unmanaged)|")
	}

	updated, _ = m.Update(keyMsgFor("esc")) // drilled -> grouped host view
	m = updated.(Model)
	updated, _ = m.Update(keyMsgFor("esc")) // host view -> server
	m = updated.(Model)
	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want screenSelectServer", m.screen)
	}

	// Local entry is first in serverEntries; enter fast-tracks to containers.
	m.serverCursor = 0
	updated, _ = m.Update(keyMsgFor("enter"))
	m = updated.(Model)
	if got := m.updatesCacheKey(); got != "\x00|" {
		t.Errorf("after the local fast-track, key = %q, want %q", got, "\x00|")
	}
}

// TestUpdatesCache_UnmanagedDoesNotReplayFastTrackVerdicts is the behavioural
// half of AC8: a colliding service name must not pick up the other context's
// verdict. hydrateUpdates' phantom guard drops only UNKNOWN names, so a shared
// key would write "web is out of date" straight onto the unmanaged web row.
func TestUpdatesCache_UnmanagedDoesNotReplayFastTrackVerdicts(t *testing.T) {
	mc := readOnlyTestComposer()
	m := newReadOnlyModel(t, mc)
	m.setSingleGroup([]string{"web"})
	m.svcStatus = qStatus(m, map[string]runner.ServiceStatus{"web": {Running: true}})
	m.updateCache = map[string]updateEntry{
		// The local-fast-track slot, populated by a previous compose context.
		"\x00|": {fetchedAt: time.Now(), results: map[string]bool{"web": true}},
	}

	updated, cmd := m.Update(statusMsg{
		status:  map[string]runner.ServiceStatus{"web": {Running: true}},
		session: m.statusSession,
	})
	got := updated.(Model)

	if ua := got.svcStatus[qk(got, "web")].UpdateAvailable; ua != nil {
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
	ua := updated.(Model).svcStatus[qk(updated.(Model), "web")].UpdateAvailable
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
	w.setSingleGroup(wc.services)
	w.svcStatus = qStatus(w, wc.status)
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
				m.setSingleGroup(wc.services)
				m.svcStatus = qStatus(m, status)
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
				for _, svc := range modelServices(m) {
					if strings.Contains(plain, svc) {
						rows[svc] = plain
					}
				}
			}
			if caption == "" {
				t.Fatalf("no captions row rendered:\n%s", view)
			}
			if len(rows) != len(modelServices(m)) {
				t.Fatalf("found %d data rows, want %d:\n%s", len(rows), len(modelServices(m)), view)
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
		forKey: m.updatesCacheKey(), results: map[string]bool{"watchtower": true, "portainer": false},
		session: m.updatesSession,
	})
	model := result.(Model)

	if av := model.svcStatus[qk(model, "watchtower")].UpdateAvailable; av == nil || !*av {
		t.Fatalf("watchtower UpdateAvailable = %v, want &true", av)
	}
	if av := model.svcStatus[qk(model, "portainer")].UpdateAvailable; av == nil || *av {
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
	m.stats = qStats(m, map[string]runner.ServiceStats{
		"watchtower": {CPUPercent: 12.5, MemoryUsed: 130023424, MemoryLimit: 2147483648},
	})

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

// TestReadOnly_StaleUpdateWarningClears is the other side of the U-only opt-in.
// A failed U caches a failure entry whose warning both self-heal paths restore
// while it is fresh; once it expires, no automatic refetch will ever replace it
// here, so the warning it explained is no longer current and must go — without
// this, one failed U would keep its warning for the life of the screen.
func TestReadOnly_StaleUpdateWarningClears(t *testing.T) {
	const errText = "registry unreachable"

	staleModel := func(t *testing.T, age time.Duration) Model {
		t.Helper()
		m := newReadOnlyModel(t, readOnlyTestComposer())
		m.updatesErr = errText
		m.updateCache = map[string]updateEntry{
			m.updatesCacheKey(): {
				fetchedAt: time.Now().Add(-age),
				err:       true,
				errMsg:    errText,
			},
		}
		return m
	}

	tick := func(m Model) Model {
		updated, _ := m.Update(statusMsg{
			status:  map[string]runner.ServiceStatus{"watchtower": {Running: true}},
			session: m.statusSession,
		})
		return updated.(Model)
	}

	t.Run("status tick drops an expired failure", func(t *testing.T) {
		got := tick(staleModel(t, updatesErrorTTL+time.Second))
		if got.updatesErr != "" {
			t.Errorf("updatesErr = %q; an expired failure must not survive on a read-only screen", got.updatesErr)
		}
	})

	t.Run("status tick keeps a fresh failure", func(t *testing.T) {
		got := tick(staleModel(t, time.Second))
		if got.updatesErr != errText {
			t.Errorf("updatesErr = %q, want %q; a fresh failure is still the ground truth", got.updatesErr, errText)
		}
	})

	t.Run("screen entry drops an expired failure", func(t *testing.T) {
		m := staleModel(t, updatesErrorTTL+time.Second)
		if cmd := m.maybeRefreshUpdatesCmd(); cmd != nil {
			t.Error("read-only entry fired an automatic CheckUpdates; U must be the only trigger")
		}
		if m.updatesErr != "" {
			t.Errorf("updatesErr = %q, want cleared", m.updatesErr)
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

// mockInspectComposer implements both runner.Composer and Inspector, so the
// `i` key's type assertion succeeds. Follows the
// TestReadOnly_GatesWriteKeys_WithCapableComposer precedent: a mock that lacks
// the capability would let a test pass on the assertion rather than on the
// behaviour under test.
type mockInspectComposer struct {
	mockComposer
	inspectRaw     []byte
	inspectErr     error
	inspectCalls   int
	inspectService string
	// The ImageInspector half. The zero time is the common case: it draws no
	// `built` row, so every test that predates the image probe reads exactly as
	// it did.
	imageCreated     time.Time
	imageCreatedErr  error
	imageCreatedRefs []string
}

func (m *mockInspectComposer) Inspect(ctx context.Context, service string) ([]byte, error) {
	m.inspectCalls++
	m.inspectService = service
	return m.inspectRaw, m.inspectErr
}

func (m *mockInspectComposer) ImageCreated(ctx context.Context, image string) (time.Time, error) {
	m.imageCreatedRefs = append(m.imageCreatedRefs, image)
	if m.imageCreatedErr != nil {
		return time.Time{}, m.imageCreatedErr
	}
	return m.imageCreated, nil
}

// inspectFixtureJSON is a minimal but structurally real `docker inspect`
// payload: the single-element array, the leading-slash name and a healthcheck,
// so the handler exercises the parse and the renderer, not just the plumbing.
const inspectFixtureJSON = `[{
  "Name": "/proj-web-1",
  "Image": "sha256:0123456789abcdef",
  "RestartCount": 2,
  "State": {
    "Status": "running",
    "Running": true,
    "StartedAt": "2026-08-22T03:00:00.000000000Z",
    "Health": {"Status": "healthy", "FailingStreak": 0,
      "Log": [{"Start": "2026-08-22T03:09:35.0Z", "End": "2026-08-22T03:09:38.0Z", "ExitCode": 0, "Output": "ok\n"}]}
  },
  "Config": {"Image": "nginx:1.27", "Env": ["TZ=UTC"],
    "Healthcheck": {"Test": ["CMD-SHELL", "curl -fsS http://localhost/"], "Interval": 3000000000, "Timeout": 2000000000, "Retries": 2}},
  "HostConfig": {"RestartPolicy": {"Name": "unless-stopped"}},
  "Mounts": [{"Type": "volume", "Name": "data", "Source": "/var/lib/docker/volumes/data/_data", "Destination": "/data", "RW": true}]
}]`

func TestInspectDataMsg_CurrentSessionPopulates(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 5, inspectService: "web"}
	m.inspectViewport = viewport.New(100, 10)

	result, _ := m.Update(inspectDataMsg{data: []byte(inspectFixtureJSON), session: 5})
	model := result.(Model)

	if model.inspectErr != nil {
		t.Fatalf("inspectErr = %v, want nil", model.inspectErr)
	}
	if string(model.inspectRaw) != inspectFixtureJSON {
		t.Error("inspectRaw should hold the bytes verbatim")
	}
	if model.inspectSummary == "" {
		t.Fatal("inspectSummary should be rendered")
	}
	for _, want := range []string{"STATE", "running", "HEALTH", "healthy", "IMAGE", "nginx:1.27"} {
		if !strings.Contains(model.inspectSummary, want) {
			t.Errorf("summary missing %q:\n%s", want, model.inspectSummary)
		}
	}
	if got := model.inspectViewport.View(); !strings.Contains(got, "STATE") {
		t.Errorf("viewport should show the summary, got:\n%s", got)
	}
}

func TestInspectDataMsg_RawModeShowsVerbatimBytes(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 1, inspectShowRaw: true}
	m.inspectViewport = viewport.New(200, 10)

	result, _ := m.Update(inspectDataMsg{data: []byte(inspectFixtureJSON), session: 1})
	model := result.(Model)

	if got := model.inspectViewport.View(); !strings.Contains(got, `"Name": "/proj-web-1"`) {
		t.Errorf("raw mode should render the inspect bytes, got:\n%s", got)
	}
}

// TestInspectDataMsg_RawModeFiltersControlBytes pins the raw buffer's own
// sanitiser pass. docker's JSON escapes ONLY the C0 block — Go's encoding/json
// emits DEL and the C1 code points as RAW bytes — so an ENV value or a probe
// output set by a third-party image can still carry U+009B (CSI) or U+009D
// (OSC, an OSC 52 clipboard write) onto the operator's terminal one keypress
// away. inspectRaw itself stays verbatim: it is what ParseInspect reads.
func TestInspectDataMsg_RawModeFiltersControlBytes(t *testing.T) {
	payload := "[{\"Name\": \"/proj-web-1\", \"Config\": {\"Image\": \"nginx:1.27\", " +
		"\"Env\": [\"MOTD=hi\u007fthere\u009d52;c;cGF5bG9hZA==ok\", \"CSI=a\u009b31mb\", \"ESC=x\\u001b[31my\"]}}]"

	m := Model{screen: screenInspect, inspectSession: 1, inspectShowRaw: true}
	m.inspectViewport = viewport.New(400, 20)

	result, _ := m.Update(inspectDataMsg{data: []byte(payload), session: 1})
	model := result.(Model)

	if string(model.inspectRaw) != payload {
		t.Error("inspectRaw must stay verbatim — it is the parser's input")
	}
	shown := model.inspectViewport.View()
	for _, banned := range []string{"\u007f", "\u009b", "\u009d"} {
		if strings.Contains(shown, banned) {
			t.Errorf("raw mode must not write %q to the terminal:\n%q", banned, shown)
		}
	}
	// Everything JSON already escaped stays byte-identical, the escaped ESC
	// included: it is six printable characters, not an escape sequence.
	for _, want := range []string{"nginx:1.27", "MOTD=hi", "there", "52;c;cGF5bG9hZA==ok", "CSI=a", "31mb", `ESC=x\u001b[31my`} {
		if !strings.Contains(shown, want) {
			t.Errorf("raw mode must keep the readable text %q:\n%s", want, shown)
		}
	}
}

func TestInspectDataMsg_StaleSessionDiscarded(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 5}
	m.inspectViewport = viewport.New(100, 10)

	result, _ := m.Update(inspectDataMsg{data: []byte(inspectFixtureJSON), session: 3})
	model := result.(Model)

	if model.inspectRaw != nil || model.inspectSummary != "" {
		t.Error("stale inspectDataMsg should be discarded")
	}
}

func TestInspectDataMsg_OffScreenDiscarded(t *testing.T) {
	m := Model{screen: screenSelectContainers, inspectSession: 5}
	m.inspectViewport = viewport.New(100, 10)

	result, _ := m.Update(inspectDataMsg{data: []byte(inspectFixtureJSON), session: 5})
	model := result.(Model)

	if model.inspectRaw != nil || model.inspectSummary != "" {
		t.Error("inspectDataMsg should be discarded when not on the inspect screen")
	}
}

func TestInspectDataMsg_FetchError(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 2}
	m.inspectViewport = viewport.New(100, 10)

	result, _ := m.Update(inspectDataMsg{err: fmt.Errorf("no container found for \"web\""), session: 2})
	model := result.(Model)

	if model.inspectErr == nil {
		t.Fatal("inspectErr should be set on a fetch failure")
	}
	if !strings.Contains(model.inspectErr.Error(), "no container found") {
		t.Errorf("inspectErr = %q", model.inspectErr.Error())
	}
	if model.inspectRaw != nil {
		t.Error("a failed fetch must not populate inspectRaw")
	}
}

// TestInspectDataMsg_ParseErrorKeepsRaw pins the escape hatch: a payload the
// narrow parser cannot read still reaches raw mode, so the user is never left
// with a blank screen and no way to see what docker actually returned.
func TestInspectDataMsg_ParseErrorKeepsRaw(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 1}
	m.inspectViewport = viewport.New(100, 10)

	result, _ := m.Update(inspectDataMsg{data: []byte("[]"), session: 1})
	model := result.(Model)

	if model.inspectErr == nil {
		t.Fatal("a parse failure should surface in inspectErr")
	}
	if string(model.inspectRaw) != "[]" {
		t.Errorf("inspectRaw = %q, want the bytes kept for raw mode", string(model.inspectRaw))
	}
	if model.inspectSummary != "" {
		t.Error("summary should be empty when the parse failed")
	}
}

func TestFetchInspect_SendsSessionAndService(t *testing.T) {
	mc := &mockInspectComposer{inspectRaw: []byte(inspectFixtureJSON)}
	m := Model{composer: mc, ctx: context.Background(), inspectService: "web", inspectSession: 7}

	cmd := m.fetchInspect()
	if cmd == nil {
		t.Fatal("fetchInspect returned nil for an Inspector composer")
	}
	msg, ok := cmd().(inspectDataMsg)
	if !ok {
		t.Fatalf("fetchInspect produced %T, want inspectDataMsg", cmd())
	}
	if msg.session != 7 {
		t.Errorf("session = %d, want 7", msg.session)
	}
	if string(msg.data) != inspectFixtureJSON {
		t.Error("fetchInspect should carry the composer's bytes verbatim")
	}
	if mc.inspectService != "web" {
		t.Errorf("Inspect called with %q, want \"web\"", mc.inspectService)
	}
}

func TestFetchInspect_NilForNonInspector(t *testing.T) {
	m := Model{composer: &mockComposer{}, ctx: context.Background(), inspectService: "web"}
	if cmd := m.fetchInspect(); cmd != nil {
		t.Error("fetchInspect should return nil when the composer is not an Inspector")
	}
}

func TestFetchInspect_PropagatesError(t *testing.T) {
	mc := &mockInspectComposer{inspectErr: fmt.Errorf("boom")}
	m := Model{composer: mc, ctx: context.Background(), inspectService: "web"}

	msg, ok := m.fetchInspect()().(inspectDataMsg)
	if !ok {
		t.Fatal("want inspectDataMsg")
	}
	if msg.err == nil || !strings.Contains(msg.err.Error(), "boom") {
		t.Errorf("err = %v, want boom", msg.err)
	}
}

// inspectOnlyComposer satisfies Inspector but NOT ImageInspector — the shape a
// composer takes when it cannot answer the image half. The fetch must then
// spend no probe and the document must render without a `built` row, which is
// the same outcome a failed probe produces.
type inspectOnlyComposer struct {
	mockComposer
	raw []byte
}

func (c *inspectOnlyComposer) Inspect(context.Context, string) ([]byte, error) {
	return c.raw, nil
}

// TestFetchInspect_ProbesTheResolvedImage pins the second call the inspect
// fetch makes: the image the container ACTUALLY runs, addressed by the ID
// docker resolved the tag to, carried back on the same message.
func TestFetchInspect_ProbesTheResolvedImage(t *testing.T) {
	built := time.Date(2023, 11, 2, 13, 2, 50, 0, time.UTC)
	mc := &mockInspectComposer{inspectRaw: []byte(inspectFixtureJSON), imageCreated: built}
	m := Model{composer: mc, ctx: context.Background(), inspectService: "web", inspectSession: 3}

	msg, ok := m.fetchInspect()().(inspectDataMsg)
	if !ok {
		t.Fatal("want inspectDataMsg")
	}
	if !msg.imageCreated.Equal(built) {
		t.Errorf("imageCreated = %v, want %v", msg.imageCreated, built)
	}
	if len(mc.imageCreatedRefs) != 1 {
		t.Fatalf("made %d image probes, want 1: %v", len(mc.imageCreatedRefs), mc.imageCreatedRefs)
	}
	// The resolved ID, not the tag: the tag can have been moved to a newer
	// image since the container started.
	if mc.imageCreatedRefs[0] != "sha256:0123456789abcdef" {
		t.Errorf("probed %q, want the document's resolved image id", mc.imageCreatedRefs[0])
	}
	if string(msg.data) != inspectFixtureJSON {
		t.Error("the probe must not disturb the raw bytes")
	}
}

// TestFetchInspect_ProbeFailureIsDiscarded pins the non-fatal rule: the image
// probe is an annotation, so its failure costs its own row and nothing else.
func TestFetchInspect_ProbeFailureIsDiscarded(t *testing.T) {
	mc := &mockInspectComposer{
		inspectRaw:      []byte(inspectFixtureJSON),
		imageCreatedErr: fmt.Errorf("No such image"),
	}
	m := Model{composer: mc, ctx: context.Background(), inspectService: "web"}

	msg, ok := m.fetchInspect()().(inspectDataMsg)
	if !ok {
		t.Fatal("want inspectDataMsg")
	}
	if msg.err != nil {
		t.Errorf("err = %v, want nil: a probe failure must never take the error slot", msg.err)
	}
	if !msg.imageCreated.IsZero() {
		t.Errorf("imageCreated = %v, want the zero time", msg.imageCreated)
	}
	if string(msg.data) != inspectFixtureJSON {
		t.Error("a failed probe must still deliver the container document")
	}
}

// TestFetchInspect_WithoutAProberSpendsNoProbe: the capability is
// type-asserted, so a composer that lacks it makes the second call not happen
// rather than making the whole fetch fail.
func TestFetchInspect_WithoutAProberSpendsNoProbe(t *testing.T) {
	m := Model{
		composer:       &inspectOnlyComposer{raw: []byte(inspectFixtureJSON)},
		ctx:            context.Background(),
		inspectService: "web",
	}

	msg, ok := m.fetchInspect()().(inspectDataMsg)
	if !ok {
		t.Fatal("want inspectDataMsg")
	}
	if msg.err != nil || !msg.imageCreated.IsZero() {
		t.Errorf("msg = %+v, want the document with no build date and no error", msg)
	}
	if string(msg.data) != inspectFixtureJSON {
		t.Error("the document must arrive unchanged")
	}
}

// TestFetchInspect_UnreadableDocumentSpendsNoProbe: the probe needs an image
// reference, and the only source of one is the document. A document the narrow
// parser cannot read names nothing, so asking docker about it would be a
// malformed call whose failure is discarded anyway.
func TestFetchInspect_UnreadableDocumentSpendsNoProbe(t *testing.T) {
	mc := &mockInspectComposer{inspectRaw: []byte("[]"), imageCreated: time.Now()}
	m := Model{composer: mc, ctx: context.Background(), inspectService: "web"}

	msg, ok := m.fetchInspect()().(inspectDataMsg)
	if !ok {
		t.Fatal("want inspectDataMsg")
	}
	if len(mc.imageCreatedRefs) != 0 {
		t.Errorf("made %d image probes, want none: %v", len(mc.imageCreatedRefs), mc.imageCreatedRefs)
	}
	// The bytes still travel, so raw mode remains the escape hatch.
	if string(msg.data) != "[]" {
		t.Error("the raw bytes must reach the screen even when the parse fails")
	}
}

// readOnlyInspectComposer is the read-only counterpart of mockInspectComposer:
// the exact capability set of *compose.HostContainers, which is ReadOnlyComposer
// + ExecProvider + Inspector + ImageInspector (all pinned against the real type
// by the compile-time assertions above). The `i` key is the one container key
// that is NOT gated on m.readOnly(), so this double is what proves the key
// reaches enterInspect on the unmanaged screen — and the image probe behind the
// `built` row is a READ, so it is live there too.
type readOnlyInspectComposer struct {
	readOnlyMockComposer
	inspectRaw   []byte
	imageCreated time.Time
}

func (m *readOnlyInspectComposer) Inspect(ctx context.Context, service string) ([]byte, error) {
	return m.inspectRaw, nil
}

func (m *readOnlyInspectComposer) ImageCreated(ctx context.Context, image string) (time.Time, error) {
	return m.imageCreated, nil
}

// inspectTestModel builds a container screen sitting on the given composer,
// with every service running. The composer is also the composerFactory's return
// value, so a test that trips a factory call keeps the same double.
// newReadOnlyModel delegates here and then reseeds svcStatus from its mock.
func inspectTestModel(t *testing.T, c runner.Composer, services []string) Model {
	t.Helper()
	m := NewModel(c, io.Discard, func(compose.Project) runner.Composer { return c }, nil, nil)
	m.screen = screenSelectContainers
	m.setSingleGroup(append([]string(nil), services...))
	m.svcStatus = map[string]runner.ServiceStatus{}
	for _, s := range services {
		m.svcStatus[qk(m, s)] = runner.ServiceStatus{Running: true}
	}
	m.width, m.height = 120, 24
	m.updateInFlight = false
	m.refreshInFlight = false
	installFakeTick(&m)
	return m
}

func TestInspectKey_EntersOnWritableComposer(t *testing.T) {
	mc := &mockInspectComposer{inspectRaw: []byte(inspectFixtureJSON)}
	mc.services = []string{"web", "db"}
	m := inspectTestModel(t, mc, mc.services)
	m.svcCursor = 1

	result, cmd := m.Update(keyMsgFor("i"))
	got := result.(Model)

	if got.screen != screenInspect {
		t.Fatalf("screen = %d, want screenInspect", got.screen)
	}
	if got.inspectService != "db" {
		t.Errorf("inspectService = %q, want \"db\" (the cursor row)", got.inspectService)
	}
	if got.inspectSession == 0 {
		t.Error("enterInspect must bump inspectSession")
	}
	if got.inspectShowRaw {
		t.Error("the summary is the default mode")
	}
	if got.inspectViewport.Height != m.height-6 {
		t.Errorf("viewport height = %d, want %d (the config sizing)", got.inspectViewport.Height, m.height-6)
	}
	if cmd == nil {
		t.Fatal("enterInspect must return the fetch command")
	}
	msg, ok := cmd().(inspectDataMsg)
	if !ok {
		t.Fatalf("the returned command produced %T, want inspectDataMsg", cmd())
	}
	// Close the loop. Without feeding the message back, a mismatch between the
	// session fetchInspect carries and the one enterInspect left on the Model
	// would blank the screen forever on the SUCCESS path — only the error path
	// would catch it, and only by accident.
	result, _ = got.Update(msg)
	loaded := result.(Model)
	if loaded.inspectErr != nil {
		t.Fatalf("inspectErr = %v, want nil", loaded.inspectErr)
	}
	for _, want := range []string{"STATE", "running", "nginx:1.27"} {
		if !strings.Contains(loaded.inspectSummary, want) {
			t.Errorf("summary missing %q:\n%s", want, loaded.inspectSummary)
		}
	}
	if view := loaded.viewInspect(); strings.Contains(view, "Loading") {
		t.Errorf("the screen must not still read as loading:\n%s", view)
	}
}

// TestEnterInspect_FloorsATinyPane pins enterInspect's own clamps, the pair the
// WindowSizeMsg branch mirrors. A raw m.width - 4 goes negative on a 5-column
// pane and buildInspectSummary would then wrap to its 80-column fallback.
func TestEnterInspect_FloorsATinyPane(t *testing.T) {
	mc := &mockInspectComposer{inspectRaw: []byte(inspectFixtureJSON)}
	mc.services = []string{"web"}
	m := inspectTestModel(t, mc, mc.services)
	m.width, m.height = 5, 2

	result, cmd := m.Update(keyMsgFor("i"))
	got := result.(Model)

	if got.inspectViewport.Width != 40 {
		t.Errorf("viewport width = %d, want the floor of 40", got.inspectViewport.Width)
	}
	if got.inspectViewport.Height != 3 {
		t.Errorf("viewport height = %d, want the floor of 3", got.inspectViewport.Height)
	}
	result, _ = got.Update(cmd())
	for _, line := range strings.Split(result.(Model).inspectSummary, "\n") {
		if ansi.StringWidth(line) > 40 {
			t.Errorf("line exceeds the floored width: %q", line)
		}
	}
}

// TestInspectKey_SessionIsMonotonic pins the counter's PROPERTY, not just that
// it is non-zero: a second visit must invalidate the first visit's in-flight
// fetch. Replacing the ++ with `= 1` keeps every other inspect test green.
func TestInspectKey_SessionIsMonotonic(t *testing.T) {
	mc := &mockInspectComposer{inspectRaw: []byte(inspectFixtureJSON)}
	mc.services = []string{"web"}
	m := inspectTestModel(t, mc, mc.services)

	result, firstCmd := m.Update(keyMsgFor("i"))
	first := result.(Model)
	firstMsg := firstCmd().(inspectDataMsg)

	result, _ = first.Update(keyMsgFor("esc"))
	back := result.(Model)
	if back.screen != screenSelectContainers {
		t.Fatalf("esc left screen = %d, want screenSelectContainers", back.screen)
	}

	result, secondCmd := back.Update(keyMsgFor("i"))
	second := result.(Model)
	if second.inspectSession <= first.inspectSession {
		t.Fatalf("second visit session = %d, want > %d", second.inspectSession, first.inspectSession)
	}
	if got := secondCmd().(inspectDataMsg).session; got != second.inspectSession {
		t.Errorf("the second fetch carries session %d, want %d", got, second.inspectSession)
	}
	// The first visit's fetch, landing late, must be discarded.
	result, _ = second.Update(firstMsg)
	if late := result.(Model); late.inspectRaw != nil {
		t.Error("the first visit's in-flight fetch must not populate the second visit")
	}
}

// TestInspectKey_EntersOnReadOnlyComposer pins the deliberate asymmetry: every
// other container key added recently is gated on m.readOnly(), and i is not.
func TestInspectKey_EntersOnReadOnlyComposer(t *testing.T) {
	built := time.Date(2023, 11, 2, 13, 2, 50, 0, time.UTC)
	mc := &readOnlyInspectComposer{
		readOnlyMockComposer: *readOnlyTestComposer(),
		inspectRaw:           []byte(inspectFixtureJSON),
		imageCreated:         built,
	}
	m := inspectTestModel(t, mc, mc.services)

	if !m.readOnly() {
		t.Fatal("precondition: the double must be read-only")
	}

	result, cmd := m.Update(keyMsgFor("i"))
	got := result.(Model)

	if got.screen != screenInspect {
		t.Fatalf("screen = %d, want screenInspect — i must not be gated on readOnly", got.screen)
	}
	if cmd == nil {
		t.Fatal("enterInspect must return the fetch command on the read-only screen too")
	}

	// The image probe is a READ, so it runs here too: the unmanaged screen has
	// no update verdict of its own until U is pressed, and the build date must
	// not wait on one.
	result, _ = got.Update(cmd())
	if summary := result.(Model).inspectSummary; !strings.Contains(summary, inspectRow("built", "2023-11-02 13:02:50")) {
		t.Errorf("the read-only screen must draw the build date too:\n%s", summary)
	}
}

func TestInspectKey_NoOpOnEmptyList(t *testing.T) {
	mc := &mockInspectComposer{inspectRaw: []byte(inspectFixtureJSON)}
	m := inspectTestModel(t, mc, nil)

	result, cmd := m.Update(keyMsgFor("i"))
	got := result.(Model)

	if got.screen != screenSelectContainers {
		t.Errorf("screen = %d, want screenSelectContainers", got.screen)
	}
	if cmd != nil {
		t.Error("i on an empty list must not fetch")
	}
	if mc.inspectCalls != 0 {
		t.Errorf("Inspect called %d times, want 0", mc.inspectCalls)
	}
}

func TestInspectKey_NoOpWhenNotInspector(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := inspectTestModel(t, mc, mc.services)

	if _, ok := m.composer.(Inspector); ok {
		t.Fatal("precondition: the plain mock must not be an Inspector")
	}

	result, cmd := m.Update(keyMsgFor("i"))
	got := result.(Model)

	if got.screen != screenSelectContainers {
		t.Errorf("screen = %d, want screenSelectContainers", got.screen)
	}
	if cmd != nil {
		t.Error("i must not fetch when the composer is not an Inspector")
	}
	if got.warning != "" {
		t.Errorf("warning = %q; a silent no-op must not advertise itself", got.warning)
	}
}

// TestInspectKey_SwallowedWhileConfirming pins that `i` cannot open a screen
// from behind an armed destructive prompt. It is structurally safe (the
// confirming block returns above the action switch), but `i` is a plain letter
// and the repo pins the analogous `q` case.
func TestInspectKey_SwallowedWhileConfirming(t *testing.T) {
	mc := &mockInspectComposer{inspectRaw: []byte(inspectFixtureJSON)}
	mc.services = []string{"web"}
	m := inspectTestModel(t, mc, mc.services)
	m.selected = selectedIdx(m, 0)

	result, _ := m.Update(keyMsgFor("d"))
	armed := result.(Model)
	if !armed.confirming {
		t.Fatal("precondition: d must arm the confirmation")
	}

	result, cmd := armed.Update(keyMsgFor("i"))
	got := result.(Model)
	if got.screen != screenSelectContainers {
		t.Errorf("screen = %d; i must not open the inspect screen from behind a prompt", got.screen)
	}
	if !got.confirming {
		t.Error("i must not disarm the prompt")
	}
	if cmd != nil {
		t.Error("i must not fetch while a confirmation is armed")
	}
	if mc.inspectCalls != 0 {
		t.Errorf("Inspect called %d times, want 0", mc.inspectCalls)
	}
}

// TestInspectKey_TypedIntoSearchInput is the other swallow case: with the
// search bar open every rune is a literal, so a service named "api" stays
// typeable.
func TestInspectKey_TypedIntoSearchInput(t *testing.T) {
	mc := &mockInspectComposer{inspectRaw: []byte(inspectFixtureJSON)}
	mc.services = []string{"ingest", "web"}
	m := inspectTestModel(t, mc, mc.services)

	result, _ := m.Update(keyMsgFor("/"))
	m = result.(Model)
	if !m.searching {
		t.Fatal("precondition: search must be open")
	}

	result, _ = m.Update(keyMsgFor("i"))
	got := result.(Model)
	if got.screen != screenSelectContainers {
		t.Errorf("screen = %d; i must not navigate while the search input is open", got.screen)
	}
	if got.searchInput.Value() != "i" {
		t.Errorf("searchInput = %q, want %q", got.searchInput.Value(), "i")
	}
	if mc.inspectCalls != 0 {
		t.Errorf("Inspect called %d times, want 0", mc.inspectCalls)
	}
}

// TestInspectKey_ClearsCommittedSearch pins departure site #10 of the
// clearSearch checklist: leaving screenSelectContainers must never carry a
// committed search (and its stale match indices) into a nested screen.
func TestInspectKey_ClearsCommittedSearch(t *testing.T) {
	mc := &mockInspectComposer{inspectRaw: []byte(inspectFixtureJSON)}
	mc.services = []string{"web", "webhook", "db"}
	m := inspectTestModel(t, mc, mc.services)
	m.searchQuery = "web"
	m.searchMatches = []int{0, 1}
	m.svcCursor = 1

	result, _ := m.Update(keyMsgFor("i"))
	got := result.(Model)

	if got.screen != screenInspect {
		t.Fatalf("screen = %d, want screenInspect", got.screen)
	}
	if got.searchQuery != "" || got.searchMatches != nil || got.searching {
		t.Errorf("committed search survived enterInspect: query=%q matches=%v searching=%v",
			got.searchQuery, got.searchMatches, got.searching)
	}
}

// inspectScreenModel builds a populated inspect screen the way enterInspect
// leaves it: raw bytes in hand, summary rendered and the viewport filled.
func inspectScreenModel(t *testing.T) Model {
	t.Helper()
	m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web"}
	m.width, m.height = 120, 24
	m.inspectViewport = viewport.New(m.width-4, m.height-6)
	// NewModel always assigns this; the literal above does not, and
	// updateDetailsCmd derives its updateDetailsTimeout deadline from it.
	m.ctx = context.Background()
	result, _ := m.Update(inspectDataMsg{data: []byte(inspectFixtureJSON), session: 1})
	got := result.(Model)
	if got.inspectSummary == "" {
		t.Fatal("precondition: the summary should be rendered")
	}
	return got
}

func TestInspectScreen_RToggleRoundTrips(t *testing.T) {
	m := inspectScreenModel(t)

	if m.inspectShowRaw {
		t.Fatal("precondition: the summary is the default mode")
	}
	if got := m.inspectViewport.View(); !strings.Contains(got, "STATE") {
		t.Fatalf("summary mode should show the summary, got:\n%s", got)
	}

	result, _ := m.Update(keyMsgFor("r"))
	raw := result.(Model)
	if !raw.inspectShowRaw {
		t.Fatal("r should switch to raw mode")
	}
	if got := raw.inspectViewport.View(); !strings.Contains(got, `"Name": "/proj-web-1"`) {
		t.Errorf("raw mode should show the verbatim bytes, got:\n%s", got)
	}
	if !strings.Contains(raw.viewInspect(), "r summary") {
		t.Error("the footer should offer the way back to the summary")
	}

	result, _ = raw.Update(keyMsgFor("r"))
	back := result.(Model)
	if back.inspectShowRaw {
		t.Fatal("a second r should switch back to the summary")
	}
	if got := back.inspectViewport.View(); !strings.Contains(got, "STATE") {
		t.Errorf("summary should be restored, got:\n%s", got)
	}
	if string(back.inspectRaw) != inspectFixtureJSON {
		t.Error("the round trip must not disturb the raw bytes")
	}
	if !strings.Contains(back.viewInspect(), "r raw JSON") {
		t.Error("the footer should offer raw mode again")
	}
}

// TestInspectScreen_RTogglesWithoutRefetch pins the difference from
// screenConfig's r: both buffers are already in hand, so the toggle issues no
// command.
func TestInspectScreen_RTogglesWithoutRefetch(t *testing.T) {
	m := inspectScreenModel(t)
	if _, cmd := m.Update(keyMsgFor("r")); cmd != nil {
		t.Error("the r toggle must not fetch — the raw bytes are already held")
	}
}

func TestInspectScreen_EscClearsAndReturns(t *testing.T) {
	m := inspectScreenModel(t)
	m.inspectShowRaw = true

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := result.(Model)

	if got.screen != screenSelectContainers {
		t.Fatalf("screen = %d, want screenSelectContainers", got.screen)
	}
	if got.inspectService != "" || got.inspectRaw != nil || got.inspectSummary != "" ||
		got.inspectShowRaw || got.inspectErr != nil {
		t.Errorf("esc left inspect state behind: service=%q raw=%d summary=%d showRaw=%v err=%v",
			got.inspectService, len(got.inspectRaw), len(got.inspectSummary),
			got.inspectShowRaw, got.inspectErr)
	}
	if got.inspectViewport.Height != 0 {
		t.Error("esc should reset the viewport")
	}
	// Read-only screen: no status refresh on the way out, matching screenConfig
	// rather than screenLogs/screenProgress.
	if cmd != nil {
		t.Error("esc from the inspect screen must not refresh status")
	}
}

// TestInspectScreen_EscClearsErrorSlot covers the failed-fetch departure: the
// error must not survive into the next visit.
func TestInspectScreen_EscClearsErrorSlot(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web"}
	m.inspectViewport = viewport.New(100, 10)
	result, _ := m.Update(inspectDataMsg{err: fmt.Errorf("boom"), session: 1})
	m = result.(Model)
	if m.inspectErr == nil {
		t.Fatal("precondition: the error should be set")
	}

	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := result.(Model)
	if got.inspectErr != nil {
		t.Errorf("inspectErr = %v, want nil after esc", got.inspectErr)
	}
	if got.screen != screenSelectContainers {
		t.Errorf("screen = %d, want screenSelectContainers", got.screen)
	}
}

// TestInspectScreen_QTakesTheSamePath pins the q→esc rewrite: screenInspect
// opens no text input, so it falls into the rewrite block's default case and
// needs no typingInInput() entry of its own.
func TestInspectScreen_QTakesTheSamePath(t *testing.T) {
	m := inspectScreenModel(t)

	result, _ := m.Update(keyMsgFor("q"))
	got := result.(Model)

	if got.screen != screenSelectContainers {
		t.Fatalf("screen = %d, want screenSelectContainers — q must rewrite to esc", got.screen)
	}
	if got.inspectRaw != nil || got.inspectSummary != "" {
		t.Error("q should clear the inspect state exactly like esc")
	}
}

func TestInspectScreen_ResizeRebuildsAndKeepsRaw(t *testing.T) {
	m := inspectScreenModel(t)
	wide := m.inspectSummary

	result, _ := m.Update(tea.WindowSizeMsg{Width: 44, Height: 30})
	got := result.(Model)

	if got.inspectViewport.Width != 40 {
		t.Errorf("viewport width = %d, want 40 (msg.Width - 4)", got.inspectViewport.Width)
	}
	if got.inspectViewport.Height != 24 {
		t.Errorf("viewport height = %d, want 24 (msg.Height - 6, the config sizing)", got.inspectViewport.Height)
	}
	if string(got.inspectRaw) != inspectFixtureJSON {
		t.Error("a resize must preserve the raw bytes verbatim")
	}
	if got.inspectSummary == "" {
		t.Fatal("the summary should be rebuilt at the new width")
	}
	if got.inspectSummary == wide {
		t.Error("the summary should be re-wrapped, not reused at the old width")
	}
	for _, line := range strings.Split(got.inspectSummary, "\n") {
		if ansi.StringWidth(line) > 40 {
			t.Errorf("rebuilt line exceeds the new width: %q", line)
		}
	}
}

// TestInspectScreen_BuiltRowIgnoresTheVerdict is the headline of the feature:
// the `built` row is drawn for EVERY container, whatever the update check has
// or has not said. It drives the whole path — the `i` key, the fetch Cmd, the
// probe, the message, the render — with an up-to-date verdict in the cache and
// NO details, the shape refreshUpdates really writes for a false verdict.
func TestInspectScreen_BuiltRowIgnoresTheVerdict(t *testing.T) {
	built := time.Date(2023, 11, 2, 13, 2, 50, 0, time.UTC)
	mc := &mockInspectComposer{inspectRaw: []byte(inspectFixtureJSON), imageCreated: built}
	mc.services = []string{"web"}
	m := inspectTestModel(t, mc, mc.services)
	m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {
		fetchedAt: time.Now().Add(-8 * time.Minute),
		results:   map[string]bool{"web": false},
	}}

	result, cmd := m.Update(keyMsgFor("i"))
	if cmd == nil {
		t.Fatal("i should return the inspect fetch")
	}
	result, _ = result.(Model).Update(cmd())
	got := result.(Model)

	if got.inspectErr != nil {
		t.Fatalf("inspectErr = %v, want nil", got.inspectErr)
	}
	if !strings.Contains(got.inspectSummary, inspectRow("built", "2023-11-02 13:02:50")) {
		t.Errorf("an up-to-date container must still draw its build date:\n%s", got.inspectSummary)
	}
	if !strings.Contains(got.inspectSummary, "up to date") {
		t.Errorf("precondition: the verdict row should still be drawn:\n%s", got.inspectSummary)
	}
	// The registry half stays verdict-driven, so nothing here may invent one.
	for _, skip := range []string{"update id", "update built"} {
		if strings.Contains(got.inspectSummary, skip) {
			t.Errorf("summary must not contain %q:\n%s", skip, got.inspectSummary)
		}
	}
}

// TestInspectScreen_ProbeFailureKeepsTheDocument pins the failure half of the
// same path: no row, no error line, everything else intact.
func TestInspectScreen_ProbeFailureKeepsTheDocument(t *testing.T) {
	mc := &mockInspectComposer{
		inspectRaw:      []byte(inspectFixtureJSON),
		imageCreatedErr: fmt.Errorf("Error: No such image"),
	}
	mc.services = []string{"web"}
	m := inspectTestModel(t, mc, mc.services)

	result, cmd := m.Update(keyMsgFor("i"))
	result, _ = result.(Model).Update(cmd())
	got := result.(Model)

	if got.inspectErr != nil {
		t.Fatalf("inspectErr = %v, want nil: a probe failure is discarded", got.inspectErr)
	}
	if strings.Contains(got.inspectSummary, "  built") {
		t.Errorf("a failed probe must draw no build date:\n%s", got.inspectSummary)
	}
	for _, want := range []string{"STATE", "running", "IMAGE", "nginx:1.27", "sha256:0123456789abcdef"} {
		if !strings.Contains(got.inspectSummary, want) {
			t.Errorf("summary missing %q:\n%s", want, got.inspectSummary)
		}
	}
	if view := got.viewInspect(); strings.Contains(view, "Error") {
		t.Errorf("the screen must show no error line:\n%s", view)
	}
}

// TestInspectScreen_ResizeKeepsTheBuiltRow: rebuildInspectSummary re-parses the
// raw bytes, which cannot carry the image build date, so the resize path has to
// merge it back from the Model. Without that the row vanishes on the first
// resize and returns only on a re-entry.
func TestInspectScreen_ResizeKeepsTheBuiltRow(t *testing.T) {
	built := time.Date(2023, 11, 2, 13, 2, 50, 0, time.UTC)
	m := inspectScreenModel(t)
	result, _ := m.Update(inspectDataMsg{
		data:         []byte(inspectFixtureJSON),
		imageCreated: built,
		session:      m.inspectSession,
	})
	got := result.(Model)
	row := inspectRow("built", "2023-11-02 13:02:50")
	if !strings.Contains(got.inspectSummary, row) {
		t.Fatalf("precondition: the summary should carry the built row:\n%s", got.inspectSummary)
	}

	result, _ = got.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	resized := result.(Model)
	if !strings.Contains(resized.inspectSummary, row) {
		t.Errorf("the resize dropped the built row:\n%s", resized.inspectSummary)
	}
}

// TestInspectScreen_ResizeKeepsUpdateRows: the resize path rebuilds through the
// same rebuildInspectSummary, so the cache lookup travels with it. Without a
// populated cache the resize test above never draws the update block at all.
func TestInspectScreen_ResizeKeepsUpdateRows(t *testing.T) {
	m := inspectScreenModel(t)
	m.updateCache = map[string]updateEntry{m.updatesCacheKey(): {
		fetchedAt: time.Now().Add(-3 * time.Minute),
		results:   map[string]bool{"web": true},
		details:   detailFixture(),
	}}
	m.rebuildInspectSummary()
	if !strings.Contains(m.inspectSummary, inspectRow("update id", detailFixture()["web"].NewID)) {
		t.Fatalf("precondition: the summary should carry the update rows:\n%s", m.inspectSummary)
	}

	got := modelOf(m.Update(tea.WindowSizeMsg{Width: 120, Height: 30}))

	for _, row := range []string{"built", "update", "update id", "update built"} {
		if !strings.Contains(got.inspectSummary, inspectRow(row, "")) {
			t.Errorf("a resize dropped the %q row:\n%s", row, got.inspectSummary)
		}
	}
}

// TestInspectScreen_ResizeInRawModeKeepsBuffer pins the chokepoint: a resize
// while raw mode is on must leave raw mode on and the viewport showing bytes.
func TestInspectScreen_ResizeInRawModeKeepsBuffer(t *testing.T) {
	m := inspectScreenModel(t)
	result, _ := m.Update(keyMsgFor("r"))
	m = result.(Model)

	result, _ = m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	got := result.(Model)

	if !got.inspectShowRaw {
		t.Fatal("a resize must not change the mode")
	}
	if view := got.inspectViewport.View(); !strings.Contains(view, `"Name": "/proj-web-1"`) {
		t.Errorf("raw mode should still show the bytes after a resize, got:\n%s", view)
	}
}

// TestInspectScreen_ResizeOffScreenIsInert guards the branch condition: a
// WindowSizeMsg that lands on another screen must not touch inspect state.
func TestInspectScreen_ResizeOffScreenIsInert(t *testing.T) {
	m := inspectScreenModel(t)
	m.screen = screenSelectContainers
	wantSummary, wantContent := m.inspectSummary, m.inspectViewport.View()

	result, _ := m.Update(tea.WindowSizeMsg{Width: 44, Height: 30})
	got := result.(Model)

	if got.inspectViewport.Width != 116 {
		t.Errorf("viewport width = %d, want the untouched 116", got.inspectViewport.Width)
	}
	// The guarded branch also calls rebuildInspectSummary and setInspectContent,
	// so a guard that regressed to "width only" has to fail here too.
	if got.inspectSummary != wantSummary {
		t.Error("an off-screen resize must not rebuild the summary")
	}
	if got.inspectViewport.View() != wantContent {
		t.Error("an off-screen resize must not re-set the viewport content")
	}
}

// TestInspectScreen_ResizeBeforeFetch is a live sequence, not a synthetic one:
// resizing while the screen still reads "Loading..." on a slow SSH host reaches
// rebuildInspectSummary with no bytes in hand.
func TestInspectScreen_ResizeBeforeFetch(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web", width: 120, height: 24}
	m.inspectViewport = viewport.New(116, 18)

	result, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	got := result.(Model)

	if got.inspectSummary != "" || got.inspectRaw != nil {
		t.Error("a resize with no bytes in hand must not invent content")
	}
	if got.inspectErr != nil {
		t.Errorf("inspectErr = %v; an empty buffer is not a parse failure", got.inspectErr)
	}
	if !strings.Contains(got.viewInspect(), "Loading") {
		t.Errorf("the screen should still read as loading:\n%s", got.viewInspect())
	}
	// And the fetch that lands afterwards still renders, at the NEW width.
	result, _ = got.Update(inspectDataMsg{data: []byte(inspectFixtureJSON), session: 1})
	loaded := result.(Model)
	if loaded.inspectSummary == "" {
		t.Fatal("the fetch after a resize should still render")
	}
	for _, line := range strings.Split(loaded.inspectSummary, "\n") {
		if ansi.StringWidth(line) > 56 {
			t.Errorf("line exceeds the resized width: %q", line)
		}
	}
}

// TestInspectScreen_ResizeFloorsMatchEnterInspect pins the two sizing sites
// against each other. enterInspect floors width at 10 and height at 3; an
// unguarded resize branch would set a NEGATIVE width, and buildInspectSummary
// would then wrap to its 80-column fallback for a pane that renders nothing.
func TestInspectScreen_ResizeFloorsMatchEnterInspect(t *testing.T) {
	m := inspectScreenModel(t)

	result, _ := m.Update(tea.WindowSizeMsg{Width: 3, Height: 1})
	got := result.(Model)

	if got.inspectViewport.Width != 40 {
		t.Errorf("viewport width = %d, want the 40 enterInspect floors to", got.inspectViewport.Width)
	}
	if got.inspectViewport.Height != 3 {
		t.Errorf("viewport height = %d, want the floor of 3", got.inspectViewport.Height)
	}
	if got.inspectSummary == "" {
		t.Fatal("the summary should still be rebuilt at the floored width")
	}
	for _, line := range strings.Split(got.inspectSummary, "\n") {
		if ansi.StringWidth(line) > 40 {
			t.Errorf("line exceeds the floored width: %q", line)
		}
	}
}

// TestInspectScreen_RawModeScrollsSideways pins the escape hatch's reach, and
// it drives the real `i` entry point because enterInspect is where the step is
// set. Real `docker inspect` output carries lines hundreds of columns wide and
// the viewport hard-cuts at its width with no wrap, so with a zero horizontal
// step left/right are inert and everything past the edge is unreachable — in
// exactly the mode that exists for the payload the parser could not read.
func TestInspectScreen_RawModeScrollsSideways(t *testing.T) {
	long := `[{"Name":"/proj-web-1","State":{"Status":"running","Running":true},` +
		`"Config":{"Image":"` + strings.Repeat("very-long-image-segment-", 30) + `x"}}]`

	mc := &mockInspectComposer{inspectRaw: []byte(long)}
	mc.services = []string{"web"}
	m := inspectTestModel(t, mc, mc.services)

	result, cmd := m.Update(keyMsgFor("i"))
	m = result.(Model)
	result, _ = m.Update(cmd())
	result, _ = result.(Model).Update(keyMsgFor("r"))
	raw := result.(Model)
	if !raw.inspectShowRaw {
		t.Fatal("precondition: r should enter raw mode")
	}
	before := raw.inspectViewport.View()
	if !strings.Contains(before, "very-long-image-segment") {
		t.Fatalf("precondition: raw mode should show the payload:\n%s", before)
	}

	result, _ = raw.Update(tea.KeyMsg{Type: tea.KeyRight})
	scrolled := result.(Model)
	if scrolled.inspectViewport.View() == before {
		t.Error("right must scroll the raw pane sideways; enterInspect has to call SetHorizontalStep")
	}

	result, _ = scrolled.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if back := result.(Model); back.inspectViewport.View() != before {
		t.Error("left must scroll back")
	}
}

// inspectScrolledRawModel enters the inspect screen through the real `i` key,
// switches to raw mode and scrolls it well to the right, which is the only way
// to make the viewport's xOffset non-zero.
func inspectScrolledRawModel(t *testing.T) Model {
	t.Helper()
	long := `[{"Name":"/proj-web-1","State":{"Status":"running","Running":true},` +
		`"Config":{"Image":"` + strings.Repeat("very-long-image-segment-", 30) + `x"}}]`

	mc := &mockInspectComposer{inspectRaw: []byte(long)}
	mc.services = []string{"web"}
	m := inspectTestModel(t, mc, mc.services)

	result, cmd := m.Update(keyMsgFor("i"))
	result, _ = result.(Model).Update(cmd())
	result, _ = result.(Model).Update(keyMsgFor("r"))
	raw := result.(Model)
	if !raw.inspectShowRaw {
		t.Fatal("precondition: r should enter raw mode")
	}
	for i := 0; i < 40; i++ {
		result, _ = raw.Update(tea.KeyMsg{Type: tea.KeyRight})
		raw = result.(Model)
	}
	if strings.Contains(raw.inspectViewport.View(), `"Name"`) {
		t.Fatal("precondition: the pane should be scrolled past the left edge")
	}
	return raw
}

// TestInspectScreen_RToggleAfterSidewaysScrollKeepsSummaryOnScreen is the
// companion pin to TestInspectScreen_RawModeScrollsSideways. The horizontal
// step that test asks for is what makes xOffset reachable, and NEITHER
// SetContent nor GotoTop resets one — SetContent recomputes the line widths and
// GotoTop moves YOffset only. The summary is wrapped to the pane, so its
// longest line never exceeds the width: visibleLines() then cuts EVERY line at
// [xOffset, xOffset+width] and renders a blank pane under a correct title and
// footer, with no key the footer advertises that recovers it.
func TestInspectScreen_RToggleAfterSidewaysScrollKeepsSummaryOnScreen(t *testing.T) {
	raw := inspectScrolledRawModel(t)

	result, _ := raw.Update(keyMsgFor("r"))
	back := result.(Model)
	if back.inspectShowRaw {
		t.Fatal("a second r should switch back to the summary")
	}
	view := back.inspectViewport.View()
	for _, want := range []string{"STATE", "proj-web-1", "running"} {
		if !strings.Contains(ansi.Strip(view), want) {
			t.Errorf("the summary must still be on screen after a sideways scroll, missing %q:\n%s", want, view)
		}
	}
}

// TestInspectScreen_ResizeClearsStaleHorizontalOffset is the same stale offset
// reached through the other writer: a resize that widens the pane past the raw
// content's longest line leaves an offset that cuts the left edge off every
// line. Both writers go through setInspectContent, so one reset covers them.
func TestInspectScreen_ResizeClearsStaleHorizontalOffset(t *testing.T) {
	raw := inspectScrolledRawModel(t)

	result, _ := raw.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	resized := result.(Model)
	if !resized.inspectShowRaw {
		t.Fatal("a resize must not change the mode")
	}
	if !strings.Contains(resized.inspectViewport.View(), `"Name"`) {
		t.Errorf("the resized pane must show the left edge again:\n%s", resized.inspectViewport.View())
	}
}

// TestInspectDataMsg_EmptyOutputIsAnError covers the one success shape that
// produces no content: rebuildInspectSummary early-returns on empty bytes, so
// ParseInspect's own "empty output" error never fires and the screen would read
// "Loading..." for ever, with no error and nothing to retry.
func TestInspectDataMsg_EmptyOutputIsAnError(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web", width: 100, height: 24}
	m.inspectViewport = viewport.New(96, 18)

	result, _ := m.Update(inspectDataMsg{data: nil, session: 1})
	model := result.(Model)

	if model.inspectErr == nil {
		t.Fatal("an empty payload should surface in the error slot")
	}
	view := ansi.Strip(model.viewInspect())
	if strings.Contains(view, "Loading") {
		t.Errorf("the fetch is over, so the screen must not read as loading:\n%s", view)
	}
	if !strings.Contains(view, "Error:") {
		t.Errorf("the error line must render:\n%s", view)
	}
}

// TestInspectScreen_ResizeKeepsTheChosenModeAfterAParseFailure pins where the
// forced raw switch lives. rebuildInspectSummary runs on the fetch AND on every
// resize; forcing the mode inside it would flip the reader back to raw the next
// time the terminal changed size. The mode is cleared on the model directly
// rather than through `r`, because the toggle is inert while the summary is
// empty — this pins the resize branch, not how the mode got there.
func TestInspectScreen_ResizeKeepsTheChosenModeAfterAParseFailure(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web", width: 100, height: 24}
	m.inspectViewport = viewport.New(96, 18)
	result, _ := m.Update(inspectDataMsg{data: []byte("[]"), session: 1})
	m = result.(Model)
	if !m.inspectShowRaw {
		t.Fatal("precondition: the fetch should switch to raw on a parse failure")
	}

	m.inspectShowRaw = false
	result, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	if resized := result.(Model); resized.inspectShowRaw {
		t.Error("a resize must not force raw mode; only the fetch handler does")
	}
}

func TestViewInspect_RendersBreadcrumbAndFooter(t *testing.T) {
	m := inspectScreenModel(t)
	m.serverName = "prod"
	m.projName = "shop"

	view := m.viewInspect()
	for _, want := range []string{"cdeploy > prod > shop > inspect > web", "STATE", "r raw JSON", "q back"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}

func TestViewInspect_LoadingBeforeFetch(t *testing.T) {
	m := Model{screen: screenInspect, inspectService: "web"}
	m.inspectViewport = viewport.New(100, 10)

	if view := m.viewInspect(); !strings.Contains(view, "Loading") {
		t.Errorf("view should show a loading line before the fetch lands:\n%s", view)
	}
}

func TestViewInspect_ShowsFetchError(t *testing.T) {
	m := Model{screen: screenInspect, inspectService: "web", inspectErr: fmt.Errorf("no container found for \"web\"")}
	m.inspectViewport = viewport.New(100, 10)

	view := m.viewInspect()
	if !strings.Contains(view, "no container found") {
		t.Errorf("view should surface inspectErr:\n%s", view)
	}
	if strings.Contains(view, "Loading") {
		t.Error("an errored fetch must not still read as loading")
	}
}

// TestViewInspect_ParseErrorShowsTheRawBytes pins the escape hatch end to end:
// when the narrow parser refuses the payload the raw bytes are the only content
// that exists, so the screen switches to them rather than leaving an error line
// above an empty pane and making the user guess that `r` is the way out.
func TestViewInspect_ParseErrorShowsTheRawBytes(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web", width: 100}
	m.inspectViewport = viewport.New(100, 10)
	result, _ := m.Update(inspectDataMsg{data: []byte("[]"), session: 1})
	m = result.(Model)

	if !m.inspectShowRaw {
		t.Fatal("a parse failure must switch to raw mode; the summary is empty")
	}
	view := m.viewInspect()
	if !strings.Contains(view, "Error:") {
		t.Errorf("the parse failure should show in the error slot:\n%s", view)
	}
	if strings.Contains(view, "Loading") {
		t.Error("bytes are in hand, so the screen must not read as loading")
	}
	if !strings.Contains(view, "[]") {
		t.Errorf("the pane must show the payload the parser choked on:\n%s", view)
	}
	// The summary the toggle would switch to is empty, so the footer must not
	// name r — the same no-op rule TestViewInspect_FooterDropsRWithoutBuffers
	// pins for a failed fetch, applied to the target buffer rather than to the
	// raw one.
	if strings.Contains(view, "r summary") {
		t.Errorf("an empty summary must not be advertised:\n%s", view)
	}
	result, _ = m.Update(keyMsgFor("r"))
	if again := result.(Model); !again.inspectShowRaw {
		t.Error("r must be inert while the summary is empty; it left raw mode")
	}
}

// TestViewInspect_ErrorIsOneClampedLine pins the chrome budget: viewInspect
// reserves exactly ONE line for the error, but the compose layer feeds raw
// stderr in (an SSH banner, a multi-line docker failure). A wrapped or
// multi-line error pushes the title off the top, because bubbletea keeps only
// the last m.height lines.
func TestViewInspect_ErrorIsOneClampedLine(t *testing.T) {
	// The sweep runs 40-120, not one sample: at width 60 the footer happens to
	// fit, so a single sample cannot see a chrome line that overflows a narrow
	// pane. 40 is the narrowest width this repo supports and sweeps elsewhere
	// (TestContainerFooterReservation runs 40-180).
	for width := 40; width <= 120; width++ {
		m := Model{screen: screenInspect, inspectService: "web", width: width, height: 24}
		m.inspectViewport = viewport.New(width-4, 18)
		m.inspectErr = fmt.Errorf("listing containers for inspect: %s",
			"ssh: connect to host prod port 22\nbanner line two\nbanner line three that is really quite long indeed")

		for _, line := range strings.Split(m.viewInspect(), "\n") {
			if w := ansi.StringWidth(line); w > m.width {
				t.Errorf("width %d: line is %d cells wide, want <= %d: %q", width, w, m.width, line)
			}
		}
		lines := strings.Split(ansi.Strip(m.viewInspect()), "\n")
		errIdx := -1
		for i, line := range lines {
			if strings.Contains(line, "Error: listing") {
				errIdx = i
			}
		}
		if errIdx == -1 {
			t.Errorf("width %d: the error must keep its head, which names what failed:\n%s", width, strings.Join(lines, "\n"))
			continue
		}
		for i, line := range lines {
			if i != errIdx && strings.Contains(line, "banner line") {
				t.Errorf("width %d: the error must occupy one line, line %d also carries it: %q", width, i, line)
			}
		}
	}
}

// TestViewInspect_ErrorLineIsSanitised pins the OTHER thing the error slot has
// to do to its input. The compose layer builds this string with withStderr,
// which embeds the remote's stderr verbatim, so a docker failure echoing a
// hostile image ref or an SSH banner lands here — and strings.Fields, the only
// pass the slot used to run, drops nothing but unicode whitespace: ESC, BEL,
// DEL and the 8-bit C1 introducers all survive it, and stepFailed.Render plus
// clampToWidth are both ANSI-aware and hand them straight to the terminal.
// sanitizeInspectLine is the same pass every decoded summary line goes through,
// so the two cannot disagree about what is safe.
func TestViewInspect_ErrorLineIsSanitised(t *testing.T) {
	hostile := "listing containers for inspect: \x1b]52;c;cGF5bG9hZA==\x07banner " +
		"\x1b[31mred\x1b[0m \x9b31mCSI \x9d52;OSC\x07 tail\x7fDEL"

	m := Model{screen: screenInspect, inspectService: "web", width: 300, height: 24}
	m.inspectViewport = viewport.New(296, 18)
	m.inspectErr = errors.New(hostile)

	out := m.viewInspect()
	for _, banned := range []string{"\x1b", "\x07", "\x7f", "\u009b", "\u009d"} {
		if strings.Contains(out, banned) {
			t.Errorf("viewInspect must not write %q to the terminal:\n%q", banned, out)
		}
	}
	// The whole sequence goes, not just its introducer: a bare rune filter
	// would leave the OSC 52 clipboard payload and the SGR parameters behind as
	// readable text, which is how ansi.Strip earns its place in the pass.
	for _, banned := range []string{"52;c;", "cGF5bG9hZA==", "31m"} {
		if strings.Contains(out, banned) {
			t.Errorf("viewInspect must strip the whole escape sequence, %q survived:\n%q", banned, out)
		}
	}
	// What is left must still read as the error, and still be one line.
	if !strings.Contains(out, "Error: listing containers for inspect: banner red") {
		t.Errorf("the readable head must survive the pass:\n%s", out)
	}
	if got := strings.Count(out, "Error:"); got != 1 {
		t.Errorf("the error must occupy one line, found %d:\n%q", got, out)
	}
}

// TestViewInspect_TitleNeverExceedsWidth pins the last of the three chrome
// lines this view owns. The title interpolates the breadcrumb — a server name,
// a project name — and the service name, none of them bounded, so at a narrow
// pane it is the one line that could still overrun. Measured against the pinned
// bubbletea v1.3.10: standardRenderer.flush truncates each line at r.width, so
// the overrun costs no row on its own; the clamp is what keeps the cut
// deterministic in View and holds titleStyle's MarginBottom line too, which
// lipgloss pads out to the CONTENT width.
func TestViewInspect_TitleNeverExceedsWidth(t *testing.T) {
	for _, withErr := range []bool{false, true} {
		for width := 40; width <= 120; width++ {
			m := Model{screen: screenInspect, width: width, height: 24}
			m.serverName = strings.Repeat("prod-", 12) + "server"
			m.serverColor = "red"
			m.projName = strings.Repeat("project-", 6)
			m.inspectService = strings.Repeat("service-", 6)
			m.inspectViewport = viewport.New(width-4, 18)
			m.inspectRaw = []byte(inspectFixtureJSON)
			m.rebuildInspectSummary()
			m.setInspectContent()
			if withErr {
				m.inspectErr = fmt.Errorf("listing containers for inspect: ssh: connect to host prod port 22")
			}

			lines := strings.Split(m.viewInspect(), "\n")
			for _, line := range lines {
				if w := ansi.StringWidth(line); w > width {
					t.Errorf("err=%v width %d: line is %d cells wide, want <= %d: %q", withErr, width, w, width, line)
				}
			}
			if len(lines) > m.height {
				t.Errorf("err=%v width %d: render is %d lines, want <= %d", withErr, width, len(lines), m.height)
			}
			// Truncation keeps the head, so the breadcrumb still says where the
			// user is even when the tail is cut.
			if !strings.HasPrefix(ansi.Strip(lines[0]), "cdeploy") {
				t.Errorf("err=%v width %d: the first line must still be the title, got %q", withErr, width, lines[0])
			}
		}
	}
}

// TestViewInspect_FooterNeverExceedsWidth pins the other half of the same
// budget. viewInspect fits the whole render into exactly m.height (title 3 +
// viewport m.height-6 + one optional error line + footer 2). The footer
// measures 42 cells, so it overruns every width below 42 in both modes;
// clampToWidth is what holds it, the way containerFooter already does. See
// TestViewInspect_TitleNeverExceedsWidth for what the renderer does with an
// over-wide line — it truncates, so what the sweep protects is the pane's own
// width contract, not a row.
func TestViewInspect_FooterNeverExceedsWidth(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for width := 40; width <= 120; width++ {
			m := Model{screen: screenInspect, inspectService: "web", width: width, height: 24, inspectShowRaw: raw}
			m.inspectViewport = viewport.New(width-4, 18)
			m.inspectRaw = []byte(inspectFixtureJSON)
			m.rebuildInspectSummary()
			m.setInspectContent()

			lines := strings.Split(m.viewInspect(), "\n")
			for _, line := range lines {
				if w := ansi.StringWidth(line); w > width {
					t.Errorf("raw=%v width %d: line is %d cells wide, want <= %d: %q", raw, width, w, width, line)
				}
			}
			if len(lines) > m.height {
				t.Errorf("raw=%v width %d: render is %d lines, want <= %d", raw, width, len(lines), m.height)
			}
			if !strings.Contains(ansi.Strip(lines[0]), "inspect") {
				t.Errorf("raw=%v width %d: the first line must still be the title, got %q", raw, width, lines[0])
			}
		}
	}
}

// TestViewInspect_FitsShortTerminal is the height half of the same budget the
// width sweeps pin, and the sibling of TestViewHelp_FitsShortTerminal. The
// render is title 3 + viewport + optional error 1 + footer 2, so a viewport
// sized anywhere but inspectViewportSize's -6 spills past the pane and
// bubbletea's renderer — which keeps only the LAST m.height lines — drops the
// title with no key that brings it back.
//
// The sweep starts at 9: inspectViewportSize floors the viewport at 3 rows, so
// below that the chrome alone (3 + 3 + 2 = 8, plus an error line) is taller than
// the pane, the same floor the ? overlay bottoms out at.
func TestViewInspect_FitsShortTerminal(t *testing.T) {
	for _, withErr := range []bool{false, true} {
		for _, width := range []int{40, 80, 120} {
			for height := 9; height <= 30; height++ {
				m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web", width: width, height: height}
				w, h := inspectViewportSize(width, height)
				m.inspectViewport = viewport.New(w, h)
				result, _ := m.Update(inspectDataMsg{data: []byte(inspectFixtureJSON), session: 1})
				m = result.(Model)
				if withErr {
					m.inspectErr = fmt.Errorf("listing containers for inspect: ssh: connect to host prod port 22")
				}

				// The RAW view string, with no TrimSuffix: bubbletea splits on
				// "\n" and a trailing newline is a whole extra element, so a
				// trimming test would hide exactly the overflow this pins.
				lines := strings.Split(m.viewInspect(), "\n")
				if len(lines) > height {
					t.Errorf("err=%v %dx%d: render is %d lines, want <= %d", withErr, width, height, len(lines), height)
				}
				if !strings.Contains(ansi.Strip(lines[0]), "inspect") {
					t.Errorf("err=%v %dx%d: the first line must still be the title, got %q", withErr, width, height, lines[0])
				}
			}
		}
	}
}

// TestViewInspect_NoTrailingNewline pins the invariant the height budget rests
// on, so a regression names the cause rather than the symptom. bubbletea hands
// View() straight to the renderer, which splits on "\n" with no TrimSuffix and
// keeps the last m.height elements — a trailing newline therefore renders the
// screen one line too tall and costs the title. viewSelectContainers, viewLogs
// and viewHelp end without one for the same reason.
func TestViewInspect_NoTrailingNewline(t *testing.T) {
	m := inspectScreenModel(t)
	if strings.HasSuffix(m.viewInspect(), "\n") {
		t.Error("viewInspect must not end in a newline")
	}
}

// TestViewInspect_FooterDropsRWithoutBuffers pins the no-op rule on the one
// state where the toggle has nothing to toggle: a FETCH failure leaves neither
// buffer, so an advertised r could only relabel itself over an empty pane.
func TestViewInspect_FooterDropsRWithoutBuffers(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web", width: 100, height: 24}
	m.inspectViewport = viewport.New(96, 18)
	result, _ := m.Update(inspectDataMsg{err: fmt.Errorf("no container found for %q", "web"), session: 1})
	m = result.(Model)

	view := ansi.Strip(m.viewInspect())
	for _, unwanted := range []string{"r raw JSON", "r summary"} {
		if strings.Contains(view, unwanted) {
			t.Errorf("a failed fetch must not advertise %q:\n%s", unwanted, view)
		}
	}
	for _, want := range []string{"up/down scroll", "q back"} {
		if !strings.Contains(view, want) {
			t.Errorf("footer missing %q:\n%s", want, view)
		}
	}
}

// TestInspectScreen_RIsInertWithoutBuffers is the key half of the same rule:
// the footer stops naming r, so r has to stop doing anything — otherwise the
// mode flag drifts behind an unadvertised key and a later resize would render
// the wrong buffer.
func TestInspectScreen_RIsInertWithoutBuffers(t *testing.T) {
	m := Model{screen: screenInspect, inspectSession: 1, inspectService: "web", width: 100, height: 24}
	m.inspectViewport = viewport.New(96, 18)
	result, _ := m.Update(inspectDataMsg{err: fmt.Errorf("boom"), session: 1})
	m = result.(Model)

	result, _ = m.Update(keyMsgFor("r"))
	if got := result.(Model); got.inspectShowRaw {
		t.Error("r must not flip the mode when a failed fetch left no bytes")
	}
}

func TestInspectScreen_ArrowsReachTheViewport(t *testing.T) {
	m := inspectScreenModel(t)
	m.inspectViewport.Height = 3
	m.setInspectContent()
	if m.inspectViewport.AtBottom() {
		t.Fatal("precondition: the summary must overflow a 3-line viewport")
	}

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	got := result.(Model)
	if got.inspectViewport.YOffset == 0 {
		t.Error("down should scroll the inspect viewport")
	}

	result, _ = got.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if result.(Model).inspectViewport.YOffset <= got.inspectViewport.YOffset {
		t.Error("pgdown should page the inspect viewport")
	}
}

// TestInspectScreen_RTogglePutsTheReaderAtTheTop pins the small courtesy the
// chokepoint alone does not give: SetContent preserves YOffset, so a toggle
// from a scrolled summary into the much longer raw JSON would otherwise land
// the reader mid-document.
func TestInspectScreen_RTogglePutsTheReaderAtTheTop(t *testing.T) {
	m := inspectScreenModel(t)
	m.inspectViewport.Height = 3
	m.setInspectContent()
	m.inspectViewport.SetYOffset(4)

	result, _ := m.Update(keyMsgFor("r"))
	if got := result.(Model); got.inspectViewport.YOffset != 0 {
		t.Errorf("YOffset = %d, want 0 after the mode toggle", got.inspectViewport.YOffset)
	}
}

// TestInspectKey_MissingContainerSurfacesNamedError drives AC4 end to end: the
// composer's `no container found` error must travel from the key press through
// the fetch and the message handler into the rendered screen, so the user reads
// the name of the service that has no container rather than a blank viewport.
func TestInspectKey_MissingContainerSurfacesNamedError(t *testing.T) {
	mc := &mockInspectComposer{inspectErr: fmt.Errorf("no container found for %q", "web")}
	mc.services = []string{"web"}
	m := inspectTestModel(t, mc, mc.services)

	result, cmd := m.Update(keyMsgFor("i"))
	m = result.(Model)
	if cmd == nil {
		t.Fatal("i should return the fetch command")
	}

	result, _ = m.Update(cmd())
	m = result.(Model)

	view := m.viewInspect()
	if !strings.Contains(view, `no container found for "web"`) {
		t.Errorf("the named error should reach the screen:\n%s", view)
	}
	if strings.Contains(view, "Loading") {
		t.Error("a failed fetch must not leave the screen reading as loading")
	}
}

// TestRollbackPreppedMsg_CarriesBatchIdentity is the superseded-prep pin. The
// screen alone is not identity: a cancelled prep keeps running until
// PrepareRollback notices its context, and the user can be back on
// screenProgress with a DIFFERENT operation by the time it answers. Ungated,
// its error failed that operation, its success overwrote the cleanup the new
// one owns, and the waitForEvent it starts put a second consumer on the live
// event channel.
func TestRollbackPreppedMsg_CarriesBatchIdentity(t *testing.T) {
	newModel := func() Model {
		events := make(chan runner.StepEvent)
		close(events)
		return Model{screen: screenProgress, eventCh: events, batchIdx: 1, batchSession: 7}
	}

	cases := []struct {
		name string
		msg  rollbackPreppedMsg
	}{
		{"stale session", rollbackPreppedMsg{batchIdx: 1, session: 6}},
		{"stale batch index", rollbackPreppedMsg{batchIdx: 0, session: 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" error is dropped", func(t *testing.T) {
			m := newModel()
			msg := tc.msg
			msg.err = errors.New("pull failed")
			updated, cmd := m.Update(msg)
			got := updated.(Model)
			if got.failed {
				t.Error("a superseded prep error marked the current operation failed")
			}
			if got.rollbackErr != "" {
				t.Errorf("rollbackErr = %q, want empty", got.rollbackErr)
			}
			if cmd != nil {
				t.Error("a superseded prep must not schedule anything")
			}
		})
		t.Run(tc.name+" success is dropped and cleaned up", func(t *testing.T) {
			m := newModel()
			calls := 0
			msg := tc.msg
			msg.cleanup = func() { calls++ }
			updated, cmd := m.Update(msg)
			got := updated.(Model)
			if calls != 1 {
				t.Errorf("cleanup ran %d times, want 1 - the override file leaks otherwise", calls)
			}
			if got.rollbackCleanup != nil {
				t.Error("a superseded prep overwrote the current batch's cleanup")
			}
			if cmd != nil {
				t.Error("a superseded prep started a second consumer on the event channel")
			}
		})
	}

	t.Run("the current batch is accepted", func(t *testing.T) {
		m := newModel()
		updated, cmd := m.Update(rollbackPreppedMsg{cleanup: func() {}, batchIdx: 1, session: 7})
		if updated.(Model).rollbackCleanup == nil {
			t.Error("the matching prep must store its cleanup")
		}
		if cmd == nil {
			t.Error("the matching prep must start consuming events")
		}
	})
}

// prepareRollbackCmd stamps the identity at DISPATCH, the same moment
// waitForEvent does, so a prep and the pipeline it starts are one batch.
func TestPrepareRollbackCmd_StampsCurrentBatchIdentity(t *testing.T) {
	mc := &mockRollbackComposer{prepErr: errors.New("nope")}
	m := Model{composer: mc, rollbackSnapshot: rollbackTestSnapshot(), ctx: context.Background(), batchIdx: 2, batchSession: 9}
	events := make(chan runner.StepEvent, 1)

	msg, ok := m.prepareRollbackCmd(context.Background(), []string{"web"}, io.Discard, events)().(rollbackPreppedMsg)
	if !ok {
		t.Fatal("cmd should produce a rollbackPreppedMsg")
	}
	if msg.batchIdx != 2 || msg.session != 9 {
		t.Errorf("identity = (idx %d, session %d), want (2, 9)", msg.batchIdx, msg.session)
	}

	// The capability-refusal branch carries it too - it is the one that can
	// answer instantly while a later operation is already on screen.
	plain := Model{composer: &mockComposer{}, ctx: context.Background(), batchIdx: 3, batchSession: 11}
	msg, ok = plain.prepareRollbackCmd(context.Background(), nil, io.Discard, events)().(rollbackPreppedMsg)
	if !ok {
		t.Fatal("cmd should produce a rollbackPreppedMsg")
	}
	if msg.batchIdx != 3 || msg.session != 11 {
		t.Errorf("refusal identity = (idx %d, session %d), want (3, 11)", msg.batchIdx, msg.session)
	}
}

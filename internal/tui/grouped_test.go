// Tests for the GROUPED container screen: the row model over multiple projects,
// the grouped loader and its reload, fold/select, the group header line,
// drill-in and drill-out, action-time composer binding, and the grouped update
// scan. The multi-batch progress sequence those operations feed lives in
// batch_test.go.

package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

// groupedModel installs a multi-group container screen by hand, bypassing the
// grouped loader. It is the only way to pin the header rows a single group
// never emits.
func groupedModel(groups ...svcGroup) Model {
	m := Model{selected: make(map[string]bool), screen: screenSelectContainers}
	m.svcGroups = groups
	m.svcEntries = rebuildSvcEntries(m.svcGroups)
	return m
}

// modelServices flattens the Model's groups back into the plain name list the
// tests used to read off a m.services field. That field is gone — svcGroups is
// the single representation — so the flattening lives here, in the one place
// that still wants a flat list.
func modelServices(m Model) []string {
	var out []string
	for _, g := range m.svcGroups {
		out = append(out, g.services...)
	}
	return out
}

// svcGroupOf is testGroup for the common unfolded case.
func svcGroupOf(name string, services ...string) svcGroup {
	return testGroup(name, false, services...)
}

// TestSvcCursor_MovesOverHeaderRows pins that the cursor indexes svcEntries,
// not services: it stops on group headers and its lower bound is the row count,
// which is larger than the service count as soon as a second group exists.
func TestSvcCursor_MovesOverHeaderRows(t *testing.T) {
	m := groupedModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "postgres"))

	wantKinds := []svcEntryKind{
		entrySvcGroupHeader, entryService, entryService,
		entrySvcGroupHeader, entryService,
	}
	if len(m.svcEntries) != len(wantKinds) {
		t.Fatalf("svcEntries has %d rows, want %d", len(m.svcEntries), len(wantKinds))
	}
	for i, want := range wantKinds {
		if m.svcEntries[i].kind != want {
			t.Fatalf("entry %d kind = %v, want %v", i, m.svcEntries[i].kind, want)
		}
	}

	// Walk to the bottom: five rows, so four presses land on the last one and a
	// fifth must not move past it.
	for i := 0; i < 6; i++ {
		updated, _ := m.Update(keyMsgFor("j"))
		m = updated.(Model)
	}
	if m.svcCursor != len(m.svcEntries)-1 {
		t.Errorf("svcCursor = %d, want %d (the cursor must reach the last ROW, headers included)", m.svcCursor, len(m.svcEntries)-1)
	}

	// Step back onto the second group's header.
	updated, _ := m.Update(keyMsgFor("k"))
	m = updated.(Model)
	if m.svcCursor != 3 {
		t.Fatalf("svcCursor = %d, want 3 (the second group's header)", m.svcCursor)
	}
	if _, ok := m.cursorService(); ok {
		t.Error("cursorService reported a service while the cursor sits on a header")
	}
	if e, ok := m.cursorEntry(); !ok || e.groupIdx != 1 {
		t.Errorf("cursorEntry = (%+v, %v), want the second group's header", e, ok)
	}

	m.svcCursor = 1
	svc, ok := m.cursorService()
	if !ok || svc != "api" {
		t.Errorf("cursorService = (%q, %v), want (\"api\", true)", svc, ok)
	}
}

// TestSpaceOnHeaderSelectsNothing pins that the selection stays empty when the
// cursor sits on a header: svcKeyAt answers "" for a header row, and an empty
// key must never enter the selection map.
func TestSpaceOnHeaderSelectsNothing(t *testing.T) {
	m := groupedModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"))
	m.svcCursor = 0 // the "web" header

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(Model)

	if len(m.selected) != 0 {
		t.Errorf("selected = %v, want empty (space on a header selects nothing)", m.selected)
	}
	if m.selectedCount() != 0 {
		t.Errorf("selectedCount = %d, want 0", m.selectedCount())
	}
}

// TestComputeMatches_SkipsGroupHeaders pins the two halves of the search rule:
// the returned indices are ROW indices, and a header never matches even when
// the project name contains the query.
func TestComputeMatches_SkipsGroupHeaders(t *testing.T) {
	entries := rebuildSvcEntries([]svcGroup{
		{proj: compose.Project{Name: "webproj"}, services: []string{"api", "web"}},
		{proj: compose.Project{Name: "db"}, services: []string{"web", "cache"}},
	})

	got := computeMatches(entries, "web")
	want := []int{2, 4}
	if !slices.Equal(got, want) {
		t.Errorf("computeMatches(..., %q) = %v, want %v (row indices of the two web SERVICES)", "web", got, want)
	}

	if got := computeMatches(entries, "webproj"); got != nil {
		t.Errorf("computeMatches(..., %q) = %v, want nil (a header must never match)", "webproj", got)
	}

	// The skip keys on the row KIND, not on a header's name happening to be
	// empty: a header that carried a name would otherwise become a jump target
	// with no service behind it.
	named := []svcEntry{
		{kind: entrySvcGroupHeader, groupIdx: 0, name: "web"},
		{kind: entryService, groupIdx: 0, name: "web"},
	}
	if got := computeMatches(named, "web"); !slices.Equal(got, []int{1}) {
		t.Errorf("computeMatches(named header, %q) = %v, want [1]", "web", got)
	}
}

// TestSearchJump_LandsOnServiceRow drives the real `/` flow over a grouped list
// and pins that the cursor lands on the matching service row, not on the header
// that precedes it.
func TestSearchJump_LandsOnServiceRow(t *testing.T) {
	m := groupedModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"))

	updated, _ := m.Update(keyMsgFor("/"))
	m = updated.(Model)
	if !m.searching {
		t.Fatal("precondition: search must be open")
	}
	for _, r := range "postgres" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if !slices.Equal(m.searchMatches, []int{3}) {
		t.Fatalf("searchMatches = %v, want [3] (the postgres ROW)", m.searchMatches)
	}
	if m.svcCursor != 3 {
		t.Errorf("svcCursor = %d, want 3", m.svcCursor)
	}
	svc, ok := m.cursorService()
	if !ok || svc != "postgres" {
		t.Errorf("cursorService = (%q, %v), want (\"postgres\", true) — the jump must not stop on a header", svc, ok)
	}
}

// TestFixSvcOffset_CountsHeaderRows is the discriminating pin for the offset
// math: with two headers the row count exceeds the service count, so an offset
// clamped against len(services) would refuse to scroll at all.
func TestFixSvcOffset_CountsHeaderRows(t *testing.T) {
	m := groupedModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "postgres"))
	m.width = 120
	m.height = 9 // 3 header lines + 3 footer lines leaves 3 visible rows

	visible := m.svcVisibleCount()
	if visible != 3 {
		t.Fatalf("svcVisibleCount = %d, want 3 (the assertions below assume it)", visible)
	}

	m.svcCursor = len(m.svcEntries) - 1
	m.fixSvcOffset()

	wantOffset := len(m.svcEntries) - visible
	if m.svcOffset != wantOffset {
		t.Errorf("svcOffset = %d, want %d (the window must scroll over ROWS, headers included)", m.svcOffset, wantOffset)
	}
	if m.svcCursor < m.svcOffset || m.svcCursor >= m.svcOffset+visible {
		t.Errorf("cursor %d outside the window [%d,%d)", m.svcCursor, m.svcOffset, m.svcOffset+visible)
	}

	m.height = 0
	if got := m.svcVisibleCount(); got != len(m.svcEntries) {
		t.Errorf("svcVisibleCount at height 0 = %d, want %d (all ROWS)", got, len(m.svcEntries))
	}
}

// TestSelectedContainers_IncludesFoldedGroup pins that folding hides ROWS and
// nothing else: a service selected before its group folded must still reach the
// operation, so the selection helpers read svcGroups rather than svcEntries.
func TestSelectedContainers_IncludesFoldedGroup(t *testing.T) {
	m := groupedModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"))
	m.selected[svcKey("web", "api")] = true
	m.selected[svcKey("db", "postgres")] = true

	m.svcGroups[1].folded = true
	m.svcEntries = rebuildSvcEntries(m.svcGroups)

	if got := m.selectedCount(); got != 2 {
		t.Errorf("selectedCount = %d, want 2 (a folded group keeps its selection)", got)
	}
	want := []string{"api", "postgres"}
	if got := m.selectedContainers(); !slices.Equal(got, want) {
		t.Errorf("selectedContainers = %v, want %v (bare names, folded group included)", got, want)
	}
	if !m.allSelected() {
		t.Error("allSelected = false, want true (every service in every group is selected)")
	}
}

// TestViewSelectContainers_RendersGroupHeaders pins the grouped render: an open
// group shows its marker and its services, a folded one shows only its header.
func TestViewSelectContainers_RendersGroupHeaders(t *testing.T) {
	m := groupedModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"))
	m.width = 80
	m.height = 20
	m.svcGroups[1].folded = true
	m.svcEntries = rebuildSvcEntries(m.svcGroups)

	out := ansi.Strip(m.viewSelectContainers())
	if !strings.Contains(out, "▼ web") {
		t.Errorf("open group header missing from:\n%s", out)
	}
	if !strings.Contains(out, "▶ db") {
		t.Errorf("folded group header missing from:\n%s", out)
	}
	if !strings.Contains(out, "api") {
		t.Errorf("the open group's service is missing from:\n%s", out)
	}
	if strings.Contains(out, "postgres") {
		t.Errorf("a folded group must not render its services:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Grouped host view (Task 6): loader, landing flow and refresh dispatch.
// ---------------------------------------------------------------------------

// mockGrouper is the HostGrouper seam double. It is deliberately NOT a
// runner.Composer method set of its own — the TUI reaches it by type-asserting
// whatever composerFactory(compose.Project{Unmanaged: true}) returns, so the
// double has to be reachable through a factory exactly as the real one is.
type mockGrouper struct {
	mockComposer
	groupedStatus map[string]map[string]runner.ServiceStatus
	groupedStats  map[string]map[string]runner.ServiceStats
	statusErr2    error
	statsErr2     error
	statusCalls2  int
	statsCalls2   int
	// statsEntered is closed by the stats half before it blocks, and statsGate
	// releases it. Both nil by default, so every other test drives a
	// non-blocking double. The close is sync.Once-guarded like the release
	// side: a test driving two gated cycles would otherwise panic inside the
	// mock instead of failing an assertion.
	statsEntered chan struct{}
	statsEnter   sync.Once
	statsGate    chan struct{}
	// statusEntries is the listing handle the status half hands out — a test
	// that leaves it unset gets defaultStampedEntries. statsEntries records the
	// one the stats half was handed; compose.HostEntries is opaque here, so
	// recording it is the only way to pin which listing the chain joined
	// against. statsCtx records the context that call ran under, which is how
	// the deadline bound on the chained fetch is pinned.
	statusEntries compose.HostEntries
	statsEntries  compose.HostEntries
	statsCtx      context.Context
	// statsBlockUntilDone makes the stats half hang until its context ends,
	// standing in for the wedged transport groupedStatsTimeout exists for.
	statsBlockUntilDone bool
}

// stampedHostEntries builds a genuine, STAMPED compose.HostEntries by driving
// the production seam over a faked docker. The handle is opaque outside the
// compose package and only GroupHostStatus stamps one, so this is the only way
// a tui test double can hand out a listing the stats half will accept — which
// is exactly what the stamp is for.
//
// The `docker ps` line is deliberately minimal and deliberately its own: the
// compose package's fixtures pin the PARSE, this pins only that a stamped
// handle survives the trip through the message, so the two are free to drift.
// A failure here is a broken fixture, not a test outcome, hence the panic.
func stampedHostEntries(id, name, proj, svc string) compose.HostEntries {
	c := compose.New("")
	line := fmt.Sprintf(
		`{"ID":%q,"Names":%q,"Image":"example:1","State":"running","Status":"Up 3 hours","Labels":"com.docker.compose.project=%s,com.docker.compose.service=%s"}`,
		id, name, proj, svc)
	c.SetTestHooks(nil, func(*exec.Cmd) ([]byte, error) { return []byte(line), nil })
	snap, err := compose.NewLocalHostContainers(c).GroupHostStatus(context.Background())
	if err != nil {
		panic("building a stamped host listing: " + err.Error())
	}
	if !snap.Entries.Listed() {
		panic("GroupHostStatus returned an unstamped handle")
	}
	return snap.Entries
}

// defaultStampedEntries is what mockGrouper hands out when a test does not name
// a listing of its own. A test double that returned the ZERO handle would be
// refused by the very gate the chain relies on, so every grouped test would
// fail on a fixture defect rather than on the behaviour it drives.
var defaultStampedEntries = sync.OnceValue(func() compose.HostEntries {
	return stampedHostEntries("aaa111222333", "shop-api-1", "shop", "api")
})

func (m *mockGrouper) GroupHostStatus(ctx context.Context) (compose.GroupedHostSnapshot, error) {
	m.statusCalls2++
	if m.statusErr2 != nil {
		return compose.GroupedHostSnapshot{}, m.statusErr2
	}
	entries := m.statusEntries
	if !entries.Listed() {
		entries = defaultStampedEntries()
	}
	return compose.GroupedHostSnapshot{Status: m.groupedStatus, Entries: entries}, nil
}

func (m *mockGrouper) GroupHostStats(ctx context.Context, entries compose.HostEntries) (map[string]map[string]runner.ServiceStats, error) {
	// Everything a test reads about this call is recorded BEFORE the gate, in
	// one step: a caller blocked on the gate must be able to observe the call
	// count and the handle it was given at the same moment.
	m.statsEntries = entries
	m.statsCtx = ctx
	m.statsCalls2++
	if m.statsEntered != nil {
		m.statsEnter.Do(func() { close(m.statsEntered) })
	}
	if m.statsGate != nil {
		<-m.statsGate
	}
	if m.statsBlockUntilDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if m.statsErr2 != nil {
		return nil, m.statsErr2
	}
	return m.groupedStats, nil
}

// Compile-time pin, for the same reason the Inspector block above carries one:
// the grouped screen reaches the seam through a runtime type assertion, so a
// signature drift on the real implementation would leave the suite green while
// the whole host view silently rendered no status.
var _ HostGrouper = (*compose.HostContainers)(nil)

func groupedTestModel(g *mockGrouper, projects []compose.Project) Model {
	m := NewModel(nil, io.Discard, func(compose.Project) runner.Composer { return g }, nil, nil)
	installFakeTick(&m)
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) { return projects, nil }
	m.screen = screenSelectContainers
	m.grouped = true
	return m
}

func groupedFixture() (*mockGrouper, []compose.Project) {
	g := &mockGrouper{
		groupedStatus: map[string]map[string]runner.ServiceStatus{
			"shop":                       {"api": {Running: true}, "db": {}},
			"blog":                       {"web": {Running: true}},
			compose.UnmanagedProjectName: {"watchtower": {Running: true}},
		},
		groupedStats: map[string]map[string]runner.ServiceStats{
			"shop": {"api": {CPUPercent: 12.5, MemoryUsed: 100}},
			"blog": {"web": {CPUPercent: 1}},
		},
	}
	projects := []compose.Project{
		{Name: "blog", ConfigDir: "/srv/blog"},
		{Name: "shop", ConfigDir: "/srv/shop"},
		{Name: compose.UnmanagedProjectName, Unmanaged: true},
	}
	return g, projects
}

func TestLoadGroups_MergesLoaderAndHostStatus(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)

	msg, ok := m.loadGroups()().(servicesMsg)
	if !ok {
		t.Fatalf("loadGroups() produced %T, want servicesMsg", m.loadGroups()())
	}
	if !msg.groupedPayload {
		t.Fatal("loadGroups() must mark its payload grouped; an empty host is a valid result")
	}
	if len(msg.projects) != 3 || len(msg.hostStatus) != 3 {
		t.Fatalf("payload = %d projects / %d host groups, want 3/3", len(msg.projects), len(msg.hostStatus))
	}
	if msg.session != m.statusSession {
		t.Errorf("session = %d, want %d (loadGroups reuses statusSession)", msg.session, m.statusSession)
	}
	// The payload stays in BARE-name form on the wire: qualification happens at
	// arrival, so nothing qualified can travel back out to runner or compose.
	for _, svcs := range msg.hostStatus {
		for name := range svcs {
			if strings.Contains(name, svcKeySep) {
				t.Errorf("host status key %q is qualified; the wire form must be bare", name)
			}
		}
	}
}

func TestLoadGroups_ErrorPaths(t *testing.T) {
	// EVERY return carries groupedPayload, failures included: the flag is the
	// message's SHAPE, and this message is where refreshInFlight is settled —
	// on a failure it is the LAST arrival of the cycle, since there is no
	// listing to chain the stats half off. An error return without the flag
	// latched the guard and silenced the 5-second refresh for the rest of the
	// visit.
	t.Run("no loader", func(t *testing.T) {
		g, _ := groupedFixture()
		m := groupedTestModel(g, nil)
		m.projectLoader = nil
		msg := m.loadGroups()().(servicesMsg)
		if msg.err == nil {
			t.Fatal("a missing loader must report an error, not an empty host")
		}
		if !msg.groupedPayload {
			t.Error("a missing loader must still mark the payload grouped, or refreshInFlight latches")
		}
	})
	t.Run("loader failure", func(t *testing.T) {
		g, _ := groupedFixture()
		m := groupedTestModel(g, nil)
		m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
			return nil, errors.New("compose ls failed")
		}
		msg := m.loadGroups()().(servicesMsg)
		if msg.err == nil {
			t.Fatalf("loader failure = %+v, want an error", msg)
		}
		if !msg.groupedPayload {
			t.Error("a loader failure must still mark the payload grouped, or refreshInFlight latches")
		}
		if len(msg.projects) != 0 || len(msg.hostStatus) != 0 {
			t.Errorf("loader failure carried data: %+v", msg)
		}
	})
	t.Run("host ps failure", func(t *testing.T) {
		g, projects := groupedFixture()
		g.statusErr2 = errors.New("docker ps failed")
		m := groupedTestModel(g, projects)
		msg := m.loadGroups()().(servicesMsg)
		if msg.err == nil {
			t.Fatal("a host-ps failure must land in svcErr, not be swallowed")
		}
		if !msg.groupedPayload {
			t.Error("a host-ps failure must still mark the payload grouped, or refreshInFlight latches")
		}
	})
	t.Run("factory without a grouper still lists projects", func(t *testing.T) {
		mc := &mockComposer{}
		m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
		m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
			return []compose.Project{{Name: "shop"}}, nil
		}
		msg := m.loadGroups()().(servicesMsg)
		if msg.err != nil {
			t.Fatalf("err = %v; a composer that is no HostGrouper means no status, not a failure", msg.err)
		}
		if len(msg.projects) != 1 || msg.hostStatus != nil {
			t.Errorf("payload = %+v, want the project list with no host status", msg)
		}
	})
}

func TestGroupedServicesMsg_Hydrates(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)

	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	if m.svcErr != nil {
		t.Fatalf("svcErr = %v", m.svcErr)
	}
	if shape := groupShape(m.svcGroups); !slices.Equal(shape, []string{"blog[web]", "shop[api,db]", "(unmanaged)[watchtower]"}) {
		t.Fatalf("groups = %v", shape)
	}
	// Three headers plus four services.
	if len(m.svcEntries) != 7 {
		t.Errorf("entries = %d, want 7 (3 headers + 4 services)", len(m.svcEntries))
	}
	if !m.svcStatus[svcKey("shop", "api")].Running {
		t.Errorf("svcStatus = %v; shop/api should be running", m.svcStatus)
	}
	if _, ok := m.svcStatus["api"]; ok {
		t.Error("a bare-name key survived arrival; the handler must qualify")
	}
	if got := modelServices(m); len(got) != 4 {
		t.Errorf("services = %v, want the flat list of all four", got)
	}
}

func TestGroupedServicesMsg_RejectsStaleSession(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	payload := m.loadGroups()().(servicesMsg)
	m.statusSession++ // a context change happened while the fetch was in flight

	updated, _ := m.Update(payload)
	m = updated.(Model)

	if m.svcGroups != nil {
		t.Errorf("a stale grouped payload hydrated the screen: %v", groupShape(m.svcGroups))
	}
}

func TestGroupedServicesMsg_OffScreenIsDropped(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	payload := m.loadGroups()().(servicesMsg)
	m.screen = screenLogs

	updated, _ := m.Update(payload)
	if got := updated.(Model); got.svcGroups != nil {
		t.Error("an off-screen grouped payload must be dropped")
	}
}

// The grouped payload is BOTH the initial load and the 5s refresh, so a reload
// must not fight the user: cursor, selection, fold and search all survive.
func TestGroupedServicesMsg_ReloadPreservesUserState(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	m.svcGroups[1].folded = true
	m.setGroups(m.svcGroups)
	m.svcCursor = 2
	m.selected[svcKey("shop", "api")] = true
	m.selected[svcKey("gone", "ghost")] = true

	updated, _ = m.Update(m.loadGroups()())
	m = updated.(Model)

	if !m.svcGroups[1].folded {
		t.Error("the fold was lost across a reload")
	}
	if m.svcCursor != 2 {
		t.Errorf("svcCursor = %d, want 2 (a reload must not reset the cursor)", m.svcCursor)
	}
	if !m.selected[svcKey("shop", "api")] {
		t.Error("the selection was lost across a reload")
	}
	if m.selected[svcKey("gone", "ghost")] {
		t.Error("a selection whose service no longer exists must be pruned")
	}
}

func TestGroupedServicesMsg_ShrinkClampsCursor(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	m.svcCursor = len(m.svcEntries) - 1

	g.groupedStatus = map[string]map[string]runner.ServiceStatus{"blog": {"web": {}}}
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return []compose.Project{{Name: "blog"}}, nil
	}
	updated, _ = m.Update(m.loadGroups()())
	m = updated.(Model)

	if m.svcCursor >= len(m.svcEntries) || m.svcCursor < 0 {
		t.Errorf("svcCursor = %d with %d entries; the cursor must be clamped", m.svcCursor, len(m.svcEntries))
	}
}

// groupedCycle drives the WHOLE grouped fetch: the loadGroups arrival, then the
// stats half that arrival chains. Two messages is the shape — the rows must not
// wait on the host-wide `docker stats` — so any test that wants CPU/Mem cells
// has to run both.
func groupedCycle(t *testing.T, m Model) Model {
	t.Helper()
	updated, cmd := m.Update(m.loadGroups()())
	m = updated.(Model)
	if cmd == nil {
		// Every caller wants the CPU/Mem cells. Returning the status-only model
		// instead would leave them asserting on a half-run cycle.
		t.Fatal("the grouped arrival did not chain the stats half")
	}
	updated, _ = m.Update(cmd())
	return updated.(Model)
}

// The grouped fetch is one `docker ps` split across two messages: the rows
// arrive first, the CPU/Mem join is chained off the listing they came from.
func TestGroupedStatsMsg_Hydrates(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	m.refreshInFlight = true

	m = groupedCycle(t, m)

	if m.statsErr != nil {
		t.Fatalf("statsErr = %v", m.statsErr)
	}
	if m.refreshInFlight {
		t.Error("refreshInFlight must clear on a current-session stats arrival")
	}
	if m.stats[svcKey("shop", "api")].CPUPercent != 12.5 {
		t.Errorf("stats = %v, want shop/api at 12.5%%", m.stats)
	}
	if _, ok := m.stats["api"]; ok {
		t.Error("a bare-name stats key survived arrival")
	}
	// One listing between the two halves: the stats half consumes the handle
	// the status half returned instead of running its own `docker ps`.
	if g.statusCalls2 != 1 || g.statsCalls2 != 1 {
		t.Errorf("seam calls = %d status / %d stats, want 1/1", g.statusCalls2, g.statsCalls2)
	}
}

// A stats failure never costs the status view: it lands in statsErr, which the
// error ladder renders below svcErr.
func TestGroupedStatsMsg_FailureIsSoft(t *testing.T) {
	g, projects := groupedFixture()
	g.statsErr2 = errors.New("docker stats failed")
	m := groupedTestModel(g, projects)
	m = groupedCycle(t, m)

	if m.statsErr == nil {
		t.Fatal("statsErr should carry the stats failure")
	}
	if m.svcErr != nil {
		t.Errorf("svcErr = %v; a stats failure must not blank the status view", m.svcErr)
	}
	if len(m.svcGroups) != 3 {
		t.Error("the grouped rows should survive a stats failure")
	}
	if m.refreshInFlight {
		t.Error("a failed stats half must still clear refreshInFlight")
	}
}

// A factory that produces no HostGrouper (every test mock) must still render
// the projects the loader found: no host data is not an error.
func TestLoadGroups_NoGrouperIsEmptyNotAnError(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), nil, nil)
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return []compose.Project{{Name: "shop", ConfigDir: "/srv/shop"}}, nil
	}
	msg := m.loadGroups()().(servicesMsg)
	if msg.err != nil || !msg.groupedPayload || msg.hostStatus != nil {
		t.Errorf("servicesMsg = %+v, want an empty grouped payload with no error", msg)
	}
	// And nothing to chain: no HostGrouper means no stats half, so the cycle
	// ends at this message and the guard clear must stay with it. The payload
	// carries neither the seam nor a stamped listing, which is the gate.
	if msg.hostGrouper != nil {
		t.Error("the payload must carry no seam when the factory produced none")
	}
	if msg.hostEntries.Listed() != false {
		t.Error("a payload with no status half must carry an unstamped listing")
	}
	if m.groupedStatsCmd(msg.hostGrouper, msg.hostEntries) != nil {
		t.Error("groupedStatsCmd must be nil without a HostGrouper, or refreshInFlight latches")
	}
	if len(msg.projects) != 1 {
		t.Errorf("projects = %v, want the loader result to survive", msg.projects)
	}
}

// statsRefreshCmd is nil in grouped mode: the grouped stats half needs the
// listing loadGroups returns, so it is CHAINED off the arrival instead of
// batched beside it — batching it would be a second `docker ps`. Every call
// site batches the pair, and tea.Batch drops a nil, so no site needs a mode
// branch.
func TestStatsRefreshCmd_NilInGroupedMode(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	if m.statsRefreshCmd() != nil {
		t.Error("statsRefreshCmd() must be nil in grouped mode")
	}
	m.grouped = false
	m.composer = &mockComposer{}
	if m.statsRefreshCmd() == nil {
		t.Error("statsRefreshCmd() must still fetch in drilled mode")
	}
}

// The tick gate used to read "m.composer == nil". Grouped mode holds no
// composer by design, so the gate has to consult the factory instead — testing
// for a composer would silence the periodic refresh on the host view.
func TestRefreshTick_GroupedDispatchesHostWideFetches(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	m.refreshInFlight = false

	updated, cmd := m.Update(refreshTickMsg{})
	m = updated.(Model)
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("tick produced %T, want a fetch batch", cmd())
	}
	// The tick fans out the status half only; the stats half is chained off its
	// arrival, so it takes feeding the message back to reach the second call.
	for _, c := range batch {
		if msg := c(); msg != nil {
			updated, next := m.Update(msg)
			m = updated.(Model)
			if next != nil {
				next()
			}
		}
	}
	if g.statusCalls2 != 1 || g.statsCalls2 != 1 {
		t.Errorf("grouped tick made %d status / %d stats calls, want 1/1", g.statusCalls2, g.statsCalls2)
	}
	if g.statusCalls != 0 || g.statsCalls != 0 {
		t.Errorf("grouped tick fell through to the per-project composer: %d/%d", g.statusCalls, g.statsCalls)
	}
}

func TestRefreshTick_GroupedWithoutFactoryIsRescheduleOnly(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	m.refreshInFlight = false
	m.composerFactory = nil

	_, cmd := m.Update(refreshTickMsg{})
	if _, ok := cmd().(tea.BatchMsg); ok {
		t.Error("a grouped screen with no factory has nothing to fetch through; expected a bare reschedule")
	}
}

// Drilled mode keeps the exact per-project calls it always made.
func TestRefreshTick_DrilledStillUsesTheComposer(t *testing.T) {
	g, _ := groupedFixture()
	m := NewModel(&g.mockComposer, io.Discard, func(compose.Project) runner.Composer { return g }, nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.refreshInFlight = false

	_, cmd := m.Update(refreshTickMsg{})
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("tick produced %T, want a fetch batch", cmd())
	}
	for _, c := range batch {
		c()
	}
	if g.statusCalls != 1 || g.statsCalls != 1 {
		t.Errorf("drilled tick made %d status / %d stats calls, want 1/1", g.statusCalls, g.statsCalls)
	}
	if g.statusCalls2 != 0 || g.statsCalls2 != 0 {
		t.Errorf("drilled tick used the host-wide seam: %d/%d", g.statusCalls2, g.statsCalls2)
	}
}

func TestInit_GroupedDispatchesGroupedLoad(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)

	batch, ok := m.Init()().(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init() produced %T, want a batch", m.Init()())
	}
	for _, c := range batch {
		if msg := c(); msg != nil {
			updated, next := m.Update(msg)
			m = updated.(Model)
			if next != nil {
				next()
			}
		}
	}
	if g.statusCalls2 != 1 || g.statsCalls2 != 1 {
		t.Errorf("Init() made %d grouped status / %d grouped stats calls, want 1/1", g.statusCalls2, g.statsCalls2)
	}
	if g.updatesCalls != 0 {
		t.Error("Init() must not scan for updates in grouped mode")
	}
}

func TestEnterGroupedContainers_ResetsAndBumpsSessions(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	m.screen = screenSelectServer
	m.grouped = false
	m.composer = &g.mockComposer
	m.projName, m.projDir = "old", "/old"
	m.drilledFromHost = true
	m.setSingleGroup([]string{"stale"})
	m.svcStatus = map[string]runner.ServiceStatus{"old/stale": {}}
	m.stats = map[string]runner.ServiceStats{"old/stale": {}}
	m.statsErr = errors.New("old")
	m.svcErr = errors.New("old")
	m.selected = map[string]bool{"old/stale": true}
	m.svcCursor, m.svcOffset = 3, 2
	m.updatesErr = "old"
	m.updateInFlight = true
	stats, status, updates := m.statsSession, m.statusSession, m.updatesSession

	cmd := m.enterGroupedContainers()

	if m.screen != screenSelectContainers || !m.grouped {
		t.Fatalf("screen = %d grouped = %v", m.screen, m.grouped)
	}
	if m.composer != nil {
		t.Error("grouped mode must hold no composer")
	}
	for name, got := range map[string]bool{
		"svcGroups":       m.svcGroups != nil,
		"svcEntries":      m.svcEntries != nil,
		"svcStatus":       m.svcStatus != nil,
		"stats":           m.stats != nil,
		"statsErr":        m.statsErr != nil,
		"svcErr":          m.svcErr != nil,
		"drilledFromHost": m.drilledFromHost,
		"selection":       len(m.selected) != 0,
		"cursor":          m.svcCursor != 0,
		"offset":          m.svcOffset != 0,
		"projName":        m.projName != "",
		"projDir":         m.projDir != "",
		"updatesErr":      m.updatesErr != "",
	} {
		if got {
			t.Errorf("%s survived the landing", name)
		}
	}
	if m.statsSession == stats || m.statusSession == status || m.updatesSession == updates {
		t.Error("the landing site must bump all three session counters")
	}
	if !m.refreshInFlight {
		t.Error("refreshInFlight should be armed for the stats fetch in the returned batch")
	}
	if m.updateInFlight {
		t.Error("updateInFlight must be reset before the batch, like every context-change site")
	}
	if cmd == nil {
		t.Fatal("enterGroupedContainers() returned no load batch")
	}
}

func TestConnectSuccess_BumpsTheThreeCountersAndLandsGrouped(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	stats, status, updates := m.statsSession, m.statusSession, m.updatesSession
	m.updateInFlight = true

	updated, cmd := m.Update(connectResultMsg{err: nil})
	m = updated.(Model)

	if m.screen != screenSelectContainers || !m.grouped {
		t.Fatalf("screen = %d grouped = %v, want the grouped host view", m.screen, m.grouped)
	}
	// The inverse of the old rule: this site now starts fetching live data, so
	// it is the site that must invalidate whatever a previous server left in
	// flight.
	if m.statsSession == stats || m.statusSession == status || m.updatesSession == updates {
		t.Error("the connect-success path must bump statsSession/statusSession/updatesSession")
	}
	if m.updateInFlight || !m.refreshInFlight {
		t.Errorf("updateInFlight = %v refreshInFlight = %v", m.updateInFlight, m.refreshInFlight)
	}
	if cmd == nil {
		t.Error("connect success should return the grouped load batch")
	}
}

func TestConnectError_ClearsGroupedState(t *testing.T) {
	mc := &mockComposer{}
	m := NewModel(nil, io.Discard, mockFactory(mc), testServers, mockConnectCb(mc))
	m.grouped = true
	m.setGroups([]svcGroup{{proj: compose.Project{Name: "shop"}, services: []string{"api"}}})
	m.svcStatus = map[string]runner.ServiceStatus{svcKey("shop", "api"): {}}
	m.stats = map[string]runner.ServiceStats{svcKey("shop", "api"): {}}
	m.selected = map[string]bool{svcKey("shop", "api"): true}
	m.svcCursor, m.svcOffset = 1, 1

	updated, _ := m.Update(connectResultMsg{err: errors.New("ssh failed")})
	m = updated.(Model)

	if m.grouped {
		t.Error("a failed connect leaves the server screen; grouped must be cleared")
	}
	if m.svcGroups != nil || m.svcEntries != nil {
		t.Error("the grouped rows must not outlive the failed connect")
	}
	if m.svcStatus != nil || m.stats != nil || len(m.selected) != 0 {
		t.Error("status, stats and selection must be cleared on the connect error path")
	}
	if m.svcCursor != 0 || m.svcOffset != 0 {
		t.Errorf("cursor/offset = %d/%d, want 0/0", m.svcCursor, m.svcOffset)
	}
}

func TestGroupedScreen_EscGoesBackToServerScreen(t *testing.T) {
	g, projects := groupedFixture()
	m := NewModel(nil, io.Discard, func(compose.Project) runner.Composer { return g }, testServers, mockConnectCb(&g.mockComposer))
	installFakeTick(&m)
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) { return projects, nil }
	m.enterGroupedContainers()
	m.serverName = "prod"
	disconnected := false
	m.disconnectFunc = func() error { disconnected = true; return nil }

	if !m.canGoBack() {
		t.Fatal("the grouped screen is not a root screen when servers are configured")
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if m.screen != screenSelectServer {
		t.Fatalf("screen = %d, want %d", m.screen, screenSelectServer)
	}
	if m.grouped || m.svcGroups != nil || m.serverName != "" || m.disconnectFunc != nil {
		t.Error("esc from the grouped screen must clear grouped state and the remote connection")
	}
	if cmd == nil {
		t.Fatal("esc from a remote grouped screen should run the disconnect")
	}
	cmd()
	if !disconnected {
		t.Error("the disconnect callback was not invoked")
	}
}

// Standalone local run: the grouped screen is a ROOT — esc does nothing and q
// quits, exactly as the screen under the server picker has always behaved.
func TestGroupedScreen_IsRootWithoutServers(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)

	if m.canGoBack() {
		t.Fatal("with no servers and no config the grouped screen has no parent")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got := updated.(Model); got.screen != screenSelectContainers {
		t.Errorf("esc navigated off a root screen: screen = %d", got.screen)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Error("q must quit from the root grouped screen")
	}
}

// Grouped mode has no single composer, and a selection may span projects, so
// the write ops are refused rather than left to dereference nil. The READ keys
// (l, x, i, c) bind one from the cursor row's group instead — see the drill-in
// tests below.
// groupedOpModel builds the loaded grouped screen used by the operation pins.
// Rows: 0 header blog, 1 web, 2 header shop, 3 api, 4 db,
//
//	5 header (unmanaged), 6 watchtower.
func groupedOpModel(t *testing.T) Model {
	t.Helper()
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	if got := len(m.svcEntries); got != 7 {
		t.Fatalf("precondition: %d rows, want 7", got)
	}
	return m
}

// An empty selection is not a mistake in grouped mode: the cursor names a
// project, so d/r/s arm the whole-project batch that the runner reads as ALL
// services — including the never-created ones the host-wide ps cannot see.
func TestGroupedScreen_ArmsWholeGroupOpFromCursor(t *testing.T) {
	base := groupedOpModel(t)
	base.width = 100
	base.height = 24

	ops := map[string]runner.Operation{"d": runner.Deploy, "r": runner.Restart, "s": runner.StopOnly}
	for _, key := range []string{"d", "r", "s"} {
		t.Run(key, func(t *testing.T) {
			m := base
			m.svcCursor = 3 // shop/api, a service row inside the second group
			updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			got := updated.(Model)
			if !got.confirming {
				t.Fatalf("%q did not arm a confirmation; warning = %q", key, got.warning)
			}
			if got.pendingOp != ops[key] {
				t.Errorf("%q armed %v, want %v", key, got.pendingOp, ops[key])
			}
			batches := got.partitionSelection()
			if len(batches) != 1 || batches[0].proj.Name != "shop" || len(batches[0].services) != 0 {
				t.Fatalf("batches = %+v, want one whole-project shop batch", batches)
			}
			if view := got.viewSelectContainers(); !strings.Contains(view, "shop (all)") {
				t.Errorf("prompt must name the batch; got:\n%s", view)
			}
		})
	}
}

// The confirmation prompt names the project each service belongs to, because a
// bare service list cannot say which "api" is meant on a host that has two.
func TestGroupedScreen_ConfirmPromptNamesTheBatch(t *testing.T) {
	m := groupedOpModel(t)
	m.width = 100
	m.height = 24
	m.selected["shop/api"] = true
	m.selected["shop/db"] = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	if !m.confirming {
		t.Fatalf("d did not arm a confirmation; warning = %q", m.warning)
	}
	view := m.viewSelectContainers()
	if !strings.Contains(view, "Deploy shop (api, db)?") {
		t.Errorf("prompt should name the batch, got:\n%s", view)
	}
}

// The prompt occupies exactly the rows containerFooterLines reserved for it, so
// a batch list long enough to wrap must be truncated rather than pushed onto a
// second physical line.
func TestGroupedScreen_ConfirmPromptClampsToWidth(t *testing.T) {
	m := groupedOpModel(t)
	m.width = 40
	m.height = 24
	m.selected["shop/api"] = true
	m.selected["shop/db"] = true
	m.confirming = true
	m.pendingOp = runner.Deploy

	want := m.containerFooterLines()
	view := m.viewSelectContainers()
	lines := strings.Split(view, "\n")
	// The prompt is the tail of the view: the reserved bar line, helpStyle's
	// MarginTop blank, then the prompt padded to the footer's line count.
	for _, line := range lines[len(lines)-want:] {
		if w := ansi.StringWidth(line); w > m.width {
			t.Errorf("prompt line %q is %d cells wide, want <= %d", line, w, m.width)
		}
	}
}

// A selection that spans two projects is a two-batch sequence, not a refusal:
// the progress screen runs one pipeline per project, in screen order.
func TestGroupedScreen_ArmsCrossProjectSelection(t *testing.T) {
	for _, key := range []string{"d", "r", "s"} {
		t.Run(key, func(t *testing.T) {
			// Built INSIDE the subtest: a Model copied by value still shares
			// its selected map, svcGroups and svcEntries, so one subtest that
			// selected or folded would silently rewrite the next one's fixture.
			m := groupedOpModel(t)
			m.width = 100
			m.height = 24
			m.selected["blog/web"] = true
			m.selected["shop/api"] = true
			updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			got := updated.(Model)
			if !got.confirming {
				t.Fatalf("%q refused a cross-project selection; warning = %q", key, got.warning)
			}
			if got.warning != "" {
				t.Errorf("%q warning = %q, want none", key, got.warning)
			}
			if shape := batchShape(got.partitionSelection()); len(shape) != 2 {
				t.Fatalf("batches = %v, want two", shape)
			}
			// The prompt names both batches in screen order.
			if view := got.viewSelectContainers(); !strings.Contains(view, "blog (web) → shop (api)") {
				t.Errorf("prompt must name both batches, got:\n%s", view)
			}
		})
	}
}

// The unmanaged bucket has no compose project behind it, so an operation keyed
// off the cursor there has nothing to run and must say so.
func TestGroupedScreen_UnmanagedCursorHasNoOperation(t *testing.T) {
	base := groupedOpModel(t)

	for _, cursor := range []int{5, 6} { // the unmanaged header and its row
		for _, key := range []string{"d", "r", "s"} {
			m := base
			m.svcCursor = cursor
			updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			got := updated.(Model)
			if got.confirming {
				t.Errorf("cursor %d: %q armed an operation on the unmanaged bucket", cursor, key)
			}
			if got.warning != warnNoSelection {
				t.Errorf("cursor %d: %q warning = %q, want %q", cursor, key, got.warning, warnNoSelection)
			}
		}
	}
}

// Grouped mode holds no composer between actions, so confirming an operation
// must bind the BATCH's one — the cursor's group is not necessarily the
// selection's.
func TestGroupedScreen_ConfirmBindsTheBatchComposer(t *testing.T) {
	g, projects := groupedFixture()
	target := &mockComposer{services: []string{"api", "db"}}
	var asked []string
	m := groupedTestModel(g, projects)
	m.composerFactory = func(p compose.Project) runner.Composer {
		if p.Unmanaged {
			return g
		}
		asked = append(asked, p.Name)
		return target
	}
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	m.width = 80
	m.height = 24
	m.selected["shop/api"] = true
	m.svcCursor = 1 // blog/web: the cursor is NOT in the selected group

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	if !m.confirming {
		t.Fatalf("d did not arm a confirmation; warning = %q", m.warning)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.screen != screenProgress {
		t.Fatalf("screen = %d, want screenProgress", m.screen)
	}
	if m.composer != runner.Composer(target) {
		t.Error("the batch composer was not bound before the pipeline started")
	}
	if len(asked) == 0 || asked[len(asked)-1] != "shop" {
		t.Errorf("factory asked for %v, want the selected group (shop) last", asked)
	}
	if len(m.opContainers) != 1 || m.opContainers[0] != "api" {
		t.Errorf("opContainers = %v, want the batch's bare service names", m.opContainers)
	}
}

// A rollback restores one project's recorded digests from that project's own
// snapshot file. There is no host-wide snapshot, so a capture that spans
// projects is refused permanently — not as scaffolding.
func TestGroupedScreen_RollbackRefusesCrossProject(t *testing.T) {
	g, projects := groupedFixture()
	rb := &mockRollbackComposer{
		mockComposer: mockComposer{services: []string{"api", "db"}},
		snap:         rollbackTestSnapshot(),
	}
	m := groupedTestModel(g, projects)
	m.composerFactory = func(p compose.Project) runner.Composer {
		if p.Unmanaged {
			return g
		}
		return rb
	}
	updated, _ := m.Update(m.loadGroups()())
	base := updated.(Model)

	t.Run("cross project refused", func(t *testing.T) {
		m := base
		// A fresh map per subtest: the Model is a value but m.selected is a
		// reference, so a shared one would leak the first case's targets.
		m.selected = map[string]bool{"blog/web": true, "shop/api": true}
		updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
		got := updated.(Model)
		if got.warning != warnRollbackCrossProject {
			t.Errorf("warning = %q, want %q", got.warning, warnRollbackCrossProject)
		}
		if cmd != nil {
			t.Error("a cross-project R must not fire a snapshot fetch")
		}
		if got.rollbackTargets != nil {
			t.Errorf("rollbackTargets = %v, want nothing captured", got.rollbackTargets)
		}
		if got.composer != nil {
			t.Error("a refused R must not leave a composer bound in grouped mode")
		}
	})

	t.Run("single project fires the fetch", func(t *testing.T) {
		m := base
		m.selected = map[string]bool{"shop/api": true, "shop/db": true}
		updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
		got := updated.(Model)
		if got.warning != "" {
			t.Errorf("warning = %q, want none", got.warning)
		}
		if cmd == nil {
			t.Fatal("a single-project R must fire the snapshot fetch")
		}
		if got.composer != runner.Composer(rb) {
			t.Error("R must bind the captured project's composer")
		}
		if !slices.Equal(got.rollbackTargets, []string{"api", "db"}) {
			t.Errorf("rollbackTargets = %v, want [api db] (bare names)", got.rollbackTargets)
		}
	})
}

// The grouped whole-group op has no selection to name, so the progress title
// reads the set the pipeline actually got rather than an empty tail.
func TestGroupedScreen_ProgressTitleNamesTheTarget(t *testing.T) {
	base := groupedOpModel(t)
	base.width = 100
	base.height = 24

	t.Run("whole group", func(t *testing.T) {
		m := base
		m.svcCursor = 2 // the shop header
		m.opContainers = nil
		m.pendingOp = runner.Deploy
		m.screen = screenProgress
		if view := m.viewProgress(); !strings.Contains(view, "Deploy > all services") {
			t.Errorf("title should name the whole-group target, got:\n%s", view)
		}
	})

	t.Run("named services", func(t *testing.T) {
		m := base
		m.opContainers = []string{"api", "db"}
		m.pendingOp = runner.Deploy
		m.screen = screenProgress
		if view := m.viewProgress(); !strings.Contains(view, "Deploy > api, db") {
			t.Errorf("title should name the batch services, got:\n%s", view)
		}
	})
}

// A composer without the rollback capability leaves R inert, and grouped mode
// must not stay holding the composer it bound to find that out.
func TestGroupedScreen_RollbackNonPreparerUnbinds(t *testing.T) {
	m := groupedOpModel(t)
	m.selected["shop/api"] = true
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	got := updated.(Model)
	if cmd != nil || got.confirming {
		t.Error("R on a non-preparer composer must be a silent no-op")
	}
	if got.composer != nil {
		t.Error("R must unbind the grouped composer it probed")
	}
}

func TestGroupedMode_NoAutomaticUpdateScan(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)

	if m.autoUpdatesAllowed() {
		t.Error("autoUpdatesAllowed must be false in grouped mode")
	}
	if cmd := m.maybeRefreshUpdatesCmd(); cmd != nil {
		t.Error("maybeRefreshUpdatesCmd must schedule nothing in grouped mode")
	}
	// U is the only trigger, and it scans the CURSOR row's group — this model
	// has loaded no rows, so there is no group to scan and the key is inert.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	if cmd != nil {
		t.Error("U must not fan out to the registry with no group under the cursor")
	}
	if got := updated.(Model); got.updateInFlight {
		t.Error("U must not leave updateInFlight latched")
	}
	if g.updatesCalls != 0 {
		t.Errorf("CheckUpdates ran %d times in grouped mode", g.updatesCalls)
	}
}

func TestBackToServerScreen_RestoresLocalCallbacks(t *testing.T) {
	g, projects := groupedFixture()
	localLoader := func(ctx context.Context) ([]compose.Project, error) { return projects, nil }
	m := NewModel(nil, io.Discard, func(compose.Project) runner.Composer { return g }, testServers, mockConnectCb(&g.mockComposer),
		WithLocalProjectLoader(localLoader))
	m.composerFactory = func(compose.Project) runner.Composer { return nil }
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) { return nil, nil }
	m.grouped = true
	statusSess := m.statusSession

	m.backToServerScreen()

	if m.projectLoader == nil || m.composerFactory == nil {
		t.Fatal("the local callbacks must be restored, never left nil")
	}
	if m.statusSession == statusSess {
		t.Error("swapping the projectLoader must invalidate the grouped load it feeds")
	}
	if m.refreshInFlight || m.updateInFlight {
		t.Error("the new context must start with both in-flight guards clear")
	}
}

// The grouped header row is drawn in a later task; this only pins that the
// current renderer survives multi-group entries and lists every service, so
// the landing flow shipped here is not a crash waiting for that task.
func TestGroupedScreen_RendersWithoutPanicking(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	m.width, m.height = 100, 30
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	v := m.View()
	for _, name := range []string{"api", "db", "web", "watchtower"} {
		if !strings.Contains(v, name) {
			t.Errorf("grouped render is missing %q:\n%s", name, v)
		}
	}
	if strings.Contains(v, "Loading services") {
		t.Errorf("a hydrated grouped screen must not claim to be loading:\n%s", v)
	}
}

// An empty host is a real result, not a stuck load: buildSvcGroups returns a
// non-nil empty slice so setGroups installs a non-nil services list.
func TestGroupedScreen_EmptyHostLeavesLoadingState(t *testing.T) {
	g := &mockGrouper{}
	m := groupedTestModel(g, nil)
	m.width, m.height = 100, 30
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	if m.svcGroups == nil {
		t.Fatal("an empty host must still install a non-nil services slice")
	}
	if v := m.View(); strings.Contains(v, "Loading services") {
		t.Errorf("an empty host must not render as still loading:\n%s", v)
	}
}

// ---------------------------------------------------------------------------
// Grouped host view (Task 7): fold, header aggregates, grouped rendering.
// ---------------------------------------------------------------------------

// unmanagedGroupOf builds the synthetic bucket the host-wide fetch produces for
// containers with no compose project label.
func unmanagedGroupOf(services ...string) svcGroup {
	return svcGroup{
		proj:     compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true},
		services: services,
	}
}

// groupedScreenModel is groupedModel plus the facts the renderer and the key
// dispatch need: grouped mode, a terminal size and a live search-match cache.
func groupedScreenModel(groups ...svcGroup) Model {
	m := groupedModel(groups...)
	m.grouped = true
	m.width, m.height = 100, 30
	return m
}

// pressGroupKey drives one key through Update. It is the string-keyed sibling
// of pressKey (help_test.go), because space arrives as its own tea.KeyType.
func pressGroupKey(m Model, key string) Model {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	switch key {
	case " ":
		msg = tea.KeyMsg{Type: tea.KeySpace}
	case "left":
		msg = tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		msg = tea.KeyMsg{Type: tea.KeyRight}
	}
	updated, _ := m.Update(msg)
	return updated.(Model)
}

// TestGroupedSpace_OnHeaderFoldsAndUnfolds pins space's first meaning: on a
// group header it folds, which hides ROWS and nothing else — the selection the
// group carried survives, because svcRefs ignores fold state.
func TestGroupedSpace_OnHeaderFoldsAndUnfolds(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "postgres"))
	m.selected[svcKey("web", "api")] = true
	m.svcCursor = 0 // the "web" header

	m = pressGroupKey(m, " ")
	if !m.svcGroups[0].folded {
		t.Fatal("space on a header did not fold the group")
	}
	if got := len(m.svcEntries); got != 3 {
		t.Errorf("folded row count = %d, want 3 (two headers + the open group's one service)", got)
	}
	if !m.selected[svcKey("web", "api")] {
		t.Error("folding cleared the group's selection; folding hides rows, not services")
	}
	if got := m.selectedCount(); got != 1 {
		t.Errorf("selectedCount after fold = %d, want 1", got)
	}

	m = pressGroupKey(m, " ")
	if m.svcGroups[0].folded {
		t.Fatal("a second space did not unfold the group")
	}
	if got := len(m.svcEntries); got != 5 {
		t.Errorf("unfolded row count = %d, want 5", got)
	}
}

// The fold rebuild renumbers svcEntries, and searchMatches holds ROW indices —
// so it must be re-derived, exactly as the grouped reload does.
func TestGroupedSpace_FoldRecomputesSearchMatches(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "api-db"))
	m.searchQuery = "api"
	m.searchMatches = computeMatches(m.svcEntries, m.searchQuery)
	if want := []int{1, 4}; !slices.Equal(m.searchMatches, want) {
		t.Fatalf("pre-fold matches = %v, want %v", m.searchMatches, want)
	}

	m.svcCursor = 0
	m = pressGroupKey(m, " ")
	if want := []int{2}; !slices.Equal(m.searchMatches, want) {
		t.Errorf("post-fold matches = %v, want %v (folded rows are gone, the rest renumbered)", m.searchMatches, want)
	}
}

// TestGroupedSpace_OnServiceSelects pins space's second meaning, unchanged from
// the drilled screen.
func TestGroupedSpace_OnServiceSelects(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"))
	m.svcCursor = 1 // the "api" row

	m = pressGroupKey(m, " ")
	if !m.selected[svcKey("web", "api")] {
		t.Fatal("space on a service row did not select it")
	}
	if m.svcGroups[0].folded {
		t.Error("space on a service row folded its group")
	}

	m = pressGroupKey(m, " ")
	if m.selected[svcKey("web", "api")] {
		t.Error("space on a selected service row did not deselect it")
	}
}

// An unmanaged container has no compose project behind it, so its row draws no
// checkbox and space must not fill one in.
func TestGroupedSpace_OnUnmanagedRowIsNoOp(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"), unmanagedGroupOf("watchtower"))
	m.svcCursor = 3 // the watchtower row

	m = pressGroupKey(m, " ")
	if len(m.selected) != 0 {
		t.Errorf("space selected an unmanaged row: %v", m.selected)
	}
	if m.svcGroups[1].folded {
		t.Error("space on an unmanaged SERVICE row folded its group")
	}
}

// foldedCount is the fold state of the whole screen in one number, for the
// tests that assert on all of it.
func foldedCount(m Model) int {
	n := 0
	for _, g := range m.svcGroups {
		if g.folded {
			n++
		}
	}
	return n
}

// groupedLanding drives a real landing: enterGroupedContainers arms the
// one-shot fold, and the load batch it returns carries the payload that
// consumes it.
func groupedLanding(t *testing.T, m Model) Model {
	t.Helper()
	cmd := m.enterGroupedContainers()
	if cmd == nil {
		t.Fatal("enterGroupedContainers() returned no load batch")
	}
	updated, _ := m.Update(cmd())
	return updated.(Model)
}

// A host running several projects lands FOLDED: the header aggregates are the
// host summary, and ten projects of eight services bury them under ninety rows.
func TestGroupedLanding_MultipleGroupsArriveFolded(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedLanding(t, groupedTestModel(g, projects))

	if len(m.svcGroups) != 3 {
		t.Fatalf("landed with %d groups, want 3", len(m.svcGroups))
	}
	if got := foldedCount(m); got != 3 {
		t.Errorf("%d of 3 groups folded on landing", got)
	}
	if got := len(m.svcEntries); got != 3 {
		t.Errorf("row count = %d, want 3 (one header per folded group)", got)
	}
}

// A single-project host lands OPEN. Folding it would leave one header row and
// nothing else on screen, which hides the whole host to summarise one project.
func TestGroupedLanding_SingleGroupArrivesOpen(t *testing.T) {
	g := &mockGrouper{groupedStatus: map[string]map[string]runner.ServiceStatus{
		"blog": {"web": {Running: true}, "api": {}},
	}}
	projects := []compose.Project{{Name: "blog", ConfigDir: "/srv/blog"}}
	m := groupedLanding(t, groupedTestModel(g, projects))

	if len(m.svcGroups) != 1 {
		t.Fatalf("landed with %d groups, want 1", len(m.svcGroups))
	}
	if m.svcGroups[0].folded {
		t.Fatal("a lone group must land open; folded it shows one header and no rows")
	}
	if got := len(m.svcEntries); got != 2 {
		t.Errorf("row count = %d, want 2 (the group's services, no header)", got)
	}
}

// The fold is ONE-SHOT. The grouped servicesMsg branch is also the 5-second
// reload, so a group the user opened must stay open across every later payload.
func TestGroupedLanding_FoldFiresOnceNotOnEveryReload(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedLanding(t, groupedTestModel(g, projects))
	if m.groupFoldPending {
		t.Fatal("the landing payload must consume the one-shot")
	}

	m.svcCursor = 0
	m = pressGroupKey(m, "right")
	if m.svcGroups[0].folded {
		t.Fatal("precondition: the user opened the first group")
	}

	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	if m.svcGroups[0].folded {
		t.Error("the 5-second reload re-folded a group the user opened")
	}
	if got := foldedCount(m); got != 2 {
		t.Errorf("%d groups folded after the reload, want 2 (the two untouched ones)", got)
	}
}

// The flag is cleared whether or not it folded, so a host that GAINS a second
// project does not fold under an ordinary reload.
func TestGroupedLanding_SingleGroupHostDoesNotFoldWhenItGrows(t *testing.T) {
	g := &mockGrouper{groupedStatus: map[string]map[string]runner.ServiceStatus{
		"blog": {"web": {Running: true}},
	}}
	projects := []compose.Project{{Name: "blog", ConfigDir: "/srv/blog"}}
	m := groupedTestModel(g, projects)
	m = groupedLanding(t, m)

	g.groupedStatus["shop"] = map[string]runner.ServiceStatus{"api": {Running: true}}
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return []compose.Project{{Name: "blog", ConfigDir: "/srv/blog"}, {Name: "shop", ConfigDir: "/srv/shop"}}, nil
	}
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	if got := foldedCount(m); got != 0 {
		t.Errorf("%d groups folded on a plain reload; the one-shot was already spent", got)
	}
}

// Drill-out lands through enterGroupedContainers like every other entry, so it
// re-folds — that is the intent, not a side effect.
func TestGroupedLanding_DrillOutRefolds(t *testing.T) {
	m, _ := drillTestModel(t)
	m.svcCursor = 2 // the shop header
	updated, _ := m.Update(keyMsgFor("enter"))
	m = updated.(Model)
	if m.grouped {
		t.Fatal("precondition: drill-in should have left grouped mode")
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("drill-out must reload the host view")
	}
	updated, _ = m.Update(cmd())
	m = updated.(Model)

	if got := foldedCount(m); got != len(m.svcGroups) || len(m.svcGroups) != 3 {
		t.Errorf("%d of %d groups folded after drill-out, want all 3", got, len(m.svcGroups))
	}
}

// z folds from a SERVICE row, which is the whole point: space folds only from
// the header, twenty rows above. The cursor lands on the header, because the
// row it sat on is gone.
func TestGroupedFold_ZFoldsFromAServiceRow(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "postgres"))
	m.selected[svcKey("web", "nginx")] = true
	m.svcCursor = 2 // the "nginx" row, inside the web group

	m = pressGroupKey(m, "z")

	if !m.svcGroups[0].folded {
		t.Fatal("z on a service row did not fold its group")
	}
	if m.svcCursor != 0 {
		t.Errorf("svcCursor = %d, want 0 (the folded group's own header)", m.svcCursor)
	}
	if !m.selected[svcKey("web", "nginx")] {
		t.Error("folding dropped the selection; it hides rows, not services")
	}

	m = pressGroupKey(m, "z")
	if m.svcGroups[0].folded {
		t.Error("a second z did not unfold the group")
	}
}

// Z is one key with the shape `a` already uses: any group open means fold all,
// otherwise unfold all.
func TestGroupedFold_ZAllTogglesEveryGroup(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"), unmanagedGroupOf("watchtower"))
	m.svcGroups[1].folded = true
	m.setGroups(m.svcGroups)
	m.svcCursor = 1 // the "api" row

	m = pressGroupKey(m, "Z")
	if got := foldedCount(m); got != 3 {
		t.Fatalf("%d of 3 groups folded, want all — one open group means fold all", got)
	}
	if m.svcCursor != 0 {
		t.Errorf("svcCursor = %d, want 0 (the cursor group's header)", m.svcCursor)
	}

	m = pressGroupKey(m, "Z")
	if got := foldedCount(m); got != 0 {
		t.Errorf("%d groups still folded, want 0 — all closed means unfold all", got)
	}
}

// ← and → are directional and idempotent, unlike z.
func TestGroupedFold_ArrowsAreDirectional(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "postgres"))
	m.svcCursor = 2 // the "nginx" row

	m = pressGroupKey(m, "left")
	if !m.svcGroups[0].folded {
		t.Fatal("left did not fold the cursor's group")
	}
	m = pressGroupKey(m, "left")
	if !m.svcGroups[0].folded {
		t.Error("a second left unfolded the group; left only ever folds")
	}

	m = pressGroupKey(m, "right")
	if m.svcGroups[0].folded {
		t.Fatal("right did not unfold the group")
	}
	m = pressGroupKey(m, "right")
	if m.svcGroups[0].folded {
		t.Error("a second right folded the group; right only ever unfolds")
	}
	if m.svcGroups[1].folded {
		t.Error("the arrows touched a group the cursor was not in")
	}

	// A no-op fold must not move the cursor either: right lands on an open
	// group routinely, and re-aiming at the header there is not a no-op.
	m.svcCursor = 2 // the "nginx" row again
	m = pressGroupKey(m, "right")
	if m.svcCursor != 2 {
		t.Errorf("svcCursor = %d after a no-op right, want 2", m.svcCursor)
	}
}

// The fold keys are grouped-only and write-only: the drilled screen has one
// group and no header to fold, and a read-only host must advertise no no-op.
func TestGroupedFold_KeysAreInertDrilledAndReadOnly(t *testing.T) {
	keys := []string{"z", "Z", "left", "right"}

	for _, key := range keys {
		m := singleGroupModel([]string{"api", "web"})
		m.screen = screenSelectContainers
		m.width, m.height = 100, 30

		m = pressGroupKey(m, key)
		if m.svcGroups[0].folded || len(m.svcEntries) != 2 {
			t.Errorf("drilled %q: folded=%v rows=%d, want false/2", key, m.svcGroups[0].folded, len(m.svcEntries))
		}
	}

	for _, key := range keys {
		m := groupedScreenModel(unmanagedGroupOf("watchtower", "portainer"))
		if !m.readOnly() {
			t.Fatal("precondition: an unmanaged-only host reads as read-only")
		}

		m = pressGroupKey(m, key)
		if m.svcGroups[0].folded {
			t.Errorf("read-only %q folded the unmanaged bucket", key)
		}
	}
}

// A fold renumbers every row, and searchMatches holds ROW indices — the new
// keys owe the same re-derive the space path already does.
func TestGroupedFold_KeysRecomputeSearchMatches(t *testing.T) {
	for _, key := range []string{"z", "Z", "left"} {
		m := groupedScreenModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "api-db"))
		m.searchQuery = "api"
		m.searchMatches = computeMatches(m.svcEntries, m.searchQuery)
		if want := []int{1, 4}; !slices.Equal(m.searchMatches, want) {
			t.Fatalf("%q: pre-fold matches = %v, want %v", key, m.searchMatches, want)
		}
		m.svcCursor = 1 // the "api" row, inside the web group

		m = pressGroupKey(m, key)

		want := []int{2}
		if key == "Z" {
			want = nil
		}
		if !slices.Equal(m.searchMatches, want) {
			t.Errorf("%q: post-fold matches = %v, want %v", key, m.searchMatches, want)
		}
	}
}

// The fold aims the cursor at the group it just closed, never at the first
// header on screen: closing the SECOND project from one of its rows must leave
// the cursor on that project, or every fold jumps to the top of the host.
func TestGroupedFold_AimsAtTheCursorsOwnGroupHeader(t *testing.T) {
	for _, key := range []string{"z", "left", "Z"} {
		m := groupedScreenModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "postgres", "redis"))
		m.svcCursor = 5 // "redis", the last row of the SECOND group

		m = pressGroupKey(m, key)

		if !m.svcGroups[1].folded {
			t.Fatalf("%q did not fold the cursor's group", key)
		}
		want := 3 // the db header, still row 3 with web open
		if key == "Z" {
			want = 1 // both folded: the web header, then db's
		}
		if m.svcCursor != want {
			t.Errorf("%q: svcCursor = %d, want %d (the cursor group's OWN header)", key, m.svcCursor, want)
		}
	}
}

// A fold shrinks the row count under a scrolled window, so it owes the same
// re-clamp every cursor move does — without it the window runs past the last
// row and the cursor sits outside it.
func TestGroupedFold_ReclampsTheScrollWindow(t *testing.T) {
	build := func() Model {
		var web, db []string
		for i := 0; i < 10; i++ {
			web = append(web, fmt.Sprintf("web-%02d", i))
			db = append(db, fmt.Sprintf("db-%02d", i))
		}
		m := groupedScreenModel(svcGroupOf("web", web...), svcGroupOf("db", db...))
		m.height = 12
		m.svcCursor = len(m.svcEntries) - 1
		m.fixSvcOffset()
		return m
	}

	for _, key := range []string{"z", "Z", "left"} {
		m := build()
		if m.svcOffset == 0 {
			t.Fatalf("%q: precondition: the list must scroll at this height", key)
		}

		m = pressGroupKey(m, key)

		visible := m.svcVisibleCount()
		maxOffset := len(m.svcEntries) - visible
		if maxOffset < 0 {
			maxOffset = 0
		}
		if m.svcOffset > maxOffset {
			t.Errorf("%q: svcOffset = %d, want at most %d; the window runs past the last row", key, m.svcOffset, maxOffset)
		}
		if m.svcCursor < m.svcOffset || m.svcCursor >= m.svcOffset+visible {
			t.Errorf("%q: cursor %d outside the window [%d,%d)", key, m.svcCursor, m.svcOffset, m.svcOffset+visible)
		}
	}
}

// The typing intercept sits ABOVE the key switch, so z and Z are literal runes
// while the search bar is open — a query naming zookeeper has to be typable.
func TestGroupedFold_KeysAreLiteralWhileSearching(t *testing.T) {
	for _, key := range []string{"z", "Z"} {
		m := groupedScreenModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "zookeeper"))
		m.searchInput = textinput.New()
		m.searchInput.Focus()
		m.searching = true

		m = pressGroupKey(m, key)

		if m.searchQuery != key {
			t.Errorf("%q: searchQuery = %q, want the keystroke in the bar", key, m.searchQuery)
		}
		if foldedCount(m) != 0 {
			t.Errorf("%q folded a group instead of typing", key)
		}
		if !m.searching {
			t.Errorf("%q closed the search bar", key)
		}
	}
}

// An svcErr replaces the whole list with the error screen, so every row key
// goes inert — the fold keys included, or the user reshapes a list that is not
// on screen and cannot see what changed.
func TestGroupedFold_KeysInertOnTheErrorScreen(t *testing.T) {
	for _, key := range []string{"z", "Z", "left", "right"} {
		m := groupedScreenModel(svcGroupOf("web", "api", "nginx"), svcGroupOf("db", "postgres"))
		m.svcGroups[1].folded = true
		m.setGroups(m.svcGroups)
		m.svcErr = errors.New("docker daemon gone")
		m.svcCursor = 3 // the folded db header: z and right would open it, Z would close web

		m = pressGroupKey(m, key)

		if m.svcGroups[0].folded || !m.svcGroups[1].folded {
			t.Errorf("%q changed the fold state behind the error screen (web folded=%v, db folded=%v)",
				key, m.svcGroups[0].folded, m.svcGroups[1].folded)
		}
	}
}

// The confirmation intercept swallows the fold keys too. The armed prompt names
// a batch that enter resolves AGAIN from the cursor, and a fold re-aims the
// cursor at a group header — so folding behind the prompt edits its target.
func TestGroupedFold_KeysInertWhileAConfirmationIsArmed(t *testing.T) {
	for _, key := range []string{"z", "Z", "left", "right"} {
		m := groupedOpModel(t)
		m.width, m.height = 100, 24
		m.svcCursor = 3 // shop/api, a service row inside the second group
		armed, _ := m.Update(keyMsgFor("d"))
		m = armed.(Model)
		if !m.confirming {
			t.Fatalf("precondition: d did not arm a prompt; warning = %q", m.warning)
		}

		m = pressGroupKey(m, key)

		if !m.confirming {
			t.Errorf("%q dismissed the armed prompt", key)
		}
		if foldedCount(m) != 0 {
			t.Errorf("%q folded a group behind the armed prompt", key)
		}
		if m.svcCursor != 3 {
			t.Errorf("%q moved the cursor to row %d, retargeting the armed batch", key, m.svcCursor)
		}
	}
}

// The one-shot fold runs BEFORE the branch re-derives anything from the rows.
// searchMatches holds ROW indices, and the landing payload is async: the user
// can open the search bar while it is still in flight.
func TestGroupedLanding_FoldsBeforeTheRowDerivedStateSettles(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	cmd := m.enterGroupedContainers()
	if cmd == nil {
		t.Fatal("enterGroupedContainers() returned no load batch")
	}
	m.searchInput = textinput.New()
	m.searchInput.Focus()
	m.searching = true
	m.searchQuery = "web"

	updated, _ := m.Update(cmd())
	got := updated.(Model)

	if n := foldedCount(got); n != len(got.svcGroups) || n == 0 {
		t.Fatalf("precondition: %d of %d groups folded on landing", n, len(got.svcGroups))
	}
	if len(got.searchMatches) != 0 {
		t.Errorf("searchMatches = %v; every group is folded, so no service row can match — the matches were derived before the fold",
			got.searchMatches)
	}
}

// The unmanaged bucket's own header still folds — the row is a header like any
// other, and folding is display state, not an operation.
func TestGroupedSpace_OnUnmanagedHeaderFolds(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"), unmanagedGroupOf("watchtower"))
	m.svcCursor = 2 // the "(unmanaged)" header

	m = pressGroupKey(m, " ")
	if !m.svcGroups[1].folded {
		t.Error("space on the unmanaged header did not fold it")
	}
}

// `a` and allSelected go through eachSelectableRef: an unmanaged row draws no
// checkbox, so select-all must not claim to have ticked it.
func TestGroupedSelectAll_SkipsUnmanagedRows(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api", "nginx"), unmanagedGroupOf("watchtower"))

	m = pressGroupKey(m, "a")
	want := map[string]bool{svcKey("web", "api"): true, svcKey("web", "nginx"): true}
	if len(m.selected) != len(want) {
		t.Fatalf("select-all produced %v, want %v", m.selected, want)
	}
	for k := range want {
		if !m.selected[k] {
			t.Errorf("select-all missed %q", k)
		}
	}
	if !m.allSelected() {
		t.Error("allSelected = false after `a`; the unmanaged row must not hold it open")
	}

	m = pressGroupKey(m, "a")
	if m.selectedCount() != 0 {
		t.Errorf("a second `a` did not clear the selection: %v", m.selected)
	}
}

// A host with ONLY unmanaged containers has nothing selectable, so allSelected
// must stay false rather than report an empty set as fully selected.
func TestAllSelected_UnmanagedOnlyHostIsFalse(t *testing.T) {
	m := groupedScreenModel(unmanagedGroupOf("watchtower", "portainer"))
	if m.allSelected() {
		t.Error("allSelected = true on a host with no selectable rows")
	}
}

// A host that runs nothing but unmanaged containers is the degenerate grouped
// render: one group, so no header and no indent, and every row is unselectable.
// The screen must still draw, and every write key must refuse.
func TestGroupedScreen_UnmanagedOnlyHost(t *testing.T) {
	m := groupedScreenModel(unmanagedGroupOf("watchtower", "portainer"))
	m.svcStatus = map[string]runner.ServiceStatus{
		svcKey(compose.UnmanagedProjectName, "watchtower"): {Running: true},
		svcKey(compose.UnmanagedProjectName, "portainer"):  {Running: true},
	}

	out := ansi.Strip(m.viewSelectContainers())
	if strings.Contains(out, compose.UnmanagedProjectName) {
		t.Errorf("a lone group must emit no header:\n%s", out)
	}
	rows := 0
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, "watchtower") && !strings.Contains(l, "portainer") {
			continue
		}
		rows++
		if strings.Contains(l, "[") {
			t.Errorf("unmanaged row = %q, want no checkbox", l)
		}
	}
	if rows != 2 {
		t.Fatalf("rendered %d container rows, want 2:\n%s", rows, out)
	}

	if got := pressGroupKey(m, "a"); got.selectedCount() != 0 {
		t.Errorf("`a` selected %d rows on an unmanaged-only host", got.selectedCount())
	}
	for _, key := range []string{"d", "r", "s", "R", "c"} {
		got := pressGroupKey(m, key)
		if got.confirming {
			t.Errorf("%q armed an operation on an unmanaged-only host", key)
		}
		if got.screen != screenSelectContainers {
			t.Errorf("%q navigated away; screen = %v", key, got.screen)
		}
	}
}

// TestGroupHeaderLine_Aggregates pins the header's live summary: the running
// count always, the unhealthy count only when something is wrong.
func TestGroupHeaderLine_Aggregates(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api", "nginx", "cache"), svcGroupOf("db", "postgres"))
	m.svcStatus = map[string]runner.ServiceStatus{
		svcKey("web", "api"):     {Running: true},
		svcKey("web", "nginx"):   {Running: true, Health: "unhealthy"},
		svcKey("web", "cache"):   {},
		svcKey("db", "postgres"): {},
	}

	got := ansi.Strip(m.groupHeaderLine(0))
	if !strings.Contains(got, "▼ web") {
		t.Errorf("header = %q, want the open marker and the name", got)
	}
	if !strings.Contains(got, "● 2 up") {
		t.Errorf("header = %q, want the running count", got)
	}
	if !strings.Contains(got, "✗ 1") {
		t.Errorf("header = %q, want the unhealthy count", got)
	}

	// Nothing unhealthy: the ✗ cell is absent entirely, not "✗ 0".
	got = ansi.Strip(m.groupHeaderLine(1))
	if !strings.Contains(got, "● 0 up") {
		t.Errorf("header = %q, want `● 0 up` for a stopped group", got)
	}
	if strings.Contains(got, "✗") {
		t.Errorf("header = %q, want no unhealthy cell when nothing is unhealthy", got)
	}
}

// A folded group keeps its aggregates — that is the whole point of folding one.
func TestGroupHeaderLine_FoldedKeepsAggregates(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"))
	m.svcStatus = map[string]runner.ServiceStatus{svcKey("web", "api"): {Running: true}}
	m.svcGroups[0].folded = true

	got := ansi.Strip(m.groupHeaderLine(0))
	if !strings.HasPrefix(got, "▶ web") {
		t.Errorf("folded header = %q, want the ▶ marker", got)
	}
	if !strings.Contains(got, "● 1 up") {
		t.Errorf("folded header = %q, want the running count", got)
	}
}

// A group that owns no services renders a bare header: "0 up" for a project
// with nothing to run reads as a fault it is not.
func TestGroupHeaderLine_EmptyGroupIsBare(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"), svcGroupOf("empty"))
	if got := ansi.Strip(m.groupHeaderLine(1)); got != "▶ empty" && got != "▼ empty" {
		t.Errorf("empty group header = %q, want the marker and the name only", got)
	}
}

// An out-of-range index yields "" rather than panicking, matching groupProjName.
func TestGroupHeaderLine_OutOfRange(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"))
	if got := m.groupHeaderLine(7); got != "" {
		t.Errorf("groupHeaderLine(7) = %q, want the empty string", got)
	}
}

// TestViewSelectContainers_GroupedIndent pins the tree shape: service rows sit
// 2 cells in from their header, the caption follows them, and the header itself
// does not move.
func TestViewSelectContainers_GroupedIndent(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"))
	m.svcStatus = map[string]runner.ServiceStatus{
		svcKey("web", "api"):     {Running: true, Uptime: "3h"},
		svcKey("db", "postgres"): {Running: true, Uptime: "2d"},
	}

	m.svcCursor = 1 // off the header, so the cursor prefix does not skew it
	lines := strings.Split(ansi.Strip(m.viewSelectContainers()), "\n")
	find := func(sub string) string {
		t.Helper()
		for _, l := range lines {
			if strings.Contains(l, sub) {
				return l
			}
		}
		t.Fatalf("no line containing %q in:\n%s", sub, strings.Join(lines, "\n"))
		return ""
	}
	// Display COLUMN, not byte offset: ● and the fold markers are multi-byte.
	col := func(line, sub string) int {
		t.Helper()
		i := strings.Index(line, sub)
		if i < 0 {
			t.Fatalf("%q does not contain %q", line, sub)
		}
		return ansi.StringWidth(line[:i])
	}

	if got := find("▼ web"); !strings.HasPrefix(got, "  ▼ web") {
		t.Errorf("header line = %q, want it at the cursor column", got)
	}
	// cursor(2) + indent(2) + checkbox(3) = the name column starts at 12.
	if got := find(" api"); !strings.HasPrefix(got, ">   [") {
		t.Errorf("service line = %q, want the cursor prefix then the 2-cell indent", got)
	}
	if got := find(" postgres"); !strings.HasPrefix(got, "    [") {
		t.Errorf("service line = %q, want a 2-cell indent before the checkbox", got)
	}
	if got := col(find("Service"), "Service"); got != 12 {
		t.Errorf("caption puts Service at column %d, want 12 (10 + the 2-cell indent)", got)
	}
	if got := col(find(" api"), "api"); got != 12 {
		t.Errorf("row puts the name at column %d, want 12 (aligned with the caption)", got)
	}
}

// A grouped host that holds exactly ONE project draws no headers, so it must
// draw no indent either — it renders exactly as the drilled screen does.
func TestViewSelectContainers_GroupedSingleProjectHasNoIndent(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"))
	m.svcStatus = map[string]runner.ServiceStatus{svcKey("web", "api"): {Running: true, Uptime: "3h"}}

	out := ansi.Strip(m.viewSelectContainers())
	if strings.Contains(out, "▼ web") {
		t.Errorf("a single group must emit no header:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "api") && !strings.HasPrefix(l, "> [") {
			t.Errorf("single-group service line = %q, want no indent", l)
		}
	}
}

// Unmanaged rows keep the checkbox's 3 cells but draw no checkbox in them, so
// the columns stay aligned with the compose groups around them.
func TestViewSelectContainers_UnmanagedRowHasNoCheckbox(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"), unmanagedGroupOf("watchtower"))
	m.svcStatus = map[string]runner.ServiceStatus{
		svcKey("web", "api"): {Running: true},
		svcKey(compose.UnmanagedProjectName, "watchtower"): {Running: true},
	}

	var svcLine, unmanagedLine string
	for _, l := range strings.Split(ansi.Strip(m.viewSelectContainers()), "\n") {
		switch {
		case strings.Contains(l, "api"):
			svcLine = l
		case strings.Contains(l, "watchtower"):
			unmanagedLine = l
		}
	}
	if svcLine == "" || unmanagedLine == "" {
		t.Fatal("both a compose row and an unmanaged row must render")
	}
	if !strings.HasPrefix(svcLine, "    [ ]") {
		t.Errorf("compose row = %q, want a checkbox", svcLine)
	}
	if strings.Contains(unmanagedLine, "[") {
		t.Errorf("unmanaged row = %q, want no checkbox", unmanagedLine)
	}
	if a, b := strings.Index(svcLine, "api"), strings.Index(unmanagedLine, "watchtower"); a != b {
		t.Errorf("name columns disagree: compose at %d, unmanaged at %d", a, b)
	}
}

// The title's denominator counts selectable rows, so `a` cannot look like it
// left rows behind.
func TestViewSelectContainers_TitleCountsSelectableRowsOnly(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api", "nginx"), unmanagedGroupOf("watchtower"))
	m = pressGroupKey(m, "a")

	if got := ansi.Strip(m.viewSelectContainers()); !strings.Contains(got, "(2/2 selected)") {
		t.Errorf("title must read (2/2 selected) after `a`; got:\n%s", got)
	}
}

// The scroll indicators follow the service rows' indent so the windowed list
// keeps one left edge.
func TestViewSelectContainers_GroupedScrollIndicatorsIndent(t *testing.T) {
	many := make([]string, 20)
	for i := range many {
		many[i] = fmt.Sprintf("svc%02d", i)
	}
	m := groupedScreenModel(svcGroupOf("web", many...), svcGroupOf("db", "postgres"))
	m.height = 14
	m.svcCursor = 10
	m.fixSvcOffset()

	out := ansi.Strip(m.viewSelectContainers())
	for _, marker := range []string{"▲", "▼"} {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, marker+" ") && strings.Contains(l, "more") {
				if !strings.HasPrefix(l, "    "+marker) {
					t.Errorf("indicator line = %q, want the 2-cell indent", l)
				}
			}
		}
	}
}

// TestContainerFooter_GroupedIdlePair pins the third footer variant: space
// names both meanings, enter names the row kind only grouped mode has, and the
// drilled pair is untouched.
func TestContainerFooter_GroupedIdlePair(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"))
	line1, line2 := m.containerHelpLines()
	if !strings.Contains(line1, "space fold/select") {
		t.Errorf("grouped line1 = %q, want the fold/select token", line1)
	}
	if !strings.Contains(line2, "enter drill in") {
		t.Errorf("grouped line2 = %q, want the drill-in token", line2)
	}

	m.grouped = false
	if l1, _ := m.containerHelpLines(); !strings.Contains(l1, "space toggle") {
		t.Errorf("drilled line1 = %q, want the unchanged toggle token", l1)
	}
}

// The line COUNT must stay state-independent: containerFooterLines reads the
// IDLE pair alone, so opening or committing a search cannot resize the list.
func TestContainerFooter_GroupedLineCountIsStateIndependent(t *testing.T) {
	for _, width := range []int{40, 50, 60, 80, 120} {
		base := groupedScreenModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"))
		base.width = width
		want := base.containerFooterLines()

		searching := base
		searching.searchInput = textinput.New()
		searching.searching = true
		if got := searching.containerFooterLines(); got != want {
			t.Errorf("width %d: searching footer lines = %d, want %d", width, got, want)
		}
		committed := base
		committed.searchQuery = "api"
		if got := committed.containerFooterLines(); got != want {
			t.Errorf("width %d: committed footer lines = %d, want %d", width, got, want)
		}
		confirming := base
		confirming.confirming = true
		if got := confirming.containerFooterLines(); got != want {
			t.Errorf("width %d: confirming footer lines = %d, want %d", width, got, want)
		}
	}
}

// Every rendered footer line is clamped, so the grouped pair can never wrap on
// a narrow pane and steal a service row.
func TestContainerFooter_GroupedClampsToWidth(t *testing.T) {
	for _, width := range []int{10, 20, 30, 45, 60} {
		m := groupedScreenModel(svcGroupOf("web", "api"), svcGroupOf("db", "postgres"))
		m.width = width
		lines := strings.Split(m.containerFooter(), "\n")
		// helpStyle's MarginTop(1) prepends a blank line; it is the gap
		// svcVisibleCount already reserves, not a footer row.
		if len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
		if got, want := len(lines), m.containerFooterLines(); got != want {
			t.Errorf("width %d: footer rendered %d lines, want %d", width, got, want)
		}
		for _, l := range lines {
			if w := ansi.StringWidth(l); w > width {
				t.Errorf("width %d: footer line %q is %d cells wide", width, ansi.Strip(l), w)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Drill-in, drill-out and the action-time composer (Task 8).
// ---------------------------------------------------------------------------

// capableComposer answers every capability the grouped read keys assert on, so
// a test that presses l / x / i / c fails on the BINDING rather than on a
// double that could never have served the key.
type capableComposer struct {
	mockComposer
	proj string
}

func (c *capableComposer) ConfigFile(context.Context) ([]byte, error) {
	return []byte("services: {}"), nil
}

func (c *capableComposer) ConfigResolved(context.Context) ([]byte, error) {
	return []byte("services: {}"), nil
}

func (c *capableComposer) EditCommand(context.Context) (*exec.Cmd, error) {
	return exec.Command("echo", "edit"), nil
}

func (c *capableComposer) ValidateConfig(context.Context) error { return nil }

func (c *capableComposer) ExecCommand(_ context.Context, service string, _ []string) (*exec.Cmd, error) {
	return exec.Command("echo", service), nil
}

func (c *capableComposer) Inspect(context.Context, string) ([]byte, error) {
	return []byte(inspectFixtureJSON), nil
}

// drillFactory hands out one capableComposer per project and keeps them, so a
// test can assert WHICH project's composer a key bound — the whole point of
// action-time binding is that two rows on one screen bind two different ones.
// The synthetic unmanaged project keeps returning the grouper, because that is
// the address hostGrouper() reaches the host-wide seam through.
type drillFactory struct {
	grouper *mockGrouper
	made    map[string]*capableComposer
}

func newDrillFactory(g *mockGrouper) *drillFactory {
	return &drillFactory{grouper: g, made: map[string]*capableComposer{}}
}

func (f *drillFactory) factory() ComposerFactory {
	return func(p compose.Project) runner.Composer {
		if p.Unmanaged {
			return f.grouper
		}
		c, ok := f.made[p.Name]
		if !ok {
			c = &capableComposer{proj: p.Name}
			c.services = []string{p.Name + "-api", p.Name + "-db"}
			f.made[p.Name] = c
		}
		return c
	}
}

// drillTestModel is a loaded grouped screen over groupedFixture's host. Its row
// order is fixed and every drill test indexes it directly:
//
//	0 ▼ blog · 1 web · 2 ▼ shop · 3 api · 4 db · 5 ▼ (unmanaged) · 6 watchtower
func drillTestModel(t *testing.T) (Model, *drillFactory) {
	t.Helper()
	g, projects := groupedFixture()
	f := newDrillFactory(g)
	m := NewModel(nil, io.Discard, f.factory(), nil, nil)
	installFakeTick(&m)
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) { return projects, nil }
	m.screen = screenSelectContainers
	m.grouped = true
	m.width, m.height = 120, 40
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	if len(m.svcEntries) != 7 {
		t.Fatalf("fixture drift: %d rows, want 7", len(m.svcEntries))
	}
	return m, f
}

func TestGroupedScreen_EnterDrillsIntoProject(t *testing.T) {
	m, f := drillTestModel(t)
	m.svcCursor = 2 // the shop header
	m.selected[svcKey("blog", "web")] = true
	m.searchQuery = "e"
	m.searchMatches = computeMatches(m.svcEntries, "e")
	status, stats, updates := m.statusSession, m.statsSession, m.updatesSession

	updated, cmd := m.Update(keyMsgFor("enter"))
	m = updated.(Model)

	if m.grouped {
		t.Error("enter on a group header must leave the grouped host view")
	}
	if m.screen != screenSelectContainers {
		t.Errorf("screen = %d, want the container screen (drill-in is not a new screen)", m.screen)
	}
	if m.composer != runner.Composer(f.made["shop"]) {
		t.Errorf("composer = %#v, want shop's own composer", m.composer)
	}
	if m.projName != "shop" || m.projDir != "/srv/shop" {
		t.Errorf("project identity = %q %q, want shop /srv/shop", m.projName, m.projDir)
	}
	if !strings.Contains(m.breadcrumb(), "shop") {
		t.Errorf("breadcrumb = %q, want it to name the drilled project", m.breadcrumb())
	}
	if len(m.svcGroups) != 1 || m.svcGroups[0].proj.Name != "shop" {
		t.Errorf("svcGroups = %+v, want the one drilled group", m.svcGroups)
	}
	if got := modelServices(m); !slices.Equal(got, []string{"api", "db"}) {
		t.Errorf("services = %v, want the group's rows painted immediately", got)
	}
	if !m.drilledFromHost {
		t.Error("the drilled screen must report a parent so esc can drill back out")
	}
	if len(m.selected) != 0 {
		t.Error("a selection that spanned projects must not survive the drill-in")
	}
	if m.searchQuery != "" || m.searchMatches != nil {
		t.Error("search is ephemeral across a drill-in")
	}
	if m.svcCursor != 0 || m.svcOffset != 0 {
		t.Errorf("cursor/offset = %d/%d, want the drilled list to start at the top", m.svcCursor, m.svcOffset)
	}
	if m.statusSession == status || m.statsSession == stats || m.updatesSession == updates {
		t.Error("drill-in swaps the composer: all three session counters must bump")
	}
	if cmd == nil {
		t.Error("drill-in must dispatch the single-project reload")
	}
}

// The drilled reload is loadServices, not the grouped fetch: only it can report
// a service the compose file declares but the host has never created.
func TestGroupedScreen_DrillInReloadsThroughTheGroupComposer(t *testing.T) {
	m, f := drillTestModel(t)
	m.svcCursor = 2 // the shop header

	updated, _ := m.Update(keyMsgFor("enter"))
	m = updated.(Model)

	msg, ok := m.loadServices()().(servicesMsg)
	if !ok {
		t.Fatal("the drilled screen must load through loadServices")
	}
	if msg.groupedPayload {
		t.Error("drilled mode must not fetch the host-wide payload")
	}
	if !slices.Equal(msg.services, f.made["shop"].services) {
		t.Errorf("services = %v, want the composer's full list %v", msg.services, f.made["shop"].services)
	}
}

// enter answers on a SERVICE row too: a service names its project as
// unambiguously as a header does, and binding the key to headers alone stranded
// the single-project host, where no header is emitted at all.
func TestGroupedScreen_EnterDrillsFromAServiceRow(t *testing.T) {
	m, f := drillTestModel(t)
	m.svcCursor = 3 // the shop/api service row

	updated, cmd := m.Update(keyMsgFor("enter"))
	got := updated.(Model)

	if got.grouped {
		t.Error("enter on a service row must drill into that row's project")
	}
	if got.composer != runner.Composer(f.made["shop"]) {
		t.Errorf("composer = %#v, want shop's own composer", got.composer)
	}
	if got.projName != "shop" {
		t.Errorf("projName = %q, want shop", got.projName)
	}
	if cmd == nil {
		t.Error("drill-in must dispatch the drilled reload")
	}
}

// A host running exactly ONE compose project emits no header (that degenerate
// render is what keeps the drilled screen byte-identical), so before enter
// answered on service rows the drilled screen — with it the never-created
// services, the automatic update scan and the project breadcrumb — was
// unreachable on the commonest remote-server shape there is.
func TestGroupedScreen_SingleProjectHostCanDrillIn(t *testing.T) {
	g := &mockGrouper{
		groupedStatus: map[string]map[string]runner.ServiceStatus{
			"shop": {"api": {Running: true}, "db": {}},
		},
	}
	f := newDrillFactory(g)
	m := NewModel(nil, io.Discard, f.factory(), nil, nil)
	installFakeTick(&m)
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return []compose.Project{{Name: "shop", ConfigDir: "/srv/shop"}}, nil
	}
	m.screen = screenSelectContainers
	m.grouped = true
	m.width, m.height = 120, 40
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	for _, e := range m.svcEntries {
		if e.kind == entrySvcGroupHeader {
			t.Fatal("a single group must still emit no header row")
		}
	}
	if len(m.svcEntries) != 2 {
		t.Fatalf("rows = %d, want the two service rows", len(m.svcEntries))
	}

	updated, _ = m.Update(keyMsgFor("enter"))
	got := updated.(Model)
	if got.grouped {
		t.Fatal("a single-project host must be able to reach the drilled screen")
	}
	if got.projName != "shop" || got.composer != runner.Composer(f.made["shop"]) {
		t.Errorf("drilled into %q with composer %#v, want shop", got.projName, got.composer)
	}
	if !got.autoUpdatesAllowed() {
		t.Error("the drilled screen must restore the automatic update scan")
	}
}

// On the drilled screen enter belongs to the confirmation prompt; with no
// prompt up it must stay inert rather than re-entering the project.
func TestDrilledScreen_EnterIsInert(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.setSingleGroup(mc.services)
	m.composer = mc

	updated, cmd := m.Update(keyMsgFor("enter"))
	got := updated.(Model)

	if got.confirming || got.screen != screenSelectContainers {
		t.Errorf("enter armed something: confirming=%v screen=%d", got.confirming, got.screen)
	}
	if cmd != nil {
		t.Error("idle enter on the drilled screen must dispatch nothing")
	}
}

// Drill-in then drill-out is a round trip: the composer the header bound is
// dropped and the host view reloads.
func TestGroupedScreen_DrillRoundTrip(t *testing.T) {
	m, _ := drillTestModel(t)
	m.svcCursor = 2 // the shop header
	updated, _ := m.Update(keyMsgFor("enter"))
	m = updated.(Model)
	if m.grouped {
		t.Fatal("precondition: drill-in should have left grouped mode")
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)

	if !m.grouped || m.screen != screenSelectContainers {
		t.Errorf("esc left screen %d grouped %v, want the grouped host view", m.screen, m.grouped)
	}
	if m.composer != nil {
		t.Error("drill-out must drop the project composer")
	}
	if m.projName != "" || m.projDir != "" {
		t.Error("drill-out must drop the project identity")
	}
	if m.drilledFromHost {
		t.Error("the grouped screen is not reached through a picker")
	}
	if cmd == nil {
		t.Error("drill-out must reload the host view")
	}
}

// TestEscChain_DrilledToGroupedToServer walks the whole back-navigation ladder
// the deleted project picker used to sit in the middle of: a drilled project
// esc's out to the grouped host view, and the host view esc's out to the server
// picker. Nothing between them is a screen of its own any more.
func TestEscChain_DrilledToGroupedToServer(t *testing.T) {
	g, projects := groupedFixture()
	f := newDrillFactory(g)
	m := NewModel(nil, io.Discard, f.factory(), testServers, mockConnectCb(&g.mockComposer))
	installFakeTick(&m)
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) { return projects, nil }
	m.screen = screenSelectContainers
	m.grouped = true
	m.serverName = "prod"
	m.disconnectFunc = func() error { return nil }
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	m.svcCursor = headerIndexFor(t, m.svcEntries, 1) // the shop header
	updated, _ = m.Update(keyMsgFor("enter"))
	m = updated.(Model)
	if m.grouped || m.projName != "shop" {
		t.Fatalf("drill-in left grouped=%v proj=%q, want the drilled shop screen", m.grouped, m.projName)
	}
	if !m.canGoBack() {
		t.Fatal("a drilled screen reached by drill-in must report a parent")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if !m.grouped || m.screen != screenSelectContainers {
		t.Fatalf("first esc left screen %d grouped %v, want the grouped host view", m.screen, m.grouped)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.screen != screenSelectServer {
		t.Fatalf("second esc left screen %d, want the server picker", m.screen)
	}
	if m.serverName != "" || m.disconnectFunc != nil {
		t.Error("esc to the server screen must drop the remote connection state")
	}
	if m.svcGroups != nil || m.svcEntries != nil {
		t.Error("esc to the server screen must drop the host rows")
	}
}

// TestRootScreenQ_ContainerModes pins the q rule for both modes of the one
// screen whose LEAVE binding is conditional. q quits ONLY where esc has no
// parent to reach; everywhere else it rewrites to esc, and the `?` overlay's
// LEAVE group reads the same predicate.
func TestRootScreenQ_ContainerModes(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*Model)
		servers    []config.Server
		wantQuit   bool
		wantScreen screen
	}{
		{
			name:     "grouped standalone quits",
			setup:    func(m *Model) { m.grouped = true },
			wantQuit: true,
		},
		{
			name:       "grouped with servers goes back",
			setup:      func(m *Model) { m.grouped = true },
			servers:    testServers,
			wantScreen: screenSelectServer,
		},
		{
			name:     "drilled standalone quits",
			setup:    func(m *Model) { m.setSingleGroup([]string{"web"}) },
			wantQuit: true,
		},
		{
			name: "drilled from the host view goes back",
			setup: func(m *Model) {
				m.drilledFromHost = true
				m.setSingleGroup([]string{"web"})
			},
			wantScreen: screenSelectContainers,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc := &mockComposer{services: []string{"web"}}
			m := NewModel(mc, io.Discard, mockFactory(mc), tt.servers, nil)
			installFakeTick(&m)
			m.screen = screenSelectContainers
			m.composer = mc
			tt.setup(&m)

			if got := m.canGoBack(); got == tt.wantQuit {
				t.Fatalf("canGoBack() = %v, want %v — q and the LEAVE group read it", got, !tt.wantQuit)
			}

			updated, cmd := m.Update(keyMsgFor("q"))
			um := updated.(Model)

			if tt.wantQuit {
				if cmd == nil {
					t.Fatal("q at a root screen must quit")
				}
				if _, ok := cmd().(tea.QuitMsg); !ok {
					t.Errorf("q returned %T, want tea.QuitMsg", cmd())
				}
				return
			}
			if um.screen != tt.wantScreen {
				t.Errorf("q left screen %d, want %d", um.screen, tt.wantScreen)
			}
			if tt.wantScreen == screenSelectContainers && !um.grouped {
				t.Error("q on a drilled screen must drill out to the grouped host view")
			}
		})
	}
}

// actionBindCase drives one read key from a grouped row and back again.
type actionBindCase struct {
	name       string
	key        string
	cursor     int
	wantProj   string
	wantScreen screen
}

func TestGroupedScreen_ReadKeysBindTheCursorGroupComposer(t *testing.T) {
	cases := []actionBindCase{
		{"logs", "l", 1, "blog", screenLogs},
		{"inspect", "i", 3, "shop", screenInspect},
		{"config", "c", 1, "blog", screenConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, f := drillTestModel(t)
			m.svcCursor = tc.cursor

			updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
			m = updated.(Model)

			if m.screen != tc.wantScreen {
				t.Fatalf("%q left screen %d, want %d", tc.key, m.screen, tc.wantScreen)
			}
			if m.composer != runner.Composer(f.made[tc.wantProj]) {
				t.Errorf("%q bound %#v, want %s's composer", tc.key, m.composer, tc.wantProj)
			}
			if !m.grouped {
				t.Error("an action key must not leave grouped mode; only enter drills in")
			}
			if cmd == nil {
				t.Errorf("%q should dispatch its fetch", tc.key)
			}

			updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
			m = updated.(Model)
			if m.screen != screenSelectContainers || !m.grouped {
				t.Fatalf("esc left screen %d grouped %v, want the grouped host view", m.screen, m.grouped)
			}
			if m.composer != nil {
				t.Errorf("%q: the action-time composer survived the return to the host view", tc.key)
			}
			if cmd == nil {
				t.Errorf("%q: returning to the host view must reload it", tc.key)
			}
		})
	}
}

func TestGroupedScreen_ExecBindsAcrossThePromptAndUnbinds(t *testing.T) {
	m, f := drillTestModel(t)
	m.svcCursor = 3 // shop/api, running in the fixture

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = updated.(Model)
	if !m.confirming || !m.pendingExec {
		t.Fatalf("x did not arm the exec prompt: confirming=%v pendingExec=%v", m.confirming, m.pendingExec)
	}
	if m.composer != runner.Composer(f.made["shop"]) {
		t.Errorf("x bound %#v, want shop's composer to survive the prompt", m.composer)
	}

	// Cancelling the prompt is a return to the idle host view: the composer goes.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.confirming || m.pendingExec {
		t.Error("esc must cancel the exec prompt")
	}
	if m.composer != nil {
		t.Error("cancelling the exec prompt must unbind the composer")
	}

	// And the completed exec unbinds it too.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = updated.(Model)
	updated, _ = m.Update(keyMsgFor("enter"))
	m = updated.(Model)
	updated, _ = m.Update(execDoneMsg{})
	m = updated.(Model)
	if m.composer != nil {
		t.Error("execDoneMsg must unbind the action-time composer")
	}
}

// The unmanaged bucket has no compose file, so c is refused before a composer
// is even bound — the key must not advertise a no-op by half-running.
func TestGroupedScreen_ConfigRefusedOnUnmanagedGroup(t *testing.T) {
	for _, cursor := range []int{5, 6} { // the unmanaged header and its row
		m, _ := drillTestModel(t)
		m.svcCursor = cursor

		updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
		got := updated.(Model)

		if got.screen != screenSelectContainers {
			t.Errorf("cursor %d: c opened screen %d on the unmanaged group", cursor, got.screen)
		}
		if got.composer != nil {
			t.Errorf("cursor %d: c bound a composer for the unmanaged group", cursor)
		}
		if cmd != nil {
			t.Errorf("cursor %d: c dispatched a command on the unmanaged group", cursor)
		}
	}
}

// A header row names a project, not a service, so the keys that act on ONE
// container leave it alone — and must not strand a bound composer behind.
func TestGroupedScreen_ServiceKeysInertOnAHeaderRow(t *testing.T) {
	for _, key := range []string{"l", "i", "x"} {
		m, _ := drillTestModel(t)
		m.svcCursor = 2 // the shop header

		updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		got := updated.(Model)

		if got.screen != screenSelectContainers || got.confirming {
			t.Errorf("%q acted on a header row: screen=%d confirming=%v", key, got.screen, got.confirming)
		}
		if got.composer != nil {
			t.Errorf("%q left a composer bound from a header row", key)
		}
		if cmd != nil {
			t.Errorf("%q dispatched a command from a header row", key)
		}
	}
}

// Binding an unmanaged group's read-only composer must not repaint the grouped
// screen as a read-only one: the binding is transient, the screen is not.
func TestGroupedScreen_BoundReadOnlyComposerDoesNotFlipTheScreen(t *testing.T) {
	m, _ := drillTestModel(t)
	m.composer = &readOnlyMockComposer{}

	if m.readOnly() {
		t.Error("grouped mode must not read a transiently bound composer as read-only")
	}
	line1, _ := m.containerHelpLines()
	if !strings.Contains(line1, "fold") {
		t.Errorf("footer = %q, want the grouped pair to survive a bound composer", line1)
	}
}

// --- Task 10: sequential batch pipeline -------------------------------------

// groupedUpdatesModel is groupedOpModel with a factory that hands out a
// DISTINCT composer per project, so a scan can be attributed to the project it
// was dispatched for. The row order it produces is the fixture's:
//
//	0 blog header   1 blog/web
//	2 shop header   3 shop/api   4 shop/db
//	5 (unmanaged) header   6 watchtower
func groupedUpdatesModel(t *testing.T) (Model, map[string]*mockComposer) {
	t.Helper()
	g, projects := groupedFixture()
	per := map[string]*mockComposer{
		"blog": {updates: map[string]bool{"web": true}},
		"shop": {updates: map[string]bool{"api": true, "db": false}},
	}
	m := groupedTestModel(g, projects)
	m.composerFactory = func(p compose.Project) runner.Composer {
		if p.Unmanaged {
			return g
		}
		if c, ok := per[p.Name]; ok {
			return c
		}
		return nil
	}
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	if got := len(m.svcEntries); got != 7 {
		t.Fatalf("precondition: %d rows, want 7", got)
	}
	return m, per
}

// pressU drives the U key and runs the Cmd it returns, delivering the resulting
// updatesMsg. It returns the model after the scan has landed.
func pressU(t *testing.T, m Model) Model {
	t.Helper()
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("U produced no scan command")
	}
	if !m.updateInFlight {
		t.Error("U must raise updateInFlight before the fetch returns")
	}
	if m.grouped && m.composer != nil {
		t.Error("grouped mode must not stay holding the composer U bound")
	}
	msg, ok := cmd().(updatesMsg)
	if !ok {
		t.Fatalf("U's command produced %T, want updatesMsg", cmd())
	}
	updated, _ = m.Update(msg)
	m = updated.(Model)
	if m.updateInFlight {
		t.Error("the arrival must clear updateInFlight")
	}
	return m
}

// U scans ONE group — the cursor row's — through that group's own composer, and
// files the verdicts under that group's cache key. Every other group is left
// untouched: no fetch, no cache entry, no glyph.
func TestGroupedU_ScansCursorGroupOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cursor int
	}{
		{"service row", 3},  // shop/api
		{"group header", 2}, // the shop header — a header IS its group
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, per := groupedUpdatesModel(t)
			m.svcCursor = tc.cursor
			m = pressU(t, m)

			if per["shop"].updatesCalls != 1 {
				t.Errorf("shop CheckUpdates ran %d times, want 1", per["shop"].updatesCalls)
			}
			if per["blog"].updatesCalls != 0 {
				t.Errorf("blog CheckUpdates ran %d times; U must not fan out across the host", per["blog"].updatesCalls)
			}
			shopKey := m.projUpdatesCacheKey(compose.Project{Name: "shop", ConfigDir: "/srv/shop"})
			if _, ok := m.updateCache[shopKey]; !ok {
				t.Fatalf("no cache entry under %q; cache = %v", shopKey, m.updateCache)
			}
			blogKey := m.projUpdatesCacheKey(compose.Project{Name: "blog", ConfigDir: "/srv/blog"})
			if _, ok := m.updateCache[blogKey]; ok {
				t.Errorf("a scan of shop wrote an entry under blog's key %q", blogKey)
			}
			if st := m.svcStatus[svcKey("shop", "api")]; st.UpdateAvailable == nil || !*st.UpdateAvailable {
				t.Errorf("shop/api verdict = %v, want true", st.UpdateAvailable)
			}
			if st := m.svcStatus[svcKey("shop", "db")]; st.UpdateAvailable == nil || *st.UpdateAvailable {
				t.Errorf("shop/db verdict = %v, want false", st.UpdateAvailable)
			}
			if st := m.svcStatus[svcKey("blog", "web")]; st.UpdateAvailable != nil {
				t.Errorf("blog/web verdict = %v, want nil (never scanned)", st.UpdateAvailable)
			}
		})
	}
}

// The unmanaged bucket keeps the "unmanaged|" prefix its key has always
// carried, so a local host cannot collide it with the fast-track slot.
func TestGroupedU_UnmanagedGroupKeepsCachePrefix(t *testing.T) {
	m, _ := groupedUpdatesModel(t)
	m.svcCursor = 5 // the (unmanaged) header

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("U on the unmanaged group produced no scan command")
	}
	msg := cmd().(updatesMsg)
	if !strings.HasPrefix(msg.forKey, "unmanaged|") {
		t.Errorf("forKey = %q, want the unmanaged prefix", msg.forKey)
	}
}

// The key travels WITH the message. Moving the cursor (or folding a group)
// while the scan runs must not re-file the verdicts under another project.
func TestGroupedU_ForKeySurvivesCursorMove(t *testing.T) {
	m, per := groupedUpdatesModel(t)
	m.svcCursor = 3 // shop/api

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	m = updated.(Model)
	msg := cmd().(updatesMsg)

	// The user walks back up to the blog group while the registry is busy.
	m.svcCursor = 1
	updated, _ = m.Update(msg)
	m = updated.(Model)

	shopKey := m.projUpdatesCacheKey(compose.Project{Name: "shop", ConfigDir: "/srv/shop"})
	if _, ok := m.updateCache[shopKey]; !ok {
		t.Fatalf("the verdicts were not filed under the scanned project; cache = %v", m.updateCache)
	}
	if len(m.updateCache) != 1 {
		t.Errorf("cache = %v, want exactly the scanned project's entry", m.updateCache)
	}
	if st := m.svcStatus[svcKey("blog", "web")]; st.UpdateAvailable != nil {
		t.Error("the cursor's group must not inherit another group's verdicts")
	}
	if per["blog"].updatesCalls != 0 {
		t.Error("moving the cursor must not trigger a scan of its own")
	}
}

// A second group's scan must not blank the first's glyphs — grouped mode holds
// several projects' verdicts at once, and each scan owns only its own.
func TestGroupedU_SecondScanKeepsFirstGroupsVerdicts(t *testing.T) {
	m, _ := groupedUpdatesModel(t)
	m.svcCursor = 1 // blog/web
	m = pressU(t, m)
	m.svcCursor = 3 // shop/api
	m = pressU(t, m)

	if st := m.svcStatus[svcKey("blog", "web")]; st.UpdateAvailable == nil || !*st.UpdateAvailable {
		t.Errorf("blog/web verdict = %v after scanning shop, want true", st.UpdateAvailable)
	}
	if st := m.svcStatus[svcKey("shop", "api")]; st.UpdateAvailable == nil || !*st.UpdateAvailable {
		t.Errorf("shop/api verdict = %v, want true", st.UpdateAvailable)
	}
}

// A failing scan blanks its OWN group only, and still raises the soft warning.
func TestGroupedU_FailureBlanksOnlyItsOwnGroup(t *testing.T) {
	m, per := groupedUpdatesModel(t)
	m.svcCursor = 1 // blog/web
	m = pressU(t, m)

	per["shop"].updatesErr = errors.New("registry unreachable")
	per["shop"].updates = nil
	m.svcCursor = 3 // shop/api
	m = pressU(t, m)

	if m.updatesErr == "" {
		t.Error("a failed scan must raise the soft warning")
	}
	if st := m.svcStatus[svcKey("blog", "web")]; st.UpdateAvailable == nil || !*st.UpdateAvailable {
		t.Errorf("blog/web verdict = %v; one project's failure must not discard another's", st.UpdateAvailable)
	}
	if st := m.svcStatus[svcKey("shop", "api")]; st.UpdateAvailable != nil {
		t.Errorf("shop/api verdict = %v, want nil after its own scan failed", st.UpdateAvailable)
	}
}

// The grouped payload is the 5-second refresh as well as the initial load, so
// it rebuilds svcStatus from scratch. Without the cache replay every glyph U
// painted would vanish on the next tick, with nothing queued to fetch it back.
func TestGroupedUpdates_SurviveAPeriodicReload(t *testing.T) {
	m, _ := groupedUpdatesModel(t)
	m.svcCursor = 3
	m = pressU(t, m)

	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	if st := m.svcStatus[svcKey("shop", "api")]; st.UpdateAvailable == nil || !*st.UpdateAvailable {
		t.Errorf("shop/api verdict = %v after a reload, want true", st.UpdateAvailable)
	}
}

// The header aggregates the group's cached verdicts, so a FOLDED group still
// reports how much of it is out of date.
func TestGroupedHeader_ShowsUpdateCount(t *testing.T) {
	m, _ := groupedUpdatesModel(t)
	m.svcCursor = 3
	m = pressU(t, m)

	shopHeader := m.groupHeaderLine(1)
	if !strings.Contains(shopHeader, compose.UpdateGlyph+" 1") {
		t.Errorf("shop header = %q, want the update glyph and a count of 1", shopHeader)
	}
	if blogHeader := m.groupHeaderLine(0); strings.Contains(blogHeader, compose.UpdateGlyph) {
		t.Errorf("blog header = %q; an unscanned group must report no updates", blogHeader)
	}

	// Fold shop: the rows go, the aggregate stays.
	m.svcCursor = 2
	m = pressGroupKey(m, " ")
	if !m.svcGroups[1].folded {
		t.Fatal("precondition: space on the shop header did not fold it")
	}
	if got := m.groupHeaderLine(1); !strings.Contains(got, compose.UpdateGlyph+" 1") {
		t.Errorf("folded shop header = %q, want the update count preserved", got)
	}
}

// Drilling into a scanned project must REUSE the entry U wrote — the two modes
// share one key per project, so the drill costs no second registry pass.
func TestGroupedUpdates_DrillInReplaysCachedEntry(t *testing.T) {
	m, per := groupedUpdatesModel(t)
	m.svcCursor = 3 // shop/api
	m = pressU(t, m)
	if per["shop"].updatesCalls != 1 {
		t.Fatalf("precondition: shop scanned %d times, want 1", per["shop"].updatesCalls)
	}
	per["shop"].services = []string{"api", "db"}
	per["shop"].status = map[string]runner.ServiceStatus{"api": {Running: true}, "db": {}}

	m.svcCursor = 2 // the shop header
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.grouped {
		t.Fatal("enter on a header must drill in")
	}
	if got := m.updatesCacheKey(); got != m.projUpdatesCacheKey(compose.Project{Name: "shop", ConfigDir: "/srv/shop"}) {
		t.Fatalf("drilled key = %q; the two modes must share one key per project", got)
	}
	if _, fresh := m.updatesCacheLookup(); !fresh {
		t.Fatal("the drilled screen must find the entry the grouped scan wrote")
	}
	if cmd != nil {
		cmd() // loadServices + stats; must not add a CheckUpdates pass
	}
	if per["shop"].updatesCalls != 1 {
		t.Errorf("shop scanned %d times after the drill, want 1 (the cache entry is shared)", per["shop"].updatesCalls)
	}
}

// The inspect screen reads the CURSOR row's group entry. Grouped mode has an
// empty project identity, so updatesCacheKey alone would read whichever entry
// happened to sit under "|<server>".
func TestGroupedInspect_ReadsCursorGroupEntry(t *testing.T) {
	m, _ := groupedUpdatesModel(t)
	m.svcCursor = 3 // shop/api
	m = pressU(t, m)

	if got, want := m.inspectUpdateKey(), m.projUpdatesCacheKey(compose.Project{Name: "shop", ConfigDir: "/srv/shop"}); got != want {
		t.Fatalf("inspectUpdateKey = %q, want %q", got, want)
	}
	m.inspectService = "api"
	upd := m.currentUpdateInfo()
	if upd.verdict == nil || !*upd.verdict {
		t.Errorf("currentUpdateInfo verdict = %v, want true", upd.verdict)
	}
	if upd.checkedAt.IsZero() {
		t.Error("currentUpdateInfo must report when the scan ran")
	}

	// A row in a group nothing has scanned reads back as unknown, not as the
	// scanned group's answer.
	m.svcCursor = 1 // blog/web
	m.inspectService = "web"
	if upd := m.currentUpdateInfo(); upd.verdict != nil || !upd.checkedAt.IsZero() {
		t.Errorf("an unscanned group must read as unknown, got %+v", upd)
	}
}

// The three automatic entry points stay shut in grouped mode: the screen-entry
// helper, the statusMsg self-heal and NewModel's in-flight seed.
func TestGroupedMode_AllAutoScanEntryPointsRefuse(t *testing.T) {
	m, per := groupedUpdatesModel(t)

	if m.autoUpdatesAllowed() {
		t.Error("autoUpdatesAllowed must be false in grouped mode")
	}
	if cmd := m.maybeRefreshUpdatesCmd(); cmd != nil {
		t.Error("maybeRefreshUpdatesCmd must schedule nothing in grouped mode")
	}
	if m.updateInFlight {
		t.Error("a grouped model must start with the in-flight guard clear, or the first U is refused")
	}

	// The statusMsg self-heal: no cache entry, nothing in flight — and still
	// no fetch, because U is the only trigger here.
	updated, cmd := m.Update(statusMsg{status: map[string]runner.ServiceStatus{}, session: m.statusSession})
	m = updated.(Model)
	if cmd != nil {
		t.Error("the statusMsg self-heal must not fire in grouped mode")
	}
	for name, c := range per {
		if c.updatesCalls != 0 {
			t.Errorf("%s CheckUpdates ran %d times with no U press", name, c.updatesCalls)
		}
	}
}

// The in-flight guard is checked BEFORE anything is bound, so a refused U
// leaves the grouped screen exactly as it found it.
func TestGroupedU_InFlightGuardBindsNothing(t *testing.T) {
	m, per := groupedUpdatesModel(t)
	m.svcCursor = 3
	m.updateInFlight = true

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	m = updated.(Model)
	if cmd != nil {
		t.Error("U must be refused while a scan is in flight")
	}
	if m.composer != nil {
		t.Error("a refused U must not leave a composer bound")
	}
	if per["shop"].updatesCalls != 0 {
		t.Errorf("shop CheckUpdates ran %d times, want 0", per["shop"].updatesCalls)
	}
}

// Grouped mode fetches no detail rows: it holds no composer of its own, and one
// an armed prompt left bound may belong to a different project entirely.
func TestGroupedU_FetchesNoDetailBatch(t *testing.T) {
	g, projects := groupedFixture()
	detail := &mockDetailComposer{
		mockComposer: mockComposer{updates: map[string]bool{"api": true}},
		details:      map[string]compose.UpdateDetail{"api": {NewID: "sha256:beef"}},
	}
	m := groupedTestModel(g, projects)
	m.composerFactory = func(p compose.Project) runner.Composer {
		if p.Unmanaged {
			return g
		}
		return detail
	}
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	m.svcCursor = 3 // shop/api

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	m = updated.(Model)
	msg := cmd().(updatesMsg)
	// An exec prompt could have left a composer bound; the gate must be the
	// MODE, not the nil.
	m.composer = detail
	updated, detailsCmd := m.Update(msg)
	m = updated.(Model)

	if detailsCmd != nil {
		t.Error("grouped mode must enqueue no detail batch")
	}
	if detail.detailsCalls != 0 {
		t.Errorf("UpdateDetails ran %d times in grouped mode, want 0", detail.detailsCalls)
	}
	if m.detailsInFlight {
		t.Error("no batch was dispatched, so the detail guard must stay clear")
	}
	// The entry keeps details == nil, which is exactly the state a drill-in
	// refills.
	entry := m.updateCache[msg.forKey]
	if entry.details != nil {
		t.Errorf("entry.details = %v, want nil so refillUpdateDetailsCmd can fill it", entry.details)
	}
}

// Init()'s standalone-container fast path is the third automatic entry point.
// A model that lands grouped must never reach it: the grouped branch above it
// dispatches the host loader and the host stats, and nothing else.
func TestGroupedInit_FiresNoUpdateScan(t *testing.T) {
	g, projects := groupedFixture()
	scanned := &mockComposer{updates: map[string]bool{"api": true}}
	m := NewModel(nil, io.Discard, func(p compose.Project) runner.Composer {
		if p.Unmanaged {
			return g
		}
		return scanned
	}, nil, nil, WithLocalProjectLoader(func(ctx context.Context) ([]compose.Project, error) {
		return projects, nil
	}))
	installFakeTick(&m)

	if !m.grouped {
		t.Fatal("no cwd compose file and no servers must land on the grouped host view")
	}
	if m.updateInFlight {
		t.Fatal("NewModel must leave the guard clear when it schedules no scan")
	}

	batch, ok := m.Init()().(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init() produced %T, want tea.BatchMsg", m.Init()())
	}
	for _, c := range batch {
		if c != nil {
			c()
		}
	}
	if scanned.updatesCalls != 0 || g.updatesCalls != 0 {
		t.Errorf("Init() ran CheckUpdates %d/%d times; grouped mode waits for U",
			scanned.updatesCalls, g.updatesCalls)
	}
}

// --- Review-round regression pins ---

// A grouped R must build its batch from the project captured at press time.
// m.currentProject() is the ZERO project in grouped mode, so the batch carried
// no name and the post-success invalidation deleted "|<server>" instead of the
// rolled-back project's key.
func TestGroupedRollback_BatchCarriesTheCapturedProject(t *testing.T) {
	rc := &mockRollbackComposer{mockComposer: mockComposer{services: []string{"api", "db"}}}
	m := groupedScreenModel(
		svcGroup{proj: compose.Project{Name: "blog", ConfigDir: "/srv/blog"}, services: []string{"web"}},
		svcGroup{proj: compose.Project{Name: "shop", ConfigDir: "/srv/shop"}, services: []string{"api", "db"}},
	)
	m.ctx = context.Background()
	m.composerFactory = func(compose.Project) runner.Composer { return rc }
	m.svcCursor = 3 // shop/api
	m.selected[svcKey("shop", "api")] = true

	updated, cmd := m.Update(keyMsgFor("R"))
	m = updated.(Model)
	if m.rollbackProj.Name != "shop" || m.rollbackProj.ConfigDir != "/srv/shop" {
		t.Fatalf("rollbackProj = %+v, want the shop project captured at press time", m.rollbackProj)
	}
	if cmd == nil {
		t.Fatal("R must fetch the snapshot")
	}
	if m.composer != runner.Composer(rc) {
		t.Error("R must bind the captured project's composer")
	}

	// Arm the prompt the way rollbackSnapshotMsg does, then confirm.
	m.rollbackSnapshot = &compose.Snapshot{Services: map[string]compose.SnapshotEntry{"api": {}}}
	m.pendingOp = runner.Rollback
	m.confirming = true
	running, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := running.(Model)
	if len(got.batches) != 1 || got.batches[0].proj.Name != "shop" {
		t.Fatalf("batches = %+v, want one shop batch", got.batches)
	}
	if got.projUpdatesCacheKey(got.batches[0].proj) != "/srv/shop\x00shop|" {
		t.Errorf("batch cache key = %q, want the shop project's own key",
			got.projUpdatesCacheKey(got.batches[0].proj))
	}
}

// buildSvcGroups synthesises a group with NO ConfigDir when `docker compose ls`
// and `docker ps` disagree. compose.New("") leaves cmd.Dir unset, so every
// operation on such a group would run against cdeploy's own working directory.
func TestGroupedScreen_RefusesAProjectWithNoComposeDir(t *testing.T) {
	newModel := func() Model {
		m := groupedScreenModel(
			svcGroupOf("ghost", "api"), // no ConfigDir
			unmanagedGroupOf("watchtower"),
		)
		m.composerFactory = func(compose.Project) runner.Composer { return &mockComposer{} }
		m.svcCursor = 1 // the ghost/api service row
		return m
	}

	for _, key := range []string{"d", "r", "s"} {
		m := newModel()
		updated, _ := m.Update(keyMsgFor(key))
		got := updated.(Model)
		if got.confirming {
			t.Errorf("%q armed an operation on a project with no compose directory", key)
		}
		if got.warning != warnNoComposeDir {
			t.Errorf("%q warning = %q, want %q", key, got.warning, warnNoComposeDir)
		}
	}

	// enter on its header must not drill in either.
	m := newModel()
	m.svcCursor = 0
	updated, cmd := m.Update(keyMsgFor("enter"))
	got := updated.(Model)
	if !got.grouped || cmd != nil {
		t.Error("enter must refuse to drill into a project with no compose directory")
	}
	if got.warning != warnNoComposeDir {
		t.Errorf("drill warning = %q, want %q", got.warning, warnNoComposeDir)
	}

	// And the bind itself refuses, which is what covers l/x/i/c too.
	m = newModel()
	if m.bindProjComposer(compose.Project{Name: "ghost"}) {
		t.Error("bindProjComposer must refuse a grouped project with no compose directory")
	}
	if m.composer != nil {
		t.Error("a refused bind must leave no composer behind")
	}
}

// The refusal is GROUPED-only: the drilled local fast track legitimately runs
// with an empty ConfigDir, because compose finds the file in the working
// directory the user launched cdeploy in.
func TestOperableProject_DrilledEmptyDirIsFine(t *testing.T) {
	m := Model{}
	if !m.operableProject(compose.Project{}) {
		t.Error("drilled mode must accept an empty ConfigDir")
	}
	m.grouped = true
	if m.operableProject(compose.Project{Name: "ghost"}) {
		t.Error("grouped mode must refuse an empty ConfigDir")
	}
	if !m.operableProject(compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true}) {
		t.Error("the unmanaged bucket needs no directory")
	}
}

// Two dirless groups must not share one update-cache slot, and neither may
// collide with the local fast track's bare "|".
func TestProjUpdatesCacheKey_DirlessProjectsStayDistinct(t *testing.T) {
	m := Model{}
	a := m.projUpdatesCacheKey(compose.Project{Name: "ghost"})
	b := m.projUpdatesCacheKey(compose.Project{Name: "phantom"})
	if a == b {
		t.Errorf("two dirless projects share the key %q", a)
	}
	if a == m.projUpdatesCacheKey(compose.Project{}) {
		t.Errorf("a dirless project collides with the local fast-track slot %q", a)
	}
	// The unmanaged prefix rule is unchanged.
	un := m.projUpdatesCacheKey(compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true})
	if want := "unmanaged|\x00" + compose.UnmanagedProjectName + "|"; un != want {
		t.Errorf("unmanaged key = %q, want %q", un, want)
	}
	// updatesCacheKey is projUpdatesCacheKey applied to the current project.
	if m.updatesCacheKey() != m.projUpdatesCacheKey(m.currentProject()) {
		t.Error("the two key producers must agree by construction")
	}
}

// A host whose only group is the unmanaged bucket has no write key that can do
// anything, so it must get the read-only help table and footer rather than
// advertise three no-ops.
func TestGroupedScreen_UnmanagedOnlyHostIsReadOnly(t *testing.T) {
	m := groupedScreenModel(unmanagedGroupOf("watchtower", "portainer"))
	if !m.readOnly() {
		t.Fatal("an unmanaged-only grouped host must read as read-only")
	}
	line1, line2 := m.containerHelpLines()
	for _, tok := range []string{"d deploy", "r restart", "space"} {
		if strings.Contains(line1+line2, tok) {
			t.Errorf("the footer advertises %q, which refuses on this host: %q / %q", tok, line1, line2)
		}
	}
	// enter is a different case: it is NOT gated on readOnly, so it really does
	// drill into the unmanaged group. The footer omits it because it is a
	// curated subset and a lone group renders no header — the drilled screen
	// shows the same rows. The `?` overlay is what has to name it, and
	// TestHelpGroups_ReadOnlyGroupedNamesTheDrill pins that half.
	if strings.Contains(line1+line2, "enter drill in") {
		t.Errorf("the read-only footer must keep its four tokens: %q / %q", line1, line2)
	}
	// A host with a compose project alongside it stays writable.
	mixed := groupedScreenModel(svcGroupOf("shop", "api"), unmanagedGroupOf("watchtower"))
	if mixed.readOnly() {
		t.Error("a host with a compose project must stay writable")
	}
	// An empty/loading host is not read-only either.
	if (Model{grouped: true}).readOnly() {
		t.Error("a host with no groups yet must not flip to read-only")
	}
}

// A folded group hides its ROWS, and search jumps between rows — so a match
// inside one has to open it, or `/db` reports "(no match)" for a service that
// is plainly on the host.
func TestSearch_UnfoldsAGroupHoldingAMatch(t *testing.T) {
	m := groupedScreenModel(
		svcGroupOf("blog", "web"),
		svcGroupOf("shop", "api", "db"),
	)
	m.svcGroups[1].folded = true
	m.svcEntries = rebuildSvcEntries(m.svcGroups)
	m.searchInput = textinput.New()
	m.searchInput.Focus()
	m.searching = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("db")})
	got := updated.(Model)

	if got.svcGroups[1].folded {
		t.Error("the group holding the match must be unfolded")
	}
	if len(got.searchMatches) != 1 {
		t.Fatalf("searchMatches = %v, want the one db row", got.searchMatches)
	}
	if name := got.svcEntries[got.searchMatches[0]].name; name != "db" {
		t.Errorf("matched row = %q, want db", name)
	}
	if got.svcCursor != got.searchMatches[0] {
		t.Error("the cursor must jump to the match it just revealed")
	}
}

// U is the only trigger in grouped mode, so a cached failure's warning has to
// age out on its own or it outlives the failure for the rest of the visit.
func TestGroupedUpdatesErr_ExpiresWithItsCacheEntry(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("shop", "api"))
	key := m.projUpdatesCacheKey(m.svcGroups[0].proj)
	m.updatesErr = "registry unreachable"

	m.updateCache = map[string]updateEntry{key: {fetchedAt: time.Now(), err: true, errMsg: "registry unreachable"}}
	m.syncGroupedUpdatesErr()
	if m.updatesErr == "" {
		t.Error("a FRESH failure entry must keep its warning")
	}

	m.updateCache[key] = updateEntry{fetchedAt: time.Now().Add(-2 * updatesErrorTTL), err: true}
	m.syncGroupedUpdatesErr()
	if m.updatesErr != "" {
		t.Errorf("updatesErr = %q, want it cleared once the entry expired", m.updatesErr)
	}
}

// A grouped reload can add or drop rows, which renumbers every entry index —
// so a committed search must be re-derived, or n/N cycles stale rows.
func TestGroupedReload_RederivesCommittedSearchMatches(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	m.searchQuery = "web"
	m.searchMatches = computeMatches(m.svcEntries, m.searchQuery)
	if len(m.searchMatches) != 1 {
		t.Fatalf("precondition: matches = %v", m.searchMatches)
	}
	before := m.searchMatches[0]

	// A project appears above blog, shifting every row index down.
	g.groupedStatus["aaa"] = map[string]runner.ServiceStatus{"svc": {Running: true}}
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return append([]compose.Project{{Name: "aaa", ConfigDir: "/srv/aaa"}}, projects...), nil
	}
	updated, _ = m.Update(m.loadGroups()())
	m = updated.(Model)

	if len(m.searchMatches) != 1 {
		t.Fatalf("matches = %v, want the one web row after the reload", m.searchMatches)
	}
	if m.searchMatches[0] == before {
		t.Error("the reload renumbered the rows; searchMatches must be re-derived")
	}
	if name := m.svcEntries[m.searchMatches[0]].name; name != "web" {
		t.Errorf("match points at %q, want web", name)
	}
}

// The confirming enter refuses when the first batch cannot bind: the prompt is
// dropped and the user stays on the container screen, with the reason named.
//
// takeSvcReload() has a POINTER receiver, so its call must be hoisted out of
// the `return m, …` operand list: the copy of m in that return is evaluated in
// an unspecified order relative to the call, so an unhoisted one may return a
// model that still carries the flag the call cleared. The svcReloadPending
// assertion below states that contract; it cannot MECHANICALLY catch a missing
// hoist, because gc happens to evaluate the call first today — the spec is what
// makes the hoist required, not the current compiler.
func TestConfirmEnter_BindFailureCancelsThePrompt(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("shop", "api"))
	m.composerFactory = func(compose.Project) runner.Composer { return nil }
	m.svcCursor = 0
	m.pendingOp = runner.Deploy
	m.confirming = true
	m.svcReloadPending = true

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)

	if got.confirming {
		t.Error("a prompt that cannot bind must not stay armed")
	}
	if got.screen != screenSelectContainers || cmd != nil {
		t.Error("a refused confirmation must not navigate")
	}
	if got.svcReloadPending {
		t.Error("the refusal must consume the pending reload on the RETURNED model")
	}
}

// The composer-binding refusal guards: each one makes an action key silently
// inert, so each needs a pin of its own.
func TestBindComposerGuards(t *testing.T) {
	t.Run("bindProjComposer with no factory", func(t *testing.T) {
		m := Model{grouped: true}
		if m.bindProjComposer(compose.Project{Name: "shop", ConfigDir: "/srv/shop"}) {
			t.Error("no factory must refuse")
		}
	})
	t.Run("bindProjComposer when the factory returns nil", func(t *testing.T) {
		m := Model{grouped: true, composerFactory: func(compose.Project) runner.Composer { return nil }}
		if m.bindProjComposer(compose.Project{Name: "shop", ConfigDir: "/srv/shop"}) {
			t.Error("a nil composer must refuse")
		}
		if m.composer != nil {
			t.Error("a refused bind must leave no composer behind")
		}
	})
	t.Run("bindCursorComposer off every row", func(t *testing.T) {
		m := Model{grouped: true, composerFactory: func(compose.Project) runner.Composer { return &mockComposer{} }}
		if m.bindCursorComposer() {
			t.Error("a cursor on no row must refuse")
		}
	})
	t.Run("bindCursorComposer is a pass-through when drilled", func(t *testing.T) {
		m := Model{}
		if !m.bindCursorComposer() {
			t.Error("drilled mode already owns its composer")
		}
	})
	t.Run("drillIntoGroup out of range", func(t *testing.T) {
		m := groupedScreenModel(svcGroupOf("shop", "api"))
		m.composerFactory = func(compose.Project) runner.Composer { return &mockComposer{} }
		if cmd := m.drillIntoGroup(-1); cmd != nil {
			t.Error("a negative index must refuse")
		}
		if cmd := m.drillIntoGroup(9); cmd != nil {
			t.Error("an out-of-range index must refuse")
		}
		if !m.grouped {
			t.Error("a refused drill must not change mode")
		}
	})
	t.Run("drillIntoGroup with no factory", func(t *testing.T) {
		m := groupedScreenModel(svcGroupOf("shop", "api"))
		m.svcGroups[0].proj.ConfigDir = "/srv/shop"
		if cmd := m.drillIntoGroup(0); cmd != nil {
			t.Error("no factory must refuse")
		}
	})
	t.Run("drillIntoGroup when the factory returns nil", func(t *testing.T) {
		m := groupedScreenModel(svcGroupOf("shop", "api"))
		m.svcGroups[0].proj.ConfigDir = "/srv/shop"
		m.composerFactory = func(compose.Project) runner.Composer { return nil }
		if cmd := m.drillIntoGroup(0); cmd != nil {
			t.Error("a nil composer must refuse")
		}
		if !m.grouped || m.composer != nil {
			t.Error("a refused drill must leave the grouped screen untouched")
		}
	})
}

// setGroups(nil) is "still loading" — the nil-vs-empty distinction the screen's
// loading state reads. An EMPTY but non-nil slice is a real, empty host.
func TestSetGroups_NilIsLoadingEmptyIsAHost(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("shop", "api"))
	m.setGroups(nil)
	if m.svcGroups != nil || m.svcEntries != nil {
		t.Fatalf("setGroups(nil) must clear the rows, got %v / %v", m.svcGroups, m.svcEntries)
	}
	if out := m.viewSelectContainers(); !strings.Contains(out, "Loading services") {
		t.Errorf("a nil group slice must render the loading state:\n%s", out)
	}

	m.setGroups([]svcGroup{})
	if m.svcGroups == nil {
		t.Fatal("an empty but non-nil slice is a real host, not a loading state")
	}
	if out := m.viewSelectContainers(); strings.Contains(out, "Loading services") {
		t.Errorf("an empty host must not claim to still be loading:\n%s", out)
	}
}

// The out-of-range row-model guards return "" rather than panicking, which is
// the whole reason they exist.
func TestRowModel_OutOfRangeGuards(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("shop", "api"), svcGroupOf("blog", "web"))
	for _, gi := range []int{-1, 2, 99} {
		if got := m.groupProjName(gi); got != "" {
			t.Errorf("groupProjName(%d) = %q, want empty", gi, got)
		}
	}
	for _, i := range []int{-1, len(m.svcEntries), 99} {
		if got := m.svcKeyAt(i); got != "" {
			t.Errorf("svcKeyAt(%d) = %q, want empty", i, got)
		}
	}
	// A header row carries no service, so it has no key either.
	if got := m.svcKeyAt(0); got != "" {
		t.Errorf("svcKeyAt(header) = %q, want empty", got)
	}
	if got := m.svcKeyAt(1); got != svcKey("shop", "api") {
		t.Errorf("svcKeyAt(1) = %q, want the qualified key", got)
	}
}

// A long project name plus its aggregates must never wrap: the wrap would cost
// the list a row svcVisibleCount already spent.
func TestGroupHeaderRow_IsClampedToWidth(t *testing.T) {
	long := strings.Repeat("verylongproject", 6)
	m := groupedScreenModel(svcGroupOf(long, "api"), svcGroupOf("blog", "web"))
	m.width = 40
	m.svcStatus = map[string]runner.ServiceStatus{svcKey(long, "api"): {Running: true}}

	found := false
	for _, l := range strings.Split(ansi.Strip(m.viewSelectContainers()), "\n") {
		if !strings.Contains(l, "▼") && !strings.Contains(l, "▶") {
			continue
		}
		found = true
		if ansi.StringWidth(l) > m.width {
			t.Errorf("header overruns width %d (%d cells): %q", m.width, ansi.StringWidth(l), l)
		}
	}
	if !found {
		t.Fatal("precondition: the fixture must draw group headers")
	}
}

// --- second-round review pins -------------------------------------------

// A grouped fetch that FAILS ends the cycle at the servicesMsg: there is no
// listing to chain the stats half off, so nothing else will ever arrive to
// clear refreshInFlight. An error return that dropped groupedPayload never
// reached the clear, latching the guard true and turning every later
// refreshTickMsg into a pure reschedule, so one transient `docker compose ls`
// failure froze the host view on its error line for the rest of the visit.
func TestGroupedFetchError_ClearsRefreshInFlightAndKeepsTicking(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.grouped = true
	m.width, m.height = 100, 30
	m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
		return nil, errors.New("compose ls failed")
	}
	m.refreshInFlight = true

	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	if m.refreshInFlight {
		t.Fatal("a failed grouped fetch must clear refreshInFlight, or the 5s refresh is silenced forever")
	}
	if m.svcErr == nil {
		t.Error("the failure must still reach svcErr")
	}

	// And the next tick actually fetches again, which is the self-heal path.
	_, cmd := m.Update(refreshTickMsg{})
	if cmd == nil {
		t.Fatal("the tick must dispatch something")
	}
	if _, ok := cmd().(tea.BatchMsg); !ok {
		t.Error("the tick after a failed fetch must fan out a fresh fetch batch, not just reschedule")
	}
}

// The confirmation prompt names a target set derived from the rows, and the
// confirm resolves that set AGAIN on enter. In grouped mode servicesMsg IS the
// 5-second refresh, so a reload landing mid-prompt could prune the selection —
// which partitionSelection then reads as "the cursor's whole group", i.e. every
// service in the project — or clamp the cursor into a different group.
func TestGroupedReload_IsDroppedWhileAPromptIsArmed(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	installFakeTick(&m)
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	m.composerFactory = func(p compose.Project) runner.Composer {
		if p.Unmanaged {
			return g
		}
		return &mockComposer{}
	}
	m.width, m.height = 100, 30
	m.svcCursor = 3 // shop/api
	m.selected[svcKey("shop", "api")] = true

	armed, _ := m.Update(keyMsgFor("d"))
	m = armed.(Model)
	if !m.confirming {
		t.Fatalf("precondition: d must arm the prompt; warning = %q", m.warning)
	}
	if got := formatBatchTargets(m.partitionSelection()); got != "shop (api)" {
		t.Fatalf("precondition: prompt names %q, want shop (api)", got)
	}
	m.refreshInFlight = true

	// A reload in which shop lost every container: without the guard this
	// prunes the selection and the confirm silently deploys the whole project.
	stripped := map[string]map[string]runner.ServiceStatus{
		"blog": {"web": {Running: true}},
	}
	reloaded, _ := m.Update(servicesMsg{
		groupedPayload: true,
		projects:       projects,
		hostStatus:     stripped,
		session:        m.statusSession,
	})
	m = reloaded.(Model)

	if m.refreshInFlight {
		t.Fatal("the dropped payload must still clear refreshInFlight — the guard sits AFTER the clear, and a dropped payload chains no stats half")
	}
	if !m.selected[svcKey("shop", "api")] {
		t.Fatal("a reload under an armed prompt must not prune the selection it names")
	}
	if got := formatBatchTargets(m.partitionSelection()); got != "shop (api)" {
		t.Errorf("prompt target after the reload = %q, want shop (api)", got)
	}
	if m.svcCursor != 3 {
		t.Errorf("svcCursor = %d, want 3 (the reload must not move it under the prompt)", m.svcCursor)
	}
}

// Every gated container key re-clamps before returning: the dispatch clears
// m.warning above the switch, and a refusal that puts it back costs
// svcVisibleCount a row.
func TestGroupedU_RefusalReclamps(t *testing.T) {
	m := groupedScreenModel(
		svcGroupOf("blog", "a", "b", "c", "d"),
		svcGroup{proj: compose.Project{Name: "ghost"}, services: []string{"api"}}, // no ConfigDir
	)
	m.composerFactory = func(compose.Project) runner.Composer { return &mockComposer{} }
	m.height = 12
	m.svcCursor = len(m.svcEntries) - 1 // the ghost/api row
	m.fixSvcOffset()

	updated, _ := m.Update(keyMsgFor("U"))
	got := updated.(Model)

	if got.warning != warnNoComposeDir {
		t.Fatalf("warning = %q, want %q", got.warning, warnNoComposeDir)
	}
	if got.composer != nil {
		t.Error("a refused U must leave no composer bound")
	}
	visible := got.svcVisibleCount()
	if got.svcCursor < got.svcOffset || got.svcCursor >= got.svcOffset+visible {
		t.Errorf("cursor %d outside the window [%d, %d) after the warning line appeared",
			got.svcCursor, got.svcOffset, got.svcOffset+visible)
	}
}

// R's two CAPABILITY refusals — the composer is not a RollbackPreparer, and the
// list is empty — are gated keys like every other: the dispatch clears m.warning
// above the switch, which frees a footer row and grows svcVisibleCount() by one,
// so a return without fixSvcOffset() leaves a blank row under the last service.
// The readOnly gate and the three selection refusals already re-clamped; these
// two did not.
func TestRollbackKey_CapabilityRefusalReclamps(t *testing.T) {
	build := func(t *testing.T, n int) Model {
		t.Helper()
		var services []string
		for i := 0; i < n; i++ {
			services = append(services, fmt.Sprintf("svc-%02d", i))
		}
		mc := &mockComposer{services: services} // deliberately NOT a RollbackPreparer
		m := inspectTestModel(t, mc, services)
		m.height = 24
		// The x-on-stopped warning is the state this reproduces from.
		m.warning = "Container is not running"
		if n > 0 {
			m.svcCursor = len(m.svcEntries) - 1
		}
		m.fixSvcOffset()
		return m
	}

	t.Run("not a RollbackPreparer", func(t *testing.T) {
		m := build(t, 30)
		if m.svcOffset == 0 {
			t.Fatal("precondition: the list must scroll at this height")
		}

		updated, _ := m.Update(keyMsgFor("R"))
		got := updated.(Model)

		if got.confirming {
			t.Fatal("a composer with no rollback capability must not arm the prompt")
		}
		if got.warning != "" {
			t.Fatalf("precondition: the dispatch must clear the warning, got %q", got.warning)
		}
		want := len(got.svcEntries) - got.svcVisibleCount()
		if got.svcOffset != want {
			t.Errorf("svcOffset = %d, want %d; the refusal left a blank row at the bottom", got.svcOffset, want)
		}
	})

	t.Run("empty list", func(t *testing.T) {
		rc := &mockRollbackComposer{}
		m := build(t, 0)
		m.composer = rc
		m.clearSvcGroups()
		m.svcOffset = 7 // a stale offset the empty list must clamp away

		updated, _ := m.Update(keyMsgFor("R"))
		got := updated.(Model)

		if got.confirming {
			t.Fatal("an empty list must not arm the prompt")
		}
		if got.svcOffset != 0 {
			t.Errorf("svcOffset = %d, want 0; the refusal left the window past the end", got.svcOffset)
		}
	})
}

// R binds the composer at press time, but U and every l/x/i refusal unbind it
// while the snapshot read is still in flight. startBatch deliberately does not
// rebind for a Rollback, so the prep failed with "rollback is not supported for
// this connection" — a message naming the wrong cause entirely.
func TestGroupedRollback_StartBatchRebindsALostComposer(t *testing.T) {
	rc := &mockRollbackComposer{mockComposer: mockComposer{services: []string{"api"}}}
	m := groupedScreenModel(
		svcGroup{proj: compose.Project{Name: "shop", ConfigDir: "/srv/shop"}, services: []string{"api"}},
		svcGroupOf("blog", "web"),
	)
	m.ctx = context.Background()
	m.composerFactory = func(compose.Project) runner.Composer { return rc }
	m.svcCursor = 1 // shop/api
	m.selected[svcKey("shop", "api")] = true

	updated, _ := m.Update(keyMsgFor("R"))
	m = updated.(Model)
	if m.rollbackProj.Name != "shop" {
		t.Fatalf("precondition: rollbackProj = %+v", m.rollbackProj)
	}

	// U (or an l/x/i refusal) drops the action-time composer while the async
	// snapshot read is still out.
	m.unbindGroupedComposer()
	if m.composer != nil {
		t.Fatal("precondition: the composer must be unbound")
	}

	m.rollbackSnapshot = &compose.Snapshot{Services: map[string]compose.SnapshotEntry{"api": {}}}
	m.rollbackTargets = []string{"api"}
	m.pendingOp = runner.Rollback
	m.confirming = true
	running, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := running.(Model)

	if got.failed {
		t.Fatal("the rollback must not fail for want of a composer the batch can name")
	}
	if got.composer != runner.Composer(rc) {
		t.Fatalf("composer = %#v, want the captured project's own", got.composer)
	}
	if cmd == nil {
		t.Fatal("the rollback batch must dispatch its prep")
	}
	if rc.prepCalls == 0 {
		// The prep runs inside the batched cmd; drive it.
		if batch, ok := cmd().(tea.BatchMsg); ok {
			for _, c := range batch {
				if msg, ok := c().(rollbackPreppedMsg); ok && msg.err != nil {
					t.Fatalf("prep error = %v, want none", msg.err)
				}
			}
		}
	}
	if rc.prepCalls != 1 {
		t.Errorf("PrepareRollback calls = %d, want 1", rc.prepCalls)
	}
}

// TestGroupedReload_CursorSurvivesByIdentity is the pin for the cursor's third
// piece of user state. The grouped reload IS the 5-second refresh and its rows
// come from a live `docker ps`, so a container started anywhere on the host
// inserts a row and slides every index below it. The cursor addresses drill-in,
// l/i/c/U and — through the empty-selection rule — the whole-project d/r/s, so
// an index-only survival re-aimed every one of them without a keystroke.
func TestGroupedReload_CursorSurvivesByIdentity(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	// Park the cursor on the LAST row and record what it names.
	m.svcCursor = len(m.svcEntries) - 1
	before := rowIDAt(m.svcEntries, m.svcGroups, m.svcCursor)
	if before.service != "watchtower" {
		t.Fatalf("precondition: the last row is %+v, want the watchtower service row", before)
	}

	// A container appears in the FIRST group, sorting above its sibling: every
	// row below it shifts down by one.
	g.groupedStatus["blog"] = map[string]runner.ServiceStatus{
		"cache": {Running: true},
		"web":   {Running: true},
	}
	updated, _ = m.Update(m.loadGroups()())
	m = updated.(Model)

	after := rowIDAt(m.svcEntries, m.svcGroups, m.svcCursor)
	if after != before {
		t.Errorf("cursor moved from %+v to %+v; the row it names must survive the rebuild", before, after)
	}
	if m.svcCursor != len(m.svcEntries)-1 {
		t.Errorf("svcCursor = %d, want %d (the new last row)", m.svcCursor, len(m.svcEntries)-1)
	}
}

// A header and a service row of the same project are different rows, so the
// kind is part of the identity — otherwise a header ID would silently land on
// that project's first service after a single-project host drops its header.
func TestRowIDAt_HeaderAndServiceAreDifferentRows(t *testing.T) {
	m := groupedModel(svcGroupOf("blog", "web"), svcGroupOf("shop", "api"))
	head := rowIDAt(m.svcEntries, m.svcGroups, 0)
	if !head.header || head.proj != "blog" || head.service != "" {
		t.Fatalf("row 0 = %+v, want the blog header", head)
	}
	svc := rowIDAt(m.svcEntries, m.svcGroups, 1)
	if svc.header || svc.service != "web" {
		t.Fatalf("row 1 = %+v, want the web service row", svc)
	}
	// A single-group host emits NO header, so the header ID must not resolve.
	one := groupedModel(svcGroupOf("blog", "web"))
	if i, ok := indexOfRowID(one.svcEntries, one.svcGroups, head); ok {
		t.Errorf("the blog header resolved to row %d on a headerless list", i)
	}
	if i, ok := indexOfRowID(one.svcEntries, one.svcGroups, svc); !ok || i != 0 {
		t.Errorf("the web row resolved to (%d,%v), want (0,true)", i, ok)
	}
	// An out-of-range cursor names nothing, and nothing must not restore.
	if id := rowIDAt(m.svcEntries, m.svcGroups, 99); id.ok() {
		t.Errorf("an out-of-range cursor named %+v", id)
	}
}

// A row that is genuinely gone falls back to the clamp rather than jumping to
// an unrelated row.
func TestRestoreCursorRow_FallsBackToClamp(t *testing.T) {
	m := groupedModel(svcGroupOf("blog", "web"), svcGroupOf("shop", "api"))
	gone := svcRowID{proj: "ghost", service: "vanished"}
	m.svcCursor = 99
	m.restoreCursorRow(gone)
	if m.svcCursor != len(m.svcEntries)-1 {
		t.Errorf("svcCursor = %d, want the clamped last row %d", m.svcCursor, len(m.svcEntries)-1)
	}
}

// drilledConfirmModel is a DRILLED (non-grouped) container screen with an armed
// confirmation prompt — the state in which a still-in-flight loadServices
// payload has to be refused.
func drilledConfirmModel(t *testing.T, mc *mockComposer) Model {
	t.Helper()
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.composer = mc
	m.ctx = context.Background()
	m.width, m.height = 100, 24
	// drillIntoGroup installs the host-ps subset first, then dispatches
	// loadServices — so the screen has rows to select from while the fetch runs.
	m.setSingleGroup([]string{"web"})
	m.selected = map[string]bool{m.svcKeyAt(0): true}
	m.confirming = true
	m.pendingOp = runner.Restart
	return m
}

// TestDrilledServicesMsg_ConfirmingReloadIsRedispatched pins the completion of
// the round-2 confirming guard. Dropping the drilled payload is right — it
// resets the selection and the cursor wholesale — but nothing else re-dispatches
// loadServices during a drilled visit, so the screen would keep the host-ps
// subset (and swallow the fetch's error) for the whole visit.
func TestDrilledServicesMsg_ConfirmingReloadIsRedispatched(t *testing.T) {
	mc := &mockComposer{services: []string{"api", "web", "worker"}}
	m := drilledConfirmModel(t, mc)

	arrived, cmd := m.Update(servicesMsg{services: mc.services, session: m.statusSession})
	m = arrived.(Model)
	if cmd != nil {
		t.Errorf("an armed prompt must drop the payload, got %T", cmd())
	}
	if !m.svcReloadPending {
		t.Fatal("the dropped drilled payload must be remembered")
	}
	if len(modelServices(m)) != 1 {
		t.Fatalf("services = %v, want the pre-fetch subset", modelServices(m))
	}

	// Cancelling the prompt fires it again.
	closed, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = closed.(Model)
	if m.svcReloadPending {
		t.Error("the flag must clear once the reload is re-dispatched")
	}
	if cmd == nil {
		t.Fatal("closing the prompt must re-dispatch loadServices")
	}
	msg, ok := cmd().(servicesMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want servicesMsg", cmd())
	}
	if len(msg.services) != 3 {
		t.Errorf("the re-dispatch returned %v, want all three compose services", msg.services)
	}
}

// enterExec's refusal paths are prompt CLOSES that stay on the container
// screen, so they owe what the esc branch owes: takeSvcReload() settles a
// drilled reload the armed prompt dropped, and fixSvcOffset() re-clamps for the
// footer row the closing prompt frees. containerFetchBatch — the flag's other
// consumer — is only reached by LEAVING the screen, which a refusal does not
// do, so without this the reload sat pending until some later prompt closed.
func TestEnterExec_RefusalSettlesThePendingReload(t *testing.T) {
	mc := &mockComposer{services: []string{"api", "web", "worker"}} // NOT an ExecProvider
	m := drilledConfirmModel(t, mc)
	m.pendingExec = true
	m.svcReloadPending = true

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)

	if got.screen != screenSelectContainers {
		t.Fatalf("screen = %v, want the container screen (the exec was refused)", got.screen)
	}
	if got.confirming || got.pendingExec {
		t.Errorf("the refusal must close the prompt, got confirming=%v pendingExec=%v", got.confirming, got.pendingExec)
	}
	if got.svcReloadPending {
		t.Error("the refusal must consume the pending drilled reload")
	}
	if cmd == nil {
		t.Fatal("the refusal must re-dispatch loadServices")
	}
	msg, ok := cmd().(servicesMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want servicesMsg", cmd())
	}
	if len(msg.services) != 3 {
		t.Errorf("the re-dispatch returned %v, want all three compose services", msg.services)
	}
}

// The same refusal is a gated key for the offset too: the dispatch cleared
// m.warning above the switch, and closing the prompt frees another footer row.
func TestEnterExec_RefusalReclampsOffset(t *testing.T) {
	var services []string
	for i := 0; i < 30; i++ {
		services = append(services, fmt.Sprintf("svc-%02d", i))
	}
	mc := &mockComposer{services: services} // NOT an ExecProvider
	m := inspectTestModel(t, mc, services)
	m.ctx = context.Background()
	m.height = 24
	m.confirming = true
	m.pendingExec = true
	m.svcCursor = len(m.svcEntries) - 1
	m.svcOffset = len(m.svcEntries) // a stale offset past the end

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)

	if got.confirming || got.pendingExec {
		t.Fatalf("precondition: the refusal must close the prompt, got confirming=%v pendingExec=%v", got.confirming, got.pendingExec)
	}
	want := len(got.svcEntries) - got.svcVisibleCount()
	if got.svcOffset != want {
		t.Errorf("svcOffset = %d, want %d; the refusal left a blank row at the bottom", got.svcOffset, want)
	}
}

// The grouped payload keeps the old behaviour: it is dropped and NOT
// remembered, because the 5-second tick refetches it on its own.
func TestGroupedServicesMsg_ConfirmingDropIsNotRemembered(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	m.confirming = true

	arrived, _ := m.Update(m.loadGroups()())
	if arrived.(Model).svcReloadPending {
		t.Error("the grouped payload has its own refetch; it must not set the flag")
	}
}

// A prompt closed by CONFIRMING leaves for the progress/exec screen instead of
// reloading, so containerFetchBatch is the second consumer of the same flag —
// the return from progress settles the debt.
func TestSvcReloadPending_SettledByTheReturnFromProgress(t *testing.T) {
	mc := &mockComposer{services: []string{"api", "web", "worker"}}
	m := drilledConfirmModel(t, mc)
	m.confirming = false
	m.svcReloadPending = true

	cmd := m.containerFetchBatch(false)
	if m.svcReloadPending {
		t.Error("containerFetchBatch must consume the flag")
	}
	if cmd == nil {
		t.Fatal("containerFetchBatch returned no command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want tea.BatchMsg", cmd())
	}
	found := false
	for _, c := range batch {
		if msg, ok := c().(servicesMsg); ok && len(msg.services) == 3 {
			found = true
		}
	}
	if !found {
		t.Error("a pending reload must make containerFetchBatch(false) run loadServices, not refreshStatus")
	}
}

// TestUpdatesMsg_GroupedSuccessKeepsAnotherGroupsWarning pins the scoped clear.
// A U scan covers ONE group, so a flat `updatesErr = ""` on success erased a
// warning that belonged to a different one — and nothing in grouped mode would
// put it back, since U is the only trigger there.
func TestUpdatesMsg_GroupedSuccessKeepsAnotherGroupsWarning(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("blog", "web"), svcGroupOf("shop", "api"))
	m.selected = map[string]bool{}
	blogKey := m.projUpdatesCacheKey(m.svcGroups[0].proj)
	shopKey := m.projUpdatesCacheKey(m.svcGroups[1].proj)
	m.updateCache = map[string]updateEntry{
		blogKey: {fetchedAt: time.Now(), err: true, errMsg: "updates: blog registry down"},
	}
	m.updatesErr = "updates: blog registry down"

	updated, _ := m.Update(updatesMsg{results: map[string]bool{"api": true}, session: m.updatesSession, forKey: shopKey})
	got := updated.(Model)

	if got.updatesErr != "updates: blog registry down" {
		t.Errorf("updatesErr = %q, want blog's still-fresh failure preserved", got.updatesErr)
	}

	// And a success on the group that FAILED does clear it.
	updated, _ = got.Update(updatesMsg{results: map[string]bool{"web": false}, session: got.updatesSession, forKey: blogKey})
	if cleared := updated.(Model).updatesErr; cleared != "" {
		t.Errorf("updatesErr = %q, want cleared once blog itself succeeded", cleared)
	}
}

// The drilled screen holds ONE project, so its clear stays unconditional.
func TestUpdatesMsg_DrilledSuccessClearsTheWarning(t *testing.T) {
	mc := &mockComposer{services: []string{"web"}}
	m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
	installFakeTick(&m)
	m.screen = screenSelectContainers
	m.updatesErr = "updates: boom"

	updated, _ := m.Update(updatesMsg{results: map[string]bool{"web": false}, session: m.updatesSession, forKey: m.updatesCacheKey()})
	if got := updated.(Model).updatesErr; got != "" {
		t.Errorf("updatesErr = %q, want cleared", got)
	}
}

// TestMaybeRefreshUpdates_GroupedExpiresErrWhileAScanIsInFlight pins the branch
// ORDER. The grouped branch schedules nothing, so the in-flight guard has
// nothing to protect there — and behind the guard the one thing the branch does
// do (age out the soft warning) was skipped for the whole life of a U scan.
func TestMaybeRefreshUpdates_GroupedExpiresErrWhileAScanIsInFlight(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("blog", "web"))
	key := m.projUpdatesCacheKey(m.svcGroups[0].proj)
	m.updateCache = map[string]updateEntry{
		key: {fetchedAt: time.Now().Add(-2 * updatesErrorTTL), err: true, errMsg: "updates: boom"},
	}
	m.updatesErr = "updates: boom"
	m.updateInFlight = true

	if cmd := m.maybeRefreshUpdatesCmd(); cmd != nil {
		t.Errorf("grouped mode must schedule nothing, got %T", cmd())
	}
	if m.updatesErr != "" {
		t.Errorf("updatesErr = %q, want the expired warning aged out despite the in-flight scan", m.updatesErr)
	}
}

// TestContainerKeys_UnsupportedCapabilityReclampsOffset is the type-assert twin
// of TestReadOnly_GatedKeyReclampsOffset: i, x and c also return early when the
// composer implements neither Inspector, ExecProvider nor ConfigProvider, and
// the dispatch cleared m.warning above them.
func TestContainerKeys_UnsupportedCapabilityReclampsOffset(t *testing.T) {
	for _, key := range []string{"i", "x", "c"} {
		t.Run(key, func(t *testing.T) {
			var services []string
			for i := 0; i < 30; i++ {
				services = append(services, fmt.Sprintf("svc-%02d", i))
			}
			mc := &mockComposer{services: services}
			m := NewModel(mc, io.Discard, mockFactory(mc), nil, nil)
			installFakeTick(&m)
			m.screen = screenSelectContainers
			m.composer = mc
			m.ctx = context.Background()
			m.width, m.height = 100, 24
			m.setSingleGroup(services)
			m.warning = "Container is not running"
			m.svcCursor = len(m.svcEntries) - 1
			m.fixSvcOffset()
			if m.svcOffset == 0 {
				t.Fatal("precondition: the list must scroll at this height")
			}

			updated, _ := m.Update(keyMsgFor(key))
			got := updated.(Model)

			if got.screen != screenSelectContainers {
				t.Fatalf("key %q changed screen to %d; the mock supports none of the three", key, got.screen)
			}
			want := len(got.svcEntries) - got.svcVisibleCount()
			if got.svcOffset != want {
				t.Errorf("svcOffset = %d, want %d; the refusal left a blank row at the bottom", got.svcOffset, want)
			}
		})
	}
}

// TestSearch_EscWhileTypingRestoresTheRowAfterAnUnfold pins the identity twin of
// searchReturn. Typing UNFOLDS a group holding a match, which grows svcEntries
// under the raw index the `/` press captured.
func TestSearch_EscWhileTypingRestoresTheRowAfterAnUnfold(t *testing.T) {
	// The folded group sits ABOVE the cursor, so opening it pushes every row
	// below it down by one — which is what makes the raw index wrong.
	m := groupedScreenModel(
		svcGroupOf("archive", "db"),
		svcGroupOf("shop", "api"),
	)
	m.svcGroups[0].folded = true
	m.svcEntries = rebuildSvcEntries(m.svcGroups)
	// Rows: 0 = archive header (folded), 1 = shop header, 2 = api.
	m.svcCursor = 2
	before := rowIDAt(m.svcEntries, m.svcGroups, m.svcCursor)
	if before.service != "api" {
		t.Fatalf("precondition: row 2 is %+v, want the api row", before)
	}

	m = pressGroupKey(m, "/")
	if !m.searching {
		t.Fatal("precondition: / must open the search input")
	}
	m = pressGroupKey(m, "db")
	if m.svcGroups[0].folded {
		t.Fatal("precondition: the match must unfold its group")
	}
	if len(m.svcEntries) != 4 {
		t.Fatalf("precondition: the unfold must grow the list, got %d rows", len(m.svcEntries))
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(Model)
	after := rowIDAt(got.svcEntries, got.svcGroups, got.svcCursor)
	if after != before {
		t.Errorf("cursor restored to %+v, want the row it left from, %+v", after, before)
	}
}

// evilLabel is a compose-LOOKING label carrying an OSC 52 clipboard write plus
// a CSI screen clear - the payload a `docker run --label` can plant on any
// docker host cdeploy connects to.
const (
	evilOSC     = "\x1b]52;c;cHduZWQ="
	evilCSI     = "\x1b[2J\x1b[H"
	evilPayload = "52;c;cHduZWQ="
)

// TestGroupedView_NeverEmitsLabelControlSequences is the terminal-injection pin.
// Project and service names on the host view come from container labels, and the
// screen redraws itself every 5 seconds, so an escape sequence in a label would
// be replayed into the user's terminal with no keystroke at all. Every surface a
// label-derived name reaches is checked: the group header, the service row, the
// breadcrumb after drilling in, and the confirmation prompt.
func TestGroupedView_NeverEmitsLabelControlSequences(t *testing.T) {
	g := &mockGrouper{
		groupedStatus: map[string]map[string]runner.ServiceStatus{
			"sh" + evilOSC + "op": {"ap" + evilCSI + "i": {Running: true}},
			"blog":                {"web": {Running: true}},
		},
	}
	projects := []compose.Project{
		{Name: "sh" + evilOSC + "op", ConfigDir: "/srv/shop"},
		{Name: "blog", ConfigDir: "/srv/blog"},
	}
	m := groupedTestModel(g, projects)
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	m.width, m.height = 100, 30

	clean := func(t *testing.T, where, view string) {
		t.Helper()
		if strings.Contains(view, evilPayload) {
			t.Errorf("%s replayed the OSC payload:\n%q", where, view)
		}
		if strings.Contains(view, "\x1b]") {
			t.Errorf("%s emitted an OSC introducer:\n%q", where, view)
		}
		if strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\x1b[H") {
			t.Errorf("%s emitted a screen-clear sequence:\n%q", where, view)
		}
	}

	// The header and the row both carry the name.
	view := m.viewSelectContainers()
	clean(t, "the grouped list", view)
	stripped := ansi.Strip(view)
	if !strings.Contains(stripped, "shop") || !strings.Contains(stripped, "api") {
		t.Errorf("the sanitized names must still be shown:\n%s", stripped)
	}

	// The confirmation prompt names the batch (project plus services).
	m.selected[svcKey("shop", "api")] = true
	armed := pressGroupKey(m, "d")
	if !armed.confirming {
		t.Fatalf("precondition: d must arm the prompt; warning = %q", armed.warning)
	}
	clean(t, "the confirm prompt", armed.viewSelectContainers())

	// And the breadcrumb once drilled in.
	m.svcCursor = headerIndexFor(t, m.svcEntries, 0)
	drilled, _ := m.Update(keyMsgFor("enter"))
	dm := drilled.(Model)
	if dm.grouped {
		t.Fatalf("precondition: enter must drill in")
	}
	clean(t, "the drilled breadcrumb", dm.breadcrumb())
	if !strings.Contains(ansi.Strip(dm.breadcrumb()), "shop") {
		t.Errorf("breadcrumb lost the project name: %q", dm.breadcrumb())
	}
}

// TestGroupedRefreshError_GatesRowActions is the invisible-target pin. A refresh
// failure replaces the whole list with an error screen while the groups and the
// selection behind it are deliberately kept, so d/r/s used to arm a prompt the
// renderer had no slot to draw - and the enter that followed ran the operation
// against rows the user could not see.
func TestGroupedRefreshError_GatesRowActions(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)
	m.width, m.height = 100, 30
	m.selected[svcKey("shop", "api")] = true

	// A failed grouped refresh: the rows survive, the renderer hides them.
	errored, _ := m.Update(servicesMsg{groupedPayload: true, err: errors.New("docker daemon gone"), session: m.statusSession})
	m = errored.(Model)
	if m.svcErr == nil || len(m.svcGroups) == 0 {
		t.Fatalf("precondition: the error must be set and the groups kept")
	}
	if !strings.Contains(ansi.Strip(m.viewSelectContainers()), "docker daemon gone") {
		t.Fatal("precondition: the error screen must replace the list")
	}

	for _, key := range []string{"d", "r", "s", "R", "a", " ", "/", "i", "l", "c", "x", "U"} {
		got := pressGroupKey(m, key)
		if got.confirming {
			t.Errorf("%q armed a confirmation the error screen cannot draw", key)
		}
		if got.searching {
			t.Errorf("%q opened the search bar behind the error screen", key)
		}
		if got.screen != screenSelectContainers {
			t.Errorf("%q left the error screen for screen %d", key, got.screen)
		}
	}

	// enter cannot start an operation either - there is no armed prompt to
	// confirm, and it must not drill into an invisible row.
	got, _ := m.Update(keyMsgFor("enter"))
	after := got.(Model)
	if after.screen != screenSelectContainers || !after.grouped {
		t.Error("enter acted on a row the error screen hides")
	}

	// The two keys the error screen DOES advertise stay live. esc drills back
	// out of a project reached from the host view, and ctrl+c still quits.
	drilled := m
	drilled.grouped = false
	drilled.drilledFromHost = true
	out, _ := drilled.Update(keyMsgFor("esc"))
	if !out.(Model).grouped {
		t.Error("esc must still leave the error screen")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Error("ctrl+c must still quit from the error screen")
	}
}

// The svcErr key swallow sits BELOW the confirmation intercept, so it can only
// stop a prompt from being ARMED. servicesMsg drops its whole payload while
// confirming; statusMsg had no such guard, so an in-flight refreshStatus that
// failed mid-prompt hid the prompt behind the error screen while enter still
// launched the operation against a selection the user could no longer see.
func TestStatusMsgError_DroppedWhileAConfirmationIsArmed(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("shop", "api"))
	m.grouped = false
	m.svcCursor = 0
	m.selected = map[string]bool{svcKey("shop", "api"): true}
	m.confirming = true
	m.pendingOp = runner.Deploy

	updated, _ := m.Update(statusMsg{err: errors.New("docker daemon gone"), session: m.statusSession})
	got := updated.(Model)

	if got.svcErr != nil {
		t.Fatalf("svcErr = %v, want the error dropped while a prompt is armed", got.svcErr)
	}
	if !got.confirming {
		t.Error("the armed prompt must survive the dropped refresh")
	}
	if !strings.Contains(ansi.Strip(got.viewSelectContainers()), "Deploy") {
		t.Error("the confirm prompt must still be drawn")
	}
}

// The search bar is the other sub-state the error view cannot draw: keystrokes
// went into an input that was not on screen.
func TestStatusMsgError_DroppedWhileSearching(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("shop", "api"))
	m.grouped = false
	m.searching = true

	updated, _ := m.Update(statusMsg{err: errors.New("docker daemon gone"), session: m.statusSession})
	got := updated.(Model)
	if got.svcErr != nil {
		t.Fatalf("svcErr = %v, want the error dropped while the search bar is open", got.svcErr)
	}
	if !got.searching {
		t.Error("the open search bar must survive the dropped refresh")
	}
}

// A successful status refresh is harmless mid-prompt — it only repaints the
// dots — so the guard must be scoped to the ERROR path.
func TestStatusMsgSuccess_StillLandsWhileConfirming(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("shop", "api"))
	m.grouped = false
	m.confirming = true

	updated, _ := m.Update(statusMsg{status: map[string]runner.ServiceStatus{"api": {Running: true}}, session: m.statusSession})
	got := updated.(Model)
	if st := got.svcStatus[svcKey("shop", "api")]; !st.Running {
		t.Error("a successful status refresh must still land while confirming")
	}
}

// Every grouped landing site blanks updatesErr, and U is the only thing that can
// refresh a grouped scan — so the warning has to be re-derived from the cache
// when the group list is installed, not merely aged out.
func TestGroupedUpdatesErr_RestoredOnTheGroupedPayload(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	updated, _ := m.Update(m.loadGroups()())
	m = updated.(Model)

	// A failed U on blog caches an error entry and raises the warning.
	blog := compose.Project{Name: "blog", ConfigDir: "/srv/blog"}
	m.updateCache = map[string]updateEntry{
		m.projUpdatesCacheKey(blog): {fetchedAt: time.Now(), err: true, errMsg: "registry unreachable"},
	}
	// Drilling in and back out blanks the field; nothing re-derived it.
	m.updatesErr = ""

	again, _ := m.Update(m.loadGroups()())
	got := again.(Model)
	if got.updatesErr != "registry unreachable" {
		t.Errorf("updatesErr = %q, want the cached failure restored", got.updatesErr)
	}

	// And it still ages out once the entry expires.
	got.updateCache[got.projUpdatesCacheKey(blog)] = updateEntry{fetchedAt: time.Now().Add(-2 * updatesErrorTTL), err: true, errMsg: "registry unreachable"}
	expired, _ := got.Update(got.loadGroups()())
	if e := expired.(Model).updatesErr; e != "" {
		t.Errorf("updatesErr = %q, want it cleared once the entry expired", e)
	}
}

// The refill reads the CURRENT cache key and fetches through the CURRENT
// composer. Grouped mode has neither — updatesCacheKey resolves to the
// degenerate "\x00|" there — so a refill would fill one project's details into
// another project's entry. maybeRefreshUpdatesCmd guards its own call site; the
// updateDetailsMsg arrival tail runs on every screen and cannot.
func TestRefillUpdateDetails_RefusesInGroupedMode(t *testing.T) {
	m := groupedScreenModel(svcGroupOf("shop", "api"))
	// A composer IS bound in the failing case the guard covers: grouped mode
	// binds one at press time for the read keys (l, x, i, c).
	m.composer = &mockDetailComposer{}
	m.updateCache = map[string]updateEntry{
		m.updatesCacheKey(): {fetchedAt: time.Now(), results: map[string]bool{"api": true}},
	}
	if cmd := m.refillUpdateDetailsCmd(); cmd != nil {
		t.Error("a refill fired in grouped mode against the degenerate key")
	}
	if m.detailsInFlight {
		t.Error("the refused refill raised detailsInFlight for a foreign entry")
	}
	m.grouped = false
	if cmd := m.refillUpdateDetailsCmd(); cmd == nil {
		t.Error("the drilled refill must still fire — the guard is grouped-only")
	}
}

// The redraw gate must compare the batch's key against the key the INSPECT
// screen reads through, not against updatesCacheKey(). Grouped mode has no
// project identity, so updatesCacheKey resolves to the degenerate "\x00|" and
// never equals the batch's key: a detail batch landing while the inspect screen
// was open repainted nothing, and the rows appeared only after esc and a second `i`.
func TestUpdateDetailsMsg_RepaintsTheGroupedInspectScreen(t *testing.T) {
	m, _ := groupedUpdatesModel(t)
	m.svcCursor = 3 // shop/api
	m = pressU(t, m)

	key := m.projUpdatesCacheKey(compose.Project{Name: "shop", ConfigDir: "/srv/shop"})
	entry := m.updateCache[key]
	if entry.results == nil {
		t.Fatal("precondition: the scan must have written a success entry")
	}
	if key == m.updatesCacheKey() {
		t.Fatal("precondition: grouped mode must not resolve to the group's own key")
	}

	// Stand on the inspect screen with a raw document in hand so
	// rebuildInspectSummary has something to draw.
	m.screen = screenInspect
	m.inspectService = "api"
	m.inspectRaw = []byte(`[{"Name":"/shop-api-1","State":{"Status":"running"},"Config":{"Image":"nginx:1"}}]`)
	m.width, m.height = 100, 30
	m.rebuildInspectSummary()
	before := m.inspectSummary

	updated, _ := m.Update(updateDetailsMsg{
		details:  map[string]compose.UpdateDetail{"api": {NewID: "sha256:beef"}},
		forKey:   key,
		forEntry: entry.fetchedAt,
	})
	got := updated.(Model)
	if got.updateCache[key].details == nil {
		t.Fatal("the batch did not merge onto its entry")
	}
	if got.inspectSummary == before {
		t.Error("the inspect screen was not repainted for the batch that landed")
	}
}

// --- grouped fetch: two messages over one listing --------------------------

// mustNotBlockWait is mustNotBlock's window: long enough that a loaded CI box
// cannot trip it, short enough that a regression is named in seconds rather
// than hanging the package for the full `go test` timeout (600s -> 11.75s when
// this was measured).
const mustNotBlockWait = 10 * time.Second

// mustNotBlock runs fn on its own goroutine and FAILS rather than letting the
// package hang. The regression this section pins is a seam that folds the stats
// call back into the status half, and under that shape the status fetch blocks
// on the gate below — on the test goroutine, before any cleanup exists to
// release it. Without this the whole package deadlocks until the go test
// timeout instead of naming the defect. The passing path never waits.
func mustNotBlock(t *testing.T, what string, fn tea.Cmd) tea.Msg {
	t.Helper()
	out := make(chan tea.Msg, 1)
	go func() { out <- fn() }()
	select {
	case msg := <-out:
		return msg
	case <-time.After(mustNotBlockWait):
		t.Fatalf("%s never returned; the rows must not wait on the stats half", what)
		return nil
	}
}

// The grouped screen's first paint used to sit behind `docker stats
// --no-stream`, which dominates the cycle by more than an order of magnitude
// (measured in docs/architecture/tui-multi-project.md) against the listing
// pair that actually produces the rows. The seam is two methods over
// one listing now, so the rows must be on the Model while the stats half is
// still running.
func TestGroupedFirstPaint_DoesNotWaitOnStats(t *testing.T) {
	g, projects := groupedFixture()
	g.statsEntered = make(chan struct{})
	g.statsGate = make(chan struct{})
	// Release the stats half however the test ends, so a failure cannot strand
	// the goroutine below.
	var once sync.Once
	release := func() { once.Do(func() { close(g.statsGate) }) }
	t.Cleanup(release)

	m := groupedTestModel(g, projects)
	m.refreshInFlight = true

	// The status half must return on its own; a merged seam would block here.
	updated, cmd := m.Update(mustNotBlock(t, "loadGroups", m.loadGroups()))
	m = updated.(Model)
	// Stated as an assertion too, so the merged seam is named in milliseconds
	// by whichever of the two trips first.
	if g.statsCalls2 != 0 {
		t.Errorf("stats calls during the status half = %d, want none", g.statsCalls2)
	}

	if len(m.svcGroups) != 3 {
		t.Fatalf("groups = %d, want the rows painted before the stats half runs", len(m.svcGroups))
	}
	if !m.svcStatus[svcKey("shop", "api")].Running {
		t.Error("the status half must be applied on the first message")
	}
	if cmd == nil {
		t.Fatal("the grouped arrival must chain the stats half")
	}
	if !m.refreshInFlight {
		t.Error("the chaining arrival re-arms refreshInFlight; the chained statsMsg owns the clear")
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	// While the stats call is blocked, the rows are already on screen.
	<-g.statsEntered
	if len(m.svcEntries) == 0 {
		t.Fatal("the rows must survive while the stats half is still in flight")
	}
	if m.stats != nil {
		t.Errorf("stats = %v, want the first cycle's cells still unset", m.stats)
	}

	release()
	updated, _ = m.Update(<-done)
	m = updated.(Model)

	if m.stats[svcKey("shop", "api")].CPUPercent != 12.5 {
		t.Errorf("stats = %v, want the chained half to fill shop/api", m.stats)
	}
	if m.refreshInFlight {
		t.Error("the chained statsMsg must clear refreshInFlight")
	}
}

// refreshInFlight is the fetch-stacking guard: latched true, the 5-second
// refresh dies silently for the rest of the session. The grouped cycle is two
// messages now, so the clear has ONE rule — the arrival that ends the cycle
// owns it, and the only arrival that may leave the flag set is the one that
// chains the stats half.
func TestGroupedArrivals_ClearRefreshInFlight(t *testing.T) {
	tests := []struct {
		name string
		// setup runs on a fresh grouped model with refreshInFlight already
		// armed, and returns the message to deliver.
		setup     func(t *testing.T, m *Model, g *mockGrouper) tea.Msg
		wantFlag  bool // refreshInFlight after the arrival
		wantChain bool // the arrival returned the stats half
	}{
		{
			name: "no loader",
			setup: func(t *testing.T, m *Model, g *mockGrouper) tea.Msg {
				m.projectLoader = nil
				return m.loadGroups()()
			},
		},
		{
			name: "loader failure",
			setup: func(t *testing.T, m *Model, g *mockGrouper) tea.Msg {
				m.projectLoader = func(ctx context.Context) ([]compose.Project, error) {
					return nil, errors.New("compose ls failed")
				}
				return m.loadGroups()()
			},
		},
		{
			name: "host ps failure",
			setup: func(t *testing.T, m *Model, g *mockGrouper) tea.Msg {
				g.statusErr2 = errors.New("docker ps failed")
				return m.loadGroups()()
			},
		},
		{
			name: "no grouper behind the factory",
			setup: func(t *testing.T, m *Model, g *mockGrouper) tea.Msg {
				mc := &mockComposer{}
				m.composerFactory = mockFactory(mc)
				return m.loadGroups()()
			},
		},
		{
			name: "dropped under an armed prompt",
			setup: func(t *testing.T, m *Model, g *mockGrouper) tea.Msg {
				m.confirming = true
				return m.loadGroups()()
			},
		},
		{
			name: "arrives off-screen",
			setup: func(t *testing.T, m *Model, g *mockGrouper) tea.Msg {
				msg := m.loadGroups()()
				m.screen = screenLogs
				return msg
			},
		},
		{
			name: "success chains the stats half",
			setup: func(t *testing.T, m *Model, g *mockGrouper) tea.Msg {
				return m.loadGroups()()
			},
			wantFlag:  true,
			wantChain: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, projects := groupedFixture()
			m := groupedTestModel(g, projects)
			m.refreshInFlight = true
			msg := tt.setup(t, &m, g)

			updated, cmd := m.Update(msg)
			m = updated.(Model)

			if m.refreshInFlight != tt.wantFlag {
				t.Errorf("refreshInFlight = %v, want %v", m.refreshInFlight, tt.wantFlag)
			}
			if (cmd != nil) != tt.wantChain {
				t.Errorf("chained cmd = %v, want %v", cmd != nil, tt.wantChain)
			}
			if !tt.wantChain {
				return
			}
			// The chained half always answers, so the flag always comes back
			// down — that pairing is what makes the re-arm safe.
			updated, _ = m.Update(cmd())
			if updated.(Model).refreshInFlight {
				t.Error("the chained statsMsg must clear refreshInFlight")
			}
		})
	}
}

// A stale grouped arrival must not touch the guard at all: the context-change
// site that bumped the session already reset it, and a stale message that
// cleared it would let the tick stack a second fetch on the live one.
func TestGroupedArrival_StaleSessionLeavesTheGuardAlone(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	msg := m.loadGroups()().(servicesMsg)
	m.statusSession++
	m.refreshInFlight = true

	updated, cmd := m.Update(msg)
	m = updated.(Model)

	if !m.refreshInFlight {
		t.Error("a stale grouped arrival must leave refreshInFlight to its owner")
	}
	if cmd != nil {
		t.Error("a stale grouped arrival must not chain a stats fetch")
	}
	if g.statsCalls2 != 0 {
		t.Errorf("stats calls = %d, want none for a stale arrival", g.statsCalls2)
	}
}

// The chained stats half rides statsSession, the counter the drilled
// refreshStats uses. Both counters are bumped at every context change, so a
// stats message dispatched under one context can never land in another.
func TestGroupedStatsChain_RidesStatsSession(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	// Desynchronise the two counters. Left at their shared zero, the assertion
	// below is satisfied by EITHER field, so a chain riding statusSession ships
	// green — and lands its stats under a session the statsMsg gate rejects.
	m.statusSession, m.statsSession = 7, 3

	updated, cmd := m.Update(m.loadGroups()())
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("precondition: the arrival must chain the stats half")
	}
	msg, ok := cmd().(statsMsg)
	if !ok {
		t.Fatalf("chained cmd produced %T, want statsMsg", cmd())
	}
	if !msg.groupedPayload {
		t.Error("the chained message must carry the grouped shape, or its keys land bare")
	}
	if msg.session != m.statsSession {
		t.Errorf("session = %d, want statsSession %d (statusSession is %d)", msg.session, m.statsSession, m.statusSession)
	}

	m.statsSession++
	m.refreshInFlight = true
	updated, _ = m.Update(msg)
	m = updated.(Model)
	if m.stats != nil {
		t.Errorf("stats = %v, want a stale chained message dropped", m.stats)
	}
	if !m.refreshInFlight {
		t.Error("a stale statsMsg must leave refreshInFlight to its owner")
	}
}

// The chained stats fetch must join against the listing the ARRIVAL carried.
// Handing it a fresh or zero handle costs no test call and raises no error —
// GroupHostStats answers an empty listing with (nil, nil) — so the grouped
// screen would simply render blank CPU/Mem cells forever, with nothing red
// anywhere. Only the handle's identity catches that.
func TestGroupedStatsChain_ConsumesTheArrivalsListing(t *testing.T) {
	g, projects := groupedFixture()
	// A handle distinct from the mock's default, so a chain that re-derived one
	// instead of reading the payload's is caught by identity rather than by
	// luck.
	g.statusEntries = stampedHostEntries("ccc999888777", "blog-web-1", "blog", "web")
	m := groupedTestModel(g, projects)

	msg := m.loadGroups()().(servicesMsg)
	if !reflect.DeepEqual(msg.hostEntries, g.statusEntries) {
		t.Fatal("loadGroups dropped the listing its status half returned")
	}
	updated, cmd := m.Update(msg)
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("precondition: the arrival must chain the stats half")
	}
	cmd()

	if !reflect.DeepEqual(g.statsEntries, g.statusEntries) {
		t.Error("the stats half joined against a different listing than the arrival carried")
	}
}

// The grouped status arrival deliberately leaves m.stats and m.statsErr alone:
// it is the 5-second refresh as well as the first load, and the chained half it
// starts is the slow half of the cycle. Clearing either would blank the CPU/Mem
// column — or drop the "stats unavailable" warning — for that whole window, on
// every tick.
func TestGroupedStatusArrival_KeepsTheStatsCells(t *testing.T) {
	t.Run("cells survive the next status message", func(t *testing.T) {
		g, projects := groupedFixture()
		m := groupedTestModel(g, projects)
		m = groupedCycle(t, m)
		if m.stats[svcKey("shop", "api")].CPUPercent != 12.5 {
			t.Fatalf("precondition: stats = %v, want the first cycle to fill shop/api", m.stats)
		}

		updated, _ := m.Update(m.loadGroups()())
		m = updated.(Model)

		if m.stats[svcKey("shop", "api")].CPUPercent != 12.5 {
			t.Errorf("stats = %v, want the previous cycle's cells until the chained half lands", m.stats)
		}
	})

	t.Run("the soft warning survives the next status message", func(t *testing.T) {
		g, projects := groupedFixture()
		g.statsErr2 = errors.New("docker stats failed")
		m := groupedTestModel(g, projects)
		m = groupedCycle(t, m)
		if m.statsErr == nil {
			t.Fatal("precondition: the first cycle must set statsErr")
		}

		updated, _ := m.Update(m.loadGroups()())
		m = updated.(Model)

		if m.statsErr == nil {
			t.Error("statsErr must survive the status arrival; only the chained statsMsg owns it")
		}
	})
}

// The window between the two grouped messages is where the fetch-stacking guard
// earns its keep: the 5-second tick keeps firing while the slow stats half is
// still out, and an unguarded one would put a second host-wide `docker ps` on
// the wire for every chain already running. The re-arm beside the chain is what
// closes it, so this pins the consequence rather than the flag.
func TestGroupedChainWindow_TickMakesNoSecondFetch(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	m.refreshInFlight = true

	updated, chain := m.Update(m.loadGroups()())
	m = updated.(Model)
	if chain == nil {
		t.Fatal("precondition: the arrival must chain the stats half")
	}
	listings := g.statusCalls2

	// The tick lands with the chained half still out.
	_, cmd := m.Update(refreshTickMsg{})
	if cmd == nil {
		t.Fatal("the tick must reschedule itself whatever it decides about fetching")
	}
	if batch, isBatch := cmd().(tea.BatchMsg); isBatch {
		for _, c := range batch {
			_ = c()
		}
	}

	if g.statusCalls2 != listings {
		t.Errorf("host listings = %d, want no second `docker ps` while the chain is out", g.statusCalls2-listings)
	}
}

// The chained stats fetch runs under a DEADLINE, not the bare m.ctx. The
// arrival that chains it re-arms refreshInFlight and the chained statsMsg is
// the only thing that clears the guard again, so "the message always arrives"
// has to be enforced rather than assumed: m.ctx is a plain WithCancel of
// context.Background() and no dockerRunner adds a deadline of its own.
func TestGroupedStatsChain_IsDeadlineBounded(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	if _, hasDeadline := m.ctx.Deadline(); hasDeadline {
		t.Fatal("precondition: m.ctx must carry no deadline, or this pins nothing")
	}

	updated, cmd := m.Update(m.loadGroups()())
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("precondition: the arrival must chain the stats half")
	}
	cmd()

	deadline, ok := g.statsCtx.Deadline()
	if !ok {
		t.Fatal("the chained stats fetch ran under a context with no deadline")
	}
	if d := time.Until(deadline); d > groupedStatsTimeout {
		t.Errorf("deadline is %v out, want at most groupedStatsTimeout (%v)", d, groupedStatsTimeout)
	}
}

// A wedged transport — `docker ps` returns, `docker stats` never does — must
// degrade to an ordinary soft stats failure: blank cells, a warning, the guard
// CLEARED and the 5-second loop alive. Latched, the guard turns every later
// tick into a pure reschedule and the host view freezes with no error line, no
// spinner and no self-heal short of a context change.
func TestGroupedStatsChain_TimeoutClearsTheGuard(t *testing.T) {
	g, projects := groupedFixture()
	g.statsBlockUntilDone = true
	m := groupedTestModel(g, projects)
	// groupedStatsTimeout is a session-scale bound, so the expiry is driven
	// through the PARENT: context.WithTimeout takes the earlier of the two, and
	// what is under test is that the fetch ends and the guard follows.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	m.ctx = ctx
	m.refreshInFlight = true

	updated, cmd := m.Update(m.loadGroups()())
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("precondition: the arrival must chain the stats half")
	}
	if !m.refreshInFlight {
		t.Fatal("precondition: the chaining arrival re-arms the guard")
	}

	msg := mustNotBlock(t, "the chained stats fetch", cmd)
	updated, _ = m.Update(msg)
	m = updated.(Model)

	if m.refreshInFlight {
		t.Error("a timed-out stats half must still clear refreshInFlight")
	}
	if m.statsErr == nil {
		t.Error("a timed-out stats half must name itself in the soft warning slot")
	}
	if m.stats != nil {
		t.Errorf("stats = %v, want blank cells beside the warning", m.stats)
	}
	if m.svcErr != nil {
		t.Errorf("svcErr = %v; a stats timeout must not blank the status view", m.svcErr)
	}
	if len(m.svcGroups) != 3 {
		t.Error("the rows must survive a stats timeout")
	}
	// And the loop is alive: the next tick fetches instead of rescheduling.
	updated, tickCmd := m.Update(refreshTickMsg{})
	if tickCmd == nil {
		t.Fatal("the tick must always reschedule")
	}
	if !updated.(Model).refreshInFlight {
		t.Error("the periodic refresh stayed silenced after a stats timeout")
	}
}

// An UNSTAMPED listing must not be chained on. Nothing in production sends one
// today, but the handle is a plain struct any package can write, and
// GroupHostStats answers a zero one the way it answers an empty host: the
// screen would render blank CPU/Mem cells for ever with nothing red anywhere.
// The gate is the payload's stamp, not a second seam resolution.
func TestGroupedStatsChain_RefusesAnUnstampedListing(t *testing.T) {
	g, projects := groupedFixture()
	m := groupedTestModel(g, projects)
	m.refreshInFlight = true

	msg := m.loadGroups()().(servicesMsg)
	if !msg.hostEntries.Listed() {
		t.Fatal("precondition: a successful status half must stamp its listing")
	}
	msg.hostEntries = compose.HostEntries{}

	updated, cmd := m.Update(msg)
	m = updated.(Model)

	if cmd != nil {
		t.Error("an unstamped listing must not chain a stats fetch")
	}
	if m.refreshInFlight {
		t.Error("the cycle ends at this message, so the guard clear must stand")
	}
	if g.statsCalls2 != 0 {
		t.Errorf("stats calls = %d, want none for an unstamped listing", g.statsCalls2)
	}
}

// The chain reads the seam off the ARRIVAL, never from a second
// m.hostGrouper() resolution. Nothing forces the two to agree — the factory is
// a mutable Model field — and a chain resolving its own seam would join one
// grouper's stats against another's listing, or hand a freshly built grouper
// the zero handle it answers exactly as it answers an empty host.
func TestGroupedStatsChain_UsesTheArrivalsSeam(t *testing.T) {
	dispatched, projects := groupedFixture()
	m := groupedTestModel(dispatched, projects)

	msg := m.loadGroups()().(servicesMsg)
	if msg.hostGrouper == nil || !msg.hostEntries.Listed() {
		t.Fatal("precondition: the payload must carry both the seam and its listing")
	}

	// The factory now yields a DIFFERENT grouper — a connect, a drill, or just
	// the fresh *HostContainers the real factory builds on every call.
	arrival, _ := groupedFixture()
	m.composerFactory = func(compose.Project) runner.Composer { return arrival }

	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("precondition: the arrival must chain the stats half")
	}
	cmd()

	if dispatched.statsCalls2 != 1 {
		t.Errorf("dispatch-time seam got %d stats calls, want 1", dispatched.statsCalls2)
	}
	if arrival.statsCalls2 != 0 {
		t.Errorf("arrival-time seam got %d stats calls, want none", arrival.statsCalls2)
	}
}

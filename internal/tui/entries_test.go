package tui

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/runner"
)

func testGroup(name string, folded bool, services ...string) svcGroup {
	return svcGroup{proj: compose.Project{Name: name}, services: services, folded: folded}
}

func TestRebuildSvcEntries(t *testing.T) {
	tests := []struct {
		name   string
		groups []svcGroup
		want   []svcEntry
	}{
		{
			name:   "no groups",
			groups: nil,
			want:   nil,
		},
		{
			name:   "single group emits no header",
			groups: []svcGroup{testGroup("web", false, "nginx", "api")},
			want: []svcEntry{
				{kind: entryService, groupIdx: 0, name: "nginx"},
				{kind: entryService, groupIdx: 0, name: "api"},
			},
		},
		{
			// The lone exception to "a single group emits no header": with the
			// header suppressed AND the services folded away, the screen would
			// have zero rows — no cursor, so no way to unfold it again.
			name:   "single folded group keeps its header",
			groups: []svcGroup{testGroup("web", true, "nginx", "api")},
			want: []svcEntry{
				{kind: entrySvcGroupHeader, groupIdx: 0},
			},
		},
		{
			// Same rule, other shape: a sole project whose containers are all
			// gone would otherwise render as a blank screen naming nothing.
			name:   "single empty group keeps its header",
			groups: []svcGroup{testGroup("web", false)},
			want: []svcEntry{
				{kind: entrySvcGroupHeader, groupIdx: 0},
			},
		},
		{
			name: "multi group emits a header per group",
			groups: []svcGroup{
				testGroup("web", false, "nginx", "api"),
				testGroup("db", false, "postgres"),
			},
			want: []svcEntry{
				{kind: entrySvcGroupHeader, groupIdx: 0},
				{kind: entryService, groupIdx: 0, name: "nginx"},
				{kind: entryService, groupIdx: 0, name: "api"},
				{kind: entrySvcGroupHeader, groupIdx: 1},
				{kind: entryService, groupIdx: 1, name: "postgres"},
			},
		},
		{
			name: "folded group keeps its header and hides its services",
			groups: []svcGroup{
				testGroup("web", true, "nginx", "api"),
				testGroup("db", false, "postgres"),
			},
			want: []svcEntry{
				{kind: entrySvcGroupHeader, groupIdx: 0},
				{kind: entrySvcGroupHeader, groupIdx: 1},
				{kind: entryService, groupIdx: 1, name: "postgres"},
			},
		},
		{
			name: "empty group renders a bare header",
			groups: []svcGroup{
				testGroup("web", false, "nginx"),
				testGroup("db", false),
			},
			want: []svcEntry{
				{kind: entrySvcGroupHeader, groupIdx: 0},
				{kind: entryService, groupIdx: 0, name: "nginx"},
				{kind: entrySvcGroupHeader, groupIdx: 1},
			},
		},
		{
			name: "duplicate service names stay separate entries",
			groups: []svcGroup{
				testGroup("web", false, "db"),
				testGroup("shop", false, "db"),
			},
			want: []svcEntry{
				{kind: entrySvcGroupHeader, groupIdx: 0},
				{kind: entryService, groupIdx: 0, name: "db"},
				{kind: entrySvcGroupHeader, groupIdx: 1},
				{kind: entryService, groupIdx: 1, name: "db"},
			},
		},
		{
			name: "unmanaged group is an ordinary group",
			groups: []svcGroup{
				testGroup("web", false, "nginx"),
				{proj: compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true}, services: []string{"portainer"}},
			},
			want: []svcEntry{
				{kind: entrySvcGroupHeader, groupIdx: 0},
				{kind: entryService, groupIdx: 0, name: "nginx"},
				{kind: entrySvcGroupHeader, groupIdx: 1},
				{kind: entryService, groupIdx: 1, name: "portainer"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rebuildSvcEntries(tt.groups)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d entries %+v, want %d %+v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestRebuildSvcEntries_FoldToggleIsIdempotent(t *testing.T) {
	groups := []svcGroup{
		testGroup("web", false, "nginx", "api"),
		testGroup("db", false, "postgres"),
	}
	before := rebuildSvcEntries(groups)

	groups[0].folded = true
	folded := rebuildSvcEntries(groups)
	if len(folded) != 3 {
		t.Fatalf("folded: got %d entries, want 3", len(folded))
	}

	groups[0].folded = false
	after := rebuildSvcEntries(groups)
	if len(after) != len(before) {
		t.Fatalf("unfold did not restore: got %d entries, want %d", len(after), len(before))
	}
	for i := range after {
		if after[i] != before[i] {
			t.Errorf("entry %d = %+v, want %+v", i, after[i], before[i])
		}
	}
}

func TestSvcKey(t *testing.T) {
	tests := []struct {
		name    string
		proj    string
		service string
		want    string
	}{
		{name: "plain", proj: "web", service: "nginx", want: "web/nginx"},
		{name: "empty project", proj: "", service: "nginx", want: "/nginx"},
		{name: "unmanaged", proj: compose.UnmanagedProjectName, service: "portainer", want: "(unmanaged)/portainer"},
		{name: "dashes and underscores", proj: "my-app_2", service: "web_1", want: "my-app_2/web_1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := svcKey(tt.proj, tt.service); got != tt.want {
				t.Errorf("svcKey(%q, %q) = %q, want %q", tt.proj, tt.service, got, tt.want)
			}
		})
	}
}

// The separator is what makes the key unambiguous: docker compose rejects "/"
// in both a project and a service name, so no two distinct pairs can collide.
func TestSvcKey_Distinctness(t *testing.T) {
	pairs := [][2]string{
		{"web", "db"},
		{"shop", "db"},
		{"web", "api"},
		{"webdb", ""},
		{"web", ""},
		{"", "webdb"},
	}

	seen := map[string][2]string{}
	for _, p := range pairs {
		k := svcKey(p[0], p[1])
		if prev, dup := seen[k]; dup {
			t.Errorf("key %q collides: %v and %v", k, prev, p)
			continue
		}
		seen[k] = p
	}
}

// twoGroupModel builds the multi-group shape by hand. The grouped loader lands
// in a later task; the qualified-key contract is already load-bearing, so the
// tests below install the groups directly.
func twoGroupModel(groups ...svcGroup) Model {
	m := Model{selected: map[string]bool{}}
	m.svcGroups = groups
	m.svcEntries = rebuildSvcEntries(m.svcGroups)
	return m
}

// TestSelection_SurvivesFoldRebuild is the reason m.selected is keyed by svcKey
// rather than by row index: folding a group renumbers every row below it, so an
// index-keyed selection would silently slide onto a different service.
func TestSelection_SurvivesFoldRebuild(t *testing.T) {
	m := twoGroupModel(
		testGroup("web", false, "nginx", "api"),
		testGroup("shop", false, "db"),
	)
	// rows: 0 header web, 1 nginx, 2 api, 3 header shop, 4 db
	if got := m.svcKeyAt(4); got != "shop/db" {
		t.Fatalf("precondition: svcKeyAt(4) = %q, want shop/db", got)
	}
	m.selected[m.svcKeyAt(4)] = true

	m.svcGroups[0].folded = true
	m.svcEntries = rebuildSvcEntries(m.svcGroups)

	// rows: 0 header web (folded), 1 header shop, 2 db
	if got := m.svcKeyAt(2); got != "shop/db" {
		t.Fatalf("after fold: svcKeyAt(2) = %q, want shop/db", got)
	}
	if !m.selected[m.svcKeyAt(2)] {
		t.Error("shop/db lost its selection across the fold rebuild")
	}
	for i := range m.svcEntries {
		if i == 2 {
			continue
		}
		if m.selected[m.svcKeyAt(i)] {
			t.Errorf("row %d picked up a selection it never had", i)
		}
	}
}

// TestQualifiedKeys_DuplicateServiceNamesStayDistinct pins the collision the
// whole key scheme exists for: two projects on one host routinely both own a
// "db", and selection, status and stats must not merge them.
func TestQualifiedKeys_DuplicateServiceNamesStayDistinct(t *testing.T) {
	m := twoGroupModel(
		testGroup("web", false, "db"),
		testGroup("shop", false, "db"),
	)
	// rows: 0 header web, 1 db, 2 header shop, 3 db
	webDB, shopDB := m.svcKeyAt(1), m.svcKeyAt(3)
	if webDB == shopDB {
		t.Fatalf("both rows resolved to %q — the group prefix is missing", webDB)
	}

	m.selected[webDB] = true
	if m.selected[shopDB] {
		t.Error("selecting web/db also selected shop/db")
	}

	m.svcStatus = map[string]runner.ServiceStatus{
		webDB:  {Running: true},
		shopDB: {Running: false},
	}
	m.stats = map[string]runner.ServiceStats{
		webDB:  {CPUPercent: 10},
		shopDB: {CPUPercent: 90},
	}
	if !m.svcStatus[webDB].Running || m.svcStatus[shopDB].Running {
		t.Error("the two db rows share one status entry")
	}
	if m.stats[webDB].CPUPercent != 10 || m.stats[shopDB].CPUPercent != 90 {
		t.Error("the two db rows share one stats entry")
	}
}

// recordingComposer captures the container slice every pipeline step receives,
// so a test can assert what actually crossed the tui → runner boundary.
//
// The pipeline runs on its own goroutine, so every access is behind mu: the
// test goroutine reads the slice while runner.Run is still appending to it.
type recordingComposer struct {
	mockComposer
	mu            sync.Mutex
	gotContainers [][]string
	recorded      chan struct{} // one send per step; buffered past the step count so record never blocks
}

func (c *recordingComposer) record(containers []string) {
	c.mu.Lock()
	c.gotContainers = append(c.gotContainers, append([]string(nil), containers...))
	c.mu.Unlock()
	if c.recorded != nil {
		c.recorded <- struct{}{}
	}
}

// calls returns a snapshot of what the pipeline has recorded so far.
func (c *recordingComposer) calls() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]string(nil), c.gotContainers...)
}

func (c *recordingComposer) Stop(ctx context.Context, containers []string, w io.Writer) error {
	c.record(containers)
	return nil
}

func (c *recordingComposer) Remove(ctx context.Context, containers []string, w io.Writer) error {
	c.record(containers)
	return nil
}

func (c *recordingComposer) Pull(ctx context.Context, containers []string, w io.Writer) error {
	c.record(containers)
	return nil
}

func (c *recordingComposer) Create(ctx context.Context, containers []string, w io.Writer) error {
	c.record(containers)
	return nil
}

func (c *recordingComposer) Start(ctx context.Context, containers []string, w io.Writer) error {
	c.record(containers)
	return nil
}

// TestQualifiedKeys_NeverCrossIntoRunner is the boundary pin. Qualified keys
// live only inside the Model: runner and compose address a service by the bare
// name docker compose knows it by, so a leaked "proj/svc" would reach the
// docker CLI as a service that does not exist.
func TestQualifiedKeys_NeverCrossIntoRunner(t *testing.T) {
	steps := len(runner.Steps(runner.Deploy))
	rc := &recordingComposer{
		mockComposer: mockComposer{
			services: []string{"api", "db"},
			status:   map[string]runner.ServiceStatus{"api": {Running: true}, "db": {Running: true}},
		},
		recorded: make(chan struct{}, steps),
	}
	m := NewModel(rc, io.Discard, func(compose.Project) runner.Composer { return rc }, nil, nil)
	m.screen = screenSelectContainers
	m.projName = "shop"
	m.setSingleGroup(rc.services)
	m.svcStatus = qStatus(m, rc.status)
	m.width, m.height = 120, 24
	installFakeTick(&m)

	m.selected[m.svcKeyAt(0)] = true
	if got := m.selectedContainers(); len(got) != 1 || got[0] != "api" {
		t.Fatalf("selectedContainers() = %v, want [api] — the boundary must strip the prefix", got)
	}

	// d → confirm: enterProgress records the pipeline's target set.
	armed, _ := m.Update(keyMsgFor("d"))
	m = armed.(Model)
	if !m.confirming {
		t.Fatal("precondition: d must arm the confirmation")
	}
	running, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = running.(Model)
	for _, name := range m.opContainers {
		if strings.Contains(name, svcKeySep) {
			t.Errorf("opContainers carries a qualified key: %q", name)
		}
	}
	if len(m.opContainers) != 1 || m.opContainers[0] != "api" {
		t.Fatalf("opContainers = %v, want [api]", m.opContainers)
	}

	// Drain the pipeline so the composer actually sees its arguments.
	if cmd != nil {
		cmd()
	}
	deadline := time.After(2 * time.Second)
	for i := 0; i < steps; i++ {
		select {
		case <-rc.recorded:
		case <-deadline:
			t.Fatalf("pipeline did not finish: %v", rc.calls())
		}
	}
	for _, call := range rc.calls() {
		for _, name := range call {
			if strings.Contains(name, svcKeySep) {
				t.Errorf("a qualified key reached the composer: %q", name)
			}
		}
	}

	// The wait phase seeds NewWaitState from the same set.
	waited, _ := m.Update(pipelineDoneMsg{})
	m = waited.(Model)
	if !m.waiting {
		t.Fatal("precondition: a Deploy must enter the wait phase")
	}
	for _, name := range m.waitState.Services {
		if strings.Contains(name, svcKeySep) {
			t.Errorf("waitState seeded with a qualified key: %q", name)
		}
	}
	for name := range m.waitState.Verdicts {
		if strings.Contains(name, svcKeySep) {
			t.Errorf("waitState verdict keyed by a qualified key: %q", name)
		}
	}
}

func groupShape(groups []svcGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.proj.Name+"["+strings.Join(g.services, ",")+"]")
	}
	return out
}

func TestBuildSvcGroups_OrderComesFromTheLoader(t *testing.T) {
	projects := []compose.Project{
		{Name: "blog", ConfigDir: "/srv/blog"},
		{Name: "shop", ConfigDir: "/srv/shop"},
		{Name: compose.UnmanagedProjectName, Unmanaged: true},
	}
	host := map[string]map[string]runner.ServiceStatus{
		"shop":                       {"api": {Running: true}, "db": {}},
		"blog":                       {"web": {Running: true}},
		compose.UnmanagedProjectName: {"watchtower": {Running: true}},
	}

	got := buildSvcGroups(projects, host, nil)
	want := []string{"blog[web]", "shop[api,db]", "(unmanaged)[watchtower]"}
	if shape := groupShape(got); !slices.Equal(shape, want) {
		t.Errorf("groups = %v, want %v", shape, want)
	}
	if got[1].proj.ConfigDir != "/srv/shop" {
		t.Errorf("shop ConfigDir = %q, want the loader's value", got[1].proj.ConfigDir)
	}
	if !got[2].proj.Unmanaged {
		t.Error("the unmanaged row must keep its Unmanaged flag")
	}
}

// A project the loader reported but the host has no container for is a real
// state (everything removed), so it renders as an empty group rather than
// vanishing.
func TestBuildSvcGroups_EmptyProjectKeepsItsGroup(t *testing.T) {
	got := buildSvcGroups([]compose.Project{{Name: "idle"}}, nil, nil)
	if len(got) != 1 || got[0].proj.Name != "idle" || len(got[0].services) != 0 {
		t.Fatalf("groups = %v, want one empty idle group", groupShape(got))
	}
	// A lone group with no service rows KEEPS its header: without it the screen
	// has zero rows, hence no cursor, hence no way to drill in or even see which
	// project is being looked at.
	entries := rebuildSvcEntries(got)
	if len(entries) != 1 || entries[0].kind != entrySvcGroupHeader {
		t.Errorf("a lone empty group must render its header, got %v", entries)
	}
}

// `docker compose ls` and `docker ps` are two calls and can disagree. A
// container whose project the loader missed must still be visible.
func TestBuildSvcGroups_HostOnlyProjectsAppendedLast(t *testing.T) {
	host := map[string]map[string]runner.ServiceStatus{
		"known":                      {"a": {}},
		"zulu":                       {"b": {}},
		"alpha":                      {"c": {}},
		compose.UnmanagedProjectName: {"stray": {}},
	}
	got := buildSvcGroups([]compose.Project{{Name: "known"}}, host, nil)
	want := []string{"known[a]", "alpha[c]", "zulu[b]", "(unmanaged)[stray]"}
	if shape := groupShape(got); !slices.Equal(shape, want) {
		t.Errorf("groups = %v, want %v", shape, want)
	}
	if !got[3].proj.Unmanaged {
		t.Error("a host-only unmanaged bucket must still be flagged Unmanaged")
	}
}

// Folding is UI state the user set, and buildSvcGroups also runs on the 5s
// reload, so it has to survive a refresh. It is matched by project NAME
// because the group slice is rebuilt wholesale.
func TestBuildSvcGroups_PreservesFoldState(t *testing.T) {
	prev := []svcGroup{
		{proj: compose.Project{Name: "shop"}, folded: true},
		{proj: compose.Project{Name: "blog"}},
	}
	host := map[string]map[string]runner.ServiceStatus{"shop": {"api": {}}, "blog": {"web": {}}}
	got := buildSvcGroups([]compose.Project{{Name: "shop"}, {Name: "blog"}}, host, prev)
	if !got[0].folded {
		t.Error("shop lost its fold across the reload")
	}
	if got[1].folded {
		t.Error("blog was never folded")
	}
}

func TestBuildSvcGroups_DuplicateAndUnnamedProjectsDropped(t *testing.T) {
	projects := []compose.Project{{Name: "shop"}, {Name: "shop"}, {Name: ""}}
	got := buildSvcGroups(projects, map[string]map[string]runner.ServiceStatus{"shop": {"api": {}}}, nil)
	if shape := groupShape(got); !slices.Equal(shape, []string{"shop[api]"}) {
		t.Errorf("groups = %v, want a single shop group", shape)
	}
}

func TestBuildSvcGroups_EmptyHostIsNonNil(t *testing.T) {
	got := buildSvcGroups(nil, nil, nil)
	if got == nil {
		t.Fatal("buildSvcGroups() = nil; an empty host must still install an empty group slice")
	}
	if len(got) != 0 {
		t.Errorf("groups = %v, want none", groupShape(got))
	}
}

func TestFlattenQualified(t *testing.T) {
	if got := flattenQualified[runner.ServiceStatus](nil); got != nil {
		t.Errorf("flattenQualified(nil) = %v, want nil", got)
	}
	// Two projects owning a service of the same name is the case the qualified
	// key exists for: neither may overwrite the other.
	host := map[string]map[string]runner.ServiceStatus{
		"shop": {"db": {Running: true}},
		"blog": {"db": {Running: false}},
	}
	got := flattenQualified(host)
	if len(got) != 2 {
		t.Fatalf("flattenQualified() = %v, want 2 distinct keys", got)
	}
	if !got[svcKey("shop", "db")].Running || got[svcKey("blog", "db")].Running {
		t.Errorf("flattenQualified() = %v; the two db services collided", got)
	}
}

// groupsHaveHeaders is the single home of the header rule, and the renderer's
// indent reads it too — so a grouped host holding exactly one project must
// report false, or the drilled screen and the one-project host would disagree.
//
// The two single-group exceptions are the finding pin: a lone group that
// contributes no service rows keeps its header, because suppressing it empties
// the screen and an empty screen has no cursor to unfold or drill from.
func TestGroupsHaveHeaders(t *testing.T) {
	tests := []struct {
		name   string
		groups []svcGroup
		want   bool
	}{
		{"none", nil, false},
		{"one", []svcGroup{{proj: compose.Project{Name: "web"}, services: []string{"nginx"}}}, false},
		{"one folded", []svcGroup{{proj: compose.Project{Name: "web"}, services: []string{"nginx"}, folded: true}}, true},
		{"one empty", []svcGroup{{proj: compose.Project{Name: "web"}}}, true},
		{"two", []svcGroup{{proj: compose.Project{Name: "web"}}, {proj: compose.Project{Name: "db"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := groupsHaveHeaders(tt.groups); got != tt.want {
				t.Errorf("groupsHaveHeaders = %v, want %v", got, tt.want)
			}
			m := Model{svcGroups: tt.groups}
			if got := groupsHaveHeaders(m.svcGroups); got != tt.want {
				t.Errorf("groupsHaveHeaders = %v, want %v", got, tt.want)
			}
			// The rule and the entry model may never disagree.
			hasHeader := false
			for _, e := range rebuildSvcEntries(tt.groups) {
				if e.kind == entrySvcGroupHeader {
					hasHeader = true
				}
			}
			if len(tt.groups) > 0 && hasHeader != tt.want {
				t.Errorf("rebuildSvcEntries emitted headers = %v, want %v", hasHeader, tt.want)
			}
		})
	}
}

func TestGroupCounts(t *testing.T) {
	g := svcGroup{
		proj:     compose.Project{Name: "web"},
		services: []string{"api", "nginx", "cache", "gone"},
	}
	yes, no := true, false
	status := map[string]runner.ServiceStatus{
		svcKey("web", "api"):   {Running: true, UpdateAvailable: &yes},
		svcKey("web", "nginx"): {Running: true, Health: "unhealthy", UpdateAvailable: &no},
		// "cache" was never checked: a nil verdict counts exactly like a false
		// one, so a folded header can only report what a scan established.
		svcKey("web", "cache"): {Health: "starting"},
		// "gone" has no entry at all — the host reports only containers that
		// exist, and an absent one must NOT inflate any of the totals.
		svcKey("db", "api"): {Running: true, UpdateAvailable: &yes},
	}
	up, unhealthy, updates := groupCounts(g, status)
	if up != 2 {
		t.Errorf("up = %d, want 2", up)
	}
	if unhealthy != 1 {
		t.Errorf("unhealthy = %d, want 1", unhealthy)
	}
	if updates != 1 {
		t.Errorf("updates = %d, want 1 (only web/api has a true verdict)", updates)
	}

	if up, unhealthy, updates = groupCounts(g, nil); up != 0 || unhealthy != 0 || updates != 0 {
		t.Errorf("groupCounts with no status = (%d, %d, %d), want (0, 0, 0)", up, unhealthy, updates)
	}
}

// eachSelectableRef drops the unmanaged bucket: those rows draw no checkbox, so
// nothing that reads a selection may count them.
func TestSelectableRefs_DropsUnmanaged(t *testing.T) {
	m := Model{svcGroups: []svcGroup{
		{proj: compose.Project{Name: "web"}, services: []string{"api", "nginx"}},
		{proj: compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true}, services: []string{"watchtower"}},
	}}
	if got := len(m.svcRefs()); got != 3 {
		t.Fatalf("svcRefs = %d, want 3 (every service, unmanaged included)", got)
	}
	var refs []svcRef
	m.eachSelectableRef(func(r svcRef) bool {
		refs = append(refs, r)
		return true
	})
	if len(refs) != 2 {
		t.Fatalf("eachSelectableRef yielded %v, want the two compose services", refs)
	}
	for _, r := range refs {
		if r.groupIdx != 0 {
			t.Errorf("eachSelectableRef kept %+v from the unmanaged bucket", r)
		}
	}
	if !m.groupUnmanaged(1) {
		t.Error("groupUnmanaged(1) = false, want true")
	}
	for _, gi := range []int{-1, 0, 7} {
		if m.groupUnmanaged(gi) {
			t.Errorf("groupUnmanaged(%d) = true, want false", gi)
		}
	}
}

// cursorGroup answers on a header row, which is exactly what separates it from
// cursorService: drill-in and config act on a whole project, and a header IS
// that project.
func TestCursorGroup(t *testing.T) {
	m := Model{}
	m.svcGroups = []svcGroup{
		{proj: compose.Project{Name: "web"}, services: []string{"api"}},
		{proj: compose.Project{Name: "db"}, services: []string{"postgres"}},
	}
	m.svcEntries = rebuildSvcEntries(m.svcGroups)

	cases := []struct {
		cursor int
		want   string
	}{
		{0, "web"}, // header
		{1, "web"}, // service
		{2, "db"},  // header
		{3, "db"},  // service
	}
	for _, tc := range cases {
		m.svcCursor = tc.cursor
		g, ok := m.cursorGroup()
		if !ok || g.proj.Name != tc.want {
			t.Errorf("cursor %d: cursorGroup() = %q %v, want %q true", tc.cursor, g.proj.Name, ok, tc.want)
		}
	}

	m.svcCursor = 99
	if _, ok := m.cursorGroup(); ok {
		t.Error("an out-of-range cursor must report no group, not panic")
	}

	empty := Model{}
	if _, ok := empty.cursorGroup(); ok {
		t.Error("an empty row model must report no group")
	}
}

// batchShape renders the partition compactly so a test can assert order and
// membership in one comparison. "(all)" marks the empty slice the runner reads
// as every service.
func batchShape(batches []opBatch) []string {
	out := make([]string, 0, len(batches))
	for _, b := range batches {
		if len(b.services) == 0 {
			out = append(out, b.proj.Name+":all")
			continue
		}
		out = append(out, b.proj.Name+":"+strings.Join(b.services, ","))
	}
	return out
}

func partitionModel() Model {
	m := Model{selected: map[string]bool{}}
	m.svcGroups = []svcGroup{
		{proj: compose.Project{Name: "web"}, services: []string{"nginx", "api"}},
		{proj: compose.Project{Name: "shop"}, services: []string{"api", "db"}},
		{proj: compose.Project{Name: compose.UnmanagedProjectName, Unmanaged: true}, services: []string{"watchtower"}},
	}
	m.svcEntries = rebuildSvcEntries(m.svcGroups)
	return m
}

func TestPartitionSelection_OrderedByScreenPosition(t *testing.T) {
	m := partitionModel()
	// Select bottom-up and out of group order; the partition must still come
	// back in row order, because that is the order the batches will run in.
	m.selected["shop/db"] = true
	m.selected["web/api"] = true
	m.selected["shop/api"] = true

	got := batchShape(m.partitionSelection())
	want := []string{"web:api", "shop:api,db"}
	if !slices.Equal(got, want) {
		t.Errorf("partitionSelection() = %v, want %v", got, want)
	}
}

func TestPartitionSelection_EmptySelectionIsTheCursorGroup(t *testing.T) {
	m := partitionModel()
	// rows: 0 header web, 1 nginx, 2 api, 3 header shop, 4 api, 5 db,
	//       6 header (unmanaged), 7 watchtower
	cases := []struct {
		cursor int
		want   []string
	}{
		{0, []string{"web:all"}},  // header
		{2, []string{"web:all"}},  // service row inside web
		{3, []string{"shop:all"}}, // header
		{5, []string{"shop:all"}}, // service row inside shop
		{6, nil},                  // unmanaged header: no pipeline to run
		{7, nil},                  // unmanaged service row
		{99, nil},                 // no row at all
	}
	for _, tc := range cases {
		m.svcCursor = tc.cursor
		got := batchShape(m.partitionSelection())
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("cursor %d: partitionSelection() = %v, want %v", tc.cursor, got, tc.want)
		}
	}
}

// The whole-group batch must carry an EMPTY slice, not the row list: the rows
// come from `docker ps`, so a never-created service has none — only the empty
// slice reaches it, because compose resolves it as "all".
func TestPartitionSelection_WholeGroupBatchCarriesNoServices(t *testing.T) {
	m := partitionModel()
	m.svcCursor = 1
	batches := m.partitionSelection()
	if len(batches) != 1 {
		t.Fatalf("partitionSelection() = %v, want one batch", batchShape(batches))
	}
	if len(batches[0].services) != 0 {
		t.Errorf("whole-group batch services = %v, want empty (compose resolves all)", batches[0].services)
	}
}

// An unmanaged key cannot be selected (the space handler refuses it), but the
// partition refuses it a second time so a stale key can never become a batch
// against a project that has no compose file.
func TestPartitionSelection_UnmanagedNeverEntersABatch(t *testing.T) {
	m := partitionModel()
	m.selected[svcKey(compose.UnmanagedProjectName, "watchtower")] = true
	m.selected["web/nginx"] = true

	got := batchShape(m.partitionSelection())
	if !slices.Equal(got, []string{"web:nginx"}) {
		t.Errorf("partitionSelection() = %v, want [web:nginx]", got)
	}
}

func TestPartitionSelection_NoGroups(t *testing.T) {
	m := Model{selected: map[string]bool{}}
	if batches := m.partitionSelection(); batches != nil {
		t.Errorf("partitionSelection() = %v on an empty row model, want nil", batchShape(batches))
	}
}

func TestFormatBatchTargets(t *testing.T) {
	tests := []struct {
		name    string
		batches []opBatch
		want    string
	}{
		{"none", nil, ""},
		{
			"one project with services",
			[]opBatch{{proj: compose.Project{Name: "web"}, services: []string{"nginx", "api"}}},
			"web (nginx, api)",
		},
		{
			"whole project",
			[]opBatch{{proj: compose.Project{Name: "db"}}},
			"db (all)",
		},
		{
			"two projects",
			[]opBatch{
				{proj: compose.Project{Name: "web"}, services: []string{"nginx", "api"}},
				{proj: compose.Project{Name: "db"}},
			},
			"web (nginx, api) → db (all)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatBatchTargets(tc.batches); got != tc.want {
				t.Errorf("formatBatchTargets() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSanitizeName_StripsTerminalControls pins the sanitiser the grouped view
// depends on: project and service names come from container LABELS, which any
// `docker run --label` sets to arbitrary bytes.
func TestSanitizeName_StripsTerminalControls(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain name is untouched", "shop-api_1.0", "shop-api_1.0"},
		{"OSC 52 clipboard write", "web\x1b]52;c;cHduZWQ=\a", "web"},
		{"CSI screen clear", "web\x1b[2J\x1b[Hgone", "webgone"},
		{"bare BEL and CR", "web\a\r", "web"},
		{"8-bit CSI introducer", "web\u009b2Kx", "web2Kx"},
		{"8-bit OSC introducer", "web\u009d52;c;x", "web52;c;x"},
		{"DEL", "web\u007f", "web"},
		{"tab and newline", "we\tb\nc", "webc"},
		{"unicode survives", "web-Ω", "web-Ω"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeName(tt.in); got != tt.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// Idempotent: svcKey re-applies it to names the groups already hold.
			if got := sanitizeName(sanitizeName(tt.in)); got != tt.want {
				t.Errorf("sanitizeName is not idempotent for %q", tt.in)
			}
		})
	}
}

// TestBuildSvcGroups_SanitizesLabelDerivedNames is the injection pin. The
// grouped screen redraws every 5 seconds on its own, so an escape sequence in a
// label would be replayed into the terminal without the user pressing a key.
// The row must survive - hiding a container is not the fix - with the escape
// gone and its status still joined to it.
func TestBuildSvcGroups_SanitizesLabelDerivedNames(t *testing.T) {
	const evilProj = "sh\x1b]52;c;cHduZWQ=\aop"
	const evilSvc = "ap\x1b[2Ji"
	host := map[string]map[string]runner.ServiceStatus{
		evilProj: {evilSvc: {Running: true}},
	}
	got := buildSvcGroups([]compose.Project{{Name: evilProj, ConfigDir: "/srv/shop"}}, host, nil)
	if len(got) != 1 {
		t.Fatalf("groups = %v, want one", groupShape(got))
	}
	if got[0].proj.Name != "shop" {
		t.Errorf("project name = %q, want %q", got[0].proj.Name, "shop")
	}
	if len(got[0].services) != 1 || got[0].services[0] != "api" {
		t.Fatalf("services = %v, want [api]", got[0].services)
	}
	// The status map still joins: flattenQualified goes through svcKey, which
	// sanitizes both halves, so the row keeps its running dot.
	status := flattenQualified(host)
	if !status[svcKey(got[0].proj.Name, got[0].services[0])].Running {
		t.Error("the sanitized row lost its status; the key halves disagree")
	}
}

// A group whose fold state was set before a refresh must keep it, and the fold
// map is keyed by the sanitized name on both sides.
func TestBuildSvcGroups_FoldSurvivesSanitizing(t *testing.T) {
	const evilProj = "sh\x1b[31mop"
	host := map[string]map[string]runner.ServiceStatus{evilProj: {"api": {}}}
	first := buildSvcGroups([]compose.Project{{Name: evilProj}}, host, nil)
	first[0].folded = true
	second := buildSvcGroups([]compose.Project{{Name: evilProj}}, host, first)
	if len(second) != 1 || !second[0].folded {
		t.Errorf("fold state lost across a refresh: %+v", second)
	}
}

// TestBuildSvcGroups_AuthenticNameWinsACollision is the displacement pin. Order
// comes from the loader, which sorts RAW names, so a label crafted to sanitize
// onto a real project's name can sort BEFORE it. A plain first-wins dedupe then
// dropped the real project and left the forged row — carrying an attacker-chosen
// ConfigDir — occupying the real project's display name, which is what
// bindProjComposer / drillIntoGroup would hand to the composer factory.
func TestBuildSvcGroups_AuthenticNameWinsACollision(t *testing.T) {
	const forged = "we\x01b" // sanitizes to "web" and sorts before it
	host := map[string]map[string]runner.ServiceStatus{
		forged: {"evil": {Running: true}},
		"web":  {"nginx": {Running: true}},
	}
	// The loader's order: the forged name first, exactly as sortProjects would.
	projects := []compose.Project{
		{Name: forged, ConfigDir: "/tmp/attacker"},
		{Name: "web", ConfigDir: "/srv/web"},
	}
	got := buildSvcGroups(projects, host, nil)
	if len(got) != 1 {
		t.Fatalf("groups = %v, want the collision collapsed to one", groupShape(got))
	}
	if got[0].proj.Name != "web" {
		t.Fatalf("group name = %q, want web", got[0].proj.Name)
	}
	if got[0].proj.ConfigDir != "/srv/web" {
		t.Errorf("ConfigDir = %q, want the REAL project's /srv/web", got[0].proj.ConfigDir)
	}
	if len(got[0].services) != 1 || got[0].services[0] != "nginx" {
		t.Errorf("services = %v, want the real project's [nginx]", got[0].services)
	}
}

// The same collapse inside one group produced duplicate rows: svcs was sorted
// but never deduped, so one svcKey addressed two rows.
func TestBuildSvcGroups_DedupesCollidingServiceNames(t *testing.T) {
	host := map[string]map[string]runner.ServiceStatus{
		"web": {"api": {Running: true}, "ap\x01i": {}},
	}
	got := buildSvcGroups([]compose.Project{{Name: "web", ConfigDir: "/srv/web"}}, host, nil)
	if len(got) != 1 {
		t.Fatalf("groups = %v, want one", groupShape(got))
	}
	if len(got[0].services) != 1 || got[0].services[0] != "api" {
		t.Errorf("services = %v, want the single deduped [api]", got[0].services)
	}
}

// The leftover pass is the "never leave a running container invisible" net, so a
// real project the loader missed must still appear even when a forged label
// already claimed its sanitized name.
func TestBuildSvcGroups_LeftoverRealProjectDisplacesAForgedRow(t *testing.T) {
	const forged = "we\x01b"
	host := map[string]map[string]runner.ServiceStatus{
		forged: {"evil": {}},
		"web":  {"nginx": {Running: true}},
	}
	// Only the forged project is reported by the loader; `web` is a leftover.
	got := buildSvcGroups([]compose.Project{{Name: forged, ConfigDir: "/tmp/attacker"}}, host, nil)
	if len(got) != 1 {
		t.Fatalf("groups = %v, want one", groupShape(got))
	}
	if got[0].proj.ConfigDir != "" || len(got[0].services) != 1 || got[0].services[0] != "nginx" {
		t.Errorf("group = %+v, want the real leftover web group", got[0])
	}
}

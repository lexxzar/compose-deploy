package tui

import (
	"context"
	"io"
	"strings"
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
				{kind: entrySvcService, groupIdx: 0, name: "nginx"},
				{kind: entrySvcService, groupIdx: 0, name: "api"},
			},
		},
		{
			name:   "single folded group emits nothing",
			groups: []svcGroup{testGroup("web", true, "nginx", "api")},
			want:   nil,
		},
		{
			name:   "single empty group emits nothing",
			groups: []svcGroup{testGroup("web", false)},
			want:   nil,
		},
		{
			name: "multi group emits a header per group",
			groups: []svcGroup{
				testGroup("web", false, "nginx", "api"),
				testGroup("db", false, "postgres"),
			},
			want: []svcEntry{
				{kind: entrySvcGroupHeader, groupIdx: 0},
				{kind: entrySvcService, groupIdx: 0, name: "nginx"},
				{kind: entrySvcService, groupIdx: 0, name: "api"},
				{kind: entrySvcGroupHeader, groupIdx: 1},
				{kind: entrySvcService, groupIdx: 1, name: "postgres"},
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
				{kind: entrySvcService, groupIdx: 1, name: "postgres"},
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
				{kind: entrySvcService, groupIdx: 0, name: "nginx"},
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
				{kind: entrySvcService, groupIdx: 0, name: "db"},
				{kind: entrySvcGroupHeader, groupIdx: 1},
				{kind: entrySvcService, groupIdx: 1, name: "db"},
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
				{kind: entrySvcService, groupIdx: 0, name: "nginx"},
				{kind: entrySvcGroupHeader, groupIdx: 1},
				{kind: entrySvcService, groupIdx: 1, name: "portainer"},
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
type recordingComposer struct {
	mockComposer
	gotContainers [][]string
}

func (c *recordingComposer) record(containers []string) {
	c.gotContainers = append(c.gotContainers, append([]string(nil), containers...))
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
	rc := &recordingComposer{mockComposer: mockComposer{
		services: []string{"api", "db"},
		status:   map[string]runner.ServiceStatus{"api": {Running: true}, "db": {Running: true}},
	}}
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
	for len(rc.gotContainers) < len(runner.Steps(runner.Deploy)) {
		select {
		case <-deadline:
			t.Fatalf("pipeline did not finish: %v", rc.gotContainers)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	for _, call := range rc.gotContainers {
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

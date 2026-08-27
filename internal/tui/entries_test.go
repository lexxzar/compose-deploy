package tui

import (
	"testing"

	"github.com/lexxzar/compose-deploy/internal/compose"
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

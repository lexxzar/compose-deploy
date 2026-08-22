package compose

import (
	"context"
	"errors"
	"testing"
)

// The project-picker row and its count. The fakeDockerRunner seam and the
// hostPsMixed fixture live in hostcontainers_test.go, beside the composer they
// drive.

func TestHostContainers_CountUnmanaged(t *testing.T) {
	tests := []struct {
		name string
		ps   string
		want int
	}{
		{
			name: "mixed",
			ps:   hostPsMixed,
			want: 3,
		},
		{
			name: "all managed",
			ps: `{"ID":"aaa111222333","Names":"web","State":"running","Labels":"com.docker.compose.project=my-app"}
{"ID":"bbb444555666","Names":"db","State":"running","Labels":"com.docker.compose.project=my-app"}`,
			want: 0,
		},
		{
			name: "none at all",
			ps:   "",
			want: 0,
		},
		{
			name: "single unmanaged",
			ps:   `{"ID":"bbb444555666","Names":"watchtower","State":"running","Labels":""}`,
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(tt.ps)}}
			got, err := h.CountUnmanaged(context.Background())
			if err != nil {
				t.Fatalf("CountUnmanaged() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("CountUnmanaged() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestHostContainers_CountUnmanaged_Error(t *testing.T) {
	h := &HostContainers{docker: &fakeDockerRunner{runErr: errors.New("docker daemon not running")}}

	got, err := h.CountUnmanaged(context.Background())
	if err == nil {
		t.Fatal("CountUnmanaged() error = nil, want error")
	}
	if got != 0 {
		t.Errorf("CountUnmanaged() = %d, want 0 on error", got)
	}
}

func TestWithUnmanagedRow(t *testing.T) {
	base := []Project{
		{Name: "my-app", Status: "running(3)", ConfigDir: "/srv/my-app"},
		{Name: "other", Status: "running(1)", ConfigDir: "/srv/other"},
	}

	tests := []struct {
		name      string
		ps        string
		runErr    error
		wantExtra bool
		wantDesc  string
	}{
		{name: "appended when non-zero", ps: hostPsMixed, wantExtra: true, wantDesc: "3 containers"},
		{name: "singular when one", ps: `{"ID":"bbb444555666","Names":"watchtower","Labels":""}`, wantExtra: true, wantDesc: "1 container"},
		{name: "absent when zero", ps: `{"ID":"aaa111222333","Names":"web","Labels":"com.docker.compose.project=my-app"}`},
		// CountUnmanaged returns (0, err) on every failure, so this case can
		// only ever exercise the same outcome as "absent when zero". It pins
		// the CONTRACT — a discovery failure must never invent a row — and
		// would catch a future CountUnmanaged that returned a partial count
		// alongside an error.
		{name: "absent on error", runErr: errors.New("boom")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(tt.ps), runErr: tt.runErr}}
			got := WithUnmanagedRow(context.Background(), h, append([]Project(nil), base...))

			if !tt.wantExtra {
				if len(got) != len(base) {
					t.Fatalf("projects = %+v, want no extra row", got)
				}
				return
			}
			if len(got) != len(base)+1 {
				t.Fatalf("projects = %+v, want one extra row", got)
			}
			row := got[len(got)-1]
			if row.Name != UnmanagedProjectName {
				t.Errorf("row name = %q, want %q", row.Name, UnmanagedProjectName)
			}
			if row.Desc != tt.wantDesc {
				t.Errorf("row desc = %q, want %q", row.Desc, tt.wantDesc)
			}
			if row.Status != "" {
				t.Errorf("row Status = %q, want empty — the picker text lives in Desc", row.Status)
			}
			if !row.Unmanaged {
				t.Error("row Unmanaged = false, want true")
			}
			if row.ConfigDir != "" {
				t.Errorf("row ConfigDir = %q, want empty", row.ConfigDir)
			}
		})
	}
}

// TestWithUnmanagedRow_AlwaysLast pins that the row bypasses sortProjects: "("
// sorts before every letter, so a sorted row would land first.
func TestWithUnmanagedRow_AlwaysLast(t *testing.T) {
	projects := []Project{{Name: "zebra"}, {Name: "alpha"}}
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(hostPsMixed)}}

	got := WithUnmanagedRow(context.Background(), h, projects)
	if len(got) != 3 {
		t.Fatalf("projects = %+v, want 3 rows", got)
	}
	if got[0].Name != "zebra" || got[1].Name != "alpha" {
		t.Errorf("existing order changed: %+v", got)
	}
	if !got[2].Unmanaged {
		t.Errorf("last row = %+v, want the unmanaged row", got[2])
	}
}

func TestWithUnmanagedRow_NilComposer(t *testing.T) {
	projects := []Project{{Name: "my-app"}}
	if got := WithUnmanagedRow(context.Background(), nil, projects); len(got) != 1 {
		t.Errorf("projects = %+v, want the input unchanged", got)
	}
}

func TestWithUnmanagedRow_EmptyProjectList(t *testing.T) {
	h := &HostContainers{docker: &fakeDockerRunner{runOut: []byte(hostPsMixed)}}

	got := WithUnmanagedRow(context.Background(), h, nil)
	if len(got) != 1 || !got[0].Unmanaged {
		t.Fatalf("projects = %+v, want the unmanaged row alone", got)
	}
}

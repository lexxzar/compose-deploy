package compose

import (
	"os"
	"strings"
	"testing"
)

func TestParseHostContainers_NDJSON(t *testing.T) {
	data := []byte(`{"ID":"aaa111222333","Names":"web","Image":"nginx:latest","State":"running","Status":"Up 2 hours (healthy)","Ports":"0.0.0.0:8080->80/tcp","Labels":"foo=bar","CreatedAt":"2026-08-21 13:15:17 +0300 EEST"}
{"ID":"bbb444555666","Names":"db","Image":"postgres:16","State":"exited","Status":"Exited (0) 3 hours ago","Ports":"","Labels":"","CreatedAt":"2026-08-20 09:00:00 +0300 EEST"}`)

	got, err := parseHostContainers(data)
	if err != nil {
		t.Fatalf("parseHostContainers() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "aaa111222333" || got[0].Names != "web" || got[0].Image != "nginx:latest" {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[0].State != "running" || got[0].Status != "Up 2 hours (healthy)" {
		t.Errorf("entry 0 state/status = %q/%q", got[0].State, got[0].Status)
	}
	if got[0].Ports != "0.0.0.0:8080->80/tcp" {
		t.Errorf("entry 0 ports = %q", got[0].Ports)
	}
	if got[1].Names != "db" || got[1].State != "exited" {
		t.Errorf("entry 1 = %+v", got[1])
	}
}

func TestParseHostContainers_ArrayForm(t *testing.T) {
	data := []byte(`[{"ID":"aaa111222333","Names":"web","State":"running"},{"ID":"bbb444555666","Names":"db","State":"exited"}]`)

	got, err := parseHostContainers(data)
	if err != nil {
		t.Fatalf("parseHostContainers() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Names != "web" || got[1].Names != "db" {
		t.Errorf("names = %q, %q", got[0].Names, got[1].Names)
	}
}

func TestParseHostContainers_Empty(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   \n  "},
		{"empty array", "[]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHostContainers([]byte(tt.in))
			if err != nil {
				t.Fatalf("parseHostContainers() error = %v", err)
			}
			if got != nil {
				t.Errorf("got %v, want nil", got)
			}
		})
	}
}

func TestParseHostContainers_BlankLinesSkipped(t *testing.T) {
	data := []byte("\n{\"ID\":\"aaa111222333\",\"Names\":\"web\"}\n\n{\"ID\":\"bbb444555666\",\"Names\":\"db\"}\n")
	got, err := parseHostContainers(data)
	if err != nil {
		t.Fatalf("parseHostContainers() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestParseHostContainers_MalformedLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"ndjson", "{\"ID\":\"aaa111222333\"}\nnot json\n"},
		{"array", `[{"ID":"aaa"},`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHostContainers([]byte(tt.in))
			if err == nil {
				t.Fatalf("expected error, got %v", got)
			}
			if !strings.Contains(err.Error(), "parsing host containers") {
				t.Errorf("error = %v, want it to mention parsing host containers", err)
			}
		})
	}
}

// TestParseHostContainers_RealFixture parses a capture from a live daemon
// (`docker ps -a --format '{{json .}}'`) so the field names stay pinned to the
// real output shape rather than a hand-written approximation.
func TestParseHostContainers_RealFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/docker_ps_host.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	entries, err := parseHostContainers(data)
	if err != nil {
		t.Fatalf("parseHostContainers() error = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("fixture parsed to zero entries")
	}

	var managed, unmanaged int
	for _, e := range entries {
		if e.ID == "" {
			t.Errorf("entry with empty ID: %+v", e)
		}
		if len(e.ID) != 12 {
			t.Errorf("ID %q is not the 12-char short form", e.ID)
		}
		if e.Names == "" {
			t.Errorf("entry %s has empty Names", e.ID)
		}
		if e.State == "" {
			t.Errorf("entry %s has empty State", e.ID)
		}
		if isComposeManaged(e.Labels) {
			managed++
			continue
		}
		unmanaged++
	}
	if managed == 0 {
		t.Error("fixture has no compose-managed containers; it cannot pin the filter")
	}
	if unmanaged == 0 {
		t.Error("fixture has no unmanaged containers; it cannot pin the filter")
	}
}

func TestIsComposeManaged(t *testing.T) {
	tests := []struct {
		name   string
		labels string
		want   bool
	}{
		{"empty", "", false},
		{"managed, only label", "com.docker.compose.project=my-app", true},
		{"managed, first of many", "com.docker.compose.project=my-app,com.docker.compose.service=web", true},
		{"managed, last of many", "org.opencontainers.image.vendor=acme,com.docker.compose.project=my-app", true},
		{"managed, middle of many", "a=1,com.docker.compose.project=my-app,b=2", true},
		{"unmanaged", "org.opencontainers.image.vendor=acme,maintainer=nobody", false},
		// The sibling keys must NOT count: they exist on every compose container
		// but also stand alone on nothing, and the "=" in the prefix excludes them.
		{"config_files sibling only", "com.docker.compose.project.config_files=/srv/app/compose.yml", false},
		{"working_dir sibling only", "com.docker.compose.project.working_dir=/srv/app", false},
		// A label VALUE containing a comma must not be mis-sliced into a fake token.
		{"comma inside a value", `description=one,two,three,maintainer=nobody`, false},
		{"value that mentions the key mid-token", "description=set com.docker.compose.project=x manually", false},
		{"value ending in the key with no comma boundary", "description=xcom.docker.compose.project=x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isComposeManaged(tt.labels); got != tt.want {
				t.Errorf("isComposeManaged(%q) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestParseHealthFromStatus(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Up 2 hours (healthy)", "healthy"},
		{"Up 2 hours (unhealthy)", "unhealthy"},
		{"Up 5 seconds (health: starting)", "starting"},
		{"Up 2 hours", ""},
		{"", ""},
		{"   ", ""},
		{"Created", ""},
		{"Exited (0) 2 hours ago", ""},
		{"Exited (255) 3 months ago", ""},
		{"Restarting (1) 3 seconds ago", ""},
		{"Up 2 hours (Paused)", ""},
		{"Up 2 hours (HEALTHY)", "healthy"},
		{"Up 2 hours (healthy)  ", "healthy"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseHealthFromStatus(tt.in); got != tt.want {
				t.Errorf("parseHealthFromStatus(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHostContainerName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"web", "web"},
		{"web,web-alias", "web"},
		{" web , alias ", "web"},
		{"", ""},
		{",alias", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := hostContainerName(tt.in); got != tt.want {
				t.Errorf("hostContainerName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

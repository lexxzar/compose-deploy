package compose

import (
	"encoding/json"
	"testing"
)

func TestPickInspectContainer(t *testing.T) {
	tests := []struct {
		name    string
		entries []psEntry
		service string
		wantID  string
		wantOK  bool
	}{
		{
			name:    "empty slice",
			entries: nil,
			service: "web",
			wantOK:  false,
		},
		{
			name:    "empty service name",
			entries: []psEntry{{ID: "abc", Service: "web", State: "running", Status: "Up 2 hours"}},
			service: "",
			wantOK:  false,
		},
		{
			name:    "no match",
			entries: []psEntry{{ID: "abc", Service: "web", State: "running", Status: "Up 2 hours"}},
			service: "db",
			wantOK:  false,
		},
		{
			name:    "single running match",
			entries: []psEntry{{ID: "abc", Service: "web", State: "running", Status: "Up 2 hours"}},
			service: "web",
			wantID:  "abc",
			wantOK:  true,
		},
		{
			name: "single stopped match falls back to the entry",
			entries: []psEntry{
				{ID: "dead", Service: "web", State: "exited", Status: "Exited (1) 5 minutes ago"},
			},
			service: "web",
			wantID:  "dead",
			wantOK:  true,
		},
		{
			name: "scaled service picks the longest running replica",
			entries: []psEntry{
				{ID: "short", Service: "web", State: "running", Status: "Up 5 minutes"},
				{ID: "long", Service: "web", State: "running", Status: "Up 3 days"},
				{ID: "mid", Service: "web", State: "running", Status: "Up 2 hours"},
			},
			service: "web",
			wantID:  "long",
			wantOK:  true,
		},
		{
			name: "running beats restarting even when restarting comes first",
			entries: []psEntry{
				{ID: "flapping", Service: "web", State: "restarting", Status: "Restarting (1) 2 seconds ago"},
				{ID: "healthy", Service: "web", State: "running", Status: "Up 10 seconds"},
			},
			service: "web",
			wantID:  "healthy",
			wantOK:  true,
		},
		{
			name: "running beats a longer-looking restarting entry",
			entries: []psEntry{
				{ID: "healthy", Service: "web", State: "running", Status: "Up 10 seconds"},
				{ID: "flapping", Service: "web", State: "restarting", Status: "Restarting (1) 2 seconds ago"},
			},
			service: "web",
			wantID:  "healthy",
			wantOK:  true,
		},
		{
			name: "all restarting falls back to the first restarting replica",
			entries: []psEntry{
				{ID: "flap1", Service: "web", State: "restarting", Status: "Restarting (1) 2 seconds ago"},
				{ID: "flap2", Service: "web", State: "restarting", Status: "Restarting (1) 1 second ago"},
			},
			service: "web",
			wantID:  "flap1",
			wantOK:  true,
		},
		{
			name: "other services are ignored",
			entries: []psEntry{
				{ID: "dbid", Service: "db", State: "running", Status: "Up 9 days"},
				{ID: "webid", Service: "web", State: "running", Status: "Up 1 hour"},
			},
			service: "web",
			wantID:  "webid",
			wantOK:  true,
		},
		{
			name: "entry without an ID is skipped",
			entries: []psEntry{
				{ID: "", Service: "web", State: "running", Status: "Up 3 days"},
				{ID: "real", Service: "web", State: "running", Status: "Up 1 hour"},
			},
			service: "web",
			wantID:  "real",
			wantOK:  true,
		},
		{
			name: "only entries without IDs yields no match",
			entries: []psEntry{
				{ID: "", Service: "web", State: "running", Status: "Up 3 days"},
			},
			service: "web",
			wantOK:  false,
		},
		{
			name: "health suffix does not confuse the uptime gate",
			entries: []psEntry{
				{ID: "sick", Service: "web", State: "running", Status: "Up 2 minutes (unhealthy)"},
				{ID: "fine", Service: "web", State: "running", Status: "Up 4 hours (healthy)"},
			},
			service: "web",
			wantID:  "fine",
			wantOK:  true,
		},
		{
			name: "stopped replica loses to a running one",
			entries: []psEntry{
				{ID: "gone", Service: "web", State: "exited", Status: "Exited (0) 1 hour ago"},
				{ID: "live", Service: "web", State: "running", Status: "Up 30 seconds"},
			},
			service: "web",
			wantID:  "live",
			wantOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotOK := pickInspectContainer(tt.entries, tt.service)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotID != tt.wantID {
				t.Errorf("id = %q, want %q", gotID, tt.wantID)
			}
		})
	}
}

// TestPickInspectContainer_MatchesUptimeColumn pins the picker against the Uptime
// column parseContainerStatus renders: on a running+restarting mix both must land
// on the running replica.
func TestPickInspectContainer_MatchesUptimeColumn(t *testing.T) {
	entries := []psEntry{
		{ID: "flapping", Service: "web", State: "restarting", Status: "Restarting (1) 2 seconds ago"},
		{ID: "healthy", Service: "web", State: "running", Status: "Up 45 minutes"},
	}

	id, ok := pickInspectContainer(entries, "web")
	if !ok || id != "healthy" {
		t.Fatalf("picker = (%q, %v), want (\"healthy\", true)", id, ok)
	}

	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal entries: %v", err)
	}
	status, err := parseContainerStatus(raw)
	if err != nil {
		t.Fatalf("parseContainerStatus: %v", err)
	}
	if got := status["web"].Uptime; got != "45m" {
		t.Fatalf("uptime column = %q, want %q — picker and column disagree", got, "45m")
	}
}

func TestPickHostInspectContainer(t *testing.T) {
	tests := []struct {
		name    string
		entries []hostPsEntry
		target  string
		wantID  string
		wantOK  bool
	}{
		{
			name:    "empty slice",
			entries: nil,
			target:  "watchtower",
			wantOK:  false,
		},
		{
			name:    "empty name",
			entries: []hostPsEntry{{ID: "abc", Names: "watchtower"}},
			target:  "",
			wantOK:  false,
		},
		{
			name:    "match",
			entries: []hostPsEntry{{ID: "abc", Names: "watchtower"}},
			target:  "watchtower",
			wantID:  "abc",
			wantOK:  true,
		},
		{
			name:    "comma joined names takes the first",
			entries: []hostPsEntry{{ID: "abc", Names: "watchtower,wt-alias"}},
			target:  "watchtower",
			wantID:  "abc",
			wantOK:  true,
		},
		{
			name:    "alias in a comma joined name does not match",
			entries: []hostPsEntry{{ID: "abc", Names: "watchtower,wt-alias"}},
			target:  "wt-alias",
			wantOK:  false,
		},
		{
			name: "first match wins",
			entries: []hostPsEntry{
				{ID: "one", Names: "pg"},
				{ID: "two", Names: "pg"},
			},
			target: "pg",
			wantID: "one",
			wantOK: true,
		},
		{
			name:    "no match",
			entries: []hostPsEntry{{ID: "abc", Names: "watchtower"}},
			target:  "redis",
			wantOK:  false,
		},
		{
			name:    "entry without an ID is skipped",
			entries: []hostPsEntry{{ID: "", Names: "redis"}, {ID: "real", Names: "redis"}},
			target:  "redis",
			wantID:  "real",
			wantOK:  true,
		},
		{
			name:    "whitespace around the name is trimmed",
			entries: []hostPsEntry{{ID: "abc", Names: " redis , alias"}},
			target:  "redis",
			wantID:  "abc",
			wantOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotOK := pickHostInspectContainer(tt.entries, tt.target)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotID != tt.wantID {
				t.Errorf("id = %q, want %q", gotID, tt.wantID)
			}
		})
	}
}

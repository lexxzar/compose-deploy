package compose

import (
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0B", 0, false},
		{"512B", 512, false},
		{"124MiB", 130023424, false},
		{"1.5GiB", 1610612736, false},
		{"100kB", 100000, false},
		{"100KB", 100000, false},
		{"1.5GB", 1500000000, false},
		{"2GiB", 2 * 1024 * 1024 * 1024, false},
		{"1TiB", 1024 * 1024 * 1024 * 1024, false},
		// Case-insensitive unit
		{"512mib", 512 * 1024 * 1024, false},
		// Whitespace tolerance
		{"  124MiB  ", 130023424, false},
		// No unit → assume bytes
		{"1024", 1024, false},
		// Errors
		{"abc", 0, true},
		{"MiB", 0, true},
		{"1.5XX", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseSize(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSize(%q) expected error, got nil (value=%d)", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSize(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseCPUPercent(t *testing.T) {
	tests := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"4.20%", 4.2, false},
		{"0.00%", 0, false},
		{"", 0, false},
		{"100%", 100, false},
		{"250.5%", 250.5, false},
		// Whitespace tolerance
		{"  4.20%  ", 4.2, false},
		// No percent suffix is still accepted (some output forms omit it)
		{"4.2", 4.2, false},
		// Errors
		{"abc", 0, true},
		{"4.2.0%", 0, true},
		{"%", 0, false}, // empty after stripping → 0
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseCPUPercent(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseCPUPercent(%q) expected error, got nil (value=%v)", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCPUPercent(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseCPUPercent(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1K"},
		{1536, "1.5K"}, // 1.5 KiB
		{2048, "2K"},
		{1024 * 1024, "1M"},
		{130023424, "124M"}, // 124 MiB exact
		{1610612736, "1.5G"},
		{1024 * 1024 * 1024, "1G"},
		{int64(1024) * 1024 * 1024 * 1024, "1T"},
		// Negative values clamp to 0
		{-1, "0B"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatBytes(tt.in)
			if got != tt.want {
				t.Errorf("FormatBytes(%d) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseStatsOutput(t *testing.T) {
	// Canned NDJSON output, 3 containers, mixed units and CPU percentages.
	in := strings.Join([]string{
		`{"ID":"abc123","Name":"proj-api-1","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}`,
		`{"ID":"def456","Name":"proj-db-1","CPUPerc":"0.50%","MemUsage":"50MB / 1GiB"}`,
		`{"ID":"ghi789","Name":"proj-worker-1","CPUPerc":"100.00%","MemUsage":"1.5GiB / 2GiB"}`,
	}, "\n")

	got, err := parseStatsOutput([]byte(in))
	if err != nil {
		t.Fatalf("parseStatsOutput error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}

	api, ok := got["abc123"]
	if !ok {
		t.Fatalf("missing abc123 in result")
	}
	if api.CPUPercent != 4.2 {
		t.Errorf("abc123 CPU = %v, want 4.2", api.CPUPercent)
	}
	if api.MemoryUsed != 130023424 {
		t.Errorf("abc123 MemoryUsed = %d, want 130023424", api.MemoryUsed)
	}
	if api.MemoryLimit != 536870912 {
		t.Errorf("abc123 MemoryLimit = %d, want 536870912", api.MemoryLimit)
	}

	db, ok := got["def456"]
	if !ok {
		t.Fatalf("missing def456 in result")
	}
	if db.CPUPercent != 0.5 {
		t.Errorf("def456 CPU = %v, want 0.5", db.CPUPercent)
	}
	if db.MemoryUsed != 50_000_000 {
		t.Errorf("def456 MemoryUsed = %d, want 50000000", db.MemoryUsed)
	}
	if db.MemoryLimit != 1024*1024*1024 {
		t.Errorf("def456 MemoryLimit = %d, want %d", db.MemoryLimit, 1024*1024*1024)
	}

	worker, ok := got["ghi789"]
	if !ok {
		t.Fatalf("missing ghi789 in result")
	}
	if worker.CPUPercent != 100 {
		t.Errorf("ghi789 CPU = %v, want 100", worker.CPUPercent)
	}
	if worker.MemoryUsed != 1610612736 {
		t.Errorf("ghi789 MemoryUsed = %d, want 1610612736", worker.MemoryUsed)
	}
}

func TestParseStatsOutput_JSONArray(t *testing.T) {
	// Same data as TestParseStatsOutput but in JSON-array form.
	in := `[
		{"ID":"abc123","Name":"proj-api-1","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"},
		{"ID":"def456","Name":"proj-db-1","CPUPerc":"0.50%","MemUsage":"50MB / 1GiB"},
		{"ID":"ghi789","Name":"proj-worker-1","CPUPerc":"100.00%","MemUsage":"1.5GiB / 2GiB"}
	]`

	got, err := parseStatsOutput([]byte(in))
	if err != nil {
		t.Fatalf("parseStatsOutput (array) error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got["abc123"].CPUPercent != 4.2 {
		t.Errorf("abc123 CPU = %v, want 4.2", got["abc123"].CPUPercent)
	}
	if got["def456"].MemoryUsed != 50_000_000 {
		t.Errorf("def456 MemoryUsed = %d, want 50000000", got["def456"].MemoryUsed)
	}
	if got["ghi789"].MemoryLimit != 2*1024*1024*1024 {
		t.Errorf("ghi789 MemoryLimit = %d, want %d", got["ghi789"].MemoryLimit, 2*1024*1024*1024)
	}
}

func TestParseStatsOutput_Empty(t *testing.T) {
	cases := []string{"", "   ", "[]", "\n\n"}
	for _, in := range cases {
		got, err := parseStatsOutput([]byte(in))
		if err != nil {
			t.Errorf("parseStatsOutput(%q) error: %v", in, err)
			continue
		}
		if len(got) != 0 {
			t.Errorf("parseStatsOutput(%q) = %v, want empty map", in, got)
		}
	}
}

func TestParseStatsOutput_Malformed(t *testing.T) {
	// Second line is malformed JSON.
	in := strings.Join([]string{
		`{"ID":"abc123","Name":"proj-api-1","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}`,
		`{not valid json`,
	}, "\n")
	_, err := parseStatsOutput([]byte(in))
	if err == nil {
		t.Fatal("expected error for malformed NDJSON, got nil")
	}
}

func TestParseStatsOutput_MalformedArray(t *testing.T) {
	in := `[{"ID":"abc"}, not valid]`
	_, err := parseStatsOutput([]byte(in))
	if err == nil {
		t.Fatal("expected error for malformed JSON array, got nil")
	}
}

func TestParseStatsOutput_SkipsEmptyID(t *testing.T) {
	in := strings.Join([]string{
		`{"ID":"","Name":"weird","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}`,
		`{"ID":"abc123","Name":"proj-api-1","CPUPerc":"4.20%","MemUsage":"124MiB / 512MiB"}`,
	}, "\n")
	got, err := parseStatsOutput([]byte(in))
	if err != nil {
		t.Fatalf("parseStatsOutput error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 entry (empty-ID dropped), got %d", len(got))
	}
	if _, ok := got["abc123"]; !ok {
		t.Errorf("missing abc123 in result")
	}
}

func TestParseStatsOutput_PropagatesCPUError(t *testing.T) {
	in := `{"ID":"abc","Name":"x","CPUPerc":"not-a-number","MemUsage":"124MiB / 512MiB"}`
	_, err := parseStatsOutput([]byte(in))
	if err == nil {
		t.Fatal("expected error from malformed CPU percent, got nil")
	}
}

func TestParseStatsOutput_PropagatesMemError(t *testing.T) {
	in := `{"ID":"abc","Name":"x","CPUPerc":"4.2%","MemUsage":"oops"}`
	_, err := parseStatsOutput([]byte(in))
	if err == nil {
		t.Fatal("expected error from malformed memory usage, got nil")
	}
}

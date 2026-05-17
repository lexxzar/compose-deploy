package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// readFixture loads a captured `docker compose` output sample from testdata/.
// Failing here is a setup error (fixture file missing), so use t.Fatalf.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return string(b)
}

func TestParseDryRunOutput_Fixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    map[string]bool
	}{
		{
			name:    "mixed",
			fixture: "dryrun_mixed.txt",
			// fakebuild is build-only (absent); alpine current (false);
			// needupdate would be pulled (true). The trailing "Pulled" line
			// for needupdate is a no-op.
			want: map[string]bool{
				"alpine":     false,
				"needupdate": true,
			},
		},
		{
			name:    "all_current",
			fixture: "dryrun_all_current.txt",
			want: map[string]bool{
				"needupdate": false,
				"alpine":     false,
			},
		},
		{
			name:    "all_update",
			fixture: "dryrun_all_update.txt",
			// Two services would be pulled; the trailing "Pulled" lines are
			// completion markers and don't change the verdict.
			want: map[string]bool{
				"needupdate": true,
				"alpine":     true,
			},
		},
		{
			name:    "build_only",
			fixture: "dryrun_build_only.txt",
			// Build-only services produce an empty map (absent = unknown).
			want: map[string]bool{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDryRunOutput(readFixture(t, tt.fixture))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseDryRunOutput(%s) = %#v, want %#v", tt.fixture, got, tt.want)
			}
		})
	}
}

func TestParseDryRunOutput_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]bool
	}{
		{
			name: "empty",
			in:   "",
			want: map[string]bool{},
		},
		{
			name: "only_whitespace",
			in:   "   \n\t\n",
			want: map[string]bool{},
		},
		{
			name: "missing_prefix",
			in:   "nginx Pulling\n",
			want: map[string]bool{},
		},
		{
			name: "prefix_no_payload",
			in:   "DRY-RUN MODE -   \n",
			want: map[string]bool{},
		},
		{
			name: "service_no_verdict",
			in:   "DRY-RUN MODE -  web\n",
			want: map[string]bool{},
		},
		{
			name: "explicit_pull_required",
			// "Pull required" is reserved per the plan even though current
			// Compose doesn't emit it; the parser must still recognise it.
			in:   "DRY-RUN MODE -  api Pull required\n",
			want: map[string]bool{"api": true},
		},
		{
			name: "noise_lines_between",
			in:   "warning: deprecated key\n DRY-RUN MODE -  web Pulling \nrandom message\n DRY-RUN MODE -  db Skipped - Image is already present locally \n",
			want: map[string]bool{"web": true, "db": false},
		},
		{
			name: "multiple_replicas_same_service_last_wins",
			// Compose dedupes per service in dry-run, so duplicates shouldn't
			// occur, but if they did the last verdict wins (map semantics).
			in:   "DRY-RUN MODE -  web Pulling \nDRY-RUN MODE -  web Skipped - Image is already present locally\n",
			want: map[string]bool{"web": false},
		},
		{
			name: "alternative_already_present_phrasing",
			// Defensive: any "already present" substring counts as current.
			in:   "DRY-RUN MODE -  cache Image already present\n",
			want: map[string]bool{"cache": false},
		},
		{
			name: "unrecognised_verdict_skipped",
			// "Error: foo" style lines are not classified — service absent.
			in:   "DRY-RUN MODE -  web Error: connection refused\n",
			want: map[string]bool{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDryRunOutput(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseDryRunOutput(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDetectDryRunFromHelp_Fixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    bool
	}{
		{"with_dryrun", "pull_help_with_dryrun.txt", true},
		{"without_dryrun", "pull_help_without_dryrun.txt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectDryRunFromHelp(readFixture(t, tt.fixture))
			if got != tt.want {
				t.Fatalf("detectDryRunFromHelp(%s) = %v, want %v", tt.fixture, got, tt.want)
			}
		})
	}
}

func TestDetectDryRunFromHelp_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"plain_mention", "Some option --dry-run does X", true},
		{"different_flag_only", "Some option --dryrun (no dash) does X", false},
		{"case_sensitive", "Use --DRY-RUN flag", false},
		{"flag_in_description_text", "Pre-flight check (no --dry-run available)", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectDryRunFromHelp(tt.in)
			if got != tt.want {
				t.Fatalf("detectDryRunFromHelp(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

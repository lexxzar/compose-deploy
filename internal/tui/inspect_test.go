package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/lexxzar/compose-deploy/internal/compose"
)

// loadInspectFixture reads one of the real `docker inspect` captures. The
// fixtures live in internal/compose/testdata because every docker-output parser
// in this repo does; the renderer is the only half that lives here, so it reads
// across rather than keeping a second copy that could drift from the parser's.
func loadInspectFixture(t *testing.T, name string) compose.InspectDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "compose", "testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	doc, err := compose.ParseInspect(raw)
	if err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
	return doc
}

// TestBuildInspectSummary_UnhealthyProbeOutput is the wedge: the last failing
// probe's stdout is the one thing `docker compose config` cannot answer, so it
// must reach the summary verbatim.
func TestBuildInspectSummary_UnhealthyProbeOutput(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_unhealthy.json")
	out := buildInspectSummary(doc, 120)

	const probe = "curl: (7) Failed to connect to localhost port 9999 after 0 ms: Could not connect to server"
	if !strings.Contains(out, probe) {
		t.Errorf("probe output missing from summary:\n%s", out)
	}

	for _, want := range []string{
		"HEALTH",
		"status", "unhealthy",
		"failing streak", "5",
		"CMD-SHELL curl -fsS http://localhost:9999/healthz",
		"interval", "3s",
		"timeout", "2s",
		"retries", "2",
		"last probe", "exit 7 at 2026-08-22 03:09:38",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestBuildInspectSummary_HealthyFixture(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_healthy.json")
	out := buildInspectSummary(doc, 120)

	for _, want := range []string{
		"STATE",
		"running",
		"started         2026-08-22 03:09:23",
		"restart policy  no",
		"restarts        0",
		"HEALTH",
		"healthy",
		"failing streak  0",
		"interval        5s",
		"timeout         3s",
		"retries         3",
		"last probe      exit 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}

	// A running container's exit code is always 0 and says nothing.
	if strings.Contains(out, "exit code") {
		t.Errorf("running container should not render an exit code row:\n%s", out)
	}
	// The healthy probes have empty Output, so no block follows the header.
	if n := strings.Count(out, "last probe"); n != 1 {
		t.Errorf("last probe rows = %d, want 1:\n%s", n, out)
	}
}

func TestBuildInspectSummary_StoppedFixture(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_stopped.json")
	out := buildInspectSummary(doc, 120)

	for _, want := range []string{
		"STATE",
		"status          exited",
		"exit code       3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "HEALTH") {
		t.Errorf("container without a healthcheck must not render HEALTH:\n%s", out)
	}
	if strings.Contains(out, "oom killed") {
		t.Errorf("container that was not OOM-killed must not render the row:\n%s", out)
	}
}

// TestBuildInspectSummary_ProbeOutputWrapsNotTruncates pins the soft-wrap: at a
// width the probe line cannot fit, every rune must still be present.
func TestBuildInspectSummary_ProbeOutputWrapsNotTruncates(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_unhealthy.json")
	const probe = "curl: (7) Failed to connect to localhost port 9999 after 0 ms: Could not connect to server"

	for _, width := range []int{30, 40, 60} {
		out := buildInspectSummary(doc, width)
		if strings.Contains(out, probe) {
			t.Fatalf("width %d: probe unexpectedly fits on one line", width)
		}
		// A wrap can land inside a run of spaces, and a line's trailing padding
		// is dropped, so the comparison ignores spaces: what it pins is that no
		// rune of the probe output was cut at the terminal edge.
		if !strings.Contains(squeeze(out), squeeze(probe)) {
			t.Errorf("width %d: probe output truncated, got:\n%s", width, out)
		}
	}
}

// squeeze drops every space so a comparison survives a wrap that lands inside a
// run of spaces.
func squeeze(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "\n", "")
}

// TestBuildInspectSummary_NeverExceedsWidth pins the wrap-not-truncate promise
// in DISPLAY CELLS, which is the unit the pane is measured in. An ASCII-only
// sweep cannot fail it: runes and cells agree there, so a rune-counting wrap
// looks correct right up until a probe output, an env value or a mount path
// carries CJK or an emoji and renders at 2x the pane width. The "wide runes"
// doc is what makes the assertion load-bearing.
func TestBuildInspectSummary_NeverExceedsWidth(t *testing.T) {
	// minWidth is the narrowest pane a doc can be measured at. A single wide
	// grapheme occupies 2 cells and cannot be split, so a doc carrying one
	// cannot hold a 1-cell pane; ASCII docs are swept down to width 1, where
	// the narrow-pane guards in kv and block actually fire.
	type widthCase struct {
		doc      compose.InspectDoc
		minWidth int
	}
	docs := map[string]widthCase{
		"healthy":   {doc: loadInspectFixture(t, "docker_inspect_healthy.json")},
		"unhealthy": {doc: loadInspectFixture(t, "docker_inspect_unhealthy.json")},
		"stopped":   {doc: loadInspectFixture(t, "docker_inspect_stopped.json")},
		"long values": {doc: compose.InspectDoc{
			Name:         "verbose",
			RestartCount: 3,
			State: compose.InspectState{
				Status:    "running",
				Running:   true,
				StartedAt: "2026-08-22T03:09:23.118711129Z",
				Health: &compose.InspectHealth{
					Status:        "unhealthy",
					FailingStreak: 12,
					Log: []compose.InspectHealthLog{{
						ExitCode: 1,
						End:      "2026-08-22T03:09:38.509504387Z",
						Output:   strings.Repeat("stack frame at /very/long/path/to/a/module/file.go:1234 ", 12),
					}},
				},
			},
			Config: compose.InspectConfig{
				Image:      "registry.example.com/team/" + strings.Repeat("very-long-image-name-", 4) + "service:2026.08.22-rc1",
				Cmd:        []string{"/app/server", "--config", strings.Repeat("/deep/path/segment", 6) + "/config.yaml"},
				Entrypoint: []string{"/usr/local/bin/" + strings.Repeat("entrypoint-", 6) + "wrapper.sh"},
				Env: []string{
					"DATABASE_URL=postgres://appuser:hunter2@" + strings.Repeat("sub.", 12) + "example.com:5432/appdb",
					"SHORT=1",
				},
				Healthcheck: &compose.InspectHealthcheck{
					Test:        []string{"CMD-SHELL", strings.Repeat("curl -fsS http://localhost/healthz && ", 8) + "true"},
					Interval:    5 * time.Second,
					Timeout:     3 * time.Second,
					StartPeriod: 2 * time.Second,
					Retries:     3,
				},
			},
			Mounts: []compose.InspectMount{{
				Type:        "bind",
				Source:      strings.Repeat("/a-rather-long-directory-name", 5),
				Destination: strings.Repeat("/another-long-directory-name", 4),
				RW:          true,
			}},
		}},
		"wide runes": {minWidth: 2, doc: compose.InspectDoc{
			Name:         "\u30a6\u30a7\u30d6\u30b5\u30fc\u30d0\u30fc",
			RestartCount: 1,
			State: compose.InspectState{
				Status: "running",
				Error:  "\u8d77\u52d5\u306b\u5931\u6557\u3057\u307e\u3057\u305f\uff1a\u5b9f\u884c\u30d5\u30a1\u30a4\u30eb\u304c\u898b\u3064\u304b\u308a\u307e\u305b\u3093",
				Health: &compose.InspectHealth{
					Status:        "unhealthy",
					FailingStreak: 3,
					Log: []compose.InspectHealthLog{{
						ExitCode: 1,
						Output:   strings.Repeat("\u30d8\u30eb\u30b9\u30c1\u30a7\u30c3\u30af\u306b\u5931\u6557\u3057\u307e\u3057\u305f \U0001f525 ", 6),
					}},
				},
			},
			Config: compose.InspectConfig{
				Image: "registry.example.com/\u30c1\u30fc\u30e0/\u30a6\u30a7\u30d6\u30b5\u30fc\u30d0\u30fc:1.0",
				Cmd:   []string{"/app/server", "--\u8a2d\u5b9a", "/etc/\u8a2d\u5b9a/\u30d5\u30a1\u30a4\u30eb.yaml"},
				Env: []string{
					"MESSAGE=\u3053\u3093\u306b\u3061\u306f\u4e16\u754c\u3001\u3053\u308c\u306f\u3068\u3066\u3082\u9577\u3044\u5024\u3067\u3059 \U0001f680\U0001f680\U0001f680",
					"SHORT=1",
				},
				Healthcheck: &compose.InspectHealthcheck{
					Test:     []string{"CMD-SHELL", "curl -fsS http://localhost/\u30d8\u30eb\u30b9 || exit 1"},
					Interval: 5 * time.Second,
				},
			},
			Mounts: []compose.InspectMount{{
				Type:        "bind",
				Source:      "/srv/\u30c7\u30fc\u30bf/\u30a6\u30a7\u30d6\u30b5\u30a4\u30c8",
				Destination: "/usr/share/nginx/\u516c\u958b",
				RW:          false,
			}},
		}},
	}

	for name, tc := range docs {
		for _, width := range []int{1, 4, 10, 20, 30, 40, 60, 80, 120} {
			if width < tc.minWidth {
				continue
			}
			out := buildInspectSummary(tc.doc, width)
			for i, line := range strings.Split(out, "\n") {
				if w := ansi.StringWidth(line); w > width {
					t.Errorf("%s at width %d: line %d is %d cells: %q", name, width, i, w, line)
				}
			}
		}
	}
}

func TestBuildInspectSummary_StateAlwaysRenders(t *testing.T) {
	out := buildInspectSummary(compose.InspectDoc{}, 80)
	for _, want := range []string{"STATE", "status          unknown", "exit code       0", "restart policy  no"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty doc summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "HEALTH") {
		t.Errorf("empty doc must not render HEALTH:\n%s", out)
	}
	if strings.Contains(out, "started") {
		t.Errorf("empty doc has no start time and must not render the row:\n%s", out)
	}
	for _, section := range []string{"IMAGE", "MOUNTS", "ENV"} {
		if strings.Contains(out, section) {
			t.Errorf("empty doc must not render %s:\n%s", section, out)
		}
	}
}

// TestBuildInspectSummary_StateNamesTheContainerAndItsError pins the two rows
// that answer "which replica am I looking at" and "why did it not start". The
// breadcrumb names the SERVICE, so on a scaled service the container row is the
// only place the picked replica is named; State.Error is docker's own reason,
// and on a container that never ran the exit code alone says nothing.
func TestBuildInspectSummary_StateNamesTheContainerAndItsError(t *testing.T) {
	doc := compose.InspectDoc{
		Name: "shop-worker-2",
		State: compose.InspectState{
			Status:   "created",
			ExitCode: 127,
			Error:    `exec: "worker": executable file not found in $PATH`,
		},
	}
	out := buildInspectSummary(doc, 100)
	for _, want := range []string{
		"container       shop-worker-2",
		"status          created",
		"exit code       127",
		`error           exec: "worker": executable file not found in $PATH`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

// TestBuildInspectSummary_StateOmitsEmptyRows is the other direction: a blank
// name or a blank error must not render a labelled row with nothing after it.
func TestBuildInspectSummary_StateOmitsEmptyRows(t *testing.T) {
	doc := compose.InspectDoc{State: compose.InspectState{Status: "running", Running: true, Error: "   "}}
	out := buildInspectSummary(doc, 80)
	for _, skip := range []string{"container", "error"} {
		if strings.Contains(out, skip) {
			t.Errorf("summary must not render an empty %q row:\n%s", skip, out)
		}
	}
}

func TestBuildInspectSummary_OOMKilledAndRestartPolicy(t *testing.T) {
	doc := compose.InspectDoc{
		RestartCount: 7,
		State: compose.InspectState{
			Status:    "exited",
			ExitCode:  137,
			OOMKilled: true,
			StartedAt: "2026-08-22T03:09:23Z",
		},
		HostConfig: compose.InspectHostConfig{
			RestartPolicy: compose.InspectRestartPolicy{Name: "on-failure", MaximumRetryCount: 5},
		},
	}
	out := buildInspectSummary(doc, 80)
	for _, want := range []string{
		"status          exited",
		"exit code       137",
		"oom killed      yes",
		"started         2026-08-22 03:09:23",
		"restart policy  on-failure (max 5)",
		"restarts        7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestBuildInspectSummary_HealthSectionPresence(t *testing.T) {
	tests := []struct {
		name        string
		hc          *compose.InspectHealthcheck
		state       *compose.InspectHealth
		wantSection bool
		wantRows    []string
		skipRows    []string
	}{
		{
			name:        "no healthcheck at all",
			wantSection: false,
		},
		{
			name:        "explicitly disabled",
			hc:          &compose.InspectHealthcheck{Test: []string{"NONE"}},
			wantSection: false,
		},
		{
			name:        "declared but never run",
			hc:          &compose.InspectHealthcheck{Test: []string{"CMD", "true"}, Interval: 4 * time.Second},
			wantSection: true,
			wantRows:    []string{"test            CMD true", "interval        4s"},
			skipRows:    []string{"failing streak", "last probe"},
		},
		{
			name:        "runtime state but no config block",
			state:       &compose.InspectHealth{Status: "starting"},
			wantSection: true,
			wantRows:    []string{"status          starting", "failing streak  0"},
			skipRows:    []string{"interval", "last probe"},
		},
		{
			// hasHealthcheck answers "is there a healthcheck", which a
			// zero-valued config satisfies while having nothing to say. A bare
			// header with no rows under it contradicts the documented "a
			// section with nothing to say is omitted".
			name:        "zero-valued healthcheck has nothing to say",
			hc:          &compose.InspectHealthcheck{},
			wantSection: false,
		},
		{
			name:        "runtime state with no status reads as unknown",
			state:       &compose.InspectHealth{},
			wantSection: true,
			wantRows:    []string{"status          unknown", "failing streak  0"},
		},
		{
			name:        "start period is rendered beside the other intervals",
			hc:          &compose.InspectHealthcheck{Test: []string{"CMD", "true"}, StartPeriod: 30 * time.Second},
			wantSection: true,
			wantRows:    []string{"start period    30s"},
		},
		{
			name:        "probe with no output renders only the header",
			hc:          &compose.InspectHealthcheck{Test: []string{"CMD", "true"}},
			state:       &compose.InspectHealth{Status: "healthy", Log: []compose.InspectHealthLog{{ExitCode: 0}}},
			wantSection: true,
			wantRows:    []string{"last probe      exit 0"},
		},
		{
			name: "last probe wins over earlier ones",
			hc:   &compose.InspectHealthcheck{Test: []string{"CMD", "true"}},
			state: &compose.InspectHealth{Status: "unhealthy", FailingStreak: 2, Log: []compose.InspectHealthLog{
				{ExitCode: 0, Output: "first probe output"},
				{ExitCode: 9, Output: "newest probe output"},
			}},
			wantSection: true,
			wantRows:    []string{"last probe      exit 9", "newest probe output"},
			skipRows:    []string{"first probe output"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := compose.InspectDoc{
				State:  compose.InspectState{Status: "running", Running: true, Health: tt.state},
				Config: compose.InspectConfig{Healthcheck: tt.hc},
			}
			out := buildInspectSummary(doc, 80)
			if got := strings.Contains(out, "HEALTH"); got != tt.wantSection {
				t.Fatalf("HEALTH present = %v, want %v:\n%s", got, tt.wantSection, out)
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

// TestBuildInspectSummary_MultiLineProbeOutput pins that a probe's own newlines
// survive — a stack trace must not collapse onto one line.
func TestBuildInspectSummary_MultiLineProbeOutput(t *testing.T) {
	doc := compose.InspectDoc{
		State: compose.InspectState{Status: "running", Running: true, Health: &compose.InspectHealth{
			Status: "unhealthy",
			Log:    []compose.InspectHealthLog{{ExitCode: 1, Output: "line one\nline two\nline three\n"}},
		}},
	}
	out := buildInspectSummary(doc, 80)
	for _, want := range []string{"    line one", "    line two", "    line three"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.HasSuffix(out, "\n") {
		t.Errorf("summary must not end in a newline: %q", out[max(len(out)-20, 0):])
	}
}

func TestBuildInspectSummary_ZeroWidthFallsBackToDefault(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_unhealthy.json")
	out := buildInspectSummary(doc, 0)
	for i, line := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(line); w > inspectDefaultWidth {
			t.Errorf("line %d is %d cells with an unknown width: %q", i, w, line)
		}
	}
	if !strings.Contains(out, "STATE") {
		t.Errorf("summary lost its STATE section at width 0:\n%s", out)
	}
}

func TestFormatInspectTime(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "whitespace", in: "   ", want: ""},
		{name: "docker zero time", in: "0001-01-01T00:00:00Z", want: ""},
		{name: "rfc3339 nano", in: "2026-08-22T03:09:23.118711129Z", want: "2026-08-22 03:09:23"},
		{name: "rfc3339 seconds", in: "2026-08-22T03:09:23Z", want: "2026-08-22 03:09:23"},
		{name: "keeps the offset zone", in: "2026-08-22T03:09:23+02:00", want: "2026-08-22 03:09:23"},
		{name: "unparseable passes through", in: "not a time", want: "not a time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatInspectTime(tt.in); got != tt.want {
				t.Errorf("formatInspectTime(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatRestartPolicy(t *testing.T) {
	tests := []struct {
		name string
		in   compose.InspectRestartPolicy
		want string
	}{
		{name: "empty means no", want: "no"},
		{name: "no", in: compose.InspectRestartPolicy{Name: "no"}, want: "no"},
		{name: "always", in: compose.InspectRestartPolicy{Name: "always"}, want: "always"},
		{
			name: "on-failure with a cap",
			in:   compose.InspectRestartPolicy{Name: "on-failure", MaximumRetryCount: 5},
			want: "on-failure (max 5)",
		},
		{
			name: "on-failure without a cap",
			in:   compose.InspectRestartPolicy{Name: "on-failure"},
			want: "on-failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatRestartPolicy(tt.in); got != tt.want {
				t.Errorf("formatRestartPolicy(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// inspectRow builds the "label<pad>value" text a kv row renders to, so a test
// pins the alignment without hard-coding a run of spaces it has to recount every
// time a label changes length.
func inspectRow(label, value string) string {
	pad := max(inspectValueCol-len("  "+label), 1)
	return label + strings.Repeat(" ", pad) + value
}

func TestBuildInspectSummary_ImageSection(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_healthy.json")
	out := buildInspectSummary(doc, 200)

	for _, want := range []string{
		"IMAGE",
		inspectRow("image", "nginx:latest"),
		inspectRow("digest", "sha256:d090ef0c3fa38df49d89dfcca52ce77f71d88a8db6bd8388d78817cad20a0c1f"),
		inspectRow("command", "nginx -g daemon off;"),
		inspectRow("entrypoint", "/docker-entrypoint.sh"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

// TestBuildInspectSummary_ImageSectionShellCommand pins that a shell-form command
// reaches the summary as one row, quotes and redirections intact — that string is
// the reason the stopped fixture exited 3.
func TestBuildInspectSummary_ImageSectionShellCommand(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_stopped.json")
	out := buildInspectSummary(doc, 200)

	const cmd = `echo 'migration failed: relation "users" does not exist' >&2; exit 3`
	if !strings.Contains(out, inspectRow("command", cmd)) {
		t.Errorf("summary missing the shell command %q:\n%s", cmd, out)
	}
	if !strings.Contains(out, inspectRow("entrypoint", "/bin/sh -c")) {
		t.Errorf("summary missing the entrypoint:\n%s", out)
	}
}

func TestBuildInspectSummary_ImageSectionPresence(t *testing.T) {
	tests := []struct {
		name    string
		doc     compose.InspectDoc
		want    bool
		wantRow string
		skipRow string
	}{
		{name: "nothing to say", want: false},
		{
			name:    "digest only",
			doc:     compose.InspectDoc{Image: "sha256:abc"},
			want:    true,
			wantRow: inspectRow("digest", "sha256:abc"),
			skipRow: "image  ",
		},
		{
			name:    "configured ref only",
			doc:     compose.InspectDoc{Config: compose.InspectConfig{Image: "redis:7"}},
			want:    true,
			wantRow: inspectRow("image", "redis:7"),
			skipRow: "digest",
		},
		{
			name:    "command only",
			doc:     compose.InspectDoc{Config: compose.InspectConfig{Cmd: []string{"sleep", "infinity"}}},
			want:    true,
			wantRow: inspectRow("command", "sleep infinity"),
			skipRow: "entrypoint",
		},
		{
			name:    "empty command slice is not a command",
			doc:     compose.InspectDoc{Image: "sha256:abc", Config: compose.InspectConfig{Cmd: []string{}}},
			want:    true,
			wantRow: inspectRow("digest", "sha256:abc"),
			skipRow: "command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := buildInspectSummary(tt.doc, 80)
			if got := strings.Contains(out, "IMAGE"); got != tt.want {
				t.Fatalf("IMAGE present = %v, want %v:\n%s", got, tt.want, out)
			}
			if tt.wantRow != "" && !strings.Contains(out, tt.wantRow) {
				t.Errorf("summary missing %q:\n%s", tt.wantRow, out)
			}
			if tt.skipRow != "" && strings.Contains(out, tt.skipRow) {
				t.Errorf("summary must not contain %q:\n%s", tt.skipRow, out)
			}
		})
	}
}

func TestBuildInspectSummary_MountsSection(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_healthy.json")
	if len(doc.Mounts) != 2 {
		t.Fatalf("fixture mounts = %d, want 2", len(doc.Mounts))
	}
	out := buildInspectSummary(doc, 200)

	if !strings.Contains(out, "MOUNTS") {
		t.Fatalf("summary missing MOUNTS:\n%s", out)
	}
	for _, m := range doc.Mounts {
		body := formatInspectMount(m)
		// The needle comes from the function under test, so an empty return
		// would make Contains trivially true.
		if body == "" {
			t.Fatalf("formatInspectMount(%+v) = \"\"; the assertion below would be vacuous", m)
		}
		want := inspectRow(m.Type, body)
		if !strings.Contains(out, want) {
			t.Errorf("summary missing mount row %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "/usr/share/nginx/html  ro") {
		t.Errorf("read-only bind mount lost its access flag:\n%s", out)
	}
	if !strings.Contains(out, "/var/cache/nginx  rw") {
		t.Errorf("read-write volume lost its access flag:\n%s", out)
	}
}

// TestBuildInspectSummary_MountsWrapNotTruncate pins that a long bind source
// survives a narrow pane — the destination is the half that falls off the edge
// without a wrap, and it is the half that identifies the mount.
func TestBuildInspectSummary_MountsWrapNotTruncate(t *testing.T) {
	// LITERAL expectations, not formatInspectMount's own output: a needle
	// produced by the function under test would make this test pass vacuously
	// the moment that function returned "" (strings.Contains(x, "") is true).
	want := []string{
		"/srv/cdeployfixture/site → /usr/share/nginx/html  ro",
		"/var/lib/docker/volumes/cdeployfixture_webdata/_data → /var/cache/nginx  rw",
	}
	doc := loadInspectFixture(t, "docker_inspect_healthy.json")
	if len(doc.Mounts) != len(want) {
		t.Fatalf("fixture mounts = %d, want %d", len(doc.Mounts), len(want))
	}
	for _, width := range []int{40, 60, 80} {
		out := squeeze(buildInspectSummary(doc, width))
		for _, w := range want {
			if !strings.Contains(out, squeeze(w)) {
				t.Errorf("width %d: mount %q truncated:\n%s", width, w, buildInspectSummary(doc, width))
			}
		}
	}
}

// TestBuildInspectSummary_MountLabelEdges pins the two mount-row labels that do
// not come from docker's short type enum. A mount Type is free-form JSON, so it
// is the one kv label that can outgrow the value column: the fallback keeps a
// typeless row identifiable, and an over-long one must still leave a space
// between the label and the value rather than running them together.
func TestBuildInspectSummary_MountLabelEdges(t *testing.T) {
	tests := []struct {
		name string
		in   compose.InspectMount
		want string
	}{
		{
			name: "no type falls back to mount",
			in:   compose.InspectMount{Source: "/srv/data", Destination: "/data", RW: true},
			want: "mount           /srv/data → /data  rw",
		},
		{
			name: "a type wider than the value column still separates",
			in:   compose.InspectMount{Type: "some-very-long-mount-type", Source: "/srv/data", Destination: "/data"},
			want: "some-very-long-mount-type /srv/data → /data  ro",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := buildInspectSummary(compose.InspectDoc{Mounts: []compose.InspectMount{tt.in}}, 80)
			if !strings.Contains(out, tt.want) {
				t.Errorf("summary missing %q:\n%s", tt.want, out)
			}
		})
	}
}

func TestBuildInspectSummary_MountsAbsent(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_unhealthy.json")
	if len(doc.Mounts) != 0 {
		t.Fatalf("fixture mounts = %d, want 0", len(doc.Mounts))
	}
	if out := buildInspectSummary(doc, 120); strings.Contains(out, "MOUNTS") {
		t.Errorf("container with no mounts must not render MOUNTS:\n%s", out)
	}
}

func TestFormatInspectMount(t *testing.T) {
	tests := []struct {
		name string
		in   compose.InspectMount
		want string
	}{
		{
			name: "read-only bind",
			in:   compose.InspectMount{Type: "bind", Source: "/srv/site", Destination: "/usr/share/nginx/html"},
			want: "/srv/site → /usr/share/nginx/html  ro",
		},
		{
			name: "read-write volume",
			in:   compose.InspectMount{Type: "volume", Name: "appdata", Source: "/var/lib/docker/volumes/appdata/_data", Destination: "/data", RW: true},
			want: "/var/lib/docker/volumes/appdata/_data → /data  rw",
		},
		{
			name: "anonymous volume falls back to its name",
			in:   compose.InspectMount{Type: "volume", Name: "a1b2c3", Destination: "/cache", RW: true},
			want: "a1b2c3 → /cache  rw",
		},
		{
			name: "no source and no name",
			in:   compose.InspectMount{Type: "tmpfs", Destination: "/tmp", RW: true},
			want: "(unnamed) → /tmp  rw",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatInspectMount(tt.in); got != tt.want {
				t.Errorf("formatInspectMount(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestBuildInspectSummary_EnvVerbatim pins the no-masking decision: the values
// the running container holds are rendered exactly as docker reports them,
// secrets included. Masking here without masking raw mode would protect nothing.
func TestBuildInspectSummary_EnvVerbatim(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_healthy.json")
	out := buildInspectSummary(doc, 200)

	if !strings.Contains(out, "ENV") {
		t.Fatalf("summary missing ENV:\n%s", out)
	}
	for _, want := range []string{
		"  POSTGRES_PASSWORD=s3cr3t-pw",
		"  DATABASE_URL=postgres://appuser:hunter2@db:5432/appdb",
		"  APP_ENV=production",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing env entry %q:\n%s", want, out)
		}
	}
	if got, want := strings.Count(out, "PATH=/usr/local/sbin"), 1; got != want {
		t.Errorf("PATH entries = %d, want %d:\n%s", got, want, out)
	}
}

func TestBuildInspectSummary_EnvPresence(t *testing.T) {
	tests := []struct {
		name  string
		env   []string
		want  bool
		lines []string
	}{
		{name: "nil", want: false},
		{name: "empty slice", env: []string{}, want: false},
		{name: "one entry", env: []string{"A=1"}, want: true, lines: []string{"  A=1"}},
		{
			name:  "blank entries are dropped",
			env:   []string{"A=1", "", "   ", "B=2"},
			want:  true,
			lines: []string{"  A=1", "  B=2"},
		},
		{
			name:  "a value with no equals is still shown",
			env:   []string{"BARE"},
			want:  true,
			lines: []string{"  BARE"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := compose.InspectDoc{Config: compose.InspectConfig{Env: tt.env}}
			out := buildInspectSummary(doc, 80)
			if got := strings.Contains(out, "ENV"); got != tt.want {
				t.Fatalf("ENV present = %v, want %v:\n%s", got, tt.want, out)
			}
			for _, line := range tt.lines {
				if !strings.Contains(out, line) {
					t.Errorf("summary missing %q:\n%s", line, out)
				}
			}
			if got := envBlockLines(out); tt.want && got != len(tt.lines) {
				t.Errorf("ENV block has %d lines, want %d:\n%s", got, len(tt.lines), out)
			}
		})
	}
}

// TestBuildInspectSummary_SectionOrder pins the five sections into the documented
// reading order: what the container is doing, then why it is unhealthy, then what
// it runs, then what it has attached.
func TestBuildInspectSummary_SectionOrder(t *testing.T) {
	doc := loadInspectFixture(t, "docker_inspect_healthy.json")
	out := buildInspectSummary(doc, 200)

	prev := -1
	for _, section := range []string{"STATE", "HEALTH", "IMAGE", "MOUNTS", "ENV"} {
		at := strings.Index(out, section)
		if at < 0 {
			t.Fatalf("summary missing %s:\n%s", section, out)
		}
		if at <= prev {
			t.Errorf("%s at %d is not after the previous section at %d:\n%s", section, at, prev, out)
		}
		prev = at
	}
}

// envBlockLines counts the entries rendered under the ENV header. It matches
// the section header EXACTLY (after stripping the header's styling) and stops
// at the blank line that separates sections, rather than assuming ENV is last
// and that no other line contains the substring "ENV" — either assumption
// would turn the exact-count assertions above into silent tautologies.
func envBlockLines(summary string) int {
	lines := strings.Split(summary, "\n")
	for i, line := range lines {
		if strings.TrimSpace(ansi.Strip(line)) != "ENV" {
			continue
		}
		n := 0
		for _, entry := range lines[i+1:] {
			if strings.TrimSpace(entry) == "" {
				break
			}
			n++
		}
		return n
	}
	return 0
}

package compose

import (
	"strings"
	"testing"
	"time"
)

func TestLocalProbeArgs(t *testing.T) {
	args := localProbeArgs("nginx:1.27")
	want := []string{"image", "inspect", "--format", "{{.Created}}|{{.Os}}|{{.Architecture}}", "nginx:1.27"}
	if len(args) != len(want) {
		t.Fatalf("localProbeArgs() = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("localProbeArgs()[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	// `docker image inspect` is a top-level docker command; routing it through
	// command()/remoteCommand() would produce `docker compose image inspect`.
	for _, a := range args {
		if a == "compose" {
			t.Fatalf("localProbeArgs() must carry no compose element: %v", args)
		}
	}
	// A template field the docker struct lacks is a hard execution error, so
	// .Variant must never appear here.
	if got := args[3]; got != localProbeFormat {
		t.Errorf("format arg = %q, want %q", got, localProbeFormat)
	}
	if strings.Contains(localProbeFormat, "Variant") {
		t.Errorf("localProbeFormat must not reference .Variant: %q", localProbeFormat)
	}
}

func TestParseLocalProbe(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantErr     bool
		wantOS      string
		wantArch    string
		wantCreated time.Time
	}{
		{
			name:        "full line",
			in:          "2026-07-07T17:47:22.123456789Z|linux|arm64\n",
			wantOS:      "linux",
			wantArch:    "arm64",
			wantCreated: time.Date(2026, 7, 7, 17, 47, 22, 123456789, time.UTC),
		},
		{
			name:        "no fractional seconds",
			in:          "2026-07-07T17:47:22Z|linux|amd64",
			wantOS:      "linux",
			wantArch:    "amd64",
			wantCreated: time.Date(2026, 7, 7, 17, 47, 22, 0, time.UTC),
		},
		{
			name:     "unparseable timestamp keeps platform",
			in:       "not-a-time|linux|amd64",
			wantOS:   "linux",
			wantArch: "amd64",
		},
		{
			name:     "epoch sentinel keeps platform",
			in:       "1970-01-01T00:00:00Z|linux|amd64",
			wantOS:   "linux",
			wantArch: "amd64",
		},
		{
			name:     "empty timestamp keeps platform",
			in:       "|linux|amd64",
			wantOS:   "linux",
			wantArch: "amd64",
		},
		{name: "too few fields", in: "2026-07-07T17:47:22Z|linux", wantErr: true},
		{name: "too many fields", in: "2026-07-07T17:47:22Z|linux|amd64|v8", wantErr: true},
		{name: "empty output", in: "   \n", wantErr: true},
		{name: "missing os", in: "2026-07-07T17:47:22Z||amd64", wantErr: true},
		{name: "missing arch", in: "2026-07-07T17:47:22Z|linux|", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLocalProbe([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseLocalProbe(%q) = %+v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLocalProbe(%q) error: %v", tt.in, err)
			}
			if got.os != tt.wantOS {
				t.Errorf("os = %q, want %q", got.os, tt.wantOS)
			}
			if got.arch != tt.wantArch {
				t.Errorf("arch = %q, want %q", got.arch, tt.wantArch)
			}
			if !got.created.Equal(tt.wantCreated) {
				t.Errorf("created = %v, want %v", got.created, tt.wantCreated)
			}
		})
	}
}

func TestParseImageTimestamp(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{name: "empty", in: ""},
		{name: "garbage", in: "yesterday"},
		{name: "epoch sentinel", in: "1970-01-01T00:00:00Z"},
		{name: "before epoch", in: "1969-12-31T23:59:59Z"},
		{
			name: "real timestamp",
			in:   "2026-08-19T19:14:43Z",
			want: time.Date(2026, 8, 19, 19, 14, 43, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseImageTimestamp(tt.in)
			if !got.Equal(tt.want) {
				t.Errorf("parseImageTimestamp(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestUpdateDetail_ZeroValueIsAllUnknown(t *testing.T) {
	var d UpdateDetail
	if !d.LocalCreated.IsZero() || !d.NewCreated.IsZero() || d.NewID != "" {
		t.Fatalf("zero UpdateDetail must carry no data, got %+v", d)
	}
}

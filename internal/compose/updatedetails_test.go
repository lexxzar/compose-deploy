package compose

import (
	"os"
	"path/filepath"
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

func TestManifestIndexArgs(t *testing.T) {
	args := manifestIndexArgs("nginx:1.27")
	want := []string{"buildx", "imagetools", "inspect", "--format", "{{json .Manifest}}", "nginx:1.27"}
	if len(args) != len(want) {
		t.Fatalf("manifestIndexArgs() = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("manifestIndexArgs()[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	// `docker buildx imagetools inspect` is a top-level docker command.
	for _, a := range args {
		if a == "compose" {
			t.Fatalf("manifestIndexArgs() must carry no compose element: %v", args)
		}
	}
	// {{.Manifest.Digest}} silently falls through to the human block on
	// buildx v0.30.1; only the {{json .Manifest}} form substitutes.
	if manifestIndexFormat != "{{json .Manifest}}" {
		t.Errorf("manifestIndexFormat = %q, want {{json .Manifest}}", manifestIndexFormat)
	}
}

func TestParseIndexPlatformDigest_Fixtures(t *testing.T) {
	nginx := readFixture(t, "imagetools_manifest_index_nginx.json")
	postgres := readFixture(t, "imagetools_manifest_index_postgres.json")

	tests := []struct {
		name       string
		data       []byte
		os, arch   string
		wantDigest string
	}{
		{
			name: "nginx linux/arm64", data: nginx, os: "linux", arch: "arm64",
			wantDigest: "sha256:40ea9867eb2d91315bb6831f40286f77c086df2a132c36bce50019a54581aea7",
		},
		{
			name: "nginx linux/amd64", data: nginx, os: "linux", arch: "amd64",
			wantDigest: "sha256:c2e305ef468149bdc3297621cea453b47b095816fec4fc7be6ff837bce8deb7d",
		},
		{
			name: "postgres linux/arm64", data: postgres, os: "linux", arch: "arm64",
			wantDigest: "sha256:7ae1143a9f249af815f056751a122a86d7e44ddce0926f2b227e3d5c434444f4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dg, hasIndex, found := parseIndexPlatformDigest(tt.data, tt.os, tt.arch)
			if !hasIndex || !found {
				t.Fatalf("hasIndex=%v found=%v, want true/true", hasIndex, found)
			}
			if dg != tt.wantDigest {
				t.Errorf("digest = %q, want %q", dg, tt.wantDigest)
			}
		})
	}
}

func TestParseIndexPlatformDigest_NeverReturnsAttestation(t *testing.T) {
	// 8 of nginx's 16 entries are attestation manifests carrying
	// platform.architecture == "unknown". Selecting one would pin a
	// provenance blob instead of a runnable image.
	nginx := readFixture(t, "imagetools_manifest_index_nginx.json")
	attestations := map[string]bool{
		"sha256:792be14f71a03d7c73c4d7fb9c1f0d0b83dc294544ae1926db39b8ecaf414a1d": true,
		"sha256:0dfea8046dfa75a40933444052a7a697d5a05387d6460cd3e33822d7aedd5fd5": true,
	}
	for _, arch := range []string{"amd64", "arm64", "arm", "386", "ppc64le", "riscv64", "s390x"} {
		dg, _, found := parseIndexPlatformDigest(nginx, "linux", arch)
		if !found {
			t.Errorf("linux/%s not found in the nginx index", arch)
			continue
		}
		if attestations[dg] {
			t.Errorf("linux/%s selected an attestation digest %q", arch, dg)
		}
	}
	// The "unknown" platform itself must never resolve.
	if dg, hasIndex, found := parseIndexPlatformDigest(nginx, "unknown", "unknown"); found {
		t.Errorf("unknown/unknown = %q hasIndex=%v found=%v, want abort", dg, hasIndex, found)
	}
}

func TestParseIndexPlatformDigest_PlatformAbsent(t *testing.T) {
	// An index exists but the host's platform is missing: abort this image
	// rather than fall back to the single-manifest path, which would run
	// --raw against the index and yield an empty NewID.
	nginx := readFixture(t, "imagetools_manifest_index_nginx.json")
	tests := []struct{ os, arch string }{
		{"windows", "amd64"},
		{"linux", "mips64le"},
		{"darwin", "arm64"},
	}
	for _, tt := range tests {
		t.Run(tt.os+"/"+tt.arch, func(t *testing.T) {
			dg, hasIndex, found := parseIndexPlatformDigest(nginx, tt.os, tt.arch)
			if !hasIndex {
				t.Errorf("hasIndex = false, want true (an index IS present)")
			}
			if found || dg != "" {
				t.Errorf("digest = %q found = %v, want \"\"/false", dg, found)
			}
		})
	}
}

func TestParseIndexPlatformDigest_SingleManifest(t *testing.T) {
	// A single-arch ref returns a manifest with no `manifests` key. The
	// caller must keep the ORIGINAL ref for the remaining steps.
	single := []byte(`{
	  "schemaVersion": 2,
	  "mediaType": "application/vnd.oci.image.manifest.v1+json",
	  "digest": "sha256:` + strings.Repeat("a", 64) + `",
	  "size": 1234,
	  "config": {
	    "mediaType": "application/vnd.oci.image.config.v1+json",
	    "digest": "sha256:` + strings.Repeat("b", 64) + `",
	    "size": 4567
	  },
	  "layers": []
	}`)
	dg, hasIndex, found := parseIndexPlatformDigest(single, "linux", "arm64")
	if hasIndex {
		t.Errorf("hasIndex = true, want false for a single-manifest document")
	}
	if found || dg != "" {
		t.Errorf("digest = %q found = %v, want \"\"/false", dg, found)
	}
}

func TestParseIndexPlatformDigest_VariantTieBreaker(t *testing.T) {
	unqualified := "sha256:" + strings.Repeat("1", 64)
	v7 := "sha256:" + strings.Repeat("2", 64)
	v6 := "sha256:" + strings.Repeat("3", 64)

	entry := func(dg, arch, variant string) string {
		p := `{"os":"linux","architecture":"` + arch + `"`
		if variant != "" {
			p += `,"variant":"` + variant + `"`
		}
		p += `}`
		return `{"digest":"` + dg + `","platform":` + p + `}`
	}
	index := func(entries ...string) []byte {
		return []byte(`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
			strings.Join(entries, ",") + `]}`)
	}

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			// The local probe cannot report a variant, so an unqualified
			// entry wins even when a qualified one comes first.
			name: "unqualified entry wins over a variant",
			data: index(entry(v7, "arm", "v7"), entry(unqualified, "arm", "")),
			want: unqualified,
		},
		{
			name: "unqualified entry wins when it comes first",
			data: index(entry(unqualified, "arm", ""), entry(v7, "arm", "v7")),
			want: unqualified,
		},
		{
			// Variant is a tie-breaker, never a requirement: with only
			// qualified entries, index order decides.
			name: "first variant wins when no unqualified entry exists",
			data: index(entry(v6, "arm", "v6"), entry(v7, "arm", "v7")),
			want: v6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dg, hasIndex, found := parseIndexPlatformDigest(tt.data, "linux", "arm")
			if !hasIndex || !found {
				t.Fatalf("hasIndex=%v found=%v, want true/true", hasIndex, found)
			}
			if dg != tt.want {
				t.Errorf("digest = %q, want %q", dg, tt.want)
			}
		})
	}
}

func TestParseIndexPlatformDigest_FailsClosed(t *testing.T) {
	// Everything the parser cannot trust returns the ABORT state, never the
	// single-manifest state — the latter would silently continue against a
	// ref the caller has no reason to believe in.
	valid := "sha256:" + strings.Repeat("f", 64)
	tests := []struct {
		name     string
		data     string
		os, arch string
	}{
		{name: "malformed json", data: `{"manifests":[`, os: "linux", arch: "arm64"},
		{name: "not json at all", data: "Name: docker.io/library/nginx:latest", os: "linux", arch: "arm64"},
		{name: "empty input", data: "", os: "linux", arch: "arm64"},
		{name: "manifests is null", data: `{"manifests":null}`, os: "linux", arch: "arm64"},
		{name: "manifests is an object", data: `{"manifests":{"a":1}}`, os: "linux", arch: "arm64"},
		{name: "manifests is empty", data: `{"manifests":[]}`, os: "linux", arch: "arm64"},
		{
			name: "digest is not a sha256",
			data: `{"manifests":[{"digest":"latest","platform":{"os":"linux","architecture":"arm64"}}]}`,
			os:   "linux", arch: "arm64",
		},
		{
			name: "digest is truncated",
			data: `{"manifests":[{"digest":"sha256:abc","platform":{"os":"linux","architecture":"arm64"}}]}`,
			os:   "linux", arch: "arm64",
		},
		{
			name: "digest carries trailing junk",
			data: `{"manifests":[{"digest":"` + valid + ` extra","platform":{"os":"linux","architecture":"arm64"}}]}`,
			os:   "linux", arch: "arm64",
		},
		{
			name: "entry has no platform",
			data: `{"manifests":[{"digest":"` + valid + `"}]}`,
			os:   "linux", arch: "arm64",
		},
		{
			name: "empty os",
			data: `{"manifests":[{"digest":"` + valid + `","platform":{"os":"linux","architecture":"arm64"}}]}`,
			os:   "", arch: "arm64",
		},
		{
			name: "empty arch",
			data: `{"manifests":[{"digest":"` + valid + `","platform":{"os":"linux","architecture":"arm64"}}]}`,
			os:   "linux", arch: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dg, hasIndex, found := parseIndexPlatformDigest([]byte(tt.data), tt.os, tt.arch)
			if !hasIndex {
				t.Errorf("hasIndex = false, want true — doubt must abort, not take the single-manifest path")
			}
			if found || dg != "" {
				t.Errorf("digest = %q found = %v, want \"\"/false", dg, found)
			}
		})
	}
}

func TestValidImagetoolsDigest(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "canonical", in: "sha256:" + hex64, want: "sha256:" + hex64},
		{name: "surrounding space", in: "  sha256:" + hex64 + "\n", want: "sha256:" + hex64},
		{name: "uppercase normalized", in: "sha256:" + strings.ToUpper(hex64), want: "sha256:" + hex64},
		{name: "empty", in: ""},
		{name: "too short", in: "sha256:abcdef"},
		{name: "too long", in: "sha256:" + hex64 + "a"},
		{name: "wrong algorithm", in: "sha512:" + hex64},
		{name: "embedded in a line", in: "Digest: sha256:" + hex64},
		{name: "non-hex", in: "sha256:" + strings.Repeat("z", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validImagetoolsDigest(tt.in); got != tt.want {
				t.Errorf("validImagetoolsDigest(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// readFixture loads a captured registry response from testdata. Every parser
// test reads a fixture rather than calling a registry: a suite that hit Docker
// Hub would reproduce the anonymous-quota 429 in CI.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

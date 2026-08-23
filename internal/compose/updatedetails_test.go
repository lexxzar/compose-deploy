package compose

import (
	"context"
	"errors"
	"fmt"
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

func TestRawManifestArgs(t *testing.T) {
	args := rawManifestArgs("nginx@sha256:" + strings.Repeat("a", 64))
	want := []string{"buildx", "imagetools", "inspect", "--raw", "nginx@sha256:" + strings.Repeat("a", 64)}
	if len(args) != len(want) {
		t.Fatalf("rawManifestArgs() = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("rawManifestArgs()[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	for _, a := range args {
		if a == "compose" {
			t.Fatalf("rawManifestArgs() must carry no compose element: %v", args)
		}
	}
}

func TestImageConfigArgs(t *testing.T) {
	args := imageConfigArgs("nginx:1.27")
	want := []string{"buildx", "imagetools", "inspect", "--format", "{{json .Image}}", "nginx:1.27"}
	if len(args) != len(want) {
		t.Fatalf("imageConfigArgs() = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("imageConfigArgs()[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	for _, a := range args {
		if a == "compose" {
			t.Fatalf("imageConfigArgs() must carry no compose element: %v", args)
		}
	}
	// Only the {{json .X}} forms substitute on buildx v0.30.1; the dotted
	// forms silently fall through to the human block.
	if imageConfigFormat != "{{json .Image}}" {
		t.Errorf("imageConfigFormat = %q, want {{json .Image}}", imageConfigFormat)
	}
}

func TestParseConfigDigest_Fixture(t *testing.T) {
	raw := readFixture(t, "imagetools_raw_manifest.json")
	want := "sha256:c05eced01234567890abcdef1234567890abcdef1234567890abcdef12345678"
	if got := parseConfigDigest(raw); got != want {
		t.Errorf("parseConfigDigest() = %q, want %q", got, want)
	}
}

func TestParseConfigDigest_OmitsOnDoubt(t *testing.T) {
	hex64 := strings.Repeat("d", 64)
	tests := []struct {
		name string
		in   string
	}{
		{name: "malformed json", in: `{"config":`},
		{name: "not json at all", in: "Name: docker.io/library/nginx:latest"},
		{name: "empty input", in: ""},
		{name: "no config key", in: `{"schemaVersion":2,"layers":[]}`},
		{name: "config is null", in: `{"config":null}`},
		{name: "config has no digest", in: `{"config":{"size":7443}}`},
		{name: "digest is empty", in: `{"config":{"digest":""}}`},
		{name: "digest is a tag", in: `{"config":{"digest":"latest"}}`},
		{name: "digest is truncated", in: `{"config":{"digest":"sha256:abc"}}`},
		{name: "digest is sha512", in: `{"config":{"digest":"sha512:` + hex64 + `"}}`},
		{name: "digest carries trailing junk", in: `{"config":{"digest":"sha256:` + hex64 + ` extra"}}`},
		// An INDEX has manifests and no config: running --raw against one is
		// exactly what the hasIndex=true/found=false abort exists to prevent,
		// and if it ever happens the row must drop rather than be guessed.
		{name: "index document", in: `{"manifests":[{"digest":"sha256:` + hex64 + `"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseConfigDigest([]byte(tt.in)); got != "" {
				t.Errorf("parseConfigDigest(%q) = %q, want \"\"", tt.in, got)
			}
		})
	}
}

func TestParseConfigDigest_NormalizesCase(t *testing.T) {
	hex64 := strings.Repeat("A", 64)
	want := "sha256:" + strings.Repeat("a", 64)
	if got := parseConfigDigest([]byte(`{"config":{"digest":"sha256:` + hex64 + `"}}`)); got != want {
		t.Errorf("parseConfigDigest() = %q, want %q", got, want)
	}
}

func TestParseImageCreated_ObjectForm(t *testing.T) {
	// A PINNED ref returns the OCI image config directly.
	data := readFixture(t, "imagetools_image_config_object.json")
	want := time.Date(2026, 8, 19, 19, 14, 43, 123456789, time.UTC)
	got, ok := parseImageCreated(data, "linux", "arm64")
	if !ok {
		t.Fatalf("parseImageCreated() ok = false, want true")
	}
	if !got.Equal(want) {
		t.Errorf("created = %v, want %v", got, want)
	}
	// The bare object is the only config there is, so the requested platform
	// does not gate it — a single-arch ref reaches step 4 unpinned.
	got, ok = parseImageCreated(data, "linux", "amd64")
	if !ok || !got.Equal(want) {
		t.Errorf("parseImageCreated(mismatched platform) = %v/%v, want %v/true", got, ok, want)
	}
}

func TestParseImageCreated_MapForm(t *testing.T) {
	// A BARE tag returns a platform-keyed map.
	data := readFixture(t, "imagetools_image_config_map.json")
	tests := []struct {
		name     string
		os, arch string
		want     time.Time
	}{
		{
			name: "linux/arm64", os: "linux", arch: "arm64",
			want: time.Date(2026, 8, 19, 19, 14, 43, 123456789, time.UTC),
		},
		{
			name: "linux/amd64", os: "linux", arch: "amd64",
			want: time.Date(2026, 8, 19, 19, 14, 40, 500000000, time.UTC),
		},
		{
			// The variant-qualified key still matches on os+arch alone.
			name: "linux/arm resolves the v7 key", os: "linux", arch: "arm",
			want: time.Date(2026, 8, 19, 19, 14, 38, 250000000, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseImageCreated(data, tt.os, tt.arch)
			if !ok {
				t.Fatalf("parseImageCreated() ok = false, want true")
			}
			if !got.Equal(tt.want) {
				t.Errorf("created = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseImageCreated_EpochSentinel(t *testing.T) {
	// Reproducible builds (distroless, ko, Bazel, nix) report 1970-01-01.
	// That is a placeholder, not a build date, so the row must drop.
	data := readFixture(t, "imagetools_image_config_map.json")
	if got, ok := parseImageCreated(data, "linux", "386"); ok {
		t.Errorf("parseImageCreated(linux/386) = %v/true, want zero/false", got)
	}
	obj := []byte(`{"created":"1970-01-01T00:00:00Z","architecture":"amd64","os":"linux"}`)
	if got, ok := parseImageCreated(obj, "linux", "amd64"); ok {
		t.Errorf("parseImageCreated(epoch object) = %v/true, want zero/false", got)
	}
}

func TestParseImageCreated_OmitsOnDoubt(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		os, arch string
	}{
		{name: "malformed json", in: `{"created":`, os: "linux", arch: "amd64"},
		{name: "not json at all", in: "Name: docker.io/library/nginx:latest", os: "linux", arch: "amd64"},
		{name: "empty input", in: "", os: "linux", arch: "amd64"},
		{name: "empty object", in: `{}`, os: "linux", arch: "amd64"},
		{name: "json array", in: `[{"created":"2026-08-19T19:14:43Z"}]`, os: "linux", arch: "amd64"},
		// Neither shape: no "/" in the keys and no image-config field either.
		{name: "unrecognised shape", in: `{"foo":1,"bar":2}`, os: "linux", arch: "amd64"},
		{name: "object with no created", in: `{"architecture":"amd64","os":"linux","rootfs":{}}`, os: "linux", arch: "amd64"},
		{name: "object created is garbage", in: `{"created":"yesterday","rootfs":{}}`, os: "linux", arch: "amd64"},
		{
			name: "map platform absent",
			in:   `{"linux/amd64":{"created":"2026-08-19T19:14:43Z"}}`,
			os:   "windows", arch: "amd64",
		},
		{
			name: "map arch absent",
			in:   `{"linux/amd64":{"created":"2026-08-19T19:14:43Z"}}`,
			os:   "linux", arch: "s390x",
		},
		{
			// An attestation entry must never satisfy a platform request.
			name: "map only unknown platform",
			in:   `{"unknown/unknown":{"created":"2026-08-19T19:14:43Z"}}`,
			os:   "unknown", arch: "unknown",
		},
		{
			name: "map with empty os argument",
			in:   `{"linux/amd64":{"created":"2026-08-19T19:14:43Z"}}`,
			os:   "", arch: "amd64",
		},
		{
			name: "map with empty arch argument",
			in:   `{"linux/amd64":{"created":"2026-08-19T19:14:43Z"}}`,
			os:   "linux", arch: "",
		},
		{
			name: "map value is not an object",
			in:   `{"linux/amd64":"2026-08-19T19:14:43Z"}`,
			os:   "linux", arch: "amd64",
		},
		{
			name: "map key has too many segments",
			in:   `{"linux/arm/v7/extra":{"created":"2026-08-19T19:14:43Z"}}`,
			os:   "linux", arch: "arm",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseImageCreated([]byte(tt.in), tt.os, tt.arch)
			if ok || !got.IsZero() {
				t.Errorf("parseImageCreated(%q) = %v/%v, want zero/false", tt.in, got, ok)
			}
		})
	}
}

func TestParseImageCreated_VariantTieBreaker(t *testing.T) {
	// Go map iteration order is not stable, so the choice among matching keys
	// must not depend on it: an unqualified key wins, and among qualified keys
	// the lexicographically first wins.
	unqualified := `"linux/arm":{"created":"2026-08-19T10:00:00Z"}`
	v6 := `"linux/arm/v6":{"created":"2026-08-19T11:00:00Z"}`
	v7 := `"linux/arm/v7":{"created":"2026-08-19T12:00:00Z"}`

	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{
			name: "unqualified key wins",
			in:   `{` + v7 + `,` + unqualified + `,` + v6 + `}`,
			want: time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC),
		},
		{
			name: "lowest variant key wins when no unqualified key exists",
			in:   `{` + v7 + `,` + v6 + `}`,
			want: time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Repeat so a map-order-dependent implementation cannot pass by luck.
			for i := 0; i < 50; i++ {
				got, ok := parseImageCreated([]byte(tt.in), "linux", "arm")
				if !ok {
					t.Fatalf("parseImageCreated() ok = false, want true")
				}
				if !got.Equal(tt.want) {
					t.Fatalf("created = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// detailStep names one of the four round-trips fetchUpdateDetail makes, so a
// test can reply per step instead of matching whole argv slices.
type detailStep int

const (
	stepUnknown detailStep = iota
	stepLocal
	stepIndex
	stepRaw
	stepConfig
)

func (s detailStep) String() string {
	switch s {
	case stepLocal:
		return "local probe"
	case stepIndex:
		return "manifest index"
	case stepRaw:
		return "raw manifest"
	case stepConfig:
		return "image config"
	}
	return "unknown"
}

func classifyDetailCall(args []string) detailStep {
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		return stepLocal
	}
	for _, a := range args {
		switch a {
		case "--raw":
			return stepRaw
		case manifestIndexFormat:
			return stepIndex
		case imageConfigFormat:
			return stepConfig
		}
	}
	return stepUnknown
}

// detailRunner wires a per-step reply function into the same dockerRunner seam
// production uses, so the whole four-step sequence is exercised without Docker.
// The image reference is always the LAST argv element on all four shapes.
func detailRunner(t *testing.T, reply func(step detailStep, ref string) ([]byte, error)) *fakeDockerRunner {
	t.Helper()
	f := &fakeDockerRunner{}
	f.runFunc = func(args []string) ([]byte, error) {
		for _, a := range args {
			if a == "compose" {
				t.Errorf("update-detail argv must carry no compose element: %v", args)
			}
		}
		step := classifyDetailCall(args)
		if step == stepUnknown {
			t.Errorf("unexpected docker call: %v", args)
			return nil, fmt.Errorf("unexpected docker call")
		}
		return reply(step, args[len(args)-1])
	}
	return f
}

// stepRefs returns the reference each recorded call addressed, in order.
func stepRefs(f *fakeDockerRunner) []string {
	out := make([]string, 0, len(f.runCalls))
	for _, args := range f.runCalls {
		out = append(out, args[len(args)-1])
	}
	return out
}

const (
	detailLocalProbeARM64 = "2026-07-07T17:47:22Z|linux|arm64\n"
	detailNginxARM64      = "sha256:40ea9867eb2d91315bb6831f40286f77c086df2a132c36bce50019a54581aea7"
	detailNewID           = "sha256:c05eced01234567890abcdef1234567890abcdef1234567890abcdef12345678"
)

var (
	detailWantLocalCreated = time.Date(2026, 7, 7, 17, 47, 22, 0, time.UTC)
	detailWantNewCreated   = time.Date(2026, 8, 19, 19, 14, 43, 123456789, time.UTC)
)

func TestScanUpdateDetails_HappyPath(t *testing.T) {
	index := readFixture(t, "imagetools_manifest_index_nginx.json")
	raw := readFixture(t, "imagetools_raw_manifest.json")
	config := readFixture(t, "imagetools_image_config_object.json")

	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		switch step {
		case stepLocal:
			return []byte(detailLocalProbeARM64), nil
		case stepIndex:
			return index, nil
		case stepRaw:
			return raw, nil
		default:
			return config, nil
		}
	})

	out, err := scanUpdateDetails(context.Background(), map[string]string{"web": "nginx:1.27"}, f)
	if err != nil {
		t.Fatalf("scanUpdateDetails() error = %v, want nil", err)
	}
	det, ok := out["web"]
	if !ok {
		t.Fatalf("scanUpdateDetails() = %v, want an entry for web", out)
	}
	if !det.LocalCreated.Equal(detailWantLocalCreated) {
		t.Errorf("LocalCreated = %v, want %v", det.LocalCreated, detailWantLocalCreated)
	}
	if det.NewID != detailNewID {
		t.Errorf("NewID = %q, want %q", det.NewID, detailNewID)
	}
	if !det.NewCreated.Equal(detailWantNewCreated) {
		t.Errorf("NewCreated = %v, want %v", det.NewCreated, detailWantNewCreated)
	}

	if len(f.runCalls) != 4 {
		t.Fatalf("made %d docker calls, want 4: %v", len(f.runCalls), f.runCalls)
	}
	// Steps 3 and 4 must address the PINNED ref: an unpinned bare tag costs
	// ~4.3s and returns a platform-keyed map from which the wrong platform
	// could be picked.
	pinned := "nginx@" + detailNginxARM64
	refs := stepRefs(f)
	want := []string{"nginx:1.27", "nginx:1.27", pinned, pinned}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("call %d addressed %q, want %q", i, refs[i], want[i])
		}
	}
}

func TestScanUpdateDetails_SingleManifest(t *testing.T) {
	// A single-manifest ref has no manifests key, so there is nothing to pin
	// to and the ORIGINAL ref must carry the last two steps. Step 4 then
	// returns the platform-keyed map form rather than a bare object.
	raw := readFixture(t, "imagetools_raw_manifest.json")
	config := readFixture(t, "imagetools_image_config_map.json")

	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		switch step {
		case stepLocal:
			return []byte(detailLocalProbeARM64), nil
		case stepIndex:
			return raw, nil // no manifests key
		case stepRaw:
			return raw, nil
		default:
			return config, nil
		}
	})

	out, err := scanUpdateDetails(context.Background(), map[string]string{"web": "internal/app:v3"}, f)
	if err != nil {
		t.Fatalf("scanUpdateDetails() error = %v, want nil", err)
	}
	det, ok := out["web"]
	if !ok {
		t.Fatalf("scanUpdateDetails() = %v, want an entry for web", out)
	}
	if det.NewID != detailNewID {
		t.Errorf("NewID = %q, want %q", det.NewID, detailNewID)
	}
	if !det.NewCreated.Equal(detailWantNewCreated) {
		t.Errorf("NewCreated = %v, want %v", det.NewCreated, detailWantNewCreated)
	}
	for i, ref := range stepRefs(f) {
		if ref != "internal/app:v3" {
			t.Errorf("call %d addressed %q, want the original ref", i, ref)
		}
	}
}

func TestScanUpdateDetails_PlatformAbsentAborts(t *testing.T) {
	// An index that does not carry the host's platform must abort the image
	// rather than fall through to the single-manifest path: --raw against an
	// index yields no config.digest, and the config step would return a map
	// from which any platform could be picked.
	index := readFixture(t, "imagetools_manifest_index_nginx.json")
	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		switch step {
		case stepLocal:
			return []byte("2026-07-07T17:47:22Z|windows|amd64"), nil
		case stepIndex:
			return index, nil
		}
		t.Errorf("step %v must not run after the platform-absent abort", step)
		return nil, fmt.Errorf("unreachable")
	})

	out, err := scanUpdateDetails(context.Background(), map[string]string{"web": "nginx:1.27"}, f)
	if err != nil {
		t.Fatalf("scanUpdateDetails() error = %v, want nil — a per-image abort is not a scan failure", err)
	}
	if len(out) != 0 {
		t.Errorf("scanUpdateDetails() = %v, want no entries", out)
	}
	if len(f.runCalls) != 2 {
		t.Errorf("made %d docker calls, want 2 — the registry steps must be skipped", len(f.runCalls))
	}
}

func TestScanUpdateDetails_PerImageFailureOmitsService(t *testing.T) {
	index := readFixture(t, "imagetools_manifest_index_nginx.json")
	raw := readFixture(t, "imagetools_raw_manifest.json")
	config := readFixture(t, "imagetools_image_config_object.json")

	tests := []struct {
		name      string
		failAt    detailStep
		localOut  string
		wantCalls int
	}{
		{name: "local probe fails", failAt: stepLocal, wantCalls: 1},
		{name: "manifest index fails", failAt: stepIndex, wantCalls: 2},
		{name: "raw manifest fails", failAt: stepRaw, wantCalls: 3},
		{name: "image config fails", failAt: stepConfig, wantCalls: 4},
		{name: "local probe unparseable", localOut: "not-a-probe-line", wantCalls: 1},
		{name: "local probe has no platform", localOut: "2026-07-07T17:47:22Z||", wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
				if tt.failAt != stepUnknown && step == tt.failAt {
					return nil, fmt.Errorf("docker: boom")
				}
				switch step {
				case stepLocal:
					if tt.localOut != "" {
						return []byte(tt.localOut), nil
					}
					return []byte(detailLocalProbeARM64), nil
				case stepIndex:
					return index, nil
				case stepRaw:
					return raw, nil
				default:
					return config, nil
				}
			})

			out, err := scanUpdateDetails(context.Background(), map[string]string{"web": "nginx:1.27"}, f)
			if err != nil {
				t.Fatalf("scanUpdateDetails() error = %v, want nil — a per-image failure is absorbed", err)
			}
			if len(out) != 0 {
				t.Errorf("scanUpdateDetails() = %v, want no entries", out)
			}
			if len(f.runCalls) != tt.wantCalls {
				t.Errorf("made %d docker calls, want %d — the sequence must stop at the first failure",
					len(f.runCalls), tt.wantCalls)
			}
		})
	}
}

func TestScanUpdateDetails_PartialResultIsValid(t *testing.T) {
	// One image failing must not drop the services whose image resolved.
	index := readFixture(t, "imagetools_manifest_index_nginx.json")
	raw := readFixture(t, "imagetools_raw_manifest.json")
	config := readFixture(t, "imagetools_image_config_object.json")

	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		if strings.HasPrefix(ref, "broken") && step == stepRaw {
			return nil, fmt.Errorf("docker: manifest unknown")
		}
		switch step {
		case stepLocal:
			return []byte(detailLocalProbeARM64), nil
		case stepIndex:
			return index, nil
		case stepRaw:
			return raw, nil
		default:
			return config, nil
		}
	})

	wanted := map[string]string{"web": "nginx:1.27", "api": "broken:1"}
	out, err := scanUpdateDetails(context.Background(), wanted, f)
	if err != nil {
		t.Fatalf("scanUpdateDetails() error = %v, want nil", err)
	}
	if _, ok := out["api"]; ok {
		t.Errorf("scanUpdateDetails() = %v, want api absent", out)
	}
	if _, ok := out["web"]; !ok {
		t.Errorf("scanUpdateDetails() = %v, want web present", out)
	}
}

func TestScanUpdateDetails_AbsentFieldKeepsEntry(t *testing.T) {
	// A step that SUCCEEDS but whose parser returns "absent" drops only its own
	// row: the entry survives so the other rows still render.
	index := readFixture(t, "imagetools_manifest_index_nginx.json")
	raw := readFixture(t, "imagetools_raw_manifest.json")
	config := readFixture(t, "imagetools_image_config_object.json")

	tests := []struct {
		name           string
		garbleRaw      bool
		garbleConfig   bool
		wantNewID      string
		wantNewCreated bool
	}{
		{name: "raw manifest unparseable", garbleRaw: true, wantNewCreated: true},
		{name: "image config unparseable", garbleConfig: true, wantNewID: detailNewID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
				switch step {
				case stepLocal:
					return []byte(detailLocalProbeARM64), nil
				case stepIndex:
					return index, nil
				case stepRaw:
					if tt.garbleRaw {
						return []byte("Name: docker.io/library/nginx:1.27\n"), nil
					}
					return raw, nil
				default:
					if tt.garbleConfig {
						return []byte("Name: docker.io/library/nginx:1.27\n"), nil
					}
					return config, nil
				}
			})

			out, err := scanUpdateDetails(context.Background(), map[string]string{"web": "nginx:1.27"}, f)
			if err != nil {
				t.Fatalf("scanUpdateDetails() error = %v, want nil", err)
			}
			det, ok := out["web"]
			if !ok {
				t.Fatalf("scanUpdateDetails() = %v, want an entry for web", out)
			}
			if !det.LocalCreated.Equal(detailWantLocalCreated) {
				t.Errorf("LocalCreated = %v, want %v", det.LocalCreated, detailWantLocalCreated)
			}
			if det.NewID != tt.wantNewID {
				t.Errorf("NewID = %q, want %q", det.NewID, tt.wantNewID)
			}
			if got := !det.NewCreated.IsZero(); got != tt.wantNewCreated {
				t.Errorf("NewCreated set = %v, want %v (%v)", got, tt.wantNewCreated, det.NewCreated)
			}
		})
	}
}

func TestScanUpdateDetails_TransportAbort(t *testing.T) {
	// A dead SSH hop poisons every remaining image, so the scan returns at the
	// first errSSHTransport with the partial map alongside the error — the same
	// contract scanImageUpdates follows.
	index := readFixture(t, "imagetools_manifest_index_nginx.json")
	raw := readFixture(t, "imagetools_raw_manifest.json")
	config := readFixture(t, "imagetools_image_config_object.json")

	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		if strings.HasPrefix(ref, "dead") {
			return nil, fmt.Errorf("%w: ssh: connect to host db1 port 22: no route to host", errSSHTransport)
		}
		switch step {
		case stepLocal:
			return []byte(detailLocalProbeARM64), nil
		case stepIndex:
			return index, nil
		case stepRaw:
			return raw, nil
		default:
			return config, nil
		}
	})

	// Services are visited in sorted order, so "a-web" resolves, "b-api"
	// aborts, and "c-cache" is never reached.
	wanted := map[string]string{"a-web": "nginx:1.27", "b-api": "dead:1", "c-cache": "redis:7"}
	out, err := scanUpdateDetails(context.Background(), wanted, f)
	if !errors.Is(err, errSSHTransport) {
		t.Fatalf("scanUpdateDetails() error = %v, want it to wrap errSSHTransport", err)
	}
	if _, ok := out["a-web"]; !ok {
		t.Errorf("scanUpdateDetails() = %v, want the partial result to keep a-web", out)
	}
	if len(out) != 1 {
		t.Errorf("scanUpdateDetails() = %v, want exactly the pre-abort entry", out)
	}
	for _, ref := range stepRefs(f) {
		if strings.HasPrefix(ref, "redis") {
			t.Errorf("reached %q after the transport abort: %v", ref, stepRefs(f))
		}
	}
}

func TestScanUpdateDetails_MemoizesByImage(t *testing.T) {
	// Three services off one image cost one call set. Host containers repeat a
	// single tag constantly, and every round-trip here is a registry request
	// against an anonymous quota.
	index := readFixture(t, "imagetools_manifest_index_nginx.json")
	raw := readFixture(t, "imagetools_raw_manifest.json")
	config := readFixture(t, "imagetools_image_config_object.json")

	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		switch step {
		case stepLocal:
			return []byte(detailLocalProbeARM64), nil
		case stepIndex:
			return index, nil
		case stepRaw:
			return raw, nil
		default:
			return config, nil
		}
	})

	wanted := map[string]string{"web": "nginx:1.27", "edge": "nginx:1.27", "proxy": "nginx:1.27"}
	out, err := scanUpdateDetails(context.Background(), wanted, f)
	if err != nil {
		t.Fatalf("scanUpdateDetails() error = %v, want nil", err)
	}
	if len(out) != 3 {
		t.Fatalf("scanUpdateDetails() = %v, want all three services", out)
	}
	for svc, det := range out {
		if det.NewID != detailNewID {
			t.Errorf("%s NewID = %q, want %q", svc, det.NewID, detailNewID)
		}
	}
	if len(f.runCalls) != 4 {
		t.Errorf("made %d docker calls, want 4 — one call set per distinct image", len(f.runCalls))
	}
}

func TestScanUpdateDetails_MemoizesFailures(t *testing.T) {
	// A failing image must not be retried per service either — the cost is the
	// same whichever way it ends.
	calls := 0
	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("docker: no such image")
	})

	wanted := map[string]string{"web": "nginx:1.27", "edge": "nginx:1.27"}
	out, err := scanUpdateDetails(context.Background(), wanted, f)
	if err != nil {
		t.Fatalf("scanUpdateDetails() error = %v, want nil", err)
	}
	if len(out) != 0 {
		t.Errorf("scanUpdateDetails() = %v, want no entries", out)
	}
	if calls != 1 {
		t.Errorf("made %d docker calls, want 1 — the failure is memoized too", calls)
	}
}

func TestScanUpdateDetails_EmptyInput(t *testing.T) {
	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		t.Errorf("no docker call is allowed for an empty service set")
		return nil, nil
	})
	out, err := scanUpdateDetails(context.Background(), nil, f)
	if err != nil {
		t.Fatalf("scanUpdateDetails() error = %v, want nil", err)
	}
	if len(out) != 0 {
		t.Errorf("scanUpdateDetails() = %v, want an empty map", out)
	}
}

func TestPinnedImageRef(t *testing.T) {
	index := readFixture(t, "imagetools_manifest_index_nginx.json")
	single := readFixture(t, "imagetools_raw_manifest.json")
	probe := localProbe{os: "linux", arch: "arm64"}

	tests := []struct {
		name    string
		image   string
		data    []byte
		probe   localProbe
		want    string
		wantErr bool
	}{
		{
			name: "index pins by digest", image: "nginx:1.27", data: index, probe: probe,
			want: "nginx@" + detailNginxARM64,
		},
		{
			// StripTag keeps a registry port intact, which is why the repo
			// portion is never re-derived here.
			name: "registry port survives", image: "localhost:5000/nginx:1.27", data: index, probe: probe,
			want: "localhost:5000/nginx@" + detailNginxARM64,
		},
		{
			name: "single manifest keeps the original ref", image: "internal/app:v3", data: single, probe: probe,
			want: "internal/app:v3",
		},
		{
			name: "platform absent aborts", image: "nginx:1.27", data: index,
			probe: localProbe{os: "windows", arch: "amd64"}, wantErr: true,
		},
		{
			name: "malformed index aborts", image: "nginx:1.27", data: []byte("Name: nginx"), probe: probe,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pinnedImageRef(tt.image, tt.data, tt.probe)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("pinnedImageRef() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("pinnedImageRef() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("pinnedImageRef() = %q, want %q", got, tt.want)
			}
		})
	}
}

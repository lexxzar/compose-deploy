package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// "arm" is deliberately absent: the real index carries it twice (v5 and
	// v7), which is the ambiguity TestParseIndexPlatformDigest_AmbiguousVariant
	// pins as an abort.
	for _, arch := range []string{"amd64", "arm64", "386", "ppc64le", "riscv64", "s390x"} {
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
			// A single variant-qualified entry is unambiguous: it is the only
			// thing the os+arch pair can mean.
			name: "the only variant resolves",
			data: index(entry(v7, "arm", "v7"), entry(unqualified, "amd64", "")),
			want: v7,
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

	// Two variant-qualified entries and no unqualified one is a GUESS, and a
	// guess draws another image's digest and build date under a row inviting
	// comparison with the local one. Abort instead.
	ambiguous := index(entry(v6, "arm", "v6"), entry(v7, "arm", "v7"))
	dg, hasIndex, found := parseIndexPlatformDigest(ambiguous, "linux", "arm")
	if !hasIndex {
		t.Errorf("hasIndex = false, want true (an index IS present)")
	}
	if found || dg != "" {
		t.Errorf("ambiguous variants = %q/%v, want \"\"/false", dg, found)
	}
}

func TestParseIndexPlatformDigest_AmbiguousVariant(t *testing.T) {
	// Both real captures carry linux/arm twice — nginx as v5+v7, postgres as
	// v6+v7. A 32-bit ARM host (Raspberry Pi OS) reports architecture "arm"
	// with no variant the local probe can see, so neither entry can be chosen
	// without guessing. The rows drop; arm64 and amd64 are unaffected.
	for _, name := range []string{"imagetools_manifest_index_nginx.json", "imagetools_manifest_index_postgres.json"} {
		t.Run(name, func(t *testing.T) {
			data := readFixture(t, name)
			dg, hasIndex, found := parseIndexPlatformDigest(data, "linux", "arm")
			if !hasIndex {
				t.Errorf("hasIndex = false, want true")
			}
			if found || dg != "" {
				t.Errorf("linux/arm = %q/%v, want \"\"/false", dg, found)
			}
			if _, _, ok := parseIndexPlatformDigest(data, "linux", "arm64"); !ok {
				t.Errorf("linux/arm64 must still resolve")
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
		// A null document unmarshals into a NIL map without an error, so the
		// manifests lookup misses and the parser would fall through to the
		// single-manifest state — failing OPEN, against the rule above.
		{name: "null document", data: "null", os: "linux", arch: "arm64"},
		{name: "empty object", data: "{}", os: "linux", arch: "arm64"},
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
	// Step 4 only ever addresses a ref that resolves to exactly one manifest,
	// so buildx returns the OCI image config directly.
	data := readFixture(t, "imagetools_image_config_object.json")
	want := time.Date(2026, 8, 19, 19, 14, 43, 123456789, time.UTC)
	got, ok := parseImageCreated(data)
	if !ok {
		t.Fatalf("parseImageCreated() ok = false, want true")
	}
	if !got.Equal(want) {
		t.Errorf("created = %v, want %v", got, want)
	}
}

func TestParseImageCreated_EpochSentinel(t *testing.T) {
	// Reproducible builds (distroless, ko, Bazel, nix) report 1970-01-01.
	// That is a placeholder, not a build date, so the row must drop.
	obj := []byte(`{"created":"1970-01-01T00:00:00Z","architecture":"amd64","os":"linux"}`)
	if got, ok := parseImageCreated(obj); ok {
		t.Errorf("parseImageCreated(epoch object) = %v/true, want zero/false", got)
	}
}

func TestParseImageCreated_OmitsOnDoubt(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "malformed json", in: `{"created":`},
		{name: "not json at all", in: "Name: docker.io/library/nginx:latest"},
		{name: "empty input", in: ""},
		{name: "empty object", in: `{}`},
		{name: "json array", in: `[{"created":"2026-08-19T19:14:43Z"}]`},
		{name: "unrecognised shape", in: `{"foo":1,"bar":2}`},
		{name: "object with no created", in: `{"architecture":"amd64","os":"linux","rootfs":{}}`},
		{name: "object created is garbage", in: `{"created":"yesterday","rootfs":{}}`},
		{
			// The platform-keyed map a BARE multi-platform tag returns cannot
			// reach step 4 — such a tag always carries an index, so it is
			// pinned before step 4 or aborted at step 2. It decodes cleanly
			// with no top-level "created", so it omits rather than guesses.
			name: "platform-keyed map is not read",
			in:   `{"linux/amd64":{"created":"2026-08-19T19:14:43Z"},"linux/arm/v7":{"created":"2026-08-19T19:14:38Z"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseImageCreated([]byte(tt.in))
			if ok || !got.IsZero() {
				t.Errorf("parseImageCreated(%q) = %v/%v, want zero/false", tt.in, got, ok)
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
	// to and the ORIGINAL ref must carry the remaining steps. Step 2's own
	// output IS that manifest, so its config.digest is already in hand and
	// step 3 must NOT run — every skipped registry call is one further from
	// the anonymous rate limit.
	raw := readFixture(t, "imagetools_raw_manifest.json")
	config := readFixture(t, "imagetools_image_config_object.json")

	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		switch step {
		case stepLocal:
			return []byte(detailLocalProbeARM64), nil
		case stepIndex:
			return raw, nil // no manifests key
		case stepRaw:
			t.Errorf("step 3 must not run when step 2 already returned the manifest")
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
	if len(f.runCalls) != 3 {
		t.Errorf("made %d docker calls, want 3 (step 3 reuses step 2's output): %v", len(f.runCalls), f.runCalls)
	}
	for i, ref := range stepRefs(f) {
		if ref != "internal/app:v3" {
			t.Errorf("call %d addressed %q, want the original ref", i, ref)
		}
	}
}

func TestScanUpdateDetails_SingleManifestFallsBackToRaw(t *testing.T) {
	// If step 2's {{json .Manifest}} does NOT carry a config descriptor (a
	// buildx version whose template output differs), the --raw call still runs
	// so the `update id` row survives.
	raw := readFixture(t, "imagetools_raw_manifest.json")
	config := readFixture(t, "imagetools_image_config_object.json")
	rawSteps := 0

	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		switch step {
		case stepLocal:
			return []byte(detailLocalProbeARM64), nil
		case stepIndex:
			return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`), nil
		case stepRaw:
			rawSteps++
			return raw, nil
		default:
			return config, nil
		}
	})

	out, err := scanUpdateDetails(context.Background(), map[string]string{"web": "internal/app:v3"}, f)
	if err != nil {
		t.Fatalf("scanUpdateDetails() error = %v, want nil", err)
	}
	if rawSteps != 1 {
		t.Errorf("step 3 ran %d times, want 1", rawSteps)
	}
	if out["web"].NewID != detailNewID {
		t.Errorf("NewID = %q, want %q", out["web"].NewID, detailNewID)
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
		name       string
		image      string
		data       []byte
		probe      localProbe
		want       string
		wantPinned bool
		wantErr    bool
	}{
		{
			name: "index pins by digest", image: "nginx:1.27", data: index, probe: probe,
			want: "nginx@" + detailNginxARM64, wantPinned: true,
		},
		{
			// StripTag keeps a registry port intact, which is why the repo
			// portion is never re-derived here.
			name: "registry port survives", image: "localhost:5000/nginx:1.27", data: index, probe: probe,
			want: "localhost:5000/nginx@" + detailNginxARM64, wantPinned: true,
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
			got, pinned, err := pinnedImageRef(tt.image, tt.data, tt.probe)
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
			// pinned=false is what tells the caller step 2's output IS the
			// manifest, so step 3 can be skipped.
			if pinned != tt.wantPinned {
				t.Errorf("pinnedImageRef() pinned = %v, want %v", pinned, tt.wantPinned)
			}
		})
	}
}

// --- composer bindings -------------------------------------------------------

// detailStepReply is the canned four-step reply set every composer-binding test
// replays. It reads the same fixtures the parser tests do, so a fixture change
// breaks both layers together rather than leaving the bindings green against
// stale shapes.
func detailStepReply(t *testing.T) func(step detailStep, ref string) ([]byte, error) {
	t.Helper()
	index := readFixture(t, "imagetools_manifest_index_nginx.json")
	raw := readFixture(t, "imagetools_raw_manifest.json")
	config := readFixture(t, "imagetools_image_config_object.json")
	return func(step detailStep, ref string) ([]byte, error) {
		switch step {
		case stepLocal:
			return []byte(detailLocalProbeARM64), nil
		case stepIndex:
			return index, nil
		case stepRaw:
			return raw, nil
		case stepConfig:
			return config, nil
		}
		return nil, fmt.Errorf("unexpected docker call for %q", ref)
	}
}

// assertDetail checks the three UpdateDetail fields against the fixture values
// the happy path produces, so every binding test proves it wired the parsers
// through rather than merely returning a non-empty map.
func assertDetail(t *testing.T, label string, det UpdateDetail) {
	t.Helper()
	if !det.LocalCreated.Equal(detailWantLocalCreated) {
		t.Errorf("%s: LocalCreated = %v, want %v", label, det.LocalCreated, detailWantLocalCreated)
	}
	if det.NewID != detailNewID {
		t.Errorf("%s: NewID = %q, want %q", label, det.NewID, detailNewID)
	}
	if !det.NewCreated.Equal(detailWantNewCreated) {
		t.Errorf("%s: NewCreated = %v, want %v", label, det.NewCreated, detailWantNewCreated)
	}
}

const detailComposeConfig = `{"services":{"web":{"image":"nginx:1.27"},"db":{"image":"postgres:16"}}}`

// TestComposeUpdateDetails_SeamAndFilter pins the local binding: the four
// detail steps must reach `docker <verb>` directly, and only the requested
// service may be resolved. filterServices reads an empty slice as ALL services,
// so an unfiltered call would spend three registry round-trips per service in
// the project — the rate-limit failure the true-only gate exists to prevent.
func TestComposeUpdateDetails_SeamAndFilter(t *testing.T) {
	reply := detailStepReply(t)
	var argv [][]string

	c := New(t.TempDir())
	c.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		argv = append(argv, append([]string(nil), cmd.Args...))
		if slices.Contains(cmd.Args, "compose") {
			return []byte(detailComposeConfig), nil
		}
		return reply(classifyDetailCall(cmd.Args[1:]), cmd.Args[len(cmd.Args)-1])
	})

	out, err := c.UpdateDetails(context.Background(), []string{"web"})
	if err != nil {
		t.Fatalf("UpdateDetails() error = %v, want nil", err)
	}
	if len(out) != 1 {
		t.Fatalf("UpdateDetails() = %v, want exactly the requested service", out)
	}
	det, ok := out["web"]
	if !ok {
		t.Fatalf("UpdateDetails() = %v, want an entry for web", out)
	}
	assertDetail(t, "web", det)

	if len(argv) != 5 {
		t.Fatalf("made %d docker calls, want 5 (config + 4 steps): %v", len(argv), argv)
	}
	// The discovery call IS a compose subcommand; the four detail steps are
	// top-level docker commands and must never be routed through command().
	if !slices.Contains(argv[0], "compose") {
		t.Errorf("discovery call = %v, want the compose config subcommand", argv[0])
	}
	for _, args := range argv[1:] {
		if slices.Contains(args, "compose") {
			t.Errorf("detail argv %v must carry no compose element", args)
		}
		if args[0] != "docker" {
			t.Errorf("detail argv %v must invoke docker directly", args)
		}
	}
}

// TestComposeUpdateDetails_DiscoveryErrorPropagates pins the one error the
// binding does surface. A failed `docker compose config` yields no image map at
// all, so there is nothing partial to return.
func TestComposeUpdateDetails_DiscoveryErrorPropagates(t *testing.T) {
	c := New(t.TempDir())
	c.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		return nil, errors.New("no configuration file provided")
	})

	out, err := c.UpdateDetails(context.Background(), nil)
	if err == nil {
		t.Fatalf("UpdateDetails() = %v, want an error", out)
	}
	if out != nil {
		t.Errorf("UpdateDetails() = %v, want a nil map on discovery failure", out)
	}
}

// classifyRemoteDetailCall classifies one remote docker command string by the
// same markers classifyDetailCall reads. shellEscape only wraps each argument
// in single quotes and none of the four argv shapes contains one, so matching
// the escaped token is exact rather than a substring guess.
func classifyRemoteDetailCall(remote string) detailStep {
	if strings.Contains(remote, shellEscape("image")+" "+shellEscape("inspect")) {
		return stepLocal
	}
	markers := []struct {
		token string
		step  detailStep
	}{
		{"--raw", stepRaw},
		{manifestIndexFormat, stepIndex},
		{imageConfigFormat, stepConfig},
	}
	for _, m := range markers {
		if strings.Contains(remote, shellEscape(m.token)) {
			return m.step
		}
	}
	return stepUnknown
}

// TestRemoteUpdateDetails_SplicesSSHExtraArgsBeforeHost pins the remote binding
// against the repo-wide convention: SSHExtraArgs land immediately before the
// `--` separator that precedes the host argument, on EVERY call. Missing the
// splice on one of the four steps would send that step to the default port or
// without the CI identity file while the other three succeed.
func TestRemoteUpdateDetails_SplicesSSHExtraArgsBeforeHost(t *testing.T) {
	reply := detailStepReply(t)
	extras := []string{"-p", "2222"}
	host := "user@example.com"
	var argv [][]string

	r := &RemoteCompose{
		Host:         host,
		ProjectDir:   "/srv/app",
		SocketPath:   "/tmp/cdeploy-ctrl-abc-99",
		SSHExtraArgs: extras,
	}
	r.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		argv = append(argv, append([]string(nil), cmd.Args...))
		remote := cmd.Args[len(cmd.Args)-1]
		if strings.Contains(remote, "compose") {
			return []byte(detailComposeConfig), nil
		}
		return reply(classifyRemoteDetailCall(remote), remote)
	})

	out, err := r.UpdateDetails(context.Background(), []string{"web"})
	if err != nil {
		t.Fatalf("UpdateDetails() error = %v, want nil", err)
	}
	det, ok := out["web"]
	if !ok {
		t.Fatalf("UpdateDetails() = %v, want an entry for web", out)
	}
	assertDetail(t, "web", det)

	if len(argv) != 5 {
		t.Fatalf("made %d ssh calls, want 5 (config + 4 steps): %v", len(argv), argv)
	}
	for i, args := range argv {
		assertExtraBeforeHost(t, fmt.Sprintf("RemoteCompose.UpdateDetails call %d", i), args, host, extras)
	}
	for i, args := range argv[1:] {
		remote := args[len(args)-1]
		if strings.Contains(remote, "compose") {
			t.Errorf("detail call %d must NOT go through compose: %q", i+1, remote)
		}
		if !strings.HasPrefix(remote, "docker ") {
			t.Errorf("detail call %d = %q, want a top-level docker command", i+1, remote)
		}
	}
}

// hostPsDetailMixed exercises every entry hostImageMap drops: a compose-managed
// container, a bare image ID, and a container with no image reference at all.
// Only watchtower survives.
const hostPsDetailMixed = `{"ID":"aaa111222333","Names":"web","Image":"nginx:1.27","State":"running","Status":"Up 3 hours","Labels":"com.docker.compose.project=my-app"}
{"ID":"bbb444555666","Names":"watchtower","Image":"nginx:1.27","State":"running","Status":"Up 2 days","Labels":"org.opencontainers.image.title=watchtower"}
{"ID":"ccc777888999","Names":"scratch","Image":"9c7a54a9a43c","State":"running","Status":"Up 1 hour","Labels":""}
{"ID":"ddd000111222","Names":"noimage","Image":"","State":"running","Status":"Up 1 hour","Labels":""}`

// TestHostContainersUpdateDetails_DropsUnresolvableRefs pins the read-only
// binding: the image map comes from the Image field `docker ps` already
// returned, and the same two kinds of entry CheckUpdates drops are dropped here
// — a bare image ID names no repository a registry can be asked about, and an
// empty ref would make step 1 a malformed call.
func TestHostContainersUpdateDetails_DropsUnresolvableRefs(t *testing.T) {
	reply := detailStepReply(t)
	f := &fakeDockerRunner{runFunc: func(args []string) ([]byte, error) {
		if slices.Contains(args, "compose") {
			return nil, fmt.Errorf("host argv must carry no compose element: %v", args)
		}
		if len(args) >= 1 && args[0] == "ps" {
			return []byte(hostPsDetailMixed), nil
		}
		return reply(classifyDetailCall(args), args[len(args)-1])
	}}

	out, err := (&HostContainers{docker: f}).UpdateDetails(context.Background(), nil)
	if err != nil {
		t.Fatalf("UpdateDetails() error = %v, want nil", err)
	}
	if len(out) != 1 {
		t.Fatalf("UpdateDetails() = %v, want only the resolvable unmanaged container", out)
	}
	det, ok := out["watchtower"]
	if !ok {
		t.Fatalf("UpdateDetails() = %v, want an entry for watchtower", out)
	}
	assertDetail(t, "watchtower", det)

	if len(f.runCalls) != 5 {
		t.Fatalf("made %d docker calls, want 5 (ps + 4 steps): %v", len(f.runCalls), f.runCalls)
	}
	for _, args := range f.runCalls {
		if slices.Contains(args, "compose") {
			t.Errorf("host argv %v must carry no compose element", args)
		}
	}
}

// TestHostContainersUpdateDetails_DiscoveryErrorPropagates mirrors the Compose
// case: a failed `docker ps` yields no image map, so there is nothing partial
// to return.
func TestHostContainersUpdateDetails_DiscoveryErrorPropagates(t *testing.T) {
	f := &fakeDockerRunner{runErr: errors.New("Cannot connect to the Docker daemon")}

	out, err := (&HostContainers{docker: f}).UpdateDetails(context.Background(), nil)
	if err == nil {
		t.Fatalf("UpdateDetails() = %v, want an error", out)
	}
	if out != nil {
		t.Errorf("UpdateDetails() = %v, want a nil map on discovery failure", out)
	}
	if len(f.runCalls) != 1 {
		t.Errorf("made %d docker calls, want 1 — a failed discovery must not reach the registry", len(f.runCalls))
	}
}

// TestScanUpdateDetails_VisitsInSortedOrder pins the ordering rule directly
// rather than through the transport-abort test, which could pass on Go's map
// iteration order alone. Every image costs three registry round-trips, so which
// images are reached before an abort must be deterministic.
func TestScanUpdateDetails_VisitsInSortedOrder(t *testing.T) {
	wanted := map[string]string{"c-cache": "redis:7", "a-web": "nginx:1.27", "b-api": "alpine:3"}
	want := []string{"nginx:1.27", "alpine:3", "redis:7"}

	// Repeat so a map-order-dependent implementation cannot pass by luck.
	for i := 0; i < 50; i++ {
		f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
			// Fail at step 1 so each image costs exactly one recorded call.
			return nil, fmt.Errorf("no such image: %s", ref)
		})
		if _, err := scanUpdateDetails(context.Background(), wanted, f); err != nil {
			t.Fatalf("scanUpdateDetails() error = %v, want nil", err)
		}
		if got := stepRefs(f); !slices.Equal(got, want) {
			t.Fatalf("visit order = %v, want %v", got, want)
		}
	}
}

// TestScanUpdateDetails_StopsOnCancelledContext pins the early return. Without
// it a cancelled context turns every remaining d.run into a per-image failure,
// which only continues the loop — so the scan would spawn a doomed process for
// every image left. A superseded refresh is the normal way this ends.
func TestScanUpdateDetails_StopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := detailRunner(t, func(step detailStep, ref string) ([]byte, error) {
		t.Errorf("no docker call is allowed once the context is cancelled")
		return nil, nil
	})
	out, err := scanUpdateDetails(ctx, map[string]string{"web": "nginx:1.27", "api": "redis:7"}, f)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scanUpdateDetails() error = %v, want it to wrap context.Canceled", err)
	}
	if len(out) != 0 {
		t.Errorf("scanUpdateDetails() = %v, want no entries", out)
	}
}

// TestComposeUpdateDetails_UnknownServiceCostsNothing pins the filter for a
// verdict naming a service the compose file no longer carries: it resolves to
// no image, so the scan must reach the registry zero times rather than reading
// an empty filter result as "all services".
func TestComposeUpdateDetails_UnknownServiceCostsNothing(t *testing.T) {
	var argv [][]string
	c := New("/srv/app")
	c.SetStandalone(false)
	c.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		argv = append(argv, append([]string(nil), cmd.Args...))
		return []byte(detailComposeConfig), nil
	})

	out, err := c.UpdateDetails(context.Background(), []string{"ghost"})
	if err != nil {
		t.Fatalf("UpdateDetails() error = %v, want nil", err)
	}
	if len(out) != 0 {
		t.Errorf("UpdateDetails() = %v, want no entries", out)
	}
	if len(argv) != 1 {
		t.Errorf("made %d docker calls, want 1 (the discovery call only): %v", len(argv), argv)
	}
}

// TestRemoteUpdateDetails_DiscoveryErrorPropagates mirrors the Compose and
// HostContainers cases: a failed `docker compose config` yields no image map,
// so there is nothing partial to return and no registry call may follow.
func TestRemoteUpdateDetails_DiscoveryErrorPropagates(t *testing.T) {
	var calls int
	r := &RemoteCompose{Host: "user@example.com", ProjectDir: "/srv/app"}
	r.SetTestHooks(nil, func(cmd *exec.Cmd) ([]byte, error) {
		calls++
		return nil, errors.New("ssh: connect to host example.com port 22: Connection refused")
	})

	out, err := r.UpdateDetails(context.Background(), []string{"web"})
	if err == nil {
		t.Fatalf("UpdateDetails() = %v, want an error", out)
	}
	if out != nil {
		t.Errorf("UpdateDetails() = %v, want a nil map on discovery failure", out)
	}
	if calls != 1 {
		t.Errorf("made %d ssh calls, want 1 — a failed discovery must not reach the registry", calls)
	}
}

// hostPsDetailTwo carries two resolvable unmanaged containers, so the services
// argument is the only thing that can narrow the scan.
const hostPsDetailTwo = `{"ID":"bbb444555666","Names":"watchtower","Image":"nginx:1.27","State":"running","Status":"Up 2 days","Labels":"org.opencontainers.image.title=watchtower"}
{"ID":"eee333444555","Names":"portainer","Image":"redis:7","State":"running","Status":"Up 5 days","Labels":""}`

// TestHostContainersUpdateDetails_HonoursServiceFilter pins the filter on the
// read-only binding. The unmanaged screen is the worst case for the registry
// quota — every `docker ps -a` container contributes an image, and each image
// costs three registry calls — so the filter is the only guard.
func TestHostContainersUpdateDetails_HonoursServiceFilter(t *testing.T) {
	reply := detailStepReply(t)
	f := &fakeDockerRunner{runFunc: func(args []string) ([]byte, error) {
		if len(args) >= 1 && args[0] == "ps" {
			return []byte(hostPsDetailTwo), nil
		}
		if strings.Contains(args[len(args)-1], "redis") {
			t.Errorf("redis:7 must not be reached: UpdateDetails asked for watchtower only")
		}
		return reply(classifyDetailCall(args), args[len(args)-1])
	}}

	out, err := (&HostContainers{docker: f}).UpdateDetails(context.Background(), []string{"watchtower"})
	if err != nil {
		t.Fatalf("UpdateDetails() error = %v, want nil", err)
	}
	if len(out) != 1 {
		t.Fatalf("UpdateDetails() = %v, want only the requested container", out)
	}
	if _, ok := out["watchtower"]; !ok {
		t.Errorf("UpdateDetails() = %v, want an entry for watchtower", out)
	}
	if len(f.runCalls) != 5 {
		t.Errorf("made %d docker calls, want 5 (ps + 4 steps): %v", len(f.runCalls), f.runCalls)
	}
}

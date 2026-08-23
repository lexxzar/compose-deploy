package compose

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Update-detail parsers and helpers.
//
// CheckUpdates answers "is there a newer image?" with a bool. This file answers
// the two follow-up questions the inspect screen asks once that verdict is
// true: WHICH image is waiting (its config digest, which equals the docker
// image ID it will get once pulled) and WHEN each side was built.
//
// Every command here is a TOP-LEVEL docker command, never a compose
// subcommand, so all of them go through the dockerRunner seam exactly as
// compareImageDigestVia does — routing one through command() / remoteCommand()
// would produce a malformed `docker compose image inspect` argv.

// UpdateDetail carries the extra IMAGE-section rows the inspect screen draws
// beside the "⇧" verdict. Every field is optional: a zero LocalCreated or
// NewCreated, or an empty NewID, means "unknown" and the matching row is
// omitted rather than guessed. Only fields a row actually draws live here — a
// parsed-but-unrendered field is a promise the screen does not keep.
type UpdateDetail struct {
	LocalCreated time.Time // build time of the running image; zero = unknown/sentinel
	NewID        string    // config digest of the registry image = its docker image ID
	NewCreated   time.Time // build time of the registry image; zero = unknown/sentinel
}

// localProbe is the parsed result of step 1 of the detail fetch: the build time
// of the local image plus the platform pair used to select the host's entry out
// of a multi-arch index.
type localProbe struct {
	created time.Time
	os      string
	arch    string
}

// localProbeFormat is the Go template step 1 hands `docker image inspect`.
//
// `.Variant` is DELIBERATELY absent. A template referencing a field the docker
// struct lacks is a hard execution error, not an empty string (the same policy
// hostPsArgs documents), so an older docker host would fail step 1 for every
// image and silently disable the whole feature. Variant is only ever a
// tie-breaker in the index match, so the local side treats it as always-empty.
// `.Id` is absent for the same "only what a row draws" reason — the image ID
// row already comes from the container's own inspect document.
const localProbeFormat = "{{.Created}}|{{.Os}}|{{.Architecture}}"

// localProbeArgs builds the step-1 argv: the build timestamp and platform pair
// of the LOCAL image. This is the only step that does not touch the registry.
func localProbeArgs(image string) []string {
	return []string{"image", "inspect", "--format", localProbeFormat, image}
}

// parseLocalProbe parses the single line localProbeArgs produces.
//
// An unparseable or epoch-sentinel timestamp yields a ZERO created time rather
// than an error: reproducible builds (distroless, ko, Bazel, nix) legitimately
// report 1970-01-01, and the platform pair beside it is still usable, so the
// scan continues and only the `built` row drops.
//
// A wrong field count, or an empty os/arch, IS an error. Without a platform
// pair the index match can only fail, and erroring here saves the three
// registry round-trips that would follow.
func parseLocalProbe(out []byte) (localProbe, error) {
	line := strings.TrimSpace(string(out))
	if line == "" {
		return localProbe{}, fmt.Errorf("empty local image probe output")
	}
	parts := strings.Split(line, "|")
	if len(parts) != 3 {
		return localProbe{}, fmt.Errorf("unexpected local image probe output: %q", line)
	}
	p := localProbe{
		os:   strings.TrimSpace(parts[1]),
		arch: strings.TrimSpace(parts[2]),
	}
	if p.os == "" || p.arch == "" {
		return localProbe{}, fmt.Errorf("local image probe has no platform: %q", line)
	}
	p.created = parseImageTimestamp(strings.TrimSpace(parts[0]))
	return p, nil
}

// parseImageTimestamp parses an RFC3339Nano image build timestamp, returning
// the zero time for anything it cannot trust. The `Unix() <= 0` guard covers
// the 1970-01-01 reproducible-build sentinel, which is a placeholder rather
// than data; the TUI's formatInspectTimeValue applies the same guard so the
// row is omitted on either path.
func parseImageTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	if t.Unix() <= 0 {
		return time.Time{}
	}
	return t
}

// manifestIndexFormat is the Go template step 2 hands `docker buildx imagetools
// inspect`.
//
// The `--format` flag is only SELECTIVELY usable on this command. Verified on
// Docker 29.1.3 / buildx v0.30.1-desktop.1: `{{json .Manifest}}` and
// `{{json .Image}}` substitute correctly, while the documented
// `{{.Manifest.Digest}}` silently falls through and prints the human block
// instead — the regression imagetoolsInspectArgs already carries a pin
// against. Every parser here therefore validates strictly and returns "absent"
// rather than a guess, so a future fall-through drops rows instead of drawing
// wrong ones.
const manifestIndexFormat = "{{json .Manifest}}"

// manifestIndexArgs builds the step-2 argv: the registry index (or the single
// manifest) of an image, as JSON. Top-level docker command, so it bypasses
// command() / remoteCommand() like every other call in this file.
func manifestIndexArgs(image string) []string {
	return []string{"buildx", "imagetools", "inspect", "--format", manifestIndexFormat, image}
}

// indexPlatform is the platform descriptor of one entry in a manifest index.
type indexPlatform struct {
	OS      string `json:"os"`
	Arch    string `json:"architecture"`
	Variant string `json:"variant"`
}

// indexManifest is one entry of a manifest index. Only the two fields a caller
// acts on are decoded; mediaType, size and annotations are deliberately absent.
type indexManifest struct {
	Digest   string        `json:"digest"`
	Platform indexPlatform `json:"platform"`
}

// unknownPlatform is the architecture/os an attestation manifest carries. A
// real multi-arch index interleaves them with the platform entries — 8 of
// nginx's 16 — and they must never be selected: their digest addresses a
// provenance/SBOM blob, not a runnable image.
const unknownPlatform = "unknown"

// parseIndexPlatformDigest picks the manifest digest for one platform out of
// the JSON manifestIndexArgs returns. The three-state return distinguishes two
// failures a (string, bool) pair would conflate:
//
//	hasIndex=false               no manifests key — a single-manifest ref, so
//	                             the caller keeps the ORIGINAL ref for the
//	                             remaining steps
//	hasIndex=true, found=false   abort this image
//	hasIndex=true, found=true    pin to the returned digest and continue
//
// Conflating the middle state with the first is the silent failure mode: the
// --raw step would then run against the index (no config.digest, so NewID
// comes back empty) and the config step would return the platform-keyed map,
// from which the code could pick a platform that is not the host's.
//
// hasIndex=false is returned ONLY for a well-formed document that has no
// manifests key. Malformed JSON, or a manifests value that is not a list,
// returns the abort state instead — every doubt fails closed.
//
// Variant is a tie-breaker, never a requirement: the local probe cannot report
// one (localProbeFormat omits .Variant deliberately), so an unqualified entry
// wins when both forms are present and index order decides otherwise.
func parseIndexPlatformDigest(data []byte, os, arch string) (digest string, hasIndex bool, found bool) {
	if os == "" || arch == "" {
		return "", true, false
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", true, false
	}
	rawList, ok := doc["manifests"]
	if !ok {
		return "", false, false
	}
	var entries []indexManifest
	if err := json.Unmarshal(rawList, &entries); err != nil {
		return "", true, false
	}
	var fallback string
	for _, e := range entries {
		if e.Platform.OS == unknownPlatform || e.Platform.Arch == unknownPlatform {
			continue
		}
		if !strings.EqualFold(e.Platform.OS, os) || !strings.EqualFold(e.Platform.Arch, arch) {
			continue
		}
		dg := validImagetoolsDigest(e.Digest)
		if dg == "" {
			continue
		}
		if e.Platform.Variant == "" {
			return dg, true, true
		}
		if fallback == "" {
			fallback = dg
		}
	}
	if fallback != "" {
		return fallback, true, true
	}
	return "", true, false
}

// validImagetoolsDigest returns the normalized digest when s is EXACTLY a
// canonical sha256:<64 hex> string, and "" otherwise. The whole-string check
// (rather than a substring search) is what keeps a fallen-through --format
// line from being mistaken for a digest.
func validImagetoolsDigest(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if imagetoolsDigestRE.FindString(s) != s {
		return ""
	}
	return "sha256:" + strings.ToLower(s[len("sha256:"):])
}

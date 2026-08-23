package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// rawManifestArgs builds the step-3 argv: the RAW manifest bytes of a pinned
// image ref, whose config.digest is the docker image ID the image gets once it
// is pulled. `--raw` is used rather than a template because the digest lives on
// the manifest document itself, not on anything the --format tree exposes.
func rawManifestArgs(image string) []string {
	return []string{"buildx", "imagetools", "inspect", "--raw", image}
}

// imageConfigFormat is the Go template step 4 hands `docker buildx imagetools
// inspect`. Like manifestIndexFormat this is one of the two forms verified to
// substitute on buildx v0.30.1; the dotted-field forms fall through to the
// human block.
const imageConfigFormat = "{{json .Image}}"

// imageConfigArgs builds the step-4 argv: the OCI image config of an image, as
// JSON. On a PINNED ref this returns a bare config object and costs ~1.2s; on a
// bare tag it returns a platform-keyed map and costs ~4.3s, so callers pin
// whenever step 2 gave them a digest.
func imageConfigArgs(image string) []string {
	return []string{"buildx", "imagetools", "inspect", "--format", imageConfigFormat, image}
}

// rawManifestDoc decodes only the config descriptor of a raw manifest. The
// layers, annotations and mediaType are deliberately absent — nothing draws
// them.
type rawManifestDoc struct {
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
}

// parseConfigDigest reads config.digest out of the raw manifest bytes
// rawManifestArgs returns. That digest is the registry image's config digest,
// which equals the docker image ID the local daemon assigns after a pull — so
// it is directly comparable with the inspect screen's `image id` row.
//
// Anything it cannot validate as a canonical sha256 digest yields "": an index
// document (which has no config key), a fallen-through --format line, and
// malformed JSON all take that path, and the `update id` row is then omitted
// rather than drawn wrong.
func parseConfigDigest(raw []byte) string {
	var doc rawManifestDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return validImagetoolsDigest(doc.Config.Digest)
}

// imageConfigDoc decodes only the build timestamp of an OCI image config.
type imageConfigDoc struct {
	Created string `json:"created"`
}

// parseImageCreated reads the build timestamp out of the JSON imageConfigArgs
// returns, accepting BOTH shapes that command produces:
//
//	bare object          a pinned ref returns the OCI image config directly
//	platform-keyed map   a bare tag returns {"linux/amd64": {...}, ...}
//
// Both must be handled because a single-arch ref reaches step 4 unpinned — the
// index step returns hasIndex=false for it, so there is no digest to pin with.
//
// The bare-object form is trusted without a platform cross-check: it is only
// reached for a ref that resolves to exactly one manifest, so its config is the
// only one there is. The map form IS matched on the requested platform, with
// variant as a tie-breaker exactly as parseIndexPlatformDigest applies it —
// an unqualified `os/arch` key wins, and among variant-qualified keys the
// lexicographically first wins, since Go map iteration order is not stable.
//
// The false return covers every doubt: malformed JSON, a shape that is neither
// form, an absent platform, and the 1970-01-01 reproducible-build sentinel that
// parseImageTimestamp rejects.
func parseImageCreated(data []byte, os, arch string) (time.Time, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil || len(doc) == 0 {
		return time.Time{}, false
	}
	raw, ok := pickImageConfig(doc, data, os, arch)
	if !ok {
		return time.Time{}, false
	}
	var cfg imageConfigDoc
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return time.Time{}, false
	}
	t := parseImageTimestamp(strings.TrimSpace(cfg.Created))
	if t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}

// imageConfigKeys are top-level fields only an OCI image config carries. A
// platform-keyed map's keys always contain a "/" instead, which is the primary
// discriminator; this set is the fail-closed second opinion, so a document that
// is neither shape returns "absent" rather than being read as one of them.
var imageConfigKeys = []string{"created", "architecture", "rootfs", "config", "history"}

// pickImageConfig returns the raw config object for the requested platform out
// of either shape parseImageCreated accepts, or ok=false when the document is
// neither shape or the platform is absent.
func pickImageConfig(doc map[string]json.RawMessage, data []byte, os, arch string) (json.RawMessage, bool) {
	platformKeyed := false
	for k := range doc {
		if strings.Contains(k, "/") {
			platformKeyed = true
			break
		}
	}
	if !platformKeyed {
		for _, k := range imageConfigKeys {
			if _, ok := doc[k]; ok {
				return data, true
			}
		}
		return nil, false
	}
	if os == "" || arch == "" {
		return nil, false
	}
	var qualified []string
	for k := range doc {
		exact, match := platformKeyMatches(k, os, arch)
		if !match {
			continue
		}
		if exact {
			return doc[k], true
		}
		qualified = append(qualified, k)
	}
	if len(qualified) == 0 {
		return nil, false
	}
	sort.Strings(qualified)
	return doc[qualified[0]], true
}

// platformKeyMatches reports whether a platform map key names the requested
// os/arch, and whether it does so without a variant suffix. An entry whose os
// or architecture is "unknown" is an attestation and never matches.
func platformKeyMatches(key, os, arch string) (exact, match bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 && len(parts) != 3 {
		return false, false
	}
	if parts[0] == unknownPlatform || parts[1] == unknownPlatform {
		return false, false
	}
	if !strings.EqualFold(parts[0], os) || !strings.EqualFold(parts[1], arch) {
		return false, false
	}
	return len(parts) == 2, true
}

// updateDetailResult is one image's memoized detail outcome inside
// scanUpdateDetails. A non-nil err means the image contributed no entry; the
// error is kept rather than discarded so a transport failure can still abort
// the batch when it arrives from the memo cache.
type updateDetailResult struct {
	detail UpdateDetail
	err    error
}

// scanUpdateDetails resolves the extra IMAGE-section values for every service
// in wanted, mirroring scanImageUpdates: distinct images are fetched ONCE, and
// a service whose image could not be resolved stays ABSENT from the map so the
// inspect screen omits the rows rather than drawing a guess.
//
// Unlike scanImageUpdates there are no systemic-failure cascades here. The
// glyph is the load-bearing signal and these rows are a bonus, so a partial
// result is a valid result and the caller discards the error entirely rather
// than letting it reach updatesMsg.err — a registry 429 during this phase must
// not blank the "⇧" column.
//
// The transport abort is the one early return: an errSSHTransport failure
// poisons every remaining image, so the scan stops and returns the partial map
// ALONGSIDE the error, matching the untrusted-partial-map contract
// scanImageUpdates follows.
//
// Services are visited in sorted order. Each image costs four round-trips
// (three of them to the registry), so which images are reached before a
// transport abort must not depend on Go's map iteration order.
func scanUpdateDetails(ctx context.Context, wanted map[string]string, d dockerRunner) (map[string]UpdateDetail, error) {
	out := make(map[string]UpdateDetail, len(wanted))
	svcs := make([]string, 0, len(wanted))
	for svc := range wanted {
		svcs = append(svcs, svc)
	}
	sort.Strings(svcs)
	seen := make(map[string]updateDetailResult, len(wanted))
	for _, svc := range svcs {
		img := wanted[svc]
		r, cached := seen[img]
		if !cached {
			det, err := fetchUpdateDetail(ctx, d, img)
			r = updateDetailResult{detail: det, err: err}
			seen[img] = r
		}
		if r.err != nil {
			if errors.Is(r.err, errSSHTransport) {
				return out, fmt.Errorf("update detail transport failure: %w", r.err)
			}
			continue
		}
		out[svc] = r.detail
	}
	return out, nil
}

// fetchUpdateDetail runs the four-step sequence for one image reference:
//
//	1  docker image inspect                  local build time + platform pair
//	2  buildx imagetools inspect --format     the index; select the host platform
//	3  buildx imagetools inspect --raw        config.digest → NewID
//	4  buildx imagetools inspect --format     created → NewCreated
//
// Steps 2-4 are registry calls, so the sequence returns at the FIRST failure
// rather than pressing on: the caller omits the whole entry either way, and a
// wasted round-trip here is a step closer to the registry's anonymous rate
// limit. Step 2's three-state answer decides which reference the last two steps
// address — the pinned one when the index named the host's platform, the
// original when there is no index at all, and none at all when an index exists
// but the host's platform is missing from it.
//
// A step that SUCCEEDS but whose parser returns "absent" is not a failure: the
// matching field stays zero and its row is omitted, while the rows the other
// steps filled still render.
func fetchUpdateDetail(ctx context.Context, d dockerRunner, image string) (UpdateDetail, error) {
	localOut, err := d.run(ctx, localProbeArgs(image)...)
	if err != nil {
		return UpdateDetail{}, fmt.Errorf("inspecting local image %q: %w", image, err)
	}
	probe, err := parseLocalProbe(localOut)
	if err != nil {
		return UpdateDetail{}, fmt.Errorf("inspecting local image %q: %w", image, err)
	}

	indexOut, err := d.run(ctx, manifestIndexArgs(image)...)
	if err != nil {
		return UpdateDetail{}, fmt.Errorf("fetching manifest index for %q: %w", image, err)
	}
	ref, err := pinnedImageRef(image, indexOut, probe)
	if err != nil {
		return UpdateDetail{}, err
	}

	rawOut, err := d.run(ctx, rawManifestArgs(ref)...)
	if err != nil {
		return UpdateDetail{}, fmt.Errorf("fetching raw manifest for %q: %w", ref, err)
	}
	configOut, err := d.run(ctx, imageConfigArgs(ref)...)
	if err != nil {
		return UpdateDetail{}, fmt.Errorf("fetching image config for %q: %w", ref, err)
	}

	det := UpdateDetail{LocalCreated: probe.created, NewID: parseConfigDigest(rawOut)}
	if created, ok := parseImageCreated(configOut, probe.os, probe.arch); ok {
		det.NewCreated = created
	}
	return det, nil
}

// pinnedImageRef turns step 2's three-state answer into the reference steps 3
// and 4 address. StripTag is reused rather than re-deriving the repo portion,
// so a registry port (localhost:5000/foo:1) survives the rewrite.
func pinnedImageRef(image string, indexOut []byte, probe localProbe) (string, error) {
	digest, hasIndex, found := parseIndexPlatformDigest(indexOut, probe.os, probe.arch)
	switch {
	case !hasIndex:
		// Single-manifest reference: there is nothing to pin to, and the
		// original ref already addresses the only manifest there is.
		return image, nil
	case !found:
		return "", fmt.Errorf("no %s/%s manifest in the index for %q", probe.os, probe.arch, image)
	default:
		return StripTag(image) + "@" + digest, nil
	}
}

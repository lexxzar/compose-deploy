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
// of the local image plus the platform triple used to select the host's entry
// out of a multi-arch index.
type localProbe struct {
	created time.Time
	os      string
	arch    string
	variant string // "" when the image config names none — unknown, never a requirement
}

// platform renders the probe's platform triple the way an index descriptor
// spells it — linux/arm/v7, or linux/amd64 when the architecture carries no
// variant. The variant is the NORMALISED one, so the "no ... manifest in the
// index" error names the platform that was actually searched for: an arm image
// config carrying no variant was looked up as linux/arm/v7.
func (p localProbe) platform() string {
	s := p.os + "/" + p.arch
	if v := normalizePlatformVariant(p.arch, p.variant); v != "" {
		s += "/" + v
	}
	return s
}

// localProbeFormat is the Go template step 1 hands `docker image inspect`.
//
// The WHOLE DOCUMENT is requested rather than a pipe-joined list of NAMED
// fields, and that is what makes the variant safe to read. Naming a field the
// CLI's struct lacks is a hard execution error: the CLI retries such a template
// against the raw JSON with missingkey=error, and `Variant` is omitempty, so an
// image without one carries no such key and the retry fails too — one unknown
// field would kill step 1 for EVERY image and silently disable the whole
// feature. `{{json .}}` cannot fail that way: a field an older CLI does not
// know simply never appears, and an absent variant parses as the empty string
// the index match already treats as "unknown". hostPsArgs documents the same
// rule for `docker ps`, and it is the reason the matcher can be strict.
//
// Nothing else the document carries is read — `Id` in particular, because the
// `image id` row already comes from the container's own inspect document.
const localProbeFormat = "{{json .}}"

// localProbeDoc decodes the four fields step 1 reads. Go matches JSON keys
// case-insensitively, so this survives a CLI that lower-cases them.
type localProbeDoc struct {
	Created      string `json:"Created"`
	OS           string `json:"Os"`
	Architecture string `json:"Architecture"`
	Variant      string `json:"Variant"`
}

// localProbeArgs builds the step-1 argv: the build timestamp and platform
// triple of the LOCAL image. This is the only step that does not touch the
// registry.
func localProbeArgs(image string) []string {
	return []string{"image", "inspect", "--format", localProbeFormat, image}
}

// parseLocalProbe parses the image document localProbeArgs asks for.
//
// An unparseable or epoch-sentinel timestamp yields a ZERO created time rather
// than an error: reproducible builds (distroless, ko, Bazel, nix) legitimately
// report 1970-01-01, and the platform triple beside it is still usable, so the
// scan continues and only the `built` row drops. An ABSENT variant is ordinary
// data, not a failure — real arm64 images split both ways (library/nginx records
// v8, qdrant/qdrant records none) — so it is never required;
// normalizePlatformVariant resolves what it MEANS at match time, where an
// absent arm variant is v7 and an absent arm64 one is v8.
//
// Unparseable output, and an empty os or architecture, ARE errors. Without a
// platform pair the index match can only fail, and erroring here saves the
// three registry round-trips that would follow.
func parseLocalProbe(out []byte) (localProbe, error) {
	var doc localProbeDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return localProbe{}, fmt.Errorf("unexpected local image probe output: %w", err)
	}
	p := localProbe{
		os:      strings.TrimSpace(doc.OS),
		arch:    strings.TrimSpace(doc.Architecture),
		variant: strings.TrimSpace(doc.Variant),
	}
	if p.os == "" || p.arch == "" {
		return localProbe{}, fmt.Errorf("local image probe has no platform: %q", strings.TrimSpace(string(out)))
	}
	p.created = parseImageTimestamp(strings.TrimSpace(doc.Created))
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

// normalizePlatformVariant canonicalises a platform's CPU variant, so the two
// spellings of one platform compare equal and two different platforms never do.
//
// The OCI image-spec makes `variant` an OPTIONAL descriptor property and
// standardises only four values (image-index.md, "Platform Variants": arm/v6,
// arm/v7, arm/v8, arm64/v8), so the spec itself never says what an ABSENT
// variant means. containerd answers that, and docker and buildx inherit the
// answer through it — platforms/database.go, normalizeArch:
//
//	arm    with ""  or "7"   =>  v7        an unqualified linux/arm IS ARMv7
//	arm    with "5"/"6"/"8"  =>  v5/v6/v8
//	arm64  with "8" or "v8"  =>  ""        arm64 and arm64/v8 are one platform
//
// The first line is the load-bearing one: an unqualified `linux/arm` descriptor
// is not a wildcard over every 32-bit ARM host, so an ARMv5 or ARMv6 host must
// not select it. The third makes library/nginx's single `arm64/v8` entry an
// EXACT match for an arm64 image config that records no variant.
//
// Only that arm/arm64 pair is folded. The architecture ALIASES containerd also
// normalises (aarch64, x86_64, i386) cannot appear on either side here: both
// the descriptor and the local probe come from docker's own JSON, which spells
// the architecture as a Go GOARCH value.
func normalizePlatformVariant(arch, variant string) string {
	arch = strings.ToLower(strings.TrimSpace(arch))
	variant = strings.ToLower(strings.TrimSpace(variant))
	switch arch {
	case "arm":
		switch variant {
		case "", "7":
			return "v7"
		case "5", "6", "8":
			return "v" + variant
		}
	case "arm64":
		switch variant {
		case "8", "v8":
			return ""
		}
	}
	return variant
}

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
// hasIndex=false is returned ONLY for a well-formed document that carries keys
// but no manifests key. Malformed JSON, a manifests value that is not a list,
// and a document with no keys at all (`null` unmarshals into a nil map, `{}`
// into an empty one) return the abort state instead — every doubt fails closed,
// and an empty document describes no manifest to fall through to.
//
// The variant is matched, not guessed — and both sides are CANONICALISED
// first, through normalizePlatformVariant. Step 1 reports the local image's own
// variant, so the entry whose normalised variant EQUALS the probe's normalised
// one wins. That covers the ordinary case where both are empty, the arm host
// whose image config names no variant (both sides normalise to v7), and
// library/nginx's single linux/arm64/v8 entry read by a config that records
// none (both sides normalise to the empty variant).
//
// ONE fallback survives that: a lone entry whose normalised variant is EMPTY
// claims the whole architecture, so it is taken whatever the local image says
// — portainer and qdrant publish linux/arm64 unqualified, and a host reporting
// a newer variant still runs the baseline image. It can no longer fire for
// linux/arm at all, where an unqualified entry normalises to v7 and is
// therefore an exact match or a mismatch, never a wildcard.
//
// Everything else aborts, and the ABORT is the point: linux/arm ships as v5+v7
// (nginx) or v6+v7 (postgres), so a KNOWN local variant with no entry of its own
// — an ARMv6 host offered only v5 and v7 — must draw nothing rather than a
// digest and a build date belonging to an image the host will never run. That is
// the same silent-wrong-value the three-state return exists to prevent, one
// level finer, and it is worse than a blank row because a blank row is honest.
// The mirror rule went the same way: a lone VARIANT-QUALIFIED entry is no longer
// accepted for a probe that names no variant, because after normalisation such
// a probe is a concrete platform (arm ⇒ v7, arm64 ⇒ v8) rather than "unknown",
// so taking an arm64/v9 or amd64/v3 entry for it would be this same bug one
// variant further out.
func parseIndexPlatformDigest(data []byte, probe localProbe) (digest string, hasIndex bool, found bool) {
	if probe.os == "" || probe.arch == "" {
		return "", true, false
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil || len(doc) == 0 {
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
	want := normalizePlatformVariant(probe.arch, probe.variant)
	var unqualified []string
	for _, e := range entries {
		if e.Platform.OS == unknownPlatform || e.Platform.Arch == unknownPlatform {
			continue
		}
		if !strings.EqualFold(e.Platform.OS, probe.os) || !strings.EqualFold(e.Platform.Arch, probe.arch) {
			continue
		}
		dg := validImagetoolsDigest(e.Digest)
		if dg == "" {
			continue
		}
		// Both sides go through the same fold, which is also what makes the
		// comparison case-insensitive: normalizePlatformVariant lower-cases.
		v := normalizePlatformVariant(e.Platform.Arch, e.Platform.Variant)
		if v == want {
			return dg, true, true
		}
		if v == "" {
			unqualified = append(unqualified, dg)
		}
	}
	// Reached only when no entry named the host's platform exactly. A single
	// architecture-wide entry still answers for it; two would describe one
	// platform twice, which is a malformed index and a doubt like any other.
	if len(unqualified) == 1 {
		return unqualified[0], true, true
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
// returns.
//
// Only the BARE OBJECT shape is read. Step 4 addresses either a
// platform-pinned repo@digest ref or a ref the index step reported as
// single-manifest, and both resolve to exactly one manifest, so buildx returns
// the OCI image config directly. The platform-keyed map it returns for a bare
// multi-platform tag cannot reach here: such a tag always has an index, so it
// is pinned before step 4 or aborted at step 2.
//
// No platform cross-check is applied for the same reason: the config is the
// only one the ref resolves to, and rejecting it on a mismatch would drop the
// row for every legitimate single-arch image.
//
// The false return covers every doubt: malformed JSON, any other shape (a
// platform-keyed map decodes cleanly with an empty created, so it takes this
// path too), and the 1970-01-01 reproducible-build sentinel that
// parseImageTimestamp rejects.
func parseImageCreated(data []byte) (time.Time, bool) {
	var cfg imageConfigDoc
	if err := json.Unmarshal(data, &cfg); err != nil {
		return time.Time{}, false
	}
	t := parseImageTimestamp(strings.TrimSpace(cfg.Created))
	if t.IsZero() {
		return time.Time{}, false
	}
	return t, true
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
		// A cancelled context makes every remaining d.run fail, and a per-image
		// failure only continues the loop — so without this the scan would spawn
		// a doomed process for every image left. A superseded refresh is the
		// normal way this ends.
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("update detail scan stopped: %w", err)
		}
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

// fetchUpdateDetail runs the per-image sequence for one image reference:
//
//	1  docker image inspect                local build time + platform triple
//	2  buildx imagetools inspect --format  the index; select the host platform
//	3  buildx imagetools inspect --raw     config.digest → NewID; SKIPPED when
//	                                       step 2 already returned the manifest
//	4  buildx imagetools inspect --format  created → NewCreated
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
	ref, pinned, err := pinnedImageRef(image, indexOut, probe)
	if err != nil {
		return UpdateDetail{}, err
	}

	// On the single-manifest path step 2 ALREADY returned the manifest, so its
	// config.digest is in hand and step 3 would re-fetch it. Falling through to
	// step 3 when that yields nothing keeps the row on a buildx whose
	// {{json .Manifest}} does not carry the config descriptor.
	newID := ""
	if !pinned {
		newID = parseConfigDigest(indexOut)
	}
	if newID == "" {
		rawOut, err := d.run(ctx, rawManifestArgs(ref)...)
		if err != nil {
			return UpdateDetail{}, fmt.Errorf("fetching raw manifest for %q: %w", ref, err)
		}
		newID = parseConfigDigest(rawOut)
	}

	configOut, err := d.run(ctx, imageConfigArgs(ref)...)
	if err != nil {
		return UpdateDetail{}, fmt.Errorf("fetching image config for %q: %w", ref, err)
	}

	det := UpdateDetail{LocalCreated: probe.created, NewID: newID}
	if created, ok := parseImageCreated(configOut); ok {
		det.NewCreated = created
	}
	return det, nil
}

// pinnedImageRef turns step 2's three-state answer into the reference steps 3
// and 4 address. stripTag is reused rather than re-deriving the repo portion,
// so a registry port (localhost:5000/foo:1) survives the rewrite. pinned is
// false on the single-manifest path, which tells the caller step 2's own output
// is the manifest and step 3 can be skipped.
func pinnedImageRef(image string, indexOut []byte, probe localProbe) (ref string, pinned bool, err error) {
	digest, hasIndex, found := parseIndexPlatformDigest(indexOut, probe)
	switch {
	case !hasIndex:
		// Single-manifest reference: there is nothing to pin to, and the
		// original ref already addresses the only manifest there is.
		return image, false, nil
	case !found:
		return "", false, fmt.Errorf("no %s manifest in the index for %q", probe.platform(), image)
	default:
		return stripTag(image) + "@" + digest, true, nil
	}
}

// fetchImageCreated returns the build time of a LOCAL image: step 1 of the
// detail fetch on its own, with steps 2-4 (the registry half) left out.
//
// The inspect screen's `built` row describes the image the container is running
// right now, so it must not depend on an update verdict, on the detail batch
// having been dispatched, or on a registry being reachable. Step 1 is purely
// local — one `docker image inspect` — which is why it can be reused verbatim
// here instead of a second parser: localProbeFormat names NO field, and an
// unknown field would be a hard template error.
//
// A top-level docker command, so it goes through the dockerRunner seam and
// never through command() / remoteCommand(), which would build a malformed
// `docker compose image inspect` argv.
//
// parseLocalProbe's platform requirement is inherited rather than relaxed: a
// document with no os/architecture is malformed for either caller, and every
// failure here is discarded by the caller anyway — the row is omitted and the
// rest of the container document still renders.
func fetchImageCreated(ctx context.Context, d dockerRunner, image string) (time.Time, error) {
	if strings.TrimSpace(image) == "" {
		return time.Time{}, errors.New("no image reference to inspect")
	}
	out, err := d.run(ctx, localProbeArgs(image)...)
	if err != nil {
		return time.Time{}, fmt.Errorf("inspecting local image %q: %w", image, err)
	}
	probe, err := parseLocalProbe(out)
	if err != nil {
		return time.Time{}, fmt.Errorf("inspecting local image %q: %w", image, err)
	}
	return probe.created, nil
}

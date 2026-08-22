package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Update-detection parsers and helpers.
//
// This package always uses per-image digest comparison via
// `docker image inspect` (local) and `docker manifest inspect` (registry).
// The Compose v2.22+ `docker compose pull --dry-run` path was explored and
// rejected: with `--policy=always` every non-build service reports "Pulling"
// regardless of currency (defeating the purpose); with `--policy=missing` the
// signal is "image present locally" rather than "image current in registry",
// producing false negatives for the most common scenario (deployed yesterday,
// registry has a new tag today). The manifest-inspect path is authoritative
// and the per-image SSH round-trip cost is acceptable for a per-screen-entry
// fetch with a 10-minute TTL cache.
//
// UpdateGlyph is the shared rune for the inline "update available" indicator.
// Owned here (alongside FormatBytes) so the TUI and CLI render the same
// character; bumping it changes both surfaces in one place.
const UpdateGlyph = "⇧"

// configImagesArgs fetches a structured project model so CheckUpdates can map
// service-name → image. The output is JSON keyed by service name (Compose v2
// schema).
var configImagesArgs = []string{"config", "--format", "json"}

// configImagesView is the minimal projection of `docker compose config --format
// json` consumed by CheckUpdates. Only the per-service `image` field is read;
// services with no image (i.e. build-only) are absent from the resulting map
// and stay "unknown" (tri-state preserved).
type configImagesView struct {
	Services map[string]struct {
		Image string `json:"image"`
	} `json:"services"`
}

// parseConfigImages parses `docker compose config --format json` into a
// flat map of service-name → image. Services with empty `image` (build-only,
// or interpolation failure) are omitted so the caller can leave their cell
// blank. Returns an error only when the bytes are not valid JSON; a parse
// success with zero services yields an empty map.
func parseConfigImages(data []byte) (map[string]string, error) {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return map[string]string{}, nil
	}
	var v configImagesView
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("parsing compose config: %w", err)
	}
	out := make(map[string]string, len(v.Services))
	for name, svc := range v.Services {
		img := strings.TrimSpace(svc.Image)
		if img == "" {
			continue
		}
		out[name] = img
	}
	return out, nil
}

// imageInspectArgs builds the `docker image inspect` argv for retrieving the
// local repo digests of an image. Bypasses Compose.command() because `docker
// image inspect` is a top-level docker CLI command, not a compose subcommand —
// mirrors the AllContainerStats precedent. The full RepoDigests slice is
// emitted (one per line via the range template) so parseLocalDigest can match
// the entry whose repo name corresponds to the compose-file image reference,
// rather than blindly taking RepoDigests[0] which is undefined when the image
// has been tagged under multiple repositories.
func imageInspectArgs(image string) []string {
	return []string{"image", "inspect", "--format", "{{range .RepoDigests}}{{println .}}{{end}}", image}
}

// imagetoolsInspectArgs builds the `docker buildx imagetools inspect` argv
// for retrieving the remote registry's MANIFEST-LIST digest of an image
// (for multi-arch) or the single-manifest digest (for single-arch). This is
// the multi-arch-correct alternative to `docker manifest inspect`: when an
// image is multi-arch, `image inspect` returns the manifest-list digest
// (because docker pulled via the list), and the `Digest:` line printed by
// `imagetools inspect` is also the manifest-list digest — they match.
// `manifest inspect --verbose` returns per-platform descriptors that never
// match the local manifest-list digest, producing false positives on
// popular Docker Hub images (nginx, postgres, alpine, node, redis).
//
// Bypasses Compose.command() because `docker buildx imagetools inspect` is
// a top-level docker CLI command, not a compose subcommand.
//
// IMPORTANT: we deliberately do NOT pass `--format '{{.Manifest.Digest}}'`
// here. Empirically verified on Docker 29.1.3 / buildx v0.30.1-desktop.1:
// the template substitution silently falls through and prints the same
// human-readable Name:/MediaType:/Digest:/Manifests: block as the default
// formatter. A permissive "first non-empty line" parser would then return
// `Name:      docker.io/library/nginx:latest` as the "digest" — a
// universal false-positive where every image is flagged as updated.
// Dropping the flag yields the default human format which our parser
// reliably extracts the `Digest:` line from. See parseImagetoolsDigest and
// testdata/buildx_imagetools_default_output.txt for the captured fixture.
// The buildx plugin ships with Docker Desktop and recent Docker Engine
// releases (v23+); when missing, CheckUpdates falls back to
// `docker manifest inspect`.
func imagetoolsInspectArgs(image string) []string {
	return []string{"buildx", "imagetools", "inspect", image}
}

// manifestInspectArgs builds the `docker manifest inspect` argv for retrieving
// the remote registry digest of an image. Used as the fallback when
// `docker buildx imagetools inspect` is unavailable (older Docker without
// the buildx plugin). Bypasses Compose.command() because `docker manifest
// inspect` is a top-level docker CLI command, not a compose subcommand.
// Output is parsed via parseManifestDigest.
//
// Multi-arch limitation when this path is used: for multi-arch images
// (nginx, postgres, alpine, node, redis, etc.) `image inspect` returns the
// manifest-LIST digest while `manifest inspect --verbose` array form
// returns per-platform descriptor digests. These never match by
// construction, so multi-arch images show a false "update available" on
// older Docker installs that lack buildx. Upgrading to Docker v23+ (or
// installing the buildx plugin) is the recommended fix; the imagetools
// path above handles multi-arch correctly.
func manifestInspectArgs(image string) []string {
	return []string{"manifest", "inspect", "--verbose", image}
}

// imagetoolsDigestRE matches a `sha256:` digest followed by exactly 64 hex
// characters (case-insensitive). This is the canonical content-address form
// for registry manifests; anything else returned by the parser is treated
// as "no data" so the manifest-inspect fallback kicks in.
var imagetoolsDigestRE = regexp.MustCompile(`(?i)\bsha256:[0-9a-f]{64}\b`)

// parseImagetoolsDigest extracts the manifest-list digest from the default
// human-readable output of `docker buildx imagetools inspect <image>`. The
// output looks like:
//
//	Name:      docker.io/library/nginx:latest
//	MediaType: application/vnd.oci.image.index.v1+json
//	Digest:    sha256:06aa3d7be10bc6307990c81bdca075793132e9163391abc370c015e344e23128
//	...
//	Manifests:
//	  Name:        ...@sha256:<per-platform>
//	  ...
//
// We extract the top-level `Digest:` line — that's the manifest-LIST digest
// which matches the local `RepoDigests` entry for multi-arch images. The
// per-platform digests under `Manifests:` are NOT what we want (they would
// reintroduce the multi-arch false positive that motivated switching away
// from `docker manifest inspect --verbose`).
//
// Returns "" when no top-level `Digest:` line is found OR when the value
// isn't a canonical `sha256:<64 hex>` string — callers treat empty as
// "unknown" and fall back to manifest inspect. The strict format check
// exists because a permissive "first non-empty line" parser produces a
// universal false-positive (every image flagged as updated) when the
// format string silently falls through to the default human-readable
// formatter — see imagetoolsInspectArgs for the gory details. There is
// no defensive "bare sha256 line" fallback: aborting on the first
// non-matching line means any wrapper that prints a leading banner
// (e.g. a `motd`-injecting docker context) silently defeats it; the
// primary `Digest:` line path handles every real buildx output we've
// observed.
func parseImagetoolsDigest(data []byte) string {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return ""
	}
	// Scan line-by-line and return the first top-level `Digest:` line's hex
	// value. Indented `Digest:` lines (under "Manifests:") are skipped via
	// the raw-line indentation check so we don't accidentally pick up a
	// per-platform descriptor.
	for _, raw := range strings.Split(s, "\n") {
		// Indented lines belong to a per-platform manifest entry; skip.
		if len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t') {
			continue
		}
		line := strings.TrimSpace(raw)
		// Match either `Digest:    sha256:...` or `Digest: sha256:...`
		// (the formatter pads to column alignment).
		if !strings.HasPrefix(line, "Digest:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "Digest:"))
		rest = strings.Trim(rest, "\"'")
		if m := imagetoolsDigestRE.FindString(rest); m != "" {
			// Normalize to lower-case algorithm/hex for stable comparison.
			return "sha256:" + strings.ToLower(m[len("sha256:"):])
		}
	}
	return ""
}

// parseLocalDigest extracts a digest from the multi-line output of
// `docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}'`,
// preferring the entry whose repo name matches the compose-file image
// reference. Each line has the form `<repo>@sha256:<hex>`; the function strips
// any tag from imageRef before comparing repo names so `nginx:latest` matches
// `nginx@sha256:...` and `repo.example.com/web:v3` matches
// `repo.example.com/web@sha256:...`. When no matching entry is found, falls
// back to the first non-empty digest (preserving the legacy behavior for
// single-tag images) so the previous test suite continues to pass. Empty
// input returns "" — callers treat empty as "unknown".
func parseLocalDigest(out, imageRef string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	wantedRepo := stripTag(imageRef)
	var first string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		at := strings.LastIndex(line, "@")
		if at < 0 || at+1 >= len(line) {
			continue
		}
		repo := line[:at]
		digest := line[at+1:]
		if first == "" {
			first = digest
		}
		if wantedRepo != "" && repo == wantedRepo {
			return digest
		}
	}
	return first
}

// StripTag is the exported form of stripTag. It is the single source of truth
// for deriving the repo portion of an image reference (used by the digest-pin in
// rollbackImageRef and by cmd/rollback.go's plan-line rendering), so the repo the
// plan advertises can never diverge from the repo that actually gets pinned.
func StripTag(ref string) string { return stripTag(ref) }

// stripTag returns the repo portion of an image reference, removing the tag
// (`:<tag>`) and any explicit digest (`@sha256:...`). Registry hosts with an
// explicit port (e.g. `localhost:5000/foo`) preserve the port — only the tag
// (last colon after the last `/`) is stripped. Reused by parseLocalDigest to
// match `image inspect` repo names against the compose-file image reference.
func stripTag(ref string) string {
	if ref == "" {
		return ""
	}
	// Drop explicit digest if present (`name@sha256:...`).
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	// Find the last `/` to scope the tag search to the final path component
	// (so registry ports like `localhost:5000/foo` aren't confused for tags).
	slash := strings.LastIndex(ref, "/")
	tail := ref
	if slash >= 0 {
		tail = ref[slash+1:]
	}
	if colon := strings.LastIndex(tail, ":"); colon >= 0 {
		if slash >= 0 {
			return ref[:slash+1+colon]
		}
		return tail[:colon]
	}
	return ref
}

// parseManifestDigest extracts the `Descriptor.digest` field from the output
// of `docker manifest inspect --verbose`. The output can be either a single
// object (single-arch image) or a JSON array (multi-arch / manifest list);
// for arrays we use the first entry's descriptor digest as a representative
// remote identity. Returns "" when the digest cannot be located — callers
// treat empty as "unknown".
//
// See manifestInspectArgs for the multi-arch caveat: array-form descriptor
// digests are per-platform and won't match the local manifest-list digest.
func parseManifestDigest(data []byte) string {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return ""
	}
	// Array form: take the first descriptor's digest.
	if strings.HasPrefix(s, "[") {
		var arr []struct {
			Descriptor struct {
				Digest string `json:"digest"`
			} `json:"Descriptor"`
		}
		if err := json.Unmarshal([]byte(s), &arr); err != nil || len(arr) == 0 {
			return ""
		}
		return strings.TrimSpace(arr[0].Descriptor.Digest)
	}
	// Object form.
	var obj struct {
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"Descriptor"`
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return ""
	}
	return strings.TrimSpace(obj.Descriptor.Digest)
}

// CheckUpdates reports per-service "image update available" verdicts for the
// project. The returned map is keyed by service name; only services that were
// successfully checked appear. Services absent from the map are "unknown" —
// build-only services, services whose image lookup failed, and services
// outside the requested subset stay absent so the TUI/CLI can render a blank
// cell rather than a false negative.
//
// Implementation expands service → image via `docker compose config --format
// json`, then for each image runs `docker image inspect` (local digest) and
// `docker manifest inspect` (remote digest) — both are top-level docker CLI
// commands, so they bypass c.command() and are built via exec.CommandContext
// directly, mirroring AllContainerStats. A service is reported as `true` when
// both digests are known and differ, `false` when both are known and match,
// and absent when either side fails (network error, no auth, image not
// pulled yet, etc.) — soft failure preserves the tri-state.
//
// services may be empty to mean "all services in the project". When non-empty,
// the config-derived image map is filtered to the requested subset.
//
// Systemic-failure detection: two parallel cascades surface errors so the
// caller renders "updates unavailable" instead of an all-blank cell column.
//
//  1. Registry cascade: when EVERY remote-digest fetch we attempted failed
//     with a stderr matching `looksLikeNetworkErr` AND no service got a
//     verdict, return `registry unreachable: <first such err>`.
//  2. Local-docker cascade: when EVERY local `docker image inspect` failed
//     AND every such failure looks like a daemon-down condition per
//     `looksLikeLocalDaemonErr` AND no service got a verdict, return
//     `local docker unavailable: <first such err>` — covers daemon-stopped
//     / socket-missing scenarios. Per-image "No such image" failures
//     (fresh deploy where the user hasn't pulled images yet) are absorbed
//     as absent rather than blanking the screen with a misleading
//     "local docker unavailable" diagnostic.
//
// A single non-network per-image failure (one image not pulled while
// others succeed, auth required for one repo, etc.) does NOT trigger
// either cascade — partial hiccups stay absorbed-as-unknown so they
// don't blank an otherwise-correct screen.
//
// Errors are returned for: (a) top-level `docker compose config` failure,
// (b) registry cascade, (c) local-docker cascade.
func (c *Compose) CheckUpdates(ctx context.Context, services []string) (map[string]bool, error) {
	images, err := c.fetchServiceImages(ctx)
	if err != nil {
		return nil, err
	}
	return scanImageUpdates(ctx, filterServices(images, services), c.compareImageDigest)
}

// imageComparer is the per-image verdict function scanImageUpdates folds into
// its result map. The contract matches compareImageDigestVia: (updated, ok,
// err) where ok=true and err=nil is the only definitive verdict.
type imageComparer func(ctx context.Context, image string) (bool, bool, error)

// imageVerdict is one image's memoized comparison outcome inside
// scanImageUpdates — the same (updated, ok, err) triple imageComparer returns.
type imageVerdict struct {
	updated bool
	ok      bool
	err     error
}

// scanImageUpdates compares every image in wanted and folds the per-image
// outcomes into a verdict map plus the systemic-failure cascades. Services
// whose comparison failed, or yielded no definitive answer, stay ABSENT from
// the map so the caller renders the tri-state blank cell rather than a false
// negative.
//
// All three diagnostics run unconditionally: which one can fire is decided by
// the ERROR SHAPE the caller's comparer emits, not by a flag. Compose wraps a
// failed local `docker image inspect` in errLocalImageInspect, and only that
// sentinel reaches the daemon counters — RemoteCompose and HostContainers pass
// a nil wrapper, so the daemon cascade cannot fire on the far side of an SSH
// hop where "local docker unavailable" would name the wrong machine
// (TestCompareImageDigest_SentinelBinding pins that binding on all three).
// errSSHTransport likewise only exists on the remote runner, via
// classifySSHError. A flag would only re-state what the sentinel already
// decides — and fetchRemoteDigestVia already short-circuits on errSSHTransport
// with no flag one call level below.
//
// A cascade fires only when NO service got a verdict — a partial hiccup (one
// image not pulled while the rest resolve) must not blank an otherwise
// correct screen. The transport abort is the one exception: it returns as soon
// as the first errSSHTransport lands, verdicts collected so far included.
func scanImageUpdates(ctx context.Context, wanted map[string]string, compare imageComparer) (map[string]bool, error) {
	out := make(map[string]bool, len(wanted))
	var (
		localAttempts   int
		localFailures   int
		daemonFailures  int
		firstDaemonErr  error
		remoteAttempts  int
		networkFailures int
		firstNetErr     error
	)
	// Distinct images are compared ONCE. A compose project usually gives every
	// service its own image, but host containers repeat one image constantly
	// (three MCP sidecars off the same tag), and each comparison costs a local
	// inspect plus up to two registry inspects — every one a separate SSH
	// round-trip on a remote host. The cache also keeps the cascade ratios
	// honest: each distinct image contributes exactly one attempt.
	seen := make(map[string]imageVerdict, len(wanted))
	for svc, img := range wanted {
		v, cached := seen[img]
		if !cached {
			updated, ok, rerr := compare(ctx, img)
			v = imageVerdict{updated: updated, ok: ok, err: rerr}
			seen[img] = v
			localAttempts++
		}
		if v.err != nil {
			// Transport first: a dead SSH hop poisons every remaining
			// image, so abort before any classification. Checked ahead of
			// the sentinel dispatch below because the two buckets are
			// independent and transport is the terminal one.
			if errors.Is(v.err, errSSHTransport) {
				return out, fmt.Errorf("remote update check transport failure: %w", v.err)
			}
			if !cached {
				// Distinguish local-side vs remote-side errors. Local
				// errors carry the errLocalImageInspect sentinel;
				// everything else reached the remote fetch and failed
				// there.
				if errors.Is(v.err, errLocalImageInspect) {
					localFailures++
					// Only count this toward the cascade when the stderr
					// looks like a daemon-down failure, not a benign per-
					// image "No such image" (fresh deploy where images
					// haven't been pulled yet). Without this gate, a multi-
					// service fresh deploy fires the cascade and produces
					// a confusing "local docker unavailable" diagnostic —
					// matching RemoteCompose, which already classifies
					// per-image failures via stderr content.
					if looksLikeLocalDaemonErr(v.err) {
						daemonFailures++
						if firstDaemonErr == nil {
							firstDaemonErr = v.err
						}
					}
				} else {
					remoteAttempts++
					if looksLikeNetworkErr(v.err) {
						networkFailures++
						if firstNetErr == nil {
							firstNetErr = v.err
						}
					}
				}
			}
			continue
		}
		if !v.ok {
			continue
		}
		out[svc] = v.updated
	}
	if len(out) == 0 {
		// Local-docker cascade fires only when EVERY per-image local
		// failure looks like a daemon-down condition. Per-image
		// "No such image" failures (fresh deploy) are absorbed as
		// absent, matching RemoteCompose semantics.
		if localAttempts > 0 && localFailures == localAttempts &&
			daemonFailures == localFailures && firstDaemonErr != nil {
			return out, fmt.Errorf("local docker unavailable: %w", firstDaemonErr)
		}
		if remoteAttempts > 0 && networkFailures == remoteAttempts {
			return out, fmt.Errorf("registry unreachable: %w", firstNetErr)
		}
	}
	return out, nil
}

// errLocalImageInspect is the sentinel wrapped around local
// `docker image inspect` failures so scanImageUpdates can distinguish local-
// side failures (daemon down, socket missing, image never pulled) from
// remote-side failures (registry unreachable, manifest not found).
// Sentinel-based dispatch keeps `compareImageDigestVia` returning a single
// error type that callers can switch on via `errors.Is`. Only the local
// binding applies it (via wrapLocalImageInspectErr); the remote one passes a
// nil wrapper, which is what keeps the daemon cascade off the SSH path.
var errLocalImageInspect = errors.New("local image inspect failed")

// Failure-classifier overview (three buckets, precedence enforced by callers):
//
//  1. SSH transport — sshTransportStderrPatterns in internal/compose/remote.go,
//     checked FIRST by classifySSHError. Wrapped in errSSHTransport so
//     RemoteCompose.CheckUpdates can abort the whole batch (every subsequent
//     image will fail the same way). Patterns are deliberately SSH-only
//     signatures (ssh:, kex_exchange_identification, mux_client, etc.); the
//     generic networking phrases ("connection refused", "broken pipe") were
//     removed because they ALSO appear in docker stderr when docker fails to
//     reach the registry, and misclassifying them as SSH transport would
//     blank the entire glyph column on a Docker Hub outage.
//  2. Registry network — looksLikeNetworkErr (below). Checked SECOND, after
//     SSH transport has been ruled out. Drives the "registry unreachable"
//     cascade when EVERY per-image remote fetch matches.
//  3. Local docker daemon — looksLikeLocalDaemonErr (below). Only applies
//     to Compose (local) — RemoteCompose doesn't see local-docker failures
//     because the docker invocation happens on the remote side via SSH.
//     Drives the "local docker unavailable" cascade when EVERY per-image
//     local inspect failure matches a daemon-down shape.
//
// Patterns live next to their classifiers (here for #2 and #3,
// remote.go for #1) so adding/removing one only touches a single list.

// looksLikeNetworkErr returns true when err carries a stderr (via
// withStderr's wrapping or a plain error string) that smells like a
// network/registry failure. Heuristic substring match against lower-cased
// err.Error(); patterns chosen to catch the common Docker Hub outage /
// DNS / TLS / connection-refused cases without over-firing on per-image
// docker errors like "No such image" or "manifest unknown".
//
// `"no such host"` is the canonical net.DNSError tail string. The previous
// `"lookup "` substring was narrowed away because it matched benign error
// fragments like "failed to lookup image" / "manifest lookup failed" /
// "image lookup error", surfacing misleading "registry unreachable"
// diagnostics on what were actually per-image docker errors. The remaining
// patterns are specific enough that the same false-positive risk doesn't
// apply.
func looksLikeNetworkErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	patterns := []string{
		"no such host",
		"connection refused",
		"connection reset",
		"connection closed",
		"connection timed out",
		"timeout exceeded",
		"i/o timeout",
		"network is unreachable",
		"no route to host",
		"tls handshake",
		"x509:",
		"too many requests", // 429 from registry — systemic if all fail
		"server misbehaving",
		"temporary failure",
		"dial tcp",
	}
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// looksLikeLocalDaemonErr returns true when err carries a stderr that smells
// like the local Docker daemon being unavailable (daemon not running, socket
// missing, docker CLI missing). Heuristic substring match against lower-cased
// err.Error().
//
// The local-docker cascade requires every per-image failure to match this
// classifier (mirroring the looksLikeNetworkErr + registry-cascade pattern),
// so per-image "No such image" failures (fresh deploy with multiple
// un-pulled images) are absorbed as absent and the cascade only fires on
// true daemon-down / socket-missing scenarios.
func looksLikeLocalDaemonErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	patterns := []string{
		"cannot connect to the docker daemon",
		"is the docker daemon running",
		"docker daemon is not running",
		"docker.sock: connect: no such file or directory",
		"docker.sock: connect: connection refused",
		"docker.sock: connect: permission denied",
		`"docker": executable file not found`,
		"docker: command not found",
	}
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// fetchServiceImages runs `docker compose config --format json` and returns
// the service-name → image map. Build-only services (no `image:`) are absent.
func (c *Compose) fetchServiceImages(ctx context.Context) (map[string]string, error) {
	cmd := c.command(ctx, configImagesArgs...)
	var out []byte
	var err error
	if c.outputCmd != nil {
		out, err = c.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("fetching compose config: %w", withStderr(err))
	}
	return parseConfigImages(out)
}

// filterServices returns a copy of all containing only entries whose key
// appears in wanted. When wanted is empty (or nil), all is returned as-is
// (mapping the "no filter = all services" CheckUpdates contract).
func filterServices(all map[string]string, wanted []string) map[string]string {
	if len(wanted) == 0 {
		return all
	}
	out := make(map[string]string, len(wanted))
	for _, name := range wanted {
		if img, ok := all[name]; ok {
			out[name] = img
		}
	}
	return out
}

// wrapLocalImageInspectErr tags a failed local `docker image inspect` with the
// errLocalImageInspect sentinel so scanImageUpdates routes it to the daemon
// cascade instead of the registry one. It is the local half of
// compareImageDigestVia's error-classification knob; the remote half passes nil
// because a remote inspect failure is indistinguishable from any other
// per-image docker error on the far side of the SSH hop.
func wrapLocalImageInspectErr(err error) error {
	return fmt.Errorf("%w: %w", errLocalImageInspect, err)
}

// compareImageDigestVia fetches the local and remote digests for image through
// the dockerRunner seam and returns (updateAvailable, ok, err). ok=true with
// nil err means a definitive verdict; ok=false with nil err means at least one
// side could not be determined for a non-failure reason (parse returned empty,
// etc.). ok=false with non-nil err means EITHER the local image inspect failed
// (already passed through localErrWrap) OR the remote fetch failed with an
// error that survived the imagetools→manifest fallback.
//
// The seam is the only local/remote variation point: every call is a TOP-LEVEL
// docker CLI command, not a compose subcommand, so neither runner may route it
// through command() / remoteCommand(). localErrWrap is the second variation
// point — the two runners emit different error shapes, and only the local one
// carries a sentinel the daemon cascade can dispatch on. A nil localErrWrap
// passes the error through untouched.
//
// Multi-arch correctness: the remote-digest path tries
// `docker buildx imagetools inspect` first (default human format —
// parser extracts the top-level `Digest:` line, which is the manifest-LIST
// digest matching the local RepoDigest for multi-arch images). When
// buildx is unavailable (older Docker, no buildx plugin) OR when the
// imagetools call fails, we fall back to `docker manifest inspect
// --verbose`. The fallback retains the multi-arch false-positive
// limitation but preserves the feature on older Docker installs.
func compareImageDigestVia(ctx context.Context, d dockerRunner, image string, localErrWrap func(error) error) (bool, bool, error) {
	localOut, lerr := d.run(ctx, imageInspectArgs(image)...)
	if lerr != nil {
		if localErrWrap != nil {
			return false, false, localErrWrap(lerr)
		}
		return false, false, lerr
	}
	localDigest := parseLocalDigest(string(localOut), image)
	if localDigest == "" {
		return false, false, nil
	}

	remoteDigest, ok, rerr := fetchRemoteDigestVia(ctx, d, image)
	if rerr != nil {
		return false, false, rerr
	}
	if !ok || remoteDigest == "" {
		return false, false, nil
	}
	return localDigest != remoteDigest, true, nil
}

// fetchRemoteDigestVia queries the registry's manifest digest for image through
// the dockerRunner seam, preferring `docker buildx imagetools inspect`
// (multi-arch-correct) and falling back to `docker manifest inspect --verbose`
// when imagetools is missing or fails. Returns (digest, true, nil) on a
// definitive verdict; ("", false, nil) when the command succeeded but parsing
// yielded an empty result (callers treat empty as "unknown" without polluting
// the cascade counters); ("", false, err) only when BOTH paths failed AND the
// last error survived — so the systemic-failure detector in scanImageUpdates
// sees a non-nil error only when both attempts hit something that prevented
// digest retrieval.
//
// An errSSHTransport failure short-circuits the fallback: retrying over a dead
// SSH hop only burns a round-trip, and the caller needs the sentinel intact to
// abort the batch. The local runner never emits that sentinel, so the branch is
// inert there.
func fetchRemoteDigestVia(ctx context.Context, d dockerRunner, image string) (string, bool, error) {
	out, err := d.run(ctx, imagetoolsInspectArgs(image)...)
	if err == nil {
		if dg := parseImagetoolsDigest(out); dg != "" {
			return dg, true, nil
		}
	} else if errors.Is(err, errSSHTransport) {
		return "", false, err
	}
	// Fallback: `docker manifest inspect --verbose`. Multi-arch will be
	// wrong here (per-platform descriptor digest); documented limitation.
	out, err = d.run(ctx, manifestInspectArgs(image)...)
	if err != nil {
		return "", false, err
	}
	dg := parseManifestDigest(out)
	if dg == "" {
		return "", false, nil
	}
	return dg, true, nil
}

// compareImageDigest is Compose's binding of compareImageDigestVia: the local
// dockerRunner (which routes through runDockerCmd, capturing stderr explicitly
// so the classifier helpers see real stderr whether the command ran for real or
// through the outputCmd test hook) plus the errLocalImageInspect wrapper that
// drives the local-docker cascade.
func (c *Compose) compareImageDigest(ctx context.Context, image string) (bool, bool, error) {
	return compareImageDigestVia(ctx, localDockerRunner{c: c}, image, wrapLocalImageInspectErr)
}

// runDockerCmd runs a top-level `docker <args...>` command, bypassing
// c.command() (which is compose-specific) so update-detection callers can
// invoke `docker image inspect` / `docker buildx imagetools inspect` /
// `docker manifest inspect` without a malformed `docker compose <verb>`
// argv. Stderr is captured explicitly via a strings.Builder so the
// classifier helpers (looksLikeLocalDaemonErr, looksLikeNetworkErr) see
// real stderr text whether the command was invoked in production (full
// exec) or via the outputCmd test hook. Returns (stdout, err); when err
// is non-nil it carries any captured stderr text suffix, mirroring the
// SSH path's runRemoteDockerCmd convention.
func (c *Compose) runDockerCmd(ctx context.Context, dockerArgs []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	if c.outputCmd != nil {
		out, err := c.outputCmd(cmd)
		if err != nil {
			// outputCmd test hook does not surface a separate stderr —
			// the err's string IS the diagnostic the classifier sees.
			return nil, err
		}
		return out, nil
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return nil, fmt.Errorf("%w: %s", err, s)
		}
		return nil, err
	}
	return out, nil
}

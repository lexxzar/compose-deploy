package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Update-detection parsers.
//
// Output format pinned to Docker Compose v2 (verified against v2.40.3 — the
// stable release line as of 2026-05). The dry-run subcommand was introduced in
// Compose v2.22; earlier daemons fall back to per-image digest comparison
// (implemented in Task 2 on the Composer methods themselves).
//
// `docker compose pull --dry-run --quiet` writes one line per service to
// STDERR (not stdout) in the form:
//
//   " DRY-RUN MODE -  <service> Pulling "
//   " DRY-RUN MODE -  <service> Pulled "
//   " DRY-RUN MODE -  <service> Skipped - Image is already present locally "
//   " DRY-RUN MODE -  <service> Skipped - No image to be pulled "
//
// Leading and trailing whitespace varies between versions; we trim aggressively
// before classifying. The "Pulled" lines are completion markers and add no
// signal beyond "Pulling" so the parser ignores them.

// dryRunPrefix is the marker Compose emits at the start of every dry-run line.
// Lines without this prefix are non-service noise (warnings, progress) and are
// skipped.
const dryRunPrefix = "DRY-RUN MODE -"

// parseDryRunOutput parses the stderr captured from
// `docker compose pull --dry-run --quiet` and returns a map keyed by service
// name. Values:
//
//   - true  → an update will be pulled (line says "Pulling")
//   - false → image is current locally (line says "Skipped - Image is already
//     present locally")
//
// Services absent from the result are deliberately so — tri-state preserved.
// In particular, build-only services (line says "Skipped - No image to be
// pulled") are NOT added to the map; callers treat absent as "unknown" and
// render a blank cell. Unrecognised lines are also dropped silently so future
// Compose output additions don't crash the parser.
//
// Service names containing spaces are not supported by Docker Compose, so the
// parser uses the first whitespace-separated token after the prefix as the
// service name. The remainder of the line is the verdict text.
func parseDryRunOutput(stderr string) map[string]bool {
	out := make(map[string]bool)
	for _, raw := range strings.Split(stderr, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// Locate the dry-run prefix; if absent, this is noise.
		idx := strings.Index(line, dryRunPrefix)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(line[idx+len(dryRunPrefix):])
		if rest == "" {
			continue
		}
		// First whitespace-separated token is the service name.
		var service, verdict string
		if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
			service = rest[:sp]
			verdict = strings.TrimSpace(rest[sp+1:])
		} else {
			// Line has only the service name with no verdict — ambiguous, skip.
			continue
		}
		if service == "" {
			continue
		}
		switch {
		case strings.HasPrefix(verdict, "Pulling"):
			// First-class "update available" signal.
			out[service] = true
		case strings.Contains(verdict, "Pull required"):
			// Reserved phrasing per the plan; current Compose doesn't emit it
			// but we accept it defensively for forward compatibility.
			out[service] = true
		case strings.Contains(verdict, "Image is already present locally"),
			strings.Contains(verdict, "already present"):
			// Image current — explicit "no update" signal. Only emitted when
			// the caller passes `--policy=missing`; with the default policy
			// the dry-run path doesn't distinguish current images, which is
			// handled at the Composer layer (Task 2).
			out[service] = false
		default:
			// "Skipped - No image to be pulled" (build-only) and the "Pulled"
			// completion marker fall through here. Build-only services stay
			// absent (tri-state "unknown"); "Pulled" duplicates the "Pulling"
			// signal already recorded — no-op either way.
		}
	}
	return out
}

// detectDryRunFromHelp reports whether the help text of `docker compose pull`
// advertises the `--dry-run` flag. The probe is a substring match against the
// canonical flag spelling — short-name aliases (none currently) would need
// extending here. The probe is one-shot per Composer instance, cached on the
// struct (see Task 2).
func detectDryRunFromHelp(help string) bool {
	return strings.Contains(help, "--dry-run")
}

// dryRunArgs is the package-internal argv slice appended after the compose
// subcommand prefix when invoking the dry-run path. Kept as a package-level
// `var` (not a helper function) so test assertions can compare the exact
// argv shape without re-spelling the flags. `--policy=missing` is intentional:
// against the default `--policy=always`, every non-build service reports
// "Pulling" regardless of local image age (the Compose v2 dry-run does not
// distinguish updates from current images with that policy). With
// `--policy=missing` the dry-run emits "Skipped - Image is already present
// locally" for services whose image is on disk, which the parser surfaces as
// `false`. The signal is still weak — "Skipped" only confirms local presence,
// not registry-level currency — so callers treating presence as "no update"
// trade some false negatives for cheap detection on hosts where the image was
// recently pulled. The manifest-inspect fallback path is authoritative when
// the dry-run path is unavailable.
var dryRunArgs = []string{"pull", "--dry-run", "--quiet", "--policy=missing"}

// pullHelpArgs probes whether `docker compose pull` accepts `--dry-run`.
// Compose v2.22+ advertises the flag; earlier daemons do not. The probe is
// one-shot per Composer instance — see Compose.dryRunDetected.
var pullHelpArgs = []string{"pull", "--help"}

// configImagesArgs fetches a structured project model so CheckUpdates can map
// service-name → image when falling back to per-image `docker manifest
// inspect`. The output is JSON keyed by service name (Compose v2 schema).
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
// local digest of an image. Bypasses Compose.command() because `docker image
// inspect` is a top-level docker CLI command, not a compose subcommand —
// mirrors the AllContainerStats precedent.
func imageInspectArgs(image string) []string {
	return []string{"image", "inspect", "--format", "{{index .RepoDigests 0}}", image}
}

// manifestInspectArgs builds the `docker manifest inspect` argv for retrieving
// the remote registry digest of an image. Bypasses Compose.command() because
// `docker manifest inspect` is a top-level docker CLI command, not a compose
// subcommand. Output is parsed via parseManifestDigest.
func manifestInspectArgs(image string) []string {
	return []string{"manifest", "inspect", "--verbose", image}
}

// parseLocalDigest extracts the digest portion of a `docker image inspect`
// `{{index .RepoDigests 0}}` template result, which has the form
// `<image>@sha256:<hex>`. Returns the digest after the `@` so callers can
// compare against the remote manifest digest. An empty input or a value with
// no `@` returns "" — callers treat empty as "unknown".
func parseLocalDigest(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if at := strings.LastIndex(s, "@"); at >= 0 && at+1 < len(s) {
		return s[at+1:]
	}
	return ""
}

// parseManifestDigest extracts the `Descriptor.digest` field from the output
// of `docker manifest inspect --verbose`. The output can be either a single
// object (single-arch image) or a JSON array (multi-arch / manifest list);
// for arrays we use the first entry's descriptor digest as a representative
// remote identity. Returns "" when the digest cannot be located — callers
// treat empty as "unknown".
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

// detectDryRunSupport runs the one-shot probe that determines whether the
// installed `docker compose` advertises `--dry-run` on the `pull` subcommand.
// Caches the result on the Compose struct so subsequent CheckUpdates calls
// skip the round-trip. A probe failure (binary missing, permission denied)
// is treated as "unsupported" rather than propagated — CheckUpdates falls
// back to the manifest-inspect path in that case.
func (c *Compose) detectDryRunSupport(ctx context.Context) {
	if c.dryRunDetected {
		return
	}
	cmd := c.command(ctx, pullHelpArgs...)
	var out []byte
	var err error
	if c.outputCmd != nil {
		out, err = c.outputCmd(cmd)
	} else {
		out, err = cmd.CombinedOutput()
	}
	c.dryRunDetected = true
	if err != nil {
		c.dryRunSupported = false
		return
	}
	c.dryRunSupported = detectDryRunFromHelp(string(out))
}

// CheckUpdates reports per-service "image update available" verdicts for the
// project. The returned map is keyed by service name; only services that were
// successfully checked appear. Services absent from the map are "unknown" —
// build-only services, services whose image lookup failed, and services
// outside the requested subset stay absent so the TUI/CLI can render a blank
// cell rather than a false negative.
//
// Two implementation paths:
//
//  1. Dry-run path (preferred when `docker compose pull` advertises
//     `--dry-run` — Compose v2.22+). Runs `docker compose pull --dry-run
//     --quiet --policy=missing [services...]` and parses the stderr verdict
//     lines. Pre-flight observation: dry-run does NOT dedupe services that
//     share an image — every service is reported individually — so no
//     `docker compose config` expansion pass is needed (the plan called for
//     it conditionally; here it is intentionally absent). The signal under
//     `--policy=missing` is "image present locally" rather than "image is
//     current" — see dryRunArgs doc for the trade-off.
//
//  2. Fallback path. When dry-run is unavailable, expand service → image via
//     `docker compose config --format json`, then for each image run
//     `docker image inspect` (local digest) and `docker manifest inspect`
//     (remote digest) — both are top-level docker CLI commands, so they
//     bypass c.command() and are built via exec.CommandContext directly,
//     mirroring AllContainerStats. A service is reported as `true` when
//     both digests are known and differ, `false` when both are known and
//     match, and absent when either side fails (network error, no auth,
//     image not pulled yet, etc.) — soft failure preserves the tri-state.
//
// services may be empty to mean "all services in the project". When
// non-empty, the dry-run path passes the names through to compose; the
// fallback path filters the config-derived image map to the requested
// subset.
//
// Errors are returned only for top-level setup failures (e.g. the config
// fetch itself fails). Per-image failures in the fallback path are silently
// absorbed into "unknown" rather than aborting the whole batch.
func (c *Compose) CheckUpdates(ctx context.Context, services []string) (map[string]bool, error) {
	c.detectDryRunSupport(ctx)
	if c.dryRunSupported {
		return c.checkUpdatesDryRun(ctx, services)
	}
	return c.checkUpdatesFallback(ctx, services)
}

// checkUpdatesDryRun runs the `docker compose pull --dry-run` path and parses
// the stderr verdict. The dry-run subcommand writes verdicts to STDERR, so
// CombinedOutput is used to capture them — stdout is empty under `--quiet`.
func (c *Compose) checkUpdatesDryRun(ctx context.Context, services []string) (map[string]bool, error) {
	args := append([]string{}, dryRunArgs...)
	args = append(args, services...)
	cmd := c.command(ctx, args...)
	var out []byte
	var err error
	if c.outputCmd != nil {
		out, err = c.outputCmd(cmd)
	} else {
		out, err = cmd.CombinedOutput()
	}
	if err != nil {
		// Dry-run can exit non-zero on registry errors yet still emit useful
		// per-service lines on stderr — return whatever the parser can recover
		// alongside the error so callers can surface partial results.
		results := parseDryRunOutput(string(out))
		return results, fmt.Errorf("compose pull --dry-run: %w", withStderr(err))
	}
	return parseDryRunOutput(string(out)), nil
}

// checkUpdatesFallback implements the manifest-inspect path used when
// `--dry-run` is unavailable. See CheckUpdates doc for the full contract.
func (c *Compose) checkUpdatesFallback(ctx context.Context, services []string) (map[string]bool, error) {
	images, err := c.fetchServiceImages(ctx)
	if err != nil {
		return nil, err
	}
	wanted := filterServices(images, services)
	out := make(map[string]bool, len(wanted))
	for svc, img := range wanted {
		updated, ok := c.compareImageDigest(ctx, img)
		if !ok {
			continue
		}
		out[svc] = updated
	}
	return out, nil
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

// compareImageDigest fetches the local and remote digests for image and
// returns (updateAvailable, ok). ok=false means at least one side could not
// be determined (treat as "unknown" — caller leaves cell blank). The argv
// for both inspect calls bypasses c.command() because they are top-level
// docker CLI commands, not compose subcommands; the outputCmd test hook is
// still honored so the join logic is exercised without invoking Docker.
func (c *Compose) compareImageDigest(ctx context.Context, image string) (bool, bool) {
	localCmd := exec.CommandContext(ctx, "docker", imageInspectArgs(image)...)
	var localOut []byte
	var lerr error
	if c.outputCmd != nil {
		localOut, lerr = c.outputCmd(localCmd)
	} else {
		localOut, lerr = localCmd.Output()
	}
	if lerr != nil {
		return false, false
	}
	localDigest := parseLocalDigest(string(localOut))
	if localDigest == "" {
		return false, false
	}

	remoteCmd := exec.CommandContext(ctx, "docker", manifestInspectArgs(image)...)
	var remoteOut []byte
	var rerr error
	if c.outputCmd != nil {
		remoteOut, rerr = c.outputCmd(remoteCmd)
	} else {
		remoteOut, rerr = remoteCmd.Output()
	}
	if rerr != nil {
		return false, false
	}
	remoteDigest := parseManifestDigest(remoteOut)
	if remoteDigest == "" {
		return false, false
	}
	return localDigest != remoteDigest, true
}

// Compile-time guard: every *Compose must satisfy the CheckUpdates shape, so
// CheckUpdates additions remain in sync with the runner.Composer assertion
// in compose.go.
var _ interface {
	CheckUpdates(ctx context.Context, services []string) (map[string]bool, error)
} = (*Compose)(nil)

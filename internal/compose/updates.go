package compose

import "strings"

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

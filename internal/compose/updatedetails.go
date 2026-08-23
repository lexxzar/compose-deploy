package compose

import (
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

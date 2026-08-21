package compose

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// hostPsEntry matches `docker ps --format '{{json .}}'` — NOT psEntry, which matches
// `docker compose ps --format json`. Different field names, different shape.
type hostPsEntry struct {
	ID        string `json:"ID"`        // already 12-char short form
	Names     string `json:"Names"`     // comma-joined; take the first
	Image     string `json:"Image"`     // may be an image ID for untagged images
	State     string `json:"State"`     // "running", "exited", ...
	Status    string `json:"Status"`    // "Up 2 hours (healthy)" — health lives here
	Ports     string `json:"Ports"`     // text form -> reuse parsePortsString
	Labels    string `json:"Labels"`    // comma-joined k=v
	CreatedAt string `json:"CreatedAt"` // "2006-01-02 15:04:05 -0700 MST"
}

// composeProjectLabel is the label key that marks a container as belonging to a
// Docker Compose project. The trailing "=" keeps it from matching the sibling keys
// com.docker.compose.project.config_files and com.docker.compose.project.working_dir.
const composeProjectLabel = "com.docker.compose.project="

// parseHostContainers parses the output of `docker ps -a --format '{{json .}}'`.
// Tolerant of both NDJSON (the template form emits one object per line) and the
// JSON-array form, matching the parseContainerStatus and parseStatsOutput convention.
func parseHostContainers(data []byte) ([]hostPsEntry, error) {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "[]" {
		return nil, nil
	}

	var entries []hostPsEntry

	if strings.HasPrefix(s, "[") {
		if err := json.Unmarshal([]byte(s), &entries); err != nil {
			return nil, fmt.Errorf("parsing host containers: %w", err)
		}
		return entries, nil
	}

	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry hostPsEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("parsing host containers: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// isComposeManaged reports whether the comma-joined label string carries a
// com.docker.compose.project key.
//
// The scan walks tokens rather than splitting into k=v pairs: a label VALUE may
// legally contain a comma, so a split-and-map can mis-slice. Checking the prefix
// only at a token start means a false verdict would need a label value containing
// the literal ",com.docker.compose.project=".
func isComposeManaged(labels string) bool {
	for rest := labels; rest != ""; {
		if strings.HasPrefix(rest, composeProjectLabel) {
			return true
		}
		i := strings.IndexByte(rest, ',')
		if i < 0 {
			return false
		}
		rest = rest[i+1:]
	}
	return false
}

// healthSuffixCaptureRe captures the contents of a trailing "(...)" annotation.
// It is anchored at the end so "Exited (255) 3 months ago" does not match.
var healthSuffixCaptureRe = regexp.MustCompile(`\(([^)]*)\)\s*$`)

// parseHealthFromStatus extracts the health value from a host-level Status string.
// Host-level `docker ps` has no separate Health field — it lives in the trailing
// annotation, e.g. "Up 2 hours (healthy)". Returns "healthy", "unhealthy",
// "starting", or "" when the container has no healthcheck.
func parseHealthFromStatus(status string) string {
	m := healthSuffixCaptureRe.FindStringSubmatch(strings.TrimSpace(status))
	if m == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(m[1])) {
	case "healthy":
		return "healthy"
	case "unhealthy":
		return "unhealthy"
	case "health: starting", "starting":
		return "starting"
	}
	return ""
}

// hostContainerName returns the first element of the comma-joined Names field.
func hostContainerName(names string) string {
	if i := strings.IndexByte(names, ','); i >= 0 {
		names = names[:i]
	}
	return strings.TrimSpace(names)
}

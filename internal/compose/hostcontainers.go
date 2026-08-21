package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/lexxzar/compose-deploy/internal/runner"
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

// ErrReadOnly is returned by the five runner.Composer write methods on
// HostContainers. A container with no compose project has no compose file, so
// stop/rm/pull/create/start cannot be expressed as a compose verb. The TUI
// gates every key that would reach these methods, so the sentinel is a
// backstop rather than a user-facing path.
var ErrReadOnly = errors.New("read-only: container is not managed by docker compose")

// dockerRunner is the ONLY local/remote variation point of HostContainers.
// It needs three methods rather than one *exec.Cmd builder because the three
// call shapes are mutually exclusive: run CAPTURES output and classifies the
// error, stream writes to an io.Writer and must NOT allocate a TTY, and tty
// must (the remote form splices -t, matching RemoteCompose.ExecCommand).
type dockerRunner interface {
	run(ctx context.Context, args ...string) ([]byte, error)
	stream(ctx context.Context, w io.Writer, args ...string) error
	tty(ctx context.Context, args ...string) (*exec.Cmd, error)
}

// HostContainers is a read-only runner.Composer over the containers on a docker
// host that carry no com.docker.compose.project label. Local and remote differ
// only in the dockerRunner seam, so the remote form inherits runRemoteDockerCmd's
// SSH transport classification and stderr capture for free.
type HostContainers struct {
	docker dockerRunner
}

// localDockerRunner adapts *Compose. run and stream reuse the existing
// top-level docker primitives (which already bypass command(), since
// `docker ps` is not a compose subcommand); tty builds the argv directly,
// mirroring Compose.ExecCommand's "return the cmd, let the caller run it".
type localDockerRunner struct{ c *Compose }

func (l localDockerRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	return l.c.runDockerCmd(ctx, args)
}

func (l localDockerRunner) stream(ctx context.Context, w io.Writer, args ...string) error {
	return l.c.runDockerCmdStream(ctx, args, w)
}

func (l localDockerRunner) tty(ctx context.Context, args ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "docker", args...), nil
}

// remoteDockerRunner adapts *RemoteCompose. Every form goes through sshArgs()
// so SSHExtraArgs land immediately before the host argument.
type remoteDockerRunner struct{ r *RemoteCompose }

func (rd remoteDockerRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	return rd.r.runRemoteDockerCmd(ctx, args)
}

func (rd remoteDockerRunner) stream(ctx context.Context, w io.Writer, args ...string) error {
	return rd.r.runRemoteDockerCmdStream(ctx, args, w)
}

func (rd remoteDockerRunner) tty(ctx context.Context, args ...string) (*exec.Cmd, error) {
	escaped := make([]string, 0, len(args)+1)
	escaped = append(escaped, "docker")
	for _, a := range args {
		escaped = append(escaped, shellEscape(a))
	}
	sshArgv := rd.r.sshArgs(
		[]string{"-t", "-S", rd.r.SocketPath, "-o", "ControlMaster=no"},
		strings.Join(escaped, " "),
	)
	return exec.CommandContext(ctx, "ssh", sshArgv...), nil
}

// NewLocalHostContainers returns a HostContainers reading the local docker host.
func NewLocalHostContainers(c *Compose) *HostContainers {
	return &HostContainers{docker: localDockerRunner{c: c}}
}

// NewRemoteHostContainers returns a HostContainers reading the remote docker
// host over the existing ControlMaster socket.
func NewRemoteHostContainers(r *RemoteCompose) *HostContainers {
	return &HostContainers{docker: remoteDockerRunner{r: r}}
}

func (h *HostContainers) Stop(ctx context.Context, containers []string, w io.Writer) error {
	return ErrReadOnly
}

func (h *HostContainers) Remove(ctx context.Context, containers []string, w io.Writer) error {
	return ErrReadOnly
}

func (h *HostContainers) Pull(ctx context.Context, containers []string, w io.Writer) error {
	return ErrReadOnly
}

func (h *HostContainers) Create(ctx context.Context, containers []string, w io.Writer) error {
	return ErrReadOnly
}

func (h *HostContainers) Start(ctx context.Context, containers []string, w io.Writer) error {
	return ErrReadOnly
}

// CheckUpdates is a stub: it returns no verdicts, which the tri-state contract
// reads as "unknown" for every container, so the ⇧ column stays blank.
// TODO(task 12): source the name-to-image map from the Image field already
// returned by docker ps and reuse the extracted digest-compare loop.
func (h *HostContainers) CheckUpdates(ctx context.Context, services []string) (map[string]bool, error) {
	return nil, nil
}

// hostPsArgs is the discovery argv. The `{{json .}}` template form is used
// rather than the bare `json` keyword, which only exists on Docker CLI >= 23.0;
// this repo deliberately supports legacy hosts via Detect(), and the template
// produces identical output on every CLI version.
var hostPsArgs = []string{"ps", "-a", "--format", "{{json .}}"}

// unmanagedEntries lists the host containers that carry no compose project
// label. Entries whose first name is empty are dropped — an unnamed row could
// not be addressed by any of the read methods.
func (h *HostContainers) unmanagedEntries(ctx context.Context) ([]hostPsEntry, error) {
	out, err := h.docker.run(ctx, hostPsArgs...)
	if err != nil {
		return nil, fmt.Errorf("listing host containers: %w", err)
	}
	entries, err := parseHostContainers(out)
	if err != nil {
		return nil, err
	}
	unmanaged := make([]hostPsEntry, 0, len(entries))
	for _, e := range entries {
		if isComposeManaged(e.Labels) || hostContainerName(e.Names) == "" {
			continue
		}
		unmanaged = append(unmanaged, e)
	}
	return unmanaged, nil
}

// ListServices returns the names of the unmanaged containers, sorted.
func (h *HostContainers) ListServices(ctx context.Context) ([]string, error) {
	entries, err := h.unmanagedEntries(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, hostContainerName(e.Names))
	}
	sort.Strings(names)
	return names, nil
}

// ContainerStatus maps each unmanaged container name to its status. There is no
// replica aggregation here — a host container is its own row — so the fields map
// one-to-one from the ps entry.
func (h *HostContainers) ContainerStatus(ctx context.Context) (map[string]runner.ServiceStatus, error) {
	entries, err := h.unmanagedEntries(ctx)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	status := make(map[string]runner.ServiceStatus, len(entries))
	for _, e := range entries {
		st := runner.ServiceStatus{
			Running: e.State == "running",
			Health:  parseHealthFromStatus(e.Status),
			Uptime:  formatUptime(e.Status),
		}
		if t, ok := parseCreatedAt(e.CreatedAt); ok {
			st.Created = t.Format("2006-01-02 15:04")
		}
		if e.Ports != "" {
			st.Ports = dedupAndSortPorts(parsePortsString(e.Ports))
		}
		status[hostContainerName(e.Names)] = st
	}
	return status, nil
}

// hostStatsArgs is the host-wide stats argv. It keeps the bare `json` keyword
// that AllContainerStats already uses, rather than the ps template form: only
// `docker ps` needed the template workaround for legacy CLIs.
var hostStatsArgs = []string{"stats", "--no-stream", "--format", "json"}

// ContainerStats returns CPU and memory usage for each unmanaged container,
// keyed by container name.
//
// It goes through the dockerRunner seam directly rather than through
// AllContainerStats / AllContainerStatsRemote: those take a concrete *Compose
// or *RemoteCompose, which a seam-held HostContainers cannot supply without a
// type switch that would defeat the seam. Only the two-line argv build is
// duplicated; the pure parser and the join helper are reused.
//
// A container that `docker ps` reports but `docker stats` omits (stopped, or
// stopped between the two calls) is silently skipped, matching
// Compose.ContainerStats.
func (h *HostContainers) ContainerStats(ctx context.Context) (map[string]runner.ServiceStats, error) {
	entries, err := h.unmanagedEntries(ctx)
	if err != nil {
		return nil, err
	}
	out, err := h.docker.run(ctx, hostStatsArgs...)
	if err != nil {
		return nil, fmt.Errorf("fetching host container stats: %w", err)
	}
	all, err := parseStatsOutput(out)
	if err != nil {
		return nil, err
	}
	pairs := make([]psIDService, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		pairs = append(pairs, psIDService{ID: shortContainerID(e.ID), Service: hostContainerName(e.Names)})
	}
	return aggregateStatsByService(pairs, all), nil
}

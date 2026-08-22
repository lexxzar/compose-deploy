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
// NDJSON only, unlike the sibling parseContainerStatus/parseStatsOutput: those
// read a --format json flag whose shape changed across Docker versions, while
// a per-container Go template emits one object per line by construction on
// every version, so there is no array form to tolerate.
func parseHostContainers(data []byte) ([]hostPsEntry, error) {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return nil, nil
	}

	var entries []hostPsEntry
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
// The match is anchored at a token start rather than done by splitting into k=v
// pairs: a label VALUE may legally contain a comma, so a split-and-map can
// mis-slice. A false verdict would need a label value containing the literal
// ",com.docker.compose.project=".
func isComposeManaged(labels string) bool {
	return strings.HasPrefix(labels, composeProjectLabel) ||
		strings.Contains(labels, ","+composeProjectLabel)
}

// parseHealthFromStatus extracts the health value from a host-level Status string.
// Host-level `docker ps` has no separate Health field — it lives in the trailing
// annotation, e.g. "Up 2 hours (healthy)". Returns "healthy", "unhealthy",
// "starting", or "" when the container has no healthcheck.
//
// It reads the SAME healthSuffixRe that formatUptime strips (uptime.go), so the
// two consumers of the trailing-annotation grammar cannot drift apart. The
// end anchor is load-bearing: without it "Exited (255) 3 months ago" would match
// its first paren group.
func parseHealthFromStatus(status string) string {
	m := healthSuffixRe.FindStringSubmatch(strings.TrimSpace(status))
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

// errReadOnly is returned by the five runner.Composer write methods on
// HostContainers. A container with no compose project has no compose file, so
// stop/rm/pull/create/start cannot be expressed as a compose verb. The TUI
// gates every key that would reach these methods, so the sentinel is a
// backstop rather than a user-facing path.
var errReadOnly = errors.New("read-only: container is not managed by docker compose")

// dockerRunner is the ONLY local/remote variation point of HostContainers.
// It needs three methods rather than one *exec.Cmd builder because the three
// call shapes are mutually exclusive: run CAPTURES output and classifies the
// error, stream writes to an io.Writer and must NOT allocate a TTY, and tty
// must (the remote form splices -t, matching RemoteCompose.ExecCommand).
type dockerRunner interface {
	run(ctx context.Context, args ...string) ([]byte, error)
	stream(ctx context.Context, w io.Writer, args ...string) error
	// tty builds the interactive argv without running it. It returns no
	// error because both implementations are pure argv construction.
	tty(ctx context.Context, args ...string) *exec.Cmd
}

// HostContainers is a read-only runner.Composer over the containers on a docker
// host that carry no com.docker.compose.project label. Local and remote differ
// only in the dockerRunner seam, so the remote form inherits runRemoteDockerCmd's
// SSH transport classification and stderr capture for free.
type HostContainers struct {
	docker dockerRunner
}

// HostContainers implements every runner.Composer method: the five reads are
// real, the five writes refuse with errReadOnly. It also satisfies
// tui.ExecProvider (asserted in internal/tui/app_test.go, since compose cannot
// import tui) but deliberately NOT tui.ConfigProvider or tui.RollbackPreparer —
// a container with no compose file has no config to show and no snapshot to
// roll back to, so the c and R keys gate themselves.
var _ runner.Composer = (*HostContainers)(nil)

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

func (l localDockerRunner) tty(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "docker", args...)
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

func (rd remoteDockerRunner) tty(ctx context.Context, args ...string) *exec.Cmd {
	sshArgv := rd.r.sshArgs(
		[]string{"-t", "-S", rd.r.SocketPath, "-o", "ControlMaster=no"},
		rd.r.remoteDockerCmdString(args),
	)
	return exec.CommandContext(ctx, "ssh", sshArgv...)
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

// ReadOnlyComposer reports that every write method refuses. It satisfies
// tui.ReadOnlyComposer, which the TUI type-asserts to gate the write keys and
// the selection widgets. The method is named (rather than the interface being a
// method-less marker) because an empty interface is satisfied by every composer.
func (h *HostContainers) ReadOnlyComposer() bool { return true }

func (h *HostContainers) Stop(ctx context.Context, containers []string, w io.Writer) error {
	return errReadOnly
}

func (h *HostContainers) Remove(ctx context.Context, containers []string, w io.Writer) error {
	return errReadOnly
}

func (h *HostContainers) Pull(ctx context.Context, containers []string, w io.Writer) error {
	return errReadOnly
}

func (h *HostContainers) Create(ctx context.Context, containers []string, w io.Writer) error {
	return errReadOnly
}

func (h *HostContainers) Start(ctx context.Context, containers []string, w io.Writer) error {
	return errReadOnly
}

// CheckUpdates reports per-container "image update available" verdicts. See
// Compose.CheckUpdates for the full tri-state contract — a container is absent
// from the map whenever no definitive answer was reached, which the caller
// renders as a blank cell rather than a false negative.
//
// The name-to-image map comes from the Image field that `docker ps` already
// returns, so there is no second discovery call and no `docker compose config`
// (an unmanaged container has no compose file to read).
//
// Which systemic-failure cascade can fire is decided by the error shape the
// comparer emits, not by a flag. The registry cascade applies: an outage must
// surface as "registry unreachable" rather than a silently blank glyph column.
// The daemon cascade cannot, because compareImageDigest below passes a nil
// localErrWrap and so never emits errLocalImageInspect — right on both
// runners: locally a dead daemon fails the `docker ps` discovery call above
// long before any image inspect runs, and remotely there is no local daemon to
// diagnose. The transport abort applies on the remote runner and is inert on
// the local one, which never emits errSSHTransport.
//
// An untagged image is absorbed as absent rather than an error: `docker ps`
// reports an image ID for it, which either yields no RepoDigests (so the
// comparison is not definitive) or fails the registry inspect with a reference
// error that no cascade classifier matches.
func (h *HostContainers) CheckUpdates(ctx context.Context, services []string) (map[string]bool, error) {
	entries, err := h.unmanagedEntries(ctx)
	if err != nil {
		return nil, err
	}
	return scanImageUpdates(ctx, filterServices(hostImageMap(entries), services), h.compareImageDigest)
}

// bareImageIDRe matches the image-ID form `docker ps` reports for a container
// whose image carries no repository tag — a 12-char short ID or the full
// 64-char digest hex, with no registry, repository or tag.
var bareImageIDRe = regexp.MustCompile(`^[0-9a-f]{12}([0-9a-f]{52})?$`)

// hostImageMap builds the container-name → image map that scanImageUpdates
// consumes. Two kinds of entry are dropped:
//
//   - no image reference at all — an empty ref would turn `docker image inspect`
//     into a malformed call whose failure would pollute the cascade counters;
//   - a bare image ID — no registry can be asked about a ref with no repository,
//     so the verdict is the tri-state absent either way, and including it would
//     only buy a wasted registry round-trip per hand-started container and skew
//     the cascade ratios.
func hostImageMap(entries []hostPsEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		name := hostContainerName(e.Names)
		if name == "" || e.Image == "" || bareImageIDRe.MatchString(e.Image) {
			continue
		}
		out[name] = e.Image
	}
	return out
}

// compareImageDigest is HostContainers' binding of compareImageDigestVia: the
// dockerRunner seam plus a nil local-error wrapper, which is the local/remote
// difference collapsing into one binding. See CheckUpdates for why the daemon
// cascade the wrapper would drive is off on both paths.
func (h *HostContainers) compareImageDigest(ctx context.Context, image string) (bool, bool, error) {
	return compareImageDigestVia(ctx, h.docker, image, nil)
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

// hostContainerRunning reports whether a ps entry is up. State is the primary
// source, but the `docker ps` JSON keys are reflected from the CLI's own
// formatter and .State only exists on Docker CLI >= 20.10 — the same legacy
// hosts hostPsArgs uses the `{{json .}}` template for. Status carries the same
// fact ("Up 2 hours"), so an absent State falls back to it rather than
// rendering a live container as stopped and refusing x exec on it.
func hostContainerRunning(e hostPsEntry) bool {
	if e.State != "" {
		return e.State == "running"
	}
	return strings.HasPrefix(strings.TrimSpace(e.Status), "Up ")
}

// ContainerStatus maps each unmanaged container name to its status. There is no
// replica aggregation here — a host container is its own row — so the fields map
// one-to-one from the ps entry.
func (h *HostContainers) ContainerStatus(ctx context.Context) (map[string]runner.ServiceStatus, error) {
	entries, err := h.unmanagedEntries(ctx)
	if err != nil {
		return nil, err
	}
	status := make(map[string]runner.ServiceStatus, len(entries))
	for _, e := range entries {
		st := runner.ServiceStatus{
			Running: hostContainerRunning(e),
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

// hostStatsArgs is the host-wide stats argv. It takes the same `{{json .}}`
// template form hostPsArgs does, for the same reason: the bare `json` keyword
// only exists on Docker CLI >= 23.0, and this file supports the legacy hosts
// Detect() does. The output is identical either way — the CLI rewrites the
// keyword to exactly this template — so the two argv builders in this file
// agree on one host-version position rather than arguing opposite ones.
// AllContainerStats keeps the keyword; that is pre-existing and untouched here.
var hostStatsArgs = []string{"stats", "--no-stream", "--format", "{{json .}}"}

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
	// The join against an empty pair list is guaranteed empty, and
	// `docker stats --no-stream` is a ~1.5s host-wide call (a full SSH
	// round-trip remotely) that the 5s refresh tick would otherwise pay
	// forever. The guard buys that saved call — ContainerStatus needs no
	// counterpart, because its loop already yields an empty map for free.
	if len(entries) == 0 {
		return nil, nil
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

// Logs streams the logs of one host container to w. It goes through the
// stream seam, which wires BOTH Stdout and Stderr to w: `docker logs` writes
// the container's stderr to its own stderr, and that is where most application
// logs land (matching Compose.run).
//
// The stream seam must NOT allocate a TTY — a pseudo-terminal would let docker
// line-buffer and echo, corrupting the chunked reads the TUI log viewer makes.
func (h *HostContainers) Logs(ctx context.Context, service string, follow bool, tail int, w io.Writer) error {
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	if tail > 0 {
		args = append(args, "--tail", fmt.Sprintf("%d", tail))
	}
	args = append(args, service)
	return h.docker.stream(ctx, w, args...)
}

// ExecCommand returns an exec.Cmd that runs `docker exec -it <container> <command...>`.
// When command is empty it defaults to DefaultExecCommand, which tries bash and
// falls back to sh. Unlike `docker compose exec`, the plain `docker exec` form
// allocates no TTY of its own, so -it is explicit here.
//
// The caller attaches stdin/stdout/stderr and runs the command; the TUI does
// that through tea.ExecProcess. This is the tui.ExecProvider capability, not a
// runner.Composer method.
func (h *HostContainers) ExecCommand(ctx context.Context, service string, command []string) (*exec.Cmd, error) {
	if len(command) == 0 {
		command = DefaultExecCommand
	}
	args := append([]string{"exec", "-it", service}, command...)
	return h.docker.tty(ctx, args...), nil
}

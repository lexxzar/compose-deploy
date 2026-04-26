package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

// maxPortRangeExpansion caps the number of ports a single "lo-hi" range may expand
// into, so a malformed entry (e.g. "1-65535") cannot allocate an unbounded slice.
const maxPortRangeExpansion = 1024

// portKey identifies a unique published port mapping (Host, HostPort, ContainerPort,
// Protocol). Including Host means two distinct bind interfaces on the same port
// (e.g. 127.0.0.1:8080 and 192.168.1.10:8080) are NOT collapsed by primary dedup.
// True IPv4/IPv6 mirrors of the same (HostPort, ContainerPort, Protocol) are
// collapsed by a separate pass — see collapseIPv6Mirrors.
type portKey struct {
	host          string
	hostPort      int
	containerPort int
	protocol      string
}

// Project represents a running Docker Compose project discovered via `docker compose ls`.
type Project struct {
	Name      string
	Status    string // e.g. "running(3)"
	ConfigDir string // directory containing the compose file
}

// Compile-time interface satisfaction checks.
var _ runner.Composer = (*Compose)(nil)

// composeFile candidates for HasComposeFile detection.
var composeFiles = []string{
	"compose.yml",
	"compose.yaml",
	"docker-compose.yml",
	"docker-compose.yaml",
}

// HasComposeFile reports whether dir contains a recognized compose file.
func HasComposeFile(dir string) bool {
	for _, name := range composeFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// ListProjects returns all Docker Compose projects on the system, including stopped ones.
// It respects the Standalone field to use the correct binary.
func (c *Compose) ListProjects(ctx context.Context) ([]Project, error) {
	cmd := c.command(ctx, "ls", "-a", "--format", "json")
	var out []byte
	var err error
	if c.outputCmd != nil {
		out, err = c.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", withStderr(err))
	}
	return parseProjects(out)
}

// lsEntry matches the JSON schema of `docker compose ls --format json`.
type lsEntry struct {
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	ConfigFiles string `json:"ConfigFiles"`
}

// parseProjects parses the JSON output of `docker compose ls --format json`.
func parseProjects(data []byte) ([]Project, error) {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "[]" {
		return nil, nil
	}

	var entries []lsEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing project list: %w", err)
	}

	projects := make([]Project, 0, len(entries))
	for _, e := range entries {
		configFile := e.ConfigFiles
		if i := strings.Index(configFile, ","); i >= 0 {
			configFile = configFile[:i]
		}
		projects = append(projects, Project{
			Name:      e.Name,
			Status:    e.Status,
			ConfigDir: filepath.Dir(configFile),
		})
	}

	sortProjects(projects)
	return projects, nil
}

// sortProjects sorts projects by name, case-insensitive.
func sortProjects(projects []Project) {
	for i := 1; i < len(projects); i++ {
		for j := i; j > 0 && strings.ToLower(projects[j].Name) < strings.ToLower(projects[j-1].Name); j-- {
			projects[j], projects[j-1] = projects[j-1], projects[j]
		}
	}
}

// findComposeFile returns the path to the first recognized compose file in ProjectDir.
// It probes the same candidates as HasComposeFile but returns the full path.
func (c *Compose) findComposeFile() (string, error) {
	for _, name := range composeFiles {
		p := filepath.Join(c.ProjectDir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no compose file found in %s", c.ProjectDir)
}

// ConfigFile returns the raw content of the compose file.
func (c *Compose) ConfigFile(ctx context.Context) ([]byte, error) {
	path, err := c.findComposeFile()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// ConfigResolved returns the interpolated/resolved compose config
// (output of `docker compose config`).
func (c *Compose) ConfigResolved(ctx context.Context) ([]byte, error) {
	cmd := c.command(ctx, "config")
	var out []byte
	var err error
	if c.outputCmd != nil {
		out, err = c.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, withStderr(err)
	}
	return out, nil
}

// EditCommand returns an exec.Cmd that opens the compose file in the user's editor.
// It checks $EDITOR, then $VISUAL, then falls back to "vi".
// Multi-word values like "code --wait" are split into executable + args.
func (c *Compose) EditCommand(ctx context.Context) (*exec.Cmd, error) {
	path, err := c.findComposeFile()
	if err != nil {
		return nil, err
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		parts = []string{"vi"}
	}
	args := append(parts[1:], path)
	return exec.CommandContext(ctx, parts[0], args...), nil
}

// ValidateConfig runs `docker compose config --quiet` and returns any error
// with stderr captured so users see why validation failed.
func (c *Compose) ValidateConfig(ctx context.Context) error {
	cmd := c.command(ctx, "config", "--quiet")
	if c.outputCmd != nil {
		_, err := c.outputCmd(cmd)
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// Compose wraps Docker Compose v2 CLI calls.
type Compose struct {
	ProjectDir string // directory containing docker-compose.yml
	UID        string // "uid:gid" for CURRENT_UID env var
	Standalone bool   // use standalone docker-compose binary instead of docker compose plugin

	detected bool // true after Detect() or SetStandalone() has been called

	// testing hooks; nil = use real exec
	runCmd    func(*exec.Cmd) error
	outputCmd func(*exec.Cmd) ([]byte, error)
}

// New creates a Compose instance with the current user's UID:GID.
func New(projectDir string) *Compose {
	return &Compose{
		ProjectDir: projectDir,
		UID:        fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
	}
}

// SetTestHooks sets the testing hooks for command execution.
func (c *Compose) SetTestHooks(run func(*exec.Cmd) error, output func(*exec.Cmd) ([]byte, error)) {
	c.runCmd = run
	c.outputCmd = output
}

// Detect probes for the docker compose variant available on the system.
// It tries "docker compose version" first (plugin mode), then falls back to
// "docker-compose version" (standalone binary). Sets Standalone accordingly.
// No-ops if already detected. Returns an error if neither variant is found.
func (c *Compose) Detect(ctx context.Context) error {
	if c.detected {
		return nil
	}

	// Try plugin mode: docker compose version
	pluginCmd := exec.CommandContext(ctx, "docker", "compose", "version")
	var pluginErr error
	if c.outputCmd != nil {
		_, pluginErr = c.outputCmd(pluginCmd)
	} else {
		_, pluginErr = pluginCmd.Output()
	}
	if pluginErr == nil {
		c.Standalone = false
		c.detected = true
		return nil
	}

	// Try standalone mode: docker-compose version
	standaloneCmd := exec.CommandContext(ctx, "docker-compose", "version")
	var standaloneErr error
	if c.outputCmd != nil {
		_, standaloneErr = c.outputCmd(standaloneCmd)
	} else {
		_, standaloneErr = standaloneCmd.Output()
	}
	if standaloneErr == nil {
		c.Standalone = true
		c.detected = true
		return nil
	}

	return fmt.Errorf("neither 'docker compose' nor 'docker-compose' found")
}

// SetStandalone sets the Standalone flag and marks detection as complete.
func (c *Compose) SetStandalone(standalone bool) {
	c.Standalone = standalone
	c.detected = true
}

// ListServices returns the list of services defined in the compose file.
func (c *Compose) ListServices(ctx context.Context) ([]string, error) {
	cmd := c.command(ctx, "config", "--services")
	var out []byte
	var err error
	if c.outputCmd != nil {
		out, err = c.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", withStderr(err))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var services []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			services = append(services, l)
		}
	}
	return services, nil
}

// Stop stops the specified containers (or all if containers is empty).
func (c *Compose) Stop(ctx context.Context, containers []string, w io.Writer) error {
	return c.run(ctx, w, append([]string{"stop"}, containers...)...)
}

// Remove removes the specified containers with -f (force).
func (c *Compose) Remove(ctx context.Context, containers []string, w io.Writer) error {
	return c.run(ctx, w, append([]string{"rm", "-f"}, containers...)...)
}

// Pull pulls images for the specified containers.
func (c *Compose) Pull(ctx context.Context, containers []string, w io.Writer) error {
	return c.run(ctx, w, append([]string{"pull"}, containers...)...)
}

// Create creates containers without starting them (up --no-start).
func (c *Compose) Create(ctx context.Context, containers []string, w io.Writer) error {
	return c.run(ctx, w, append([]string{"up", "--no-start"}, containers...)...)
}

// Start starts the specified containers.
func (c *Compose) Start(ctx context.Context, containers []string, w io.Writer) error {
	return c.run(ctx, w, append([]string{"start"}, containers...)...)
}

// withStderr appends any captured stderr to an exec.ExitError so the
// caller sees the actual diagnostic message, not just the exit code.
func withStderr(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return err
}

func (c *Compose) command(ctx context.Context, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if c.Standalone {
		cmd = exec.CommandContext(ctx, "docker-compose", args...)
	} else {
		cmd = exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
	}
	cmd.Env = append(os.Environ(), "CURRENT_UID="+c.UID)
	if c.ProjectDir != "" {
		cmd.Dir = c.ProjectDir
	}
	return cmd
}

// psEntry matches the JSON schema of `docker compose ps --format json`.
type psEntry struct {
	Service    string        `json:"Service"`
	State      string        `json:"State"`
	Health     string        `json:"Health"`
	CreatedAt  string        `json:"CreatedAt"`
	Status     string        `json:"Status"`
	Publishers []psPublisher `json:"Publishers"`
	Ports      string        `json:"Ports"`
}

// psPublisher matches a single entry in the `Publishers` array of `docker compose ps --format json` (Compose v2).
type psPublisher struct {
	URL           string `json:"URL"`
	TargetPort    int    `json:"TargetPort"`
	PublishedPort int    `json:"PublishedPort"`
	Protocol      string `json:"Protocol"`
}

// normalizeHost normalizes a bind-host string from a published port: an empty input is
// rewritten to "0.0.0.0" (Compose's default for unspecified bind) and a bracketed-IPv6
// form ("[::]", "[::1]") has its brackets stripped. All other inputs are returned as-is.
// Used by extractPorts and splitHostPort to share a single normalization contract.
func normalizeHost(s string) string {
	if s == "" {
		return "0.0.0.0"
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") && len(s) >= 2 {
		return s[1 : len(s)-1]
	}
	return s
}

// extractPorts converts the Publishers array of a single ps entry to a slice of runner.Port.
// Skips entries with PublishedPort == 0 (those are `expose:`-only, not actually published).
// Distinct bind interfaces on the same (HostPort, ContainerPort, Protocol) are preserved
// (e.g. 127.0.0.1:8080->80 and 192.168.1.10:8080->80 both survive). Only the IPv4/IPv6
// wildcard pair "0.0.0.0" ↔ "::" on the same (HostPort, ContainerPort, Protocol) tuple
// is collapsed (matching Compose's default dual-stack bind behavior); the "::" entry is
// dropped in favor of "0.0.0.0". Non-wildcard IPv6 hosts (e.g. "::1", "2001:db8::1")
// are distinct user-visible binds and survive intact.
// An empty/missing URL is normalized to "0.0.0.0" (Compose's default for unspecified bind).
func extractPorts(entry psEntry) []runner.Port {
	if len(entry.Publishers) == 0 {
		return nil
	}
	// preserve insertion order; map tracks index into ports slice for full-tuple dedup
	idx := make(map[portKey]int)
	var ports []runner.Port
	for _, pub := range entry.Publishers {
		if pub.PublishedPort == 0 {
			continue
		}
		mergePort(&ports, idx, runner.Port{
			Host:          normalizeHost(pub.URL),
			HostPort:      pub.PublishedPort,
			ContainerPort: pub.TargetPort,
			Protocol:      pub.Protocol,
		})
	}
	return collapseIPv6Mirrors(ports)
}

// mergePort appends p to ports unless an entry with the same (Host, HostPort,
// ContainerPort, Protocol) tuple already exists, in which case it is dropped (exact
// duplicate). idx tracks the index into ports for each full-tuple key. Both are
// mutated in place. IPv4/IPv6 mirror collapse is handled separately by
// collapseIPv6Mirrors after all entries have been merged.
func mergePort(ports *[]runner.Port, idx map[portKey]int, p runner.Port) {
	k := portKey{host: p.Host, hostPort: p.HostPort, containerPort: p.ContainerPort, protocol: p.Protocol}
	if _, ok := idx[k]; ok {
		return
	}
	idx[k] = len(*ports)
	*ports = append(*ports, p)
}

// collapseIPv6Mirrors collapses the IPv4/IPv6 wildcard mirror pair that Compose
// emits when a port is published to all interfaces on a dual-stack host: the IPv6
// unspecified address "::" is dropped when an IPv4 unspecified address "0.0.0.0"
// sibling exists on the same (HostPort, ContainerPort, Protocol) tuple. Only the
// wildcard pair "0.0.0.0" ↔ "::" is treated as a mirror — all other host strings
// (including IPv6 loopback "::1", link-local "fe80::1", or explicit IPv6 addresses
// like "2001:db8::1") are distinct user-visible binds and must survive even when an
// IPv4 entry exists on the same tuple. Likewise, multiple non-wildcard IPv6 entries
// on the same tuple do not collapse against each other (full-tuple dedup in
// mergePort already removes exact (Host, HostPort, ContainerPort, Protocol)
// repeats). Stable order of survivors is preserved.
func collapseIPv6Mirrors(ports []runner.Port) []runner.Port {
	if len(ports) < 2 {
		return ports
	}
	type tupleKey struct {
		hostPort      int
		containerPort int
		protocol      string
	}
	hasIPv4Wildcard := make(map[tupleKey]bool)
	for _, p := range ports {
		if p.Host == "0.0.0.0" {
			hasIPv4Wildcard[tupleKey{p.HostPort, p.ContainerPort, p.Protocol}] = true
		}
	}
	out := make([]runner.Port, 0, len(ports))
	for _, p := range ports {
		if p.Host == "::" && hasIPv4Wildcard[tupleKey{p.HostPort, p.ContainerPort, p.Protocol}] {
			// drop: IPv6 wildcard mirror of an IPv4 wildcard sibling
			continue
		}
		out = append(out, p)
	}
	return out
}

// isIPv6Host reports whether host contains a colon — i.e., is an IPv6 address in any
// form (wildcard "::", loopback "::1", link-local "fe80::1", etc.). Used for sort
// ordering and bracket-wrapping in display formatting.
func isIPv6Host(host string) bool {
	return strings.Contains(host, ":")
}

// dedupAndSortPorts dedupes ports across replicas by the (Host, HostPort,
// ContainerPort, Protocol) tuple and returns a slice sorted ascending by
// HostPort, then by ContainerPort, then by Protocol, then by Host for stable
// output. Distinct bind interfaces on the same port survive (e.g.
// 127.0.0.1:8080 and 192.168.1.10:8080). Only the IPv4 unspecified address
// "0.0.0.0" and the IPv6 unspecified address "::" are treated as mirrors —
// when both wildcards appear on the same (HostPort, ContainerPort, Protocol)
// tuple, the "::" entry is dropped. All other host strings are distinct
// user-visible binds and survive (e.g. IPv6 loopback "::1" or explicit IPv6
// addresses are NOT collapsed against an IPv4 sibling). This matches the
// behavior of extractPorts and parsePortsString.
func dedupAndSortPorts(ports []runner.Port) []runner.Port {
	if len(ports) == 0 {
		return nil
	}
	idx := make(map[portKey]int)
	out := make([]runner.Port, 0, len(ports))
	for _, p := range ports {
		mergePort(&out, idx, p)
	}
	out = collapseIPv6Mirrors(out)
	sort.SliceStable(out, func(i, j int) bool { return portLess(out[i], out[j]) })
	return out
}

// portLess defines stable sort ordering for runner.Port: HostPort ascending,
// then ContainerPort, then Protocol, then Host.
func portLess(a, b runner.Port) bool {
	if a.HostPort != b.HostPort {
		return a.HostPort < b.HostPort
	}
	if a.ContainerPort != b.ContainerPort {
		return a.ContainerPort < b.ContainerPort
	}
	if a.Protocol != b.Protocol {
		return a.Protocol < b.Protocol
	}
	return a.Host < b.Host
}

// parsePortsString is a fallback parser for Compose's `Ports` text field, used when
// the structured `Publishers` array is empty/missing (older Compose versions).
//
// It accepts a comma-separated list of entries shaped like:
//
//	0.0.0.0:8080->80/tcp
//	:::8080->80/tcp
//	[::]:8080->80/tcp
//	[::1]:8443->443/tcp
//	0.0.0.0:1812->1812/udp
//	0.0.0.0:8080-8090->8080-8090/tcp     (port ranges, expanded 1:1)
//
// Entries without a `->` (e.g. plain "80/tcp" exposed-only) are skipped. Malformed
// entries are skipped silently (best-effort). Port ranges (e.g. `8080-8090`) are
// expanded to one runner.Port per (host_port, container_port) pair when the host and
// container range widths match; mismatched widths cause the entry to be skipped.
// Distinct bind interfaces on the same port survive (e.g. 127.0.0.1:8080 and
// 192.168.1.10:8080). Only the IPv4/IPv6 wildcard pair ("0.0.0.0" ↔ "::") on the
// same (HostPort, ContainerPort, Protocol) tuple is collapsed to the IPv4 form;
// non-wildcard IPv6 hosts (e.g. "::1", "2001:db8::1") survive intact.
func parsePortsString(text string) []runner.Port {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	idx := make(map[portKey]int)
	var ports []runner.Port
	for _, raw := range strings.Split(text, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		// Must contain "->"; otherwise it's an internal-only entry like "80/tcp".
		arrowIdx := strings.Index(entry, "->")
		if arrowIdx < 0 {
			continue
		}
		left := entry[:arrowIdx]
		right := entry[arrowIdx+2:]

		// Parse host portion: either "host:port[-port]" or "[ipv6]:port[-port]" or
		// "::port[-port]" (Compose's IPv6 unspecified shorthand).
		host, hostPortStr, ok := splitHostPort(left)
		if !ok {
			continue
		}
		hostPorts, ok := parsePortOrRange(hostPortStr)
		if !ok {
			continue
		}

		// Parse container portion: "port[-port]/proto" or "port[-port]".
		cpStr := right
		proto := ""
		if slash := strings.Index(right, "/"); slash >= 0 {
			cpStr = right[:slash]
			proto = right[slash+1:]
		}
		containerPorts, ok := parsePortOrRange(cpStr)
		if !ok {
			continue
		}

		// Range widths must match for a 1:1 mapping (e.g. 8080-8090 -> 8080-8090).
		if len(hostPorts) != len(containerPorts) {
			continue
		}

		host = normalizeHost(host)

		for i := range hostPorts {
			mergePort(&ports, idx, runner.Port{
				Host:          host,
				HostPort:      hostPorts[i],
				ContainerPort: containerPorts[i],
				Protocol:      proto,
			})
		}
	}
	return collapseIPv6Mirrors(ports)
}

// parsePortOrRange parses a port number or a "lo-hi" inclusive range and returns the
// expanded slice of port numbers. Returns ok=false if the input is malformed, contains
// non-positive ports, or the range is reversed (lo > hi). Ranges are capped at 1024
// ports to avoid unbounded expansion from malformed input.
func parsePortOrRange(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	if dash := strings.Index(s, "-"); dash >= 0 {
		lo, err := strconv.Atoi(s[:dash])
		if err != nil || lo <= 0 {
			return nil, false
		}
		hi, err := strconv.Atoi(s[dash+1:])
		if err != nil || hi <= 0 || hi < lo {
			return nil, false
		}
		if hi-lo+1 > maxPortRangeExpansion {
			return nil, false
		}
		out := make([]int, 0, hi-lo+1)
		for p := lo; p <= hi; p++ {
			out = append(out, p)
		}
		return out, true
	}
	p, err := strconv.Atoi(s)
	if err != nil || p <= 0 {
		return nil, false
	}
	return []int{p}, true
}

// splitHostPort splits a "host:port" or "[ipv6]:port" string into (host, port).
// Returns ok=false if the input has no port separator or is malformed.
// IPv6 brackets are stripped from the returned host. Bare-IPv6 forms (e.g. "::8080",
// "::1:8443", "2001:db8::1:8080") are recognized: when the input contains 2 or more
// colons and is not bracketed, the last colon is treated as the host:port separator
// and everything before it (e.g. "::", "::1", "2001:db8::1") is returned as the host.
// The 2-colon "::port" form (Compose's IPv6 unspecified shorthand, e.g. "::8080") is
// treated as host="::" / port="8080".
func splitHostPort(s string) (host, port string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	// Bracketed IPv6: "[..]:port"
	if strings.HasPrefix(s, "[") {
		closeIdx := strings.Index(s, "]")
		if closeIdx < 0 || closeIdx+1 >= len(s) || s[closeIdx+1] != ':' {
			return "", "", false
		}
		host = s[1:closeIdx]
		port = s[closeIdx+2:]
		if port == "" {
			return "", "", false
		}
		return host, port, true
	}
	lastColon := strings.LastIndex(s, ":")
	if lastColon < 0 {
		return "", "", false
	}
	port = s[lastColon+1:]
	if port == "" {
		return "", "", false
	}
	// Special case: "::port" (e.g. "::8080") — bare IPv6 unspecified shorthand.
	// LastIndex of ":" lands at index 1, leaving host=":". Detect this and rewrite
	// to host="::" so the IPv6 wildcard form parses consistently with ":::port".
	if lastColon == 1 && s[0] == ':' {
		return "::", port, true
	}
	host = s[:lastColon]
	return host, port, true
}

// ContainerStatus returns a map of service name to ServiceStatus.
func (c *Compose) ContainerStatus(ctx context.Context) (map[string]runner.ServiceStatus, error) {
	cmd := c.command(ctx, "ps", "-a", "--format", "json")
	var out []byte
	var err error
	if c.outputCmd != nil {
		out, err = c.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("listing container status: %w", withStderr(err))
	}
	return parseContainerStatus(out)
}

// healthPriority returns a numeric priority for health values.
// Higher = worse. Used to pick worst-case health for scaled services.
func healthPriority(h string) int {
	switch h {
	case "unhealthy":
		return 3
	case "starting":
		return 2
	case "healthy":
		return 1
	default:
		return 0 // no healthcheck
	}
}

// parseCreatedAt attempts to parse Docker's CreatedAt timestamp string.
// Docker uses the format "2006-01-02 15:04:05 -0700 MST".
// Returns the parsed time and whether parsing succeeded.
func parseCreatedAt(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// parseContainerStatus parses the JSON output of `docker compose ps --format json`.
// Docker Compose v2.21+ outputs a JSON array; older versions output NDJSON (one object per line).
func parseContainerStatus(data []byte) (map[string]runner.ServiceStatus, error) {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "[]" {
		return nil, nil
	}

	var entries []psEntry

	if strings.HasPrefix(s, "[") {
		// JSON array format (Docker Compose v2.21+)
		if err := json.Unmarshal([]byte(s), &entries); err != nil {
			return nil, fmt.Errorf("parsing container status: %w", err)
		}
	} else {
		// NDJSON format (older Docker Compose)
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var entry psEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				return nil, fmt.Errorf("parsing container status: %w", err)
			}
			entries = append(entries, entry)
		}
	}

	// Track aggregation state for scaled services.
	type svcAgg struct {
		oldestCreated      time.Time     // oldest CreatedAt across all replicas
		oldestCreatedValid bool          //
		longestUpDur       time.Duration // longest actual uptime among running replicas
		longestUpStr       string        // uptime string of the longest-running replica
		longestFromRunning bool          // true if longestUpStr came from a running replica
		ports              []runner.Port // accumulated published ports across all replicas (deduped/sorted later)
	}
	agg := make(map[string]*svcAgg)

	status := make(map[string]runner.ServiceStatus)
	for _, entry := range entries {
		if entry.Service == "" {
			continue
		}
		prev := status[entry.Service]
		prev.Running = prev.Running || entry.State == "running"
		if healthPriority(entry.Health) > healthPriority(prev.Health) {
			prev.Health = entry.Health
		}

		// Initialize aggregation tracking for this service.
		a := agg[entry.Service]
		if a == nil {
			a = &svcAgg{}
			agg[entry.Service] = a
		}

		// Parse CreatedAt for this replica.
		entryCreated, entryValid := parseCreatedAt(entry.CreatedAt)
		entryUptime := formatUptime(entry.Status)

		// Track oldest CreatedAt across all replicas (for the Created column).
		if entryValid {
			if !a.oldestCreatedValid || entryCreated.Before(a.oldestCreated) {
				a.oldestCreated = entryCreated
				a.oldestCreatedValid = true
			}
		}

		// Track longest-running replica (for the Uptime column).
		// Use entry.State to determine running status rather than parsing Status text.
		// Running replicas always take priority over restarting ones.
		if entry.State == "running" && entryUptime != "" {
			dur := parseUptimeDuration(entryUptime)
			if !a.longestFromRunning || dur > a.longestUpDur {
				a.longestUpDur = dur
				a.longestUpStr = entryUptime
				a.longestFromRunning = true
			}
		} else if entryUptime == "restarting" && a.longestUpStr == "" {
			a.longestUpStr = entryUptime
		}

		// Accumulate ports for this replica. Prefer the structured Publishers field
		// (Compose v2); fall back to the Ports text string when Publishers is empty.
		if replicaPorts := extractPorts(entry); len(replicaPorts) > 0 {
			a.ports = append(a.ports, replicaPorts...)
		} else if entry.Ports != "" {
			a.ports = append(a.ports, parsePortsString(entry.Ports)...)
		}

		status[entry.Service] = prev
	}

	// Apply aggregated Created, Uptime, and Ports to final status.
	for svc, a := range agg {
		st := status[svc]
		if a.oldestCreatedValid {
			st.Created = a.oldestCreated.Format("2006-01-02 15:04")
		}
		st.Uptime = a.longestUpStr
		st.Ports = dedupAndSortPorts(a.ports)
		status[svc] = st
	}

	return status, nil
}

// DefaultExecCommand is the shell command used when no explicit command is given to ExecCommand.
// It uses /bin/sh as outer binary (works on Alpine/minimal images) and tries bash first for better UX.
// The bash existence check is done separately (command -v) so that exec bash runs with a clean
// stderr — redirecting stderr on exec would silence bash's entire session, breaking prompt and
// tab completion.
var DefaultExecCommand = []string{"/bin/sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}

// ExecCommand returns an exec.Cmd that runs `docker compose exec <service> <command...>`.
// When command is empty, it defaults to DefaultExecCommand which tries bash, falling back to sh.
// The caller is responsible for attaching stdin/stdout/stderr and running the command.
func (c *Compose) ExecCommand(ctx context.Context, service string, command []string) (*exec.Cmd, error) {
	if len(command) == 0 {
		command = DefaultExecCommand
	}
	args := append([]string{"exec", service}, command...)
	cmd := c.command(ctx, args...)
	return cmd, nil
}

// Logs streams docker compose logs for a single service.
func (c *Compose) Logs(ctx context.Context, service string, follow bool, tail int, w io.Writer) error {
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	if tail > 0 {
		args = append(args, "--tail", fmt.Sprintf("%d", tail))
	}
	args = append(args, service)
	return c.run(ctx, w, args...)
}

func (c *Compose) run(ctx context.Context, w io.Writer, args ...string) error {
	cmd := c.command(ctx, args...)
	cmd.Stdout = w
	cmd.Stderr = w
	if c.runCmd != nil {
		return c.runCmd(cmd)
	}
	return cmd.Run()
}

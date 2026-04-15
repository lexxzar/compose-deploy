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
	"strings"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

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
	if c.outputCmd != nil {
		return c.outputCmd(cmd)
	}
	return cmd.Output()
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
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
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

	status := make(map[string]runner.ServiceStatus)
	for _, entry := range entries {
		if entry.Service != "" {
			prev := status[entry.Service]
			prev.Running = prev.Running || entry.State == "running"
			if healthPriority(entry.Health) > healthPriority(prev.Health) {
				prev.Health = entry.Health
			}
			status[entry.Service] = prev
		}
	}

	return status, nil
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

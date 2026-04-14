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

// execListProjects is the function that executes `docker compose ls`. Overridable in tests.
var execListProjects = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", "compose", "ls", "-a", "--format", "json").Output()
}

// ListProjects returns all Docker Compose projects on the system, including stopped ones.
func ListProjects(ctx context.Context) ([]Project, error) {
	out, err := execListProjects(ctx)
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

// Compose wraps Docker Compose v2 CLI calls.
type Compose struct {
	ProjectDir string // directory containing docker-compose.yml
	UID        string // "uid:gid" for CURRENT_UID env var

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
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
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

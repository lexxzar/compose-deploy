package compose

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

// Compile-time interface satisfaction checks.
var _ runner.Composer = (*RemoteCompose)(nil)

// remoteComposeFiles lists the compose file candidates to probe on the remote host.
var remoteComposeFiles = []string{
	"compose.yml",
	"compose.yaml",
	"docker-compose.yml",
	"docker-compose.yaml",
}

// RemoteCompose implements runner.Composer by wrapping docker compose commands
// in SSH calls over a ControlMaster connection.
type RemoteCompose struct {
	Host       string
	ProjectDir string
	SocketPath string
	Standalone bool // use standalone docker-compose binary on the remote host

	detected bool // true after Detect() or SetStandalone() has been called

	// testing hooks; nil = use real exec
	runCmd    func(*exec.Cmd) error
	outputCmd func(*exec.Cmd) ([]byte, error)
}

// NewRemote creates a RemoteCompose instance. The socket path is deterministic
// based on the host and scoped to the current process PID.
func NewRemote(host, projectDir string) *RemoteCompose {
	h := sha256.Sum256([]byte(host))
	socket := fmt.Sprintf("/tmp/cdeploy-ctrl-%x-%d", h[:6], os.Getpid())
	return &RemoteCompose{
		Host:       host,
		ProjectDir: projectDir,
		SocketPath: socket,
	}
}

// SetTestHooks sets the testing hooks for command execution.
func (r *RemoteCompose) SetTestHooks(run func(*exec.Cmd) error, output func(*exec.Cmd) ([]byte, error)) {
	r.runCmd = run
	r.outputCmd = output
}

// Detect probes for the docker compose variant available on the remote host.
// It builds its own SSH probe command directly (not via remoteCommand()) to
// avoid unnecessary CURRENT_UID and cd prefix. Tries "docker compose version"
// first, then "docker-compose version". No-ops if already detected.
func (r *RemoteCompose) Detect(ctx context.Context) error {
	if r.detected {
		return nil
	}

	// Try plugin mode: docker compose version
	pluginCmd := exec.CommandContext(ctx, "ssh",
		"-S", r.SocketPath,
		"-o", "ControlMaster=no",
		r.Host,
		"docker compose version",
	)
	var pluginErr error
	if r.outputCmd != nil {
		_, pluginErr = r.outputCmd(pluginCmd)
	} else {
		_, pluginErr = pluginCmd.Output()
	}
	if pluginErr == nil {
		r.Standalone = false
		r.detected = true
		return nil
	}

	// Try standalone mode: docker-compose version
	standaloneCmd := exec.CommandContext(ctx, "ssh",
		"-S", r.SocketPath,
		"-o", "ControlMaster=no",
		r.Host,
		"docker-compose version",
	)
	var standaloneErr error
	if r.outputCmd != nil {
		_, standaloneErr = r.outputCmd(standaloneCmd)
	} else {
		_, standaloneErr = standaloneCmd.Output()
	}
	if standaloneErr == nil {
		r.Standalone = true
		r.detected = true
		return nil
	}

	return fmt.Errorf("neither 'docker compose' nor 'docker-compose' found on host")
}

// SetStandalone sets the Standalone flag and marks detection as complete.
func (r *RemoteCompose) SetStandalone(standalone bool) {
	r.Standalone = standalone
	r.detected = true
}

// shellEscape wraps an argument in single quotes for safe SSH transport.
func shellEscape(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

// ConnectCmd returns the SSH ControlMaster connect command without running it.
// The TUI uses this with tea.ExecProcess to give SSH full terminal access for
// password prompts.
func (r *RemoteCompose) ConnectCmd(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, "ssh",
		"-fNM",
		"-S", r.SocketPath,
		r.Host,
	)
}

// Connect establishes the ControlMaster connection by running ConnectCmd.
func (r *RemoteCompose) Connect(ctx context.Context) error {
	cmd := r.ConnectCmd(ctx)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if r.runCmd != nil {
		return r.runCmd(cmd)
	}
	return cmd.Run()
}

// Close tears down the ControlMaster connection.
// Uses a 5-second timeout to prevent hanging on stale sockets.
func (r *RemoteCompose) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh",
		"-S", r.SocketPath,
		"-O", "exit",
		r.Host,
	)
	if r.runCmd != nil {
		return r.runCmd(cmd)
	}
	return cmd.Run()
}

// remoteCommand builds an ssh command that runs a docker compose subcommand
// on the remote host via the ControlMaster socket.
func (r *RemoteCompose) remoteCommand(ctx context.Context, args ...string) *exec.Cmd {
	var escaped []string
	for _, a := range args {
		escaped = append(escaped, shellEscape(a))
	}

	composeBin := "docker compose"
	if r.Standalone {
		composeBin = "docker-compose"
	}

	remoteCmd := fmt.Sprintf("CURRENT_UID=$(id -u):$(id -g) %s %s",
		composeBin, strings.Join(escaped, " "))

	if r.ProjectDir != "" {
		remoteCmd = fmt.Sprintf("cd %s && %s", shellEscape(r.ProjectDir), remoteCmd)
	}

	return exec.CommandContext(ctx, "ssh",
		"-S", r.SocketPath,
		"-o", "ControlMaster=no",
		r.Host,
		remoteCmd,
	)
}

func (r *RemoteCompose) run(ctx context.Context, w io.Writer, args ...string) error {
	cmd := r.remoteCommand(ctx, args...)
	cmd.Stdout = w
	cmd.Stderr = w
	if r.runCmd != nil {
		return r.runCmd(cmd)
	}
	return cmd.Run()
}

// Stop stops the specified containers (or all if containers is empty).
func (r *RemoteCompose) Stop(ctx context.Context, containers []string, w io.Writer) error {
	return r.run(ctx, w, append([]string{"stop"}, containers...)...)
}

// Remove removes the specified containers with -f (force).
func (r *RemoteCompose) Remove(ctx context.Context, containers []string, w io.Writer) error {
	return r.run(ctx, w, append([]string{"rm", "-f"}, containers...)...)
}

// Pull pulls images for the specified containers.
func (r *RemoteCompose) Pull(ctx context.Context, containers []string, w io.Writer) error {
	return r.run(ctx, w, append([]string{"pull"}, containers...)...)
}

// Create creates containers without starting them (up --no-start).
func (r *RemoteCompose) Create(ctx context.Context, containers []string, w io.Writer) error {
	return r.run(ctx, w, append([]string{"up", "--no-start"}, containers...)...)
}

// Start starts the specified containers.
func (r *RemoteCompose) Start(ctx context.Context, containers []string, w io.Writer) error {
	return r.run(ctx, w, append([]string{"start"}, containers...)...)
}

// Logs streams docker compose logs for a single service on the remote host.
func (r *RemoteCompose) Logs(ctx context.Context, service string, follow bool, tail int, w io.Writer) error {
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	if tail > 0 {
		args = append(args, "--tail", fmt.Sprintf("%d", tail))
	}
	args = append(args, service)
	return r.run(ctx, w, args...)
}

// ListServices returns the list of services defined in the remote compose file.
func (r *RemoteCompose) ListServices(ctx context.Context) ([]string, error) {
	cmd := r.remoteCommand(ctx, "config", "--services")
	var out []byte
	var err error
	if r.outputCmd != nil {
		out, err = r.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("listing remote services: %w", withStderr(err))
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

// ContainerStatus returns a map of service name to ServiceStatus on the remote host.
func (r *RemoteCompose) ContainerStatus(ctx context.Context) (map[string]runner.ServiceStatus, error) {
	cmd := r.remoteCommand(ctx, "ps", "-a", "--format", "json")
	var out []byte
	var err error
	if r.outputCmd != nil {
		out, err = r.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("listing remote container status: %w", withStderr(err))
	}
	return parseContainerStatus(out)
}

// findRemoteComposeFile runs a single SSH command that probes all compose file
// candidates and returns the first match. Avoids multiple SSH round-trips.
func (r *RemoteCompose) findRemoteComposeFile(ctx context.Context) (string, error) {
	// Build: for f in compose.yml compose.yaml ...; do test -f "$projDir/$f" && echo "$f" && break; done
	var testExpr string
	if r.ProjectDir != "" {
		testExpr = fmt.Sprintf(
			"for f in %s; do test -f %s/$f && echo $f && break; done",
			strings.Join(remoteComposeFiles, " "),
			shellEscape(r.ProjectDir),
		)
	} else {
		testExpr = fmt.Sprintf(
			"for f in %s; do test -f $f && echo $f && break; done",
			strings.Join(remoteComposeFiles, " "),
		)
	}

	cmd := exec.CommandContext(ctx, "ssh",
		"-S", r.SocketPath,
		"-o", "ControlMaster=no",
		r.Host,
		testExpr,
	)
	var out []byte
	var err error
	if r.outputCmd != nil {
		out, err = r.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return "", fmt.Errorf("finding remote compose file: %w", withStderr(err))
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("no compose file found on remote host")
	}
	return name, nil
}

// ConfigFile returns the raw content of the compose file on the remote host.
func (r *RemoteCompose) ConfigFile(ctx context.Context) ([]byte, error) {
	name, err := r.findRemoteComposeFile(ctx)
	if err != nil {
		return nil, err
	}
	filePath := name
	if r.ProjectDir != "" {
		filePath = r.ProjectDir + "/" + name
	}

	cmd := exec.CommandContext(ctx, "ssh",
		"-S", r.SocketPath,
		"-o", "ControlMaster=no",
		r.Host,
		"cat "+shellEscape(filePath),
	)
	var out []byte
	if r.outputCmd != nil {
		out, err = r.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("reading remote compose file: %w", withStderr(err))
	}
	return out, nil
}

// ConfigResolved returns the interpolated/resolved compose config on the remote host
// (output of `docker compose config`).
func (r *RemoteCompose) ConfigResolved(ctx context.Context) ([]byte, error) {
	cmd := r.remoteCommand(ctx, "config")
	var out []byte
	var err error
	if r.outputCmd != nil {
		out, err = r.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, withStderr(err)
	}
	return out, nil
}

// EditCommand returns an exec.Cmd that opens the remote compose file in an editor
// via SSH with a TTY. Uses $EDITOR on the remote host, falling back to vi.
func (r *RemoteCompose) EditCommand(ctx context.Context) (*exec.Cmd, error) {
	name, err := r.findRemoteComposeFile(ctx)
	if err != nil {
		return nil, err
	}

	var remoteCmd string
	if r.ProjectDir != "" {
		remoteCmd = fmt.Sprintf("cd %s && ${EDITOR:-vi} %s", shellEscape(r.ProjectDir), shellEscape(name))
	} else {
		remoteCmd = fmt.Sprintf("${EDITOR:-vi} %s", shellEscape(name))
	}

	return exec.CommandContext(ctx, "ssh",
		"-t",
		"-S", r.SocketPath,
		"-o", "ControlMaster=no",
		r.Host,
		remoteCmd,
	), nil
}

// ValidateConfig runs `docker compose config --quiet` on the remote host and returns
// any error with stderr captured so users see why validation failed.
func (r *RemoteCompose) ValidateConfig(ctx context.Context) error {
	cmd := r.remoteCommand(ctx, "config", "--quiet")
	if r.outputCmd != nil {
		_, err := r.outputCmd(cmd)
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

// ListProjects returns all Docker Compose projects on the remote host.
func (r *RemoteCompose) ListProjects(ctx context.Context) ([]Project, error) {
	cmd := r.remoteCommand(ctx, "ls", "-a", "--format", "json")
	var out []byte
	var err error
	if r.outputCmd != nil {
		out, err = r.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("listing remote projects: %w", withStderr(err))
	}
	return parseProjects(out)
}

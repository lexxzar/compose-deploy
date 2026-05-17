package compose

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

// errSSHTransport is a sentinel marker wrapped around errors that originated
// from the SSH transport layer (socket dead, host unreachable, connection
// reset, hostname unresolved, etc.) — as distinguished from errors that
// originated from the remote `docker` command itself (image not found,
// manifest auth failure, unknown image, etc.). CheckUpdates uses this to
// decide whether to abort the batch (transport failure → every subsequent
// call will fail the same way) or absorb the failure as a per-image
// "unknown" (matching local Compose.CheckUpdates).
var errSSHTransport = errors.New("ssh transport failure")

// sshTransportStderrPatterns is the heuristic match list applied to stderr
// of a failed `ssh ... docker ...` invocation to classify it as transport
// rather than per-image. Anything not matching these patterns is treated as
// a docker-layer error (per-image absent, possibly elevated to
// registry-network-failure cascade if looksLikeNetworkErr matches).
// Patterns are matched case-insensitively against the trimmed stderr string.
//
// Patterns are deliberately SSH-specific (iteration 4 narrowing): every
// entry below names a string only ssh(1) or its ControlMaster machinery
// emits, never the docker daemon or registry client. The previous list
// included generic networking phrases ("connection refused",
// "connection reset", "broken pipe", "no route to host", "network is
// unreachable", etc.) that also appear in docker daemon stderr when
// docker fails to reach a remote registry — classifying those as SSH
// transport would abort the entire CheckUpdates batch even though
// other images would have succeeded. Registry network failures are now
// detected separately via looksLikeNetworkErr (used by both local
// Compose.CheckUpdates and RemoteCompose.CheckUpdates cascades). The
// only SSH-side pattern that ALSO appears in docker stderr is
// "broken pipe" (a transport keepalive), so it's gated by sharing the
// line with one of the explicitly-SSH markers ("mux_client",
// "client_loop") rather than as a standalone pattern.
//
// ControlMaster failure modes (mux_client_*, client_loop,
// "ControlSocket ... No such file or directory", "session open refused")
// are included so a dead persistent socket during a long-running TUI
// session triggers cascading-failure abort rather than silently
// classifying every per-image inspect as "absent".
var sshTransportStderrPatterns = []string{
	"ssh:",
	// iteration-5: explicit underscore-prefixed ssh(1) handshake diagnostics —
	// `ssh:` (with colon) does NOT match `ssh_exchange_identification` because
	// the latter has an underscore between `ssh` and the next token. Without
	// this entry, common real-world SSH handshake failures (peer closed during
	// banner exchange, RST during identification) fell through to
	// looksLikeNetworkErr and produced misleading "registry unreachable"
	// diagnostics — and worst case, N per-image calls all waited on the
	// same broken transport instead of aborting after the first.
	"ssh_exchange_identification",
	"could not resolve hostname",
	"kex_exchange_identification",
	"permission denied (publickey",
	"control socket connect",
	"port forwarding failed",
	"lost connection", // ssh(1) emits "lost connection" on abrupt transport teardown
	// ControlMaster (persistent socket) failure modes — all of these are
	// emitted only by ssh(1)'s multiplexing layer, never by docker:
	"mux_client",           // mux_client_request_session / mux_client_hello_exchange
	"client_loop",          // client_loop: send disconnect
	"controlsocket",        // ControlSocket /tmp/cdeploy-ctrl-...: No such file or directory
	"multiplex",            // multiplex master state
	"session open refused", // Session open refused by peer
}

// classifySSHError inspects stderr from a failed `ssh ... docker ...`
// invocation and returns the error wrapped in errSSHTransport when the
// stderr matches one of the known transport-failure patterns; otherwise
// returns the error wrapped only with the stderr diagnostic so the caller
// sees actionable detail. ssh(1) is hardcoded to exit 255 on transport
// errors, but that signal alone is unreliable (some hosts return arbitrary
// exit codes for transport-adjacent failures), so we drive classification
// from stderr content. stderrText is the trimmed stderr from the exec.Cmd.
func classifySSHError(err error, stderrText string) error {
	stderrText = strings.TrimSpace(stderrText)
	lower := strings.ToLower(stderrText)
	for _, p := range sshTransportStderrPatterns {
		if strings.Contains(lower, p) {
			if stderrText != "" {
				return fmt.Errorf("%w: %v: %s", errSSHTransport, err, stderrText)
			}
			return fmt.Errorf("%w: %v", errSSHTransport, err)
		}
	}
	if stderrText != "" {
		return fmt.Errorf("%w: %s", err, stderrText)
	}
	return err
}

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

	// SSHExtraArgs are extra ssh CLI args spliced immediately before the host
	// argument in every SSH argv build site (Detect, ConnectCmd, Close,
	// remoteCommand, findRemoteComposeFile, ConfigFile, EditCommand,
	// ExecCommand). Used by --ssh ad-hoc connections to pass things like
	// "-p 2222" without persisting them to ~/.ssh/config. Default nil = no
	// behavior change.
	SSHExtraArgs []string

	detected bool // true after Detect() or SetStandalone() has been called

	// testing hooks; nil = use real exec
	runCmd    func(*exec.Cmd) error
	outputCmd func(*exec.Cmd) ([]byte, error)

	// outputErrCmd is an optional richer test hook used only by
	// runRemoteDockerCmd. It returns (stdout, stderr, err) so the test can
	// drive classifySSHError with explicit stderr text rather than relying
	// on the err.Error() heuristic — which was only ever an approximation
	// of production's real stderr capture and could diverge in non-obvious
	// ways. When nil, runRemoteDockerCmd falls back to outputCmd and
	// synthesises stderr from err.Error() (the legacy behaviour).
	outputErrCmd func(*exec.Cmd) ([]byte, string, error)
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

// SetOutputErrHook installs an optional richer test hook for
// runRemoteDockerCmd that returns (stdout, stderr, err). Use this in
// tests that want to drive classifySSHError with explicit stderr text
// instead of the err.Error() fallback. Pass nil to clear.
func (r *RemoteCompose) SetOutputErrHook(fn func(*exec.Cmd) ([]byte, string, error)) {
	r.outputErrCmd = fn
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
	pluginArgs := r.sshArgs(
		[]string{"-S", r.SocketPath, "-o", "ControlMaster=no"},
		"docker compose version",
	)
	pluginCmd := exec.CommandContext(ctx, "ssh", pluginArgs...)
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
	standaloneArgs := r.sshArgs(
		[]string{"-S", r.SocketPath, "-o", "ControlMaster=no"},
		"docker-compose version",
	)
	standaloneCmd := exec.CommandContext(ctx, "ssh", standaloneArgs...)
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

// sshArgs builds the ssh argv: prefix + SSHExtraArgs + "--" + host + suffix.
// SSHExtraArgs is spliced immediately before the host argument so options
// (like -p 2222) precede the destination, matching ssh's CLI expectations.
// A literal `--` separator is inserted between options and the destination
// so that ssh interprets the host argument positionally even if it (or a
// future caller) somehow starts with a `-`. This is defense-in-depth on top
// of `config.ParseSSHTarget`, which already rejects user/host values
// starting with `-` to prevent ssh option injection.
func (r *RemoteCompose) sshArgs(prefix []string, suffix ...string) []string {
	out := make([]string, 0, len(prefix)+len(r.SSHExtraArgs)+2+len(suffix))
	out = append(out, prefix...)
	out = append(out, r.SSHExtraArgs...)
	out = append(out, "--")
	out = append(out, r.Host)
	out = append(out, suffix...)
	return out
}

// ConnectCmd returns the SSH ControlMaster connect command without running it.
// The TUI uses this with tea.ExecProcess to give SSH full terminal access for
// password prompts.
func (r *RemoteCompose) ConnectCmd(ctx context.Context) *exec.Cmd {
	args := r.sshArgs([]string{"-fNM", "-S", r.SocketPath})
	return exec.CommandContext(ctx, "ssh", args...)
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
	args := r.sshArgs([]string{"-S", r.SocketPath, "-O", "exit"})
	cmd := exec.CommandContext(ctx, "ssh", args...)
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

	sshArgv := r.sshArgs(
		[]string{"-S", r.SocketPath, "-o", "ControlMaster=no"},
		remoteCmd,
	)
	return exec.CommandContext(ctx, "ssh", sshArgv...)
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

// ContainerStats returns CPU and memory usage for each running service in this
// remote project. It fetches the project's container IDs via `docker compose ps`,
// calls AllContainerStatsRemote to retrieve host-wide stats over the same SSH
// ControlMaster connection, then joins by container ID and sum-aggregates per
// service. See Compose.ContainerStats for the full contract.
func (r *RemoteCompose) ContainerStats(ctx context.Context) (map[string]runner.ServiceStats, error) {
	all, err := AllContainerStatsRemote(ctx, r)
	if err != nil {
		return nil, err
	}
	return r.ContainerStatsFromBulk(ctx, all)
}

// ContainerStatsFromBulk joins a pre-fetched host-wide stats map (from
// AllContainerStatsRemote) against this remote project's container IDs and
// returns per-service aggregated stats. See Compose.ContainerStatsFromBulk
// for the full contract — this is the SSH counterpart that shares one
// host-wide `docker stats` call across every project on the remote host.
func (r *RemoteCompose) ContainerStatsFromBulk(ctx context.Context, bulk map[string]runner.ServiceStats) (map[string]runner.ServiceStats, error) {
	cmd := r.remoteCommand(ctx, "ps", "-a", "--format", "json")
	var out []byte
	var err error
	if r.outputCmd != nil {
		out, err = r.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("listing remote project containers for stats: %w", withStderr(err))
	}
	idToService, err := parsePsIDToService(out)
	if err != nil {
		return nil, err
	}
	return aggregateStatsByService(idToService, bulk), nil
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

	sshArgv := r.sshArgs(
		[]string{"-S", r.SocketPath, "-o", "ControlMaster=no"},
		testExpr,
	)
	cmd := exec.CommandContext(ctx, "ssh", sshArgv...)
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

	sshArgv := r.sshArgs(
		[]string{"-S", r.SocketPath, "-o", "ControlMaster=no"},
		"cat "+shellEscape(filePath),
	)
	cmd := exec.CommandContext(ctx, "ssh", sshArgv...)
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

	sshArgv := r.sshArgs(
		[]string{"-t", "-S", r.SocketPath, "-o", "ControlMaster=no"},
		remoteCmd,
	)
	return exec.CommandContext(ctx, "ssh", sshArgv...), nil
}

// ExecCommand returns an exec.Cmd that runs `docker compose exec <service> <command...>`
// on the remote host via SSH with TTY allocation (-t). When command is empty, it defaults
// to DefaultExecCommand which tries bash, falling back to sh.
// The caller is responsible for attaching stdin/stdout/stderr and running the command.
func (r *RemoteCompose) ExecCommand(ctx context.Context, service string, command []string) (*exec.Cmd, error) {
	if len(command) == 0 {
		command = DefaultExecCommand
	}

	composeBin := "docker compose"
	if r.Standalone {
		composeBin = "docker-compose"
	}

	// Build the exec args: exec <service> <command...>
	var escapedArgs []string
	escapedArgs = append(escapedArgs, shellEscape("exec"))
	escapedArgs = append(escapedArgs, shellEscape(service))
	for _, a := range command {
		escapedArgs = append(escapedArgs, shellEscape(a))
	}

	remoteCmd := fmt.Sprintf("CURRENT_UID=$(id -u):$(id -g) %s %s",
		composeBin, strings.Join(escapedArgs, " "))

	if r.ProjectDir != "" {
		remoteCmd = fmt.Sprintf("cd %s && %s", shellEscape(r.ProjectDir), remoteCmd)
	}

	sshArgv := r.sshArgs(
		[]string{"-t", "-S", r.SocketPath, "-o", "ControlMaster=no"},
		remoteCmd,
	)
	return exec.CommandContext(ctx, "ssh", sshArgv...), nil
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

// CheckUpdates reports per-service "image update available" verdicts for the
// remote project. See Compose.CheckUpdates for the full contract — this is
// the SSH counterpart and behaves identically with respect to tri-state
// semantics, soft per-image failure, and absent-as-unknown.
//
// Implementation builds the SSH argv DIRECTLY for `docker image inspect`
// and `docker manifest inspect` — both are top-level docker CLI commands,
// NOT compose subcommands, so remoteCommand() would build a malformed
// `docker compose image inspect` argv. SSHExtraArgs is spliced immediately
// before the host argument via sshArgs(), mirroring the
// AllContainerStatsRemote / EditCommand / ExecCommand convention.
//
// SSH transport robustness: failures are classified into two buckets via
// `runRemoteDockerCmd`'s sentinel-error machinery (errSSHTransport):
//  1. Transport-level failures (SSH socket dead, host unreachable, connection
//     reset, etc.) — these poison the entire batch. If even ONE image hits a
//     transport failure, we surface the wrapped error so the caller doesn't
//     silently treat "SSH dead" as "no updates available". Returning early
//     skips the remaining round-trips (they'll fail the same way anyway).
//  2. Per-image docker errors (image not pulled, manifest auth failure,
//     unknown manifest, etc.) — these are absorbed into "absent" tri-state,
//     matching local Compose.CheckUpdates. A fresh-deploy single-service
//     project no longer trips a false cascade just because the image hasn't
//     been pulled to the remote yet ("No such image" is a normal docker
//     exit-1 condition, not a transport failure).
func (r *RemoteCompose) CheckUpdates(ctx context.Context, services []string) (map[string]bool, error) {
	images, err := r.fetchServiceImages(ctx)
	if err != nil {
		return nil, err
	}
	wanted := filterServices(images, services)
	out := make(map[string]bool, len(wanted))
	// Track per-image network-failure outcomes to detect a systemic registry
	// problem on the remote host (parity with local Compose). Without this,
	// the SSH-only sshTransportStderrPatterns means a Docker Hub outage seen
	// from the remote becomes a silent absent-for-everything verdict — every
	// service blank, no diagnostic. The cascade fires when EVERY non-
	// transport failure matches looksLikeNetworkErr AND no service got a
	// verdict.
	var (
		networkAttempts int
		networkFailures int
		firstNetErr     error
	)
	for svc, img := range wanted {
		updated, ok, derr := r.compareImageDigest(ctx, img)
		if derr != nil {
			if errors.Is(derr, errSSHTransport) {
				// Transport failure — every subsequent image will hit the same
				// error, so abort the batch and surface the diagnostic.
				return out, fmt.Errorf("remote update check transport failure: %w", derr)
			}
			// Non-transport per-image failure: classify for the registry
			// cascade. Network-shaped errors (DNS, connection refused, TLS,
			// 429, etc.) feed the network-failure counter so a host-wide
			// Docker Hub / registry outage surfaces as
			// "registry unreachable" rather than blank glyphs. Anything
			// else (image not pulled on remote, manifest unknown, auth
			// required) stays absorbed as "unknown" — matching local
			// Compose.CheckUpdates.
			networkAttempts++
			if looksLikeNetworkErr(derr) {
				networkFailures++
				if firstNetErr == nil {
					firstNetErr = derr
				}
			}
			continue
		}
		if !ok {
			continue
		}
		out[svc] = updated
	}
	if len(out) == 0 && networkAttempts > 0 && networkFailures == networkAttempts {
		return out, fmt.Errorf("registry unreachable: %w", firstNetErr)
	}
	return out, nil
}

// fetchServiceImages runs `docker compose config --format json` on the
// remote host and returns the service-name → image map. Build-only services
// (no `image:`) are absent. Goes through remoteCommand() — regular compose
// subcommand.
func (r *RemoteCompose) fetchServiceImages(ctx context.Context) (map[string]string, error) {
	cmd := r.remoteCommand(ctx, configImagesArgs...)
	var out []byte
	var err error
	if r.outputCmd != nil {
		out, err = r.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("fetching remote compose config: %w", withStderr(err))
	}
	return parseConfigImages(out)
}

// compareImageDigest fetches the local and remote digests for image and
// returns (updateAvailable, ok, err). ok=true with nil err means a
// definitive verdict; ok=false with nil err means at least one side could
// not be determined for a non-failure reason (parse returned empty —
// image not pulled on the remote, manifest output didn't carry a digest).
// ok=false with non-nil err means the underlying SSH/docker call failed;
// transport failures are wrapped in errSSHTransport so CheckUpdates can
// abort the whole batch, everything else is a per-image failure that
// CheckUpdates classifies via looksLikeNetworkErr for the registry
// cascade. Signature matches Compose.compareImageDigest (3-tuple) so
// empty-digest cases are distinguishable from positive verdicts without
// polluting the cascade counters with synthetic errors.
//
// Both inspect calls are top-level docker CLI commands (`docker image
// inspect`, `docker manifest inspect` / `docker buildx imagetools
// inspect`), so they CANNOT go through remoteCommand() — that would
// prepend `compose` and produce a malformed argv on the remote host. The
// SSH argv is built directly via r.sshArgs() so SSHExtraArgs is spliced
// immediately before the host argument, matching the
// AllContainerStatsRemote / EditCommand / ExecCommand convention.
//
// The remote shell command is `docker image inspect ...` / `docker manifest
// inspect ...` — args are shell-escaped before being joined into the SSH
// command string so values containing spaces or quotes survive transport.
//
// See manifestInspectArgs for the multi-arch limitation.
func (r *RemoteCompose) compareImageDigest(ctx context.Context, image string) (bool, bool, error) {
	localOut, lerr := r.runRemoteDockerCmd(ctx, imageInspectArgs(image))
	if lerr != nil {
		return false, false, lerr
	}
	localDigest := parseLocalDigest(string(localOut), image)
	if localDigest == "" {
		return false, false, nil
	}
	remoteDigest, ok, rerr := r.fetchRemoteDigest(ctx, image)
	if rerr != nil {
		return false, false, rerr
	}
	if !ok || remoteDigest == "" {
		return false, false, nil
	}
	return localDigest != remoteDigest, true, nil
}

// fetchRemoteDigest queries the remote registry's manifest digest for image
// over SSH, preferring `docker buildx imagetools inspect` (multi-arch-
// correct: returns the manifest-list digest which matches the local
// RepoDigest) and falling back to `docker manifest inspect --verbose` when
// imagetools fails (older Docker on the remote, no buildx plugin, etc.).
// Returns (digest, true, nil) on a definitive verdict;
// ("", false, nil) when both commands succeeded-or-failed cleanly but
// parsing yielded no digest (callers treat empty as "unknown");
// ("", false, err) when a transport error or surviving docker error
// prevented digest retrieval. Transport errors short-circuit the fallback
// since the SSH hop is dead.
func (r *RemoteCompose) fetchRemoteDigest(ctx context.Context, image string) (string, bool, error) {
	out, err := r.runRemoteDockerCmd(ctx, imagetoolsInspectArgs(image))
	if err == nil {
		if d := parseImagetoolsDigest(out); d != "" {
			return d, true, nil
		}
	} else if errors.Is(err, errSSHTransport) {
		// Don't retry over a broken SSH transport — surface the failure so
		// CheckUpdates can abort the batch.
		return "", false, err
	}
	// Fallback: `docker manifest inspect --verbose` (multi-arch limitation).
	out, err = r.runRemoteDockerCmd(ctx, manifestInspectArgs(image))
	if err != nil {
		return "", false, err
	}
	d := parseManifestDigest(out)
	if d == "" {
		return "", false, nil
	}
	return d, true, nil
}

// runRemoteDockerCmd runs a top-level `docker <args...>` command on the
// remote host via SSH, bypassing remoteCommand() (which is compose-specific).
// SSHExtraArgs is spliced immediately before the host argument via
// r.sshArgs(). Returns (output, err) — err non-nil on any failure. Errors
// are classified by classifySSHError: transport failures (SSH socket dead,
// host unreachable, etc.) are wrapped in errSSHTransport so callers can
// distinguish them from per-image docker errors (image not pulled, manifest
// auth failure, etc.) and decide whether to abort the batch or absorb the
// failure as "unknown".
//
// Stderr is captured explicitly via a bytes.Buffer (rather than relying on
// cmd.Output's automatic ExitError.Stderr population, which works only for
// *exec.ExitError) so test hooks that synthesise non-ExitError failures
// still surface stderr context, AND so the classifySSHError heuristic can
// inspect stderr regardless of how the failure was produced.
func (r *RemoteCompose) runRemoteDockerCmd(ctx context.Context, dockerArgs []string) ([]byte, error) {
	escaped := make([]string, 0, len(dockerArgs)+1)
	escaped = append(escaped, "docker")
	for _, a := range dockerArgs {
		escaped = append(escaped, shellEscape(a))
	}
	remoteCmd := strings.Join(escaped, " ")
	sshArgv := r.sshArgs(
		[]string{"-S", r.SocketPath, "-o", "ControlMaster=no"},
		remoteCmd,
	)
	cmd := exec.CommandContext(ctx, "ssh", sshArgv...)
	// Richest test hook: explicit (stdout, stderr, err) so classifySSHError
	// sees real stderr-equivalent text rather than guessing from err.Error().
	if r.outputErrCmd != nil {
		out, stderr, err := r.outputErrCmd(cmd)
		if err != nil {
			return nil, classifySSHError(err, stderr)
		}
		return out, nil
	}
	var out []byte
	var err error
	if r.outputCmd != nil {
		out, err = r.outputCmd(cmd)
		if err != nil {
			// Legacy test hook path: outputCmd returns only (stdout, err)
			// with no separate stderr, so classifySSHError sees an empty
			// stderr string and falls back to plain-error wrapping. Tests
			// that want to drive transport-vs-per-image classification
			// must use SetOutputErrHook (outputErrCmd above), which gives
			// the test explicit control over the stderr text. Previous
			// "synthesise stderr from err.Error()" divergence between
			// production (real stderr) and tests (err string) led to
			// false confidence in test coverage of the classifier.
			return nil, classifySSHError(err, "")
		}
		return out, nil
	}
	// Production path: capture stderr to inspect it for transport patterns.
	// cmd.Output() would set Stderr only when *exec.ExitError, which is the
	// usual case for a non-zero exit; capturing explicitly removes the type
	// constraint and gives us deterministic stderr text for classification.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	if err != nil {
		return nil, classifySSHError(err, stderr.String())
	}
	return out, nil
}

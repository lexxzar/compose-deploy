package compose

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	// ExtraComposeFiles, when non-nil, are spliced into every remote compose
	// invocation as shell-escaped `-f <file>` pairs immediately after the
	// compose binary (`docker compose` / `docker-compose`), before the
	// subcommand. Because `-f` disables compose's file auto-discovery, the
	// discovered main compose file MUST be first in this slice. Default nil =
	// no `-f` flags, producing a byte-identical remote command string to the
	// pre-ExtraComposeFiles behavior.
	ExtraComposeFiles []string

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

	// Splice shell-escaped `-f <file>` pairs immediately after the compose
	// binary, before the subcommand. fileFlags keeps a trailing space so the
	// nil case ("") reproduces the exact byte-identical command string.
	var fileFlags string
	for _, f := range r.ExtraComposeFiles {
		fileFlags += "-f " + shellEscape(f) + " "
	}

	remoteCmd := fmt.Sprintf("CURRENT_UID=$(id -u):$(id -g) %s %s%s",
		composeBin, fileFlags, strings.Join(escaped, " "))

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
// The scan itself is the shared scanImageUpdates loop; only the comparer
// differs from the local path, and the comparer is what selects the cascades:
//
//   - registry: applies. A Docker Hub outage seen from the remote host must
//     surface as "registry unreachable" rather than a silent blank glyph
//     column, because sshTransportStderrPatterns is deliberately SSH-only and
//     will not catch it.
//   - daemon: cannot fire, and must not. compareImageDigest below passes a nil
//     localErrWrap, so no failure carries errLocalImageInspect. The docker CLI
//     runs on the far side of the SSH hop, so a failed image inspect is
//     indistinguishable from any other per-image docker error and there is no
//     local daemon to diagnose.
//   - transport abort: applies. A dead SSH hop fails every remaining image the
//     same way, so the loop returns on the first errSSHTransport instead of
//     burning the rest of the round-trips.
//
// The per-image inspect calls reach the remote host through the
// remoteDockerRunner seam, which builds the SSH argv DIRECTLY — `docker image
// inspect` and `docker manifest inspect` are top-level docker CLI commands,
// NOT compose subcommands, so remoteCommand() would build a malformed
// `docker compose image inspect` argv. SSHExtraArgs is spliced immediately
// before the host argument via sshArgs(), mirroring the
// AllContainerStatsRemote / EditCommand / ExecCommand convention.
func (r *RemoteCompose) CheckUpdates(ctx context.Context, services []string) (map[string]bool, error) {
	images, err := r.fetchServiceImages(ctx)
	if err != nil {
		return nil, err
	}
	return scanImageUpdates(ctx, filterServices(images, services), r.compareImageDigest)
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

// compareImageDigest is RemoteCompose's binding of compareImageDigestVia: the
// remote dockerRunner (which routes through runRemoteDockerCmd, so every
// inspect call gets SSH argv construction, shell escaping, explicit stderr
// capture and classifySSHError for free) plus a nil local-error wrapper.
//
// The nil wrapper is load-bearing: it is what keeps the daemon cascade OFF the
// SSH path. errLocalImageInspect exists so scanImageUpdates can route a failed
// LOCAL `docker image inspect` to the "local docker unavailable" diagnostic; on
// the remote side that inspect runs on the far host and is indistinguishable
// from any other per-image docker error, so the failure must feed the registry
// counters instead.
func (r *RemoteCompose) compareImageDigest(ctx context.Context, image string) (bool, bool, error) {
	return compareImageDigestVia(ctx, remoteDockerRunner{r: r}, image, nil)
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

// SnapshotServices captures the image digest each *running* container of the
// requested services actually uses on the REMOTE host, so a later rollback can
// pin those digests. This mirrors Compose.SnapshotServices exactly (see its
// doc comment for the four-step flow and the not-running / no-digest warning
// semantics); the only differences are the transport-specific primitives:
//
//   - `docker compose config` / `docker compose ps` go through remoteCommand
//     (compose subcommands) via fetchServiceImages / runningContainerIDs.
//   - `docker inspect` / `docker image inspect` are TOP-LEVEL docker CLI
//     commands, so they MUST go through runRemoteDockerCmd (which builds the
//     SSH argv directly, shell-escapes each arg, and splices SSHExtraArgs
//     immediately before the host) — the same CLAUDE.md invariant that keeps
//     CheckUpdates' inspect calls off remoteCommand().
//
// The project dir stamped into the snapshot uses remoteProjectDir (POSIX
// normalization) so `-C ./app` and its absolute spelling key the same file.
func (r *RemoteCompose) SnapshotServices(ctx context.Context, services []string) (SnapshotResult, error) {
	images, err := r.fetchServiceImages(ctx)
	if err != nil {
		return SnapshotResult{}, err
	}
	running, err := r.runningContainerIDs(ctx)
	if err != nil {
		return SnapshotResult{}, err
	}

	targets := services
	if len(targets) == 0 {
		targets = sortedStringKeys(images)
	}

	// Batch a single `docker inspect` over the container IDs we actually need
	// (targets that both have an image ref AND a running replica).
	var toInspect []string
	seen := map[string]bool{}
	for _, svc := range targets {
		if _, ok := images[svc]; !ok {
			continue
		}
		cid, ok := running[svc]
		if !ok || seen[cid] {
			continue
		}
		seen[cid] = true
		toInspect = append(toInspect, cid)
	}
	imageIDs, err := r.inspectContainerImageIDs(ctx, toInspect)
	if err != nil {
		return SnapshotResult{}, err
	}

	snap := &Snapshot{
		Schema:     snapshotSchemaVersion,
		ProjectDir: remoteProjectDir(r.ProjectDir),
		Services:   map[string]SnapshotEntry{},
	}
	recordedAt := snapshotClock().Format(time.RFC3339)
	var warnings []string
	for _, svc := range targets {
		imageRef, hasImage := images[svc]
		if !hasImage {
			warnings = append(warnings, fmt.Sprintf("%s: no image in compose config (build-only?), skipped", svc))
			continue
		}
		cid, ok := running[svc]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s: not running, skipped", svc))
			continue
		}
		out, derr := r.runRemoteDockerCmd(ctx, imageInspectArgs(imageIDs[cid]))
		if derr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: inspecting image failed: %v, skipped", svc, derr))
			continue
		}
		digest := parseLocalDigest(string(out), imageRef)
		if digest == "" {
			warnings = append(warnings, fmt.Sprintf("%s: no repository digest (locally built?), skipped", svc))
			continue
		}
		snap.Services[svc] = SnapshotEntry{
			Image:      imageRef,
			Digest:     digest,
			RecordedAt: recordedAt,
		}
	}
	return SnapshotResult{Snapshot: snap, Warnings: warnings}, nil
}

// runningContainerIDs runs `docker compose ps --format json` on the remote host
// and returns a map of service name → the FULL container ID of its first
// running replica (see Compose.runningContainerIDs for the full-ID rationale).
// Goes through remoteCommand — regular compose subcommand.
func (r *RemoteCompose) runningContainerIDs(ctx context.Context) (map[string]string, error) {
	cmd := r.remoteCommand(ctx, "ps", "--format", "json")
	var out []byte
	var err error
	if r.outputCmd != nil {
		out, err = r.outputCmd(cmd)
	} else {
		out, err = cmd.Output()
	}
	if err != nil {
		return nil, fmt.Errorf("listing remote containers for snapshot: %w", withStderr(err))
	}
	return parseRunningContainerIDs(out)
}

// inspectContainerImageIDs runs a single batched
// `docker inspect --format '{{.Image}}' <ids...>` on the remote host and
// returns container ID → image ID. Bypasses remoteCommand() because
// `docker inspect` is a top-level docker CLI command, not a compose subcommand;
// runRemoteDockerCmd shell-escapes each arg and splices SSHExtraArgs before the
// host (same convention as CheckUpdates' inspect calls). Output order is
// preserved by `docker inspect`, so image IDs are zipped back positionally.
//
// If the batch fails or returns a mismatched line count (a container vanished
// between `ps` and `inspect`), it falls back to inspecting each container
// individually so one disappearing container only drops its own entry instead of
// failing the whole capture — mirrors Compose.inspectContainerImageIDs. A partial
// result is not an error (best-effort, warn-and-proceed snapshot contract).
func (r *RemoteCompose) inspectContainerImageIDs(ctx context.Context, containerIDs []string) (map[string]string, error) {
	if len(containerIDs) == 0 {
		return map[string]string{}, nil
	}
	args := append([]string{"inspect", "--format", "{{.Image}}"}, containerIDs...)
	if out, err := r.runRemoteDockerCmd(ctx, args); err == nil {
		if lines := nonEmptyLines(string(out)); len(lines) == len(containerIDs) {
			m := make(map[string]string, len(containerIDs))
			for i, id := range containerIDs {
				m[id] = lines[i]
			}
			return m, nil
		}
	}
	m := make(map[string]string, len(containerIDs))
	for _, id := range containerIDs {
		out, err := r.runRemoteDockerCmd(ctx, []string{"inspect", "--format", "{{.Image}}", id})
		if err != nil {
			continue
		}
		if lines := nonEmptyLines(string(out)); len(lines) == 1 {
			m[id] = lines[0]
		}
	}
	return m, nil
}

// remoteStatePath returns the absolute remote path of this project's state file
// as a shell expression the remote shell expands. The literal `$HOME` prefix is
// left UNQUOTED-in-single-quotes (see writeRemoteFile / ReadSnapshot, which wrap
// it in DOUBLE quotes) so the remote shell resolves the docker host's home
// directory — the state must live on the HOST so CI deploys and laptop
// rollbacks share one authoritative history. The key is derived from the
// POSIX-normalized project dir so `-C ./app` and its absolute spelling collapse
// to one file.
func (r *RemoteCompose) remoteStatePath() string {
	return "$HOME/" + stateFileRelPath(remoteProjectDir(r.ProjectDir))
}

// writeRemoteFile writes data to a file on the remote host atomically (temp
// file in the same directory + rename), piping the bytes over the existing
// ControlMaster socket via stdin. The remote command is
// `mkdir -p "$(dirname PATH)" && cat > PATH.tmp && mv PATH.tmp PATH`.
//
// path is a PROGRAM-GENERATED, shell-safe remote path that may reference
// `$HOME` (state file) or be an absolute `/tmp/...` path (rollback override,
// Task 8). It is interpolated inside DOUBLE quotes — not single-quote
// shellEscape — precisely so the remote shell expands `$HOME`; single-quoting
// would defeat that. Callers must therefore only pass trusted, generated paths
// (hex snapshot keys, PIDs, fixed directory names), never user input.
//
// SSHExtraArgs is spliced immediately before the host argument via sshArgs()
// and the `-S <SocketPath> -o ControlMaster=no` prefix reuses the persistent
// connection (same convention as remoteCommand / runRemoteDockerCmd) rather
// than opening a fresh SSH session.
func (r *RemoteCompose) writeRemoteFile(ctx context.Context, path string, data []byte) error {
	// %[1]s references path in every position; the literal `$$` (remote shell
	// PID) makes the temp name unique so concurrent deploys don't clash, and
	// keeps it in the same directory as the final path for an atomic rename.
	remoteCmd := fmt.Sprintf(
		`mkdir -p "$(dirname "%[1]s")" && cat > "%[1]s.$$.tmp" && mv "%[1]s.$$.tmp" "%[1]s"`,
		path,
	)
	sshArgv := r.sshArgs(
		[]string{"-S", r.SocketPath, "-o", "ControlMaster=no"},
		remoteCmd,
	)
	cmd := exec.CommandContext(ctx, "ssh", sshArgv...)
	cmd.Stdin = bytes.NewReader(data)
	if r.runCmd != nil {
		return r.runCmd(cmd)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return fmt.Errorf("writing remote file: %w: %s", err, s)
		}
		return fmt.Errorf("writing remote file: %w", err)
	}
	return nil
}

// ReadSnapshot reads and parses this project's remote state file over SSH.
// The remote command guards the read with a `[ -f ]` test so a MISSING file
// yields empty output and exit 0 (the normal first-deploy case → (nil, nil)),
// while a present-but-unreadable file surfaces cat's non-zero exit as an error.
// A non-empty payload is parsed strictly via parseSnapshot (schema/JSON errors
// are returned typed, distinguishable from not-found).
func (r *RemoteCompose) ReadSnapshot(ctx context.Context) (*Snapshot, error) {
	remoteCmd := fmt.Sprintf(`f="%s"; if [ -f "$f" ]; then cat "$f"; fi`, r.remoteStatePath())
	sshArgv := r.sshArgs(
		[]string{"-S", r.SocketPath, "-o", "ControlMaster=no"},
		remoteCmd,
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
		return nil, fmt.Errorf("reading remote snapshot: %w", withStderr(err))
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil
	}
	return parseSnapshot(out)
}

// WriteSnapshot merges fresh into the existing remote state file (if any) and
// writes the result atomically over SSH. A missing or malformed-JSON existing
// file is treated as empty so one bad state file never blocks recording a fresh,
// good snapshot; a future-schema file or a transient SSH/read failure instead
// ABORTS the write (see existingForMerge) so the merge history is preserved
// rather than clobbered. Mirrors Compose.WriteSnapshot. Both composers satisfy
// the same read+merge+write seam consumed by the deploy/rollback flows.
//
// Concurrency caveat (v1, documented): unlike the local path — which serializes
// the read-modify-rename with an advisory flock (see lockStateFile) — the remote
// read (cat) and write (stdin-pipe + rename) are two separate SSH round-trips
// with the merge done host-locally in between, so a cross-SSH lock would need a
// held remote session or shell-side JSON merge. Both are out of the plan's v1
// scope (depth-1, best-effort, warn-and-proceed). Two concurrent deploys of the
// SAME project to the SAME host from different machines can therefore race and
// the later write can clobber the earlier deploy's fresh entry. This is
// acceptable in v1: the local single-host case is the common one, and snapshot
// writes are best-effort (a lost entry only means that one service can't be
// rolled back to the exact digest of an overlapping concurrent deploy).
func (r *RemoteCompose) WriteSnapshot(ctx context.Context, fresh *Snapshot) error {
	existing, err := existingForMerge(r.ReadSnapshot(ctx))
	if err != nil {
		return fmt.Errorf("refusing to overwrite snapshot state: %w", err)
	}
	merged := mergeSnapshot(existing, fresh)
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling snapshot: %w", err)
	}
	return r.writeRemoteFile(ctx, r.remoteStatePath(), data)
}

// runRemoteDockerCmdStream runs a top-level `docker <args...>` command on the
// remote host over SSH STREAMING its combined stdout+stderr to w, bypassing
// remoteCommand() (which is compose-specific). It is the streaming counterpart
// of runRemoteDockerCmd (which CAPTURES output for parsing) and mirrors that
// method's argv build exactly: `docker` + shell-escaped args joined into a
// single remote command, wrapped by sshArgs() so SSHExtraArgs is spliced
// immediately before the host argument and the `-S <SocketPath> -o
// ControlMaster=no` prefix rides the existing ControlMaster socket. Used for
// `docker pull <repo>@<digest>` live progress during rollback prep. The runCmd
// test hook is honored so the argv is exercised without a real SSH hop.
func (r *RemoteCompose) runRemoteDockerCmdStream(ctx context.Context, dockerArgs []string, w io.Writer) error {
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
	cmd.Stdout = w
	cmd.Stderr = w
	if r.runCmd != nil {
		return r.runCmd(cmd)
	}
	return cmd.Run()
}

// PrepareRollback readies a digest-pinned rollback on the REMOTE host WITHOUT
// touching the running pipeline. It mirrors Compose.PrepareRollback (see that
// doc comment for the full presence-check / pull-by-digest / abort-before-
// pipeline / same-digest-advisory contract); only the transport-specific
// primitives differ:
//
//   - the presence check (`docker image inspect`) and pull-by-digest go through
//     runRemoteDockerCmd / runRemoteDockerCmdStream (top-level docker commands,
//     NOT compose subcommands), matching the CLAUDE.md inspect-bypass invariant.
//   - the generated override is delivered to
//     /tmp/cdeploy-rollback-<pid>-<rand>.yml via writeRemoteFile (atomic stdin
//     pipe over the ControlMaster socket). The random suffix (see
//     remoteRollbackOverridePath) makes the path unique across CLIENTS — the
//     local PID alone collides when two machines roll back the same project on
//     the same host.
//   - the main compose file is discovered via findRemoteComposeFile (a bare
//     name relative to ProjectDir, which remoteCommand cd's into), placed FIRST
//     in ExtraComposeFiles so `-f` auto-discovery disabling keeps the right
//     project.
//
// cleanup `rm -f`s the remote override file over SSH and RESETS
// ExtraComposeFiles to nil.
func (r *RemoteCompose) PrepareRollback(ctx context.Context, entries map[string]SnapshotEntry, services []string, w io.Writer) (func(), error) {
	targets := rollbackTargets(entries, services)

	main, err := r.findRemoteComposeFile(ctx)
	if err != nil {
		return nil, fmt.Errorf("rollback prep: %w", err)
	}

	// Advisory same-digest check (best-effort, non-fatal).
	r.warnAlreadyAtSnapshot(ctx, entries, targets, w)

	// Ensure each target digest is present on the remote host, pulling by digest
	// when missing. Abort BEFORE writing the override / mutating the field.
	for _, svc := range targets {
		entry, ok := entries[svc]
		if !ok {
			continue
		}
		ref := rollbackImageRef(entry)
		if _, ierr := r.runRemoteDockerCmd(ctx, imagePresenceArgs(ref)); ierr != nil {
			fmt.Fprintf(w, "pulling %s (not cached on host)\n", ref)
			if perr := r.runRemoteDockerCmdStream(ctx, []string{"pull", ref}, w); perr != nil {
				return nil, fmt.Errorf("rollback prep: %s: image %s unavailable (not cached on host and pull failed): %w", svc, ref, perr)
			}
		}
	}

	override := buildOverrideYAML(entries, targets)
	// Generate the unique remote path ONCE and use the SAME value for both the
	// delivery and the cleanup rm -f, so cleanup removes the exact file written.
	remotePath, err := remoteRollbackOverridePath()
	if err != nil {
		return nil, fmt.Errorf("rollback prep: %w", err)
	}
	if err := r.writeRemoteFile(ctx, remotePath, override); err != nil {
		return nil, fmt.Errorf("rollback prep: writing override: %w", err)
	}

	r.ExtraComposeFiles = []string{main, remotePath}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sshArgv := r.sshArgs(
			[]string{"-S", r.SocketPath, "-o", "ControlMaster=no"},
			"rm -f "+shellEscape(remotePath),
		)
		cmd := exec.CommandContext(cctx, "ssh", sshArgv...)
		if r.runCmd != nil {
			_ = r.runCmd(cmd)
		} else {
			_ = cmd.Run()
		}
		r.ExtraComposeFiles = nil
	}
	return cleanup, nil
}

// remoteRollbackOverridePath returns a per-invocation-unique path for the remote
// rollback override file. The local process PID is NOT unique across client
// machines, so two clients rolling back the same project against the same docker
// host would otherwise collide on an identical remote path — one overwriting the
// other's digest-pinned override, or a cleanup `rm -f` deleting the file while
// the other client's pipeline/wait still references it in ExtraComposeFiles. A
// crypto/rand hex suffix appended to the PID makes the final path unique across
// clients. Deviates from the plan's literal /tmp/cdeploy-rollback-<pid>.yml spec,
// which is defective for the cross-client case. The hex suffix is shell-safe.
func remoteRollbackOverridePath() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating unique override path: %w", err)
	}
	return fmt.Sprintf("/tmp/cdeploy-rollback-%d-%s.yml", os.Getpid(), hex.EncodeToString(b[:])), nil
}

// warnAlreadyAtSnapshot writes an "already at snapshot" advisory to w for each
// target service whose currently-running remote container already uses the
// snapshot digest. Mirrors Compose.warnAlreadyAtSnapshot: the current digest is
// the running container's actual digest (via SnapshotServices), and the whole
// probe is best-effort — any capture error skips the advisory without failing
// prep.
func (r *RemoteCompose) warnAlreadyAtSnapshot(ctx context.Context, entries map[string]SnapshotEntry, targets []string, w io.Writer) {
	cur, err := r.SnapshotServices(ctx, targets)
	if err != nil {
		return
	}
	for _, svc := range targets {
		entry, ok := entries[svc]
		if !ok {
			continue
		}
		if now, ok := cur.Snapshot.Services[svc]; ok && now.Digest == entry.Digest {
			fmt.Fprintf(w, "%s: already at snapshot digest %s\n", svc, entry.Digest)
		}
	}
}

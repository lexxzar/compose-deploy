package cmd

import (
	"context"
	"fmt"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
)

// noopCleanup is returned by remote-resolve helpers on error so callers can
// `defer cleanup()` unconditionally without nil-checking.
func noopCleanup() {}

// checkRemoteMutex returns an error when remote-selection flags conflict.
// Rules:
//   - --server and --ssh are mutually exclusive (--server reads a named entry
//     from ~/.cdeploy/servers.yml; --ssh provides an ad-hoc connection string).
//   - --identity is only valid alongside --ssh: named-server users belong in
//     ~/.ssh/config via IdentityFile.
func checkRemoteMutex(serverName, sshTarget, identityFile string) error {
	if serverName != "" && sshTarget != "" {
		return fmt.Errorf("--ssh (%q) and --server (%q) are mutually exclusive", sshTarget, serverName)
	}
	if identityFile != "" && sshTarget == "" {
		return fmt.Errorf("--identity requires --ssh")
	}
	return nil
}

// resolveSSHRemote parses an ad-hoc SSH connection string and builds a
// connected, detected RemoteCompose ready to run docker compose commands.
//
// The newRemote factory is taken as a parameter so each subcommand can pass
// its own injectable factory variable (e.g., opNewRemote, execNewRemote,
// logsNewRemote, listNewRemote) — preserving existing test seams.
//
// When identityFile is non-empty, it is validated via config.ParseIdentity
// and appended to SSHExtraArgs as `-i <cleanPath>`, alongside any port args
// from the SSH target.
//
// The returned cleanup is always non-nil; on the error path it is a no-op so
// callers can write `defer cleanup()` immediately after the call without
// nil-checking. On Detect failure the helper closes the ControlMaster
// connection internally before returning, so callers should not call cleanup
// when err is non-nil (it is a no-op anyway).
func resolveSSHRemote(
	ctx context.Context,
	sshTarget, projectDir, identityFile string,
	newRemote func(host, projDir string) *compose.RemoteCompose,
) (*compose.RemoteCompose, func(), error) {
	if projectDir == "" {
		return nil, noopCleanup, fmt.Errorf("--ssh requires --project-dir")
	}

	target, err := config.ParseSSHTarget(sshTarget)
	if err != nil {
		return nil, noopCleanup, fmt.Errorf("invalid --ssh value %q: %w", sshTarget, err)
	}

	extraArgs := target.PortArgs()
	if identityFile != "" {
		cleanPath, err := config.ParseIdentity(identityFile)
		if err != nil {
			return nil, noopCleanup, fmt.Errorf("invalid --identity value %q: %w", identityFile, err)
		}
		extraArgs = append(extraArgs, "-i", cleanPath)
	}

	rc := newRemote(target.SSHHost(), projectDir)
	rc.SSHExtraArgs = extraArgs

	if err := rc.Connect(ctx); err != nil {
		return nil, noopCleanup, fmt.Errorf("connecting to %s: %w", target.SSHHost(), err)
	}
	if err := rc.Detect(ctx); err != nil {
		_ = rc.Close()
		return nil, noopCleanup, err
	}

	return rc, func() { _ = rc.Close() }, nil
}

// resolveServerRemote loads the configured server `serverName` from the user's
// servers.yml and builds a connected, detected RemoteCompose.
//
// projectDirOverride takes precedence over the server's configured project_dir.
// If both are empty the helper returns an error. The newRemote factory is
// passed by each subcommand to preserve test seams.
//
// Like resolveSSHRemote, the returned cleanup is always non-nil — a no-op on
// the error path so callers can `defer cleanup()` immediately after the call.
func resolveServerRemote(
	ctx context.Context,
	serverName, projectDirOverride string,
	newRemote func(host, projDir string) *compose.RemoteCompose,
) (*compose.RemoteCompose, func(), error) {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return nil, noopCleanup, fmt.Errorf("loading config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, noopCleanup, err
	}
	server, err := cfg.FindServer(serverName)
	if err != nil {
		return nil, noopCleanup, err
	}

	projDir := server.ProjectDir
	if projectDirOverride != "" {
		projDir = projectDirOverride
	}
	if projDir == "" {
		return nil, noopCleanup, fmt.Errorf("--server %q requires --project-dir or project_dir in config", serverName)
	}

	rc := newRemote(server.Host, projDir)
	if err := rc.Connect(ctx); err != nil {
		return nil, noopCleanup, fmt.Errorf("connecting to %s: %w", serverName, err)
	}
	if err := rc.Detect(ctx); err != nil {
		_ = rc.Close()
		return nil, noopCleanup, err
	}

	return rc, func() { _ = rc.Close() }, nil
}

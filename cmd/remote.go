package cmd

import (
	"context"
	"fmt"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
)

// checkRemoteMutex returns an error when both --server and --ssh are set.
// The two flags are mutually exclusive: --server reads a named entry from
// ~/.cdeploy/servers.yml, while --ssh provides an ad-hoc connection string.
func checkRemoteMutex(serverName, sshTarget string) error {
	if serverName != "" && sshTarget != "" {
		return fmt.Errorf("--ssh and --server are mutually exclusive")
	}
	return nil
}

// resolveSSHRemote parses an ad-hoc SSH connection string and builds a
// connected, detected RemoteCompose ready to run docker compose commands.
//
// The newRemote factory is taken as a parameter so each subcommand can pass
// its own injectable factory variable (e.g., opNewRemote, execNewRemote,
// logsNewRemote, newRemote) — preserving existing test seams.
//
// On Connect or Detect failure, the helper closes any already-opened
// connection internally and returns a nil cleanup function alongside the
// error. This keeps caller code simple: cleanup is only meaningful when err
// is nil. Callers must check the error before invoking cleanup.
func resolveSSHRemote(
	ctx context.Context,
	sshTarget, projectDir string,
	newRemote func(host, projDir string) *compose.RemoteCompose,
) (*compose.RemoteCompose, func(), error) {
	if projectDir == "" {
		return nil, nil, fmt.Errorf("--ssh requires --project-dir")
	}

	target, err := config.ParseSSHTarget(sshTarget)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid --ssh value %q: %w", sshTarget, err)
	}

	rc := newRemote(target.SSHHost(), projectDir)
	rc.SSHExtraArgs = target.PortArgs()

	if err := rc.Connect(ctx); err != nil {
		return nil, nil, fmt.Errorf("connecting to %s: %w", target.SSHHost(), err)
	}
	if err := rc.Detect(ctx); err != nil {
		_ = rc.Close()
		return nil, nil, err
	}

	return rc, func() { _ = rc.Close() }, nil
}

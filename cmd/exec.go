package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/spf13/cobra"
)

var (
	execNewLocal   = compose.New
	execHasCompose = compose.HasComposeFile
	execNewRemote  = compose.NewRemote
	// execRunCmd is the function used to run the exec command.
	// It can be replaced in tests to avoid actually exec-ing into a container.
	execRunCmd = func(cmd *exec.Cmd) error { return cmd.Run() }
)

func newExecCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec <service> [-- command...]",
		Short: "Exec into a running container",
		Long:  "Opens an interactive shell (or runs a command) inside a running Docker Compose service container.",
		Example: `  # Exec into nginx with default shell (tries bash, falls back to sh)
  cdeploy exec nginx

  # Run a specific command
  cdeploy exec web -- rails console

  # Exec on a remote server
  cdeploy exec nginx -s prod -C /opt/myapp

  # Run psql in a database container
  cdeploy exec db -- psql -U postgres`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			var command []string
			if dashIdx := cmd.ArgsLenAtDash(); dashIdx >= 0 {
				// Everything after -- is the command
				command = args[dashIdx:]
			} else if len(args) > 1 {
				// All args after the service name are the command (-- is optional)
				command = args[1:]
			}

			return runExec(cmd.Context(), service, command)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	return cmd
}

// execCommander is the minimal interface for building an exec command for
// either a local or remote composer.
type execCommander interface {
	ExecCommand(ctx context.Context, service string, command []string) (*exec.Cmd, error)
}

func runExec(ctx context.Context, service string, command []string) error {
	if err := checkRemoteMutex(serverName, sshTarget, ""); err != nil {
		return err
	}

	var c execCommander
	switch {
	case sshTarget != "":
		rc, cleanup, err := resolveSSHRemote(ctx, sshTarget, projectDir, execNewRemote)
		if err != nil {
			return err
		}
		defer cleanup()
		c = rc
	case serverName != "":
		rc, cleanup, err := resolveServerRemote(ctx, serverName, projectDir, execNewRemote)
		if err != nil {
			return err
		}
		defer cleanup()
		c = rc
	default:
		dir := projectDir
		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("getting current directory: %w", err)
			}
		}
		if !execHasCompose(dir) {
			return fmt.Errorf("no compose file found in %s (use -s to specify a remote server)", dir)
		}
		lc := execNewLocal(dir)
		if err := lc.Detect(ctx); err != nil {
			return err
		}
		c = lc
	}

	cmd, err := c.ExecCommand(ctx, service, command)
	if err != nil {
		return fmt.Errorf("building exec command: %w", err)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return runInteractiveExec(cmd)
}

// runInteractiveExec runs an interactive command while intercepting SIGINT in
// the parent process via signal.Notify. This prevents Ctrl+C from killing
// cdeploy while the user is inside an interactive shell, ensuring deferred
// cleanup (e.g. SSH socket teardown) runs. Using signal.Notify (not
// signal.Ignore) keeps the OS-level signal disposition at SIG_DFL for child
// processes, so Ctrl+C still works correctly inside the container.
func runInteractiveExec(cmd *exec.Cmd) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	return execRunCmd(cmd)
}

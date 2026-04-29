package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/logging"
	"github.com/lexxzar/compose-deploy/internal/runner"
	"github.com/spf13/cobra"
)

var (
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	styleFailed  = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	styleWarning = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)

	opNewLocal  = compose.New
	opNewRemote = compose.NewRemote
	opNewLogger = logging.NewLogger
)

func newDeployCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "deploy [containers...]",
		Short: "Deploy containers: stop, remove, pull, create, start",
		Long:  "Deploys Docker Compose containers by stopping, removing, pulling new images, creating, and starting them.",
		Example: `  # Deploy specific containers
  cdeploy deploy nginx postgres

  # Deploy all containers
  cdeploy deploy -a

  # Deploy on a remote server
  cdeploy deploy -s prod -C /opt/myapp nginx`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOperation(cmd.Context(), runner.Deploy, all, args)
		},
	}

	cmd.Flags().BoolVarP(&all, "all", "a", false, "operate on all containers")

	return cmd
}

func newRestartCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "restart [containers...]",
		Short: "Restart containers: stop, remove, create, start",
		Long:  "Restarts Docker Compose containers by stopping, removing, creating, and starting them.",
		Example: `  # Restart specific containers
  cdeploy restart nginx postgres

  # Restart all containers
  cdeploy restart -a

  # Restart on a remote server
  cdeploy restart -s prod -C /opt/myapp nginx`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOperation(cmd.Context(), runner.Restart, all, args)
		},
	}

	cmd.Flags().BoolVarP(&all, "all", "a", false, "operate on all containers")

	return cmd
}

func newStopCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "stop [containers...]",
		Short: "Stop containers",
		Long:  "Stops Docker Compose containers.",
		Example: `  # Stop specific containers
  cdeploy stop nginx postgres

  # Stop all containers
  cdeploy stop -a

  # Stop on a remote server
  cdeploy stop -s prod -C /opt/myapp nginx`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOperation(cmd.Context(), runner.StopOnly, all, args)
		},
	}

	cmd.Flags().BoolVarP(&all, "all", "a", false, "operate on all containers")

	return cmd
}

func runOperation(ctx context.Context, op runner.Operation, all bool, containers []string) error {
	// Mutex check runs before container-arg validation so that misuse of
	// `--ssh` together with `--server` reports the mutex error consistently
	// across subcommands (matching exec/logs/list ordering), regardless of
	// whether `-a` or container names were also supplied.
	if err := checkRemoteMutex(serverName, sshTarget, ""); err != nil {
		return err
	}

	if !all && len(containers) == 0 {
		return fmt.Errorf("specify container names or use -a for all\n\nExamples:\n  cdeploy %s nginx postgres\n  cdeploy %s -a",
			strings.ToLower(op.String()), strings.ToLower(op.String()))
	}

	if all && len(containers) > 0 {
		return fmt.Errorf("-a/--all cannot be combined with explicit container names")
	}

	if all {
		containers = nil // empty slice = all containers
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	var c runner.Composer
	switch {
	case sshTarget != "":
		rc, cleanup, err := resolveSSHRemote(ctx, sshTarget, projectDir, "", opNewRemote)
		if err != nil {
			return err
		}
		defer cleanup()
		c = rc
	case serverName != "":
		rc, cleanup, err := resolveServerRemote(ctx, serverName, projectDir, opNewRemote)
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
		lc := opNewLocal(dir)
		if err := lc.Detect(ctx); err != nil {
			return err
		}
		c = lc
	}

	logger, err := opNewLogger(logDir)
	if err != nil {
		return fmt.Errorf("creating logger: %w", err)
	}
	defer logger.Close()

	w := io.MultiWriter(logger.Writer(), os.Stdout)
	events := make(chan runner.StepEvent, 20)

	go runner.Run(ctx, c, op, containers, w, events)

	containerLabel := "all containers"
	if len(containers) > 0 {
		containerLabel = strings.Join(containers, ", ")
	}

	for event := range events {
		if event.Status == runner.StatusRunning {
			fmt.Fprintf(os.Stderr, "%s %s: ", event.Step, containerLabel)
		} else if event.Status == runner.StatusDone {
			fmt.Fprintln(os.Stderr, styleOK.Render("OK"))
		} else if event.Status == runner.StatusFailed {
			fmt.Fprintln(os.Stderr, styleFailed.Render("Failed"))
			fmt.Fprintf(os.Stderr, "\nFor details see logfile: %s\n", logger.Path())
			return fmt.Errorf("%s failed: %w", event.Step, event.Err)
		}
	}

	fmt.Fprintf(os.Stderr, "\nFor details see logfile: %s\n", logger.Path())
	return nil
}

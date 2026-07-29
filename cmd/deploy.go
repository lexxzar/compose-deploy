package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

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

	// Health-wait flags, shared by the deploy and restart subcommands (stop
	// never registers them, so a stop invocation keeps waitEnabled==false).
	// Package-level like the other cross-cutting flags (serverName, projectDir,
	// …); runOperation reads them directly.
	waitEnabled bool
	waitTimeout time.Duration
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
	addWaitFlags(cmd)

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
	addWaitFlags(cmd)

	return cmd
}

// addWaitFlags registers the health-wait flags on a subcommand. Only deploy and
// restart get them — stop has no health phase.
func addWaitFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&waitEnabled, "wait", false,
		"after the operation, wait for services to become healthy (exit 2 if any fail)")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", runner.DefaultWaitTimeout,
		"maximum time to wait for health (with --wait)")
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
	return runOperationWithPrep(ctx, op, all, containers, nil)
}

// runOperationWithPrep is runOperation with an optional op-specific pre-run
// hook. When prep is non-nil it runs AFTER the composer is built and BEFORE the
// pipeline, so it can apply refusal rules and configure the composer (rollback
// uses it to pin ExtraComposeFiles to a generated digest override). The cleanup
// it returns is deferred function-scoped — it runs after the wait phase and,
// crucially, before the composer's remote SSH teardown (LIFO), so a remote
// `rm -f` of the override file still rides the live ControlMaster socket. deploy,
// restart and stop pass a nil prep and keep the original code path.
//
// prep also returns the set of services the wait phase should gate on. For
// rollback this is the resolved snapshot services — NOT the raw containers arg,
// which is nil for `-a` and would otherwise make WaitHealthy resolve the target
// set via ListServices() (every compose service, dragging build-only /
// run-to-completion services that were never rolled back into the health gate).
// An empty waitTargets falls back to containers (the deploy/restart behavior).
func runOperationWithPrep(ctx context.Context, op runner.Operation, all bool, containers []string, prep func(context.Context, runner.Composer) (func(), []string, error)) error {
	// Mutex check runs before container-arg validation so that misuse of
	// `--ssh` together with `--server` reports the mutex error consistently
	// across subcommands (matching exec/logs/list ordering), regardless of
	// whether `-a` or container names were also supplied.
	if err := checkRemoteMutex(serverName, sshTarget, identityFile); err != nil {
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
		rc, cleanup, err := resolveSSHRemote(ctx, sshTarget, projectDir, identityFile, opNewRemote)
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

	// Op-specific pre-run hook (rollback: read snapshot, apply refusal rules,
	// print the plan, prepare the digest override). Runs before the pipeline;
	// a refusal here aborts without touching any container. The cleanup is
	// deferred so it runs after the wait phase but before the SSH teardown.
	waitContainers := containers
	if prep != nil {
		cleanup, waitTargets, err := prep(ctx, c)
		if err != nil {
			return err
		}
		if cleanup != nil {
			defer cleanup()
		}
		if len(waitTargets) > 0 {
			waitContainers = waitTargets
		}
	}

	logger, err := opNewLogger(logDir)
	if err != nil {
		return fmt.Errorf("creating logger: %w", err)
	}
	defer logger.Close()

	// Snapshot the currently-running image digests BEFORE the pipeline touches
	// anything, so `cdeploy rollback` can restore them. Deploy only (a restart
	// after a bad deploy must not overwrite the good snapshot). Best-effort:
	// SnapshotServices/WriteSnapshot failures warn but never block the deploy.
	if op == runner.Deploy {
		if s, ok := c.(snapshotter); ok {
			recordSnapshot(ctx, s, containers, os.Stderr)
		}
	}

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

	// Health-wait phase. Only deploy and restart register --wait; stop is
	// excluded defensively so a leaked global can never make a stop wait. For
	// rollback, waitContainers is the resolved snapshot service set (see prep).
	if waitEnabled && op != runner.StopOnly {
		return waitForHealth(ctx, c, op, waitContainers)
	}

	return nil
}

// waitForHealth runs the post-operation health wait and prints the per-service
// verdict table. It returns a *WaitError (mapped to exit code 2 by main.go) when
// any targeted service failed to become healthy or the wait could not complete;
// nil when every service passed. Deploy failures also print the rollback hint.
func waitForHealth(ctx context.Context, c runner.Composer, op runner.Operation, containers []string) error {
	timeout := waitTimeout
	if timeout <= 0 {
		timeout = runner.DefaultWaitTimeout
	}

	fmt.Fprintf(os.Stderr, "\nWaiting for health (timeout %s)...\n", timeout)
	report, err := runner.WaitHealthy(ctx, c, containers, runner.WaitOptions{Timeout: timeout})
	fmt.Fprint(os.Stderr, formatWaitReport(report))

	// An operator interrupt (Ctrl-C cancels the signal context) is NOT a health
	// failure: return the raw error so main.go treats it as a generic abort
	// (exit 1), the same as interrupting the pipeline itself — never exit 2, and
	// never the rollback hint, which would misread a deliberate abort as
	// "deployed but unhealthy".
	if errors.Is(err, context.Canceled) {
		return err
	}

	if err != nil || !report.OK {
		if op == runner.Deploy {
			fmt.Fprintln(os.Stderr, styleWarning.Render("run 'cdeploy rollback' to restore the previous images"))
		}
		return &WaitError{Report: report, Err: err}
	}

	fmt.Fprintln(os.Stderr, styleOK.Render("All services healthy"))
	return nil
}

// WaitError signals that a post-operation health wait failed. main.go maps it to
// exit code 2 via errors.As, letting CI distinguish "deployed but unhealthy"
// (code 2) from "pipeline step failed" (code 1). Report carries the per-service
// verdicts; Err (if set) is the operational failure that aborted the wait
// (context cancellation, repeated poll failures, service resolution), exposed via
// Unwrap so callers can inspect the underlying cause.
type WaitError struct {
	Report runner.WaitReport
	Err    error
}

func (e *WaitError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("health wait failed: %v", e.Err)
	}
	var failed []string
	for name, v := range e.Report.Verdicts {
		if !v.OK() {
			label := string(v)
			if v == runner.VerdictPending {
				label = "pending"
			}
			failed = append(failed, fmt.Sprintf("%s (%s)", name, label))
		}
	}
	sort.Strings(failed)
	return fmt.Sprintf("health wait failed: %s", strings.Join(failed, ", "))
}

func (e *WaitError) Unwrap() error { return e.Err }

// snapshotter is the subset of the concrete composers used to record a pre-deploy
// digest snapshot. Both *compose.Compose and *compose.RemoteCompose satisfy it;
// a composer that does not (e.g. a test mock) is silently skipped, so snapshotting
// is a best-effort capability rather than part of the runner.Composer contract.
type snapshotter interface {
	SnapshotServices(ctx context.Context, services []string) (compose.SnapshotResult, error)
	WriteSnapshot(ctx context.Context, fresh *compose.Snapshot) error
}

// recordSnapshot captures the currently-running image digests for the targeted
// services and merge-writes them to the host state file. It is strictly
// best-effort: a capture error, per-service warnings, and a write failure are all
// surfaced to warn without ever returning an error, so a snapshot problem can
// never block the deploy that follows.
func recordSnapshot(ctx context.Context, s snapshotter, services []string, warn io.Writer) {
	res, err := s.SnapshotServices(ctx, services)
	if err != nil {
		fmt.Fprintf(warn, "%s rollback snapshot skipped: %v\n", styleWarning.Render("Warning:"), err)
		return
	}
	for _, msg := range res.Warnings {
		fmt.Fprintf(warn, "%s rollback snapshot: %s\n", styleWarning.Render("Warning:"), msg)
	}
	if err := s.WriteSnapshot(ctx, res.Snapshot); err != nil {
		fmt.Fprintf(warn, "%s failed to write rollback snapshot: %v (deploy continues)\n",
			styleWarning.Render("Warning:"), err)
	}
}

// waitVerdictIcon renders the shared verdict glyph (runner.WaitVerdict.Icon)
// with the CLI's colors: green for a pass, yellow for pending, red for failure.
func waitVerdictIcon(v runner.WaitVerdict) string {
	icon := v.Icon()
	switch {
	case v.OK():
		return styleOK.Render(icon)
	case v == runner.VerdictPending:
		return styleWarning.Render(icon)
	default:
		return styleFailed.Render(icon)
	}
}

// formatWaitReport renders the per-service verdict table (services sorted by
// name, left-aligned). It is a pure function of the report so it can be
// golden-tested; colors come from the shared styles and collapse to plain text
// when the output is not a terminal.
func formatWaitReport(report runner.WaitReport) string {
	names := make([]string, 0, len(report.Verdicts))
	width := 0
	for name := range report.Verdicts {
		names = append(names, name)
		if len(name) > width {
			width = len(name)
		}
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		v := report.Verdicts[name]
		label := string(v)
		if v == runner.VerdictPending {
			label = "pending"
		}
		fmt.Fprintf(&b, "  %s %-*s  %s\n", waitVerdictIcon(v), width, name, label)
	}
	return b.String()
}

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/runner"
	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	var (
		tail     int
		noFollow bool
	)

	cmd := &cobra.Command{
		Use:   "logs <service>",
		Short: "Stream logs for a service",
		Long:  "Streams Docker Compose logs for a single service. Follows by default; use --no-follow to dump and exit.",
		Example: `  # Tail logs for nginx
  cdeploy logs nginx

  # Dump last 100 lines and exit
  cdeploy logs nginx -n 100 --no-follow

  # Tail logs on a remote server
  cdeploy logs nginx -s prod -C /opt/myapp`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd.Context(), args[0], !noFollow, tail)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.Flags().IntVarP(&tail, "tail", "n", 50, "number of historical lines to show")
	cmd.Flags().BoolVar(&noFollow, "no-follow", false, "dump logs and exit (don't follow)")

	return cmd
}

func runLogs(ctx context.Context, service string, follow bool, tail int) error {
	dir := projectDir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("getting current directory: %w", err)
		}
	}

	var c runner.Composer
	if serverName != "" {
		cfg, err := config.Load(config.DefaultPath())
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		if len(cfg.Servers) > 0 {
			if err := cfg.Validate(); err != nil {
				return err
			}
		}
		server, err := cfg.FindServer(serverName)
		if err != nil {
			return err
		}

		projDir := server.ProjectDir
		if projectDir != "" {
			projDir = projectDir
		}
		if projDir == "" {
			return fmt.Errorf("--server %q requires --project-dir or project_dir in config", serverName)
		}

		rc := compose.NewRemote(server.Host, projDir)
		if err := rc.Connect(ctx); err != nil {
			return fmt.Errorf("connecting to %s: %w", serverName, err)
		}
		defer rc.Close()
		c = rc
	} else {
		if !compose.HasComposeFile(dir) {
			return fmt.Errorf("no compose file found in %s (use -s to specify a remote server)", dir)
		}
		c = compose.New(dir)
	}

	// Set up signal handling for graceful Ctrl+C
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	return c.Logs(ctx, service, follow, tail, os.Stdout)
}

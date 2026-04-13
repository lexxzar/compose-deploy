package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/logging"
	"github.com/lexxzar/compose-deploy/internal/runner"
	"github.com/lexxzar/compose-deploy/internal/tui"
	"github.com/spf13/cobra"
)

var (
	logDir     string
	projectDir string
	serverName string
)

func NewRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "cdeploy",
		Short: "Docker Compose deploy and restart tool",
		Long: `A TUI/CLI tool for managing Docker Compose container deployments and restarts.

Run without arguments to launch the interactive TUI.
Run with a subcommand (deploy, restart, stop, list, logs) for non-interactive CLI usage.

Remote server configuration (~/.cdeploy/servers.yml):

  servers:
    - name: prod
      host: user@prod.example.com
      project_dir: /opt/myapp
    - name: staging
      host: user@staging.example.com
      group: dev`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := projectDir
			if dir == "" {
				var err error
				dir, err = os.Getwd()
				if err != nil {
					return err
				}
			}

			factory := func(d string) runner.Composer { return compose.New(d) }

			var c runner.Composer
			if compose.HasComposeFile(dir) {
				c = compose.New(dir)
			}

			// Load server config
			cfg, err := config.Load(config.DefaultPath())
			if err != nil {
				return err
			}
			if len(cfg.Servers) > 0 {
				if err := cfg.Validate(); err != nil {
					return err
				}
			}

			// Build connect callback for TUI
			var connectCb tui.ConnectCallback
			if len(cfg.Servers) > 0 {
				connectCb = func(server config.Server) (*exec.Cmd, tui.ComposerFactory, tui.ProjectLoader, func() error) {
					projDir := server.ProjectDir
					if projectDir != "" {
						projDir = projectDir
					}
					rc := compose.NewRemote(server.Host, projDir)
					connectCmd := rc.ConnectCmd(cmd.Context())
					remoteFactory := func(d string) runner.Composer {
						return compose.NewRemote(server.Host, d)
					}
					loader := func(ctx context.Context) ([]compose.Project, error) {
						return rc.ListProjects(ctx)
					}
					return connectCmd, remoteFactory, loader, rc.Close
				}
			}

			logger, err := logging.NewLogger(logDir)
			if err != nil {
				return err
			}
			defer logger.Close()

			var tuiOpts []tui.Option
			if serverName != "" && len(cfg.Servers) == 0 {
				return fmt.Errorf("--server %q specified but no servers configured in %s", serverName, config.DefaultPath())
			}
			if serverName != "" && len(cfg.Servers) > 0 {
				idx := -1
				for i, s := range cfg.Servers {
					if s.Name == serverName {
						idx = i
						break
					}
				}
				if idx < 0 {
					return fmt.Errorf("server %q not found in config", serverName)
				}
				tuiOpts = append(tuiOpts, tui.WithPreselectedServer(idx))
			}

			return tui.Run(c, logger.Writer(), factory, cfg.Servers, connectCb, tuiOpts...)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().StringVar(&logDir, "log-dir", "", "log directory (default ~/.cdeploy/logs/)")
	rootCmd.PersistentFlags().StringVarP(&projectDir, "project-dir", "C", "", "docker compose project directory (default: current directory)")
	rootCmd.PersistentFlags().StringVarP(&serverName, "server", "s", "", "remote server name from ~/.cdeploy/servers.yml")

	rootCmd.AddCommand(newDeployCmd(), newRestartCmd(), newStopCmd(), newListCmd(), newLogsCmd())

	return rootCmd
}

func Execute() error {
	return NewRootCmd().Execute()
}

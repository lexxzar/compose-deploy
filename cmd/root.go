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
	sshTarget  string
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
			// `--ssh` is a CLI-only flag; the TUI doesn't support ad-hoc SSH
			// connections (it expects a configured server entry). Reject the
			// flag here rather than silently ignoring it.
			if sshTarget != "" {
				return fmt.Errorf("--ssh is not valid for the interactive TUI; use it with a subcommand")
			}

			dir := projectDir
			if dir == "" {
				var err error
				dir, err = os.Getwd()
				if err != nil {
					return err
				}
			}

			// Load server config first — this determines whether we must
			// have a working local Docker installation.
			cfg, err := config.Load(config.DefaultPath())
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return err
			}

			// localDetector is lazily initialized on first Detect() call.
			// It avoids failing TUI startup when Docker is not installed locally
			// (the user may only target remote servers).
			localDetector := compose.New(dir)
			var localDetected bool

			detectLocal := func(ctx context.Context) error {
				if localDetected {
					return nil
				}
				if err := localDetector.Detect(ctx); err != nil {
					return err
				}
				localDetected = true
				return nil
			}

			factory := func(d string) runner.Composer {
				lc := compose.New(d)
				if localDetected {
					lc.SetStandalone(localDetector.Standalone)
				}
				return lc
			}

			// When the cwd has a compose file, try to detect the local
			// Docker variant so the TUI can skip the project picker.
			// If servers are configured, detection failure is non-fatal —
			// the user may only target remote servers.
			var c runner.Composer
			if compose.HasComposeFile(dir) {
				if err := detectLocal(cmd.Context()); err != nil {
					if len(cfg.Servers) == 0 {
						return err
					}
					// Servers available — local Docker not required.
				} else {
					c = localDetector
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
						newRC := compose.NewRemote(server.Host, d)
						newRC.SetStandalone(rc.Standalone)
						return newRC
					}
					loader := func(ctx context.Context) ([]compose.Project, error) {
						if err := rc.Detect(ctx); err != nil {
							return nil, err
						}
						return rc.ListProjects(ctx)
					}
					return connectCmd, remoteFactory, loader, rc.Close
				}
			}

			// Local project loader — lazily detects standalone mode
			localLoader := func(ctx context.Context) ([]compose.Project, error) {
				if err := detectLocal(ctx); err != nil {
					return nil, err
				}
				return localDetector.ListProjects(ctx)
			}

			logger, err := logging.NewLogger(logDir)
			if err != nil {
				return err
			}
			defer logger.Close()

			tuiOpts := []tui.Option{
				tui.WithLocalProjectLoader(localLoader),
				tui.WithConfigPath(config.DefaultPath()),
				tui.WithConfig(cfg),
			}
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
	rootCmd.PersistentFlags().StringVarP(&sshTarget, "ssh", "S", "", "ad-hoc SSH connection string [user@]host[:port] (mutually exclusive with --server)")

	rootCmd.AddCommand(newDeployCmd(), newRestartCmd(), newStopCmd(), newListCmd(), newLogsCmd(), newExecCmd())

	return rootCmd
}

func Execute() error {
	return NewRootCmd().Execute()
}

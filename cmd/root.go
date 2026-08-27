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
	logDir       string
	projectDir   string
	serverName   string
	sshTarget    string
	identityFile string
)

// Build-time metadata. Overridden via -ldflags by GoReleaser; the defaults
// here are what `go build` from a working tree produces.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func NewRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:     "cdeploy",
		Version: fmt.Sprintf("%s (commit %s, built %s)", version, commit, date),
		Short:   "Docker Compose deploy and restart tool",
		Long: `A TUI/CLI tool for managing Docker Compose container deployments and restarts.

Run without arguments to launch the interactive TUI.
Run with a subcommand (deploy, restart, stop, rollback, list, logs) for non-interactive CLI usage.

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
			// `--identity` is also CLI-only; it only makes sense paired with
			// `--ssh`. Reject early in the TUI path.
			if identityFile != "" {
				return fmt.Errorf("--identity is not valid for the interactive TUI; use it with a subcommand")
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

			factory := func(proj compose.Project) runner.Composer {
				return localComposerFor(proj, localDetector, localDetected)
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
					remoteFactory := func(proj compose.Project) runner.Composer {
						return remoteComposerFor(proj, rc)
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
	rootCmd.PersistentFlags().StringVarP(&identityFile, "identity", "i", "", "path to SSH private key (requires --ssh)")

	rootCmd.AddCommand(newDeployCmd(), newRestartCmd(), newStopCmd(), newRollbackCmd(), newListCmd(), newLogsCmd(), newExecCmd(), newSkillCmd())

	return rootCmd
}

func Execute() error {
	return NewRootCmd().Execute()
}

// The TUI ProjectLoader is plain ListProjects on both paths. The synthetic
// "(unmanaged)" group is NOT appended here: buildSvcGroups derives it from the
// host-wide status map the grouped loader already fetched, so asking the host
// to count unmanaged containers separately would be a second `docker ps` per
// refresh for an answer the first one already carries.

// localComposerFor is the local tui.ComposerFactory body. The synthetic
// unmanaged row has no compose file and no ConfigDir, so it gets the read-only
// host-container composer; every other row gets a Compose rooted at its config
// directory, inheriting the plugin/standalone verdict when one was detected.
func localComposerFor(proj compose.Project, detector *compose.Compose, detected bool) runner.Composer {
	if proj.Unmanaged {
		return compose.NewLocalHostContainers(detector)
	}
	lc := compose.New(proj.ConfigDir)
	if detected {
		lc.SetStandalone(detector.Standalone)
	}
	return lc
}

// remoteComposerFor is the remote twin of localComposerFor. The unmanaged row
// reuses the LIVE RemoteCompose so the existing ControlMaster socket carries
// the docker ps / stats / logs calls; a compose project gets a fresh composer
// pointed at the same host.
func remoteComposerFor(proj compose.Project, rc *compose.RemoteCompose) runner.Composer {
	if proj.Unmanaged {
		return compose.NewRemoteHostContainers(rc)
	}
	newRC := compose.NewRemote(rc.Host, proj.ConfigDir)
	newRC.SetStandalone(rc.Standalone)
	return newRC
}

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

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
	projectName  string
	serverName   string
	sshTarget    string
	identityFile string
)

// rootNewLocal builds the local composer the TUI fast track starts on.
// Injectable so tests can attach compose test hooks, mirroring opNewLocal /
// listNewLocal / execNewLocal / logsNewLocal.
var rootNewLocal = compose.New

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
			// `--project-name` is CLI-only too. The TUI discovers projects
			// through `docker compose ls` and carries each row's real name into
			// the composer it builds, so naming one up front would either be
			// ignored or silently override the row the user picked.
			if projectName != "" {
				return fmt.Errorf("--project-name is not valid for the interactive TUI; use it with a subcommand")
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
			var localDetect detectGuard

			detectLocal := func(ctx context.Context) error {
				return localDetect.resolve(
					func() error { return localDetector.Detect(ctx) },
					func() bool { return localDetector.Standalone },
				)
			}

			factory := func(proj compose.Project) runner.Composer {
				standalone, detected := localDetect.verdict()
				return localComposerFor(proj, localDetector, standalone, detected)
			}

			// When the cwd has a compose file, try to detect the local
			// Docker variant so the TUI can fast-track to the drilled screen.
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
					standalone, _ := localDetect.verdict()
					c = fastTrackComposer(dir, standalone)
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
					var remoteDetect detectGuard
					remoteFactory := func(proj compose.Project) runner.Composer {
						standalone, _ := remoteDetect.verdict()
						return remoteComposerFor(proj, rc, standalone)
					}
					loader := func(ctx context.Context) ([]compose.Project, error) {
						if err := remoteDetect.resolve(
							func() error { return rc.Detect(ctx) },
							func() bool { return rc.Standalone },
						); err != nil {
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
	rootCmd.PersistentFlags().StringVarP(&projectName, "project-name", "p", "", "docker compose project name (default: derived from the project directory)")
	rootCmd.PersistentFlags().StringVarP(&serverName, "server", "s", "", "remote server name from ~/.cdeploy/servers.yml")
	rootCmd.PersistentFlags().StringVarP(&sshTarget, "ssh", "S", "", "ad-hoc SSH connection string [user@]host[:port] (mutually exclusive with --server)")
	rootCmd.PersistentFlags().StringVarP(&identityFile, "identity", "i", "", "path to SSH private key (requires --ssh)")

	rootCmd.AddCommand(newDeployCmd(), newRestartCmd(), newStopCmd(), newRollbackCmd(), newListCmd(), newLogsCmd(), newExecCmd(), newSkillCmd())

	return rootCmd
}

func Execute() error {
	return NewRootCmd().Execute()
}

// fastTrackComposer builds the composer the TUI starts on when the working
// directory holds a compose file and the project picker is skipped.
//
// It names no project, exactly like `docker compose` run in that directory: it
// addresses the project compose derives from the directory. A picker row for
// that same project carries that derived name explicitly, and canonicalStateName
// folds the two spellings onto one rollback state file — so the fast track and a
// grouped drill-in converge with NO host lookup. Resolving the name from
// `docker compose ls` is what a previous round did, and it made the identity
// depend on containers the deploy pipeline removes.
func fastTrackComposer(dir string, standalone bool) runner.Composer {
	lc := rootNewLocal(dir)
	lc.SetStandalone(standalone)
	return lc
}

// localComposerFor is the local tui.ComposerFactory body. The synthetic
// unmanaged row has no compose file and no ConfigDir, so it gets the read-only
// host-container composer; every other row gets a Compose rooted at its config
// directory, inheriting the plugin/standalone verdict when one was detected.
//
// The project NAME is carried through as well, not just the directory. Compose
// derives a project from the working directory when none is named, and that is
// not the same project for anything deployed with `docker compose -p` or
// COMPOSE_PROJECT_NAME: `docker compose ls` reports those under their real
// names while several of them share one ConfigDir. Dropping the name made every
// such row address the directory's default project instead — the picker showed
// one project and `d`/`r`/`s` stopped and removed another's containers.
//
// The FILE SET is carried for the same reason at one level down. `-p` pins which
// project a command addresses; the files pin what that project is made of. A
// project created from `-f prod.yml` in a directory that also holds a
// docker-compose.yml was recreated from the wrong service definitions under the
// right label, and a project whose only file is stack.yml reported "no compose
// file found" from the `c` screen and from rollback prep. It goes through
// PinComposeFilesLocal, which drops a file set auto-discovery would find anyway
// — `-f` disables discovery, and the label docker reports was stamped when the
// containers were created, so pinning it froze out any override added later.
// The LOCAL variant reads the directory, so it drops the pin only when
// discovery's PRECEDENCE resolves the row's own main file: a default name is
// not enough, since compose keeps just the first of the ones it finds.
func localComposerFor(proj compose.Project, detector *compose.Compose, standalone, detected bool) runner.Composer {
	if proj.Unmanaged {
		return compose.NewLocalHostContainers(detector)
	}
	lc := compose.New(proj.ConfigDir)
	lc.ProjectName = proj.Name
	lc.ComposeFiles = compose.PinComposeFilesLocal(proj.ConfigDir, proj.ConfigFiles)
	if detected {
		lc.SetStandalone(standalone)
	}
	return lc
}

// remoteComposerFor is the remote twin of localComposerFor. The unmanaged row
// reuses the LIVE RemoteCompose so the existing ControlMaster socket carries
// the docker ps / stats / logs calls; a compose project gets a fresh composer
// pointed at the same host, named by the same project the row came from.
//
// SSHExtraArgs is copied along with the rest: it carries the ad-hoc port and
// `-i <key>` that reach the host at all, so a composer built without them would
// dial a different endpoint than the connection it was derived from.
//
// The file set goes through the pure PinComposeFiles, NOT the precedence-aware
// PinComposeFilesLocal: resolving discovery needs the project directory read,
// and here that is an SSH round trip per row on every 5-second grouped reload.
// The gap that leaves is a project created from an explicit `-f` naming a
// LOWER-precedence default file (a `-f docker-compose.yml` project in a
// directory that also holds compose.yaml) — see PinComposeFilesLocal.
func remoteComposerFor(proj compose.Project, rc *compose.RemoteCompose, standalone bool) runner.Composer {
	if proj.Unmanaged {
		return compose.NewRemoteHostContainers(rc)
	}
	newRC := compose.NewRemote(rc.Host, proj.ConfigDir)
	newRC.ProjectName = proj.Name
	newRC.ComposeFiles = compose.PinComposeFiles(proj.ConfigDir, proj.ConfigFiles)
	newRC.SSHExtraArgs = rc.SSHExtraArgs
	newRC.SetStandalone(standalone)
	return newRC
}

// detectGuard serialises the plugin/standalone probe between the two goroutines
// that both need its verdict. The ProjectLoader runs inside a tea.Cmd goroutine
// and is what calls Detect(); the ComposerFactory runs on the UI goroutine and
// reads the verdict to stamp every composer it builds. In the grouped host view
// the loader runs on the 5-second tick and the factory on every action key, so
// the two are live together for the whole session — an unguarded bool plus a
// Compose.Standalone field written by one and read by the other is a data race
// whose failure mode is a composer built with the wrong docker variant.
//
// The verdict is COPIED out of the composer under the lock rather than read
// through the pointer later, so the factory never touches a field Detect writes.
//
// TWO mutexes, and the split is load-bearing. probeMu serialises the probe so
// only one Detect() ever writes Compose.Standalone; mu guards the recorded pair
// and is NEVER held across the probe. A single mutex would put a subprocess
// exec — or, remotely, an SSH round-trip under the TUI's deadline-less context
// — inside the lock verdict() takes, and verdict() runs on the Bubble Tea UI
// goroutine inside Update: one hung probe (a dead ControlMaster socket falling
// back to a direct connect) would freeze the whole TUI, ctrl+c included.
type detectGuard struct {
	mu         sync.Mutex
	probeMu    sync.Mutex
	detected   bool
	standalone bool
}

// resolve runs probe unless a previous call already succeeded, then records
// what standalone reports. A failed probe records nothing, so the next caller
// retries — that is what keeps a TUI started without local Docker usable if
// Docker appears later in the session.
func (d *detectGuard) resolve(probe func() error, standalone func() bool) error {
	if _, detected := d.verdict(); detected {
		return nil
	}
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	// Re-check: a probe that finished while this call waited on probeMu has
	// already recorded the verdict, and probing again would re-run Detect for
	// an answer that is already in hand.
	if _, detected := d.verdict(); detected {
		return nil
	}
	if err := probe(); err != nil {
		return err
	}
	s := standalone()
	d.mu.Lock()
	d.standalone = s
	d.detected = true
	d.mu.Unlock()
	return nil
}

// verdict returns the recorded (standalone, detected) pair.
func (d *detectGuard) verdict() (standalone, detected bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.standalone, d.detected
}

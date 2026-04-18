package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/runner"
	"github.com/spf13/cobra"
)

var (
	newRemote        = compose.NewRemote
	newLocalComposer = func(dir string) *compose.Compose { return compose.New(dir) }
	hasLocalCompose  = compose.HasComposeFile
)

type serviceStatus struct {
	Project string `json:"project,omitempty"`
	Name    string `json:"service"`
	Running bool   `json:"running"`
	Health  string `json:"health,omitempty"`
}

// projectServices groups service statuses under a project name for grouped display.
type projectServices struct {
	Name     string
	Services []serviceStatus
}

// mergeStatus combines the canonical service list with container status.
// Services missing from the status map are treated as stopped.
func mergeStatus(services []string, status map[string]runner.ServiceStatus) []serviceStatus {
	result := make([]serviceStatus, len(services))
	for i, svc := range services {
		st := status[svc]
		result[i] = serviceStatus{
			Name:    svc,
			Running: st.Running,
			Health:  st.Health,
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

// healthIcon returns a colored health icon for CLI output.
func healthIcon(health string) string {
	switch health {
	case "healthy":
		return styleOK.Render("♥")
	case "unhealthy":
		return styleFailed.Render("✗")
	case "starting":
		return styleWarning.Render("~")
	default:
		return " "
	}
}

// formatDots renders service statuses as colored dot lines with aligned names.
func formatDots(items []serviceStatus) string {
	if len(items) == 0 {
		return ""
	}

	maxLen := 0
	for _, item := range items {
		if len(item.Name) > maxLen {
			maxLen = len(item.Name)
		}
	}

	var b strings.Builder
	for i, item := range items {
		if i > 0 {
			b.WriteByte('\n')
		}
		if item.Running {
			b.WriteString(styleOK.Render("●"))
		} else {
			b.WriteString(styleFailed.Render("○"))
		}
		b.WriteByte(' ')
		b.WriteString(healthIcon(item.Health))
		b.WriteByte(' ')
		b.WriteString(fmt.Sprintf("%-*s", maxLen, item.Name))
		if item.Running {
			b.WriteString("  running")
		} else {
			b.WriteString("  stopped")
		}
	}
	return b.String()
}

// formatDotsGrouped renders multiple projects with their service statuses.
// Each project gets a header line followed by indented service lines.
func formatDotsGrouped(projects []projectServices) string {
	if len(projects) == 0 {
		return ""
	}

	var b strings.Builder
	for i, proj := range projects {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(proj.Name)

		maxLen := 0
		for _, item := range proj.Services {
			if len(item.Name) > maxLen {
				maxLen = len(item.Name)
			}
		}

		for _, item := range proj.Services {
			b.WriteByte('\n')
			b.WriteString("  ")
			if item.Running {
				b.WriteString(styleOK.Render("●"))
			} else {
				b.WriteString(styleFailed.Render("○"))
			}
			b.WriteByte(' ')
			b.WriteString(healthIcon(item.Health))
			b.WriteByte(' ')
			b.WriteString(fmt.Sprintf("%-*s", maxLen, item.Name))
			if item.Running {
				b.WriteString("  running")
			} else {
				b.WriteString("  stopped")
			}
		}
	}
	return b.String()
}

// formatJSON renders service statuses as a JSON array.
func formatJSON(items []serviceStatus) (string, error) {
	data, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("marshaling status: %w", err)
	}
	return string(data), nil
}

func newListCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List services and their running status",
		Long: `Shows all services defined in the compose file with their running/stopped status.

When no -C (project directory) is specified, discovers all compose projects and
displays services grouped by project. Works both locally and with -s (remote server).
When -C is specified, shows only that project's services in a flat list.`,
		Example: `  # List services in current directory (if compose file exists)
  cdeploy list

  # List all compose projects on the local system
  cdeploy list   # (when no compose file in current directory)

  # List all projects on a remote server
  cdeploy list -s prod

  # List a specific project
  cdeploy list -C /opt/myapp
  cdeploy list -s prod -C /opt/myapp

  # Output as JSON for scripting
  cdeploy list --json
  cdeploy list -s prod --json | jq '.[] | select(.running)'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd.Context(), jsonOutput)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")

	return cmd
}

// listSingleProject lists services for a single composer and prints the result.
func listSingleProject(ctx context.Context, c runner.Composer, jsonOutput bool) error {
	services, err := c.ListServices(ctx)
	if err != nil {
		return fmt.Errorf("listing services: %w", err)
	}

	status, err := c.ContainerStatus(ctx)
	if err != nil {
		return fmt.Errorf("getting container status: %w", err)
	}

	items := mergeStatus(services, status)

	if jsonOutput {
		out, err := formatJSON(items)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, out)
	} else {
		out := formatDots(items)
		if out != "" {
			fmt.Fprintln(os.Stdout, out)
		}
	}

	return nil
}

// collectMultiProject gathers service statuses for each project using the factory to create composers.
// Per-project errors are non-fatal: a warning is printed to stderr and the project is skipped.
func collectMultiProject(ctx context.Context, projects []compose.Project, factory func(dir string) runner.Composer) []projectServices {
	var result []projectServices
	for _, proj := range projects {
		c := factory(proj.ConfigDir)

		services, err := c.ListServices(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping project %q: %v\n", proj.Name, err)
			continue
		}

		status, err := c.ContainerStatus(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping project %q: %v\n", proj.Name, err)
			continue
		}

		items := mergeStatus(services, status)
		result = append(result, projectServices{Name: proj.Name, Services: items})
	}
	return result
}

// flattenProjectServices converts grouped project services to a flat slice with the Project field set.
func flattenProjectServices(projects []projectServices) []serviceStatus {
	var flat []serviceStatus
	for _, proj := range projects {
		for _, svc := range proj.Services {
			svc.Project = proj.Name
			flat = append(flat, svc)
		}
	}
	return flat
}

// printMultiProject formats and prints grouped project services.
func printMultiProject(grouped []projectServices, jsonOutput bool) error {
	if jsonOutput {
		flat := flattenProjectServices(grouped)
		out, err := formatJSON(flat)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, out)
	} else {
		out := formatDotsGrouped(grouped)
		if out != "" {
			fmt.Fprintln(os.Stdout, out)
		}
	}
	return nil
}

func runList(ctx context.Context, jsonOutput bool) error {
	dir := projectDir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("getting current directory: %w", err)
		}
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	if serverName != "" {
		cfg, err := config.Load(config.DefaultPath())
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		server, err := cfg.FindServer(serverName)
		if err != nil {
			return err
		}

		// For list, only honor explicit -C; ignore server.ProjectDir so
		// multi-project discovery works by default.
		projDir := projectDir

		rc := newRemote(server.Host, projDir)
		if err := rc.Connect(ctx); err != nil {
			return fmt.Errorf("connecting to %s: %w", serverName, err)
		}
		defer rc.Close()
		if err := rc.Detect(ctx); err != nil {
			return err
		}

		// Single-project mode: -C explicitly specified
		if projDir != "" {
			return listSingleProject(ctx, rc, jsonOutput)
		}

		// Multi-project mode: discover all projects on the server
		projects, err := rc.ListProjects(ctx)
		if err != nil {
			return fmt.Errorf("listing projects on %s: %w", serverName, err)
		}
		if len(projects) == 0 {
			fmt.Fprintln(os.Stderr, "no compose projects found on server")
			return nil
		}

		factory := func(d string) runner.Composer {
			rc2 := newRemote(server.Host, d)
			rc2.SetStandalone(rc.Standalone)
			return rc2
		}
		grouped := collectMultiProject(ctx, projects, factory)
		return printMultiProject(grouped, jsonOutput)
	}

	// Local mode: single-project only when -C is explicitly given
	c := newLocalComposer(dir)

	if projectDir != "" {
		if !hasLocalCompose(dir) {
			return fmt.Errorf("no compose file found in %s", dir)
		}
		if err := c.Detect(ctx); err != nil {
			return err
		}
		return listSingleProject(ctx, c, jsonOutput)
	}

	// Local multi-project: discover all projects on the system
	if err := c.Detect(ctx); err != nil {
		return err
	}
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("listing projects: %w", err)
	}
	if len(projects) == 0 {
		return fmt.Errorf("no compose projects found (use -C to specify a project directory)")
	}

	factory := func(d string) runner.Composer {
		lc := newLocalComposer(d)
		lc.SetStandalone(c.Standalone)
		return lc
	}
	grouped := collectMultiProject(ctx, projects, factory)
	return printMultiProject(grouped, jsonOutput)
}

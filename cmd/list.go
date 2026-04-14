package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/runner"
	"github.com/spf13/cobra"
)

type serviceStatus struct {
	Name    string `json:"service"`
	Running bool   `json:"running"`
	Health  string `json:"health,omitempty"`
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
		return styleOK.Render("H")
	case "unhealthy":
		return styleFailed.Render("U")
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
		Long:  "Shows all services defined in the compose file with their running/stopped status.",
		Example: `  # List services in current directory
  cdeploy list

  # List services on a remote server
  cdeploy list -s prod

  # Output as JSON for scripting
  cdeploy list --json
  cdeploy list --json | jq '.[] | select(.running)'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd.Context(), jsonOutput)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")

	return cmd
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

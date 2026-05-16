package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/lexxzar/compose-deploy/internal/compose"
	"github.com/lexxzar/compose-deploy/internal/config"
	"github.com/lexxzar/compose-deploy/internal/runner"
	"github.com/spf13/cobra"
)

var (
	listNewRemote  = compose.NewRemote
	listNewLocal   = func(dir string) *compose.Compose { return compose.New(dir) }
	listHasCompose = compose.HasComposeFile
)

type serviceStatus struct {
	Project string        `json:"project,omitempty"`
	Name    string        `json:"service"`
	Running bool          `json:"running"`
	Health  string        `json:"health,omitempty"`
	Created string        `json:"created,omitempty"`
	Uptime  string        `json:"uptime,omitempty"`
	Ports   []runner.Port `json:"ports,omitempty"`
	// Stats fields: populated only when --stats is set. Pointer types ensure
	// zero values (e.g. an idle container at 0% CPU) are still emitted in JSON
	// when stats were requested, while nil pointers are omitted entirely when
	// --stats was absent — preserving wire-shape compatibility.
	CPUPercent  *float64 `json:"cpu_percent,omitempty"`
	MemoryUsed  *int64   `json:"memory_used,omitempty"`
	MemoryLimit *int64   `json:"memory_limit,omitempty"`
}

// projectServices groups service statuses under a project name for grouped display.
type projectServices struct {
	Name     string
	Services []serviceStatus
}

// mergeStatus combines the canonical service list with container status.
// Services missing from the status map are treated as stopped.
func mergeStatus(services []string, status map[string]runner.ServiceStatus) []serviceStatus {
	return mergeStatusStats(services, status, nil, false)
}

// mergeStatusStats is the stats-aware variant of mergeStatus. When statsRequested
// is true, stats fields are populated for every service (with zero values for
// services absent from the stats map — typically stopped containers, which
// renders as blank cells). When statsRequested is false, stats fields stay nil
// and are omitted from JSON output (wire-shape compatible).
func mergeStatusStats(services []string, status map[string]runner.ServiceStatus, stats map[string]runner.ServiceStats, statsRequested bool) []serviceStatus {
	result := make([]serviceStatus, len(services))
	for i, svc := range services {
		st := status[svc]
		result[i] = serviceStatus{
			Name:    svc,
			Running: st.Running,
			Health:  st.Health,
			Created: st.Created,
			Uptime:  st.Uptime,
			Ports:   st.Ports,
		}
		if statsRequested {
			if s, ok := stats[svc]; ok {
				cpu := s.CPUPercent
				used := s.MemoryUsed
				limit := s.MemoryLimit
				result[i].CPUPercent = &cpu
				result[i].MemoryUsed = &used
				result[i].MemoryLimit = &limit
			} else {
				// Service is absent from stats (stopped, or race window between
				// ps and stats). Emit zero values so JSON consumers see the
				// fields and tabular output shows blank cells consistently.
				// Each pointer gets its own backing variable to avoid aliasing.
				zeroF := 0.0
				zeroUsed := int64(0)
				zeroLimit := int64(0)
				result[i].CPUPercent = &zeroF
				result[i].MemoryUsed = &zeroUsed
				result[i].MemoryLimit = &zeroLimit
			}
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

// formatCPUCell renders a service's CPU% cell. Running services with stats get
// "<x.y>%" (one decimal); other services (stopped, or stats fetch failed) get
// an empty string. The caller pads to the column width — empty strings render
// as blank cells.
func formatCPUCell(s serviceStatus) string {
	if s.CPUPercent == nil || !s.Running {
		return ""
	}
	return fmt.Sprintf("%.1f%%", *s.CPUPercent)
}

// formatMemCell renders a service's memory cell as "<used>/<limit>" using
// compose.FormatBytes. Stopped services or services without stats render as an
// empty string for blank-cell alignment.
func formatMemCell(s serviceStatus) string {
	if s.MemoryUsed == nil || s.MemoryLimit == nil || !s.Running {
		return ""
	}
	return compose.FormatBytes(*s.MemoryUsed) + "/" + compose.FormatBytes(*s.MemoryLimit)
}

// formatDots renders service statuses as colored dot lines with aligned names.
func formatDots(items []serviceStatus) string {
	if len(items) == 0 {
		return ""
	}

	maxName := 0
	maxCreated := 0
	maxUptime := 0
	maxCPU := 0
	maxMem := 0
	maxPorts := 0
	portsStr := make([]string, len(items))
	cpuStr := make([]string, len(items))
	memStr := make([]string, len(items))
	for i, item := range items {
		if len(item.Name) > maxName {
			maxName = len(item.Name)
		}
		if len(item.Created) > maxCreated {
			maxCreated = len(item.Created)
		}
		if len(item.Uptime) > maxUptime {
			maxUptime = len(item.Uptime)
		}
		cpuStr[i] = formatCPUCell(item)
		if w := utf8.RuneCountInString(cpuStr[i]); w > maxCPU {
			maxCPU = w
		}
		memStr[i] = formatMemCell(item)
		if w := utf8.RuneCountInString(memStr[i]); w > maxMem {
			maxMem = w
		}
		portsStr[i] = compose.FormatPorts(item.Ports)
		if w := utf8.RuneCountInString(portsStr[i]); w > maxPorts {
			maxPorts = w
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
		b.WriteString(fmt.Sprintf("%-*s", maxName, item.Name))
		if maxCreated > 0 {
			b.WriteString("  ")
			b.WriteString(fmt.Sprintf("%-*s", maxCreated, item.Created))
		}
		if maxUptime > 0 {
			b.WriteString("  ")
			b.WriteString(fmt.Sprintf("%-*s", maxUptime, item.Uptime))
		}
		if maxCPU > 0 {
			b.WriteString("  ")
			b.WriteString(fmt.Sprintf("%-*s", maxCPU, cpuStr[i]))
		}
		if maxMem > 0 {
			b.WriteString("  ")
			b.WriteString(fmt.Sprintf("%-*s", maxMem, memStr[i]))
		}
		if maxPorts > 0 {
			b.WriteString("  ")
			b.WriteString(fmt.Sprintf("%-*s", maxPorts, portsStr[i]))
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

		maxName := 0
		maxCreated := 0
		maxUptime := 0
		maxCPU := 0
		maxMem := 0
		maxPorts := 0
		portsStr := make([]string, len(proj.Services))
		cpuStr := make([]string, len(proj.Services))
		memStr := make([]string, len(proj.Services))
		for i, item := range proj.Services {
			if len(item.Name) > maxName {
				maxName = len(item.Name)
			}
			if len(item.Created) > maxCreated {
				maxCreated = len(item.Created)
			}
			if len(item.Uptime) > maxUptime {
				maxUptime = len(item.Uptime)
			}
			cpuStr[i] = formatCPUCell(item)
			if w := utf8.RuneCountInString(cpuStr[i]); w > maxCPU {
				maxCPU = w
			}
			memStr[i] = formatMemCell(item)
			if w := utf8.RuneCountInString(memStr[i]); w > maxMem {
				maxMem = w
			}
			portsStr[i] = compose.FormatPorts(item.Ports)
			if w := utf8.RuneCountInString(portsStr[i]); w > maxPorts {
				maxPorts = w
			}
		}

		for i, item := range proj.Services {
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
			b.WriteString(fmt.Sprintf("%-*s", maxName, item.Name))
			if maxCreated > 0 {
				b.WriteString("  ")
				b.WriteString(fmt.Sprintf("%-*s", maxCreated, item.Created))
			}
			if maxUptime > 0 {
				b.WriteString("  ")
				b.WriteString(fmt.Sprintf("%-*s", maxUptime, item.Uptime))
			}
			if maxCPU > 0 {
				b.WriteString("  ")
				b.WriteString(fmt.Sprintf("%-*s", maxCPU, cpuStr[i]))
			}
			if maxMem > 0 {
				b.WriteString("  ")
				b.WriteString(fmt.Sprintf("%-*s", maxMem, memStr[i]))
			}
			if maxPorts > 0 {
				b.WriteString("  ")
				b.WriteString(fmt.Sprintf("%-*s", maxPorts, portsStr[i]))
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
	var showStats bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List services and their running status",
		Long: `Shows all services defined in the compose file with their running/stopped status.

When no -C (project directory) is specified, discovers all compose projects and
displays services grouped by project. Works both locally and with -s (remote server).
When -C is specified, shows only that project's services in a flat list.

With --stats, includes per-service CPU and memory usage columns. The bulk
docker stats call adds ~1.5s of latency, so --stats is off by default; scripts
and CI pay nothing unless they ask for it. On stats fetch failure the rest of
the listing still renders and a warning is printed to stderr.`,
		Example: `  # List services in current directory (if compose file exists)
  cdeploy list

  # List all compose projects on the local system
  cdeploy list   # (when no compose file in current directory)

  # List all projects on a remote server
  cdeploy list -s prod

  # List a specific project
  cdeploy list -C /opt/myapp
  cdeploy list -s prod -C /opt/myapp

  # Include CPU and memory usage
  cdeploy list --stats
  cdeploy list -s prod --stats --json

  # Output as JSON for scripting
  cdeploy list --json
  cdeploy list -s prod --json | jq '.[] | select(.running)'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd.Context(), jsonOutput, showStats)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	cmd.Flags().BoolVar(&showStats, "stats", false, "include CPU and memory usage columns (adds ~1.5s latency)")

	return cmd
}

// listSingleProject lists services for a single composer and prints the result.
// When showStats is true, fetches per-service CPU/memory via ContainerStats.
// Stats fetch failures are soft: a warning is printed to stderr, stats cells
// render blank, and the listing succeeds (stats are a secondary view).
func listSingleProject(ctx context.Context, c runner.Composer, jsonOutput, showStats bool) error {
	services, err := c.ListServices(ctx)
	if err != nil {
		return fmt.Errorf("listing services: %w", err)
	}

	status, err := c.ContainerStatus(ctx)
	if err != nil {
		return fmt.Errorf("getting container status: %w", err)
	}

	var stats map[string]runner.ServiceStats
	if showStats {
		stats, err = c.ContainerStats(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cdeploy: stats unavailable: %v\n", err)
			stats = nil // fall through with empty stats map → blank cells
		}
	}

	items := mergeStatusStats(services, status, stats, showStats)

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
	return collectMultiProjectStats(ctx, projects, factory, false)
}

// collectMultiProjectStats is the stats-aware variant of collectMultiProject.
// When showStats is true, it invokes ContainerStats() on each project's composer.
// Per-project stats failures degrade gracefully: a warning is logged to stderr,
// stats cells render blank for that project, and the rest of the listing
// continues. Per-project status (ListServices/ContainerStatus) failures remain
// fatal-to-the-project (the project is skipped entirely), matching the existing
// convention — stats is strictly additive and never causes a project to drop.
//
// Note: each project's composer calls ContainerStats independently, which —
// under the current Composer interface — performs its own host-wide
// `docker stats` fetch. A future optimization could share one bulk call across
// projects; the interface intentionally does not expose that machinery so the
// trade-off is contained to this file when needed.
func collectMultiProjectStats(ctx context.Context, projects []compose.Project, factory func(dir string) runner.Composer, showStats bool) []projectServices {
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

		var stats map[string]runner.ServiceStats
		if showStats {
			stats, err = c.ContainerStats(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cdeploy: stats unavailable for %q: %v\n", proj.Name, err)
				stats = nil
			}
		}

		items := mergeStatusStats(services, status, stats, showStats)
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

func runList(ctx context.Context, jsonOutput, showStats bool) error {
	if err := checkRemoteMutex(serverName, sshTarget, identityFile); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	if sshTarget != "" {
		// --ssh always implies a single project (resolveSSHRemote requires --project-dir).
		rc, cleanup, err := resolveSSHRemote(ctx, sshTarget, projectDir, identityFile, listNewRemote)
		if err != nil {
			return err
		}
		defer cleanup()
		return listSingleProject(ctx, rc, jsonOutput, showStats)
	}

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

		rc := listNewRemote(server.Host, projDir)
		if err := rc.Connect(ctx); err != nil {
			return fmt.Errorf("connecting to %s: %w", serverName, err)
		}
		defer rc.Close()
		if err := rc.Detect(ctx); err != nil {
			return err
		}

		// Single-project mode: -C explicitly specified
		if projDir != "" {
			return listSingleProject(ctx, rc, jsonOutput, showStats)
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
			rc2 := listNewRemote(server.Host, d)
			rc2.SetStandalone(rc.Standalone)
			return rc2
		}
		grouped := collectMultiProjectStats(ctx, projects, factory, showStats)
		return printMultiProject(grouped, jsonOutput)
	}

	// Local mode: single-project only when -C is explicitly given
	dir := projectDir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("getting current directory: %w", err)
		}
	}
	c := listNewLocal(dir)

	if projectDir != "" {
		if !listHasCompose(dir) {
			return fmt.Errorf("no compose file found in %s", dir)
		}
		if err := c.Detect(ctx); err != nil {
			return err
		}
		return listSingleProject(ctx, c, jsonOutput, showStats)
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
		lc := listNewLocal(d)
		lc.SetStandalone(c.Standalone)
		return lc
	}
	grouped := collectMultiProjectStats(ctx, projects, factory, showStats)
	return printMultiProject(grouped, jsonOutput)
}

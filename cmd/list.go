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
	// UpdateAvailable is the tri-state "newer image in registry" hint. nil =
	// unknown (not checked, build-only, error), &true = update available,
	// &false = current. Pointer + omitempty keeps the field out of JSON output
	// for callers that did not opt into update detection (--updates is opt-in
	// in both single- and multi-project modes), so existing JSON consumers see
	// the original wire shape.
	UpdateAvailable *bool `json:"update_available,omitempty"`
}

// projectServices groups service statuses under a project name for grouped display.
type projectServices struct {
	Name     string
	Services []serviceStatus
}

// mergeStatus combines the canonical service list with container status.
// Services missing from the status map are treated as stopped.
func mergeStatus(services []string, status map[string]runner.ServiceStatus) []serviceStatus {
	return mergeStatusStats(services, status, nil, false, nil)
}

// mergeStatusStats is the stats-aware variant of mergeStatus. When statsRequested
// is true, stats fields are populated for every service (with zero values for
// services absent from the stats map — typically stopped containers, which
// renders as blank cells). When statsRequested is false, stats fields stay nil
// and are omitted from JSON output (wire-shape compatible).
//
// The updates map is tri-state per service: presence in the map sets
// UpdateAvailable to &v (so &true and &false are both possible), absence
// leaves it nil. A nil map skips hydration entirely — callers that did not
// opt into update detection get the legacy wire shape.
func mergeStatusStats(services []string, status map[string]runner.ServiceStatus, stats map[string]runner.ServiceStats, statsRequested bool, updates map[string]bool) []serviceStatus {
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
		if statsRequested && stats != nil {
			if s, ok := stats[svc]; ok {
				cpu := s.CPUPercent
				used := s.MemoryUsed
				limit := s.MemoryLimit
				result[i].CPUPercent = &cpu
				result[i].MemoryUsed = &used
				result[i].MemoryLimit = &limit
			}
			// Service absent from stats (stopped, or ps/stats race): pointers
			// stay nil so tabular cells render blank and JSON omits the fields.
			// A legitimate zero from docker (idle container at 0% CPU) arrives
			// as an entry in the stats map with zero-valued fields — that case
			// still emits &0 pointers and renders "0.0%" / "0B/0B".
		}
		if updates != nil {
			if v, ok := updates[svc]; ok {
				vv := v
				result[i].UpdateAvailable = &vv
			}
			// Absent from updates: pointer stays nil (build-only services, or
			// services dropped by the parser) — renders as blank cell, JSON
			// omits the field.
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

	// Always reserve 2 trailing cells in the name column for the inline update
	// glyph (leading space + U+21E7), regardless of whether any service in the
	// rendered list currently carries the flag. Reserving unconditionally
	// keeps following columns aligned across invocations and matches the TUI
	// rendering (see internal/tui/app.go).
	maxName += 2

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
		// nameCell is padded manually rather than via %-*s because the glyph
		// is multi-byte (3 bytes) but renders in one terminal cell, and the
		// styled rendering carries ANSI escapes that don't count toward
		// display width. utf8.RuneCountInString is the right width metric.
		nameWidth := utf8.RuneCountInString(item.Name)
		nameCell := item.Name
		if item.UpdateAvailable != nil && *item.UpdateAvailable {
			nameCell = item.Name + " " + styleWarning.Render(compose.UpdateGlyph)
			nameWidth += 2 // space + glyph cell
		}
		if pad := maxName - nameWidth; pad > 0 {
			nameCell += strings.Repeat(" ", pad)
		}
		b.WriteString(nameCell)
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

		// Always reserve +2 cells in name column for the inline update glyph,
		// regardless of whether any service in this project currently has the
		// flag. Per-project (not global) so each project's column widths stay
		// independent — matches the existing per-project pattern for
		// Created/Uptime/CPU/Mem.
		maxName += 2

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
			// Manual padding because the glyph is multi-byte / single-cell
			// and the styled rendering carries ANSI escapes that don't count
			// toward display width.
			nameWidth := utf8.RuneCountInString(item.Name)
			nameCell := item.Name
			if item.UpdateAvailable != nil && *item.UpdateAvailable {
				nameCell = item.Name + " " + styleWarning.Render(compose.UpdateGlyph)
				nameWidth += 2
			}
			if pad := maxName - nameWidth; pad > 0 {
				nameCell += strings.Repeat(" ", pad)
			}
			b.WriteString(nameCell)
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
	var showUpdates bool

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
the listing still renders and a warning is printed to stderr.

With --updates, includes a per-service image-update indicator: a ⇧ glyph next
to services whose registry image is newer than the local one. Each service
costs one registry round-trip (buildx/manifest-inspect); for projects with
many services this can add noticeable latency (especially on remote SSH), so
--updates is off by default in both single- and multi-project modes. Failures
are non-fatal: a warning is written to stderr, the cell stays blank, and the
rest of the listing renders.`,
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

  # Include image-update indicators (opt-in; costs one registry probe per service)
  cdeploy list --updates
  cdeploy list -C /opt/myapp --updates
  cdeploy list -s prod --updates --json

  # Output as JSON for scripting
  cdeploy list --json
  cdeploy list -s prod --json | jq '.[] | select(.running)'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd.Context(), jsonOutput, showStats, showUpdates)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	cmd.Flags().BoolVar(&showStats, "stats", false, "include CPU and memory usage columns (adds ~1.5s latency)")
	cmd.Flags().BoolVar(&showUpdates, "updates", false, "check each service's image for available updates (adds one registry round-trip per service)")

	return cmd
}

// listSingleProject lists services for a single composer and prints the result.
// When showStats is true, fetches per-service CPU/memory via ContainerStats.
// Stats fetch failures are soft: a warning is printed to stderr, stats cells
// render blank, and the listing succeeds (stats are a secondary view).
//
// checkUpdates controls whether per-service update verdicts are fetched via
// Composer.CheckUpdates. The flag is decoupled from showStats so callers can
// opt into either view independently. On error: stderr warning "cdeploy:
// updates unavailable: <err>" (mirrors the existing stats-failure phrasing),
// exit 0, update_available cells stay nil / blank.
func listSingleProject(ctx context.Context, c runner.Composer, jsonOutput, showStats, checkUpdates bool) error {
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
			stats = nil // mergeStatusStats leaves pointers nil → blank cells, fields omitted from JSON
		}
	}

	var updates map[string]bool
	if checkUpdates {
		updates, err = c.CheckUpdates(ctx, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cdeploy: updates unavailable: %v\n", err)
			updates = nil // mergeStatusStats leaves UpdateAvailable nil → blank cell, field omitted from JSON
		}
	}

	items := mergeStatusStats(services, status, stats, showStats, updates)

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

// listComposerFactory builds the read-only composer one discovered project is
// listed through. It takes the WHOLE project, not just its directory, mirroring
// tui.ComposerFactory: a directory does not identify a project. Two projects
// deployed with `docker compose -p blue` / `-p green` in one tree share a
// ConfigDir, so a directory-keyed composer listed the SAME container set under
// both headers — identical services, status, stats and update verdicts, in text
// and in --json — while the TUI showed the two apart on the same host.
type listComposerFactory func(proj compose.Project) runner.Composer

// collectMultiProject gathers service statuses for each project using the factory to create composers.
// Per-project errors are non-fatal: a warning is printed to stderr and the project is skipped.
func collectMultiProject(ctx context.Context, projects []compose.Project, factory listComposerFactory) []projectServices {
	return collectMultiProjectStats(ctx, projects, factory, false, nil, false)
}

// bulkStatsAggregator is the optional capability that a composer can implement
// to consume a pre-fetched host-wide `docker stats` map and join only its own
// `docker compose ps` output against it. Both *compose.Compose and
// *compose.RemoteCompose satisfy this; test mocks that don't implement it fall
// back to the per-project ContainerStats() path.
type bulkStatsAggregator interface {
	ContainerStatsFromBulk(ctx context.Context, bulk map[string]runner.ServiceStats) (map[string]runner.ServiceStats, error)
}

// collectMultiProjectStats is the stats-aware variant of collectMultiProject.
// When showStats is true, it populates per-service CPU/Mem cells for every
// project. Per-project stats failures degrade gracefully: a warning is logged
// to stderr, stats cells render blank for that project, and the rest of the
// listing continues. Per-project status (ListServices/ContainerStatus)
// failures remain fatal-to-the-project (the project is skipped entirely),
// matching the existing convention — stats is strictly additive and never
// causes a project to drop.
//
// Bulk-call sharing: when bulkStats is non-nil and the per-project composer
// implements bulkStatsAggregator, ContainerStatsFromBulk runs against the
// pre-fetched map — only `docker compose ps` runs per project, so the host
// pays one ~1.5s `docker stats` cost regardless of project count. Callers
// that fail to pre-fetch should still pass a non-nil empty map so this path
// is preserved (no N×retry of the host-wide stats call). When bulkStats is
// nil, the composer's ContainerStats() runs per project as a fallback —
// reserved for callers that didn't request bulk sharing at all (e.g. test
// mocks that don't implement bulkStatsAggregator).
func collectMultiProjectStats(ctx context.Context, projects []compose.Project, factory listComposerFactory, showStats bool, bulkStats map[string]runner.ServiceStats, checkUpdates bool) []projectServices {
	var result []projectServices
	for _, proj := range projects {
		// An empty ConfigDir means docker reported no compose file for this
		// project, so there is no directory to run compose in. The factory
		// would hand back a composer rooted at cdeploy's own cwd, and every
		// service it then listed would be printed under THIS project's header.
		// Skipping matches the per-project warn-and-continue rule below.
		if proj.ConfigDir == "" {
			fmt.Fprintf(os.Stderr, "warning: skipping project %q: no compose file reported for it\n", proj.Name)
			continue
		}
		c := factory(proj)

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
			if bsa, ok := c.(bulkStatsAggregator); ok && bulkStats != nil {
				stats, err = bsa.ContainerStatsFromBulk(ctx, bulkStats)
			} else {
				stats, err = c.ContainerStats(ctx)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "cdeploy: stats unavailable for %q: %v\n", proj.Name, err)
				stats = nil
			}
		}

		// Per-project update check is opt-in (--updates flag) because each
		// project triggers its own registry probes (dry-run or manifest
		// inspect) — at multi-project scale on a host with many projects
		// this can balloon registry traffic. Failure is non-fatal: warn,
		// blank cells, continue. Phrasing mirrors `stats unavailable for ...`.
		var updates map[string]bool
		if checkUpdates {
			updates, err = c.CheckUpdates(ctx, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cdeploy: updates unavailable for %q: %v\n", proj.Name, err)
				updates = nil
			}
		}

		items := mergeStatusStats(services, status, stats, showStats, updates)
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

func runList(ctx context.Context, jsonOutput, showStats, showUpdates bool) error {
	if err := checkRemoteMutex(serverName, sshTarget, identityFile); err != nil {
		return err
	}

	// `list` without -C is host-wide discovery: it builds one composer per
	// project it finds, each already carrying that project's own name, so there
	// is nothing for --project-name to select. Refusing beats silently ignoring
	// a flag the user spelled to narrow the output.
	if projectName != "" && projectDir == "" {
		return fmt.Errorf("--project-name requires --project-dir (list without -C discovers every project)")
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	if sshTarget != "" {
		// --ssh always implies a single project (resolveSSHRemote requires --project-dir).
		// Update check is opt-in via --updates because each service costs one
		// SSH round-trip to buildx/manifest-inspect — 20+ services adds 10s+.
		rc, cleanup, err := resolveSSHRemote(ctx, sshTarget, projectDir, projectName, identityFile, listNewRemote)
		if err != nil {
			return err
		}
		defer cleanup()
		return listSingleProject(ctx, rc, jsonOutput, showStats, showUpdates)
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
		// Only the single-project path (projDir set) reads this; multi-project
		// discovery is refused above when --project-name is set, and the
		// per-project factory names each composer from its own row.
		rc.ProjectName = projectName
		if err := rc.Connect(ctx); err != nil {
			return fmt.Errorf("connecting to %s: %w", serverName, err)
		}
		defer rc.Close()
		if err := rc.Detect(ctx); err != nil {
			return err
		}

		// Single-project mode: -C explicitly specified. Update check is opt-in
		// via --updates because each service costs one SSH round-trip to
		// buildx/manifest-inspect.
		if projDir != "" {
			return listSingleProject(ctx, rc, jsonOutput, showStats, showUpdates)
		}

		// Multi-project mode: discover all projects on the server.
		// Updates are opt-in via --updates (registry probes per project).
		projects, err := rc.ListProjects(ctx)
		if err != nil {
			return fmt.Errorf("listing projects on %s: %w", serverName, err)
		}
		if len(projects) == 0 {
			fmt.Fprintln(os.Stderr, "no compose projects found on server")
			return nil
		}

		// The remote rows keep the pure PinComposeFiles: resolving compose's
		// discovery precedence needs the project directory read, which is an
		// SSH round trip per project here. See remoteComposerFor in root.go.
		factory := func(proj compose.Project) runner.Composer {
			rc2 := listNewRemote(server.Host, proj.ConfigDir)
			rc2.ProjectName = proj.Name
			rc2.ComposeFiles = compose.PinComposeFiles(proj.ConfigDir, proj.ConfigFiles)
			rc2.SSHExtraArgs = rc.SSHExtraArgs
			rc2.SetStandalone(rc.Standalone)
			return rc2
		}
		// One host-wide `docker stats` call shared across every project. When
		// the bulk fetch fails, warn once and pass a non-nil empty map so the
		// per-project loop still uses the bulk path (just `docker compose ps`)
		// instead of retrying the host-wide stats call N more times.
		var bulkStats map[string]runner.ServiceStats
		if showStats {
			bulkStats, err = compose.AllContainerStatsRemote(ctx, rc)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cdeploy: stats unavailable: %v\n", err)
				bulkStats = map[string]runner.ServiceStats{}
			}
		}
		grouped := collectMultiProjectStats(ctx, projects, factory, showStats, bulkStats, showUpdates)
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
		// Single-project mode. Update check is opt-in via --updates because
		// each service costs one registry round-trip to buildx/manifest-inspect.
		//
		// Same split as the remote branch: only this path carries a name and
		// resolves an identity; the multi-project factory below names each
		// composer from its own row.
		if !listHasCompose(dir) {
			return fmt.Errorf("no compose file found in %s", dir)
		}
		if err := prepareLocalComposer(ctx, c, projectName); err != nil {
			return err
		}
		return listSingleProject(ctx, c, jsonOutput, showStats, showUpdates)
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

	factory := func(proj compose.Project) runner.Composer {
		lc := listNewLocal(proj.ConfigDir)
		lc.ProjectName = proj.Name
		lc.ComposeFiles = compose.PinComposeFilesLocal(proj.ConfigDir, proj.ConfigFiles)
		lc.SetStandalone(c.Standalone)
		return lc
	}
	// Single host-wide stats fetch shared across all local projects. On
	// failure pass a non-nil empty map so per-project loop uses the bulk
	// path (just `docker compose ps`) instead of retrying `docker stats`
	// N more times.
	var bulkStats map[string]runner.ServiceStats
	if showStats {
		bulkStats, err = compose.AllContainerStats(ctx, c)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cdeploy: stats unavailable: %v\n", err)
			bulkStats = map[string]runner.ServiceStats{}
		}
	}
	grouped := collectMultiProjectStats(ctx, projects, factory, showStats, bulkStats, showUpdates)
	return printMultiProject(grouped, jsonOutput)
}

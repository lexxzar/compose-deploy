package runner

import (
	"context"
	"io"
)

// Port describes a single published port mapping for a service.
// Aggregated across replicas at the ServiceStatus level: deduped by
// (Host, HostPort, ContainerPort, Protocol) and sorted ascending by HostPort.
//
// JSON tags are intentionally present: cmd/list.go re-exports runner.Port
// directly in its `--json` output, so the wire shape is owned here. Other
// runner types are wire-format-agnostic.
type Port struct {
	Host          string `json:"host"`           // bind interface, e.g. "0.0.0.0", "127.0.0.1"
	HostPort      int    `json:"host_port"`      // published host port
	ContainerPort int    `json:"container_port"` // target container port
	Protocol      string `json:"protocol"`       // "tcp", "udp", "sctp"
}

// ServiceStatus holds the running state and health check status of a service.
type ServiceStatus struct {
	Running bool
	Health  string // "healthy", "unhealthy", "starting", or "" (no healthcheck)
	Created string // formatted creation time, e.g. "2024-01-15 09:30"
	Uptime  string // compact uptime, e.g. "3h", "2d", or "" if not running
	Ports   []Port // aggregated/deduped/sorted across replicas; see Port doc
	// UpdateAvailable is a tri-state hint about whether a newer image exists in
	// the registry for this service's image. nil = unknown (not checked, build-only,
	// or an error occurred); &true = update available; &false = current. Populated
	// by callers that invoke Composer.CheckUpdates; absent by default so existing
	// status reads remain backward compatible.
	UpdateAvailable *bool
}

// ServiceStats holds CPU and memory usage for a service, sourced from
// `docker stats --no-stream --format json`.
//
// Aggregation contract for scaled services (multiple replicas of the same
// service name): all three fields are summed across replicas. A 3-replica
// service each using 50% CPU and 100MiB of a 512MiB limit reports
// CPUPercent=150.0, MemoryUsed=300MiB, MemoryLimit=1536MiB. This matches
// the "how much is this service costing me" intuition that users budget
// against. Only running containers are included; stopped services are
// absent from the result map of ContainerStats.
type ServiceStats struct {
	CPUPercent  float64 // 100.0 = 1 full core; can exceed 100 for scaled or multi-core saturated services
	MemoryUsed  int64   // bytes
	MemoryLimit int64   // bytes; whatever Docker reports (often host memory if no explicit limit)
}

// Composer is the interface consumed by the runner, implemented by compose.Compose.
type Composer interface {
	Stop(ctx context.Context, containers []string, w io.Writer) error
	Remove(ctx context.Context, containers []string, w io.Writer) error
	Pull(ctx context.Context, containers []string, w io.Writer) error
	Create(ctx context.Context, containers []string, w io.Writer) error
	Start(ctx context.Context, containers []string, w io.Writer) error
	ListServices(ctx context.Context) ([]string, error)
	// ContainerStatus returns a map of service name to ServiceStatus.
	// For scaled services, Running uses OR (any running = running),
	// Health uses worst-case priority (unhealthy > starting > healthy),
	// Created uses the oldest replica's creation time, and
	// Uptime uses the longest-running replica's uptime string.
	// Ports are deduped across replicas by (Host, HostPort, ContainerPort,
	// Protocol) and sorted ascending by HostPort.
	ContainerStatus(ctx context.Context) (map[string]ServiceStatus, error)
	// ContainerStats returns a map of service name to ServiceStats.
	// Only running containers are included; stopped services are absent.
	// For scaled services (multiple replicas of the same service name),
	// CPUPercent, MemoryUsed, and MemoryLimit are all summed across
	// replicas — see the ServiceStats type doc for the full contract.
	ContainerStats(ctx context.Context) (map[string]ServiceStats, error)
	// CheckUpdates returns a map of service name to "update available" verdict.
	// An empty services slice means "check every service in the project".
	// Only services for which a verdict could be derived appear in the map;
	// absent entries mean "unknown" (build-only services, registry errors,
	// per-image inspect failures, etc.) and the caller must treat that as
	// the tri-state nil — same contract as ServiceStatus.UpdateAvailable.
	//
	// Error contract: when a non-nil error is returned, callers SHOULD treat
	// the accompanying map as untrusted. Implementations may return a partial
	// map alongside an error (e.g. RemoteCompose returns the verdicts it
	// resolved before an SSH transport failure aborted the batch), but those
	// partial verdicts may be inconsistent with the unresolved services — a
	// service shown as "current" might actually have an update that the
	// failed fetch would have detected. Soft-failure consumers (CLI list,
	// TUI) should display the error and discard the partial verdicts (i.e.
	// `if err != nil { updates = nil }`) rather than rendering a mixed view.
	// Tests/callers that need every-verdict-or-fail semantics can still
	// detect via `err != nil`.
	CheckUpdates(ctx context.Context, services []string) (map[string]bool, error)
	// Logs streams docker compose logs for a single service to w.
	// When follow is true, it streams until ctx is cancelled.
	// tail controls how many historical lines to show (0 = all).
	Logs(ctx context.Context, service string, follow bool, tail int, w io.Writer) error
}

// Operation represents the type of deployment operation.
type Operation int

const (
	Restart  Operation = iota // stop → rm → create → start
	Deploy                    // stop → rm → pull → create → start
	StopOnly                  // stop
)

func (o Operation) String() string {
	switch o {
	case Restart:
		return "Restart"
	case Deploy:
		return "Deploy"
	case StopOnly:
		return "Stop"
	default:
		return "Unknown"
	}
}

// Step names for events.
const (
	StepStopping = "Stopping"
	StepRemoving = "Removing"
	StepPulling  = "Pulling"
	StepCreating = "Creating"
	StepStarting = "Starting"
)

// Status values for events.
const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// StepEvent reports progress of a pipeline step.
type StepEvent struct {
	Step   string
	Status string
	Err    error
}

// Steps returns the ordered step names for an operation.
func Steps(op Operation) []string {
	switch op {
	case Deploy:
		return []string{StepStopping, StepRemoving, StepPulling, StepCreating, StepStarting}
	case StopOnly:
		return []string{StepStopping}
	default: // Restart
		return []string{StepStopping, StepRemoving, StepCreating, StepStarting}
	}
}

type stepFunc func(ctx context.Context, containers []string, w io.Writer) error

// Run executes the operation pipeline, sending StepEvents to the events channel.
// The channel is closed when the pipeline completes or fails.
func Run(ctx context.Context, c Composer, op Operation, containers []string, w io.Writer, events chan<- StepEvent) {
	defer close(events)

	steps := buildSteps(c, op)
	for _, s := range steps {
		events <- StepEvent{Step: s.name, Status: StatusRunning}

		if err := s.fn(ctx, containers, w); err != nil {
			events <- StepEvent{Step: s.name, Status: StatusFailed, Err: err}
			return
		}

		events <- StepEvent{Step: s.name, Status: StatusDone}
	}
}

type step struct {
	name string
	fn   stepFunc
}

func buildSteps(c Composer, op Operation) []step {
	switch op {
	case StopOnly:
		return []step{{StepStopping, c.Stop}}
	default:
		base := []step{
			{StepStopping, c.Stop},
			{StepRemoving, c.Remove},
		}
		if op == Deploy {
			base = append(base, step{StepPulling, c.Pull})
		}
		base = append(base,
			step{StepCreating, c.Create},
			step{StepStarting, c.Start},
		)
		return base
	}
}

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

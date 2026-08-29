package compose

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// errNoContainer is the one producer of the "no container found" text. Both
// compose Inspect methods reach it through resolveInspectID and
// HostContainers.Inspect calls it directly, so the wording two test suites pin
// as a user-visible contract cannot be spelled two ways.
func errNoContainer(name string) error {
	return fmt.Errorf("no container found for %q", name)
}

// pickInspectContainer selects which replica of a compose service to inspect.
//
// The selection rule MIRRORS the Uptime aggregation gate in parseContainerStatus
// (compose.go, the svcAgg block): any running replica beats a restarting one, and
// among running replicas the longest actual uptime wins. A naive max-duration
// picker would disagree with the Uptime column the container screen renders on a
// running+restarting mix, so the two must stay in step.
//
// When no replica satisfies the gate (a stopped or exited service has no uptime at
// all) the first matching entry is returned, so a stopped container is still
// inspectable. Only a service with no matching entry at all yields ("", false).
func pickInspectContainer(entries []psEntry, service string) (string, bool) {
	if service == "" {
		return "", false
	}

	var (
		fallback string
		chosen   string
		// longestFromRunning separates "no running replica seen yet" from "a
		// running replica whose parsed duration is zero", which a zero
		// longestUpDur alone cannot.
		longestUpDur       time.Duration
		longestFromRunning bool
	)

	for _, entry := range entries {
		if entry.Service != service || entry.ID == "" {
			continue
		}
		if fallback == "" {
			fallback = entry.ID
		}

		uptime := formatUptime(entry.Status)
		switch {
		case entry.State == "running" && uptime != "":
			if dur := parseUptimeDuration(uptime); !longestFromRunning || dur > longestUpDur {
				longestUpDur, longestFromRunning, chosen = dur, true, entry.ID
			}
		case uptime == "restarting" && chosen == "":
			chosen = entry.ID
		}
	}

	if chosen == "" {
		chosen = fallback
	}
	return chosen, chosen != ""
}

// resolveInspectID turns one `docker compose ps` payload into the container ID
// to inspect. Both Compose.Inspect and RemoteCompose.Inspect share this middle
// half — only their transports differ.
func resolveInspectID(psOut []byte, service string) (string, error) {
	entries, err := parsePsEntries(psOut)
	if err != nil {
		return "", err
	}
	id, ok := pickInspectContainer(entries, service)
	if !ok {
		return "", errNoContainer(service)
	}
	return id, nil
}

// pickHostInspectContainer selects the unmanaged host container to inspect.
//
// Docker enforces unique container names per host and HostContainers keys its
// status map one-to-one, so this is a first match with no longest-running rule.
func pickHostInspectContainer(entries []hostPsEntry, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	for _, e := range entries {
		if e.ID != "" && hostContainerName(e.Names) == name {
			return e.ID, true
		}
	}
	return "", false
}

// parsePsEntries parses `docker compose ps --format json` into the raw entry
// slice the inspect pickers consume. Docker Compose v2.21+ emits a JSON array;
// older versions emit NDJSON, so both shapes are accepted — the same tolerance
// parseContainerStatus and parsePsIDToService apply to the very same output.
func parsePsEntries(data []byte) ([]psEntry, error) {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "[]" {
		return nil, nil
	}

	var entries []psEntry
	if strings.HasPrefix(s, "[") {
		if err := json.Unmarshal([]byte(s), &entries); err != nil {
			return nil, fmt.Errorf("parsing ps for inspect: %w", err)
		}
		return entries, nil
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e psEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("parsing ps for inspect: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// InspectDoc is the narrow projection of `docker inspect` that the summary
// renderer consumes — only the fields the five summary sections draw, not a full
// Docker API type. Raw mode reads the original bytes, so nothing here has to
// round-trip.
//
// The whole family is exported because the renderer lives in internal/tui and
// builds these values directly in its table tests; an exported field carrying an
// unexported type would be readable there but not constructible.
type InspectDoc struct {
	Name         string            `json:"Name"`
	Image        string            `json:"Image"`
	RestartCount int               `json:"RestartCount"`
	State        InspectState      `json:"State"`
	Config       InspectConfig     `json:"Config"`
	HostConfig   InspectHostConfig `json:"HostConfig"`
	Mounts       []InspectMount    `json:"Mounts"`

	// Filled by a SECOND, local `docker image inspect`, never by the container
	// document: docker's own `Created` key there is when the CONTAINER was
	// created, so the tag keeps that value out of this field.
	ImageCreated time.Time `json:"-"`
}

// ImageRef names the image to ask image-level questions about. The resolved ID
// wins: it addresses what the container runs even after the tag has moved.
func (d InspectDoc) ImageRef() string {
	if d.Image != "" {
		return d.Image
	}
	return d.Config.Image
}

// InspectState mirrors the `.State` object. Health is a pointer because docker
// omits the key entirely for an image with no healthcheck, and the HEALTH section
// is dropped on exactly that distinction — a zero-valued struct could not carry it.
type InspectState struct {
	Status string `json:"Status"`
	// Running gates the exit-code row: a running container's ExitCode is
	// always 0 and says nothing.
	Running   bool `json:"Running"`
	OOMKilled bool `json:"OOMKilled"`
	ExitCode  int  `json:"ExitCode"`
	// Error is docker's own reason a container failed to start (a missing
	// executable, an OOM detail). On a stopped container it is often the whole
	// answer, so the STATE section renders it when non-empty.
	Error     string         `json:"Error"`
	StartedAt string         `json:"StartedAt"`
	Health    *InspectHealth `json:"Health"`
}

// InspectHealth mirrors `.State.Health`.
type InspectHealth struct {
	Status        string             `json:"Status"`
	FailingStreak int                `json:"FailingStreak"`
	Log           []InspectHealthLog `json:"Log"`
}

// InspectHealthLog is one probe result in `.State.Health.Log`. Output is the
// probe's combined stdout/stderr — the field the inspect screen exists for.
type InspectHealthLog struct {
	End      string `json:"End"`
	ExitCode int    `json:"ExitCode"`
	Output   string `json:"Output"`
}

// InspectConfig mirrors the `.Config` object.
type InspectConfig struct {
	Image       string              `json:"Image"`
	Cmd         []string            `json:"Cmd"`
	Entrypoint  []string            `json:"Entrypoint"`
	Env         []string            `json:"Env"`
	Healthcheck *InspectHealthcheck `json:"Healthcheck"`
}

// InspectHealthcheck mirrors `.Config.Healthcheck`. Docker reports the three
// intervals as nanosecond counts, which unmarshal into time.Duration directly, so
// the renderer gets "5s" from String() with no conversion of its own.
type InspectHealthcheck struct {
	Test        []string      `json:"Test"`
	Interval    time.Duration `json:"Interval"`
	Timeout     time.Duration `json:"Timeout"`
	StartPeriod time.Duration `json:"StartPeriod"`
	Retries     int           `json:"Retries"`
}

// InspectHostConfig mirrors the `.HostConfig` fields the STATE section renders.
type InspectHostConfig struct {
	RestartPolicy InspectRestartPolicy `json:"RestartPolicy"`
}

// InspectRestartPolicy mirrors `.HostConfig.RestartPolicy`.
type InspectRestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

// InspectMount is one entry of the top-level `.Mounts` array. Name is set for
// named volumes and empty for bind mounts.
type InspectMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// ParseInspect parses `docker inspect` output into an InspectDoc.
//
// docker inspect always emits a JSON array, even for one container. cdeploy asks
// about exactly one, so the first element is the answer; a multi-element array is
// not an error, it just takes the first. An empty array means the ID resolved to
// nothing and is reported as such rather than yielding a blank summary.
func ParseInspect(raw []byte) (InspectDoc, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return InspectDoc{}, fmt.Errorf("parsing docker inspect: empty output")
	}

	var docs []InspectDoc
	if err := json.Unmarshal([]byte(s), &docs); err != nil {
		return InspectDoc{}, fmt.Errorf("parsing docker inspect: %w", err)
	}
	if len(docs) == 0 {
		return InspectDoc{}, fmt.Errorf("parsing docker inspect: no container in output")
	}

	doc := docs[0]
	// docker reports the container name with a leading slash; every other name in
	// this package is bare, so it is stripped here rather than in the renderer.
	doc.Name = strings.TrimPrefix(doc.Name, "/")
	return doc, nil
}

package compose

import (
	"fmt"
	"strings"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

// FormatPort renders a single Port as "host:hostPort→containerPort"
// with an optional "/proto" suffix when Protocol is non-empty and not "tcp".
// Examples:
//
//	{Host: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}   -> "0.0.0.0:8080→80"
//	{Host: "0.0.0.0", HostPort: 1812, ContainerPort: 1812, Protocol: "udp"} -> "0.0.0.0:1812→1812/udp"
//	{Host: "127.0.0.1", HostPort: 9000, ContainerPort: 9000}                -> "127.0.0.1:9000→9000"
func FormatPort(p runner.Port) string {
	base := fmt.Sprintf("%s:%d→%d", p.Host, p.HostPort, p.ContainerPort)
	if p.Protocol != "" && p.Protocol != "tcp" {
		base += "/" + p.Protocol
	}
	return base
}

// FormatPorts renders a slice of Ports as a comma-joined string.
// Returns "" for an empty/nil slice.
func FormatPorts(ports []runner.Port) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, FormatPort(p))
	}
	return strings.Join(parts, ", ")
}

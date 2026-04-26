package compose

import (
	"fmt"
	"strings"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

// FormatPort renders a single Port as "host:hostPort→containerPort" (IPv4) or
// "[host]:hostPort→containerPort" (IPv6) with an optional "/proto" suffix when
// Protocol is non-empty and not "tcp". IPv6 hosts (any host containing a colon)
// are wrapped in brackets to disambiguate the host:port boundary.
//
// Wildcard hosts ("0.0.0.0" and "::") are omitted entirely so the all-interfaces
// case — the common default — renders as a bare "hostPort→containerPort". Any
// non-wildcard bind (loopback, explicit address) keeps its host so the LAN-vs-
// loopback distinction stays visible at a glance.
//
// Examples:
//
//	{Host: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}   -> "8080→80"
//	{Host: "0.0.0.0", HostPort: 1812, ContainerPort: 1812, Protocol: "udp"} -> "1812→1812/udp"
//	{Host: "::", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}        -> "8080→80"
//	{Host: "127.0.0.1", HostPort: 9000, ContainerPort: 9000}                -> "127.0.0.1:9000→9000"
//	{Host: "::1", HostPort: 8443, ContainerPort: 443, Protocol: "tcp"}      -> "[::1]:8443→443"
func FormatPort(p runner.Port) string {
	var base string
	if isWildcardHost(p.Host) {
		base = fmt.Sprintf("%d→%d", p.HostPort, p.ContainerPort)
	} else {
		host := p.Host
		if isIPv6Host(host) {
			host = "[" + host + "]"
		}
		base = fmt.Sprintf("%s:%d→%d", host, p.HostPort, p.ContainerPort)
	}
	if p.Protocol != "" && p.Protocol != "tcp" {
		base += "/" + p.Protocol
	}
	return base
}

// isWildcardHost reports whether host is the IPv4 or IPv6 unspecified address,
// which Docker uses to mean "publish on all interfaces".
func isWildcardHost(host string) bool {
	return host == "0.0.0.0" || host == "::"
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

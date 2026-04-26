package compose

import (
	"strings"
	"testing"

	"github.com/lexxzar/compose-deploy/internal/runner"
)

func TestFormatPort_TCPOmitsSuffix(t *testing.T) {
	got := FormatPort(runner.Port{Host: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"})
	want := "8080→80"
	if got != want {
		t.Fatalf("FormatPort tcp = %q, want %q", got, want)
	}
}

func TestFormatPort_EmptyProtocolOmitsSuffix(t *testing.T) {
	got := FormatPort(runner.Port{Host: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: ""})
	want := "8080→80"
	if got != want {
		t.Fatalf("FormatPort empty proto = %q, want %q", got, want)
	}
}

func TestFormatPort_UDPSuffix(t *testing.T) {
	got := FormatPort(runner.Port{Host: "0.0.0.0", HostPort: 1812, ContainerPort: 1812, Protocol: "udp"})
	want := "1812→1812/udp"
	if got != want {
		t.Fatalf("FormatPort udp = %q, want %q", got, want)
	}
}

func TestFormatPort_SCTPSuffix(t *testing.T) {
	got := FormatPort(runner.Port{Host: "0.0.0.0", HostPort: 36412, ContainerPort: 36412, Protocol: "sctp"})
	want := "36412→36412/sctp"
	if got != want {
		t.Fatalf("FormatPort sctp = %q, want %q", got, want)
	}
}

func TestFormatPort_IPv4WildcardHostHidden(t *testing.T) {
	got := FormatPort(runner.Port{Host: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"})
	if strings.Contains(got, "0.0.0.0") {
		t.Fatalf("FormatPort wildcard = %q must not contain 0.0.0.0", got)
	}
}

func TestFormatPort_NonWildcardIPv4HostPreserved(t *testing.T) {
	got := FormatPort(runner.Port{Host: "192.168.1.10", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"})
	want := "192.168.1.10:8080→80"
	if got != want {
		t.Fatalf("FormatPort lan bind = %q, want %q", got, want)
	}
}

func TestFormatPort_LocalhostBindPreserved(t *testing.T) {
	got := FormatPort(runner.Port{Host: "127.0.0.1", HostPort: 9000, ContainerPort: 9000, Protocol: "tcp"})
	want := "127.0.0.1:9000→9000"
	if got != want {
		t.Fatalf("FormatPort localhost = %q, want %q", got, want)
	}
}

func TestFormatPort_ArrowIsExactRune(t *testing.T) {
	got := FormatPort(runner.Port{Host: "127.0.0.1", HostPort: 80, ContainerPort: 80, Protocol: "tcp"})
	if !strings.Contains(got, "\u2192") {
		t.Fatalf("FormatPort = %q missing U+2192 arrow", got)
	}
	if strings.Contains(got, "->") {
		t.Fatalf("FormatPort = %q must not contain ASCII '->'", got)
	}
}

func TestFormatPorts_EmptySlice(t *testing.T) {
	if got := FormatPorts(nil); got != "" {
		t.Fatalf("FormatPorts(nil) = %q, want empty", got)
	}
	if got := FormatPorts([]runner.Port{}); got != "" {
		t.Fatalf("FormatPorts(empty) = %q, want empty", got)
	}
}

func TestFormatPorts_MultiPortJoin(t *testing.T) {
	ports := []runner.Port{
		{Host: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"},
		{Host: "0.0.0.0", HostPort: 443, ContainerPort: 443, Protocol: "tcp"},
		{Host: "127.0.0.1", HostPort: 9000, ContainerPort: 9000, Protocol: "tcp"},
	}
	got := FormatPorts(ports)
	want := "80→80, 443→443, 127.0.0.1:9000→9000"
	if got != want {
		t.Fatalf("FormatPorts multi = %q, want %q", got, want)
	}
}

func TestFormatPort_IPv6WildcardHidden(t *testing.T) {
	got := FormatPort(runner.Port{Host: "::", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"})
	want := "8080→80"
	if got != want {
		t.Fatalf("FormatPort ipv6 wildcard = %q, want %q", got, want)
	}
}

func TestFormatPort_IPv6LoopbackBracketed(t *testing.T) {
	got := FormatPort(runner.Port{Host: "::1", HostPort: 8443, ContainerPort: 443, Protocol: "tcp"})
	want := "[::1]:8443→443"
	if got != want {
		t.Fatalf("FormatPort ipv6 loopback = %q, want %q", got, want)
	}
}

func TestFormatPort_IPv6FullAddressBracketed(t *testing.T) {
	got := FormatPort(runner.Port{Host: "2001:db8::1", HostPort: 80, ContainerPort: 80, Protocol: "tcp"})
	want := "[2001:db8::1]:80→80"
	if got != want {
		t.Fatalf("FormatPort ipv6 full = %q, want %q", got, want)
	}
}

func TestFormatPort_IPv6WildcardWithUDPSuffix(t *testing.T) {
	got := FormatPort(runner.Port{Host: "::", HostPort: 1812, ContainerPort: 1812, Protocol: "udp"})
	want := "1812→1812/udp"
	if got != want {
		t.Fatalf("FormatPort ipv6 wildcard udp = %q, want %q", got, want)
	}
}

func TestFormatPorts_MixedProtocols(t *testing.T) {
	ports := []runner.Port{
		{Host: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"},
		{Host: "0.0.0.0", HostPort: 1812, ContainerPort: 1812, Protocol: "udp"},
	}
	got := FormatPorts(ports)
	want := "80→80, 1812→1812/udp"
	if got != want {
		t.Fatalf("FormatPorts mixed = %q, want %q", got, want)
	}
}

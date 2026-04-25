package config

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// SSHTarget represents a parsed ad-hoc SSH connection string in the form
// `[user@]host[:port]`. Port is 0 when not specified.
type SSHTarget struct {
	User string // optional
	Host string // required
	Port int    // 0 if not specified
}

// ParseSSHTarget parses a connection string in the form `[user@]host[:port]`.
// IPv6 addresses are not supported.
func ParseSSHTarget(s string) (SSHTarget, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return SSHTarget{}, fmt.Errorf("ssh target is empty")
	}
	for _, r := range s {
		if unicode.IsSpace(r) {
			return SSHTarget{}, fmt.Errorf("ssh target must not contain whitespace")
		}
	}
	if strings.HasPrefix(s, "[") {
		return SSHTarget{}, fmt.Errorf("IPv6 not supported")
	}

	var t SSHTarget
	rest := s
	if i := strings.Index(rest, "@"); i >= 0 {
		user := rest[:i]
		rest = rest[i+1:]
		if user == "" {
			return SSHTarget{}, fmt.Errorf("user is empty")
		}
		t.User = user
	}

	hostPart := rest
	portPart := ""
	if i := strings.Index(rest, ":"); i >= 0 {
		hostPart = rest[:i]
		portPart = rest[i+1:]
	}

	if hostPart == "" {
		return SSHTarget{}, fmt.Errorf("host is empty")
	}
	t.Host = hostPart

	if portPart != "" {
		port, err := strconv.Atoi(portPart)
		if err != nil {
			return SSHTarget{}, fmt.Errorf("port %q is not a number", portPart)
		}
		if port < 1 || port > 65535 {
			return SSHTarget{}, fmt.Errorf("port %d out of range (1-65535)", port)
		}
		t.Port = port
	}

	return t, nil
}

// SSHHost returns the connection target in `user@host` form (or just `host` if
// no user is set), suitable for use as the destination argument to ssh.
func (t SSHTarget) SSHHost() string {
	if t.User != "" {
		return t.User + "@" + t.Host
	}
	return t.Host
}

// PortArgs returns ssh CLI args to select the port (`-p N`), or nil when no
// port is configured.
func (t SSHTarget) PortArgs() []string {
	if t.Port == 0 {
		return nil
	}
	return []string{"-p", strconv.Itoa(t.Port)}
}

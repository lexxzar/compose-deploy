package config

import (
	"fmt"
	"strconv"
	"strings"
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
//
// Errors are returned with bare descriptive wording (e.g., `host is empty`,
// `port "abc" is not a number`) because callers wrap them with their own
// context (e.g., `invalid --ssh value %q: ...`). The empty-input case keeps
// the `ssh target ...` prefix since it has no other context.
func ParseSSHTarget(s string) (SSHTarget, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return SSHTarget{}, fmt.Errorf("ssh target is empty")
	}
	if strings.ContainsAny(s, " \t\n\r") {
		return SSHTarget{}, fmt.Errorf("must not contain whitespace")
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
		// Reject multiple `@` — `user@host@host` and `user@@host` are not
		// valid SSH user@host syntax. This also catches `user:pass@host`-like
		// inputs that snuck a `@` into the user field accidentally.
		if strings.Contains(rest, "@") {
			return SSHTarget{}, fmt.Errorf("must contain at most one '@'")
		}
		// Reject `:` inside the user part — `user:pass@host` is not supported.
		if strings.Contains(user, ":") {
			return SSHTarget{}, fmt.Errorf("user must not contain ':'")
		}
		// Reject user values starting with `-` to prevent ssh option injection
		// (e.g., `-oProxyCommand=...@host` or `-F/tmp/cfg@host`). Without this
		// guard, the user field would land in ssh's argv as if it were an
		// option flag, allowing arbitrary ssh configuration to be supplied.
		if strings.HasPrefix(user, "-") {
			return SSHTarget{}, fmt.Errorf("user must not start with '-'")
		}
		t.User = user
	}

	// More than one `:` in the host[:port] tail is ambiguous. This catches
	// bare IPv6 (e.g., `::1`, `fe80::1`) as well as malformed inputs like
	// `host:22:30`. Either way the parser cannot disambiguate, so treat them
	// uniformly with a single descriptive error.
	if strings.Count(rest, ":") > 1 {
		return SSHTarget{}, fmt.Errorf("too many ':' separators (IPv6 not supported)")
	}

	hostPart := rest
	portPart := ""
	colonPresent := false
	if i := strings.Index(rest, ":"); i >= 0 {
		colonPresent = true
		hostPart = rest[:i]
		portPart = rest[i+1:]
	}

	if hostPart == "" {
		return SSHTarget{}, fmt.Errorf("host is empty")
	}
	// Reject host values starting with `-` to prevent ssh option injection
	// (e.g., `-Jjump@evil` or `-oProxyCommand=...`). Without this guard, the
	// host field would land in ssh's argv as if it were an option flag,
	// allowing arbitrary ssh configuration to be supplied via the connection
	// string.
	if strings.HasPrefix(hostPart, "-") {
		return SSHTarget{}, fmt.Errorf("host must not start with '-'")
	}
	t.Host = hostPart

	if colonPresent && portPart == "" {
		return SSHTarget{}, fmt.Errorf("port is empty")
	}
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

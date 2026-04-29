package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ParseIdentity validates and resolves a path to an SSH private key file
// supplied via `-i/--identity`. It supports `~/` and bare `~` expansion via
// `os.UserHomeDir()`; the `~user` form is rejected (no `getpwnam` lookup).
//
// Validation: the resolved path must exist, be a regular file, and be readable
// (an immediate `os.Open` confirms ACL/perm readability beyond mode bits).
// Permission/mode checks (e.g. 0600) are intentionally skipped — `ssh(1)` already
// enforces those at connect time.
//
// Errors are returned with bare descriptive wording (e.g., `path is empty`,
// `not a regular file`) because callers wrap them with their own context
// (e.g., `invalid --identity value %q: ...`).
//
// The cleaned path is returned. Relative paths are passed through unchanged
// (matching `ssh -i` behavior, which resolves them against cwd).
func ParseIdentity(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("path is empty")
	}

	// Tilde expansion: `~/foo` and bare `~` are supported. `~user` is not.
	if s == "~" || strings.HasPrefix(s, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve home directory: %w", err)
		}
		if s == "~" {
			s = home
		} else {
			s = filepath.Join(home, s[2:])
		}
	} else if strings.HasPrefix(s, "~") {
		return "", fmt.Errorf("only ~/ is supported (no ~user)")
	}

	s = filepath.Clean(s)

	info, err := os.Stat(s)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("not found: %s", s)
		}
		return "", fmt.Errorf("cannot stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}

	f, err := os.Open(s)
	if err != nil {
		return "", fmt.Errorf("not readable: %w", err)
	}
	_ = f.Close()

	return s, nil
}

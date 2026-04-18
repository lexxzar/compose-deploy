package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// Group represents a named server group with a shared color.
type Group struct {
	Name  string `yaml:"name"`
	Color string `yaml:"color,omitempty"`
}

// Server represents a configured remote server.
type Server struct {
	Name       string `yaml:"name"`
	Host       string `yaml:"host"`
	ProjectDir string `yaml:"project_dir,omitempty"`
	Group      string `yaml:"group,omitempty"`
	Color      string `yaml:"color,omitempty"`
}

// ValidColors lists the allowed server badge color names, in cycle order.
var ValidColors = []string{
	"red", "green", "yellow", "blue", "magenta", "cyan", "white", "gray",
}

// Config holds the cdeploy configuration.
type Config struct {
	Groups  []Group  `yaml:"groups,omitempty"`
	Servers []Server `yaml:"servers"`
}

// GroupColor returns the color for the named group, or "" if not found.
func (c *Config) GroupColor(groupName string) string {
	for _, g := range c.Groups {
		if g.Name == groupName {
			return g.Color
		}
	}
	return ""
}

// DefaultPath returns the default config file path: ~/.cdeploy/servers.yml.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".cdeploy", "servers.yml")
	}
	return filepath.Join(home, ".cdeploy", "servers.yml")
}

// Load reads and parses the config file at path.
// Returns an empty config (not an error) if the file does not exist.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.migrate()

	return &cfg, nil
}

// migrate converts old-format configs (per-server colors on grouped servers)
// to the new format (colors on Group entries). It also auto-creates Group
// entries for any group names referenced by servers but missing from Groups.
// This is idempotent — already-migrated configs are unaffected.
func (c *Config) migrate() {
	// Build lookup of existing groups
	existing := make(map[string]int) // group name → index in c.Groups
	for i, g := range c.Groups {
		existing[g.Name] = i
	}

	// Only auto-create groups for legacy configs (no explicit groups declared).
	// When groups are already declared, unmatched references are likely typos
	// and should be caught by Validate().
	isLegacy := len(c.Groups) == 0

	for i, s := range c.Servers {
		if s.Group == "" {
			continue
		}
		if idx, ok := existing[s.Group]; ok {
			// Group exists; adopt server's color if group has none (first-server-wins)
			if c.Groups[idx].Color == "" && s.Color != "" {
				c.Groups[idx].Color = s.Color
			}
			// Strip color from grouped server
			c.Servers[i].Color = ""
		} else if isLegacy {
			// Auto-create group with server's color (legacy format only)
			c.Groups = append(c.Groups, Group{Name: s.Group, Color: s.Color})
			existing[s.Group] = len(c.Groups) - 1
			c.Servers[i].Color = ""
		}
	}
}

// Validate checks required fields and uniqueness constraints.
func (c *Config) Validate() error {
	// Validate groups
	seenGroup := make(map[string]bool)
	for i, g := range c.Groups {
		if g.Name == "" {
			return fmt.Errorf("group[%d]: name is required", i)
		}
		if g.Color != "" && !slices.Contains(ValidColors, g.Color) {
			return fmt.Errorf("group %q: unknown color %q", g.Name, g.Color)
		}
		if seenGroup[g.Name] {
			return fmt.Errorf("duplicate group name %q", g.Name)
		}
		seenGroup[g.Name] = true
	}

	// Validate servers
	seen := make(map[string]bool)
	for i, s := range c.Servers {
		if s.Name == "" {
			return fmt.Errorf("server[%d]: name is required", i)
		}
		if s.Host == "" {
			return fmt.Errorf("server %q: host is required", s.Name)
		}
		if s.Color != "" && !slices.Contains(ValidColors, s.Color) {
			return fmt.Errorf("server %q: unknown color %q", s.Name, s.Color)
		}
		if s.Group != "" && !seenGroup[s.Group] {
			return fmt.Errorf("server %q: references unknown group %q", s.Name, s.Group)
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate server name %q", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

// Save writes the config to the given path atomically.
// It creates parent directories if needed.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".servers-*.yml")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming config: %w", err)
	}
	return nil
}

// FindServer looks up a server by name.
func (c *Config) FindServer(name string) (*Server, error) {
	for i := range c.Servers {
		if c.Servers[i].Name == name {
			return &c.Servers[i], nil
		}
	}
	return nil, fmt.Errorf("server %q not found in config", name)
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

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
	Servers []Server `yaml:"servers"`
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

	return &cfg, nil
}

// Validate checks required fields and uniqueness constraints.
func (c *Config) Validate() error {
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

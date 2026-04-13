package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Server represents a configured remote server.
type Server struct {
	Name       string `yaml:"name"`
	Host       string `yaml:"host"`
	ProjectDir string `yaml:"project_dir,omitempty"`
	Group      string `yaml:"group,omitempty"`
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
		if seen[s.Name] {
			return fmt.Errorf("duplicate server name %q", s.Name)
		}
		seen[s.Name] = true
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

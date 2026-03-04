// Package config handles loading and saving the gobarrier configuration file.
package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration.
type Config struct {
	Server  ServerConfig            `toml:"server"`
	Screens map[string]ScreenConfig `toml:"screens"`
	Links   []LinkConfig            `toml:"links"`
}

// ServerConfig holds networking options.
type ServerConfig struct {
	// Host to listen on. Default "" means all interfaces.
	Host string `toml:"host"`
	// Port to listen on. Default 24800.
	Port int `toml:"port"`
	// TLS enables TLS encryption. Requires CertFile and KeyFile.
	TLS      bool   `toml:"tls"`
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
	// ScreenName is the name of this machine (the primary/server screen).
	ScreenName string `toml:"screen_name"`
}

// ScreenConfig holds per-screen options.
type ScreenConfig struct {
	// Optional override for the OS hostname.
	Aliases []string `toml:"aliases"`
	// SwitchDelay in milliseconds before switching screens at the edge.
	SwitchDelay int `toml:"switch_delay"`
}

// LinkConfig defines a directional edge between two screens.
// Example: machine "mac" has screen "ubuntu" to its Right.
type LinkConfig struct {
	From      string `toml:"from"`
	Direction string `toml:"direction"` // left | right | top | bottom
	To        string `toml:"to"`
}

// Load reads a TOML config file from the given path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var cfg Config
	if _, err := toml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 24800
	}
	return &cfg, nil
}

// DefaultConfig returns a usable starter configuration.
func DefaultConfig(primaryName string) *Config {
	return &Config{
		Server: ServerConfig{
			Host:       "",
			Port:       24800,
			ScreenName: primaryName,
		},
		Screens: map[string]ScreenConfig{
			primaryName: {},
		},
		Links: []LinkConfig{},
	}
}

// Example returns an example TOML config as a string.
func Example() string {
	return `# gobarrier server configuration
[server]
host        = ""           # listen on all interfaces
port        = 24800
screen_name = "mac"        # this machine's screen name
tls         = false
# cert_file = "server.crt"
# key_file  = "server.key"

# Declare every screen that will connect.
[screens.mac]
[screens.ubuntu]
  switch_delay = 200        # ms before crossing edge
[screens.windows]
  switch_delay = 200

# Define screen adjacency.
# "mac has ubuntu to its right, and windows to its left"
[[links]]
  from      = "mac"
  direction = "right"
  to        = "ubuntu"

[[links]]
  from      = "mac"
  direction = "left"
  to        = "windows"
`
}

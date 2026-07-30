// Package config loads (and writes) ~/.config/plan-usage/config.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath -- the conventional config location.
const DefaultPath = ".config/plan-usage/config.yaml"

// Config is the user-overridable configuration.
type Config struct {
	Providers      map[string]ProviderConfig `yaml:"providers"`
	Enabled        []string                  `yaml:"enabled"`         // optional allowlist
	RefreshEvery   time.Duration             `yaml:"refresh_interval"`
	ProbeMaxTokens int                       `yaml:"probe_max_tokens"`
	Polybar        PolybarConfig             `yaml:"polybar"`
	StateDir       string                    `yaml:"state_dir"`
	Debug          bool                      `yaml:"debug"`
	DryRun         bool                      `yaml:"dry_run"`
}

// ProviderConfig holds per-provider overrides.
type ProviderConfig struct {
	Disabled bool   `yaml:"disabled"`
	APIKey   string `yaml:"api_key"`
	Endpoint string `yaml:"endpoint"`
	Token    string `yaml:"token"`
}

// PolybarConfig controls the polybar-widget formatting.
type PolybarConfig struct {
	Format       string `yaml:"format"`
	Separator    string `yaml:"separator"`
	HideIfNoAuth bool   `yaml:"hide_if_no_auth"`
	NoAuthText   string `yaml:"no_auth_text"`
}

// Defaults applies sane defaults to zero-valued fields.
func (c *Config) Defaults() {
	if c.RefreshEvery == 0 {
		c.RefreshEvery = 60 * time.Second
	}
	if c.RefreshEvery < 5*time.Second {
		c.RefreshEvery = 5 * time.Second // floor to avoid flooding
	}
	if c.ProbeMaxTokens == 0 {
		c.ProbeMaxTokens = 1
	}
	if c.Polybar.Format == "" {
		c.Polybar.Format = "{icon} {name} {percent}%"
	}
	if c.Polybar.Separator == "" {
		c.Polybar.Separator = " · "
	}
	if c.Polybar.NoAuthText == "" {
		c.Polybar.NoAuthText = "—"
	}
	if c.StateDir == "" {
		xdg := os.Getenv("XDG_STATE_HOME")
		if xdg == "" {
			home, _ := os.UserHomeDir()
			xdg = filepath.Join(home, ".local", "state")
		}
		c.StateDir = filepath.Join(xdg, "plan-usage")
	}
}

// IsProviderEnabled reports whether the named provider should be queried.
func (c *Config) IsProviderEnabled(name string) bool {
	if len(c.Enabled) > 0 {
		for _, n := range c.Enabled {
			if n == name {
				return true
			}
		}
		return false
	}
	if pc, ok := c.Providers[name]; ok {
		return !pc.Disabled
	}
	return true // default-on
}

// Override returns a Credential if the user supplied one explicitly,
// or ok=false when we should fall back to the native CLI config.
func (c *Config) Override(name string) (key, endpoint, token string, ok bool) {
	pc, exists := c.Providers[name]
	if !exists {
		return
	}
	if pc.APIKey == "" && pc.Token == "" && pc.Endpoint == "" {
		return
	}
	return pc.APIKey, pc.Endpoint, pc.Token, true
}

// Load reads the config from disk. A missing file is not an error -
// defaults are returned instead.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, DefaultPath)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg.Defaults()
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.Defaults()
	return cfg, nil
}

// Write serialises the config to disk.
func (c *Config) Write(path string) error {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, DefaultPath)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

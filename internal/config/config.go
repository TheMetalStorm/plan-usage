// Package config loads (and writes) ~/.config/plan-usage/config.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath -- the conventional config location.
const DefaultPath = ".config/plan-usage/config.yaml"

// Config is the user-overridable configuration.
type Config struct {
	// ConfigPath is the source file used by Load. It is intentionally not
	// serialised; desktop settings can persist back to the same file.
	ConfigPath     string                    `yaml:"-"`
	Providers      map[string]ProviderConfig `yaml:"providers"`
	Enabled        []string                  `yaml:"enabled"` // optional allowlist
	RefreshEvery   time.Duration             `yaml:"refresh_interval"`
	ProbeMaxTokens int                       `yaml:"probe_max_tokens"`
	StateDir       string                    `yaml:"state_dir"`
	Debug          bool                      `yaml:"debug"`
	DryRun         bool                      `yaml:"dry_run"`

	// mu guards the runtime-mutable fields (Enabled, Providers, scalar
	// settings) against concurrent readers -- daemon refresh goroutines and
	// the TUI/tray toggle handlers -- while a provider visibility toggle
	// rewrites the allowlist. It is unexported so yaml never serialises it.
	mu sync.RWMutex
}

// ProviderConfig holds per-provider overrides.
type ProviderConfig struct {
	Disabled bool   `yaml:"disabled"`
	APIKey   string `yaml:"api_key"`
	Endpoint string `yaml:"endpoint"`
	Token    string `yaml:"token"`
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
	c.mu.RLock()
	defer c.mu.RUnlock()
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

// EnabledSet returns the set of provider names currently enabled, following
// the same semantics as IsProviderEnabled. allNames is the stable provider
// registry order; when the allowlist is empty (default-on) the set is
// materialized from allNames minus any provider with Disabled set.
func (c *Config) EnabledSet(allNames []string) map[string]bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.enabledSetLocked(allNames)
}

// enabledSetLocked implements EnabledSet; the caller must hold at least
// the read lock.
func (c *Config) enabledSetLocked(allNames []string) map[string]bool {
	set := make(map[string]bool, len(allNames))
	if len(c.Enabled) > 0 {
		for _, n := range c.Enabled {
			set[n] = true
		}
		return set
	}
	for _, n := range allNames {
		set[n] = true
	}
	for n, pc := range c.Providers {
		if pc.Disabled {
			delete(set, n)
		}
	}
	return set
}

// SetProviderEnabled updates the enabled allowlist so that name is included
// or excluded, then persists the config. allNames is the stable provider
// registry order; when the allowlist was empty (default-on) it is
// materialized so an exclusion is actually recorded on disk. The write
// happens under the config lock so concurrent refresh readers never observe
// a partially-updated allowlist.
//
// The in-memory allowlist is only committed once the disk write succeeds.
// If the write fails, c.Enabled is rolled back to its previous value so the
// running process never shows a provider as toggled when the change was not
// actually persisted -- otherwise the UI would reflect the new selection
// during the session while the next restart silently reverted it.
func (c *Config) SetProviderEnabled(allNames []string, name string, enabled bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	set := c.enabledSetLocked(allNames)
	if enabled {
		set[name] = true
	} else {
		delete(set, name)
	}
	next := orderedEnabled(allNames, c.Enabled, set)
	path := c.ConfigPath
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, DefaultPath)
	}
	prev := c.Enabled
	c.Enabled = next
	if err := c.writePathLocked(path); err != nil {
		// Roll back the in-memory allowlist so it stays in sync with the
		// (unchanged) on-disk config; a failed write must not look applied.
		c.Enabled = prev
		return err
	}
	c.ConfigPath = path
	return nil
}

// orderedEnabled returns the enabled names in registry order, followed by
// any pre-existing allowlist entries that are still enabled but not part of
// the registry (kept so a toggle never silently drops user data).
func orderedEnabled(allNames, previous []string, set map[string]bool) []string {
	out := make([]string, 0, len(set))
	seen := make(map[string]bool, len(set))
	for _, n := range allNames {
		if set[n] && !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	for _, n := range previous {
		if set[n] && !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	return out
}

// ApplyFresh merges runtime-mutable settings from a freshly loaded config
// into c under the config lock. Long-running processes (the tray) use it to
// pick up provider-visibility changes made by another process (e.g. the TUI)
// without restarting.
func (c *Config) ApplyFresh(fresh *Config) {
	if fresh == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Enabled = append([]string(nil), fresh.Enabled...)
	if fresh.RefreshEvery >= 5*time.Second {
		c.RefreshEvery = fresh.RefreshEvery
	}
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
	cfg.ConfigPath = path
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, DefaultPath)
	}
	return c.writePathLocked(path)
}

// writePathLocked serialises the config to path; the caller must hold mu.
func (c *Config) writePathLocked(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

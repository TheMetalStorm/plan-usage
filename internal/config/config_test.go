package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsUseOneMinuteRefreshInterval(t *testing.T) {
	cfg := &Config{}
	cfg.Defaults()
	if cfg.RefreshEvery != 60*time.Second {
		t.Fatalf("RefreshEvery = %s, want 60s", cfg.RefreshEvery)
	}
}

func TestDefaultsClampRefreshIntervalFloor(t *testing.T) {
	cfg := &Config{RefreshEvery: time.Second}
	cfg.Defaults()
	if cfg.RefreshEvery != 5*time.Second {
		t.Fatalf("RefreshEvery = %s, want 5s floor", cfg.RefreshEvery)
	}
}

func TestLoadTracksConfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("refresh_interval: 7m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigPath != path {
		t.Fatalf("ConfigPath = %q, want %q", cfg.ConfigPath, path)
	}
	if cfg.RefreshEvery != 7*time.Minute {
		t.Fatalf("RefreshEvery = %s, want 7m", cfg.RefreshEvery)
	}
}

func TestWriteRoundTripsRefreshInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{RefreshEvery: 11 * time.Minute, StateDir: "/tmp/plan-usage-test"}
	cfg.Defaults()
	if err := cfg.Write(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RefreshEvery != 11*time.Minute {
		t.Fatalf("round-tripped RefreshEvery = %s, want 11m", loaded.RefreshEvery)
	}
}

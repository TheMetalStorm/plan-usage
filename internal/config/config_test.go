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

var testAllNames = []string{"opencodego", "codex", "clinepass", "commandcode", "freebuff"}

func TestSetProviderEnabledDisablesMaterializesAllowlist(t *testing.T) {
	cfg := &Config{ConfigPath: filepath.Join(t.TempDir(), "config.yaml")}
	if err := cfg.SetProviderEnabled(testAllNames, "codex", false); err != nil {
		t.Fatal(err)
	}
	want := []string{"opencodego", "clinepass", "commandcode", "freebuff"}
	if len(cfg.Enabled) != len(want) {
		t.Fatalf("Enabled = %v, want %v", cfg.Enabled, want)
	}
	for i := range want {
		if cfg.Enabled[i] != want[i] {
			t.Fatalf("Enabled = %v, want %v", cfg.Enabled, want)
		}
	}
	if cfg.IsProviderEnabled("codex") {
		t.Fatal("codex must be disabled after SetProviderEnabled(codex, false)")
	}
	if !cfg.IsProviderEnabled("freebuff") {
		t.Fatal("freebuff must stay enabled by default")
	}
}

func TestSetProviderEnabledRemovesFromExistingAllowlist(t *testing.T) {
	cfg := &Config{
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		Enabled:    append([]string(nil), testAllNames...),
	}
	if err := cfg.SetProviderEnabled(testAllNames, "freebuff", false); err != nil {
		t.Fatal(err)
	}
	if cfg.IsProviderEnabled("freebuff") {
		t.Fatal("freebuff must be removed from the allowlist")
	}
	if len(cfg.Enabled) != len(testAllNames)-1 {
		t.Fatalf("Enabled = %v, want all but freebuff", cfg.Enabled)
	}
	for _, n := range []string{"opencodego", "codex", "clinepass", "commandcode"} {
		if !cfg.IsProviderEnabled(n) {
			t.Fatalf("%s must stay enabled", n)
		}
	}
}

func TestSetProviderEnabledAddsToAllowlist(t *testing.T) {
	cfg := &Config{
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		Enabled:    []string{"codex"},
	}
	if err := cfg.SetProviderEnabled(testAllNames, "freebuff", true); err != nil {
		t.Fatal(err)
	}
	if !cfg.IsProviderEnabled("freebuff") || !cfg.IsProviderEnabled("codex") {
		t.Fatalf("Enabled = %v, want codex and freebuff", cfg.Enabled)
	}
	// Registry order preserved: codex precedes freebuff.
	if len(cfg.Enabled) != 2 || cfg.Enabled[0] != "codex" || cfg.Enabled[1] != "freebuff" {
		t.Fatalf("Enabled = %v, want [codex freebuff]", cfg.Enabled)
	}
}

func TestSetProviderEnabledPersistsToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{ConfigPath: path}
	if err := cfg.SetProviderEnabled(testAllNames, "commandcode", false); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.IsProviderEnabled("commandcode") {
		t.Fatal("reloaded config must still have commandcode disabled")
	}
	if !loaded.IsProviderEnabled("opencodego") {
		t.Fatal("reloaded config must keep the other providers enabled")
	}
}

func TestSetProviderEnabledHonorsDisabledProviderFlags(t *testing.T) {
	cfg := &Config{
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		Providers:  map[string]ProviderConfig{"codex": {Disabled: true}},
	}
	if cfg.IsProviderEnabled("codex") {
		t.Fatal("codex starts disabled via the provider flag")
	}
	if err := cfg.SetProviderEnabled(testAllNames, "codex", true); err != nil {
		t.Fatal(err)
	}
	if !cfg.IsProviderEnabled("codex") {
		t.Fatal("explicitly enabling codex must win over the flag")
	}
	if len(cfg.Enabled) != len(testAllNames) || cfg.Enabled[1] != "codex" {
		t.Fatalf("Enabled = %v, want the full allowlist in registry order", cfg.Enabled)
	}
}

func TestEnabledSetMatchesIsProviderEnabled(t *testing.T) {
	cases := []*Config{
		{Enabled: []string{"codex", "freebuff"}},
		{Enabled: []string{"opencodego", "codex", "clinepass", "commandcode", "freebuff"}},
		{Providers: map[string]ProviderConfig{"codex": {Disabled: true}}},
		{},
	}
	for _, cfg := range cases {
		set := cfg.EnabledSet(testAllNames)
		for _, n := range testAllNames {
			if set[n] != cfg.IsProviderEnabled(n) {
				t.Fatalf("EnabledSet()[%s] = %v, IsProviderEnabled = %v (cfg %+v)", n, set[n], cfg.IsProviderEnabled(n), cfg)
			}
		}
	}
}

func TestApplyFreshCopiesEnabledAndRefreshInterval(t *testing.T) {
	cfg := &Config{Enabled: []string{"codex"}, RefreshEvery: time.Minute}
	fresh := &Config{Enabled: []string{"opencodego", "freebuff"}, RefreshEvery: 7 * time.Minute}
	cfg.ApplyFresh(fresh)
	if len(cfg.Enabled) != 2 || cfg.Enabled[0] != "opencodego" || cfg.Enabled[1] != "freebuff" {
		t.Fatalf("Enabled = %v, want the fresh allowlist", cfg.Enabled)
	}
	if cfg.RefreshEvery != 7*time.Minute {
		t.Fatalf("RefreshEvery = %s, want 7m", cfg.RefreshEvery)
	}
	// Mutating the source after ApplyFresh must not alias into cfg.
	fresh.Enabled[0] = "mutated"
	if cfg.Enabled[0] == "mutated" {
		t.Fatal("ApplyFresh must defensively copy the allowlist")
	}
}

func TestApplyFreshFloorsRefreshInterval(t *testing.T) {
	cfg := &Config{RefreshEvery: time.Minute}
	cfg.ApplyFresh(&Config{RefreshEvery: time.Second})
	if cfg.RefreshEvery != time.Minute {
		t.Fatalf("RefreshEvery = %s, want the old 1m (sub-5s fresh values ignored)", cfg.RefreshEvery)
	}
}

// TestSetProviderEnabledRevertsInMemoryOnWriteFailure locks in the fix for
// the "toggle works during the session but is lost on restart" bug: when the
// disk write fails, the in-memory allowlist must roll back so the running
// process never advertises a selection that was not persisted. The config
// path's parent is a regular file, so MkdirAll (and therefore the write)
// cannot succeed.
func TestSetProviderEnabledRevertsInMemoryOnWriteFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{ConfigPath: filepath.Join(blocker, "config.yaml")}
	before := cfg.IsProviderEnabled("codex")
	if err := cfg.SetProviderEnabled(testAllNames, "codex", false); err == nil {
		t.Fatal("SetProviderEnabled must fail when the config path is not writable")
	}
	if cfg.IsProviderEnabled("codex") != before {
		t.Fatalf("in-memory allowlist changed after a failed write: codex enabled = %v, want %v",
			cfg.IsProviderEnabled("codex"), before)
	}
}

// TestSetProviderEnabledSurvivesReloadSequence simulates the full
// tray/TUI lifecycle -- several toggles followed by a process restart that
// reloads the config from the same path -- and asserts every selection
// survives. This is the regression test for the persistence bug where
// selections made during a session were lost on the next program start.
func TestSetProviderEnabledSurvivesReloadSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := Load(path) // no file yet -> defaults, default-on
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProviderEnabled(testAllNames, "codex", false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProviderEnabled(testAllNames, "freebuff", false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetProviderEnabled(testAllNames, "clinepass", true); err != nil {
		t.Fatal(err) // no-op toggle of an already-enabled provider
	}

	// Simulate closing and reopening the program: reload from disk.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	wantEnabled := map[string]bool{
		"opencodego":  true,
		"codex":       false,
		"clinepass":   true,
		"commandcode": true,
		"freebuff":    false,
	}
	for _, name := range testAllNames {
		if got := reloaded.IsProviderEnabled(name); got != wantEnabled[name] {
			t.Fatalf("after restart %s enabled = %v, want %v (Enabled = %v)",
				name, got, wantEnabled[name], reloaded.Enabled)
		}
	}
}

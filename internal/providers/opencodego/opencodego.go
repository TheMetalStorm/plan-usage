// Package opencodego implements the types.Provider interface for the
// OpenCode Go subscription plan. The Go plan has three dollar-bounded
// windows:
//
//	$12.00 / 5h  rolling
//	$30.00 / 1 week (Sun boundary, local TZ)
//	$60.00 / 1 month (1st-of-month, local TZ)
//
// It also satisfies providers.MultiWindowProvider so the TUI can show
// three progress bars side by side.
//
// OpenCode currently exposes NO public billing API endpoint, so we
// surface the static plan limits and a plain-English note telling the
// user where to consult the live numbers (opencode.ai/auth).  When
// OpenCode ships a billing endpoint, only this file needs to change.
package opencodego

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/simon/usage/internal/auth"
	"github.com/simon/usage/internal/config"
	"github.com/simon/usage/internal/probe"
	"github.com/simon/usage/internal/types"
)

// Provider implements types.Provider for OpenCode Go.
type Provider struct {
	a     *auth.Finder
	probe *probe.Client
	cfg   *config.Config
}

// New returns a Provider with probe initialized.
func New() *Provider { return &Provider{probe: probe.New()} }

// NewWith builds the provider with explicit deps.
func NewWith(a *auth.Finder, c *config.Config) *Provider {
	return &Provider{a: a, probe: probe.New(), cfg: c}
}

func (p *Provider) Name() string        { return "opencodego" }
func (p *Provider) DisplayName() string { return "OpenCode Go" }
func (p *Provider) Icon() string        { return "" }

// The Go plan uses the same model catalog as Zen (free models list);
// repeated here so users can see what the dollar caps gate against.
var opencodegoFreeModels = []types.FreeModel{
	{ID: "big-pickle", Label: "Big Pickle", Notes: "stealth model"},
	{ID: "deepseek-v4-flash-free", Label: "DeepSeek V4 Flash Free"},
	{ID: "mimo-v2.5-free", Label: "MiMo-V2.5 Free"},
	{ID: "laguna-s-2.1-free", Label: "Laguna S 2.1 Free"},
	{ID: "ling-3.0-flash-free", Label: "Ling-3.0-flash Free"},
	{ID: "north-mini-code-free", Label: "North Mini Code Free"},
	{ID: "nemotron-3-ultra-free", Label: "Nemotron 3 Ultra Free"},
}

func (p *Provider) AvailableModels() []types.FreeModel { return opencodegoFreeModels }

// IsConfigured: auth.json has an `opencode-go` entry with a key.
func (p *Provider) IsConfigured() error {
	a := p.a
	if a == nil {
		var err error
		a, err = auth.NewFinder()
		if err != nil {
			return err
		}
		p.a = a
	}
	if p.cfg != nil {
		if k, _, _, ok := p.cfg.Override("opencodego"); ok && k != "" {
			return nil
		}
	}
	tokens, src, err := a.OpenCodeAuth()
	if err != nil {
		return fmt.Errorf("opencodego: no auth.json: %w (looked at %s)", err, src)
	}
	raw, ok := tokens["opencode-go"]
	if !ok || len(raw) == 0 {
		return errors.New("opencodego: auth.json has no `opencode-go` entry; see opencode.ai/docs/go to subscribe")
	}
	var entry struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Key == "" {
		return errors.New("opencodego: `opencode-go` entry is missing the api key field")
	}
	return nil
}

// defaultUsage is the canonical Go plan scaffold returned when no live
// data is available.  These are the published limits as of opencode.ai/docs/go.
func defaultPrimary() types.UsageStats {
	return types.UsageStats{
		Used:         0,
		Total:        12,
		Unit:         types.UnitUSD,
		WindowLabel:  "5h",
		Note:         "static — check opencode.ai/auth for live numbers",
		LastProbeAt: time.Now(),
	}
}

// FetchUsage returns the primary window (5h) with a static default;
// the snap.Windows is populated separately by SnapshotWindows().
func (p *Provider) FetchUsage(ctx context.Context) (*types.UsageStats, error) {
	a := p.a
	if a == nil {
		var err error
		a, err = auth.NewFinder()
		if err != nil {
			return nil, err
		}
		p.a = a
	}
	tokens, _, err := a.OpenCodeAuth()
	if err != nil {
		stats := defaultPrimary()
		stats.Error = err.Error()
		return &stats, nil
	}
	raw, ok := tokens["opencode-go"]
	if !ok {
		stats := defaultPrimary()
		stats.Error = "no `opencode-go` entry in auth.json"
		return &stats, nil
	}
	var entry struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	_ = json.Unmarshal(raw, &entry)

	stats := defaultPrimary()
	if entry.Key == "" {
		stats.Error = "opencode-go token missing the `key` field"
		return &stats, nil
	}

	// Best-effort: try OpenCode's billing endpoint. Today it returns 404
	// (no public billing API yet); surface real network errors but stay
	// quiet on the expected-404 so the UI note stays user-friendly.
	req, _ := probe.NewPOST(ctx, "https://opencode.ai/api/v1/go/usage", map[string]string{
		"Authorization": "Bearer " + entry.Key,
	}, probe.NewChatBody("opencode-big-pickle", 1))
	res, perr := p.probe.Do(ctx, req)
	switch {
	case perr != nil && res == nil:
		stats.Note = fmt.Sprintf("probe failed: %s", perr.Error())
	case res != nil && res.Status == 404:
		// expected — see opencode.ai/auth for live numbers
	case res != nil && res.Status >= 400:
		stats.Note = fmt.Sprintf("static (HTTP %d from opencode.ai/api/v1/go/usage)", res.Status)
	}
	return &stats, nil
}

// SnapshotWindows returns the three Go-plan windows. The first entry is
// the "primary" window (5h) that the TUI's compact view summarizes.
// All windows default to used=0 because OpenCode does not expose a
// billing endpoint as of writing.
func (p *Provider) SnapshotWindows() []types.UsageStats {
	return []types.UsageStats{
		{Used: 0, Total: 12, Unit: types.UnitUSD, WindowLabel: "5h", Note: "rolling"},
		{Used: 0, Total: 30, Unit: types.UnitUSD, WindowLabel: "weekly", Note: "Sun reset"},
		{Used: 0, Total: 60, Unit: types.UnitUSD, WindowLabel: "monthly", Note: "1st reset"},
	}
}

// Compile-time check that Provider exposes the SnapshotWindows method.
// The providers.MultiWindowProvider interface itself lives in the
// parent package and can't be imported here without creating a cycle,
// so we verify the method signature structurally.
var _ interface {
	SnapshotWindows() []types.UsageStats
} = (*Provider)(nil)

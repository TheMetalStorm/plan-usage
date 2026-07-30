// Package opencode implements the types.Provider interface for
// OpenCode Zen (free + pay-as-you-go).
//
// Strategy: hit the opencode.ai /_server RPC endpoint using the API key
// from ~/.local/share/opencode/auth.json (or a config override). The
// endpoint returns workspace usage — a rolling 5-hour window and an
// optional weekly window.
//
// The CODEXBAR_OPENCODE_WORKSPACE_ID env var can force a specific
// workspace (raw wrk_… ID or full opencode.ai/workspace/… URL).
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/TheMetalStorm/provider-usage/internal/auth"
	"github.com/TheMetalStorm/provider-usage/internal/config"
	"github.com/TheMetalStorm/provider-usage/internal/opencodeutil"
	"github.com/TheMetalStorm/provider-usage/internal/types"
)

// Provider implements types.Provider for OpenCode Zen.
type Provider struct {
	a       *auth.Finder
	cfg     *config.Config
	apiKey  string // resolved Bearer token
}

// New returns a fully-initialized provider.
func New() *Provider { return &Provider{} }

// NewWith builds the provider with explicit deps (used by the registry).
func NewWith(a *auth.Finder, c *config.Config) *Provider {
	return &Provider{a: a, cfg: c}
}

// Name returns the canonical short identifier.
func (p *Provider) Name() string { return "opencode" }

// DisplayName returns a human-friendly label.
func (p *Provider) DisplayName() string { return "OpenCode Zen" }

// Icon: circle (matches Provider iface).
func (p *Provider) Icon() string { return "" }

// opencodeFreeModels is the static list of free-tier models as documented
// at opencode.ai/docs/zen.
var opencodeFreeModels = []types.FreeModel{
	{ID: "big-pickle", Label: "Big Pickle", Notes: "stealth model"},
	{ID: "deepseek-v4-flash-free", Label: "DeepSeek V4 Flash Free"},
	{ID: "mimo-v2.5-free", Label: "MiMo-V2.5 Free"},
	{ID: "laguna-s-2.1-free", Label: "Laguna S 2.1 Free"},
	{ID: "ling-3.0-flash-free", Label: "Ling-3.0-flash Free"},
	{ID: "north-mini-code-free", Label: "North Mini Code Free"},
	{ID: "nemotron-3-ultra-free", Label: "Nemotron 3 Ultra Free"},
	{ID: "zen-credits", Label: "Zen credits (auto-reload)", Notes: "Pay-as-you-go balance; auto-reload at $5 → $20. See opencode.ai/zen"},
}

// AvailableModels returns the static list above.
func (p *Provider) AvailableModels() []types.FreeModel { return opencodeFreeModels }

// IsConfigured returns nil iff a credential is available.
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
		if k, _, _, ok := p.cfg.Override("opencode"); ok && k != "" {
			p.apiKey = k
			return nil
		}
	}
	tokens, src, err := a.OpenCodeAuth()
	if err != nil {
		return fmt.Errorf("opencode: no auth.json: %w (looked at %s)", err, src)
	}
	// Extract first API key.
	for _, raw := range tokens {
		var entry struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		}
		if json.Unmarshal(raw, &entry) == nil && entry.Key != "" {
			p.apiKey = entry.Key
			return nil
		}
	}
	return fmt.Errorf("opencode: no API key found in auth.json (looked at %s)", src)
}

// FetchUsage probes the opencode.ai /_server endpoint for rolling usage.
func (p *Provider) FetchUsage(ctx context.Context) (*types.UsageStats, error) {
	// Ensure auth is resolved.
	if p.apiKey == "" {
		_ = p.IsConfigured()
	}

	if p.apiKey == "" {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       "no API key configured",
			Note:        "Provide an OpenCode API key in auth.json or config",
		}, nil
	}

	// Build the server client.
	client := opencodeutil.NewServerClient(p.apiKey)

	// Attach cookie cache if available (for session-based auth fallback).
	if cc, err := opencodeutil.NewCookieCache(); err == nil {
		client.SetCookieCache(cc)
	}

	// Resolve workspace ID from env override.
	workspaceID := opencodeutil.ResolveWorkspaceID(p.cfgWorkspaceOverride())

	// Fetch usage.
	usage, err := client.FetchUsage(ctx, workspaceID)
	if err != nil {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       fmt.Sprintf("server: %v", err),
			Note:        "Could not reach opencode.ai/_server; check network or auth",
		}, nil
	}

	// Build primary (rolling 5h) window.
	resetIn := time.Duration(usage.RollingReset) * time.Second
	stats := &types.UsageStats{
		Used:         usage.RollingPercent,
		Total:        100,
		Unit:         types.UnitCount,
		WindowLabel:  "5h rolling",
		ResetIn:      resetIn,
		ResetAt:      time.Now().Add(resetIn),
		LastProbeAt:  time.Now(),
		Note:         "rolling usage from opencode.ai",
	}

	// Add weekly info if available.
	if usage.HasWeekly {
		stats.Note = fmt.Sprintf("rolling: %.1f%%, weekly: %.1f%% — opencode.ai",
			usage.RollingPercent, usage.WeeklyPercent)
	}

	return stats, nil
}

// cfgWorkspaceOverride checks the user's config for a workspace override.
func (p *Provider) cfgWorkspaceOverride() string {
	if p.cfg == nil {
		return ""
	}
	if pc, ok := p.cfg.Providers["opencode"]; ok {
		if pc.Endpoint != "" {
			return pc.Endpoint
		}
	}
	return ""
}

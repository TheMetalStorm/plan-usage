// Package opencode implements the types.Provider interface for
// OpenCode Zen (free + pay-as-you-go).
//
// Strategy (tried in order):
//  1. Running local `opencode serve` server (http://127.0.0.1:4096) —
//     sum today's tokens from /session.
//  2. opencode.ai /_server RPC endpoint — rolling 5-hour and weekly
//     usage percentages (requires a session cookie).
//  3. Clear error message explaining what's needed.
//
// The CODEXBAR_OPENCODE_WORKSPACE_ID env var can force a specific
// workspace for the _server path.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/TheMetalStorm/provider-usage/internal/auth"
	"github.com/TheMetalStorm/provider-usage/internal/config"
	"github.com/TheMetalStorm/provider-usage/internal/opencodeutil"
	"github.com/TheMetalStorm/provider-usage/internal/types"
)

// Provider implements types.Provider for OpenCode Zen.
type Provider struct {
	a     *auth.Finder
	cfg   *config.Config
	apiKey string // from auth.json
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

// IsConfigured returns nil iff we have any hope of getting data.
// Zen can work with a running local server, a session cookie, or even
// just an API key (for the OpenAI proxy probe fallback).
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
	// Always try to resolve API key — used for local proxy probe.
	if p.cfg != nil {
		if k, _, _, ok := p.cfg.Override("opencode"); ok && k != "" {
			p.apiKey = k
			return nil
		}
	}
	tokens, _, err := a.OpenCodeAuth()
	if err == nil {
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
	}
	// No API key — still might have a local server or a cookie.
	return nil // optimistic; FetchUsage will report specific gaps
}

// FetchUsage probes data sources in priority order.
func (p *Provider) FetchUsage(ctx context.Context) (*types.UsageStats, error) {
	// 1. Try local opencode serve server (fastest, most detailed).
	if stats := p.tryLocalServer(ctx); stats != nil {
		return stats, nil
	}

	// 2. Try _server endpoint with cookie.
	if stats := p.tryServerEndpoint(ctx); stats != nil {
		return stats, nil
	}

	// 3. Nothing worked — explain what the user can do.
	return p.makeFallbackStats(), nil
}

// -- Local server (opencode serve) --

type serverStatus struct {
	OK bool `json:"ok"`
}

func (p *Provider) tryLocalServer(ctx context.Context) *types.UsageStats {
	c := &http.Client{Timeout: 2 * time.Second}

	// Health check.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:4096/global/health", nil)
	if err != nil {
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var s serverStatus
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || json.Unmarshal(body, &s) != nil || !s.OK {
		return nil
	}

	// Fetch session data.
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:4096/session?limit=20", nil)
	if err != nil {
		return nil
	}
	resp, err = c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       fmt.Sprintf("local server: HTTP %d", resp.StatusCode),
			Note:        "opencode serve returned an error",
		}
	}

	type summarySession struct {
		CreatedAt int64 `json:"createdAt"`
		Tokens    struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
		} `json:"tokens"`
		Cost float64 `json:"cost,omitempty"`
	}
	var sessions []summarySession
	if err := json.Unmarshal(body, &sessions); err != nil {
		return nil
	}

	today := time.Now().Truncate(24 * time.Hour)
	var used int64
	var cost float64
	for _, s := range sessions {
		created := time.UnixMilli(s.CreatedAt)
		if created.Before(today) {
			continue
		}
		used += int64(s.Tokens.Input + s.Tokens.Output + s.Tokens.Reasoning)
		cost += s.Cost
	}

	stats := &types.UsageStats{
		Used:        float64(used),
		Total:       0, // Zen doesn't publish a hard cap
		Unit:        types.UnitTokens,
		WindowLabel: "today",
		Note:        "local opencode serve — today's tokens",
		LastProbeAt: time.Now(),
	}
	if cost > 0 {
		stats.Note = fmt.Sprintf("local server — today: $%.2f, %d tokens", cost, used)
	}
	return stats
}

// -- _server endpoint (needs cookie) --

func (p *Provider) tryServerEndpoint(ctx context.Context) *types.UsageStats {
	// Check if we have a cookie.
	cc, err := opencodeutil.NewCookieCache()
	if err != nil || cc.Cookie() == "" {
		return nil
	}

	client := opencodeutil.NewServerClient("") // no Bearer token
	client.SetCookieCache(cc)

	workspaceID := opencodeutil.ResolveWorkspaceID(p.cfgWorkspaceOverride())

	usage, err := client.FetchUsage(ctx, workspaceID)
	if err != nil {
		return nil // skip silently; other fallbacks exist
	}

	resetIn := time.Duration(usage.RollingReset) * time.Second
	stats := &types.UsageStats{
		Used:        usage.RollingPercent,
		Total:       100,
		Unit:        types.UnitCount,
		WindowLabel: "5h rolling",
		ResetIn:     resetIn,
		ResetAt:     time.Now().Add(resetIn),
		LastProbeAt: time.Now(),
		Note:        "rolling usage from opencode.ai",
	}
	if usage.HasWeekly {
		stats.Note = fmt.Sprintf("rolling: %.1f%%, weekly: %.1f%%", usage.RollingPercent, usage.WeeklyPercent)
	}
	return stats
}

// -- Config helpers --

func (p *Provider) cfgWorkspaceOverride() string {
	if p.cfg == nil {
		return ""
	}
	if pc, ok := p.cfg.Providers["opencode"]; ok {
		return pc.Endpoint
	}
	return ""
}

func (p *Provider) makeFallbackStats() *types.UsageStats {
	msg := "Run `opencode` to start the local server. "
	msg += "Or log into opencode.ai in your browser and save the session cookie:\n"
	msg += "  echo '{\"cookie\":\"your_session_cookie\",\"source\":\"manual\"}' > \\\n"
	msg += "    ~/.local/share/provider-usage/opencode-cookies.json"
	return &types.UsageStats{
		LastProbeAt: time.Now(),
		Error:       "no data source available",
		Note:        msg,
	}
}

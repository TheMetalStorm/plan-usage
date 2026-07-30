// Package opencode implements the types.Provider interface for OpenCode Zen.
//
// Strategy: the opencode CLI runs a local HTTP server
// ("opencode serve", default http://127.0.0.1:4096) with an OpenAPI 3.1
// spec at /doc. We use that server when available; otherwise we fall back
// to a tiny probe-request in OpenAI-compat /anthropic modes with the token
// read from ~/.local/share/opencode/auth.json.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/TheMetalStorm/provider-usage/internal/auth"
	"github.com/TheMetalStorm/provider-usage/internal/config"
	"github.com/TheMetalStorm/provider-usage/internal/probe"
	"github.com/TheMetalStorm/provider-usage/internal/types"
)

// Provider implements types.Provider for OpenCode.
type Provider struct {
	a     *auth.Finder
	probe *probe.Client
	cfg   *config.Config
}

// New returns a fully-initialized provider with a probe client.
func New() *Provider { return &Provider{probe: probe.New()} }

// NewWith builds the provider with explicit deps (used by the registry).
func NewWith(a *auth.Finder, c *config.Config) *Provider {
	return &Provider{a: a, probe: probe.New(), cfg: c}
}

// Name returns the canonical short identifier.
func (p *Provider) Name() string { return "opencode" }

// DisplayName returns a human-friendly label.
func (p *Provider) DisplayName() string { return "OpenCode Zen" }

// Icon: circle (matches Provider iface).
func (p *Provider) Icon() string { return "" }

// opencodeFreeModels is the static list of free-tier models as documented
// at opencode.ai/docs/zen (kept here so the UI can show them even if the
// probe times out).
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
			return nil
		}
	}
	_, src, err := a.OpenCodeAuth()
	if err != nil {
		return fmt.Errorf("opencode: no auth.json: %w (looked at %s)", err, src)
	}
	return nil
}

// serverStatus pings the local server's /global/health.
type serverStatus struct {
	OK bool `json:"ok"`
}

// FetchUsage:
//  1. Try the running local server (if available).
//  2. Fall back to a tiny probe via the opencode SDK proxy.
//
// We design the probe as: an OpenAI-compatible chat-completions request
// against https://opencode.ai/api/v1/chat/completions with a no-op prompt
// and max_tokens=1. The server returns the same x-ratelimit-* headers as
// OpenAI's API, which we surface to the UI.
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
	tokens, src, err := a.OpenCodeAuth()
	_ = tokens // raw map - key names vary by provider
	_ = src
	if err != nil {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       err.Error(),
			Note:        "Auth not found - provide an OpenCode API key in config",
		}, nil
	}

	// Fallback: probe via opencode.ai public endpoint with the first
	// available token. Since the auth.json is provider-scoped, we
	// extract one entry that looks like {"type":"api","key":"..."}.
	tok := ""
	for _, raw := range tokens {
		var entry struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		}
		if json.Unmarshal(raw, &entry) == nil && entry.Key != "" {
			tok = entry.Key
			break
		}
	}
	if tok == "" {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       "no api key found in auth.json",
		}, nil
	}

	// 1. Local opencode server is preferred when up — cheaper than
	//    the off-host probe and avoids burning remote quotas.
	if p.tryLocalServer(ctx) {
		return p.fetchLocalServer(ctx)
	}

	// 2. Fall back to a tiny off-host probe.
	body := probe.NewChatBody("opencode/big-pickle", 1)
	req, err := probe.NewPOST(ctx, "https://opencode.ai/api/v1/chat/completions", map[string]string{
		"Authorization": "Bearer " + tok,
	}, body)
	if err != nil {
		return nil, err
	}
	res, err := p.probe.Do(ctx, req)
	if err != nil {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       err.Error(),
		}, nil
	}
	if res.Status >= 400 {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       fmt.Sprintf("HTTP %d: %s", res.Status, snippet(res.Body)),
		}, nil
	}
	stats := &types.UsageStats{
		LastProbeAt: time.Now(),
	}
	applyRateLimit(stats, res.RateLimit)
	return stats, nil
}

// tryLocalServer checks if the opencode CLI is running locally.
// It does not require an auth token because the local server is bound
// to the loopback interface.
func (p *Provider) tryLocalServer(ctx context.Context) bool {
	c := &http.Client{Timeout: 1 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:4096/global/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var s serverStatus
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 200 && (json.Unmarshal(body, &s) == nil && s.OK) {
		return true
	}
	return false
}

// fetchLocalServer reads /session and aggregates token usage.
func (p *Provider) fetchLocalServer(ctx context.Context) (*types.UsageStats, error) {
	c := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:4096/session?limit=20", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       fmt.Sprintf("local server: HTTP %d: %s", resp.StatusCode, snippet(body)),
		}, nil
	}
	// Sum today's tokens.
	type summarySession struct {
		CreatedAt int64 `json:"createdAt"`
		Tokens    struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
		} `json:"tokens"`
	}
	var sessions []summarySession
	if err := json.Unmarshal(body, &sessions); err != nil {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       "local server: failed to parse /session",
		}, nil
	}
	today := time.Now().Truncate(24 * time.Hour)
	var used int64
	for _, s := range sessions {
		created := time.UnixMilli(s.CreatedAt)
		if created.Before(today) {
			continue
		}
		used += int64(s.Tokens.Input + s.Tokens.Output + s.Tokens.Reasoning)
	}
	return &types.UsageStats{
		Used:         float64(used),
		Total:        0, // opencode zen free doesn't publish a hard number
		Unit:         types.UnitTokens,
		WindowLabel:  "today",
		Note:         "opencode CLI local server summary",
		LastProbeAt: time.Now(),
	}, nil
}

func applyRateLimit(s *types.UsageStats, rl probe.RateLimit) {
	if !rl.HasData {
		s.Note = "no x-ratelimit-* headers in response"
		return
	}
	if rl.ReqLimit > 0 {
		s.Total = float64(rl.ReqLimit)
		s.Used = float64(rl.ReqLimit - rl.ReqRemain)
		s.Unit = types.UnitCount
		s.WindowLabel = "requests"
	} else if rl.TokLimit > 0 {
		s.Total = float64(rl.TokLimit)
		s.Used = float64(rl.TokLimit - rl.TokRemain)
		s.Unit = types.UnitTokens
		s.WindowLabel = "tokens"
	}
	if d, lbl := rl.ChooseReset(); d > 0 {
		s.ResetIn = d
		if s.WindowLabel == "" {
			s.WindowLabel = lbl
		}
	}
}

// snippet trims a body to ~120 chars.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

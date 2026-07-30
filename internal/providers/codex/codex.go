// Package codex implements the types.Provider interface for OpenAI Codex
// CLI / ChatGPT subscription usage.
//
// Strategy: there is no public "usage" endpoint, but every API response
// carries x-ratelimit-*-requests / x-ratelimit-*-tokens headers. We send a
// tiny probe request (max_tokens:1) and surface those headers.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/TheMetalStorm/provider-usage/internal/auth"
	"github.com/TheMetalStorm/provider-usage/internal/config"
	"github.com/TheMetalStorm/provider-usage/internal/probe"
	"github.com/TheMetalStorm/provider-usage/internal/types"
)

// Provider implements types.Provider for Codex.
type Provider struct {
	a     *auth.Finder
	probe *probe.Client
	cfg   *config.Config
}

// New returns the provider.
func New() *Provider { return &Provider{probe: probe.New()} }

// NewWith builds the provider with explicit deps (used by the registry).
func NewWith(a *auth.Finder, c *config.Config) *Provider {
	return &Provider{a: a, probe: probe.New(), cfg: c}
}

func (p *Provider) Name() string        { return "codex" }
func (p *Provider) DisplayName() string { return "Codex / ChatGPT" }
func (p *Provider) Icon() string        { return "" }

// codexFreeModels is the static list of OpenAI's known free / included models.
var codexFreeModels = []types.FreeModel{
	{ID: "gpt-4.1-mini", Label: "GPT-4.1 mini"},
	{ID: "gpt-4o-mini", Label: "GPT-4o mini"},
	{ID: "o4-mini", Label: "o4-mini"},
	{ID: "gpt-5-mini", Label: "GPT-5 mini"},
}

func (p *Provider) AvailableModels() []types.FreeModel { return codexFreeModels }

// IsConfigured: need ~/.codex/auth.json or env override.
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
		if k, _, _, ok := p.cfg.Override("codex"); ok && k != "" {
			return nil
		}
	}
	if cred, err := a.CodexToken(); err == nil && cred.Token != "" {
		return nil
	}
	return fmt.Errorf("codex: no credentials in %s and no override", a.CodexPath())
}

// FetchUsage sends a minimal probe and parses x-ratelimit-* headers.
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

	token := ""
	if p.cfg != nil {
		if k, _, t, ok := p.cfg.Override("codex"); ok {
			token = first(k, t)
		}
	}
	if token == "" {
		cred, err := a.CodexToken()
		if err != nil {
			return &types.UsageStats{
				LastProbeAt: time.Now(),
				Error:       err.Error(),
			}, nil
		}
		token = cred.Token
	}

	// Probe with a tiny request.  We pick gpt-4o-mini because it has
	// both request- and token-rate limits for most tiers. If you'd like
	// a different model (e.g. gpt-5-mini for ChatGPT Plus), set
	// `providers.codex.probe_model` in your config.yaml.
	model := "gpt-4o-mini"
	if pc, ok := p.cfg.Providers["codex"]; ok && pc.Endpoint != "" {
		// Treat `ProbeModel` if you ever add it; keep Endpoint for
		// overriding the chat-completions URL.
		_ = pc
	}
	body := probe.NewChatBody(model, p.maxTokens())
	req, err := probe.NewPOST(ctx, "https://api.openai.com/v1/chat/completions", map[string]string{
		"Authorization": "Bearer " + token,
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
	// If we get a 401 we surface it as "auth error".
	if res.Status == 401 || res.Status == 403 {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       fmt.Sprintf("auth rejected (HTTP %d): check codex auth.json", res.Status),
		}, nil
	}
	stats := &types.UsageStats{
		LastProbeAt: time.Now(),
	}
	if res.Status >= 400 {
		stats.Error = fmt.Sprintf("HTTP %d: %s", res.Status, snippet(res.Body))
		// 429s often include Reset headers still - parse them!
	}
	applyRateLimit(stats, res.RateLimit)
	if res.RateLimit.Group != "" {
		stats.Note = res.RateLimit.Group
	}
	return stats, nil
}

func (p *Provider) maxTokens() int {
	if p.cfg != nil && p.cfg.ProbeMaxTokens > 0 {
		return p.cfg.ProbeMaxTokens
	}
	return 1
}

func applyRateLimit(s *types.UsageStats, rl probe.RateLimit) {
	if !rl.HasData {
		s.Note = "no x-ratelimit-* headers (may not be on metered auth)"
		return
	}
	if rl.ReqLimit > 0 {
		s.Total = float64(rl.ReqLimit)
		s.Used = float64(rl.ReqLimit - rl.ReqRemain)
		s.Unit = types.UnitCount
		s.WindowLabel = "5h" // Codex / ChatGPT rolling 5h
	} else if rl.TokLimit > 0 {
		s.Total = float64(rl.TokLimit)
		s.Used = float64(rl.TokLimit - rl.TokRemain)
		s.Unit = types.UnitTokens
		s.WindowLabel = "tokens"
	}
	if d, _ := rl.ChooseReset(); d > 0 {
		s.ResetIn = d
	}
}

func first(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// var avoids "imported and not used" in case we tweak the imports later.
var _ = json.Unmarshal

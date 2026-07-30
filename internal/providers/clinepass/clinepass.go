// Package clinepass implements the types.Provider interface for ClinePass.
//
// ClinePass is a subscription sold by the Cline team that exposes an
// OpenAI-compatible chat-completions endpoint at api.cline.bot. The
// /provider/... endpoints expose rate-limit headers that we parse.
package clinepass

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/auth"
	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/probe"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

// Provider implements types.Provider for ClinePass.
type Provider struct {
	a     *auth.Finder
	probe *probe.Client
	cfg   *config.Config
}

// New returns a ClinePass provider.
func New() *Provider { return &Provider{probe: probe.New()} }

// NewWith wires up the provider.
func NewWith(a *auth.Finder, c *config.Config) *Provider {
	return &Provider{a: a, probe: probe.New(), cfg: c}
}

func (p *Provider) Name() string        { return "clinepass" }
func (p *Provider) DisplayName() string { return "Cline Pass" }
func (p *Provider) Icon() string        { return "" }

// clinepassFreeModels is the static list of ClinePass-bundled models
// (drawn from Cline docs / pricing page).
var clinepassFreeModels = []types.FreeModel{
	{ID: "cline-pass/glm-5.2", Label: "GLM-5.2"},
	{ID: "cline-pass/kimi-k2.7-code", Label: "Kimi K2.7 Code"},
	{ID: "cline-pass/deepseek-v4-pro", Label: "DeepSeek V4 Pro"},
	{ID: "cline-pass/mimo-v2.5-pro", Label: "MiMo V2.5 Pro"},
	{ID: "cline-pass/minimax-m3", Label: "minimax-m3"},
	{ID: "cline-pass/qwen3.7-plus", Label: "Qwen3.7 Plus"},
}

func (p *Provider) AvailableModels() []types.FreeModel { return clinepassFreeModels }

// IsConfigured: need cline config with apiKey, or override.
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
		if k, _, _, ok := p.cfg.Override("clinepass"); ok && k != "" {
			return nil
		}
	}
	// Native: try scanning.
	if cred, err := a.ClinePassCredentials(); err == nil && cred.Token != "" {
		return nil
	}
	return fmt.Errorf("clinepass: no credentials; set api_key under providers.clinepass in config or sign in via the Cline extension")
}

// FetchUsage: probe api.cline.bot with the cheapest model.
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
		if k, _, _, ok := p.cfg.Override("clinepass"); ok {
			token = k
		}
	}
	if token == "" {
		cred, err := a.ClinePassCredentials()
		if err != nil {
			return &types.UsageStats{
				LastProbeAt: time.Now(),
				Error:       err.Error(),
				Note:        "ClinePass requires a Cline account; sign in via the IDE or CLI",
			}, nil
		}
		token = cred.Token
	}

	body := probe.NewChatBody("cline-pass/minimax-m3", p.maxTokens())
	req, err := probe.NewPOST(ctx, "https://api.cline.bot/api/v1/chat/completions", map[string]string{
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
	stats := &types.UsageStats{
		LastProbeAt: time.Now(),
		WindowLabel: "rolling 5h",
	}
	if res.Status == 401 || res.Status == 403 {
		stats.Error = "auth rejected - check your ClinePass API key"
		return stats, nil
	}
	if res.Status >= 400 {
		stats.Error = fmt.Sprintf("HTTP %d: %s", res.Status, snippet(res.Body))
	}
	applyRateLimit(stats, res.RateLimit)
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
		s.Note = "no rate-limit headers - ClinePass may not be reporting quotas on this tier"
		return
	}
	if rl.ReqLimit > 0 {
		s.Total = float64(rl.ReqLimit)
		s.Used = float64(rl.ReqLimit - rl.ReqRemain)
		s.Unit = types.UnitCount
	} else if rl.TokLimit > 0 {
		s.Total = float64(rl.TokLimit)
		s.Used = float64(rl.TokLimit - rl.TokRemain)
		s.Unit = types.UnitTokens
	}
	if d, _ := rl.ChooseReset(); d > 0 {
		s.ResetIn = d
	}
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

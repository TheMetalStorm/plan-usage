// Package commandcode implements the types.Provider interface for CommandCode.
//
// Strategy: prefer the CLI's /usage subcommand if available; fall back to a
// tiny OpenAI-compatible probe against the CommandCode provider API.
package commandcode

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/auth"
	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/probe"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

// Provider implements types.Provider for CommandCode.
type Provider struct {
	a     *auth.Finder
	probe *probe.Client
	cfg   *config.Config
}

// New returns a CommandCode provider with a probe client.
func New() *Provider { return &Provider{probe: probe.New()} }

// NewWith wires up explicit deps.
func NewWith(a *auth.Finder, c *config.Config) *Provider {
	return &Provider{a: a, probe: probe.New(), cfg: c}
}

func (p *Provider) Name() string        { return "commandcode" }
func (p *Provider) DisplayName() string { return "Command Code" }
func (p *Provider) Icon() string        { return "" }

// commandcodeFreeModels is the static list of zero-credit models documented
// at commandcode.ai/docs/studio.
var commandcodeFreeModels = []types.FreeModel{
	{ID: "laguna-s-2.1", Label: "Laguna S 2.1", Notes: "free while capacity is available"},
	{ID: "ling-3.0-flash", Label: "Ling-3.0-flash", Notes: "free until 2 August 2026"},
}

func (p *Provider) AvailableModels() []types.FreeModel { return commandcodeFreeModels }

// IsConfigured: need an api key in ~/.commandcode/auth.json or env.
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
		if k, _, _, ok := p.cfg.Override("commandcode"); ok && k != "" {
			return nil
		}
	}
	if cred, err := a.CommandCodeToken(); err == nil && cred.Token != "" {
		return nil
	}
	return fmt.Errorf("commandcode: no credentials in %s or COMMAND_CODE_API_KEY env", a.CommandCodeAuthPath())
}

// FetchUsage: try a `cmd /usage --json` subprocess first; fall back to API probe.
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

	// 1. Subprocess path.
	if stats, ok := p.fetchCLI(ctx); ok {
		return stats, nil
	}

	// 2. API probe fallback.
	token := ""
	if p.cfg != nil {
		if k, _, _, ok := p.cfg.Override("commandcode"); ok {
			token = k
		}
	}
	if token == "" {
		cred, err := a.CommandCodeToken()
		if err != nil {
			return &types.UsageStats{
				LastProbeAt: time.Now(),
				Error:       err.Error(),
				Note:        "Run `cmd login` once to populate ~/.commandcode/auth.json",
			}, nil
		}
		token = cred.Token
	}

	body := probe.NewChatBody("commandcode/laguna-s-2.1", p.maxTokens())
	req, err := probe.NewPOST(ctx, "https://api.commandcode.ai/v1/chat/completions", map[string]string{
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
		WindowLabel: "5h",
	}
	if res.Status == 401 || res.Status == 403 {
		stats.Error = "auth rejected - check COMMAND_CODE_API_KEY"
		return stats, nil
	}
	if res.Status >= 400 {
		stats.Error = fmt.Sprintf("HTTP %d: %s", res.Status, snippet(res.Body))
	}
	applyRateLimit(stats, res.RateLimit)
	return stats, nil
}

// fetchCLI runs the local CLI for usage info.
func (p *Provider) fetchCLI(ctx context.Context) (*types.UsageStats, bool) {
	for _, bin := range []string{"cmd", "cc", "command-code", "commandcode"} {
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		// Try several CLI invocations (different versions shipped different flags).
		for _, arg := range [][]string{{"/usage", "--json"}, {"/usage", "--format=json"}, {"usage", "--json"}} {
			cmd := exec.CommandContext(ctx, bin, arg...)
			out, err := cmd.Output()
			if err != nil {
				continue
			}
			var doc struct {
				Used   float64 `json:"used"`
				Total  float64 `json:"total"`
				Window string  `json:"window"`
				Reset  string  `json:"reset"`
				Unit   string  `json:"unit"`
			}
			if json.Unmarshal(out, &doc) == nil && doc.Total > 0 {
				s := &types.UsageStats{
					Used:         doc.Used,
					Total:        doc.Total,
					Unit:         types.UnitUSD,
					WindowLabel:  doc.Window,
					LastProbeAt:  time.Now(),
					Note:         "via cmd /usage",
				}
				if doc.Reset != "" {
					if d, err := time.ParseDuration(doc.Reset); err == nil {
						s.ResetIn = d
					}
				}
				return s, true
			}
		}
	}
	return nil, false
}

func (p *Provider) maxTokens() int {
	if p.cfg != nil && p.cfg.ProbeMaxTokens > 0 {
		return p.cfg.ProbeMaxTokens
	}
	return 1
}

func applyRateLimit(s *types.UsageStats, rl probe.RateLimit) {
	if !rl.HasData {
		s.Note = "no rate-limit headers - CommandCode may not surface them on this tier"
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

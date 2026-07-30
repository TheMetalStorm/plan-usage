// Package freebuff implements the types.Provider interface for Freebuff.
//
// Freebuff does not expose a public REST API.  We rely on:
//   1. The Freebuff CLI's authenticated session (GitHub OAuth).
//   2. A minimal GET against the same backend endpoint the CLI polls for
//      session info. The exact path & response shape is the empirical
//      reverse-engineered one; if it changes this provider will fail
//      gracefully and just show the static free-model list.
//
// The auth token comes from ~/.config/manicode/credentials.json.
package freebuff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/simon/usage/internal/auth"
	"github.com/simon/usage/internal/config"
	"github.com/simon/usage/internal/probe"
	"github.com/simon/usage/internal/types"
)

// Provider implements types.Provider for Freebuff.
type Provider struct {
	a     *auth.Finder
	probe *probe.Client
	cfg   *config.Config
	hc    *http.Client
}

// New returns a Freebuff provider.
func New() *Provider {
	return &Provider{hc: &http.Client{Timeout: 8 * time.Second}, probe: probe.New()}
}

// NewWith wires up explicit deps.
func NewWith(a *auth.Finder, c *config.Config) *Provider {
	return &Provider{
		a:     a,
		cfg:   c,
		hc:    &http.Client{Timeout: 8 * time.Second},
		probe: probe.New(),
	}
}

func (p *Provider) Name() string        { return "freebuff" }
func (p *Provider) DisplayName() string { return "Freebuff" }
func (p *Provider) Icon() string        { return "" }

// freebuffFreeModels is drawn from freebuff.com / CodebuffAI repos.
var freebuffFreeModels = []types.FreeModel{
	{ID: "minimax-m3", Label: "minimax-m3", Notes: "smartest unlimited model; data may train"},
	{ID: "minimax-m2.7", Label: "minimax-m2.7", Notes: "fastest unlimited model"},
	{ID: "deepseek-v4-pro", Label: "DeepSeek V4 Pro"},
	{ID: "deepseek-v4-flash", Label: "DeepSeek V4 Flash"},
	{ID: "mimo-2.5-pro", Label: "MiMo 2.5 Pro"},
	{ID: "mimo-2.5", Label: "MiMo 2.5"},
	{ID: "kimi-k2.7-code", Label: "Kimi K2.7 Code"},
	{ID: "kimi-k2.6", Label: "Kimi K2.6"},
	{ID: "gemini-3.1-flash-lite-preview", Label: "Gemini 3.1 Flash Lite Preview", Notes: "for file search / research"},
}

func (p *Provider) AvailableModels() []types.FreeModel { return freebuffFreeModels }

// IsConfigured: need ~/.config/manicode/credentials.json with authToken.
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
		if k, _, _, ok := p.cfg.Override("freebuff"); ok && k != "" {
			return nil
		}
	}
	cred, err := a.FreebuffCredentials()
	if err != nil || cred.Token == "" {
		return fmt.Errorf("freebuff: %w (run `freebuff` once to authenticate)", err)
	}
	return nil
}

// FetchUsage tries:
//   1. GET https://freebuff.com/api/v1/session (server-side summary).
//   2. Fallback: mark as available + show static models only.
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
	cred, err := a.FreebuffCredentials()
	if err != nil {
		if p.cfg != nil {
			if k, _, _, ok := p.cfg.Override("freebuff"); ok {
				cred = &auth.Credential{Token: k, Source: "config override"}
			}
		}
		if cred == nil {
			return &types.UsageStats{
				LastProbeAt: time.Now(),
				Error:       err.Error(),
				Note:        "Freebuff requires `freebuff login` once to persist credentials",
			}, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://freebuff.com/api/v1/session", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.Token)
	req.Header.Set("User-Agent", p.probe.UserAgent)
	resp, err := p.hc.Do(req)
	if err != nil {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       err.Error(),
			Note:        "freebuff backend unreachable - showing static free-model list",
		}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if resp.StatusCode == http.StatusForbidden {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       "free mode requires CLI auth (HTTP 403)",
			Note:        "freebuff's API blocks direct calls; the CLI is the supported path",
		}, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       "freebuff credentials rejected (HTTP 401) -- re-run `freebuff` to refresh",
		}, nil
	}
	if resp.StatusCode >= 400 {
		// Endpoint guess might be wrong -- fall back to the static
		// free-models list and surface a soft hint rather than a raw
		// HTTP error.
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Note: fmt.Sprintf("freebuff backend returned HTTP %d; showing static free-models list only "+
				"(body: %s)", resp.StatusCode, snippet(body)),
		}, nil
	}

	var doc struct {
		MessagesToday int       `json:"messages_today"`
		MessagesLimit int       `json:"messages_limit"`
		ResetAt       time.Time `json:"reset_at"`
		Mode          string    `json:"mode"` // "full" or "limited"
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       "freebuff backend response shape unrecognized",
			Note:        fmt.Sprintf("body=%s", snippet(body)),
		}, nil
	}
	stats := &types.UsageStats{
		LastProbeAt: time.Now(),
	}
	if doc.MessagesLimit > 0 {
		stats.Unit = types.UnitCount
		stats.WindowLabel = "today"
		stats.Used = float64(doc.MessagesToday)
		stats.Total = float64(doc.MessagesLimit)
		if !doc.ResetAt.IsZero() {
			stats.ResetAt = doc.ResetAt
		}
	} else {
		stats.Note = fmt.Sprintf("freebuff mode=%s; no numeric limit exposed", doc.Mode)
	}
	return stats, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

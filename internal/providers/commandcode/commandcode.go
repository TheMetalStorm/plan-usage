// Package commandcode implements the types.Provider interface for CommandCode.
//
// Strategy: query the CommandCode alpha API endpoints
// (/alpha/whoami, /alpha/billing/credits, /alpha/billing/subscriptions,
// /alpha/usage/summary) to obtain real per-window usage data for the three
// plan windows — 5-hour rolling, weekly rolling, and monthly billing cycle.
// Falls back to a cheap OpenAI-compatible probe against the Provider API
// (https://api.commandcode.ai/provider/v1/chat/completions) when the alpha
// endpoints are unreachable.
package commandcode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

	// lastWin caches the windows from the most recent API fetch so
	// SnapshotWindows can populate Snapshot.Windows for the TUI.
	mu      sync.Mutex
	lastWin []types.UsageStats
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

// FetchUsage queries the CommandCode alpha API for real usage data across
// all three plan windows (5h, weekly, monthly). The first window (5h) is
// returned as the primary UsageStats; the full set is cached so
// SnapshotWindows can feed the TUI's multi-bar view.
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

	// 1. Alpha API path (primary — returns real per-window usage).
	if stats, ok := p.fetchAPIUsage(ctx); ok {
		return stats, nil
	}

	// 2. API probe fallback (Provider API chat-completions rate-limit headers).
	return p.fetchProbe(ctx)
}

// resolveToken returns the API key from config override or auth file.
func (p *Provider) resolveToken() (string, error) {
	if p.cfg != nil {
		if k, _, _, ok := p.cfg.Override("commandcode"); ok && k != "" {
			return k, nil
		}
	}
	if p.a == nil {
		var err error
		p.a, err = auth.NewFinder()
		if err != nil {
			return "", err
		}
	}
	cred, err := p.a.CommandCodeToken()
	if err != nil {
		return "", err
	}
	return cred.Token, nil
}

// ccAPIBase is the production CommandCode API root.
const ccAPIBase = "https://api.commandcode.ai"

// ccPlanMonthlyCredits maps CommandCode plan IDs to their monthly credit
// allocation (in USD). Sourced from the CLI's getPlanInfo table.
var ccPlanMonthlyCredits = map[string]float64{
	"individual-go":          10,
	"individual-pro":         30,
	"individual-provider":    15,
	"individual-max":         150,
	"individual-ultra":       300,
	"teams-pro":              40,
}

// --- alpha API response types ---

type ccCreditsResp struct {
	Credits struct {
		MonthlyCredits   float64 `json:"monthlyCredits"`
		PurchasedCredits float64 `json:"purchasedCredits"`
		FreeCredits      float64 `json:"freeCredits"`
	} `json:"credits"`
	WindowLimits struct {
		FiveHour struct {
			Used    float64 `json:"used"`
			Cap     float64 `json:"cap"`
			ResetAt int64   `json:"resetAt"` // Unix ms; 0 = window not yet opened
		} `json:"fiveHour"`
		Weekly struct {
			Used    float64 `json:"used"`
			Cap     float64 `json:"cap"`
			ResetAt int64   `json:"resetAt"` // Unix ms
		} `json:"weekly"`
	} `json:"windowLimits"`
}

type ccSubResp struct {
	Success bool `json:"success"`
	Data    struct {
		Status           string `json:"status"`
		CurrentPeriodEnd string `json:"currentPeriodEnd"` // RFC3339
		PlanID           string `json:"planId"`
	} `json:"data"`
}

type ccSummaryResp struct {
	TotalCost           float64 `json:"totalCost"`
	TotalMonthlyCredits float64 `json:"totalMonthlyCredits"`
	TotalCount          int     `json:"totalCount"`
}

// fetchAPIUsage calls the CommandCode alpha API to retrieve real usage data
// for all three plan windows. Returns (stats, true) on success.
func (p *Provider) fetchAPIUsage(ctx context.Context) (*types.UsageStats, bool) {
	token, err := p.resolveToken()
	if err != nil || token == "" {
		return nil, false
	}
	now := time.Now()

	// /alpha/whoami — confirms auth and gets user/org context.
	whoRes, err := p.getJSON(ctx, ccAPIBase+"/alpha/whoami", token)
	if err != nil || whoRes.Status != 200 {
		return nil, false
	}

	// /alpha/billing/credits — window limits (5h + weekly) and credit balances.
	creditsRes, err := p.getJSON(ctx, ccAPIBase+"/alpha/billing/credits", token)
	if err != nil || creditsRes.Status != 200 {
		return nil, false
	}

	// /alpha/billing/subscriptions — plan info and billing cycle end.
	subRes, err := p.getJSON(ctx, ccAPIBase+"/alpha/billing/subscriptions", token)
	if err != nil || subRes.Status != 200 {
		return nil, false
	}

	// /alpha/usage/summary — total cost within the billing period.
	sumRes, err := p.getJSON(ctx, ccAPIBase+"/alpha/usage/summary", token)
	if err != nil || sumRes.Status != 200 {
		return nil, false
	}

	var credits ccCreditsResp
	if err := json.Unmarshal(creditsRes.Body, &credits); err != nil {
		return nil, false
	}
	var sub ccSubResp
	if err := json.Unmarshal(subRes.Body, &sub); err != nil {
		return nil, false
	}
	var summary ccSummaryResp
	if err := json.Unmarshal(sumRes.Body, &summary); err != nil {
		return nil, false
	}

	windows := p.buildWindows(credits, sub, summary, now)
	if len(windows) == 0 {
		return nil, false
	}
	p.cacheWindows(windows)
	return &windows[0], true
}

// getJSON performs a GET request against the CommandCode API and returns
// the raw probe.Result.
func (p *Provider) getJSON(ctx context.Context, url, token string) (*probe.Result, error) {
	req, err := probe.NewGET(ctx, url, map[string]string{
		"Authorization": "Bearer " + token,
		"Accept":        "application/json",
	})
	if err != nil {
		return nil, err
	}
	return p.probe.Do(ctx, req)
}

// buildWindows constructs the three UsageStats windows (5h, weekly, monthly)
// from the alpha API responses.
func (p *Provider) buildWindows(credits ccCreditsResp, sub ccSubResp, summary ccSummaryResp, now time.Time) []types.UsageStats {
	// Derive the monthly credit pool from the subscription plan; fall back
	// to deriving it from the 5h cap (30% of monthly) if the plan is unknown.
	planCredits := ccPlanMonthlyCredits[sub.Data.PlanID]
	if planCredits == 0 && credits.WindowLimits.FiveHour.Cap > 0 {
		planCredits = credits.WindowLimits.FiveHour.Cap / 0.30
	}

	// 5-hour rolling window.
	fiveHour := types.UsageStats{
		Used:        credits.WindowLimits.FiveHour.Used,
		Total:       credits.WindowLimits.FiveHour.Cap,
		Unit:        types.UnitUSD,
		WindowLabel: "5h",
		LastProbeAt: now,
		Note:        "5h rolling window (30% of monthly)",
	}
	if credits.WindowLimits.FiveHour.ResetAt > 0 {
		fiveHour.ResetAt = time.UnixMilli(credits.WindowLimits.FiveHour.ResetAt)
		fiveHour.ResetIn = time.Until(fiveHour.ResetAt)
	}

	// Weekly rolling window.
	weekly := types.UsageStats{
		Used:        credits.WindowLimits.Weekly.Used,
		Total:       credits.WindowLimits.Weekly.Cap,
		Unit:        types.UnitUSD,
		WindowLabel: "weekly",
		LastProbeAt: now,
		Note:        "weekly rolling window (60% of monthly)",
	}
	if credits.WindowLimits.Weekly.ResetAt > 0 {
		weekly.ResetAt = time.UnixMilli(credits.WindowLimits.Weekly.ResetAt)
		weekly.ResetIn = time.Until(weekly.ResetAt)
	}

	// Monthly billing-cycle window.
	monthly := types.UsageStats{
		Used:        summary.TotalCost,
		Total:       planCredits,
		Unit:        types.UnitUSD,
		WindowLabel: "monthly",
		LastProbeAt: now,
		Note:        "monthly billing cycle",
	}
	if sub.Data.CurrentPeriodEnd != "" {
		if t, err := time.Parse(time.RFC3339, sub.Data.CurrentPeriodEnd); err == nil {
			monthly.ResetAt = t
			monthly.ResetIn = time.Until(t)
		}
	}

	return []types.UsageStats{fiveHour, weekly, monthly}
}

// SnapshotWindows implements providers.MultiWindowProvider. The first entry
// is the primary window (5h); the rest populate Snapshot.Windows for the
// TUI's multi-bar view.
func (p *Provider) SnapshotWindows() []types.UsageStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.lastWin) == 0 {
		return nil
	}
	out := make([]types.UsageStats, len(p.lastWin))
	copy(out, p.lastWin)
	return out
}

// cacheWindows caches the windows from the most recent API fetch.
func (p *Provider) cacheWindows(win []types.UsageStats) {
	if len(win) == 0 {
		return
	}
	p.mu.Lock()
	p.lastWin = append([]types.UsageStats(nil), win...)
	p.mu.Unlock()
}

// Compile-time check that Provider satisfies MultiWindowProvider.
var _ interface {
	SnapshotWindows() []types.UsageStats
} = (*Provider)(nil)

// --- probe fallback ---

// fetchProbe sends a tiny chat-completions request to the CommandCode
// Provider API and derives usage from the x-ratelimit-* headers. This is
// a last-resort fallback when the alpha usage endpoints are unreachable.
func (p *Provider) fetchProbe(ctx context.Context) (*types.UsageStats, error) {
	token, err := p.resolveToken()
	if err != nil {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       err.Error(),
			Note:        "Run `cmd login` once to populate ~/.commandcode/auth.json",
		}, nil
	}

	body := probe.NewChatBody("deepseek/deepseek-v4-flash", p.maxTokens())
	req, err := probe.NewPOST(ctx, "https://api.commandcode.ai/provider/v1/chat/completions", map[string]string{
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

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
// Strategy:
//  1. Read local opencode.db SQLite database for per-session cost/token
//     data. This gives us real Used $ figures for each window.
//  2. Optionally hit the opencode.ai /_server RPC endpoint for server-
//     side rolling/weekly/monthly percentages, which overlay the local
//     snapshot when a session cookie is available.
//  3. The local monthly window is anchored at the earliest local cost
//     row and can drift from the real billing cycle.
package opencodego

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/auth"
	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/opencodeutil"
	"github.com/TheMetalStorm/plan-usage/internal/probe"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

// Provider implements types.Provider for OpenCode Go.
type Provider struct {
	a      *auth.Finder
	cfg    *config.Config
	apiKey string // resolved Bearer token
	db     *opencodeutil.OpenCodeDB
	probe  *probe.Client
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

// Plan limits for the three Go subscription windows.
const (
	plan5h     = 12.0
	planWeekly = 30.0
	planMonth  = 60.0
)

// IsConfigured: configured if auth.json has an `opencode-go` entry OR
// we can read the local opencode.db with cost data.
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

	// Always try to open the local DB — powers cost history.
	db, dbErr := opencodeutil.OpenDB()
	if dbErr == nil && db != nil {
		p.db = db
	}

	// Check config override first.
	if p.cfg != nil {
		if k, _, _, ok := p.cfg.Override("opencodego"); ok && k != "" {
			p.apiKey = k
			return nil // configured via override
		}
	}

	// Try auth.json for opencode-go key.
	tokens, src, err := a.OpenCodeAuth()
	if err == nil {
		raw, ok := tokens["opencode-go"]
		if ok && len(raw) > 0 {
			var entry struct {
				Type string `json:"type"`
				Key  string `json:"key"`
			}
			if json.Unmarshal(raw, &entry) == nil && entry.Key != "" {
				p.apiKey = entry.Key
				return nil // configured via API key
			}
			if p.db == nil {
				return errors.New("opencodego: `opencode-go` entry is missing the key field")
			}
			return nil // DB available even without valid API key
		}
	}

	// DB alone is enough to be "configured".
	if p.db != nil {
		return nil
	}

	return fmt.Errorf("opencodego: no auth.json entry for opencode-go (looked at %s) and cannot read local DB: %v", src, dbErr)
}

// FetchUsage returns the primary window (weekly costs by default, since
// the 5h rolling window is frequently zero). The SnapshotWindows method
// provides all three windows (5h, weekly, monthly) for multi-window UIs.
func (p *Provider) FetchUsage(ctx context.Context) (*types.UsageStats, error) {
	// Ensure auth / DB are resolved.
	if p.apiKey == "" && p.db == nil {
		_ = p.IsConfigured()
	}

	now := time.Now()

	// Compute local costs for each window.
	cost5h := p.costSince(now.Add(-5 * time.Hour))
	costWeekly := p.costSince(weekStart(now))
	costMonth := p.costSince(monthStart(now))

	// Try server-side overlay for rolling percentage if we have a cookie.
	serverData, _ := p.fetchServerUsage(ctx)

	// Pick the most meaningful primary window.
	// Prefer weekly (often has data), then 5h, then monthly.
	var primary types.UsageStats

	if serverData != nil && serverData.RollingPercent > 0 {
		// Server overlay available — show rolling percentage.
		resetIn := time.Duration(serverData.RollingReset) * time.Second
		primary = types.UsageStats{
			Used:        serverData.RollingPercent,
			Total:       100,
			Unit:        types.UnitCount,
			WindowLabel: "5h rolling",
			ResetIn:     resetIn,
			ResetAt:     now.Add(resetIn),
			LastProbeAt: now,
			Note:        "server usage",
		}
	} else if costWeekly > 0 {
		// Weekly local cost is non-zero — show that.
		primary = types.UsageStats{
			Used:        costWeekly,
			Total:       planWeekly,
			Unit:        types.UnitUSD,
			WindowLabel: "weekly",
			LastProbeAt: now,
			Note:        "local costs since Sunday",
		}
	} else if costMonth > 0 {
		primary = types.UsageStats{
			Used:        costMonth,
			Total:       planMonth,
			Unit:        types.UnitUSD,
			WindowLabel: "monthly",
			LastProbeAt: now,
			Note:        "local costs since 1st",
		}
	} else {
		primary = types.UsageStats{
			Used:        cost5h,
			Total:       plan5h,
			Unit:        types.UnitUSD,
			WindowLabel: "5h",
			LastProbeAt: now,
			Note:        "no recent paid usage — check opencode.ai/auth",
		}
	}

	return &primary, nil
}

// SnapshotWindows returns the three Go-plan windows with data from
// the local opencode.db. The first entry is the "primary" window (5h)
// that the TUI's compact view summarizes.
func (p *Provider) SnapshotWindows() []types.UsageStats {
	now := time.Now()

	// Compute local costs for each window.
	cost5h := p.costSince(now.Add(-5 * time.Hour))
	costWeekly := p.costSince(weekStart(now))
	costMonth := p.costSince(monthStart(now))

	// Monthly history note.
	historyNote := p.dailyHistoryNote(14)

	return []types.UsageStats{
		{
			Used:         cost5h,
			Total:        plan5h,
			Unit:         types.UnitUSD,
			WindowLabel:  "5h",
			Note:         "local costs, last 5 hours",
			LastProbeAt:  now,
		},
		{
			Used:         costWeekly,
			Total:        planWeekly,
			Unit:         types.UnitUSD,
			WindowLabel:  "weekly",
			Note:         "local costs since Sunday",
			LastProbeAt:  now,
		},
		{
			Used:         costMonth,
			Total:        planMonth,
			Unit:         types.UnitUSD,
			WindowLabel:  "monthly",
			Note:         historyNote,
			LastProbeAt:  now,
		},
	}
}

// -- helpers --

// costSince returns total cost from local DB since a given time.
func (p *Provider) costSince(since time.Time) float64 {
	if p.db == nil {
		return 0
	}
	cost, err := p.db.TotalCostSince(since.UnixMilli())
	if err != nil {
		return 0
	}
	return cost
}

// dailyHistoryNote returns a compact summary of recent daily costs.
func (p *Provider) dailyHistoryNote(days int) string {
	if p.db == nil {
		return "local costs since 1st"
	}
	history, err := p.db.DailyCostHistory(days)
	if err != nil || len(history) == 0 {
		return "local costs since 1st"
	}
	total := 0.0
	for _, d := range history {
		total += d.Cost
	}
	return fmt.Sprintf("local costs since 1st (last %d days: $%.2f, %d sessions)", len(history), total, history[0].Sessions)
}

// fetchServerUsage tries the _server endpoint for overlay data.
func (p *Provider) fetchServerUsage(ctx context.Context) (*opencodeutil.ServerUsage, error) {
	if p.apiKey == "" && !p.haveCookie() {
		return nil, errors.New("no auth available for server query")
	}
	client := opencodeutil.NewServerClient(p.apiKey)
	if cc, err := opencodeutil.NewCookieCache(); err == nil {
		client.SetCookieCache(cc)
	}
	workspaceID := opencodeutil.ResolveWorkspaceID(p.cfgWorkspaceOverride())
	return client.FetchUsage(ctx, workspaceID)
}

// haveCookie checks if a cookie cache exists with a valid cookie.
func (p *Provider) haveCookie() bool {
	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		return false
	}
	return cc.Cookie() != ""
}

// cfgWorkspaceOverride checks the user's config for a workspace override.
func (p *Provider) cfgWorkspaceOverride() string {
	if p.cfg == nil {
		return ""
	}
	if pc, ok := p.cfg.Providers["opencodego"]; ok {
		if pc.Endpoint != "" {
			return pc.Endpoint
		}
	}
	return ""
}

// weekStart returns the start of the current week (Sunday 00:00 local).
func weekStart(t time.Time) time.Time {
	daysSinceSunday := int(t.Weekday()) // 0=Sun, 1=Mon, ...
	y, m, d := t.AddDate(0, 0, -daysSinceSunday).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// monthStart returns the start of the current month.
func monthStart(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
}

// Compile-time check for SnapshotWindows method.
var _ interface {
	SnapshotWindows() []types.UsageStats
} = (*Provider)(nil)

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
	"sync"
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

	mu            sync.Mutex
	lastServer    *opencodeutil.ServerUsage // cached server-side windows
	lastServerAt  time.Time                 // when lastServer was fetched
	lastServerErr string                    // stale-cookie hint when server data is unavailable
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

	// serverDataTTL bounds how long the server-side windows cached by
	// FetchUsage stay fresh for SnapshotWindows (no extra network I/O).
	serverDataTTL = 10 * time.Minute
)

// staleCookieNote is appended to the local-fallback monthly bar when a
// cookie existed (cached or just imported) but the server produced no
// usage — the opencode.ai session has expired or been logged out.
const staleCookieNote = "server data unavailable — cookie expired, re-import with 'plan-usage opencode-cookie import'"

// loginHintNote is appended to the local-fallback monthly bar when there is
// no session cookie anywhere: the user must log in at opencode.ai (or paste
// a cookie) before the live server percentages can load.
const loginHintNote = "server data unavailable — log in at opencode.ai to enable live usage"

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

// FetchUsage returns the primary window. When the server-side billing
// data is available (browser session cookie cached), the primary window is
// the most pressured server window; otherwise it falls back to the local
// opencode.db costs. The server result is cached so SnapshotWindows can
// render all three bars from the same data.
func (p *Provider) FetchUsage(ctx context.Context) (*types.UsageStats, error) {
	// Ensure auth / DB are resolved.
	if p.apiKey == "" && p.db == nil {
		_ = p.IsConfigured()
	}

	now := time.Now()

	// Capture whether a cookie existed before the fetch: fetchServerUsage
	// may auto-import one from the browser when the cache is empty.
	hadCookie := p.haveCookie()

	// Try server-side overlay for the authoritative billing percentages.
	serverData, _ := p.fetchServerUsage(ctx)

	// A cookie is present (cached or just imported) yet the server produced
	// no usage: the session is stale. Drop it so the next refresh re-imports
	// from the browser, and surface a hint on the local fallback.
	stale := serverData == nil && (hadCookie || p.haveCookie())

	// Cache the latest server result (or clear it on failure) so that
	// SnapshotWindows renders server data without its own network call.
	p.mu.Lock()
	if serverData != nil {
		p.lastServer = serverData
		p.lastServerAt = now
		p.lastServerErr = ""
	} else {
		p.lastServer = nil
		p.lastServerAt = time.Time{}
		if stale {
			p.lastServerErr = staleCookieNote
		} else {
			// No cookie anywhere: the card should tell the user to log in
			// at opencode.ai (or paste a cookie) to enable live usage.
			p.lastServerErr = loginHintNote
		}
	}
	p.mu.Unlock()

	if stale {
		p.clearCookieCache()
	}

	if serverData != nil && serverData.AnyWindow() {
		primary := p.serverPrimary(serverData, now)
		return &primary, nil
	}

	// No server data — use local costs.
	cost5h := p.costSince(now.Add(-5 * time.Hour))
	costWeekly := p.costSince(weekStart(now))
	costMonth := p.costSince(monthStart(now))
	primary := p.localPrimary(cost5h, costWeekly, costMonth, now)
	return &primary, nil
}

// SnapshotWindows returns the three Go-plan windows. When the server-side
// billing data was fetched recently (see FetchUsage), the bars show the
// authoritative server percentages; otherwise they fall back to local
// opencode.db costs.
func (p *Provider) SnapshotWindows() []types.UsageStats {
	now := time.Now()
	if s := p.freshServerData(now); s != nil {
		return p.serverWindows(s, now)
	}
	return p.localWindows(now)
}

// -- window builders --

// freshServerData returns the cached server usage if it was fetched within
// serverDataTTL; otherwise nil.
func (p *Provider) freshServerData(now time.Time) *opencodeutil.ServerUsage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastServer == nil || p.lastServerAt.IsZero() {
		return nil
	}
	if now.Sub(p.lastServerAt) > serverDataTTL {
		return nil
	}
	return p.lastServer
}

// serverPrimary picks the most pressured server window as the primary.
func (p *Provider) serverPrimary(s *opencodeutil.ServerUsage, now time.Time) types.UsageStats {
	type cand struct {
		pct   float64
		reset int64
		label string
	}
	cands := make([]cand, 0, 3)
	if s.RollingReset > 0 {
		cands = append(cands, cand{s.RollingPercent, s.RollingReset, "5h rolling"})
	}
	if s.WeeklyReset > 0 {
		cands = append(cands, cand{s.WeeklyPercent, s.WeeklyReset, "weekly"})
	}
	if s.MonthlyReset > 0 {
		cands = append(cands, cand{s.MonthlyPercent, s.MonthlyReset, "monthly"})
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if c.pct > best.pct {
			best = c
		}
	}
	resetIn := time.Duration(best.reset) * time.Second
	return types.UsageStats{
		Used:        best.pct,
		Total:       100,
		Unit:        types.UnitCount,
		WindowLabel: best.label,
		ResetIn:     resetIn,
		ResetAt:     now.Add(resetIn),
		LastProbeAt: now,
		Note:        "server usage",
	}
}

// serverWindows builds one bar per server window that was present.
func (p *Provider) serverWindows(s *opencodeutil.ServerUsage, now time.Time) []types.UsageStats {
	ws := make([]types.UsageStats, 0, 3)
	add := func(pct float64, reset int64, label string) {
		if reset <= 0 {
			return
		}
		resetIn := time.Duration(reset) * time.Second
		ws = append(ws, types.UsageStats{
			Used:        pct,
			Total:       100,
			Unit:        types.UnitCount,
			WindowLabel: label,
			ResetIn:     resetIn,
			ResetAt:     now.Add(resetIn),
			LastProbeAt: now,
			Note:        "server usage",
		})
	}
	add(s.RollingPercent, s.RollingReset, "5h")
	add(s.WeeklyPercent, s.WeeklyReset, "weekly")
	add(s.MonthlyPercent, s.MonthlyReset, "monthly")
	return ws
}

// localPrimary picks the most meaningful local-DB window. Weekly first
// (often has data), then monthly, then the 5h rolling window.
func (p *Provider) localPrimary(cost5h, costWeekly, costMonth float64, now time.Time) types.UsageStats {
	if costWeekly > 0 {
		return types.UsageStats{
			Used:        costWeekly,
			Total:       planWeekly,
			Unit:        types.UnitUSD,
			WindowLabel: "weekly",
			LastProbeAt: now,
			Note:        "local costs since Sunday",
		}
	}
	if costMonth > 0 {
		return types.UsageStats{
			Used:        costMonth,
			Total:       planMonth,
			Unit:        types.UnitUSD,
			WindowLabel: "monthly",
			LastProbeAt: now,
			Note:        "local costs since 1st",
		}
	}
	return types.UsageStats{
		Used:        cost5h,
		Total:       plan5h,
		Unit:        types.UnitUSD,
		WindowLabel: "5h",
		LastProbeAt: now,
		Note:        "no recent paid usage — check opencode.ai/auth",
	}
}

// localWindows returns the three Go-plan windows computed from the local
// opencode.db costs (calendar-anchored: last 5h / since Sunday / since 1st).
func (p *Provider) localWindows(now time.Time) []types.UsageStats {
	// Compute local costs for each window.
	cost5h := p.costSince(now.Add(-5 * time.Hour))
	costWeekly := p.costSince(weekStart(now))
	costMonth := p.costSince(monthStart(now))

	// Monthly history note (plus the stale-cookie hint when the server
	// produced no usage despite having a cookie).
	historyNote := p.dailyHistoryNote(14)
	if errNote := p.serverErrNote(); errNote != "" {
		historyNote += "; " + errNote
	}

	return []types.UsageStats{
		{
			Used:        cost5h,
			Total:       plan5h,
			Unit:        types.UnitUSD,
			WindowLabel: "5h",
			Note:        "local costs, last 5 hours",
			LastProbeAt: now,
		},
		{
			Used:        costWeekly,
			Total:       planWeekly,
			Unit:        types.UnitUSD,
			WindowLabel: "weekly",
			Note:        "local costs since Sunday",
			LastProbeAt: now,
		},
		{
			Used:        costMonth,
			Total:       planMonth,
			Unit:        types.UnitUSD,
			WindowLabel: "monthly",
			Note:        historyNote,
			LastProbeAt: now,
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
	sessions := 0
	for _, d := range history {
		total += d.Cost
		sessions += d.Sessions
	}
	return fmt.Sprintf("local costs since 1st (last %d days: $%.2f, %d sessions)", len(history), total, sessions)
}

// fetchServerUsage tries the _server endpoint for overlay data. When no
// cookie is cached it first tries to import one from the browser, so a
// missing/expired cookie self-heals on the next refresh.
func (p *Provider) fetchServerUsage(ctx context.Context) (*opencodeutil.ServerUsage, error) {
	// The _server endpoint only accepts a browser session cookie; API keys
	// are rejected there.
	p.importCookieIfMissing()

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

// importCookieIfMissing imports the opencode.ai auth cookie from the
// browser when the cache is empty. Bounded: a no-op whenever a cookie is
// already cached.
func (p *Provider) importCookieIfMissing() {
	cc, err := opencodeutil.NewCookieCache()
	if err != nil || cc.Cookie() != "" {
		return
	}
	value, err := opencodeutil.ImportOpenCodeCookie()
	if err != nil || value == "" {
		return
	}
	_ = cc.Write(&opencodeutil.CacheCookie{Source: "chrome-import", Cookie: value, CachedAt: time.Now()})
}

// clearCookieCache removes the cached cookie so the next refresh re-imports
// from the browser (which may hold a fresh session).
func (p *Provider) clearCookieCache() {
	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		return
	}
	_ = cc.Write(&opencodeutil.CacheCookie{})
}

// serverErrNote returns the stale-cookie hint ("" when none), under mutex.
func (p *Provider) serverErrNote() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastServerErr
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

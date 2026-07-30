// Package clinepass implements the types.Provider interface for ClinePass.
//
// ClinePass is a subscription sold by the Cline team. Two HTTP endpoints
// are used by this provider:
//
//	GET https://api.cline.bot/api/v1/users/me/plan/usage-limits
//	  Auth: Bearer <Cline Pass API key, prefix "sk_…">
//	  Returns the three concurrent usage windows the dashboard draws
//	  (5-hour rolling, weekly rolling, monthly rolling) as percentages
//	  with absolute reset timestamps.
//
//	GET https://api.cline.bot/api/v1/models
//	  Returns the catalogue of models available on the plan.
//
// Important: the WorkOS OAuth session token that the standalone CLINE
// CLI writes to ~/.cline/data/settings/providers.json (its accessToken
// has the "workos:…" prefix) is NOT accepted as a Bearer by either of
// these endpoints — the backend distinguishes between the dashboard
// session and the Cline Pass API key. The auth resolver therefore
// prefers an explicit Cline Pass API key (config override or
// $CLINE_API_KEY) before falling back to the WorkOS session token; on
// a 401 we tell the user which source the rejected token came from so
// they can fix exactly one thing.
package clinepass

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/auth"
	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/probe"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

// Provider implements types.Provider for ClinePass.
type Provider struct {
	a       *auth.Finder
	probe   *probe.Client
	cfg     *config.Config
	mu      sync.Mutex         // protects lastWin
	lastWin []types.UsageStats // last multi-window snapshot from a successful probe
}

// New returns a ClinePass provider. The providers registry
// (internal/providers/provider.go) only knows about New() — never
// NewWith(...) — so every Provider handed to providers.All() would
// otherwise arrive with cfg == nil and a == nil. Clinepass has no
// CLI/DB fallback (unlike codex/opencodego), so an un-wired Provider
// always fails IsConfigured and the TUI renders "✗ no-auth". We
// lazy-init *config.Config and *auth.Finder here from their canonical
// paths so the returned Provider behaves identically to NewWith().
func New() *Provider {
	p := &Provider{probe: probe.New()}
	if cfg, err := config.Load(""); err == nil {
		p.cfg = cfg
	}
	if a, err := auth.NewFinder(); err == nil {
		p.a = a
	}
	return p
}

// NewWith wires up the provider.
func NewWith(a *auth.Finder, c *config.Config) *Provider {
	return &Provider{a: a, probe: probe.New(), cfg: c}
}

func (p *Provider) Name() string        { return "clinepass" }
func (p *Provider) DisplayName() string { return "Cline Pass" }
func (p *Provider) Icon() string        { return "" }

// clinepassFreeModels is the static fallback list used when /v1/models
// cannot be reached (auth missing, 401, 5xx, parse error). Drawn from
// the plan's entitlements + the public Cline docs / pricing page.
var clinepassFreeModels = []types.FreeModel{
	{ID: "cline-pass/glm-5.2", Label: "GLM-5.2"},
	{ID: "cline-pass/kimi-k2.7-code", Label: "Kimi K2.7 Code"},
	{ID: "cline-pass/deepseek-v4-pro", Label: "DeepSeek V4 Pro"},
	{ID: "cline-pass/mimo-v2.5-pro", Label: "MiMo V2.5 Pro"},
	{ID: "cline-pass/minimax-m3", Label: "minimax-m3"},
	{ID: "cline-pass/qwen3.7-plus", Label: "Qwen3.7 Plus"},
}

// Endpoints + tuning knobs.
const (
	clinepassUsageLimitsEndpoint = "https://api.cline.bot/api/v1/users/me/plan/usage-limits"
	clinepassModelsEndpoint      = "https://api.cline.bot/api/v1/models"
	clinepassModelsPrefix        = "cline-pass/"
	clinepassModelsFetchLimit    = 5 * time.Second
	clinepassModelsCacheTTL      = 5 * time.Minute
)

// Package-level cache that survives the per-snapshot Provider instances
// produced by providers.All(). The mutex serialises in-flight fetches so
// concurrent callers (daemon + TUI render) don't burst the endpoint.
var (
	modelsMu       sync.Mutex
	cachedModels   []types.FreeModel
	cachedModelsAt time.Time
)

// --- /v1/models (dynamic catalogue, static fallback on failure) --------

// AvailableModels returns the Cline Pass model list, preferring a fresh
// GET /v1/models response over the static fallback. When the endpoint is
// unreachable (auth missing, 401, 5xx, parse error), we serve the
// clinepassFreeModels fallback list and annotate the first entry's Notes
// field so the UI can show why the dynamic list isn't reflected.
func (p *Provider) AvailableModels() []types.FreeModel {
	modelsMu.Lock()
	defer modelsMu.Unlock()

	if len(cachedModels) > 0 && !cachedModelsAt.IsZero() &&
		time.Since(cachedModelsAt) < clinepassModelsCacheTTL {
		return cachedModels
	}

	live, reason := p.fetchClineModelsLocked()
	if live != nil {
		cachedModels = live
		cachedModelsAt = time.Now()
		return cachedModels
	}
	return cacheOrStatic(reason)
}

// fetchClineModelsLocked calls GET /v1/models with the Bearer token
// resolved from the override > env > WorkOS chain (same resolution
// FetchUsage uses). Returns a nil slice on any failure together with a
// short, UI-friendly reason. Caller must hold modelsMu.
func (p *Provider) fetchClineModelsLocked() (live []types.FreeModel, reason string) {
	a := p.a
	if a == nil {
		var err error
		a, err = auth.NewFinder()
		if err != nil {
			return nil, fmt.Sprintf("auth finder init failed: %v", err)
		}
		p.a = a
	}
	token, tokenSrc := p.resolveClineToken()
	if token == "" {
		return nil, "not signed in to Cline Pass (set providers.clinepass.api_key, CLINE_API_KEY, or sign in via Cline CLI)"
	}

	ctx, cancel := context.WithTimeout(context.Background(), clinepassModelsFetchLimit)
	defer cancel()
	req, err := probe.NewGET(ctx, clinepassModelsEndpoint, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if err != nil {
		return nil, fmt.Sprintf("build model-list request: %v", err)
	}
	res, err := p.probe.Do(ctx, req)
	if err != nil {
		return nil, fmt.Sprintf("probe /v1/models failed: %v", err)
	}
	if res.Status == 401 || res.Status == 403 {
		return nil, fmt.Sprintf("auth rejected on /v1/models (source: %s); api.cline.bot requires a Cline Pass API key (sk_…) — generate one at https://app.cline.bot/settings/api-keys and set providers.clinepass.api_key", tokenSrc)
	}
	if res.Status >= 400 {
		return nil, fmt.Sprintf("/v1/models returned HTTP %d", res.Status)
	}
	var doc struct {
		Data []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		return nil, fmt.Sprintf("parse /v1/models: %v", err)
	}
	out := make([]types.FreeModel, 0, len(doc.Data))
	for _, m := range doc.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		// Tolerate the API returning un-prefixed ids so we still render
		// a sensible model line.
		if !strings.HasPrefix(id, clinepassModelsPrefix) {
			id = clinepassModelsPrefix + id
		}
		out = append(out, types.FreeModel{
			ID:    id,
			Label: strings.TrimPrefix(id, clinepassModelsPrefix),
		})
	}
	if len(out) == 0 {
		return nil, "/v1/models returned an empty list"
	}
	return out, ""
}

// cacheOrStatic returns either a fresh shallow copy of the previous-good
// cached list (annotated with reason on failure) or, when nothing has
// ever been cached, a copy of the static fallback annotated with reason.
// Caller must hold modelsMu.
func cacheOrStatic(reason string) []types.FreeModel {
	if len(cachedModels) > 0 {
		out := make([]types.FreeModel, len(cachedModels))
		copy(out, cachedModels)
		if reason != "" && len(out) > 0 {
			out[0].Notes = reason
		}
		cachedModels = out
		cachedModelsAt = time.Now()
		return out
	}
	out := make([]types.FreeModel, len(clinepassFreeModels))
	copy(out, clinepassFreeModels)
	if reason != "" && len(out) > 0 {
		out[0].Notes = reason + "; showing static fallback list"
	}
	cachedModels = out
	cachedModelsAt = time.Now()
	return out
}

// --- /v1/users/me/plan/usage-limits (live quota windows) ---------------

// IsConfigured reports whether the provider has a usable credential
// available, without performing any network I/O. Delegates to
// resolveClineToken so the priority chain stays in one place.
func (p *Provider) IsConfigured() error {
	if token, _ := p.resolveClineToken(); token != "" {
		return nil
	}
	return fmt.Errorf("clinepass: no credentials; set api_key under providers.clinepass in config, sign in via the Cline CLI (`cline auth`), or export CLINE_API_KEY (a Cline Pass API key with the sk_… prefix — generate at https://app.cline.bot/settings/api-keys)")
}

// FetchUsage probes the Cline Pass quota endpoint and emits up to three
// rolling-window UsageStats (5-hour / weekly / monthly). On any error
// it returns a single primary UsageStats with Error populated and lets
// SnapshotWindows() fall back to the cached lastWin or the default
// scaffold so the TUI still shows three consistent bars.
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

	now := time.Now()
	token, tokenSrc := p.resolveClineToken()
	if token == "" {
		return &types.UsageStats{
			LastProbeAt: now,
			WindowLabel: "5h",
			Error:       "clinepass: no credentials resolved (set providers.clinepass.api_key or CLINE_API_KEY, or sign in via Cline CLI)",
			Note:        "no bearer available",
		}, nil
	}

	req, err := probe.NewGET(ctx, clinepassUsageLimitsEndpoint, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if err != nil {
		return nil, err
	}
	res, err := p.probe.Do(ctx, req)
	if err != nil {
		return &types.UsageStats{
			LastProbeAt: now,
			WindowLabel: "5h",
			Error:       err.Error(),
		}, nil
	}
	if res.Status == 401 || res.Status == 403 {
		return &types.UsageStats{
			LastProbeAt: now,
			WindowLabel: "5h",
			Error: fmt.Sprintf(
				"Cline Pass API rejected the bearer (source: %s). api.cline.bot requires a separate Cline Pass API key (prefix sk_…); the WorkOS OAuth session stored by the standalone CLINE CLI in providers.json is not accepted by this endpoint. Generate a key at https://app.cline.bot/settings/api-keys and set providers.clinepass.api_key (or export CLINE_API_KEY).",
				tokenSrc,
			),
		}, nil
	}
	if res.Status >= 400 {
		return &types.UsageStats{
			LastProbeAt: now,
			WindowLabel: "5h",
			Error:       fmt.Sprintf("HTTP %d: %s", res.Status, snippet(res.Body)),
		}, nil
	}
	wins, err := parseClineLimits(res.Body, now)
	if err != nil {
		return &types.UsageStats{
			LastProbeAt: now,
			WindowLabel: "5h",
			Error:       err.Error(),
		}, nil
	}
	p.mu.Lock()
	p.lastWin = make([]types.UsageStats, len(wins))
	copy(p.lastWin, wins)
	p.mu.Unlock()

	// Pick the 5-hour window as the primary (legacy single-bar callers
	// expect one UsageStats without needing to consult SnapshotWindows).
	primary := wins[0]
	for _, w := range wins {
		if w.WindowLabel == "5h" {
			primary = w
			break
		}
	}
	return &primary, nil
}

// SnapshotWindows implements providers.MultiWindowProvider. Always
// returns 3 windows so the TUI's multi-bar renderer formats correctly
// even before a successful probe. Returns a defensive copy so concurrent
// callers can't mutate the cache.
func (p *Provider) SnapshotWindows() []types.UsageStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.lastWin) == 0 {
		return defaultClineWindows()
	}
	out := make([]types.UsageStats, len(p.lastWin))
	copy(out, p.lastWin)
	return out
}

// resolveClineToken picks the bearer in priority order:
//  1. config override  (providers.clinepass.api_key)
//  2. $CLINE_API_KEY env
//  3. standalone CLINE CLI providers.json (WorkOS session — usually
//     rejected by api.cline.bot/api/v1/, kept as last-resort fallback
//     so the user at least sees "signed in" instead of "no creds").
//
// Returns (token, source-string) so the 401 path can show exactly which
// file or env var the user should edit.
func (p *Provider) resolveClineToken() (string, string) {
	if p.cfg != nil {
		if k, _, _, ok := p.cfg.Override("clinepass"); ok && k != "" {
			return k, "config override (providers.clinepass.api_key)"
		}
	}
	if env := strings.TrimSpace(os.Getenv("CLINE_API_KEY")); env != "" {
		return env, "env:CLINE_API_KEY"
	}
	if p.a != nil {
		cred, err := p.a.ClinePassCredentials()
		if err == nil && cred.Token != "" {
			return cred.Token, cred.Source
		}
	}
	return "", "no credential resolved"
}

// defaultClineWindows returns the canonical 3-window scaffold so the
// TUI renders multi-bar headings even before any successful probe.
func defaultClineWindows() []types.UsageStats {
	now := time.Now()
	return []types.UsageStats{
		{Used: 0, Total: 100, Unit: types.UnitCount, WindowLabel: "5h", LastProbeAt: now, Note: "no probe yet"},
		{Used: 0, Total: 100, Unit: types.UnitCount, WindowLabel: "weekly", LastProbeAt: now, Note: "no probe yet"},
		{Used: 0, Total: 100, Unit: types.UnitCount, WindowLabel: "monthly", LastProbeAt: now, Note: "no probe yet"},
	}
}

// --- /v1/users/me/plan/usage-limits JSON parsing ----------------------

// clineLimitsResponse mirrors the JSON returned by
// GET /v1/users/me/plan/usage-limits. Field names track Cline's
// camelCase payload exactly (percentUsed, resetsAt, type).
type clineLimitsResponse struct {
	Data struct {
		Limits []clineLimit `json:"limits"`
	} `json:"data"`
	Success bool `json:"success"`
}

type clineLimit struct {
	Type        string  `json:"type"`
	PercentUsed float64 `json:"percentUsed"`
	ResetsAt    string  `json:"resetsAt"`
}

// clineWindowLabels maps the API's "type" enum to our canonical labels.
// Anything outside this set is dropped (forward compatibility: a future
// "daily" or "annual" window wouldn't break parsing).
var clineWindowLabels = map[string]string{
	"five_hour": "5h",
	"weekly":    "weekly",
	"monthly":   "monthly",
}

// parseClineLimits converts the JSON body into 3-or-fewer UsageStats
// records. The order in the API response is not guaranteed, but
// FetchUsage picks the "5h" entry as the primary so the TUI's
// single-bar fallback renders the most-revealing window.
func parseClineLimits(body []byte, now time.Time) ([]types.UsageStats, error) {
	var resp clineLimitsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse /v1/users/me/plan/usage-limits: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("parse /v1/users/me/plan/usage-limits: success=false")
	}
	out := make([]types.UsageStats, 0, len(clineWindowLabels))
	// Dedup by label: if the API ever returns two entries with the
	// same type, we keep the first one so SnapshotWindows returns at
	// most one UsageStats per known label.
	seen := map[string]bool{}
	for _, lim := range resp.Data.Limits {
		label, ok := clineWindowLabels[lim.Type]
		if !ok || seen[label] {
			continue
		}
		seen[label] = true
		// Cline emits RFC3339Nano. If the field ever arrives in a
		// different layout, fall back to a zero ResetAt / ResetIn so
		// the UI still renders the % rather than crashing.
		var resetAt time.Time
		if lim.ResetsAt != "" {
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
				if t, err := time.Parse(layout, lim.ResetsAt); err == nil {
					resetAt = t
					break
				}
			}
		}
		var resetIn time.Duration
		if !resetAt.IsZero() {
			resetIn = time.Until(resetAt)
			// Clamp: clock skew or a freshly-expired window can
			// produce a negative duration, which would render as
			// "-3h" in the TUI. We prefer "now" until the next
			// successful probe replaces this entry.
			if resetIn < 0 {
				resetIn = 0
			}
		}
		used := lim.PercentUsed
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		out = append(out, types.UsageStats{
			Used:        used,
			Total:       100,
			Unit:        types.UnitCount,
			WindowLabel: label,
			Note:        fmt.Sprintf("rolling %s", label),
			ResetAt:     resetAt,
			ResetIn:     resetIn,
			LastProbeAt: now,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("parse /v1/users/me/plan/usage-limits: no recognised window (response had %d limits)", len(resp.Data.Limits))
	}
	return out, nil
}

// snippet shortens a response body for inclusion in error messages so
// we don't dump multi-kilobyte JSON into the TUI.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// Compile-time check that clinepass.Provider implements the optional
// multi-window capability (used by providers.EnrichWindows).
var _ providersMultiWindowProvider = (*Provider)(nil)

// providersMultiWindowProvider mirrors the upstream signature; declared
// here as a local alias so the compile-time check below has a stable
// type without dragging the providers package in (which would cycle).
type providersMultiWindowProvider interface {
	SnapshotWindows() []types.UsageStats
}

// Package freebuff implements the read-only Freebuff session usage probe.
package freebuff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/auth"
	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/probe"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

const sessionPath = "/api/v1/freebuff/session"

// Provider implements types.Provider for Freebuff.
type Provider struct {
	a     *auth.Finder
	probe *probe.Client
	cfg   *config.Config
	hc    *http.Client

	// endpointOverride is useful for tests and compatible staging endpoints.
	// The request path remains the read-only session endpoint.
	endpointOverride string
}

// New returns a Freebuff provider.
func New() *Provider {
	return &Provider{
		hc:    &http.Client{Timeout: 8 * time.Second},
		probe: probe.New(),
	}
}

// NewWith wires up explicit dependencies.
func NewWith(a *auth.Finder, c *config.Config) *Provider {
	root := "https://www.codebuff.com"
	if c != nil {
		if ep, ok := c.Providers["freebuff"]; ok && ep.Endpoint != "" {
			root = strings.TrimRight(ep.Endpoint, "/")
		}
	}
	return &Provider{
		a:                a,
		cfg:              c,
		hc:               &http.Client{Timeout: 8 * time.Second},
		probe:            probe.New(),
		endpointOverride: root,
	}
}

func (p *Provider) Name() string        { return "freebuff" }
func (p *Provider) DisplayName() string { return "Freebuff" }
func (p *Provider) Icon() string        { return "" }

// Keep this list aligned with the model selector exposed by the Freebuff CLI.
// API quota snapshots can contain compatibility, referral, or experimental
// keys; those keys are deliberately not treated as user-facing models.
var freebuffPremiumCatalog = []types.FreeModel{
	{ID: "deepseek/deepseek-v4-pro", Label: "DeepSeek V4 Pro", Premium: true},
	{ID: "minimax/minimax-m3", Label: "MiniMax M3", Premium: true},
	{ID: "openai/gpt-5.6-luna", Label: "GPT-5.6 Luna", Premium: true},
	{ID: "mimo/mimo-v2.5-pro", Label: "MiMo 2.5 Pro", Premium: true},
}

var freebuffStandardCatalog = []types.FreeModel{
	{ID: "deepseek/deepseek-v4-flash", Label: "DeepSeek V4 Flash"},
	{ID: "mimo/mimo-v2.5", Label: "MiMo 2.5"},
}

// AvailableModels returns the static catalog shown by the Freebuff CLI.
func (p *Provider) AvailableModels() []types.FreeModel {
	out := make([]types.FreeModel, 0, len(freebuffPremiumCatalog)+len(freebuffStandardCatalog))
	out = append(out, freebuffPremiumCatalog...)
	out = append(out, freebuffStandardCatalog...)
	return out
}

// IsConfigured returns nil iff a usable Freebuff bearer token is available.
func (p *Provider) IsConfigured() error {
	if p.cfg != nil {
		if k, _, _, ok := p.cfg.Override("freebuff"); ok && strings.TrimSpace(k) != "" {
			return nil
		}
	}
	a := p.a
	if a == nil {
		var err error
		a, err = auth.NewFinder()
		if err != nil {
			return err
		}
		p.a = a
	}
	cred, err := a.FreebuffCredentials()
	if err != nil || strings.TrimSpace(cred.Token) == "" {
		if err == nil {
			err = fmt.Errorf("empty auth token")
		}
		return fmt.Errorf("freebuff: %w (run `freebuff login` once to authenticate)", err)
	}
	return nil
}

// FetchUsage reads the authenticated session snapshot. The GET endpoint does
// not create, claim, end, or mutate a session; only the Freebuff CLI's POST
// path claims the single session slot.
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

	stats, err := p.fetchSession(ctx, a)
	if err != nil {
		return offlineStats(a, err.Error()), nil
	}
	return stats, nil
}

func (p *Provider) endpointURL() string {
	root := strings.TrimRight(p.endpointOverride, "/")
	if root == "" {
		root = "https://www.codebuff.com"
	}
	return root + sessionPath
}

func (p *Provider) fetchSession(ctx context.Context, a *auth.Finder) (*types.UsageStats, error) {
	token := p.resolveToken(a)
	if token == "" {
		return offlineStats(a, "no Freebuff bearer token"), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpointURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if p.probe != nil && p.probe.UserAgent != "" {
		req.Header.Set("User-Agent", p.probe.UserAgent)
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if readErr != nil {
		return nil, fmt.Errorf("freebuff session: read response: %w", readErr)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("freebuff session: HTTP %d: %s", resp.StatusCode, upstreamMessage(body))
	}

	env, err := parseSessionEnvelope(body)
	if err != nil {
		return nil, fmt.Errorf("freebuff session: parse: %w", err)
	}
	if env.ErrorStat != "" {
		return nil, fmt.Errorf("freebuff session: %s", nonEmpty(env.Message, env.ErrorStat))
	}
	return renderSessionStats(env)
}

// sessionEnvelope mirrors the stable fields used by the official client. The
// optional entitlementBreakdown is retained for server snapshots that expose
// per-entitlement detail in addition to the shared model quota.
type sessionEnvelope struct {
	Status       string                      `json:"status"`
	AccessTier   string                      `json:"accessTier"`
	RateLimits   map[string]sessionRateLimit `json:"rateLimitsByModel"`
	RateLimit    *sessionRateLimit           `json:"rateLimit"`
	Model        string                      `json:"model"`
	RemainingMs  float64                     `json:"remainingMs"`
	CurrentModel string                      `json:"currentModel"`
	CountryCode  string                      `json:"countryCode"`
	CountryBlock string                      `json:"countryBlockReason"`
	Referral     json.RawMessage             `json:"referral"`
	ErrorStat    string                      `json:"error"`
	Message      string                      `json:"message"`
}

type sessionRateLimit struct {
	Model                string                     `json:"model"`
	Limit                float64                    `json:"limit"`
	RecentCount          float64                    `json:"recentCount"`
	ResetAt              string                     `json:"resetAt"`
	ResetTimeZone        string                     `json:"resetTimeZone"`
	Period               string                     `json:"period"`
	WindowHours          float64                    `json:"windowHours"`
	EntitlementBreakdown map[string]json.RawMessage `json:"entitlementBreakdown"`
}

func parseSessionEnvelope(raw []byte) (sessionEnvelope, error) {
	var env sessionEnvelope
	if len(strings.TrimSpace(string(raw))) == 0 {
		return env, fmt.Errorf("empty body")
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, err
	}
	return env, nil
}

func renderSessionStats(env sessionEnvelope) (*types.UsageStats, error) {
	tier := strings.ToLower(strings.TrimSpace(env.AccessTier))
	if tier != "full" && tier != "limited" {
		return nil, fmt.Errorf("missing or unknown accessTier %q", env.AccessTier)
	}

	quota, err := chooseQuota(env, tier)
	if err != nil {
		return nil, err
	}
	if quota.Limit <= 0 || quota.RecentCount < 0 {
		return nil, fmt.Errorf("invalid quota for %s: limit=%v recentCount=%v", tier, quota.Limit, quota.RecentCount)
	}
	reset, err := parseResetAt(quota.ResetAt)
	if err != nil {
		return nil, fmt.Errorf("invalid quota resetAt %q: %w", quota.ResetAt, err)
	}

	now := time.Now()
	stats := &types.UsageStats{
		Used:        quota.RecentCount,
		Total:       quota.Limit,
		Unit:        types.UnitCount,
		WindowLabel: quotaWindowLabel(quota.Period, quota.ResetTimeZone),
		ResetAt:     reset,
		LastProbeAt: now,
	}

	status := strings.TrimSpace(env.Status)
	note := []string{tierLabel(tier) + " session quota"}
	if status != "" {
		note = append(note, status)
	}
	if status == "active" && env.RemainingMs > 0 {
		// remainingMs describes the active session's lifetime, not the daily
		// session quota represented by Used/Total/ResetAt.
		note = append(note, fmt.Sprintf("active session %.0fm remaining", env.RemainingMs/60000))
	}
	if env.Referral != nil && len(env.Referral) > 0 && string(env.Referral) != "null" {
		note = append(note, "referral quota is separate")
	}
	stats.Note = strings.Join(note, " · ")
	return stats, nil
}

func chooseQuota(env sessionEnvelope, tier string) (sessionRateLimit, error) {
	if env.RateLimit != nil && validRateLimit(*env.RateLimit) {
		return quotaForTier(*env.RateLimit, tier)
	}

	preferred := freebuffStandardIDs
	if tier == "full" {
		preferred = freebuffPremiumIDs
	}
	for _, id := range preferred {
		if q, ok := env.RateLimits[id]; ok && validRateLimit(q) {
			return quotaForTier(q, tier)
		}
	}

	// The server may key an entry by an alias while still including its model.
	for key, q := range env.RateLimits {
		if !validRateLimit(q) {
			continue
		}
		if isPreferredModel(key, q.Model, preferred) || strings.EqualFold(key, tier) || strings.EqualFold(key, "general") || strings.EqualFold(key, "limited") {
			return quotaForTier(q, tier)
		}
	}
	return sessionRateLimit{}, fmt.Errorf("missing rateLimitsByModel quota for %s access tier", tier)
}

func isPreferredModel(key, model string, preferred []string) bool {
	for _, id := range preferred {
		if key == id || model == id {
			return true
		}
	}
	return false
}

func quotaForTier(q sessionRateLimit, tier string) (sessionRateLimit, error) {
	if len(q.EntitlementBreakdown) == 0 {
		return q, nil
	}
	keys := []string{tier}
	if tier == "full" {
		keys = append(keys, "premium")
	} else {
		keys = append(keys, "standard", "general")
	}
	for _, key := range keys {
		raw, ok := q.EntitlementBreakdown[key]
		if !ok {
			continue
		}
		var detail sessionRateLimit
		if err := json.Unmarshal(raw, &detail); err != nil {
			return sessionRateLimit{}, fmt.Errorf("invalid entitlementBreakdown.%s: %w", key, err)
		}
		if validRateLimit(detail) {
			if detail.Model == "" {
				detail.Model = q.Model
			}
			if detail.Period == "" {
				detail.Period = q.Period
			}
			if detail.ResetTimeZone == "" {
				detail.ResetTimeZone = q.ResetTimeZone
			}
			return detail, nil
		}
	}
	return q, nil
}

func validRateLimit(q sessionRateLimit) bool {
	return q.Limit > 0 && q.RecentCount >= 0
}

var freebuffPremiumIDs = []string{
	"deepseek/deepseek-v4-pro",
	"minimax/minimax-m3",
	"openai/gpt-5.6-luna",
	"mimo/mimo-v2.5-pro",
}

var freebuffStandardIDs = []string{
	"deepseek/deepseek-v4-flash",
	"mimo/mimo-v2.5",
}

func quotaWindowLabel(period, zone string) string {
	period = strings.ToLower(strings.TrimSpace(period))
	zone = strings.TrimSpace(zone)
	if period == "pacific_day" || period == "daily" {
		return "daily (Pacific)"
	}
	if period == "pacific_week" || period == "weekly" {
		return "weekly (Pacific)"
	}
	if period != "" {
		return period
	}
	if zone != "" {
		return "daily (" + zone + ")"
	}
	return "daily"
}

func tierLabel(tier string) string {
	if tier == "full" {
		return "Premium"
	}
	return "Standard"
}

func parseResetAt(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("missing resetAt")
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("expected RFC3339 timestamp")
}

func (p *Provider) resolveToken(a *auth.Finder) string {
	if p.cfg != nil {
		if key, _, _, ok := p.cfg.Override("freebuff"); ok && strings.TrimSpace(key) != "" {
			return strings.TrimSpace(key)
		}
	}
	cred, err := a.FreebuffCredentials()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cred.Token)
}

// offlineReminder is appended to every degraded/offline snapshot so the user
// is reminded (a) that Freebuff and the usage display will not work while a
// VPN is active, and (b) that after a restart usage only appears once a
// premium session has been run and at least one message has been sent.
const offlineReminder = "Freebuff and this usage display will not work with a VPN. After a restart, run a premium session and send at least one message for usage to appear here."

func offlineStats(a *auth.Finder, reason string) *types.UsageStats {
	errText := reason
	if errText == "" {
		errText = "no live Freebuff session quota"
	}
	errText += " · " + offlineReminder

	note := "no live Freebuff session quota"
	if reason != "" {
		note += " (" + reason + ")"
	}
	note += " · " + offlineReminder
	if a != nil {
		if name, email, ok := a.FreebuffAccount(); ok {
			identity := name
			if email != "" {
				identity += " <" + email + ">"
			}
			note += " · account: " + identity
		}
	}
	return &types.UsageStats{
		Unit:        types.UnitCount,
		WindowLabel: "daily (Pacific)",
		LastProbeAt: time.Now(),
		Note:        note + " · showing the static model catalog",
		Error:       errText,
	}
}

func upstreamMessage(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func nonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

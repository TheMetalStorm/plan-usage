package freebuff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/auth"
)

const testResetAt = "2026-07-31T07:00:00-07:00"

func TestFetchUsageUsesReadOnlyBearerGET(t *testing.T) {
	p := NewWith(testFinder(t), nil)
	p.hc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		for _, header := range []string{"x-freebuff-model", "x-freebuff-instance-id"} {
			if got := r.Header.Get(header); got != "" {
				t.Errorf("unexpected %s header %q", header, got)
			}
		}
		return jsonResponse(http.StatusOK, `{"status":"none","accessTier":"full","rateLimitsByModel":{"deepseek/deepseek-v4-pro":{"model":"deepseek/deepseek-v4-pro","limit":6,"recentCount":3.6,"resetAt":"`+testResetAt+`","period":"pacific_day"}}}`), nil
	})}
	stats, err := p.FetchUsage(context.Background())
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if stats.Used != 3.6 || stats.Total != 6 {
		t.Fatalf("usage = %v/%v, want 3.6/6", stats.Used, stats.Total)
	}
	if got := stats.ResetAt.Format(time.RFC3339); got != testResetAt {
		t.Fatalf("ResetAt = %q, want %q", got, testResetAt)
	}
}

func TestSessionStatusesUseDailyQuota(t *testing.T) {
	for _, status := range []string{"none", "active", "ended"} {
		t.Run(status, func(t *testing.T) {
			p := NewWith(testFinder(t), nil)
			p.hc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, fmt.Sprintf(`{"status":%q,"accessTier":"full","remainingMs":3600000,"rateLimitsByModel":{"minimax/minimax-m3":{"model":"minimax/minimax-m3","limit":6,"recentCount":3.6,"resetAt":"%s","period":"pacific_day"}}}`, status, testResetAt)), nil
			})}
			stats, err := p.FetchUsage(context.Background())
			if err != nil {
				t.Fatalf("FetchUsage() error = %v", err)
			}
			if stats.Used != 3.6 || stats.Total != 6 || stats.ResetAt.IsZero() {
				t.Fatalf("stats = %#v, want shared daily quota", stats)
			}
			if status == "active" && !strings.Contains(stats.Note, "remaining") {
				t.Fatalf("active note = %q, want session duration note", stats.Note)
			}
		})
	}
}

func TestKnownQuotaKeyPreferredAndReplicasNotAdded(t *testing.T) {
	q := sessionEnvelope{
		AccessTier: "full",
		RateLimits: map[string]sessionRateLimit{
			"hidden/internal-test":     {Model: "hidden/internal-test", Limit: 99, RecentCount: 90, ResetAt: testResetAt},
			"deepseek/deepseek-v4-pro": {Model: "deepseek/deepseek-v4-pro", Limit: 6, RecentCount: 3.6, ResetAt: testResetAt},
			"mimo/mimo-v2.5-pro":       {Model: "mimo/mimo-v2.5-pro", Limit: 6, RecentCount: 3.6, ResetAt: testResetAt},
		},
	}
	stats, err := renderSessionStats(q)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Used != 3.6 || stats.Total != 6 {
		t.Fatalf("usage = %v/%v, want one shared 3.6/6 quota", stats.Used, stats.Total)
	}
}

func TestLimitedTierUsesStandardQuota(t *testing.T) {
	q := sessionEnvelope{
		AccessTier: "limited",
		RateLimits: map[string]sessionRateLimit{
			"deepseek/deepseek-v4-flash": {Model: "deepseek/deepseek-v4-flash", Limit: 5, RecentCount: 2, ResetAt: testResetAt, Period: "pacific_day"},
			"deepseek/deepseek-v4-pro":   {Model: "deepseek/deepseek-v4-pro", Limit: 6, RecentCount: 6, ResetAt: testResetAt, Period: "pacific_day"},
		},
	}
	stats, err := renderSessionStats(q)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Used != 2 || stats.Total != 5 {
		t.Fatalf("usage = %v/%v, want limited standard 2/5 quota", stats.Used, stats.Total)
	}
}

func TestEntitlementBreakdownOverridesSharedEntry(t *testing.T) {
	q := sessionEnvelope{
		AccessTier: "full",
		RateLimits: map[string]sessionRateLimit{
			"deepseek/deepseek-v4-pro": {
				Model:       "deepseek/deepseek-v4-pro",
				Limit:       99,
				RecentCount: 90,
				ResetAt:     testResetAt,
				EntitlementBreakdown: map[string]json.RawMessage{
					"premium": json.RawMessage(`{"limit":6,"recentCount":3.6,"resetAt":"` + testResetAt + `","period":"pacific_day"}`),
				},
			},
		},
	}
	stats, err := renderSessionStats(q)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Used != 3.6 || stats.Total != 6 {
		t.Fatalf("usage = %v/%v, want entitlement 3.6/6", stats.Used, stats.Total)
	}
}

func TestAvailableModelsIsVisibleCatalog(t *testing.T) {
	models := New().AvailableModels()
	if len(models) != 6 {
		t.Fatalf("model count = %d, want 6", len(models))
	}
	for _, model := range models {
		if model.ID == "moonshotai/kimi-k2.7-code" || strings.Contains(strings.ToLower(model.Label), "glm") || strings.Contains(strings.ToLower(model.Label), "test") {
			t.Fatalf("hidden model surfaced: %#v", model)
		}
	}
	for i := 0; i < 4; i++ {
		if !models[i].Premium {
			t.Fatalf("model %q is not marked Premium", models[i].Label)
		}
	}
	for i := 4; i < len(models); i++ {
		if models[i].Premium {
			t.Fatalf("standard model %q marked Premium", models[i].Label)
		}
	}
}

func TestSessionErrorsDegradeWithClearReason(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "invalid json", status: http.StatusOK, body: "{", want: "parse"},
		{name: "missing quota", status: http.StatusOK, body: `{"status":"none","accessTier":"full"}`, want: "missing rateLimitsByModel"},
		{name: "unknown quota model", status: http.StatusOK, body: `{"status":"none","accessTier":"full","rateLimitsByModel":{"internal/test":{"model":"internal/test","limit":6,"recentCount":1,"resetAt":"` + testResetAt + `"}}}`, want: "missing rateLimitsByModel"},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, want: "HTTP 401"},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":"forbidden"}`, want: "HTTP 403"},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`, want: "HTTP 429"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := NewWith(testFinder(t), nil)
			p.hc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(test.status, test.body), nil
			})}
			stats, err := p.FetchUsage(context.Background())
			if err != nil {
				t.Fatalf("FetchUsage() error = %v", err)
			}
			if !strings.Contains(stats.Error, test.want) {
				t.Fatalf("Error = %q, want %q", stats.Error, test.want)
			}
		})
	}
}

func TestFetchUsageTimeoutDegrades(t *testing.T) {
	p := NewWith(testFinder(t), nil)
	p.hc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	stats, err := p.FetchUsage(ctx)
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if stats.Error == "" {
		t.Fatal("timeout returned no degradation reason")
	}
}

func testFinder(t *testing.T) *auth.Finder {
	t.Helper()
	root := t.TempDir()
	f := &auth.Finder{Home: root, XDG: filepath.Join(root, "config"), Data: filepath.Join(root, "data")}
	path := f.FreebuffCredentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"default":{"authToken":"test-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestNetworkFailureAddsReminder(t *testing.T) {
	p := NewWith(testFinder(t), nil)
	p.hc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: lookup www.codebuff.com: no such host")
	})}
	stats, err := p.FetchUsage(context.Background())
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if !strings.Contains(stats.Error, "no such host") {
		t.Fatalf("Error = %q, want the underlying network reason", stats.Error)
	}
	for _, want := range []string{"will not work with a VPN", "run a premium session", "at least one message"} {
		if !strings.Contains(stats.Error, want) {
			t.Fatalf("Error = %q, want the offline reminder fragment %q", stats.Error, want)
		}
	}
}

func TestAuthFailureStillShowsReminder(t *testing.T) {
	p := NewWith(testFinder(t), nil)
	p.hc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"error":"unauthorized"}`), nil
	})}
	stats, err := p.FetchUsage(context.Background())
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if !strings.Contains(stats.Error, "HTTP 401") {
		t.Fatalf("Error = %q, want the HTTP 401 reason", stats.Error)
	}
	// The offline reminder (VPN + after-restart tip) is shown on every
	// degraded snapshot, auth failures included.
	for _, want := range []string{"will not work with a VPN", "run a premium session", "at least one message"} {
		if !strings.Contains(stats.Error, want) {
			t.Fatalf("Error = %q, want the offline reminder fragment %q", stats.Error, want)
		}
	}
}

func TestOfflineStatsAlwaysIncludesReminder(t *testing.T) {
	reasons := []string{
		"dial tcp: connection refused",
		"freebuff session: HTTP 401: unauthorized",
		"freebuff session: parse: invalid character",
		"no Freebuff bearer token",
	}
	for _, reason := range reasons {
		stats := offlineStats(nil, reason)
		if !strings.Contains(stats.Error, reason) {
			t.Errorf("reason %q: Error = %q, want the reason preserved", reason, stats.Error)
		}
		for _, want := range []string{"will not work with a VPN", "run a premium session", "at least one message"} {
			if !strings.Contains(stats.Error, want) {
				t.Errorf("reason %q: Error = %q, want reminder fragment %q", reason, stats.Error, want)
			}
			if !strings.Contains(stats.Note, want) {
				t.Errorf("reason %q: Note = %q, want reminder fragment %q", reason, stats.Note, want)
			}
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

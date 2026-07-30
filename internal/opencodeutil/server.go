package opencodeutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Server function IDs (SHA-256 hashes published by opencode.ai).
const (
	FuncWorkspaces      = "def39973159c7f0483d8793a822b8dbb10d067e12c65455fcb4608459ba0234f"
	FuncSubscriptionGet = "7abeebee372f304e050aaaf92be863f4a86490e382f8c79db68fd94040d691b4"
	serverBase          = "https://opencode.ai/_server"
	serverOrigin        = "https://opencode.ai"
	chromeUA            = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
)

// ServerUsage is the workspace usage returned by the _server endpoint.
type ServerUsage struct {
	RollingPercent float64
	RollingReset   int64 // seconds
	WeeklyPercent  float64
	WeeklyReset    int64 // seconds
	HasWeekly      bool
	UpdatedAt      time.Time
}

// ServerClient talks to the opencode.ai /_server RPC endpoint.
type ServerClient struct {
	hc      *http.Client
	cookies *CookieCache
	apiKey  string // optional Bearer token from auth.json
}

// NewServerClient creates a client that will try API-key Bearer auth first,
// then fall back to cookie cache.
func NewServerClient(apiKey string) *ServerClient {
	return &ServerClient{
		hc: &http.Client{
			Timeout: 10 * time.Second,
		},
		cookies: nil,
		apiKey:  apiKey,
	}
}

// SetCookieCache attaches a cookie cache for session-based auth.
func (s *ServerClient) SetCookieCache(cc *CookieCache) {
	s.cookies = cc
}

// FetchUsage calls the workspace and subscription functions and returns
// the combined usage data. If a workspace ID is provided (from env var
// or config), it skips workspace resolution and uses it directly.
func (s *ServerClient) FetchUsage(ctx context.Context, workspaceID string) (*ServerUsage, error) {
	// 1. If no explicit workspace ID, try workspace lookup.
	if workspaceID == "" {
		w, err := s.callWorkspace(ctx)
		if err != nil {
			return nil, fmt.Errorf("workspace lookup: %w", err)
		}
		workspaceID = w
	}
	if workspaceID == "" {
		return nil, fmt.Errorf("no workspace ID available")
	}

	// Strip full URL down to raw ID if needed.
	workspaceID = normalizeWorkspaceID(workspaceID)

	// 2. Fetch subscription/usage for this workspace.
	usage, err := s.callSubscription(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("subscription.get: %w", err)
	}
	return usage, nil
}

// -- Workspace lookup --

// callWorkspace resolves the user's workspace ID from the _server endpoint.
// Uses GET first, falls back to POST.
func (s *ServerClient) callWorkspace(ctx context.Context) (string, error) {
	// Try GET first.
	text, err := s.fetchServerText(ctx, serverRequest{
		serverID: FuncWorkspaces,
		args:     nil,
		method:   "GET",
		referer:  serverOrigin,
	})
	if err != nil {
		return "", err
	}

	ids := parseWorkspaceIDs(text)
	if len(ids) > 0 {
		return ids[0], nil
	}

	// Fall back to POST.
	text, err = s.fetchServerText(ctx, serverRequest{
		serverID: FuncWorkspaces,
		args:     []any{},
		method:   "POST",
		referer:  serverOrigin,
	})
	if err != nil {
		return "", err
	}

	ids = parseWorkspaceIDs(text)
	if len(ids) > 0 {
		return ids[0], nil
	}

	return "", fmt.Errorf("workspace ID not found in response: %s", trimBody([]byte(text)))
}

// -- Subscription lookup --

// callSubscription fetches usage data for a given workspace.
// Uses GET first, falls back to POST.
func (s *ServerClient) callSubscription(ctx context.Context, workspaceID string) (*ServerUsage, error) {
	billingRef := fmt.Sprintf("https://opencode.ai/workspace/%s/billing", workspaceID)

	text, err := s.fetchServerText(ctx, serverRequest{
		serverID: FuncSubscriptionGet,
		args:     []any{workspaceID},
		method:   "GET",
		referer:  billingRef,
	})
	if err != nil {
		return nil, err
	}

	// Check for explicit null payload.
	if isExplicitNull(text) {
		return nil, fmt.Errorf("subscription returned null for workspace %s", workspaceID)
	}

	// Try to parse usage.
	usage := parseSubscriptionText(text)
	if usage != nil {
		return usage, nil
	}

	// Fall back to POST.
	text, err = s.fetchServerText(ctx, serverRequest{
		serverID: FuncSubscriptionGet,
		args:     []any{workspaceID},
		method:   "POST",
		referer:  billingRef,
	})
	if err != nil {
		return nil, err
	}

	if isExplicitNull(text) {
		return nil, fmt.Errorf("subscription returned null for workspace %s (POST)", workspaceID)
	}

	usage = parseSubscriptionText(text)
	if usage != nil {
		return usage, nil
	}

	return nil, fmt.Errorf("could not parse subscription response: %s", trimBody([]byte(text)))
}

// -- HTTP request --

type serverRequest struct {
	serverID string
	args     []any
	method   string
	referer  string
}

// fetchServerText sends a request to the _server endpoint and returns the
// response body as text. Handles auth (Bearer token or cookie headers).
func (s *ServerClient) fetchServerText(ctx context.Context, req serverRequest) (string, error) {
	urlStr, bodyBytes := s.buildRequest(req)

	httpReq, err := http.NewRequestWithContext(ctx, req.method, urlStr, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}

	// Set headers matching the CodexBar implementation.
	httpReq.Header.Set("User-Agent", chromeUA)
	httpReq.Header.Set("Accept", "text/javascript, application/json;q=0.9, */*;q=0.8")
	httpReq.Header.Set("Origin", serverOrigin)
	httpReq.Header.Set("Referer", req.referer)
	httpReq.Header.Set("X-Server-Id", req.serverID)
	httpReq.Header.Set("X-Server-Instance", fmt.Sprintf("server-fn:%d", time.Now().UnixNano()))

	// Auth: try Bearer token first, then cookie.
	if s.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	if s.cookies != nil {
		s.cookies.AttachToRequest(httpReq)
	}

	if req.method != "GET" && len(bodyBytes) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.hc.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		errMsg := extractServerError(body)
		if errMsg != "" {
			return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, errMsg)
		}
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return string(body), nil
}

// buildRequest constructs the URL and optional body for a server request.
func (s *ServerClient) buildRequest(req serverRequest) (url string, body []byte) {
	if req.method == "GET" {
		// GET: ?id=<serverID>&args=<json-encoded-args>
		u := serverBase + "?id=" + req.serverID
		if len(req.args) > 0 {
			argsJSON, _ := json.Marshal(req.args)
			u += "&args=" + string(argsJSON)
		}
		return u, nil
	}

	// POST: body is the JSON-encoded args array
	body, _ = json.Marshal(req.args)
	return serverBase, body
}

// -- Response parsing --

var (
	jsWrapperRx = regexp.MustCompile(`(?s)^/\*[-!].*?\*/\s*`)
	jsonObjRx   = regexp.MustCompile(`(?s)(\{.*\})`)
)

// Workspace ID regex: matches `id: "wrk_..."` in JS object literals.
var workspaceIDRx = regexp.MustCompile(`id\s*:\s*"(wrk_[^"]+)"`)

// Usage field extraction regexes (JS object literal fallback).
var (
	rollingPercentRx = regexp.MustCompile(`rollingUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`)
	rollingResetRx   = regexp.MustCompile(`rollingUsage[^}]*?resetInSec\s*:\s*([0-9]+)`)
	weeklyPercentRx  = regexp.MustCompile(`weeklyUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`)
	weeklyResetRx    = regexp.MustCompile(`weeklyUsage[^}]*?resetInSec\s*:\s*([0-9]+)`)
)

// parseWorkspaceIDs extracts workspace IDs from a JS object-literal response.
func parseWorkspaceIDs(text string) []string {
	matches := workspaceIDRx.FindAllStringSubmatch(text, -1)
	var ids []string
	for _, m := range matches {
		if len(m) >= 2 {
			ids = append(ids, m[1])
		}
	}
	return ids
}

// parseSubscriptionText parses a subscription response into ServerUsage.
// Tries JSON first, then regex fallback.
func parseSubscriptionText(text string) *ServerUsage {
	now := time.Now()

	// Try JSON parsing first.
	if usage := parseSubscriptionJSON(text, now); usage != nil {
		return usage
	}

	// Fall back to regex extraction.
	return parseSubscriptionRegex(text, now)
}

// parseSubscriptionJSON attempts to parse the response as JSON.
func parseSubscriptionJSON(text string, now time.Time) *ServerUsage {
	text = strings.TrimSpace(text)
	// Strip JS comment wrappers.
	text = jsWrapperRx.ReplaceAllString(text, "")
	// Find JSON object.
	m := jsonObjRx.FindStringSubmatch(text)
	if len(m) < 2 {
		return nil
	}
	jsonStr := m[1]

	var data any
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return nil
	}

	return extractUsageFromObject(data, now)
}

// extractUsageFromObject walks a decoded JSON structure looking for
// rollingUsage and weeklyUsage with usagePercent and resetInSec.
func extractUsageFromObject(obj any, now time.Time) *ServerUsage {
	dict, ok := obj.(map[string]any)
	if !ok {
		return nil
	}

	// Check direct "data"/"result"/"usage"/"billing"/"payload" wrappers.
	for _, key := range []string{"data", "result", "usage", "billing", "payload"} {
		if nested, ok := dict[key].(map[string]any); ok {
			if usage := extractFromDict(nested, now); usage != nil {
				return usage
			}
		}
	}

	return extractFromDict(dict, now)
}

// extractFromDict extracts usage from a flat or nested dict.
func extractFromDict(dict map[string]any, now time.Time) *ServerUsage {
	// Check for nested "usage" key.
	if usage, ok := dict["usage"].(map[string]any); ok {
		return extractFromDict(usage, now)
	}

	// Try direct rollingUsage/weeklyUsage keys.
	rollingKeys := []string{"rollingUsage", "rolling", "rolling_usage", "rollingWindow", "rolling_window"}
	weeklyKeys := []string{"weeklyUsage", "weekly", "weekly_usage", "weeklyWindow", "weekly_window"}

	var rollingDict map[string]any
	for _, k := range rollingKeys {
		if d, ok := dict[k].(map[string]any); ok {
			rollingDict = d
			break
		}
	}
	var weeklyDict map[string]any
	for _, k := range weeklyKeys {
		if d, ok := dict[k].(map[string]any); ok {
			weeklyDict = d
			break
		}
	}

	if rollingDict == nil || weeklyDict == nil {
		return nil
	}

	return buildUsage(rollingDict, weeklyDict, now)
}

// buildUsage constructs ServerUsage from rolling and weekly sub-dicts.
func buildUsage(rolling, weekly map[string]any, now time.Time) *ServerUsage {
	rPct := extractPercent(rolling)
	rReset := extractResetSec(rolling)
	wPct := extractPercent(weekly)
	wReset := extractResetSec(weekly)

	if rPct == nil || rReset == nil || wPct == nil || wReset == nil {
		return nil
	}

	return &ServerUsage{
		RollingPercent: *rPct,
		RollingReset:   *rReset,
		WeeklyPercent:  *wPct,
		WeeklyReset:    *wReset,
		HasWeekly:      true,
		UpdatedAt:      now,
	}
}

// extractPercent extracts a usagePercent value from a map, trying multiple keys.
func extractPercent(dict map[string]any) *float64 {
	for _, k := range []string{"usagePercent", "usedPercent", "percentUsed", "percent", "usage_percent"} {
		if v, ok := dict[k]; ok {
			if f := toFloat64(v); f != nil {
				// If it's <= 1.0, it might be a fraction → convert to percent.
				if *f <= 1.0 && *f >= 0 {
					*f *= 100
				}
				return f
			}
		}
	}
	return nil
}

// extractResetSec extracts a resetInSec value from a map.
func extractResetSec(dict map[string]any) *int64 {
	for _, k := range []string{"resetInSec", "resetInSeconds", "resetSeconds", "reset_sec", "reset_in_sec"} {
		if v, ok := dict[k]; ok {
			if f := toFloat64(v); f != nil {
				i := int64(*f)
				return &i
			}
		}
	}
	return nil
}

// toFloat64 converts a JSON value to float64.
func toFloat64(v any) *float64 {
	switch val := v.(type) {
	case float64:
		return &val
	case int:
		f := float64(val)
		return &f
	case int64:
		f := float64(val)
		return &f
	case json.Number:
		f, err := val.Float64()
		if err != nil {
			return nil
		}
		return &f
	case string:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil
		}
		return &f
	}
	return nil
}

// parseSubscriptionRegex falls back to regex-based extraction.
func parseSubscriptionRegex(text string, now time.Time) *ServerUsage {
	rPct := extractRegexFloat(rollingPercentRx, text)
	rReset := extractRegexInt(rollingResetRx, text)
	wPct := extractRegexFloat(weeklyPercentRx, text)
	wReset := extractRegexInt(weeklyResetRx, text)

	if rPct == nil || rReset == nil {
		return nil
	}

	usage := &ServerUsage{
		RollingPercent: *rPct,
		RollingReset:   *rReset,
		UpdatedAt:      now,
	}

	if wPct != nil && wReset != nil {
		usage.WeeklyPercent = *wPct
		usage.WeeklyReset = *wReset
		usage.HasWeekly = true
	}

	return usage
}

func extractRegexFloat(rx *regexp.Regexp, text string) *float64 {
	m := rx.FindStringSubmatch(text)
	if len(m) < 2 {
		return nil
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return nil
	}
	return &f
}

func extractRegexInt(rx *regexp.Regexp, text string) *int64 {
	m := rx.FindStringSubmatch(text)
	if len(m) < 2 {
		return nil
	}
	i, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return nil
	}
	return &i
}

// isExplicitNull checks if the response text is literally "null".
func isExplicitNull(text string) bool {
	return strings.TrimSpace(text) == "null"
}

// extractServerError tries to extract an error message from a JSON error response.
func extractServerError(body []byte) string {
	var resp struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		Detail  string `json:"detail"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	switch {
	case resp.Message != "":
		return resp.Message
	case resp.Error != "":
		return resp.Error
	case resp.Detail != "":
		return resp.Detail
	}
	return ""
}

// -- Public helpers --

// ResolveWorkspaceID normalizes a workspace override and returns the raw ID.
func ResolveWorkspaceID(override string) string {
	if override == "" {
		override = os.Getenv("CODEXBAR_OPENCODE_WORKSPACE_ID")
	}
	return normalizeWorkspaceID(override)
}

func normalizeWorkspaceID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, "https://opencode.ai/workspace/") {
		parts := strings.Split(strings.TrimSuffix(id, "/"), "/")
		return parts[len(parts)-1]
	}
	return id
}

func trimBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 150 {
		return s[:150] + "…"
	}
	return s
}

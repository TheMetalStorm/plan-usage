// Package probe wraps a tiny HTTP client specialised for "send the smallest
// possible request and read x-ratelimit-* headers".
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Client is a small HTTP client suitable for cheap probes.
type Client struct {
	HC        *http.Client
	UserAgent string
}

// New returns a Client with sane defaults.
func New() *Client {
	t := &http.Transport{
		MaxIdleConns:        8,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	return &Client{
		HC:        &http.Client{Timeout: 8 * time.Second, Transport: t},
		UserAgent: "plan-usage/0.1 (+https://freebuff.com)",
	}
}

// Result is a probe response with parsed rate-limit headers.
type Result struct {
	Status    int
	Header    http.Header
	Body      []byte
	RateLimit RateLimit
}

// RateLimit is the parsed x-ratelimit-* triplet.
type RateLimit struct {
	HasData     bool
	Source      string // which headers we parsed
	ReqLimit    int64
	ReqRemain   int64
	ReqReset    time.Duration
	TokLimit    int64
	TokRemain   int64
	TokReset    time.Duration
	Group       string // rate-limit tier label, if present
	HasRequests bool
	HasTokens   bool
}

// Percent returns 0-100, 0 when no limit known.
func (r RateLimit) Percent() float64 {
	switch {
	case r.ReqLimit > 0:
		return float64(r.ReqLimit-r.ReqRemain) / float64(r.ReqLimit) * 100
	case r.TokLimit > 0:
		return float64(r.TokLimit-r.TokRemain) / float64(r.TokLimit) * 100
	default:
		return 0
	}
}

// Total / Used / WindowLabel helpers for the provider interface.
func (r RateLimit) Total() (total, used float64, unit string) {
	if r.ReqLimit > 0 {
		return float64(r.ReqLimit), float64(r.ReqLimit - r.ReqRemain), "requests"
	}
	if r.TokLimit > 0 {
		return float64(r.TokLimit), float64(r.TokLimit - r.TokRemain), "tokens"
	}
	return 0, 0, ""
}

// ChooseReset returns the smallest non-zero reset window - that's the next
// "free" moment for a rolling-window provider.
func (r RateLimit) ChooseReset() (time.Duration, string) {
	if r.ReqReset > 0 && (r.TokReset == 0 || r.ReqReset < r.TokReset) {
		return r.ReqReset, "requests"
	}
	if r.TokReset > 0 {
		return r.TokReset, "tokens"
	}
	return 0, ""
}

// parseRateLimit recognises OpenAI-style and Anthropic-style headers.
func parseRateLimit(h http.Header) (RateLimit, string) {
	// OpenAI uses x-ratelimit-limit-requests etc.
	rl := RateLimit{
		Group: h.Get("x-ratelimit-group"),
	}
	if v := h.Get("x-ratelimit-limit-requests"); v != "" {
		rl.ReqLimit, _ = strconv.ParseInt(v, 10, 64)
		rl.HasRequests = true
	}
	if v := h.Get("x-ratelimit-remaining-requests"); v != "" {
		rl.ReqRemain, _ = strconv.ParseInt(v, 10, 64)
		rl.HasRequests = true
	}
	if v := h.Get("x-ratelimit-reset-requests"); v != "" {
		rl.ReqReset, _ = parseDur(v)
		rl.HasRequests = true
	}
	if v := h.Get("x-ratelimit-limit-tokens"); v != "" {
		rl.TokLimit, _ = strconv.ParseInt(v, 10, 64)
		rl.HasTokens = true
	}
	if v := h.Get("x-ratelimit-remaining-tokens"); v != "" {
		rl.TokRemain, _ = strconv.ParseInt(v, 10, 64)
		rl.HasTokens = true
	}
	if v := h.Get("x-ratelimit-reset-tokens"); v != "" {
		rl.TokReset, _ = parseDur(v)
		rl.HasTokens = true
	}
	if rl.HasRequests || rl.HasTokens {
		rl.HasData = true
		return rl, "openai"
	}

	// Anthropic uses x-ratelimit-* (no -requests/-tokens suffix).
	if v := h.Get("anthropic-ratelimit-tokens-limit"); v != "" {
		rl.TokLimit, _ = strconv.ParseInt(v, 10, 64)
		rl.HasTokens = true
	}
	if v := h.Get("anthropic-ratelimit-tokens-remaining"); v != "" {
		rl.TokRemain, _ = strconv.ParseInt(v, 10, 64)
		rl.HasTokens = true
	}
	if v := h.Get("anthropic-ratelimit-tokens-reset"); v != "" {
		rl.TokReset, _ = parseDur(v)
		rl.HasTokens = true
	}
	if rl.HasTokens {
		rl.HasData = true
		return rl, "anthropic"
	}
	return rl, ""
}

// parseDur parses OpenAI's "1s"/"12ms"/"6m0s" and Anthropic's
// ISO-ish "2024-01-01T00:00:00Z".
func parseDur(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return time.Until(t), nil
	}
	return 0, fmt.Errorf("unparseable duration: %q", s)
}

// ChatRequest is a minimal OpenAI-compatible chat probe body.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Stream      bool      `json:"stream"`
	Temperature float64   `json:"temperature"`
}

// Message is a minimal OpenAI message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// NewChatBody builds a no-op chat-completions request body designed to
// consume the smallest possible amount of quota.
func NewChatBody(model string, maxTokens int) []byte {
	if maxTokens == 0 {
		maxTokens = 1
	}
	body := ChatRequest{
		Model:       model,
		MaxTokens:   maxTokens,
		Stream:      false,
		Temperature: 0,
		Messages:    []Message{{Role: "user", Content: "."}},
	}
	raw, _ := json.Marshal(body)
	return raw
}

// NewPOST returns a *http.Request with sensible defaults.
func NewPOST(ctx context.Context, url string, headers map[string]string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c := New(); c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body == nil {
		req.Body = http.NoBody
		req.ContentLength = 0
	}
	return req, nil
}

// NewGET returns a *http.Request with method=GET and the standard probe
// User-Agent. Used for OAuth-style GETs (e.g. wham/usage) where there's
// no body.
func NewGET(ctx context.Context, url string, headers map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c := New(); c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// Do executes the request and returns the (possibly errored) response plus
// parsed rate-limit headers.
func (c *Client) Do(ctx context.Context, req *http.Request) (*Result, error) {
	resp, err := c.HC.Do(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	defer resp.Body.Close()
	// Drain body (small) to allow connection re-use.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	rl, _ := parseRateLimit(resp.Header)
	return &Result{
		Status:    resp.StatusCode,
		Header:    resp.Header,
		Body:      body,
		RateLimit: rl,
	}, nil
}

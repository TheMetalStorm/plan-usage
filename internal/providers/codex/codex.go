// Package codex implements the types.Provider interface for OpenAI Codex
// CLI / ChatGPT subscription usage.
//
// The preferred path is the local Codex CLI app-server JSON-RPC when the
// codex binary is on PATH and the user is signed in. This avoids burning
// tokens on probe requests and returns structured multi-window rate-limit
// data straight from Codex's own source. We fall back to the legacy
// `~/.codex/auth.json` + small api.openai.com probe when the CLI is
// missing, not logged in, or rejects us.
//
// Strategy 1 (preferred): spawn `codex -s read-only -a untrusted app-server`,
// speak newline-delimited JSON-RPC over stdin/stdout, call initialize,
// account/read, and account/rateLimits/read.  Each call has a tight per-
// method timeout (5s for initialize, 3s otherwise); we kill the child on
// timeout so the daemon doesn't get wedged waiting for stdout.
//
// Strategy 2 (fallback): read bearer from `~/.codex/auth.json` and send a
// `max_tokens:1` probe to https://api.openai.com/v1/chat/completions.
// Parse the `x-ratelimit-*` headers (OpenAI-style).
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/TheMetalStorm/provider-usage/internal/auth"
	"github.com/TheMetalStorm/provider-usage/internal/config"
	"github.com/TheMetalStorm/provider-usage/internal/probe"
	"github.com/TheMetalStorm/provider-usage/internal/types"
)

// Provider implements types.Provider for Codex.
type Provider struct {
	a     *auth.Finder
	probe *probe.Client
	cfg   *config.Config

	// lastWindows caches the windows from the most recent CLI fetch so
	// providers.EnrichWindows can populate Snapshot.Windows for the TUI.
	mu      sync.Mutex
	lastWin []types.UsageStats
	// cliFailAt tracks the last time the codex CLI refused to give us
	// usable data. Used to skip spawning for `cliCooldown` afterwards.
	cliFailAt time.Time
	// cliFailShort is set alongside cliFailAt when the failure was a
	// recoverable auth error (server-side token_invalidated). It picks
	// the shorter `cliCooldownShort` so a user who runs `codex login`
	// after seeing the friendly error doesn't have to wait the full
	// 30 minutes for the daemon to retry.
	cliFailShort bool
}

// New returns a Provider with a probe client.
func New() *Provider { return &Provider{probe: probe.New()} }

// NewWith builds the provider with explicit deps (used by the registry).
func NewWith(a *auth.Finder, c *config.Config) *Provider {
	return &Provider{a: a, probe: probe.New(), cfg: c}
}

func (p *Provider) Name() string        { return "codex" }
func (p *Provider) DisplayName() string { return "Codex / ChatGPT" }
func (p *Provider) Icon() string        { return "" }

// codexFreeModels is the static list of OpenAI's known free / included models
// surfaced in the UI even when probes are offline.
var codexFreeModels = []types.FreeModel{
	{ID: "gpt-4.1-mini", Label: "GPT-4.1 mini"},
	{ID: "gpt-4o-mini", Label: "GPT-4o mini"},
	{ID: "o4-mini", Label: "o4-mini"},
	{ID: "gpt-5-mini", Label: "GPT-5 mini"},
}

func (p *Provider) AvailableModels() []types.FreeModel { return codexFreeModels }

// SnapshotWindows implements providers.MultiWindowProvider. The first
// entry is the "primary" window surfaced via UsageStats by FetchUsage;
// the rest populate Snapshot.Windows for the TUI's multi-bar view.
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

// IsConfigured returns nil iff at least one of these is available:
//  1. explicit `providers.codex.api_key` / `token` override in config;
//  2. `codex` CLI on PATH (the binary will gate further by RPC handshake);
//  3. bearer token in `~/.codex/auth.json`.
//
// Per the types.Provider contract this performs no network I/O.
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
		if k, _, _, ok := p.cfg.Override("codex"); ok && k != "" {
			return nil
		}
	}
	if _, err := exec.LookPath("codex"); err == nil {
		return nil
	}
	if cred, err := a.CodexToken(); err == nil && cred.Token != "" {
		return nil
	}
	return fmt.Errorf("codex: no credentials in %s, no codex CLI on PATH, and no override", a.CodexPath())
}

// FetchUsage tries the local codex CLI first (when present and not
// overridden), and falls back to the legacy probe path on any failure.
// Errors from the CLI attempt are swallowed so the user gets the second-
// best reading (rate-limit headers via api.openai.com) instead of an
// "RPC error" — the typical cause is the user simply not being logged in
// to the Codex CLI yet.
func (p *Provider) FetchUsage(ctx context.Context) (*types.UsageStats, error) {
	if !p.isExplicitOverride() {
		if _, err := exec.LookPath("codex"); err == nil {
			if p.cliBackoffActive() {
				if stats, ok, err := p.fetchOAuth(ctx); err == nil && ok {
					p.recordCLISuccess() // clear stale short/long cooldown
					return stats, nil
				}
				return p.fetchAPI(ctx)
			}
			res, err := p.fetchCLI(ctx)
			if err == nil && res != nil {
				p.recordCLISuccess()
				p.cacheWindows(res.windows)
				stats := res.primary
				return &stats, nil
			}
			// CLI refused us. Classify by failure kind so the cooldown
			// policy matches the cause:
			//   - recoverable auth (token_invalidated) -> 5s cooldown
			//     so `codex login` takes effect on the next tick
			//   - daemon-side timeout -> no cooldown (recordCLITimeout)
			//   - everything else -> 30-min cooldown
			switch {
			case isAuthInvalidated(err):
				p.recordCLIFailureShort()
			case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
				p.recordCLITimeout()
			default:
				p.recordCLIFailure()
			}
			// Fall through: if the CLI's app-server couldn't fetch
			// rateLimits (e.g. token invalidated upstream), try the
			// OAuth wham/usage endpoint directly using our own OAuth
			// access_token read from ~/.codex/auth.json.
			if stats, ok, err := p.fetchOAuth(ctx); err == nil && ok {
				p.recordCLISuccess() // clear stale cooldown if OAuth cured it
				return stats, nil
			}
		}
	}
	return p.fetchAPI(ctx)
}

// isExplicitOverride returns true when the user supplied an api_key /
// token in config — when set, we skip the CLI even if it would also have
// worked (override semantics are "use this, period").
func (p *Provider) isExplicitOverride() bool {
	if p.cfg == nil {
		return false
	}
	if k, _, t, ok := p.cfg.Override("codex"); ok && (k != "" || t != "") {
		return true
	}
	return false
}

func (p *Provider) cacheWindows(win []types.UsageStats) {
	if len(win) == 0 {
		return
	}
	p.mu.Lock()
	p.lastWin = append([]types.UsageStats(nil), win...)
	p.mu.Unlock()
}

// cliCooldown is the time we wait after a CLI failure before retrying.
// CodexBar deliberately skips 30 min after launch failures; we mirror
// that policy so a logged-out user isn't forked every cycle.
const cliCooldown = 30 * time.Minute

// cliCooldownShort is used when the failure looks recoverable (e.g. the
// server explicitly returned token_invalidated).  After the user runs
// `codex login` again, we want the daemon to pick up the fresh token on
// its very next tick — 5 seconds beats 30 minutes by a wide margin.
//
// Note: the daemon's refresh_interval (default 60s) is the actual retry
// cadence when the daemon is running. 5s is the floor used for manual
// `provider-usage refresh` and TUI `r` keystrokes; it isn't a hot-loop.
const cliCooldownShort = 5 * time.Second

// errAuthInvalidated is a sentinel wrapped around any underlying error
// that signals "the server rejected our token, the user should re-login".
// fetchCLI parses the JSON-RPC error body for this case; fetchOAuth
// checks the wham/usage HTTP body directly.  FetchUsage inspects this
// sentinel to choose the short CLI cooldown.
var errAuthInvalidated = errors.New("codex auth invalidated")

// cliBackoffActive returns true if the most recent CLI failure was
// recent enough to warrant skipping the spawn. Failures tagged
// recoverable (token_invalidated) clear out of backoff in seconds
// instead of minutes.
func (p *Provider) cliBackoffActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cliFailAt.IsZero() {
		return false
	}
	cd := cliCooldown
	if p.cliFailShort {
		cd = cliCooldownShort
	}
	return time.Since(p.cliFailAt) < cd
}

// recordCLIFailure marks a non-auth CLI failure (spawn error, malformed
// response, transport drop, etc.) and applies the long cooldown.
func (p *Provider) recordCLIFailure() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cliFailAt = time.Now()
	p.cliFailShort = false
}

// recordCLIFailureShort marks a recoverable auth failure — typically
// a server-side token_invalidated — so the next daemon tick (>= 60s
// later) can re-try without the user having to wait 30 minutes after
// `codex login`.
func (p *Provider) recordCLIFailureShort() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cliFailAt = time.Now()
	p.cliFailShort = true
}

func (p *Provider) recordCLISuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cliFailAt = time.Time{}
	p.cliFailShort = false
}

// isAuthInvalidated reports whether err (or anything in its chain)
// matches the recoverable-auth sentinel.
func isAuthInvalidated(err error) bool {
	return err != nil && errors.Is(err, errAuthInvalidated)
}

// recordCLITimeout notes a timeout-driven failure without bumping the
// backoff timer.  When the daemon's 8s context deadline fires, the user
// isn't at fault — we just don't want to flip into the 30-min cooldown
// and lock them out.
func (p *Provider) recordCLITimeout() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// No-op for now; kept separate from recordCLIFailure so future
	// policies (e.g. shorter cooldown or warning-only) have an obvious
	// landing spot.
}

// ---------- strategy 1: codex CLI app-server ----------

// rpcReq models a single JSON-RPC client -> server frame written to
// stdin. We send one request per line and read responses via bufio.
type rpcReq struct {
	ID     int            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

// rpcFrame models any line the app-server may emit on stdout: a response
// to one of our calls, or a server-initiated notification (no id, has
// method). We ignore notifications for now.
type rpcFrame struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
	Method string          `json:"method,omitempty"`
}

// rateLimitsEnvelope is the loose superset of shapes the codex
// app-server has shipped across minor versions. v0.146.0 returns the
// raw wham/usage payload, but earlier (and possibly future) builds
// nested the windows inside `windows`, `primary`/`secondary`, or
// `additional`. We accept any of them.
type rateLimitsEnvelope struct {
	Windows                 []whamWindow   `json:"windows"`
	Primary                 *whamWindow    `json:"primary"`
	Secondary               *whamWindow    `json:"secondary"`
	Additional              []whamWindow   `json:"additional"`
	RateLimit               *whamRateLimit `json:"rate_limit"`
	RateLimitAlt            *whamRateLimit `json:"rateLimit"`
	AdditionalRateLimits    []whamAddLimit `json:"additional_rate_limits"`
	AdditionalRateLimitsAlt []whamAddLimit `json:"additionalRateLimits"`
}

// ToWindows flattens any combination of the loose shapes into a single
// ordered slice: primary, secondary, then windows[] and additional[].
// Used by the CLI response parser; wham/usage uses rate_limit instead.
func (e *rateLimitsEnvelope) ToWindows() []whamWindow {
	out := []whamWindow{}
	if e.Primary != nil {
		out = append(out, *e.Primary)
	}
	if e.Secondary != nil {
		out = append(out, *e.Secondary)
	}
	if e.RateLimit != nil {
		if e.RateLimit.Primary != nil {
			out = append(out, *e.RateLimit.Primary)
		}
		if e.RateLimit.Secondary != nil {
			out = append(out, *e.RateLimit.Secondary)
		}
	}
	if e.RateLimitAlt != nil {
		if e.RateLimitAlt.Primary != nil && e.RateLimit == nil {
			out = append(out, *e.RateLimitAlt.Primary)
		}
		if e.RateLimitAlt.Secondary != nil && e.RateLimit == nil {
			out = append(out, *e.RateLimitAlt.Secondary)
		}
	}
	out = append(out, e.Windows...)
	out = append(out, e.Additional...)
	for _, a := range e.AdditionalRateLimits {
		if a.Primary != nil {
			w := *a.Primary
			if w.Label == "" {
				w.Label = firstNonEmpty(a.LimitName, a.LimitAlt)
			}
			out = append(out, w)
		}
	}
	for _, a := range e.AdditionalRateLimitsAlt {
		if a.Primary != nil {
			w := *a.Primary
			if w.Label == "" {
				w.Label = firstNonEmpty(a.LimitName, a.LimitAlt)
			}
			out = append(out, w)
		}
	}
	return out
}

// accountInfo mirrors the v0.146.0 app-server `account/read` result:
// everything sits under `result.account.{email,type,planType}` plus a
// top-level `requiresOpenaiAuth` hint. We just pull out the fields we
// surface to the UI / Note.
type accountInfo struct {
	Email     string `json:"email"`
	Type      string `json:"type"`
	PlanType  string `json:"planType"`
	OpenAIReq bool   `json:"requiresOpenaiAuth"`
}

// whamWindow models a single rate-limit window.  Two encodings exist:
// snake_case (wham/usage) and camelCase (app-server wrappers).
// limit_window_seconds wins when both are present.
type whamWindow struct {
	Label                 string  `json:"label,omitempty"`
	UsedPercent           float64 `json:"used_percent"`
	UsedPercentAlt        float64 `json:"usedPercent"`
	LimitWindowSeconds    int     `json:"limit_window_seconds"`
	LimitWindowSecAlt     int     `json:"limitWindowSeconds"`
	ResetAtSeconds        int64   `json:"reset_at_seconds"`
	ResetAtSecAlt         int64   `json:"resetAtSeconds"`
	LimitWindowMinutes    int     `json:"window_minutes"`
	LimitWindowMinutesAlt int     `json:"windowMinutes"`
	ResetsAt              string  `json:"resets_at"`
	ResetsAtAlt           string  `json:"resetsAt"`
}

// resetIn returns time.Duration until reset, preferring the absolute
// timestamp (RFC3339) and falling back to the unix-seconds field.
func (w whamWindow) resetIn(now time.Time) (time.Duration, time.Time, bool) {
	for _, raw := range []string{w.ResetsAt, w.ResetsAtAlt} {
		if raw == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			d := time.Until(t)
			if d < 0 {
				d = 0
			}
			return d, t, true
		}
	}
	for _, sec := range []int64{w.ResetAtSeconds, w.ResetAtSecAlt} {
		if sec <= 0 {
			continue
		}
		t := time.Unix(sec, 0)
		return time.Until(t), t, true
	}
	return 0, time.Time{}, false
}

func (w whamWindow) windowMinutes() int {
	if w.LimitWindowMinutes > 0 {
		return w.LimitWindowMinutes
	}
	if w.LimitWindowMinutesAlt > 0 {
		return w.LimitWindowMinutesAlt
	}
	if w.LimitWindowSeconds > 0 {
		return w.LimitWindowSeconds / 60
	}
	if w.LimitWindowSecAlt > 0 {
		return w.LimitWindowSecAlt / 60
	}
	return 0
}

// whamResponse matches the chatgpt.com/backend-api/wham/usage payload.
// We also tolerate the app-server wrapping the same envelope under
// "rate_limit" / "rateLimit" so a CLI response that escapes its
// container still parses.
type whamResponse struct {
	PlanType           string         `json:"plan_type"`
	PlanTypeAlt        string         `json:"planType"`
	RateLimit          whamRateLimit  `json:"rate_limit"`
	RateLimitAlt       whamRateLimit  `json:"rateLimit"`
	AdditionalLimits   []whamAddLimit `json:"additional_rate_limits"`
	AdditionalLimitsAl []whamAddLimit `json:"additionalRateLimits"`
}

type whamAddLimit struct {
	LimitName string      `json:"limit_name"`
	LimitAlt  string      `json:"limitName"`
	Primary   *whamWindow `json:"primary_window"`
	SecWindow *whamWindow `json:"secondary_window"`
}

type whamRateLimit struct {
	Primary   *whamWindow `json:"primary_window"`
	Secondary *whamWindow `json:"secondary_window"`
}

// windows flattens the wham/usage response into an ordered slice: primary
// first, secondary next, named additions (Spark etc.) after. We track
// whether we've already emitted a primary so the camelCase and snake_case
// rate_limit shapes don't duplicate.
func (r whamResponse) windows() []whamWindow {
	out := []whamWindow{}
	if r.RateLimit.Primary != nil {
		out = append(out, *r.RateLimit.Primary)
	} else if r.RateLimitAlt.Primary != nil {
		out = append(out, *r.RateLimitAlt.Primary)
	}
	if r.RateLimit.Secondary != nil {
		out = append(out, *r.RateLimit.Secondary)
	} else if r.RateLimitAlt.Secondary != nil {
		out = append(out, *r.RateLimitAlt.Secondary)
	}
	adds := r.AdditionalLimits
	if len(adds) == 0 {
		adds = r.AdditionalLimitsAl
	}
	for _, a := range adds {
		if a.Primary != nil {
			w := *a.Primary
			if w.Label == "" {
				w.Label = firstNonEmpty(a.LimitName, a.LimitAlt)
			}
			out = append(out, w)
		}
		if a.SecWindow != nil {
			w := *a.SecWindow
			if w.Label == "" {
				w.Label = firstNonEmpty(a.LimitName, a.LimitAlt) + " weekly"
			}
			out = append(out, w)
		}
	}
	return out
}

// firstNonEmpty returns the first non-empty string in ss, or "" if all empty.
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// cliResult bundles the primary window with the full ordered slice so
// callers can hydrate both UsageStats (primary) and Snapshot.Windows.
type cliResult struct {
	primary types.UsageStats
	windows []types.UsageStats
}

// fetchCLI spawns the codex app-server, asks for account/rateLimits, and
// returns the parsed windows.  A non-nil err means "fetchAPI should fall
// back".  The child process is always reaped before we return so we
// never leak an orphan process or block on stdout EOF.
func (p *Provider) fetchCLI(ctx context.Context) (*cliResult, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return nil, err
	}
	// -s read-only: prevents the server from mutating the user's session.
	// -a untrusted: sandbox approval mode; we never want code-exec here.
	cmd := exec.CommandContext(ctx, bin, "-s", "read-only", "-a", "untrusted", "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}

	// cleanup shuts stdin and waits up to 750ms for the child to exit
	// gracefully; if it hasn't, we KILL it and reap.  Always call this
	// inline before waiting on readerDone — deferring it causes the
	// reader goroutine to block on stdout EOF indefinitely.
	cleanup := func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }() //nolint:errcheck
		select {
		case <-done:
		case <-time.After(750 * time.Millisecond):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				<-done
			}
		}
	}

	// pending correlates request ids to the per-call response channel.
	pending := make(map[int]chan rpcFrame)
	var pendingMu sync.Mutex

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(stdout)
		// Generous line cap in case a server notification contains a
		// long trace or stack frame.
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var frame rpcFrame
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				continue
			}
			if frame.Method != "" && frame.ID == 0 {
				continue // notification, ignore
			}
			pendingMu.Lock()
			ch, ok := pending[frame.ID]
			if ok {
				delete(pending, frame.ID)
			}
			pendingMu.Unlock()
			if ok {
				select {
				case ch <- frame:
				default:
				}
				close(ch)
			}
		}
	}()

	initTimeout := 6 * time.Second
	callTimeout := 4 * time.Second

	call := func(method string, params map[string]any) (rpcFrame, error) {
		id := 0
		pendingMu.Lock()
		for {
			id++
			if _, dup := pending[id]; !dup {
				break
			}
		}
		ch := make(chan rpcFrame, 1)
		pending[id] = ch
		pendingMu.Unlock()

		req := rpcReq{ID: id, Method: method, Params: params}
		buf, _ := json.Marshal(req)
		buf = append(buf, '\n')
		if _, err := stdin.Write(buf); err != nil {
			pendingMu.Lock()
			delete(pending, id)
			pendingMu.Unlock()
			return rpcFrame{}, err
		}

		t := callTimeout
		if method == "initialize" {
			t = initTimeout
		}
		select {
		case frame, ok := <-ch:
			if !ok {
				return rpcFrame{}, fmt.Errorf("%s: response channel closed (server exited?)", method)
			}
			if len(frame.Error) > 0 {
				errMsg := string(frame.Error)
				// The codex app-server embeds the underlying
				// wham/usage body in error messages for rate-limit
				// failures. Detect the recoverable token_invalidated
				// state and wrap with our sentinel so FetchUsage
				// applies the short cooldown.
				if strings.Contains(strings.ToLower(errMsg), "token_invalidated") ||
					strings.Contains(strings.ToLower(errMsg), "token invalidated") {
					return rpcFrame{}, fmt.Errorf("%s: %w: %s", method, errAuthInvalidated, errMsg)
				}
				return rpcFrame{}, fmt.Errorf("%s: server error: %s", method, errMsg)
			}
			return frame, nil
		case <-time.After(t):
			pendingMu.Lock()
			delete(pending, id)
			pendingMu.Unlock()
			// Wrap with context.DeadlineExceeded so FetchUsage's
			// errors.Is check correctly routes to recordCLITimeout,
			// keeping the user out of the 30-min cooldown when the
			// daemon's per-call timer (not the user's auth) is what
			// fired.
			return rpcFrame{}, fmt.Errorf("%s timed out after %s: %w", method, t, context.DeadlineExceeded)
		case <-ctx.Done():
			pendingMu.Lock()
			delete(pending, id)
			pendingMu.Unlock()
			return rpcFrame{}, ctx.Err()
		}
	}

	// 1. initialize (required by the JSON-RPC handshake).
	if _, err := call("initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "provider-usage",
			"version": "0.1.0",
		},
	}); err != nil {
		cleanup()
		<-readerDone
		return nil, err
	}

	// 2. account/read (best-effort; failure is non-fatal).
	var acct accountInfo
	if frame, err := call("account/read", map[string]any{}); err == nil {
		_ = json.Unmarshal(frame.Result, &acct)
	}

	// 3. account/rateLimits/read — the one we care about.
	frame, err := call("account/rateLimits/read", map[string]any{})
	if err != nil {
		cleanup()
		<-readerDone
		return nil, err
	}

	var env rateLimitsEnvelope
	if err := json.Unmarshal(frame.Result, &env); err != nil {
		cleanup()
		<-readerDone
		return nil, fmt.Errorf("rate limits: parse: %w", err)
	}

	wins := env.ToWindows()
	if len(wins) == 0 {
		cleanup()
		<-readerDone
		return nil, fmt.Errorf("rate limits: no windows in response")
	}

	stats := make([]types.UsageStats, 0, len(wins))
	for i := range wins {
		stats = append(stats, buildWindowStats(wins[i], acct.PlanType))
	}

	// The v0.146.0 app-server has no `shutdown` method — closing stdin
	// (and waiting briefly for the graceful exit) is the only signal it
	// accepts.  The post-sleep cleanup() then KILLs anything still alive
	// after 750ms.
	time.Sleep(150 * time.Millisecond)
	cleanup()
	<-readerDone
	return &cliResult{primary: stats[0], windows: stats}, nil
}

func buildWindowStats(w whamWindow, planType string) types.UsageStats {
	used := w.UsedPercent
	if used == 0 {
		used = w.UsedPercentAlt
	}
	mins := w.windowMinutes()
	now := time.Now()
	s := types.UsageStats{
		// wham/usage only publishes a percent, not an absolute cap, so
		// we scale Used/Total so the progress bar fills proportionally.
		Used:        used,
		Total:       100,
		Unit:        types.UnitCount,
		WindowLabel: windowLabel(w.Label, mins),
		LastProbeAt: now,
		Note:        buildNote(planType, mins),
	}
	if d, t, ok := w.resetIn(now); ok {
		s.ResetIn = d
		s.ResetAt = t
	}
	return s
}

func windowLabel(label string, mins int) string {
	if label != "" {
		return label
	}
	switch {
	case mins <= 0:
		return "rolling"
	case mins < 60:
		return fmt.Sprintf("%dm", mins)
	case mins == 60*24*7:
		return "weekly" // CodexBar convention for the 7-day secondary window
	case mins == 60*24*30:
		return "monthly"
	case mins%60 == 0:
		return fmt.Sprintf("%dh", mins/60)
	case mins%1440 == 0:
		return fmt.Sprintf("%dd", mins/1440)
	default:
		return fmt.Sprintf("%dh", mins/60)
	}
}

func buildNote(planType string, mins int) string {
	var b strings.Builder
	if planType != "" {
		b.WriteString(planType)
	}
	if mins > 0 {
		if b.Len() > 0 {
			b.WriteString(" · ")
		}
		fmt.Fprintf(&b, "rolling %s", windowLabel("", mins))
	}
	return b.String()
}

// ---------- strategy 2: legacy auth.json + api.openai.com probe ----------

// fetchAPI returns a UsageStats derived from `~/.codex/auth.json` and the
// `x-ratelimit-*` headers of a tiny probe request to api.openai.com.
func (p *Provider) fetchAPI(ctx context.Context) (*types.UsageStats, error) {
	a := p.a
	if a == nil {
		var err error
		a, err = auth.NewFinder()
		if err != nil {
			return nil, err
		}
		p.a = a
	}

	token := ""
	if p.cfg != nil {
		if k, _, t, ok := p.cfg.Override("codex"); ok {
			token = firstNonEmpty(k, t)
		}
	}
	if token == "" {
		cred, err := a.CodexToken()
		if err != nil {
			return &types.UsageStats{
				LastProbeAt: time.Now(),
				Error:       err.Error(),
				Note:        "codex not logged in — run `codex login` or supply an api_key in config",
			}, nil
		}
		token = cred.Token
	}

	model := "gpt-4o-mini"
	body := probe.NewChatBody(model, p.maxTokens())
	req, err := probe.NewPOST(ctx, "https://api.openai.com/v1/chat/completions", map[string]string{
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
	if res.Status == 401 || res.Status == 403 {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       fmt.Sprintf("auth rejected (HTTP %d): check codex auth.json", res.Status),
		}, nil
	}
	stats := &types.UsageStats{LastProbeAt: time.Now()}
	if res.Status >= 400 {
		stats.Error = fmt.Sprintf("HTTP %d: %s", res.Status, snippet(res.Body))
	}
	applyRateLimit(stats, res.RateLimit)
	if res.RateLimit.Group != "" {
		stats.Note = res.RateLimit.Group
	}
	return stats, nil
}

// fetchOAuth calls chatgpt.com/backend-api/wham/usage with the OAuth
// access_token stored at ~/.codex/auth.json (top-level tokens block).
// Returns (stats, true, nil) on success, (nil, false, nil) when the user
// isn't on the chatgpt auth_mode, and (nil, false, err) for transport /
// parsing failures. A 401 with a `token_invalidated` body is surfaced as
// Error so the UI can prompt a `codex login`.
func (p *Provider) fetchOAuth(ctx context.Context) (*types.UsageStats, bool, error) {
	a := p.a
	if a == nil {
		var err error
		a, err = auth.NewFinder()
		if err != nil {
			return nil, false, err
		}
		p.a = a
	}
	if a.CodexAuthMode() != "chatgpt" {
		return nil, false, nil
	}
	access, _, _, ok := a.CodexOAuthToken()
	if !ok || access == "" {
		return nil, false, nil
	}
	req, err := probe.NewGET(ctx, "https://chatgpt.com/backend-api/wham/usage", map[string]string{
		"Authorization": "Bearer " + access,
		"Accept":        "application/json",
	})
	if err != nil {
		return nil, false, err
	}
	res, err := p.probe.Do(ctx, req)
	if err != nil {
		return nil, false, err
	}
	if res.Status == 401 || res.Status == 403 {
		body := strings.ToLower(string(res.Body))
		if strings.Contains(body, "token_invalidated") || strings.Contains(body, "token invalidated") {
			// Apply the short cooldown so the daemon re-checks the
			// (newly-arrived) access_token from ~/.codex/auth.json
			// after `codex login` instead of waiting 30 minutes.
			p.recordCLIFailureShort()
			return &types.UsageStats{
				LastProbeAt: time.Now(),
				Error:       "codex OAuth token invalidated — run `codex login` again",
				Note:        "wham/usage 401: token_invalidated",
			}, true, nil
		}
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       fmt.Sprintf("wham/usage auth rejected (HTTP %d)", res.Status),
		}, true, nil
	}
	if res.Status >= 400 {
		return &types.UsageStats{
			LastProbeAt: time.Now(),
			Error:       fmt.Sprintf("wham/usage HTTP %d: %s", res.Status, snippet(res.Body)),
		}, true, nil
	}
	var env whamResponse
	if err := json.Unmarshal(res.Body, &env); err != nil {
		return nil, false, fmt.Errorf("wham/usage: parse: %w", err)
	}
	wins := env.windows()
	if len(wins) == 0 {
		return nil, false, fmt.Errorf("wham/usage: no windows in response")
	}
	plan := env.PlanType
	if plan == "" {
		plan = env.PlanTypeAlt
	}
	stats := make([]types.UsageStats, 0, len(wins))
	for i := range wins {
		stats = append(stats, buildWindowStats(wins[i], plan))
	}
	p.cacheWindows(stats)
	return &stats[0], true, nil
}

func (p *Provider) maxTokens() int {
	if p.cfg != nil && p.cfg.ProbeMaxTokens > 0 {
		return p.cfg.ProbeMaxTokens
	}
	return 1
}

func applyRateLimit(s *types.UsageStats, rl probe.RateLimit) {
	if !rl.HasData {
		s.Note = "no x-ratelimit-* headers (may not be on metered auth)"
		return
	}
	if rl.ReqLimit > 0 {
		s.Total = float64(rl.ReqLimit)
		s.Used = float64(rl.ReqLimit - rl.ReqRemain)
		s.Unit = types.UnitCount
		s.WindowLabel = "5h"
	} else if rl.TokLimit > 0 {
		s.Total = float64(rl.TokLimit)
		s.Used = float64(rl.TokLimit - rl.TokRemain)
		s.Unit = types.UnitTokens
		s.WindowLabel = "tokens"
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

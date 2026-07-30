// Package types contains the core data model shared across providers,
// the daemon, the TUI and the polybar widget.
package types

import (
	"context"
	"time"
)

// Provider is the interface every usage-tracking provider implements.
type Provider interface {
	// Name returns the canonical short identifier (e.g. "opencode").
	Name() string
	// DisplayName returns a human-friendly label.
	DisplayName() string
	// Icon returns a short glyph for use in the TUI / polybar.
	Icon() string
	// IsConfigured returns nil iff credentials are available to attempt a
	// probe. It must NOT perform any network I/O.
	IsConfigured() error
	// AvailableModels returns free-tier models, if known. May be nil.
	AvailableModels() []FreeModel
	// FetchUsage performs a lightweight probe and returns UsageStats.
	FetchUsage(ctx context.Context) (*UsageStats, error)
}

// FreeModel describes a model surfaced in the "Premium models" /
// "Free models" sections of the TUI.
//
// Premium == false is the historical default: every provider's static
// list was a back-of-the-envelope "free-tier" list and the TUI rendered
// one block labelled "Free models". For Freebuff, Premium means that the
// model consumes the shared Premium session quota. It does not imply that
// the user needs a paid subscription. Leaving Premium at its zero value
// keeps older providers backwards-compatible without touching their tables.
type FreeModel struct {
	ID      string // canonical provider-side identifier
	Label   string // human-friendly label
	Notes   string // optional caveat (e.g. "may log data for training")
	Premium bool   // true = consumes Freebuff's Premium session quota
}

// UsageStats is the canonical usage snapshot across providers.
type UsageStats struct {
	Used  float64
	Total float64
	Unit  UsageUnit

	WindowLabel string        // e.g. "5h", "weekly", "monthly"
	Note        string        // optional caveat
	ResetAt     time.Time     // fixed reset timestamp (zero = stale/rolling)
	ResetIn     time.Duration // duration until Used "frees" (rolling windows)
	LastProbeAt time.Time
	Error       string // non-fatal error the UI may still display
}

// UsageUnit describes what Used/Total represent.
type UsageUnit int

const (
	UnitUnknown UsageUnit = iota
	UnitUSD               // dollar-based plan
	UnitCount             // messages / requests
	UnitTokens            // tokens
)

// Percent returns 0-100. 0 when Total is zero.
func (u UsageStats) Percent() float64 {
	if u.Total <= 0 {
		return 0
	}
	pct := (u.Used / u.Total) * 100
	if pct > 100 {
		pct = 100
	}
	return pct
}

// Remaining returns Total - Used, floored at zero.
func (u UsageStats) Remaining() float64 {
	r := u.Total - u.Used
	if r < 0 {
		return 0
	}
	return r
}

// Snapshot is the per-provider record persisted by the state store.
type Snapshot struct {
	Provider    string      `json:"provider"`
	DisplayName string      `json:"display_name"`
	Icon        string      `json:"icon"`
	Usage       *UsageStats `json:"usage,omitempty"`
	// Windows holds additional usage windows (e.g. OpenCode Go's
	// $12/5h + $30/week + $60/month). Providers that implement the
	// optional MultiWindowProvider interface populate this slice;
	// consumers fall back to Usage when Windows is empty.
	Windows     []UsageStats `json:"windows,omitempty"`
	FreeModels  []FreeModel  `json:"free_models,omitempty"`
	Err         string       `json:"error,omitempty"`
	RefreshedAt time.Time    `json:"refreshed_at"`
}

// Aggregate is the entire state-file payload.
type Aggregate struct {
	GeneratedAt time.Time           `json:"generated_at"`
	Providers   map[string]Snapshot `json:"providers"`
}

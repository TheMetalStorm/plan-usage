// Package providers aggregates every provider implementation and exposes
// a stable registry used by the TUI, daemon and tray popup.
package providers

import (
	"fmt"
	"sync"

	"github.com/TheMetalStorm/plan-usage/internal/providers/clinepass"
	"github.com/TheMetalStorm/plan-usage/internal/providers/codex"
	"github.com/TheMetalStorm/plan-usage/internal/providers/commandcode"
	"github.com/TheMetalStorm/plan-usage/internal/providers/freebuff"
	"github.com/TheMetalStorm/plan-usage/internal/providers/opencodego"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

// Builder returns a Provider instance.
type Builder func() (types.Provider, error)

var (
	mu       sync.RWMutex
	builders = map[string]Builder{}
)

// order defines the canonical iteration order (UI / daemon / tray).
var order = []string{
	"opencodego",
	"codex",
	"clinepass",
	"commandcode",
	"freebuff",
}

// Register exposes a provider under the given name (used by side-effect
// imports).
func Register(name string, b Builder) {
	mu.Lock()
	defer mu.Unlock()
	builders[name] = b
}

// All returns every registered provider in stable order.
func All() []types.Provider {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]types.Provider, 0, len(builders))
	for _, name := range order {
		b, ok := builders[name]
		if !ok {
			continue
		}
		p, err := b()
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// AllNames returns every registered provider name.
func AllNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(order))
	for _, n := range order {
		if _, ok := builders[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// Get fetches a single provider by name.
func Get(name string) (types.Provider, error) {
	mu.RLock()
	defer mu.RUnlock()
	b, ok := builders[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	return b()
}

// MultiWindowProvider is an optional capability implemented by providers
// that expose more than one usage window (e.g. OpenCode Go's
// $12/5h + $30/week + $60/month).  Providers that don't implement it
// have only a single UsageStats at snapshot time.
type MultiWindowProvider interface {
	types.Provider
	// SnapshotWindows returns the additional windows for this provider.
	// The first entry is treated as the "primary" window by the TUI.
	SnapshotWindows() []types.UsageStats
}

// EnrichWindows copies the optional MultiWindowProvider plan into the
// snapshot and uses the first window as the primary UsageStats if Usage
// is still nil.  This covers the three places that build a Snapshot:
// the daemon's cycle, `usage refresh`, and the TUI's local probe path.
func EnrichWindows(p types.Provider, snap *types.Snapshot) {
	if snap == nil || len(snap.Windows) > 0 {
		return
	}
	mw, ok := p.(MultiWindowProvider)
	if !ok {
		return
	}
	ws := mw.SnapshotWindows()
	if len(ws) == 0 {
		return
	}
	snap.Windows = ws
	if snap.Usage == nil {
		u := ws[0]
		snap.Usage = &u
	}
}

// init registers every provider implementation.  Runs at package load
// time; the order above defines the canonical UI / daemon / tray
// iteration sequence.
func init() {
	Register("opencodego", func() (types.Provider, error) { return opencodego.New(), nil })
	Register("codex", func() (types.Provider, error) { return codex.New(), nil })
	Register("clinepass", func() (types.Provider, error) { return clinepass.New(), nil })
	Register("commandcode", func() (types.Provider, error) { return commandcode.New(), nil })
	Register("freebuff", func() (types.Provider, error) { return freebuff.New(), nil })
}

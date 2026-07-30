// Package daemon runs the background poller that keeps the state file
// fresh for polybar and the TUI.
//
// One goroutine per enabled provider fetches concurrently and writes the
// aggregate snapshot atomically.  The daemon is resilient to a single
// provider failing: it logs and moves on.
package daemon

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/providers"
	"github.com/TheMetalStorm/plan-usage/internal/state"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

// Daemon wraps the loop.
type Daemon struct {
	cfg   *config.Config
	store *state.Store
}

// New creates a daemon.
func New(cfg *config.Config) (*Daemon, error) {
	store, err := state.New(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	return &Daemon{cfg: cfg, store: store}, nil
}

// Refresh runs one provider refresh cycle. It is safe to call from a
// desktop tray process or another foreground consumer; the caller owns
// the lifetime of ctx.
func (d *Daemon) Refresh(ctx context.Context) {
	d.cycle(ctx)
}

// Run executes the main loop until SIGINT / SIGTERM. Before each
// scheduled cycle it reloads the configured refresh interval, so changing
// refresh_interval from the tray takes effect without restarting systemd.
func (d *Daemon) Run() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	d.Refresh(ctx)
	for {
		timer := time.NewTimer(d.cfg.RefreshEvery)
		select {
		case <-timer.C:
			d.reloadRefreshInterval()
			d.Refresh(ctx)
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (d *Daemon) reloadRefreshInterval() {
	if d.cfg.ConfigPath == "" {
		return
	}
	fresh, err := config.Load(d.cfg.ConfigPath)
	if err == nil && fresh.RefreshEvery >= 5*time.Second {
		d.cfg.RefreshEvery = fresh.RefreshEvery
	}
}

// cycle refreshes every enabled provider in parallel and writes the
// aggregate snapshot.
func (d *Daemon) cycle(ctx context.Context) {
	agg := types.Aggregate{
		GeneratedAt: time.Now(),
		Providers:   map[string]types.Snapshot{},
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range providers.All() {
		name := p.Name()
		if !d.cfg.IsProviderEnabled(name) {
			continue
		}
		if cfgErr := p.IsConfigured(); cfgErr != nil {
			agg.Providers[name] = d.snapWithError(p, cfgErr)
			continue
		}
		wg.Add(1)
		go func(p types.Provider) {
			defer wg.Done()
			pctx, pcancel := context.WithTimeout(ctx, 8*time.Second)
			defer pcancel()
			snap := d.snapFromProvider(p, pctx)
			mu.Lock()
			agg.Providers[name] = snap
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	if err := d.store.Replace(agg); err != nil {
		fmt.Fprintln(os.Stderr, "daemon: write state:", err)
	}
}

// snapWithError builds a snapshot for a provider that fails IsConfigured.
func (d *Daemon) snapWithError(p types.Provider, cfgErr error) types.Snapshot {
	snap := types.Snapshot{
		Provider:    p.Name(),
		DisplayName: p.DisplayName(),
		Icon:        p.Icon(),
		Err:         cfgErr.Error(),
		FreeModels:  p.AvailableModels(),
		RefreshedAt: time.Now(),
	}
	// Plan shape is still useful even without live data.
	providers.EnrichWindows(p, &snap)
	return snap
}

// snapFromProvider runs a probe (with panic recovery) and merges in the
// optional MultiWindowProvider plan in the deferred tail.
func (d *Daemon) snapFromProvider(p types.Provider, ctx context.Context) (snap types.Snapshot) {
	snap = types.Snapshot{
		Provider:    p.Name(),
		DisplayName: p.DisplayName(),
		Icon:        p.Icon(),
		FreeModels:  p.AvailableModels(),
		RefreshedAt: time.Now(),
	}
	defer func() {
		if r := recover(); r != nil {
			snap = types.Snapshot{
				Provider:    p.Name(),
				DisplayName: p.DisplayName(),
				Icon:        p.Icon(),
				FreeModels:  p.AvailableModels(),
				Err:         fmt.Sprintf("panic: %v", r),
				RefreshedAt: time.Now(),
			}
			fmt.Fprintf(os.Stderr, "daemon: %s panicked: %v\n", p.Name(), r)
		}
		providers.EnrichWindows(p, &snap)
	}()
	stats, err := p.FetchUsage(ctx)
	if err != nil {
		snap.Err = err.Error()
		return snap
	}
	snap.Usage = stats
	if stats != nil && stats.Error != "" {
		snap.Err = stats.Error
	}
	if stats != nil && !stats.LastProbeAt.IsZero() {
		snap.RefreshedAt = stats.LastProbeAt
	}
	return snap
}

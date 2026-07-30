package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/simon/usage/internal/config"
	"github.com/simon/usage/internal/providers"
	"github.com/simon/usage/internal/state"
	"github.com/simon/usage/internal/types"
)

// runRefresh triggers one refresh cycle and writes the resulting
// snapshot to the state file.  All progress messages go to stderr so
// piping into JSON-aware tools stays clean.
func runRefresh(cfg *config.Config) error {
	agg := types.Aggregate{
		GeneratedAt: time.Now(),
		Providers:   map[string]types.Snapshot{},
	}
	store, err := state.New(cfg.StateDir)
	if err != nil {
		return err
	}
	for _, p := range providers.All() {
		name := p.Name()
		if !cfg.IsProviderEnabled(name) {
			continue
		}
		if cfgErr := p.IsConfigured(); cfgErr != nil {
			agg.Providers[name] = types.Snapshot{
				Provider:    name,
				DisplayName: p.DisplayName(),
				Icon:        p.Icon(),
				Err:         cfgErr.Error(),
				FreeModels:  p.AvailableModels(),
				RefreshedAt: time.Now(),
			}
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", name, cfgErr)
			continue
		}
		if cfg.DryRun {
			fmt.Fprintf(os.Stderr, "dry-run: would refresh %s\n", name)
			// Record a placeholder so dry-run still produces a consistent
			// snapshot (6 in, 6 entries out).
			agg.Providers[name] = types.Snapshot{
				Provider:    name,
				DisplayName: p.DisplayName(),
				Icon:        p.Icon(),
				FreeModels:  p.AvailableModels(),
				Err:         "dry-run: not actually probed",
				RefreshedAt: time.Now(),
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		snap, err := probeSnap(ctx, p)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error %s: %v\n", name, err)
		}
		// Single-source-of-truth for plan scaffolds (OpenCode Go etc).
		providers.EnrichWindows(p, &snap)
		agg.Providers[name] = snap
	}
	if err := store.Replace(agg); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "refreshed %d provider(s) at %s\n",
		len(agg.Providers), agg.GeneratedAt.Format(time.RFC3339))
	return nil
}

func probeSnap(ctx context.Context, p types.Provider) (types.Snapshot, error) {
	snap := types.Snapshot{
		Provider:    p.Name(),
		DisplayName: p.DisplayName(),
		Icon:        p.Icon(),
		FreeModels:  p.AvailableModels(),
		RefreshedAt: time.Now(),
	}
	stats, ferr := p.FetchUsage(ctx)
	if ferr != nil {
		snap.Err = ferr.Error()
		return snap, ferr
	}
	snap.Usage = stats
	if stats != nil && stats.Error != "" {
		snap.Err = stats.Error
	}
	if stats != nil && !stats.LastProbeAt.IsZero() {
		snap.RefreshedAt = stats.LastProbeAt
	}
	return snap, nil
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/simon/usage/internal/config"
	"github.com/simon/usage/internal/providers"
	"github.com/simon/usage/internal/state"
	"github.com/simon/usage/internal/types"
)

// runCheck dumps either a single provider (when called with a name) or
// the entire aggregate from the state file as JSON to stdout.
func runCheck(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		store, err := state.New(cfg.StateDir)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(store.All())
	}
	name := args[0]
	p, err := providers.Get(name)
	if err != nil {
		return err
	}
	if err := p.IsConfigured(); err != nil {
		return fmt.Errorf("not configured: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	stats, err := p.FetchUsage(ctx)
	if err != nil {
		return err
	}
	snap := types.Snapshot{
		Provider:    p.Name(),
		DisplayName: p.DisplayName(),
		Icon:        p.Icon(),
		Usage:       stats,
		FreeModels:  p.AvailableModels(),
		RefreshedAt: stats.LastProbeAt,
	}
	if stats != nil && stats.Error != "" {
		snap.Err = stats.Error
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}

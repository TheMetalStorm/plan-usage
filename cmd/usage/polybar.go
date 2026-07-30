package main

import (
	"fmt"
	"strings"

	"github.com/simon/usage/internal/config"
	"github.com/simon/usage/internal/format"
	"github.com/simon/usage/internal/state"
)

// runPolybar prints the compact polybar line.
//
// It is designed to be silent on failure: if the daemon hasn't yet
// produced any data, or the state dir is unreadable, we print the
// configured "no auth" placeholder so polybar doesn't show an ugly error.
func runPolybar(cfg *config.Config) error {
	store, err := state.New(cfg.StateDir)
	if err != nil {
		fmt.Print(cfg.Polybar.NoAuthText)
		return nil
	}
	agg := store.All()
	if len(agg.Providers) == 0 {
		fmt.Print(cfg.Polybar.NoAuthText)
		return nil
	}
	out := format.Aggregate(agg,
		cfg.Polybar.Format,
		cfg.Polybar.Separator,
		cfg.Polybar.NoAuthText,
		format.AggregateOpts{HideIfNoAuth: cfg.Polybar.HideIfNoAuth})
	if out == "" {
		out = cfg.Polybar.NoAuthText
	}
	fmt.Print(strings.TrimSpace(out))
	return nil
}

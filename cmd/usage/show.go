package main

import (
	"fmt"

	"github.com/simon/usage/internal/config"
	"github.com/simon/usage/internal/state"
	"github.com/simon/usage/internal/tui"
)

// runShow opens the interactive TUI.
func runShow(cfg *config.Config, _ []string) error {
	store, err := state.New(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if err := tui.RunProgram(tui.New(cfg, store)); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

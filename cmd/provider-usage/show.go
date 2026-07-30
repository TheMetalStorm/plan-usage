package main

import (
	"fmt"

	"github.com/TheMetalStorm/provider-usage/internal/config"
	"github.com/TheMetalStorm/provider-usage/internal/state"
	"github.com/TheMetalStorm/provider-usage/internal/tui"
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

package main

import (
	"fmt"

	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/tray"
)

// runTray starts the native desktop tray popup. It never launches Bubble Tea
// or a terminal emulator.
func runTray(cfg *config.Config) error {
	if err := tray.Run(cfg); err != nil {
		return fmt.Errorf("tray: %w", err)
	}
	return nil
}

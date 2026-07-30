package main

import (
	"fmt"

	"github.com/simon/usage/internal/config"
	"github.com/simon/usage/internal/daemon"
)

// runDaemon starts the long-running poller.
func runDaemon(cfg *config.Config) error {
	d, err := daemon.New(cfg)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	d.Run()
	return nil
}

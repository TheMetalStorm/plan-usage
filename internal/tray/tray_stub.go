//go:build !linux || !cgo

package tray

import (
	"fmt"

	"github.com/TheMetalStorm/plan-usage/internal/config"
)

// Run explains why the native popup cannot run on this build.
func Run(_ *config.Config) error {
	return fmt.Errorf("native tray popup requires Linux/X11 and CGO; rebuild on Linux with CGO_ENABLED=1 and GTK3 development libraries")
}

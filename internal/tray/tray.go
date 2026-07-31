// Package tray provides the native desktop tray popup.
//
// The view model in this file deliberately has no GUI dependencies. Platform
// files provide the actual tray process, while these helpers keep the provider
// cards and geometry easy to test on every build platform.
package tray

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/providers"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

const (
	windowTitle        = "plan-usage"
	popupWidth         = 1120
	popupHeight        = 760
	popupGridColumns   = 4
	popupBorder        = 8
	popupSpacing       = 6
	popupCardSpacing   = 4
	popupModelMaxChars = 48
)

// ProviderCard is the complete representation rendered by the popup for one
// enabled provider.
type ProviderCard struct {
	Name        string
	DisplayName string
	Icon        string
	Status      string
	Error       string
	Updated     string
	Windows     []WindowCard
	Models      []types.FreeModel
}

// WindowCard is one quota window shown inside a provider card.
type WindowCard struct {
	Label   string
	Percent float64
	Used    string
	Total   string
	Reset   string
	Note    string
}

// Rect is a screen work area in absolute coordinates.
type Rect struct {
	X, Y, Width, Height int
}

// RefreshGate prevents a timer and a manual refresh request from running at
// the same time. It is intentionally tiny so concurrency behavior is tested
// without GTK or a display server.
type RefreshGate struct{ mu sync.Mutex }

// TryBegin claims the refresh slot, returning false when one is in progress.
func (g *RefreshGate) TryBegin() bool { return g.mu.TryLock() }

// End releases the refresh slot.
func (g *RefreshGate) End() { g.mu.Unlock() }

// BuildCards returns one card for every enabled provider, in registry order.
// Missing snapshots are retained as cards so an unavailable provider is never
// silently omitted from the popup. The model catalog is copied into the card
// even when usage data is unavailable, so users can still see the plan's
// available models while fixing authentication.
func BuildCards(cfg *config.Config, agg types.Aggregate) []ProviderCard {
	if cfg == nil {
		return nil
	}
	cards := make([]ProviderCard, 0, len(providers.AllNames()))
	for _, name := range providers.AllNames() {
		if !cfg.IsProviderEnabled(name) {
			continue
		}
		snap := agg.Providers[name]
		card := ProviderCard{
			Name:        name,
			DisplayName: snap.DisplayName,
			Icon:        snap.Icon,
			Updated:     formatUpdated(snap.RefreshedAt),
			Models:      append([]types.FreeModel(nil), snap.FreeModels...),
		}
		if card.DisplayName == "" {
			if p, err := providers.Get(name); err == nil {
				card.DisplayName = p.DisplayName()
				card.Icon = p.Icon()
			}
		}
		if card.DisplayName == "" {
			card.DisplayName = name
		}
		if card.Icon == "" {
			card.Icon = "•"
		}
		card.Error = snap.Err
		if snap.Err != "" {
			card.Status = snap.Err
		} else if len(snap.Windows) == 0 && snap.Usage == nil {
			card.Status = "No usage data yet"
		} else {
			card.Status = "Usage available"
		}
		for _, usage := range snap.Windows {
			card.Windows = append(card.Windows, windowCard(usage))
		}
		if len(card.Windows) == 0 && snap.Usage != nil {
			card.Windows = append(card.Windows, windowCard(*snap.Usage))
		}
		cards = append(cards, card)
	}
	return cards
}

func freeModelsOnly(models []types.FreeModel) []types.FreeModel {
	out := make([]types.FreeModel, 0, len(models))
	for _, model := range models {
		if !model.Premium {
			out = append(out, model)
		}
	}
	return out
}

func premiumModelsOnly(models []types.FreeModel) []types.FreeModel {
	out := make([]types.FreeModel, 0, len(models))
	for _, model := range models {
		if model.Premium {
			out = append(out, model)
		}
	}
	return out
}

func windowCard(u types.UsageStats) WindowCard {
	return WindowCard{
		Label:   nonEmpty(u.WindowLabel, "rolling"),
		Percent: u.Percent(),
		Used:    humanValue(u),
		Total:   humanTotal(u),
		Reset:   resetText(u),
		Note:    u.Note,
	}
}

func resetText(u types.UsageStats) string {
	if !u.ResetAt.IsZero() {
		return "resets " + durationText(time.Until(u.ResetAt))
	}
	if u.ResetIn > 0 {
		return "resets " + durationText(u.ResetIn)
	}
	return "reset time unavailable"
}

func durationText(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	if d < time.Minute {
		return fmt.Sprintf("in %ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("in %dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("in %dd %dh", int(d.Hours()/24), int(d.Hours())%24)
}

func formatUpdated(t time.Time) string {
	if t.IsZero() {
		return "Last update: never"
	}
	age := time.Since(t)
	if age < 0 {
		age = 0
	}
	return "Last update: " + durationText(age) + " ago"
}

// popupGridPosition returns the zero-based grid position for a provider card.
func popupGridPosition(index int) (column, row int) {
	if index < 0 {
		return 0, 0
	}
	return index % popupGridColumns, index / popupGridColumns
}

func popupSizeForWorkarea(width, height int, work Rect) (int, int) {
	if work.Width > 0 && width > work.Width {
		width = work.Width
	}
	if work.Height > 0 && height > work.Height {
		height = work.Height
	}
	return width, height
}

// PopupClickOutside reports whether local popup coordinates fall outside the
// current popup allocation. GTK's logical grab routes outside clicks here.
func PopupClickOutside(x, y float64, width, height int) bool {
	return x < 0 || y < 0 || x >= float64(width) || y >= float64(height)
}

// PopupPosition clamps a pointer-aligned popup into a monitor work area.
func PopupPosition(pointerX, pointerY, popupWidth, popupHeight int, work Rect) (int, int) {
	if popupWidth < 0 {
		popupWidth = 0
	}
	if popupHeight < 0 {
		popupHeight = 0
	}
	x := pointerX
	y := pointerY
	if work.Width <= 0 || work.Height <= 0 {
		return x, y
	}
	maxX := work.X + work.Width - popupWidth
	maxY := work.Y + work.Height - popupHeight
	if maxX < work.X {
		maxX = work.X
	}
	if maxY < work.Y {
		maxY = work.Y
	}
	if x < work.X {
		x = work.X
	}
	if x > maxX {
		x = maxX
	}
	if y < work.Y {
		y = work.Y
	}
	if y > maxY {
		y = maxY
	}
	return x, y
}

// checkDesktopSession rejects unsupported display environments before GTK is
// initialized. The popup intentionally supports Linux/X11 only.
func checkDesktopSession() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") || os.Getenv("WAYLAND_DISPLAY") != "" {
		return fmt.Errorf("Wayland is not supported: plan-usage tray requires Linux/X11 with DISPLAY set")
	}
	if os.Getenv("DISPLAY") == "" {
		return fmt.Errorf("no X11 display detected: DISPLAY is unset; plan-usage tray requires Linux/X11")
	}
	return nil
}

func humanValue(s types.UsageStats) string {
	switch s.Unit {
	case types.UnitUSD:
		return fmt.Sprintf("$%.2f", s.Used)
	case types.UnitTokens:
		return formatUsageAmount(s.Used) + " tokens"
	case types.UnitCount:
		return formatUsageAmount(s.Used) + " requests"
	default:
		return fmt.Sprintf("%.2f", s.Used)
	}
}

func humanTotal(s types.UsageStats) string {
	if s.Total == 0 {
		return "—"
	}
	switch s.Unit {
	case types.UnitUSD:
		return fmt.Sprintf("$%.2f", s.Total)
	case types.UnitTokens:
		return formatUsageAmount(s.Total) + " tokens"
	case types.UnitCount:
		return formatUsageAmount(s.Total) + " requests"
	default:
		return fmt.Sprintf("%.2f", s.Total)
	}
}

// formatUsageAmount keeps fractional session counts visible. Rounding a
// nearly-full quota such as 5.9/6 up to 6/6 hides that another session may
// still be available.
func formatUsageAmount(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func aggregateAge(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	age := time.Since(t)
	if age < 0 {
		age = 0
	}
	return durationText(age)
}

// contains is retained for small package-level tests and platform code.
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// aggregateHasNetworkError reports whether any provider snapshot in agg has a
// connectivity-class error (timeout, refused, DNS, …). The tray uses it to
// retry faster while a VPN/proxy is likely blocking access, and to stop the
// accelerated retry once every provider has recovered or only permanent
// auth/config errors remain.
func aggregateHasNetworkError(agg types.Aggregate) bool {
	for _, snap := range agg.Providers {
		if types.LooksLikeNetworkError(snap.Err) {
			return true
		}
	}
	return false
}

// blockedTooltipText returns a tray tooltip that names the providers whose
// snapshots look network-blocked, or "" when none do (so the caller restores
// the default tooltip). Providers are iterated in stable registry order.
func blockedTooltipText(agg types.Aggregate) string {
	var blocked []string
	for _, name := range providers.AllNames() {
		snap, ok := agg.Providers[name]
		if !ok {
			continue
		}
		if !types.LooksLikeNetworkError(snap.Err) {
			continue
		}
		display := snap.DisplayName
		if display == "" {
			display = name
		}
		blocked = append(blocked, display)
	}
	if len(blocked) == 0 {
		return ""
	}
	return "plan-usage: " + strings.Join(blocked, ", ") + " · VPN/proxy may be blocking access"
}

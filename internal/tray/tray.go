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
	popupWidth         = 960
	popupHeight        = 680
	popupGridColumns   = 4
	popupBorder        = 8
	popupSpacing       = 6
	popupCardSpacing   = 4
	popupModelMaxChars = 24
	// popupCardTitleMaxChars and popupLineMaxChars bound the minimum width a
	// single card can demand. The popup places cards in a 4-column
	// homogeneous grid, so the widest card's minimum is multiplied by four;
	// without these caps the popup could never shrink below ~1000px and
	// would overflow small or HiDPI displays.
	popupCardTitleMaxChars = 24
	popupLineMaxChars      = 36
	// popupEdgeMargin keeps the popup clear of every screen edge. An
	// edge-positioned system tray bar (top-right in the reference i3 +
	// polybar setup) therefore stays visible and clickable, and there is
	// always a clickable "outside" area left to dismiss the popup.
	popupEdgeMargin = 32
	// popupPointerOffset moves the popup away from the click point so it
	// never covers the tray icon that opened it.
	popupPointerOffset = 16
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

// clip truncates s to at most max runes, appending "…" when truncated. It
// bounds the minimum width a label can demand even when the text contains a
// single unbreakable token, because GTK labels only wrap at word boundaries
// and an over-long word would otherwise force the popup wider than its
// monitor.
func clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
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

// popupInnerRect returns the monitor area the popup is allowed to occupy:
// the work area shrunk by popupEdgeMargin on every side. Keeping the popup
// strictly inside this rect guarantees that an edge-positioned system tray
// bar stays visible and that a clickable "outside" region always remains,
// so outside-click-to-hide and the tray icon keep working.
func popupInnerRect(work Rect) Rect {
	m := popupEdgeMargin
	inner := work
	inner.X += m
	inner.Width -= 2 * m
	if inner.Width < 0 {
		inner.Width = 0
	}
	inner.Y += m
	inner.Height -= 2 * m
	if inner.Height < 0 {
		inner.Height = 0
	}
	return inner
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

package tray

import (
	"strings"
	"testing"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

func TestBuildCardsIncludesEnabledProvidersInRegistryOrder(t *testing.T) {
	cfg := &config.Config{Enabled: []string{"freebuff", "codex"}}
	agg := types.Aggregate{Providers: map[string]types.Snapshot{
		"freebuff": {DisplayName: "Freebuff", Icon: "F", FreeModels: []types.FreeModel{{Label: "must not render"}}, Usage: &types.UsageStats{Used: 2, Total: 4, Unit: types.UnitCount}},
		"codex":    {DisplayName: "Codex", Icon: "C", Err: "not authenticated"},
	}}
	cards := BuildCards(cfg, agg)
	if len(cards) != 2 {
		t.Fatalf("BuildCards() returned %d cards, want 2", len(cards))
	}
	if cards[0].Name != "codex" || cards[1].Name != "freebuff" {
		t.Fatalf("card order = [%s %s], want [codex freebuff]", cards[0].Name, cards[1].Name)
	}
	if cards[0].Error != "not authenticated" {
		t.Fatalf("codex error = %q, want auth error", cards[0].Error)
	}
	if len(cards[1].Windows) != 1 || cards[1].Windows[0].Percent != 50 {
		t.Fatalf("freebuff windows = %#v, want one 50%% window", cards[1].Windows)
	}
	for _, card := range cards {
		for _, window := range card.Windows {
			if strings.Contains(window.Label, "must not render") {
				t.Fatal("FreeModels leaked into popup card")
			}
		}
	}
}

func TestBuildCardsRendersEveryUsageWindow(t *testing.T) {
	cfg := &config.Config{Enabled: []string{"codex"}}
	agg := types.Aggregate{Providers: map[string]types.Snapshot{
		"codex": {
			Windows: []types.UsageStats{
				{WindowLabel: "5h", Used: 3, Total: 10, Unit: types.UnitUSD, ResetIn: time.Hour},
				{WindowLabel: "weekly", Used: 12, Total: 30, Unit: types.UnitUSD, Note: "shared quota"},
			},
		},
	}}
	cards := BuildCards(cfg, agg)
	if len(cards) != 1 || len(cards[0].Windows) != 2 {
		t.Fatalf("multi-window cards = %#v, want one card with 2 windows", cards)
	}
	if cards[0].Windows[0].Label != "5h" || cards[0].Windows[1].Label != "weekly" {
		t.Fatalf("window labels = %#v, want [5h weekly]", cards[0].Windows)
	}
}

func TestPopupClickOutsideChecksAllEdges(t *testing.T) {
	cases := []struct {
		name string
		x, y float64
		want bool
	}{
		{"inside", 10, 10, false},
		{"left", -1, 10, true},
		{"top", 10, -1, true},
		{"right", 440, 10, true},
		{"bottom", 10, 620, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PopupClickOutside(tc.x, tc.y, 440, 620); got != tc.want {
				t.Fatalf("PopupClickOutside(%v,%v) = %v, want %v", tc.x, tc.y, got, tc.want)
			}
		})
	}
}

func TestPopupPositionClampsEveryMonitorEdge(t *testing.T) {
	work := Rect{X: -1920, Y: 40, Width: 1920, Height: 1040}
	cases := []struct {
		name         string
		pointerX     int
		pointerY     int
		wantX, wantY int
	}{
		{"left-top", -2000, 0, -1920, 40},
		{"right-bottom", 100, 2000, -480, 480},
		{"inside", -1000, 300, -1000, 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, y := PopupPosition(tc.pointerX, tc.pointerY, 480, 600, work)
			if x != tc.wantX || y != tc.wantY {
				t.Fatalf("PopupPosition() = (%d,%d), want (%d,%d)", x, y, tc.wantX, tc.wantY)
			}
		})
	}
}

func TestCheckDesktopSessionRejectsWaylandAndMissingDisplay(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")
	t.Setenv("XDG_SESSION_TYPE", "wayland")
	if err := checkDesktopSession(); err == nil || !strings.Contains(err.Error(), "Wayland") {
		t.Fatalf("Wayland check error = %v, want a clear Wayland error", err)
	}

	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("XDG_SESSION_TYPE", "")
	if err := checkDesktopSession(); err == nil || !strings.Contains(err.Error(), "DISPLAY") {
		t.Fatalf("missing-display check error = %v, want a clear DISPLAY error", err)
	}
}

func TestRefreshGateSerializesManualAndTimerRefreshes(t *testing.T) {
	var gate RefreshGate
	if !gate.TryBegin() {
		t.Fatal("first refresh did not acquire gate")
	}
	if gate.TryBegin() {
		t.Fatal("concurrent refresh acquired gate")
	}
	gate.End()
	if !gate.TryBegin() {
		t.Fatal("gate did not reopen after refresh completed")
	}
	gate.End()
}

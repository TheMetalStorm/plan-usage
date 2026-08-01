package tray

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/providers"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

func TestBuildCardsIncludesEnabledProvidersInRegistryOrder(t *testing.T) {
	cfg := &config.Config{Enabled: []string{"freebuff", "codex"}}
	agg := types.Aggregate{Providers: map[string]types.Snapshot{
		"freebuff": {DisplayName: "Freebuff", Icon: "F", FreeModels: []types.FreeModel{{Label: "free model"}}, Usage: &types.UsageStats{Used: 2, Total: 4, Unit: types.UnitCount}},
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
	if len(cards[1].Models) != 1 || cards[1].Models[0].Label != "free model" {
		t.Fatalf("freebuff models = %#v, want the snapshot model catalog", cards[1].Models)
	}
	if got := freeModelsOnly([]types.FreeModel{{Label: "paid", Premium: true}, {Label: "free"}}); len(got) != 1 || got[0].Label != "free" {
		t.Fatalf("freeModelsOnly() = %#v, want only the free model", got)
	}
	if got := premiumModelsOnly([]types.FreeModel{{Label: "paid", Premium: true}, {Label: "free"}}); len(got) != 1 || got[0].Label != "paid" {
		t.Fatalf("premiumModelsOnly() = %#v, want only the premium model", got)
	}
	if got := resetText(types.UsageStats{ResetIn: 90 * time.Minute}); got != "resets in 1h 30m" {
		t.Fatalf("resetText() = %q, want a relative reset", got)
	}
}

func TestPremiumModelsAreOnlyEligibleForFreebuffSection(t *testing.T) {
	models := []types.FreeModel{{Label: "standard"}, {Label: "premium", Premium: true}}
	if got := freeModelsOnly(models); len(got) != 1 || got[0].Label != "standard" {
		t.Fatalf("free models = %#v, want standard only", got)
	}
	if got := premiumModelsOnly(models); len(got) != 1 || got[0].Label != "premium" {
		t.Fatalf("premium models = %#v, want premium only", got)
	}
}

func TestUsageFormattingPreservesFractionalSessionCounts(t *testing.T) {
	stats := types.UsageStats{Used: 5.9, Total: 6, Unit: types.UnitCount}
	if got := humanValue(stats); got != "5.9 requests" {
		t.Fatalf("humanValue(5.9) = %q, want 5.9 requests", got)
	}
	if got := humanTotal(stats); got != "6 requests" {
		t.Fatalf("humanTotal(6) = %q, want 6 requests", got)
	}

	tokens := types.UsageStats{Used: 5.9, Total: 6, Unit: types.UnitTokens}
	if got := humanValue(tokens); got != "5.9 tokens" {
		t.Fatalf("humanValue(5.9 tokens) = %q, want 5.9 tokens", got)
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
		{"top", 10, -1, true}, {"right", popupWidth, 10, true},
		{"bottom", 10, popupHeight, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PopupClickOutside(tc.x, tc.y, popupWidth, popupHeight); got != tc.want {
				t.Fatalf("PopupClickOutside(%v,%v) = %v, want %v", tc.x, tc.y, got, tc.want)
			}
		})
	}
}

func TestPopupClickDebounced(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		shown time.Time
		now   time.Time
		want  bool
	}{
		{"zero-shown-never-debounced", time.Time{}, now, false},
		{"instant", now, now, true},
		{"just-under-window", now, now.Add(popupClickDebounce - time.Millisecond), true},
		{"at-window-boundary", now, now.Add(popupClickDebounce), false},
		{"well-after", now, now.Add(5 * time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := popupClickDebounced(tc.shown, tc.now); got != tc.want {
				t.Fatalf("popupClickDebounced(shown=%v, now=%v) = %v, want %v", tc.shown, tc.now, got, tc.want)
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
		{"right-bottom", 100, 2000, work.X + work.Width - popupWidth, work.Y + work.Height - popupHeight},
		{"inside", -1600, 200, -1600, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, y := PopupPosition(tc.pointerX, tc.pointerY, popupWidth, popupHeight, work)
			if x != tc.wantX || y != tc.wantY {
				t.Fatalf("PopupPosition() = (%d,%d), want (%d,%d)", x, y, tc.wantX, tc.wantY)
			}
		})
	}
}

func TestPopupSizeFitsMonitorWorkarea(t *testing.T) {
	work := Rect{Width: 800, Height: 600}
	width, height := popupSizeForWorkarea(popupWidth, popupHeight, work)
	if width != 800 || height != 600 {
		t.Fatalf("popupSizeForWorkarea() = (%d,%d), want (800,600)", width, height)
	}

	work = Rect{Width: 1920, Height: 1040}
	width, height = popupSizeForWorkarea(popupWidth, popupHeight, work)
	if width != popupWidth || height != popupHeight {
		t.Fatalf("popupSizeForWorkarea() = (%d,%d), want (%d,%d)", width, height, popupWidth, popupHeight)
	}
}

func TestPopupInnerRectShrinksByMargin(t *testing.T) {
	work := Rect{X: 100, Y: 50, Width: 1920, Height: 1080}
	inner := popupInnerRect(work)
	want := Rect{X: 100 + popupEdgeMargin, Y: 50 + popupEdgeMargin, Width: 1920 - 2*popupEdgeMargin, Height: 1080 - 2*popupEdgeMargin}
	if inner != want {
		t.Fatalf("popupInnerRect() = %+v, want %+v", inner, want)
	}
}

func TestPopupInnerRectDegenerateWorkArea(t *testing.T) {
	// Tiny work areas must not go negative; zero width/height are allowed
	// but the helpers must stay deterministic.
	inner := popupInnerRect(Rect{Width: 10, Height: 10})
	if inner.Width < 0 || inner.Height < 0 || inner.X < 0 || inner.Y < 0 {
		t.Fatalf("popupInnerRect() = %+v, want non-negative", inner)
	}
	if inner := popupInnerRect(Rect{X: 5, Y: 5, Width: 100, Height: 100}); inner.Width != 100-2*popupEdgeMargin || inner.Height != 100-2*popupEdgeMargin {
		t.Fatalf("popupInnerRect() = %+v, want shrunk by margin", inner)
	}
}

func TestPopupSizeNeverExceedsInnerWorkarea(t *testing.T) {
	// A default size larger than the usable monitor area must be clamped
	// down to the inner rect so the popup can never be bigger than the
	// monitor.
	inner := popupInnerRect(Rect{Width: 1024, Height: 768}) // 960x704
	width, height := popupSizeForWorkarea(popupWidth+200, popupHeight+200, inner)
	if width != inner.Width || height != inner.Height {
		t.Fatalf("popupSizeForWorkarea() = (%d,%d), want clamped to inner (%d,%d)", width, height, inner.Width, inner.Height)
	}
	// A fitting default stays unchanged and still fits inside the inner rect.
	width, height = popupSizeForWorkarea(popupWidth, popupHeight, inner)
	if width != popupWidth || height != popupHeight {
		t.Fatalf("popupSizeForWorkarea() = (%d,%d), want (%d,%d)", width, height, popupWidth, popupHeight)
	}
	if width > inner.Width || height > inner.Height {
		t.Fatalf("popup (%d,%d) exceeds inner (%d,%d)", width, height, inner.Width, inner.Height)
	}
}

func TestPopupPositionWithOffsetStaysInsideWork(t *testing.T) {
	inner := popupInnerRect(Rect{X: 0, Y: 0, Width: 1920, Height: 1080})
	cases := []struct {
		name     string
		pointerX int
		pointerY int
	}{
		{"tray corner", inner.X + inner.Width - 60, inner.Y + 30},
		{"top-left", inner.X - 100, inner.Y - 100},
		{"bottom-right", inner.X + inner.Width + 100, inner.Y + inner.Height + 100},
		{"middle", inner.X + inner.Width/2, inner.Y + inner.Height/2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, y := PopupPosition(tc.pointerX+popupPointerOffset, tc.pointerY+popupPointerOffset, popupWidth, popupHeight, inner)
			if x < inner.X || y < inner.Y {
				t.Fatalf("PopupPosition() = (%d,%d): outside inner top-left %+v", x, y, inner)
			}
			if x+popupWidth > inner.X+inner.Width {
				t.Fatalf("PopupPosition() x=%d: popup right edge %d exceeds inner right edge %d", x, x+popupWidth, inner.X+inner.Width)
			}
			if y+popupHeight > inner.Y+inner.Height {
				t.Fatalf("PopupPosition() y=%d: popup bottom edge %d exceeds inner bottom edge %d", y, y+popupHeight, inner.Y+inner.Height)
			}
		})
	}
}

func TestClipBoundsUnbreakableText(t *testing.T) {
	if got := clip("short", 10); got != "short" {
		t.Fatalf("clip(short) = %q, want unchanged", got)
	}
	got := clip("claude-sonnet-4-5-20250929-superlongunbreakabletoken", 24)
	if len([]rune(got)) != 25 || got[len(got)-3:] != "…" {
		t.Fatalf("clip(long,24) = %q (runes=%d), want 24 runes + …", got, len([]rune(got)))
	}
	if got := clip("", 5); got != "" {
		t.Fatalf("clip(\"\") = %q, want empty", got)
	}
	if got := clip("abc", 0); got != "" {
		t.Fatalf("clip(abc,0) = %q, want empty", got)
	}
	if got := clip("ümlaut-zeichen", 5); len([]rune(got)) != 6 || got[len(got)-3:] != "…" {
		t.Fatalf("clip(ümlaut,5) = %q (runes=%d), want 5 runes + …", got, len([]rune(got)))
	}
}

func TestPopupGridPositionUsesFourColumns(t *testing.T) {
	cases := []struct {
		index      int
		wantColumn int
		wantRow    int
	}{
		{index: 0, wantColumn: 0, wantRow: 0},
		{index: 3, wantColumn: 3, wantRow: 0},
		{index: 4, wantColumn: 0, wantRow: 1},
		{index: 7, wantColumn: 3, wantRow: 1},
	}
	for _, tc := range cases {
		column, row := popupGridPosition(tc.index)
		if column != tc.wantColumn || row != tc.wantRow {
			t.Errorf("popupGridPosition(%d) = (%d,%d), want (%d,%d)", tc.index, column, row, tc.wantColumn, tc.wantRow)
		}
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

func TestAggregateHasNetworkError(t *testing.T) {
	blocked := types.Aggregate{Providers: map[string]types.Snapshot{
		"freebuff": {Err: "dial tcp: connection refused"},
	}}
	if !aggregateHasNetworkError(blocked) {
		t.Fatal("want a network error to be detected")
	}
	authOnly := types.Aggregate{Providers: map[string]types.Snapshot{
		"codex": {Err: "not authenticated"},
	}}
	if aggregateHasNetworkError(authOnly) {
		t.Fatal("an auth error must not count as a network error")
	}
	if aggregateHasNetworkError(types.Aggregate{}) {
		t.Fatal("an empty aggregate must not report a network error")
	}
}

func TestBlockedTooltipTextNamesNetworkBlockedProviders(t *testing.T) {
	agg := types.Aggregate{Providers: map[string]types.Snapshot{
		"freebuff": {DisplayName: "Freebuff", Err: `Get "...": dial tcp: connection refused`},
		"codex":    {DisplayName: "Codex", Err: "not authenticated"},
	}}
	text := blockedTooltipText(agg)
	if !strings.Contains(text, "Freebuff") {
		t.Fatalf("tooltip = %q, want Freebuff named", text)
	}
	if !strings.Contains(text, "VPN") {
		t.Fatalf("tooltip = %q, want a VPN/proxy hint", text)
	}
	if strings.Contains(text, "Codex") {
		t.Fatalf("tooltip = %q, an auth error must not be listed as blocked", text)
	}
	if blockedTooltipText(types.Aggregate{Providers: map[string]types.Snapshot{
		"codex": {DisplayName: "Codex", Err: "not authenticated"},
	}}) != "" {
		t.Fatal("want an empty tooltip when no provider is network-blocked")
	}
}

func TestBuildCardsRespectsToggledOffProvider(t *testing.T) {
	cfg := &config.Config{ConfigPath: filepath.Join(t.TempDir(), "config.yaml")}
	agg := types.Aggregate{Providers: map[string]types.Snapshot{
		"codex":    {DisplayName: "Codex", Icon: "C"},
		"freebuff": {DisplayName: "Freebuff", Icon: "F"},
	}}
	if err := cfg.SetProviderEnabled(providers.AllNames(), "codex", false); err != nil {
		t.Fatal(err)
	}
	cards := BuildCards(cfg, agg)
	for _, card := range cards {
		if card.Name == "codex" {
			t.Fatal("codex card must not be built after being toggled off")
		}
	}
	if len(cards) == 0 {
		t.Fatal("other providers must still be built after one is toggled off")
	}
}

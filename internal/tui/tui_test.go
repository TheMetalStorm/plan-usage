package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/TheMetalStorm/plan-usage/internal/types"
)

func TestRenderModelSectionsSeparatesFreebuffTiers(t *testing.T) {
	var b strings.Builder
	renderModelSections(&b, []types.FreeModel{
		{Label: "Premium model", Premium: true},
		{Label: "Standard model"},
	})
	got := b.String()
	for _, want := range []string{"Premium models", "Premium model", "Standard models", "Standard model"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered model sections do not contain %q: %q", want, got)
		}
	}
}

func TestRenderModelSectionsKeepsLegacySingleBlock(t *testing.T) {
	var b strings.Builder
	renderModelSections(&b, []types.FreeModel{{Label: "Other provider model"}})
	got := b.String()
	if !strings.Contains(got, "Free models") || strings.Contains(got, "Premium models") || strings.Contains(got, "Standard models") {
		t.Fatalf("legacy model sections changed: %q", got)
	}
}

func TestHumanFormatPreservesFractionalSessionUnits(t *testing.T) {
	for _, tc := range []struct {
		used float64
		want string
	}{
		{used: 3.6, want: "3.6"},
		{used: 5.9, want: "5.9"},
	} {
		stats := &types.UsageStats{Used: tc.used, Total: 6, Unit: types.UnitCount}
		if got := humanFormat(stats); got != tc.want {
			t.Fatalf("humanFormat(%v) = %q, want %q", tc.used, got, tc.want)
		}
	}
	stats := &types.UsageStats{Used: 5.9, Total: 6, Unit: types.UnitCount}
	if got := humanTotal(stats); got != "6" {
		t.Fatalf("humanTotal(6) = %q, want 6", got)
	}
}

func TestPanelWidthsMatchDebugLayout(t *testing.T) {
	m := &Model{width: 120, debug: true}
	listW, detailW := m.panelWidths()
	if listW != 30 || detailW != 30 {
		t.Fatalf("panelWidths() = (%d, %d), want (30, 30)", listW, detailW)
	}

	m.items = []types.Snapshot{
		{Provider: "short", DisplayName: "Freebuff"},
		{Provider: "long", DisplayName: "Codex / ChatGPT", Err: "missing credentials"},
		{Provider: "usage", DisplayName: "Command Code", Usage: &types.UsageStats{Used: 9, Total: 10}},
	}
	for _, line := range strings.Split(strings.TrimSuffix(m.renderList(listW), "\n"), "\n") {
		if lipgloss.Width(line) > panelContentWidth(listW) {
			t.Errorf("list line width %d exceeds content width %d: %q", lipgloss.Width(line), panelContentWidth(listW), line)
		}
	}
}

func TestMultiWindowColumnsStayAlignedWhenNarrow(t *testing.T) {
	m := &Model{
		width: 120,
		items: []types.Snapshot{{
			Provider:    "commandcode",
			DisplayName: "Command Code",
			Windows: []types.UsageStats{
				{WindowLabel: "5h rolling", Used: 1, Total: 10, Unit: types.UnitUSD, ResetIn: time.Hour},
				{WindowLabel: "weekly", Used: 2, Total: 10, Unit: types.UnitUSD, ResetIn: 2 * time.Hour},
				{WindowLabel: "monthly", Used: 3, Total: 10, Unit: types.UnitUSD, ResetIn: 3 * time.Hour},
			},
		}},
	}

	const panelW = 30
	m.selected = 0
	lines := strings.Split(m.renderDetail(panelW), "\n")
	barStarts := make([]int, 0, 3)
	pctStarts := make([]int, 0, 3)
	for _, line := range lines {
		plain := ansi.Strip(line)
		if !strings.Contains(plain, "%") {
			continue
		}
		bar := strings.IndexAny(plain, "█░")
		if bar < 0 {
			t.Fatalf("usage row has no bar: %q", plain)
		}
		barStarts = append(barStarts, bar)
		for _, pct := range []string{"10%", "20%", "30%"} {
			if i := strings.Index(plain, pct); i >= 0 {
				pctStarts = append(pctStarts, i)
				break
			}
		}
		if lipgloss.Width(line) > panelContentWidth(panelW) {
			t.Errorf("usage row width %d exceeds content width %d: %q", lipgloss.Width(line), panelContentWidth(panelW), plain)
		}
	}
	if len(barStarts) != 3 || len(pctStarts) != 3 {
		t.Fatalf("found %d bar starts and %d percentage starts, want 3 each", len(barStarts), len(pctStarts))
	}
	for _, want := range []string{"reset in 1h", "reset in 2h", "reset in 3h"} {
		if !strings.Contains(ansi.Strip(m.renderDetail(panelW)), want) {
			t.Errorf("multi-window detail is missing %q", want)
		}
	}
	for i := 1; i < 3; i++ {
		if barStarts[i] != barStarts[0] {
			t.Errorf("bar start positions = %v, not aligned", barStarts)
		}
		if pctStarts[i] != pctStarts[0] {
			t.Errorf("percentage start positions = %v, not aligned", pctStarts)
		}
	}
}

func TestTruncateDisplayUsesTerminalWidth(t *testing.T) {
	if got := truncateDisplay("OpenCode Go", 5); lipgloss.Width(got) > 5 {
		t.Fatalf("truncateDisplay() width = %d, want <= 5 (%q)", lipgloss.Width(got), got)
	}
	if got := truncateDisplay("界界界", 3); lipgloss.Width(got) > 3 {
		t.Fatalf("truncateDisplay() wide-character width = %d, want <= 3 (%q)", lipgloss.Width(got), got)
	}
}

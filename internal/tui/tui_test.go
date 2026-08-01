package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/debug"
	"github.com/TheMetalStorm/plan-usage/internal/providers"
	"github.com/TheMetalStorm/plan-usage/internal/state"
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

func TestPickerRenderShowsAllProvidersWithCheckboxes(t *testing.T) {
	cfg := &config.Config{Enabled: []string{"codex"}}
	m := &Model{cfg: cfg, allNames: providers.AllNames(), picker: true, selected: 0, pending: map[string]bool{}}
	got := ansi.Strip(m.renderList(30))
	for _, want := range []string{
		"SHOW/HIDE PROVIDERS",
		"[x] Codex / ChatGPT",
		"[ ] OpenCode Go",
		"[ ] Cline Pass",
		"[ ] Command Code",
		"[ ] Freebuff",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("picker list missing %q:\n%s", want, got)
		}
	}
}

func TestToggleSelectedProviderHidesFromItems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{ConfigPath: path}
	cfg.Defaults()
	m := &Model{cfg: cfg, log: debug.New(64), pending: map[string]bool{}, allNames: providers.AllNames()}
	m.rebuildItems()
	if len(m.items) != len(m.allNames) {
		t.Fatalf("initial items = %d, want %d", len(m.items), len(m.allNames))
	}
	for i, s := range m.items {
		if s.Provider == "codex" {
			m.selected = i
		}
	}
	cmd := m.toggleSelectedCmd()
	if cmd == nil {
		t.Fatal("toggleSelectedCmd() returned nil")
	}
	msg := cmd()
	tm, ok := msg.(toggleDoneMsg)
	if !ok {
		t.Fatalf("cmd() = %#v, want toggleDoneMsg", msg)
	}
	if tm.name != "codex" || tm.enabled {
		t.Fatalf("toggleDoneMsg = %#v, want codex hidden", tm)
	}
	m.Update(tm)
	for _, s := range m.items {
		if s.Provider == "codex" {
			t.Fatal("codex still listed after hiding")
		}
	}
	if cfg.IsProviderEnabled("codex") {
		t.Fatal("codex still enabled after toggle")
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.IsProviderEnabled("codex") {
		t.Fatal("disk config still enables codex after toggle")
	}
}

func TestToggleSelectedProviderShowsAgainFromPicker(t *testing.T) {
	cfg := &config.Config{Enabled: []string{"freebuff"}}
	m := &Model{cfg: cfg, log: debug.New(64), pending: map[string]bool{}, allNames: providers.AllNames()}
	m.rebuildItems()
	if len(m.items) != 1 {
		t.Fatalf("items = %d, want only the one enabled provider", len(m.items))
	}
	// Picker walks the full registry; select the hidden codex.
	m.picker = true
	for i, name := range m.allNames {
		if name == "codex" {
			m.selected = i
		}
	}
	cmd := m.toggleSelectedCmd()
	if cmd == nil {
		t.Fatal("toggleSelectedCmd() returned nil")
	}
	msg := cmd()
	tm, ok := msg.(toggleDoneMsg)
	if !ok || !tm.enabled || tm.name != "codex" {
		t.Fatalf("cmd() = %#v, want codex shown", msg)
	}
	m.Update(tm)
	if !cfg.IsProviderEnabled("codex") {
		t.Fatal("codex should be enabled after showing")
	}
	found := false
	for _, s := range m.items {
		if s.Provider == "codex" {
			found = true
		}
	}
	if !found {
		t.Fatal("codex not re-added to the list after showing")
	}
}

func TestRebuildItemsFiltersDisabledProviders(t *testing.T) {
	store, err := state.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	agg := types.Aggregate{Providers: map[string]types.Snapshot{
		"codex":    {DisplayName: "Codex / ChatGPT", Icon: "C"},
		"freebuff": {DisplayName: "Freebuff", Icon: "F"},
	}}
	if err := store.Replace(agg); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Enabled: []string{"codex"}}
	m := &Model{cfg: cfg, store: store, allNames: providers.AllNames()}
	m.rebuildItems()
	if len(m.items) != 1 || m.items[0].Provider != "codex" {
		t.Fatalf("items = %#v, want only codex", m.items)
	}
}

func TestPickerModeSelectionClamping(t *testing.T) {
	cfg := &config.Config{Enabled: []string{"codex"}}
	m := &Model{cfg: cfg, allNames: providers.AllNames(), pending: map[string]bool{}}
	m.rebuildItems()
	if m.selected != 0 {
		t.Fatalf("selected = %d, want 0 after rebuild", m.selected)
	}
	m.enterPicker()
	if !m.picker {
		t.Fatal("picker not entered")
	}
	if len(m.allNames) < 3 {
		t.Fatal("test needs several registered providers")
	}
	m.selected = len(m.allNames) - 1
	m.picker = false
	m.clampSelectedToItems()
	if m.selected >= len(m.items) {
		t.Fatalf("selected = %d after exit, want < %d (items)", m.selected, len(m.items))
	}
}

func TestPickerFooterAndNormalFooterHints(t *testing.T) {
	m := &Model{picker: true}
	if got := m.renderFooter(); !strings.Contains(got, "space/enter toggle") {
		t.Fatalf("picker footer = %q, want toggle hint", got)
	}
	m.picker = false
	if got := m.renderFooter(); !strings.Contains(got, "x show/hide") {
		t.Fatalf("normal footer = %q, want show/hide hint", got)
	}
}

func TestPickerDetailShowsHiddenProvider(t *testing.T) {
	cfg := &config.Config{Enabled: []string{"codex"}}
	m := &Model{cfg: cfg, allNames: providers.AllNames(), picker: true, pending: map[string]bool{}}
	m.rebuildItems()
	for i, name := range m.allNames {
		if name == "freebuff" {
			m.selected = i
		}
	}
	got := ansi.Strip(m.renderDetail(30))
	if !strings.Contains(got, "Freebuff") {
		t.Fatalf("picker detail for a hidden provider = %q, want its display name", got)
	}
}

func TestPickerSpaceKeyTogglesSelectedProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{ConfigPath: path}
	cfg.Defaults()
	m := &Model{
		cfg:      cfg,
		log:      debug.New(64),
		pending:  map[string]bool{},
		allNames: providers.AllNames(),
		picker:   true,
	}
	m.rebuildItems()
	for i, name := range m.allNames {
		if name == "codex" {
			m.selected = i
		}
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if cmd == nil {
		t.Fatal("space key did not return a toggle command")
	}
	msg := cmd()
	toggled, ok := msg.(toggleDoneMsg)
	if !ok || toggled.name != "codex" || toggled.enabled {
		t.Fatalf("space command result = %#v, want codex hidden", msg)
	}
}

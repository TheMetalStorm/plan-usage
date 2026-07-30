package tui

import (
	"strings"
	"testing"

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
	stats := &types.UsageStats{Used: 3.6, Total: 6, Unit: types.UnitCount}
	if got := humanFormat(stats); got != "3.6" {
		t.Fatalf("humanFormat(3.6) = %q, want 3.6", got)
	}
	if got := humanTotal(stats); got != "6" {
		t.Fatalf("humanTotal(6) = %q, want 6", got)
	}
}

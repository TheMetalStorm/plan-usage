package format

import (
	"strings"
	"testing"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/types"
)

func TestAggregateIncludesPrimaryResetByDefault(t *testing.T) {
	agg := types.Aggregate{Providers: map[string]types.Snapshot{
		"codex": {
			Provider:    "codex",
			DisplayName: "Codex",
			Usage:       &types.UsageStats{Used: 2, Total: 10, ResetIn: 2 * time.Hour},
		},
	}}
	got := Aggregate(agg, "{name} {percent}%", " · ", "—", AggregateOpts{})
	if !strings.Contains(got, "codex 20% (reset 2h)") {
		t.Fatalf("Aggregate() = %q, want the primary reset", got)
	}
}

func TestAggregateIncludesEveryMultiWindowReset(t *testing.T) {
	agg := types.Aggregate{Providers: map[string]types.Snapshot{
		"opencodego": {
			Provider: "opencodego",
			Windows: []types.UsageStats{
				{WindowLabel: "5h", Used: 2, Total: 10, ResetIn: time.Hour},
				{WindowLabel: "weekly", Used: 3, Total: 10, ResetIn: 24 * time.Hour},
			},
		},
	}}
	got := Aggregate(agg, "{name}", " · ", "—", AggregateOpts{})
	for _, want := range []string{"5h:20% (reset in 1h)", "weekly:30% (reset in 1d)"} {
		if !strings.Contains(got, want) {
			t.Errorf("Aggregate() = %q, missing %q", got, want)
		}
	}
}

func TestResetHumanMarksMissingResetAsUnknown(t *testing.T) {
	if got := resetHuman(types.UsageStats{}); got != "unknown" {
		t.Fatalf("resetHuman() = %q, want unknown", got)
	}
}

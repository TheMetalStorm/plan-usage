package opencodeutil

import (
	"testing"
	"time"
)

func TestParseSubscriptionJSON_AllThree(t *testing.T) {
	const payload = `{
		"rollingUsage": {"usagePercent": 5, "resetInSec": 3600},
		"weeklyUsage": {"usagePercent": 0, "resetInSec": 259200},
		"monthlyUsage": {"usagePercent": 98, "resetInSec": 1728000}
	}`
	usage := parseSubscriptionText(payload)
	if usage == nil {
		t.Fatal("parseSubscriptionText returned nil for three-window JSON")
	}
	if usage.RollingPercent != 5 || usage.RollingReset != 3600 {
		t.Fatalf("rolling = %v/%d, want 5/3600", usage.RollingPercent, usage.RollingReset)
	}
	if usage.WeeklyPercent != 0 || usage.WeeklyReset != 259200 || !usage.HasWeekly {
		t.Fatalf("weekly = %v/%d (has=%v), want 0/259200/true", usage.WeeklyPercent, usage.WeeklyReset, usage.HasWeekly)
	}
	if usage.MonthlyPercent != 98 || usage.MonthlyReset != 1728000 || !usage.HasMonthly {
		t.Fatalf("monthly = %v/%d (has=%v), want 98/1728000/true", usage.MonthlyPercent, usage.MonthlyReset, usage.HasMonthly)
	}
	if !usage.AnyWindow() {
		t.Fatal("AnyWindow() = false, want true")
	}
}

func TestParseSubscriptionJSON_NoMonthly(t *testing.T) {
	const payload = `{
		"rollingUsage": {"usagePercent": 5, "resetInSec": 3600},
		"weeklyUsage": {"usagePercent": 0, "resetInSec": 259200}
	}`
	usage := parseSubscriptionText(payload)
	if usage == nil {
		t.Fatal("parseSubscriptionText returned nil for rolling+weekly JSON")
	}
	if usage.RollingPercent != 5 || usage.WeeklyPercent != 0 {
		t.Fatalf("rolling/weekly = %v/%v, want 5/0", usage.RollingPercent, usage.WeeklyPercent)
	}
	if usage.HasMonthly {
		t.Fatal("HasMonthly = true, want false")
	}
}

func TestParseSubscriptionJSON_Nested(t *testing.T) {
	const payload = `{
		"data": {
			"usage": {
				"rollingUsage": {"usagePercent": 1, "resetInSec": 120},
				"weeklyUsage": {"usagePercent": 2, "resetInSec": 240},
				"monthlyUsage": {"usagePercent": 98, "resetInSec": 480}
			}
		}
	}`
	usage := parseSubscriptionText(payload)
	if usage == nil {
		t.Fatal("parseSubscriptionText returned nil for nested JSON")
	}
	if usage.MonthlyPercent != 98 || usage.MonthlyReset != 480 {
		t.Fatalf("monthly = %v/%d, want 98/480", usage.MonthlyPercent, usage.MonthlyReset)
	}
}

func TestParseSubscriptionRegex_AllThree(t *testing.T) {
	const payload = `rollingUsage:$R[10]={usagePercent:5,resetInSec:3600}weeklyUsage:$R[11]={usagePercent:0,resetInSec:259200}monthlyUsage:$R[12]={usagePercent:98,resetInSec:1728000}`
	usage := parseSubscriptionText(payload)
	if usage == nil {
		t.Fatal("parseSubscriptionText returned nil for JS-literal payload")
	}
	if usage.RollingPercent != 5 || usage.RollingReset != 3600 {
		t.Fatalf("rolling = %v/%d, want 5/3600", usage.RollingPercent, usage.RollingReset)
	}
	if usage.WeeklyPercent != 0 || usage.WeeklyReset != 259200 {
		t.Fatalf("weekly = %v/%d, want 0/259200", usage.WeeklyPercent, usage.WeeklyReset)
	}
	if usage.MonthlyPercent != 98 || usage.MonthlyReset != 1728000 || !usage.HasMonthly {
		t.Fatalf("monthly = %v/%d (has=%v), want 98/1728000/true", usage.MonthlyPercent, usage.MonthlyReset, usage.HasMonthly)
	}
}

func TestParseSubscriptionRegex_NoMonthly(t *testing.T) {
	const payload = `rollingUsage:$R[10]={usagePercent:5,resetInSec:3600}weeklyUsage:$R[11]={usagePercent:0,resetInSec:259200}`
	usage := parseSubscriptionText(payload)
	if usage == nil {
		t.Fatal("parseSubscriptionText returned nil for rolling+weekly literals")
	}
	if usage.RollingPercent != 5 || usage.WeeklyPercent != 0 {
		t.Fatalf("rolling/weekly = %v/%v, want 5/0", usage.RollingPercent, usage.WeeklyPercent)
	}
	if usage.HasMonthly {
		t.Fatal("HasMonthly = true, want false")
	}
}

func TestBuildUsage_RequiresResetAndPercent(t *testing.T) {
	now := time.Now()
	// Rolling is missing resetInSec -> skipped; weekly complete -> kept;
	// monthly empty map -> skipped.
	rolling := map[string]any{"usagePercent": 50}
	weekly := map[string]any{"usagePercent": 25, "resetInSec": 3600}
	monthly := map[string]any{}

	usage := buildUsage(rolling, weekly, monthly, now)
	if usage == nil {
		t.Fatal("buildUsage returned nil when one window is complete")
	}
	if usage.RollingReset != 0 {
		t.Fatalf("rolling reset = %d, want 0 (incomplete window must be skipped)", usage.RollingReset)
	}
	if usage.WeeklyPercent != 25 || usage.WeeklyReset != 3600 || !usage.HasWeekly {
		t.Fatalf("weekly = %v/%d (has=%v), want 25/3600/true", usage.WeeklyPercent, usage.WeeklyReset, usage.HasWeekly)
	}
	if usage.HasMonthly {
		t.Fatal("HasMonthly = true, want false")
	}

	// Nothing complete -> nil.
	empty := buildUsage(map[string]any{}, map[string]any{}, map[string]any{}, now)
	if empty != nil {
		t.Fatalf("buildUsage with no complete window = %#v, want nil", empty)
	}
}

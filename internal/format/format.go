// Package format implements the polybar format-string mini-language.
//
// Tokens: {name} {display} {icon} {percent} {used} {total} {unit}
//         {window} {reset_human} {error} {sep} {source}
package format

import (
	"fmt"
	"strings"
	"time"

	"github.com/TheMetalStorm/provider-usage/internal/types"
)

// Token expansion happens per-provider.  Empty fields are substituted with
// the alternative text (configurable).
type Vars struct {
	Name         string
	Display      string
	Icon         string
	Percent      float64
	Used         float64
	Total        float64
	Unit         string
	Window       string
	ResetHuman   string
	Error        string
	Source       string
	EmptyText    string
	NowLabel     string
}

// FromSnapshot builds Vars from a snapshot.
func FromSnapshot(s types.Snapshot, emptyText string) Vars {
	v := Vars{
		Name:       s.Provider,
		Display:    s.DisplayName,
		Icon:       s.Icon,
		EmptyText:  emptyText,
		Window:     "",
		ResetHuman: "—",
	}
	if s.Err != "" {
		v.Error = s.Err
	}
	if s.Usage != nil {
		u := s.Usage
		v.Percent = u.Percent()
		v.Used = u.Used
		v.Total = u.Total
		v.Unit = unitLabel(u.Unit)
		v.Window = u.WindowLabel
		if !u.ResetAt.IsZero() {
			v.ResetHuman = humanDuration(time.Until(u.ResetAt))
		} else if u.ResetIn > 0 {
			v.ResetHuman = humanDuration(u.ResetIn)
		}
		v.Source = lastProbe(u)
	}
	return v
}

func unitLabel(u types.UsageUnit) string {
	switch u {
	case types.UnitUSD:
		return "$"
	case types.UnitCount:
		return "msg"
	case types.UnitTokens:
		return "tok"
	default:
		return ""
	}
}

func lastProbe(u *types.UsageStats) string {
	if u.LastProbeAt.IsZero() {
		return ""
	}
	return humanDuration(time.Since(u.LastProbeAt)) + " ago"
}

// humanDuration renders a short "3h 14m" style duration.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) - days*24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, h)
}

// Render expands tokens in tmpl.  Unrecognised tokens are left intact so
// the user can spot typos in the polybar config.
func Render(tmpl string, v Vars) string {
	if tmpl == "" {
		return ""
	}
	out := tmpl
	out = replace(out, "{name}", v.Name)
	out = replace(out, "{display}", v.Display)
	out = replace(out, "{icon}", v.Icon)
	out = replace(out, "{percent}", formatPercent(v.Percent))
	out = replace(out, "{used}", formatFloat(v.Used))
	out = replace(out, "{total}", formatFloat(v.Total))
	out = replace(out, "{unit}", v.Unit)
	out = replace(out, "{window}", v.Window)
	out = replace(out, "{reset_human}", v.ResetHuman)
	out = replace(out, "{error}", v.Error)
	out = replace(out, "{source}", v.Source)
	out = replace(out, "{sep}", v.EmptyText)
	return out
}

func replace(s, key, val string) string {
	if val == "" {
		val = "—"
	}
	return strings.ReplaceAll(s, key, val)
}

func formatPercent(f float64) string {
	if f <= 0 {
		return "0"
	}
	if f >= 100 {
		return "100"
	}
	return fmt.Sprintf("%.0f", f)
}

func formatFloat(f float64) string {
	if f == 0 {
		return "0"
	}
	if f < 10 {
		return fmt.Sprintf("%.2f", f)
	}
	return fmt.Sprintf("%.0f", f)
}

// compactWindows renders " 5h:0% wk:0% mo:0%" from the windows list.
// Used by Aggregate() to append a suffix for multi-window providers
// (e.g. OpenCode Go).
func compactWindows(ws []types.UsageStats) string {
	out := ""
	for _, w := range ws {
		out += fmt.Sprintf(" %s:%d%%", w.WindowLabel, int(w.Percent()))
	}
	return out
}

// Aggregate renders the polybar widget line from a state aggregate.
//
// The format strings come from cfg.Polybar.Format (per-provider) and
// cfg.Polybar.Separator (between providers).
func Aggregate(agg types.Aggregate, perProviderTmpl, separator, noAuthText string, opts AggregateOpts) string {
	parts := make([]string, 0, len(agg.Providers))
	names := make([]string, 0, len(agg.Providers))
	for n := range agg.Providers {
		names = append(names, n)
	}
	sortStrings(names)
	for _, name := range names {
		s := agg.Providers[name]
		vars := FromSnapshot(s, noAuthText)
		// Hide entries that have neither usage data nor any window
		// scaffolding (i.e. nothing useful to show).
		if opts.HideIfNoAuth && (s.Err != "" && s.Usage == nil && len(s.Windows) == 0) {
			continue
		}
		base := Render(perProviderTmpl, vars)
		// Append compact window summary for multi-window providers.
		if len(s.Windows) > 1 {
			base += compactWindows(s.Windows)
		}
		parts = append(parts, base)
	}
	return strings.Join(parts, separator)
}

// AggregateOpts configures Aggregate.
type AggregateOpts struct {
	HideIfNoAuth bool
}

// sortStrings is a tiny insertion sort for deterministic iteration.
func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1] > in[j]; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
}

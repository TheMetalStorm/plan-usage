// Package tui is the Charm/Bubble Tea dashboard.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TheMetalStorm/plan-usage/internal/config"
	"github.com/TheMetalStorm/plan-usage/internal/debug"
	"github.com/TheMetalStorm/plan-usage/internal/providers"
	"github.com/TheMetalStorm/plan-usage/internal/state"
	"github.com/TheMetalStorm/plan-usage/internal/types"
)

// Model is the bubbletea root model.
type Model struct {
	cfg         *config.Config
	store       *state.Store
	log         *debug.Log
	selected    int
	items       []types.Snapshot
	debug       bool
	lastRefresh time.Time
	width       int
	height      int
	pending     map[string]bool // providers with an in-flight probe
}

// New builds the initial model.  Backed by the on-disk snapshot; if it is
// empty, the model is seeded with per-provider errors so the table has
// rows immediately.
func New(cfg *config.Config, store *state.Store) *Model {
	m := &Model{
		cfg:     cfg,
		store:   store,
		log:     debug.New(64),
		debug:   cfg.Debug,
		pending: map[string]bool{},
		items:   loadItems(store),
	}
	if len(m.items) == 0 {
		m.items = seedFromProviders(cfg)
	}
	return m
}

// RunProgram launches bubbletea for the given model and blocks until
// the user quits.
func RunProgram(m *Model) error {
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// ---------- tea.Model interface ----------

// Init triggers an immediate refresh for every enabled, configured
// provider.  It does NOT write to the on-disk snapshot -- only the
// daemon owns that file.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tickEvery(2 * time.Second)}
	m.lastRefresh = time.Now()
	for _, p := range providers.All() {
		name := p.Name()
		if !m.cfg.IsProviderEnabled(name) {
			continue
		}
		if err := p.IsConfigured(); err != nil {
			continue
		}
		cmds = append(cmds, m.refreshOneCmd(name))
	}
	return tea.Batch(cmds...)
}

// Update reacts to keys, ticks and background probe completions.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch mm := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = mm.Width, mm.Height
		return m, nil

	case tickMsg:
		// Auto-refresh stale (>90s) snapshots in the background.
		m.lastRefresh = time.Now()
		var cmds []tea.Cmd
		for name := range m.pending {
			cmds = append(cmds, m.refreshOneCmd(name))
		}
		if len(cmds) > 0 {
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case probeDoneMsg:
		delete(m.pending, mm.name)
		for i := range m.items {
			if m.items[i].Provider == mm.name {
				m.items[i] = mm.snap
				break
			}
		}
		m.log.Ok(mm.name, "probe ok")
		return m, nil

	case probeErrMsg:
		delete(m.pending, mm.name)
		m.log.Error(mm.name, "probe failed: %s", mm.err)
		for i := range m.items {
			if m.items[i].Provider == mm.name {
				m.items[i].Err = mm.err
				break
			}
		}
		return m, nil

	case tea.KeyMsg:
		switch {
		case keyMatches(mm, "q", "ctrl+c"):
			return m, tea.Quit
		case keyMatches(mm, "up", "k"):
			if m.selected > 0 {
				m.selected--
			}
		case keyMatches(mm, "down", "j"):
			if m.selected < len(m.items)-1 {
				m.selected++
			}
		case keyMatches(mm, "r"):
			return m, m.refreshSelectedCmd()
		case keyMatches(mm, "R"):
			return m, m.refreshAllCmd()
		case keyMatches(mm, "D"):
			m.debug = !m.debug
		}

	case tea.MouseMsg:
		return m, m.handleMouse(mm)
	}
	return m, nil
}

// panelWidths returns the total rendered widths assigned to the list and
// detail panels. Keeping this in one place prevents renderers from measuring
// themselves against a different layout than View actually renders.
func (m *Model) panelWidths() (listW, detailW int) {
	if m.debug && m.width >= 120 {
		each := m.width / 4
		return each, each
	}

	listW = m.width / 3
	if listW < 22 {
		listW = 22
	}
	detailW = m.width - listW
	if detailW < 18 {
		detailW = 18
	}
	return listW, detailW
}

// handleMouse processes mouse events for the TUI.
func (m *Model) handleMouse(mm tea.MouseMsg) tea.Cmd {
	listW, _ := m.panelWidths()

	switch mm.Type {
	case tea.MouseLeft:
		// Click in the left panel (provider list).
		// Layout (0-indexed Y):
		//   Y=0: header
		//   Y=1: panel top border
		//   Y=2: "PROVIDERS" header
		//   Y=3: blank line (margin-bottom from listHeaderStyle)
		//   Y=4+: provider entries (2 lines each: name + age/probing)
		if int(mm.X) < listW && int(mm.Y) >= 4 {
			idx := (int(mm.Y) - 4) / 2
			if idx < len(m.items) {
				m.selected = idx
			}
			return nil
		}

	case tea.MouseWheelUp:
		if m.selected > 0 {
			m.selected--
		}
		return nil

	case tea.MouseWheelDown:
		if m.selected < len(m.items)-1 {
			m.selected++
		}
		return nil
	}
	return nil
}

func (m *Model) View() string {
	if m.width == 0 {
		m.width = 110
	}
	if m.width < 80 {
		m.width = 80
	}
	useDebugColumns := m.debug && m.width >= 120
	header := m.renderHeader()
	footer := m.renderFooter()
	var body string
	if useDebugColumns {
		each := m.width / 4
		cols := []string{
			panel(m.renderList(each), each),
			panel(m.renderDetail(each), each),
			panel(m.renderLog(), each),
		}
		body = lipgloss.JoinHorizontal(lipgloss.Top, cols...)
	} else {
		listW, detailW := m.panelWidths()
		body = lipgloss.JoinHorizontal(lipgloss.Top,
			panel(m.renderList(listW), listW),
			panel(m.renderDetail(detailW), detailW),
		)
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		headerStyle.Render(header),
		body,
		footerStyle.Render(footer),
	)
}

// ---------- commands ----------

type tickMsg time.Time
type probeDoneMsg struct {
	name string
	snap types.Snapshot
}
type probeErrMsg struct {
	name string
	err  string
}

func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// refreshOneCmd returns a tea.Cmd that probes one provider and posts
// the result back via probeDoneMsg / probeErrMsg.  Multi-window plans
// are attached by providers.EnrichWindows before the message is sent.
func (m *Model) refreshOneCmd(name string) tea.Cmd {
	m.pending[name] = true
	p, err := providers.Get(name)
	if err != nil {
		return func() tea.Msg { return probeErrMsg{name, err.Error()} }
	}
	return func() (msg tea.Msg) {
		var snap types.Snapshot
		defer func() {
			if r := recover(); r != nil {
				msg = probeErrMsg{name: name, err: fmt.Sprintf("panic: %v", r)}
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		snap = types.Snapshot{
			Provider:    name,
			DisplayName: p.DisplayName(),
			Icon:        p.Icon(),
			FreeModels:  p.AvailableModels(),
			RefreshedAt: time.Now(),
		}
		stats, err := p.FetchUsage(ctx)
		if err != nil {
			snap.Err = err.Error()
			return probeErrMsg{name: name, err: err.Error()}
		}
		if stats != nil {
			snap.Usage = stats
			if stats.Error != "" {
				snap.Err = stats.Error
			}
			if !stats.LastProbeAt.IsZero() {
				snap.RefreshedAt = stats.LastProbeAt
			}
		}
		providers.EnrichWindows(p, &snap)
		return probeDoneMsg{name: name, snap: snap}
	}
}

// refreshSelectedCmd fires a probe for the highlighted provider.
func (m *Model) refreshSelectedCmd() tea.Cmd {
	if m.selected < 0 || m.selected >= len(m.items) {
		return nil
	}
	name := m.items[m.selected].Provider
	p, err := providers.Get(name)
	if err != nil {
		m.log.Error(name, "lookup: %v", err)
		return nil
	}
	if e := p.IsConfigured(); e != nil {
		m.log.Warn(name, "skip refresh: %v", e)
		return nil
	}
	m.log.Provider(name, "refresh requested")
	return m.refreshOneCmd(name)
}

// refreshAllCmd fires probes for every enabled provider.
func (m *Model) refreshAllCmd() tea.Cmd {
	m.log.Provider("daemon", "refresh-all requested")
	cmds := make([]tea.Cmd, 0, len(m.items))
	for _, s := range m.items {
		name := s.Provider
		if !m.cfg.IsProviderEnabled(name) {
			continue
		}
		if _, busy := m.pending[name]; busy {
			continue
		}
		p, err := providers.Get(name)
		if err != nil {
			continue
		}
		if cfgErr := p.IsConfigured(); cfgErr != nil {
			continue
		}
		cmds = append(cmds, m.refreshOneCmd(name))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// ---------- helpers ----------

func loadItems(store *state.Store) []types.Snapshot {
	agg := store.All()
	out := make([]types.Snapshot, 0, len(agg.Providers))
	for name, snap := range agg.Providers {
		snap.Provider = name
		out = append(out, snap)
	}
	sortByProvider(out)
	return out
}

func sortByProvider(s []types.Snapshot) {
	priority := map[string]int{
		"opencodego": 0, "codex": 1,
		"clinepass": 2, "commandcode": 3, "freebuff": 4,
	}
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && priority[s[j-1].Provider] > priority[s[j].Provider]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func seedFromProviders(cfg *config.Config) []types.Snapshot {
	out := make([]types.Snapshot, 0)
	for _, p := range providers.All() {
		name := p.Name()
		if !cfg.IsProviderEnabled(name) {
			continue
		}
		s := types.Snapshot{
			Provider:    name,
			DisplayName: p.DisplayName(),
			Icon:        p.Icon(),
			FreeModels:  p.AvailableModels(),
		}
		if err := p.IsConfigured(); err != nil {
			s.Err = err.Error()
		}
		// Multi-window providers carry their plan scaffold with them;
		// surface it immediately so the user sees the bars before the
		// first live probe completes.
		providers.EnrichWindows(p, &s)
		out = append(out, s)
	}
	sortByProvider(out)
	return out
}

func keyMatches(k tea.KeyMsg, want ...string) bool {
	for _, w := range want {
		if k.String() == w {
			return true
		}
	}
	return false
}

// ---------- view rendering ----------

func (m *Model) renderHeader() string {
	age := time.Since(m.lastRefresh)
	title := titleStyle.Render("plan-usage")
	subtitle := subStyle.Render("multi-provider coding-plan monitor")
	right := subStyle.Render(fmt.Sprintf("⟳ refreshed %s ago", shortDur(age)))
	gap := m.width - lipgloss.Width(title) - lipgloss.Width(subtitle) - lipgloss.Width(right) - 6
	if gap < 1 {
		gap = 1
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, title, " ", subtitle, strings.Repeat(" ", gap), right)
}

func (m *Model) renderList(panelW int) string {
	var b strings.Builder
	b.WriteString(listHeaderStyle.Render("PROVIDERS"))
	b.WriteString("\n")

	// Available content width inside the list panel:
	//   panel(colW) uses Width(colW - 2) before borders, and the style
	//   has Padding(0,1) = 2 cols padding + 2 cols border.
	//   So the visible content area = colW - 4.
	contentW := panelContentWidth(panelW)
	if contentW < 8 {
		contentW = 8
	}

	// First pass: find the widest badge so we can use a single fixed name
	// width for all rows. This ensures badges start at the same column
	// regardless of how wide each badge is.
	maxBadgeW := 0
	type rowBadge struct {
		str string
		col lipgloss.Color
	}
	badges := make([]rowBadge, len(m.items))
	for i, s := range m.items {
		badge, col := statusBadge(s)
		badgeStr := lipgloss.NewStyle().Foreground(col).Render(badge)
		badges[i] = rowBadge{badgeStr, col}
		if bw := lipgloss.Width(badgeStr); bw > maxBadgeW {
			maxBadgeW = bw
		}
	}

	// Fixed name column width — same for every row
	fixedNameW := contentW - 2 - 1 - maxBadgeW
	if fixedNameW < 4 {
		fixedNameW = 4
	}
	nameStyle := lipgloss.NewStyle().Width(fixedNameW).MaxWidth(fixedNameW)

	for i, s := range m.items {
		cursor := "  "
		if i == m.selected {
			cursor = "▸ "
		}

		// Truncate display name to fixedNameW (display-width aware)
		name := truncateDisplay(s.DisplayName, fixedNameW)

		// Render the fixed-width name column; every row gets the same width
		nameRendered := nameStyle.Render(name)

		row := cursor + nameRendered + " " + badges[i].str
		if i == m.selected {
			row = selectedStyle.Render(row)
		}
		b.WriteString(row)
		b.WriteString("\n")
		if _, busy := m.pending[s.Provider]; busy {
			writeStyledLine(&b, subStyle, "   ⟳ probing…")
		} else if !s.RefreshedAt.IsZero() {
			writeStyledLine(&b, subStyle, fmt.Sprintf("   %s ago", shortDur(time.Since(s.RefreshedAt))))
		}
	}
	if len(m.items) == 0 {
		writeStyledLine(&b, subStyle, "  (no providers)")
	}
	return b.String()
}

func statusBadge(s types.Snapshot) (string, lipgloss.Color) {
	if s.Err != "" && s.Usage == nil && len(s.Windows) == 0 {
		return "✗ no-auth", red
	}
	if s.Usage == nil && len(s.Windows) == 0 {
		return "—", grey
	}
	// Pick the highest pressure signal across windows.
	winnerPct := 0.0
	if s.Usage != nil {
		winnerPct = s.Usage.Percent()
	}
	for _, w := range s.Windows {
		if w.Percent() > winnerPct {
			winnerPct = w.Percent()
		}
	}
	if winnerPct >= 90 {
		return fmt.Sprintf("⚠ %.0f%%", winnerPct), red
	}
	if winnerPct >= 70 {
		return fmt.Sprintf("▲ %.0f%%", winnerPct), yellow
	}
	return fmt.Sprintf("● %.0f%%", winnerPct), green
}

func (m *Model) renderDetail(panelW int) string {
	if len(m.items) == 0 || m.selected >= len(m.items) {
		return "no provider\n"
	}
	s := m.items[m.selected]
	var b strings.Builder
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top,
		titleStyle.Render(s.DisplayName), "  ", subStyle.Render(s.Provider)))
	b.WriteString("\n\n")

	switch {
	case s.Err != "" && s.Usage == nil && len(s.Windows) == 0:
		b.WriteString(errorStyle.Render("✗ Authentication required"))
		b.WriteString("\n")
		b.WriteString(wrap(s.Err, 60))
		b.WriteString("\n")
	default:
		detailW := panelContentWidth(panelW)

		if len(s.Windows) > 1 {
			// Pre-scan window labels for alignment
			maxLabelW := 0
			for _, w := range s.Windows {
				lw := lipgloss.Width(empty(w.WindowLabel))
				if lw > maxLabelW {
					maxLabelW = lw
				}
			}

			usages := make([]string, len(s.Windows))
			maxUsageW := 0
			for i := range s.Windows {
				usages[i] = fmt.Sprintf("%s / %s", humanFormat(&s.Windows[i]), humanTotal(&s.Windows[i]))
				if uw := lipgloss.Width(usages[i]); uw > maxUsageW {
					maxUsageW = uw
				}
			}

			// Keep label, bar, and percentage columns together. Usage details
			// stay inline only when the actual panel is wide enough; otherwise
			// they move to a continuation line instead of forcing the row to wrap.
			const pctW = 5
			const rowPrefixW = 2 + 1 + 1 + 2 + pctW + 2 // margins/separators
			barW := detailW - rowPrefixW - maxLabelW - maxUsageW
			barW = max(barW, 4)
			barW = min(barW, 24)
			inlineUsage := detailW >= rowPrefixW+maxLabelW+barW+maxUsageW

			b.WriteString(sectionStyle.Render("Multi-window plan"))
			b.WriteString("\n")
			for i, w := range s.Windows {
				// Fixed-width columns: label (maxLabelW), bar (1+barW), pct (5)
				label := lipgloss.NewStyle().Width(maxLabelW).Render(empty(w.WindowLabel))
				bar := progressBar(w.Percent(), barW)
				pct := fmt.Sprintf("%5s", fmt.Sprintf("%.0f%%", w.Percent()))
				if inlineUsage {
					b.WriteString(fmt.Sprintf("  %s %s  %s  %s\n", label, bar, pct, usages[i]))
				} else {
					row := fmt.Sprintf("  %s %s  %s\n", label, bar, pct)
					b.WriteString(row)
					writeStyledLine(&b, subStyle, "    "+truncateDisplay(usages[i], detailW-4))
				}

				if w.Note != "" {
					// Note follows the fixed label/bar/percentage columns.
					noteIndent := 2 + maxLabelW + 1 + 1 + barW + 2 + pctW + 2
					if noteIndent > detailW-4 {
						noteIndent = max(detailW-4, 2)
					}
					note := truncateDisplay(w.Note, detailW-noteIndent)
					writeStyledLine(&b, subStyle, strings.Repeat(" ", noteIndent)+note)
				}
			}
		} else if s.Usage != nil {
			u := s.Usage

			// Bar + percentage line
			barW := detailW - 9 // leave room for "  " + pct(≈5) = 9 cols
			barW = max(barW, 4)
			barW = min(barW, 32)
			b.WriteString(progressBar(u.Percent(), barW))
			b.WriteString(fmt.Sprintf("  %.0f%%\n\n", u.Percent()))

			// Key-value pairs. Labels are padded so values align at column 13,
			// chosen to fit all current keys + "refreshed" (9 chars).
			// "  " + key + padding → column 13.
			const valueCol = 13
			for _, kv := range []struct {
				label string
				value string
			}{
				{"used", humanFormat(u)},
				{"total", humanTotal(u)},
				{"window", empty(u.WindowLabel)},
				{"resets", humanReset(u)},
			} {
				padding := valueCol - 2 - lipgloss.Width(kv.label)
				if padding < 1 {
					padding = 1
				}
				b.WriteString(fmt.Sprintf("  %s%s%s\n", kv.label, strings.Repeat(" ", padding), kv.value))
			}

			if u.Note != "" {
				b.WriteString("\n")
				b.WriteString(subStyle.Render("  note: " + u.Note))
				b.WriteString("\n")
			}
		} else {
			b.WriteString(subStyle.Render("  (no usage data yet — press r to refresh)"))
			b.WriteString("\n")
		}
	}
	if !s.RefreshedAt.IsZero() {
		b.WriteString("\n")
		b.WriteString(subStyle.Render(fmt.Sprintf("  refreshed  %s ago", shortDur(time.Since(s.RefreshedAt)))))
		b.WriteString("\n")
	}

	b.WriteString("\n\n")
	b.WriteString(sectionStyle.Render("Free models"))
	b.WriteString("\n")
	if len(s.FreeModels) == 0 {
		b.WriteString(subStyle.Render("  (none)"))
		b.WriteString("\n")
	} else {
		for _, fm := range s.FreeModels {
			notes := ""
			if fm.Notes != "" {
				notes = subStyle.Render("  — " + fm.Notes)
			}
			b.WriteString(fmt.Sprintf("  • %s%s\n", fm.Label, notes))
		}
	}
	return b.String()
}

func (m *Model) renderLog() string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("Debug log"))
	b.WriteString("\n")
	for _, e := range m.log.Snapshot() {
		ts := e.Time.Format("15:04:05")
		col := grey
		switch e.Level {
		case debug.LevelWarn:
			col = yellow
		case debug.LevelError:
			col = red
		case debug.LevelOk:
			col = green
		}
		line := fmt.Sprintf("%s %-12s %s\n", ts, e.Provider, e.Msg)
		writeStyledLine(&b, lipgloss.NewStyle().Foreground(col), strings.TrimSuffix(line, "\n"))
	}
	return b.String()
}

func (m *Model) renderFooter() string {
	parts := []string{"↑/k ↓/j switch", "r refresh", "R refresh all", "D debug", "q quit"}
	return subStyle.Render(strings.Join(parts, "   "))
}

// ---------- styling ----------

var (
	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(green)
	subStyle        = lipgloss.NewStyle().Foreground(grey)
	sectionStyle    = lipgloss.NewStyle().Bold(true).Foreground(green)
	headerStyle     = lipgloss.NewStyle().Bold(true).Foreground(green)
	footerStyle     = lipgloss.NewStyle().Foreground(grey)
	selectedStyle   = lipgloss.NewStyle().Bold(true).Foreground(green)
	listHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(muted).MarginBottom(1)
	errorStyle      = lipgloss.NewStyle().Foreground(red).Bold(true)

	grey   lipgloss.Color = "245"
	green  lipgloss.Color = "82"
	red    lipgloss.Color = "196"
	yellow lipgloss.Color = "214"
	muted  lipgloss.Color = "240"
)

func panel(content string, width int) string {
	width = max(width, 18)
	return panelStyle.
		Width(width - 2).
		Render(content)
}

func panelWidth(width int) int {
	return max(width, 18)
}

// panelContentWidth returns the width available to content inside a panel.
// panel() reserves two border cells and two padding cells.
func panelContentWidth(width int) int {
	return max(panelWidth(width)-4, 4)
}

// writeStyledLine avoids Lip Gloss padding the empty line after a trailing
// newline. Rendering the newline separately keeps the next row at column 0.
func writeStyledLine(b *strings.Builder, style lipgloss.Style, line string) {
	b.WriteString(style.Render(line))
	b.WriteByte('\n')
}

// ---------- formatting helpers ----------

func progressBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(float64(width) * pct / 100.0)
	if filled > width {
		filled = width
	}
	empty := width - filled
	color := green
	switch {
	case pct >= 90:
		color = red
	case pct >= 70:
		color = yellow
	}
	filledStyle := lipgloss.NewStyle().Foreground(color)
	emptyStyle := lipgloss.NewStyle().Foreground(grey)
	return " " + filledStyle.Render(strings.Repeat("█", filled)) + emptyStyle.Render(strings.Repeat("░", empty))
}

func humanFormat(u *types.UsageStats) string {
	if u == nil {
		return "—"
	}
	switch u.Unit {
	case types.UnitUSD:
		return fmt.Sprintf("$%.2f", u.Used)
	case types.UnitCount:
		return fmt.Sprintf("%d", int(u.Used))
	case types.UnitTokens:
		return fmt.Sprintf("%d tok", int(u.Used))
	}
	return fmt.Sprintf("%.0f", u.Used)
}

func humanTotal(u *types.UsageStats) string {
	if u == nil || u.Total == 0 {
		return "—"
	}
	switch u.Unit {
	case types.UnitUSD:
		return fmt.Sprintf("$%.2f", u.Total)
	case types.UnitCount:
		return fmt.Sprintf("%d", int(u.Total))
	case types.UnitTokens:
		return fmt.Sprintf("%d tok", int(u.Total))
	}
	return fmt.Sprintf("%.0f", u.Total)
}

func humanReset(u *types.UsageStats) string {
	if u == nil {
		return "—"
	}
	if !u.ResetAt.IsZero() {
		return u.ResetAt.Format("15:04 2006-01-02")
	}
	if u.ResetIn > 0 {
		return "in " + shortDur(u.ResetIn)
	}
	return "rolling"
}

func shortDur(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		return fmt.Sprintf("%dm", m)
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
	return fmt.Sprintf("%dd", days)
}

func empty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// truncateDisplay limits a plain string to terminal cells and adds an
// ellipsis when possible. Unlike slicing bytes or runes, this accounts for
// wide terminal characters.
func truncateDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)+"…") > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func wrap(s string, width int) string {
	if len(s) <= width {
		return "  " + s
	}
	var out strings.Builder
	for len(s) > width {
		out.WriteString("  ")
		out.WriteString(s[:width])
		out.WriteString("\n")
		s = s[width:]
	}
	if s != "" {
		out.WriteString("  ")
		out.WriteString(s)
		out.WriteString("\n")
	}
	return out.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// panelStyle is shared.
var panelStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(grey).Padding(0, 1)

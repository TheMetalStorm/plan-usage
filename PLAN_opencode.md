# Plan: Fix OpenCode Zen + Go providers

**Status:** ✅ plan approved, implementation complete (+ UI nav improvements ✅, TUI layout alignment fix ✅, provider-independent responsive layout ✅, tray popup monitor/size fix ✅)

## Problem

- OpenCode Zen: uses local `opencode serve` server (unavailable) or OpenAI-proxy rate-limit headers (no real usage). **No real usage data shown.**
- OpenCode Go: purely static defaults ($12/5h, $30/week, $60/month with Used=0). The billing endpoint returns 404. **No real data shown.**

## Strategy

The `opencode.ai` web app exposes a `/_server` RPC endpoint that returns workspace usage (rolling 5-hour window + optional weekly window) with percentage and reset data. The local `opencode.db` SQLite database contains per-session cost/token data for OpenCode Go assistant sessions.

1. **Shared utility package** (`internal/opencodeutil/`) — `_server` client, cookie cache, SQLite DB reader.
2. **Rewrite Zen provider** — hit `/_server` for rolling + weekly usage percentages; support `CODEXBAR_OPENCODE_WORKSPACE_ID`.
3. **Rewrite Go provider** — read local `opencode.db` for daily cost history; overlay `/_server` quota windows when session available.
4. **No scope changes:** existing model lists, auth patterns, and provider registration stay.

## Files to touch

| File | Action |
|---|---|
| `internal/opencodeutil/db.go` | **Create** — SQLite reader for opencode.db |
| `internal/opencodeutil/server.go` | **Create** — `/_server` RPC client with regex parsing |
| `internal/opencodeutil/cookie.go` | **Create** — file-based cookie cache |
| `internal/providers/opencode/opencode.go` | **Rewrite** — Zen provider with `/_server` |
| `internal/providers/opencodego/opencodego.go` | **Rewrite** — Go provider with local DB + `/_server` |
| `go.mod` / `go.sum` | **Update** — add `modernc.org/sqlite` dependency |

## Decisions

- Use `modernc.org/sqlite` (pure Go, no CGo) for DB reads.
- Cookie cache at `~/.local/share/plan-usage/opencode-cookies.json`.
- The `CODEXBAR_OPENCODE_WORKSPACE_ID` env var can set a workspace ID (raw `wrk_…` or full URL).
- No browser cookie import (Linux). Cookies must be cached via env/config or the cache file.
- API key from `auth.json` is tried first as a Bearer token on `/_server`; falls back to cookie auth.
- Regex parsing for `text/javascript` responses (the `/_server` endpoint returns serialized JS objects, not JSON).
- `server.go` will parse the scalar-like serialized object wrapping that JSGI/RPC style endpoints return.
- When `/_server` fails, Zen shows a `Note` with the error; Go still shows local DB data.

---

### Part 1 — Create `internal/opencodeutil/` package   ✅

Files:
- `db.go` — `OpenDB()`, `DailyCostHistory()`, `TotalCostSince()`
- `server.go` — `ServerClient` struct with `FetchWorkspaceUsage()`, `FetchSubscription()`, regex response parsing
- `cookie.go` — `CookieCache` struct for read/write file-based cookie store



---

### Part 2 — Rewrite OpenCode Zen provider   ✅

- Replace local-server + probe fallback with `/_server` endpoint usage
- Primary window: rolling 5-hour (usagePercent, resetInSec)
- Zen balance from subscription.get if available
- Support `CODEXBAR_OPENCODE_WORKSPACE_ID` env var

---

### Part 3 — Rewrite OpenCode Go provider   ✅

- Read local `opencode.db` for daily cost history
- Query `session` table grouped by day for cost data
- Three windows: 5h rolling, weekly, monthly — with real cost data from local DB and optional _server overlays
- Implement SnapshotWindows with real data

---

### Part 4 — Wire up deps, update go.mod, verify build   ✅

- Add `modernc.org/sqlite` to go.mod (done)
- Run `go mod tidy`
- Verify `go build ./...` compiles

(commits 77084ea, aec235c)

---

### Part 5 — TUI navigation: up/down + clickable provider names   ✅

- Replace left/right arrow + h/l navigation with up/down arrow + k/j
- Add mouse click handler: clicking a provider name in the left panel selects it
- Add mouse wheel support (wheel up/down navigates providers)
- Update footer keybinding hints
- File: `internal/tui/tui.go`

---

### Part 6 — Fix TUI layout: align names, badges, bars, prevent wrapping   ✅

**Problem:** Provider names and percentage badges in the left panel weren't vertically aligned because each row used a different name-column width. Window labels and bars in the multi‑window section didn't align (labels wider than the `%-7s` format pushed bars right). Single‑window key-value labels were hardcoded. Text wrapped in narrow terminals.

**Changes in `internal/tui/tui.go`:**
- **Left panel `renderList()` — two-pass layout**: first pass measures the widest badge across all rows; second pass uses a single fixed `nameW` width for every row. This guarantees badges and names start at the same column regardless of badge width. Name truncation uses `lipgloss.Width()` (display‑width aware, not byte count).
- **Shared `listPanelWidth()` helper**: extracted the list‑panel width computation into one method (`m.width / 3`, min 22) used by `View()`, `handleMouse()`, `renderList()`, and `detailContentWidth()`. Changes ratio `1/4 → 1/3` for more name room.
- **Multi‑window section**: pre‑scans window labels to find `maxLabelW`; uses that to align all labels. Adaptive bar width (`barW`) computed from `detailContentWidth()` so rows never wrap. Percentage column uses `%5s` fixed width. Note indent computed dynamically from rendered prefix width.
- **Single‑window section**: key-value labels (`used`/`total`/`window`/`resets`) aligned to a dynamic `valueCol` computed from the widest label (kept at column 13 to match the `refreshed` line). Adaptive bar width so `progressBar` + percentage never wraps the detail panel.
- **Responsive ("reaktiv")**: all widths adapt to terminal size via `detailContentWidth()` / `listPanelWidth()`; bar widths shrink in narrow panels and grow in wide ones.

**Verification:** `go build ./...` compiles; `go vet ./internal/tui/` passes; test suite passes.

---

### Part 7 — Make layout provider-independent and width-correct   ✅

**Problem:** Part 6 used a list width based on `1/3` while debug mode renders the list at `1/4`, so the list was formatted for a wider panel than it received. Multi-window rows also need to preserve aligned bars/percentages when usage text does not fit.

**Changes in `internal/tui/tui.go`:**
- Centralize list/detail panel widths and pass the actual assigned panel width into renderers.
- Make list columns use the actual panel width in normal and debug layouts.
- Keep labels, bars, and percentage columns fixed; move usage details to a continuation line when the responsive width cannot fit them inline.
- Add TUI layout tests covering non-OpenCode labels, debug columns, alignment, and narrow widths.

**Verification:** `go test ./internal/tui/`, `go build ./...`, and `go vet ./internal/tui/` pass.

### Part 8 — Tray popup: bound size to the monitor and keep the tray clear   ✅

**Problem (deep investigation, empirically verified on the user's i3/X11 setup):** The popup defaulted to 1120×760 (fills most of a 1080p screen). Content forced a ~1026px minimum width — the 4-column homogeneous grid multiplies the widest card's minimum, and `SetMaxWidthChars(48)` model lines made that card ~248px — so `Resize()` could never shrink below ~1026px, overflowing any display whose logical width is smaller (HiDPI scale-2 laptops, small panels). The size clamp in `showAtPointer()` only ran in the success path; seat/pointer/monitor failures showed the popup unclamped. Because i3 never sets `_NET_WORKAREA`, `gdk_monitor_get_workarea()` returns the full monitor geometry (verified with and without polybar running), so the tray/bar region is never excluded; opening at the top-right tray icon placed the popup at (800,30), covering the icon, and the seat grab then swallowed icon clicks while the covered click was "inside" the popup → no dismiss.

**Changes:**
- `internal/tray/tray.go`: smaller default size (960×680); per-card minimum-width caps (title 24 chars, status/usage/meta lines 36 chars, model lines 24 chars); new `popupInnerRect()` margin helper; new `popupEdgeMargin` (32) / `popupPointerOffset` (16) constants; new `clip()` helper for unbreakable model-name tokens.
- `internal/tray/tray_linux.go`: `showAtPointer()` always clamps size and position into the monitor work area minus the edge margin (falling back to the primary monitor when no monitor is found — no more unclamped fallbacks), and offsets the popup from the click point so it never sits on the tray icon; `renderCard()`/`appendModelSection()` cap dynamic label widths.
- `internal/tray/tray_test.go`: tests for `popupInnerRect`, size clamp against the inner rect, offset positioning staying inside the work area, and `clip`.

**Key empirical finding (measured with a temporary GTK probe):** `SetMaxWidthChars` alone does NOT bound a label's minimum width when the text contains one unbreakable token (GTK wraps only at word boundaries) — a 53-char model ID still forced min 336px. Only `SetEllipsize(pango.ELLIPSIZE_END)` (plus `clip()` for model names) bounds the minimum: measured grid minimum dropped 1010 → 846px, window default now renders at 960×680, and a tray-corner click places the popup at y=78, leaving the top bar (y 0–40) free.

**Verification:** `go build ./...`, `go test ./...` (all packages), and `go vet ./internal/tray/` pass.

## Out of scope

- Browser cookie import (Chrome/Dia) — that's macOS-specific CodexBar functionality.
- Keychain integration — not applicable on Linux.
- Writing to opencode.db — read-only access only.

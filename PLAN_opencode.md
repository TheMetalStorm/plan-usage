# Plan: Fix OpenCode Zen + Go providers

**Status:** ✅ plan approved, implementation complete

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
- Cookie cache at `~/.local/share/provider-usage/opencode-cookies.json`.
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

## Out of scope

- Browser cookie import (Chrome/Dia) — that's macOS-specific CodexBar functionality.
- Keychain integration — not applicable on Linux.
- Writing to opencode.db — read-only access only.

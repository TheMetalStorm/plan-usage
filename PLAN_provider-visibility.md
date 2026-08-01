# PLAN — provider show/hide toggles (TUI + tray)

**Status:** 🚧 draft, in progress

**Feature:** Let the user select and deselect which providers are shown in
the terminal (TUI) and in the tray. Toggles persist to the `enabled`
allowlist in `~/.config/plan-usage/config.yaml`, so both surfaces and the
daemon refresh agree.

**Contract sections:** `README.md` §Configuration, §Features, §Tray startup
and operation. (This repo has no `SPEC.md`/`AGENTS.md`; the README is the
de-facto contract and must be updated in the same commits per docs-precedence.)

**Builds on:** nothing — current HEAD (`26a60c1`).

## Decisions

1. **Persistence** — every toggle writes the `enabled` allowlist in
   `config.yaml` (same field the README already documents). The first
   toggle materializes the list when it was empty (default-on), so an
   exclusion is actually recorded.
2. **Scope** — toggling gates both *refresh* and *rendering*, which is
   exactly what `enabled` already means today (`daemon.cycle`,
   `tui.Init`/`refreshAllCmd`, `tray.BuildCards` all consult
   `IsProviderEnabled`). No new query-vs-display split.
3. **TUI interaction** — `x` opens a provider visibility picker listing
   every registered provider with `[x]`/`[ ]`. `space`/`enter` toggles the
   selected provider (persists immediately), `esc`/`x` returns to the
   normal list. This keeps hidden providers reachable (a plain `x`-on-row
   toggle would strand them).
4. **Tray interaction** — the right-click context menu gains one checkbox
   per provider (`Show <DisplayName>`) via `systray.AddMenuItemCheckbox`.
   Toggling persists and re-renders the popup immediately. The popup card
   grid itself stays a pure read-only surface.
5. **Concurrency** — `config.Config` gains a `sync.RWMutex` guarding
   `Enabled`/`Providers` so daemon refresh goroutines (readers) never race
   the tray-menu or TUI toggle writers. Mutations replace the `Enabled`
   slice (never mutate in place) and persist under the lock.
6. **Cross-process convergence** — the tray reloads `config.yaml` once per
   refresh-loop tick and merges `Enabled`/`RefreshEvery` into the shared
   cfg, so a toggle made in a concurrently running `plan-usage show`
   reaches the tray within one refresh interval. The TUI reads config once
   at startup (a tray-side toggle applies on next `show` start).

## Files to touch

- `internal/config/config.go` — `sync.RWMutex`, `IsProviderEnabled` under
  lock, new `EnabledSet`, `SetProviderEnabled`, `ApplyFresh`, split
  `Write` into locked public + `writePathLocked`.
- `internal/config/config_test.go` — new tests for the helpers.
- `internal/tui/tui.go` — picker mode (`allNames`, `picker` fields),
  `x` key, `space`/`enter` toggle command, `rebuildItems`/`itemsForEnabled`
  (replaces `loadItems`/`seedFromProviders`), picker branch in
  `renderList`, `renderDetailPanel` split, footer hints, mouse guards.
- `internal/tui/tui_test.go` — picker/toggle tests.
- `internal/tray/tray_linux.go` — provider checkboxes in the context menu,
  `toggleProvider` handler, `menuMu`, `reloadConfig` in `refreshLoop`.
- `README.md` — document the controls; replace the
  "tray deliberately shows no provider selector" sentence.
- `PLAN_provider-visibility.md` — this file (tracking doc, committed with
  the branch, removed via `git rm` when fully resolved per repo
  convention seen in `cda950e`).

## Parts

### Part 1 — config: allowlist mutation helpers   ✅ (commit 1713b00)

- `SetProviderEnabled` materializes the default-on allowlist on first
  toggle and persists under the new config mutex; `ApplyFresh` lets the
  tray pick up toggles from a concurrently running TUI.

Add concurrency-safe enable/disable helpers to `internal/config/config.go`:

- `mu sync.RWMutex` on `Config` (unexported, skipped by yaml).
- `IsProviderEnabled` acquires `RLock`.
- `EnabledSet(allNames []string) map[string]bool` — the current enabled
  set per `IsProviderEnabled` semantics (materializes default-on set from
  `allNames` minus `Providers[name].Disabled` when `Enabled` is empty).
- `SetProviderEnabled(allNames []string, name string, enabled bool) error`
  — rebuild the `Enabled` allowlist in registry order, then persist via
  `writePathLocked(ConfigPath)` (defaulting ConfigPath to
  `~/.config/plan-usage/config.yaml` when empty).
- `ApplyFresh(fresh *Config)` — copy `Enabled` (defensive copy) and
  `RefreshEvery` (floored at 5s) from a freshly loaded config under lock.
- Refactor `Write(path)` into public locked `Write` + internal
  `writePathLocked(path)`.

Verify: `cd <repo> && go test ./internal/config/ && go vet ./internal/config/`

### Part 2 — TUI provider picker   ✅

- `x` toggles a picker listing every provider with `[x]`/`[ ]`;
  `space`/`enter` persists a toggle, `esc`/`x` returns. Hidden providers
  stay reachable and the detail panel follows the picker cursor.

`internal/tui/tui.go`:

- `Model` gains `allNames []string` and `picker bool`.
- `New` seeds `allNames` from `providers.AllNames()` and builds items via
  `rebuildItems()` (replaces `loadItems` + `seedFromProviders`).
- `itemsForEnabled` returns one `Snapshot` per enabled provider in registry
  order, preferring store data and seeding the rest (this also filters
  stale store entries for providers disabled since the snapshot was
  written).
- Keys: normal mode `x` enters picker; picker mode `space`/`enter` toggles
  the selected provider via `toggleSelectedCmd` (runs
  `cfg.SetProviderEnabled`, posts `toggleDoneMsg`/`toggleErrMsg`);
  `esc`/`x` exits; `q`/`ctrl+c` still quits. Selection clamps between
  `len(items)` (normal) and `len(allNames)` (picker).
- `toggleDoneMsg` handler calls `rebuildItems()` and logs; `toggleErrMsg`
  logs the failure.
- `renderList`: picker branch renders all providers with `[x]`/`[ ]`, a
  different header, dimmed rows for hidden providers, `▸` cursor.
- `renderDetail` split into `renderDetailPanel(panelW, s)`; in picker mode
  the detail panel shows the selected provider even when hidden.
- `renderFooter`: picker hint `space/enter toggle   esc/x back   q quit`;
  normal hint gains `x show/hide`.
- `handleMouse`: in picker mode, wheel moves within `allNames`, left-clicks
  are ignored (rows are single-line there).

Verify: `cd <repo> && go test ./internal/tui/ && go vet ./internal/tui/`

### Part 3 — Tray context-menu checkboxes   🚧

`internal/tray/tray_linux.go`:

- `popup` gains `menuMu sync.Mutex` and `toggleItems []*systray.MenuItem`.
- In `onReady`, after Refresh/Quit, add a separator and one
  `systray.AddMenuItemCheckbox("Show <DisplayName>", ...)` per provider
  (initial state from `cfg.IsProviderEnabled`). Each gets a goroutine
  draining `ClickedCh` → `p.toggleProvider(name, item)`.
- `toggleProvider` — under `menuMu`: compute `want := !enabled`, call
  `cfg.SetProviderEnabled(providers.AllNames(), name, want)`, on error show
  the message in the popup status bar; otherwise `item.Check()`/`Uncheck()`
  and `p.requestRefresh()` (the next `render` filters via the live cfg, so
  even an in-flight refresh renders the new selection).
- `refreshLoop` tick calls `p.reloadConfig()` before `requestRefresh()`:
  `config.Load(cfg.ConfigPath)` → `cfg.ApplyFresh(fresh)` → re-sync
  checkbox states under `menuMu`.

Verify: `cd <repo> && go build ./... && go vet ./...`

### Part 4 — README + plan close-out   🚧

`README.md`:

- Features: replace "The tray deliberately shows **no provider selector**."
  with a description of the context-menu checkboxes.
- New "Provider visibility" subsection under Configuration: TUI `x` picker,
  tray right-click checkboxes, persistence to `enabled`, materialization on
  first toggle, and the tray's one-cycle config reload.
- Config example: annotate that `enabled` is what the toggles edit.

Verify: read the rendered README section.

## Test plan

`internal/config/config_test.go`:
- `TestSetProviderEnabledDisablesMaterializesAllowlist` — empty `Enabled`
  (default-on), disable `codex` → `Enabled == allNames minus codex` in
  registry order; `IsProviderEnabled("codex")` false.
- `TestSetProviderEnabledRemovesFromExistingAllowlist` — full `Enabled`,
  disable `freebuff` → removed.
- `TestSetProviderEnabledAddsToAllowlist` — `Enabled: ["codex"]`, enable
  `freebuff` → `Enabled` contains both in registry order.
- `TestSetProviderEnabledPersistsToDisk` — `ConfigPath` in temp dir; toggle;
  `Load` shows the new allowlist.
- `TestSetProviderEnabledHonorsDisabledProviderFlags` — `Providers[codex].
  Disabled: true` with empty allowlist; enable `codex` → allowlist includes
  it and `IsProviderEnabled` true.
- `TestEnabledSetMatchesIsProviderEnabled` — for empty/full/partial
  allowlists, `EnabledSet(allNames)[n] == IsProviderEnabled(n)` for every n.
- `TestApplyFreshCopiesEnabledAndRefreshInterval` — old cfg, fresh with new
  `Enabled` + `RefreshEvery` → merged; floor respected.

`internal/tui/tui_test.go`:
- `TestPickerRenderShowsAllProvidersWithCheckboxes` — picker on; `renderList`
  contains `[x]` for enabled and `[ ]` for hidden providers by display name.
- `TestToggleSelectedProviderHidesFromItems` — temp config path; `x`-flow:
  `toggleSelectedCmd()` result fed to `Update` → `items` drops the provider,
  `cfg.IsProviderEnabled` flips, file on disk reflects it.
- `TestRebuildItemsFiltersDisabledProviders` — store snapshot for a disabled
  provider is not rendered.
- `TestPickerModeSelectionClamping` — enter/exit picker clamps `selected`
  to `len(items)` / `len(allNames)`.
- `TestPickerFooterAndNormalFooterHints` — footer strings mention
  `space/enter toggle` (picker) and `x show/hide` (normal).

## Atomic-commit messages

1. `feat(config): add provider-enabled allowlist mutations` (Part 1)
2. `feat(tui): add provider show/hide picker` (Part 2)
3. `feat(tray): add provider show/hide context menu` (Part 3)
4. `docs: document provider visibility controls` (Part 4)
5. `docs: remove resolved PLAN_provider-visibility.md` (final, `git rm`)

## Risks / callouts

- **Concurrency** — daemon refresh goroutines read `cfg.Enabled` while the
  tray menu / TUI toggle writers replace it; the new `config.mu` serializes
  these. The systray `MenuItem` bool fields are updated only under
  `popup.menuMu`.
- **Config file writes** — a toggle rewrites the whole YAML (existing
  `Write` behavior); comments are not preserved. Acceptable; matches today's
  `Write`.
- **Cross-process lost updates** — TUI and tray both write the same file;
  last writer wins. Documented limitation.
- **`toggleSelectedCmd` writes in tests** — tests must set `ConfigPath` to a
  temp file so a stray toggle never touches the real config.

## Done.

Land the four commits above, run `go build ./... && go vet ./... && go test ./...`,
flip this plan's status to ✅, then remove the resolved plan file via
`git rm` in a final docs commit and report to the user.

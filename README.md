# plan-usage

> **⚠️ WIP — This project is under active development.**

Multi-provider coding-plan usage monitor for OpenCode, Codex / ChatGPT,
ClinePass, CommandCode, and Freebuff. Choose the interactive TUI
(`plan-usage show`) or the native Linux system-tray popup
(`plan-usage tray`) to view all enabled providers at once.

## Features

- **Five providers, one monitor** in stable registry order.
- **Interactive TUI** (`plan-usage show`) — keyboard-navigable, mouse-aware
  dashboard with provider list, usage bars, and detail panels.
- **Native Linux tray** (`plan-usage tray`) via `fyne.io/systray` and a
  compact, high-contrast usage icon.
- **True X11 popup** created with GTK3 `GTK_WINDOW_POPUP`. It is borderless,
  positioned at the pointer, clamped to the current monitor work area, and
  ignored by i3's managed window tree. It never opens a terminal.
- **All enabled providers appear simultaneously** as vertically scrollable
  cards. Each card includes provider name/icon, status or authentication error,
  every usage window, progress, percent, used/total, reset information, notes,
  last update time, and the free model catalog below the usage details when
  one is available. Free model names wrap inside the popup. The tray context
  menu (right-click) has a **Toggle providers** submenu with one checkbox per
  provider, so the visible set can be changed without touching the config file.
- **Provider visibility toggles in both surfaces** — the TUI's `x` picker and
  the tray's context-menu checkboxes write the same `enabled` allowlist, so
  the selection is shared, persists across restarts, and controls which
  providers are refreshed and rendered.
- **Refresh now** in the popup and tray context menu, with timer/manual refresh
  requests serialized so only one provider refresh runs at a time.
- **Escape, focus loss, outside click, or another tray click** hides the popup.

## Install

The native popup is **Linux + X11 + CGO only**. Building it needs Go, a C
compiler, `pkg-config`, GTK3 development headers, and a StatusNotifierItem
(SNI)/AppIndicator host in the desktop panel. i3 users need an SNI host such
as `snixembed`, `trayer`, or a compatible bar/plugin; GTK and D-Bus alone do
not create a visible tray area.

### Ubuntu / Debian / Linux Mint

```bash
sudo apt update
sudo apt install build-essential pkg-config libgtk-3-dev \
  libayatana-appindicator3-dev
# Older releases may provide libappindicator3-dev instead.
```

### Fedora

```bash
sudo dnf install gcc gcc-c++ make pkgconf-pkg-config gtk3-devel \
  libappindicator-gtk3-devel
```

### Arch Linux / Manjaro

```bash
sudo pacman -S --needed base-devel pkgconf gtk3 libappindicator-gtk3
```

Then build and install:

```bash
git clone https://github.com/TheMetalStorm/plan-usage
cd plan-usage
go build -o ~/.local/bin/plan-usage ./cmd/plan-usage
```

For Linux builds without CGO, and for non-Linux systems, the `tray` command
is intentionally a stub that reports: Linux/X11 and CGO are required. The TUI
(`plan-usage show`) remains available on all platforms.

The repository vendors `gotk3 v0.6.4` because that release contains an upstream
GDK callback reference that does not compile on GTK3. The vendored copy applies
the narrow fix needed by this application (the unused seat-grab prepare callback
is passed as nil). Keep using `GOFLAGS=-mod=vendor` for reproducible builds; a
future gotk3 upgrade should remove the workaround only after the upstream release
builds cleanly.

## Which executable should start the tray?

There are two different launchers in a setup that uses Surfshark:

- **`plan-usage`** is the project binary built from `./cmd/plan-usage`. Use it
  directly for normal operation:
  `plan-usage show`, `plan-usage tray`, or `plan-usage init tray`.
- **`plan-usage-after-vpn`** is an optional shell wrapper in
  `scripts/plan-usage-after-vpn`. It is not part of the Go binary and is not
  installed automatically. In an i3 setup it starts the configured Flatpak VPN,
  waits for successful HTTPS traffic through the recognized VPN interface, and
  then runs `plan-usage tray`.

Install the optional wrapper and its example configuration like this:

```bash
install -Dm755 scripts/plan-usage-after-vpn ~/.local/bin/plan-usage-after-vpn
mkdir -p ~/.config/plan-usage
if test ! -e ~/.config/plan-usage/vpn.env; then
  install -m 600 scripts/plan-usage-after-vpn.env.example ~/.config/plan-usage/vpn.env
fi
```

The wrapper reads `~/.config/plan-usage/vpn.env`. Configure
`PLAN_USAGE_VPN_APP`, `PLAN_USAGE_VPN_APP_ARGS`,
`PLAN_USAGE_VPN_INTERFACE_REGEX`, `PLAN_USAGE_VPN_HEALTH_URL`,
`PLAN_USAGE_VPN_TIMEOUT`, `PLAN_USAGE_VPN_TRAY_PROCESSES`, and `PLAN_USAGE_BIN`
there. The defaults target Surfshark's Flatpak and the Codebuff HTTPS host;
the health URL must remain HTTPS. Set `PLAN_USAGE_VPN_TRAY_PROCESSES` to an
empty value when no panel-process readiness check is needed.

Use the direct binary when the VPN is not required for the provider checks:

```i3
exec --no-startup-id "$HOME/.local/bin/plan-usage" tray
```

Use the local wrapper only when the tray must wait for Surfshark:

```i3
exec --no-startup-id "$HOME/.local/bin/plan-usage-after-vpn"
```

These are alternatives. Do not configure both, or the tray may start twice.
The wrapper is currently specific to a Linux + i3 + Surfshark Flatpak setup and
is therefore documented as local integration rather than application behavior.

## Tray startup and operation

Run the tray from a terminal **inside an X11 graphical login session**:

```bash
plan-usage tray
```

Left-clicking the tray icon toggles the popup. Right-clicking opens the tray
menu with **Refresh now**, one **Show <Provider>** checkbox per provider, and
**Quit**. The popup also has **Refresh** and **Hide** buttons. It is a GTK
`WINDOW_POPUP`, not a normal top-level window;
under X11 this is an override-redirect popup, so i3 does not create or manage a
new container for it. The popup is kept inside the work area of the monitor
containing the pointer. If the screen is too short for all cards, the cards
scroll together in one view.

Create a desktop autostart entry with:

```bash
mkdir -p ~/.config/autostart
plan-usage init tray > ~/.config/autostart/plan-usage-tray.desktop
```

The generated entry uses `Terminal=false`. `init tray` emits the Linux desktop
entry; macOS and other platform launchers are intentionally not supported for
this GTK popup. If the optional `plan-usage-after-vpn` i3 wrapper is enabled,
do not also enable this desktop entry; choose one tray startup path.

**Wayland and Sway are not supported yet.** If `WAYLAND_DISPLAY` is active,
or the GTK backend is not X11, `plan-usage tray` exits with a clear error
instead of falling back to a normal managed window. `DISPLAY` must be set and
the process must be launched with access to the current user graphical session;
do not use `sudo` or a headless SSH/TTY session.

## Refresh ownership

The tray owns polling automatically — it runs a periodic refresh loop
using `Daemon.Refresh(context.Context)` and atomically writes
`$XDG_STATE_HOME/plan-usage/snapshot.json`. The TUI reads that snapshot
when started, so running `plan-usage tray` alongside `plan-usage show`
gives you live data without a separate poller.

## Commands

| command | purpose |
|---|---|
| `plan-usage show` | Open the interactive TUI dashboard. |
| `plan-usage tray` | Run the Linux/X11 native tray and GTK popup. |
| `plan-usage version` | Print version information. |

Global flags include `--config PATH`, `--state-dir PATH`, `--debug`, and
`--dry-run`; they may appear before or after the subcommand.

## Configuration

`~/.config/plan-usage/config.yaml`:

```yaml
providers:
  opencodego: {}
  codex: {}
  clinepass: {}
  commandcode: {}
  freebuff: {}

enabled: [opencodego, codex, clinepass, commandcode, freebuff]
refresh_interval: 60s     # tray cadence; minimum 5s
probe_max_tokens: 1
debug: false
```

The tray uses `enabled` and the provider configuration to decide which cards
to show. A provider with missing credentials still gets its card and displays
its authentication/status error. Available models are read from the persisted
snapshot and displayed below the usage windows; a refresh updates both the
free model catalog and quota data. The Codebuff/Freebuff card additionally
shows a separate **Premium models (free for 6h/day)** section because those
models can be used within the daily free allowance. Premium-only entries for
other providers are not shown in the tray. Reset timestamps are shown when a
provider supplies them, with a clear unavailable marker otherwise.

### Provider visibility

Both surfaces can select which providers are shown, and both write the same
`enabled` allowlist shown above, so the choice persists and applies to the
next launch of the other surface:

- **TUI (`plan-usage show`)** — press `x` to open the provider picker. Every
  registered provider is listed with `[x]`/`[ ]`; `space`/`enter` toggles the
  highlighted provider (saved immediately), `esc`/`x` returns to the list.
- **Tray** — right-click the tray icon, hover **Toggle providers**, and
  tick/untick the provider entries. The popup re-renders immediately. The tray
  also reloads
  `config.yaml` on every refresh cycle, so a toggle made in a concurrently
  running TUI is picked up within one refresh interval (the TUI reads the
  config at startup, so a tray-side toggle applies on the next `show`).

The first toggle materializes `enabled` from the default-on registry order if
the key was absent, so an exclusion is actually recorded on disk. When two
processes toggle at once, the last write wins. Toggling a provider off also
stops it from being queried; toggling it on again resumes polling.

## Architecture and privacy

Provider implementations live under `internal/providers/<name>/`. The daemon
and tray refresh providers concurrently, retain individual provider errors, and
replace the snapshot atomically. The TUI does not write the snapshot.

Authentication is discovered from the native CLI configuration files where
possible, or from explicit YAML/environment overrides. Tokens are not stored in
the snapshot, TUI debug panel, or tray cards.

## License

MIT.

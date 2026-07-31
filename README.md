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
  and last update time. The tray deliberately shows **no provider selector and
  no model/FreeModels list**.
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

## Tray startup and operation

Run the tray from a terminal **inside an X11 graphical login session**:

```bash
plan-usage tray
```

Left-clicking the tray icon toggles the popup. Right-clicking opens the tray
menu with **Refresh now** and **Quit**. The popup also has **Refresh** and
**Hide** buttons. It is a GTK `WINDOW_POPUP`, not a normal top-level window;
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
this GTK popup.

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
its authentication/status error. Models remain part of the TUI/provider data
model but are completely ignored by the tray popup.

## Architecture and privacy

Provider implementations live under `internal/providers/<name>/`. The daemon
and tray refresh providers concurrently, retain individual provider errors, and
replace the snapshot atomically. The TUI does not write the snapshot.

Authentication is discovered from the native CLI configuration files where
possible, or from explicit YAML/environment overrides. Tokens are not stored in
the snapshot, TUI debug panel, or tray cards.

## License

MIT.

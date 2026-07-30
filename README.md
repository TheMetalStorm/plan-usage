# plan-usage

> Multi-provider coding-plan usage monitor — keep an eye on OpenCode,
> Codex / ChatGPT, ClinePass, CommandCode and Freebuff right from your
> Polybar bar. Click for a TUI dashboard.

```
┌──────────────────────────────────────────────────────────────────────┐
│  plan-usage                 multi-provider coding-plan monitor   │
├──────────┬───────────────────────────────────────────────────────────┤
│ ▸ OpenCode   ● 0%      │  OpenCode Zen                              │
│   Codex      ▲ 75%     │  ████████████████░░░░░░░░░░░░  75% (5h)      │
│   ClinePass  ✗ no-auth │  used:       $9.00                          │
│   CommandCode● 12%     │  total:      $12.00                         │
│   Freebuff   ● 80%     │  window:     5h                             │
│                       │  resets:     in 3h 14m                       │
│                       │                                              │
│                       │  Free models:                                │
│                       │  • Big Pickle                                │
│                       │  • DeepSeek V4 Flash Free                    │
│                       │  • Ling-3.0-flash Free                       │
├──────────┴───────────────────────────────────────────────────────────┤
│  ←/h →/l switch    r refresh    R refresh all    D debug    q quit   │
└──────────────────────────────────────────────────────────────────────┘
```

in your Polybar:

```
[module/plan-usage]
type    = custom/script
exec    = plan-usage polybar
interval= 60
click-left = plan-usage show
format-prefix = " "
```

## ⭐ Features

- **Five providers, one widget**: OpenCode (Zen), Codex / ChatGPT,
  Cline Pass, Command Code, Freebuff.
- **Polybar widget** with click-through to the TUI.
- **Hybrid auth**: zero-config if you already have the native CLI
  installed (`~/.codex/auth.json`, `~/.local/share/opencode/auth.json`,
  `~/.commandcode/auth.json`, `~/.config/manicode/credentials.json`,
  VS Code / Cline global storage). Override via `~/.config/plan-usage/config.yaml`.
- **Systemd-user-service** for always-fresh data; `plan-usage daemon` polls every
  `refresh_interval` and writes a tiny `snapshot.json` to
  `$XDG_STATE_HOME/plan-usage`.
- **Debug mode** (`D` in the TUI or `--debug` flag) shows the last 64 log
  entries for every refresh attempt.
- **Cheap probes**: each refresh sends a `max_tokens=1` no-op request —
  typically billed as a fraction of a cent.

## 🚀 Install

```bash
git clone https://github.com/TheMetalStorm/plan-usage
cd plan-usage
go build -o ~/.local/bin/plan-usage ./cmd/plan-usage

# system service (auto-start, restart on crash)
plan-usage init system | sed "s|%h|$HOME|g" > ~/.config/systemd/user/plan-usage.service
systemctl --user daemon-reload
systemctl --user enable --now plan-usage.service

# polybar snippet
plan-usage init polybar >> ~/.config/polybar/config.ini
polybar msgcmd restart
```

## 🧭 Subcommands

| command              | what it does                                                         |
|----------------------|----------------------------------------------------------------------|
| `plan-usage show`         | open the interactive TUI                                             |
| `plan-usage polybar`      | write one line for Polybar (reads the daemon-cached state file)      |
| `plan-usage daemon`       | long-running poller; writes `$XDG_STATE_HOME/plan-usage/snapshot.json`    |
| `plan-usage check [name]` | dump one (`name`) or the whole aggregate as JSON, on stdout          |
| `plan-usage refresh`      | one-shot refresh cycle, writes the snapshot file                     |
| `plan-usage init polybar` | print a polybar `custom/script` module snippet                       |
| `plan-usage init system`  | print a `systemd --user` unit file                                   |
| `plan-usage version`      | print version                                                        |

All subcommands accept `--config PATH`, `--state-dir PATH`, `--debug`,
`--dry-run`.

## ⚙️ Config

`~/.config/plan-usage/config.yaml`:

```yaml
providers:
  opencode: {}     # leave empty for native-CLI-config discovery
  codex: {}        # reads ~/.codex/auth.json by default
  clinepass:
    api_key: sk-…  # explicit override
  commandcode: {}  # reads ~/.commandcode/auth.json or COMMAND_CODE_API_KEY
  freebuff: {}     # reads ~/.config/manicode/credentials.json

refresh_interval: 60s   # daemon poll cadence (floor 5s)
probe_max_tokens: 1     # sent with every probe request
polybar:
  format: "{icon} {name} {percent}%"
  separator: " · "
  hide_if_no_auth: true
  no_auth_text: "—"
debug: false
```

## 🏗️ Architecture

```
┌────────────────────┐    probe    ┌────────────────────┐
│ plan-usage daemon       │ ──────────▶ │ OpenCode / Codex / │
│ (every 60s)        │             │ ClinePass /        │
└────────────────────┘             │ CommandCode /      │
            │                       │ Freebuff           │
            ▼ atomically            └────────────────────┘
   $XDG_STATE_HOME/plan-usage/snapshot.json
            │
            ├──── plan-usage polybar  ──  reads file -> printf
            └──── plan-usage show     ──  reads file + live probes
```

- The **TUI** never writes to `snapshot.json` -- the daemon is the single
  source of truth, so concurrent refreshes don't clobber each other.
- Provider implementations live under `internal/providers/<name>/`.
- The `probe` package knows nothing about providers; it just sends a tiny
  HTTP request and parses `x-ratelimit-*-*` (OpenAI-style) and
  `anthropic-ratelimit-*-*` (Anthropic-style) headers.
- Auth discovery is in `internal/auth` (`Finder` type).

## 🧩 Adding a provider

Subclass `types.Provider`:

```go
type Foo struct { a *auth.Finder }
func (p *Foo) Name() string                                            { return "foo" }
func (p *Foo) DisplayName() string                                     { return "Foo Bart" }
func (p *Foo) Icon() string                                            { return "" }
func (p *Foo) IsConfigured() error                                     { /* see auth */ }
func (p *Foo) AvailableModels() []types.FreeModel                      { /* static list */ }
func (p *Foo) FetchUsage(ctx context.Context) (*types.UsageStats, error) {
    /* see internal/probe/probe.go for the request helper */
}
```

then register it in `internal/providers/provider.go`:

```go
Register("foo", func() (types.Provider, error) { return foo.New(), nil })
```

and add it to `order`.

## 🛡️ Privacy & security

`plan-usage` reads tokens from native CLI config files only when the user is
already logged in. Tokens never appear in:
- the state file (`snapshot.json`),
- the polybar output,
- the TUI debug panel,
- log output.

If you'd rather **not** store tokens, set the override in your YAML —
the providers will use the override and ignore the native config.

## 🤝 License

MIT.

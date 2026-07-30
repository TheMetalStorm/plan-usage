package main

import (
	"errors"
	"fmt"
	"io"
)

// runInit emits example snippets the user can pipe into their config.
// Errors are written to stderr; the returned error drives the exit code
// (errInt does not print, so no double-printing).
func runInit(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "init requires a subcommand: polybar | system")
		return errors.New("init requires a subcommand")
	}
	switch args[0] {

	case "polybar":
		_, _ = stdout.Write([]byte(polybarSnippet + "\n"))
		return nil

	case "system", "systemd":
		_, _ = stdout.Write([]byte(systemdSnippet + "\n"))
		return nil

	default:
		fmt.Fprintf(stderr, "init: unknown subcommand %q (try polybar or system)\n", args[0])
		return errors.New("unknown init subcommand")
	}
}

const polybarSnippet = `; Add this to your polybar config (~/.config/polybar/config.ini):

[module/usage]
type = custom/script
exec = usage polybar
interval = 60
click-left = usage show
click-right = usage refresh
format-prefix = " "
format-foreground = #83a598
format-underline = #83a598
label = %output%

; Then in your bar config:
;   modules-right = usage volum pulseaudio
`

const systemdSnippet = `# Save to ~/.config/systemd/user/usage.service, then:
#   systemctl --user daemon-reload
#   systemctl --user enable --now usage.service
#
[Unit]
Description=usage daemon — multi-provider coding-plan monitor
After=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/usage daemon
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=default.target
`

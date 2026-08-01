// Command plan-usage is a multi-provider coding-plan usage monitor with
// an interactive TUI dashboard and a native system-tray popup.
//
// Global flags (--config, --state-dir, --debug, --dry-run) may appear
// ANYWHERE on the command line; the first non-flag positional determines
// the subcommand.
//
// Subcommands:
//
//	plan-usage show              open the TUI dashboard
//	plan-usage tray              run the native system-tray popup
//	plan-usage opencode-cookie   store/clear the opencode.ai auth cookie
//	plan-usage version           print version info
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/TheMetalStorm/plan-usage/internal/config"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's pure-form helper (kept for testability).
func run(args []string, stdout, stderr *os.File) int {
	scanned := scanArgs(args)
	if scanned.cmd == "" {
		usageTo(stdout)
		return 0
	}

	cfg, err := loadConfig(scanned)
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		cfg = &config.Config{}
		cfg.Defaults()
	}

	switch scanned.cmd {

	case "show", "ui", "tui":
		return errInt(runShow(cfg, scanned.cmdArgs))

	case "tray":
		if err := runTray(cfg); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0

	case "init":
		return runInitStatus(scanned.cmdArgs, stdout, stderr)

	case "opencode-cookie":
		return runCookieStatus(scanned.cmdArgs, stdout, stderr)

	case "version", "-v", "--version":
		fmt.Fprintln(stdout, "plan-usage", version)
		return 0

	case "help", "-h", "--help":
		usageTo(stdout)
		return 0

	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", scanned.cmd)
		usageTo(stderr)
		return 2
	}
}

// scannedArgs holds a parsed command line.
type scannedArgs struct {
	cmd     string
	cmdArgs []string
	cfgPath string
	stateD  string
	debug   bool
	dryRun  bool
}

// scanArgs walks args once, finding flags anywhere and recording the
// first non-flag positional as the subcommand.
func scanArgs(args []string) scannedArgs {
	var out scannedArgs
	var cmdFound bool
	var rest []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		known := false
		switch {
		case a == "--":
			// POSIX end-of-flags: stop scanning, treat the rest as
			// pure positional (first becomes cmd, rest becomes cmdArgs).
			pos := args[i+1:]
			for _, p := range pos {
				if !cmdFound {
					out.cmd = p
					cmdFound = true
					continue
				}
				rest = append(rest, p)
			}
			i = len(args)
			known = true
		case a == "--debug":
			out.debug = true
			known = true
		case a == "--dry-run":
			out.dryRun = true
			known = true
		case strings.HasPrefix(a, "--config="):
			out.cfgPath = strings.TrimPrefix(a, "--config=")
			known = true
		case a == "--config":
			// Only consume the next arg as a value if it doesn't itself
			// look like a flag; this prevents --config --debug from
			// misinterpreting --debug as the path.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				out.cfgPath = args[i+1]
				i++
				known = true
			} else {
				known = true
			}
		case strings.HasPrefix(a, "--state-dir="):
			out.stateD = strings.TrimPrefix(a, "--state-dir=")
			known = true
		case a == "--state-dir":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				out.stateD = args[i+1]
				i++
				known = true
			} else {
				known = true
			}
		}
		if known {
			continue
		}
		// Unknown flag or positional — if it starts with "-" it belongs
		// to the subcommand handler.
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		// First non-flag = subcommand; subsequent go to cmdArgs.
		if !cmdFound {
			out.cmd = a
			cmdFound = true
			continue
		}
		rest = append(rest, a)
	}
	out.cmdArgs = rest
	return out
}

// loadConfig hooks the scanned values into a real Config.
func loadConfig(s scannedArgs) (*config.Config, error) {
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		return nil, err
	}
	if s.stateD != "" {
		cfg.StateDir = s.stateD
	}
	if s.debug {
		cfg.Debug = true
	}
	if s.dryRun {
		cfg.DryRun = true
	}
	return cfg, nil
}

// errInt converts nil to 0 and an error to exit code 1.
func errInt(err error) int {
	if err != nil {
		return 1
	}
	return 0
}

func usageTo(w io.Writer) {
	fmt.Fprint(w, `plan-usage — multi-provider coding-plan monitor

subcommands:
  show           open the interactive TUI dashboard
  tray           run the native system-tray popup
  opencode-cookie
                 store/clear the opencode.ai auth cookie for live Go usage
  version        print version and exit

flags: --config PATH, --state-dir PATH, --debug, --dry-run
   flags may appear before OR after the subcommand.
`)
}

// runInitStatus is a thin wrapper that emits init errors to stderr AND
// returns a non-zero exit code (so the runInit inline stderr write
// doesn't get printed twice by errInt).
func runInitStatus(args []string, stdout, stderr io.Writer) int {
	if err := runInit(args, stdout, stderr); err != nil {
		return 1
	}
	return 0
}

// runCookieStatus mirrors runInitStatus for the opencode-cookie subcommand.
func runCookieStatus(args []string, stdout, stderr io.Writer) int {
	if err := runCookie(args, stdout, stderr); err != nil {
		return 1
	}
	return 0
}

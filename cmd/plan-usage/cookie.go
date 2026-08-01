package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/opencodeutil"
)

// runCookie reads or writes the OpenCode AI session cookie cache used by
// the opencodego provider's server-side usage overlay. The raw cookie value
// is never printed back to the terminal.
func runCookie(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		return fmt.Errorf("cookie cache: %w", err)
	}

	for _, a := range args {
		switch a {
		case "--clear", "-c":
			if err := cc.Write(&opencodeutil.CacheCookie{}); err != nil {
				return fmt.Errorf("clear cookie: %w", err)
			}
			fmt.Fprintln(stdout, "opencode cookie cleared")
			return nil
		case "--help", "-h":
			fmt.Fprint(stdout, cookieUsage)
			return nil
		}
	}

	if len(args) > 0 {
		switch args[0] {
		case "import", "-i":
			return runCookieImport(cc, stdout)
		case "-":
			value, err := readStdinValue(stdin)
			if err != nil {
				return err
			}
			if err := cc.Write(&opencodeutil.CacheCookie{Source: "cli", Cookie: value, CachedAt: time.Now()}); err != nil {
				return fmt.Errorf("write cookie: %w", err)
			}
			fmt.Fprintln(stdout, "opencode cookie saved")
			return nil
		}
		if args[0] != "" {
			if err := cc.Write(&opencodeutil.CacheCookie{Source: "cli", Cookie: args[0], CachedAt: time.Now()}); err != nil {
				return fmt.Errorf("write cookie: %w", err)
			}
			fmt.Fprintln(stdout, "opencode cookie saved")
			return nil
		}
	}

	// No value: report cache state without leaking the session secret.
	cached, err := cc.Read()
	if err != nil {
		return fmt.Errorf("read cookie: %w", err)
	}
	if cached == nil || cached.Cookie == "" {
		fmt.Fprintln(stdout, "no opencode cookie cached")
	} else {
		fmt.Fprintf(stdout, "opencode cookie cached: source=%s cached_at=%s\n", cached.Source, cached.CachedAt.Format(time.RFC3339))
	}
	fmt.Fprint(stdout, cookieUsage)
	return nil
}

// runCookieImport pulls the opencode.ai auth cookie out of the browser
// cookie stores and caches it. It never prints the cookie value.
func runCookieImport(cc *opencodeutil.CookieCache, stdout io.Writer) error {
	value, err := opencodeutil.ImportOpenCodeCookie()
	if err != nil {
		return err
	}
	if value == "" {
		fmt.Fprintln(stdout, "no opencode.ai auth cookie found in browser cookies — log in at opencode.ai, then re-run 'plan-usage opencode-cookie import', or paste the cookie with 'plan-usage opencode-cookie -'")
		return nil
	}
	if err := cc.Write(&opencodeutil.CacheCookie{Source: "browser-import", Cookie: value, CachedAt: time.Now()}); err != nil {
		return fmt.Errorf("write cookie: %w", err)
	}
	fmt.Fprintln(stdout, "opencode cookie imported from browser cookie store")
	return nil
}

// readStdinValue reads one cookie value from stdin, trimming surrounding
// whitespace/newlines.
func readStdinValue(stdin io.Reader) (string, error) {
	if stdin == nil {
		return "", fmt.Errorf("read cookie: no stdin reader")
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read cookie from stdin: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

const cookieUsage = `usage:
  plan-usage opencode-cookie "<cookie value>"   store the opencode.ai auth cookie
  plan-usage opencode-cookie import             import the auth cookie from Chrome/Chromium/Brave/Edge/Firefox/Safari
  plan-usage opencode-cookie -                  store the auth cookie read from stdin
  plan-usage opencode-cookie --clear            remove the cached cookie
  plan-usage opencode-cookie                    show cache state (never the value)

The OpenCode Go card reads live usage percentages (5h rolling / weekly /
monthly) from opencode.ai. The _server endpoint only accepts a browser
session cookie — the "auth" cookie for opencode.ai (browser DevTools ->
Application -> Cookies -> https://opencode.ai). Without it the card falls
back to local opencode.db cost estimates (labeled "local costs ...").

Import re-reads the cookie from your browser, and the tray/daemon does the
same automatically whenever no cookie is cached, so you only need to stay
logged in at opencode.ai.
`

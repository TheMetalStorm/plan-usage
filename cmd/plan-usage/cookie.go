package main

import (
	"fmt"
	"io"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/opencodeutil"
)

// runCookie reads or writes the OpenCode AI session cookie cache used by
// the opencodego provider's server-side usage overlay. The raw cookie value
// is never printed back to the terminal.
func runCookie(args []string, stdout, stderr io.Writer) error {
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

	if len(args) > 0 && args[0] != "" {
		if err := cc.Write(&opencodeutil.CacheCookie{Source: "cli", Cookie: args[0], CachedAt: time.Now()}); err != nil {
			return fmt.Errorf("write cookie: %w", err)
		}
		fmt.Fprintln(stdout, "opencode cookie saved")
		return nil
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

const cookieUsage = `usage:
  plan-usage opencode-cookie "<cookie value>"   store the opencode.ai auth cookie
  plan-usage opencode-cookie --clear            remove the cached cookie
  plan-usage opencode-cookie                    show cache state (never the value)

The OpenCode Go card reads live usage percentages (5h rolling / weekly /
monthly) from opencode.ai. The _server endpoint only accepts a browser
session cookie — the "auth" cookie for opencode.ai (Chrome DevTools ->
Application -> Cookies -> https://opencode.ai). Without it the card falls
back to local opencode.db cost estimates (labeled "local costs ...").
`

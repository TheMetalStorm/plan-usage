// Package opencodeutil provides shared helpers for the OpenCode (Zen) and
// OpenCode Go providers — reading the local SQLite DB, talking to the
// opencode.ai /_server RPC endpoint, and caching session cookies.
package opencodeutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CacheCookie holds one cached OpenCode AI session cookie.
type CacheCookie struct {
	Source   string    `json:"source"`    // e.g. "manual", "browser-import"
	Cookie   string    `json:"cookie"`    // raw Cookie header value
	CachedAt time.Time `json:"cached_at"` // when we stored it
}

// CookieCache manages a file-backed cookie store for opencode.ai sessions.
type CookieCache struct {
	path string
}

// NewCookieCache creates a cache rooted at ~/.local/share/plan-usage.
func NewCookieCache() (*CookieCache, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		dir = filepath.Join(home, ".local", "state", "plan-usage")
	} else {
		dir = filepath.Join(dir, "plan-usage")
	}
	return &CookieCache{path: filepath.Join(dir, "opencode-cookies.json")}, nil
}

// Read loads the cached cookie, if any.
func (c *CookieCache) Read() (*CacheCookie, error) {
	raw, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cc CacheCookie
	if err := json.Unmarshal(raw, &cc); err != nil {
		return nil, fmt.Errorf("cookie cache: parse: %w", err)
	}
	return &cc, nil
}

// Write stores a cookie.
func (c *CookieCache) Write(cc *CacheCookie) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, raw, 0o600)
}

// Cookie returns the stored cookie value (or empty string).
func (c *CookieCache) Cookie() string {
	cc, err := c.Read()
	if err != nil || cc == nil {
		return ""
	}
	return cc.Cookie
}

// AttachToRequest sets the Cookie header on an outgoing request from cache.
func (c *CookieCache) AttachToRequest(req *http.Request) {
	if cc, _ := c.Read(); cc != nil && cc.Cookie != "" {
		if value := cookieHeaderValue(cc.Cookie); value != "" {
			req.Header.Set("Cookie", value)
		}
	}
}

// cookieHeaderValue turns the bare auth value stored by browsers into a valid
// HTTP Cookie header. Complete Cookie header strings pass through unchanged
// for compatibility with values captured from browser DevTools.
func cookieHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "=") {
		return value
	}
	return "auth=" + value
}

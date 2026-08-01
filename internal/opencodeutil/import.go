package opencodeutil

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// browserProfile describes one Chrome-family browser's cookie store.
type browserProfile struct {
	dir string // config dir name, e.g. "google-chrome"
	app string // flatpak app id, e.g. "com.google.Chrome" ("" = no flatpak)
}

// cookieBrowserProfiles lists the browsers we scan for opencode.ai cookies,
// native installs first, then Flatpak sandboxes, in preference order.
var cookieBrowserProfiles = []browserProfile{
	{dir: "google-chrome", app: "com.google.Chrome"},
	{dir: "chromium", app: "org.chromium.Chromium"},
	{dir: "brave-browser"},
	{dir: "microsoft-edge"},
	{dir: "google-chrome-unstable", app: "com.google.ChromeDev"},
	{dir: "chromium", app: "io.github.ungoogled_software.ungoogled_chromium"},
}

// DefaultCookieDBPaths returns the candidate Chrome-family cookie database
// paths for this machine (native config dirs plus Flatpak sandboxes). Paths
// that do not exist are omitted, so the list reflects only stores that are
// actually installed.
func DefaultCookieDBPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}
	for _, b := range cookieBrowserProfiles {
		add(filepath.Join(home, ".config", b.dir, "Default", "Cookies"))
		if b.app != "" {
			add(filepath.Join(home, ".var", "app", b.app, "config", b.dir, "Default", "Cookies"))
		}
	}
	return paths
}

// ImportOpenCodeCookieFrom scans the given Chrome-family cookie databases
// for the opencode.ai "auth" session cookie and returns its raw value. It
// returns ("", nil) when no opencode.ai cookie is present. It returns an
// error when a matching cookie exists but cannot be used directly (e.g. the
// value is encrypted, as some Chrome installs do).
func ImportOpenCodeCookieFrom(paths []string) (string, error) {
	for _, path := range paths {
		value, err := readOpenCodeAuthCookie(path)
		if err != nil {
			return "", err
		}
		if value != "" {
			return value, nil
		}
	}
	return "", nil
}

// ImportOpenCodeCookie imports the opencode.ai "auth" session cookie from
// the default browser cookie stores.
func ImportOpenCodeCookie() (string, error) {
	return ImportOpenCodeCookieFrom(DefaultCookieDBPaths())
}

// readOpenCodeAuthCookie looks up the opencode.ai "auth" cookie in one
// cookie database. Returns "" when the DB exists but has no usable
// opencode.ai cookie. The DB is opened read-only and never modified.
func readOpenCodeAuthCookie(path string) (string, error) {
	db, err := openCookieDB(path)
	if err != nil {
		// Unreadable store (e.g. locked mid-write by the browser): skip it.
		return "", nil
	}
	defer db.Close()

	const query = `SELECT value FROM cookies
		WHERE name = 'auth'
		  AND (host_key = 'opencode.ai' OR host_key LIKE '%.opencode.ai')
		ORDER BY creation_utc DESC LIMIT 1`
	var value string
	err = db.QueryRow(query).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("cookie db %s: query: %w", path, err)
	}
	if strings.HasPrefix(value, "v10") || strings.HasPrefix(value, "v20") {
		return "", fmt.Errorf("cookie db %s: opencode.ai cookie is encrypted (v10/v20) and cannot be imported; use 'plan-usage opencode-cookie -' to paste it", path)
	}
	return value, nil
}

// openCookieDB opens a Chrome cookie database read-only. Chrome keeps the
// store in WAL mode, so a plain read-only connection is tried first (it can
// read the WAL); an immutable fallback covers stores that are locked or
// mid-checkpoint, at the cost of only seeing checkpointed rows.
func openCookieDB(path string) (*sql.DB, error) {
	dsns := []string{
		path + "?mode=ro&_journal_mode=WAL&_query_only=true",
		path + "?mode=ro&immutable=1&_query_only=true",
	}
	var lastErr error
	for _, dsn := range dsns {
		db, err := sql.Open("sqlite", dsn)
		if err == nil {
			if err = db.Ping(); err == nil {
				return db, nil
			}
			db.Close()
			lastErr = err
			continue
		}
		lastErr = err
	}
	return nil, lastErr
}

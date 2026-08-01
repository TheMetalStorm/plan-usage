package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheMetalStorm/plan-usage/internal/opencodeutil"

	_ "modernc.org/sqlite"
)

// withCookieState points the cookie cache at a temp XDG_STATE_HOME for the
// duration of one test.
func withCookieState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

// emptyStdin is the io.Reader passed to runCookie when a test exercises
// modes that don't read stdin.
var emptyStdin = strings.NewReader("")

// seedBrowserCookie writes a minimal Chrome-style cookie DB at path holding a
// plaintext "auth" cookie for .opencode.ai.
func seedBrowserCookie(t *testing.T, path, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?mode=rwc")
	if err != nil {
		t.Fatalf("open cookie db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE cookies (
		creation_utc INTEGER NOT NULL,
		host_key TEXT NOT NULL,
		name TEXT NOT NULL,
		value TEXT NOT NULL,
		path TEXT NOT NULL,
		expires_utc INTEGER NOT NULL,
		is_secure INTEGER NOT NULL,
		is_httponly INTEGER NOT NULL,
		last_access_utc INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create cookies table: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO cookies (creation_utc, host_key, name, value, path, expires_utc, is_secure, is_httponly, last_access_utc)
		 VALUES (?, '.opencode.ai', 'auth', ?, '/', 0, 1, 1, 0)`,
		int64(1_300_000_000_000_000), value,
	); err != nil {
		t.Fatalf("insert cookie: %v", err)
	}
}

func TestRunCookieWritesAndRoundTrips(t *testing.T) {
	withCookieState(t)
	var stdout, stderr bytes.Buffer
	if err := runCookie([]string{"secret-cookie"}, emptyStdin, &stdout, &stderr); err != nil {
		t.Fatalf("runCookie(write) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "saved") {
		t.Fatalf("write output missing confirmation: %q", stdout.String())
	}

	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	cached, err := cc.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cached == nil || cached.Cookie != "secret-cookie" || cached.Source != "cli" {
		t.Fatalf("cached = %#v, want Cookie=secret-cookie Source=cli", cached)
	}
}

func TestRunCookieNoArgDoesNotLeak(t *testing.T) {
	withCookieState(t)
	var stdout bytes.Buffer
	if err := runCookie([]string{"super-secret-value"}, emptyStdin, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(write) error = %v", err)
	}

	stdout.Reset()
	if err := runCookie(nil, emptyStdin, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(status) error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "source=cli") {
		t.Fatalf("status output missing cache state: %q", out)
	}
	if strings.Contains(out, "super-secret-value") {
		t.Fatalf("status output leaked the cookie value: %q", out)
	}
}

func TestRunCookieClear(t *testing.T) {
	withCookieState(t)
	if err := runCookie([]string{"secret"}, emptyStdin, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(write) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runCookie([]string{"--clear"}, emptyStdin, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(clear) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "cleared") {
		t.Fatalf("clear output = %q, want confirmation", stdout.String())
	}

	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	if cc.Cookie() != "" {
		t.Fatalf("cookie still set after --clear: %q", cc.Cookie())
	}
}

func TestRunCookieStdin(t *testing.T) {
	withCookieState(t)
	var stdout bytes.Buffer
	if err := runCookie([]string{"-"}, strings.NewReader("stdin-cookie\n"), &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(stdin) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "saved") {
		t.Fatalf("stdin output = %q, want confirmation", stdout.String())
	}

	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	cached, err := cc.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cached == nil || cached.Cookie != "stdin-cookie" || cached.Source != "cli" {
		t.Fatalf("cached = %#v, want Cookie=stdin-cookie Source=cli", cached)
	}
}

func TestRunCookieImportNotInstalled(t *testing.T) {
	withCookieState(t)
	t.Setenv("HOME", t.TempDir()) // no browser cookie stores installed

	var stdout bytes.Buffer
	if err := runCookie([]string{"import"}, emptyStdin, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(import) error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "no opencode.ai auth cookie found in browser cookies") {
		t.Fatalf("import output = %q, want not-found message", out)
	}

	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	if cc.Cookie() != "" {
		t.Fatalf("cache populated despite no cookie found: %q", cc.Cookie())
	}
}

func TestRunCookieImportFromBrowser(t *testing.T) {
	withCookieState(t)

	// Fake an installed google-chrome store holding a plaintext auth cookie.
	home := t.TempDir()
	t.Setenv("HOME", home)
	chrome := filepath.Join(home, ".config", "google-chrome", "Default")
	if err := os.MkdirAll(chrome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(chrome, "Cookies")
	seedBrowserCookie(t, path, "browser-imported-cookie")

	var stdout bytes.Buffer
	if err := runCookie([]string{"import"}, emptyStdin, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(import) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "imported from browser cookie store") {
		t.Fatalf("import output = %q, want imported confirmation", stdout.String())
	}

	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	cached, err := cc.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cached == nil || cached.Cookie != "browser-imported-cookie" || cached.Source != "chrome-import" {
		t.Fatalf("cached = %#v, want Cookie=browser-imported-cookie Source=chrome-import", cached)
	}
}

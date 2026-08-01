package opencodeutil

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// cookieRow is one (host_key, name, value) row to seed into a temp cookie DB.
type cookieRow struct {
	host string
	name string
	val  string
}

// seedCookieDB creates a temp Chrome-style cookie DB with the given rows and
// returns its path. Rows are inserted in order, so later rows are newer.
func seedCookieDB(t *testing.T, rows []cookieRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Cookies")
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
		last_access_utc INTEGER NOT NULL,
		has_expires INTEGER NOT NULL DEFAULT 1,
		is_persistent INTEGER NOT NULL DEFAULT 1,
		priority INTEGER NOT NULL DEFAULT 1,
		encrypted_value BLOB DEFAULT '',
		samesite INTEGER NOT NULL DEFAULT -1
	)`); err != nil {
		t.Fatalf("create cookies table: %v", err)
	}
	for i, r := range rows {
		utc := int64(1_300_000_000_000_000) + int64(i)*1_000_000 // chrome ticks since 1601
		if _, err := db.Exec(
			`INSERT INTO cookies (creation_utc, host_key, name, value, path, expires_utc, is_secure, is_httponly, last_access_utc)
			 VALUES (?, ?, ?, ?, '/', 0, 1, 1, 0)`,
			utc, r.host, r.name, r.val,
		); err != nil {
			t.Fatalf("insert cookie %d: %v", i, err)
		}
	}
	return path
}

func TestImport_FindsPlaintextAuthCookie(t *testing.T) {
	path := seedCookieDB(t, []cookieRow{
		{".opencode.ai", "session", "v10-encrypted-decoy"}, // encrypted but wrong cookie name
		{"example.com", "auth", "v20-encrypted-decoy"},     // encrypted but wrong host
		{".opencode.ai", "auth", ""},                       // empty value, older
		{".opencode.ai", "auth", "real-value"},             // newest plaintext
	})
	got, err := ImportOpenCodeCookieFrom([]string{path})
	if err != nil {
		t.Fatalf("ImportOpenCodeCookieFrom error = %v", err)
	}
	if got != "real-value" {
		t.Fatalf("got %q, want %q", got, "real-value")
	}
}

func TestImport_IgnoresWrongHost(t *testing.T) {
	path := seedCookieDB(t, []cookieRow{
		{"example.com", "auth", "secret"},
		{".notopencode.ai", "auth", "secret2"},
	})
	got, err := ImportOpenCodeCookieFrom([]string{path})
	if err != nil {
		t.Fatalf("ImportOpenCodeCookieFrom error = %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestImport_EncryptedValueError(t *testing.T) {
	path := seedCookieDB(t, []cookieRow{
		{".opencode.ai", "auth", "v20really-encrypted"},
	})
	_, err := ImportOpenCodeCookieFrom([]string{path})
	if err == nil {
		t.Fatal("expected an error for an encrypted opencode.ai cookie")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q should mention the db path %q", err, path)
	}
}

func TestImport_EmptyPaths(t *testing.T) {
	got, err := ImportOpenCodeCookieFrom(nil)
	if err != nil {
		t.Fatalf("ImportOpenCodeCookieFrom(nil) error = %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestDefaultCookieDBPaths_SkipsMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Only google-chrome exists.
	chrome := filepath.Join(home, ".config", "google-chrome", "Default")
	if err := os.MkdirAll(chrome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chrome, "Cookies"), nil, 0o600); err != nil {
		t.Fatalf("write Cookies: %v", err)
	}

	paths := DefaultCookieDBPaths()
	if len(paths) != 1 {
		t.Fatalf("DefaultCookieDBPaths len = %d, want 1 (only existing stores): %v", len(paths), paths)
	}
	want := filepath.Join(chrome, "Cookies")
	if paths[0] != want {
		t.Fatalf("path[0] = %q, want %q", paths[0], want)
	}
}

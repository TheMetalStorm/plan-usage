package opencodeutil

import (
	"database/sql"
	"encoding/binary"
	"math"
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

func TestImport_EncryptedColumnError(t *testing.T) {
	path := seedCookieDB(t, []cookieRow{{".opencode.ai", "auth", ""}})
	db, err := sql.Open("sqlite", path+"?mode=rwc")
	if err != nil {
		t.Fatalf("open cookie db: %v", err)
	}
	if _, err := db.Exec(`UPDATE cookies SET encrypted_value = ? WHERE name = 'auth'`, []byte("v10ciphertext")); err != nil {
		db.Close()
		t.Fatalf("update encrypted cookie: %v", err)
	}
	db.Close()

	_, err = ImportOpenCodeCookieFrom([]string{path})
	if err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("error = %v, want encrypted-cookie error", err)
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

func TestDefaultCookieDBPaths_FindsProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, profile := range []string{"Default", "Profile 1"} {
		dir := filepath.Join(home, ".config", "microsoft-edge", profile)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", profile, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Cookies"), nil, 0o600); err != nil {
			t.Fatalf("write %s: %v", profile, err)
		}
	}

	paths := DefaultCookieDBPaths()
	want := []string{
		filepath.Join(home, ".config", "microsoft-edge", "Default", "Cookies"),
		filepath.Join(home, ".config", "microsoft-edge", "Profile 1", "Cookies"),
	}
	if len(paths) != len(want) {
		t.Fatalf("DefaultCookieDBPaths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("path[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}

func TestDefaultFirefoxCookieDBPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profile := filepath.Join(home, ".mozilla", "firefox", "abc.default-release")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatalf("mkdir Firefox profile: %v", err)
	}
	want := filepath.Join(profile, "cookies.sqlite")
	if err := os.WriteFile(want, nil, 0o600); err != nil {
		t.Fatalf("write Firefox cookie DB: %v", err)
	}

	paths := DefaultFirefoxCookieDBPaths()
	if len(paths) != 1 || paths[0] != want {
		t.Fatalf("DefaultFirefoxCookieDBPaths = %v, want [%q]", paths, want)
	}
}

type firefoxCookieRow struct {
	host       string
	name       string
	value      string
	lastAccess int64
	created    int64
}

func seedFirefoxCookieDB(t *testing.T, rows []firefoxCookieRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.sqlite")
	db, err := sql.Open("sqlite", path+"?mode=rwc")
	if err != nil {
		t.Fatalf("open Firefox cookie DB: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE moz_cookies (
		id INTEGER PRIMARY KEY,
		baseDomain TEXT,
		originAttributes TEXT,
		name TEXT,
		value TEXT,
		host TEXT,
		path TEXT,
		expires INTEGER,
		lastAccessed INTEGER,
		creationTime INTEGER
	)`); err != nil {
		t.Fatalf("create moz_cookies: %v", err)
	}
	for i, row := range rows {
		if _, err := db.Exec(`INSERT INTO moz_cookies
			(id, baseDomain, originAttributes, name, value, host, path, expires, lastAccessed, creationTime)
			VALUES (?, ?, '', ?, ?, ?, '/', 0, ?, ?)`,
			i+1, strings.TrimPrefix(row.host, "."), row.name, row.value, row.host, row.lastAccess, row.created); err != nil {
			t.Fatalf("insert Firefox cookie: %v", err)
		}
	}
	return path
}

func TestImportFirefoxPlaintextCookie(t *testing.T) {
	path := seedFirefoxCookieDB(t, []firefoxCookieRow{
		{host: ".opencode.ai", name: "auth", value: "old-value", lastAccess: 1, created: 1},
		{host: "opencode.ai", name: "auth", value: "new-value", lastAccess: 2, created: 2},
	})
	got, err := importFirefoxOpenCodeCookieFrom([]string{path})
	if err != nil {
		t.Fatalf("Firefox import error = %v", err)
	}
	if got != "new-value" {
		t.Fatalf("Firefox import = %q, want %q", got, "new-value")
	}
}

func TestImportFirefoxIgnoresWrongHost(t *testing.T) {
	path := seedFirefoxCookieDB(t, []firefoxCookieRow{
		{host: ".example.com", name: "auth", value: "foreign", lastAccess: 10, created: 10},
		{host: ".notopencode.ai", name: "auth", value: "foreign2", lastAccess: 11, created: 11},
	})
	got, err := importFirefoxOpenCodeCookieFrom([]string{path})
	if err != nil {
		t.Fatalf("Firefox import error = %v", err)
	}
	if got != "" {
		t.Fatalf("Firefox import = %q, want empty", got)
	}
}

type safariFixtureCookie struct {
	domain   string
	name     string
	value    string
	creation float64
}

func safariBinaryFixture(rows []safariFixtureCookie) []byte {
	records := make([][]byte, 0, len(rows))
	for _, row := range rows {
		domain := append([]byte(row.domain), 0)
		name := append([]byte(row.name), 0)
		path := []byte("/")
		path = append(path, 0)
		value := append([]byte(row.value), 0)
		record := make([]byte, 56+len(domain)+len(name)+len(path)+len(value))
		binary.LittleEndian.PutUint32(record[0:4], uint32(len(record)))
		cursor := 56
		binary.LittleEndian.PutUint32(record[16:20], uint32(cursor))
		copy(record[cursor:], domain)
		cursor += len(domain)
		binary.LittleEndian.PutUint32(record[20:24], uint32(cursor))
		copy(record[cursor:], name)
		cursor += len(name)
		binary.LittleEndian.PutUint32(record[24:28], uint32(cursor))
		copy(record[cursor:], path)
		cursor += len(path)
		binary.LittleEndian.PutUint32(record[28:32], uint32(cursor))
		copy(record[cursor:], value)
		binary.LittleEndian.PutUint64(record[48:56], math.Float64bits(row.creation))
		records = append(records, record)
	}

	headerSize := 8 + len(records)*4
	pageSize := headerSize + 4
	for _, record := range records {
		pageSize += len(record)
	}
	page := make([]byte, pageSize)
	binary.BigEndian.PutUint32(page[0:4], 0x00000100)
	binary.LittleEndian.PutUint32(page[4:8], uint32(len(records)))
	cursor := headerSize
	for i, record := range records {
		binary.LittleEndian.PutUint32(page[8+i*4:12+i*4], uint32(cursor))
		copy(page[cursor:], record)
		cursor += len(record)
	}

	data := make([]byte, 8+4+pageSize)
	copy(data[:4], []byte("cook"))
	binary.BigEndian.PutUint32(data[4:8], 1)
	binary.BigEndian.PutUint32(data[8:12], uint32(pageSize))
	copy(data[12:], page)
	return data
}

func TestSafariBinaryCookieImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Cookies.binarycookies")
	data := safariBinaryFixture([]safariFixtureCookie{
		{domain: ".example.com", name: "auth", value: "foreign", creation: 99},
		{domain: ".opencode.ai", name: "auth", value: "old-value", creation: 1},
		{domain: "opencode.ai", name: "auth", value: "safari-value", creation: 2},
	})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write Safari fixture: %v", err)
	}
	got, err := importSafariOpenCodeCookieFrom([]string{path})
	if err != nil {
		t.Fatalf("Safari import error = %v", err)
	}
	if got != "safari-value" {
		t.Fatalf("Safari import = %q, want %q", got, "safari-value")
	}
}

func TestSafariBinaryCookieIgnoresWrongDomain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Cookies.binarycookies")
	data := safariBinaryFixture([]safariFixtureCookie{{domain: ".notopencode.ai", name: "auth", value: "foreign", creation: 1}})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write Safari fixture: %v", err)
	}
	got, err := importSafariOpenCodeCookieFrom([]string{path})
	if err != nil {
		t.Fatalf("Safari import error = %v", err)
	}
	if got != "" {
		t.Fatalf("Safari import = %q, want empty", got)
	}
}

func TestSafariBinaryCookieMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Cookies.binarycookies")
	if err := os.WriteFile(path, []byte("cook\x01\x00"), 0o600); err != nil {
		t.Fatalf("write malformed Safari fixture: %v", err)
	}
	got, err := importSafariOpenCodeCookieFrom([]string{path})
	if got != "" {
		t.Fatalf("Safari malformed import = %q, want empty", got)
	}
	if err == nil {
		t.Fatal("Safari malformed import error = nil, want malformed-file error")
	}
}

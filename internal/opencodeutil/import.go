package opencodeutil

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
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
	{dir: "brave-browser", app: "com.brave.Browser"},
	{dir: "microsoft-edge", app: "com.microsoft.Edge"},
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
		addChromiumProfiles(filepath.Join(home, ".config", b.dir), add)
		if b.app != "" {
			addChromiumProfiles(filepath.Join(home, ".var", "app", b.app, "config", b.dir), add)
		}
	}
	return paths
}

// addChromiumProfiles adds the Default and named Profile directories for one
// Chromium-family browser. filepath.Glob returns sorted matches, making the
// scan deterministic while still finding users who logged in outside Default.
func addChromiumProfiles(root string, add func(string)) {
	add(filepath.Join(root, "Default", "Cookies"))
	profiles, _ := filepath.Glob(filepath.Join(root, "Profile *", "Cookies"))
	for _, path := range profiles {
		add(path)
	}
}

// DefaultFirefoxCookieDBPaths returns Firefox cookie stores from native,
// Flatpak, and Snap profile roots. Missing profiles are omitted.
func DefaultFirefoxCookieDBPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	roots := []string{
		filepath.Join(home, ".config", "mozilla", "firefox"),
		filepath.Join(home, ".mozilla", "firefox"),
		filepath.Join(home, ".var", "app", "org.mozilla.firefox", ".mozilla", "firefox"),
		filepath.Join(home, "snap", "firefox", "common", ".mozilla", "firefox"),
	}
	var paths []string
	seen := map[string]bool{}
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, "*", "cookies.sqlite"))
		for _, path := range matches {
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	return paths
}

// DefaultSafariCookiePaths returns the common macOS Safari binary cookie
// locations. On non-macOS systems these simply return an empty list.
func DefaultSafariCookiePaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	candidates := []string{
		filepath.Join(home, "Library", "Cookies", "Cookies.binarycookies"),
		filepath.Join(home, "Library", "Containers", "com.apple.Safari", "Data", "Library", "Cookies", "Cookies.binarycookies"),
	}
	var paths []string
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			paths = append(paths, path)
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
	var firstErr error
	for _, path := range paths {
		value, err := readOpenCodeAuthCookie(path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if value != "" {
			return value, nil
		}
	}
	return "", firstErr
}

// ImportOpenCodeCookie imports the opencode.ai "auth" session cookie from
// the default Chromium, Firefox, and Safari cookie stores.
func ImportOpenCodeCookie() (string, error) {
	var firstErr error
	for _, scan := range []func() (string, error){
		func() (string, error) { return ImportOpenCodeCookieFrom(DefaultCookieDBPaths()) },
		func() (string, error) { return importFirefoxOpenCodeCookieFrom(DefaultFirefoxCookieDBPaths()) },
		func() (string, error) { return importSafariOpenCodeCookieFrom(DefaultSafariCookiePaths()) },
	} {
		value, err := scan()
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if value != "" {
			return value, nil
		}
	}
	return "", firstErr
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

	const query = `SELECT value, encrypted_value FROM cookies
		WHERE name = 'auth'
		  AND (host_key = 'opencode.ai' OR host_key LIKE '%.opencode.ai')
		ORDER BY creation_utc DESC LIMIT 1`
	var value string
	var encrypted []byte
	err = db.QueryRow(query).Scan(&value, &encrypted)
	if err != nil && strings.Contains(err.Error(), "no such column: encrypted_value") {
		// Older Chromium-compatible stores may not have the encrypted_value
		// column. Preserve support for their plaintext value column.
		const legacyQuery = `SELECT value FROM cookies
			WHERE name = 'auth'
			  AND (host_key = 'opencode.ai' OR host_key LIKE '%.opencode.ai')
			ORDER BY creation_utc DESC LIMIT 1`
		err = db.QueryRow(legacyQuery).Scan(&value)
	}
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("cookie db %s: query: %w", path, err)
	}
	if value != "" {
		if isEncryptedCookieValue(value) {
			return "", encryptedCookieError(path)
		}
		return value, nil
	}
	if len(encrypted) > 0 {
		return "", encryptedCookieError(path)
	}
	return "", nil
}

func encryptedCookieError(path string) error {
	return fmt.Errorf("cookie db %s: opencode.ai cookie is encrypted and cannot be imported; use 'plan-usage opencode-cookie -' to paste it", path)
}

func isEncryptedCookieValue(value string) bool {
	return strings.HasPrefix(value, "v10") || strings.HasPrefix(value, "v11") || strings.HasPrefix(value, "v20")
}

func importFirefoxOpenCodeCookieFrom(paths []string) (string, error) {
	var firstErr error
	for _, path := range paths {
		value, err := readFirefoxOpenCodeAuthCookie(path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if value != "" {
			return value, nil
		}
	}
	return "", firstErr
}

func readFirefoxOpenCodeAuthCookie(path string) (string, error) {
	// Firefox keeps cookies.sqlite open while the browser runs. Prefer an
	// immutable read first so a live Firefox profile does not make the query
	// report SQLITE_BUSY after Ping succeeds.
	db, err := openFirefoxCookieDB(path)
	if err != nil {
		return "", nil
	}
	defer db.Close()

	const query = `SELECT value FROM moz_cookies
		WHERE name = 'auth'
		  AND (host = 'opencode.ai' OR host LIKE '%.opencode.ai')
		ORDER BY lastAccessed DESC, creationTime DESC LIMIT 1`
	var value string
	if err := db.QueryRow(query).Scan(&value); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("Firefox cookie db %s: query: %w", path, err)
	}
	return value, nil
}

func openFirefoxCookieDB(path string) (*sql.DB, error) {
	return openCookieDBWithDSNs(path, []string{
		"file:" + path + "?immutable=1&mode=ro",
		path + "?mode=ro&_journal_mode=WAL&_query_only=true",
	})
}

func importSafariOpenCodeCookieFrom(paths []string) (string, error) {
	var firstErr error
	for _, path := range paths {
		value, err := readSafariOpenCodeAuthCookie(path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if value != "" {
			return value, nil
		}
	}
	return "", firstErr
}

func readSafariOpenCodeAuthCookie(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	cookies, err := parseSafariBinaryCookies(raw)
	if err != nil {
		return "", fmt.Errorf("Safari cookie file %s: %w", path, err)
	}
	var best safariCookie
	found := false
	for _, cookie := range cookies {
		if cookie.Name != "auth" || !isOpenCodeHost(cookie.Domain) {
			continue
		}
		if !found || cookie.Creation > best.Creation {
			best = cookie
			found = true
		}
	}
	if !found {
		return "", nil
	}
	return best.Value, nil
}

func isOpenCodeHost(host string) bool {
	host = strings.TrimSpace(strings.TrimPrefix(host, "."))
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}

type safariCookie struct {
	Domain   string
	Name     string
	Value    string
	Creation float64
}

// parseSafariBinaryCookies parses Apple's Cookies.binarycookies format. Page
// sizes are big-endian; page and record fields are little-endian. All offsets
// are checked against their containing page before any slice is taken.
func parseSafariBinaryCookies(data []byte) ([]safariCookie, error) {
	if len(data) < 8 || string(data[:4]) != "cook" {
		return nil, fmt.Errorf("invalid binary cookie header")
	}
	pageCount := int(binary.BigEndian.Uint32(data[4:8]))
	if pageCount < 1 || pageCount > 10000 || len(data) < 8+pageCount*4 {
		return nil, fmt.Errorf("invalid binary cookie page table")
	}
	pageSizes := make([]int, pageCount)
	pos := 8
	for i := range pageSizes {
		size := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		pos += 4
		if size < 12 || size > len(data)-pos {
			return nil, fmt.Errorf("invalid binary cookie page size")
		}
		pageSizes[i] = size
	}

	var cookies []safariCookie
	for _, pageSize := range pageSizes {
		page := data[pos : pos+pageSize]
		pos += pageSize
		if len(page) < 8 || binary.BigEndian.Uint32(page[:4]) != 0x00000100 {
			return nil, fmt.Errorf("invalid binary cookie page header")
		}
		count := int(binary.LittleEndian.Uint32(page[4:8]))
		if count < 0 || count > 100000 || 8+count*4 > len(page) {
			return nil, fmt.Errorf("invalid binary cookie record table")
		}
		for i := 0; i < count; i++ {
			off := int(binary.LittleEndian.Uint32(page[8+i*4 : 12+i*4]))
			cookie, ok := parseSafariCookieRecord(page, off)
			if ok {
				cookies = append(cookies, cookie)
			}
		}
	}
	return cookies, nil
}

func parseSafariCookieRecord(page []byte, offset int) (safariCookie, bool) {
	var out safariCookie
	if offset < 0 || offset+56 > len(page) {
		return out, false
	}
	record := page[offset:]
	size := int(binary.LittleEndian.Uint32(record[:4]))
	if size < 56 || size > len(record) {
		return out, false
	}
	record = record[:size]
	readString := func(fieldOffset int) (string, bool) {
		if fieldOffset+4 > len(record) {
			return "", false
		}
		start := int(binary.LittleEndian.Uint32(record[fieldOffset : fieldOffset+4]))
		if start < 0 || start >= len(record) {
			return "", false
		}
		end := start
		for end < len(record) && record[end] != 0 {
			end++
		}
		if end == len(record) {
			return "", false
		}
		return string(record[start:end]), true
	}
	domain, ok := readString(16)
	if !ok {
		return out, false
	}
	name, ok := readString(20)
	if !ok {
		return out, false
	}
	value, ok := readString(28)
	if !ok {
		return out, false
	}
	creation := math.Float64frombits(binary.LittleEndian.Uint64(record[48:56]))
	return safariCookie{Domain: domain, Name: name, Value: value, Creation: creation}, true
}

// openCookieDB opens a Chrome cookie database read-only. Chrome keeps the
// store in WAL mode, so a plain read-only connection is tried first (it can
// read the WAL); an immutable fallback covers stores that are locked or
// mid-checkpoint, at the cost of only seeing checkpointed rows.
func openCookieDB(path string) (*sql.DB, error) {
	return openCookieDBWithDSNs(path, []string{
		path + "?mode=ro&_journal_mode=WAL&_query_only=true",
		path + "?mode=ro&immutable=1&_query_only=true",
	})
}

func openCookieDBWithDSNs(path string, dsns []string) (*sql.DB, error) {
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

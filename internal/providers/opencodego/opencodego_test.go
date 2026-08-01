package opencodego

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/opencodeutil"
	"github.com/TheMetalStorm/plan-usage/internal/types"

	_ "modernc.org/sqlite"
)

func providerWithServer(s *opencodeutil.ServerUsage, at time.Time) *Provider {
	p := New()
	p.mu.Lock()
	p.lastServer = s
	p.lastServerAt = at
	p.mu.Unlock()
	return p
}

func TestSnapshotWindows_ServerOverlay(t *testing.T) {
	now := time.Now()
	p := providerWithServer(&opencodeutil.ServerUsage{
		RollingPercent: 5,
		RollingReset:   3600,
		WeeklyPercent:  0,
		WeeklyReset:    259200,
		HasWeekly:      true,
		MonthlyPercent: 98,
		MonthlyReset:   1728000,
		HasMonthly:     true,
		UpdatedAt:      now,
	}, now)

	ws := p.SnapshotWindows()
	if len(ws) != 3 {
		t.Fatalf("SnapshotWindows len = %d, want 3", len(ws))
	}
	want := []struct {
		label string
		used  float64
		reset time.Duration
	}{
		{"5h", 5, 3600 * time.Second},
		{"weekly", 0, 259200 * time.Second},
		{"monthly", 98, 1728000 * time.Second},
	}
	for i, w := range ws {
		if w.WindowLabel != want[i].label {
			t.Errorf("window[%d] label = %q, want %q", i, w.WindowLabel, want[i].label)
		}
		if w.Used != want[i].used || w.Total != 100 {
			t.Errorf("window[%d] = %v/%v, want %v/100", i, w.Used, w.Total, want[i].used)
		}
		if w.Unit != types.UnitCount {
			t.Errorf("window[%d] unit = %v, want UnitCount", i, w.Unit)
		}
		if w.ResetIn != want[i].reset || !w.ResetAt.Equal(w.LastProbeAt.Add(w.ResetIn)) {
			t.Errorf("window[%d] reset = %v/%v, want %v", i, w.ResetIn, w.ResetAt, want[i].reset)
		}
		if w.Note != "server usage" {
			t.Errorf("window[%d] note = %q, want %q", i, w.Note, "server usage")
		}
	}
}

func TestSnapshotWindows_ServerExpired(t *testing.T) {
	now := time.Now()
	p := providerWithServer(&opencodeutil.ServerUsage{
		RollingPercent: 99, RollingReset: 3600, WeeklyPercent: 99, WeeklyReset: 3600,
		MonthlyPercent: 99, MonthlyReset: 3600, HasWeekly: true, HasMonthly: true,
	}, now.Add(-serverDataTTL-1*time.Second))

	ws := p.SnapshotWindows()
	if len(ws) != 3 {
		t.Fatalf("SnapshotWindows len = %d, want 3 (local fallback)", len(ws))
	}
	if ws[0].Total != plan5h || ws[1].Total != planWeekly || ws[2].Total != planMonth {
		t.Fatalf("expected local fallback totals 12/30/60, got %v/%v/%v", ws[0].Total, ws[1].Total, ws[2].Total)
	}
	if ws[0].Used != 0 || ws[1].Used != 0 || ws[2].Used != 0 {
		t.Fatalf("nil DB local costs should be zero, got %v/%v/%v", ws[0].Used, ws[1].Used, ws[2].Used)
	}
}

func TestSnapshotWindows_NoServer(t *testing.T) {
	p := New() // no lastServer, nil DB
	ws := p.SnapshotWindows()
	if len(ws) != 3 {
		t.Fatalf("SnapshotWindows len = %d, want 3", len(ws))
	}
	if ws[0].WindowLabel != "5h" || ws[1].WindowLabel != "weekly" || ws[2].WindowLabel != "monthly" {
		t.Fatalf("labels = %q/%q/%q, want 5h/weekly/monthly", ws[0].WindowLabel, ws[1].WindowLabel, ws[2].WindowLabel)
	}
}

func TestSnapshotWindows_PartialServer(t *testing.T) {
	now := time.Now()
	p := providerWithServer(&opencodeutil.ServerUsage{
		RollingPercent: 5, RollingReset: 3600, WeeklyPercent: 0, WeeklyReset: 259200, HasWeekly: true,
	}, now)

	ws := p.SnapshotWindows()
	if len(ws) != 2 {
		t.Fatalf("SnapshotWindows len = %d, want 2 (rolling+weekly only)", len(ws))
	}
	if ws[0].WindowLabel != "5h" || ws[1].WindowLabel != "weekly" {
		t.Fatalf("labels = %q/%q, want 5h/weekly", ws[0].WindowLabel, ws[1].WindowLabel)
	}
}

func TestServerPrimary_PicksHighestPressure(t *testing.T) {
	now := time.Now()
	p := New()
	primary := p.serverPrimary(&opencodeutil.ServerUsage{
		RollingPercent: 5, RollingReset: 3600, WeeklyPercent: 20, WeeklyReset: 259200,
		HasWeekly: true, MonthlyPercent: 98, MonthlyReset: 1728000, HasMonthly: true,
	}, now)
	if primary.WindowLabel != "monthly" || primary.Used != 98 {
		t.Fatalf("primary = %q %v, want monthly 98", primary.WindowLabel, primary.Used)
	}
	if primary.ResetIn != 1728000*time.Second {
		t.Fatalf("primary reset = %v, want 1728000s", primary.ResetIn)
	}
	if primary.Unit != types.UnitCount || primary.Total != 100 {
		t.Fatalf("primary unit/total = %v/%v, want UnitCount/100", primary.Unit, primary.Total)
	}
}

func TestServerPrimary_RollingOnly(t *testing.T) {
	now := time.Now()
	p := New()
	primary := p.serverPrimary(&opencodeutil.ServerUsage{
		RollingPercent: 42, RollingReset: 60,
	}, now)
	if primary.WindowLabel != "5h rolling" || primary.Used != 42 {
		t.Fatalf("primary = %q %v, want 5h rolling 42", primary.WindowLabel, primary.Used)
	}
}

func TestLocalPrimary_PrefersWeekly(t *testing.T) {
	now := time.Now()
	p := New()
	primary := p.localPrimary(0, 8.46, 0, now)
	if primary.WindowLabel != "weekly" || primary.Used != 8.46 || primary.Total != planWeekly {
		t.Fatalf("primary = %q %v/%v, want weekly 8.46/30", primary.WindowLabel, primary.Used, primary.Total)
	}
}

func TestLocalPrimary_FallsBackToMonthThen5h(t *testing.T) {
	now := time.Now()
	p := New()
	month := p.localPrimary(0, 0, 58.8, now)
	if month.WindowLabel != "monthly" || month.Total != planMonth {
		t.Fatalf("primary = %q, want monthly/60", month.WindowLabel)
	}
	five := p.localPrimary(2.5, 0, 0, now)
	if five.WindowLabel != "5h" || five.Total != plan5h {
		t.Fatalf("primary = %q, want 5h/12", five.WindowLabel)
	}
	none := p.localPrimary(0, 0, 0, now)
	if none.WindowLabel != "5h" || none.Note == "" {
		t.Fatalf("primary = %q note=%q, want 5h with fallback note", none.WindowLabel, none.Note)
	}
}

func TestWeekStart_SundayAndMidweek(t *testing.T) {
	loc := time.Local
	sun := time.Date(2026, 8, 2, 15, 30, 0, 0, loc) // a Sunday
	got := weekStart(sun)
	if !got.Equal(time.Date(2026, 8, 2, 0, 0, 0, 0, loc)) {
		t.Fatalf("weekStart(Sunday) = %v, want Sunday 00:00", got)
	}
	wed := time.Date(2026, 8, 5, 9, 0, 0, 0, loc) // a Wednesday
	got = weekStart(wed)
	if !got.Equal(time.Date(2026, 8, 2, 0, 0, 0, 0, loc)) {
		t.Fatalf("weekStart(Wednesday) = %v, want prior Sunday", got)
	}
}

func TestMonthStart(t *testing.T) {
	loc := time.Local
	mid := time.Date(2026, 8, 15, 12, 0, 0, 0, loc)
	got := monthStart(mid)
	if !got.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("monthStart = %v, want 2026-08-01 00:00", got)
	}
}

// withCookieState points the cookie cache at a temp XDG_STATE_HOME for the
// duration of one test.
func withCookieState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

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

// canceledCtx returns a context that is already canceled, so any network
// attempt inside a fetch fails immediately without dialing.
func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestFetchUsage_StaleCookieSetsServerErr(t *testing.T) {
	withCookieState(t)

	// A cached (but server-rejected) cookie is present.
	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	if err := cc.Write(&opencodeutil.CacheCookie{Source: "cli", Cookie: "stale-value", CachedAt: time.Now()}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	p := New()
	if _, err := p.FetchUsage(canceledCtx()); err != nil {
		t.Fatalf("FetchUsage error = %v", err)
	}

	p.mu.Lock()
	errNote := p.lastServerErr
	p.mu.Unlock()
	if !strings.Contains(errNote, "cookie expired") {
		t.Fatalf("lastServerErr = %q, want cookie-expired hint", errNote)
	}
	if p.haveCookie() {
		t.Fatal("stale cookie should be cleared after detection")
	}

	// The local fallback monthly bar surfaces the hint.
	ws := p.SnapshotWindows()
	if !strings.Contains(ws[2].Note, "cookie expired") {
		t.Fatalf("monthly note = %q, want cookie-expired hint", ws[2].Note)
	}
}

func TestFetchUsage_NoCookieShowsLoginHint(t *testing.T) {
	withCookieState(t)
	t.Setenv("HOME", t.TempDir()) // no browser cookie stores installed

	p := New()
	if _, err := p.FetchUsage(canceledCtx()); err != nil {
		t.Fatalf("FetchUsage error = %v", err)
	}

	p.mu.Lock()
	errNote := p.lastServerErr
	p.mu.Unlock()
	if !strings.Contains(errNote, "log in at opencode.ai") {
		t.Fatalf("lastServerErr = %q, want login hint", errNote)
	}

	// The local fallback monthly bar tells the user to log in.
	ws := p.SnapshotWindows()
	if !strings.Contains(ws[2].Note, "log in at opencode.ai") {
		t.Fatalf("monthly note = %q, want login hint", ws[2].Note)
	}
	if strings.Contains(ws[2].Note, "cookie expired") {
		t.Fatalf("monthly note = %q, want login hint, not stale-cookie hint", ws[2].Note)
	}
}

func TestAutoImport_WritesCookieWhenMissing(t *testing.T) {
	withCookieState(t)

	// Fake an installed google-chrome store holding a plaintext auth cookie.
	home := t.TempDir()
	t.Setenv("HOME", home)
	chrome := filepath.Join(home, ".config", "google-chrome", "Default")
	if err := os.MkdirAll(chrome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedBrowserCookie(t, filepath.Join(chrome, "Cookies"), "imported-value")

	p := New()
	// The canceled context makes the _server call fail fast; the cache
	// write already happened before it, which is what we assert on.
	_, _ = p.fetchServerUsage(canceledCtx())

	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	cached, err := cc.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cached == nil || cached.Cookie != "imported-value" || cached.Source != "browser-import" {
		t.Fatalf("cached = %#v, want Cookie=imported-value Source=browser-import", cached)
	}
}

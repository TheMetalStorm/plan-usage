package opencodeutil

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// seedGoDB creates a temp opencode.db with a session table whose rows mix
// OpenCode Go sessions with other providers' sessions (plus one NULL-model
// row), then reopens it through OpenPath (read-only) for the queries under
// test.
//
//	dayB (-2d): opencode-go $4, cline-pass $3
//	dayA (-1d): opencode-go $10, opencode $5, model NULL $2
func seedGoDB(t *testing.T) (*OpenCodeDB, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")

	db, err := sql.Open("sqlite", path+"?mode=rwc")
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE session (
		time_created INTEGER,
		cost REAL,
		model TEXT,
		tokens_input INTEGER,
		tokens_output INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	dayA := time.Now().AddDate(0, 0, -1)
	dayB := time.Now().AddDate(0, 0, -2)
	dayBTS := dayB.UnixMilli()

	rows := []struct {
		ts   int64
		cost float64
		model any
	}{
		{dayBTS, 4, `{"id":"glm-5.2","providerID":"opencode-go","variant":"default"}`},
		{dayBTS, 3, `{"id":"kimi-k3","providerID":"cline-pass","variant":"default"}`},
		{dayA.UnixMilli(), 10, `{"id":"deepseek-v4-flash-free","providerID":"opencode-go","variant":"default"}`},
		{dayA.UnixMilli(), 5, `{"id":"deepseek-v4-flash-free","providerID":"opencode","variant":"default"}`},
		{dayA.UnixMilli(), 2, nil},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO session (time_created, cost, model, tokens_input, tokens_output) VALUES (?, ?, ?, 0, 0)`, r.ts, r.cost, r.model); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	opened, err := OpenPath(path)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	t.Cleanup(func() { opened.Close() })
	return opened, dayBTS
}

func TestTotalCostSince_ProviderFiltered(t *testing.T) {
	odb, _ := seedGoDB(t)

	total, err := odb.TotalCostSince(0)
	if err != nil {
		t.Fatalf("TotalCostSince: %v", err)
	}
	if total != 14 {
		t.Fatalf("TotalCostSince = %v, want 14 (only opencode-go rows)", total)
	}
}

func TestDailyCostHistory_ProviderFiltered(t *testing.T) {
	odb, _ := seedGoDB(t)

	history, err := odb.DailyCostHistory(14)
	if err != nil {
		t.Fatalf("DailyCostHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("DailyCostHistory len = %d, want 2 day buckets", len(history))
	}
	// Results are ordered day DESC: dayA first, then dayB.
	first, second := history[0], history[1]
	if first.Cost != 10 || first.Sessions != 1 {
		t.Fatalf("dayA bucket = cost %v sessions %d, want 10/1", first.Cost, first.Sessions)
	}
	if second.Cost != 4 || second.Sessions != 1 {
		t.Fatalf("dayB bucket = cost %v sessions %d, want 4/1", second.Cost, second.Sessions)
	}
}

func TestCostInWindow_ProviderFiltered(t *testing.T) {
	odb, _ := seedGoDB(t)

	dayB := time.Now().AddDate(0, 0, -2)
	start := dayB.Add(-time.Hour).UnixMilli()
	end := time.Now().Add(time.Hour).UnixMilli()

	total, err := odb.CostInWindow(start, end)
	if err != nil {
		t.Fatalf("CostInWindow: %v", err)
	}
	if total != 14 {
		t.Fatalf("CostInWindow = %v, want 14 (only opencode-go rows)", total)
	}
}

func TestEarliestSessionTime_ProviderFiltered(t *testing.T) {
	odb, _ := seedGoDB(t)

	odb, dayBTS := seedGoDB(t)

	ts, err := odb.EarliestSessionTime()
	if err != nil {
		t.Fatalf("EarliestSessionTime: %v", err)
	}
	if ts != dayBTS {
		t.Fatalf("EarliestSessionTime = %d, want %d (dayB opencode-go row)", ts, dayBTS)
	}
}

package opencodeutil

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DailyCost holds one day's cost/usage summary from the local opencode.db.
type DailyCost struct {
	Date     string  // "2006-01-02"
	Cost     float64 // total cost for the day
	Sessions int     // number of sessions
	TokensIn int64   // input tokens
	TokensOut int64  // output tokens
}

// OpenCodeDB wraps a connection to the local opencode.sqlite database.
type OpenCodeDB struct {
	db     *sql.DB
	dbPath string
}

// OpenDB opens the opencode.db at the standard XDG data path.
// Returns nil, nil if the file does not exist (caller should check).
func OpenDB() (*OpenCodeDB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	path := filepath.Join(data, "opencode", "opencode.db")

	// Check if DB exists.
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // not an error, just no DB
		}
		return nil, fmt.Errorf("opencode db stat: %w", err)
	}

	return OpenPath(path)
}

// OpenPath opens opencode.db at a specific path.
func OpenPath(path string) (*OpenCodeDB, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro&_journal_mode=WAL&_query_only=true")
	if err != nil {
		return nil, fmt.Errorf("opencode db open: %w", err)
	}
	// Verify connection.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opencode db ping: %w", err)
	}
	return &OpenCodeDB{db: db, dbPath: path}, nil
}

// Close closes the database.
func (o *OpenCodeDB) Close() error {
	if o.db != nil {
		return o.db.Close()
	}
	return nil
}

// DBPath returns the path to the database file.
func (o *OpenCodeDB) DBPath() string { return o.dbPath }

// DailyCostHistory returns per-day cost/request buckets for the last N days.
// It queries the session table, grouping by local calendar day of time_created.
func (o *OpenCodeDB) DailyCostHistory(days int) ([]DailyCost, error) {
	if o.db == nil {
		return nil, nil
	}
	since := time.Now().AddDate(0, 0, -days).UnixMilli()

	query := `
		SELECT
			date(time_created / 1000, 'unixepoch') as day,
			SUM(cost) as total_cost,
			COUNT(*) as sessions,
			SUM(tokens_input) as tok_in,
			SUM(tokens_output) as tok_out
		FROM session
		WHERE cost IS NOT NULL
		  AND time_created > ?
		  AND time_created > 0
		GROUP BY day
		ORDER BY day DESC
	`

	rows, err := o.db.Query(query, since)
	if err != nil {
		return nil, fmt.Errorf("daily cost query: %w", err)
	}
	defer rows.Close()

	var results []DailyCost
	for rows.Next() {
		var dc DailyCost
		if err := rows.Scan(&dc.Date, &dc.Cost, &dc.Sessions, &dc.TokensIn, &dc.TokensOut); err != nil {
			return nil, fmt.Errorf("daily cost scan: %w", err)
		}
		results = append(results, dc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("daily cost rows: %w", err)
	}
	if results == nil {
		results = []DailyCost{} // empty, not nil
	}
	return results, nil
}

// TotalCostSince returns the sum of costs from the local DB since a given
// Unix millisecond timestamp.
func (o *OpenCodeDB) TotalCostSince(sinceMs int64) (float64, error) {
	if o.db == nil {
		return 0, nil
	}
	query := `SELECT COALESCE(SUM(cost), 0) FROM session WHERE time_created > ? AND time_created > 0`
	var total float64
	if err := o.db.QueryRow(query, sinceMs).Scan(&total); err != nil {
		return 0, fmt.Errorf("total cost query: %w", err)
	}
	return total, nil
}

// EarliestSessionTime returns the Unix millisecond timestamp of the earliest
// session with a non-nil cost (used for monthly window estimation).
func (o *OpenCodeDB) EarliestSessionTime() (int64, error) {
	if o.db == nil {
		return 0, nil
	}
	query := `SELECT COALESCE(MIN(time_created), 0) FROM session WHERE cost IS NOT NULL AND cost > 0 AND time_created > 0`
	var ts int64
	if err := o.db.QueryRow(query).Scan(&ts); err != nil {
		return 0, fmt.Errorf("earliest session: %w", err)
	}
	return ts, nil
}

// CostInWindow returns total cost for sessions that fall within the time
// range [startMs, endMs).
func (o *OpenCodeDB) CostInWindow(startMs, endMs int64) (float64, error) {
	if o.db == nil {
		return 0, nil
	}
	query := `SELECT COALESCE(SUM(cost), 0) FROM session WHERE time_created >= ? AND time_created < ? AND time_created > 0`
	var total float64
	if err := o.db.QueryRow(query, startMs, endMs).Scan(&total); err != nil {
		return 0, fmt.Errorf("cost in window: %w", err)
	}
	return total, nil
}

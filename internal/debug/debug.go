// Package debug is a tiny structured log ring buffer used by the TUI.
package debug

import (
	"fmt"
	"sync"
	"time"
)

// Level represents log severity.
type Level int

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
	LevelOk
)

// Entry is a single log entry.
type Entry struct {
	Time    time.Time
	Provider string
	Level   Level
	Msg     string
}

// Log is a thread-safe ring buffer of recent events.
type Log struct {
	mu      sync.RWMutex
	cap     int
	entries []Entry
}

// New returns a Log with the given capacity (capped at e.g. 256).
func New(capacity int) *Log {
	if capacity <= 0 {
		capacity = 256
	}
	return &Log{cap: capacity, entries: make([]Entry, 0, capacity)}
}

// Add records an entry.
func (l *Log) Add(provider string, lvl Level, fmtStr string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Entry{
		Time:     time.Now(),
		Provider: provider,
		Level:    lvl,
		Msg:      fmt.Sprintf(fmtStr, args...),
	}
	if len(l.entries) >= l.cap {
		// shift left
		l.entries = l.entries[1:]
	}
	l.entries = append(l.entries, e)
}

// Provider adds an entry for a specific provider.
func (l *Log) Provider(name, fmtStr string, args ...any) {
	l.Add(name, LevelInfo, fmtStr, args...)
}

// Warn, Error, Ok are convenience methods.
func (l *Log) Warn(provider, fmtStr string, args ...any) { l.Add(provider, LevelWarn, fmtStr, args...) }
func (l *Log) Error(provider, fmtStr string, args ...any) { l.Add(provider, LevelError, fmtStr, args...) }
func (l *Log) Ok(provider, fmtStr string, args ...any)    { l.Add(provider, LevelOk, fmtStr, args...) }

// Snapshot returns a copy of the buffer.
func (l *Log) Snapshot() []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

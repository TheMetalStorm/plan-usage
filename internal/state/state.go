// Package state stores the most recent usage snapshots on disk so that
// the TUI render and tray popup do not block on I/O.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/TheMetalStorm/plan-usage/internal/types"
)

// Store keeps a persistent snapshot of every provider's most recent state.
type Store struct {
	dir  string
	path string
	mu   sync.RWMutex
	data types.Aggregate
}

// New creates (or opens) a Store rooted at dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("state: mkdir: %w", err)
	}
	s := &Store{
		dir:  dir,
		path: filepath.Join(dir, "snapshot.json"),
	}
	_ = s.Load() // optional; missing file is fine
	return s, nil
}

// Path returns the on-disk snapshot file.
func (s *Store) Path() string { return s.path }

// Load reads the snapshot file into memory. A missing file is not fatal.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(raw, &s.data)
}

// Replace atomically writes the new aggregate to disk.
func (s *Store) Replace(agg types.Aggregate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = agg
	tmp, err := os.CreateTemp(s.dir, "snapshot-*.json.tmp")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(agg); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// All returns a defensive copy of every cached snapshot.
func (s *Store) All() types.Aggregate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := types.Aggregate{
		GeneratedAt: s.data.GeneratedAt,
		Providers:   make(map[string]types.Snapshot, len(s.data.Providers)),
	}
	for k, v := range s.data.Providers {
		out.Providers[k] = v
	}
	return out
}

// Get returns the snapshot for one provider.
func (s *Store) Get(name string) (types.Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data.Providers[name]
	return v, ok
}

// Age returns the time since the last refresh - or 0 if empty.
func (s *Store) Age() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data.GeneratedAt.IsZero() {
		return 0
	}
	return time.Since(s.data.GeneratedAt)
}

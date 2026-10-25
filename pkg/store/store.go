// Package store is the single persistence seam. Every persisted byte goes
// through it, and the --private guard lives INSIDE the write path rather than
// at call sites — a guard at call sites is how a --private flag ends up
// protecting nothing.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"chainsmoke/pkg/core"
)

const resultsFile = "results.json"

// New picks where results live. --private always wins over --remember: a user
// who asks for memory-only must get it regardless of what else is configured.
func New(remember, private bool) core.Store {
	switch {
	case private:
		return &Store{}
	case remember:
		return &Store{dir: core.StateDir()}
	default:
		return &Store{dir: core.RuntimeDir()}
	}
}

// Store is the one backend. An empty dir means memory-only, which is not a
// special case bolted on but the whole of --private: with nowhere to write,
// there is no write path to forget to guard.
type Store struct {
	dir     string
	results []core.Track
}

var _ core.Store = (*Store)(nil)

// SaveResults caches the numbered set. This is the SINGLE write path, and the
// private check is its first statement — the shape that cannot be bypassed.
func (s *Store) SaveResults(ts []core.Track) error {
	if s.dir == "" {
		s.results = ts
		return nil
	}

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("store dir %s: %w", s.dir, err)
	}
	blob, err := json.Marshal(ts)
	if err != nil {
		return fmt.Errorf("encode results: %w", err)
	}

	tmp, err := os.CreateTemp(s.dir, resultsFile+".*")
	if err != nil {
		return fmt.Errorf("store temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return fmt.Errorf("write results: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod results: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close results: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, resultsFile)); err != nil {
		return fmt.Errorf("commit results: %w", err)
	}
	return nil
}

// Results returns the whole last saved set in one read.
func (s *Store) Results() ([]core.Track, error) { return s.load() }

// Result returns the 1-indexed entry from the last saved set.
func (s *Store) Result(n int) (core.Track, error) {
	results, err := s.load()
	if err != nil {
		return core.Track{}, err
	}
	if n < 1 || n > len(results) {
		if len(results) == 0 {
			return core.Track{}, fmt.Errorf("no cached result #%d — run `chainsmoke search <term>` first", n)
		}
		return core.Track{}, fmt.Errorf("no cached result #%d (have 1-%d)", n, len(results))
	}
	return results[n-1], nil
}

func (s *Store) load() ([]core.Track, error) {
	if s.dir == "" {
		return s.results, nil
	}
	blob, err := os.ReadFile(filepath.Join(s.dir, resultsFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read results: %w", err)
	}
	var ts []core.Track
	if err := json.Unmarshal(blob, &ts); err != nil {
		return nil, fmt.Errorf("cached results are unreadable (%w) — run `chainsmoke search <term>` again", err)
	}
	return ts, nil
}

// Purge wipes BOTH stores, not whichever one this process happens to be using.
func (s *Store) Purge() error {
	s.results = nil

	var failed error
	for _, dir := range []string{core.RuntimeDir(), core.StateDir()} {
		if err := os.RemoveAll(dir); err != nil && failed == nil {
			failed = fmt.Errorf("purge %s: %w", dir, err)
		}
	}
	return failed
}

// Describe reports which backend is in use and how many results are cached, for
// `audit`.
func (s *Store) Describe() (mode string, results int) {
	switch {
	case s.dir == "":
		mode = "memory (--private: nothing is written)"
	case s.dir == core.StateDir():
		mode = "disk (--remember): " + s.dir
	default:
		mode = "runtime (ephemeral): " + s.dir
	}
	ts, err := s.load()
	if err != nil {
		return mode, 0
	}
	return mode, len(ts)
}

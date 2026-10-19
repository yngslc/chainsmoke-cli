// Package store is the single persistence seam. Every persisted byte goes
// through it (invariant #1), and the --private guard lives INSIDE the write
// path rather than at call sites — the bash prototype's T1.8 bug (a broken
// CS_PRIVATE guard in results_write) is the cautionary tale.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"chainsmoke/pkg/core"
)

// resultsFile is the cached numbered result set. JSON, not the prototype's TSV
// (script.sh:39): a SoundCloud title can contain a tab or a newline, and TSV
// answers that with escaping rules nobody maintains. The bash format was a
// constraint of bash.
const resultsFile = "results.json"

// New picks where results live. --private always wins over --remember: a user
// who asks for memory-only must get it regardless of what else is configured
// (invariant #4).
//
// The three modes differ only in the directory — including "nowhere at all",
// which is what makes the private guard structural rather than a check someone
// has to remember to write (T4.3).
func New(remember, private bool) core.Store {
	switch {
	case private:
		return &Store{} // dir "" — see SaveResults
	case remember:
		return &Store{dir: core.StateDir()} // T4.2
	default:
		return &Store{dir: core.RuntimeDir()} // T4.1
	}
}

// Store is the one backend. An empty dir means memory-only, which is not a
// special case bolted on but the whole of --private: with nowhere to write,
// there is no write path to forget to guard.
type Store struct {
	dir     string
	results []core.Track // private mode's entire world; dies with the process
}

var _ core.Store = (*Store)(nil)

// SaveResults caches the numbered set. This is the SINGLE write path, and the
// private check is its first statement — the shape T4.3 asks for, and the one
// the bash prototype only appeared to have. Its guard,
//
//	results_write() { : "${CS_PRIVATE:+return 0}"; cat > "$RESULTS"; }
//
// expands to a no-op argument of `:` and then writes anyway. `--private` was a
// flag that printed nothing and protected nothing (T1.8).
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

	// Write-then-rename. A `play N` running while a `search` writes must see
	// either the old set or the new one, never half a file — and rename is the
	// only way to promise that without a lock. The temp file is created in the
	// SAME directory so the rename stays on one filesystem.
	tmp, err := os.CreateTemp(s.dir, resultsFile+".*")
	if err != nil {
		return fmt.Errorf("store temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds

	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return fmt.Errorf("write results: %w", err)
	}
	// 0600 explicitly: CreateTemp's own 0600 is not contractual, and this file
	// holds every permalink the user looked at. On Windows the mode is
	// approximate — the directory ACL is what protects it there.
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

// Results returns the whole last saved set in one read. The play session
// snapshots its list with it: a loop of Result(n) re-reads and re-parses the
// file every call, measured at 5.7s for 1,700 likes against 3ms for this.
func (s *Store) Results() ([]core.Track, error) { return s.load() }

// Result returns the 1-indexed entry from the last saved set. In private mode
// it reads the in-process slice, which on a fresh `chainsmoke play N` is empty
// — that is the point of the flag, not a bug to work around.
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

// load reads the cached set. A missing file is not an error: it is the ordinary
// state of a machine where nobody has searched yet, and Result turns it into
// the message that says so.
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
		// A corrupt cache is recoverable by searching again, so say that
		// rather than making the user find the file and delete it.
		return nil, fmt.Errorf("cached results are unreadable (%w) — run `chainsmoke search <term>` again", err)
	}
	return ts, nil
}

// Purge wipes BOTH stores, not whichever one this process happens to be using
// (T4.4). A user purging after a --remember session, from a shell without the
// flag, must not be told "nothing to do" while their history sits on disk.
//
// The whole directory goes, matching the prototype's `rm -rf "$RUNDIR"`
// (script.sh:264) — a purge that left an empty chainsmoke directory behind
// would still tell anyone looking that the tool had been used. Ordering
// against the player is the caller's job and lives in core.App.Purge (T1.13).
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

// Describe reports which backend is in use and how many results are cached,
// for `audit` (TASKS.md T7.1). It is a separate optional interface rather than
// part of core.Store because it answers a reporting question, not a
// persistence one — a Store that could not describe itself would still be a
// perfectly good Store.
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

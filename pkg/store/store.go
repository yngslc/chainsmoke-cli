// Package store is the single persistence seam. Every persisted byte goes
// through it (invariant #1), and the --private guard lives INSIDE the write
// path rather than at call sites — the bash prototype's T1.8 bug (a broken
// CS_PRIVATE guard in results_write) is the cautionary tale.
package store

import (
	"fmt"

	"chainsmoke/pkg/core"
)

// New picks the backend. --private always wins over --remember: a user who
// asks for memory-only must get it regardless of what else is configured
// (invariant #4).
func New(remember, private bool) core.Store {
	if remember && !private {
		return &diskStore{}
	}
	return &memStore{}
}

// memStore is the ephemeral default: results live for exactly one process.
//
// KNOWN GAP (T4.1) — this is why `chainsmoke search` followed by
// `chainsmoke play 1` cannot work today. Under decision T0.1(a) the two verbs
// are separate short-lived processes, so an in-memory slice is discarded
// between them and Result always misses. The fix is the runtime store:
// $XDG_RUNTIME_DIR/chainsmoke (tmpfs, 0700, see core.EnsureRuntimeDir), which
// is ephemeral in the sense that matters (dies with the login session) while
// still surviving an exit. Not a Phase-1 change.
type memStore struct{ results []core.Track }

var _ core.Store = (*memStore)(nil)

func (m *memStore) SaveResults(t []core.Track) error { m.results = t; return nil }

func (m *memStore) Result(n int) (core.Track, error) {
	if n < 1 || n > len(m.results) {
		if len(m.results) == 0 {
			return core.Track{}, fmt.Errorf("no cached result #%d — run `chainsmoke search <term>` first", n)
		}
		return core.Track{}, fmt.Errorf("no cached result #%d (have 1-%d)", n, len(m.results))
	}
	return m.results[n-1], nil
}

func (m *memStore) Purge() error { m.results = nil; return nil }

// diskStore persists under $XDG_STATE_HOME/chainsmoke (TASKS.md T4.2).
// TODO(impl): it currently inherits memStore wholesale, so --remember selects a
// backend that remembers nothing. Honest stub, not a working feature.
type diskStore struct{ memStore }

var _ core.Store = (*diskStore)(nil)

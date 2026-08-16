package store

import (
	"chainsmoke/pkg/core"
	"fmt"
)

// memStore = ephemeral default; diskStore (XDG_STATE_HOME) only when --remember.
type memStore struct{ results []core.Track }

func (m *memStore) SaveResults(t []core.Track) error { m.results = t; return nil }
func (m *memStore) Result(n int) (core.Track, error) {
	if n < 1 || n > len(m.results) {
		return core.Track{}, fmt.Errorf("no cached result #%d", n)
	}
	return m.results[n-1], nil
}
func (m *memStore) Purge() error { m.results = nil; return nil }

type diskStore struct{ memStore } // TODO(impl): persist under $XDG_STATE_HOME/chainsmoke

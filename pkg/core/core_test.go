package core

import (
	"errors"
	"strings"
	"testing"
)

// log records the order subsystems were called in, which for purge is the
// whole contract (T4.4 / bash lesson T1.13).
type log struct{ calls []string }

func (l *log) note(s string) { l.calls = append(l.calls, s) }

type fakePlayer struct {
	log     *log
	downErr error
}

func (p *fakePlayer) Ensure() error               { return nil }
func (p *fakePlayer) Enqueue(string) error        { return nil }
func (p *fakePlayer) Command(...any) (any, error) { return nil, nil }
func (p *fakePlayer) NowPlaying() (string, error) { return "", nil }
func (p *fakePlayer) Down() error                 { p.log.note("player.Down"); return p.downErr }

type fakeStore struct {
	log      *log
	purgeErr error
}

func (s *fakeStore) SaveResults([]Track) error { return nil }
func (s *fakeStore) Result(int) (Track, error) { return Track{}, nil }
func (s *fakeStore) Purge() error              { s.log.note("store.Purge"); return s.purgeErr }

// TestPurgeStopsThePlayerFirst: the mpv IPC socket lives inside the runtime
// directory the store is about to delete. Reversing these two leaves an
// unreachable daemon on unix and fails outright on Windows, where an open file
// cannot be removed.
func TestPurgeStopsThePlayerFirst(t *testing.T) {
	l := &log{}
	app := &App{Play: &fakePlayer{log: l}, Store: &fakeStore{log: l}}

	if err := app.Purge(); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if got := strings.Join(l.calls, ","); got != "player.Down,store.Purge" {
		t.Errorf("call order = %s, want player.Down,store.Purge", got)
	}
}

// TestPurgeContinuesWhenThePlayerIsAlreadyGone: teardown failing must not
// abandon the wipe. A purge that stops halfway leaves exactly the state the
// user asked to destroy.
func TestPurgeContinuesWhenThePlayerIsAlreadyGone(t *testing.T) {
	l := &log{}
	app := &App{
		Play:  &fakePlayer{log: l, downErr: errors.New("no daemon")},
		Store: &fakeStore{log: l},
	}

	if err := app.Purge(); err != nil {
		t.Fatalf("Purge = %v, want nil: a dead player is not a failed purge", err)
	}
	if got := strings.Join(l.calls, ","); got != "player.Down,store.Purge" {
		t.Errorf("call order = %s, want the store purged anyway", got)
	}
}

// TestPurgeReportsStoreFailure: the one failure that DOES matter is the wipe
// itself — silently returning nil would tell the user their history is gone
// when it is not.
func TestPurgeReportsStoreFailure(t *testing.T) {
	l := &log{}
	app := &App{
		Play:  &fakePlayer{log: l},
		Store: &fakeStore{log: l, purgeErr: errors.New("permission denied")},
	}

	if err := app.Purge(); err == nil {
		t.Fatal("Purge = nil, want the store's error surfaced")
	}
}

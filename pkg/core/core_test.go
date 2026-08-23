package core

import (
	"errors"
	"io"
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

func (p *fakePlayer) Ensure() error                { return nil }
func (p *fakePlayer) Enqueue(string, string) error { return nil }
func (p *fakePlayer) Command(...any) (any, error)  { return nil, nil }
func (p *fakePlayer) NowPlaying() (string, error)  { return "", nil }
func (p *fakePlayer) Down() error                  { p.log.note("player.Down"); return p.downErr }

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

// fakeResolver hands back a stream URL per permalink; an empty entry means
// "this one fails to resolve".
type fakeResolver struct{ streams map[string]string }

func (r fakeResolver) Meta(string) ([]Track, error)        { return nil, nil }
func (r fakeResolver) Search(string, int) ([]Track, error) { return nil, nil }
func (r fakeResolver) Stream(permalink string) (string, error) {
	if u := r.streams[permalink]; u != "" {
		return u, nil
	}
	return "", errors.New("could not resolve")
}

// queueingPlayer records what actually reached mpv, and refuses any URL listed
// as dead — the shape ErrDeadStream arrives in from a failed preflight.
type queueingPlayer struct {
	fakePlayer
	dead   map[string]bool
	queued []string
}

func (p *queueingPlayer) Enqueue(url, _ string) error {
	if p.dead[url] {
		return errors.New("dead stream")
	}
	p.queued = append(p.queued, url)
	return nil
}

func tracks(names ...string) []Track {
	var ts []Track
	for _, n := range names {
		ts = append(ts, Track{Title: n, Permalink: "https://soundcloud.com/a/" + n})
	}
	return ts
}

// TestEnqueueAheadSkipsNotAborts is T1.14 applied to Phase 5: a track that will
// not resolve, and a track whose stream is already dead, must each cost only
// themselves. The queue is what the user asked for; one bad entry is not a
// reason to hand it back empty.
func TestEnqueueAheadSkipsNotAborts(t *testing.T) {
	res := fakeResolver{streams: map[string]string{
		"https://soundcloud.com/a/two":   "https://cdn/two",
		"https://soundcloud.com/a/three": "https://cdn/three",
	}} // "one" resolves to nothing
	play := &queueingPlayer{fakePlayer: fakePlayer{log: &log{}}}
	app := &App{Res: res, Play: play, Out: io.Discard}

	if err := app.EnqueueAhead(tracks("one", "two", "three")); err != nil {
		t.Fatalf("EnqueueAhead() = %v, want nil — two tracks were queueable", err)
	}
	if len(play.queued) != 2 {
		t.Fatalf("queued %v, want the two that resolved", play.queued)
	}
}

// TestEnqueueAheadStopsAtPreloadAhead: resolving the whole set would cost a Tor
// round-trip per track and hand mpv URLs that expire before playback reaches
// them (T5.4).
func TestEnqueueAheadStopsAtPreloadAhead(t *testing.T) {
	streams := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		streams["https://soundcloud.com/a/"+n] = "https://cdn/" + n
	}
	play := &queueingPlayer{fakePlayer: fakePlayer{log: &log{}}}
	app := &App{Res: fakeResolver{streams: streams}, Play: play, Out: io.Discard}

	if err := app.EnqueueAhead(tracks("a", "b", "c", "d", "e")); err != nil {
		t.Fatalf("EnqueueAhead() = %v", err)
	}
	if len(play.queued) != preloadAhead {
		t.Errorf("queued %d tracks, want %d", len(play.queued), preloadAhead)
	}
}

// TestEnqueueAheadFailsWhenNothingQueues: skip-not-abort is not "never fail".
// If every track is dead the user must hear about it, not get silence.
func TestEnqueueAheadFailsWhenNothingQueues(t *testing.T) {
	play := &queueingPlayer{fakePlayer: fakePlayer{log: &log{}}, dead: map[string]bool{"https://cdn/x": true}}
	app := &App{
		Res:  fakeResolver{streams: map[string]string{"https://soundcloud.com/a/x": "https://cdn/x"}},
		Play: play, Out: io.Discard,
	}

	if err := app.EnqueueAhead(tracks("x")); err == nil {
		t.Fatal("EnqueueAhead() = nil with every track dead, want an error")
	}
}

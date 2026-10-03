package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"chainsmoke/pkg/core"
)

// playlistPlayer is a fake mpv playlist: append-play starts the first entry,
// playlist-next/-prev move along it and fail past either end the way mpv does
// ("error running command"), and stop clears it.
type playlistPlayer struct {
	recordingPlayer
	list []string // titles, in playlist order
	pos  int      // -1 when idle
	sent []string // every command, as fmt %v renders it
}

func newPlaylistPlayer() *playlistPlayer { return &playlistPlayer{pos: -1} }

func (p *playlistPlayer) Enqueue(_, title string) error {
	p.list = append(p.list, title)
	if p.pos < 0 {
		p.pos = len(p.list) - 1
	}
	return nil
}

func (p *playlistPlayer) Command(args ...any) (any, error) {
	p.sent = append(p.sent, fmt.Sprintf("%v", args))
	if len(args) == 2 && args[0] == "playlist-remove" {
		i := args[1].(int)
		p.list = append(p.list[:i], p.list[i+1:]...)
		return nil, nil
	}
	switch fmt.Sprintf("%v", args) {
	case "[stop]":
		p.list, p.pos = nil, -1
	case "[playlist-next]":
		if p.pos+1 >= len(p.list) {
			return nil, errors.New("mpv: playlist-next: error running command")
		}
		p.pos++
	case "[playlist-prev]":
		if p.pos <= 0 {
			return nil, errors.New("mpv: playlist-prev: error running command")
		}
		p.pos--
	case "[get_property playlist-pos]":
		return float64(p.pos), nil
	}
	return nil, nil
}

func (p *playlistPlayer) playing() string {
	if p.pos < 0 {
		return "(idle)"
	}
	return p.list[p.pos]
}

// listStore is a cached result set of n tracks, t1..tn.
type listStore struct{ n int }

func (s listStore) SaveResults([]core.Track) error { return nil }
func (s listStore) Purge() error                   { return nil }
func (s listStore) Result(i int) (core.Track, error) {
	if i < 1 || i > s.n {
		return core.Track{}, fmt.Errorf("no cached result #%d (have 1-%d)", i, s.n)
	}
	return core.Track{Title: fmt.Sprintf("t%d", i), Permalink: fmt.Sprintf("https://soundcloud.com/a/t%d", i)}, nil
}

// listResolver counts Meta and Stream separately: list playback must resolve
// each track exactly once, with no Meta round-trip for a single track.
type listResolver struct{ metas, streams int }

func (r *listResolver) Meta(string) ([]core.Track, error) {
	r.metas++
	return []core.Track{{Title: "t", Permalink: "p"}}, nil
}
func (r *listResolver) Stream(string) (string, error)            { r.streams++; return "u", nil }
func (r *listResolver) Search(string, int) ([]core.Track, error) { return nil, nil }

func listApp(n int) (*core.App, *playlistPlayer, *listResolver) {
	p, r := newPlaylistPlayer(), &listResolver{}
	return &core.App{Net: &recordingNet{}, Res: r, Play: p, Store: listStore{n}, Out: io.Discard}, p, r
}

// TestListPlaybackWalksTheResults: playing result N from likes (or a search, or
// a playlist) used to queue N alone, so `next` failed with "playlist-next:
// error running command" (dev's report, 2026-10-03). The cached results are a
// playlist: next/prev walk them, one track is always preloaded so a skip is
// instant, and each track is resolved exactly once.
func TestListPlaybackWalksTheResults(t *testing.T) {
	app, p, r := listApp(5)
	q := &listQueue{app: app}

	if err := q.start(2); err != nil {
		t.Fatalf("start(2) = %v", err)
	}
	for _, want := range []string{"t2", "t3", "t4", "t5"} {
		if got := p.playing(); got != want {
			t.Fatalf("playing %s, want %s (playlist %v)", got, want, p.list)
		}
		if want != "t5" {
			if n := len(p.list) - 1 - p.pos; n != 1 {
				t.Errorf("at %s, %d tracks preloaded, want 1 — next would wait on yt-dlp", want, n)
			}
			if err := q.next(); err != nil {
				t.Fatalf("next() after %s = %v", want, err)
			}
		}
	}
	if err := q.next(); err == nil || !strings.Contains(err.Error(), "end of the list") {
		t.Errorf("next() past the last result = %v, want end of the list", err)
	}
	if err := q.prev(); err != nil || p.playing() != "t4" {
		t.Errorf("prev() = %v, playing %s, want t4", err, p.playing())
	}
	if r.metas != 0 || r.streams != 4 {
		t.Errorf("%d Meta / %d Stream calls, want 0 / 4 (t2..t5, each once)", r.metas, r.streams)
	}
}

// TestListPrevBeforeTheFirstQueued: `play 3` then prev must reach result 2,
// which was never in mpv's playlist.
func TestListPrevBeforeTheFirstQueued(t *testing.T) {
	app, p, _ := listApp(5)
	q := &listQueue{app: app}
	_ = q.start(3)
	if err := q.prev(); err != nil || p.playing() != "t2" {
		t.Fatalf("prev() = %v, playing %s, want t2", err, p.playing())
	}
	if err := q.prev(); err != nil || p.playing() != "t1" {
		t.Fatalf("prev() = %v, playing %s, want t1", err, p.playing())
	}
	if err := q.prev(); err == nil || !strings.Contains(err.Error(), "start of the list") {
		t.Errorf("prev() before result 1 = %v, want start of the list", err)
	}
}

// TestListPreloadFollowsNaturalAdvance: when a track ends, mpv moves to the
// preloaded one by itself. The session's next tick must queue the one after,
// or playback stops two tracks in.
func TestListPreloadFollowsNaturalAdvance(t *testing.T) {
	app, p, _ := listApp(5)
	q := &listQueue{app: app}
	_ = q.start(1)
	p.pos = 1 // t1 ended; mpv is on t2
	q.preload()
	if len(p.list) != 3 || p.list[2] != "t3" {
		t.Errorf("after a natural advance, playlist = %v, want t3 preloaded", p.list)
	}
}

// TestListShufflePlaysTheRestOnce: shuffle (dev's request, 2026-10-03) plays the
// rest of the list in random order, every track once, starting with the very
// next one — the track already preloaded in order is swapped out.
func TestListShufflePlaysTheRestOnce(t *testing.T) {
	app, p, _ := listApp(20)
	q := &listQueue{app: app}
	_ = q.start(1)
	if err := q.setShuffle(true); err != nil {
		t.Fatalf("setShuffle(true) = %v", err)
	}
	seen := map[string]bool{"t1": true}
	inOrder := true
	for k := 2; k <= 20; k++ {
		if err := q.next(); err != nil {
			t.Fatalf("next() = %v after %v", err, seen)
		}
		if seen[p.playing()] {
			t.Fatalf("shuffle played %s twice: %v", p.playing(), p.list)
		}
		seen[p.playing()] = true
		inOrder = inOrder && p.playing() == fmt.Sprintf("t%d", k)
	}
	if inOrder { // 1 in 19! for a real shuffle
		t.Error("shuffle played t2..t20 in list order")
	}
	if err := q.next(); err == nil || !strings.Contains(err.Error(), "end of the list") {
		t.Errorf("next() with every track played = %v, want end of the list", err)
	}
}

// TestListRepeatAllWrapsAround: with repeat all, the last track is followed by
// the first, not by "end of the list".
func TestListRepeatAllWrapsAround(t *testing.T) {
	app, p, _ := listApp(3)
	q := &listQueue{app: app}
	_ = q.start(2)
	if err := q.setRepeat("all"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"t3", "t1", "t2"} {
		if err := q.next(); err != nil || p.playing() != want {
			t.Fatalf("next() = %v, playing %s, want %s", err, p.playing(), want)
		}
	}
}

// TestRepeatOneLoopsInMpv: repeat one is mpv's loop-file, and turning repeat
// off clears it — a daemon left on loop-file would replay one track forever in
// the next session too.
func TestRepeatOneLoopsInMpv(t *testing.T) {
	app, p, _ := listApp(3)
	q := &listQueue{app: app}
	_ = q.start(1)
	_ = q.setRepeat("one")
	_ = q.setRepeat("off")
	for _, want := range []string{"[set_property loop-file inf]", "[set_property loop-file no]"} {
		if !slices.Contains(p.sent, want) {
			t.Errorf("never sent %s: %v", want, p.sent)
		}
	}
}

// TestListAnnouncesANaturalAdvance: a track that ended on its own used to start
// the next one silently, so the dev hit `next` thinking nothing was playing and
// skipped a song (2026-10-03). The move is announced, with what follows it.
func TestListAnnouncesANaturalAdvance(t *testing.T) {
	app, p, _ := listApp(5)
	var out strings.Builder
	app.Out = &out
	q := &listQueue{app: app}
	_ = q.start(1)
	if !strings.Contains(out.String(), "▶ 1. t1") || !strings.Contains(out.String(), "next: 2. t2") {
		t.Errorf("start announced %q, want the track and what is next", out.String())
	}

	out.Reset()
	p.pos = 1 // t1 ended; mpv moved to t2 by itself
	i, moved := q.follow()
	if !moved || i != 1 {
		t.Fatalf("follow() = %d, %v — want the move to t2 noticed", i, moved)
	}
	q.announce(i)
	if !strings.Contains(out.String(), "▶ 2. t2") || !strings.Contains(out.String(), "next: 3. t3") {
		t.Errorf("natural advance announced %q, want ▶ 2. t2 and next: 3. t3", out.String())
	}
	if _, moved := q.follow(); moved {
		t.Error("follow() reported the same track as a new one")
	}
}

// TestSessionPromptTakesShuffleAndRepeat: both are prompt commands (the dev got
// "No help topic for 'shuffle'"), and the session's end undoes repeat so the
// daemon does not loop in a later session.
func TestSessionPromptTakesShuffleAndRepeat(t *testing.T) {
	app, p, _ := listApp(5)
	q := &listQueue{app: app}
	in := strings.NewReader("play 1\nshuffle\nrepeat one\nq\n")
	playSession(app, q, in, true, make(chan os.Signal), time.Millisecond)

	if !q.shuffle {
		t.Error("`shuffle` at the prompt did not turn shuffle on")
	}
	on := slices.Index(p.sent, "[set_property loop-file inf]")
	off := slices.Index(p.sent, "[set_property loop-file no]")
	if on < 0 || off < on || p.sent[len(p.sent)-1] != "[stop]" {
		t.Errorf("want `repeat one` to set loop-file inf, the session's end to clear it, then stop: %v", p.sent)
	}
}

// TestPlayIndexPlaysFromTheList: `play N` goes through the list, not through
// Meta + EnqueueAhead, which spent a yt-dlp round-trip over Tor re-describing a
// track the cache already described.
func TestPlayIndexPlaysFromTheList(t *testing.T) {
	app, p, r := listApp(5)
	if err := runVerb(app, "play", "2"); err != nil {
		t.Fatalf("play 2 = %v", err)
	}
	if r.metas != 0 || len(p.list) != 2 || p.list[0] != "t2" || p.list[1] != "t3" {
		t.Errorf("play 2: %d Meta calls, playlist %v — want 0 and [t2 t3]", r.metas, p.list)
	}
}

// TestSessionHintNamesEveryPromptVerb: the hint is built from the command
// table, so a verb added later shows up without anyone remembering to.
func TestSessionHintNamesEveryPromptVerb(t *testing.T) {
	app, _, _ := listApp(1)
	hint := sessionHint(commands(app))
	if !strings.HasSuffix(hint, "q stops") {
		t.Errorf("hint does not end with q stops:\n%s", hint)
	}
	for _, own := range []string{"shuffle", "repeat one|all|off"} { // the prompt's own verbs
		if !strings.Contains(hint, own) {
			t.Errorf("hint is missing %q:\n%s", own, hint)
		}
	}
	for _, c := range commands(app) {
		if notAtThePrompt[c.Name] {
			if strings.Contains(hint, " "+c.Name) {
				t.Errorf("hint offers %q, which does not belong at the prompt:\n%s", c.Name, hint)
			}
			continue
		}
		if !strings.Contains(hint, c.Name) {
			t.Errorf("hint is missing %q:\n%s", c.Name, hint)
		}
	}
}

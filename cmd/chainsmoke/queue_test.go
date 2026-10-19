package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
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
	if len(args) == 2 && args[0] == "playlist-play-index" {
		p.pos = args[1].(int)
		return nil, nil
	}
	switch fmt.Sprintf("%v", args) {
	case "[playlist-clear]": // all but the current entry (measured on mpv 0.41)
		if p.pos >= 0 {
			p.list, p.pos = []string{p.list[p.pos]}, 0
		}
	case "[get_property playlist-count]":
		return float64(len(p.list)), nil
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
func (s listStore) Results() ([]core.Track, error) {
	var ts []core.Track
	for i := 1; i <= s.n; i++ {
		t, _ := s.Result(i)
		ts = append(ts, t)
	}
	return ts, nil
}

// cacheStore is a result cache whose contents a test can replace mid-play,
// the way a `search` typed at the prompt, or run in another window, does.
type cacheStore struct{ ts []core.Track }

func (s *cacheStore) SaveResults(ts []core.Track) error { s.ts = ts; return nil }
func (s *cacheStore) Purge() error                      { return nil }
func (s *cacheStore) Results() ([]core.Track, error)    { return s.ts, nil }
func (s *cacheStore) Result(i int) (core.Track, error) {
	if i < 1 || i > len(s.ts) {
		return core.Track{}, fmt.Errorf("no cached result #%d (have 1-%d)", i, len(s.ts))
	}
	return s.ts[i-1], nil
}

// tracksNamed is a result set p1..pn.
func tracksNamed(prefix string, n int) []core.Track {
	var ts []core.Track
	for i := 1; i <= n; i++ {
		ts = append(ts, core.Track{Title: fmt.Sprintf("%s%d", prefix, i), Permalink: fmt.Sprintf("https://soundcloud.com/a/%s%d", prefix, i)})
	}
	return ts
}

// listResolver counts Meta and Stream separately: list playback must resolve
// each track exactly once, with no Meta round-trip for a single track. It
// keeps the URLs Meta was asked for, returns set from Meta when given one,
// fails the permalinks in fail, and holds a Stream for a permalink in gate
// until that channel closes. Preloads call Stream from a background
// goroutine, hence the lock.
type listResolver struct {
	mu             sync.Mutex
	metas, streams int
	metaURLs       []string
	set            []core.Track
	fail           map[string]bool
	gate           map[string]chan struct{}
}

func (r *listResolver) Meta(url string) ([]core.Track, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metas++
	r.metaURLs = append(r.metaURLs, url)
	if r.set != nil {
		return r.set, nil
	}
	return []core.Track{{Title: "t", Permalink: "p"}}, nil
}
func (r *listResolver) Stream(permalink string) (string, error) {
	r.mu.Lock()
	r.streams++
	gate, fail := r.gate[permalink], r.fail[permalink]
	r.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if fail {
		return "", errors.New("could not resolve")
	}
	return "u", nil
}
func (r *listResolver) Search(string, int) ([]core.Track, error) { return nil, nil }

// counts reads the call counters under the lock.
func (r *listResolver) counts() (metas, streams int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.metas, r.streams
}

// urls reads the URLs Meta was asked for, under the lock.
func (r *listResolver) urls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.metaURLs)
}

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
			q.settle() // the preload resolves in the background now
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
	if metas, streams := r.counts(); metas != 0 || streams != 4 {
		t.Errorf("%d Meta / %d Stream calls, want 0 / 4 (t2..t5, each once)", metas, streams)
	}
}

// TestListPlaybackIgnoresALaterSearch: the list being played is the one `play N`
// started on. It used to read the shared result cache on every step, so a
// `search`, `likes` or `playlist` typed at the prompt (or run in another
// window) spliced a different result set into playback mid-session — and the
// `next:` line named a track from it (2026-10-05 review, R1, reproduced live).
func TestListPlaybackIgnoresALaterSearch(t *testing.T) {
	for _, shuffle := range []bool{false, true} {
		p, st := newPlaylistPlayer(), &cacheStore{ts: tracksNamed("a", 4)}
		app := &core.App{Net: &recordingNet{}, Res: &listResolver{}, Play: p, Store: st, Out: io.Discard}
		q := &listQueue{app: app, shuffle: shuffle}
		if err := q.start(1); err != nil {
			t.Fatalf("shuffle=%v: start(1) = %v", shuffle, err)
		}
		_ = st.SaveResults(tracksNamed("b", 4)) // a search lands while a1 plays

		for range 3 {
			if err := q.next(); err != nil {
				t.Fatalf("shuffle=%v: next() = %v", shuffle, err)
			}
			if got := p.playing(); !strings.HasPrefix(got, "a") {
				t.Errorf("shuffle=%v: playing %s from the later search, want the list play 1 started (playlist %v)", shuffle, got, p.list)
			}
		}
		if err := q.next(); err == nil || !strings.Contains(err.Error(), "end of the list") {
			t.Errorf("shuffle=%v: next() after a4 = %v, want end of the list", shuffle, err)
		}
	}
}

// TestBareVerbAtThePromptReadsTheNextLine: bare `play`, `likes` or `playlist`
// at the prompt asks for its input on the next line. It used to read stdin
// itself, racing the prompt's own reader: the prompt won, the verb got nothing,
// and the URL then ran as a command — "No help topic for 'https://…'"
// (2026-10-05 review, R3). The session's input is the only reader now.
func TestBareVerbAtThePromptReadsTheNextLine(t *testing.T) {
	app, _, r := listApp(3)
	in := strings.NewReader("play\nhttps://soundcloud.com/a/b\nlikes\nsomeone\nq\n")
	app.Stdin = in // as in production: verbs read app.Stdin, the session scans the same input
	playSession(app, &listQueue{app: app}, in, true, make(chan os.Signal), time.Millisecond)

	want := []string{"https://soundcloud.com/a/b", "https://soundcloud.com/someone/likes"}
	if got := r.urls(); !slices.Equal(got, want) {
		t.Errorf("resolver was asked for %v, want %v — the next line never reached the verb", got, want)
	}
	if app.Stdin != in {
		t.Error("the session did not give app.Stdin back when it ended")
	}
}

// TestURLTypedAtThePromptIsPlayed: `play <url>` at the prompt was refused as
// "already reached argv… treat it as exposed" — but a typed line never reaches
// argv or the shell's history, the same exposure as the URL prompt itself
// (2026-10-05 review, R4). Real argv is still refused (TestPlayRejectsBadInput…).
func TestURLTypedAtThePromptIsPlayed(t *testing.T) {
	app, _, r := listApp(3)
	in := strings.NewReader("play https://soundcloud.com/a/typed\nq\n")
	playSession(app, &listQueue{app: app}, in, true, make(chan os.Signal), time.Millisecond)

	if got := r.urls(); !slices.Equal(got, []string{"https://soundcloud.com/a/typed"}) {
		t.Errorf("resolver was asked for %v, want the typed URL played", got)
	}
}

// TestPlayNKeepsTheOldTrackWhenTheNewOneFails: `play N` used to stop what was
// playing before resolving N, so a resolve that failed (a DRM track, a bad
// circuit) left silence (2026-10-05 review, R5). Nothing changes now until N
// is in hand.
func TestPlayNKeepsTheOldTrackWhenTheNewOneFails(t *testing.T) {
	app, p, r := listApp(5)
	r.fail = map[string]bool{"https://soundcloud.com/a/t3": true}
	q := &listQueue{app: app}
	_ = q.start(1)

	if err := q.start(3); err == nil {
		t.Fatal("start(3) = nil, want the resolve failure")
	}
	if p.playing() != "t1" {
		t.Errorf("after a failed play 3, playing %s, want t1 still", p.playing())
	}
	if err := q.start(4); err != nil || p.playing() != "t4" || len(p.list) > 2 {
		t.Errorf("start(4) = %v, playing %s, playlist %v — want t4 alone (plus a preload)", err, p.playing(), p.list)
	}
}

// TestStdinSetPlaysAsAList: a set URL on stdin used to queue two tracks and
// say "run `chainsmoke next` when this pair ends" — a next that then failed
// (2026-10-05 review, R2). Its tracks are a list like any search now.
func TestStdinSetPlaysAsAList(t *testing.T) {
	app, p, r := listApp(0)
	r.set = tracksNamed("s", 3)
	q := &listQueue{app: app}

	if err := q.startList(r.set); err != nil {
		t.Fatalf("startList = %v", err)
	}
	for _, want := range []string{"s2", "s3"} {
		if err := q.next(); err != nil || p.playing() != want {
			t.Fatalf("next() = %v, playing %s, want %s", err, p.playing(), want)
		}
	}
	if err := q.startList(nil); err == nil {
		t.Error("startList(nothing) = nil, want an error")
	}

	// Skip-not-abort (T1.14): a dead first track costs only itself, and a set
	// where nothing plays is an error, not silence.
	app, p, r = listApp(0)
	r.fail = map[string]bool{"https://soundcloud.com/a/s1": true}
	if err := (&listQueue{app: app}).startList(tracksNamed("s", 3)); err != nil || p.playing() != "s2" {
		t.Errorf("startList with s1 dead = %v, playing %s, want s2", err, p.playing())
	}
	app, _, r = listApp(0)
	r.fail = map[string]bool{"https://soundcloud.com/a/d1": true, "https://soundcloud.com/a/d2": true}
	if err := (&listQueue{app: app}).startList(tracksNamed("d", 2)); err == nil {
		t.Error("startList with every track dead = nil, want an error")
	}
}

// TestPreloadRunsInTheBackground: the resolve of the next track used to run on
// the prompt's own goroutine — up to 3 tracks × 3 attempts × 45s — so typing
// was ignored and Ctrl+C waited for it (2026-10-05 review, R6, H5). Here t2's
// resolve never finishes on its own: start must still return, announce what
// is next at once, and the prompt must keep answering and still quit.
func TestPreloadRunsInTheBackground(t *testing.T) {
	app, p, r := listApp(5)
	release := make(chan struct{})
	defer close(release)
	r.gate = map[string]chan struct{}{"https://soundcloud.com/a/t2": release}
	var out strings.Builder
	app.Out = &out

	done := make(chan struct{})
	go func() {
		in := strings.NewReader("play 1\nvol 40\nq\n")
		playSession(app, &listQueue{app: app}, in, true, make(chan os.Signal), time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session was held by a preload that had not finished")
	}
	if !slices.Contains(p.sent, "[set_property volume 40]") {
		t.Errorf("`vol 40` never ran while t2 was resolving: %v", p.sent)
	}
	if !strings.Contains(out.String(), "▶ 1. t1  ·  next: 2. t2") {
		t.Errorf("announced %q, want t1 and what is next, without waiting for t2", out.String())
	}
}

// TestPreloadSkipsATrackThatWillNotResolve: one dead track costs only itself —
// the rule EnqueueAhead used to pin (T1.14).
func TestPreloadSkipsATrackThatWillNotResolve(t *testing.T) {
	app, p, r := listApp(4)
	r.fail = map[string]bool{"https://soundcloud.com/a/t2": true}
	q := &listQueue{app: app}
	_ = q.start(1)
	if err := q.next(); err != nil || p.playing() != "t3" {
		t.Errorf("next() = %v, playing %s, want t3 with t2 skipped", err, p.playing())
	}
}

// TestAnnounceScrubsHostileTitles: titles are third-party text on a terminal —
// an ESC repaints it, a newline forges a line (the other rule EnqueueAhead
// pinned).
func TestAnnounceScrubsHostileTitles(t *testing.T) {
	p, r := newPlaylistPlayer(), &listResolver{}
	var out strings.Builder
	st := &cacheStore{ts: []core.Track{{Title: "red \x1b[31mALERT\x1b[0m\n▶ forged", Permalink: "https://soundcloud.com/a/x"}}}
	app := &core.App{Net: &recordingNet{}, Res: r, Play: p, Store: st, Out: &out}
	_ = (&listQueue{app: app}).start(1)
	if strings.ContainsRune(out.String(), 0x1b) || strings.Count(out.String(), "\n") != 1 {
		t.Errorf("the ▶ line printed the title raw: %q", out.String())
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
	q.settle()
	p.pos = 1 // t1 ended; mpv is on t2
	q.preload()
	q.settle()
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
	q.settle()

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
	if metas, _ := r.counts(); metas != 0 || len(p.list) == 0 || p.list[0] != "t2" {
		t.Errorf("play 2: %d Meta calls, playlist %v — want 0, starting with t2", metas, p.list)
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

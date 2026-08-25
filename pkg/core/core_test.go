package core

import (
	"errors"
	"fmt"
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

// --- T1.7 / T3.5: one recovery action, and the RIGHT one ----------------------

// TestYtdlpHintGivesTheFittingAdvice is the whole point of the taxonomy. Every
// one of these failures used to end in "run: yt-dlp -U", and for three of the
// four that is advice which cannot possibly help — the update fixes an
// extractor, not a blocked exit, a dead bridge, or a flag we passed ourselves.
func TestYtdlpHintGivesTheFittingAdvice(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    string
		mustNot string
	}{
		{"stale extractor", fmt.Errorf("%w: x", ErrStaleExtractor), "yt-dlp -U", "rotate"},
		{"blocked exit", fmt.Errorf("%w: x", ErrRateLimited), "chainsmoke rotate", "yt-dlp -U"},
		// Found live 2026-08-24 on a yt-dlp without curl_cffi: nothing left the
		// machine, so neither SoundCloud nor the extractor is implicated.
		{"rejected impersonate", fmt.Errorf("%w: x", ErrImpersonate), "curl-cffi", "yt-dlp -U"},
		// Found live 2026-08-24 running `likes` against a mistyped handle.
		// SoundCloud answered and the extractor parsed the answer correctly;
		// the answer was "no such page". Updating yt-dlp cannot change that.
		{"mistyped handle", fmt.Errorf("%w: x", ErrNotFound), "check the spelling", "yt-dlp -U"},
		// A dead bridge is pkg/net's problem and the error already says so;
		// appending anything would point the user away from the real fault.
		{"dead bridge", fmt.Errorf("%w: x", ErrEgress), "", "yt-dlp -U"},
		// A stub must never send anyone chasing a dependency (T1.7).
		{"stub", ErrNotImplemented, "", "yt-dlp -U"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := YtdlpHint(tc.err).Error()
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("YtdlpHint = %q, want it to mention %q", got, tc.want)
			}
			if strings.Contains(got, tc.mustNot) {
				t.Errorf("YtdlpHint = %q, must NOT suggest %q for this failure", got, tc.mustNot)
			}
		})
	}

	if YtdlpHint(nil) != nil {
		t.Error("YtdlpHint(nil) invented an error")
	}
	// Anything unclassified still gets the extractor hint: a resolve that failed
	// for a reason we do not recognise is most often the extractor.
	if got := YtdlpHint(errors.New("something new")).Error(); !strings.Contains(got, "yt-dlp -U") {
		t.Errorf("unclassified hint = %q, want the extractor fallback", got)
	}
}

// -----------------------------------------------------------------------------
// Index-vs-stdin resolution (TASKS.md T9.1)
// -----------------------------------------------------------------------------

// indexStore records which index was asked for, so the argv path can be
// distinguished from the stdin path rather than inferred from the result.
type indexStore struct {
	asked int
	err   error
}

func (s *indexStore) SaveResults([]Track) error { return nil }
func (s *indexStore) Purge() error              { return nil }
func (s *indexStore) Result(n int) (Track, error) {
	s.asked = n
	if s.err != nil {
		return Track{}, s.err
	}
	return Track{Title: "cached", Permalink: "https://soundcloud.com/a/cached"}, nil
}

// TestResolveIndexOrStdinRoutes covers the three-way branch that implements
// invariant #3. Each row is a different CLASS of input, and getting the class
// wrong is what makes the failure dangerous rather than merely annoying: a URL
// that falls through to the stdin path would be silently accepted from argv.
func TestResolveIndexOrStdinRoutes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arg       string
		stdin     string
		wantIndex int    // 0 = the store must not be consulted
		wantPerma string // "" = expect an error
		wantErr   string
	}{
		{name: "index reaches the store", arg: "3", wantIndex: 3,
			wantPerma: "https://soundcloud.com/a/cached"},
		{name: "index 1 is not confused with empty", arg: "1", wantIndex: 1,
			wantPerma: "https://soundcloud.com/a/cached"},
		{name: "url in argv is refused", arg: "https://soundcloud.com/a/b",
			wantErr: "not accepted in argv"},
		{name: "bare word in argv is refused the same way", arg: "notanumber",
			wantErr: "not accepted in argv"},
		{name: "overflow is an index error, not a URL error", arg: "99999999999999999999",
			wantErr: "out of range"},
		{name: "no arg reads one line from stdin", stdin: "https://soundcloud.com/a/piped\n",
			wantPerma: "https://soundcloud.com/a/piped"},
		{name: "stdin is trimmed", stdin: "  https://soundcloud.com/a/pad  \n",
			wantPerma: "https://soundcloud.com/a/pad"},
		{name: "empty stdin is a clear error", stdin: "", wantErr: "no result index"},
		{name: "blank line on stdin is a clear error", stdin: "   \n", wantErr: "empty URL on stdin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &indexStore{}
			app := &App{Store: st, Stdin: strings.NewReader(tc.stdin)}

			got, err := app.ResolveIndexOrStdin(tc.arg)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("got %+v, want an error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Permalink != tc.wantPerma {
				t.Errorf("permalink = %q, want %q", got.Permalink, tc.wantPerma)
			}
			if st.asked != tc.wantIndex {
				t.Errorf("store was asked for index %d, want %d", st.asked, tc.wantIndex)
			}
		})
	}
}

// TestResolveIndexOrStdinRefusalNamesTheExposure: by the time we see a URL in
// argv, the OS process list already has it. The message has to say so and give
// the routes that leak nothing — telling the user "invalid argument" would let
// them retype it and leak it twice.
func TestResolveIndexOrStdinRefusalNamesTheExposure(t *testing.T) {
	app := &App{Store: &indexStore{}, Stdin: strings.NewReader("")}

	_, err := app.ResolveIndexOrStdin("https://soundcloud.com/artist/secret-track")
	if err == nil {
		t.Fatal("a URL in argv was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"world-readable", "already reached it", "chainsmoke play"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal missing %q:\n%s", want, msg)
		}
	}
}

// TestResolveIndexOrStdinDoesNotConsultTheStoreForAURL: the store lookup is
// skipped entirely on the argv-URL path. If it were consulted first, a cache
// miss would mask the invariant-#3 message behind "no cached result".
func TestResolveIndexOrStdinDoesNotConsultTheStoreForAURL(t *testing.T) {
	st := &indexStore{err: errors.New("no cached result #0")}
	app := &App{Store: st, Stdin: strings.NewReader("")}

	_, err := app.ResolveIndexOrStdin("https://soundcloud.com/a/b")
	if err == nil || !strings.Contains(err.Error(), "not accepted in argv") {
		t.Fatalf("error = %v, want the argv refusal", err)
	}
	if st.asked != 0 {
		t.Errorf("store was consulted (index %d) for a URL argument", st.asked)
	}
}

// TestResolveIndexOrStdinPropagatesStoreErrors: a real cache miss must surface
// as itself, with the remediation the store wrote.
func TestResolveIndexOrStdinPropagatesStoreErrors(t *testing.T) {
	miss := errors.New("no cached result #7 — run `chainsmoke search <term>` first")
	app := &App{Store: &indexStore{err: miss}, Stdin: strings.NewReader("")}

	if _, err := app.ResolveIndexOrStdin("7"); !errors.Is(err, miss) {
		t.Errorf("ResolveIndexOrStdin = %v, want the store's error", err)
	}
}

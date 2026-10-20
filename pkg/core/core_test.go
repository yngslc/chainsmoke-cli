package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

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
func (s *fakeStore) Results() ([]Track, error) { return nil, nil }
func (s *fakeStore) Result(int) (Track, error) { return Track{}, nil }
func (s *fakeStore) Purge() error              { s.log.note("store.Purge"); return s.purgeErr }

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

func TestYtdlpHintGivesTheFittingAdvice(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    string
		mustNot string
	}{
		{"stale extractor", fmt.Errorf("%w: x", ErrStaleExtractor), "yt-dlp -U", "rotate"},
		{"blocked exit", fmt.Errorf("%w: x", ErrRateLimited), "chainsmoke rotate", "yt-dlp -U"},
		{"rejected impersonate", fmt.Errorf("%w: x", ErrImpersonate), "curl-cffi", "yt-dlp -U"},
		{"mistyped handle", fmt.Errorf("%w: x", ErrNotFound), "check the spelling", "yt-dlp -U"},
		{"dead bridge", fmt.Errorf("%w: x", ErrEgress), "", "yt-dlp -U"},
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
	if got := YtdlpHint(errors.New("something new")).Error(); !strings.Contains(got, "yt-dlp -U") {
		t.Errorf("unclassified hint = %q, want the extractor fallback", got)
	}
}

type indexStore struct {
	asked int
	err   error
}

func (s *indexStore) SaveResults([]Track) error { return nil }
func (s *indexStore) Results() ([]Track, error) { return nil, s.err }
func (s *indexStore) Purge() error              { return nil }
func (s *indexStore) Result(n int) (Track, error) {
	s.asked = n
	if s.err != nil {
		return Track{}, s.err
	}
	return Track{Title: "cached", Permalink: "https://soundcloud.com/a/cached"}, nil
}

func TestResolveIndexOrStdinRoutes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arg       string
		typed     bool
		stdin     string
		wantIndex int
		wantPerma string
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
		{name: "url typed at the session prompt is taken as a URL", arg: "https://soundcloud.com/a/typed",
			typed: true, wantPerma: "https://soundcloud.com/a/typed"},
		{name: "an index typed at the prompt is still an index", arg: "2", typed: true, wantIndex: 2,
			wantPerma: "https://soundcloud.com/a/cached"},
		{name: "overflow typed at the prompt is still an index error", arg: "99999999999999999999",
			typed: true, wantErr: "out of range"},
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

			got, err := app.ResolveIndexOrStdin(tc.arg, tc.typed)

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

func TestResolveIndexOrStdinRefusalNamesTheExposure(t *testing.T) {
	app := &App{Store: &indexStore{}, Stdin: strings.NewReader("")}

	_, err := app.ResolveIndexOrStdin("https://soundcloud.com/artist/secret-track", false)
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

func TestResolveIndexOrStdinDoesNotConsultTheStoreForAURL(t *testing.T) {
	st := &indexStore{err: errors.New("no cached result #0")}
	app := &App{Store: st, Stdin: strings.NewReader("")}

	_, err := app.ResolveIndexOrStdin("https://soundcloud.com/a/b", false)
	if err == nil || !strings.Contains(err.Error(), "not accepted in argv") {
		t.Fatalf("error = %v, want the argv refusal", err)
	}
	if st.asked != 0 {
		t.Errorf("store was consulted (index %d) for a URL argument", st.asked)
	}
}

func TestResolveIndexOrStdinPropagatesStoreErrors(t *testing.T) {
	miss := errors.New("no cached result #7 — run `chainsmoke search <term>` first")
	app := &App{Store: &indexStore{err: miss}, Stdin: strings.NewReader("")}

	if _, err := app.ResolveIndexOrStdin("7", false); !errors.Is(err, miss) {
		t.Errorf("ResolveIndexOrStdin = %v, want the store's error", err)
	}
}

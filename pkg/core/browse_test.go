package core

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestLikesURL(t *testing.T) {
	got, err := LikesURL("ghostinnafurcoat")
	if err != nil {
		t.Fatalf("LikesURL: %v", err)
	}
	if want := "https://soundcloud.com/ghostinnafurcoat/likes"; got != want {
		t.Errorf("LikesURL = %q, want %q", got, want)
	}
	if got, err := LikesURL("  /ghostinnafurcoat/ "); err != nil || !strings.HasSuffix(got, "/ghostinnafurcoat/likes") {
		t.Errorf("LikesURL(padded) = %q, %v", got, err)
	}
}

func TestSetURL(t *testing.T) {
	const want = "https://soundcloud.com/ghostinnafurcoat/sets/late-night"
	for _, in := range []string{
		"ghostinnafurcoat/late-night",
		"ghostinnafurcoat/sets/late-night",
		"ghostinnafurcoat/late-night/",
	} {
		got, err := SetURL(in)
		if err != nil {
			t.Errorf("SetURL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("SetURL(%q) = %q, want %q", in, got, want)
		}
	}

	if _, err := SetURL("ghostinnafurcoat"); err == nil {
		t.Error("SetURL accepted a bare handle")
	} else if !strings.Contains(err.Error(), "<user>/<set>") {
		t.Errorf("error %q does not show the expected form", err)
	}
}

func TestPageURLsCannotLeaveSoundCloud(t *testing.T) {
	hostile := []string{
		"https://evil.com/x",
		"http://evil.com",
		"evil.com",
		"soundcloud.com.evil.net",
		"//evil.com",
		"user@evil.com",
		"x/../../evil.com",
		"x?redirect=evil.com",
		"x#evil",
		"x%2f..%2fevil.com",
		"x:8080",
		"x\nHost: evil.com",
		"x y",
		"",
		"   ",
		"..",
		"/",
	}
	check := func(t *testing.T, name, raw string, err error) {
		t.Helper()
		if err != nil {
			return
		}
		u, perr := url.Parse(raw)
		if perr != nil {
			t.Errorf("%s produced an unparseable URL %q: %v", name, raw, perr)
			return
		}
		if u.Scheme != "https" || u.Host != "soundcloud.com" {
			t.Errorf("%s escaped to %q (scheme %q, host %q)", name, raw, u.Scheme, u.Host)
		}
	}
	for _, in := range hostile {
		got, err := LikesURL(in)
		check(t, "LikesURL("+in+")", got, err)

		got, err = SetURL(in)
		check(t, "SetURL("+in+")", got, err)

		got, err = SetURL(in + "/" + in)
		check(t, "SetURL("+in+"/"+in+")", got, err)
	}
}

func TestPastedURLGetsTheInvariantMessage(t *testing.T) {
	for _, in := range []string{
		"https://soundcloud.com/ghostinnafurcoat/likes",
		"soundcloud.com/ghostinnafurcoat",
		"www.soundcloud.com/x",
	} {
		_, err := LikesURL(in)
		if err == nil {
			t.Errorf("LikesURL(%q) was accepted", in)
			continue
		}
		if !strings.Contains(err.Error(), "world-readable") {
			t.Errorf("LikesURL(%q) = %q, want the no-URLs-in-argv explanation", in, err)
		}
	}
}

type metaResolver struct {
	tracks []Track
	err    error
	asked  string
}

func (r *metaResolver) Meta(pageURL string) ([]Track, error) {
	r.asked = pageURL
	return r.tracks, r.err
}
func (r *metaResolver) Stream(string) (string, error)       { return "", nil }
func (r *metaResolver) Search(string, int) ([]Track, error) { return nil, nil }

type recordingStore struct {
	saved []Track
	err   error
}

func (s *recordingStore) SaveResults(ts []Track) error { s.saved = ts; return s.err }
func (s *recordingStore) Results() ([]Track, error)    { return s.saved, s.err }
func (s *recordingStore) Result(int) (Track, error)    { return Track{}, nil }
func (s *recordingStore) Purge() error                 { return nil }

func collectTracks(n int) []Track {
	ts := make([]Track, n)
	for i := range ts {
		ts[i] = Track{Title: string(rune('A' + i)), Permalink: "p"}
	}
	return ts
}

func TestCollectCachesExactlyWhatItPrints(t *testing.T) {
	res := &metaResolver{tracks: collectTracks(5)}
	st := &recordingStore{}
	var out bytes.Buffer
	app := &App{Res: res, Store: st, Out: &out}

	if err := app.Collect("https://soundcloud.com/x/likes", 3); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.asked != "https://soundcloud.com/x/likes" {
		t.Errorf("resolver was asked for %q", res.asked)
	}
	if len(st.saved) != 3 {
		t.Fatalf("cached %d tracks, want the limit of 3", len(st.saved))
	}
	printed := strings.Count(strings.TrimRight(out.String(), "\n"), "\n") + 1
	if printed != len(st.saved) {
		t.Errorf("printed %d rows but cached %d — `play N` would not match the screen\n%s",
			printed, len(st.saved), out.String())
	}
	for i, tr := range st.saved {
		if !strings.Contains(out.String(), tr.Title) {
			t.Errorf("cached track %d (%q) never appeared on screen", i+1, tr.Title)
		}
	}
}

func TestCollectWithoutLimitKeepsEverything(t *testing.T) {
	res := &metaResolver{tracks: collectTracks(20)}
	st := &recordingStore{}
	app := &App{Res: res, Store: st, Out: &bytes.Buffer{}}

	for _, limit := range []int{0, -1, 50} {
		st.saved = nil
		if err := app.Collect("u", limit); err != nil {
			t.Fatalf("Collect(limit=%d): %v", limit, err)
		}
		if len(st.saved) != 20 {
			t.Errorf("limit=%d cached %d, want all 20", limit, len(st.saved))
		}
	}
}

func TestCollectAddsTheRecoveryHint(t *testing.T) {
	boom := errors.New("HTTP Error 404")
	app := &App{
		Res:   &metaResolver{err: boom},
		Store: &recordingStore{},
		Out:   &bytes.Buffer{},
	}
	err := app.Collect("u", 0)
	if !errors.Is(err, boom) {
		t.Fatalf("Collect = %v, want the resolver error wrapped", err)
	}
	if !strings.Contains(err.Error(), "yt-dlp -U") {
		t.Errorf("Collect = %q, want the recovery hint appended", err)
	}
}

func TestCollectDoesNotPrintWhenTheCacheFails(t *testing.T) {
	var out bytes.Buffer
	app := &App{
		Res:   &metaResolver{tracks: collectTracks(3)},
		Store: &recordingStore{err: errors.New("disk full")},
		Out:   &out,
	}
	if err := app.Collect("u", 0); err == nil {
		t.Fatal("Collect = nil despite a failed cache write")
	}
	if out.Len() != 0 {
		t.Errorf("printed a list that play N cannot index:\n%s", out.String())
	}
}

func TestHandleOrStdin(t *testing.T) {
	app := &App{Stdin: strings.NewReader("from-stdin\n")}

	if got, err := app.HandleOrStdin("from-argv", "p: ", "usage"); err != nil || got != "from-argv" {
		t.Errorf("HandleOrStdin(argv) = %q, %v", got, err)
	}
	if got, err := app.HandleOrStdin("", "p: ", "usage"); err != nil || got != "from-stdin" {
		t.Errorf("HandleOrStdin(stdin) = %q, %v", got, err)
	}

	empty := &App{Stdin: strings.NewReader("")}
	_, err := empty.HandleOrStdin("", "p: ", "usage: chainsmoke likes <user>")
	if err == nil {
		t.Fatal("HandleOrStdin accepted an empty stdin")
	}
	if err.Error() == "EOF" || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("HandleOrStdin = %q, want the usage line", err)
	}
}

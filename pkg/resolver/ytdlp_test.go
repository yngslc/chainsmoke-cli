package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"chainsmoke/pkg/core"
)

// fakeNet is a Net that never brings anything up. gate decides whether Assert
// permits egress; rotates counts what the retry policy asked for.
type fakeNet struct {
	gate      error
	proxy     string
	rotateErr error
	rotates   int
}

func (n *fakeNet) Up() error        { return nil }
func (n *fakeNet) Down() error      { return nil }
func (n *fakeNet) Rotate() error    { n.rotates++; return n.rotateErr }
func (n *fakeNet) SelfTest() error  { return nil }
func (n *fakeNet) ProxyURL() string { return n.proxy }
func (n *fakeNet) Assert() error    { return n.gate }
func (n *fakeNet) Verified() bool   { return n.gate == nil }

// TestFailClosed is the T0.2 regression guard: with the gate shut, no resolver
// entry point may reach the exec path. It passes on a machine with no yt-dlp
// precisely because nothing should be run.
func TestFailClosed(t *testing.T) {
	r := New(&fakeNet{gate: errors.New("egress not verified")}, "chrome")

	if _, err := r.Meta("https://soundcloud.com/a/b"); err == nil {
		t.Error("Meta: want error when Assert fails, got nil")
	}
	if _, err := r.Stream("https://soundcloud.com/a/b"); err == nil {
		t.Error("Stream: want error when Assert fails, got nil")
	}
	if _, err := r.Search("kendrick", 10); err == nil {
		t.Error("Search: want error when Assert fails, got nil")
	}
}

// TestProxyArgs pins the sentinel handling: ProxyURL returns prose, not a URL,
// when there is no proxy, and "--proxy DIRECT (no proxy)" must never be built.
func TestProxyArgs(t *testing.T) {
	for _, tc := range []struct {
		proxy string
		want  []string
	}{
		{"http://127.0.0.1:41235", []string{"--proxy", "http://127.0.0.1:41235"}},
		{"DIRECT (no proxy)", nil},
		{"<down>", nil},
		{"", nil},
	} {
		got := (&YTDLP{net: &fakeNet{proxy: tc.proxy}}).proxyArgs()
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("proxyArgs(%q) = %v, want %v", tc.proxy, got, tc.want)
		}
	}
}

// TestClassify feeds classify the stderr real yt-dlp 2026.07.04 produced on the
// dev target. Synthetic strings would only prove the matchers match themselves.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stderr string
		want   error
	}{
		{
			// curl_cffi path (--impersonate): names the extractor, never the
			// proxy. Classifying this as a stale extractor is the exact
			// misdiagnosis T3.5 exists to prevent.
			name:   "dead http proxy via curl_cffi",
			stderr: "ERROR: [soundcloud] Unable to download webpage: Failed to perform, curl: (7) Failed to connect to 127.0.0.1 port 9 after 2038 ms: Could not connect to server.",
			want:   core.ErrEgress,
		},
		{
			name:   "dead http proxy via urllib",
			stderr: "ERROR: [soundcloud:search] Unable to download webpage: ('Unable to connect to proxy', NewConnectionError(\"HTTPSConnection(host='127.0.0.1', port=9)\"))",
			want:   core.ErrEgress,
		},
		{
			// The OS message is localized; only the number is stable.
			name:   "dead socks proxy, localized OS error",
			stderr: "ERROR: [soundcloud] Unable to download webpage: SocksHTTPSConnection(host='soundcloud.com', port=443): Failed to establish a new connection: [WinError 10061] Подключение не установлено",
			want:   core.ErrEgress,
		},
		{
			name:   "blocked exit",
			stderr: "ERROR: [soundcloud] 12345: Unable to download JSON metadata: HTTP Error 403: Forbidden",
			want:   core.ErrRateLimited,
		},
		{
			name:   "rate limited",
			stderr: "ERROR: [soundcloud] Unable to download API page: HTTP Error 429: Too Many Requests",
			want:   core.ErrRateLimited,
		},
		{
			name:   "stale extractor",
			stderr: "ERROR: [soundcloud] Unable to extract client_id; please report this issue on https://github.com/yt-dlp/yt-dlp/issues",
			want:   core.ErrStaleExtractor,
		},
		{
			// Found live 2026-08-24 on a yt-dlp without curl_cffi. Nothing left
			// the machine, so the retry policy must not spend circuits on it and
			// the hint must not blame SoundCloud.
			name:   "rejected impersonate target",
			stderr: "yt_dlp.utils.YoutubeDLError: Impersonate target \"smoke-invalid\" is not available. Use --list-impersonate-targets to see available targets. You may be missing dependencies required to support this target.",
			want:   core.ErrImpersonate,
		},
		{
			// Verbatim stderr from `chainsmoke likes` against a mistyped handle,
			// 2026-08-24. The Python deprecation warning is deliberately kept in
			// the fixture: it is what yt-dlp actually printed first, and it used
			// to lead the message the user saw.
			name: "no such user (404)",
			stderr: "Deprecated Feature: Support for Python version 3.10 has been deprecated. Please update to Python 3.11 or above\n" +
				"ERROR: [soundcloud:user] ktotyt: Unable to download JSON metadata: HTTP Error 404: Not Found (caused by <HTTPError 404: Not Found>)",
			want: core.ErrNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classify(context.Background(), errors.New("exit status 1"), tc.stderr)
			if !errors.Is(err, tc.want) {
				t.Fatalf("classify() = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "https://") {
				t.Errorf("classified error leaked a URL: %v", err)
			}
		})
	}
}

// TestClassifyTimeout: the killed process reports a generic status, so the
// deadline has to be read off the context or it is reported as a yt-dlp fault.
func TestClassifyTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()

	err := classify(ctx, errors.New("signal: killed"), "")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("classify() = %v, want a timeout error", err)
	}
	// It must also be an egress fault: that is what makes run() rotate and
	// retry rather than surrender, and what keeps the "yt-dlp -U" hint away
	// from a failure yt-dlp cannot fix.
	if !errors.Is(err, core.ErrEgress) {
		t.Errorf("classify() = %v, want it to wrap core.ErrEgress", err)
	}
}

// TestParseBothShapes covers T3.1's real requirement: `play <track-url>` and
// `play <set-url>` hand Meta two different JSON documents.
func TestParseBothShapes(t *testing.T) {
	const set = `{"_type":"playlist","id":"98765","title":"a set",
	  "entries":[
	    {"id":"1","title":"one","url":"https://soundcloud.com/a/one","duration":123.4},
	    {"id":"2","title":"two","webpage_url":"https://soundcloud.com/a/two","uploader":"a","duration":60}
	  ]}`
	const single = `{"id":"7","title":"solo","webpage_url":"https://soundcloud.com/a/solo",
	  "uploader":"a","duration":211.9}`

	var pl ytResult
	if err := json.Unmarshal([]byte(set), &pl); err != nil {
		t.Fatalf("set: %v", err)
	}
	tracks := toTracks(entriesFrom(pl, "https://soundcloud.com/a/sets/x"))
	if len(tracks) != 2 {
		t.Fatalf("set: got %d tracks, want 2", len(tracks))
	}
	// T1.11: entry 1 has only "url", entry 2 only "webpage_url".
	if tracks[0].Permalink != "https://soundcloud.com/a/one" ||
		tracks[1].Permalink != "https://soundcloud.com/a/two" {
		t.Errorf("set: permalinks = %q, %q", tracks[0].Permalink, tracks[1].Permalink)
	}
	if tracks[0].Duration != 123 { // float in the JSON, seconds in core.Track
		t.Errorf("set: duration = %d, want 123", tracks[0].Duration)
	}

	var one ytResult
	if err := json.Unmarshal([]byte(single), &one); err != nil {
		t.Fatalf("single: %v", err)
	}
	if len(one.Entries) != 0 {
		t.Fatalf("single: got %d entries, want 0 (it is the flat object itself)", len(one.Entries))
	}
	tracks = toTracks(entriesFrom(one, "https://soundcloud.com/a/solo"))
	if len(tracks) != 1 || tracks[0].Permalink != "https://soundcloud.com/a/solo" {
		t.Fatalf("single: got %+v", tracks)
	}

	// An EMPTY set must not degrade into "one track whose URL is the set page":
	// mpv would be handed an HTML page to play.
	var empty ytResult
	if err := json.Unmarshal([]byte(`{"_type":"playlist","id":"5","entries":[]}`), &empty); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if got := entriesFrom(empty, "https://soundcloud.com/a/sets/empty"); len(got) != 0 {
		t.Errorf("empty set: got %d entries, want 0", len(got))
	}
}

// TestToTracksSkips: an entry with no URL is unplayable, but it must cost only
// itself (T1.14), and a blank title must not print as a blank row.
func TestToTracksSkips(t *testing.T) {
	got := toTracks([]ytEntry{
		{ID: "1", Title: "kept", URL: "https://soundcloud.com/a/1"},
		{ID: "2", Title: "no url"},
		{ID: "3", URL: "https://soundcloud.com/a/late-night-drive"},
	})
	if len(got) != 2 {
		t.Fatalf("got %d tracks, want 2", len(got))
	}
	if got[1].Title != "Late night drive" {
		t.Errorf("title = %q, want the slug-derived name", got[1].Title)
	}
}

// TestTitleFromSlug covers the shape that made `playlist` unusable. MEASURED
// 2026-08-24: a SET's flat entries are `_type: "url_transparent"` and carry
// album, id and url — and NO title — so every row printed "(untitled)" until
// the slug became the fallback.
func TestTitleFromSlug(t *testing.T) {
	cases := map[string]string{
		"https://soundcloud.com/changing-currents/like-a-star-x-bicycle-changing-currents-mashup": "Like a star x bicycle changing currents mashup",
		"https://soundcloud.com/a/late-night-drive":                                               "Late night drive",
		"https://soundcloud.com/a/track_two":                                                      "Track two",
		"https://soundcloud.com/a/plain":                                                          "Plain",
		"https://soundcloud.com/a/trailing/":                                                      "Trailing",
		"https://soundcloud.com/a/x?utm_source=copy":                                              "X",
		"https://soundcloud.com/a/y#t=30":                                                         "Y",
		"":                                                                                        "",
		"https://soundcloud.com/a/":                                                               "",
	}
	for in, want := range cases {
		if got := titleFromSlug(in); got != want {
			t.Errorf("titleFromSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSlugTitleIsNotAURL is the invariant-#3 half of the decision above. The
// slug discloses exactly what a title column already discloses; what must never
// escape is anything that reconstructs the address — the scheme, the host, the
// user segment, or a pasteable link.
func TestSlugTitleIsNotAURL(t *testing.T) {
	const permalink = "https://soundcloud.com/ghostinnafurcoat/lemon-black"
	got := toTracks([]ytEntry{{ID: "1", URL: permalink}})
	if len(got) != 1 {
		t.Fatalf("got %d tracks", len(got))
	}
	title := got[0].Title
	for _, forbidden := range []string{"http", "://", "soundcloud.com", "/", "ghostinnafurcoat"} {
		if strings.Contains(title, forbidden) {
			t.Errorf("derived title %q contains %q — that is URL structure, not a name", title, forbidden)
		}
	}
	if got[0].Permalink != permalink {
		t.Errorf("permalink was altered: %q", got[0].Permalink)
	}
}

// TestFirstLine: yt-dlp prints a deprecation warning before the ERROR line, and
// diagnostics can carry a signed CDN URL — a credential.
func TestFirstLine(t *testing.T) {
	const stderr = "Deprecated Feature: Support for Python version 3.10 has been deprecated\n" +
		"ERROR: unable to download https://cf-hls-media.sndcdn.com/x?Policy=abc&Signature=secret\n"

	got := firstLine(stderr)
	if !strings.HasPrefix(got, "ERROR:") {
		t.Errorf("firstLine() = %q, want the ERROR line, not the warning", got)
	}
	if strings.Contains(got, "sndcdn.com") || strings.Contains(got, "Signature") {
		t.Errorf("firstLine() leaked a signed URL: %q", got)
	}
}

// TestClassifyImpersonate: a rejected --impersonate is our own flag failing
// before any request leaves, and yt-dlp reports it as a Python traceback whose
// first line ("Traceback (most recent call last):") says nothing. Real stderr
// from yt-dlp 2026.07.04.
func TestClassifyImpersonate(t *testing.T) {
	const stderr = `Traceback (most recent call last):
  File "YoutubeDL.py", line 770, in __init__
yt_dlp.utils.YoutubeDLError: Impersonate target "smoke-invalid" is not available. Use --list-impersonate-targets to see available targets.
[PYI-5716:ERROR] Failed to execute script '__main__' due to unhandled exception!
`

	err := classify(context.Background(), errors.New("exit status 1"), stderr)
	if err == nil || !strings.Contains(err.Error(), "rejected --impersonate") {
		t.Fatalf("classify() = %v, want the --impersonate diagnosis", err)
	}
	if strings.Contains(err.Error(), "Traceback") {
		t.Errorf("classify() surfaced the traceback header instead of the exception: %v", err)
	}
	// It is not a SoundCloud problem: none of the three sentinels may claim it,
	// or the CLI hint sends the user to `yt-dlp -U` for a flag they set.
	for _, sentinel := range []error{core.ErrEgress, core.ErrRateLimited, core.ErrStaleExtractor} {
		if errors.Is(err, sentinel) {
			t.Errorf("classify() = %v, must not be %v", err, sentinel)
		}
	}
}

// TestClassifyTorTimeout: over Tor the common failure is not a refusal but a
// circuit that never completes. Real stderr from a live run through a bad exit.
func TestClassifyTorTimeout(t *testing.T) {
	const stderr = "ERROR: [soundcloud:search] Unable to download webpage: Failed to perform, " +
		"curl: (28) Connection timed out after 20003 milliseconds."

	err := classify(context.Background(), errors.New("exit status 1"), stderr)
	if !errors.Is(err, core.ErrEgress) {
		t.Fatalf("classify() = %v, want core.ErrEgress (a slow relay is not a stale extractor)", err)
	}
}

// TestParseLiveSearchShape pins the document SoundCloud actually returned
// through Tor on 2026-08-19 (trimmed to the fields we read). The thumbnails
// array is the trap it guards: entries carry nested "url" keys that must never
// be mistaken for the permalink.
func TestParseLiveSearchShape(t *testing.T) {
	const live = `{"id": "roshina", "title": "roshina", "_type": "playlist", "entries": [
	  {"_type": "url", "id": "2378008949", "title": "Shanghai Lullaby", "uploader": "Roshina",
	   "webpage_url": "https://soundcloud.com/ghostinnafurcoat/shanghai-lullaby",
	   "duration": 112.937,
	   "thumbnails": [{"url": "https://i1.sndcdn.com/artworks-mini.jpg"}]},
	  {"_type": "url", "id": "2377811302", "title": "pola woman", "uploader": "Roshina",
	   "webpage_url": "https://soundcloud.com/ghostinnafurcoat/pola-woman",
	   "duration": 160.285}
	]}`

	var res ytResult
	if err := json.Unmarshal([]byte(live), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tracks := toTracks(entriesFrom(res, "scsearch2:roshina"))
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(tracks))
	}
	if tracks[0].Permalink != "https://soundcloud.com/ghostinnafurcoat/shanghai-lullaby" {
		t.Errorf("permalink = %q — a nested thumbnail url must never win", tracks[0].Permalink)
	}
	// T8.1 needs both of these to fill its table; flat entries do carry them.
	if tracks[0].Uploader != "Roshina" || tracks[0].Duration != 112 {
		t.Errorf("uploader/duration = %q/%d, want Roshina/112", tracks[0].Uploader, tracks[0].Duration)
	}
}

// stubbed returns a resolver whose exec is scripted: errs[i] is what attempt
// i+1 returns, and a nil entry (or running past the end) succeeds. calls counts
// attempts, so a test can assert that a retry did or did not happen.
func stubbed(n *fakeNet, errs ...error) (r *YTDLP, calls *int) {
	r = New(n, "")
	count := 0
	r.invoke = func(_ context.Context, _ []string) ([]byte, error) {
		i := count
		count++
		if i < len(errs) && errs[i] != nil {
			return nil, errs[i]
		}
		return []byte(`{"_type":"playlist","entries":[
			{"id":"1","title":"t","webpage_url":"https://soundcloud.com/a/b"}]}`), nil
	}
	return r, &count
}

// TestListingsGetAListingBudget: Meta LISTS a page, and a user's likes arrive
// a page at a time, each a Tor round-trip. Under the 45s cap sized for one
// search request, a 1,700-track likes listing was killed mid-fetch on all
// three attempts (dev's report, 2026-10-03). Search and Stream keep 45s; a dead
// circuit is still caught by --socket-timeout either way.
func TestListingsGetAListingBudget(t *testing.T) {
	var budget time.Duration
	r := New(&fakeNet{}, "")
	r.invoke = func(ctx context.Context, _ []string) ([]byte, error) {
		d, _ := ctx.Deadline()
		budget = time.Until(d)
		return []byte(`{"_type":"playlist","entries":[{"id":"1","title":"t","webpage_url":"https://soundcloud.com/a/b"}]}`), nil
	}

	if _, err := r.Meta("https://soundcloud.com/someone/likes"); err != nil {
		t.Fatal(err)
	}
	if budget < listTimeout-time.Second {
		t.Errorf("Meta ran under a %s budget, want %s — a long likes list cannot finish", budget.Round(time.Second), listTimeout)
	}
	if _, err := r.Search("x", 5); err != nil {
		t.Fatal(err)
	}
	if budget > resolveTimeout {
		t.Errorf("Search ran under %s, want %s — the listing budget leaked into single requests", budget.Round(time.Second), resolveTimeout)
	}
}

// TestRetryPlan is the policy itself, checked against the two failure shapes
// measured live on 2026-08-19 (see run's comment). Pure function, so the
// 10-second distinction costs the test suite nothing.
func TestRetryPlan(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		elapsed      time.Duration
		retry, rotat bool
	}{
		{"blocked exit answers fast, still needs a new one",
			core.ErrRateLimited, 2 * time.Second, true, true},
		{"stalled circuit: rotate",
			core.ErrEgress, 17 * time.Second, true, true},
		{"circuit still warming up: retry the SAME one",
			core.ErrEgress, 5500 * time.Millisecond, true, false},
		{"stale extractor is identical everywhere",
			core.ErrStaleExtractor, time.Second, false, false},
		{"missing binary", ErrToolMissing, time.Second, false, false},
		{"unclassified", errors.New("who knows"), time.Second, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry, rotate := retryPlan(fmt.Errorf("%w: ctx", tc.err), tc.elapsed)
			if retry != tc.retry || rotate != tc.rotat {
				t.Errorf("retryPlan(%v, %s) = retry:%v rotate:%v, want retry:%v rotate:%v",
					tc.err, tc.elapsed, retry, rotate, tc.retry, tc.rotat)
			}
		})
	}
}

// TestRetryRotatesOnBlockedExit: a 403 is SoundCloud's answer, not the path's,
// so the second attempt must come from somewhere else.
func TestRetryRotatesOnBlockedExit(t *testing.T) {
	n := &fakeNet{}
	r, calls := stubbed(n, fmt.Errorf("%w: boom", core.ErrRateLimited))

	tracks, err := r.Search("x", 5)
	if err != nil {
		t.Fatalf("Search() = %v, want success on the retry", err)
	}
	if len(tracks) != 1 {
		t.Errorf("got %d tracks, want 1", len(tracks))
	}
	if *calls != 2 || n.rotates != 1 {
		t.Errorf("%d attempts / %d rotations, want 2 / 1", *calls, n.rotates)
	}
}

// TestRetryNoteIsOneShortLine: a retry used to print yt-dlp's whole error plus a
// second line, every attempt — on a slow Tor draw, a screenful before anything
// played (dev's report, 2026-10-03). The full error is still returned if every
// attempt fails; the note in between only says what is happening.
func TestRetryNoteIsOneShortLine(t *testing.T) {
	raw := "ERROR: [soundcloud] Unable to download webpage: Failed to perform, curl: (28) Connection timed out after 15017 milliseconds"
	for _, tc := range []struct {
		err    error
		rotate bool
	}{
		{fmt.Errorf("%w: %s", core.ErrEgress, raw), true},
		{fmt.Errorf("%w: %s", core.ErrEgress, raw), false},
		{fmt.Errorf("%w: %s", core.ErrRateLimited, raw), true},
	} {
		note := retryNote(tc.err, tc.rotate, 2, 3)
		if strings.Count(note, "\n") != 1 || strings.Contains(note, "curl") || !strings.Contains(note, "(2/3)") {
			t.Errorf("retryNote(%v, rotate=%v) = %q, want one short line ending in (2/3)", tc.err, tc.rotate, note)
		}
	}
}

// TestRetryKeepsCircuitOnFastFailure: the stub fails instantly, which is the
// warm-up shape. Rotating there would discard a circuit that was about to work.
func TestRetryKeepsCircuitOnFastFailure(t *testing.T) {
	n := &fakeNet{}
	r, calls := stubbed(n, fmt.Errorf("%w: boom", core.ErrEgress))

	if _, err := r.Search("x", 5); err != nil {
		t.Fatalf("Search() = %v, want success on the retry", err)
	}
	if *calls != 2 || n.rotates != 0 {
		t.Errorf("%d attempts / %d rotations, want 2 / 0", *calls, n.rotates)
	}
}

// TestNoRetryWhenRotationCannotHelp: a stale extractor is the same on every
// circuit. Retrying it only doubles the wait before the real message lands.
func TestNoRetryWhenRotationCannotHelp(t *testing.T) {
	for _, cause := range []error{core.ErrStaleExtractor, ErrToolMissing} {
		n := &fakeNet{}
		r, calls := stubbed(n, fmt.Errorf("%w: boom", cause), nil)

		if _, err := r.Search("x", 5); !errors.Is(err, cause) {
			t.Fatalf("Search() = %v, want %v surfaced immediately", err, cause)
		}
		if *calls != 1 || n.rotates != 0 {
			t.Errorf("%v: %d attempts / %d rotations, want 1 / 0", cause, *calls, n.rotates)
		}
	}
}

// TestRetryStopsAtMaxAttempts: a persistently bad path must surface the error,
// not loop forever.
func TestRetryStopsAtMaxAttempts(t *testing.T) {
	n := &fakeNet{}
	boom := fmt.Errorf("%w: boom", core.ErrEgress)
	r, calls := stubbed(n, boom, boom, boom, boom)

	if _, err := r.Search("x", 5); !errors.Is(err, core.ErrEgress) {
		t.Fatalf("Search() = %v, want core.ErrEgress", err)
	}
	if *calls != maxAttempts {
		t.Errorf("%d attempts, want %d", *calls, maxAttempts)
	}
}

// TestNoRetryWhenRotateFails: if the circuit could not be changed, the next
// attempt would be identical. Surface the ORIGINAL cause, not the rotation's —
// the user's problem is the blocked exit, not NEWNYM.
func TestNoRetryWhenRotateFails(t *testing.T) {
	n := &fakeNet{rotateErr: errors.New("no control port")}
	r, calls := stubbed(n, fmt.Errorf("%w: boom", core.ErrRateLimited), nil)

	if _, err := r.Search("x", 5); !errors.Is(err, core.ErrRateLimited) {
		t.Fatalf("Search() = %v, want the original core.ErrRateLimited", err)
	}
	if *calls != 1 || n.rotates != 1 {
		t.Errorf("%d attempts / %d rotations, want 1 / 1", *calls, n.rotates)
	}
}

// TestRetryPlanSkipsOurOwnMistakes: a rejected --impersonate is a flag WE
// passed, so no number of fresh circuits can fix it. Retrying would spend three
// Tor round-trips and ~50s to fail the same way.
func TestRetryPlanSkipsOurOwnMistakes(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: bad target", core.ErrImpersonate),
		fmt.Errorf("%w: client_id", core.ErrStaleExtractor),
	} {
		if retry, rotate := retryPlan(err, 20*time.Second); retry || rotate {
			t.Errorf("retryPlan(%v) = retry:%v rotate:%v, want both false", err, retry, rotate)
		}
	}
}

// TestUnclassifiedErrorLeadsWithTheErrorLine: the fallback branch used to join
// ALL of stderr with "; ", so an unrecognised failure led with whatever noise
// yt-dlp printed first — in the live 2026-08-24 case, a Python 3.10 deprecation
// warning that had nothing to do with the failure.
func TestUnclassifiedErrorLeadsWithTheErrorLine(t *testing.T) {
	const stderr = "Deprecated Feature: Support for Python version 3.10 has been deprecated. Please update to Python 3.11 or above\n" +
		"ERROR: [soundcloud] something we have never seen before"

	got := classify(context.Background(), errors.New("exit status 1"), stderr).Error()

	if strings.Contains(got, "Deprecated Feature") {
		t.Errorf("message leads with unrelated noise:\n  %s", got)
	}
	if !strings.Contains(got, "something we have never seen before") {
		t.Errorf("message dropped the actual error:\n  %s", got)
	}
}

// TestNotFoundIsNotRetried: a 404 is the same answer on every circuit, so
// spending two more Tor round-trips (~30s) to be told again is pure loss. This
// is the counterpart to the 403 case, which SHOULD rotate.
func TestNotFoundIsNotRetried(t *testing.T) {
	notFound := fmt.Errorf("%w: no such user", core.ErrNotFound)
	if retry, rotate := retryPlan(notFound, 20*time.Second); retry || rotate {
		t.Errorf("retryPlan(404) = retry:%v rotate:%v, want both false", retry, rotate)
	}
	// The contrast that makes the distinction meaningful.
	blocked := fmt.Errorf("%w: 403", core.ErrRateLimited)
	if retry, rotate := retryPlan(blocked, 20*time.Second); !retry || !rotate {
		t.Errorf("retryPlan(403) = retry:%v rotate:%v, want both true", retry, rotate)
	}
}

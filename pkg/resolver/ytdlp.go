// Package resolver is the ONE egress chokepoint: the only component that talks
// to SoundCloud, and it does so exclusively by shelling out to yt-dlp. Never
// reimplement api-v2 here — when SoundCloud breaks yt-dlp, the fix is
// `yt-dlp -U`, not a patch in this package.
package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"chainsmoke/pkg/core"
)

// ErrToolMissing means yt-dlp itself is absent. Kept distinct from a resolve
// failure so callers never tell the user to run `yt-dlp -U` for a binary that
// is not installed (TASKS.md T1.7).
var ErrToolMissing = errors.New("yt-dlp not found on PATH — install yt-dlp to resolve streams")

// Timeouts and attempt count are MEASURED against live Tor (2026-08-19), not
// picked round: over 6 fresh circuits, the one that worked completed in 22.9s
// while every failure stalled and was cut off at ~17s. So:
//
//   - socketTimeout 15s: no single socket operation in the successful run came
//     near it, and it turns a dead circuit into a 17s loss instead of a 30s one.
//   - maxAttempts 3: the failure is the exit you drew, so more dice beat more
//     patience. Worst case ~51s, better odds than the old 2 x 30s.
//   - resolveTimeout 45s: a backstop above yt-dlp's own timeout, with room for
//     a 23s success. For ONE request, if it fires, yt-dlp is wedged rather
//     than slow.
//   - listTimeout 3m: the same backstop for Meta, which LISTS a page. A
//     user's likes come from SoundCloud's API a page at a time, each a Tor
//     round-trip, so a 1,700-track listing can need far more than 45s while
//     bytes never stop moving. The 45s cap killed it mid-fetch on all three
//     attempts (dev's report, 2026-10-03). A dead circuit is still caught by
//     socketTimeout within ~15s; this only bounds a hung process.
const (
	resolveTimeout = 45 * time.Second
	listTimeout    = 3 * time.Minute
	socketTimeout  = "15" // seconds, passed to yt-dlp
	maxAttempts    = 3
)

// YTDLP shells out to yt-dlp, always passing the proxy from the live Net handle
// explicitly (research: socks5h env handling has had bugs — prefer --proxy)
// plus a browser-impersonation target to defend the TLS fingerprint.
type YTDLP struct {
	net         core.Net
	impersonate string // yt-dlp curl_cffi target; "" disables

	// invoke runs yt-dlp ONCE and classifies the result. It is a field, not a
	// method call, purely so the retry policy above it can be tested without a
	// yt-dlp, a network, or a Tor circuit — production always wires execOnce.
	invoke func(ctx context.Context, args []string) ([]byte, error)
}

// ytEntry is the slice of yt-dlp's JSON we consume. Everything not listed here
// is dropped rather than carried around: this struct is the boundary where
// SoundCloud's metadata stops.
type ytEntry struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	URL        string  `json:"url"`
	WebpageURL string  `json:"webpage_url"`
	Uploader   string  `json:"uploader"`
	Duration   float64 `json:"duration"` // float in yt-dlp's JSON, not int
}

// ytResult models BOTH shapes `-J` returns: a set/playlist/search comes back as
// an object with "entries", a single track as one flat object. The embedded
// ytEntry is what makes the single-track case parse without a second type
// (encoding/json promotes an anonymous struct's fields).
type ytResult struct {
	ytEntry
	Type    string    `json:"_type"` // "playlist" for sets AND search results
	Entries []ytEntry `json:"entries"`
}

// Compile-time proof the concrete type still matches the interface cmd/ is
// wired against; without it, signature drift surfaces at the wiring site.
var _ core.Resolver = (*YTDLP)(nil)

// New builds a resolver bound to a live Net handle. Taking Net here rather than
// reading ambient env is what makes the chokepoint structural: a resolver
// cannot be constructed without something that knows how to bring egress up.
func New(n core.Net, impersonate string) *YTDLP {
	r := &YTDLP{net: n, impersonate: impersonate}
	r.invoke = r.execOnce
	return r
}

// Meta resolves a page URL to its entries with `--flat-playlist -J` (T3.1).
// Flat is the point: one request for a whole set, while the per-track stream
// URLs — which expire — are fetched lazily by Stream, later.
//
// Permalinks come from `webpage_url`, falling back to `url` (bash lesson
// T1.11: flat entries carry only `url`, fully-resolved ones only `webpage_url`).
func (r *YTDLP) Meta(pageURL string) ([]core.Track, error) {
	pageURL = strings.TrimSpace(pageURL)
	if pageURL == "" {
		return nil, errors.New("empty URL")
	}

	out, err := r.runWithin(listTimeout, "--flat-playlist", "-J", "--", pageURL)
	if err != nil {
		return nil, err
	}

	var res ytResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("parse yt-dlp metadata: %w", err)
	}

	tracks := toTracks(entriesFrom(res, pageURL))
	if len(tracks) == 0 {
		return nil, errors.New("yt-dlp returned no playable entries for that URL")
	}
	return tracks, nil
}

// Stream resolves a single permalink to a fresh, direct audio URL via `yt-dlp
// -g` (T3.2). Called 1-2 tracks ahead of playback, never for a whole set:
// SoundCloud's HLS manifests expire quickly.
func (r *YTDLP) Stream(permalink string) (string, error) {
	permalink = strings.TrimSpace(permalink)
	if permalink == "" {
		return "", errors.New("empty permalink")
	}

	out, err := r.run(
		"-f", "bestaudio", "-g", "--no-playlist",
		"--extractor-args", "soundcloud:formats=hls_aac",
		"--", permalink)
	if err != nil {
		return "", err
	}
	for _, ln := range strings.Split(string(out), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln, nil // first stream URL yt-dlp prints
		}
	}
	return "", errors.New("yt-dlp returned no stream URL")
}

// Search runs SoundCloud's own search through yt-dlp's scsearch pseudo-URL
// (T3.3), returning results in the order SoundCloud ranked them.
func (r *YTDLP) Search(term string, limit int) ([]core.Track, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return nil, errors.New("empty search term")
	}
	// Clamp rather than reset: `--limit 100` silently yielding 10 results is a
	// worse answer than yielding the 50 we are willing to ask for.
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	// scsearchN:<term> is a pseudo-URL, so it goes after `--` like any other:
	// a term starting with "-" must never be read as a flag.
	out, err := r.run("--flat-playlist", "-J", "--", "scsearch"+strconv.Itoa(limit)+":"+term)
	if err != nil {
		return nil, err
	}

	var res ytResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("parse yt-dlp search output: %w", err)
	}

	tracks := toTracks(res.Entries)
	if len(tracks) == 0 {
		return nil, fmt.Errorf("no results for %q", term)
	}
	return tracks, nil
}

// -----------------------------------------------------------------------------
// The single exec path (T3.4)
// -----------------------------------------------------------------------------

// run is the retry policy, and the gate every yt-dlp invocation passes.
//
// Over Tor the dominant failure is the circuit, not the request, and it comes
// in two flavours that want OPPOSITE treatment. Measured live, 2026-08-19,
// paired across identical isolation credentials:
//
//   - Slow failure (~17s, yt-dlp's socket timeout, or a 403): the exit is
//     unusable or blocked. Raising the timeout to 30s did not save a single one
//     of these — it just cost twice as long. Rotate, then retry.
//   - Fast failure (~5s): Tor is still building the circuit. One such request
//     failed in 5.5s and the very next attempt on the SAME credentials
//     succeeded in 8.0s. Rotating here would throw away a circuit that was
//     about to work, and hand us a cold one with the same warm-up ahead of it.
//     Retry, do NOT rotate.
//
// Deliberately not retried at all: a stale extractor, a rejected --impersonate,
// a missing binary, a shut gate. No circuit fixes any of them, and retrying
// only delays the message that would.
func (r *YTDLP) run(args ...string) ([]byte, error) { return r.runWithin(resolveTimeout, args...) }

// runWithin is run with an explicit per-attempt budget (see listTimeout).
func (r *YTDLP) runWithin(budget time.Duration, args ...string) ([]byte, error) {
	if err := r.net.Assert(); err != nil { // T0.2: fail closed before any egress
		return nil, err
	}

	var err error
	for attempt := 1; ; attempt++ {
		// A budget per ATTEMPT, not per call: a retry that inherits the
		// remainder of a spent deadline is not a retry.
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		var out []byte
		started := time.Now()
		out, err = r.invoke(ctx, args)
		cancel()

		if err == nil {
			return out, nil
		}
		retry, rotate := retryPlan(err, time.Since(started))
		if !retry || attempt >= maxAttempts {
			return nil, err
		}

		// Say it out loud. A silent retry makes a stalled circuit look like a
		// slow program rather than a bad relay.
		fmt.Fprint(os.Stderr, retryNote(err, rotate, attempt+1, maxAttempts))

		if rotate {
			if rerr := r.net.Rotate(); rerr != nil {
				fmt.Fprintf(os.Stderr, "chainsmoke: rotate failed (%v) — not retrying\n", rerr)
				return nil, err
			}
		}
	}
}

// retryNote is the one line printed before a retry. It names what happened and
// what comes next, never yt-dlp's raw error: that used to fill the screen on
// every attempt, and if every attempt fails it is returned in full anyway.
func retryNote(err error, rotate bool, attempt, max int) string {
	what := "no answer through this Tor circuit"
	if errors.Is(err, core.ErrRateLimited) {
		what = "SoundCloud refused this Tor exit"
	}
	next := "retrying"
	if rotate {
		next = "trying a fresh circuit"
	}
	return fmt.Sprintf("chainsmoke: %s — %s (%d/%d)\n", what, next, attempt, max)
}

// circuitWarmup is the line between "Tor was still building" and "this exit is
// no good" — see run's measurements. A failure faster than this is worth one
// more try on the same circuit; a slower one has already proven the circuit.
const circuitWarmup = 10 * time.Second

// retryPlan decides what to do after a failed attempt: whether another one is
// worth making, and whether it needs a different circuit first. Pure so the
// policy can be tested without a network, a clock, or a Tor daemon.
func retryPlan(err error, elapsed time.Duration) (retry, rotate bool) {
	switch {
	case errors.Is(err, core.ErrRateLimited):
		// SoundCloud answered — fast — and said no. Only a new exit changes it.
		return true, true
	case errors.Is(err, core.ErrEgress):
		return true, elapsed >= circuitWarmup
	default:
		return false, false
	}
}

// execOnce is the single exec path (T3.4): proxy and impersonation are injected
// here, from the live Net handle, on every call — so a new verb cannot reach
// SoundCloud unproxied by forgetting a flag, and no ambient environment
// variable gets a vote in where the traffic goes.
func (r *YTDLP) execOnce(ctx context.Context, args []string) ([]byte, error) {
	// Checked here rather than in run() so the retry policy stays pure: no
	// binary, no network, no circuit needed to test it.
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return nil, ErrToolMissing
	}

	base := []string{
		"--ignore-config", // a user's yt-dlp config could override --proxy
		"--no-cache-dir",  // no resolved-URL crumbs under ~/.cache
		"--no-progress",
		"--socket-timeout", socketTimeout,
	}
	base = append(base, r.proxyArgs()...)
	if r.impersonate != "" {
		base = append(base, "--impersonate", r.impersonate)
	}

	// exec.CommandContext takes an explicit []string, so no value of permalink,
	// search term or impersonate can change the argument count — the
	// word-splitting bug class shellcheck flagged three times in script.sh
	// cannot exist here.
	cmd := exec.CommandContext(ctx, "yt-dlp", append(base, args...)...)
	cmd.Env = minimalEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return nil, classify(ctx, err, stderr.String())
	}
	if stdout.Len() == 0 {
		if why := firstLine(stderr.String()); why != "" {
			return nil, fmt.Errorf("yt-dlp exited 0 but produced no output: %s", why)
		}
		return nil, errors.New("yt-dlp exited 0 but produced no output")
	}
	return stdout.Bytes(), nil
}

// minimalEnv hands yt-dlp an explicit environment instead of inheriting ours.
// Ambient http_proxy/ALL_PROXY must never decide where a resolver call goes —
// the --proxy flag from the live Net handle does (T3.4) — and a fully inherited
// env is the easiest way to leak identity into a subprocess.
//
// The Windows list is not defensive padding: yt-dlp ships as a PyInstaller
// binary that unpacks itself at startup, and with only PATH+HOME it dies with
// "[PYI-8392:ERROR] Could not create temporary directory!" before it ever
// dials. Verified against yt-dlp 2026.07.04 on the dev target, 2026-08-19.
func minimalEnv() []string {
	keep := []string{"PATH", "HOME"}
	if runtime.GOOS == "windows" {
		keep = append(keep,
			"SystemRoot", "windir", "COMSPEC", "PATHEXT", // process + socket init
			"TEMP", "TMP", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "PROGRAMDATA")
	}
	env := make([]string, 0, len(keep))
	for _, k := range keep {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// proxyArgs returns the explicit --proxy flag, or nothing when egress is direct.
// ProxyURL yields human-readable sentinels ("<down>", "DIRECT (no proxy)") in
// the non-proxied cases, so test for a real URL rather than for emptiness —
// passing "--proxy <down>" to yt-dlp would be both broken and misleading.
func (r *YTDLP) proxyArgs() []string {
	if p := r.net.ProxyURL(); strings.Contains(p, "://") {
		return []string{"--proxy", p}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Error taxonomy (T3.5)
// -----------------------------------------------------------------------------

// classify maps yt-dlp's stderr onto core's sentinels so callers can react to
// the CAUSE rather than to a string. It is a plain function, not a method: it
// needs nothing from the receiver.
//
// Order is load-bearing. A 403 is unambiguous, so it goes first. Transport
// failures come next, because yt-dlp labels them with whichever extractor was
// running ("[soundcloud] Unable to download webpage: ... curl: (7)"), which
// reads exactly like a SoundCloud fault and is not one. Staleness is the
// residual — claimed only when nothing else explains the failure.
func classify(ctx context.Context, err error, stderr string) error {
	if err == nil {
		return nil
	}
	// A process killed by the context deadline reports a generic exit status,
	// so ask the context rather than the error. It is ErrEgress because that is
	// what it means over Tor: bytes were not moving. Classifying it that way is
	// also what lets run() rotate and retry instead of surrendering.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: yt-dlp timed out before SoundCloud finished answering", core.ErrEgress)
	}

	// Lowercase once: yt-dlp capitalizes ("ERROR: Unable to extract ...").
	low := strings.ToLower(stderr)
	switch {
	case contains(low, "http error 403", "http error 429", "too many requests"):
		return fmt.Errorf("%w: %s", core.ErrRateLimited, firstLine(stderr))

	// The other definitive answer from SoundCloud, and the opposite advice: 403
	// means "not from this exit, try another circuit", 404 means "this page does
	// not exist, no circuit will help". Found live 2026-08-24 on a mistyped
	// handle, where it fell through to the default and recommended `yt-dlp -U`.
	// Matched on the full status rather than a bare "not found", which is common
	// enough in other messages to over-match.
	case contains(low, "http error 404", "404: not found"):
		return fmt.Errorf("%w: %s", core.ErrNotFound, firstLine(stderr))

	// Both HTTP stacks yt-dlp can use are covered here, and they word it
	// differently: curl_cffi (the --impersonate path) says "curl: (7) Failed to
	// connect", urllib says "Unable to connect to proxy" / "Failed to establish
	// a new connection". WinError 10061 is matched by NUMBER because the OS
	// text is localized — on this dev box it comes back in Russian, and an
	// English-only matcher misses it entirely.
	case contains(low,
		"proxy", "socks", "connection refused", "tunnel connection failed",
		"failed to connect", "failed to establish a new connection",
		"curl: (7)", "winerror 10061",
		// A circuit that never completes is a transport failure too, and over
		// Tor it is the COMMON one — verified live: a bad exit hangs until
		// yt-dlp's own --socket-timeout fires ("curl: (28) Connection timed
		// out"). Without this the user is told to update yt-dlp because a
		// relay was slow.
		"timed out", "curl: (28)"):
		return fmt.Errorf("%w: %s", core.ErrEgress, firstLine(stderr))

	// A rejected --impersonate is OUR argument, not SoundCloud's fault: yt-dlp
	// refuses it before any request leaves — at argv-parse time for a malformed
	// target, at startup for one it cannot load. Reporting that as a
	// resolve failure would blame the site for a flag the user set.
	case contains(low, "impersonate target"):
		return fmt.Errorf("%w: %s", core.ErrImpersonate, firstLine(stderr))

	case contains(low, "unable to extract", "client_id", "unsupported url",
		"no video formats found"):
		return fmt.Errorf("%w: %s", core.ErrStaleExtractor, firstLine(stderr))
	}

	// firstLine here too, not the whole of stderr. Every classified branch above
	// already reduces to yt-dlp's ERROR line; this one used to dump everything
	// joined by "; ", so an unrecognised failure led with whatever noise yt-dlp
	// happened to print first. Observed live 2026-08-24:
	//
	//	yt-dlp: exit status 1: Deprecated Feature: Support for Python version
	//	3.10 has been deprecated. Please update to Python 3.11 or above;
	//	ERROR: [soundcloud:user] ktotyt: ... HTTP Error 404: Not Found
	//
	// The Python deprecation warning is not the failure and reads as though it
	// were. firstLine prefers the ERROR line and falls back to the first
	// non-empty one, so nothing useful is lost when there is no ERROR line.
	if text := firstLine(stderr); text != "" {
		return fmt.Errorf("yt-dlp: %w: %s", err, text)
	}
	return fmt.Errorf("yt-dlp: %w", err)
}

// contains reports whether s holds any of the needles. It exists only to keep
// the classify switch readable as a table of causes.
func contains(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// firstLine reduces yt-dlp's stderr to the one line worth showing, preferring
// its ERROR line over the deprecation warnings that precede it. It redacts on
// the way out: a diagnostic line can carry a signed CDN URL.
func firstLine(s string) string {
	var first, embedded string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		low := strings.ToLower(ln)
		switch {
		case strings.HasPrefix(low, "error"):
			return redactURLs(ln) // yt-dlp's own "ERROR: ..." line wins outright
		case embedded == "" && strings.Contains(low, "error"):
			// Not every failure is formatted that way: a bad --impersonate
			// comes back as a Python traceback whose first line is
			// "Traceback (most recent call last):" — useless — and whose
			// exception line is the one worth showing.
			embedded = ln
		}
		if first == "" {
			first = ln
		}
	}
	if embedded != "" {
		return redactURLs(embedded)
	}
	return redactURLs(first)
}

// redactURLs keeps yt-dlp's diagnostics useful without echoing a signed CDN URL
// into the terminal, scrollback, or shell history. Signed URLs are credentials;
// for a privacy tool, leaking one in an error message is a defect, not a nit.
func redactURLs(s string) string {
	out := make([]string, 0, 8)
	for _, ln := range strings.Split(strings.TrimSpace(s), "\n") {
		if i := strings.Index(ln, "http"); i >= 0 {
			ln = ln[:i] + "<url redacted>"
		}
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "; ")
}

// -----------------------------------------------------------------------------
// JSON -> core.Track
// -----------------------------------------------------------------------------

// permalink implements the T1.11 lesson: flat-playlist entries carry only
// "url", fully-resolved ones only "webpage_url". Prefer the latter — it is the
// canonical page URL, which is what Stream and the results cache want.
func (e ytEntry) permalink() string {
	if s := strings.TrimSpace(e.WebpageURL); s != "" {
		return s
	}
	return strings.TrimSpace(e.URL)
}

// entriesFrom picks the entry list out of a parsed -J document. It is split out
// of Meta so the shape handling — the part with the edge cases — is testable
// without a yt-dlp on PATH.
//
// "_type":"playlist" is authoritative even with no entries: an EMPTY set is not
// a single track, and treating the set's own URL as one would hand mpv a web
// page to play.
func entriesFrom(res ytResult, pageURL string) []ytEntry {
	if res.Type == "playlist" || len(res.Entries) > 0 {
		return res.Entries
	}
	e := res.ytEntry // a single track: the flat object IS the entry
	if e.permalink() == "" {
		e.WebpageURL = pageURL // yt-dlp echoed no URL back; the caller's stands
	}
	return []ytEntry{e}
}

// toTracks converts entries, skipping any without a permalink rather than
// failing the whole resolve: one unplayable entry in a 200-track set must not
// cost the user the other 199 (the skip-not-abort lesson, T1.14).
func toTracks(entries []ytEntry) []core.Track {
	tracks := make([]core.Track, 0, len(entries))
	for _, e := range entries {
		p := e.permalink()
		if p == "" {
			continue
		}
		title := strings.TrimSpace(e.Title)
		if title == "" {
			title = titleFromSlug(p)
		}
		if title == "" {
			title = "(untitled)"
		}
		tracks = append(tracks, core.Track{
			ID:        e.ID,
			Title:     title,
			Uploader:  e.Uploader,
			Duration:  int(e.Duration),
			Permalink: p,
		})
	}
	return tracks
}

// titleFromSlug recovers a display name from a permalink's last path segment
// when yt-dlp supplies no title.
//
// MEASURED 2026-08-24 through Tor, and the reason this exists: `--flat-playlist`
// returns three different shapes, and only two of them carry a title.
//
//	scsearch          title, uploader, duration
//	user pages        title only        (/likes, /tracks)
//	SETS              NEITHER           (_type "url_transparent": album, id, url)
//
// So without this, every row of `chainsmoke playlist <user>/<set>` read
// "(untitled)" — a numbered list nobody can choose from. The alternative,
// resolving each entry non-flat for its real title, costs one Tor round-trip
// per track and is exactly what T3.1 made Meta flat to avoid.
//
// This does NOT breach invariant #3. The slug is the track's own human-written
// title, lowercased and hyphenated by SoundCloud — it discloses precisely what
// the title column already discloses, and nothing about URL structure: no
// scheme, no host, no user segment, and nothing that can be pasted into a
// browser. The full permalink stays in the store, and a test asserts it.
func titleFromSlug(permalink string) string {
	rest := permalink
	if _, after, ok := strings.Cut(rest, "://"); ok {
		rest = after
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}

	// host / user / slug. Fewer segments than that means there is no track slug
	// to take, and grabbing whatever is last would print the UPLOADER's handle
	// as every row's title — confidently wrong, which is worse than "(untitled)".
	var segs []string
	for _, s := range strings.Split(rest, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) < 3 {
		return ""
	}

	slug := strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ").Replace(segs[len(segs)-1]))
	if slug == "" {
		return ""
	}
	// Capitalize the first letter only. Title-casing every word would invent
	// capitalization SoundCloud never told us about; leaving it entirely
	// lowercase reads as a fragment rather than a name.
	r := []rune(slug)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

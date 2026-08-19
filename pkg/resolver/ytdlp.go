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

	"chainsmoke/pkg/core"
)

// ErrToolMissing means yt-dlp itself is absent. Kept distinct from a resolve
// failure so callers never tell the user to run `yt-dlp -U` for a binary that
// is not installed (TASKS.md T1.7).
var ErrToolMissing = errors.New("yt-dlp not found on PATH — install yt-dlp to resolve streams")

// resolveTimeout caps EVERY invocation (T3.5: "timeouts on every exec"). One
// constant for all three verbs on purpose: they share a transport, and that
// transport is Tor, which is what makes the ceiling this generous.
const resolveTimeout = 60 * time.Second

// YTDLP shells out to yt-dlp, always passing the proxy from the live Net handle
// explicitly (research: socks5h env handling has had bugs — prefer --proxy)
// plus a browser-impersonation target to defend the TLS fingerprint.
type YTDLP struct {
	net         core.Net
	impersonate string // yt-dlp curl_cffi target; "" disables
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
	return &YTDLP{net: n, impersonate: impersonate}
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

	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	out, err := r.run(ctx, "--flat-playlist", "-J", "--", pageURL)
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

	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	out, err := r.run(ctx,
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

	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	// scsearchN:<term> is a pseudo-URL, so it goes after `--` like any other:
	// a term starting with "-" must never be read as a flag.
	out, err := r.run(ctx, "--flat-playlist", "-J", "--", "scsearch"+strconv.Itoa(limit)+":"+term)
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

// run is where every yt-dlp invocation in this program goes. Having exactly ONE
// of these is the whole of T3.4: proxy and impersonation are injected here, from
// the live Net handle, on every call — so a new verb cannot reach SoundCloud
// unproxied by forgetting a flag, and no ambient environment variable gets a
// vote in where the traffic goes.
func (r *YTDLP) run(ctx context.Context, args ...string) ([]byte, error) {
	if err := r.net.Assert(); err != nil { // T0.2: fail closed before any egress
		return nil, err
	}
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return nil, ErrToolMissing
	}

	base := []string{
		"--ignore-config", // a user's yt-dlp config could override --proxy
		"--no-cache-dir",  // no resolved-URL crumbs under ~/.cache
		"--no-progress",
		"--socket-timeout", "30",
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
	// so ask the context rather than the error.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("yt-dlp timed out after %s", resolveTimeout)
	}

	// Lowercase once: yt-dlp capitalizes ("ERROR: Unable to extract ...").
	low := strings.ToLower(stderr)
	switch {
	case contains(low, "http error 403", "http error 429", "too many requests"):
		return fmt.Errorf("%w: %s", core.ErrRateLimited, firstLine(stderr))

	// Both HTTP stacks yt-dlp can use are covered here, and they word it
	// differently: curl_cffi (the --impersonate path) says "curl: (7) Failed to
	// connect", urllib says "Unable to connect to proxy" / "Failed to establish
	// a new connection". WinError 10061 is matched by NUMBER because the OS
	// text is localized — on this dev box it comes back in Russian, and an
	// English-only matcher misses it entirely.
	case contains(low,
		"proxy", "socks", "connection refused", "tunnel connection failed",
		"failed to connect", "failed to establish a new connection",
		"curl: (7)", "winerror 10061"):
		return fmt.Errorf("%w: %s", core.ErrEgress, firstLine(stderr))

	// A rejected --impersonate is OUR argument, not SoundCloud's fault: yt-dlp
	// refuses it before any request leaves — at argv-parse time for a malformed
	// target, at startup for one it cannot load. Reporting that as a
	// resolve failure would blame the site for a flag the user set.
	case contains(low, "impersonate target"):
		return fmt.Errorf("yt-dlp rejected --impersonate: %s", firstLine(stderr))

	case contains(low, "unable to extract", "client_id", "unsupported url",
		"no video formats found"):
		return fmt.Errorf("%w: %s", core.ErrStaleExtractor, firstLine(stderr))
	}

	if text := redactURLs(stderr); text != "" {
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
			// PrintResults shows index + title only, and a blank row is
			// unusable. The permalink is NOT a fallback here — invariant #3
			// keeps it out of the terminal.
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

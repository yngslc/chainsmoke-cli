// Package resolver is the ONE egress chokepoint: the only component that talks
// to SoundCloud, and it does so exclusively by shelling out to yt-dlp.
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
// is not installed.
var ErrToolMissing = errors.New("yt-dlp not found on PATH — install yt-dlp to resolve streams")

const (
	resolveTimeout = 45 * time.Second
	listTimeout    = 3 * time.Minute
	socketTimeout  = "15"
	maxAttempts    = 3
)

// YTDLP shells out to yt-dlp, always passing the proxy from the live Net handle
// explicitly, plus a browser-impersonation target to defend the TLS fingerprint.
type YTDLP struct {
	net         core.Net
	impersonate string

	invoke func(ctx context.Context, args []string) ([]byte, error)
}

type ytEntry struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	URL        string  `json:"url"`
	WebpageURL string  `json:"webpage_url"`
	Uploader   string  `json:"uploader"`
	Duration   float64 `json:"duration"`
}

type ytResult struct {
	ytEntry
	Type    string    `json:"_type"`
	Entries []ytEntry `json:"entries"`
}

var _ core.Resolver = (*YTDLP)(nil)

// New builds a resolver bound to a live Net handle. Taking Net here rather than
// reading ambient env is what makes the chokepoint structural: a resolver
// cannot be constructed without something that knows how to bring egress up.
func New(n core.Net, impersonate string) *YTDLP {
	r := &YTDLP{net: n, impersonate: impersonate}
	r.invoke = r.execOnce
	return r
}

// Meta resolves a page URL to its entries with `--flat-playlist -J`.
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
// -g`.
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
			return ln, nil
		}
	}
	return "", errors.New("yt-dlp returned no stream URL")
}

// Search runs SoundCloud's own search through yt-dlp's scsearch pseudo-URL,
// returning results in the order SoundCloud ranked them.
func (r *YTDLP) Search(term string, limit int) ([]core.Track, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return nil, errors.New("empty search term")
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

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

func (r *YTDLP) run(args ...string) ([]byte, error) { return r.runWithin(resolveTimeout, args...) }

func (r *YTDLP) runWithin(budget time.Duration, args ...string) ([]byte, error) {
	if err := r.net.Assert(); err != nil {
		return nil, err
	}

	var err error
	for attempt := 1; ; attempt++ {
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

		fmt.Fprint(os.Stderr, retryNote(err, rotate, attempt+1, maxAttempts))

		if rotate {
			if rerr := r.net.Rotate(); rerr != nil {
				fmt.Fprintf(os.Stderr, "chainsmoke: rotate failed (%v) — not retrying\n", rerr)
				return nil, err
			}
		}
	}
}

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

const circuitWarmup = 10 * time.Second

func retryPlan(err error, elapsed time.Duration) (retry, rotate bool) {
	switch {
	case errors.Is(err, core.ErrRateLimited):
		return true, true
	case errors.Is(err, core.ErrEgress):
		return true, elapsed >= circuitWarmup
	default:
		return false, false
	}
}

func (r *YTDLP) execOnce(ctx context.Context, args []string) ([]byte, error) {
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return nil, ErrToolMissing
	}

	base := []string{
		"--ignore-config",
		"--no-cache-dir",
		"--no-progress",
		"--socket-timeout", socketTimeout,
	}
	base = append(base, r.proxyArgs()...)
	if r.impersonate != "" {
		base = append(base, "--impersonate", r.impersonate)
	}

	args, target, err := targetOnStdin(args)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "yt-dlp", append(base, args...)...)
	cmd.Env = minimalEnv()
	cmd.Stdin = strings.NewReader(target)

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	runErr := cmd.Run()
	errText := withoutStdinBanner(stderr.String())
	if runErr != nil {
		return nil, classify(ctx, runErr, errText)
	}
	if stdout.Len() == 0 {
		if why := firstLine(errText); why != "" {
			return nil, fmt.Errorf("yt-dlp exited 0 but produced no output: %s", why)
		}
		return nil, errors.New("yt-dlp exited 0 but produced no output")
	}
	return stdout.Bytes(), nil
}

func targetOnStdin(args []string) (rest []string, stdin string, err error) {
	n := len(args)
	if n < 2 || args[n-2] != "--" {
		return nil, "", errors.New("internal: yt-dlp call without a `-- <target>` tail")
	}
	target := args[n-1]
	if strings.ContainsAny(target, "\r\n") {
		return nil, "", errors.New("yt-dlp input spans more than one line")
	}
	return append(args[:n-2:n-2], "--batch-file", "-"), target + "\n", nil
}

const stdinBanner = "Reading URLs from STDIN"

func withoutStdinBanner(stderr string) string {
	var b strings.Builder
	for _, ln := range strings.SplitAfter(stderr, "\n") {
		if !strings.HasPrefix(ln, stdinBanner) {
			b.WriteString(ln)
		}
	}
	return b.String()
}

func minimalEnv() []string {
	keep := []string{"PATH", "HOME"}
	if runtime.GOOS == "windows" {
		keep = append(keep,
			"SystemRoot", "windir", "COMSPEC", "PATHEXT",
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

func (r *YTDLP) proxyArgs() []string {
	if p := r.net.ProxyURL(); strings.Contains(p, "://") {
		return []string{"--proxy", p}
	}
	return nil
}

func classify(ctx context.Context, err error, stderr string) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: yt-dlp timed out before SoundCloud finished answering", core.ErrEgress)
	}

	low := strings.ToLower(stderr)
	switch {
	case contains(low, "http error 403", "http error 429", "too many requests"):
		return fmt.Errorf("%w: %s", core.ErrRateLimited, firstLine(stderr))

	case contains(low, "http error 404", "404: not found"):
		return fmt.Errorf("%w: %s", core.ErrNotFound, firstLine(stderr))

	case contains(low,
		"proxy", "socks", "connection refused", "tunnel connection failed",
		"failed to connect", "failed to establish a new connection",
		"curl: (7)", "winerror 10061",
		"timed out", "curl: (28)"):
		return fmt.Errorf("%w: %s", core.ErrEgress, firstLine(stderr))

	case contains(low, "impersonate target"):
		return fmt.Errorf("%w: %s", core.ErrImpersonate, firstLine(stderr))

	case contains(low, "unable to extract", "client_id", "unsupported url",
		"no video formats found"):
		return fmt.Errorf("%w: %s", core.ErrStaleExtractor, firstLine(stderr))
	}

	if text := firstLine(stderr); text != "" {
		return fmt.Errorf("yt-dlp: %w: %s", err, text)
	}
	return fmt.Errorf("yt-dlp: %w", err)
}

func contains(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

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
			return redactURLs(ln)
		case embedded == "" && strings.Contains(low, "error"):
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

func (e ytEntry) permalink() string {
	if s := strings.TrimSpace(e.WebpageURL); s != "" {
		return s
	}
	return strings.TrimSpace(e.URL)
}

func entriesFrom(res ytResult, pageURL string) []ytEntry {
	if res.Type == "playlist" || len(res.Entries) > 0 {
		return res.Entries
	}
	e := res.ytEntry
	if e.permalink() == "" {
		e.WebpageURL = pageURL
	}
	return []ytEntry{e}
}

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

func titleFromSlug(permalink string) string {
	rest := permalink
	if _, after, ok := strings.Cut(rest, "://"); ok {
		rest = after
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}

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
	r := []rune(slug)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

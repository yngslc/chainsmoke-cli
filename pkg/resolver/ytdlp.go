// Package resolver is the ONE egress chokepoint: the only component that talks
// to SoundCloud, and it does so exclusively by shelling out to yt-dlp. Never
// reimplement api-v2 here — when SoundCloud breaks yt-dlp, the fix is
// `yt-dlp -U`, not a patch in this package.
package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"chainsmoke/pkg/core"
)

// ErrToolMissing means yt-dlp itself is absent. Kept distinct from a resolve
// failure so callers never tell the user to run `yt-dlp -U` for a binary that
// is not installed (TASKS.md T1.7).
var ErrToolMissing = errors.New("yt-dlp not found on PATH — install yt-dlp to resolve streams")

const resolveTimeout = 60 * time.Second

// YTDLP shells out to yt-dlp, always passing the proxy from the live Net handle
// explicitly (research: socks5h env handling has had bugs — prefer --proxy)
// plus a browser-impersonation target to defend the TLS fingerprint.
type YTDLP struct {
	net         core.Net
	impersonate string // yt-dlp curl_cffi target; "" disables
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

// Meta (raw): treats the input as one track and defers to Stream. Playlist/set
// expansion (yt-dlp --flat-playlist -J, TASKS.md T3.1) is not wired yet.
func (r *YTDLP) Meta(pageURL string) ([]core.Track, error) {
	if strings.TrimSpace(pageURL) == "" {
		return nil, errors.New("empty URL")
	}
	return []core.Track{{Permalink: pageURL, Title: pageURL}}, nil
}

// Stream resolves a permalink to a fresh, direct audio URL via `yt-dlp -g`.
func (r *YTDLP) Stream(permalink string) (string, error) {
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return "", ErrToolMissing
	}
	args := []string{"-f", "bestaudio", "-g", "--no-playlist",
		"--extractor-args", "soundcloud:formats=hls_aac"}
	args = append(args, r.proxyArgs()...)
	if r.impersonate != "" {
		args = append(args, "--impersonate", r.impersonate)
	}
	args = append(args, permalink)

	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	var out, errb bytes.Buffer
	// exec.CommandContext takes an explicit []string, so no value of permalink
	// or impersonate can change the argument count — the word-splitting bug
	// class shellcheck flagged three times in script.sh cannot exist here.
	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("yt-dlp timed out after %s resolving the stream", resolveTimeout)
		}
		return "", fmt.Errorf("yt-dlp: %v: %s", err, redactURLs(errb.String()))
	}
	for _, ln := range strings.Split(out.String(), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln, nil // first stream URL yt-dlp prints
		}
	}
	return "", errors.New("yt-dlp returned no stream URL")
}

// Search stays stubbed — the raw milestone covers play, not search (T3.3).
func (r *YTDLP) Search(string, int) ([]core.Track, error) {
	return nil, core.ErrNotImplemented
}

// proxyArgs returns the explicit --proxy flag, or nothing when egress is direct.
// ProxyURL yields human-readable sentinels ("<down>", "DIRECT (no proxy)") in
// the non-proxied cases, so test for a real URL rather than for emptiness.
func (r *YTDLP) proxyArgs() []string {
	if p := r.net.ProxyURL(); strings.Contains(p, "://") {
		return []string{"--proxy", p}
	}
	return nil
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

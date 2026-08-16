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

// isProxyURL reports whether s is a real proxy URL, as opposed to ProxyURL's
// human-readable "<down>" / "DIRECT (no proxy)" sentinels.
func isProxyURL(s string) bool { return strings.Contains(s, "://") }

// ytdlpResolver is the single egress chokepoint. It shells out to yt-dlp, always
// passing the proxy from the live Net handle explicitly (research: socks5h env
// handling has had bugs — prefer --proxy) plus a browser-impersonation target to
// defend the TLS fingerprint.
type ytdlpResolver struct {
	net         core.Net
	impersonate string // yt-dlp curl_cffi target; "" disables
}

// Meta (raw): treats the input as one track and defers to Stream. Playlist/set
// expansion (yt-dlp --flat-playlist -J, TASKS.md T3.1) is not wired yet.
func (r *ytdlpResolver) Meta(pageURL string) ([]core.Track, error) {
	if strings.TrimSpace(pageURL) == "" {
		return nil, errors.New("empty URL")
	}
	return []core.Track{{Permalink: pageURL, Title: pageURL}}, nil
}

// Stream resolves a permalink to a fresh, direct audio URL via `yt-dlp -g`.
func (r *ytdlpResolver) Stream(permalink string) (string, error) {
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return "", errors.New("yt-dlp not found on PATH — install yt-dlp to resolve streams")
	}
	args := []string{"-f", "bestaudio", "-g", "--no-playlist",
		"--extractor-args", "soundcloud:formats=hls_aac"}
	if p := r.net.ProxyURL(); isProxyURL(p) {
		args = append(args, "--proxy", p)
	}
	if r.impersonate != "" {
		args = append(args, "--impersonate", r.impersonate)
	}
	args = append(args, permalink)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("yt-dlp: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	for _, ln := range strings.Split(out.String(), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln, nil // first stream URL yt-dlp prints
		}
	}
	return "", errors.New("yt-dlp returned no stream URL")
}

// Search stays stubbed — the raw milestone covers play, not search.
func (r *ytdlpResolver) Search(string, int) ([]core.Track, error) { return nil, core.ErrNotImplemented }

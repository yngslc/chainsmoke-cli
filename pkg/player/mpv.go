// Package player owns mpv. Under decision T0.1(a) mpv is the ONLY long-lived
// process in the system: chainsmoke invocations are short-lived and own no
// state between runs, so anything that must outlive a command lives in mpv or
// in the runtime store — never in a goroutine.
package player

import (
	"errors"
	"os"
	"os/exec"

	"chainsmoke/pkg/core"
)

// ErrToolMissing means mpv itself is absent, as opposed to mpv failing.
var ErrToolMissing = errors.New("mpv not found on PATH — install mpv to play")

// MPV (raw): launches mpv per track in the foreground instead of running the
// persistent --idle daemon + JSON-IPC (TASKS.md Phase 5). No socket, no
// preload-ahead, no HEAD preflight — the simplest thing that plays audio.
type MPV struct {
	sock string // where the Phase-5 IPC daemon will listen
}

var _ core.Player = (*MPV)(nil)

// New builds a player. sock is the future IPC endpoint (core.SocketPath()):
// a unix socket on POSIX, a named pipe on Windows. Both speak the same
// newline-delimited JSON protocol; only the dial differs (TASKS.md T10.7).
func New(sock string) *MPV { return &MPV{sock: sock} }

// SocketPath exposes the configured endpoint so `audit` and the future daemon
// status verb can report it without reaching into the struct.
func (p *MPV) SocketPath() string { return p.sock }

// Ensure only has to confirm mpv exists, since raw play spawns it per track.
func (p *MPV) Ensure() error {
	if _, err := exec.LookPath("mpv"); err != nil {
		return ErrToolMissing
	}
	return nil
}

// Enqueue plays an already-resolved stream URL directly. mpv inherits the proxy
// env from Net.Up() and only ever sees the direct URL, never a SoundCloud page
// (the resolver already crossed the egress boundary). BLOCKS until playback ends.
//
// KNOWN GAP (T5.3): the name is aspirational. A real Enqueue does a proxied HEAD
// preflight and then `loadfile <url> append-play` over IPC, returning
// immediately. Until Phase 5 this is a foreground play, which is why callers
// must not run it concurrently — two of these fight over the same terminal.
//
// KNOWN GAP (invariant #3): the signed URL rides mpv's argv and is therefore
// visible in the OS process list. The IPC path fixes this by sending it over
// the socket instead. Do not treat this as acceptable past Phase 5.
func (p *MPV) Enqueue(streamURL string) error {
	cmd := exec.Command("mpv", "--no-video", "--quiet", streamURL)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func (p *MPV) Command(...any) (any, error) { return nil, core.ErrNotImplemented }
func (p *MPV) NowPlaying() (string, error) { return "", core.ErrNotImplemented }
func (p *MPV) Down() error                 { return nil }

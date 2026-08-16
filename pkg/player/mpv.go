package player

import (
	"chainsmoke/pkg/core"
	"errors"
	"os"
	"os/exec"
)

// mpvPlayer (raw): launches mpv per track in the foreground instead of running
// the persistent --idle daemon + JSON-IPC (TASKS.md Phase 5). No socket, no
// preload-ahead, no HEAD preflight — the simplest thing that plays audio.
type mpvPlayer struct{ sock string } // sock: where the future IPC daemon will live

// Ensure only has to confirm mpv exists, since raw play spawns it per track.
func (p *mpvPlayer) Ensure() error {
	if _, err := exec.LookPath("mpv"); err != nil {
		return errors.New("mpv not found on PATH — install mpv to play")
	}
	return nil
}

// Enqueue plays an already-resolved stream URL directly. mpv inherits the proxy
// env from Net.Up() and only ever sees the direct URL, never a SoundCloud page
// (the resolver already crossed the egress boundary). Blocks until playback ends.
func (p *mpvPlayer) Enqueue(streamURL string) error {
	cmd := exec.Command("mpv", "--no-video", "--quiet", streamURL)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func (p *mpvPlayer) Command(...any) (any, error) { return nil, core.ErrNotImplemented }
func (p *mpvPlayer) NowPlaying() (string, error) { return "", core.ErrNotImplemented }
func (p *mpvPlayer) Down() error                 { return nil }

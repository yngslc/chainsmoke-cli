// Package player owns mpv. mpv is the ONLY process that outlives a chainsmoke
// invocation.
package player

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"chainsmoke/pkg/core"
)

// ErrToolMissing means mpv itself is absent, as opposed to mpv failing.
var ErrToolMissing = errors.New("mpv not found on PATH — install mpv to play")

// MPV drives the persistent `mpv --idle` daemon over JSON-IPC.
type MPV struct {
	sock string
	net  core.Net
	ipc  *ipc
}

var _ core.Player = (*MPV)(nil)

// New builds a player. sock is the IPC endpoint (core.SocketPath()): a unix
// socket on POSIX, a named pipe on Windows.
func New(sock string, n core.Net) *MPV { return &MPV{sock: sock, net: n} }

// SocketPath exposes the configured endpoint so `audit` and the future daemon
// status verb can report it without reaching into the struct.
func (p *MPV) SocketPath() string { return p.sock }

var startTimeout = 10 * time.Second

const (
	spawnMarkKey = "chainsmoke-spawn"
	spawnGen     = "1"
)

// Ensure starts mpv's persistent IPC daemon if it is not already live.
func (p *MPV) Ensure() error {
	if _, err := exec.LookPath("mpv"); err != nil {
		return ErrToolMissing
	}
	live := p.live()
	if !live && len(daemons("--input-ipc-server="+p.sock)) > 0 {
		live = p.waitLive(startTimeout, nil)
	}
	switch {
	case live && p.ours():
		return nil
	case live:
		fmt.Fprintln(os.Stderr, "chainsmoke: replacing an mpv daemon this build did not start — its environment could route streams around the proxy")
		if err := p.Down(); err != nil {
			return err
		}
	default:
		if err := reap(p.sock, 0); err != nil {
			return err
		}
	}

	if runtime.GOOS != "windows" {
		if err := os.Remove(p.sock); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale mpv socket: %w", err)
		}
		if _, err := core.EnsureRuntimeDir(); err != nil {
			return err
		}
	}

	cmd := exec.Command("mpv",
		"--idle", "--no-video", "--no-config", "--no-ytdl",
		"--gapless-audio", "--cache-on-disk=no", "--volume=50",
		"--script-opts="+spawnMarkKey+"="+spawnGen,
		"--input-ipc-server="+p.sock)
	cmd.Env = withoutNoProxy(os.Environ())
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mpv: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	if p.waitLive(startTimeout, exited) {
		return nil
	}
	select {
	case <-exited:
		return fmt.Errorf("mpv exited while starting (%v) — run `mpv --idle --no-config` to see why", cmd.ProcessState)
	default:
		return fmt.Errorf("mpv IPC endpoint %s did not become responsive", p.sock)
	}
}

func (p *MPV) ours() bool {
	opts, err := p.Command("get_property", "script-opts")
	if err != nil {
		return false
	}
	if m, _ := opts.(map[string]any); m[spawnMarkKey] != spawnGen {
		return false
	}
	ytdl, err := p.Command("get_property", "ytdl")
	return err == nil && ytdl == false
}

func withoutNoProxy(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "no_proxy") {
			out = append(out, kv)
		}
	}
	return out
}

func (p *MPV) waitLive(d time.Duration, exited <-chan struct{}) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if p.live() {
			return true
		}
		select {
		case <-exited:
			return false
		default:
		}
	}
	return false
}

func (p *MPV) live() bool {
	rw, err := dial(p.sock)
	if err != nil {
		return false
	}
	defer rw.Close()
	_, err = newIPC(rw).send("get_property", "idle-active")
	return err == nil
}

const ipcTimeout = 10 * time.Second

type message struct {
	Data      any    `json:"data"`
	Error     string `json:"error"`
	RequestID int64  `json:"request_id"`
	Event     string `json:"event"`
}

type ipc struct {
	rw   io.ReadWriteCloser
	scan *bufio.Scanner
	id   int64
}

func newIPC(rw io.ReadWriteCloser) *ipc {
	sc := bufio.NewScanner(rw)
	sc.Buffer(make([]byte, 0, 8*1024), 1<<20)
	return &ipc{rw: rw, scan: sc}
}

func (c *ipc) send(args ...any) (any, error) {
	c.id++
	id := c.id

	line, err := json.Marshal(struct {
		Command   []any `json:"command"`
		RequestID int64 `json:"request_id"`
	}{args, id})
	if err != nil {
		return nil, fmt.Errorf("mpv: encode %v: %w", args, err)
	}
	if _, err := c.rw.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("mpv: write %s: %w", name(args), err)
	}

	setReadDeadline(c.rw, ipcTimeout)
	for c.scan.Scan() {
		var m message
		if err := json.Unmarshal(c.scan.Bytes(), &m); err != nil {
			continue
		}
		if m.Event != "" || m.RequestID != id {
			continue
		}
		if m.Error != "success" {
			return nil, fmt.Errorf("mpv: %s: %s", name(args), m.Error)
		}
		return m.Data, nil
	}
	if err := c.scan.Err(); err != nil {
		return nil, fmt.Errorf("mpv: read %s: %w", name(args), err)
	}
	return nil, fmt.Errorf("mpv: connection closed before answering %s", name(args))
}

func (c *ipc) Close() error { return c.rw.Close() }

func name(args []any) string {
	if len(args) == 0 {
		return "(empty command)"
	}
	return fmt.Sprint(args[0])
}

func dial(sock string) (io.ReadWriteCloser, error) {
	if runtime.GOOS == "windows" {
		return os.OpenFile(sock, os.O_RDWR, 0)
	}
	return net.Dial("unix", sock)
}

func setReadDeadline(rw io.ReadWriteCloser, d time.Duration) {
	type deadliner interface{ SetReadDeadline(time.Time) error }
	if c, ok := rw.(deadliner); ok {
		_ = c.SetReadDeadline(time.Now().Add(d))
	}
}

const preflightTimeout = 10 * time.Second

// Enqueue hands an already-resolved stream URL to the daemon: preflight, then
// `loadfile <url> append-play` over the IPC socket, then return.
func (p *MPV) Enqueue(streamURL, title string) error {
	if err := p.net.Assert(); err != nil {
		return err
	}
	if proxy := p.net.ProxyURL(); strings.Contains(proxy, "://") && !strings.HasPrefix(proxy, "http://") {
		return fmt.Errorf("refusing to queue: mpv cannot use a %s:// proxy and would fetch the stream directly",
			proxy[:strings.Index(proxy, "://")])
	}
	if err := p.preflight(streamURL); err != nil {
		return fmt.Errorf("%w: %v", core.ErrDeadStream, err)
	}
	opts := map[string]any{}
	if title != "" {
		opts["force-media-title"] = title
	}
	if proxy := p.net.ProxyURL(); strings.Contains(proxy, "://") {
		opts["stream-lavf-o"] = "http_proxy=" + proxy
	}
	args := []any{"loadfile", streamURL, "append-play"}
	if len(opts) > 0 {
		args = append(args, -1, opts)
	}
	_, err := p.Command(args...)
	return err
}

func (p *MPV) preflight(streamURL string) error {
	client, err := p.httpClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("preflight failed: %s", redactURL(err.Error(), streamURL))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("CDN answered %s", resp.Status)
	}
	return nil
}

func (p *MPV) httpClient() (*http.Client, error) {
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true}

	if proxy := p.net.ProxyURL(); strings.Contains(proxy, "://") {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("egress proxy %q is unusable: %w", proxy, err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr, Timeout: preflightTimeout}, nil
}

func redactURL(msg, streamURL string) string {
	if streamURL == "" {
		return msg
	}
	return strings.ReplaceAll(msg, streamURL, "<stream url redacted>")
}

// Command is the raw IPC entry point every transport verb goes through.
func (p *MPV) Command(args ...any) (any, error) {
	if p.ipc == nil {
		rw, err := dial(p.sock)
		if err != nil {
			return nil, fmt.Errorf("mpv IPC %s unreachable (is the daemon up?): %w", p.sock, err)
		}
		p.ipc = newIPC(rw)
	}
	return p.ipc.send(args...)
}

// NowPlaying reports the current track's title. An idle daemon is a state, not
// a failure: mpv answers "property unavailable" when the playlist is empty, and
// `np` should say so plainly rather than surface an IPC error.
func (p *MPV) NowPlaying() (string, error) {
	title, err := p.Command("get_property", "media-title")
	if err != nil {
		if strings.Contains(err.Error(), "unavailable") {
			return "nothing playing", nil
		}
		return "", err
	}
	s, ok := title.(string)
	if !ok {
		return "", fmt.Errorf("mpv: media-title was %T, want a string", title)
	}
	if strings.Contains(s, "://") || strings.Contains(s, "Signature=") {
		return "<playing: title unavailable, URL withheld>", nil
	}
	return core.Clean(s), nil
}

// Down stops the daemon and then closes our end of the pipe.
func (p *MPV) Down() error {
	if p.ipc == nil {
		if rw, err := dial(p.sock); err == nil {
			p.ipc = newIPC(rw)
		}
	}
	var err error
	grace := time.Duration(0)
	if p.ipc != nil {
		_, _ = p.Command("quit")
		err = p.ipc.Close()
		p.ipc = nil
		grace = quitGrace
	}
	return errors.Join(err, reap(p.sock, grace))
}

const quitGrace = 2 * time.Second

func reap(sock string, grace time.Duration) error {
	arg := "--input-ipc-server=" + sock
	strays := waitGone(arg, grace)
	if len(strays) == 0 {
		return nil
	}
	for _, pid := range strays {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
	}
	if left := waitGone(arg, time.Second); len(left) > 0 {
		return fmt.Errorf("mpv still running after kill (pid %v)", left)
	}
	fmt.Fprintf(os.Stderr, "chainsmoke: killed %d stray mpv process(es) that quit could not reach\n", len(strays))
	return nil
}

func waitGone(arg string, d time.Duration) []int {
	pids := daemons(arg)
	for deadline := time.Now().Add(d); len(pids) > 0 && time.Now().Before(deadline); pids = daemons(arg) {
		time.Sleep(50 * time.Millisecond)
	}
	return pids
}

// Package player owns mpv. Under decision T0.1(a) mpv is the ONLY long-lived
// process in the system: chainsmoke invocations are short-lived and own no
// state between runs, so anything that must outlive a command lives in mpv or
// in the runtime store — never in a goroutine.
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

// MPV drives the persistent `mpv --idle` daemon over JSON-IPC (TASKS.md Phase
// 5). The daemon and the IPC client are real; Enqueue still plays in the
// foreground, which is the remaining T5.3 gap documented on that method.
type MPV struct {
	sock string   // where the IPC daemon listens
	net  core.Net // egress handle: the preflight must not bypass the chokepoint
	ipc  *ipc     // lazily dialled; nil until the first Command
}

var _ core.Player = (*MPV)(nil)

// New builds a player. sock is the IPC endpoint (core.SocketPath()): a unix
// socket on POSIX, a named pipe on Windows. Both speak the same
// newline-delimited JSON protocol; only the dial differs (TASKS.md T10.7).
//
// n is taken for the same reason the resolver takes it: the preflight in
// Enqueue is a network call, so it has to pass the same gate everything else
// does. A player that could reach the CDN without a Net handle would be a
// second egress path (T0.2).
func New(sock string, n core.Net) *MPV { return &MPV{sock: sock, net: n} }

// SocketPath exposes the configured endpoint so `audit` and the future daemon
// status verb can report it without reaching into the struct.
func (p *MPV) SocketPath() string { return p.sock }

// Ensure starts mpv's persistent IPC daemon if it is not already live.
func (p *MPV) Ensure() error {
	if _, err := exec.LookPath("mpv"); err != nil {
		return ErrToolMissing
	}
	if p.live() {
		return nil
	}

	if runtime.GOOS != "windows" {
		if err := os.Remove(p.sock); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale mpv socket: %w", err)
		}
		if _, err := core.EnsureRuntimeDir(); err != nil {
			return err
		}
	}

	cmd := exec.Command("mpv", "--idle", "--no-video", "--gapless-audio", "--cache-on-disk=no", "--input-ipc-server="+p.sock)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mpv: %w", err)
	}
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.live() {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("mpv IPC endpoint %s did not become responsive", p.sock)
}

// live is the T1.15 liveness probe: a full request/reply round-trip, not a
// successful connect. A stale unix socket file left by a crashed mpv accepts a
// connection and then answers nothing, so "dial succeeded" is not evidence that
// anything is listening — which is the bash prototype's bug this replaces.
func (p *MPV) live() bool {
	rw, err := dial(p.sock)
	if err != nil {
		return false
	}
	defer rw.Close()
	_, err = newIPC(rw).send("get_property", "idle-active")
	return err == nil
}

// -----------------------------------------------------------------------------
// JSON-IPC client (TASKS.md T5.2)
// -----------------------------------------------------------------------------

// ipcTimeout bounds one request/reply round-trip. mpv answers commands
// immediately, so anything slower than this is a wedged or dead daemon, not a
// slow one. See setReadDeadline for where this is (and is not) enforceable.
const ipcTimeout = 10 * time.Second

// message is one line of mpv's protocol, in either direction. Replies and
// events arrive on the SAME stream and are told apart by shape: a reply carries
// the request_id we sent, an event carries "event" and no request_id at all.
//
// Verified against mpv 0.41 (2026-08-19):
//
//	{"data":"mpv v0.41.0","request_id":101,"error":"success"}
//	{"request_id":103,"error":"invalid parameter"}
//	{"event":"property-change","id":1,"name":"volume","data":77.000000}
//
// Note the second line: a matching request_id is NOT success. "error" is a
// string, always present on replies, and must be checked separately.
type message struct {
	Data      any    `json:"data"`
	Error     string `json:"error"`
	RequestID int64  `json:"request_id"`
	Event     string `json:"event"`
}

// ipc is one connection to mpv. It is not safe for concurrent use, and does not
// need to be: under T0.1(a) a chainsmoke process issues a handful of commands
// in sequence and exits. The Phase-10 TUI, which must read events while idle,
// needs a reader goroutine and a pending-request map instead — that is a
// different client behind this same seam, not a change to this one.
type ipc struct {
	rw   io.ReadWriteCloser
	scan *bufio.Scanner
	id   int64
}

func newIPC(rw io.ReadWriteCloser) *ipc {
	sc := bufio.NewScanner(rw)
	// bufio's 64K default is not enough for every reply: `playlist` on a long
	// queue is a single line of JSON that will exceed it, and the failure mode
	// is a silent truncated scan rather than an error.
	sc.Buffer(make([]byte, 0, 8*1024), 1<<20)
	return &ipc{rw: rw, scan: sc}
}

// send writes one command and returns ITS reply, discarding everything else on
// the wire. That discarding is the whole of T5.2: once anything calls
// observe_property, mpv pushes events at arbitrary times — including between a
// command and its answer — so a client that reads one line and calls it the
// reply will eventually hand its caller a property-change event instead.
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
			continue // not JSON we understand; it is certainly not our reply
		}
		if m.Event != "" || m.RequestID != id {
			continue // an async event, or a reply to something else
		}
		if m.Error != "success" {
			return nil, fmt.Errorf("mpv: %s: %s", name(args), m.Error)
		}
		return m.Data, nil // nil for commands that return nothing
	}
	if err := c.scan.Err(); err != nil {
		return nil, fmt.Errorf("mpv: read %s: %w", name(args), err)
	}
	return nil, fmt.Errorf("mpv: connection closed before answering %s", name(args))
}

func (c *ipc) Close() error { return c.rw.Close() }

// name renders the command for an error message. The arguments are dropped on
// purpose: `loadfile <signed url>` must not put a credential into an error
// string that ends up in a terminal or a log (invariant #3).
func name(args []any) string {
	if len(args) == 0 {
		return "(empty command)"
	}
	return fmt.Sprint(args[0])
}

// dial opens the platform's endpoint. Same protocol on both, different dial —
// which is the entirety of what T10.7 asks the IPC layer to abstract.
func dial(sock string) (io.ReadWriteCloser, error) {
	if runtime.GOOS == "windows" {
		// A named pipe is openable as a file, so the stdlib is enough to TALK
		// to mpv here — verified against mpv 0.41. What it cannot do is
		// interrupt a blocked read: Close() from another goroutine does not
		// unblock one, so a wedged daemon hangs the caller. Cancellable reads
		// need overlapped I/O (go-winio), which the TUI will want and the
		// one-shot verbs do not (T10.7).
		return os.OpenFile(sock, os.O_RDWR, 0)
	}
	return net.Dial("unix", sock)
}

// setReadDeadline applies the timeout where the platform supports one. Both
// net.Conn and *os.File expose the method, but a Windows named pipe opened as a
// file is not pollable and returns ErrNoDeadline — so on that target this is a
// no-op and the timeout is aspirational. Ignoring the error is deliberate: a
// missing deadline must not fail a command that would otherwise work.
func setReadDeadline(rw io.ReadWriteCloser, d time.Duration) {
	type deadliner interface{ SetReadDeadline(time.Time) error }
	if c, ok := rw.(deadliner); ok {
		_ = c.SetReadDeadline(time.Now().Add(d))
	}
}

// -----------------------------------------------------------------------------
// Playback
// -----------------------------------------------------------------------------

// preflightTimeout bounds the HEAD. A signed CDN URL either answers promptly or
// is gone; waiting longer only delays a skip the caller has to make anyway.
const preflightTimeout = 10 * time.Second

// Enqueue plays an already-resolved stream URL. The HEAD preflight in front of
// it is the first half of T5.3: SoundCloud's signed URLs expire, and handing
// mpv a dead one makes it sit on a connection that will never produce audio.
// Failing here with core.ErrDeadStream lets the caller skip the track instead
// of aborting the queue (T1.14, skip-not-abort).
//
// KNOWN GAP (T5.3): the name is still aspirational. A real Enqueue sends
// `loadfile <url> append-play` over the IPC socket and returns immediately;
// this one plays in the FOREGROUND and blocks until the track ends, so callers
// must not run two of them — they fight over the same terminal.
//
// KNOWN GAP (invariant #3): the signed URL rides mpv's argv and is therefore
// visible in the OS process list. The IPC path fixes this by sending it over
// the socket instead. Do not treat this as acceptable past Phase 5.
func (p *MPV) Enqueue(streamURL string) error {
	if err := p.net.Assert(); err != nil { // T0.2: fail closed before any egress
		return err
	}
	if err := p.preflight(streamURL); err != nil {
		return fmt.Errorf("%w: %v", core.ErrDeadStream, err)
	}
	cmd := exec.Command("mpv", "--no-video", "--quiet", streamURL)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// preflight asks the CDN whether the stream is still there, through the SAME
// proxy the resolver used. Taking the proxy from the live Net handle rather
// than the environment is what keeps this from becoming a second egress path:
// a preflight that leaked around the bridge would deanonymize precisely the
// request the tool exists to protect (T3.4, applied to the player).
func (p *MPV) preflight(streamURL string) error {
	client, err := p.httpClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, streamURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL is a credential, and err embeds it. Report the shape of the
		// failure, never the target (invariant #3).
		return fmt.Errorf("HEAD failed: %s", redactURL(err.Error(), streamURL))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("CDN answered %s", resp.Status)
	}
	return nil
}

// httpClient builds a client pinned to the live egress path. Proxy is set
// explicitly and never left as ProxyFromEnvironment: ambient http_proxy must
// not get a vote in where a chainsmoke request goes.
func (p *MPV) httpClient() (*http.Client, error) {
	tr := &http.Transport{Proxy: nil}

	// ProxyURL returns prose ("<down>", "DIRECT (no proxy)") when there is no
	// proxy, so test for a real URL rather than for emptiness — the same rule
	// resolver.proxyArgs follows. No proxy here means --allow-direct, which
	// only Assert can permit, and it already has.
	if proxy := p.net.ProxyURL(); strings.Contains(proxy, "://") {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("egress proxy %q is unusable: %w", proxy, err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr, Timeout: preflightTimeout}, nil
}

// redactURL keeps a transport error readable without echoing the signed URL it
// almost always quotes.
func redactURL(msg, streamURL string) string {
	if streamURL == "" {
		return msg
	}
	return strings.ReplaceAll(msg, streamURL, "<stream url redacted>")
}

// -----------------------------------------------------------------------------
// Transport controls (TASKS.md T5.5)
// -----------------------------------------------------------------------------

// Command is the raw IPC entry point every transport verb goes through. The
// connection is dialled on first use and reused: one process sends several
// commands, and a fresh dial per command would be extra syscalls and a new
// mpv-side client for no benefit.
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
	return s, nil
}

// Down closes OUR end of the IPC connection. It does not stop mpv: killing the
// daemon is the `stop` verb's job (T5.5), and core.App.Purge relies on that
// distinction — on POSIX the socket file lives in the directory purge removes,
// so a still-running mpv there is the T1.13 hazard, still open.
func (p *MPV) Down() error {
	if p.ipc == nil {
		return nil
	}
	err := p.ipc.Close()
	p.ipc = nil
	return err
}

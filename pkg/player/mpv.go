// Package player owns mpv. Under decision T0.1(a) mpv is the ONLY process that
// outlives a chainsmoke invocation. A `play` stays up for its session, since it
// carries the bridge mpv streams through, but no invocation owns state between
// runs: anything that must outlive a command lives in mpv or in the runtime
// store — never in a goroutine.
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
// 5). Enqueue queues a track with `loadfile` and returns at once (T5.3). The
// stream still rides this process's bridge, which is why `play` holds a session
// while it plays (cmd/chainsmoke/session.go).
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

// startTimeout is how long Ensure waits for a daemon to answer. It was 3s:
// the first `daemon start` of 2026-09-28 hit that limit while mpv went on to
// answer about a second later (a warm start is ~90ms). Ensure also stops
// waiting the moment the mpv it started exits, so only a hung one costs this.
var startTimeout = 10 * time.Second

// The spawn mark: Ensure puts it in mpv's argv and reads it back over IPC
// before trusting a live daemon (see ours). Bump spawnGen whenever the spawn's
// safety properties change, so daemons from older builds get replaced.
const (
	spawnMarkKey = "chainsmoke-spawn"
	spawnGen     = "1"
)

// Ensure starts mpv's persistent IPC daemon if it is not already live.
func (p *MPV) Ensure() error {
	if _, err := exec.LookPath("mpv"); err != nil {
		return ErrToolMissing
	}
	// An mpv already started on this endpoint that does not answer yet is
	// most likely still booting: one an earlier Ensure gave up on, or one
	// another process just spawned. A second would never get the pipe and
	// would idle as a stray (see Down), so this one gets the window first.
	live := p.live()
	if !live && len(daemons("--input-ipc-server="+p.sock)) > 0 {
		live = p.waitLive(startTimeout, nil)
	}
	switch {
	case live && p.ours():
		return nil
	case live:
		// Spawned by an older build (the dist/ binaries) or by hand: its
		// environment is unknown, and an inherited no_proxy sent a reused
		// daemon's streams around the proxy (measured 2026-10-03). Replace it.
		fmt.Fprintln(os.Stderr, "chainsmoke: replacing an mpv daemon this build did not start — its environment could route streams around the proxy")
		if err := p.Down(); err != nil {
			return err
		}
	default:
		// Still silent after the window: a stray or wedged, not slow, and it
		// must not make every later call wait. Clear it and start fresh.
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

	// Every flag here is load-bearing; all were checked against mpv 0.41 rather
	// than assumed:
	//   --idle              stay alive with an empty playlist (this is the daemon)
	//   --no-video          audio-only; never opens a window
	//   --no-config         the user's mpv.conf must not override any of the
	//                       above, the same reason the resolver passes
	//                       --ignore-config to yt-dlp
	//   --no-ytdl           mpv must NEVER resolve a URL itself. Its ytdl hook
	//                       fires for a page URL, and also when a direct media
	//                       URL FAILS to load (on_load_fail, measured
	//                       2026-10-01): either way it spawns yt-dlp outside the
	//                       chokepoint, with the URL in argv and no proxy.
	//   --gapless-audio     this is a music queue, not a video player
	//   --cache-on-disk=no  already the default; stated because "stream bytes
	//                       never touch the disk" is a promise, not a preference
	//   --script-opts=...   the spawn mark, read back by ours() before reuse.
	//                       In argv rather than set over IPC afterwards, so a
	//                       daemon still booting when Ensure gave up has it too.
	//   --volume=50         a new daemon starts at half volume, not mpv's 100
	//                       (dev's call, 2026-10-03). A reused daemon keeps
	//                       whatever `vol` last set.
	cmd := exec.Command("mpv",
		"--idle", "--no-video", "--no-config", "--no-ytdl",
		"--gapless-audio", "--cache-on-disk=no", "--volume=50",
		"--script-opts="+spawnMarkKey+"="+spawnGen,
		"--input-ipc-server="+p.sock)
	// The user's environment, minus no_proxy: libavformat honours it even for
	// the proxy each loadfile carries, so no_proxy=* (or a pattern matching the
	// CDN) fetched the signed URL directly while the preflight tunnelled
	// (measured 2026-10-01). Everything else stays — audio needs it, and
	// stripping the environment to PATH was the e2f2e01 leak.
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

// ours reports whether the live daemon is one this build spawned: it carries
// the spawn mark and has the ytdl hook off (both measured readable over IPC on
// mpv 0.41). Anything else has an environment Ensure cannot see.
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

// withoutNoProxy returns env without no_proxy in either spelling, which on
// Windows are one variable.
func withoutNoProxy(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "no_proxy") {
			out = append(out, kv)
		}
	}
	return out
}

// waitLive polls the liveness probe until it answers, d runs out, or exited
// closes (the process it waits for is gone; nil means not watched).
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

// Enqueue hands an already-resolved stream URL to the daemon (T5.3): preflight,
// then `loadfile <url> append-play` over the IPC socket, then return. It does
// NOT block — the track outlives this process, which is the whole of T0.1(a).
//
// The preflight is load-bearing, not belt-and-braces. Measured against mpv 0.41:
// `loadfile` answers `{"error":"success"}` for a URL pointing at a dead port and
// only fails LATER, as an async event with no request_id. So the HEAD is the one
// synchronous signal chainsmoke can act on, and without it
// skip-not-abort (T1.14) would have nothing to decide from — every track would
// "succeed" and the queue would silently fill with dead entries.
//
// Invariant #3 is improved, not solved: the signed URL no longer rides mpv's
// argv (the OS process list), but it does live in mpv's playlist, readable by
// anything that can reach the IPC socket. That is what the 0700 runtime dir is
// protecting.
func (p *MPV) Enqueue(streamURL, title string) error {
	if err := p.net.Assert(); err != nil { // T0.2: fail closed before any egress
		return err
	}
	// libavformat uses a proxy only when the value starts with exactly
	// "http://" and silently fetches DIRECT otherwise — HTTP://, https://,
	// socks5:// all go around it (mpv manual; measured 2026-10-01). Refuse
	// before the preflight, which would tunnel while the stream did not. The
	// error names the scheme only: a --proxy URL can carry credentials.
	if proxy := p.net.ProxyURL(); strings.Contains(proxy, "://") && !strings.HasPrefix(proxy, "http://") {
		return fmt.Errorf("refusing to queue: mpv cannot use a %s:// proxy and would fetch the stream directly",
			proxy[:strings.Index(proxy, "://")])
	}
	if err := p.preflight(streamURL); err != nil {
		return fmt.Errorf("%w: %v", core.ErrDeadStream, err)
	}
	// mpv falls back to the FILENAME for media-title, and an HLS stream's
	// "filename" is the whole signed URL — so without force-media-title, `np`
	// prints Policy and Signature straight to the terminal (measured 2026-08-22,
	// invariant #3). The title is the fix, not a nicety.
	opts := map[string]any{}
	if title != "" {
		opts["force-media-title"] = title
	}
	// The proxy rides the loadfile, not the daemon's environment. A daemon
	// outlives the run that spawned it and the bridge picks a new ephemeral
	// port every run, so by run 2 an inherited http_proxy names a dead port —
	// and `daemon start` never brings egress up at all. Per-load is the only
	// binding that is correct for every track (measured 2026-09-09: the option
	// propagates to HLS child segments). ProxyURL yields prose ("<down>",
	// "DIRECT (no proxy)") when there is no proxy; only a real URL is passed,
	// the same rule httpClient and resolver.proxyArgs follow.
	if proxy := p.net.ProxyURL(); strings.Contains(proxy, "://") {
		opts["stream-lavf-o"] = "http_proxy=" + proxy
	}
	// loadfile is `<url> [flags [index [options]]]` — one options map, in the
	// fourth slot, or mpv rejects the command.
	args := []any{"loadfile", streamURL, "append-play"}
	if len(opts) > 0 {
		args = append(args, -1, opts)
	}
	if _, err := p.Command(args...); err != nil {
		return err
	}
	// The daemon keeps pause state between invocations, so a session someone
	// paused yesterday would swallow today's `play` silently. Unpausing is the
	// difference between "queued" and "audible".
	_, err := p.Command("set_property", "pause", false)
	return err
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

	// A one-byte ranged GET, NOT a HEAD. TASKS.md T5.3 says "HEAD via bridge",
	// and that was measured wrong on 2026-08-22: SoundCloud's playback CDN
	// answers HEAD with `405 Method Not Allowed`, so a HEAD preflight rejects
	// every healthy stream — it skipped a track that played fine seconds later.
	// `Range: bytes=0-0` is the portable equivalent, and it proves more: the CDN
	// will actually serve bytes, not merely acknowledge the URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := client.Do(req)
	if err != nil {
		// The URL is a credential, and err embeds it. Report the shape of the
		// failure, never the target (invariant #3).
		return fmt.Errorf("preflight failed: %s", redactURL(err.Error(), streamURL))
	}
	defer resp.Body.Close()

	// 403 is the one that matters: an expired or revoked signature. Anything
	// else at or above 400 is equally unplayable from here.
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
//
// The redaction is a backstop for invariant #3. Enqueue sets force-media-title
// so this returns a real title, but anything that reaches mpv's playlist by
// another route (a user's own `loadfile`, a future code path) would fall back
// to the filename — which for an HLS stream is a signed URL. `np` must never be
// the thing that prints a credential.
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
	// The SoundCloud title Enqueue set: third-party text bound for a terminal,
	// scrubbed the same way the results table scrubs it.
	return core.Clean(s), nil
}

// Down stops the daemon and then closes our end of the pipe (T5.5). It is the
// teardown half of the pair: `stop` the VERB clears the queue and keeps the
// session alive, while this ends the session outright.
//
// core.App.Purge depends on it doing exactly that: on POSIX the IPC socket
// lives inside the directory purge is about to delete, and removing it out from
// under a live mpv leaves a daemon nobody can talk to (T1.13). On Windows an
// open handle cannot be removed at all.
//
// A daemon that is already gone is not an error — Purge must not stop halfway
// because there was nothing to tear down.
//
// Asking only reaches the mpv that OWNS the endpoint. Another mpv started on it
// never gets the pipe: it logs "Couldn't create first pipe instance", then idles
// with no IPC at all, where no `quit` can reach it (measured 2026-09-28). So
// Down ends by killing every mpv whose argv still names this endpoint — the
// endpoint, not the name "mpv", so a player the user started is left alone.
func (p *MPV) Down() error {
	if p.ipc == nil {
		// A fresh process — `daemon stop`, `purge` — has not dialled yet, and
		// returning here is what made both of them silent no-ops: exit 0, daemon
		// still running (measured 2026-09-28). Dial first; nothing answering
		// means there is no one to ask.
		if rw, err := dial(p.sock); err == nil {
			p.ipc = newIPC(rw)
		}
	}
	var err error
	grace := time.Duration(0)
	if p.ipc != nil {
		// Best effort: mpv closes the connection as it exits, so the reply to
		// `quit` frequently never arrives. That is a successful quit, not a fault.
		_, _ = p.Command("quit")
		err = p.ipc.Close()
		p.ipc = nil
		grace = quitGrace
	}
	return errors.Join(err, reap(p.sock, grace))
}

// quitGrace is how long a daemon that was asked to quit gets to exit on its own
// before Down counts it with the strays.
const quitGrace = 2 * time.Second

// reap kills whatever mpv still runs on sock once grace is up, and reports it:
// a stray is a fault worth seeing, not routine.
func reap(sock string, grace time.Duration) error {
	arg := "--input-ipc-server=" + sock
	strays := waitGone(arg, grace)
	if len(strays) == 0 {
		return nil
	}
	for _, pid := range strays {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill() // one already gone — a wrapper whose child just died — is fine
		}
	}
	// Kill is asynchronous on Windows; only the process table can say it worked.
	if left := waitGone(arg, time.Second); len(left) > 0 {
		return fmt.Errorf("mpv still running after kill (pid %v)", left)
	}
	fmt.Fprintf(os.Stderr, "chainsmoke: killed %d stray mpv process(es) that quit could not reach\n", len(strays))
	return nil
}

// waitGone polls for up to d until no mpv carries arg, and returns the pids of
// any that still do.
func waitGone(arg string, d time.Duration) []int {
	pids := daemons(arg)
	for deadline := time.Now().Add(d); len(pids) > 0 && time.Now().Before(deadline); pids = daemons(arg) {
		time.Sleep(50 * time.Millisecond)
	}
	return pids
}

package player

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"chainsmoke/pkg/core"
)

// fakeMPV serves one canned exchange over an in-memory pipe: it reads the
// command line, hands it to reply(), and writes back whatever that returns.
// net.Pipe keeps this identical on unix and Windows — the platform difference
// is the dial, not the protocol, and the dial is not what T5.2 is about.
func fakeMPV(t *testing.T, reply func(req string) []string) *ipc {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	go func() {
		sc := bufio.NewScanner(server)
		for sc.Scan() {
			for _, line := range reply(sc.Text()) {
				fmt.Fprintln(server, line)
			}
		}
	}()
	return newIPC(client)
}

// TestSendDropsEventsAndForeignReplies is the whole of T5.2. mpv pushes events
// on the same stream as replies, so the wire before OUR answer can hold
// anything — here a property-change event (no request_id at all) and a reply to
// a different request. A client that reads one line and returns it hands the
// caller an event and calls it a volume.
func TestSendDropsEventsAndForeignReplies(t *testing.T) {
	c := fakeMPV(t, func(req string) []string {
		id := requestID(t, req)
		return []string{
			`{"event":"property-change","id":1,"name":"volume","data":77.000000}`,
			`{"event":"playback-restart"}`,
			`{"data":"someone else","request_id":999,"error":"success"}`,
			fmt.Sprintf(`{"data":"mpv v0.41.0","request_id":%d,"error":"success"}`, id),
		}
	})

	got, err := c.send("get_property", "mpv-version")
	if err != nil {
		t.Fatalf("send() = %v", err)
	}
	if got != "mpv v0.41.0" {
		t.Errorf("send() = %v, want the reply matching our request_id", got)
	}
}

// TestSendFraming pins the wire format against what mpv actually accepts:
// {"command":[...],"request_id":N}. Verified live against mpv 0.41.
func TestSendFraming(t *testing.T) {
	var seen string
	c := fakeMPV(t, func(req string) []string {
		seen = req
		return []string{fmt.Sprintf(`{"request_id":%d,"error":"success"}`, requestID(t, req))}
	})

	if _, err := c.send("set_property", "volume", 77); err != nil {
		t.Fatalf("send() = %v", err)
	}
	const want = `{"command":["set_property","volume",77],"request_id":1}`
	if seen != want {
		t.Errorf("wire = %s\nwant   %s", seen, want)
	}
}

// TestMatchingIDIsNotSuccess: mpv answers a bad command with OUR request_id and
// an error string. Correlating without checking "error" reports failure as a
// nil result — the trap the live capture caught (request 103).
func TestMatchingIDIsNotSuccess(t *testing.T) {
	c := fakeMPV(t, func(req string) []string {
		return []string{fmt.Sprintf(`{"request_id":%d,"error":"invalid parameter"}`, requestID(t, req))}
	})

	_, err := c.send("no-such-command")
	if err == nil {
		t.Fatal("send() = nil, want the error mpv reported")
	}
	if !strings.Contains(err.Error(), "invalid parameter") {
		t.Errorf("send() = %v, want mpv's own wording", err)
	}
}

// TestErrorsDoNotLeakArguments: `loadfile <signed url>` is the one command
// whose arguments are a credential (invariant #3). An error string reaches the
// terminal, so the command name goes in and the arguments stay out.
func TestErrorsDoNotLeakArguments(t *testing.T) {
	const signed = "https://playback.media-streaming.soundcloud.cloud/x?Policy=abc&Signature=secret"
	c := fakeMPV(t, func(req string) []string {
		return []string{fmt.Sprintf(`{"request_id":%d,"error":"unsupported format"}`, requestID(t, req))}
	})

	_, err := c.send("loadfile", signed, "append-play")
	if err == nil {
		t.Fatal("send() = nil, want an error")
	}
	if strings.Contains(err.Error(), "Signature") || strings.Contains(err.Error(), "sndcdn") ||
		strings.Contains(err.Error(), signed) {
		t.Errorf("error leaked the signed URL: %v", err)
	}
	if !strings.Contains(err.Error(), "loadfile") {
		t.Errorf("error = %v, want the command name for context", err)
	}
}

// TestRequestIDsAreDistinct: reusing an id would let a late reply to command N
// be read as the answer to command N+1.
func TestRequestIDsAreDistinct(t *testing.T) {
	var ids []int
	c := fakeMPV(t, func(req string) []string {
		id := requestID(t, req)
		ids = append(ids, id)
		return []string{fmt.Sprintf(`{"request_id":%d,"error":"success"}`, id)}
	})

	for i := 0; i < 3; i++ {
		if _, err := c.send("get_property", "volume"); err != nil {
			t.Fatalf("send() #%d = %v", i, err)
		}
	}
	if len(ids) != 3 || ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
		t.Errorf("request ids = %v, want three distinct values", ids)
	}
}

// TestDeadDaemon: mpv exiting mid-command must produce a diagnosis, not a hang
// and not a nil result. This is the stale-socket case T5.1's probe relies on.
func TestDeadDaemon(t *testing.T) {
	client, server := net.Pipe()
	c := newIPC(client)
	go func() {
		bufio.NewScanner(server).Scan() // take the command, then vanish
		server.Close()
	}()

	done := make(chan error, 1)
	go func() { _, err := c.send("get_property", "volume"); done <- err }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("send() = nil, want an error when mpv closes the connection")
		}
		if !strings.Contains(err.Error(), "get_property") {
			t.Errorf("send() = %v, want the command named", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send() hung after the daemon closed the connection")
	}
}

// requestID pulls the id back out of a command line so a fake can answer it.
func requestID(t *testing.T, line string) int {
	t.Helper()
	var req struct {
		RequestID int `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		t.Fatalf("client wrote unparseable JSON %q: %v", line, err)
	}
	if req.RequestID == 0 {
		t.Fatalf("client sent no request_id: %s — events would be indistinguishable", line)
	}
	return req.RequestID
}

// fakeNet is a Net whose gate and proxy the test controls. The player only ever
// asks it those two questions.
type fakeNet struct {
	gate  error
	proxy string
}

func (n fakeNet) Up() error        { return nil }
func (n fakeNet) Down() error      { return nil }
func (n fakeNet) Rotate() error    { return nil }
func (n fakeNet) SelfTest() error  { return nil }
func (n fakeNet) ProxyURL() string { return n.proxy }
func (n fakeNet) Assert() error    { return n.gate }
func (n fakeNet) Verified() bool   { return n.gate == nil }

// TestEnqueueFailsClosed is T0.2 for the player: with the gate shut, Enqueue
// must refuse BEFORE the preflight, never mind before mpv. A dead HTTP address
// proves it never dialled — if the gate were checked late, this would hang or
// report a connection error instead.
func TestEnqueueFailsClosed(t *testing.T) {
	p := New("unused.sock", fakeNet{gate: errors.New("egress not verified")})

	err := p.Enqueue("http://127.0.0.1:1/never-reached", "x")
	if err == nil {
		t.Fatal("Enqueue() = nil with the gate shut, want a refusal")
	}
	if !strings.Contains(err.Error(), "egress not verified") {
		t.Errorf("Enqueue() = %v, want the gate's own error", err)
	}
}

// TestPreflightSkipsDeadStreams: an expired signed URL answers 403, and the
// caller needs that as ErrDeadStream so it can skip the track rather than hand
// mpv a connection that will never produce audio (T5.3 / T1.14).
func TestPreflightSkipsDeadStreams(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer dead.Close()

	p := New("unused.sock", fakeNet{proxy: "DIRECT (no proxy)"})
	err := p.Enqueue(dead.URL+"/expired?Signature=secret", "x")

	if !errors.Is(err, core.ErrDeadStream) {
		t.Fatalf("Enqueue() = %v, want core.ErrDeadStream", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("Enqueue() = %v, want the CDN's status for context", err)
	}
}

// TestPreflightErrorsDoNotLeakTheURL: a transport error quotes the URL it was
// given, and that URL is a credential (invariant #3).
func TestPreflightErrorsDoNotLeakTheURL(t *testing.T) {
	const signed = "http://127.0.0.1:1/x?Policy=abc&Signature=secret"
	p := New("unused.sock", fakeNet{proxy: "DIRECT (no proxy)"})

	err := p.Enqueue(signed, "x")
	if err == nil {
		t.Fatal("Enqueue() = nil against a dead port, want an error")
	}
	if strings.Contains(err.Error(), "Signature") || strings.Contains(err.Error(), signed) {
		t.Errorf("error leaked the signed URL: %v", err)
	}
}

// TestHTTPClientUsesTheLiveProxy: the preflight must go where the resolver
// went. ProxyURL's prose sentinels are not URLs and must not be dialled.
func TestHTTPClientUsesTheLiveProxy(t *testing.T) {
	for _, tc := range []struct {
		proxy string
		want  string // expected proxy for a sample request; "" = direct
	}{
		{"http://127.0.0.1:41235", "http://127.0.0.1:41235"},
		{"DIRECT (no proxy)", ""},
		{"<down>", ""},
		{"", ""},
	} {
		c, err := New("unused.sock", fakeNet{proxy: tc.proxy}).httpClient()
		if err != nil {
			t.Fatalf("httpClient(%q) = %v", tc.proxy, err)
		}
		tr := c.Transport.(*http.Transport)
		var got string
		if tr.Proxy != nil {
			req, _ := http.NewRequest(http.MethodHead, "https://example.invalid/x", nil)
			u, err := tr.Proxy(req)
			if err != nil {
				t.Fatalf("proxy func: %v", err)
			}
			if u != nil {
				got = u.String()
			}
		}
		if got != tc.want {
			t.Errorf("ProxyURL %q -> proxy %q, want %q", tc.proxy, got, tc.want)
		}
	}
}

// TestEnqueueSendsLoadfile pins the T5.3 wire format and, just as importantly,
// that Enqueue RETURNS — a blocking Enqueue is what made preload-ahead
// impossible before.
func TestEnqueueSendsLoadfile(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer origin.Close()

	client, server := net.Pipe()
	defer client.Close()
	var sent []string
	go func() {
		sc := bufio.NewScanner(server)
		for sc.Scan() {
			sent = append(sent, sc.Text())
			fmt.Fprintf(server, `{"request_id":%d,"error":"success"}`+"\n", requestID(t, sc.Text()))
		}
	}()

	p := New("unused.sock", fakeNet{proxy: "DIRECT (no proxy)"})
	p.ipc = newIPC(client) // stand in for a dialled daemon

	done := make(chan error, 1)
	go func() { done <- p.Enqueue(origin.URL+"/track.m4a?Signature=secret", "Lemon Black") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Enqueue() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue() blocked — it must hand the track to the daemon and return")
	}

	if len(sent) != 2 {
		t.Fatalf("sent %d commands, want loadfile + unpause: %v", len(sent), sent)
	}
	if !strings.Contains(sent[0], `"loadfile"`) || !strings.Contains(sent[0], `"append-play"`) {
		t.Errorf("first command = %s, want loadfile ... append-play", sent[0])
	}
	// Sticky pause from an earlier session would swallow this track silently.
	if !strings.Contains(sent[1], `"pause"`) {
		t.Errorf("second command = %s, want the unpause", sent[1])
	}
}

// TestEnqueueDoesNotQueueADeadStream: the preflight is the only synchronous
// signal (loadfile answers "success" for a dead URL), so a failed preflight
// must stop the track from ever reaching mpv's playlist.
func TestEnqueueDoesNotQueueADeadStream(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer dead.Close()

	client, server := net.Pipe()
	defer client.Close()
	var sent int
	go func() {
		sc := bufio.NewScanner(server)
		for sc.Scan() {
			sent++
		}
	}()

	p := New("unused.sock", fakeNet{proxy: "DIRECT (no proxy)"})
	p.ipc = newIPC(client)

	if err := p.Enqueue(dead.URL+"/expired", "x"); !errors.Is(err, core.ErrDeadStream) {
		t.Fatalf("Enqueue() = %v, want core.ErrDeadStream", err)
	}
	if sent != 0 {
		t.Errorf("%d commands reached mpv; a dead stream must never enter the playlist", sent)
	}
}

// TestEnqueueForcesTheTitle: without force-media-title mpv falls back to the
// filename, and an HLS "filename" is the whole signed URL — so `np` would print
// a credential. Measured against mpv 0.41 before this was added.
func TestEnqueueForcesTheTitle(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer origin.Close()

	client, server := net.Pipe()
	defer client.Close()
	var first string
	go func() {
		sc := bufio.NewScanner(server)
		for sc.Scan() {
			if first == "" {
				first = sc.Text()
			}
			fmt.Fprintf(server, `{"request_id":%d,"error":"success"}`+"\n", requestID(t, sc.Text()))
		}
	}()

	p := New("unused.sock", fakeNet{proxy: "DIRECT (no proxy)"})
	p.ipc = newIPC(client)
	if err := p.Enqueue(origin.URL+"/playlist.m3u8?Signature=secret", "Lemon Black"); err != nil {
		t.Fatalf("Enqueue() = %v", err)
	}
	if !strings.Contains(first, `"force-media-title":"Lemon Black"`) {
		t.Errorf("loadfile = %s\nwant force-media-title so `np` shows a title, not a URL", first)
	}
}

// TestEnqueueCarriesTheProxyPerLoad is the egress-shape test the 2026-09-05
// regression showed was missing. mpv has no proxy flag and no proxy default:
// the daemon's ONLY route to the bridge used to be an inherited http_proxy, and
// `cmd.Env = []string{"PATH=..."}` in Ensure() severed it — 131 tests and 35
// smoke cases stayed green while every byte of audio went direct (measured
// 2026-09-09 against a loopback origin). Binding the proxy to the daemon was
// the root cause: a daemon outlives the run that spawned it, the bridge's port
// changes every run, and `daemon start` never calls Up() at all. So the proxy
// rides each loadfile instead, and this asserts what actually crosses the
// socket — the one place the leak is visible before mpv opens a connection.
//
// Three rows, three ways the first attempt at this got it wrong: the option
// must be there with a real proxy; it must be there with NO title (it was gated
// on one); and ProxyURL's prose sentinels must never be forwarded (mpv would be
// told `http_proxy=DIRECT (no proxy)`). The options map is parsed, not
// substring-matched, because a second map appended as a fifth positional looks
// fine to Contains() and is rejected by mpv.
func TestEnqueueCarriesTheProxyPerLoad(t *testing.T) {
	// A proxy that answers everything. For a plain-http origin Go sends the
	// preflight to the proxy in absolute form rather than CONNECT, so a 200 to
	// anything is enough to stand in for the bridge.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer proxy.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer origin.Close()

	for _, tc := range []struct {
		name, proxy, title string
		wantOpt            string // "" means stream-lavf-o must be absent
	}{
		{"live proxy, titled", proxy.URL, "Lemon Black", "http_proxy=" + proxy.URL},
		{"live proxy, untitled", proxy.URL, "", "http_proxy=" + proxy.URL},
		{"direct sentinel", "DIRECT (no proxy)", "Lemon Black", ""},
		{"down sentinel", "<down>", "Lemon Black", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			var first string
			go func() {
				sc := bufio.NewScanner(server)
				for sc.Scan() {
					if first == "" {
						first = sc.Text()
					}
					fmt.Fprintf(server, `{"request_id":%d,"error":"success"}`+"\n", requestID(t, sc.Text()))
				}
			}()

			p := New("unused.sock", fakeNet{proxy: tc.proxy})
			p.ipc = newIPC(client)
			if err := p.Enqueue(origin.URL+"/playlist.m3u8?Signature=secret", tc.title); err != nil {
				t.Fatalf("Enqueue() = %v", err)
			}

			var msg struct {
				Command []json.RawMessage `json:"command"`
			}
			if err := json.Unmarshal([]byte(first), &msg); err != nil {
				t.Fatalf("loadfile is not JSON: %v\n%s", err, first)
			}

			// Collect every options map and where it sits. There must be at
			// most one, and it must be the fourth positional.
			var opts map[string]string
			for i, raw := range msg.Command {
				if len(raw) == 0 || raw[0] != '{' {
					continue
				}
				if opts != nil {
					t.Fatalf("loadfile carries two options maps — mpv takes one, in slot 4:\n%s", first)
				}
				if i != 4 {
					t.Fatalf("options map is positional %d, want 4 (after the -1 index):\n%s", i, first)
				}
				if err := json.Unmarshal(raw, &opts); err != nil {
					t.Fatalf("options map: %v\n%s", err, first)
				}
			}

			got := opts["stream-lavf-o"]
			if got != tc.wantOpt {
				t.Errorf("stream-lavf-o = %q, want %q\n%s", got, tc.wantOpt, first)
			}
			if tc.title != "" && opts["force-media-title"] != tc.title {
				t.Errorf("force-media-title = %q, want %q — the proxy must not displace the title\n%s",
					opts["force-media-title"], tc.title, first)
			}
		})
	}
}

// TestDownWithoutADaemonIsNotAnError: purge calls Down() on machines that never
// started a daemon, and a purge must not stop halfway because there was nothing
// to take down.
func TestDownWithoutADaemonIsNotAnError(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "no-daemon.sock"), fakeNet{})
	if err := p.Down(); err != nil {
		t.Fatalf("Down() with no daemon = %v, want nil", err)
	}
}

// TestDownFromAFreshProcessSendsQuit is the path `daemon stop` and `purge`
// actually take: a new process that has never dialled the daemon. Down() used to
// return early on p.ipc == nil, so both verbs exited 0 and the daemon lived on
// (measured 2026-09-28). Unix socket only — serving a Windows named pipe from a
// test needs go-winio; smoke.ps1's daemon-stop case covers that target for real.
func TestDownFromAFreshProcessSendsQuit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named-pipe server needs go-winio; covered by smoke.ps1 daemon-stop")
	}
	sock := filepath.Join(t.TempDir(), "mpv.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- ""
			return
		}
		defer c.Close()
		sc := bufio.NewScanner(c)
		if !sc.Scan() {
			got <- ""
			return
		}
		got <- sc.Text() // then close without replying, as a quitting mpv does
	}()

	p := New(sock, fakeNet{}) // fresh: nothing has dialled yet
	if err := p.Down(); err != nil {
		t.Fatalf("Down() = %v, want nil", err)
	}
	select {
	case line := <-got:
		if !strings.Contains(line, `"quit"`) {
			t.Errorf("daemon received %q, want a quit command", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Down() never reached the daemon — `daemon stop` would be a no-op")
	}
}

// TestDownKillsStrays: `quit` over IPC reaches only the mpv that owns the
// endpoint. A second mpv started on it never gets the pipe and idles forever
// with no IPC, and it used to survive both `daemon stop` and `purge`. Down must
// kill it, and must spare what merely resembles it: an mpv on another endpoint
// (a prefix match would take it) and a process that names the endpoint but is
// not mpv.
func TestDownKillsStrays(t *testing.T) {
	endpoint := filepath.Join(t.TempDir(), "mpv.sock")
	if runtime.GOOS == "windows" {
		endpoint = fmt.Sprintf(`\\.\pipe\chainsmoke-test-%d`, os.Getpid())
	}
	stray := standIn(t, "mpv", endpoint)
	spared := map[string]<-chan struct{}{
		"an mpv on another endpoint": standIn(t, "mpv", endpoint+"-other"),
		"a non-mpv process":          standIn(t, "grep", endpoint),
	}

	if err := New(endpoint, fakeNet{}).Down(); err != nil {
		t.Fatalf("Down() = %v", err)
	}
	select {
	case <-stray:
	case <-time.After(5 * time.Second):
		t.Fatal("the stray mpv outlived Down() — purge would leave it running")
	}
	time.Sleep(300 * time.Millisecond) // a wrong kill would have landed by now
	for what, exited := range spared {
		select {
		case <-exited:
			t.Errorf("Down() killed %s", what)
		default:
		}
	}
}

// standIn starts a copy of this test binary named name, with the argv Ensure()
// gives mpv, and returns a channel that closes when it exits. TestMain makes
// the copy idle until killed; cleanup kills it whatever the test did.
func standIn(t *testing.T, name, endpoint string) <-chan struct{} {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--idle", "--input-ipc-server="+endpoint)
	cmd.Env = append(os.Environ(), "CHAINSMOKE_STAND_IN=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	return exited
}

// TestMain doubles as the stand-in daemon for TestDownKillsStrays: run with
// CHAINSMOKE_STAND_IN set, this binary idles the way an mpv without its pipe
// does. The minute is a backstop, so a failed test cannot leave one behind.
func TestMain(m *testing.M) {
	if os.Getenv("CHAINSMOKE_STAND_IN") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestNowPlayingNeverPrintsAURL is the backstop: whatever ends up in mpv's
// playlist, `np` must not be the thing that puts a signed URL on a terminal.
func TestNowPlayingNeverPrintsAURL(t *testing.T) {
	const leaked = "playlist.m3u8?Policy=abc&Signature=secret"
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		sc := bufio.NewScanner(server)
		for sc.Scan() {
			fmt.Fprintf(server, `{"data":"https://cdn.example/%s","request_id":%d,"error":"success"}`+"\n",
				leaked, requestID(t, sc.Text()))
		}
	}()

	p := New("unused.sock", fakeNet{})
	p.ipc = newIPC(client)

	got, err := p.NowPlaying()
	if err != nil {
		t.Fatalf("NowPlaying() = %v", err)
	}
	if strings.Contains(got, "Signature") || strings.Contains(got, "://") {
		t.Errorf("NowPlaying() = %q — it printed the signed URL", got)
	}
}

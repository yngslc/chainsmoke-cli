package player

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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

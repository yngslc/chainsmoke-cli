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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"chainsmoke/pkg/core"
)

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

func TestDeadDaemon(t *testing.T) {
	client, server := net.Pipe()
	c := newIPC(client)
	go func() {
		bufio.NewScanner(server).Scan()
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

func TestHTTPClientUsesTheLiveProxy(t *testing.T) {
	for _, tc := range []struct {
		proxy string
		want  string
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
	p.ipc = newIPC(client)

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

	if len(sent) != 1 {
		t.Fatalf("sent %d commands, want loadfile only: %v", len(sent), sent)
	}
	if !strings.Contains(sent[0], `"loadfile"`) || !strings.Contains(sent[0], `"append-play"`) {
		t.Errorf("first command = %s, want loadfile ... append-play", sent[0])
	}
}

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

func TestEnqueueCarriesTheProxyPerLoad(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer proxy.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer origin.Close()

	for _, tc := range []struct {
		name, proxy, title string
		wantOpt            string
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

func TestDownWithoutADaemonIsNotAnError(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "no-daemon.sock"), fakeNet{})
	if err := p.Down(); err != nil {
		t.Fatalf("Down() with no daemon = %v, want nil", err)
	}
}

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
		got <- sc.Text()
	}()

	p := New(sock, fakeNet{})
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

func TestDownKillsStrays(t *testing.T) {
	endpoint := testEndpoint(t)
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
	time.Sleep(300 * time.Millisecond)
	for what, exited := range spared {
		select {
		case <-exited:
			t.Errorf("Down() killed %s", what)
		default:
		}
	}
}

func TestEnsureWaitsForADaemonStillStarting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named-pipe server needs go-winio; TestEnsureReplacesADaemonThatNeverAnswers covers the shared path")
	}
	endpoint := testEndpoint(t)
	useStandInMPV(t)
	booting := standIn(t, "mpv", endpoint)
	t.Cleanup(func() { _ = reap(endpoint, 0) })
	serveDaemon(t, endpoint, true, 300*time.Millisecond)

	if err := New(endpoint, fakeNet{}).Ensure(); err != nil {
		t.Fatalf("Ensure() = %v, want nil once the starting daemon answers", err)
	}
	if n := len(daemons("--input-ipc-server=" + endpoint)); n != 1 {
		t.Errorf("%d mpv on the endpoint, want 1 — Ensure started another while the first was booting", n)
	}
	select {
	case <-booting:
		t.Error("Ensure killed the mpv that was still starting")
	default:
	}
}

func TestEnsureReplacesADaemonThatNeverAnswers(t *testing.T) {
	endpoint := testEndpoint(t)
	useStandInMPV(t)
	startTimeout = 200 * time.Millisecond
	stray := standIn(t, "mpv", endpoint)
	t.Cleanup(func() { _ = reap(endpoint, 0) })

	_ = New(endpoint, fakeNet{}).Ensure()

	select {
	case <-stray:
	case <-time.After(5 * time.Second):
		t.Fatal("Ensure left the silent mpv running")
	}
	if n := len(daemons("--input-ipc-server=" + endpoint)); n != 1 {
		t.Errorf("%d mpv on the endpoint after Ensure, want exactly its replacement", n)
	}
}

func TestEnsureSpawnsTheDaemonSafely(t *testing.T) {
	endpoint := testEndpoint(t)
	useStandInMPV(t)
	startTimeout = 200 * time.Millisecond
	dump := filepath.Join(t.TempDir(), "env")
	t.Setenv("CHAINSMOKE_STAND_IN_ENV", dump)
	t.Setenv("no_proxy", "*")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Cleanup(func() { _ = reap(endpoint, 0) })

	_ = New(endpoint, fakeNet{}).Ensure()

	var env []string
	for deadline := time.Now().Add(5 * time.Second); env == nil && time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if b, err := os.ReadFile(dump); err == nil {
			env = strings.Split(string(b), "\n")
		}
	}
	if env == nil {
		t.Fatal("the daemon Ensure spawned never reported its environment")
	}
	for _, flag := range []string{"--no-ytdl", "--script-opts=" + spawnMarkKey + "=" + spawnGen, "--volume=50"} {
		if argv := env[0]; !slices.Contains(strings.Fields(argv), flag) {
			t.Errorf("mpv was spawned without %s: %s", flag, argv)
		}
	}
	kept := false
	for _, kv := range env[1:] {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "no_proxy") {
			t.Errorf("mpv was spawned with %s=%s — libavformat would skip the proxy", k, v)
		}
		kept = kept || strings.EqualFold(k, "http_proxy") && v == "http://127.0.0.1:1"
	}
	if !kept {
		t.Error("http_proxy did not reach mpv — the environment was stripped, not filtered")
	}
}

func TestEnqueueRefusesAProxyMpvWouldIgnore(t *testing.T) {
	for _, proxy := range []string{"HTTP://127.0.0.1:1", "https://127.0.0.1:1", "socks5://127.0.0.1:1"} {
		client, server := net.Pipe()
		got := make(chan string, 1)
		go func() {
			defer close(got)
			if sc := bufio.NewScanner(server); sc.Scan() {
				got <- sc.Text()
			}
		}()
		p := New("unused.sock", fakeNet{proxy: proxy})
		p.ipc = newIPC(client)

		err := p.Enqueue("https://cdn.example/x.m3u8?Signature=s", "t")
		switch {
		case err == nil || errors.Is(err, core.ErrDeadStream):
			t.Errorf("Enqueue with proxy %s = %v, want a refusal before the preflight", proxy, err)
		case strings.Contains(err.Error(), "127.0.0.1"):
			t.Errorf("the refusal repeats the proxy address (it may carry credentials): %v", err)
		}
		client.Close()
		if line := <-got; line != "" {
			t.Errorf("with proxy %s, mpv was sent %s", proxy, line)
		}
		server.Close()
	}
}

func TestEnsureReportsAnMpvThatExitsAtOnce(t *testing.T) {
	endpoint := testEndpoint(t)
	useStandInMPV(t)
	t.Setenv("CHAINSMOKE_STAND_IN_EXIT", "1")

	start := time.Now()
	err := New(endpoint, fakeNet{}).Ensure()
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("Ensure() = %v, want an error saying mpv exited", err)
	}
	if d := time.Since(start); d > startTimeout/2 {
		t.Errorf("Ensure took %v to notice an mpv that exited at once", d)
	}
}

func testEndpoint(t *testing.T) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`\\.\pipe\chainsmoke-test-%d-%s`, os.Getpid(), t.Name())
	}
	return filepath.Join(t.TempDir(), "mpv.sock")
}

func useStandInMPV(t *testing.T) {
	t.Setenv("PATH", filepath.Dir(standInBinary(t, "mpv")))
	t.Setenv("CHAINSMOKE_STAND_IN", "1")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	old := startTimeout
	t.Cleanup(func() { startTimeout = old })
}

func serveDaemon(t *testing.T, endpoint string, marked bool, delay time.Duration) func() []string {
	var mu sync.Mutex
	var cmds []string
	up := make(chan net.Listener, 1)
	go func() {
		time.Sleep(delay)
		ln, err := net.Listen("unix", endpoint)
		up <- ln
		for err == nil {
			var c net.Conn
			if c, err = ln.Accept(); err == nil {
				go func() {
					defer c.Close()
					sc := bufio.NewScanner(c)
					for sc.Scan() {
						var req struct {
							Command []any `json:"command"`
							ID      int   `json:"request_id"`
						}
						_ = json.Unmarshal(sc.Bytes(), &req)
						mu.Lock()
						cmds = append(cmds, fmt.Sprint(req.Command...))
						mu.Unlock()
						var data any = true
						if len(req.Command) == 2 && req.Command[0] == "get_property" {
							switch req.Command[1] {
							case "script-opts":
								data = map[string]string{}
								if marked {
									data = map[string]string{spawnMarkKey: spawnGen}
								}
							case "ytdl":
								data = !marked
							}
						}
						b, _ := json.Marshal(map[string]any{"data": data, "request_id": req.ID, "error": "success"})
						_, _ = c.Write(append(b, '\n'))
					}
				}()
			}
		}
	}()
	t.Cleanup(func() {
		if ln := <-up; ln != nil {
			ln.Close()
		}
	})
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), cmds...) }
}

func TestEnsureReusesOnlyADaemonItMarked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named-pipe server needs go-winio; the check is the same code on both targets")
	}
	for _, marked := range []bool{true, false} {
		endpoint := testEndpoint(t)
		useStandInMPV(t)
		startTimeout = 300 * time.Millisecond
		t.Cleanup(func() { _ = reap(endpoint, 0) })
		received := serveDaemon(t, endpoint, marked, 0)
		time.Sleep(50 * time.Millisecond)

		err := New(endpoint, fakeNet{}).Ensure()
		quit := slices.Contains(received(), "quit")
		spawned := len(daemons("--input-ipc-server=" + endpoint))
		switch {
		case marked && (err != nil || quit || spawned != 0):
			t.Errorf("its own daemon: Ensure() = %v, quit sent = %v, spawned %d — want it reused as is", err, quit, spawned)
		case !marked && (!quit || spawned != 1):
			t.Errorf("an unmarked daemon: quit sent = %v, spawned %d — want it replaced", quit, spawned)
		}
	}
}

func standInBinary(t *testing.T, name string) string {
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
	return bin
}

func standIn(t *testing.T, name, endpoint string) <-chan struct{} {
	t.Helper()
	cmd := exec.Command(standInBinary(t, name), "--idle", "--input-ipc-server="+endpoint)
	cmd.Env = append(os.Environ(), "CHAINSMOKE_STAND_IN=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	return exited
}

func TestMain(m *testing.M) {
	if os.Getenv("CHAINSMOKE_STAND_IN") == "1" {
		if os.Getenv("CHAINSMOKE_STAND_IN_EXIT") == "1" {
			os.Exit(3)
		}
		if f := os.Getenv("CHAINSMOKE_STAND_IN_ENV"); f != "" {
			report := append([]string{strings.Join(os.Args[1:], " ")}, os.Environ()...)
			_ = os.WriteFile(f+".tmp", []byte(strings.Join(report, "\n")), 0o600)
			_ = os.Rename(f+".tmp", f)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestNowPlayingScrubsTheTitle(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		sc := bufio.NewScanner(server)
		for sc.Scan() {
			fmt.Fprintf(server, `{"data":"red \u001b[31mALERT\u001b]0;pwned\u0007","request_id":%d,"error":"success"}`+"\n",
				requestID(t, sc.Text()))
		}
	}()

	p := New("unused.sock", fakeNet{})
	p.ipc = newIPC(client)

	got, err := p.NowPlaying()
	if err != nil {
		t.Fatalf("NowPlaying() = %v", err)
	}
	if strings.IndexFunc(got, func(r rune) bool { return r < 0x20 }) >= 0 {
		t.Errorf("NowPlaying() = %q — control characters reached the terminal", got)
	}
}

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

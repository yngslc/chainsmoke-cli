package net

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeSocks struct {
	ln       net.Listener
	gotHost  string
	gotATYP  byte
	gotUser  string
	wantAuth bool
}

func newFakeSocks(t *testing.T, wantAuth bool) *fakeSocks {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSocks{ln: ln, wantAuth: wantAuth}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeSocks) addr() string { return f.ln.Addr().String() }

func (f *fakeSocks) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeSocks) handle(c net.Conn) {
	defer c.Close()

	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	if f.wantAuth {
		if _, err := c.Write([]byte{0x05, 0x02}); err != nil {
			return
		}
		a := make([]byte, 2)
		if _, err := io.ReadFull(c, a); err != nil {
			return
		}
		user := make([]byte, a[1])
		if _, err := io.ReadFull(c, user); err != nil {
			return
		}
		f.gotUser = string(user)
		pl := make([]byte, 1)
		if _, err := io.ReadFull(c, pl); err != nil {
			return
		}
		if _, err := io.ReadFull(c, make([]byte, pl[0])); err != nil {
			return
		}
		if _, err := c.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	} else if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return
	}
	f.gotATYP = req[3]
	switch req[3] {
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return
		}
		host := make([]byte, l[0])
		if _, err := io.ReadFull(c, host); err != nil {
			return
		}
		f.gotHost = string(host)
	case 0x01:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(c, ip); err != nil {
			return
		}
		f.gotHost = net.IP(ip).String()
	}
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil {
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_, _ = io.Copy(c, c)
}

func TestConnectBridgeTunnelsAndKeepsDNSRemote(t *testing.T) {
	fs := newFakeSocks(t, false)
	b, err := StartConnectBridge(fs.addr(), false)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer b.Close()

	c, err := net.DialTimeout("tcp", strings.TrimPrefix(b.ProxyURL(), "http://"), 2*time.Second)
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer c.Close()

	if _, err := io.WriteString(c, "CONNECT api-v2.soundcloud.com:443 HTTP/1.1\r\nHost: api-v2.soundcloud.com:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if fs.gotATYP != 0x03 {
		t.Errorf("DNS LEAK: destination sent as ATYP %#02x, want 0x03 (domain)", fs.gotATYP)
	}
	if fs.gotHost != "api-v2.soundcloud.com" {
		t.Errorf("destination = %q, want the literal hostname", fs.gotHost)
	}
}

func TestConnectBridgeRefusesPlaintextProxying(t *testing.T) {
	fs := newFakeSocks(t, false)
	b, err := StartConnectBridge(fs.addr(), false)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer b.Close()

	c, err := net.DialTimeout("tcp", strings.TrimPrefix(b.ProxyURL(), "http://"), 2*time.Second)
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer c.Close()

	if _, err := io.WriteString(c, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatalf("write GET: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("want 405 for plaintext proxying, got %d", resp.StatusCode)
	}
}

func TestConnectBridgeNegotiatesIsolation(t *testing.T) {
	fs := newFakeSocks(t, true)
	b, err := StartConnectBridge(fs.addr(), true)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer b.Close()

	if err := b.Verify("api-v2.soundcloud.com:443"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if fs.gotUser != "chainsmoke" {
		t.Errorf("isolation user = %q, want %q", fs.gotUser, "chainsmoke")
	}
}

func TestPrivateTorDiesWithItsOwner(t *testing.T) {
	want := fmt.Sprintf("__OwningControllerProcess %d\n", os.Getpid())
	if rc := privateTorrc(9999, 9998, "data"); !strings.Contains(rc, want) {
		t.Errorf("the private tor's torrc does not name its owner:\n%s", rc)
	}
}

func TestStaleTorDirsAreSwept(t *testing.T) {
	exited := exec.Command(os.Args[0], "-test.run=^$")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	dead := exited.ProcessState.Pid()
	if processAlive(dead) || !processAlive(os.Getpid()) {
		t.Fatalf("processAlive: exited pid %d alive = %v, this process alive = %v", dead, processAlive(dead), processAlive(os.Getpid()))
	}

	tmp := t.TempDir()
	dirs := map[string]bool{
		fmt.Sprintf("%s%d-123", torDirPrefix, dead):         true,
		fmt.Sprintf("%s%d-456", torDirPrefix, os.Getpid()):  false,
		fmt.Sprintf("%s%d-789", torDirPrefix, os.Getppid()): false,
		"chainsmoke-tor-1234567":                            false,
		"chainsmoke-tor-pjunk-1":                            false,
	}
	for name := range dirs {
		if err := os.MkdirAll(filepath.Join(tmp, name, "data"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sweepStaleTorDirs(tmp)
	for name, swept := range dirs {
		if _, err := os.Stat(filepath.Join(tmp, name)); os.IsNotExist(err) != swept {
			t.Errorf("%s: swept = %v, want %v", name, os.IsNotExist(err), swept)
		}
	}
}

func TestNamedTorSocksNeverSpawnsATor(t *testing.T) {
	dir := t.TempDir()
	name := "tor"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("not a program"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	err := New("", false, "127.0.0.1:1", false).Up()
	if err == nil || strings.Contains(err.Error(), "tor could not be started") ||
		!strings.Contains(err.Error(), "no Tor SOCKS listener") {
		t.Errorf("Up() with --tor-socks 127.0.0.1:1 = %v, want a fail-closed refusal that never tried to spawn tor", err)
	}
}

func listenWith(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				_, _ = c.Read(make([]byte, 64))
				_, _ = io.WriteString(c, reply)
			}(c)
		}
	}()
	return ln.Addr().String()
}

const torHTTPReply = "HTTP/1.0 501 Tor is not an HTTP Proxy\r\nContent-Type: text/html; charset=iso-8859-1\r\n\r\n<html>"

func TestIsTorSocksKnowsTorFromOtherListeners(t *testing.T) {
	for _, tc := range []struct {
		name, addr string
		want       bool
	}{
		{"tor", listenWith(t, torHTTPReply), true},
		{"a SOCKS-only server, like ssh -D, hanging up", listenWith(t, ""), false},
		{"an HTTP proxy, like a v2ray mixed port", listenWith(t, "HTTP/1.1 400 Bad Request\r\n\r\n"), false},
	} {
		if got := isTorSocks(tc.addr); got != tc.want {
			t.Errorf("%s: isTorSocks = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestUpRefusesASocksListenerThatIsNotTor(t *testing.T) {
	err := New("", false, listenWith(t, ""), false).Up()
	if err == nil || !strings.Contains(err.Error(), "is not a Tor SocksPort") ||
		!strings.Contains(err.Error(), "refusing") {
		t.Errorf("Up() through a listener that is not tor = %v, want a fail-closed refusal naming it", err)
	}
}

func TestBridgeCarriesPort443Only(t *testing.T) {
	cb, err := StartConnectBridge(newFakeSocks(t, false).addr(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()

	for _, target := range []string{"smtp.example.com:25", "irc.example.com:6667", "evil.example.com:80"} {
		if got := connectVia(t, cb, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); got != http.StatusForbidden {
			t.Errorf("CONNECT %s = %d, want %d", target, got, http.StatusForbidden)
		}
	}
	if got := connectVia(t, cb, "CONNECT api-v2.soundcloud.com:443 HTTP/1.1\r\nHost: api-v2.soundcloud.com:443\r\n\r\n"); got != http.StatusOK {
		t.Errorf("CONNECT to 443 = %d, want 200", got)
	}
	if got := connectVia(t, cb, "CONNECT api-v2.soundcloud.com HTTP/1.1\r\nHost: api-v2.soundcloud.com\r\n\r\n"); got != http.StatusOK {
		t.Errorf("CONNECT with no port (443) = %d, want 200", got)
	}
	if l := cb.ledger.snapshot(); l.Refused != 3 || len(l.Targets) != 1 {
		t.Errorf("ledger: %d refused, targets %v — want 3 refused and only the 443 target", l.Refused, l.Targets)
	}
}

func deafSocks(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quit := make(chan struct{})
	t.Cleanup(func() { close(quit); _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 262)
				if _, err := io.ReadFull(c, buf[:2]); err != nil {
					return
				}
				if _, err := io.ReadFull(c, buf[:buf[1]]); err != nil {
					return
				}
				_, _ = c.Write([]byte{0x05, 0x00})
				if _, err := io.ReadFull(c, buf[:5]); err != nil {
					return
				}
				if _, err := io.ReadFull(c, buf[:int(buf[4])+2]); err != nil {
					return
				}
				_, _ = c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				_, _ = io.Copy(io.Discard, c)
				<-quit
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestTunnelEndsAtTheFirstEOF(t *testing.T) {
	cb, err := StartConnectBridge(deafSocks(t), false)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()

	ours, theirs := net.Pipe()
	returned := make(chan struct{})
	go func() { cb.handle(theirs); close(returned) }()
	if _, err := io.WriteString(ours, "CONNECT cdn.example.net:443 HTTP/1.1\r\nHost: cdn.example.net:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(ours).ReadString('\n'); err != nil || !strings.Contains(line, "200") {
		t.Fatalf("CONNECT answered %q, %v", line, err)
	}
	_ = ours.Close()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the tunnel outlived its client, waiting on a peer that never closes")
	}
}

func TestCloseSeversLiveTunnels(t *testing.T) {
	cb, err := StartConnectBridge(deafSocks(t), false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", strings.TrimPrefix(cb.ProxyURL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "CONNECT cdn.example.net:443 HTTP/1.1\r\nHost: cdn.example.net:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	if line, err := br.ReadString('\n'); err != nil || !strings.Contains(line, "200") {
		t.Fatalf("CONNECT answered %q, %v", line, err)
	}

	closed := make(chan error, 1)
	go func() { closed <- cb.Close() }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() waited on a live tunnel instead of severing it")
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.Copy(io.Discard, br); err != nil {
		t.Errorf("the client was not cut off when the bridge closed: %v", err)
	}
}

func TestDNSProbeNeverAsksForSoundCloud(t *testing.T) {
	for _, protected := range []string{"soundcloud", "sndcdn"} {
		if strings.Contains(strings.ToLower(probeName), protected) {
			t.Errorf("audit's local DNS probe resolves %q — a plaintext lookup of a protected name", probeName)
		}
	}
}

func TestConnectRequestNeverResolvesHostnames(t *testing.T) {
	req, err := connectRequest("api-v2.soundcloud.com:443")
	if err != nil {
		t.Fatalf("connectRequest: %v", err)
	}
	if req[3] != 0x03 {
		t.Fatalf("ATYP = %#02x, want 0x03 (domain)", req[3])
	}
	if got := string(req[5 : 5+req[4]]); got != "api-v2.soundcloud.com" {
		t.Errorf("host = %q, want literal hostname", got)
	}
	if p := int(req[len(req)-2])<<8 | int(req[len(req)-1]); p != 443 {
		t.Errorf("port = %d, want 443", p)
	}

	req, err = connectRequest("127.0.0.1:9050")
	if err != nil {
		t.Fatalf("connectRequest(ip): %v", err)
	}
	if req[3] != 0x01 {
		t.Errorf("ATYP for IPv4 literal = %#02x, want 0x01", req[3])
	}
}

func TestAssertFailsClosed(t *testing.T) {
	cases := []struct {
		name             string
		verified, direct bool
		allowDirect      bool
		wantAllowed      bool
	}{
		{"fresh bridge, Up never called", false, false, false, false},
		{"verified private path", true, false, false, true},
		{"direct without opt-in", false, true, false, false},
		{"direct WITH --allow-direct", false, true, true, true},
		{"allow-direct set but never fell back", false, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bridge{verified: tc.verified, direct: tc.direct, allowDirect: tc.allowDirect}
			err := b.Assert()
			if tc.wantAllowed && err != nil {
				t.Fatalf("Assert() = %v, want nil (egress should be permitted)", err)
			}
			if !tc.wantAllowed && !errors.Is(err, ErrEgressNotVerified) {
				t.Fatalf("Assert() = %v, want ErrEgressNotVerified (must fail closed)", err)
			}
		})
	}
}

func TestUpRefusesDirectWithoutOptIn(t *testing.T) {
	b := &Bridge{}
	b.allowDirect = false
	if err := b.fallback("no tor for test"); err == nil {
		t.Fatal("fallback() = nil, want a refusal when --allow-direct is absent")
	}
	if b.Assert() == nil {
		t.Fatal("Assert() allowed egress after a refused fallback")
	}

	b2 := &Bridge{allowDirect: true}
	if err := b2.fallback("no tor for test"); err != nil {
		t.Fatalf("fallback() with --allow-direct = %v, want nil", err)
	}
	if b2.Verified() {
		t.Error("Verified() = true after --allow-direct; DIRECT is permitted, never private")
	}
	if err := b2.Assert(); err != nil {
		t.Errorf("Assert() = %v, want nil once the user opted in", err)
	}
}

func TestUpIsIdempotent(t *testing.T) {
	b := &Bridge{verified: true, proxy: "http://127.0.0.1:1", torSocks: "127.0.0.1:1"}
	if err := b.Up(); err != nil {
		t.Fatalf("second Up() = %v, want nil: egress is already up in this process", err)
	}
	if got := b.ProxyURL(); got != "http://127.0.0.1:1" {
		t.Errorf("second Up() moved the proxy to %q; queued tracks still point at the first", got)
	}
}

func TestSocksErrorCarriesCode(t *testing.T) {
	wrapped := fmt.Errorf("egress self-test failed: %w", &socksError{Code: socksGeneralFailure})
	var se *socksError
	if !errors.As(wrapped, &se) {
		t.Fatal("errors.As could not recover socksError through a wrap")
	}
	if se.Code != socksGeneralFailure {
		t.Errorf("Code = %#02x, want %#02x", se.Code, socksGeneralFailure)
	}
	if !strings.Contains(se.Error(), "bootstrapping") {
		t.Errorf("0x01 message should name bootstrapping, got %q", se.Error())
	}
}

func TestWaitUsableStopsOnTerminalError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				hdr := make([]byte, 2)
				if _, err := io.ReadFull(c, hdr); err != nil {
					return
				}
				if _, err := io.ReadFull(c, make([]byte, hdr[1])); err != nil {
					return
				}
				_, _ = c.Write([]byte{0x05, 0x00})
				req := make([]byte, 4)
				if _, err := io.ReadFull(c, req); err != nil {
					return
				}
				l := make([]byte, 1)
				if _, err := io.ReadFull(c, l); err != nil {
					return
				}
				if _, err := io.ReadFull(c, make([]byte, int(l[0])+2)); err != nil {
					return
				}
				_, _ = c.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			}(c)
		}
	}()

	cb, err := StartConnectBridge(ln.Addr().String(), false)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer cb.Close()
	b := &Bridge{cb: cb}

	start := time.Now()
	err = b.waitUsable(context.Background(), "api-v2.soundcloud.com:443")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitUsable() = nil, want the 0x04 failure")
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s — a terminal SOCKS code must abort, not consume the retry budget", elapsed)
	}
}

func TestReisolate(t *testing.T) {
	d := &socks5Dialer{user: "chainsmoke", pass: isolationPass()}
	u1, p1 := d.creds()

	if !d.reisolate() {
		t.Fatal("reisolate() = false on an isolating dialer, want true")
	}
	u2, p2 := d.creds()
	if u2 != u1 {
		t.Errorf("username changed (%q -> %q); only the password keys a new circuit", u1, u2)
	}
	if p2 == p1 {
		t.Error("password unchanged — Tor would hand back the SAME circuit")
	}

	seen := map[string]bool{p1: true, p2: true}
	for i := 0; i < 200; i++ {
		d.reisolate()
		_, p := d.creds()
		if seen[p] {
			t.Fatalf("duplicate isolation key after %d rolls: %q", i, p)
		}
		seen[p] = true
	}
}

func TestReisolateWithoutIsolation(t *testing.T) {
	d := &socks5Dialer{}
	if d.reisolate() {
		t.Error("reisolate() = true with isolation off, want false")
	}
	if u, p := d.creds(); u != "" || p != "" {
		t.Errorf("creds() = %q/%q, want empty — a no-auth dialer must stay no-auth", u, p)
	}
}

func TestReisolateConcurrent(t *testing.T) {
	d := &socks5Dialer{user: "chainsmoke", pass: isolationPass()}
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			d.reisolate()
		}
	}()
	for i := 0; i < 500; i++ {
		if u, _ := d.creds(); u != "chainsmoke" {
			t.Errorf("creds() saw a torn username %q", u)
			break
		}
	}
	<-done
}

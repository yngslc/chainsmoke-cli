package net

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeSocks stands in for Tor: it completes a SOCKS5 handshake, records what it
// was asked to connect to, and then echoes. No network, no tor, no mpv — so
// these run in CI and on Windows (TASKS.md T9.1/T9.2).
type fakeSocks struct {
	ln       net.Listener
	gotHost  string // destination as it arrived on the wire
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

	// greeting: ver, nmethods, methods...
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
		// RFC1929: ver, ulen, uname, plen, passwd
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

	// CONNECT: ver, cmd, rsv, atyp, addr, port
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
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil { // port
		return
	}
	// success reply, ATYP=IPv4 0.0.0.0:0
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_, _ = io.Copy(c, c) // echo, so the tunnel can be proven to carry bytes
}

// TestConnectBridgeTunnelsAndKeepsDNSRemote is the load-bearing one: it proves
// the hostname reaches Tor as ATYP=0x03 bytes and is never resolved locally.
// That is the DNS-leak guarantee (T2.5) asserted rather than assumed.
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

// TestConnectBridgeRefusesPlaintextProxying: an absolute-form GET means a child
// is about to push cleartext through us. Refusing makes a leak a loud failure.
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

// TestConnectBridgeNegotiatesIsolation: with isolate on, the bridge must offer
// ONLY username/password. Offering 0x00 as well would let Tor pick no-auth and
// silently share a circuit — the thing IsolateSOCKSAuth exists to prevent.
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

// TestConnectRequestNeverResolvesHostnames pins the encoder directly: a
// hostname must go out as a domain, an IP literal as an IP.
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

// --- T2.3: fail-closed gate ---------------------------------------------------

// TestAssertFailsClosed pins the direction of the gate. An earlier draft had
// `if !b.verify == false`, which returned the error exactly when egress WAS
// verified — a security check that failed open. This test would have caught it.
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

// TestUpRefusesDirectWithoutOptIn: with no tor reachable and no --allow-direct,
// Up must return an error rather than warn and continue. This is T0.2.
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

// TestSocksErrorCarriesCode: waitUsable branches on the raw reply byte, so the
// code has to survive wrapping. String matching would be fragile.
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

// TestWaitUsableStopsOnTerminalError: 0x01 means "still booting, retry"; every
// other code is a real fault and must abort immediately rather than burn the
// 45s budget. A fake SOCKS server that always answers 0x04 must fail fast.
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
				_, _ = c.Write([]byte{0x05, 0x00}) // no-auth
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
				// 0x04 host unreachable — terminal, not a warmup symptom
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

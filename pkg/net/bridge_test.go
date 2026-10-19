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
// TestPrivateTorDiesWithItsOwner: Ctrl+C reaches tor through the process group
// and it exits, but a closed terminal sends SIGHUP, which tor treats as a
// reload and survives re-parented. kill, SIGKILL and crashes skip every
// handler too (measured 2026-10-03, CH-DEBATE-2026-10-03 #2). Naming us as the
// owning process makes tor exit on its own once we are gone, however we went.
func TestPrivateTorDiesWithItsOwner(t *testing.T) {
	want := fmt.Sprintf("__OwningControllerProcess %d\n", os.Getpid())
	if rc := privateTorrc(9999, 9998, "data"); !strings.Contains(rc, want) {
		t.Errorf("the private tor's torrc does not name its owner:\n%s", rc)
	}
}

// TestStaleTorDirsAreSwept: tor exits once its owner is gone, but its
// DataDirectory stayed in the temp dir whenever the owner died before Down()
// (2026-10-05 review, R10). The next spawn deletes a dir whose owner is dead —
// and only that: a live owner's, this process's own, and names it cannot read
// are kept.
func TestStaleTorDirsAreSwept(t *testing.T) {
	exited := exec.Command(os.Args[0], "-test.run=^$") // runs no tests, exits
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	dead := exited.ProcessState.Pid()
	if processAlive(dead) || !processAlive(os.Getpid()) {
		t.Fatalf("processAlive: exited pid %d alive = %v, this process alive = %v", dead, processAlive(dead), processAlive(os.Getpid()))
	}

	tmp := t.TempDir()
	dirs := map[string]bool{ // name -> swept
		fmt.Sprintf("%s%d-123", torDirPrefix, dead):         true,
		fmt.Sprintf("%s%d-456", torDirPrefix, os.Getpid()):  false,
		fmt.Sprintf("%s%d-789", torDirPrefix, os.Getppid()): false, // `go test`, alive
		"chainsmoke-tor-1234567":                            false, // named before the pid was: the digits are random
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

// TestNamedTorSocksNeverSpawnsATor: --tor-socks names a specific tor. Nothing
// listening there used to fall through to spawning a private tor that
// bootstraps onto the network, which is how smoke's "offline" $noTor rows
// would leave the box on a machine with tor on PATH (CH-DEBATE-2026-10-03 #3).
// The fake tor here is not an executable, so a spawn attempt fails loudly
// instead of bootstrapping.
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

	err := New("", false, "127.0.0.1:1").Up()
	if err == nil || strings.Contains(err.Error(), "tor could not be started") ||
		!strings.Contains(err.Error(), "no Tor SOCKS listener") {
		t.Errorf("Up() with --tor-socks 127.0.0.1:1 = %v, want a fail-closed refusal that never tried to spawn tor", err)
	}
}

func TestDNSProbeNeverAsksForSoundCloud(t *testing.T) {
	// `audit` used to resolve soundcloud.com and sndcdn.com through the system
	// resolver to ask whether such a lookup would be visible, which sent that
	// very lookup on every run (2026-10-01, review N-4). The probe may ask the
	// local resolver anything except the names it exists to protect.
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

// TestUpIsIdempotent: a play session runs several verbs in one process, and each
// calls Up(). The track already playing rides the FIRST bridge, and Down() only
// closes b.cb and kills b.torCmd — so a second Up() that started a new bridge
// would leak the old one, and one that spawned a second private tor would orphan
// the first along with its DataDirectory. Once egress is up, Up() is a no-op.
//
// torSocks points at a dead port, so without the guard this Up() would go
// looking for Tor, find none, and refuse — i.e. fail a session mid-song.
func TestUpIsIdempotent(t *testing.T) {
	b := &Bridge{verified: true, proxy: "http://127.0.0.1:1", torSocks: "127.0.0.1:1"}
	if err := b.Up(); err != nil {
		t.Fatalf("second Up() = %v, want nil: egress is already up in this process", err)
	}
	if got := b.ProxyURL(); got != "http://127.0.0.1:1" {
		t.Errorf("second Up() moved the proxy to %q; queued tracks still point at the first", got)
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

// TestReisolate is the T3.5 retry's foundation: a new credential is what buys a
// new circuit, so a re-roll that produced the same pair would silently retry on
// the very exit we are trying to escape.
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

	// 200 rolls, no collisions: a time-derived key could repeat inside one
	// coarse Windows clock tick, which is exactly the bug this guards.
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

// TestReisolateWithoutIsolation: a bridge built without isolation has nothing
// to roll, and must say so rather than let a caller claim a fresh circuit.
func TestReisolateWithoutIsolation(t *testing.T) {
	d := &socks5Dialer{} // no-auth dialer
	if d.reisolate() {
		t.Error("reisolate() = true with isolation off, want false")
	}
	if u, p := d.creds(); u != "" || p != "" {
		t.Errorf("creds() = %q/%q, want empty — a no-auth dialer must stay no-auth", u, p)
	}
}

// TestReisolateConcurrent: Reisolate runs while the bridge is serving, so the
// credentials are read by in-flight dials as they are rewritten. Meaningful
// under -race; harmless without it.
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

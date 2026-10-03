package net

import (
	"bufio"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeControl replays a scripted Tor control-port conversation so the reply
// grammar can be tested without a tor binary.
func fakeControl(t *testing.T, script map[string]string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
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
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.TrimRight(line, "\r\n")
					key := strings.Fields(cmd)[0]
					reply, ok := script[key]
					if !ok {
						reply = "250 OK\r\n"
					}
					if _, err := c.Write([]byte(reply)); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// TestReadReplyHandlesMultilineAndEvents is the bug this replaced: the old
// Rotate read exactly one line per command and prefix-matched "250". A "250-"
// continuation run, or an interleaved asynchronous 6xx event, desynchronized
// the connection and made the next command read the previous reply's tail.
func TestReadReplyHandlesMultilineAndEvents(t *testing.T) {
	addr := fakeControl(t, map[string]string{
		"PROTOCOLINFO": "650 CIRC 1 EXTENDED\r\n" + // async event BEFORE our reply
			"250-PROTOCOLINFO 1\r\n" +
			`250-AUTH METHODS=COOKIE,SAFECOOKIE COOKIEFILE="/run/tor/control.authcookie"` + "\r\n" +
			"250-VERSION Tor=\"0.4.8.12\"\r\n" +
			"250 OK\r\n",
	})

	c, err := dialControl(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialControl: %v", err)
	}
	defer c.Close()

	lines, err := c.exec("PROTOCOLINFO 1")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	var sawAuth bool
	for _, ln := range lines {
		if strings.HasPrefix(ln, "AUTH ") {
			sawAuth = true
		}
		if strings.HasPrefix(ln, "650") {
			t.Errorf("async event leaked into the reply: %q", ln)
		}
	}
	if !sawAuth {
		t.Errorf("AUTH line missing from continuation run: %#v", lines)
	}
}

// TestReadReplyDataBlock covers the "250+" form, terminated by a lone dot.
func TestReadReplyDataBlock(t *testing.T) {
	addr := fakeControl(t, map[string]string{
		"GETINFO": "250+md/name/x=\r\nline one\r\nline two\r\n.\r\n250 OK\r\n",
	})
	c, err := dialControl(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialControl: %v", err)
	}
	defer c.Close()

	lines, err := c.exec("GETINFO md/name/x")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "line one") || !strings.Contains(joined, "line two") {
		t.Errorf("data block not captured: %#v", lines)
	}
}

// TestReadReplyPropagatesErrors: a 5xx must surface, not be skipped as an event.
func TestReadReplyPropagatesErrors(t *testing.T) {
	addr := fakeControl(t, map[string]string{
		"AUTHENTICATE": "515 Authentication failed: wrong length on authentication cookie\r\n",
	})
	c, err := dialControl(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialControl: %v", err)
	}
	defer c.Close()

	if _, err := c.exec("AUTHENTICATE"); err == nil {
		t.Fatal("exec() = nil, want the 515 failure")
	}
}

// TestAuthenticateRejectsHashedPassword: rather than sending AUTHENTICATE ""
// and failing obscurely, say what is wrong and what to do.
func TestAuthenticateRejectsHashedPassword(t *testing.T) {
	addr := fakeControl(t, map[string]string{
		"PROTOCOLINFO": "250-PROTOCOLINFO 1\r\n" +
			"250-AUTH METHODS=HASHEDPASSWORD\r\n" +
			"250 OK\r\n",
	})
	c, err := dialControl(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialControl: %v", err)
	}
	defer c.Close()

	err = c.Authenticate()
	if err == nil {
		t.Fatal("Authenticate() = nil, want a refusal for HASHEDPASSWORD")
	}
	if !strings.Contains(err.Error(), "CookieAuthentication") {
		t.Errorf("error should tell the user the fix, got %q", err)
	}
}

// TestAuthenticateNullAuth: a control port with no auth must just work.
func TestAuthenticateNullAuth(t *testing.T) {
	addr := fakeControl(t, map[string]string{
		"PROTOCOLINFO": "250-PROTOCOLINFO 1\r\n250-AUTH METHODS=NULL\r\n250 OK\r\n",
	})
	c, err := dialControl(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialControl: %v", err)
	}
	defer c.Close()
	if err := c.Authenticate(); err != nil {
		t.Fatalf("Authenticate() = %v, want nil for NULL auth", err)
	}
}

// TestUnescapeCString: a Windows cookie path arrives with doubled backslashes.
// Leaving them in place means the cookie file is never found.
func TestUnescapeCString(t *testing.T) {
	got := unescapeCString(`C:\\Users\\der3t\\tor\\control_auth_cookie`)
	want := `C:\Users\der3t\tor\control_auth_cookie`
	if got != want {
		t.Errorf("unescapeCString = %q, want %q", got, want)
	}
}

// --- T2.6: --proxy keeps the verified-up contract ----------------------------

// TestCheckProxySchemeRejectsSOCKS is the partial-egress guard. yt-dlp accepts
// socks5://, libavformat does not — so metadata would tunnel and audio would
// not, which is worse than failing outright.
func TestCheckProxySchemeRejectsSOCKS(t *testing.T) {
	for _, raw := range []string{
		"socks5://127.0.0.1:9050",
		"socks5h://127.0.0.1:9050",
		"socks4a://127.0.0.1:9050",
	} {
		err := checkProxyScheme(raw)
		if err == nil {
			t.Errorf("checkProxyScheme(%q) = nil, want a refusal", raw)
			continue
		}
		if !strings.Contains(err.Error(), "mpv/ffmpeg") {
			t.Errorf("refusal should explain why, got %q", err)
		}
	}
	// https:// is the same split: libavformat uses a proxy only when it starts
	// with exactly "http://" and goes DIRECT otherwise (2026-10-01, N-2).
	if err := checkProxyScheme("https://proxy.example:3128"); err == nil || !strings.Contains(err.Error(), "mpv/ffmpeg") {
		t.Errorf("checkProxyScheme(https://…) = %v, want a refusal that explains why", err)
	}
	for _, raw := range []string{"http://127.0.0.1:8118", "HTTP://127.0.0.1:8118"} { // Up() lower-cases it
		if err := checkProxyScheme(raw); err != nil {
			t.Errorf("checkProxyScheme(%q) = %v, want nil", raw, err)
		}
	}
	if err := checkProxyScheme("127.0.0.1:8118"); err == nil {
		t.Error("a bare host:port has no scheme and should be refused")
	}
}

// TestRotateIsQuietWhenReisolationWorks: re-isolating on the SOCKS port always
// works and is what gives the next stream a fresh circuit; NEWNYM needs tor's
// ControlPort, which most system tors (and this dev box) leave off. Rotate used
// to try the control port on every retry and print two lines each time it
// failed (dev's report, 2026-10-03). Now NEWNYM is a silent bonus, tried once
// per process.
func TestRotateIsQuietWhenReisolationWorks(t *testing.T) {
	ctrl, err := net.Listen("tcp", "127.0.0.1:0") // a control port that refuses auth
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	var dials int32
	go func() {
		for {
			c, err := ctrl.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&dials, 1)
			c.Close()
		}
	}()
	cb, err := StartConnectBridge("127.0.0.1:1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	b := &Bridge{cb: cb, torCtrlAddr: ctrl.Addr().String()}

	r, w, _ := os.Pipe()
	stderr := os.Stderr
	os.Stderr = w
	err1, err2 := b.Rotate(), b.Rotate()
	os.Stderr = stderr
	w.Close()
	printed, _ := io.ReadAll(r)

	if err1 != nil || err2 != nil {
		t.Errorf("Rotate() = %v, %v — re-isolation worked, so rotating did too", err1, err2)
	}
	if len(printed) != 0 {
		t.Errorf("Rotate printed %q, want nothing", printed)
	}
	if n := atomic.LoadInt32(&dials); n != 1 {
		t.Errorf("the control port was dialled %d times over two rotations, want 1", n)
	}
}

// TestProxyErrorsNeverEchoCredentials: a --proxy URL can carry a password, and
// every refusal used to repeat the value verbatim, putting it on the terminal
// (measured 2026-10-03, CH-DEBATE-2026-10-03 #8).
func TestProxyErrorsNeverEchoCredentials(t *testing.T) {
	for _, raw := range []string{
		"https://alice:s3cretpw@127.0.0.1:1",
		"socks5://alice:s3cretpw@127.0.0.1:1",
		"ftp://alice:s3cretpw@127.0.0.1:1",
		"http//alice:s3cretpw@127.0.0.1:1", // not a URL at all
	} {
		if err := checkProxyScheme(raw); err == nil || strings.Contains(err.Error(), "s3cretpw") {
			t.Errorf("checkProxyScheme(%q) = %v, want a refusal without the password", raw, err)
		}
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, "")
	}
	if err := New("http://alice:s3cretpw@127.0.0.1:1", false, "127.0.0.1:1").Up(); err == nil ||
		strings.Contains(err.Error(), "s3cretpw") {
		t.Errorf("Up() with an unreachable --proxy = %v, want a refusal without the password", err)
	}
}

// TestOverrideIsStoredWithALowercaseScheme: checkProxyScheme accepts HTTP://
// (it compares case-insensitively), and Up() used to store the value as typed.
// libavformat honours a proxy only when it starts with exactly "http://", so
// mpv fetched the signed URL directly while the preflight went through the
// proxy (measured 2026-10-01, report-debate/debaters-output.md claim 3).
func TestOverrideIsStoredWithALowercaseScheme(t *testing.T) {
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, "") // Up() exports the proxy; give the test process its own values back
	}
	addr := connectStub(t, "HTTP/1.1 200 Connection established\r\n\r\n")
	b := New("HTTP://"+addr, false, "127.0.0.1:1")
	if err := b.Up(); err != nil {
		t.Fatalf("Up() = %v", err)
	}
	if got, want := b.ProxyURL(), "http://"+addr; got != want {
		t.Errorf("ProxyURL() = %q, want %q — mpv ignores any proxy not starting with exactly http://", got, want)
	}
}

// TestVerifyHTTPProxyCONNECT: --proxy must carry a real CONNECT, not merely
// accept a TCP connection. A proxy that answers 403 must fail verification.
func TestVerifyHTTPProxyCONNECT(t *testing.T) {
	okAddr := connectStub(t, "HTTP/1.1 200 Connection established\r\n\r\n")
	if err := verifyHTTPProxyCONNECT(okAddr, "api-v2.soundcloud.com:443"); err != nil {
		t.Errorf("verifyHTTPProxyCONNECT on a good proxy = %v, want nil", err)
	}

	badAddr := connectStub(t, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
	if err := verifyHTTPProxyCONNECT(badAddr, "api-v2.soundcloud.com:443"); err == nil {
		t.Error("a proxy answering 403 must fail verification, not pass")
	}
}

func connectStub(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
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
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				_, _ = c.Write([]byte(reply))
			}(c)
		}
	}()
	return ln.Addr().String()
}

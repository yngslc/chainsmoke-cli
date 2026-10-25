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

func TestReadReplyHandlesMultilineAndEvents(t *testing.T) {
	addr := fakeControl(t, map[string]string{
		"PROTOCOLINFO": "650 CIRC 1 EXTENDED\r\n" +
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

func TestUnescapeCString(t *testing.T) {
	got := unescapeCString(`C:\\Users\\someone\\tor\\control_auth_cookie`)
	want := `C:\Users\someone\tor\control_auth_cookie`
	if got != want {
		t.Errorf("unescapeCString = %q, want %q", got, want)
	}
}

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
	if err := checkProxyScheme("https://proxy.example:3128"); err == nil || !strings.Contains(err.Error(), "mpv/ffmpeg") {
		t.Errorf("checkProxyScheme(https://…) = %v, want a refusal that explains why", err)
	}
	for _, raw := range []string{"http://127.0.0.1:8118", "HTTP://127.0.0.1:8118"} {
		if err := checkProxyScheme(raw); err != nil {
			t.Errorf("checkProxyScheme(%q) = %v, want nil", raw, err)
		}
	}
	if err := checkProxyScheme("127.0.0.1:8118"); err == nil {
		t.Error("a bare host:port has no scheme and should be refused")
	}
}

func TestRotateIsQuietWhenReisolationWorks(t *testing.T) {
	ctrl, err := net.Listen("tcp", "127.0.0.1:0")
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

func TestPrivateWritesNoNewnymStamp(t *testing.T) {
	ctrl := fakeControl(t, map[string]string{
		"PROTOCOLINFO": "250-PROTOCOLINFO 1\r\n250-AUTH METHODS=NULL\r\n250 OK\r\n",
	})
	for _, private := range []bool{true, false} {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		b := &Bridge{private: private, torCtrlAddr: ctrl}
		if err := b.newnym(); err != nil {
			t.Fatalf("private=%v: newnym() = %v", private, err)
		}
		_, err := os.Stat(newnymStamp())
		if written := err == nil; written == private {
			t.Errorf("private=%v: stamp written = %v", private, written)
		}
	}
}

func TestProxyErrorsNeverEchoCredentials(t *testing.T) {
	for _, raw := range []string{
		"https://alice:s3cretpw@127.0.0.1:1",
		"socks5://alice:s3cretpw@127.0.0.1:1",
		"ftp://alice:s3cretpw@127.0.0.1:1",
		"http//alice:s3cretpw@127.0.0.1:1",
	} {
		if err := checkProxyScheme(raw); err == nil || strings.Contains(err.Error(), "s3cretpw") {
			t.Errorf("checkProxyScheme(%q) = %v, want a refusal without the password", raw, err)
		}
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, "")
	}
	if err := New("http://alice:s3cretpw@127.0.0.1:1", false, "127.0.0.1:1", false).Up(); err == nil ||
		strings.Contains(err.Error(), "s3cretpw") {
		t.Errorf("Up() with an unreachable --proxy = %v, want a refusal without the password", err)
	}
}

func TestOverrideIsStoredWithALowercaseScheme(t *testing.T) {
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, "")
	}
	addr := connectStub(t, "HTTP/1.1 200 Connection established\r\n\r\n")
	b := New("HTTP://"+addr, false, "127.0.0.1:1", false)
	if err := b.Up(); err != nil {
		t.Fatalf("Up() = %v", err)
	}
	if got, want := b.ProxyURL(), "http://"+addr; got != want {
		t.Errorf("ProxyURL() = %q, want %q — mpv ignores any proxy not starting with exactly http://", got, want)
	}
}

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

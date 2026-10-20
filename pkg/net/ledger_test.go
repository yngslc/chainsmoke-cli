package net

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func connectVia(t *testing.T, b *ConnectBridge, request string) int {
	t.Helper()
	c, err := net.DialTimeout("tcp", strings.TrimPrefix(b.ProxyURL(), "http://"), 2*time.Second)
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer c.Close()

	if _, err := io.WriteString(c, request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestEgressLedgerRecordsTunnels(t *testing.T) {
	fs := newFakeSocks(t, false)
	cb, err := StartConnectBridge(fs.addr(), false)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer cb.Close()

	if got := connectVia(t, cb, "CONNECT api-v2.soundcloud.com:443 HTTP/1.1\r\nHost: api-v2.soundcloud.com:443\r\n\r\n"); got != http.StatusOK {
		t.Fatalf("CONNECT = %d, want 200", got)
	}
	if got := connectVia(t, cb, "CONNECT api-v2.soundcloud.com:443 HTTP/1.1\r\nHost: api-v2.soundcloud.com:443\r\n\r\n"); got != http.StatusOK {
		t.Fatalf("second CONNECT = %d, want 200", got)
	}
	if got := connectVia(t, cb, "CONNECT cdn.example.net:443 HTTP/1.1\r\nHost: cdn.example.net:443\r\n\r\n"); got != http.StatusOK {
		t.Fatalf("third CONNECT = %d, want 200", got)
	}

	l := (&Bridge{cb: cb}).EgressLedger()
	if !l.Observed {
		t.Fatal("Observed = false while we own the bridge; audit would report nothing at all")
	}
	if got := l.Targets["api-v2.soundcloud.com:443"]; got != 2 {
		t.Errorf("api-v2.soundcloud.com:443 count = %d, want 2", got)
	}
	if got := l.Targets["cdn.example.net:443"]; got != 1 {
		t.Errorf("cdn.example.net:443 count = %d, want 1", got)
	}
	if l.Refused != 0 || l.Failed != 0 {
		t.Errorf("Refused/Failed = %d/%d, want 0/0 on a clean run", l.Refused, l.Failed)
	}

	unexpected := l.UnexpectedTargets()
	if len(unexpected) != 1 || unexpected[0] != "cdn.example.net:443" {
		t.Errorf("UnexpectedTargets() = %v, want [cdn.example.net:443]", unexpected)
	}
}

func TestEgressLedgerSnapshotIsACopy(t *testing.T) {
	l := newLedger()
	l.connect("api-v2.soundcloud.com:443")

	snap := l.snapshot()
	snap.Targets["injected:443"] = 99

	if _, ok := l.targets["injected:443"]; ok {
		t.Error("mutating the snapshot reached the live ledger — snapshot() shares its map")
	}
}

func TestEgressLedgerRecordsRefusals(t *testing.T) {
	fs := newFakeSocks(t, false)
	cb, err := StartConnectBridge(fs.addr(), false)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer cb.Close()

	if got := connectVia(t, cb, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n"); got != http.StatusMethodNotAllowed {
		t.Fatalf("plaintext GET = %d, want 405", got)
	}

	l := (&Bridge{cb: cb}).EgressLedger()
	if l.Refused != 1 {
		t.Errorf("Refused = %d, want 1", l.Refused)
	}
	if len(l.Targets) != 0 {
		t.Errorf("Targets = %v, want empty — a refused request opened no tunnel", l.Targets)
	}
}

func TestEgressLedgerRecordsFailures(t *testing.T) {
	cb, err := StartConnectBridge(deadAddr(t), false)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer cb.Close()

	if got := connectVia(t, cb, "CONNECT api-v2.soundcloud.com:443 HTTP/1.1\r\nHost: api-v2.soundcloud.com:443\r\n\r\n"); got != http.StatusBadGateway {
		t.Fatalf("CONNECT through a dead SOCKS = %d, want 502", got)
	}

	l := (&Bridge{cb: cb}).EgressLedger()
	if l.Failed != 1 {
		t.Errorf("Failed = %d, want 1", l.Failed)
	}
	if got := l.Targets["api-v2.soundcloud.com:443"]; got != 1 {
		t.Errorf("Targets[api-v2...] = %d, want 1 — a failed CONNECT was still an attempt", got)
	}
}

func TestEgressLedgerNormalizesPort(t *testing.T) {
	fs := newFakeSocks(t, false)
	cb, err := StartConnectBridge(fs.addr(), false)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer cb.Close()

	if got := connectVia(t, cb, "CONNECT api-v2.soundcloud.com HTTP/1.1\r\nHost: api-v2.soundcloud.com\r\n\r\n"); got != http.StatusOK {
		t.Fatalf("portless CONNECT = %d, want 200", got)
	}

	l := (&Bridge{cb: cb}).EgressLedger()
	if got := l.Targets["api-v2.soundcloud.com:443"]; got != 1 {
		t.Errorf("Targets = %v, want the dialed form api-v2.soundcloud.com:443", l.Targets)
	}
}

func TestEgressLedgerUnobservableWithoutOwnBridge(t *testing.T) {
	for _, b := range []*Bridge{
		{},
		{override: "http://127.0.0.1:8118"},
		{direct: true, allowDirect: true},
	} {
		l := b.EgressLedger()
		if l.Observed {
			t.Errorf("Observed = true with no bridge of our own (%+v)", b)
		}
		if len(l.Targets) != 0 {
			t.Errorf("Targets = %v, want empty when unobserved", l.Targets)
		}
	}
}

func TestVerifyIsAccountedFor(t *testing.T) {
	fs := newFakeSocks(t, false)
	cb, err := StartConnectBridge(fs.addr(), false)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer cb.Close()

	if err := cb.Verify("api-v2.soundcloud.com:443"); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	l := (&Bridge{cb: cb}).EgressLedger()
	if got := l.Targets["api-v2.soundcloud.com:443"]; got != 1 {
		t.Errorf("Targets = %v, want the self-test tunnel counted", l.Targets)
	}
	if l.Failed != 0 {
		t.Errorf("Failed = %d after a successful Verify, want 0", l.Failed)
	}
}

func TestVerifyFailureIsAccountedFor(t *testing.T) {
	cb, err := StartConnectBridge(deadAddr(t), false)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer cb.Close()

	if err := cb.Verify("api-v2.soundcloud.com:443"); err == nil {
		t.Fatal("Verify through a dead SOCKS = nil, want an error")
	}

	l := (&Bridge{cb: cb}).EgressLedger()
	if l.Failed != 1 {
		t.Errorf("Failed = %d, want 1", l.Failed)
	}
	if got := l.Targets["api-v2.soundcloud.com:443"]; got != 1 {
		t.Errorf("Targets = %v, want the attempt recorded", l.Targets)
	}
}

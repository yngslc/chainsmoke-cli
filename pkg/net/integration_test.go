//go:build integration

package net

import (
	"net"
	"os"
	"testing"
	"time"

	"chainsmoke/pkg/core"
)

func torSocksAddrForTest() string {
	if a := os.Getenv("CHAINSMOKE_TOR_SOCKS"); a != "" {
		return a
	}
	return "127.0.0.1:9050"
}

func requireTor(t *testing.T) string {
	t.Helper()
	addr := torSocksAddrForTest()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Skipf("no Tor SOCKS listener on %s (set CHAINSMOKE_TOR_SOCKS to override): %v", addr, err)
	}
	_ = c.Close()
	return addr
}

func TestIntegrationEgressChain(t *testing.T) {
	addr := requireTor(t)

	b := New("", false, addr, false)
	if err := b.Up(); err != nil {
		t.Fatalf("Up() against live Tor: %v", err)
	}
	defer b.Down()

	if !b.Verified() {
		t.Error("Verified() = false after a successful Up() — the path was permitted but not proven")
	}
	if err := b.Assert(); err != nil {
		t.Errorf("Assert() = %v, want nil once egress is verified", err)
	}
	if err := b.SelfTest(); err != nil {
		t.Errorf("SelfTest() = %v", err)
	}

	l := b.EgressLedger()
	if !l.Observed {
		t.Fatal("EgressLedger().Observed = false while we own the bridge")
	}
	if len(l.Targets) == 0 {
		t.Error("ledger recorded no targets after a verified Up()")
	}
	for target := range l.Targets {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			host = target
		}
		if !core.ExpectedHost(host) {
			t.Errorf("bridge opened a tunnel to %q, which is not a host chainsmoke should reach", target)
		}
	}
}

func TestIntegrationRemoteDNS(t *testing.T) {
	addr := requireTor(t)

	cb, err := StartConnectBridge(addr, true)
	if err != nil {
		t.Fatalf("StartConnectBridge: %v", err)
	}
	defer cb.Close()

	if err := cb.Verify("api-v2.soundcloud.com:443"); err != nil {
		t.Fatalf("Verify through live Tor: %v", err)
	}
}

func TestIntegrationRotateChangesCircuit(t *testing.T) {
	addr := requireTor(t)

	b := New("", false, addr, false)
	if err := b.Up(); err != nil {
		t.Fatalf("Up(): %v", err)
	}
	defer b.Down()

	if err := b.Rotate(); err != nil {
		t.Fatalf("Rotate() = %v, want nil (NEWNYM or credential re-isolation)", err)
	}
	if err := b.SelfTest(); err != nil {
		t.Errorf("SelfTest() after Rotate() = %v — rotation left egress broken", err)
	}
}

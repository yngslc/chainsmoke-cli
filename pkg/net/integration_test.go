//go:build integration

// Integration tests for the egress chain (TASKS.md T9.2).
//
// These need a LIVE Tor and they leave the machine, which is why they sit
// behind a build tag rather than a t.Short() check: the default `go test ./...`
// — the one CI runs and the one a contributor runs by reflex — must never open
// a circuit or touch SoundCloud. Opting in is explicit:
//
//	go test -tags integration ./pkg/net/
//
// Every test here SKIPS rather than fails when its dependency is absent. A
// missing Tor is not a broken build, and a red suite that means "you do not
// have tor installed" teaches people to ignore red suites.
package net

import (
	"net"
	"os"
	"testing"
	"time"

	"chainsmoke/pkg/core"
)

// torSocksAddrForTest lets the rig point somewhere other than the default —
// the Tor Browser bundle listens on 9150, and on this project's dev box tor
// runs inside WSL.
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

// TestIntegrationEgressChain is the acceptance test the unit suite cannot be:
// fakeSocks proves the bridge speaks SOCKS5 correctly, but only a real circuit
// proves the whole chain — bridge, Tor, exit, SoundCloud — carries bytes.
//
// Note what it asserts and what it does not. Up() succeeding means traffic
// moved; Verified() means it moved through a path we proved, which is the
// distinction T0.2 rests on.
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

	// The ledger must have recorded the self-test tunnel (T7.2). An empty
	// ledger here would mean the accounting misses the one tunnel every run
	// opens, which is what made a bare `audit` report nothing at all.
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

// TestIntegrationRemoteDNS is the leak test that only a real circuit can run.
// The unit suite proves the hostname leaves as ATYP=0x03 bytes; this proves
// that the resulting connection actually works, i.e. that Tor resolved it at
// the exit rather than us resolving it here and Tor merely connecting.
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

// TestIntegrationRotateChangesCircuit exercises the rotation path for real.
// It asserts only that rotation REPORTS success, not that the exit IP changed:
// Tor may legitimately hand back an equivalent circuit, and asserting on the
// exit would make this flaky for a reason that is not a defect.
func TestIntegrationRotateChangesCircuit(t *testing.T) {
	addr := requireTor(t)

	b := New("", false, addr, false)
	if err := b.Up(); err != nil {
		t.Fatalf("Up(): %v", err)
	}
	defer b.Down()

	// Rotate is allowed to fall back to re-isolating SOCKS credentials when no
	// ControlPort is available — that is the documented behavior on a system
	// tor, and it must still leave a usable path.
	if err := b.Rotate(); err != nil {
		t.Fatalf("Rotate() = %v, want nil (NEWNYM or credential re-isolation)", err)
	}
	if err := b.SelfTest(); err != nil {
		t.Errorf("SelfTest() after Rotate() = %v — rotation left egress broken", err)
	}
}

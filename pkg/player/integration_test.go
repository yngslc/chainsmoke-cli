//go:build integration

// Integration tests for the mpv daemon and its IPC (TASKS.md T9.2).
//
// Behind a build tag because these spawn a real mpv and talk to a real socket.
// They open no network connection, but they do leave a process behind if
// something goes wrong, which is not a thing `go test ./...` should risk by
// default. Opt in with:
//
//	go test -tags integration ./pkg/player/
//
// Skips rather than fails when mpv is absent — see the note in pkg/net's
// integration_test.go for why that matters.
package player

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"chainsmoke/pkg/core"
)

// permissiveNet lets the player run without an egress gate. Named distinctly
// from mpv_test.go's fakeNet because both files compile together under the tag.
type permissiveNet struct{}

func (permissiveNet) Up() error        { return nil }
func (permissiveNet) Down() error      { return nil }
func (permissiveNet) Rotate() error    { return nil }
func (permissiveNet) SelfTest() error  { return nil }
func (permissiveNet) ProxyURL() string { return "DIRECT (no proxy)" }
func (permissiveNet) Assert() error    { return nil }
func (permissiveNet) Verified() bool   { return false }

func requireMPV(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv not on PATH")
	}
}

// TestIntegrationDaemonLifecycle drives the real thing end to end: spawn,
// liveness-probe over IPC, a property round-trip, and a clean quit.
//
// The unit suite fakes the socket, which proves the protocol encoding. Only
// this proves that mpv accepts the flags we spawn it with — a rejected flag is
// a startup failure the fake can never surface.
func TestIntegrationDaemonLifecycle(t *testing.T) {
	requireMPV(t)

	// A per-test socket, so this never adopts or kills the developer's own
	// daemon on the shared default endpoint.
	sock := core.SocketPath()
	if runtime.GOOS != "windows" {
		sock = filepath.Join(t.TempDir(), "mpv.sock")
	}

	p := New(sock, permissiveNet{})
	if err := p.Ensure(); err != nil {
		t.Fatalf("Ensure(): %v", err)
	}
	t.Cleanup(func() { _ = p.Down() })

	// idle-active is true for a daemon started with --idle and nothing queued.
	// Asserting a real property value proves the round-trip carried data, not
	// merely that something answered.
	got, err := p.Command("get_property", "idle-active")
	if err != nil {
		t.Fatalf("get_property idle-active: %v", err)
	}
	if idle, ok := got.(bool); !ok || !idle {
		t.Errorf("idle-active = %v (%T), want true for a fresh --idle daemon", got, got)
	}

	// NowPlaying on an idle daemon is a state, not an error.
	if _, err := p.NowPlaying(); err != nil {
		t.Errorf("NowPlaying() on an idle daemon = %v, want nil", err)
	}

	if err := p.Down(); err != nil {
		t.Errorf("Down(): %v", err)
	}
}

// TestIntegrationEnsureIsIdempotent: `Ensure()` runs at the top of every play,
// and a second daemon on the same socket would leave one of them unreachable.
func TestIntegrationEnsureIsIdempotent(t *testing.T) {
	requireMPV(t)

	sock := core.SocketPath()
	if runtime.GOOS != "windows" {
		sock = filepath.Join(t.TempDir(), "mpv.sock")
	}

	p := New(sock, permissiveNet{})
	if err := p.Ensure(); err != nil {
		t.Fatalf("first Ensure(): %v", err)
	}
	t.Cleanup(func() { _ = p.Down() })

	if err := p.Ensure(); err != nil {
		t.Fatalf("second Ensure() = %v, want nil — a live daemon must be reused", err)
	}
	if _, err := p.Command("get_property", "idle-active"); err != nil {
		t.Errorf("daemon unreachable after a second Ensure(): %v", err)
	}
}

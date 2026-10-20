//go:build integration

package player

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"chainsmoke/pkg/core"
)

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

func TestIntegrationDaemonLifecycle(t *testing.T) {
	requireMPV(t)

	sock := core.SocketPath()
	if runtime.GOOS != "windows" {
		sock = filepath.Join(t.TempDir(), "mpv.sock")
	}

	p := New(sock, permissiveNet{})
	if err := p.Ensure(); err != nil {
		t.Fatalf("Ensure(): %v", err)
	}
	t.Cleanup(func() { _ = p.Down() })

	got, err := p.Command("get_property", "idle-active")
	if err != nil {
		t.Fatalf("get_property idle-active: %v", err)
	}
	if idle, ok := got.(bool); !ok || !idle {
		t.Errorf("idle-active = %v (%T), want true for a fresh --idle daemon", got, got)
	}

	if _, err := p.NowPlaying(); err != nil {
		t.Errorf("NowPlaying() on an idle daemon = %v, want nil", err)
	}

	if err := p.Down(); err != nil {
		t.Errorf("Down(): %v", err)
	}
}

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

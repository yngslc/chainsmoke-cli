// Package net owns the egress seam: bringing the Tor+Privoxy bridge up,
// verifying it, and exporting it to every child process. It is the only place
// that decides whether traffic may leave the machine.
//
// Note the import of the stdlib "net" below: legal (the package's own name is
// not an identifier in its own scope) but easy to misread. Callers should
// import this package with an alias, e.g. csnet "chainsmoke/pkg/net".
package net

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"chainsmoke/pkg/core"
)

// Bridge is the built-in egress path: Tor's SOCKS port (127.0.0.1:9050; 9150
// under the Tor Browser bundle) fronted by Privoxy, because mpv/ffmpeg have NO
// SOCKS support (INSTRUCTIONS.md "Proxy limitation") and must reach Tor as an
// ordinary HTTP CONNECT proxy.
type Bridge struct {
	override string // --proxy value, if the user supplied one
	proxy    string // egress proxy URL once Up() succeeds ("" until then)
	direct   bool   // true when no proxy was found and we fell back to direct
	up       bool   // true once Up() has run to completion in this process
}

// Compile-time proof that Bridge still satisfies the interface the rest of the
// app is wired against. Without this, a signature drift only shows up at the
// wiring site in cmd/, far from the cause.
var _ core.Net = (*Bridge)(nil)

// New builds a bridge. override is the --proxy value ("" for the built-in path).
func New(override string) *Bridge { return &Bridge{override: override} }

const (
	privoxyAddr = "127.0.0.1:8118" // Privoxy HTTP<-SOCKS bridge; what children actually use
	torCtrlAddr = "127.0.0.1:9051" // Tor control port, for NEWNYM
	dialTimeout = 3 * time.Second
)

// Up brings the egress proxy online, in precedence order:
//  1. --proxy override: trust it, just verify it accepts a TCP connection.
//  2. built-in bridge: require Privoxy on :8118 (mpv/ffmpeg need HTTP CONNECT,
//     not Tor's raw SOCKS), then export it into the child-process environment.
//  3. neither reachable: fall back to a DIRECT connection with a loud warning so
//     raw playback still works. This deliberately relaxes the fail-closed egress
//     invariant (TASKS.md T0.2) for development — tighten before shipping.
//
// KNOWN GAP (T0.2/T2.3): the readiness test here is "something accepts TCP",
// which is weaker than the contract requires — a real CONNECT through the chain
// is the actual invariant. Phase 2 replaces dialCheck with that.
func (b *Bridge) Up() error {
	b.up = true
	if b.override != "" {
		if err := dialCheck(proxyHostPort(b.override)); err != nil {
			return fmt.Errorf("--proxy %s not reachable: %w", b.override, err)
		}
		b.proxy = b.override
		exportProxyEnv(b.proxy)
		return nil
	}
	if err := dialCheck(privoxyAddr); err == nil {
		b.proxy = "http://" + privoxyAddr
		exportProxyEnv(b.proxy)
		return nil
	}
	b.direct = true
	fmt.Fprintf(os.Stderr, "chainsmoke: WARNING no egress proxy (Privoxy not on %s, no --proxy)\n", privoxyAddr)
	fmt.Fprintln(os.Stderr, "chainsmoke:         traffic will go DIRECT and is NOT anonymized — start tor+privoxy or pass --proxy")
	return nil
}

// Down tears down only what we own. We currently detect (never spawn) tor and
// privoxy, so there is nothing of ours to kill.
func (b *Bridge) Down() error { return nil }

// Rotate requests a fresh Tor circuit over the control port (NEWNYM). Assumes a
// local ControlPort with empty-password auth (the common dev setup); Tor rate-
// limits NEWNYM, so rapid calls may reuse the same circuit.
//
// KNOWN GAP (T2.4): the spec's reference design is PROTOCOLINFO -> cookie auth,
// and this single-line read desyncs on Tor's multi-line "250-" replies.
func (b *Bridge) Rotate() error {
	c, err := net.DialTimeout("tcp", torCtrlAddr, dialTimeout)
	if err != nil {
		return fmt.Errorf("tor control port %s unreachable (enable ControlPort): %w", torCtrlAddr, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(dialTimeout))
	br := bufio.NewReader(c)
	for _, step := range []string{"AUTHENTICATE \"\"\r\n", "SIGNAL NEWNYM\r\n"} {
		if _, err := fmt.Fprint(c, step); err != nil {
			return err
		}
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if !strings.HasPrefix(line, "250") {
			return fmt.Errorf("tor: %s", strings.TrimSpace(line))
		}
	}
	return nil
}

// SelfTest asserts the egress path is actually usable. In direct mode there is
// nothing anonymized to test, so it says so rather than falsely passing. The
// full DNS-leak assertion (TASKS.md T2.5) is still TODO.
func (b *Bridge) SelfTest() error {
	if !b.up {
		return errors.New("egress not brought up in this process — run a verb that calls Up() first")
	}
	if b.proxy == "" {
		return errors.New("egress is DIRECT — no proxy to test (start tor+privoxy or pass --proxy)")
	}
	return dialCheck(proxyHostPort(b.proxy))
}

func (b *Bridge) ProxyURL() string {
	switch {
	case b.direct:
		return "DIRECT (no proxy)"
	case b.proxy == "":
		return "<down>"
	default:
		return b.proxy
	}
}

// --- helpers -----------------------------------------------------------------

// dialCheck confirms something is listening at host:port.
func dialCheck(hostport string) error {
	c, err := net.DialTimeout("tcp", hostport, dialTimeout)
	if err != nil {
		return err
	}
	return c.Close()
}

// proxyHostPort extracts host:port from a proxy URL (scheme://host:port), or
// returns the input unchanged if it is already a bare host:port.
func proxyHostPort(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// exportProxyEnv makes child processes inherit the proxy. yt-dlp honors the
// upper-case vars; mpv/ffmpeg (libavformat) read lower-case http_proxy
// (INSTRUCTIONS.md "mpv wiring"), so set both cases.
func exportProxyEnv(p string) {
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		_ = os.Setenv(k, p)
	}
}

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
)

type Net interface {
	Up() error       // start Tor+Privoxy bridge, export proxy env, block until ready
	Down() error     // tear down anything we own
	Rotate() error   // NEWNYM: new Tor circuit (rate-limited by Tor)
	SelfTest() error // assert no plaintext DNS leaks for soundcloud hosts
	ProxyURL() string
}

// -----------------------------------------------------------------------------
// Stub concrete implementations (each -> its pkg/*). Bodies return ErrNotImplemented
// but the SIGNATURES and wiring are final. Fill these in one subsystem at a time,
// network layer first per the spec's "network before playback" ordering.
// -----------------------------------------------------------------------------

// torBridge is the built-in egress path: Tor's SOCKS port (127.0.0.1:9050;
// 9150 under the Tor Browser bundle) fronted by Privoxy, because mpv/ffmpeg have
// NO SOCKS support (INSTRUCTIONS.md "Proxy limitation") and must reach Tor as an
// ordinary HTTP CONNECT proxy.
type torBridge struct {
	override string // --proxy value, if the user supplied one
	proxy    string // egress proxy URL once Up() succeeds ("" until then)
	direct   bool   // true when no proxy was found and we fell back to a direct connection
}

const (
	privoxyAddr = "127.0.0.1:8118" // Privoxy HTTP<-SOCKS bridge; the endpoint children actually use
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
func (t *torBridge) Up() error {
	if t.override != "" {
		if err := dialCheck(proxyHostPort(t.override)); err != nil {
			return fmt.Errorf("--proxy %s not reachable: %w", t.override, err)
		}
		t.proxy = t.override
		exportProxyEnv(t.proxy)
		return nil
	}
	if err := dialCheck(privoxyAddr); err == nil {
		t.proxy = "http://" + privoxyAddr
		exportProxyEnv(t.proxy)
		return nil
	}
	t.direct = true
	fmt.Fprintf(os.Stderr, "chainsmoke: WARNING no egress proxy (Privoxy not on %s, no --proxy)\n", privoxyAddr)
	fmt.Fprintln(os.Stderr, "chainsmoke:         traffic will go DIRECT and is NOT anonymized — start tor+privoxy or pass --proxy")
	return nil
}

// Down tears down only what we own. We currently detect (never spawn) tor and
// privoxy, so there is nothing of ours to kill.
func (t *torBridge) Down() error { return nil }

// Rotate requests a fresh Tor circuit over the control port (NEWNYM). Assumes a
// local ControlPort with empty-password auth (the common dev setup); Tor rate-
// limits NEWNYM, so rapid calls may reuse the same circuit.
func (t *torBridge) Rotate() error {
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
func (t *torBridge) SelfTest() error {
	if t.proxy == "" {
		return errors.New("egress is DIRECT — no proxy to test (start tor+privoxy or pass --proxy)")
	}
	return dialCheck(proxyHostPort(t.proxy))
}

func (t *torBridge) ProxyURL() string {
	switch {
	case t.direct:
		return "DIRECT (no proxy)"
	case t.proxy == "":
		return "<down>"
	default:
		return t.proxy
	}
}

// --- net helpers -------------------------------------------------------------

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

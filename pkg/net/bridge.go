// Bridge, ConnectBridge and the SOCKS5 dialer. Package-level documentation and
// the honest-caveat list (T2.7) live in doc.go.
//
// Portions of the SOCKS5 handling below are derived from
// github.com/nao1215/tornago (MIT, Copyright (c) 2025 nao1215).
//
// Note the import of the stdlib "net" below: legal (the package's own name is
// not an identifier in its own scope) but easy to misread. Callers should
// import this package with an alias, e.g. csnet "chainsmoke/pkg/net".
package net

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"chainsmoke/pkg/core"
)

const (
	torSocksAddr  = "127.0.0.1:9050" // Tor SOCKS port (9150 under the Browser bundle)
	torCtrlAddr   = "127.0.0.1:9051" // Tor control port, for NEWNYM
	dialTimeout   = 3 * time.Second
	tunnelTimeout = 30 * time.Second

	// bootstrapBudget bounds the retry loop in waitUsable. Tor answers every
	// CONNECT with 0x01 until it has built circuits, which takes seconds to tens
	// of seconds on a cold start. A warm Tor satisfies the check on the first
	// attempt, so this ceiling is only ever paid once per boot.
	bootstrapBudget = 45 * time.Second
	bootstrapPoll   = 500 * time.Millisecond
)

// Bridge is the built-in egress path. Tor speaks SOCKS5; mpv/ffmpeg speak only
// HTTP CONNECT (INSTRUCTIONS.md "Proxy limitation"), so something has to sit
// between them. That something used to be Privoxy; as of T2.2 it is
// ConnectBridge, running inside this process.
type Bridge struct {
	override    string         // --proxy value, if the user supplied one
	allowDirect bool           // --allow-direct: opt in to unanonymized egress
	torSocks    string         // where Tor's SOCKS listener is expected
	proxy       string         // egress proxy URL once Up() succeeds ("" until then)
	direct      bool           // true when nothing was reachable and we fell back
	up          bool           // true once Up() has run to completion (success or not)
	verified    bool           // true ONLY once real traffic has crossed the chain
	cb          *ConnectBridge // the in-process CONNECT->SOCKS5 hop, when we own one

	torCmd      *exec.Cmd // a private tor WE spawned, if any (nil = system tor)
	torDir      string    // its scratch DataDirectory, ours to delete
	socksAddr   string    // the SOCKS listener actually in use
	torCtrlAddr string    // control port of the tor we spawned ("" = system tor)
	ctrlFailed  bool      // NEWNYM already failed in this process; do not retry it
}

var _ core.Net = (*Bridge)(nil)

// ErrEgressNotVerified is what every Resolver/Player entry point returns when
// Assert() fails. It is the enforcement half of T0.2: the invariant is not
// "a proxy was configured", it is "traffic has been proven to leave only
// through the bridge, in this process".
var ErrEgressNotVerified = errors.New("egress not verified — no network call may run (T0.2 fail-closed)")

// New builds a bridge.
//
//	override    the --proxy value ("" for the built-in path)
//	allowDirect opt in to running unanonymized when no proxy can be brought up
//	torSocks    where to look for Tor's SOCKS listener ("" = the 9050 default).
//	            Configurable because the Tor Browser bundle uses 9150, and
//	            because a test needs to be able to point at a port that is
//	            definitively dead — otherwise the fail-closed cases only pass on
//	            machines that happen to have no Tor.
func New(override string, allowDirect bool, torSocks string) *Bridge {
	if torSocks == "" {
		torSocks = torSocksAddr
	}
	return &Bridge{override: override, allowDirect: allowDirect, torSocks: torSocks}
}

// Assert is the gate every egressing call must pass. It answers a POLICY
// question — "may traffic leave?" — which is satisfied two ways: the chain was
// verified private, or the user explicitly accepted an unanonymized path with
// --allow-direct. Keeping that distinct from Verified() matters: `audit` must
// still be able to report DIRECT truthfully while calls are permitted.
//
// Fail CLOSED: anything else, including an Up() that was never called, refuses.
func (b *Bridge) Assert() error {
	switch {
	case b.verified:
		return nil
	case b.direct && b.allowDirect:
		return nil
	default:
		return ErrEgressNotVerified
	}
}

// Verified reports whether real traffic has crossed the chain in this process.
// `audit` uses it to print an honest posture rather than a configured one.
func (b *Bridge) Verified() bool { return b.verified }

// waitUsable is the whole of T2.3: it pushes real CONNECTs through
// bridge -> Tor -> exit until one succeeds, and in doing so proves BOTH that Tor
// finished bootstrapping and that the chain carries traffic. Those are not two
// checks — a CONNECT that succeeds is the bootstrap proof.
//
// The discrimination matters more than the loop. Tor replies 0x01 "general
// failure" for every request until circuits exist, so 0x01 means "wait". Any
// other SOCKS code is a real fault: retrying 0x04 for 45s would just be a slow
// way to fail. Bailing immediately on those is what keeps a genuine
// misconfiguration diagnosable.
func (b *Bridge) waitUsable(ctx context.Context, target string) error {
	ctx, cancel := context.WithTimeout(ctx, bootstrapBudget)
	defer cancel()

	t := time.NewTicker(bootstrapPoll)
	defer t.Stop()

	var last error
	for {
		err := b.cb.Verify(target)
		if err == nil {
			return nil
		}
		last = err

		var se *socksError
		if !errors.As(err, &se) || se.Code != socksGeneralFailure {
			// Not a bootstrap symptom — stop rather than burn the budget.
			return err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("tor did not become usable within %s (last: %v)", bootstrapBudget, last)
		case <-t.C:
		}
	}
}

// Up brings egress online through four gates, failing CLOSED at each (T2.3):
//
//  1. --proxy override      -> scheme check, reachable, real CONNECT (T2.6)
//  2. tor SOCKS listener    -> system tor, else spawn a private one (T2.1)
//  3. in-process bridge     -> CONNECT->SOCKS5 hop (T2.2); our bug = hard error
//  4. one real CONNECT      -> proves bootstrap AND that the chain carries bytes
//
// Only after gate 4 is `verified` set, which is what Assert() gates on. If no
// anonymized path can be built, fallback() decides: refuse, or run DIRECT if
// the user passed --allow-direct.
func (b *Bridge) Up() error {
	// Once per process. A play session runs several verbs, each calling Up(),
	// while queued tracks keep riding the first bridge: a second pass would start
	// another bridge and maybe another private tor, and Down() only tears down
	// the newest of each — orphaning the rest (TestUpIsIdempotent).
	if b.verified || b.direct {
		return nil
	}
	b.up = true

	// Gate 1 — an explicit --proxy replaces the bridge but keeps the SAME
	// verified-up contract (T2.6): reachable is not enough, it must carry a real
	// CONNECT. Anything less would make --proxy a way to silently opt out of
	// T0.2, which is the one thing the flag must not be.
	if b.override != "" {
		if err := checkProxyScheme(b.override); err != nil {
			return err
		}
		if err := dialCheck(proxyHostPort(b.override)); err != nil {
			return fmt.Errorf("--proxy %s not reachable: %w", redactedProxy(b.override), err)
		}
		if err := verifyHTTPProxyCONNECT(proxyHostPort(b.override), egressProbeTarget); err != nil {
			return fmt.Errorf("--proxy %s did not complete a CONNECT to %s: %w",
				redactedProxy(b.override), egressProbeTarget, err)
		}
		// Stored lower-cased: checkProxyScheme accepts HTTP://, but libavformat
		// honours a proxy only when it starts with exactly "http://" and fetches
		// the stream DIRECTLY otherwise, while the preflight still tunnels
		// (measured 2026-10-01). url.Parse lower-cases the scheme, nothing else.
		u, _ := url.Parse(b.override) // checkProxyScheme has already parsed it
		b.proxy = u.String()
		b.socksAddr = ""
		exportProxyEnv(b.proxy)
		b.verified = true
		return nil
	}

	// Gate 2 — a Tor SOCKS listener must exist: system tor, else one we spawn.
	socksAddr, err := b.ensureTor()
	if err != nil {
		return b.fallback(fmt.Sprintf("tor could not be started: %v", err))
	}
	if socksAddr == "" {
		return b.fallback(fmt.Sprintf("no Tor SOCKS listener on %s", b.torSocks))
	}
	b.socksAddr = socksAddr

	// Gate 3 — the bridge is ours, so a failure here is never survivable by
	// falling through to DIRECT: it would mean silently dropping anonymity
	// because of our own bug.
	cb, err := StartConnectBridge(socksAddr, true)
	if err != nil {
		return fmt.Errorf("tor is up but the CONNECT bridge failed to start: %w", err)
	}
	b.cb = cb
	b.proxy = cb.ProxyURL()
	exportProxyEnv(b.proxy)

	// Gate 4 — one real CONNECT through the whole chain. This is the T0.2
	// invariant: not "a proxy is configured" but "traffic has left this way".
	if err := b.waitUsable(context.Background(), egressProbeTarget); err != nil {
		_ = b.Down()
		b.proxy = ""
		return fmt.Errorf("egress unusable: %w", err)
	}

	b.verified = true
	return nil
}

// egressProbeTarget is what Gate 4 CONNECTs to. SoundCloud's own API host, so a
// success proves the path we actually need rather than general internet access.
const egressProbeTarget = "api-v2.soundcloud.com:443"

// fallback handles "no anonymized egress available". Fail CLOSED unless the user
// explicitly opted in with --allow-direct (INSTRUCTIONS.md:19). The old code
// warned and returned nil, which meant a machine without Tor silently ran
// unanonymized — the exact behavior T0.2 exists to forbid.
func (b *Bridge) fallback(reason string) error {
	if !b.allowDirect {
		return fmt.Errorf("%s — refusing to run unanonymized.\n"+
			"  start tor (SocksPort %s), pass --proxy, or accept the risk with --allow-direct",
			reason, b.torSocks)
	}
	b.direct = true
	fmt.Fprintf(os.Stderr, "chainsmoke: WARNING %s\n", reason)
	fmt.Fprintln(os.Stderr, "chainsmoke:         --allow-direct given: traffic goes DIRECT and is NOT anonymized")
	// Deliberately NOT verified: --allow-direct permits egress, it does not
	// pretend the path is private. Assert() still refuses, so every caller has
	// to opt in through this flag rather than inherit a false sense of safety.
	return nil
}

// Down tears down only what we own: the CONNECT bridge, and a private tor if we
// spawned one. A system tor is never touched — killing a daemon we merely
// borrowed would be a nasty surprise for whatever else is using it.
func (b *Bridge) Down() error {
	var errs []error
	if b.cb != nil {
		if err := b.cb.Close(); err != nil {
			errs = append(errs, err)
		}
		b.cb = nil
	}
	if b.torCmd != nil && b.torCmd.Process != nil {
		if err := b.torCmd.Process.Kill(); err != nil {
			errs = append(errs, err)
		}
		_, _ = b.torCmd.Process.Wait()
		b.torCmd = nil
		b.torCtrlAddr = ""
	}
	if b.torDir != "" {
		if err := os.RemoveAll(b.torDir); err != nil {
			errs = append(errs, err)
		}
		b.torDir = ""
	}
	return errors.Join(errs...)
}

// newnymCooldown is Tor's internal NEWNYM coalescing window (MAX_SIGNEWNYM_RATE,
// 10s). Inside it Tor still answers 250 OK while quietly keeping the existing
// circuit, so a successful return does NOT prove the exit changed.
const newnymCooldown = 10 * time.Second

// Rotate asks for a different exit two ways, strongest-guarantee first:
// re-rolled SOCKS isolation credentials (always available, effective on our own
// next stream) and a NEWNYM over the control port (process-wide, but only when
// a ControlPort exists and lets us in).
//
// Three honesty requirements, the first two of which the first implementation
// failed:
//
//   - Auth is negotiated (PROTOCOLINFO -> cookie / NULL), not assumed absent.
//     `AUTHENTICATE ""` fails on every distro tor, which ships
//     CookieAuthentication 1.
//   - A 250 OK is NOT proof of rotation. Tor coalesces NEWNYM signals sent
//     within newnymCooldown and reports success either way. Rather than let the
//     user believe they have a new exit, we track when we last asked and say so.
//   - Success here means "the next stream keys a new circuit", not "you have a
//     different exit". Tor picks exits at random; it may well pick the same one.
func (b *Bridge) Rotate() error {
	// Re-isolate FIRST, and unconditionally. It is the half that always works:
	// no ControlPort, no auth, no cooperation from a tor we do not own. A
	// system tor with ControlPort disabled — the common case, and what this dev
	// box runs — used to make `chainsmoke rotate` fail outright while a fresh
	// circuit was one credential away.
	fresh := b.cb != nil && b.cb.Reisolate()

	// NEWNYM over the ControlPort is a bonus, not the mechanism: re-isolation
	// already gave the next stream a fresh circuit, so a missing ControlPort
	// is nothing to report. It used to print two lines on every retry (dev's
	// report, 2026-10-03), and once it has failed it is not tried again.
	if b.ctrlFailed {
		if fresh {
			return nil
		}
		return errors.New("tor control port unavailable (enable ControlPort for NEWNYM)")
	}
	if err := b.newnym(); err != nil {
		b.ctrlFailed = true
		if !fresh {
			return err
		}
	}
	return nil
}

// newnym is the ControlPort half of Rotate, split out so a failure here can be
// weighed against whether re-isolation already delivered a new circuit.
func (b *Bridge) newnym() error {
	addr := b.ctrlAddr()
	c, err := dialControl(addr, dialTimeout)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Authenticate(); err != nil {
		return err
	}

	since, recent := sinceLastNewnym()
	if err := c.NewNym(); err != nil {
		return err
	}
	markNewnym()

	if recent {
		fmt.Fprintf(os.Stderr,
			"chainsmoke: NEWNYM sent, but the last one was %s ago — Tor coalesces signals\n"+
				"chainsmoke:         within %s, so the circuit may be unchanged. Wait and retry\n"+
				"chainsmoke:         if you need a different exit.\n",
			since.Truncate(time.Second), newnymCooldown)
	}
	return nil
}

// ctrlAddr is the control port for whichever tor we are actually using: the one
// we spawned, else the conventional system port.
func (b *Bridge) ctrlAddr() string {
	if b.torCtrlAddr != "" {
		return b.torCtrlAddr
	}
	return torCtrlAddr
}

// newnymStamp is where the last rotation time is recorded. chainsmoke processes
// are short-lived (decision T0.1a), so in-memory tracking would never observe a
// second call — the cooldown has to be visible across invocations to be useful.
func newnymStamp() string { return filepath.Join(core.RuntimeDir(), "last-newnym") }

func sinceLastNewnym() (time.Duration, bool) {
	fi, err := os.Stat(newnymStamp())
	if err != nil {
		return 0, false
	}
	d := time.Since(fi.ModTime())
	return d, d < newnymCooldown
}

func markNewnym() {
	if _, err := core.EnsureRuntimeDir(); err != nil {
		return // best effort: the warning is a nicety, not a correctness gate
	}
	now := time.Now()
	p := newnymStamp()
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_ = f.Close()
	}
	_ = os.Chtimes(p, now, now)
}

// SelfTest proves the egress path actually carries traffic, rather than that a
// port is open. When we own the bridge it pushes a real CONNECT through
// bridge -> Tor -> exit, which is the T0.2 invariant stated as a test.
func (b *Bridge) SelfTest() error {
	if !b.up {
		return errors.New("egress not brought up in this process — run a verb that calls Up() first")
	}
	if b.direct || b.proxy == "" {
		return errors.New("egress is DIRECT — nothing anonymized to test (start tor, or drop --allow-direct)")
	}
	if b.cb != nil {
		// A fresh CONNECT, not a cached verdict. Up() proved the path once at
		// startup; SelfTest exists to prove it is STILL good at the moment the
		// user asks, which is the whole point of `audit` probing rather than
		// printing config (T7.1).
		return b.cb.Verify(egressProbeTarget)
	}
	// --proxy override: we did not build it, so we can only reach it.
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

// -----------------------------------------------------------------------------
// Tor lifecycle (T2.1)
// -----------------------------------------------------------------------------

// ensureTor returns the SOCKS address to use, preferring a system tor and
// spawning a private one only if none is listening. An empty address with a nil
// error means "no tor and none could be started" — the caller decides whether
// that is fatal (it is, unless --allow-direct).
//
// Only the TCP listener is waited for here. Bootstrap is NOT checked: that is
// waitUsable's job, and doing it there means the same code path covers a system
// tor that is still warming up.
func (b *Bridge) ensureTor() (string, error) {
	if dialCheck(b.torSocks) == nil {
		return b.torSocks, nil // system tor
	}
	// A non-default --tor-socks names one specific tor (the Tor Browser's,
	// smoke's dead 127.0.0.1:1). Nothing listening there means fail closed,
	// not bootstrap a substitute onto the network (CH-DEBATE-2026-10-03 #3).
	if b.torSocks != torSocksAddr {
		return "", nil
	}
	if _, err := exec.LookPath("tor"); err != nil {
		return "", nil // nothing listening and nothing to spawn
	}

	cmd, dir, socksAddr, ctrlAddr, err := spawnPrivateTor()
	if err != nil {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
		return "", err
	}
	b.torCmd, b.torDir, b.torCtrlAddr = cmd, dir, ctrlAddr

	// Wait for the listener it was told to open — the address we were handed,
	// never the package default. Polling the wrong port was the bug in the first
	// draft: a private tor on an ephemeral port would never be seen.
	deadline := time.Now().Add(bootstrapBudget)
	for time.Now().Before(deadline) {
		if dialCheck(socksAddr) == nil {
			return socksAddr, nil
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return "", fmt.Errorf("private tor exited during startup (%s)", cmd.ProcessState)
		}
		time.Sleep(bootstrapPoll)
	}
	return "", fmt.Errorf("private tor did not open %s within %s", socksAddr, bootstrapBudget)
}

// spawnPrivateTor starts a throwaway tor with its own DataDirectory, an
// ephemeral SocksPort, and a cookie-authenticated ControlPort so Rotate() works
// against it (the first draft omitted ControlPort, which left NEWNYM dead).
func spawnPrivateTor() (cmd *exec.Cmd, dir, socksAddr, ctrlAddr string, err error) {
	dir, err = os.MkdirTemp("", "chainsmoke-tor-")
	if err != nil {
		return nil, "", "", "", err
	}
	// Let the OS pick the ports, then hand tor the numbers. Hardcoding 9050
	// would collide with the system tor we just failed to find.
	socksPort, err := freePort()
	if err != nil {
		return nil, dir, "", "", err
	}
	ctrlPort, err := freePort()
	if err != nil {
		return nil, dir, "", "", err
	}

	path := filepath.Join(dir, "torrc")
	if err := os.WriteFile(path, []byte(privateTorrc(socksPort, ctrlPort, filepath.Join(dir, "data"))), 0o600); err != nil {
		return nil, dir, "", "", err
	}

	cmd = exec.Command("tor", "-f", path)
	// tor's startup chatter would otherwise land in the middle of our stdout and
	// break `chainsmoke search x | head`.
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return nil, dir, "", "", fmt.Errorf("spawn tor: %w", err)
	}
	return cmd, dir,
		fmt.Sprintf("127.0.0.1:%d", socksPort),
		fmt.Sprintf("127.0.0.1:%d", ctrlPort), nil
}

// privateTorrc is the config for a throwaway tor. __OwningControllerProcess
// makes tor exit on its own once this process is gone, however it went: a
// closed terminal (SIGHUP, which tor treats as a reload and survives), kill,
// SIGKILL, a crash. Measured 2026-10-03: tor exited 12s after its owner died
// (CH-DEBATE-2026-10-03 #2). No signal handler covers all of those, and this
// needs none — so T10.9 has no tor to chase.
func privateTorrc(socksPort, ctrlPort int, dataDir string) string {
	return fmt.Sprintf(""+
		"SocksPort %d\n"+
		"ControlPort %d\n"+
		"CookieAuthentication 1\n"+
		"DataDirectory %s\n"+
		"AvoidDiskWrites 1\n"+
		"__OwningControllerProcess %d\n",
		socksPort, ctrlPort, dataDir, os.Getpid())
}

// freePort asks the OS for an unused loopback port and immediately releases it.
// Racy in principle; in practice the window is microseconds and the alternative
// is guessing.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// -----------------------------------------------------------------------------
// ConnectBridge — the in-process replacement for Privoxy (T2.2)
// -----------------------------------------------------------------------------

// ConnectBridge is a loopback HTTP proxy that speaks CONNECT and forwards every
// tunnel through Tor's SOCKS5 port. It exists because mpv, ffmpeg and yt-dlp are
// CHILD PROCESSES: they cannot use a Go dialer, and libavformat has no SOCKS
// support at all. This is the job Privoxy did in script.sh, done in-process so
// chainsmoke keeps its single-static-binary promise and drops a dependency.
type ConnectBridge struct {
	ledger *ledger // T7.2 accounting: every CONNECT that crossed this bridge
	ln     net.Listener
	dialer *socks5Dialer
	wg     sync.WaitGroup
	once   sync.Once
}

// StartConnectBridge listens on an ephemeral loopback port and serves CONNECT.
// isolate turns on per-bridge SOCKS credentials so Tor's IsolateSOCKSAuth gives
// this process its own circuit — something no external HTTP bridge can offer.
func StartConnectBridge(torSocksAddr string, isolate bool) (*ConnectBridge, error) {
	// 127.0.0.1 explicitly, never ":0" — an egress bridge bound to 0.0.0.0 is
	// an open proxy on the local network.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("connect bridge: listen: %w", err)
	}
	b := &ConnectBridge{
		ln:     ln,
		dialer: &socks5Dialer{addr: torSocksAddr, timeout: tunnelTimeout},
		ledger: newLedger(),
	}
	if isolate {
		b.dialer.user = "chainsmoke"
		b.dialer.pass = isolationPass()
	}
	b.wg.Add(1)
	go b.serve()
	return b, nil
}

// ProxyURL is what goes into http_proxy and yt-dlp --proxy.
func (b *ConnectBridge) ProxyURL() string { return "http://" + b.ln.Addr().String() }

// Reisolate re-rolls this bridge's SOCKS credentials so the NEXT stream lands
// on a fresh circuit — usually, though not provably, a different exit.
//
// This is the rotation that works when NEWNYM cannot: it needs no ControlPort,
// no authentication, and no cooperation from a system tor we do not own. It is
// also finer-grained than NEWNYM, which resets the whole process's circuits.
// Reports false when the bridge was built without isolation, so no caller
// claims a fresh circuit it did not get.
func (b *ConnectBridge) Reisolate() bool { return b.dialer.reisolate() }

// Verify pushes one real CONNECT through the bridge to target and tears it down.
// Success means Tor is bootstrapped and the whole chain carries bytes.
//
// It dials b.dialer directly rather than looping through our own listener, so
// the ledger has to be told here too (T7.2). Skipping it would under-report our
// OWN egress — the one direction an honesty feature must never err in — and it
// is also the only tunnel a bare `chainsmoke audit` opens, since that verb runs
// no yt-dlp and no mpv.
func (b *ConnectBridge) Verify(target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), tunnelTimeout)
	defer cancel()

	b.ledger.connect(target)
	c, err := b.dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		b.ledger.fail()
		return fmt.Errorf("egress self-test failed (%s): %w", target, err)
	}
	return c.Close()
}

func (b *ConnectBridge) serve() {
	defer b.wg.Done()
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.handle(c)
		}()
	}
}

func (b *ConnectBridge) handle(client net.Conn) {
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(tunnelTimeout))

	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	// CONNECT only. An absolute-form GET means a child is about to push
	// PLAINTEXT through us — refuse loudly rather than proxy it, so a leak
	// shows up as a visible failure instead of as quiet traffic.
	if req.Method != http.MethodConnect {
		b.ledger.refuse() // T7.2: a child tried to push plaintext through us
		_, _ = io.WriteString(client,
			"HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	target := req.Host // authority-form: "host:port"
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	b.ledger.connect(target) // T7.2: every tunnel this process opened

	ctx, cancel := context.WithTimeout(context.Background(), tunnelTimeout)
	upstream, err := b.dialer.DialContext(ctx, "tcp", target)
	cancel()
	if err != nil {
		b.ledger.fail() // T7.2: attempted, never became a tunnel
		_, _ = io.WriteString(client,
			"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	defer upstream.Close()

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}

	// Copy from br, NOT from client: http.ReadRequest may have pulled bytes
	// past the header into the bufio buffer, and a client that sends optimistic
	// data would otherwise lose its TLS ClientHello and hang.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, br)
		if tc, ok := upstream.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if tc, ok := client.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// Close stops accepting and waits for in-flight tunnels.
//
// KNOWN LIMITATION: a live audio stream keeps this blocked. Before ship, track
// open conns and kill them here — `chainsmoke stop`/`purge` want the tunnels
// severed, not drained.
func (b *ConnectBridge) Close() error {
	var err error
	b.once.Do(func() { err = b.ln.Close() })
	b.wg.Wait()
	return err
}

// -----------------------------------------------------------------------------
// SOCKS5 dialer (derived from nao1215/tornago, MIT)
// -----------------------------------------------------------------------------

type socks5Dialer struct {
	addr    string // Tor SOCKS5 listener
	timeout time.Duration

	// mu guards the isolation credentials. They are not write-once: Reisolate
	// re-rolls them while the bridge is serving, and every dial reads them, so
	// a plain field assignment here would be a data race (T3.5 retry).
	mu   sync.RWMutex
	user string // stream-isolation credentials; "" = no-auth
	pass string
}

// creds reads the pair as a unit. Splitting the read would let a handshake pair
// the old username with a re-rolled password and key an unintended circuit.
func (d *socks5Dialer) creds() (user, pass string) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.user, d.pass
}

// reisolate re-rolls the password so the NEXT stream through this dialer keys a
// different circuit. It reports false when isolation is off: there is nothing
// to roll, and no caller may claim a fresh circuit it did not get.
func (d *socks5Dialer) reisolate() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.user == "" {
		return false
	}
	d.pass = isolationPass()
	return true
}

// isolationPass is random, not time-derived. Two re-rolls inside one clock tick
// would otherwise produce the SAME key and silently reuse the very circuit we
// are trying to escape — and Windows' wall clock is coarse enough (often ~1ms,
// sometimes ~15ms) for a retry loop to land inside one tick.
func isolationPass() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Falling back to the clock is worse but still distinct-ish; returning
		// a constant would disable isolation for the rest of the process.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("socks5: unsupported network %q", network)
	}
	var dl net.Dialer
	if d.timeout > 0 {
		dl.Timeout = d.timeout
	}
	c, err := dl.DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return nil, fmt.Errorf("socks5: dial proxy %s: %w", d.addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	if err := d.handshake(c, address); err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

func (d *socks5Dialer) handshake(c net.Conn, dest string) error {
	// One read of the credentials per handshake: a mid-handshake re-roll must
	// not split this dial across two isolation keys.
	user, pass := d.creds()

	// When isolating we offer ONLY username/password. If 0x00 is on the table
	// Tor will take it, and no-auth streams share a circuit — precisely what
	// isolation exists to prevent.
	greeting := []byte{0x05, 0x01, 0x00}
	if user != "" {
		greeting = []byte{0x05, 0x01, 0x02}
	}
	if _, err := c.Write(greeting); err != nil {
		return fmt.Errorf("socks5: write greeting: %w", err)
	}
	var sel [2]byte
	if _, err := io.ReadFull(c, sel[:]); err != nil {
		return fmt.Errorf("socks5: read method selection: %w", err)
	}
	if sel[0] != 0x05 {
		return fmt.Errorf("socks5: bad proxy version %#02x", sel[0])
	}
	switch sel[1] {
	case 0x00:
	case 0x02:
		if err := d.authUserPass(c, user, pass); err != nil {
			return err
		}
	default:
		return fmt.Errorf("socks5: proxy refused our auth methods (%#02x)", sel[1])
	}

	req, err := connectRequest(dest)
	if err != nil {
		return err
	}
	if _, err := c.Write(req); err != nil {
		return fmt.Errorf("socks5: write CONNECT: %w", err)
	}
	return readConnectReply(c)
}

// authUserPass is RFC 1929. Tor does not validate the credentials; it uses the
// pair as a circuit-isolation key, so any distinct value buys a distinct path.
func (d *socks5Dialer) authUserPass(c net.Conn, user, pass string) error {
	if len(user) > 255 || len(pass) > 255 {
		return errors.New("socks5: isolation credentials too long")
	}
	buf := make([]byte, 0, 3+len(user)+len(pass))
	buf = append(buf, 0x01, byte(len(user)))
	buf = append(buf, user...)
	buf = append(buf, byte(len(pass)))
	buf = append(buf, pass...)
	if _, err := c.Write(buf); err != nil {
		return fmt.Errorf("socks5: write auth: %w", err)
	}
	var resp [2]byte
	if _, err := io.ReadFull(c, resp[:]); err != nil {
		return fmt.Errorf("socks5: read auth reply: %w", err)
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("socks5: stream-isolation auth rejected (status %#02x)", resp[1])
	}
	return nil
}

// connectRequest builds a SOCKS5 CONNECT. The hostname is sent as ATYP=0x03
// (domain) and is NEVER resolved here — that is the entire point. Tor does the
// lookup at the exit, so the local resolver never sees a soundcloud/sndcdn
// name. This is what socks5h:// means, made structural rather than configured,
// which is also what T2.5 has to assert.
func connectRequest(dest string) ([]byte, error) {
	host, portStr, err := net.SplitHostPort(dest)
	if err != nil {
		return nil, fmt.Errorf("socks5: bad destination %q: %w", dest, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("socks5: bad port in %q: %w", dest, err)
	}

	req := []byte{0x05, 0x01, 0x00}
	switch ip := net.ParseIP(host); {
	case ip == nil:
		if host == "" || len(host) > 255 {
			return nil, fmt.Errorf("socks5: hostname length %d out of range", len(host))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	case ip.To4() != nil:
		req = append(req, 0x01)
		req = append(req, ip.To4()...)
	default:
		req = append(req, 0x04)
		req = append(req, ip.To16()...)
	}

	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	return append(req, p[:]...), nil
}

// socksGeneralFailure (0x01) is the one reply code that is not necessarily a
// fault: Tor returns it for every CONNECT until it has finished bootstrapping.
// waitUsable keys its retry decision on exactly this value.
const socksGeneralFailure = 0x01

var socksReplyText = map[byte]string{
	socksGeneralFailure: "general SOCKS server failure (Tor not finished bootstrapping?)",
	0x02:                "connection not allowed by ruleset",
	0x03:                "network unreachable",
	0x04:                "host unreachable (Tor could not resolve or reach the destination)",
	0x05:                "connection refused by destination",
	0x06:                "TTL expired",
	0x07:                "command not supported",
	0x08:                "address type not supported",
}

// socksError carries the raw reply code, not just its text. Callers need the
// byte to decide whether a failure is transient (bootstrapping) or terminal;
// string matching on the message would be fragile and is what this avoids.
type socksError struct{ Code byte }

func (e *socksError) Error() string {
	if s, ok := socksReplyText[e.Code]; ok {
		return "socks5: CONNECT failed: " + s
	}
	return fmt.Sprintf("socks5: CONNECT failed (code %#02x)", e.Code)
}

func readConnectReply(c net.Conn) error {
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return fmt.Errorf("socks5: read CONNECT reply: %w", err)
	}
	if hdr[1] != 0x00 {
		return &socksError{Code: hdr[1]}
	}
	var n int64
	switch hdr[3] {
	case 0x01:
		n = 4
	case 0x04:
		n = 16
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return fmt.Errorf("socks5: read bound-domain length: %w", err)
		}
		n = int64(l[0])
	default:
		return fmt.Errorf("socks5: unknown ATYP %#02x in reply", hdr[3])
	}
	if _, err := io.CopyN(io.Discard, c, n+2); err != nil { // +2 = bound port
		return fmt.Errorf("socks5: drain bound address: %w", err)
	}
	return nil
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

// checkProxyScheme rejects a --proxy the media stack cannot actually use.
//
// This is not pedantry. mpv and ffmpeg (libavformat) have NO SOCKS support and
// read only http_proxy, while yt-dlp handles socks5 fine. Accepting
// socks5://... would therefore "work" for metadata and silently fail — or
// worse, silently go direct — for the audio stream: a partial-egress split,
// which is the exact failure the bridge exists to prevent. Refuse it and point
// at the built-in path, which fronts SOCKS with CONNECT for precisely this
// reason.
func checkProxyScheme(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("--proxy is not a URL (want http://host:port); the value is not repeated, it may hold credentials")
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return nil
	case "https":
		// Same split as SOCKS: libavformat honours a proxy only when it starts
		// with exactly "http://" and fetches the stream DIRECTLY otherwise
		// (mpv manual; measured 2026-10-01).
		return fmt.Errorf("--proxy %s is a TLS (https://) proxy, which mpv/ffmpeg cannot use — "+
			"libavformat would fetch the stream directly.\n"+
			"  yt-dlp would tunnel and playback would not — a partial-egress split.\n"+
			"  Point --proxy at the proxy's plain http:// listener, or drop it and use Tor.", u.Redacted())
	case "socks5", "socks5h", "socks4", "socks4a":
		return fmt.Errorf("--proxy %s is a SOCKS proxy, which mpv/ffmpeg cannot use "+
			"(no SOCKS support in libavformat).\n"+
			"  yt-dlp would work and playback would not — a partial-egress split.\n"+
			"  Drop --proxy and let chainsmoke front your SOCKS port with its own CONNECT bridge,\n"+
			"  or point --proxy at an HTTP proxy that speaks CONNECT.", u.Redacted())
	default:
		return fmt.Errorf("--proxy %s has unsupported scheme %q (want http://)", u.Redacted(), u.Scheme)
	}
}

// redactedProxy is a --proxy value fit for a message. A proxy URL can carry
// credentials (http://user:pass@host), and the refusals used to repeat it
// verbatim (CH-DEBATE-2026-10-03 #8). Up() only calls this after
// checkProxyScheme has parsed the value.
func redactedProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(value not shown)"
	}
	return u.Redacted()
}

// verifyHTTPProxyCONNECT speaks one CONNECT to a user-supplied HTTP proxy. This
// is what lets --proxy satisfy the same verified-up contract as the built-in
// bridge instead of being an escape hatch from it.
func verifyHTTPProxyCONNECT(proxyHostPort, target string) error {
	c, err := net.DialTimeout("tcp", proxyHostPort, dialTimeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(tunnelTimeout))

	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy answered %s", resp.Status)
	}
	return nil
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

// AuditLocalDNS implements core.DNSAuditor: it reports what this host's
// resolver can observe, which is a different (and weaker) question than whether
// chainsmoke leaked. See dnsguard.go for why that distinction matters.
func (b *Bridge) AuditLocalDNS() (bool, []string) {
	return AuditDNS(context.Background())
}

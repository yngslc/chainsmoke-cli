// Bridge, ConnectBridge and the SOCKS5 dialer. Package-level documentation and
// the honest-caveat list live in doc.go.
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
	torSocksAddr  = "127.0.0.1:9050"
	torCtrlAddr   = "127.0.0.1:9051"
	dialTimeout   = 3 * time.Second
	tunnelTimeout = 30 * time.Second

	bootstrapBudget = 45 * time.Second
	bootstrapPoll   = 500 * time.Millisecond
)

// Bridge is the built-in egress path. Tor speaks SOCKS5; mpv/ffmpeg speak only
// HTTP CONNECT, so something has to sit between them.
type Bridge struct {
	override    string
	allowDirect bool
	private     bool
	torSocks    string
	proxy       string
	direct      bool
	up          bool
	verified    bool
	cb          *ConnectBridge

	torCmd      *exec.Cmd
	torDir      string
	socksAddr   string
	torCtrlAddr string
	ctrlFailed  bool

	rotateMu sync.Mutex
}

var _ core.Net = (*Bridge)(nil)

// ErrEgressNotVerified is what every Resolver/Player entry point returns when
// Assert() fails.
var ErrEgressNotVerified = errors.New("egress not verified — no network call may run (fail-closed)")

// New builds a bridge.
func New(override string, allowDirect bool, torSocks string, private bool) *Bridge {
	if torSocks == "" {
		torSocks = torSocksAddr
	}
	return &Bridge{override: override, allowDirect: allowDirect, torSocks: torSocks, private: private}
}

// Assert is the gate every egressing call must pass.
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
func (b *Bridge) Verified() bool { return b.verified }

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
			return err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("tor did not become usable within %s (last: %v)", bootstrapBudget, last)
		case <-t.C:
		}
	}
}

// Up brings egress online through four gates, failing CLOSED at each.
func (b *Bridge) Up() error {
	if b.verified || b.direct {
		return nil
	}
	b.up = true

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
		u, _ := url.Parse(b.override)
		b.proxy = u.String()
		b.socksAddr = ""
		exportProxyEnv(b.proxy)
		b.verified = true
		return nil
	}

	socksAddr, err := b.ensureTor()
	if err != nil {
		return b.fallback(err.Error())
	}
	if socksAddr == "" {
		return b.fallback(fmt.Sprintf("no Tor SOCKS listener on %s", b.torSocks))
	}
	b.socksAddr = socksAddr

	cb, err := StartConnectBridge(socksAddr, true)
	if err != nil {
		return fmt.Errorf("tor is up but the CONNECT bridge failed to start: %w", err)
	}
	b.cb = cb
	b.proxy = cb.ProxyURL()
	exportProxyEnv(b.proxy)

	if err := b.waitUsable(context.Background(), egressProbeTarget); err != nil {
		_ = b.Down()
		b.proxy = ""
		return fmt.Errorf("egress unusable: %w", err)
	}

	b.verified = true
	return nil
}

const egressProbeTarget = "api-v2.soundcloud.com:443"

func (b *Bridge) fallback(reason string) error {
	if !b.allowDirect {
		return fmt.Errorf("%s — refusing to run unanonymized.\n"+
			"  start tor (SocksPort %s), pass --proxy, or accept the risk with --allow-direct",
			reason, b.torSocks)
	}
	b.direct = true
	fmt.Fprintf(os.Stderr, "chainsmoke: WARNING %s\n", reason)
	fmt.Fprintln(os.Stderr, "chainsmoke:         --allow-direct given: traffic goes DIRECT and is NOT anonymized")
	return nil
}

// Down tears down only what we own: the CONNECT bridge, and a private tor if we
// spawned one.
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

const newnymCooldown = 10 * time.Second

// Rotate asks for a different exit two ways, strongest-guarantee first:
// re-rolled SOCKS isolation credentials (always available, effective on our own
// next stream) and a NEWNYM over the control port (process-wide, but only when
// a ControlPort exists and lets us in).
func (b *Bridge) Rotate() error {
	b.rotateMu.Lock()
	defer b.rotateMu.Unlock()

	fresh := b.cb != nil && b.cb.Reisolate()

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

	var since time.Duration
	var recent bool
	if !b.private {
		since, recent = sinceLastNewnym()
	}
	if err := c.NewNym(); err != nil {
		return err
	}
	if !b.private {
		markNewnym()
	}

	if recent {
		fmt.Fprintf(os.Stderr,
			"chainsmoke: NEWNYM sent, but the last one was %s ago — Tor coalesces signals\n"+
				"chainsmoke:         within %s, so the circuit may be unchanged. Wait and retry\n"+
				"chainsmoke:         if you need a different exit.\n",
			since.Truncate(time.Second), newnymCooldown)
	}
	return nil
}

func (b *Bridge) ctrlAddr() string {
	if b.torCtrlAddr != "" {
		return b.torCtrlAddr
	}
	return torCtrlAddr
}

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
		return
	}
	now := time.Now()
	p := newnymStamp()
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_ = f.Close()
	}
	_ = os.Chtimes(p, now, now)
}

// SelfTest proves the egress path actually carries traffic, rather than that a
// port is open.
func (b *Bridge) SelfTest() error {
	if !b.up {
		return errors.New("egress not brought up in this process — run a verb that calls Up() first")
	}
	if b.direct || b.proxy == "" {
		return errors.New("egress is DIRECT — nothing anonymized to test (start tor, or drop --allow-direct)")
	}
	if b.cb != nil {
		return b.cb.Verify(egressProbeTarget)
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

func (b *Bridge) ensureTor() (string, error) {
	listening := dialCheck(b.torSocks) == nil
	if listening && isTorSocks(b.torSocks) {
		return b.torSocks, nil
	}
	var notTor error
	if listening {
		notTor = fmt.Errorf("%s accepts connections but is not a Tor SocksPort", b.torSocks)
	}
	if b.torSocks != torSocksAddr {
		return "", notTor
	}
	if _, err := exec.LookPath("tor"); err != nil {
		return "", notTor
	}

	cmd, dir, socksAddr, ctrlAddr, err := spawnPrivateTor()
	if err != nil {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
		return "", fmt.Errorf("tor could not be started: %w", err)
	}
	b.torCmd, b.torDir, b.torCtrlAddr = cmd, dir, ctrlAddr

	deadline := time.Now().Add(bootstrapBudget)
	for time.Now().Before(deadline) {
		if dialCheck(socksAddr) == nil {
			return socksAddr, nil
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return "", fmt.Errorf("tor could not be started: private tor exited during startup (%s)", cmd.ProcessState)
		}
		time.Sleep(bootstrapPoll)
	}
	return "", fmt.Errorf("tor could not be started: private tor did not open %s within %s", socksAddr, bootstrapBudget)
}

func isTorSocks(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(dialTimeout))
	if _, err := io.WriteString(c, "GET / HTTP/1.0\r\n\r\n"); err != nil {
		return false
	}
	reply, _ := io.ReadAll(io.LimitReader(c, 512))
	return strings.Contains(strings.ToLower(string(reply)), "not an http proxy")
}

func spawnPrivateTor() (cmd *exec.Cmd, dir, socksAddr, ctrlAddr string, err error) {
	sweepStaleTorDirs(os.TempDir())
	dir, err = os.MkdirTemp("", fmt.Sprintf("%s%d-*", torDirPrefix, os.Getpid()))
	if err != nil {
		return nil, "", "", "", err
	}
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
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return nil, dir, "", "", fmt.Errorf("spawn tor: %w", err)
	}
	return cmd, dir,
		fmt.Sprintf("127.0.0.1:%d", socksPort),
		fmt.Sprintf("127.0.0.1:%d", ctrlPort), nil
}

const torDirPrefix = "chainsmoke-tor-p"

func sweepStaleTorDirs(tmp string) {
	matches, _ := filepath.Glob(filepath.Join(tmp, torDirPrefix+"*"))
	for _, dir := range matches {
		if pid, ok := torDirOwner(filepath.Base(dir)); ok && pid != os.Getpid() && !processAlive(pid) {
			_ = os.RemoveAll(dir)
		}
	}
}

func torDirOwner(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, torDirPrefix)
	if !ok {
		return 0, false
	}
	digits, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(digits)
	return pid, err == nil && pid > 0
}

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

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// ConnectBridge is a loopback HTTP proxy that speaks CONNECT and forwards every
// tunnel through Tor's SOCKS5 port.
type ConnectBridge struct {
	ledger *ledger
	ln     net.Listener
	dialer *socks5Dialer
	wg     sync.WaitGroup
	once   sync.Once

	ctx  context.Context
	stop context.CancelFunc
}

// StartConnectBridge listens on an ephemeral loopback port and serves CONNECT.
// isolate turns on per-bridge SOCKS credentials so Tor's IsolateSOCKSAuth gives
// this process its own circuit — something no external HTTP bridge can offer.
func StartConnectBridge(torSocksAddr string, isolate bool) (*ConnectBridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("connect bridge: listen: %w", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	b := &ConnectBridge{
		ln:     ln,
		dialer: &socks5Dialer{addr: torSocksAddr, timeout: tunnelTimeout},
		ledger: newLedger(),
		ctx:    ctx,
		stop:   stop,
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
func (b *ConnectBridge) Reisolate() bool { return b.dialer.reisolate() }

// Verify pushes one real CONNECT through the bridge to target and tears it
// down.
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
	severed := context.AfterFunc(b.ctx, func() { _ = client.Close() })
	defer severed()
	_ = client.SetReadDeadline(time.Now().Add(tunnelTimeout))

	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		b.ledger.refuse()
		_, _ = io.WriteString(client,
			"HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	target := req.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	if _, port, _ := net.SplitHostPort(target); port != "443" {
		b.ledger.refuse()
		_, _ = io.WriteString(client,
			"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	b.ledger.connect(target)

	ctx, cancel := context.WithTimeout(b.ctx, tunnelTimeout)
	upstream, err := b.dialer.DialContext(ctx, "tcp", target)
	cancel()
	if err != nil {
		b.ledger.fail()
		_, _ = io.WriteString(client,
			"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	defer upstream.Close()

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()

	<-done
	_ = client.Close()
	_ = upstream.Close()
	<-done
}

// Close stops accepting and severs every tunnel, then waits for their
// goroutines, which return at once.
func (b *ConnectBridge) Close() error {
	var err error
	b.once.Do(func() {
		b.stop()
		err = b.ln.Close()
	})
	b.wg.Wait()
	return err
}

type socks5Dialer struct {
	addr    string
	timeout time.Duration

	mu   sync.RWMutex
	user string
	pass string
}

func (d *socks5Dialer) creds() (user, pass string) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.user, d.pass
}

func (d *socks5Dialer) reisolate() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.user == "" {
		return false
	}
	d.pass = isolationPass()
	return true
}

func isolationPass() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
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
	user, pass := d.creds()

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
	if _, err := io.CopyN(io.Discard, c, n+2); err != nil {
		return fmt.Errorf("socks5: drain bound address: %w", err)
	}
	return nil
}

func dialCheck(hostport string) error {
	c, err := net.DialTimeout("tcp", hostport, dialTimeout)
	if err != nil {
		return err
	}
	return c.Close()
}

func checkProxyScheme(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("--proxy is not a URL (want http://host:port); the value is not repeated, it may hold credentials")
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return nil
	case "https":
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

func redactedProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(value not shown)"
	}
	return u.Redacted()
}

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

func proxyHostPort(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

func exportProxyEnv(p string) {
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		_ = os.Setenv(k, p)
	}
}

// AuditLocalDNS implements core.DNSAuditor: it reports what this host's
// resolver can observe, which is a different (and weaker) question than whether
// chainsmoke leaked.
func (b *Bridge) AuditLocalDNS() (bool, []string) {
	return AuditDNS(context.Background())
}

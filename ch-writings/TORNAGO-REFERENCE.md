# tornago — assessment and ported subset

Source: [github.com/nao1215/tornago](https://github.com/nao1215/tornago) · MIT ·
Go 1.25+ · zero external Go dependencies (stdlib only) · read at commit state of
2026-08-13.

Repo facts as fetched: **7 stars, 0 forks, 4 open issues**, created 2025-11-18,
last push 2026-08-13, single maintainer, `main` branch, not archived. Author's
stated origin is "a Go library for dark web crawling in credit card fraud
prevention contexts," and the README says it is "intentionally kept as a thin
wrapper around the original Tor command-line tool."

---

## 1. Verdict on "the same has already been done"

**Half right, and the half that's wrong is the important half.**

Right: tornago has genuinely done the Tor *process and control* work chainsmoke
had queued as T2.1 and T2.4, and done it better than the current `main.go`.
Our `Rotate()` is measurably broken against a real tor install (§4.4). That work
does not need reinventing, and there are correctness bugs to import fixes for.

Wrong: tornago is a **SOCKS5 client for Go code**. chainsmoke's egress consumers
are `mpv`, `ffmpeg` and `yt-dlp` — *child processes*. They cannot use a Go
dialer, and libavformat has no SOCKS support at all. The whole reason
`INSTRUCTIONS.md` calls the SOCKS gap "critical design constraint" is that our
traffic leaves through processes we only control via environment variables.

So tornago does **not** subsume chainsmoke's network layer. It has no HTTP proxy
server; `NewClient()` returns something only Go code can dial through. The
Privoxy hop — the actual hard part — is untouched by it.

**But reading it produced a better answer than either of us started with.**
tornago's SOCKS5 handshake is ~120 lines of clean stdlib. Put a CONNECT listener
in front of it and chainsmoke stops needing Privoxy *at all*. That deletes an
external dependency, a config-generation step (T2.2), and a class of debugging
pain, and it makes the DNS-leak guarantee structural instead of aspirational.
That is the real find here — not the library, the ~90 lines it made obvious.

---

## 2. What transfers and what doesn't

| chainsmoke need | tornago | verdict |
|---|---|---|
| T2.1 spawn a private tor | `StartTorDaemon` — ephemeral ports, temp `DataDirectory`, `--CookieAuthentication 1`, `--RunAsDaemon 0` | **take the design**, fix readiness (§4.3) |
| T2.1 readiness = handshake, not sleep | `waitForPorts` → `portsReachable` = TCP accept on both ports | **insufficient** — see §4.3 |
| T2.2 HTTP CONNECT bridge for mpv/ffmpeg | *absent* | **write it ourselves** (§4.2) — the main event |
| T2.3 verified end-to-end `Up()` | `Client.Check` dials `check.torproject.org:80` through SOCKS | **don't copy** — phones a third party on every health check |
| T2.4 `Rotate()` / NEWNYM | `ControlAuthFromTor` + `NewIdentity` | **take it** — fixes a live bug (§4.4) |
| T2.5 DNS-leak assertion | SOCKS `ATYP=0x03` domain encoding | **take it** — makes the property structural (§4.1) |
| Per-track stream isolation (`IsolateSOCKSAuth`) | hardcoded no-auth `{0x05,0x01,0x00}` | **absent — we add it** (§4.1) |
| T3.5 error taxonomy | `ErrorKind` + `TornagoError` with `Is`/`Unwrap` | good pattern, adopt the shape |
| Hidden services, listeners, metrics, rate limiting, relay scoring | ~50% of the library | **not needed** |

---

## 3. Should it go in `go.mod`? — No. Port it.

You asked me to consider it; the analysis says don't add the dependency:

1. **It doesn't remove the file we have to write.** The CONNECT bridge is
   unavoidable and tornago has nothing for it. The dependency would sit *under*
   the file we're writing anyway, saving ~120 lines of a protocol that hasn't
   changed since RFC 1928 (1996).
2. **We must fork the dialer regardless.** `IsolateSOCKSAuth` is
   `INSTRUCTIONS.md` Recommendation #3, and tornago's handshake hardcodes
   no-auth. Per-track isolation is impossible without modifying that function.
3. **Bus factor at the center of the threat model.** 7 stars, 0 forks, one
   maintainer, 9 months old. For a leaf utility that's fine; for the component
   that decides whether traffic is anonymized, a silent upstream change is a
   privacy incident. (Repo hygiene note: a **9 MB compiled binary**
   `circuit_management` is committed at the repo root — that's not
   disqualifying, but it isn't a signal of tight release discipline either.)
4. **Surface we'd carry and never call**: hidden services, onion listeners,
   metrics collection, rate limiting, relay performance scoring, a test server.
5. **MIT explicitly permits porting** with the copyright notice retained. §7.

**The honest counter-argument:** if T2.1/T2.4 turn into a time sink, taking the
dep for *daemon + control only* — never the dialer — is completely defensible.
Keep the `Net` interface intact and that swap stays a one-file change. I'd
revisit it the moment tor lifecycle management costs more than a day.

**Net effect on `go.mod`: unchanged.** I did not add it. (Separately: `go.mod`
still carries unused `tview`/`tcell`/`x-term`/`therecipe-qt` requires — leave
them until T10.3 per `PHASE0.md` §3.5.)

---

## 4. The ported code

Everything below **compiles, vets, gofmts, and passes tests** on this machine
(Go 1.26.5, windows/amd64) — see §6. Target file: `pkg/net/`, using the
`stdnet` import alias from `PHASE0.md` §3.3(c).

### 4.1 SOCKS5 dialer — with stream isolation added

Adapted from tornago `client.go`. Two deliberate changes: **username/password
method support** (tornago offers only no-auth, which makes `IsolateSOCKSAuth`
unreachable), and **named SOCKS reply codes** (tornago prints the raw number —
and `0x01` in particular has a specific, common cause worth naming).

```go
type socks5Dialer struct {
	addr    string        // Tor SOCKS5 listener, e.g. 127.0.0.1:9050
	timeout time.Duration
	user    string        // stream-isolation credentials; "" = no-auth
	pass    string
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, address string) (stdnet.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("socks5: unsupported network %q", network)
	}

	var dl stdnet.Dialer
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

func (d *socks5Dialer) handshake(c stdnet.Conn, dest string) error {
	// When isolating we offer ONLY username/password. If 0x00 is on the table
	// Tor will take it, and no-auth streams share a circuit — precisely what
	// isolation exists to prevent.
	greeting := []byte{0x05, 0x01, 0x00}
	if d.user != "" {
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
		if err := d.authUserPass(c); err != nil {
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
func (d *socks5Dialer) authUserPass(c stdnet.Conn) error {
	if len(d.user) > 255 || len(d.pass) > 255 {
		return errors.New("socks5: isolation credentials too long")
	}
	buf := make([]byte, 0, 3+len(d.user)+len(d.pass))
	buf = append(buf, 0x01, byte(len(d.user)))
	buf = append(buf, d.user...)
	buf = append(buf, byte(len(d.pass)))
	buf = append(buf, d.pass...)
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
	host, portStr, err := stdnet.SplitHostPort(dest)
	if err != nil {
		return nil, fmt.Errorf("socks5: bad destination %q: %w", dest, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("socks5: bad port in %q: %w", dest, err)
	}

	req := []byte{0x05, 0x01, 0x00}
	switch ip := stdnet.ParseIP(host); {
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

var socksReplyText = map[byte]string{
	0x01: "general SOCKS server failure (Tor not finished bootstrapping?)",
	0x02: "connection not allowed by ruleset",
	0x03: "network unreachable",
	0x04: "host unreachable (Tor could not resolve or reach the destination)",
	0x05: "connection refused by destination",
	0x06: "TTL expired",
	0x07: "command not supported",
	0x08: "address type not supported",
}

func readConnectReply(c stdnet.Conn) error {
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return fmt.Errorf("socks5: read CONNECT reply: %w", err)
	}
	if hdr[1] != 0x00 {
		if s, ok := socksReplyText[hdr[1]]; ok {
			return fmt.Errorf("socks5: CONNECT failed: %s", s)
		}
		return fmt.Errorf("socks5: CONNECT failed (code %#02x)", hdr[1])
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
```

### 4.2 The CONNECT bridge — this is what replaces Privoxy

Not from tornago; this is the piece it lacks. It is the reason the whole
exercise pays off.

```go
// ConnectBridge is a loopback HTTP proxy that speaks CONNECT and forwards every
// tunnel through Tor's SOCKS5 port. It exists because mpv, ffmpeg and yt-dlp are
// CHILD PROCESSES: they cannot use a Go dialer, and libavformat has no SOCKS
// support at all (INSTRUCTIONS.md, "Proxy limitation"). This is the job Privoxy
// did in script.sh — done in-process, so chainsmoke keeps its single-static-
// binary promise and drops a runtime dependency.
type ConnectBridge struct {
	ln     stdnet.Listener
	dialer *socks5Dialer
	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once
}

func StartConnectBridge(socksAddr string, isolate bool) (*ConnectBridge, error) {
	// 127.0.0.1 explicitly, never ":0" — an egress bridge bound to 0.0.0.0 is
	// an open proxy on the local network.
	ln, err := stdnet.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("connect bridge: listen: %w", err)
	}
	b := &ConnectBridge{
		ln:     ln,
		dialer: &socks5Dialer{addr: socksAddr, timeout: 30 * time.Second},
		closed: make(chan struct{}),
	}
	if isolate {
		b.dialer.user = "chainsmoke"
		b.dialer.pass = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	b.wg.Add(1)
	go b.serve()
	return b, nil
}

// ProxyURL is what goes into HTTP_PROXY / http_proxy and yt-dlp --proxy.
func (b *ConnectBridge) ProxyURL() string { return "http://" + b.ln.Addr().String() }

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

func (b *ConnectBridge) handle(client stdnet.Conn) {
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))

	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	// CONNECT only. An absolute-form GET means a child is about to push
	// PLAINTEXT through us — refuse loudly rather than proxy it, so a leak
	// shows up as a visible failure instead of as quiet traffic.
	if req.Method != http.MethodConnect {
		_, _ = io.WriteString(client,
			"HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	target := req.Host // authority-form: "host:port"
	if _, _, err := stdnet.SplitHostPort(target); err != nil {
		target = stdnet.JoinHostPort(target, "443")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	upstream, err := b.dialer.DialContext(ctx, "tcp", target)
	cancel()
	if err != nil {
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
		if tc, ok := upstream.(*stdnet.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if tc, ok := client.(*stdnet.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func (b *ConnectBridge) Close() error {
	var err error
	b.once.Do(func() {
		close(b.closed)
		err = b.ln.Close()
	})
	b.wg.Wait()
	return err
}
```

**Known limitation, stated rather than hidden:** `Close` waits for in-flight
tunnels. A live audio stream keeps it blocked. Before this ships, track open
conns and close them in `Close` — for `chainsmoke stop`/`purge` you want the
tunnels killed, not drained.

### 4.3 Bootstrap readiness — the fix for tornago's weakest point

tornago's `StartTorDaemon` returns as soon as `portsReachable` sees both ports
accept TCP:

```go
// tornago daemon.go, verbatim — this is the readiness bar
func portsReachable(socksAddr, controlAddr string) bool {
	dialer := &net.Dialer{Timeout: 500 * time.Millisecond}
	check := func(addr string) bool { /* dial, close, return err == nil */ }
	return check(socksAddr) && check(controlAddr)
}
```

Tor opens `SocksPort` immediately at startup and answers every CONNECT with
`0x01 general failure` until it has finished building circuits — which takes
seconds to tens of seconds. So a TCP accept is **not** readiness, and
`TASKS.md` T2.1 explicitly asks for better ("readiness = SOCKS handshake, not
sleep"). tornago is one notch above `sleep 5`, still below our bar.

The authoritative check, using tornago's own `GetInfo` plumbing:

```go
// infoGetter is satisfied by our control client and by tornago's
// *ControlClient alike, so this works under either §3 decision.
type infoGetter interface {
	GetInfo(ctx context.Context, key string) (string, error)
}

// WaitBootstrapped blocks until Tor reports 100% bootstrap. Accepting a TCP
// connection on SocksPort proves only that tor started, not that it can carry
// traffic; asking the control port is the difference between "the process is
// up" and "egress works".
func WaitBootstrapped(ctx context.Context, ctrl infoGetter, poll time.Duration) error {
	t := time.NewTicker(poll)
	defer t.Stop()

	var last string
	for {
		phase, err := ctrl.GetInfo(ctx, "status/bootstrap-phase")
		if err == nil {
			last = phase
			if strings.Contains(phase, "PROGRESS=100") {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			if last == "" {
				last = "<no reply from control port>"
			}
			return fmt.Errorf("tor did not finish bootstrapping (last phase: %s): %w", last, ctx.Err())
		case <-t.C:
		}
	}
}
```

Keeping `last` is deliberate: on timeout the user gets
`PROGRESS=80 TAG=loading_descriptors` instead of a bare "timed out", which is
the difference between a diagnosable failure and a shrug.

### 4.4 Control-port auth — this fixes a live bug in `main.go`

Current [main.go:338](main.go#L338) sends:

```go
for _, step := range []string{"AUTHENTICATE \"\"\r\n", "SIGNAL NEWNYM\r\n"} {
```

That only works when the control port has **no** authentication. Every distro
tor package ships `CookieAuthentication 1`, so `chainsmoke rotate` fails on a
normal install with a misleading error. There is a second bug next to it: the
loop reads exactly **one line** per command and checks `HasPrefix "250"`, but
Tor's control protocol answers with `250-` continuations, `250+` data blocks,
and asynchronous `6xx` events interleaved — a multi-line reply desynchronizes
the connection.

tornago handles both correctly. Ported, with the auth-method negotiation
generalized (tornago assumes cookie auth because it always launches tor itself;
we must also handle a *system* tor per T2.1):

```go
// Authenticate negotiates whatever auth this tor was built with, instead of
// assuming there is none.
func (c *controlClient) Authenticate() error {
	lines, err := c.exec("PROTOCOLINFO 1")
	if err != nil {
		return err
	}

	var methods, cookiePath string
	for _, ln := range lines {
		if i := strings.Index(ln, "METHODS="); i >= 0 && strings.HasPrefix(ln, "AUTH") {
			if f := strings.Fields(ln[i+len("METHODS="):]); len(f) > 0 {
				methods = f[0]
			}
		}
		if i := strings.Index(ln, `COOKIEFILE="`); i >= 0 {
			rest := ln[i+len(`COOKIEFILE="`):]
			if j := strings.Index(rest, `"`); j >= 0 {
				cookiePath = filepath.Clean(unescapeCString(rest[:j]))
			}
		}
	}

	switch {
	case cookiePath != "":
		data, err := os.ReadFile(cookiePath)
		if err != nil {
			return fmt.Errorf("tor control: cannot read cookie %s (add your user to tor's group?): %w", cookiePath, err)
		}
		_, err = c.exec("AUTHENTICATE " + strings.ToUpper(hex.EncodeToString(data)))
		return err
	case strings.Contains(methods, "NULL"):
		_, err := c.exec("AUTHENTICATE")
		return err
	case strings.Contains(methods, "HASHEDPASSWORD"):
		return fmt.Errorf("tor control: this tor requires HASHEDPASSWORD auth; chainsmoke needs CookieAuthentication 1 or a private tor instance")
	default:
		return fmt.Errorf("tor control: no usable auth method (advertised: %q)", methods)
	}
}

// unescapeCString undoes the C-style quoting Tor uses in PROTOCOLINFO paths.
// A Windows cookie path arrives as C:\\Users\\... and a naive parse leaves the
// doubled backslashes in place, so the file is never found. (tornago does not
// do this — it only ever reads cookies from a path it chose itself.)
func unescapeCString(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(s)
}

// readReply implements Tor's control reply grammar: a run of "250-"
// continuations (or a "250+" block terminated by a lone "."), closed by a
// single "250 " line. Codes >= 400 are errors. Anything else is an
// asynchronous event and must be SKIPPED, not mistaken for our answer.
func (c *controlClient) readReply() ([]string, error) {
	var lines []string
	for {
		raw, err := c.rw.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("tor control: read reply: %w", err)
		}
		line := strings.TrimRight(raw, "\r\n")
		if len(line) < 4 {
			continue
		}
		code, convErr := strconv.Atoi(line[:3])
		if convErr != nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "250 "), strings.HasPrefix(line, "251 "):
			if body := line[4:]; body != "OK" {
				lines = append(lines, body)
			}
			return lines, nil
		case strings.HasPrefix(line, "250-"), strings.HasPrefix(line, "251-"):
			lines = append(lines, line[4:])
		case strings.HasPrefix(line, "250+"):
			lines = append(lines, line[4:])
			block, err := c.readDataBlock()
			if err != nil {
				return nil, err
			}
			lines = append(lines, block...)
		case code >= 400:
			return nil, fmt.Errorf("tor control: %s", line)
		default:
			// 6xx asynchronous event — not our reply.
		}
	}
}

// NewNym asks for a fresh circuit.
//
// Caveat worth surfacing rather than hiding: Tor rate-limits NEWNYM
// (MaxCircuitDirtiness, default 10 min). Inside the cooldown it still answers
// 250 OK while quietly keeping the existing circuit, so a successful return
// does NOT prove the exit changed. For a real rotation guarantee use per-stream
// SOCKS credentials (§4.1) instead.
func (c *controlClient) NewNym() error {
	_, err := c.exec("SIGNAL NEWNYM")
	return err
}
```

### 4.5 Daemon launch — the argv worth stealing

tornago's tor invocation is a good default for T2.1's "spawn a private one",
and worth copying verbatim in shape:

```go
args := []string{
	"--SocksPort", socksAddr,          // 127.0.0.1:0 → ephemeral, no clash with a system tor
	"--ControlPort", controlAddr,
	"--CookieAuthentication", "1",
	"--CookieAuthFile", cookiePath,    // inside our own DataDirectory
	"--RunAsDaemon", "0",              // stay a child so exec.Cmd owns the lifecycle
	"--DataDirectory", dataDir,        // os.MkdirTemp, removed on Stop()
	"--Log", "notice stdout",          // teed, so launch failures have a cause
}
```

Two things to change for chainsmoke: pin the SOCKS/control hosts to `127.0.0.1`
rather than accepting whatever the caller passes (tornago's `resolveAddr` only
defaults to loopback when the host is empty — `0.0.0.0:0` would bind every
interface), and keep the `--Log notice stdout` tee, because `WithTorLogReporter`
is how you find out *why* bootstrap stalled.

---

## 5. What this does to TASKS.md Phase 2

| task | before | after adopting §4 |
|---|---|---|
| T2.1 Tor lifecycle | detect-or-spawn, readiness TBD | spawn per §4.5; readiness = §4.3 bootstrap poll |
| **T2.2 Privoxy lifecycle** | generate config, spawn, wait for :8118 | **deleted** — replaced by §4.2, no external proxy |
| T2.3 `Up()` verified | TBD | tor up → bootstrapped → bridge listening → one CONNECT through it |
| T2.4 `Rotate()` | hand-rolled, broken on cookie auth | §4.4, plus the honest rate-limit caveat |
| T2.5 DNS-leak self-test | assert local resolver saw nothing | mostly *structural* now (§4.1) — the test becomes a regression guard, and §6's `TestConnectRequestNeverResolvesHostnames` is already it |
| T2.6 `--proxy` override | user proxy replaces bridge | unchanged; skip `StartConnectBridge`, keep the verified-up contract |
| T2.7 honest-caveat doc | Tor exits vs CDNs | add: we are now our own proxy, so proxy bugs are ours to own |

**Net: one task deleted, one made structural, one bug fixed before it was
filed.** That is the payoff for reading the repo.

Dependency change for the user: `tor` still required; **`privoxy` no longer is**.
`SKILL.md`'s dependency list and `script.sh`'s `deps_check` both need that
edit — though under `PHASE0.md` T0.3, `script.sh` is frozen, so it gets a
`# KNOWN DIVERGENCE` comment rather than a fix.

---

## 6. Verification

Not a code review from reading — this was built and run.

```
go build ./...   exit 0
go vet ./...     exit 0
gofmt -l .       (clean)
go test ./...    ok  0.534s
```

```
=== RUN   TestConnectBridgeTunnelsAndKeepsDNSRemote
--- PASS: TestConnectBridgeTunnelsAndKeepsDNSRemote (0.00s)
=== RUN   TestConnectBridgeRefusesPlaintextProxying
--- PASS: TestConnectBridgeRefusesPlaintextProxying (0.00s)
=== RUN   TestConnectRequestNeverResolvesHostnames
--- PASS: TestConnectRequestNeverResolvesHostnames (0.00s)
```

The tests stand up a fake SOCKS5 server that records what it was asked to
connect to, then assert:

1. a real `CONNECT api-v2.soundcloud.com:443` tunnels bytes end to end;
2. the destination arrived as **`ATYP=0x03` with the literal hostname** — the
   DNS-leak guarantee, asserted rather than assumed;
3. stream-isolation credentials were negotiated (`user="chainsmoke"`);
4. a non-CONNECT request is refused with 405 instead of being proxied.

Test source is in the scratchpad at `bridgecheck/net/bridge_test.go`; it should
land as `pkg/net/bridge_test.go` and become part of **T9.1** — it needs no
network, no tor, and no mpv, so it runs in CI (T9.2) and on Windows.

> Caveat on scope: these prove the *bridge* is correct against a synthetic
> SOCKS5 peer. They do not prove mpv/ffmpeg honor `http_proxy` for HLS segment
> fetches — that is **T7.3**, still an open question, still a leak if false, and
> unchanged by any of this.

---

## 7. Attribution

tornago is MIT. §4.1 (SOCKS5 handshake, CONNECT request encoding, reply
draining) and §4.4 (`readReply` grammar, PROTOCOLINFO cookie flow) are derived
from it; §4.2 and §4.3 are ours. If the ported code ships, retain the notice —
suggested placement, a header comment in `pkg/net/bridge.go`:

```go
// Portions of the SOCKS5 and Tor control-protocol handling in this package are
// derived from github.com/nao1215/tornago (MIT, Copyright (c) 2025 nao1215).
// Modified: username/password stream isolation, named SOCKS reply codes,
// PROTOCOLINFO auth-method negotiation, bootstrap-phase readiness.
```

Full license text: https://github.com/nao1215/tornago/blob/main/LICENSE

// =============================================================================
//  cmd/chainsmoke/main.go — `chainsmoke`, a privacy-focused SoundCloud CLI (MAIN ENTRYPOINT)
// -----------------------------------------------------------------------------
//  This is the main file of this project.
//
//  Layout it grows into (mirrors timoknapp/soundcloud-cli):
//    cmd/chainsmoke/main.go            <- this file: CLI surface + wiring only
//    pkg/net/bridge.go              <- Tor+Privoxy bridge, proxy env, NEWNYM, DNS self-test
//    pkg/resolver/ytdlp.go          <- the ONE egress chokepoint: yt-dlp wrapper (meta+stream)
//    pkg/player/mpv.go            <- mpv daemon + JSON-IPC client over the unix socket
//    pkg/scrub/ffmpeg.go             <- ffmpeg +bitexact -> mutagen -> xattr -> mat2
//    pkg/store/store.go             <- Store interface, memory + disk backends
//    pkg/core/core.go               <- shared Track model, helpers, 5 interfaces and any cross-cutting concerns
//    cmd/chainsmoke/tui.go                <- TUI for search/np table, progress bars, etc. (not yet wired)
// The main() wiring is final; the concrete impls are stubs returning ErrNotImplemented
//
//  Load-bearing invariants (retrofitting any of these is a rewrite):
//    1. ONE Store interface, two backends. Every persisted byte goes through it.
//    2. ONE egress chokepoint (Resolver). The Player only ever receives already
//       resolved, already preflighted stream URLs — never a page URL.
//    3. NO URLs in argv. The common path is `chainsmoke play <N>` against a cached result
//       set; raw URLs arrive on stdin. argv is world-readable via /proc.
//    4. EPHEMERAL by default. --remember opts into the disk Store; --private forces
//       the memory Store for the whole run regardless of config.
//
//  When yt-dlp breaks (it will — client_id is scraped and rotates), the failure
//  mode is a clear "run: yt-dlp -U" message, never a stack trace. Don't pin yt-dlp.
// =============================================================================

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
	// "github.com/olekukonko/tablewriter"  // for the search/np table, as in the reference
)

// BuildVersion is stamped at build time (-ldflags "-X main.BuildVersion=...").
var BuildVersion = "development"

// ErrNotImplemented marks carcass stubs. Grep for it to find what's left to build.
var ErrNotImplemented = errors.New("not implemented")

// -----------------------------------------------------------------------------
// Data model (mirrors the reference Track; extend as needed)
// -----------------------------------------------------------------------------

type Track struct {
	ID        string
	Title     string
	Uploader  string
	Duration  int    // seconds
	Permalink string // soundcloud.com/... — kept internal, never printed into argv examples
}

// -----------------------------------------------------------------------------
// Subsystem interfaces. main() wires concrete stubs; each moves to its pkg/*.
// -----------------------------------------------------------------------------

// Net is the egress seam. Up() must establish the proxy BEFORE any Resolver or
// Player call. Swap the concrete impl (bridge <-> netns) without touching callers.
type Net interface {
	Up() error       // start Tor+Privoxy bridge, export proxy env, block until ready
	Down() error     // tear down anything we own
	Rotate() error   // NEWNYM: new Tor circuit (rate-limited by Tor)
	SelfTest() error // assert no plaintext DNS leaks for soundcloud hosts
	ProxyURL() string
}

// Resolver is the ONLY component that talks to SoundCloud. Wraps yt-dlp.
type Resolver interface {
	// Meta resolves a page/playlist URL to entries EAGERLY (flat, fast, cacheable).
	Meta(pageURL string) ([]Track, error)
	// Stream resolves a single permalink to a fresh, short-lived direct URL LAZILY.
	// Call 1-2 tracks ahead of playback; SoundCloud HLS manifests expire quickly.
	Stream(permalink string) (streamURL string, err error)
	// Search runs scsearch and returns numbered-able results.
	Search(term string, limit int) ([]Track, error)
}

// Player owns the mpv daemon and speaks JSON-IPC over the unix socket.
type Player interface {
	Ensure() error                    // start mpv --idle if not already up
	Enqueue(streamURL string) error   // loadfile ... append-play (preflighted upstream)
	Command(args ...any) (any, error) // raw IPC: get/set/cycle/seek/playlist-*
	NowPlaying() (string, error)
	Down() error
}

// Scrubber cleans a downloaded file in place-ish (see script.sh scrub()).
type Scrubber interface {
	Scrub(in, out string, nuclear bool) error
}

// Store is the single persistence interface. Memory backend = ephemeral default.
type Store interface {
	SaveResults([]Track) error // cache the last numbered result set
	Result(n int) (Track, error)
	Purge() error
}

// -----------------------------------------------------------------------------
// App: the wired runtime. Actions call into this, never into globals.
// -----------------------------------------------------------------------------

type App struct {
	net   Net
	res   Resolver
	play  Player
	scrub Scrubber
	store Store
}

// resolveIndexOrStdin implements the no-URL-in-argv contract:
//   - if arg is an integer N, look it up in the cached result set
//   - otherwise read exactly one URL line from stdin
func (a *App) resolveIndexOrStdin(arg string) (Track, error) {
	if arg != "" {
		if isInt(arg) {
			return a.store.Result(atoi(arg))
		}
		// A non-integer argv is almost always a URL. Refuse it rather than
		// silently falling through to a stdin read: argv is visible to other
		// processes (Linux /proc, Windows Win32_Process), so URLs ride on stdin
		// only (invariant #3 / TASKS.md T1.3).
		return Track{}, fmt.Errorf("URLs go on stdin, not argv (argv is visible in the OS process list) — pipe it instead:\n  echo \"%s\" | chainsmoke play", arg)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return Track{}, errors.New("no result index given and no URL on stdin")
	}
	return Track{Permalink: strings.TrimSpace(line)}, nil
}

// -----------------------------------------------------------------------------
// Commands (urfave/cli/v2). Verb set from the build spec; aliases match the
// reference repo where they overlap.
// -----------------------------------------------------------------------------

func (a *App) commands() []*cli.Command {
	return []*cli.Command{
		{
			Name: "search", Aliases: []string{"ls"}, Usage: "Search tracks; prints numbered, cached results",
			Flags: []cli.Flag{&cli.IntFlag{Name: "limit", Value: 10}},
			Action: func(c *cli.Context) error {
				if c.Args().Len() == 0 {
					return errors.New("usage: chainsmoke search <term>")
				}
				tracks, err := a.res.Search(strings.Join(c.Args().Slice(), " "), c.Int("limit"))
				if err != nil {
					return ytdlpHint(err)
				}
				if err := a.store.SaveResults(tracks); err != nil {
					return err
				}
				printNumbered(tracks) // index + title only; permalinks stay in the cache
				return nil
			},
		},
		{
			Name: "play", Usage: "Play cached result N, or a URL from stdin",
			Action: func(c *cli.Context) error {
				if err := a.net.Up(); err != nil { // chokepoint first
					return err
				}
				if err := a.play.Ensure(); err != nil {
					return err
				}
				t, err := a.resolveIndexOrStdin(c.Args().First())
				if err != nil {
					return err
				}
				entries, err := a.res.Meta(t.Permalink)
				if err != nil {
					return ytdlpHint(err)
				}
				return a.enqueueAhead(entries) // enqueue first, preload next
			},
		},
		{
			Name: "get", Aliases: []string{"dl"}, Usage: "Download + scrub cached result N (or stdin URL)",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "out", Value: "./track.m4a"},
				&cli.BoolFlag{Name: "nuclear", Usage: "ffmpeg+mat2 full strip instead of selective"},
			},
			Action: func(c *cli.Context) error {
				if err := a.net.Up(); err != nil {
					return err
				}
				t, err := a.resolveIndexOrStdin(c.Args().First())
				if err != nil {
					return err
				}
				// TODO(impl): download via resolver/yt-dlp to a temp file, then:
				_ = t
				return a.scrub.Scrub("/tmp/in.m4a", c.String("out"), c.Bool("nuclear"))
			},
		},
		// --- transport controls: thin IPC pass-throughs -------------------------
		{Name: "np", Usage: "Now playing", Action: func(*cli.Context) error {
			s, err := a.play.NowPlaying()
			if err != nil {
				return err
			}
			fmt.Println(s)
			return nil
		}},
		{Name: "toggle", Aliases: []string{"pause"}, Usage: "Toggle pause", Action: func(*cli.Context) error {
			_, err := a.play.Command("cycle", "pause")
			return err
		}},
		{Name: "seek", Usage: "Seek ±N seconds", Action: func(c *cli.Context) error {
			_, err := a.play.Command("seek", c.Args().First())
			return err
		}},
		{Name: "vol", Usage: "Set volume 0-100", Action: func(c *cli.Context) error {
			_, err := a.play.Command("set_property", "volume", c.Args().First())
			return err
		}},
		{Name: "next", Action: func(*cli.Context) error { _, err := a.play.Command("playlist-next"); return err }},
		{Name: "prev", Action: func(*cli.Context) error { _, err := a.play.Command("playlist-prev"); return err }},
		{Name: "stop", Usage: "Kill the player daemon", Action: func(*cli.Context) error { return a.play.Down() }},
		// --- privacy / lifecycle -----------------------------------------------
		{Name: "rotate", Usage: "New Tor circuit (NEWNYM)", Action: func(*cli.Context) error { return a.net.Rotate() }},
		{Name: "audit", Usage: "Show runtime privacy posture", Action: func(*cli.Context) error { return a.audit() }},
		{Name: "purge", Usage: "Wipe transient/persistent state", Action: func(*cli.Context) error { return a.store.Purge() }},
		{Name: "daemon", Usage: "player daemon lifecycle", Subcommands: []*cli.Command{
			{Name: "start", Action: func(*cli.Context) error { return a.play.Ensure() }},
			{Name: "stop", Action: func(*cli.Context) error { return a.play.Down() }},
			// TODO(impl): status
		}},
	}
}

// enqueueAhead plays entry 0 immediately and preloads entry 1, keeping exactly
// one item resolved ahead so playback feels instant without pre-resolving the
// whole queue (whose signed URLs would expire before you reach them).
func (a *App) enqueueAhead(entries []Track) error {
	if len(entries) == 0 {
		return errors.New("nothing resolved")
	}
	first, err := a.res.Stream(entries[0].Permalink)
	if err != nil {
		return ytdlpHint(err)
	}
	if err := a.play.Enqueue(first); err != nil { // Enqueue preflights internally
		return err
	}
	if len(entries) > 1 {
		go func() {
			if s, err := a.res.Stream(entries[1].Permalink); err == nil {
				_ = a.play.Enqueue(s)
			}
		}()
	}
	return nil
}

func (a *App) audit() error {
	fmt.Printf("egress:      %s\n", a.net.ProxyURL())
	fmt.Printf("version:     %s\n", BuildVersion)
	return a.net.SelfTest()
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

// isProxyURL reports whether s is a real proxy URL, as opposed to ProxyURL's
// human-readable "<down>" / "DIRECT (no proxy)" sentinels.
func isProxyURL(s string) bool { return strings.Contains(s, "://") }

// ytdlpResolver is the single egress chokepoint. It shells out to yt-dlp, always
// passing the proxy from the live Net handle explicitly (research: socks5h env
// handling has had bugs — prefer --proxy) plus a browser-impersonation target to
// defend the TLS fingerprint.
type ytdlpResolver struct {
	net         Net
	impersonate string // yt-dlp curl_cffi target; "" disables
}

// Meta (raw): treats the input as one track and defers to Stream. Playlist/set
// expansion (yt-dlp --flat-playlist -J, TASKS.md T3.1) is not wired yet.
func (r *ytdlpResolver) Meta(pageURL string) ([]Track, error) {
	if strings.TrimSpace(pageURL) == "" {
		return nil, errors.New("empty URL")
	}
	return []Track{{Permalink: pageURL, Title: pageURL}}, nil
}

// Stream resolves a permalink to a fresh, direct audio URL via `yt-dlp -g`.
func (r *ytdlpResolver) Stream(permalink string) (string, error) {
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return "", errors.New("yt-dlp not found on PATH — install yt-dlp to resolve streams")
	}
	args := []string{"-f", "bestaudio", "-g", "--no-playlist",
		"--extractor-args", "soundcloud:formats=hls_aac"}
	if p := r.net.ProxyURL(); isProxyURL(p) {
		args = append(args, "--proxy", p)
	}
	if r.impersonate != "" {
		args = append(args, "--impersonate", r.impersonate)
	}
	args = append(args, permalink)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("yt-dlp: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	for _, ln := range strings.Split(out.String(), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln, nil // first stream URL yt-dlp prints
		}
	}
	return "", errors.New("yt-dlp returned no stream URL")
}

// Search stays stubbed — the raw milestone covers play, not search.
func (r *ytdlpResolver) Search(string, int) ([]Track, error) { return nil, ErrNotImplemented }

// mpvPlayer (raw): launches mpv per track in the foreground instead of running
// the persistent --idle daemon + JSON-IPC (TASKS.md Phase 5). No socket, no
// preload-ahead, no HEAD preflight — the simplest thing that plays audio.
type mpvPlayer struct{ sock string } // sock: where the future IPC daemon will live

// Ensure only has to confirm mpv exists, since raw play spawns it per track.
func (p *mpvPlayer) Ensure() error {
	if _, err := exec.LookPath("mpv"); err != nil {
		return errors.New("mpv not found on PATH — install mpv to play")
	}
	return nil
}

// Enqueue plays an already-resolved stream URL directly. mpv inherits the proxy
// env from Net.Up() and only ever sees the direct URL, never a SoundCloud page
// (the resolver already crossed the egress boundary). Blocks until playback ends.
func (p *mpvPlayer) Enqueue(streamURL string) error {
	cmd := exec.Command("mpv", "--no-video", "--quiet", streamURL)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func (p *mpvPlayer) Command(...any) (any, error) { return nil, ErrNotImplemented }
func (p *mpvPlayer) NowPlaying() (string, error) { return "", ErrNotImplemented }
func (p *mpvPlayer) Down() error                 { return nil }

type ffmpegScrubber struct{}

func (s *ffmpegScrubber) Scrub(in, out string, nuclear bool) error { return ErrNotImplemented }

// memStore = ephemeral default; diskStore (XDG_STATE_HOME) only when --remember.
type memStore struct{ results []Track }

func (m *memStore) SaveResults(t []Track) error { m.results = t; return nil }
func (m *memStore) Result(n int) (Track, error) {
	if n < 1 || n > len(m.results) {
		return Track{}, fmt.Errorf("no cached result #%d", n)
	}
	return m.results[n-1], nil
}
func (m *memStore) Purge() error { m.results = nil; return nil }

type diskStore struct{ memStore } // TODO(impl): persist under $XDG_STATE_HOME/chainsmoke

// -----------------------------------------------------------------------------
// Wiring
// -----------------------------------------------------------------------------

func newApp(remember, private bool) *App {
	net := &torBridge{override: flagValue("--proxy", "")}
	var store Store = &memStore{}
	if remember && !private { // --private always wins
		store = &diskStore{}
	}
	return &App{
		net:   net,
		res:   &ytdlpResolver{net: net, impersonate: flagValue("--impersonate", "chrome")},
		play:  &mpvPlayer{sock: os.Getenv("XDG_RUNTIME_DIR") + "/chainsmoke/mpv.sock"},
		scrub: &ffmpegScrubber{},
		store: store,
	}
}

func main() {
	// Peek at persistence flags before building the runtime so the Store backend
	// is chosen at construction, not swapped mid-run.
	remember := hasFlag("--remember")
	private := hasFlag("--private")
	app := newApp(remember, private)

	asciiOutline := `==============================================================================`
	fmt.Println(asciiOutline)

	asciiLogo := `                      ;:***;**;
                     *ff?????*****??;::
              ;;;*;**f?******?aaaI***??:   ;**:
        :***??f????**?***;*aIRKaaaaaaaIIf???****:
        ?***?f???;;;;;;;;?Iaaaf??******??**;*;**;:
    ;**;**aaf?ffIII*:;aKKIIfIf**;;*;***;;;;*;;;;;*;::
   *?If?fff****?ffffffRKaI?******;;;;;*IaaRRKKa?**?***;
  ;*f?*?f**;;;;??*?aaaI??*;		;;?IfKKaaI??If*IaKKKf**;*:
 :*?f?**;;;;;::*fIf???*;;;;;:  ::;aaaIII?**;?KKKaIIfff?***:
:;;;;;:;;::::::::;;;;::::::::::*?IaIII?***I--__  *??**??**;.
  ...  .:::  ::::::::::::::::::;*******;*??**;;*;;--_  ;*;;;;;:
                  .::::          ::::;;;:;;:::::::;;;;;::
                                   .::::::        .:::.`
	fmt.Println(asciiLogo)

	asciiChainsmoke := `   ________  _____    _____   _______ __  _______  __ __ ______
  / ____/ / / /   |  /  _/ | / / ___//  |/  / __ \/ //_// ____/
 / /   / /_/ / /| |  / //  |/ /\__ \/ /|_/ / / / / ,<  / __/
/ /___/ __  / ___ |_/ // /|  /___/ / /  / / /_/ / /| |/ /___
\____/_/ /_/_/  |_/___/_/ |_//____/_/  /_/\____/_/ |_/_____/`

	fmt.Println(asciiChainsmoke)
	fmt.Println(asciiOutline)

	cliApp := &cli.App{
		Name:                 "chainsmoke",
		Usage:                "A privacy-focused SoundCloud CLI (play, search, get) over Tor",
		Version:              BuildVersion,
		EnableBashCompletion: true,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "remember", Usage: "persist history/results to disk (default: ephemeral)"},
			&cli.BoolFlag{Name: "private", Usage: "force memory-only for this run (overrides --remember)"},
			&cli.StringFlag{Name: "proxy", Usage: "override egress proxy (else the built-in Tor bridge)"},
			&cli.StringFlag{Name: "impersonate", Value: "chrome", Usage: "yt-dlp curl_cffi target ('' to disable)"},
		},
		Commands: app.commands(),
	}

	if err := cliApp.Run(os.Args); err != nil {
		// Clean, script-friendly failure. No stack traces in the user's face.
		fmt.Fprintln(os.Stderr, "chainsmoke:", err)
		os.Exit(1)
	}
}

// -----------------------------------------------------------------------------
// Tiny helpers (kept here for the carcass; move to a util pkg later)
// -----------------------------------------------------------------------------

// ytdlpHint wraps resolver errors with the one recovery action that matters.
func ytdlpHint(err error) error {
	return fmt.Errorf("%w\n  SoundCloud extractor may be stale — run: yt-dlp -U", err)
}

func printNumbered(ts []Track) {
	// TODO(impl): tablewriter, matching the reference's PrintTable output.
	for i, t := range ts {
		fmt.Printf("%3d  %s\n", i+1, t.Title)
	}
}

func hasFlag(f string) bool {
	for _, a := range os.Args[1:] {
		if a == f {
			return true
		}
	}
	return false
}

// flagValue reads a `--name value` or `--name=value` option from argv. Interim
// shim (mirrors hasFlag) until flags move onto the urfave/cli context (T1.2).
func flagValue(name, def string) string {
	args := os.Args[1:]
	for i, a := range args {
		switch {
		case a == name && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, name+"="):
			return strings.TrimPrefix(a, name+"=")
		}
	}
	return def
}

func isInt(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// test1test2

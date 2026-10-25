// Package core holds the shared data model, the subsystem interfaces, the wired
// App, and the few cross-cutting helpers every other package needs.
package core

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// ErrNotImplemented marks features that are not implemented yet.
var ErrNotImplemented = errors.New("not implemented")

// Track is the only shape SoundCloud metadata takes inside chainsmoke.
type Track struct {
	ID        string
	Title     string
	Uploader  string
	Duration  int
	Permalink string
}

// Net is the egress seam.
type Net interface {
	Up() error
	Down() error
	Rotate() error
	SelfTest() error
	ProxyURL() string

	// Assert is the fail-closed gate. Every method that causes a network call must
	// call it first and refuse on error.
	Assert() error

	// Verified reports whether the path was proven private, as opposed to merely
	// permitted.
	Verified() bool
}

// Resolver is the ONLY component that talks to SoundCloud.
type Resolver interface {
	// Meta resolves a page/playlist URL to entries EAGERLY (flat, fast, cacheable).
	Meta(pageURL string) ([]Track, error)
	// Stream resolves a single permalink to a fresh, short-lived direct URL LAZILY.
	Stream(permalink string) (streamURL string, err error)
	// Search runs scsearch and returns numbered-able results.
	Search(term string, limit int) ([]Track, error)
}

// Player owns the mpv daemon and speaks JSON-IPC over the unix socket (named
// pipe on Windows).
type Player interface {
	Ensure() error
	Enqueue(streamURL, title string) error
	Command(args ...any) (any, error)
	NowPlaying() (string, error)
	Down() error
}

// Scrubber cleans a downloaded file.
type Scrubber interface {
	Scrub(in, out string, nuclear bool) error
}

// Store is the single persistence interface. Memory backend = ephemeral
// default.
type Store interface {
	SaveResults([]Track) error
	Result(n int) (Track, error)
	Results() ([]Track, error)
	Purge() error
}

// App fields are exported so cmd/chainsmoke can wire concrete impls in and
// tests can wire fakes in.
type App struct {
	Net     Net
	Res     Resolver
	Play    Player
	Scrub   Scrubber
	Store   Store
	Version string

	Egress Config

	Stdin io.Reader
	Out   io.Writer
}

func (a *App) stdin() io.Reader {
	if a.Stdin == nil {
		return os.Stdin
	}
	return a.Stdin
}

func (a *App) out() io.Writer {
	if a.Out == nil {
		return os.Stdout
	}
	return a.Out
}

// ResolveIndexOrStdin implements the no-URL-in-argv contract.
func (a *App) ResolveIndexOrStdin(arg string, typed bool) (Track, error) {
	if arg != "" {
		n, err := strconv.Atoi(arg)
		if err == nil {
			return a.Store.Result(n)
		}
		if errors.Is(err, strconv.ErrRange) {
			return Track{}, fmt.Errorf("result index %s is out of range", arg)
		}
		if typed {
			return Track{Permalink: arg}, nil
		}
		return Track{}, fmt.Errorf(
			"URLs are not accepted in argv — the OS process list is world-readable,\n"+
				"and this one already reached it. Treat %q as exposed.\n\n"+
				"  chainsmoke play              # prompts; leaks neither argv nor shell history\n"+
				"  chainsmoke play < url.txt    # for scripts\n"+
				"  chainsmoke search <term> && chainsmoke play 1   # no URL anywhere",
			arg)
	}
	line, err := a.readLine("URL (stays out of argv and shell history): ")
	if line == "" {
		if err != nil {
			return Track{}, errors.New("no result index given and no URL on stdin")
		}
		return Track{}, errors.New("empty URL on stdin")
	}
	return Track{Permalink: line}, nil
}

func (a *App) readLine(prompt string) (string, error) {
	p, typed := a.Stdin.(interface{ Prompting() bool })
	if (a.Stdin == nil && StdinIsTerminal()) || (typed && p.Prompting()) {
		fmt.Fprint(os.Stderr, prompt)
	}
	line, err := bufio.NewReader(a.stdin()).ReadString('\n')
	return strings.TrimSpace(line), err
}

// StdinIsTerminal reports whether stdin is an interactive console rather than a
// pipe, a file, or a null device (isTerminal in term_*.go).
func StdinIsTerminal() bool { return isTerminal(os.Stdin) }

// DNSAuditor is optionally implemented by a Net that can report what the local
// resolver is able to observe.
type DNSAuditor interface {
	AuditLocalDNS() (visible bool, lines []string)
}

// EgressAuditor is optionally implemented by a Net that can account for what
// crossed it.
type EgressAuditor interface {
	EgressLedger() EgressLedger
}

// StoreDescriber is optionally implemented by a Store that can say where it
// keeps things and how much is there.
type StoreDescriber interface {
	Describe() (mode string, results int)
}

// Audit reports the runtime privacy posture. Everything about the live path is
// PROBED, never read back from config — that distinction is the whole point
// of the verb, and the saved config below does not weaken it: the config is an
// EXPECTATION the probe is compared against, never a substitute for it.
func (a *App) Audit() error {
	live := a.Egress

	posture := "DOWN ✋ (no egress permitted — fail-closed)"
	switch {
	case a.Net.Verified() && live.Proxy != "":
		posture = "PROXY ⚠  (verified by a live CONNECT through --proxy; NOT proven to be Tor)"
	case a.Net.Verified():
		posture = "TOR ✓  (verified by a live CONNECT)"
	case a.Net.Assert() == nil:
		posture = "DIRECT ⚠  (permitted by --allow-direct; NOT anonymized)"
	}

	fmt.Fprintf(a.out(), "egress:      %s\n", posture)
	fmt.Fprintf(a.out(), "proxy:       %s\n", shownProxy(a.Net.ProxyURL()))
	fmt.Fprintf(a.out(), "impersonate: %s\n", describeImpersonate(live.Impersonate))

	if d, ok := a.Store.(StoreDescriber); ok {
		mode, results := d.Describe()
		fmt.Fprintf(a.out(), "store:       %s\n", mode)
		fmt.Fprintf(a.out(), "results:     %d cached\n", results)
	}
	fmt.Fprintf(a.out(), "runtime dir: %s\n", RuntimeDir())
	fmt.Fprintf(a.out(), "version:     %s\n", a.Version)

	a.auditConfig(live)
	a.auditEgressLedger()

	if d, ok := a.Net.(DNSAuditor); ok {
		_, lines := d.AuditLocalDNS()
		fmt.Fprintln(a.out(), "\nlocal DNS:")
		for _, ln := range lines {
			fmt.Fprintf(a.out(), "  %s\n", ln)
		}
	}
	return a.Net.SelfTest()
}

func shownProxy(p string) string {
	if u, err := url.Parse(p); err == nil && u.User != nil {
		return u.Redacted()
	}
	return p
}

func (a *App) auditConfig(live Config) {
	saved, exists, err := LoadConfig()
	switch {
	case err != nil:
		fmt.Fprintf(a.out(), "\nsaved config: %v\n", err)
		return
	case !exists:
		fmt.Fprintf(a.out(), "\nsaved config: none — run `chainsmoke audit --save` to record this one\n")
		return
	}

	fmt.Fprintf(a.out(), "\nsaved config: %s, verified %s\n",
		saved.SavedAt.Local().Format("2006-01-02 15:04"), saved.SavedPosture)

	drift := saved.Diff(live)
	if len(drift) == 0 {
		fmt.Fprintln(a.out(), "  ✓ this run matches it")
	} else {
		for _, d := range drift {
			fmt.Fprintf(a.out(), "  ⚠ %-12s saved %s, running %s\n", d.Field, d.Saved, d.Live)
		}
	}

	if saved.SavedPosture == "TOR" && !a.Net.Verified() {
		fmt.Fprintln(a.out(), "  ⚠ you saved this as an ANONYMIZED setup and this run is not anonymized")
	}
}

func (a *App) auditEgressLedger() {
	auditor, ok := a.Net.(EgressAuditor)
	if !ok {
		return
	}
	l := auditor.EgressLedger()

	fmt.Fprintln(a.out(), "\negress ledger (this process):")
	if !l.Observed {
		fmt.Fprintln(a.out(), "  not observable — this run does not own the bridge (--proxy or DIRECT)")
		return
	}
	if len(l.Targets) == 0 {
		fmt.Fprintln(a.out(), "  no tunnels opened yet in this process")
	}
	for _, target := range sortedTargets(l.Targets) {
		mark := "✓ expected"
		host := target
		if h, _, err := net.SplitHostPort(target); err == nil {
			host = h
		}
		if !ExpectedHost(host) {
			mark = "⚠ UNEXPECTED"
		}
		fmt.Fprintf(a.out(), "  %-46s %d  %s\n", target, l.Targets[target], mark)
	}
	if l.Refused > 0 {
		fmt.Fprintf(a.out(), "  %d request(s) refused by the bridge (plaintext, or not port 443)\n", l.Refused)
	}
	if l.Failed > 0 {
		fmt.Fprintf(a.out(), "  %d tunnel(s) failed to open (dead exit, or a blocked one)\n", l.Failed)
	}
	fmt.Fprintln(a.out(), "  (accounts for traffic through the bridge; it cannot see traffic that bypassed it)")
}

func sortedTargets(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Purge wipes stored state, and does it in the ONE order that works: the player
// goes down first.
func (a *App) Purge() error {
	if a.Play != nil {
		if err := a.Play.Down(); err != nil {
			fmt.Fprintf(os.Stderr, "chainsmoke: player teardown failed (%v) — purging anyway\n", err)
		}
	}
	return a.Store.Purge()
}

// YtdlpHint appends the ONE recovery action that fits the failure — and
// nothing at all for a stub, where "run: yt-dlp -U" would send the user chasing
// a dependency that is not the problem.
func YtdlpHint(err error) error {
	switch {
	case err == nil, errors.Is(err, ErrNotImplemented):
		return err
	case errors.Is(err, ErrEgress):
		return err
	case errors.Is(err, ErrRateLimited):
		return fmt.Errorf("%w\n  the exit is likely blocked — run: chainsmoke rotate", err)
	case errors.Is(err, ErrImpersonate):
		return fmt.Errorf("%w\n"+
			"  install the dependency:  pip install \"yt-dlp[default,curl-cffi]\"\n"+
			"  or turn it off:          chainsmoke --impersonate '' ...", err)
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w\n"+
			"  check the spelling — handles are the tail of the profile URL\n"+
			"  chainsmoke search <artist>   # to find one you are unsure of", err)
	}
	return fmt.Errorf("%w\n  SoundCloud extractor may be stale — run: yt-dlp -U", err)
}

// RuntimeDir returns the per-user transient directory — sockets, pidfiles,
// the results cache.
func RuntimeDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		if runtime.GOOS == "windows" {
			base = os.TempDir()
		} else {
			base = fmt.Sprintf("/run/user/%d", os.Getuid())
		}
	}
	return filepath.Join(base, "chainsmoke")
}

// EnsureRuntimeDir creates RuntimeDir() with mode 0700 and returns it.
func EnsureRuntimeDir() (string, error) {
	dir := RuntimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("runtime dir %s: %w", dir, err)
	}
	return dir, nil
}

// SocketPath is where the mpv JSON-IPC endpoint lives.
func SocketPath() string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\chainsmoke-mpv`
	}
	return filepath.Join(RuntimeDir(), "mpv.sock")
}

// StateDir is where `--remember` persists. It is the OPPOSITE of RuntimeDir in
// one respect that matters: nothing here is wiped by a reboot or a logout,
// which is exactly what the user asked for by passing the flag — and exactly
// why it is not the default.
func StateDir() string {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "chainsmoke")
	}
	if runtime.GOOS == "windows" {
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, "chainsmoke", "state")
		}
		return filepath.Join(os.TempDir(), "chainsmoke", "state")
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "state", "chainsmoke")
}

// EnsureStateDir creates StateDir() with mode 0700 and returns it.
func EnsureStateDir() (string, error) {
	dir := StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("state dir %s: %w", dir, err)
	}
	return dir, nil
}

// ErrStaleExtractor means yt-dlp reached SoundCloud but could not parse what it
// got back: the extractor is behind a site change.
var ErrStaleExtractor = errors.New("yt-dlp could not extract this page — its SoundCloud extractor is likely stale")

// ErrRateLimited is SoundCloud refusing us outright (403/429), which over Tor
// usually means the exit we drew is one it blocks.
var ErrRateLimited = errors.New("SoundCloud refused the request (403/429) — the current exit is likely blocked")

// ErrEgress means yt-dlp never got out at all: the proxy in front of it did not
// carry the connection.
var ErrEgress = errors.New("yt-dlp could not reach the network through the proxy")

var ErrDeadStream = errors.New("Revoked stream")

// EgressLedger is what actually crossed the bridge in this process: every
// CONNECT target, plus the two kinds of thing that did not become a tunnel.
type EgressLedger struct {
	Observed bool

	Targets map[string]int

	Refused int

	Failed int
}

// UnexpectedTargets returns the ledger entries that are not hosts chainsmoke
// has a reason to reach, sorted for stable output.
func (l EgressLedger) UnexpectedTargets() []string {
	var out []string
	for target := range l.Targets {
		host := target
		if h, _, err := net.SplitHostPort(target); err == nil {
			host = h
		}
		if !ExpectedHost(host) {
			out = append(out, target)
		}
	}
	sort.Strings(out)
	return out
}

var expectedSuffixes = []string{
	"soundcloud.com",
	"sndcdn.com",
	"soundcloud.cloud",
}

// ExpectedHost reports whether host is one chainsmoke legitimately talks to.
func ExpectedHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, suffix := range expectedSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// ErrImpersonate means yt-dlp refused the --impersonate target we gave it, so
// nothing ever left the machine.
var ErrImpersonate = errors.New("yt-dlp rejected --impersonate")

// ErrNotFound is SoundCloud answering 404: the user, track or playlist does not
// exist.
var ErrNotFound = errors.New("SoundCloud has no such user, track, or playlist (404)")

// Package core holds the shared data model, the subsystem interfaces, the wired
// App, and the few cross-cutting helpers every other package needs. It imports
// nothing from the rest of the project — that acyclicity is what lets pkg/net,
// pkg/resolver, pkg/player, pkg/scrub and pkg/store all depend on it without
// depending on each other.
//
// cmd/chainsmoke is then only CLI surface + wiring: it constructs the concrete
// impls, drops them into App, and maps verbs onto App methods. The logic lives
// here so it can be tested against fake subsystems (TASKS.md T9.1) with no CLI
// and no network in the loop.
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

// ErrNotImplemented marks carcass stubs. Grep for it to find what's left to
// build. Test it with errors.Is, never by string match — the "yt-dlp -U" hint
// is suppressed for it (TASKS.md T1.7).
var ErrNotImplemented = errors.New("not implemented")

// -----------------------------------------------------------------------------
// Data model
// -----------------------------------------------------------------------------

// Track is the only shape SoundCloud metadata takes inside chainsmoke.
//
// Every field here is one something actually fills. Streams/Likes/Reposts used
// to sit alongside these and were removed 2026-08-24: nothing ever wrote them,
// so they serialized as `"Streams":0,"Likes":0,"Reposts":0` into every cache
// file and read as data the tool had collected. `--flat-playlist` does not
// return counts at all — getting them means one non-flat resolve per track,
// which is a Tor round-trip each and exactly what T3.1 made Meta flat to avoid.
// Re-add them when something populates them, not before.
type Track struct {
	ID        string
	Title     string
	Uploader  string
	Duration  int    // seconds; 0 means "SoundCloud did not say", never "zero-length"
	Permalink string // soundcloud.com/... — kept internal, never printed into argv examples
}

// -----------------------------------------------------------------------------
// Subsystem interfaces
// -----------------------------------------------------------------------------

// Net is the egress seam. Up() must establish the proxy BEFORE any Resolver or
// Player call. Swap the concrete impl (bridge <-> netns) without touching callers.
type Net interface {
	Up() error       // bring egress up and VERIFY it; error means no traffic may leave
	Down() error     // tear down anything we own
	Rotate() error   // NEWNYM: new Tor circuit (rate-limited by Tor)
	SelfTest() error // probe the live path (audit calls this, T7.1)
	ProxyURL() string

	// Assert is the fail-closed gate (T0.2). Every method that causes a network
	// call must call it first and refuse on error. Having it on the interface is
	// what makes the invariant enforceable rather than documented — a fake Net in
	// a unit test has to answer the same question a real one does.
	Assert() error

	// Verified reports whether the path was proven private, as opposed to merely
	// permitted. Assert can pass while this is false (--allow-direct).
	Verified() bool
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

// Player owns the mpv daemon and speaks JSON-IPC over the unix socket
// (named pipe on Windows — TASKS.md T10.7).
type Player interface {
	Ensure() error                         // start mpv --idle if not already up
	Enqueue(streamURL, title string) error // loadfile ... append-play; title keeps the signed URL out of `np`
	Command(args ...any) (any, error)      // raw IPC: get/set/cycle/seek/playlist-*
	NowPlaying() (string, error)
	Down() error
}

// Scrubber cleans a downloaded file (see script.sh scrub()).
type Scrubber interface {
	Scrub(in, out string, nuclear bool) error
}

// Store is the single persistence interface. Memory backend = ephemeral default.
type Store interface {
	SaveResults([]Track) error // cache the last numbered result set
	Result(n int) (Track, error)
	Results() ([]Track, error) // the whole cached set, in order
	Purge() error
}

// -----------------------------------------------------------------------------
// App: the wired runtime. Verbs call into this, never into globals.
// -----------------------------------------------------------------------------

// App fields are exported so cmd/chainsmoke can wire concrete impls in and
// tests can wire fakes in. Nothing here reads os.Args or the cli.Context —
// flags are resolved at the edge and handed down (TASKS.md T1.2).
type App struct {
	Net     Net
	Res     Resolver
	Play    Player
	Scrub   Scrubber
	Store   Store
	Version string

	// Egress is the effective egress configuration for THIS run, resolved at
	// the CLI edge and handed down (T1.2). Audit compares it against the saved
	// config; nothing else reads it.
	Egress Config

	// Stdin is where URLs arrive (invariant #3). Overridable for tests.
	Stdin io.Reader
	// Out is normal program output. Diagnostics go to stderr at the CLI edge,
	// so that `chainsmoke search x | head` stays clean.
	Out io.Writer
}

// stdin returns the configured reader, defaulting to os.Stdin.
func (a *App) stdin() io.Reader {
	if a.Stdin == nil {
		return os.Stdin
	}
	return a.Stdin
}

// out returns the configured writer, defaulting to os.Stdout.
func (a *App) out() io.Writer {
	if a.Out == nil {
		return os.Stdout
	}
	return a.Out
}

// ResolveIndexOrStdin implements the no-URL-in-argv contract:
//   - if arg is an integer N, look it up in the cached result set
//   - if arg is anything else, refuse it (argv is world-readable) — unless
//     typed is set: then arg is a line typed at the play session's prompt,
//     which never reaches argv or the shell's history, and is taken as a URL
//     exactly like a line read from stdin (2026-10-05 review, R4)
//   - if arg is empty, read exactly one URL line from stdin
func (a *App) ResolveIndexOrStdin(arg string, typed bool) (Track, error) {
	if arg != "" {
		// strconv.Atoi rather than a homemade loop: unlike the old accumulator
		// it errors on overflow instead of silently wrapping to a negative
		// index (TASKS.md T1.5). Separate the two failure modes — a 20-digit
		// number is a bad index, not a URL, and saying "URLs go on stdin"
		// there would be actively misleading.
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
		// A non-integer argv is almost always a URL. Refuse it rather than
		// silently falling through to a stdin read: argv is visible to other
		// processes (Linux /proc, Windows Win32_Process), so URLs ride on
		// stdin only (invariant #3 / TASKS.md T1.3).
		// Note the ordering: the prompt is listed first because it is the only
		// route that leaks nothing. Piping keeps the URL out of argv but still
		// writes it to the shell's history file, so it is the fallback for
		// scripts, not the recommendation for humans.
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

// readLine takes exactly one line from stdin, prompting first when there is a
// human to prompt.
//
// On an interactive terminal a bare ReadString looks like a hung CLI, so it
// asks. The prompt is also the most private route in: what is typed at it lands
// in neither argv (visible via /proc) nor the shell's history file, where a
// pipe achieves only the first.
//
// The prompt goes to stderr so `chainsmoke play | ...` keeps a clean stdout.
func (a *App) readLine(prompt string) (string, error) {
	// A Stdin that is itself a prompt being typed into (the play session's,
	// cmd/chainsmoke/session.go) still wants the question asked.
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
// resolver is able to observe (T2.5). Optional because it is a property of the
// bridge implementation, not of the egress contract — a netns-based Net would
// have nothing to say here.
type DNSAuditor interface {
	AuditLocalDNS() (visible bool, lines []string)
}

// EgressAuditor is optionally implemented by a Net that can account for what
// crossed it (T7.2). Optional for the same reason: a Net that is someone else's
// proxy cannot see its own traffic, and must say so rather than report zero.
type EgressAuditor interface {
	EgressLedger() EgressLedger
}

// StoreDescriber is optionally implemented by a Store that can say where it
// keeps things and how much is there (T7.1).
type StoreDescriber interface {
	Describe() (mode string, results int)
}

// Audit reports the runtime privacy posture. Everything about the live path is
// PROBED, never read back from config (T7.1) — that distinction is the whole
// point of the verb, and the saved config below does not weaken it: the config
// is an EXPECTATION the probe is compared against, never a substitute for it.
//
// Three questions, in the order a user actually asks them:
//
//  1. Am I anonymized right now?          (probed posture, DNS, ledger)
//  2. Am I anonymized the way I chose?    (drift against the saved config)
//  3. What does this machine still hold?  (store mode, cached results)
func (a *App) Audit() error {
	live := a.Egress

	// Three states, not two. Collapsing the last two into "DIRECT" was wrong in
	// the direction that matters: with egress DOWN nothing is permitted to
	// leave at all (Assert fails), and telling a user their traffic is going
	// out unanonymized when it is in fact being refused sends them chasing a
	// leak that does not exist.
	//
	// And a fourth: --proxy. A live CONNECT through it is verified, but only
	// that something answered — whatever the user pointed it at, Tor or not.
	// It used to print TOR ✓ for a proxy that dialled nothing at all
	// (CH-DEBATE 2026-10-19, D-1). The built-in path earns TOR because Up()
	// checked the SOCKS listener identifies as tor.
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

// shownProxy is ProxyURL fit for a terminal: `--proxy http://user:pass@host`
// is valid, and audit used to print the password. Prose values ("<down>",
// "DIRECT (no proxy)") pass through untouched.
func shownProxy(p string) string {
	if u, err := url.Parse(p); err == nil && u.User != nil {
		return u.Redacted()
	}
	return p
}

// auditConfig answers "am I anonymized the way I chose to be?" — the question a
// probe alone cannot answer, because a run that quietly fell back to
// --allow-direct probes as a perfectly healthy DIRECT connection.
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

	// The sharpest case: the settings can match while the OUTCOME does not,
	// because a saved TOR config still runs DIRECT when Tor is down and
	// --allow-direct is in play. Comparing fields alone would call that a match.
	if saved.SavedPosture == "TOR" && !a.Net.Verified() {
		fmt.Fprintln(a.out(), "  ⚠ you saved this as an ANONYMIZED setup and this run is not anonymized")
	}
}

// auditEgressLedger prints what actually crossed the bridge (T7.2).
func (a *App) auditEgressLedger() {
	auditor, ok := a.Net.(EgressAuditor)
	if !ok {
		return
	}
	l := auditor.EgressLedger()

	fmt.Fprintln(a.out(), "\negress ledger (this process):")
	if !l.Observed {
		// Saying "0 connections" here would be a lie of omission: an external
		// proxy carries traffic we cannot see.
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
	// Scope, stated every time rather than buried in a doc: this accounts for
	// what went THROUGH the bridge, not for what may have gone around it.
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
// goes down first (TASKS.md T4.4, bash lesson T1.13).
//
// The bash prototype's cmd_purge was `rm -rf "$RUNDIR"` with no teardown, and
// the mpv IPC socket lives in that very directory. Deleting it out from under a
// running mpv leaves a daemon nobody can talk to — still playing, unreachable,
// and invisible to the next invocation, which finds no socket and starts a
// second one. On Windows the same ordering is not a nicety but a requirement:
// an open file cannot be removed at all.
//
// Player teardown failing is reported, not fatal: a purge that stops halfway
// because mpv was already gone would leave exactly the state the user asked to
// destroy.
func (a *App) Purge() error {
	if a.Play != nil {
		if err := a.Play.Down(); err != nil {
			fmt.Fprintf(os.Stderr, "chainsmoke: player teardown failed (%v) — purging anyway\n", err)
		}
	}
	return a.Store.Purge()
}

// YtdlpHint appends the ONE recovery action that fits the failure — and nothing
// at all for a stub, where "run: yt-dlp -U" would send the user chasing a
// dependency that is not the problem (TASKS.md T1.7).
//
// The T3.5 taxonomy below is what makes the advice honest: before it, a blocked Tor
// exit and a dead bridge were both reported as a stale extractor, so the hint
// pointed at yt-dlp for two failures yt-dlp cannot fix (T3.5). Anything
// unclassified still gets the -U hint — a resolve that failed for a reason we
// do not recognise is most often the extractor.
func YtdlpHint(err error) error {
	switch {
	case err == nil, errors.Is(err, ErrNotImplemented):
		return err
	case errors.Is(err, ErrEgress):
		return err // pkg/net's problem; the error already says so
	case errors.Is(err, ErrRateLimited):
		return fmt.Errorf("%w\n  the exit is likely blocked — run: chainsmoke rotate", err)
	case errors.Is(err, ErrImpersonate):
		// Nothing left the machine, so neither SoundCloud nor the extractor is
		// involved. Both fixes are offered because the cause is one of two
		// things and the message from yt-dlp does not always distinguish them.
		return fmt.Errorf("%w\n"+
			"  install the dependency:  pip install \"yt-dlp[default,curl-cffi]\"\n"+
			"  or turn it off:          chainsmoke --impersonate '' ...", err)
	case errors.Is(err, ErrNotFound):
		// SoundCloud answered, and the answer was "that does not exist". The
		// extractor parsed the reply perfectly well; updating it changes
		// nothing. Nearly always a mistyped handle.
		return fmt.Errorf("%w\n"+
			"  check the spelling — handles are the tail of the profile URL\n"+
			"  chainsmoke search <artist>   # to find one you are unsure of", err)
	}
	return fmt.Errorf("%w\n  SoundCloud extractor may be stale — run: yt-dlp -U", err)
}

// -----------------------------------------------------------------------------
// Runtime paths (TASKS.md T1.4)
// -----------------------------------------------------------------------------

// RuntimeDir returns the per-user transient directory — sockets, pidfiles, the
// results cache. It is PURE: it computes a path and never touches disk, so that
// constructing the app (or running `--version`) creates nothing.
// Call EnsureRuntimeDir when the directory actually has to exist.
//
// Precedence mirrors the bash prototype (script.sh:33):
//   - $XDG_RUNTIME_DIR/chainsmoke when set
//   - /run/user/$UID/chainsmoke on unix — NOT "/chainsmoke", which is what bare
//     os.Getenv concatenation produced when the var was unset. WSL ships with
//     XDG_RUNTIME_DIR unset, so this fallback is the common path, not an edge case.
//   - %TEMP%\chainsmoke on Windows, which has no XDG runtime dir at all
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

// EnsureRuntimeDir creates RuntimeDir() with mode 0700 and returns it. The mode
// is load-bearing: the results cache and the mpv IPC socket both live here, and
// a world-readable socket lets any local user drive playback and read what has
// been queued.
func EnsureRuntimeDir() (string, error) {
	dir := RuntimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("runtime dir %s: %w", dir, err)
	}
	return dir, nil
}

// SocketPath is where the Phase-5 mpv JSON-IPC endpoint lives. Same protocol on
// both targets, different dial: unix socket vs named pipe (TASKS.md T10.7).
func SocketPath() string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\chainsmoke-mpv`
	}
	return filepath.Join(RuntimeDir(), "mpv.sock")
}

// StateDir is where `--remember` persists (TASKS.md T4.2). It is the OPPOSITE
// of RuntimeDir in one respect that matters: nothing here is wiped by a reboot
// or a logout, which is exactly what the user asked for by passing the flag —
// and exactly why it is not the default.
//
// Precedence mirrors RuntimeDir:
//   - $XDG_STATE_HOME/chainsmoke when set (INSTRUCTIONS.md:76 — state is for
//     history, not cache and not config)
//   - %LOCALAPPDATA%\chainsmoke\state on Windows, which has no XDG state dir.
//     Roaming is deliberately avoided: a listening history must not follow the
//     user onto every machine their profile syncs to.
//   - $HOME/.local/state/chainsmoke otherwise, per the XDG spec's default
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

// EnsureStateDir creates StateDir() with mode 0700 and returns it. Same 0700
// reasoning as EnsureRuntimeDir, with more at stake: this one persists.
func EnsureStateDir() (string, error) {
	dir := StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("state dir %s: %w", dir, err)
	}
	return dir, nil
}

// -----------------------------------------------------------------------------
// Resolver error taxonomy (TASKS.md T3.5)
// -----------------------------------------------------------------------------
//
// These live in core rather than in pkg/resolver because the CLI edge has to
// react to the CAUSE of a failed resolve without importing the resolver, and
// because each one maps to a DIFFERENT recovery action. Getting that mapping
// wrong is exactly the failure T3.5 names: sending a user to `yt-dlp -U` when
// the real problem is a dead bridge. Test them with errors.Is, never by string
// match — the wrapped text comes from yt-dlp and changes between releases.

// ErrStaleExtractor means yt-dlp reached SoundCloud but could not parse what it
// got back: the extractor is behind a site change. The only recovery action is
// `yt-dlp -U` — never a patch in pkg/resolver (see that package's doc comment).
var ErrStaleExtractor = errors.New("yt-dlp could not extract this page — its SoundCloud extractor is likely stale")

// ErrRateLimited is SoundCloud refusing us outright (403/429), which over Tor
// usually means the exit we drew is one it blocks. Recovery is a new circuit,
// not an update: `chainsmoke rotate`, then retry.
var ErrRateLimited = errors.New("SoundCloud refused the request (403/429) — the current exit is likely blocked")

// ErrEgress means yt-dlp never got out at all: the proxy in front of it did not
// carry the connection. This is a pkg/net fault surfaced through the resolver,
// and must NOT be reported as a SoundCloud or yt-dlp problem.
var ErrEgress = errors.New("yt-dlp could not reach the network through the proxy")

var ErrDeadStream = errors.New("Revoked stream")

// -----------------------------------------------------------------------------
// Egress accounting (TASKS.md T7.2)
// -----------------------------------------------------------------------------

// EgressLedger is what actually crossed the bridge in this process: every
// CONNECT target, plus the two kinds of thing that did not become a tunnel.
//
// Read the scope honestly. This accounts for traffic that WENT THROUGH the
// bridge; it cannot see traffic that went around it. A ledger showing only
// SoundCloud hosts is evidence, not proof, and the difference is the open half
// of T7.2 — closing it needs the OS to answer, not us (a netns on Linux, a
// packet capture in CI).
type EgressLedger struct {
	// Observed is false when this process does not own a bridge — a user
	// --proxy, or DIRECT mode. An empty ledger then means "we could not look",
	// which must never be displayed as "nothing left the machine".
	Observed bool

	// Targets counts CONNECTs per "host:port".
	Targets map[string]int

	// Refused counts requests the bridge rejected: a non-CONNECT (a child
	// process that tried to push PLAINTEXT through us — the loud failure the
	// bridge prefers over quietly proxying it), or a CONNECT to a port other
	// than 443 (something using the bridge as a relay).
	Refused int

	// Failed counts CONNECTs that never became a tunnel (a dead exit, a
	// refused SOCKS request). Not a leak; useful for telling "SoundCloud is
	// blocking this circuit" apart from "nothing was attempted".
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

// expectedSuffixes is the set of hosts a correct chainsmoke run reaches.
//
// soundcloud.cloud is not decoration and not a typo: the December 2025 HLS/AAC
// migration moved audio to playback.media-streaming.soundcloud.cloud, and a
// live resolve on 2026-08-19 returned exactly that host. TASKS.md T7.2 was
// written as "only *.soundcloud/*.sndcdn", which would flag the tool's own
// working audio stream as foreign traffic.
var expectedSuffixes = []string{
	"soundcloud.com",
	"sndcdn.com",
	"soundcloud.cloud",
}

// ExpectedHost reports whether host is one chainsmoke legitimately talks to.
// Suffix matching is deliberately anchored on a leading dot so that a host like
// "evilsoundcloud.com" cannot pass by ending in the same letters.
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
// nothing ever left the machine. Found live 2026-08-24: without this branch the
// failure fell through to the default hint and told the user to run `yt-dlp -U`
// — an update that cannot fix a flag, for a request that never happened. The
// real fixes are the two below, and which one applies depends on whether
// curl_cffi is installed at all.
var ErrImpersonate = errors.New("yt-dlp rejected --impersonate")

// ErrNotFound is SoundCloud answering 404: the user, track or playlist does not
// exist. Found live 2026-08-24 running `chainsmoke likes` against a mistyped
// handle — it fell through to the default hint and advised `yt-dlp -U`, which
// is the one thing that cannot possibly help. The extractor parsed the reply
// correctly; the reply just said "no such page".
//
// Deliberately NOT retryable: unlike a blocked exit, a 404 is the same on every
// circuit, so retryPlan leaves it in the no-retry default.
var ErrNotFound = errors.New("SoundCloud has no such user, track, or playlist (404)")

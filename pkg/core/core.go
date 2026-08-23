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
	"os"
	"path/filepath"
	"runtime"
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

type Track struct {
	ID        string
	Title     string
	Uploader  string
	Duration  int // seconds
	Streams   int
	Likes     int
	Reposts   int
	Permalink string // soundcloud.com/... — kept internal, never printed into argv examples
}

type Playlist struct {
	ID     string
	Title  string
	Tracks []Track
	Plays  int
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
//   - if arg is anything else, refuse it (argv is world-readable)
//   - if arg is empty, read exactly one URL line from stdin
func (a *App) ResolveIndexOrStdin(arg string) (Track, error) {
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
	// On an interactive terminal, prompt instead of blocking silently — a bare
	// ReadString here looks like a hung CLI. The prompt is also the most
	// private way in: typing a URL at it puts the URL in neither argv (visible
	// via /proc) nor the shell's history file. Piping achieves the first but
	// not the second.
	//
	// The prompt goes to stderr so `chainsmoke play | ...` keeps a clean stdout.
	if a.Stdin == nil && stdinIsTerminal() {
		fmt.Fprint(os.Stderr, "URL (stays out of argv and shell history): ")
	}
	line, err := bufio.NewReader(a.stdin()).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if err != nil {
			return Track{}, errors.New("no result index given and no URL on stdin")
		}
		return Track{}, errors.New("empty URL on stdin")
	}
	return Track{Permalink: line}, nil
}

// stdinIsTerminal reports whether stdin is an interactive console rather than a
// pipe or a file. Uses the character-device bit so this needs no extra
// dependency and behaves the same on Windows and POSIX.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// preloadAhead is how many tracks are resolved and queued before this process
// exits: the one that plays, plus one waiting behind it. Not the whole
// playlist, because SoundCloud's signed URLs expire — resolving track 40 now
// would hand mpv a URL that is dead long before playback reaches it, and it
// would cost 40 Tor round-trips to do so (TASKS.md T5.4).
const preloadAhead = 2

// EnqueueAhead resolves and queues up to preloadAhead tracks, synchronously.
//
// Synchronous by decision T0.1(a): under the "mpv is the only daemon" model the
// chainsmoke process is short-lived, so a goroutine would be killed the moment
// main() returns. (The bash prototype's `&` at script.sh:237 forks a surviving
// process; a goroutine does not. The idiom was ported, its semantics were not.)
//
// Skip-not-abort (T1.14): one dead track must cost only itself. A stream that
// will not resolve, or a URL the CDN has already expired, is reported and
// stepped over — the queue is abandoned only when NOTHING could be queued.
func (a *App) EnqueueAhead(entries []Track) error {
	if len(entries) == 0 {
		return errors.New("nothing resolved")
	}

	queued := 0
	for _, t := range entries {
		if queued == preloadAhead {
			break
		}
		url, err := a.Res.Stream(t.Permalink)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chainsmoke: skipping %q: %v\n", t.Title, YtdlpHint(err))
			continue
		}
		if err := a.Play.Enqueue(url, t.Title); err != nil {
			fmt.Fprintf(os.Stderr, "chainsmoke: skipping %q: %v\n", t.Title, err)
			continue
		}
		queued++
		// Playback starts in the daemon and outlives this process, so without
		// a line here `play` would exit instantly and silently — which reads as
		// "nothing happened" rather than "it is playing".
		fmt.Fprintf(a.out(), "▶ %s\n", t.Title)
	}

	if queued == 0 {
		return fmt.Errorf("none of the %d resolved tracks could be queued", len(entries))
	}
	if len(entries) > queued {
		fmt.Fprintf(a.out(), "  (%d more in this set — run `chainsmoke next` when this pair ends)\n",
			len(entries)-queued)
	}
	return nil
}

// DNSAuditor is optionally implemented by a Net that can report what the local
// resolver is able to observe (T2.5). Optional because it is a property of the
// bridge implementation, not of the egress contract — a netns-based Net would
// have nothing to say here.
type DNSAuditor interface {
	AuditLocalDNS() (visible bool, lines []string)
}

// Audit prints the runtime privacy posture. Everything it reports is PROBED at
// runtime, never read back from config (TASKS.md T7.1) — that distinction is
// the whole point of the verb.
func (a *App) Audit() error {
	posture := "DIRECT ⚠  (not anonymized)"
	if a.Net.Verified() {
		posture = "TOR ✓  (verified by a live CONNECT)"
	}
	fmt.Fprintf(a.out(), "egress:      %s\n", posture)
	fmt.Fprintf(a.out(), "proxy:       %s\n", a.Net.ProxyURL())
	fmt.Fprintf(a.out(), "runtime dir: %s\n", RuntimeDir())
	fmt.Fprintf(a.out(), "version:     %s\n", a.Version)

	if d, ok := a.Net.(DNSAuditor); ok {
		_, lines := d.AuditLocalDNS()
		fmt.Fprintln(a.out(), "local DNS:")
		for _, ln := range lines {
			fmt.Fprintf(a.out(), "  %s\n", ln)
		}
	}
	return a.Net.SelfTest()
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

// PrintResults writes the numbered result set: index and title only. Permalinks
// stay in the store so they never reach a terminal, a scrollback buffer, or a
// shell history (invariant #3).
func (a *App) PrintResults(ts []Track) {
	// TODO(impl): tablewriter, matching the reference's PrintTable output (T8.1).
	for i, t := range ts {
		fmt.Fprintf(a.out(), "%3d  %s\n", i+1, t.Title)
	}
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

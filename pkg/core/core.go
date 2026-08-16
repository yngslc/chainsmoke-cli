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

// Player owns the mpv daemon and speaks JSON-IPC over the unix socket
// (named pipe on Windows — TASKS.md T10.7).
type Player interface {
	Ensure() error                    // start mpv --idle if not already up
	Enqueue(streamURL string) error   // loadfile ... append-play (preflighted upstream)
	Command(args ...any) (any, error) // raw IPC: get/set/cycle/seek/playlist-*
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
		return Track{}, fmt.Errorf(
			"URLs go on stdin, not argv (argv is visible in the OS process list) — pipe it instead:\n"+
				"  echo %q | chainsmoke play", arg)
	}
	// Fail fast on an interactive terminal. Without this the ReadString below
	// blocks forever with no prompt and no output, which reads as a hung or
	// crashed CLI rather than as "this verb wants stdin".
	if a.Stdin == nil && stdinIsTerminal() {
		return Track{}, errors.New(
			"no result index given, and stdin is a terminal — pass an index or pipe a URL:\n" +
				"  chainsmoke search <term>   then   chainsmoke play 1\n" +
				"  echo \"https://soundcloud.com/artist/track\" | chainsmoke play")
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

// EnqueueAhead plays entry 0, then resolves and enqueues entry 1 so exactly one
// item stays preloaded — without pre-resolving the whole queue, whose signed
// URLs would expire before playback reached them.
//
// The enqueue of N+1 is SYNCHRONOUS by decision T0.1(a): under the "mpv is the
// only daemon" model the chainsmoke process is short-lived, so a goroutine
// would be killed the moment main() returns. (The bash prototype's `&` at
// script.sh:237 forks a surviving process; a goroutine does not. The idiom was
// ported, its semantics were not.) See TASKS.md T5.4.
func (a *App) EnqueueAhead(entries []Track) error {
	if len(entries) == 0 {
		return errors.New("nothing resolved")
	}
	first, err := a.Res.Stream(entries[0].Permalink)
	if err != nil {
		return YtdlpHint(err)
	}
	if err := a.Play.Enqueue(first); err != nil { // Enqueue preflights internally
		return err
	}
	if len(entries) > 1 {
		// Best-effort: a dead entry must skip, not abort the whole play
		// (bash lesson T1.14). The error is surfaced, not swallowed silently.
		if s, err := a.Res.Stream(entries[1].Permalink); err == nil {
			if err := a.Play.Enqueue(s); err != nil {
				fmt.Fprintf(os.Stderr, "chainsmoke: preload of next track failed (skipped): %v\n", err)
			}
		} else {
			fmt.Fprintf(os.Stderr, "chainsmoke: could not resolve next track (skipped): %v\n", err)
		}
	}
	return nil
}

// Audit prints the runtime privacy posture. Everything it reports is probed at
// runtime, never read back from config (TASKS.md T7.1).
func (a *App) Audit() error {
	fmt.Fprintf(a.out(), "egress:      %s\n", a.Net.ProxyURL())
	fmt.Fprintf(a.out(), "runtime dir: %s\n", RuntimeDir())
	fmt.Fprintf(a.out(), "version:     %s\n", a.Version)
	return a.Net.SelfTest()
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

// YtdlpHint wraps resolver errors with the one recovery action that matters —
// but never for a stub, where "run: yt-dlp -U" would send the user chasing a
// dependency that is not the problem (TASKS.md T1.7).
func YtdlpHint(err error) error {
	if err == nil || errors.Is(err, ErrNotImplemented) {
		return err
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

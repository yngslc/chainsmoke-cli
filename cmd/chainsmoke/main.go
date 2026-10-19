// =============================================================================
//  cmd/chainsmoke/main.go — `chainsmoke`, a privacy-focused SoundCloud CLI
// -----------------------------------------------------------------------------
//  This file is CLI SURFACE + WIRING ONLY. All logic lives in pkg/*:
//
//    pkg/core/core.go       <- Track model, the 5 interfaces, App + its methods,
//                              runtime paths. Imports nothing else in the tree.
//    pkg/net/bridge.go      <- Tor bridge, proxy env, NEWNYM, self-test
//    pkg/resolver/ytdlp.go  <- the ONE egress chokepoint: yt-dlp wrapper
//    pkg/player/mpv.go      <- mpv; Phase 5 makes it a daemon + JSON-IPC client
//    pkg/scrub/ffmpeg.go    <- ffmpeg +bitexact -> retag -> xattr -> mat2
//    pkg/store/store.go     <- Store interface, memory + disk backends
//    cmd/chainsmoke/session.go <- play session: holds the bridge while the queue plays
//    cmd/chainsmoke/queue.go   <- list playback: play N, next/prev along the results
//    cmd/chainsmoke/tui.go  <- empty: the TUI left this project for its own
//                              (TASKS.md Phase 10, 2026-10-05)
//
//  If you find yourself adding a type or an algorithm HERE, it belongs in a
//  package. The rule that keeps this file honest: main.go may construct things
//  and map verbs onto App methods, and nothing else.
//
//  Load-bearing invariants (retrofitting any of these is a rewrite):
//    1. ONE Store interface, two backends. Every persisted byte goes through it.
//    2. ONE egress chokepoint (Resolver). The Player only ever receives already
//       resolved, already preflighted stream URLs — never a page URL.
//    3. NO URLs in argv. The common path is `chainsmoke play <N>` against a cached
//       result set; raw URLs arrive on stdin. argv is world-readable via /proc.
//    4. EPHEMERAL by default. --remember opts into the disk Store; --private forces
//       the memory Store for the whole run regardless of config.
//
//  When yt-dlp breaks (it will — client_id is scraped and rotates), the failure
//  mode is a clear "run: yt-dlp -U" message, never a stack trace. Don't pin yt-dlp.
// =============================================================================

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"chainsmoke/pkg/core"
	csnet "chainsmoke/pkg/net"
	"chainsmoke/pkg/player"
	"chainsmoke/pkg/resolver"
	"chainsmoke/pkg/scrub"
	"chainsmoke/pkg/store"
)

// BuildVersion is stamped at build time (-ldflags "-X main.BuildVersion=...").
var BuildVersion = "development"

// -----------------------------------------------------------------------------
// Wiring
// -----------------------------------------------------------------------------

// wire builds the runtime from already-parsed flags. Taking plain values rather
// than a *cli.Context keeps the constructor testable and keeps flag parsing in
// exactly one place — urfave/cli — instead of the hand-rolled argv scanning this
// replaces (TASKS.md T1.2). That scanning had real bugs: `hasFlag` matched any
// argv token, so `chainsmoke search -- --remember` silently flipped the store to
// disk, breaking ephemeral-by-default.
func wire(app *core.App, remember, private bool, proxy, impersonate string, allowDirect bool, torSocks string) {
	n := csnet.New(proxy, allowDirect, torSocks, private)
	app.Net = n
	app.Res = resolver.New(n, impersonate)
	app.Play = player.New(core.SocketPath(), n)
	app.Scrub = scrub.New()
	app.Store = store.New(remember, private)
	app.Version = BuildVersion

	// The effective egress settings, handed down rather than re-read from argv
	// deeper in (T1.2). Audit compares this against the saved config (T7.1).
	app.Egress = core.Config{
		TorSocks:    torSocks,
		Proxy:       proxy,
		AllowDirect: allowDirect,
		Impersonate: impersonate,
	}
}

// globalFlags is the app-level flag set, extracted from main() so it can be
// tested (T9.1).
func globalFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "remember", Usage: "persist history/results to disk (default: ephemeral)"},
		&cli.BoolFlag{Name: "private", Usage: "force memory-only for this run (overrides --remember)"},
		&cli.StringFlag{Name: "proxy", Usage: "override egress proxy (else the built-in Tor bridge)"},
		&cli.StringFlag{Name: "impersonate", Value: "chrome", Usage: "yt-dlp curl_cffi target ('' to disable)"},
		&cli.BoolFlag{Name: "allow-direct", Usage: "permit UNANONYMIZED egress when Tor cannot be brought up (T0.2 escape hatch)"},
		&cli.StringFlag{Name: "tor-socks", Value: "127.0.0.1:9050", Usage: "Tor SOCKS address (use 127.0.0.1:9150 for the Tor Browser bundle)"},
	}
}

// -----------------------------------------------------------------------------
// Commands
// -----------------------------------------------------------------------------

func commands(app *core.App) []*cli.Command {
	return []*cli.Command{
		{
			Name:      "search",
			Aliases:   []string{"ls"},
			Usage:     "Search tracks; prints numbered, cached results",
			ArgsUsage: "<term>...",
			Flags: []cli.Flag{
				&cli.IntFlag{Name: "limit", Value: 10, Usage: "max results to return"},
			},
			Before: banner,
			Action: func(c *cli.Context) error {
				if c.Args().Len() == 0 {
					return errors.New("usage: chainsmoke search <term>")
				}
				// T1.1: establish the chokepoint before the resolver runs, for
				// parity with play/get. Search reaches SoundCloud like any other
				// resolver call; it must not be the one verb that skips egress.
				if err := app.Net.Up(); err != nil {
					return err
				}
				term := strings.Join(c.Args().Slice(), " ")
				tracks, err := app.Res.Search(term, c.Int("limit"))
				if err != nil {
					return core.YtdlpHint(err)
				}
				if err := app.Store.SaveResults(tracks); err != nil {
					return err
				}
				app.PrintResults(tracks) // index + title only; permalinks stay cached
				return nil
			},
		},
		{
			Name:      "likes",
			Usage:     "List a user's public likes; prints numbered, cached results",
			ArgsUsage: "<user>",
			Description: "The handle rides in argv, the same exposure as `search <term>`.\n" +
				"   Give no argument to type or pipe it instead:\n" +
				"      chainsmoke likes < handle.txt",
			Flags: []cli.Flag{
				&cli.IntFlag{Name: "limit", Usage: "keep only the first N (default: the whole page)"},
			},
			Before: banner,
			Action: func(c *cli.Context) error {
				ref, err := app.HandleOrStdin(c.Args().First(), "user handle: ",
					"usage: chainsmoke likes <user>   (or pipe the handle on stdin)")
				if err != nil {
					return err
				}
				url, err := core.LikesURL(ref)
				if err != nil {
					return err
				}
				if err := app.Net.Up(); err != nil { // chokepoint before the resolver
					return err
				}
				return app.Collect(url, c.Int("limit"))
			},
		},
		{
			Name:      "playlist",
			Aliases:   []string{"set"},
			Usage:     "List a playlist's tracks; prints numbered, cached results",
			ArgsUsage: "<user>/<set>",
			Description: "A set slug is only unique within a user, so both halves are needed:\n" +
				"      chainsmoke playlist ghostinnafurcoat/late-night\n" +
				"   which is the tail of https://soundcloud.com/<user>/sets/<set>.",
			Flags: []cli.Flag{
				&cli.IntFlag{Name: "limit", Usage: "keep only the first N (default: the whole set)"},
			},
			Before: banner,
			Action: func(c *cli.Context) error {
				ref, err := app.HandleOrStdin(c.Args().First(), "<user>/<set>: ",
					"usage: chainsmoke playlist <user>/<set>   (or pipe it on stdin)")
				if err != nil {
					return err
				}
				url, err := core.SetURL(ref)
				if err != nil {
					return err
				}
				if err := app.Net.Up(); err != nil {
					return err
				}
				return app.Collect(url, c.Int("limit"))
			},
		},
		{
			Name:      "play",
			Usage:     "Play cached result N, or a URL from stdin",
			ArgsUsage: "[N]",
			Description: "With no argument, reads exactly one URL from stdin:\n" +
				"   echo \"https://soundcloud.com/artist/track\" | chainsmoke play\n" +
				"URLs are never accepted in argv — the OS process list is world-readable.",
			Before: banner,
			Action: func(c *cli.Context) error {
				// An index (or a URL refused in argv) is checked locally BEFORE
				// Up(), so a typo never costs a circuit (TASKS.md:405, as likes
				// and playlist already do). A URL from the prompt is asked for
				// only AFTER Up(): nobody should paste one to learn Tor is down.
				arg := c.Args().First()
				var t core.Track
				var err error
				if arg != "" {
					if t, err = app.ResolveIndexOrStdin(arg, c.App.Metadata[inSession] == true); err != nil {
						return err
					}
				}
				if err := app.Net.Up(); err != nil { // chokepoint before any egress
					return err
				}
				if arg == "" {
					if t, err = app.ResolveIndexOrStdin("", false); err != nil {
						return err
					}
				}
				// After the argument, so a typo does not leave a daemon behind.
				if err := app.Play.Ensure(); err != nil {
					return err
				}
				q, _ := c.App.Metadata[listKey].(*listQueue) // the session's, if typed at its prompt
				if q == nil {
					q = &listQueue{app: app}
				}
				if n, ierr := strconv.Atoi(arg); arg != "" && ierr == nil {
					// A cached result: play it as part of its list, so next and
					// prev walk the rest (queue.go).
					if err := q.start(n); err != nil {
						return err
					}
				} else {
					// A URL: a track, or a set (an album, a playlist) to walk
					// like any list (2026-10-05 review, R2).
					entries, err := app.Res.Meta(t.Permalink)
					if err != nil {
						return core.YtdlpHint(err)
					}
					if err := q.startList(entries); err != nil {
						return err
					}
				}
				if c.App.Metadata[inSession] == true {
					return nil // typed at a session prompt, which already holds the bridge
				}
				// mpv streams through the bridge in THIS process, so returning
				// now would cut it off before the first segment (session.go).
				in, interactive := app.Stdin, false
				if in == nil {
					in, interactive = os.Stdin, core.StdinIsTerminal()
				}
				sig := make(chan os.Signal, 1)
				signal.Notify(sig, os.Interrupt)
				defer signal.Stop(sig)
				playSession(app, q, in, interactive, sig, time.Second)
				return nil
			},
		},
		{
			Name:    "get",
			Aliases: []string{"dl"},
			// Hidden until download + scrub lands (TASKS.md "Deferred"): it
			// still runs, and still answers "not implemented".
			Hidden:    true,
			Usage:     "Download + scrub cached result N (or stdin URL)",
			ArgsUsage: "[N]",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "out", Value: "./track.m4a", Usage: "output file path"},
				&cli.BoolFlag{Name: "nuclear", Usage: "ffmpeg+mat2 full strip instead of selective"},
			},
			Before: banner,
			Action: func(c *cli.Context) error {
				if err := app.Net.Up(); err != nil {
					return err
				}
				t, err := app.ResolveIndexOrStdin(c.Args().First(), false)
				if err != nil {
					return err
				}
				// T1.6: no more hardcoded /tmp/in.m4a — that path does not exist
				// on the Windows dev target and collides between concurrent runs.
				// A per-run temp dir is created and removed here; the download
				// that fills it is Phase 6 (T6.1).
				dir, err := os.MkdirTemp("", "chainsmoke-")
				if err != nil {
					return fmt.Errorf("temp dir: %w", err)
				}
				defer os.RemoveAll(dir)
				_ = t // TODO(impl): resolver download into dir (T6.1)
				return app.Scrub.Scrub(filepath.Join(dir, "in.m4a"), c.String("out"), c.Bool("nuclear"))
			},
		},

		// --- transport controls: thin IPC pass-throughs -------------------------
		{
			Name: "np", Usage: "Now playing",
			Action: func(*cli.Context) error {
				s, err := app.Play.NowPlaying()
				if err != nil {
					return err
				}
				fmt.Println(s)
				return nil
			},
		},
		{
			Name: "toggle", Usage: "Toggle pause/play",
			Action: func(*cli.Context) error { _, err := app.Play.Command("cycle", "pause"); return err },
		},
		{
			Name: "seek", Usage: "Seek ±N seconds", ArgsUsage: "<±N>",
			Action: func(c *cli.Context) error {
				// Validate before handing anything to the player: mpv would
				// otherwise receive an empty or non-numeric argument and fail
				// with a message about JSON, not about the user's typo.
				raw := c.Args().First()
				if raw == "" {
					return errors.New("usage: chainsmoke seek <±N seconds>")
				}
				secs, err := strconv.Atoi(raw)
				if err != nil {
					return fmt.Errorf("seek takes whole seconds, got %q (e.g. 30 or -10)", raw)
				}
				_, err = app.Play.Command("seek", secs)
				return err
			},
		},
		{
			Name: "vol", Usage: "Set volume 0-100", ArgsUsage: "<0-100>",
			Action: func(c *cli.Context) error {
				raw := c.Args().First()
				if raw == "" {
					return errors.New("usage: chainsmoke vol <0-100>")
				}
				v, err := strconv.Atoi(raw)
				if err != nil || v < 0 || v > 100 {
					return fmt.Errorf("volume must be a whole number 0-100, got %q", raw)
				}
				_, err = app.Play.Command("set_property", "volume", v)
				return err
			},
		},
		{
			Name: "next", Usage: "Skip to the next track",
			Action: func(*cli.Context) error { _, err := app.Play.Command("playlist-next"); return err },
		},
		{
			Name: "prev", Usage: "Back to the previous track",
			Action: func(*cli.Context) error { _, err := app.Play.Command("playlist-prev"); return err },
		},
		{
			Name: "pause", Usage: "Pause the current track",
			Action: func(*cli.Context) error { _, err := app.Play.Command("set_property", "pause", true); return err },
		},
		{
			Name: "stop", Usage: "Stop playback and clear the queue (daemon stays up)",
			// mpv's own `stop` command does both halves — measured: playlist-count
			// 2 -> 0 with the daemon still answering. Killing the daemon is
			// `daemon stop`, which is a different thing to want (T5.5).
			Action: func(*cli.Context) error { _, err := app.Play.Command("stop"); return err },
		},

		// --- privacy / lifecycle -----------------------------------------------
		{
			Name: "rotate", Usage: "New Tor circuit (re-isolated SOCKS credentials at the session prompt, NEWNYM otherwise)",
			Action: func(c *cli.Context) error {
				// At a session prompt it re-isolates the session's own bridge,
				// which every stream of the session goes through (NEWNYM too, if
				// tor has a ControlPort). On its own, `chainsmoke rotate` from
				// another window has no such bridge: it used to build one with
				// Up(), re-isolate that and exit — changing nothing for what was
				// playing, yet saying the next stream had a fresh circuit
				// (2026-10-19 review, A-3). Only NEWNYM reaches past this
				// process, so on its own that is all it tries.
				inSession := c.App.Metadata[inSession] == true
				if inSession {
					if err := app.Net.Up(); err != nil { // already up: a no-op
						return err
					}
				}
				if err := app.Net.Rotate(); err != nil {
					if !inSession {
						return fmt.Errorf("NEWNYM failed (%v). Without a tor ControlPort, a playing session's "+
							"circuit can only be changed from its own prompt: type `rotate` there", err)
					}
					return err
				}
				fmt.Fprintln(os.Stderr, "chainsmoke: the next stream gets a fresh Tor circuit")
				return nil
			},
		},
		{
			Name:  "audit",
			Usage: "Show runtime privacy posture; --save records this setup as the one you want",
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "save", Usage: "save this run's egress config as your expected setup (T7.1)"},
			},
			Action: func(c *cli.Context) error {
				// audit must PROBE, not print config (T7.1). Bringing egress up
				// first is what makes the reported posture real; without it the
				// proxy field reads "<down>" even when Tor is running.
				//
				// Up()'s refusal is REPORTED here rather than obeyed, which is
				// the one place in the tool that is true. Every other verb must
				// abort on it because it is about to send traffic; audit is a
				// diagnostic that sends no SoundCloud traffic of its own (its
				// local DNS probe asks a neutral name), and a diagnostic
				// that refuses to run precisely when the thing it diagnoses is
				// broken is useless. The exit code below still carries the
				// failure, so nothing is laundered into a pass.
				upErr := app.Net.Up()
				if upErr != nil {
					// Just a heads-up here; the error itself is returned below
					// and printed once by main, rather than twice.
					fmt.Fprintln(os.Stderr,
						"chainsmoke: egress could not be brought up — reporting the state anyway:")
				}

				auditErr := app.Audit()
				if !c.Bool("save") {
					if upErr != nil {
						return upErr // the sharper of the two; already printed above
					}
					return auditErr
				}

				// Refuse to record a setup that could not carry traffic at all.
				// The point of saving is to have an expectation worth comparing
				// against; a config saved out of a total egress failure would
				// teach every future audit to call that failure "expected".
				if upErr != nil {
					fmt.Fprintln(os.Stderr,
						"chainsmoke: NOT saving — this setup could not bring egress up, so there is nothing working to record")
					return upErr
				}

				// Saved AFTER the probe, with the posture the probe proved, so
				// what lands on disk is a setup that has carried traffic —
				// never a wish. Saving an unanonymized one is allowed (someone
				// running --proxy through their own VPN wants exactly that),
				// but it is recorded and warned about, not silently blessed.
				if err := core.SaveConfig(app.Egress, app.Net.Verified()); err != nil {
					return err
				}
				fmt.Printf("\nsaved: %s\n", core.ConfigPath())
				if !app.Net.Verified() {
					fmt.Fprintln(os.Stderr,
						"chainsmoke: WARNING saved an UNANONYMIZED setup — future audits will call this one \"expected\"")
				}
				// The save succeeded; the audit may still have failed. Returning
				// it keeps the exit code a statement about the SETUP, so adding
				// --save can never turn a failing audit into a passing one.
				return auditErr
			},
		},
		{
			Name: "purge", Usage: "Wipe transient/persistent state (both stores)",
			// app.Purge, not app.Store.Purge: the player has to come down
			// before the directory its IPC socket lives in is removed (T4.4).
			Action: func(*cli.Context) error { return app.Purge() },
		},
		{
			Name: "daemon", Usage: "player daemon lifecycle",
			Subcommands: []*cli.Command{
				{Name: "start", Usage: "Start the mpv daemon if it is not running",
					Before: banner, Action: func(*cli.Context) error { return app.Play.Ensure() }},
				{Name: "stop", Usage: "Stop the mpv daemon (ends the session)",
					Before: banner, Action: func(*cli.Context) error { return app.Play.Down() }},
				{Name: "status", Usage: "Report whether the mpv daemon is reachable",
					Before: banner, Action: func(*cli.Context) error {
						s, err := app.Play.NowPlaying()
						if err != nil {
							return err
						}
						fmt.Printf("player: up  now-playing: %s\n", s)
						return nil
					}},
			},
		},
		completionCommand(),
	}
}

// -----------------------------------------------------------------------------
// Entry point
// -----------------------------------------------------------------------------

func main() {
	app := &core.App{Version: BuildVersion}

	cliApp := &cli.App{
		Name:                 "chainsmoke",
		Usage:                "A privacy-focused SoundCloud CLI over Tor: search, likes, playlists, playback",
		Version:              BuildVersion,
		EnableBashCompletion: true,
		Flags:                globalFlags(),
		// Before is the single place global flags are read. Constructing the
		// runtime here (rather than in main, from raw argv) is what lets
		// --proxy/--impersonate actually reach the subsystems — previously they
		// were declared, parsed, and then ignored (TASKS.md T1.2).
		Before: func(c *cli.Context) error {
			wire(app,
				c.Bool("remember"), c.Bool("private"),
				c.String("proxy"), c.String("impersonate"),
				c.Bool("allow-direct"), c.String("tor-socks"))
			return nil
		},
		// After runs on every exit path the CLI controls. Without it a private
		// tor spawned by Up() is ORPHANED: the process exits, Down() never runs,
		// and the daemon plus its temp DataDirectory outlive us forever. A
		// DETECTED system tor is never touched — Down() only kills what we own.
		//
		// NOT covered: signals. A running play session traps Ctrl+C and returns
		// through here (session.go); Ctrl+C, SIGTERM or a closed terminal
		// anywhere else skips this. The private tor still goes — its torrc
		// names this process as owner, so it exits ~12s after we do — and its
		// DataDirectory goes at the next private-tor spawn, which sweeps the
		// dirs of owners that are gone (T10.9; 2026-10-05 review, R10).
		After: func(*cli.Context) error {
			if app.Net != nil {
				return app.Net.Down()
			}
			return nil
		},
		Commands: commands(app),
	}

	if err := cliApp.Run(os.Args); err != nil {
		// Clean, script-friendly failure. No stack traces in the user's face.
		fmt.Fprintln(os.Stderr, "chainsmoke:", err)
		os.Exit(1)
	}
}

// banner is the Before hook on the verbs that reach SoundCloud or drive the
// player session: search, likes, playlist, play, get, and daemon start/stop/
// status. Everything else — help, --version, completion, the transport remote
// controls, audit/rotate/purge — prints no art (dev's call, 2026-09-28).
//
// A hook rather than a print in main() so that urfave decides what counts as
// help, not a hand-rolled argv scan: Command.Run returns for --help/-h and for
// the completion protocol BEFORE it calls Before (command.go, v2.27.7), so
// `search --help`, `daemon start --help` and TAB never reach it. It must sit on
// the leaf commands, never on `daemon` itself: a parent's Before runs before
// its subcommand has parsed --help.
//
// A piped stdout gets nothing, for the reason the results table becomes TSV
// there (core.PrintResults): `chainsmoke search x | cut -f2` must read data,
// not art. Any other Writer was handed to us deliberately — the tests.
func banner(c *cli.Context) error {
	w := c.App.Writer
	if w == os.Stdout && !core.StdoutIsTerminal() {
		return nil
	}
	if c.App.Metadata[inSession] == true {
		return nil // the window that opened the session already shows it
	}
	printBanner(w)
	return nil
}

// printBanner writes the logo and wordmark to w.
func printBanner(w io.Writer) {
	const outline = `==============================================================================`

	// Raw string literals: the art contains backslashes, so it must never be
	// re-quoted and must never contain a backtick (TASKS.md T10.11).
	const logo = `                      ;:***;**;
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

	const wordmark = `   ________  _____    _____   _______ __  _______  __ __ ______
  / ____/ / / /   |  /  _/ | / / ___//  |/  / __ \/ //_// ____/
 / /   / /_/ / /| |  / //  |/ /\__ \/ /|_/ / / / / ,<  / __/
/ /___/ __  / ___ |_/ // /|  /___/ / /  / / /_/ / /| |/ /___
\____/_/ /_/_/  |_/___/_/ |_//____/_/  /_/\____/_/ |_/_____/`

	fmt.Fprintln(w, outline)
	fmt.Fprintln(w, logo)
	fmt.Fprintln(w, wordmark)
	fmt.Fprintln(w, outline)
}

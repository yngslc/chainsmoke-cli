// =============================================================================
//  cmd/chainsmoke/main.go — `chainsmoke`, a privacy-focused SoundCloud CLI
// -----------------------------------------------------------------------------
//  This file is CLI SURFACE + WIRING ONLY. All logic lives in pkg/*:
//
//    pkg/core/core.go       <- Track model, the 5 interfaces, App + its methods,
//                              runtime paths. Imports nothing else in the tree.
//    pkg/net/bridge.go      <- Tor+Privoxy bridge, proxy env, NEWNYM, self-test
//    pkg/resolver/ytdlp.go  <- the ONE egress chokepoint: yt-dlp wrapper
//    pkg/player/mpv.go      <- mpv; Phase 5 makes it a daemon + JSON-IPC client
//    pkg/scrub/ffmpeg.go    <- ffmpeg +bitexact -> retag -> xattr -> mat2
//    pkg/store/store.go     <- Store interface, memory + disk backends
//    cmd/chainsmoke/tui.go  <- TUI (Phase 10, not yet wired)
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
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	n := csnet.New(proxy, allowDirect, torSocks)
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
			Action: func(c *cli.Context) error {
				if err := app.Net.Up(); err != nil { // chokepoint first
					return err
				}
				if err := app.Play.Ensure(); err != nil {
					return err
				}
				t, err := app.ResolveIndexOrStdin(c.Args().First())
				if err != nil {
					return err
				}
				entries, err := app.Res.Meta(t.Permalink)
				if err != nil {
					return core.YtdlpHint(err)
				}
				return app.EnqueueAhead(entries)
			},
		},
		{
			Name:      "get",
			Aliases:   []string{"dl"},
			Usage:     "Download + scrub cached result N (or stdin URL)",
			ArgsUsage: "[N]",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "out", Value: "./track.m4a", Usage: "output file path"},
				&cli.BoolFlag{Name: "nuclear", Usage: "ffmpeg+mat2 full strip instead of selective"},
			},
			Action: func(c *cli.Context) error {
				if err := app.Net.Up(); err != nil {
					return err
				}
				t, err := app.ResolveIndexOrStdin(c.Args().First())
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
			Name: "rotate", Usage: "New Tor circuit (NEWNYM, or re-isolated SOCKS credentials)",
			Action: func(*cli.Context) error {
				// Up() first, for the same reason audit does it: without a live
				// bridge there is nothing to re-isolate, so a tor with no
				// ControlPort made this verb fail outright even though a fresh
				// circuit was one credential away.
				if err := app.Net.Up(); err != nil {
					return err
				}
				return app.Net.Rotate()
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
				// diagnostic that sends nothing of its own, and a diagnostic
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
					Action: func(*cli.Context) error { return app.Play.Ensure() }},
				{Name: "stop", Usage: "Stop the mpv daemon (ends the session)",
					Action: func(*cli.Context) error { return app.Play.Down() }},
				{Name: "status", Usage: "Report whether the mpv daemon is reachable",
					Action: func(*cli.Context) error {
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

	// The banner still prints for every other verb — that is T10.1/T10.2's to
	// change, not this one's. It is suppressed only where it would corrupt the
	// output rather than merely clutter it: the completion protocol, whose every
	// line becomes a candidate, and `completion <shell>`, whose output is a
	// script the user redirects into a file (T8.3).
	if !bannerCorruptsOutput(os.Args) {
		printBanner()
	}

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
			&cli.BoolFlag{Name: "allow-direct", Usage: "permit UNANONYMIZED egress when Tor cannot be brought up (T0.2 escape hatch)"},
			&cli.StringFlag{Name: "tor-socks", Value: "127.0.0.1:9050", Usage: "Tor SOCKS address (use 127.0.0.1:9150 for the Tor Browser bundle)"},
		},
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
		// NOT covered: SIGINT. Ctrl+C during playback skips this entirely
		// (T10.9 territory), so a private tor can still leak that way.
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

// printBanner writes the wordmark.
//
// KNOWN GAP (T10.1/T10.2): this runs unconditionally on stdout, before argument
// parsing, so it prefixes every verb's output, pollutes pipes, and corrupts
// `--generate-bash-completion`. Phase 10 moves it behind a TTY check and
// restricts it to bare invocations and --help. Left as-is deliberately: moving
// it changes the smoke baseline and belongs with the TUI entry-point split.
func printBanner() {
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

	fmt.Println(outline)
	fmt.Println(logo)
	fmt.Println(wordmark)
	fmt.Println(outline)
}

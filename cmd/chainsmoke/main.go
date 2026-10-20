// chainsmoke is a privacy-focused SoundCloud command-line client.
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

var BuildVersion = "development"

func wire(app *core.App, remember, private bool, proxy, impersonate string, allowDirect bool, torSocks string) {
	n := csnet.New(proxy, allowDirect, torSocks, private)
	app.Net = n
	app.Res = resolver.New(n, impersonate)
	app.Play = player.New(core.SocketPath(), n)
	app.Scrub = scrub.New()
	app.Store = store.New(remember, private)
	app.Version = BuildVersion

	app.Egress = core.Config{
		TorSocks:    torSocks,
		Proxy:       proxy,
		AllowDirect: allowDirect,
		Impersonate: impersonate,
	}
}

func globalFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "remember", Usage: "persist history/results to disk (default: ephemeral)"},
		&cli.BoolFlag{Name: "private", Usage: "force memory-only for this run (overrides --remember)"},
		&cli.StringFlag{Name: "proxy", Usage: "override egress proxy (else the built-in Tor bridge)"},
		&cli.StringFlag{Name: "impersonate", Value: "chrome", Usage: "yt-dlp curl_cffi target ('' to disable)"},
		&cli.BoolFlag{Name: "allow-direct", Usage: "permit UNANONYMIZED egress when Tor cannot be brought up (escape hatch)"},
		&cli.StringFlag{Name: "tor-socks", Value: "127.0.0.1:9050", Usage: "Tor SOCKS address (use 127.0.0.1:9150 for the Tor Browser bundle)"},
	}
}

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
				app.PrintResults(tracks)
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
				if err := app.Net.Up(); err != nil {
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
				arg := c.Args().First()
				var t core.Track
				var err error
				if arg != "" {
					if t, err = app.ResolveIndexOrStdin(arg, c.App.Metadata[inSession] == true); err != nil {
						return err
					}
				}
				if err := app.Net.Up(); err != nil {
					return err
				}
				if arg == "" {
					if t, err = app.ResolveIndexOrStdin("", false); err != nil {
						return err
					}
				}
				if err := app.Play.Ensure(); err != nil {
					return err
				}
				q, _ := c.App.Metadata[listKey].(*listQueue)
				if q == nil {
					q = &listQueue{app: app}
				}
				if n, ierr := strconv.Atoi(arg); arg != "" && ierr == nil {
					if err := q.start(n); err != nil {
						return err
					}
				} else {
					entries, err := app.Res.Meta(t.Permalink)
					if err != nil {
						return core.YtdlpHint(err)
					}
					if err := q.startList(entries); err != nil {
						return err
					}
				}
				if c.App.Metadata[inSession] == true {
					return nil
				}
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
			Name:      "get",
			Aliases:   []string{"dl"},
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
				dir, err := os.MkdirTemp("", "chainsmoke-")
				if err != nil {
					return fmt.Errorf("temp dir: %w", err)
				}
				defer os.RemoveAll(dir)
				_ = t
				return app.Scrub.Scrub(filepath.Join(dir, "in.m4a"), c.String("out"), c.Bool("nuclear"))
			},
		},

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
			Action: func(*cli.Context) error { _, err := app.Play.Command("stop"); return err },
		},

		{
			Name: "rotate", Usage: "New Tor circuit (re-isolated SOCKS credentials at the session prompt, NEWNYM otherwise)",
			Action: func(c *cli.Context) error {
				inSession := c.App.Metadata[inSession] == true
				if inSession {
					if err := app.Net.Up(); err != nil {
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
				&cli.BoolFlag{Name: "save", Usage: "save this run's egress config as your expected setup"},
			},
			Action: func(c *cli.Context) error {
				upErr := app.Net.Up()
				if upErr != nil {
					fmt.Fprintln(os.Stderr,
						"chainsmoke: egress could not be brought up — reporting the state anyway:")
				}

				auditErr := app.Audit()
				if !c.Bool("save") {
					if upErr != nil {
						return upErr
					}
					return auditErr
				}

				if upErr != nil {
					fmt.Fprintln(os.Stderr,
						"chainsmoke: NOT saving — this setup could not bring egress up, so there is nothing working to record")
					return upErr
				}

				if err := core.SaveConfig(app.Egress, app.Net.Verified()); err != nil {
					return err
				}
				fmt.Printf("\nsaved: %s\n", core.ConfigPath())
				if !app.Net.Verified() {
					fmt.Fprintln(os.Stderr,
						"chainsmoke: WARNING saved an UNANONYMIZED setup — future audits will call this one \"expected\"")
				}
				return auditErr
			},
		},
		{
			Name: "purge", Usage: "Wipe transient/persistent state (both stores)",
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

func main() {
	app := &core.App{Version: BuildVersion}

	cliApp := &cli.App{
		Name:                 "chainsmoke",
		Usage:                "A privacy-focused SoundCloud CLI over Tor: search, likes, playlists, playback",
		Version:              BuildVersion,
		EnableBashCompletion: true,
		Flags:                globalFlags(),
		Before: func(c *cli.Context) error {
			wire(app,
				c.Bool("remember"), c.Bool("private"),
				c.String("proxy"), c.String("impersonate"),
				c.Bool("allow-direct"), c.String("tor-socks"))
			return nil
		},
		After: func(*cli.Context) error {
			if app.Net != nil {
				return app.Net.Down()
			}
			return nil
		},
		Commands: commands(app),
	}

	if err := cliApp.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "chainsmoke:", err)
		os.Exit(1)
	}
}

func banner(c *cli.Context) error {
	w := c.App.Writer
	if w == os.Stdout && !core.StdoutIsTerminal() {
		return nil
	}
	if c.App.Metadata[inSession] == true {
		return nil
	}
	printBanner(w)
	return nil
}

func printBanner(w io.Writer) {
	const outline = `==============================================================================`

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

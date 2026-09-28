package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"chainsmoke/pkg/core"
)

// -----------------------------------------------------------------------------
// Play session
// -----------------------------------------------------------------------------
//
// The mpv daemon outlives `play`; the Tor path it streams through does not. The
// CONNECT bridge lives in THIS process, and mpv opens each stream — and every
// later HLS segment — asynchronously, after `loadfile` has already answered
// "success". A `play` that returned at once took the bridge down before mpv had
// connected: measured 2026-09-28, `httpproxy: ... -10054` → `Failed to open` →
// daemon idle, while an identical run that kept its proxy alive played. So
// `play` holds on for as long as the queue needs the bridge, and so the window
// stays usable, it holds on as a prompt.

// inSession marks the session's own cli.App (via Metadata): a `play` typed at
// the prompt queues into the running session instead of nesting another, and
// the banner stays off a window that already showed it.
const inSession = "chainsmoke.session"

// queueStartGrace bounds how long a non-interactive session waits to see the
// daemon get busy at all — a load that fails fast is over before the first poll.
const queueStartGrace = 15 * time.Second

// playSession keeps this process — and so the bridge — alive after `play` has
// queued. On a terminal it is a prompt; otherwise (a script, or
// `echo URL | chainsmoke play`) it waits for the queue to drain. Either way,
// ending early stops playback cleanly: the bridge closes when this process
// exits, and a track cut at the next segment boundary is worse than a stop.
func playSession(app *core.App, in io.Reader, interactive bool, sig <-chan os.Signal, poll time.Duration) {
	if interactive {
		sessionPrompt(app, in, sig)
	} else if !waitForQueue(app.Play, sig, poll) {
		return // the queue ran out on its own; nothing left to stop
	}
	_, _ = app.Play.Command("stop")
	fmt.Fprintln(os.Stderr, "chainsmoke: stopped — playback ends with the session that carries it")
}

// sessionPrompt runs verbs typed at the prompt against the live App: the same
// bridge, daemon connection and result cache the queued tracks already use. It
// returns on q/quit/exit, end of input, or Ctrl+C.
func sessionPrompt(app *core.App, in io.Reader, sig <-chan os.Signal) {
	lines := make(chan string)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-done:
				return
			}
		}
	}()

	fmt.Fprintln(os.Stderr, "chainsmoke: playing — this window stays a chainsmoke prompt "+
		"(np, vol 40, next, pause, search …, play N); q stops")
	for {
		fmt.Fprint(os.Stderr, "chainsmoke> ")
		select {
		case <-sig:
			fmt.Fprintln(os.Stderr)
			return
		case line, ok := <-lines:
			if !ok {
				fmt.Fprintln(os.Stderr)
				return
			}
			args := strings.Fields(line)
			if len(args) == 0 {
				continue
			}
			if args[0] == "q" || args[0] == "quit" || args[0] == "exit" {
				return
			}
			// A fresh App per line: urfave mutates its command table while it
			// runs, and nothing guarantees an App survives a second Run.
			repl := &cli.App{
				Name:        "chainsmoke",
				Usage:       "session commands",
				HideVersion: true,
				Commands:    commands(app),
				Metadata:    map[string]any{inSession: true},
				// urfave exits the process on an ExitCoder — an unknown verb is
				// exit 3 — which would skip the stop and drop the bridge
				// mid-track. A typo must cost one line, not the session.
				ExitErrHandler: func(*cli.Context, error) {},
			}
			if err := repl.Run(append([]string{"chainsmoke"}, args...)); err != nil {
				fmt.Fprintln(os.Stderr, "chainsmoke:", err)
			}
		}
	}
}

// waitForQueue holds the bridge until mpv has nothing left to play. It reports
// whether it was interrupted rather than drained.
func waitForQueue(p core.Player, sig <-chan os.Signal, poll time.Duration) (interrupted bool) {
	busy := false
	for waited := time.Duration(0); ; waited += poll {
		v, err := p.Command("get_property", "idle-active")
		idle, ok := v.(bool)
		if err != nil || !ok {
			return false // no daemon we can read: no path worth holding open
		}
		if !idle {
			busy = true
		} else if busy || waited >= queueStartGrace {
			return false // drained — or it never started
		}
		select {
		case <-sig:
			return true
		case <-time.After(poll):
		}
	}
}

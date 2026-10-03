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

// listKey carries the session's listQueue (queue.go) to a `play N` typed at
// the prompt, so it replaces the list being walked instead of starting another.
const listKey = "chainsmoke.list"

// queueStartGrace bounds how long a non-interactive session waits to see the
// daemon get busy at all — a load that fails fast is over before the first poll.
const queueStartGrace = 15 * time.Second

// playSession keeps this process — and so the bridge — alive after `play` has
// queued. On a terminal it is a prompt; otherwise (a script, or
// `echo URL | chainsmoke play`) it waits for the queue to drain. Either way,
// ending early stops playback cleanly: the bridge closes when this process
// exits, and a track cut at the next segment boundary is worse than a stop.
func playSession(app *core.App, q *listQueue, in io.Reader, interactive bool, sig <-chan os.Signal, poll time.Duration) {
	if interactive {
		sessionPrompt(app, q, in, sig, poll)
	} else if !waitForQueue(app.Play, q, sig, poll) {
		return // the queue ran out on its own; nothing left to stop
	}
	if q.repeat == "one" || q.repeat == "all" {
		_ = q.setRepeat("off") // the daemon outlives us; it must not keep looping
	}
	_, _ = app.Play.Command("stop")
	fmt.Fprintln(os.Stderr, "chainsmoke: stopped — playback ends with the session that carries it")
}

// sessionPrompt runs verbs typed at the prompt against the live App: the same
// bridge, daemon connection and result cache the queued tracks already use. It
// returns on q/quit/exit, end of input, or Ctrl+C. Every poll it tops up the
// list's preload, which is what follows mpv when a track ends on its own.
func sessionPrompt(app *core.App, q *listQueue, in io.Reader, sig <-chan os.Signal, poll time.Duration) {
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

	fmt.Fprintln(os.Stderr, sessionHint(commands(app)))
	fmt.Fprint(os.Stderr, "chainsmoke> ")
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-sig:
			fmt.Fprintln(os.Stderr)
			return
		case <-tick.C:
			// A song ended and mpv moved on by itself: say so, on a line of its
			// own, and put the prompt back under it.
			if i, moved := q.follow(); moved {
				fmt.Fprintln(os.Stderr)
				q.announce(i)
				fmt.Fprint(os.Stderr, "chainsmoke> ")
			}
		case line, ok := <-lines:
			if !ok {
				fmt.Fprintln(os.Stderr)
				return
			}
			args := strings.Fields(line)
			switch {
			case len(args) == 0:
			case args[0] == "q" || args[0] == "quit" || args[0] == "exit":
				return
			case args[0] == "next" || args[0] == "prev":
				// Along the list (queue.go), not just mpv's own playlist, which
				// holds only the track playing and the one preloaded.
				move := q.next
				if args[0] == "prev" {
					move = q.prev
				}
				if err := move(); err != nil {
					fmt.Fprintln(os.Stderr, "chainsmoke:", err)
				}
			case args[0] == "shuffle" || args[0] == "repeat":
				if err := setMode(q, args); err != nil {
					fmt.Fprintln(os.Stderr, "chainsmoke:", err)
				} else {
					fmt.Fprintln(os.Stderr, "chainsmoke:", q.modes())
				}
			default:
				// A fresh App per line: urfave mutates its command table while
				// it runs, and nothing guarantees an App survives a second Run.
				repl := &cli.App{
					Name:        "chainsmoke",
					Usage:       "session commands",
					HideVersion: true,
					Commands:    commands(app),
					Metadata:    map[string]any{inSession: true, listKey: q},
					// urfave exits the process on an ExitCoder — an unknown verb
					// is exit 3 — which would skip the stop and drop the bridge
					// mid-track. A typo must cost one line, not the session.
					ExitErrHandler: func(*cli.Context, error) {},
				}
				if err := repl.Run(append([]string{"chainsmoke"}, args...)); err != nil {
					fmt.Fprintln(os.Stderr, "chainsmoke:", err)
				}
			}
			fmt.Fprint(os.Stderr, "chainsmoke> ")
		}
	}
}

// notAtThePrompt are the verbs the hint leaves out: daemon and purge would
// pull the player out from under the session, completion prints a shell
// script, and get is still a stub (pkg/scrub).
var notAtThePrompt = map[string]bool{"daemon": true, "purge": true, "completion": true, "get": true}

// sessionHint lists what the prompt accepts, built from the command table so a
// verb added later shows up by itself, with q last.
func sessionHint(cmds []*cli.Command) string {
	var b strings.Builder
	b.WriteString("chainsmoke: playing — this window stays a chainsmoke prompt:\n ")
	for _, c := range cmds {
		if notAtThePrompt[c.Name] {
			continue
		}
		b.WriteString(" " + c.Name)
		if c.ArgsUsage != "" {
			b.WriteString(" " + c.ArgsUsage)
		}
		b.WriteString(" ·")
	}
	b.WriteString(" shuffle · repeat one|all|off · help · q stops")
	return b.String()
}

// setMode handles the prompt's own shuffle and repeat verbs. A bare `shuffle`
// toggles it; a bare `repeat` only reports, since there are three modes.
func setMode(q *listQueue, args []string) error {
	arg := ""
	if len(args) > 1 {
		arg = args[1]
	}
	if args[0] == "repeat" {
		if arg == "" {
			return nil
		}
		return q.setRepeat(arg)
	}
	switch arg {
	case "":
		return q.setShuffle(!q.shuffle)
	case "on", "off":
		return q.setShuffle(arg == "on")
	default:
		return fmt.Errorf("usage: shuffle [on | off] (got %q)", arg)
	}
}

// waitForQueue holds the bridge until mpv has nothing left to play. It reports
// whether it was interrupted rather than drained.
func waitForQueue(p core.Player, q *listQueue, sig <-chan os.Signal, poll time.Duration) (interrupted bool) {
	busy := false
	for waited := time.Duration(0); ; waited += poll {
		v, err := p.Command("get_property", "idle-active")
		idle, ok := v.(bool)
		if err != nil || !ok {
			return false // no daemon we can read: no path worth holding open
		}
		if !idle {
			busy = true
			if i, moved := q.follow(); moved { // keep the list going, one track ahead
				q.announce(i)
			}
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

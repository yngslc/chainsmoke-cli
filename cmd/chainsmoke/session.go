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

const inSession = "chainsmoke.session"

const listKey = "chainsmoke.list"

const queueStartGrace = 15 * time.Second

func playSession(app *core.App, q *listQueue, in io.Reader, interactive bool, sig <-chan os.Signal, poll time.Duration) {
	if interactive {
		sessionPrompt(app, q, in, sig, poll)
	} else if !waitForQueue(app.Play, q, sig, poll) {
		return
	}
	if q.repeat == "one" || q.repeat == "all" {
		_ = q.setRepeat("off")
	}
	_, _ = app.Play.Command("stop")
	fmt.Fprintln(os.Stderr, "chainsmoke: stopped — playback ends with the session that carries it")
}

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

	prev := app.Stdin
	app.Stdin = &sessionInput{lines: lines}
	defer func() { app.Stdin = prev }()

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
			if i, moved := q.follow(); moved {
				fmt.Fprintln(os.Stderr)
				q.announce(i)
				fmt.Fprint(os.Stderr, "chainsmoke> ")
			}
		case p := <-q.loading:
			if err := q.landed(p); err != nil {
				fmt.Fprintln(os.Stderr)
				fmt.Fprintln(os.Stderr, "chainsmoke:", err)
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
			case notAtThePrompt[verbName(commands(app), args[0])]:
				fmt.Fprintf(os.Stderr, "chainsmoke: `%s` does not run at the prompt — q first, then run it on its own\n", args[0])
			default:
				repl := &cli.App{
					Name:           "chainsmoke",
					Usage:          "session commands",
					HideVersion:    true,
					Commands:       commands(app),
					Metadata:       map[string]any{inSession: true, listKey: q},
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

type sessionInput struct {
	lines <-chan string
	buf   []byte
}

func (s *sessionInput) Read(p []byte) (int, error) {
	if len(s.buf) == 0 {
		line, ok := <-s.lines
		if !ok {
			return 0, io.EOF
		}
		s.buf = append([]byte(line), '\n')
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (*sessionInput) Prompting() bool { return true }

var notAtThePrompt = map[string]bool{"daemon": true, "purge": true, "completion": true, "get": true}

func verbName(cmds []*cli.Command, verb string) string {
	for _, c := range cmds {
		if c.HasName(verb) {
			return c.Name
		}
	}
	return verb
}

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

func waitForQueue(p core.Player, q *listQueue, sig <-chan os.Signal, poll time.Duration) (interrupted bool) {
	busy := false
	for waited := time.Duration(0); ; waited += poll {
		v, err := p.Command("get_property", "idle-active")
		idle, ok := v.(bool)
		if err != nil || !ok {
			return false
		}
		if !idle {
			busy = true
			if i, moved := q.follow(); moved {
				q.announce(i)
			}
		} else if q.loading == nil && (busy || waited >= queueStartGrace) {
			return false
		}
		select {
		case <-sig:
			return true
		case pl := <-q.loading:
			if err := q.landed(pl); err != nil {
				fmt.Fprintln(os.Stderr, "chainsmoke:", err)
			}
		case <-time.After(poll):
		}
	}
}

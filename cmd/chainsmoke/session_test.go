package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v2"
)

// sessionPlayer records what a play session sends the daemon, and answers
// idle-active from a script (the last answer repeats).
type sessionPlayer struct {
	recordingPlayer
	sent     []string
	enqueued int
	idle     []bool
	gone     bool // no daemon: every command fails
}

func (p *sessionPlayer) Enqueue(string, string) error { p.enqueued++; return nil }
func (p *sessionPlayer) Command(args ...any) (any, error) {
	p.sent = append(p.sent, fmt.Sprintf("%v", args))
	if p.gone {
		return nil, errors.New("mpv IPC unreachable")
	}
	if len(args) == 2 && args[1] == "idle-active" && len(p.idle) > 0 {
		v := p.idle[0]
		if len(p.idle) > 1 {
			p.idle = p.idle[1:]
		}
		return v, nil
	}
	return nil, nil
}

func (p *sessionPlayer) count(cmd string) (n int) {
	for _, s := range p.sent {
		if s == cmd {
			n++
		}
	}
	return n
}

// TestPlaySessionPrompt drives the prompt the way a user would. Verbs run
// against the live App; a typo costs one line, not the session (urfave would
// otherwise os.Exit(3) on an unknown verb and kill this test binary); a `play`
// typed at the prompt queues into THIS session rather than nesting another —
// nesting would send a second stop; and nothing after `q` runs.
func TestPlaySessionPrompt(t *testing.T) {
	app, _, _, _ := wiredApp()
	// A nested session would be a non-terminal one here (the fake App's stdin is
	// a reader), and only that kind polls idle-active — so any poll means `play`
	// at the prompt nested. The script lets such a poll finish instead of hang.
	p := &sessionPlayer{idle: []bool{false, true}}
	app.Play = p

	in := strings.NewReader("vol 40\n\nbogus\nplay 1\nq\nvol 10\n")
	playSession(app, &listQueue{app: app}, in, true, make(chan os.Signal), time.Millisecond)

	if n := p.count("[get_property idle-active]"); n != 0 {
		t.Errorf("`play` at the prompt started a nested session (%d idle polls); in a "+
			"terminal that is a second prompt racing the first for stdin", n)
	}
	if p.count("[set_property volume 40]") != 1 {
		t.Errorf("`vol 40` never reached the daemon: %v", p.sent)
	}
	if p.enqueued != 1 {
		t.Errorf("`play 1` at the prompt enqueued %d tracks, want 1", p.enqueued)
	}
	// Two stops: `play 1` at the prompt replaces the queue with the list from
	// result 1 (queue.go, 2026-10-03), and q ends playback.
	if n := p.count("[stop]"); n != 2 {
		t.Errorf("sent stop %d times, want 2 (play 1 replacing the queue, then q): %v", n, p.sent)
	}
	if last := p.sent[len(p.sent)-1]; last != "[stop]" {
		t.Errorf("last command = %s, want the stop from q", last)
	}
	if p.count("[set_property volume 10]") != 0 {
		t.Error("a command after q ran — q must end the session")
	}
}

// TestPlaySessionStopsOnEOFAndCtrlC: the two other ways out of the prompt. Both
// must stop playback, because the bridge closes with this process.
func TestPlaySessionStopsOnEOFAndCtrlC(t *testing.T) {
	for name, run := range map[string]func(*sessionPlayer){
		"eof": func(p *sessionPlayer) {
			app, _, _, _ := wiredApp()
			app.Play = p
			playSession(app, &listQueue{app: app}, strings.NewReader(""), true, make(chan os.Signal), time.Millisecond)
		},
		"ctrl-c": func(p *sessionPlayer) {
			app, _, _, _ := wiredApp()
			app.Play = p
			r, w := io.Pipe() // never written: only the signal can end this
			defer w.Close()
			sig := make(chan os.Signal, 1)
			sig <- os.Interrupt
			playSession(app, &listQueue{app: app}, r, true, sig, time.Millisecond)
		},
	} {
		p := &sessionPlayer{}
		run(p)
		if p.count("[stop]") != 1 {
			t.Errorf("%s: sent %v, want exactly one stop", name, p.sent)
		}
	}
}

// TestWaitForQueue: the non-terminal session. It holds the bridge while mpv is
// busy and lets go once the queue drains — without a stop, because nothing is
// left to cut off. Interrupted, it stops; with no daemon it does not wait.
func TestWaitForQueue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		p         *sessionPlayer
		interrupt bool
		wantStop  int
	}{
		{"drains after playing", &sessionPlayer{idle: []bool{false, false, true}}, false, 0},
		{"interrupted mid-track", &sessionPlayer{idle: []bool{false}}, true, 1},
		{"no daemon to hold for", &sessionPlayer{gone: true}, false, 0},
	} {
		app, _, _, _ := wiredApp()
		app.Play = tc.p
		sig := make(chan os.Signal, 1)
		if tc.interrupt {
			sig <- os.Interrupt
		}
		done := make(chan struct{})
		go func() {
			playSession(app, &listQueue{app: app}, strings.NewReader(""), false, sig, time.Millisecond)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: session never let go of the bridge", tc.name)
		}
		if got := tc.p.count("[stop]"); got != tc.wantStop {
			t.Errorf("%s: sent stop %d times, want %d: %v", tc.name, got, tc.wantStop, tc.p.sent)
		}
	}
}

// TestBannerStaysOffInsideASession: verbs typed at the prompt carry the banner
// hook too, and the window that opened the session already showed it once.
func TestBannerStaysOffInsideASession(t *testing.T) {
	app, _, _, _ := wiredApp()
	var out bytes.Buffer
	cliApp := &cli.App{
		Name:      "chainsmoke",
		Commands:  commands(app),
		Metadata:  map[string]any{inSession: true},
		Writer:    &out,
		ErrWriter: io.Discard,
	}
	_ = cliApp.Run([]string{"chainsmoke", "search", "x"})
	if strings.Contains(out.String(), "=====") {
		t.Error("the banner printed inside a session")
	}
}

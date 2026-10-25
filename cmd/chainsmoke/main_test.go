package main

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/urfave/cli/v2"

	"chainsmoke/pkg/core"
	"chainsmoke/pkg/store"
)

type recordingNet struct {
	ups       int
	upErr     error
	downs     int
	proxy     string
	assert    error
	rotateErr error
}

func (n *recordingNet) Up() error        { n.ups++; return n.upErr }
func (n *recordingNet) Down() error      { n.downs++; return nil }
func (n *recordingNet) Rotate() error    { return n.rotateErr }
func (n *recordingNet) SelfTest() error  { return nil }
func (n *recordingNet) ProxyURL() string { return n.proxy }
func (n *recordingNet) Assert() error    { return n.assert }
func (n *recordingNet) Verified() bool   { return n.assert == nil }

type recordingResolver struct{ calls int }

func (r *recordingResolver) Meta(string) ([]core.Track, error) {
	r.calls++
	return []core.Track{{Title: "t", Permalink: "p"}}, nil
}
func (r *recordingResolver) Stream(string) (string, error) { r.calls++; return "u", nil }
func (r *recordingResolver) Search(string, int) ([]core.Track, error) {
	r.calls++
	return []core.Track{{Title: "t", Permalink: "p"}}, nil
}

type recordingPlayer struct{ ensures int }

func (p *recordingPlayer) Ensure() error                { p.ensures++; return nil }
func (p *recordingPlayer) Enqueue(string, string) error { return nil }
func (p *recordingPlayer) Command(...any) (any, error)  { return nil, nil }
func (p *recordingPlayer) NowPlaying() (string, error)  { return "", nil }
func (p *recordingPlayer) Down() error                  { return nil }

type nopStore struct{}

func (nopStore) SaveResults([]core.Track) error { return nil }
func (nopStore) Results() ([]core.Track, error) { return []core.Track{{Permalink: "p"}}, nil }
func (nopStore) Result(int) (core.Track, error) { return core.Track{Permalink: "p"}, nil }
func (nopStore) Purge() error                   { return nil }

type nopScrub struct{}

func (nopScrub) Scrub(string, string, bool) error { return nil }

func runVerb(app *core.App, args ...string) error {
	cliApp := &cli.App{
		Name:      "chainsmoke",
		Flags:     globalFlags(),
		Commands:  commands(app),
		Writer:    io.Discard,
		ErrWriter: io.Discard,
	}
	return cliApp.Run(append([]string{"chainsmoke"}, args...))
}

func wiredApp() (*core.App, *recordingNet, *recordingResolver, *recordingPlayer) {
	n := &recordingNet{}
	r := &recordingResolver{}
	p := &recordingPlayer{}
	app := &core.App{
		Net: n, Res: r, Play: p, Store: nopStore{}, Scrub: nopScrub{},
		Out:   &bytes.Buffer{},
		Stdin: strings.NewReader(""),
	}
	return app, n, r, p
}

func TestEgressVerbsGateOnUp(t *testing.T) {
	refuse := errors.New("refusing to run unanonymized")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"search", []string{"search", "kendrick"}},
		{"likes", []string{"likes", "someuser"}},
		{"playlist", []string{"playlist", "someuser/someset"}},
		{"play", []string{"play", "1"}},
		{"get", []string{"get", "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, n, r, _ := wiredApp()
			n.upErr = refuse

			err := runVerb(app, tc.args...)

			if !errors.Is(err, refuse) {
				t.Fatalf("%s = %v, want the Up() refusal to propagate", tc.name, err)
			}
			if n.ups != 1 {
				t.Errorf("Net.Up() called %d times, want exactly 1", n.ups)
			}
			if r.calls != 0 {
				t.Errorf("resolver was called %d times AFTER egress was refused — "+
					"that is an unanonymized request", r.calls)
			}
		})
	}
}

func TestNonEgressVerbsDoNotBringEgressUp(t *testing.T) {
	for _, args := range [][]string{
		{"np"}, {"toggle"}, {"vol", "50"}, {"next"}, {"prev"}, {"pause"}, {"stop"},
	} {
		app, n, _, _ := wiredApp()
		if err := runVerb(app, args...); err != nil {
			t.Errorf("%v = %v, want nil", args, err)
		}
		if n.ups != 0 {
			t.Errorf("%v brought egress up (%d times) for a local IPC command", args, n.ups)
		}
	}
}

func TestPlayRejectsBadInputBeforeStartingThePlayer(t *testing.T) {
	for name, args := range map[string][]string{
		"URL in argv":        {"play", "https://soundcloud.com/artist/track"},
		"index out of range": {"play", "99999999999999999999"},
		"no index, no stdin": {"play"},
	} {
		app, n, _, p := wiredApp()
		if err := runVerb(app, args...); err == nil {
			t.Errorf("%s: play succeeded, want an error", name)
		}
		if p.ensures != 0 {
			t.Errorf("%s: Ensure() ran %d times before the argument was rejected", name, p.ensures)
		}
		if wantUps := map[bool]int{true: 0, false: 1}[len(args) > 1]; n.ups != wantUps {
			t.Errorf("%s: Net.Up() ran %d times, want %d", name, n.ups, wantUps)
		}
	}
}

func TestCommandTableIsWellFormed(t *testing.T) {
	app, _, _, _ := wiredApp()

	seen := map[string]string{}
	var walk func(cmds []*cli.Command, path string)
	walk = func(cmds []*cli.Command, path string) {
		for _, c := range cmds {
			if c.Usage == "" {
				t.Errorf("%s%s has no Usage — it renders as a blank row in --help", path, c.Name)
			}
			if c.Action == nil && len(c.Subcommands) == 0 {
				t.Errorf("%s%s has neither an Action nor subcommands", path, c.Name)
			}
			for _, name := range append([]string{c.Name}, c.Aliases...) {
				key := path + name
				if prev, dup := seen[key]; dup {
					t.Errorf("duplicate verb %q: %q and %q — the first one wins and the "+
						"second is unreachable", key, prev, c.Name)
				}
				seen[key] = c.Name
			}
			walk(c.Subcommands, path+c.Name+" ")
		}
	}
	walk(commands(app), "")
}

func TestBannerClassification(t *testing.T) {
	want := map[string]bool{
		"search": true, "likes": true, "playlist": true, "play": true, "get": true,
		"daemon start": true, "daemon stop": true, "daemon status": true,
		"daemon": false,
		"np":     false, "toggle": false, "seek": false, "vol": false,
		"next": false, "prev": false, "pause": false, "stop": false,
		"rotate": false, "audit": false, "purge": false, "completion": false,
	}

	app, _, _, _ := wiredApp()
	hook := reflect.ValueOf(banner).Pointer()
	seen := map[string]bool{}
	var walk func(cmds []*cli.Command, path string)
	walk = func(cmds []*cli.Command, path string) {
		for _, c := range cmds {
			name := path + c.Name
			seen[name] = true
			got := c.Before != nil && reflect.ValueOf(c.Before).Pointer() == hook
			if w, listed := want[name]; !listed {
				t.Errorf("%q is unclassified: add it here, true if it reaches SoundCloud "+
					"or drives the player session, false otherwise", name)
			} else if got != w {
				t.Errorf("%q opens with the banner = %v, want %v", name, got, w)
			}
			walk(c.Subcommands, name+" ")
		}
	}
	walk(commands(app), "")
	for name := range want {
		if !seen[name] {
			t.Errorf("%q is classified here but no longer exists — drop it", name)
		}
	}
}

func TestBannerSkipsHelpAndCompletion(t *testing.T) {
	var art bytes.Buffer
	printBanner(&art)

	for _, tc := range []struct {
		args   []string
		banner bool
	}{
		{[]string{"search", "x"}, true},
		{[]string{"ls", "x"}, true},
		{[]string{"daemon", "start"}, true},

		{nil, false},
		{[]string{"--help"}, false},
		{[]string{"-h"}, false},
		{[]string{"--version"}, false},
		{[]string{"help", "search"}, false},
		{[]string{"search", "--help"}, false},
		{[]string{"search", "-h"}, false},
		{[]string{"daemon", "--help"}, false},
		{[]string{"daemon", "help"}, false},
		{[]string{"daemon", "start", "--help"}, false},
		{[]string{"search", "x", "--generate-bash-completion"}, false},
		{[]string{"vol", "50"}, false},
	} {
		app, _, _, _ := wiredApp()
		var out bytes.Buffer
		cliApp := &cli.App{
			Name:                 "chainsmoke",
			Version:              "test",
			EnableBashCompletion: true,
			Flags:                globalFlags(),
			Commands:             commands(app),
			Writer:               &out,
			ErrWriter:            io.Discard,
		}
		_ = cliApp.Run(append([]string{"chainsmoke"}, tc.args...))
		if got := strings.Contains(out.String(), art.String()); got != tc.banner {
			t.Errorf("chainsmoke %s: banner printed = %v, want %v",
				strings.Join(tc.args, " "), got, tc.banner)
		}
	}
}

func TestWireStoreSelection(t *testing.T) {
	runtimeDir, stateDir := isolateStoreDirs(t)

	for _, tc := range []struct {
		name           string
		remember, priv bool
		wantMode       string
	}{
		{"default is ephemeral", false, false, "runtime (ephemeral): " + runtimeDir},
		{"--remember goes to disk", true, false, "disk (--remember): " + stateDir},
		{"--private is memory only", false, true, "memory (--private"},
		{"--private overrides --remember", true, true, "memory (--private"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &core.App{}
			wire(app, tc.remember, tc.priv, "", "chrome", false, "127.0.0.1:9050")

			d, ok := app.Store.(core.StoreDescriber)
			if !ok {
				t.Fatalf("wired store %T cannot describe itself", app.Store)
			}
			if mode, _ := d.Describe(); !strings.Contains(mode, tc.wantMode) {
				t.Errorf("store mode = %q, want it to contain %q", mode, tc.wantMode)
			}
		})
	}
}

func TestWireHandsDownEgressFlags(t *testing.T) {
	isolateStoreDirs(t)

	app := &core.App{}
	wire(app, false, false, "http://127.0.0.1:8118", "safari", true, "127.0.0.1:9150")

	want := core.Config{
		TorSocks:    "127.0.0.1:9150",
		Proxy:       "http://127.0.0.1:8118",
		AllowDirect: true,
		Impersonate: "safari",
	}
	if app.Egress != want {
		t.Errorf("app.Egress = %+v, want %+v", app.Egress, want)
	}
	for name, got := range map[string]any{
		"Net": app.Net, "Res": app.Res, "Play": app.Play,
		"Scrub": app.Scrub, "Store": app.Store,
	} {
		if got == nil {
			t.Errorf("wire() left app.%s nil", name)
		}
	}
	if app.Version != BuildVersion {
		t.Errorf("app.Version = %q, want %q", app.Version, BuildVersion)
	}
}

func isolateStoreDirs(t *testing.T) (runtimeDir, stateDir string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", root+"/run")
	t.Setenv("XDG_STATE_HOME", root+"/state")
	return core.RuntimeDir(), core.StateDir()
}

var _ core.Store = store.New(false, true)

func TestRotateOnItsOwnBuildsNoBridge(t *testing.T) {
	app, n, _, _ := wiredApp()
	n.rotateErr = errors.New("connection refused")
	err := runVerb(app, "rotate")
	if n.ups != 0 {
		t.Errorf("rotate on its own brought egress up %d times — a bridge no stream will use", n.ups)
	}
	if err == nil || !strings.Contains(err.Error(), "type `rotate` there") {
		t.Errorf("rotate with no ControlPort = %v, want the failure, pointing at the session prompt", err)
	}

	app, n, _, _ = wiredApp()
	cliApp := &cli.App{Name: "chainsmoke", Commands: commands(app),
		Metadata: map[string]any{inSession: true}, Writer: io.Discard, ErrWriter: io.Discard}
	if err := cliApp.Run([]string{"chainsmoke", "rotate"}); err != nil || n.ups != 1 {
		t.Errorf("rotate at the prompt = %v with %d Up() calls, want nil and the session's bridge", err, n.ups)
	}
}

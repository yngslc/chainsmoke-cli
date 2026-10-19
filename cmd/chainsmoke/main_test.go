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

// -----------------------------------------------------------------------------
// Verb wiring (TASKS.md T9.1)
// -----------------------------------------------------------------------------
//
// These test the WIRING, never the entry point. That distinction is deliberate:
// T10.1/T10.2 will rewrite how a bare invocation is routed, so anything
// asserting that would have to be written twice. The command table — including
// which verbs open with the banner — wire(), and the order in which a verb
// touches its subsystems are the parts Phase 10 leaves alone.

// recordingNet counts Up() and can refuse it, which is how the fail-closed
// ordering below gets proven rather than assumed.
type recordingNet struct {
	ups    int
	upErr  error
	downs  int
	proxy  string
	assert error
}

func (n *recordingNet) Up() error        { n.ups++; return n.upErr }
func (n *recordingNet) Down() error      { n.downs++; return nil }
func (n *recordingNet) Rotate() error    { return nil }
func (n *recordingNet) SelfTest() error  { return nil }
func (n *recordingNet) ProxyURL() string { return n.proxy }
func (n *recordingNet) Assert() error    { return n.assert }
func (n *recordingNet) Verified() bool   { return n.assert == nil }

// recordingResolver records whether SoundCloud was reached for. Every method
// counts: the invariant is that NONE of them run before egress is up.
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

// runVerb drives one verb through real urfave/cli parsing against a pre-wired
// App. It deliberately sets no Before hook: wire() must not run and replace the
// fakes with real subsystems.
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
		Stdin: strings.NewReader(""), // never block on a prompt
	}
	return app, n, r, p
}

// TestEgressVerbsGateOnUp is the T0.2 invariant checked at the CLI layer, which
// is the layer where it is easiest to lose: adding a verb means remembering to
// call Net.Up() first, and nothing but a test enforces remembering. `search`
// was once exactly this bug (T1.1) — it reached the resolver without ever
// bringing the chokepoint up.
//
// With Up() refusing, the resolver must not be touched at all.
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

// TestNonEgressVerbsDoNotBringEgressUp: the transport verbs talk only to a
// local mpv socket. Calling Up() there would spawn a Tor bootstrap to change
// the volume, and on a machine with no Tor it would fail the verb outright.
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

// TestPlayRejectsBadInputBeforeStartingThePlayer: a `play` that is going to fail
// on its argument must not spawn mpv first. It used to call Ensure() before
// reading the argument, so every typo left a daemon running behind it.
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
		// An argv argument is checked before Up() too: a typo must not cost a
		// circuit (TASKS.md:405). The stdin URL is read after Up() on purpose.
		if wantUps := map[bool]int{true: 0, false: 1}[len(args) > 1]; n.ups != wantUps {
			t.Errorf("%s: Net.Up() ran %d times, want %d", name, n.ups, wantUps)
		}
	}
}

// TestCommandTableIsWellFormed catches the class of mistake that breaks every
// verb at once. A duplicate name or alias is the specific one that has bitten
// this CLI before: urfave/cli resolves the FIRST match, so a second `pause`
// silently shadows whatever it collides with.
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

// TestBannerClassification pins which commands open with the ASCII banner, and
// it covers EVERY command path: a verb added later is missing from this table
// and fails here until someone decides which side it is on. The rule (dev,
// 2026-09-28): the banner is for reaching SoundCloud or driving the player
// session — never help, completion, the remote controls, or maintenance.
func TestBannerClassification(t *testing.T) {
	want := map[string]bool{
		// reaches SoundCloud
		"search": true, "likes": true, "playlist": true, "play": true, "get": true,
		// drives the player session
		"daemon start": true, "daemon stop": true, "daemon status": true,
		// The parent stays bare: its Before runs before the subcommand parses
		// --help, so a hook here would print art above `daemon start --help`.
		"daemon": false,
		// remote controls: frequent, local, and would drown in art
		"np": false, "toggle": false, "seek": false, "vol": false,
		"next": false, "prev": false, "pause": false, "stop": false,
		// maintenance and meta
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

// TestBannerSkipsHelpAndCompletion runs real argv through urfave and checks
// what reaches the writer. The table above says which commands carry the hook;
// this proves urfave never calls it for help, --version or completion —
// `chainsmoke --help` and `chainsmoke daemon --help` were the dev's two named
// cases — and that an alias shares its command's hook.
func TestBannerSkipsHelpAndCompletion(t *testing.T) {
	var art bytes.Buffer
	printBanner(&art)

	for _, tc := range []struct {
		args   []string
		banner bool
	}{
		{[]string{"search", "x"}, true},
		{[]string{"ls", "x"}, true}, // an alias is the same *cli.Command
		{[]string{"daemon", "start"}, true},

		{nil, false}, // bare invocation shows help (until T10.1 routes it to the TUI)
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

// -----------------------------------------------------------------------------
// Store selection: the remember/private matrix (T9.1)
// -----------------------------------------------------------------------------

// TestWireStoreSelection pins invariant #4 at the point the decision is made.
// pkg/store proves each backend behaves correctly; this proves the FLAGS pick
// the backend the user asked for — and that --private wins, which is the whole
// reason it exists as a separate flag rather than an absence of --remember.
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

// TestWireHandsDownEgressFlags: the flags are read in exactly one place and
// passed down (T1.2). Before that fix --proxy and --impersonate were parsed and
// then ignored, so the values reaching the subsystems are worth asserting.
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

// isolateStoreDirs points both store directories at the test's own temp dir, so
// wiring a disk store never touches the developer's real state.
func isolateStoreDirs(t *testing.T) (runtimeDir, stateDir string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", root+"/run")
	t.Setenv("XDG_STATE_HOME", root+"/state")
	return core.RuntimeDir(), core.StateDir()
}

// keep the store import honest: wire() returns core.Store, and this asserts the
// concrete package still satisfies the describing interface the audit relies on.
var _ core.Store = store.New(false, true)

package main

import (
	"bytes"
	"errors"
	"io"
	"slices"
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
// T10.1/T10.2 will rewrite how a bare invocation is routed and where the banner
// prints, so anything asserting that would have to be written twice. The command
// table, wire(), and the order in which a verb touches its subsystems are the
// parts Phase 10 leaves alone.

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

// TestValueFlagsMatchesGlobalFlags closes a documented footgun. firstVerb() in
// completion.go must skip the VALUE of a value-taking global flag, or
// `--proxy completion` is misread as the completion verb and the banner is
// suppressed for an ordinary run. That list was maintained by hand; this makes
// the compiler-adjacent check a test instead.
func TestValueFlagsMatchesGlobalFlags(t *testing.T) {
	var want []string
	for _, f := range globalFlags() {
		// Bool flags consume nothing; everything else takes the next argument.
		if _, isBool := f.(*cli.BoolFlag); isBool {
			continue
		}
		want = append(want, f.Names()[0])
	}

	for _, name := range want {
		if !slices.Contains(valueFlags, name) {
			t.Errorf("global flag --%s takes a value but is missing from valueFlags in "+
				"completion.go; `--%s completion` would be read as the completion verb", name, name)
		}
	}
	for _, name := range valueFlags {
		if !slices.Contains(want, name) {
			t.Errorf("valueFlags lists --%s, which is not a value-taking global flag", name)
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

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chainsmoke/pkg/core"
)

// isolate points both store directories at the test's own temp dir. Setting the
// XDG vars is what keeps this test off the developer's real cache — and it
// works on Windows too, because core's path helpers check the env FIRST and only
// then fall back to %TEMP% / %LOCALAPPDATA%.
func isolate(t *testing.T) (runtime, state string) {
	t.Helper()
	root := t.TempDir()
	runtime, state = filepath.Join(root, "run"), filepath.Join(root, "state")
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	t.Setenv("XDG_STATE_HOME", state)
	return core.RuntimeDir(), core.StateDir()
}

var sample = []core.Track{
	{ID: "1", Title: "Shanghai Lullaby", Uploader: "Roshina", Duration: 112,
		Permalink: "https://soundcloud.com/ghostinnafurcoat/shanghai-lullaby"},
	{ID: "2", Title: "Pola Woman", Uploader: "Roshina", Duration: 160,
		Permalink: "https://soundcloud.com/ghostinnafurcoat/pola-woman"},
}

// TestRuntimeStoreSurvivesTheProcess is T4.1's whole reason to exist: `search`
// and `play N` are two short-lived processes under T0.1(a), so a second Store
// built from scratch — as the next invocation would — has to see the first
// one's results. An in-memory slice cannot do this, which is why it was a
// documented gap before Phase 4.
func TestRuntimeStoreSurvivesTheProcess(t *testing.T) {
	isolate(t)

	if err := New(false, false).SaveResults(sample); err != nil {
		t.Fatalf("SaveResults: %v", err)
	}

	next := New(false, false) // a fresh process would build exactly this
	got, err := next.Result(2)
	if err != nil {
		t.Fatalf("Result(2): %v", err)
	}
	if got.Permalink != sample[1].Permalink {
		t.Errorf("Result(2).Permalink = %q, want %q", got.Permalink, sample[1].Permalink)
	}
	// The permalink is the field that matters: it never reaches the terminal
	// (invariant #3), so the store is the only thing carrying it to `play N`.
	if got.Title != "Pola Woman" || got.Uploader != "Roshina" || got.Duration != 160 {
		t.Errorf("round-trip lost fields: %+v", got)
	}
}

// TestRuntimeStoreIsWhereT4_1SaysItIs: the file has to land in the runtime dir,
// 0700, or the "wiped by logout" property the design leans on is not true.
func TestRuntimeStoreLocationAndMode(t *testing.T) {
	runtimeDir, stateDir := isolate(t)

	if err := New(false, false).SaveResults(sample); err != nil {
		t.Fatalf("SaveResults: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, resultsFile)); err != nil {
		t.Fatalf("results not in the runtime dir: %v", err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("the default run touched the PERSISTENT store: %v", err)
	}
	assertPrivateMode(t, runtimeDir)
}

// TestDiskStore covers T4.2: --remember writes to the state dir and only there.
func TestDiskStore(t *testing.T) {
	runtimeDir, stateDir := isolate(t)

	if err := New(true, false).SaveResults(sample); err != nil {
		t.Fatalf("SaveResults: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, resultsFile)); err != nil {
		t.Fatalf("--remember did not persist to the state dir: %v", err)
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Errorf("--remember also wrote the runtime store: %v", err)
	}

	got, err := New(true, false).Result(1)
	if err != nil {
		t.Fatalf("Result(1) from a fresh --remember store: %v", err)
	}
	if got.Title != "Shanghai Lullaby" {
		t.Errorf("Result(1).Title = %q", got.Title)
	}
	assertPrivateMode(t, stateDir)
}

// TestPrivateWritesNothing is T4.3, and it asserts the property rather than the
// implementation: after a --private run, the filesystem must be untouched. The
// bash prototype passed a "has a guard" review and failed this one (T1.8).
func TestPrivateWritesNothing(t *testing.T) {
	runtimeDir, stateDir := isolate(t)

	s := New(false, true)
	if err := s.SaveResults(sample); err != nil {
		t.Fatalf("SaveResults: %v", err)
	}
	for _, dir := range []string{runtimeDir, stateDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("--private created %s (err=%v) — it must write nowhere", dir, err)
		}
	}

	// Within the run it still works, so `search --private` can print numbered
	// results; across processes it deliberately cannot.
	if got, err := s.Result(1); err != nil || got.Title != "Shanghai Lullaby" {
		t.Errorf("in-process Result(1) = %+v, %v; want the saved track", got, err)
	}
	if _, err := New(false, true).Result(1); err == nil {
		t.Error("a fresh --private store returned a result; nothing may survive the process")
	}
}

// TestPrivateOverridesRemember: invariant #4. Asking for memory-only while
// --remember is also set must not persist a single byte.
func TestPrivateOverridesRemember(t *testing.T) {
	runtimeDir, stateDir := isolate(t)

	if err := New(true, true).SaveResults(sample); err != nil {
		t.Fatalf("SaveResults: %v", err)
	}
	for _, dir := range []string{runtimeDir, stateDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("--private --remember wrote %s; --private must win", dir)
		}
	}
}

// TestResultsIsTheWholeSet: the play session snapshots its list with Results()
// (cmd/chainsmoke/queue.go), so it must return exactly the saved set, in
// order, from disk and from --private's memory alike.
func TestResultsIsTheWholeSet(t *testing.T) {
	want := []core.Track{{Title: "one", Permalink: "p1"}, {Title: "two", Permalink: "p2"}}
	for name, s := range map[string]*Store{"disk": {dir: t.TempDir()}, "private": {}} {
		if err := s.SaveResults(want); err != nil {
			t.Fatal(err)
		}
		got, err := s.Results()
		if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("%s: Results() = %v, %v, want %v", name, got, err, want)
		}
	}
}

// TestPurgeWipesBothStores is T4.4: purge is not "purge whichever store this
// process happens to be using". A user who ran --remember yesterday and purges
// today without the flag must still lose the persisted set.
func TestPurgeWipesBothStores(t *testing.T) {
	runtimeDir, stateDir := isolate(t)

	if err := New(false, false).SaveResults(sample); err != nil {
		t.Fatalf("runtime SaveResults: %v", err)
	}
	if err := New(true, false).SaveResults(sample); err != nil {
		t.Fatalf("disk SaveResults: %v", err)
	}

	if err := New(false, false).Purge(); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	for _, dir := range []string{runtimeDir, stateDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s survived the purge (err=%v)", dir, err)
		}
	}

	// And the tool still works afterwards: purge removes state, not the app.
	if err := New(false, false).SaveResults(sample); err != nil {
		t.Errorf("SaveResults after purge: %v", err)
	}
}

// TestPurgeOnAnEmptyMachine: purging before anything was ever stored is the
// common case for a privacy tool, and it must be silent success, not an error
// about a missing directory.
func TestPurgeOnAnEmptyMachine(t *testing.T) {
	isolate(t)
	if err := New(false, false).Purge(); err != nil {
		t.Errorf("Purge on a clean machine = %v, want nil", err)
	}
}

// TestResultBounds pins the two messages the CLI leans on — smoke.ps1 matches
// "no cached result", and a user who searched 5 things wants to know that.
func TestResultBounds(t *testing.T) {
	isolate(t)
	s := New(false, false)

	if _, err := s.Result(1); err == nil || !strings.Contains(err.Error(), "run `chainsmoke search") {
		t.Errorf("empty-store Result(1) = %v, want the search hint", err)
	}
	if err := s.SaveResults(sample); err != nil {
		t.Fatalf("SaveResults: %v", err)
	}
	for _, n := range []int{0, -1, 3} {
		if _, err := s.Result(n); err == nil || !strings.Contains(err.Error(), "have 1-2") {
			t.Errorf("Result(%d) = %v, want the valid range", n, err)
		}
	}
}

// TestCorruptCacheIsRecoverable: a half-written or hand-edited file must not
// wedge the tool. Searching again is the fix, so the error has to say so.
func TestCorruptCacheIsRecoverable(t *testing.T) {
	runtimeDir, _ := isolate(t)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, resultsFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := New(false, false).Result(1)
	if err == nil || !strings.Contains(err.Error(), "search") {
		t.Fatalf("Result(1) on a corrupt cache = %v, want a recoverable error", err)
	}
	if err := New(false, false).SaveResults(sample); err != nil {
		t.Errorf("SaveResults over a corrupt cache: %v", err)
	}
}

// TestSaveIsAtomic: a reader must never see a partial file. The observable
// proof is that no stray temp file survives a save — a leaked one would also be
// an unprotected copy of the user's permalinks.
func TestSaveLeavesNoTempFiles(t *testing.T) {
	runtimeDir, _ := isolate(t)
	s := New(false, false)
	for i := 0; i < 3; i++ {
		if err := s.SaveResults(sample); err != nil {
			t.Fatalf("SaveResults: %v", err)
		}
	}
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != resultsFile {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("runtime dir holds %v, want just %s", names, resultsFile)
	}
}

// assertPrivateMode checks the 0700 the design leans on. Windows reports a
// synthesized mode, so the assertion is unix-only rather than wrong everywhere.
func assertPrivateMode(t *testing.T, dir string) {
	t.Helper()
	if os.PathSeparator == '\\' {
		return // Windows: the directory ACL, not the mode bits, is the control
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("%s mode = %#o, want 0700 — anything wider exposes the result set", dir, perm)
	}
	f, err := os.Stat(filepath.Join(dir, resultsFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := f.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s mode = %#o, want 0600", resultsFile, perm)
	}
}

// --- T7.1: describing the store for `audit` -----------------------------------

// TestDescribeNamesTheBackendAndCount: `audit` answers "what does this machine
// still hold?", and the answer differs for each of the three modes. Getting the
// mode wrong here would tell a --private user their permalinks are on disk, or
// worse, tell a --remember user they are not.
func TestDescribeNamesTheBackendAndCount(t *testing.T) {
	runtimeDir, stateDir := isolate(t)

	cases := []struct {
		name             string
		remember, priv   bool
		wantMode, wantIn string
		wantResults      int
	}{
		{"runtime", false, false, "runtime (ephemeral)", runtimeDir, len(sample)},
		{"disk", true, false, "disk (--remember)", stateDir, len(sample)},
		// --private reads nothing, so it must report 0 even though the other two
		// stores on this machine are full — that is T4.3's isolation, restated.
		{"private", false, true, "memory (--private", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.remember, tc.priv)
			if !tc.priv {
				if err := s.SaveResults(sample); err != nil {
					t.Fatalf("SaveResults: %v", err)
				}
			}
			mode, results := describer(t, s).Describe()
			if !strings.Contains(mode, tc.wantMode) {
				t.Errorf("mode = %q, want it to contain %q", mode, tc.wantMode)
			}
			if tc.wantIn != "" && !strings.Contains(mode, tc.wantIn) {
				t.Errorf("mode = %q, want it to name the directory %q", mode, tc.wantIn)
			}
			if results != tc.wantResults {
				t.Errorf("results = %d, want %d", results, tc.wantResults)
			}
		})
	}
}

// TestDescribeOnAnEmptyMachine: never having searched is not an error, and
// `audit` must not fail on a fresh install.
func TestDescribeOnAnEmptyMachine(t *testing.T) {
	isolate(t)

	mode, results := describer(t, New(false, false)).Describe()
	if mode == "" {
		t.Error("mode is empty on a fresh machine")
	}
	if results != 0 {
		t.Errorf("results = %d, want 0", results)
	}
}

// TestDescribeSurvivesACorruptCache: a half-written results.json already
// degrades to "no results" for `play` (TestCorruptCacheIsRecoverable). Audit
// must degrade the same way instead of reporting a count it could not read.
func TestDescribeSurvivesACorruptCache(t *testing.T) {
	runtimeDir, _ := isolate(t)

	s := New(false, false)
	if err := s.SaveResults(sample); err != nil {
		t.Fatalf("SaveResults: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "results.json"), []byte("{tru"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	mode, results := describer(t, New(false, false)).Describe()
	if results != 0 {
		t.Errorf("results = %d on a corrupt cache, want 0", results)
	}
	if !strings.Contains(mode, "runtime") {
		t.Errorf("mode = %q, want the backend still named", mode)
	}
}

// describer asserts through core.StoreDescriber rather than calling the method
// on *Store directly. Audit reaches the store as an interface value and skips
// the whole section when the assertion fails, so a New() that stopped returning
// a describing store would silently shorten the report — this is where that
// gets caught.
func describer(t *testing.T, s core.Store) core.StoreDescriber {
	t.Helper()
	d, ok := s.(core.StoreDescriber)
	if !ok {
		t.Fatalf("%T does not implement core.StoreDescriber; `audit` would omit the store section", s)
	}
	return d
}

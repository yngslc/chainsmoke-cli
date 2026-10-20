package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chainsmoke/pkg/core"
)

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

func TestRuntimeStoreSurvivesTheProcess(t *testing.T) {
	isolate(t)

	if err := New(false, false).SaveResults(sample); err != nil {
		t.Fatalf("SaveResults: %v", err)
	}

	next := New(false, false)
	got, err := next.Result(2)
	if err != nil {
		t.Fatalf("Result(2): %v", err)
	}
	if got.Permalink != sample[1].Permalink {
		t.Errorf("Result(2).Permalink = %q, want %q", got.Permalink, sample[1].Permalink)
	}
	if got.Title != "Pola Woman" || got.Uploader != "Roshina" || got.Duration != 160 {
		t.Errorf("round-trip lost fields: %+v", got)
	}
}

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

	if got, err := s.Result(1); err != nil || got.Title != "Shanghai Lullaby" {
		t.Errorf("in-process Result(1) = %+v, %v; want the saved track", got, err)
	}
	if _, err := New(false, true).Result(1); err == nil {
		t.Error("a fresh --private store returned a result; nothing may survive the process")
	}
}

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

	if err := New(false, false).SaveResults(sample); err != nil {
		t.Errorf("SaveResults after purge: %v", err)
	}
}

func TestPurgeOnAnEmptyMachine(t *testing.T) {
	isolate(t)
	if err := New(false, false).Purge(); err != nil {
		t.Errorf("Purge on a clean machine = %v, want nil", err)
	}
}

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

func assertPrivateMode(t *testing.T, dir string) {
	t.Helper()
	if os.PathSeparator == '\\' {
		return
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

func describer(t *testing.T, s core.Store) core.StoreDescriber {
	t.Helper()
	d, ok := s.(core.StoreDescriber)
	if !ok {
		t.Fatalf("%T does not implement core.StoreDescriber; `audit` would omit the store section", s)
	}
	return d
}

package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolateConfig points ConfigDir at a temp dir for the duration of one test.
// XDG_CONFIG_HOME is checked first on every platform, so one Setenv covers both
// targets — no build tags, and the Windows path is exercised in CI too.
func isolateConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "chainsmoke")
}

// TestLoadConfigMissingIsNotAnError: never having run `audit --save` is the
// ordinary state, not a failure. If it came back as an error, audit would print
// a scary line at every first-time user.
func TestLoadConfigMissingIsNotAnError(t *testing.T) {
	isolateConfig(t)

	cfg, exists, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig on a fresh machine = %v, want nil", err)
	}
	if exists {
		t.Error("exists = true with no file on disk")
	}
	if cfg != (Config{}) {
		t.Errorf("cfg = %+v, want the zero value", cfg)
	}
}

// TestSaveConfigRoundTrip pins the fields that matter and the permissions. The
// file names a proxy the user relies on; 0644 would let any local process read
// which SOCKS port this machine anonymizes through.
func TestSaveConfigRoundTrip(t *testing.T) {
	dir := isolateConfig(t)

	want := Config{
		TorSocks:    "127.0.0.1:9150",
		Proxy:       "",
		AllowDirect: false,
		Impersonate: "chrome",
	}
	if err := SaveConfig(want, true); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	got, exists, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !exists {
		t.Fatal("exists = false right after a save")
	}
	if got.TorSocks != want.TorSocks || got.Proxy != want.Proxy ||
		got.AllowDirect != want.AllowDirect || got.Impersonate != want.Impersonate {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if got.SavedPosture != "TOR" {
		t.Errorf("SavedPosture = %q, want TOR when the probe verified", got.SavedPosture)
	}
	if got.SavedAt.IsZero() {
		t.Error("SavedAt is zero; audit has nothing to date the config by")
	}

	if runtime.GOOS != "windows" { // Windows has no POSIX mode bits to assert
		fi, err := os.Stat(ConfigPath())
		if err != nil {
			t.Fatalf("stat config: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("config mode = %04o, want 0600", perm)
		}
		di, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat config dir: %v", err)
		}
		if perm := di.Mode().Perm(); perm != 0o700 {
			t.Errorf("config dir mode = %04o, want 0700", perm)
		}
	}
}

// TestSaveConfigRecordsProvenPostureNotIntent is the point of passing `verified`
// in rather than reading AllowDirect. A config saved while Tor was down must not
// come back later claiming to be an anonymized setup.
func TestSaveConfigRecordsProvenPostureNotIntent(t *testing.T) {
	isolateConfig(t)

	// Everything about the intent says Tor: no --proxy, no --allow-direct.
	if err := SaveConfig(Config{TorSocks: "127.0.0.1:9050"}, false); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, _, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.SavedPosture != "DIRECT" {
		t.Errorf("SavedPosture = %q, want DIRECT — the probe did not verify", got.SavedPosture)
	}
}

// TestLoadConfigCorruptTellsTheUserWhatToDo: a half-written file must produce
// an actionable message, not a bare json syntax error.
func TestLoadConfigCorruptTellsTheUserWhatToDo(t *testing.T) {
	dir := isolateConfig(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(ConfigPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig on a corrupt file = nil, want an error")
	}
	if !strings.Contains(err.Error(), "audit --save") {
		t.Errorf("error = %q, want it to name the command that fixes it", err)
	}
}

// TestConfigDiff walks the four egress fields. The empty-string cases matter
// most: "" for Proxy means the built-in bridge and "" for Impersonate means
// disabled, and a diff that printed blanks would be unreadable.
func TestConfigDiff(t *testing.T) {
	saved := Config{TorSocks: "127.0.0.1:9050", Impersonate: "chrome"}

	if drift := saved.Diff(saved); len(drift) != 0 {
		t.Errorf("Diff against itself = %v, want none", drift)
	}

	live := Config{TorSocks: "127.0.0.1:9150", Proxy: "http://127.0.0.1:8118", AllowDirect: true}
	drift := saved.Diff(live)
	if len(drift) != 4 {
		t.Fatalf("Diff = %v, want all four fields", drift)
	}

	byField := map[string]Drift{}
	for _, d := range drift {
		byField[d.Field] = d
	}
	if d := byField["proxy"]; d.Saved != "(built-in bridge)" || d.Live != "http://127.0.0.1:8118" {
		t.Errorf("proxy drift = %+v, want the empty saved value spelled out", d)
	}
	if d := byField["impersonate"]; d.Saved != "chrome" || d.Live != "(disabled)" {
		t.Errorf("impersonate drift = %+v, want the empty live value spelled out", d)
	}
	if d := byField["allow-direct"]; d.Saved != "false" || d.Live != "true" {
		t.Errorf("allow-direct drift = %+v", d)
	}
	if d := byField["tor-socks"]; d.Saved != "127.0.0.1:9050" || d.Live != "127.0.0.1:9150" {
		t.Errorf("tor-socks drift = %+v", d)
	}
}

// TestConfigDiffIgnoresSaveMetadata: SavedAt/SavedPosture describe the saving,
// not the settings. Comparing them would report drift on every single run.
func TestConfigDiffIgnoresSaveMetadata(t *testing.T) {
	saved := Config{TorSocks: "127.0.0.1:9050", SavedPosture: "TOR"}
	live := Config{TorSocks: "127.0.0.1:9050"}

	if drift := saved.Diff(live); len(drift) != 0 {
		t.Errorf("Diff = %v, want none — only egress fields drift", drift)
	}
}

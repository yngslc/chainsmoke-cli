package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// Config is the egress setup a user has decided they want — not what is
// happening right now. Audit's job is to compare the two (TASKS.md T7.1).
//
// Why save anything at all, when the verb's whole point is to PROBE rather than
// print config: probing answers "am I anonymized?", and that is not the only
// question worth asking. "Am I anonymized the way I chose to be?" is different,
// and a probe alone cannot answer it — a run that silently fell back to
// --allow-direct, or to a --proxy someone left in a shell alias, probes as a
// perfectly healthy DIRECT connection. The saved config is what turns a healthy
// probe into a comparison, and drift into something the user is told about.
//
// Nothing here is ever trusted as a substitute for the probe. It is the
// EXPECTATION; the probe stays the reality.
type Config struct {
	TorSocks    string `json:"tor_socks"`
	Proxy       string `json:"proxy"` // "" = the built-in bridge
	AllowDirect bool   `json:"allow_direct"`
	Impersonate string `json:"impersonate"`

	// SavedAt and SavedPosture record what was PROVEN at the moment of saving,
	// so a later audit can say "this worked as TOR when you saved it, and you
	// are DIRECT now" rather than merely "a field changed".
	SavedAt      time.Time `json:"saved_at"`
	SavedPosture string    `json:"saved_posture"` // "TOR", "PROXY" or "DIRECT"
}

// configFile is the on-disk name. JSON rather than the TOML in INSTRUCTIONS.md
// line 76: TOML needs a dependency, and this file is four fields the store
// already has a JSON encoder for.
const configFile = "config.json"

// ConfigDir follows the same precedence as RuntimeDir and StateDir.
//
//   - $XDG_CONFIG_HOME/chainsmoke when set
//   - %APPDATA%\chainsmoke on Windows — config is the one kind of state that
//     SHOULD roam with a profile, unlike the listening history in StateDir
//   - $HOME/.config/chainsmoke otherwise
func ConfigDir() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "chainsmoke")
	}
	if runtime.GOOS == "windows" {
		if base := os.Getenv("APPDATA"); base != "" {
			return filepath.Join(base, "chainsmoke")
		}
		return filepath.Join(os.TempDir(), "chainsmoke", "config")
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "chainsmoke")
}

// ConfigPath is where LoadConfig reads and SaveConfig writes.
func ConfigPath() string { return filepath.Join(ConfigDir(), configFile) }

// LoadConfig reads the saved egress config. A missing file is not an error: it
// is the ordinary state of someone who has never run `audit --save`, and the
// caller distinguishes it by the bool rather than by inspecting the error.
func LoadConfig() (cfg Config, exists bool, err error) {
	blob, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("config at %s is unreadable (%w) — delete it and run `chainsmoke audit --save` again", ConfigPath(), err)
	}
	return cfg, true, nil
}

// SaveConfig writes the config 0600 inside a 0700 directory. It records what a
// probe just proved rather than what a flag merely requested, which is why the
// caller passes the posture in: saving a config that has never carried traffic
// would recreate the "printed config" problem T7.1 exists to kill.
func SaveConfig(cfg Config, verified bool) error {
	cfg.SavedAt = time.Now().UTC()
	cfg.SavedPosture = "DIRECT"
	switch {
	case verified && cfg.Proxy != "":
		cfg.SavedPosture = "PROXY" // verified, but not proven to be Tor (Audit)
	case verified:
		cfg.SavedPosture = "TOR"
	}

	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config dir %s: %w", dir, err)
	}
	blob, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(ConfigPath(), append(blob, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Drift is one field where the live run does not match what was saved.
type Drift struct {
	Field string
	Saved string
	Live  string
}

// Diff reports where the live run departs from the saved expectation. Only the
// four egress fields are compared — SavedAt and SavedPosture are metadata about
// the saving, not settings that can drift.
func (saved Config) Diff(live Config) []Drift {
	var out []Drift
	add := func(field, s, l string) {
		if s != l {
			out = append(out, Drift{Field: field, Saved: s, Live: l})
		}
	}
	add("tor-socks", saved.TorSocks, live.TorSocks)
	add("proxy", describeProxy(saved.Proxy), describeProxy(live.Proxy))
	add("allow-direct", strconv.FormatBool(saved.AllowDirect), strconv.FormatBool(live.AllowDirect))
	add("impersonate", describeImpersonate(saved.Impersonate), describeImpersonate(live.Impersonate))
	return out
}

// describeProxy renders the empty string as what it means, so a diff never
// shows a blank column the reader has to interpret.
func describeProxy(p string) string {
	if p == "" {
		return "(built-in bridge)"
	}
	return p
}

func describeImpersonate(i string) string {
	if i == "" {
		return "(disabled)"
	}
	return i
}

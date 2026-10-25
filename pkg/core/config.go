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
// happening right now.
type Config struct {
	TorSocks    string `json:"tor_socks"`
	Proxy       string `json:"proxy"`
	AllowDirect bool   `json:"allow_direct"`
	Impersonate string `json:"impersonate"`

	SavedAt      time.Time `json:"saved_at"`
	SavedPosture string    `json:"saved_posture"`
}

const configFile = "config.json"

// ConfigDir follows the same precedence as RuntimeDir and StateDir.
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

// SaveConfig writes the config 0600 inside a 0700 directory.
func SaveConfig(cfg Config, verified bool) error {
	cfg.SavedAt = time.Now().UTC()
	cfg.SavedPosture = "DIRECT"
	switch {
	case verified && cfg.Proxy != "":
		cfg.SavedPosture = "PROXY"
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

// Diff reports where the live run departs from the saved expectation.
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

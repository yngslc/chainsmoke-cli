package core

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// fakeNet is the probe half of audit. Everything audit prints about the live
// path comes through this interface, which is exactly why T7.1 says "probed,
// never printed from config" — swap the answers here and the report must change.
type fakeNet struct {
	verified  bool
	proxy     string
	selfTest  error
	assertErr error
}

func (n *fakeNet) Up() error        { return nil }
func (n *fakeNet) Down() error      { return nil }
func (n *fakeNet) Rotate() error    { return nil }
func (n *fakeNet) SelfTest() error  { return n.selfTest }
func (n *fakeNet) ProxyURL() string { return n.proxy }
func (n *fakeNet) Assert() error    { return n.assertErr }
func (n *fakeNet) Verified() bool   { return n.verified }

// auditingNet also implements EgressAuditor, the way *net.Bridge does.
type auditingNet struct {
	fakeNet
	ledger EgressLedger
}

func (n *auditingNet) EgressLedger() EgressLedger { return n.ledger }

// describingStore implements StoreDescriber; the plain fakeStore in
// core_test.go deliberately does not, so both branches are covered.
type describingStore struct {
	fakeStore
	mode    string
	results int
}

func (s *describingStore) Describe() (string, int) { return s.mode, s.results }

// runAudit runs Audit against an isolated config dir and returns what it wrote.
func runAudit(t *testing.T, app *App) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	app.Out = &buf
	if app.Store == nil {
		app.Store = &fakeStore{log: &log{}}
	}
	err := app.Audit()
	return buf.String(), err
}

// TestAuditReportsProbedPosture: the posture line comes from Verified() and
// Assert(), which only answer for what the live path actually did. A config
// file can never make this line say TOR.
//
// Three states, because DIRECT and DOWN are not the same thing and the earlier
// two-state version got the difference backwards: with egress down, nothing is
// permitted to leave, and calling that "DIRECT — not anonymized" sends the user
// hunting a leak that is not happening.
func TestAuditReportsProbedPosture(t *testing.T) {
	isolateConfig(t)

	cases := []struct {
		name string
		net  *fakeNet
		want string
		deny string
	}{
		{"verified tor", &fakeNet{verified: true, proxy: "http://127.0.0.1:1080"}, "egress:      TOR", "DIRECT"},
		{"permitted direct", &fakeNet{}, "egress:      DIRECT", "DOWN"},
		{"egress down", &fakeNet{assertErr: errors.New("fail-closed")}, "egress:      DOWN", "DIRECT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := runAudit(t, &App{Net: tc.net})
			if !strings.Contains(out, tc.want) {
				t.Errorf("posture line missing %q:\n%s", tc.want, out)
			}
			posture := out[:strings.Index(out, "\n")]
			if strings.Contains(posture, tc.deny) {
				t.Errorf("posture line %q also claims %q", posture, tc.deny)
			}
		})
	}

	// The unanonymized states have to say so in words, not just in a symbol.
	out, _ := runAudit(t, &App{Net: &fakeNet{}})
	if !strings.Contains(out, "NOT anonymized") {
		t.Errorf("DIRECT was not spelled out as unanonymized:\n%s", out)
	}
	out, _ = runAudit(t, &App{Net: &fakeNet{assertErr: errors.New("fail-closed")}})
	if !strings.Contains(out, "fail-closed") {
		t.Errorf("DOWN did not explain that nothing is permitted to leave:\n%s", out)
	}
}

// TestAuditReturnsSelfTestError: audit exits non-zero when the live path is
// broken. Printing a report and returning nil would make a failing setup look
// like a passing one to any script that checks $?.
func TestAuditReturnsSelfTestError(t *testing.T) {
	isolateConfig(t)
	boom := errors.New("egress is DIRECT")

	_, err := runAudit(t, &App{Net: &fakeNet{selfTest: boom}})
	if !errors.Is(err, boom) {
		t.Errorf("Audit() = %v, want the SelfTest error", err)
	}
}

// TestAuditWithoutSavedConfigPointsAtTheCommand: the first run must teach the
// verb that fixes it rather than silently omitting the section.
func TestAuditWithoutSavedConfigPointsAtTheCommand(t *testing.T) {
	isolateConfig(t)

	out, _ := runAudit(t, &App{Net: &fakeNet{}})
	if !strings.Contains(out, "audit --save") {
		t.Errorf("no-config report does not name the save command:\n%s", out)
	}
}

// TestAuditReportsMatchingConfig: settings equal to the saved ones is the happy
// case, and it has to be visibly stated — silence would read as "no config".
func TestAuditReportsMatchingConfig(t *testing.T) {
	isolateConfig(t)
	cfg := Config{TorSocks: "127.0.0.1:9050", Impersonate: "chrome"}
	if err := SaveConfig(cfg, true); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	out, _ := runAudit(t, &App{Net: &fakeNet{verified: true}, Egress: cfg})
	if !strings.Contains(out, "this run matches it") {
		t.Errorf("matching run not reported as a match:\n%s", out)
	}
	if strings.Contains(out, "⚠ tor-socks") || strings.Contains(out, "⚠ proxy") {
		t.Errorf("drift reported on an identical config:\n%s", out)
	}
}

// TestAuditReportsDrift is the scenario the whole feature exists for: a --proxy
// left in a shell alias. The probe is perfectly healthy; only the comparison
// notices the user is no longer running what they chose.
func TestAuditReportsDrift(t *testing.T) {
	isolateConfig(t)
	if err := SaveConfig(Config{TorSocks: "127.0.0.1:9050", Impersonate: "chrome"}, true); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	live := Config{TorSocks: "127.0.0.1:9050", Proxy: "http://127.0.0.1:8118", Impersonate: "chrome"}
	out, _ := runAudit(t, &App{Net: &fakeNet{verified: true, proxy: live.Proxy}, Egress: live})

	if !strings.Contains(out, "proxy") || !strings.Contains(out, "http://127.0.0.1:8118") {
		t.Errorf("drifted proxy not reported:\n%s", out)
	}
	if strings.Contains(out, "this run matches it") {
		t.Errorf("a drifted run was reported as a match:\n%s", out)
	}
}

// TestAuditWarnsWhenSavedTorRunsDirect is the case field comparison alone gets
// wrong. With --allow-direct saved as part of the setup, every field can match
// while the outcome is a plain unanonymized connection.
func TestAuditWarnsWhenSavedTorRunsDirect(t *testing.T) {
	isolateConfig(t)
	cfg := Config{TorSocks: "127.0.0.1:9050", AllowDirect: true}
	if err := SaveConfig(cfg, true); err != nil { // saved while Tor was up
		t.Fatalf("SaveConfig: %v", err)
	}

	// Same flags, Tor now down: fields match, posture does not.
	out, _ := runAudit(t, &App{Net: &fakeNet{verified: false}, Egress: cfg})
	if !strings.Contains(out, "this run matches it") {
		t.Fatalf("precondition: fields should still match:\n%s", out)
	}
	if !strings.Contains(out, "this run is not anonymized") {
		t.Errorf("matching fields hid a lost anonymity guarantee:\n%s", out)
	}
}

// TestAuditLedgerUnobservableIsNotZero: with someone else's proxy we cannot see
// the traffic. "not observable" and "0 connections" are opposite claims and the
// report must never substitute the second for the first.
func TestAuditLedgerUnobservableIsNotZero(t *testing.T) {
	isolateConfig(t)

	n := &auditingNet{ledger: EgressLedger{Observed: false}}
	out, _ := runAudit(t, &App{Net: n})

	if !strings.Contains(out, "not observable") {
		t.Errorf("unobserved ledger not marked as such:\n%s", out)
	}
	if strings.Contains(out, "no tunnels opened") {
		t.Errorf("unobserved ledger rendered as an empty one:\n%s", out)
	}
}

// TestAuditLedgerFlagsUnexpectedHosts: the ledger is only useful if a host we
// have no business reaching is visibly different from one we do.
func TestAuditLedgerFlagsUnexpectedHosts(t *testing.T) {
	isolateConfig(t)

	n := &auditingNet{ledger: EgressLedger{
		Observed: true,
		Targets: map[string]int{
			"api-v2.soundcloud.com:443":                     3,
			"playback.media-streaming.soundcloud.cloud:443": 1,
			"telemetry.example.com:443":                     1,
		},
		Refused: 2,
		Failed:  1,
	}}
	out, _ := runAudit(t, &App{Net: n})

	for _, want := range []string{
		"telemetry.example.com:443",
		"UNEXPECTED",
		"2 plaintext attempt(s) refused",
		"1 tunnel(s) failed",
		"cannot see traffic that bypassed it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ledger report missing %q:\n%s", want, out)
		}
	}
	// The two legitimate hosts must NOT be flagged — a report that cries wolf
	// on its own audio CDN teaches the user to ignore it.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "soundcloud") && strings.Contains(line, "UNEXPECTED") {
			t.Errorf("legitimate host flagged as unexpected: %s", line)
		}
	}
}

// TestAuditDescribesTheStore answers "what does this machine still hold?" —
// the third question audit exists to answer (T7.1).
func TestAuditDescribesTheStore(t *testing.T) {
	isolateConfig(t)

	app := &App{
		Net:   &fakeNet{},
		Store: &describingStore{mode: "memory (--private: nothing is written)", results: 0},
	}
	out, _ := runAudit(t, app)
	if !strings.Contains(out, "--private: nothing is written") {
		t.Errorf("store mode not reported:\n%s", out)
	}
	if !strings.Contains(out, "results:     0 cached") {
		t.Errorf("cached count not reported:\n%s", out)
	}
}

// TestAuditSurvivesAStoreThatCannotDescribeItself: StoreDescriber is optional,
// so a Store without it must degrade to a shorter report, not a nil panic.
func TestAuditSurvivesAStoreThatCannotDescribeItself(t *testing.T) {
	isolateConfig(t)

	out, err := runAudit(t, &App{Net: &fakeNet{}, Store: &fakeStore{log: &log{}}})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if strings.Contains(out, "store:") {
		t.Errorf("store line printed for a Store that cannot describe itself:\n%s", out)
	}
	if !strings.Contains(out, "egress:") {
		t.Errorf("report truncated entirely:\n%s", out)
	}
}

// TestExpectedHost pins the classification. The suffix match is anchored on a
// leading dot on purpose: "evilsoundcloud.com" ends in the same letters as the
// real thing, and a naive HasSuffix would wave it through.
func TestExpectedHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"soundcloud.com", true},
		{"api-v2.soundcloud.com", true},
		{"a1.sndcdn.com", true},
		{"cf-hls-media.sndcdn.com", true},
		// The December 2025 HLS/AAC migration host — TASKS.md T7.2's original
		// "only *.soundcloud/*.sndcdn" wording would flag the tool's own audio.
		{"playback.media-streaming.soundcloud.cloud", true},
		{"SoundCloud.com", true},  // case
		{"soundcloud.com.", true}, // trailing root dot
		{"evilsoundcloud.com", false},
		{"soundcloud.com.evil.net", false},
		{"sndcdn.com.attacker.io", false},
		{"example.com", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := ExpectedHost(tc.host); got != tc.want {
			t.Errorf("ExpectedHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestUnexpectedTargetsIsSorted: audit prints this list, and unstable ordering
// would make two identical runs look different.
func TestUnexpectedTargetsIsSorted(t *testing.T) {
	l := EgressLedger{Observed: true, Targets: map[string]int{
		"zeta.example.com:443":      1,
		"alpha.example.com:443":     1,
		"api-v2.soundcloud.com:443": 5,
	}}
	got := l.UnexpectedTargets()
	want := []string{"alpha.example.com:443", "zeta.example.com:443"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("UnexpectedTargets() = %v, want %v", got, want)
	}
}

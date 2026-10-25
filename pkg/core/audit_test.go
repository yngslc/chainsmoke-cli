package core

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

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

type auditingNet struct {
	fakeNet
	ledger EgressLedger
}

func (n *auditingNet) EgressLedger() EgressLedger { return n.ledger }

type describingStore struct {
	fakeStore
	mode    string
	results int
}

func (s *describingStore) Describe() (string, int) { return s.mode, s.results }

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

func TestAuditReportsProbedPosture(t *testing.T) {
	isolateConfig(t)

	cases := []struct {
		name   string
		net    *fakeNet
		egress Config
		want   string
		deny   string
	}{
		{"verified tor", &fakeNet{verified: true, proxy: "http://127.0.0.1:1080"}, Config{}, "egress:      TOR", "DIRECT"},
		{"verified --proxy", &fakeNet{verified: true, proxy: "http://127.0.0.1:8080"}, Config{Proxy: "http://127.0.0.1:8080"}, "egress:      PROXY", "TOR"},
		{"permitted direct", &fakeNet{}, Config{}, "egress:      DIRECT", "DOWN"},
		{"egress down", &fakeNet{assertErr: errors.New("fail-closed")}, Config{}, "egress:      DOWN", "DIRECT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := runAudit(t, &App{Net: tc.net, Egress: tc.egress})
			if !strings.Contains(out, tc.want) {
				t.Errorf("posture line missing %q:\n%s", tc.want, out)
			}
			posture := out[:strings.Index(out, "\n")]
			if strings.Contains(posture, tc.deny) {
				t.Errorf("posture line %q also claims %q", posture, tc.deny)
			}
		})
	}

	out, _ := runAudit(t, &App{Net: &fakeNet{}})
	if !strings.Contains(out, "NOT anonymized") {
		t.Errorf("DIRECT was not spelled out as unanonymized:\n%s", out)
	}
	out, _ = runAudit(t, &App{Net: &fakeNet{assertErr: errors.New("fail-closed")}})
	if !strings.Contains(out, "fail-closed") {
		t.Errorf("DOWN did not explain that nothing is permitted to leave:\n%s", out)
	}
}

func TestAuditNeverPrintsProxyCredentials(t *testing.T) {
	isolateConfig(t)
	out, _ := runAudit(t, &App{Net: &fakeNet{verified: true, proxy: "http://alice:s3cretpw@127.0.0.1:8118"}})
	if strings.Contains(out, "s3cretpw") {
		t.Errorf("audit printed the proxy password:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1:8118") {
		t.Errorf("audit no longer shows which proxy is in use:\n%s", out)
	}
}

func TestAuditReturnsSelfTestError(t *testing.T) {
	isolateConfig(t)
	boom := errors.New("egress is DIRECT")

	_, err := runAudit(t, &App{Net: &fakeNet{selfTest: boom}})
	if !errors.Is(err, boom) {
		t.Errorf("Audit() = %v, want the SelfTest error", err)
	}
}

func TestAuditWithoutSavedConfigPointsAtTheCommand(t *testing.T) {
	isolateConfig(t)

	out, _ := runAudit(t, &App{Net: &fakeNet{}})
	if !strings.Contains(out, "audit --save") {
		t.Errorf("no-config report does not name the save command:\n%s", out)
	}
}

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

func TestAuditWarnsWhenSavedTorRunsDirect(t *testing.T) {
	isolateConfig(t)
	cfg := Config{TorSocks: "127.0.0.1:9050", AllowDirect: true}
	if err := SaveConfig(cfg, true); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	out, _ := runAudit(t, &App{Net: &fakeNet{verified: false}, Egress: cfg})
	if !strings.Contains(out, "this run matches it") {
		t.Fatalf("precondition: fields should still match:\n%s", out)
	}
	if !strings.Contains(out, "this run is not anonymized") {
		t.Errorf("matching fields hid a lost anonymity guarantee:\n%s", out)
	}
}

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
		"2 request(s) refused by the bridge",
		"1 tunnel(s) failed",
		"cannot see traffic that bypassed it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ledger report missing %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "soundcloud") && strings.Contains(line, "UNEXPECTED") {
			t.Errorf("legitimate host flagged as unexpected: %s", line)
		}
	}
}

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

func TestExpectedHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"soundcloud.com", true},
		{"api-v2.soundcloud.com", true},
		{"a1.sndcdn.com", true},
		{"cf-hls-media.sndcdn.com", true},
		{"playback.media-streaming.soundcloud.cloud", true},
		{"SoundCloud.com", true},
		{"soundcloud.com.", true},
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

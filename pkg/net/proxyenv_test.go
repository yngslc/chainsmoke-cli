package net

import (
	"os"
	"testing"
)

// proxyEnvKeys is every variable a proxy-aware child might read. The test
// asserts against this whole set, not just the four we set, so that adding a
// fifth is a deliberate act with a measurement behind it.
var proxyEnvKeys = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
	"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	"FTP_PROXY", "ftp_proxy",
}

// TestExportProxyEnvSetsExactlyTheHonoredVars is the T7.3 regression guard, and
// it is a security test, not a tidiness one.
//
// MEASURED 2026-08-23 against mpv 0.41 on BOTH targets (native Windows 10 and
// WSL Ubuntu 26.04), by pointing each variable at a dead proxy and watching a
// local origin for a hit. The question is not "is the variable honored" — with
// a LIVE proxy all six spellings are honored on both platforms. The question is
// what happens when the proxy DIES, and there the answers diverge badly:
//
//	var           https:// URL, dead proxy      http:// URL, dead proxy
//	http_proxy    fail closed (win + linux)     fail closed (win + linux)
//	HTTP_PROXY    fail closed win, OPEN linux   fail closed win, OPEN linux
//	https_proxy   OPEN (win + linux)            n/a
//	HTTPS_PROXY   OPEN (win + linux)            n/a
//	ALL_PROXY     OPEN (win + linux)            OPEN (win + linux)
//
// "OPEN" means mpv silently opened a DIRECT connection to the origin when it
// could not reach the proxy — no error, no warning, audio keeps playing. For a
// tool whose entire premise is that nothing leaves except through Tor, that is
// the worst possible failure mode, and it is the one a bridge dying mid-track
// would hit.
//
// Lowercase http_proxy is the single load-bearing variable: it is the only
// spelling that fails closed on both platforms and for both schemes, and with
// it present the whole set fails closed (verified for the exact four below,
// and for those four plus ALL_PROXY). So the four are not interchangeable
// decoration — dropping the lowercase one in favour of the "conventional"
// upper-case spelling would silently turn the Linux build fail-open.
//
// This also corrects TASKS.md's earlier note that ALL_PROXY is "IGNORED". It is
// not ignored; it is honored right up until the moment it matters.
//
// Enforcement stops at what we set. chainsmoke does not scrub a hostile ambient
// environment here, because a user layering a VPN or proxychains on top is a
// supported setup and fighting their env would break it. The resolver's own
// chokepoint is where ambient proxy vars are neutralized (minimalEnv in
// pkg/resolver/ytdlp.go); this function only has to avoid lying.
func TestExportProxyEnvSetsExactlyTheHonoredVars(t *testing.T) {
	for _, k := range proxyEnvKeys {
		t.Setenv(k, "") // arms t.Cleanup to restore the real environment
		os.Unsetenv(k)
	}

	const proxy = "http://127.0.0.1:41337"
	exportProxyEnv(proxy)

	want := map[string]bool{
		"HTTP_PROXY": true, "HTTPS_PROXY": true,
		"http_proxy": true, "https_proxy": true,
	}
	for _, k := range proxyEnvKeys {
		got, set := os.LookupEnv(k)
		switch {
		case want[k] && got != proxy:
			t.Errorf("%s = %q (set=%v), want %q — a child that reads it would go DIRECT", k, got, set, proxy)
		case !want[k] && set:
			t.Errorf("%s was set to %q; ALL_PROXY fails OPEN on a dead proxy, and NO_PROXY/FTP_PROXY are not ours to touch", k, got)
		}
	}
}

// TestExportProxyEnvSetsTheFailClosedSpelling isolates the one assertion that
// is load-bearing rather than merely correct. It is deliberately a separate
// test with a name that says why: someone reducing the set above to the
// upper-case "convention" would still leave the tool working on Windows and on
// every happy path, and would only discover Linux fail-open behavior the first
// time a user's bridge went down. That is precisely the bug a test has to catch
// instead of a person.
func TestExportProxyEnvSetsTheFailClosedSpelling(t *testing.T) {
	for _, k := range proxyEnvKeys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	const proxy = "http://127.0.0.1:41337"
	exportProxyEnv(proxy)

	if got := os.Getenv("http_proxy"); got != proxy {
		t.Fatalf("http_proxy = %q, want %q — it is the ONLY spelling that makes mpv "+
			"fail closed on both Windows and Linux when the bridge dies (measured 2026-08-23)", got, proxy)
	}
}

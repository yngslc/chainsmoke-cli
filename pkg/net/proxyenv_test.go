package net

import (
	"os"
	"testing"
)

var proxyEnvKeys = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
	"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	"FTP_PROXY", "ftp_proxy",
}

func TestExportProxyEnvSetsExactlyTheHonoredVars(t *testing.T) {
	for _, k := range proxyEnvKeys {
		t.Setenv(k, "")
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

func TestExportProxyEnvSetsTheFailClosedSpelling(t *testing.T) {
	for _, k := range proxyEnvKeys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	const proxy = "http://127.0.0.1:41337"
	exportProxyEnv(proxy)

	if got := os.Getenv("http_proxy"); got != proxy {
		t.Fatalf("http_proxy = %q, want %q — it is the ONLY spelling that makes mpv "+
			"fail closed on both Windows and Linux when the bridge dies", got, proxy)
	}
}

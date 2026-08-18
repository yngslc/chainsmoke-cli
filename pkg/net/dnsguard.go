package net

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// DNS-leak assertion (T2.5).
//
// The strong half of this property is STRUCTURAL, not tested at runtime:
// connectRequest encodes the destination as SOCKS5 ATYP=0x03 and hands the
// hostname to Tor as bytes, so the exit performs the lookup and no local
// resolver is ever consulted. That is what socks5h:// means, made structural
// rather than configured, and TestConnectRequestNeverResolvesHostnames pins it.
// Nothing in the data path can regress without that test going red.
//
// So what is left for a runtime check? Not "did we leak" — we cannot leak by
// construction. What remains is the weaker, still-worth-reporting question:
// CAN this host resolve SoundCloud names at all? If it can, then anything on
// this machine that bypasses the bridge (a child process misconfigured, a
// future code path using http.Get directly) would leak silently. Reporting it
// tells the user what their exposure would be, which is what `audit` is for.
//
// SCOPE — read before trusting this:
//
//	Covers:     what THIS host's resolver is able to see.
//	Does NOT:   prove child processes (yt-dlp, mpv, ffmpeg) route DNS through
//	            the bridge. They have their own resolvers and never consult
//	            ours; they are covered only by inheriting http_proxy, which is
//	            precisely the unverified assumption T7.3 exists to close.
//
// The real assertion — a tshark capture proving zero plaintext DNS for
// *.sndcdn.com / soundcloud.com left the host during a play — is a CI job
// (T7.2). A process cannot prove that about itself; do not pretend otherwise.

// leakDomains are the names whose local resolvability we report on.
var leakDomains = []string{"soundcloud.com", "sndcdn.com"}

// dnsProbeTimeout keeps `audit` responsive on a host with no working resolver,
// where a lookup would otherwise sit through the full system retry schedule.
const dnsProbeTimeout = 2 * time.Second

// CheckLocalDNSVisibility asks whether this host's resolver can see a name.
//
// Interpretation is the subtle part, so it is stated rather than implied: a
// successful local resolution is NOT proof of a leak in chainsmoke's data path
// — the stream still goes through the bridge. It IS proof that the local
// resolver (and whoever operates it: ISP, DNS provider, corporate middlebox) is
// in a position to observe a lookup should anything ever bypass the bridge.
// That is a finding to report, not a failure to abort on.
func CheckLocalDNSVisibility(ctx context.Context, host string) (visible bool, detail string) {
	ctx, cancel := context.WithTimeout(ctx, dnsProbeTimeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	switch {
	case err != nil:
		return false, fmt.Sprintf("%-16s not resolvable locally — nothing to observe", host)
	case len(addrs) == 0:
		return false, fmt.Sprintf("%-16s no local answer", host)
	default:
		shown := addrs
		if len(shown) > 2 {
			shown = shown[:2]
		}
		return true, fmt.Sprintf("%-16s resolves locally (%s) — a bypass here would be visible to your resolver",
			host, strings.Join(shown, ", "))
	}
}

// AuditDNS probes every name we care about. Returns whether any was locally
// visible, plus one report line per name.
func AuditDNS(ctx context.Context) (anyVisible bool, lines []string) {
	for _, d := range leakDomains {
		v, detail := CheckLocalDNSVisibility(ctx, d)
		anyVisible = anyVisible || v
		lines = append(lines, detail)
	}
	return anyVisible, lines
}

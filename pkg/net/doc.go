// Package net is chainsmoke's egress seam. Everything below is what this
// package does NOT protect you from (T2.7). It is written down here, next to
// the code, because a privacy tool that overstates its guarantees is worse than
// one that makes none: it changes what its users are willing to do.
//
// # Tor exits versus media CDNs
//
// Streaming audio over Tor is flaky by nature, and not because of any bug here.
//
//   - Exit nodes are heavily rate-limited and frequently outright blocked by
//     media CDNs. SoundCloud's *.sndcdn.com hosts sit behind such a CDN. A
//     working circuit can still yield 403s, 429s, or stalls partway through a
//     track while an identical request from the same machine succeeds directly.
//   - Bandwidth through a three-hop circuit is variable and often below what an
//     HLS ladder wants. Buffering is expected, not a defect.
//   - Rotating the circuit (chainsmoke rotate) is the usual remedy when an exit
//     is blocked, but see the NEWNYM caveat in Bridge.Rotate: Tor coalesces
//     signals inside a 10s window and reports success regardless, so "rotate
//     then retry" is advice, not a guarantee.
//
// Practical consequence: expect to rotate through several exits before finding
// one the CDN will serve. If playback matters more than anonymity for a given
// session, --allow-direct exists and says so loudly. That is a real tradeoff,
// deliberately left to the user rather than papered over.
//
// # What "verified" actually means
//
// Bridge.Assert answers a policy question — may traffic leave — and passes both
// for a verified private path and for an explicitly accepted --allow-direct
// run. Bridge.Verified answers the narrower factual question and is true only
// when a real CONNECT completed through bridge -> Tor -> exit. Code that cares
// about privacy must read Verified, not Assert. Conflating them would let
// --allow-direct silently launder itself into "anonymized".
//
// A verified Up() proves the chain carried one CONNECT at startup. It does not
// prove the next one will succeed, that the exit is honest, or that the circuit
// has not since been rebuilt.
//
// Nor, by itself, that the chain is Tor. A SOCKS server that answered and
// dialled nothing was verified, and audit printed TOR ✓ for it (CH-DEBATE
// 2026-10-19, D-1). So the built-in path also asks the listener to identify
// itself first (isTorSocks): tor answers HTTP on its SOCKS port with a fixed
// page, locally. That is a fingerprint, not a proof — it catches ssh -D or a
// proxy client on the tor port, not a local program built to imitate tor.
// Through --proxy, Verified means a CONNECT completed through the user's
// proxy, whatever that is, and Audit reports PROXY rather than TOR.
//
// # What is not covered here
//
//   - Child-process egress, and with it child-process DNS. yt-dlp, mpv and
//     ffmpeg have their own resolvers and never consult Go's; what keeps them
//     on the bridge is the proxy each one is handed. yt-dlp gets an explicit
//     --proxy from the live Net handle and a minimal environment
//     (resolver.minimalEnv), so ambient proxy variables never pick its route;
//     its target arrives on stdin, not in its argv, which any local user can
//     read (2026-10-19). The --proxy value, the bridge's loopback address, is
//     still in its argv: a port any local process can find anyway.
//     mpv gets the proxy with every loadfile (stream-lavf-o=http_proxy=…, in
//     player.Enqueue), which reaches a reused daemon too, and its environment
//     loses no_proxy. Measured rather than assumed (TASKS.md T7.3, T10.9): on a
//     loopback fixture (2026-10-03) an HLS playlist and all six segments went
//     through that per-loadfile proxy with zero direct connections, and a
//     dead, dying or misbehaving proxy failed closed. Still not measured: HTTPS
//     segments over CONNECT through the real ConnectBridge, end to end. If
//     libavformat ever fetched a segment direct there, nothing in this package
//     would notice.
//   - Other local processes, for as long as a play session lasts. The
//     ConnectBridge listens on 127.0.0.1 with no authentication, and a play
//     session keeps it open until q; a one-shot verb holds it for as long as it
//     runs, which for a large likes listing is minutes (resolver.listTimeout,
//     up to three attempts). For that long any process on this machine, under
//     any user, can tunnel through it, and finding the port takes a loopback
//     scan. Every tunnel carries the same isolation credentials, so its
//     traffic rides this session's circuit and shares its exit — the merge
//     isolation exists to prevent. Since 2026-10-19 it can tunnel HTTPS on 443
//     only: no SMTP relay, no IRC, no plaintext inside a tunnel (measured on
//     the real bridge before that: all of them went through; CH-DEBATE
//     2026-10-19, D-3), and no plaintext relay at all (T7.2). A host allowlist
//     would narrow it to SoundCloud, at the price of breaking playback the day
//     SoundCloud moves to a domain not on it — as its December 2025 migration
//     moved audio to soundcloud.cloud (core.expectedSuffixes). Per-session
//     proxy credentials would close it; for now that is documented, not fixed
//     (dev's call, 2026-10-19 — 2026-10-05 review, R8).
//   - Traffic analysis. Tor does not hide that you are using Tor from your ISP,
//     and packet timing/volume of an audio stream is distinctive.
//   - The SoundCloud account itself. Anonymous client_id access is unauthenticated,
//     but an OAuth token (INSTRUCTIONS.md "Auth model") identifies you to
//     SoundCloud regardless of how the packets got there.
//   - Anything outside this process. chainsmoke controls its own egress; it does
//     not and cannot firewall the rest of the machine.
//
// # The netns swap seam
//
// If the bridge is ever judged not worth its cost, Bridge is the single seam to
// replace. The interface it satisfies (core.Net) says nothing about Tor: it
// promises only that Up() verifies egress and Assert() gates callers. A
// WireGuard-in-a-network-namespace implementation would satisfy the same
// contract with stronger properties, because it enforces at the kernel level
// rather than by handing children an environment variable they are trusted to
// honor:
//
//	ip netns add chainsmoke
//	wg-quick up ./chainsmoke.conf      # inside the netns
//	ip netns exec chainsmoke chainsmoke play 1
//
// In that world exportProxyEnv disappears entirely and T7.3 stops being a
// question, since a process in the namespace has no route that bypasses the
// tunnel. The cost is that it is Linux-only, needs root to set up, and trades
// Tor's anonymity for a VPN provider's promise. Different threat model, same
// seam — which is the point of keeping core.Net free of Tor vocabulary.
package net

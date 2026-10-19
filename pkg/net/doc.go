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
// # What is not covered here
//
//   - Child-process egress, and with it child-process DNS. yt-dlp, mpv and
//     ffmpeg have their own resolvers and never consult Go's; what keeps them
//     on the bridge is the proxy each one is handed. yt-dlp gets an explicit
//     --proxy from the live Net handle and a minimal environment
//     (resolver.minimalEnv), so ambient proxy variables never pick its route.
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
//     session keeps it open until q (a one-shot verb holds it for seconds).
//     For that long any process on this machine, under any user, that finds
//     the port can tunnel through it — on this session's Tor circuits, so its
//     traffic shares chainsmoke's exits. It gets nothing chainsmoke could not
//     do itself: CONNECT only, no plaintext relay (T7.2). Per-session proxy
//     credentials would close this; for now it is documented, not fixed
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

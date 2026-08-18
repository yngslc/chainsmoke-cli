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
//   - Child-process DNS. yt-dlp, mpv and ffmpeg have their own resolvers and
//     never consult Go's. They are routed only by inheriting http_proxy, which
//     for HLS segment fetches is an assumption, not a verified fact. That is
//     TASKS.md T7.3 and it is still open: if libavformat ever ignores the proxy
//     for a segment, audio would fetch direct while metadata went through Tor,
//     and nothing in this package would notice. Treat that as the largest
//     remaining hole.
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

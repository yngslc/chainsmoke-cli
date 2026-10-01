---
name: project-mpv-proxy-env-trap
description: mpv's proxy rides each loadfile (stream-lavf-o), with env as defence in depth — but it is silently skipped by no_proxy, by any value not starting with exactly http://, and by a missing --no-ytdl; "playback started working" can still mean a leak.
metadata:
  type: project
---

Since `b375de1` the proxy rides every `loadfile` as
`stream-lavf-o=http_proxy=<bridge>` (`MPV.Enqueue`), and the daemon also
inherits the four env spellings from `pkg/net.exportProxyEnv` as defence in
depth. The per-load binding is what makes `daemon start` and daemon reuse safe.

**Why:** two leaks, both measured on a loopback origin with a fake proxy and
real mpv. First, 2026-09-09: `e2f2e01` stripped mpv's env to `PATH`, and mpv
fetched the origin directly. Second, 2026-10-01, found by both the reviewer and
the debater: libavformat honours a proxy only when the value starts with exactly
`http://`, so `--proxy HTTP://` or `https://` went direct. It also honours
`no_proxy` even over the per-load option, so `no_proxy=*` went direct. In both
cases the Go preflight still tunnelled, so this was a correlation leak. All of
it is fixed:
- `Up()` lower-cases `--proxy` and refuses `https://`.
- `Enqueue` refuses any non-`http://` value before the preflight.
- `Ensure()` spawns mpv with the env minus `no_proxy`, and keeps the rest.

A before/after rig showed three BYPASS rows become clean.

**How to apply:** treat any change to how mpv is spawned, to `Enqueue`'s
options, or to how `--proxy` is stored as a possible fail-open, and treat
"playback started working again" as a leak signal, not a fix. `--no-ytdl` is
load-bearing too: without it, a failed load runs mpv's ytdl hook, which spawns
yt-dlp with the URL in argv and no proxy. Pinned by `TestEnsureSpawnsTheDaemonSafely`,
`TestEnqueueRefusesAProxyMpvWouldIgnore` and
`TestOverrideIsStoredWithALowercaseScheme`.

Re-verify before acting: read `Ensure()`/`Enqueue()` in `pkg/player/mpv.go`
and Gate 1 in `pkg/net/bridge.go`. Related: [[feedback-verify-dont-recall]].

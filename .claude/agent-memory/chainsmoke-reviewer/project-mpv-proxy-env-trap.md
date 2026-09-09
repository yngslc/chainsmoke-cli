---
name: project-mpv-proxy-env-trap
description: The mpv daemon's proxy is bound at spawn time via inherited env only — stripping or missing that env makes playback WORK while leaking directly, so "playback started working" is a leak signal here, not a fix.
metadata:
  type: project
---

The mpv daemon has no explicit proxy wiring. Its only route to the bridge is the
four env spellings `pkg/net.exportProxyEnv` sets in the chainsmoke process,
inherited at `exec.Command("mpv", ...)` spawn time. Nothing on `loadfile` carries
a proxy.

**Why:** measured 2026-09-09 during the Phase 10 gate review. Commit `e2f2e01`
added `cmd.Env = []string{"PATH=..."}` to `MPV.Ensure()`, which stripped that
inheritance. Local experiment (dead proxy on 127.0.0.1:1, loopback origin, mpv
0.41): with the stripped env mpv issued `GET /probe.mp3 HTTP/1.1` straight to the
origin; with the env inherited the origin saw nothing. Same code, one line apart.

**How to apply:** treat any change to how mpv is spawned, or any report that
"playback started working again", as a possible fail-open. The failure this
replaces — a daemon reused across runs pointing at last run's dead ephemeral
bridge port — is silent non-playback, which is the *safe* symptom. The durable
fix is per-load options on `loadfile` (`stream-lavf-o=http_proxy=...`) plus an
allowlisted env that keeps lowercase `http_proxy`, so the proxy is bound per
track rather than per daemon. Also note `daemon start` never calls `Net.Up()`, so
a daemon started that way was never proxied even before `e2f2e01`, and `Ensure()`
reuses a live daemon rather than re-spawning it.

Re-verify before acting: check whether `cmd.Env` is still set in
`pkg/player/mpv.go` and whether `Enqueue` has grown per-file options.
Related: [[feedback-verify-dont-recall]].

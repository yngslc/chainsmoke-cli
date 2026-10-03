---
name: run-chainsmoke
description: Build, run, and test chainsmoke, the privacy-focused SoundCloud Go CLI in this repo. Use when asked to run, start, build, launch, or test the app, verify CLI behavior, or re-check the verb baseline after implementing a subsystem. Make sure to read the Gotchas section.
---

`chainsmoke` is a privacy-focused SoundCloud CLI over Tor. The entry point is
`cmd/chainsmoke/main.go` — CLI surface and wiring only — over `pkg/{core,net,
resolver,player,scrub,store}`. Every subsystem is real except `pkg/scrub`,
which is still an `ErrNotImplemented` stub (so `get` dies there). Drive it via
`.claude\skills\run-chainsmoke\smoke.ps1`, which vets, builds `chainsmoke.exe`,
and asserts the current behavior baseline. `script.sh` is the frozen bash
prototype (T0.3) — reference only, never edited to fix bugs.

All commands below are PowerShell, run from the repo root.

## Prerequisites

Go, on PATH or via this portable bootstrap (no admin rights; verified on this
machine — `curl.exe` and `tar.exe` ship with Windows 10):

```powershell
curl.exe -sL -o "$env:TEMP\go.zip" https://go.dev/dl/go1.26.5.windows-amd64.zip
New-Item -ItemType Directory -Force "$env:LOCALAPPDATA\go-portable" | Out-Null
tar.exe -xf "$env:TEMP\go.zip" -C "$env:LOCALAPPDATA\go-portable"
& "$env:LOCALAPPDATA\go-portable\go\bin\go.exe" version
# → go version go1.26.5 windows/amd64
```

The smoke driver auto-detects this location when `go` is not on PATH.

The runtime deps from `script.sh` (mpv, yt-dlp, ffmpeg, jq, socat, tor,
privoxy) are not all needed: `privoxy` was dropped in T2.2 and `jq`/`socat`
were bash-only. Three matter now, and all three are installed:

- **mpv** `0.41.0` — `winget install --id shinchiro.mpv` → `C:\Program Files\MPV Player\` (added to the User PATH).
- **yt-dlp** `2026.07.04` — portable exe at `%LOCALAPPDATA%\chainsmoke-tools\bin\yt-dlp.exe` (on the User PATH).
- **tor** — runs inside WSL (Ubuntu-26.04), reachable from native Windows on `127.0.0.1:9050` through WSL2 localhost forwarding, so `chainsmoke.exe` needs no changes to use it.

`ffmpeg` ships inside the mpv build. Tor's ControlPort (9051) is NOT enabled on
this box, so `rotate` cannot send NEWNYM and falls back to re-isolating its
SOCKS credentials — which is why that verb still exits 0.
Because a fresh install persisted these to the *User* PATH, a shell started
earlier may not see them — `smoke.ps1` re-reads Machine+User PATH from the
registry to compensate.

## Setup

`go.mod`/`go.sum` are checked in (module `chainsmoke`, requires
`github.com/urfave/cli/v2`). Only if they're ever missing, regenerate:

```powershell
$env:Path = "$env:LOCALAPPDATA\go-portable\go\bin;$env:Path"
go mod init chainsmoke
go mod tidy
```

## Build

The build target is `.\cmd\chainsmoke`, **not** `.` — the repo root has held no
Go files since the T0.4 split, and `go build .` fails with `no Go files in
<repo root>`.

```powershell
$env:Path = "$env:LOCALAPPDATA\go-portable\go\bin;$env:Path"   # skip if go is on PATH
go build -o chainsmoke.exe .\cmd\chainsmoke
.\chainsmoke.exe --version
# → chainsmoke version development
```

**Release builds go through `build.ps1`, not a bare `go build`** (T8.3):

```powershell
powershell -ExecutionPolicy Bypass -File .\build.ps1 -Version 0.1.0        # this host
powershell -ExecutionPolicy Bypass -File .\build.ps1 -Version 0.1.0 -All   # 6 targets -> .\dist
```

It vets first, then builds with `-trimpath -ldflags "-s -w -X main.BuildVersion=..."`
and `CGO_ENABLED=0`. **`-trimpath` is not cosmetic:** measured 2026-08-24, a
build without it embeds 15 absolute source paths (`D:/sccli/pkg/core/browse.go`
and friends), disclosing the builder's drive letter and directory layout to
anyone holding the binary. Since 2026-10-01 `build.ps1` checks this on every
artifact, as CI does: it fails if the repo path or `%USERPROFILE%` (module-cache
paths carry the username) appears in the binary. `-s -w` takes 13 MB to 8.9 MB. The Linux artifact is
confirmed `statically linked` by `file(1)`; darwin cross-compiles but has no
dev target here, so it is untested.

`chainsmoke.exe` lands in the repo root. Note `go build .\cmd\chainsmoke`
compiles only that package's dependency graph — run `go vet ./...` (as smoke.ps1
now does first) to catch a broken `pkg/*` that nothing imports yet.

## Run (agent path)

The driver builds and exercises the whole verb surface, asserting exit codes
and output against the current baseline:

```powershell
powershell -ExecutionPolicy Bypass -File .claude\skills\run-chainsmoke\smoke.ps1
# → PASS x40, "smoke: all 40 cases pass", exit 0
```

**When you implement a subsystem, flip the corresponding expectations in the
case table inside `smoke.ps1`** — it is the CLI-wiring regression harness.

Verified behavior baseline (what the carcass does today):

| invocation | exit | behavior |
|---|---|---|
| `.\chainsmoke.exe --help` | 0 | full command/flag table |
| `.\chainsmoke.exe` (bare, piped) | 0 | the same command table — smoke's `bare-pipe`. T10.1 routes a bare TTY run to the TUI; this row must not change |
| `.\chainsmoke.exe --version` | 0 | `chainsmoke version development` |
| `.\chainsmoke.exe search kendrick` | 0/1 | real scsearch through the chokepoint (T3.3) — needs egress; fails closed without it. Prints the T8.1 table on a TTY, TSV down a pipe |
| `.\chainsmoke.exe likes <user>` | 0/1 | `/likes` page through Meta (T8.2); handle validated BEFORE `Net.Up()`, so a typo costs no circuit |
| `.\chainsmoke.exe playlist <user>/<set>` | 0/1 | same, for `/<user>/sets/<slug>`. Also accepts the `<user>/sets/<slug>` spelling |
| `.\chainsmoke.exe completion bash\|zsh\|fish` | 0 | prints a completion script, with NO banner (T8.3) |
| `.\chainsmoke.exe np` | 0 | real IPC round-trip (T5.2); `nothing playing` when the daemon is idle. Needs a daemon — without one it exits 1 with `mpv IPC ... unreachable` |
| `.\chainsmoke.exe toggle`, `vol N`, `next`, `prev` | 0 | real IPC commands against the daemon |
| `.\chainsmoke.exe play 1` | 1 | `net.Up` warns DIRECT (no proxy), then `no cached result #1 — run \`chainsmoke search <term>\` first`. No egress. |
| `.\chainsmoke.exe --proxy http://127.0.0.1:9 play 1` | 1 | `--proxy ... not reachable` — proves the flag reaches the bridge (T1.2) |
| `.\chainsmoke.exe daemon start` | 0 | spawns `mpv --idle` and liveness-probes it over IPC (T5.1); returns nil if one is already up |
| `"https://soundcloud.com/artist/track" \| .\chainsmoke.exe get` | 1 | stdin URL is read (no-URLs-in-argv contract), then dies in the scrubber stub |
| `.\chainsmoke.exe purge` | 0 | really wipes both stores now (T4.4): runtime + state dirs, with the player taken down first |
| `.\chainsmoke.exe stop` | 0 | mpv `stop`: clears the queue and ends playback, daemon stays up (T5.5). Killing the daemon is `daemon stop`, which sends `quit` |
| `.\chainsmoke.exe audit` | 1 | probes, never prints config (T7.1). Reports posture (TOR / DIRECT / DOWN), proxy, impersonate, store mode + cached count, drift against the saved config, the egress ledger (T7.2), and local DNS. Exit 1 whenever the setup is not healthy |
| `.\chainsmoke.exe audit --save` | 0/1 | records this run's egress settings with the posture the probe PROVED. Exit still tracks the audit, not the save — with Tor up it is 0, on a DIRECT run it saves, warns, and exits 1 |
| `.\chainsmoke.exe seek` / `vol 900` | 1 | arity/range rejected before mpv ever sees the argument |
| `.\chainsmoke.exe bogus` | 3 | `No help topic for 'bogus'` |

`play` today: `net.Up` (system tor on :9050, else a private one, else refuse —
`--proxy` or `--allow-direct` override) → `Ensure()` spawns the mpv daemon →
`play N` plays result N from the cached list and preloads N+1 (`queue.go`; a
stdin URL still goes `Meta` → `EnqueueAhead`) → **then `play` holds**
(`cmd/chainsmoke/session.go`): on a terminal it becomes a `chainsmoke>` prompt
that prints every verb it accepts, with `next`/`prev` walking the whole list
(`q`/Ctrl+D/Ctrl+C stops), otherwise it waits for the queue to drain, still
preloading one ahead. See the "play holds the Tor path" gotcha for why. A real
end-to-end play needs a live SoundCloud URL and makes a network request, so it
is not part of the offline smoke baseline — verified live 2026-09-28 instead:
`time-pos` ran 0 → 31.7s in real time with the daemon muted.

## Direct invocation (dev loop)

Most PRs here implement a stub. Find what's left with:

```powershell
findstr /s /n "ErrNotImplemented" cmd\*.go pkg\*.go
```

Then `go vet ./...` + the smoke driver are the inner loop. Real egress needs
tor on :9050 (privoxy is gone since T2.2 — the CONNECT→SOCKS5 bridge is
in-process and picks its own ephemeral port). Without tor every egress verb
fails closed unless `--allow-direct` is passed.
## Test

```powershell
go vet ./...    # clean, exit 0
go test ./...   # 167 passing: core 51, net 36, resolver 23, player 22, store 13, cmd 22 (+3 unix-only, skipped on Windows; WSL -race runs all 170)
```

**The integration suite is separate and opt-in** (T9.2). Five tests behind
`//go:build integration` that need a live Tor or a real mpv:

```powershell
go test -tags integration ./pkg/net/ ./pkg/player/     # 5 tests; SKIP if the dep is absent
wsl -e bash -c "cd /mnt/d/sccli && go test -tags integration ./... -run Integration"
```

A build tag rather than `testing.Short()` on purpose: the default
`go test ./...` — what CI runs and what a contributor runs by reflex — **must
never open a circuit**. Each test skips rather than fails when its dependency is
missing, because a red suite meaning "you have no tor" teaches people to ignore
red suites. `CHAINSMOKE_TOR_SOCKS` overrides the SOCKS address (9150 for the
Tor Browser bundle).

**CI is Linux, and not by preference** (`.github/workflows/ci.yml`). `-race`
needs cgo and a C toolchain, and this Windows box has neither — measured
2026-08-24: `go: -race requires cgo`. So natively the race detector has no
home except CI. On this box it also runs under WSL, which has the toolchain:
`wsl -e bash -lc "export PATH=\$PATH:/usr/local/go/bin; cd /mnt/d/sccli && go test ./... -race"`
(green 2026-10-01). It matters because `pkg/net` is the genuinely concurrent part (accept and
splice goroutines, a mutex-guarded ledger, an RWMutex in the SOCKS dialer). CI
runs gofmt, vet, `test -race`, a compile-only pass over the tagged suite so it
cannot rot behind its tag, and the six-target cross-build with an assertion that
no absolute build paths were embedded. It does **not** run the integration
tests.

Test coverage lives where the invariants are, not spread evenly: `pkg/store`
asserts `--private` writes zero bytes; `pkg/player` asserts a signed URL cannot
reach an error string; `pkg/resolver` pins the retry policy and the error
taxonomy against stderr captured from real yt-dlp, including which recovery
hint each failure earns; `pkg/net` pins the fail-closed proxy spelling and the
egress ledger; `pkg/core` pins what `audit` may claim, that a page URL built
from user input cannot leave soundcloud.com, and that the results table never
prints a permalink. `pkg/scrub` has none, and `cmd/` tests wiring only (see
below) — the CLI surface itself is covered by smoke.ps1.

`pkg/core/table_test.go` is the one place tests exist for a non-security
reason: column alignment is arithmetic (display cells, not bytes or runes), and
arithmetic is worth asserting rather than eyeballing. `TestTableColumnsAlign`
renders CJK, emoji and combining marks at five widths and checks every row ends
at the same cell.

`cmd/chainsmoke/main_test.go` (T9.1) tests **wiring, never the entry point** —
that distinction is load-bearing, because T10.1/T10.2 rewrite bare-invocation
routing, and anything asserting that would be written twice. What it does pin
survives Phase 10: `TestEgressVerbsGateOnUp` drives all five egress verbs with
`Up()` refusing and asserts the resolver is never touched;
`TestCommandTableIsWellFormed` catches a duplicate verb or alias, where urfave
resolves the FIRST match and silently shadows the second; and
`TestBannerClassification` walks every command path and fails on any verb not
explicitly classified as banner / no banner — add a verb and it makes you
decide. `TestBannerSkipsHelpAndCompletion` drives real argv through urfave to
prove help, `--version` and TAB never print the art.

## Gotchas
- **Build `.\cmd\chainsmoke`, never `.`** — see Build. This is the single most
  common way to think the tree is broken when it isn't.
- **`go build` alone does not prove the module is sound.** It only compiles the
  target's import graph, so an unreferenced or malformed `pkg/*` file slips
  through. `go vet ./...` is the check that covers everything; smoke.ps1 runs it
  before building for exactly this reason.
- **`search` is real now (T3.3), so it LEAVES THE BOX.** It runs
  `yt-dlp scsearchN:<term>` through the chokepoint. Do not add a smoke case
  that searches for real: with no tor up, that is an unanonymized request to
  SoundCloud from the dev machine. The two search cases pass
  `--impersonate smoke-invalid`, which yt-dlp rejects before it opens a socket
  — offline, and it still proves the flag reached yt-dlp (T3.4).
- **A slow `search`/`play` is the retry policy, not a hang.** Over Tor the exit
  you draw decides everything: yt-dlp is given a 15s socket timeout and up to 3
  attempts, rotating the circuit between the slow ones, so a bad draw can take
  ~65s before it gives up. `chainsmoke: no answer through this Tor circuit —
  trying a fresh circuit (2/3)` on stderr is the policy working: one line per
  retry, the full yt-dlp error only if every attempt fails (2026-10-03). Listings
  (`likes`, `playlist`) get 3 minutes per attempt, not 45s: a 1,700-track likes
  list comes a page at a time over Tor, and the 45s cap killed it mid-fetch on
  every attempt (2026-10-03, `TestListingsGetAListingBudget`). Measured 2026-08-19 over 6 fresh circuits
  (`pkg/resolver/ytdlp.go:29-31`): one completed in 22.9s, every other one
  stalled. Treat it as "most circuits fail SoundCloud", not a precise rate
  (n=6; this line said "1 in 3" until 2026-10-01).
- **`rotate` now brings egress up first.** Without a live bridge there is
  nothing to re-isolate, and on a system tor with no ControlPort (this box) the
  verb used to fail outright. It now exits 0 and says which half it managed.
- **The IPC client is real, and it is synchronous (T5.2).** One connection,
  `request_id` correlation, async events discarded — see `pkg/player/mpv.go`.
  It is NOT safe for concurrent use and does not need to be under T0.1(a). The
  TUI's event-while-idle client (T10.7) is a second implementation behind the
  same `Command` seam, not a change to this one.
- **`daemon start` leaves mpv running, and `stop` deliberately does not kill
  it.** `stop` is mpv's own `stop` (queue cleared, session kept); `daemon stop`
  is what sends `quit` (T5.5). **Until 2026-09-28 `daemon stop` and `purge` were
  silent no-ops**: `Down()` returned before dialling in a fresh process, so both
  exited 0 and the daemon lived on. Fixed, and pinned by smoke's `daemon-stop`
  → `np-gone` pair on the real named pipe. `play` checks its argument before
  `Ensure()` (2026-10-01), so a typo no longer spawns a daemon — smoke's
  `np-none` pins that — and the final `teardown` case is only a safety net.
- **`quit` only reaches the mpv that owns the pipe; `Down()` kills the rest.**
  A second mpv started on `\\.\pipe\chainsmoke-mpv` logs `Couldn't create first
  pipe instance`, then idles with no IPC, where no `quit` can reach it
  (measured 2026-09-28). So `daemon stop`/`purge` finish by killing every
  `mpv.exe`/`mpv.com` whose command line names that endpoint, and print
  `killed N stray mpv process(es)` when they do. An mpv the user started
  without that endpoint is never touched (`TestDownKillsStrays`). Note that
  `mpv` resolves to the `mpv.com` wrapper (PATHEXT), so one daemon is two
  processes in `tasklist`.
- **`audit` is the ONE verb that still runs when egress is down (T7.1).** Every
  other verb aborts on `Up()`'s refusal because it is about to send traffic;
  audit sends no SoundCloud traffic of its own (its one local DNS lookup is a
  neutral name — until 2026-10-01 it resolved soundcloud.com and sndcdn.com
  itself, the exact query INSTRUCTIONS.md:128 forbids; review N-4). And a
  diagnostic that refuses to run exactly
  when the thing it diagnoses is broken is useless. It prints the report and
  then returns the failure, so the exit code is unchanged — do not "fix" the
  non-zero exit by making the report a success.
- **Posture has THREE states, not two.** `TOR` (verified), `DIRECT` (permitted
  by `--allow-direct`, unanonymized), and `DOWN` (nothing permitted to leave —
  `Assert()` fails). Collapsing the last two tells a user their traffic is
  leaking when it is in fact being refused.
- **`audit --save` writes to the real config dir, so tests must sandbox it.**
  `%APPDATA%\chainsmoke\config.json` (or `$XDG_CONFIG_HOME`). smoke.ps1 points
  `XDG_CONFIG_HOME` at a temp sandbox it wipes at startup — without that, the
  six audit cases would both depend on and destroy the developer's real saved
  config. Any new audit case belongs after that block, and the ordered group
  (`audit-down` → `audit-nosave` → `audit-noconfig` → `audit-save` →
  `audit-match` → `audit-drift`) must stay in order: the first three assert the
  "nothing saved yet" branch.
- **The egress ledger is process-scoped, and `audit` is its own process
  (T7.2).** So a bare `audit` shows only the tunnel its own self-test opened —
  never what an earlier `play` did. That is the honest scope, not a bug:
  persisting it would mean writing destination hostnames to disk, which is the
  trail the whole tool exists to avoid. `Observed: false` (a `--proxy` or
  DIRECT run) means "we could not look" and must never be rendered as "nothing
  left the machine".
- **The proxy env vars are a security invariant, not a formatting choice
  (T7.3).** Measured 2026-08-23 on mpv 0.41, both targets: with a LIVE proxy
  all six spellings work, but with a DEAD one, `ALL_PROXY`, `https_proxy` and
  `HTTPS_PROXY` all make mpv **silently open a direct connection** — and on
  Linux so does `HTTP_PROXY`. Lowercase `http_proxy` is the only spelling that
  fails closed everywhere, and its presence is what makes the shipped four fail
  closed. Do not "simplify" `exportProxyEnv` to the upper-case convention or to
  `ALL_PROXY`; `pkg/net/proxyenv_test.go` will fail, and it is right to.
- **`search` → `play N` works now (T4.1), and that has a cost.** Results live in
  `$XDG_RUNTIME_DIR/chainsmoke/results.json` (`%TEMP%\chainsmoke` on Windows),
  0700/0600, and they SURVIVE between invocations. Any smoke case asserting
  "no cached result" must therefore purge first, or a real search from an
  earlier session makes it flaky — that is why `purge-first` sits ahead of
  `play-nocache` in the table.
- **On Windows the "ephemeral" store is a real disk write.** The design leans on
  `$XDG_RUNTIME_DIR` being tmpfs, i.e. gone at logout. `%TEMP%` is not tmpfs and
  survives a reboot, so on this dev target `purge` (or `--private`) is the only
  thing that actually removes the permalinks — worth knowing before calling the
  default "ephemeral" in front of a user.
- **`--private` is isolated, not just write-blocked (T4.3).** It reads nothing
  either, so `search` (normal) then `--private play 1` correctly reports no
  cached result. It writes zero bytes to either store directory — asserted in
  `pkg/store/store_test.go`, not just intended.
- **Unknown verbs exit 3, not 1** (urfave/cli's `CommandNotFound` path). Every
  other failure is exit 1 — there is no MISUSE/NOT_FOUND differentiation yet.
- **The banner prints only for the SoundCloud/session verbs, and only on a
  TTY (dev's call, 2026-09-28).** It is a `Before: banner` hook on `search`,
  `likes`, `playlist`, `play`, `get` and `daemon start|stop|status`; nothing
  else prints it — not help, `--version`, completion, the remote controls, or
  audit/rotate/purge. urfave returns for `--help`/`-h` and for the completion
  protocol *before* calling a command's Before, which is what keeps the art off
  `search --help` and TAB. Two ways to break it: putting the hook on `daemon`
  itself (a parent's Before runs before its subcommand parses `--help`, so the
  art lands above `daemon start --help`), and adding a verb without classifying
  it — `TestBannerClassification` catches both. A piped stdout never gets it,
  so smoke.ps1 cannot see it at all; it asserts the absence instead. This
  replaced `bannerCorruptsOutput`/`valueFlags`, which no longer exist.
- **The results table decides its columns from the DATA (T8.1).** Measured
  2026-08-24 through Tor, `--flat-playlist` returns three shapes: `scsearch`
  carries title+uploader+duration, user pages (`/likes`, `/tracks`) carry title
  only, and SETS carry *neither* — no `title` field at all. So `likes` prints a
  one-column table by design, and `playlist` titles come from
  `titleFromSlug` in `pkg/resolver`, not from SoundCloud. Do not "fix" a
  missing UPLOADER column; there is no uploader in that response.
- **A terminal gets the table, a pipe gets TSV.** Four fields, no header, no
  truncation, duration in SECONDS and empty (not 0) when unknown. smoke.ps1
  captures through a pipe, so it always sees the TSV path — assert on TSV there.
  "Terminal" means a real console/tty (`isTerminal` in `pkg/core/term_*.go`),
  not the character-device bit: `NUL` and `/dev/null` carry that bit too, so
  until 2026-10-01 `play 1 < /dev/null` (nohup, cron) opened the prompt, read
  EOF and stopped playback at once. It now waits for the queue to drain.
- **Track titles are third-party text and are scrubbed before display.** A
  newline would forge a numbered row (pointing `play N` at something the user
  never saw), a tab would wreck the TSV, an ESC would repaint the terminal.
  `core.Clean()` in `pkg/core/table.go` handles it; `TestCleanRejectsHostileTitles`
  is the guard. Since 2026-10-01 it also covers the two other places a title
  reaches the terminal, the `▶` line and `np` (09/09 review, CQ-4).
- **`likes`/`playlist` take a HANDLE, never a URL**, and that is the deliberate
  line: a handle is the same exposure as `search <term>`, a track URL is not
  (invariant #3). `cleanHandle`'s allowlist is also the reason the URL handed to
  the chokepoint can only ever point at soundcloud.com — a 17-case injection
  sweep asserts it. Do not relax it to "accept a pasted URL and parse out the
  handle"; by then argv already has the URL.
- **`play` drives the daemon over IPC — the old "foreground mpv" note was
  stale and has been corrected (2026-08-24).** T5.3 landed: `Ensure()` spawns
  `mpv --idle`, and `Enqueue` sends `loadfile <url> append-play` over the socket
  with `force-media-title`. One process, and the signed URL never touches argv
  (invariant #3). The only `exec.Command("mpv", ...)` left is the daemon spawn.
- **`play` holds the Tor path open, and must.** The CONNECT bridge lives in the
  `play` process; mpv opens each stream and every later HLS segment
  asynchronously, after `loadfile` has already answered "success". So the proxy
  rides each `loadfile` (`stream-lavf-o=http_proxy=<bridge>`, not inherited env
  — a reused daemon's env names a dead port), AND `play` stays alive while the
  queue plays. Measured 2026-09-28 on a loopback replica: a `play` that returned
  at once → `httpproxy: -10054` → `Failed to open` → daemon idle with the
  entries still queued; the identical run with its proxy kept alive played. Both
  failure modes are **fail-closed** — the origin saw zero connections once the
  proxy died, HTTPS/CONNECT path included — so "▶ printed, silence" is broken
  playback, never a leak. The flip side: if playback ever "just works" after a
  change to how mpv is spawned, suspect it went direct (see T7.3).
  Ending a session sends mpv's global `stop`, so quitting one of two concurrent
  `play` sessions stops the other's queue too: one daemon, one playlist (N-6).
- **mpv's proxy has two silent bypasses, closed 2026-10-01 — keep them closed.**
  libavformat honours a proxy only when the value starts with exactly `http://`
  (so `HTTP://` and `https://` meant DIRECT), and it honours `no_proxy` even
  over the per-`loadfile` option (so a user's `no_proxy=*` meant DIRECT). In both
  cases the Go preflight still tunnelled, so the CDN saw the same signed URL
  from Tor and from the real IP seconds apart. Now `Up()` lower-cases `--proxy`
  and refuses `https://`, `Enqueue` refuses any non-`http://` value before the
  preflight, and `Ensure()` drops `no_proxy` from mpv's environment while keeping
  the rest (stripping all of it was the `e2f2e01` leak). `--no-ytdl` is
  load-bearing as well: without it, a failed load runs mpv's ytdl hook, which
  spawns yt-dlp with the URL in argv and no proxy. Pinned by
  `TestOverrideIsStoredWithALowercaseScheme`, `TestEnqueueRefusesAProxyMpvWouldIgnore`
  and `TestEnsureSpawnsTheDaemonSafely`. These were measured before and after the
  fix on a loopback rig (fake origin plus fake proxy, real mpv on a scratch pipe):
  three BYPASS rows became clean. Re-run such a rig if you touch the spawn or
  `Enqueue`.
- **`Ensure()` trusts only a daemon it marked (2026-10-03).** The `no_proxy`
  fix reaches only daemons this build spawns. A live daemon started by an
  older chainsmoke (the 08-24 `dist/` binaries) or by hand still bypassed on
  reuse, measured before/after on the same rig. So `Ensure()` spawns with
  `--script-opts=chainsmoke-spawn=<spawnGen>` (in argv, so a still-booting
  daemon has it too) and, before reusing a live daemon, reads back that key
  and `ytdl=false`. Anything else is replaced, with a one-line notice on
  stderr. **Bump `spawnGen` whenever the spawn's safety changes**, so older
  daemons get replaced instead of trusted. `TestEnsureReusesOnlyADaemonItMarked`.
  It also now notices an mpv that dies on startup (`mpv exited while
  starting`) instead of waiting out the 10s window. That window replaced 3s
  after the first `daemon start` of 2026-09-28 hit the 3s limit while mpv went
  on to answer about a second later. A warm start is about 90ms.
- **A non-default `--tor-socks` never spawns a private tor (2026-10-03).**
  Nothing listening there means fail closed. Before, it fell through to
  spawning `tor` from PATH, which bootstraps onto the network. With any tor on
  PATH, that turned every smoke `$noTor` row into real egress (a fake `tor.exe`
  made one row take 46s). The private tor's torrc also names this process as
  its owner (`__OwningControllerProcess`), so it exits by itself about 12s
  after chainsmoke dies by any route. Its DataDirectory still stays behind
  (T10.9).
- **`play` checks an argv index or URL before `Up()`, and prompts for a stdin
  URL only after it (2026-10-03).** A typo costs no circuit (TASKS.md:405), and
  nobody pastes a URL just to learn Tor is down. That is why smoke's
  `proxy-wired` and `failclosed` rows run a bare `play`: with an index and an
  empty cache, they would stop before `Up()`.
- **The cached results play like a playlist, one track ahead (2026-10-03).**
  `play N` (from search, likes or playlist) stops whatever was queued, plays N,
  and preloads N+1 with no `Meta` round-trip. At the prompt, `next`/`prev` walk
  the list (`queue.go`), and a tick tops up the preload when mpv advances by
  itself. Never more than one ahead: signed URLs expire, and each resolve is a
  Tor round-trip. Before this, `next` after `play N` failed with
  `playlist-next: error running command`, because only N was ever queued. A
  one-shot `chainsmoke next` from another window is still plain mpv.
  The prompt also has its own `shuffle [on|off]` (the rest of the list in
  random order, each track once, from the very next track) and
  `repeat one|all|off`. One is mpv's `loop-file`; all wraps the list, and
  reshuffles in shuffle. The session's end clears repeat, because the daemon
  outlives it. Every track change is announced as `▶ N. title · next: M. title`,
  including one mpv makes by itself when a song ends. Before 2026-10-03 that
  was silent, and a `next` meant to start playback skipped a song.
- **New daemons start at volume 50** (`--volume=50`); a reused one keeps
  whatever `vol` last set.
- **Rotation is quiet.** Re-isolating on the SOCKS port is what gives the next
  stream a fresh circuit; NEWNYM on ControlPort 9051 is a silent bonus, tried
  once per process (`TestRotateIsQuietWhenReisolationWorks`). On a tor with no
  ControlPort (this box) it used to print two lines on every retry.
- **`Net.Up()` is once per process.** A play session runs several verbs, each
  calling `Up()`; a second pass would start another bridge (and maybe another
  private tor) that `Down()` never closes. `TestUpIsIdempotent` pins it.
- **mpv JSON-IPC works on BOTH targets** — unix socket under WSL, named pipe
  (`\\.\pipe\chainsmoke-mpv`) on native Windows; verified against mpv 0.41 with
  a live `get_property` round-trip. Same protocol, different dial. `core.SocketPath()`
  already returns the right endpoint per platform, and `pkg/player` abstracts
  the dial as T10.7 asked: `net.Dial("unix", ...)` on POSIX, `os.OpenFile` on
  the named pipe. Stdlib only — no go-winio — but a blocked read there cannot
  be interrupted by `Close()`, which is the TUI's problem, not the CLI's.

## TASKS
- this is a check-up prompt:
// alright, drop the status: 
// are we finished with phase <current_phase>?
// is there anything we can fix?
// is there anything we have to add?
// should run-chainsmoke be redacted for the next phases?
// should INSTRUCTIONS.md be redacted for the next phases?
- review these questions carefully each time dev mentions TASKS.md. if dev accepts your review, you may proceed with redacting each file that's mentioned during this check-up.

## Troubleshooting

- **`go : The term 'go' is not recognized`**: no Go on PATH. Run the portable
  bootstrap in Prerequisites (needs no admin), or pass the toolchain
  explicitly: `smoke.ps1 -GoExe <path-to-go.exe>`.
- **`no Go files in D:\sccli`**: you built `.` instead of `.\cmd\chainsmoke`.
- **`expected 'package', found 'EOF'`**: an empty `.go` file somewhere under
  `pkg/`. `go build .\cmd\chainsmoke` may still succeed while `go vet ./...`
  fails — trust vet.
- **WSL works** (Ubuntu-26.04, kernel 6.18, `ext4.vhdx` present) and carries
  `go`, `mpv`, `yt-dlp`, `ffmpeg`, `curl`. It is the target for unix-socket IPC
  work: `wsl -e bash -c "cd /mnt/d/sccli && go vet ./..."`. Note
  `XDG_RUNTIME_DIR` is **unset** there and the default user is uid 0, so
  `core.RuntimeDir()` resolves via the `/run/user/0` fallback — that path is
  exercised on every WSL run, not just in theory.
- **`git` is not on the native Windows PATH.** Reach it through WSL:
  `wsl -e bash -c "cd /mnt/d/sccli && git status"`.

# CH-DEBATE-2026-10-03 — phase10

**Date of debate and phase**: 2026-10-03 — `phase10` (the last `## Phase N` heading in TASKS.md without `COMPLETED`, TASKS.md:515).

**Target**: the fixes in commit `9c6a7fd` ("subtle fixes by claude itself", 2026-10-01). The main agent wrote them and the dev committed them. They close items from three reports:
- `report-review/CH-2026-10-01-phase10.md`
- `report-debate/debaters-output.md` (claims 3, 4 and 11)
- `report-driftwatch/CH-DRIFT-2026-10-01-phase10.md`

I also checked the main agent's claim list handed to me, and every doc line the commit corrected. Tree: `D:\sccli` at `9c6a7fd`. `git status --short` was clean before and after; I wrote only this file and scratchpad rigs.

**How measured** (2026-10-03). Two environments:
- **Native Windows 10:** go1.26.5 and mpv 0.41.0-244.
- **WSL Ubuntu, reachable from this sandbox:** go1.26.5, mpv 0.41.0, tor 0.4.9.11 and ffmpeg.

Constraints held throughout:
- Every rig was loopback-only and used scratch pipes (`\\.\pipe\debater-rig*`) or temp unix sockets.
- Every tor I started had `DisableNetwork 1`.
- I did not run smoke.ps1 or `-tags integration`, and nothing touched `\\.\pipe\chainsmoke-mpv`.
- `tasklist` showed no mpv or tor left over after any rig.

Rig shapes are in the appendix.

---

## Re-measured: the commit's own numbers

| claim (main agent, 2026-10-01) | 2026-10-03 |
|---|---|
| gofmt and vet clean on windows/linux/darwin | **reproduced**: `GOOS={windows,linux,darwin} go vet ./...` clean, `gofmt -l ./cmd ./pkg` empty (this checkout is `i/lf w/lf`) |
| Windows: 149 pass + 2 unix-only skips | **reproduced**. The skips are `TestDownFromAFreshProcessSendsQuit` and `TestEnsureWaitsForADaemonStillStarting`. Per package: cmd 12, core 50, net 32, player 21 + 2 skips, resolver 21, store 13, which matches SKILL.md |
| WSL `-race`: 151 pass | **reproduced**: 151 PASS, 0 SKIP, 0 `DATA RACE` |
| smoke 40/40, no mpv left | the row count of 40 is reproduced. **Not run**, as instructed |
| loopback rig: BYPASS before the fix, clean after | the "after" half is **reproduced for daemons the fixed build spawns**. It does not hold for a daemon it reuses (claim 1) |

---

## Steelman

- **Proxy fixes.** libavformat consults exactly `http_proxy` and `no_proxy` and honours only a literal `http://`, so the fix closes every input:
  - normalise the scheme at `Up()`;
  - refuse `https://`;
  - refuse non-`http://` again in `Enqueue`;
  - strip `no_proxy` at spawn.

  Pinning the spawn argv and env in a test means the next "env hygiene" pass cannot quietly undo it.
- **DNS probe.** Asking the resolver a neutral name answers "is a local resolver in a position to see a bypass" without telling it which name the tool protects. It is the least committal of the reviewer's three options.
- **`Ensure()`.** A second mpv on the endpoint can never get the pipe. So the safe move is to wait for one that is already booting, and to replace it only when it stays silent for a generous window.
- **SIGINT deferral.** A signal-time `Net.Down()` races `Up()`, which writes `torCmd`/`torDir` unlocked, and nils `b.cb`, which the main goroutine reads unlocked. Shipping that handler would trade an orphan for a panic.
- **TTY check.** Ask the console or tty driver, as `x/term` does, instead of trusting the character-device bit. That bit is shared by NUL and `/dev/null`.

---

## Contested claims

Ordered by what each costs to fix later, most expensive first.

### 1. "The `no_proxy` bypass is closed: `Ensure()` drops `no_proxy` from mpv's environment"

**Where claimed:** SKILL.md:369-383, INSTRUCTIONS.md:43 and :138, TASKS.md:672, PHASE10.md:141-143.

**Tier:** SCOPE, measured. It is true for a daemon this build spawns, and false for a daemon this build reuses.

**Basis:**
- **Run (rig1 `reuse-legacy`, Windows, 2026-10-03).** I spawned an mpv daemon on a scratch pipe exactly as builds up to `6a70a08` did: the same seven flags, no `cmd.Env`, and the caller's env carrying `no_proxy=*`. Then I ran the real `csnet.Up()` → `player.Ensure()` → `Enqueue()`.
- **Result.** `Ensure()` took the `p.live()` early return (mpv.go:65-66) and reused the old daemon in **45 ms**. The preflight went through the proxy. The origin then received `GET /signed-stream.m3u8?Signature=SECRET [libmpv]` directly. **BYPASS.**
- **Same rig, fresh daemon.** `control`, `HTTP://`, `no_proxy=*`, `NO_PROXY=*` and `no_proxy=127.0.0.1` were all clean, and `https://` was refused at `Up()`. The fix works where it reaches.
- **Where unmarked daemons come from.** T0.1(a) makes the daemon long-lived, so every installed copy is exposed for as long as an older daemon stays up:
  - every build before `9c6a7fd`, including the six `dist/chainsmoke_0.1.0_*` binaries still on disk (2026-08-24, gitignored, RR-4);
  - an mpv the user started on the endpoint;
  - any future spawn path (T10.7, the TUI) that forgets `withoutNoProxy`.
- **The reviewer's own fix asked for this.** It said to note in the gotchas that an mpv daemon started by an older build should be restarted. The commit added nothing of the kind (`grep -rni "older build\|restart"`: no hit outside TASKS.md:582).

**Why it ranks first.** This is a privacy mechanism that is a property of the *spawn*, while the trust decision is made at *reuse* without any check. T10.8's status bar would read `EGRESS: TOR ✓` over this path.

**What would settle it:**
- Make `Ensure()` mark the daemon it spawns with `set_property user-data/chainsmoke/spawn <generation>`. Measured on mpv 0.41: an unset marker answers `property not found`, and a set one reads back.
- On reuse, refuse to queue into a daemon without the marker, or replace it when idle. Also require `get_property ytdl == false` (measured: readable, `false` on our spawn), which closes the reviewer's N-3 hypothesis too.
- Then re-run rig1 `reuse-legacy`. Expect a refusal and `origin saw: []`.
- Interim: delete or rebuild `dist/`.

### 2. T10.9's premise and its planned remedy

**The premise as stated.** PHASE10.md:109-114 and main.go:472-476 say Ctrl+C outside a play session orphans the private tor and its DataDirectory. TASKS.md:633 says "`stop`/Ctrl+C never kill" the mpv daemon. The planned remedy is a signal handler that kills tor under a new mutex, trapping SIGTERM too (PHASE10.md:116-127).

**Tier:** MEASUREMENT for the premise, measured; ALTERNATIVE for the remedy.

**Basis.** Neither the tor half nor the mpv half of the premise had ever been measured; the reviewer could not reproduce it. Both children are spawned with no `SysProcAttr` (mpv.go:108, bridge.go:499), so they share the parent's process group on POSIX and its console on Windows.

**Run on Linux (rig4, WSL).** A stand-in that traps SIGINT, as the play session does, spawned mpv via the real `Ensure()` and a tor exactly as `spawnPrivateTor` does, plus `DisableNetwork 1`. I then sent `kill -INT -- -<pgid>`, which is what the tty driver does on Ctrl+C:

| | stand-in | `Ensure()` mpv | tor | tor DataDirectory |
|---|---|---|---|---|
| group SIGINT (terminal Ctrl+C) | survived (trapped) | **gone** | **exited** ("Interrupt: exiting cleanly.") | **still on disk** |
| group SIGHUP (terminal closed) | died | gone | **survived**: it reloaded config and was re-parented (ppid 1330) | still on disk |

**Run on Windows (rig3).** A process in its own hidden console, with Ctrl+C processing enabled as in a real terminal, raised `GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0)`. The stand-in trapped `os.Interrupt` and survived.
- The daemon spawned by `Ensure()` was **dead 2 s later**.
- The same daemon spawned with `CREATE_NEW_PROCESS_GROUP` survived.

**Run (tor, WSL).** I put `__OwningControllerProcess <pid>` in torrc (with `DisableNetwork 1`) and killed the owner. Tor logged "Owning controller process has vanished -- exiting now" and **exited 12 s later**.

**What this means:**
- Terminal Ctrl+C already kills the private tor on Linux, by process-group delivery. On Windows I infer the same from the console mechanism, but did not run it, because there is no tor.exe here.
- What actually leaks on Ctrl+C is the DataDirectory.
- The genuine orphan cases are terminal close (SIGHUP, measured) and `kill <pid>`, SIGKILL or OOM (inference). SIGHUP is not in PHASE10's plan, and no handler can catch SIGKILL.
- TASKS.md:633 is false on both platforms. Ctrl+C in a play session kills the daemon too.

**Concession.** The race analysis at PHASE10.md:116-125 is correct as read, and deferring the handler was the right call. What I contest is the premise the plan rests on and the remedy it prescribes.

**What would settle it:**
- Add `__OwningControllerProcess <os.Getpid()>` to the torrc in `spawnPrivateTor` (bridge.go:486-492). That covers every death, with no handler and no lock.
- Move the DataDirectory out of `os.MkdirTemp("", …)` (bridge.go:471) to a path `purge` already wipes, or sweep `chainsmoke-tor-*` directories whose owner pid is dead on the next spawn.
- Decide whether the mpv daemon should survive Ctrl+C. If it should (T0.1(a)), spawn it with `Setpgid` on POSIX and `CREATE_NEW_PROCESS_GROUP` on Windows (measured to work on Windows).
- T10.9's handler then only restores the terminal, and the `Down()` race never arises.
- Rewrite TASKS.md:633, PHASE10.md:109-127 and main.go:472-476.

### 3. "`$noTor` keeps Up() from probing through a running tor … it must never leave the box"

**Where claimed:** smoke.ps1:13 and :57-60, the commit's N-5 fix.

**Tier:** SCOPE, measured.

**Basis:**
- `--tor-socks 127.0.0.1:1` defeats only the system-tor probe. `ensureTor` then runs `exec.LookPath("tor")` and spawns a private tor (bridge.go:438-442).
- **Run (Windows, 2026-10-03).** I put a fake `tor.exe` first on PATH; it records its argv and opens no socket. I then ran a scratch build with sandboxed `XDG_*`:
  - `--tor-socks 127.0.0.1:1 --allow-direct play 1` spawned `tor -f %TEMP%\chainsmoke-tor-…\torrc` and took **46 s**. It still printed `no cached result`.
  - `--tor-socks 127.0.0.1:1 play 1` took **45 s**.
- 17 rows carry `$noTor`, so a box with a non-bootstrapping tor on PATH spends about 13 minutes in smoke.
- **Not run, because it leaves the box:** with a real tor on PATH, each row would bootstrap a private tor onto the network, and Gate 4 would CONNECT to `api-v2.soundcloud.com:443` through it. That covers this box's WSL (`/usr/sbin/tor`), Tor Expert Bundle installs on Windows, and pwsh on Linux. The `failclosed*` rows would then pass `Up()` and fail their match.

**What would settle it:**
- An explicitly given `--tor-socks` never spawns a private tor. If nothing listens at the address the user named, fail closed and say so. That is also the less surprising semantics.
- Re-run the fake-tor command. Expect under 1 s and an empty fake-tor log.

### 4. "`startTimeout` 3s → 10s: a cold start measured longer than that on 2026-09-28"

**Where claimed:** mpv.go:55-58 and mpv_test.go:573.

**Tier:** MEASUREMENT.

**Basis:**
- No number or method for that cold start is recorded anywhere in the tree (`grep -rni "cold start"`). The reviewer's L-c says "cold start not reproduced".
- **Measured today.** A warm `Ensure()` takes **86-89 ms** (7 calls across rigs).
- **Cost, measured.**
  - An `mpv` that exits at once with an option error costs `Ensure()` **10.10 s**, ending in the generic "did not become responsive". mpv's own stderr is discarded, and `go func() { _ = cmd.Wait() }()` (mpv.go:121) ignores its exit status.
  - An endpoint that never answers, with a stale mpv already present, costs **20.06 s**: wait, reap, spawn, wait. (My first rig1 run had a bad pipe name and measured exactly this.)
  - Before the commit, both cases took 3 s.
- **Scope, by code read.**
  - The test for the "wait for a booting daemon" half, `TestEnsureWaitsForADaemonStillStarting`, is skipped on Windows (mpv_test.go:580), which is the primary target.
  - On darwin, `daemons()` returns nil (reap_other.go:5-8), so the new wait-then-reap branch never runs and the L-c stray remains.

**What would settle it:** make `waitLive` return as soon as the child exits (a channel closed by the `Wait` goroutine), and report the exit status plus mpv's stderr tail. Keep the 10 s only for a child that is alive and silent. Re-run the fake-mpv case and expect about 0.1 s with mpv's message.

### 5. "`build.ps1` fails any artifact embedding the repo path or %USERPROFILE%"

**Where claimed:** SKILL.md:81-83; build.ps1:50 and :59.

**Tier:** SCOPE, measured.

**Basis:**
- `[Text.Encoding]::ASCII.GetString` turns every non-ASCII byte into `?`, while Go embeds paths as UTF-8.
- **Run (PowerShell).** I took the UTF-8 bytes of `xx C:/Users/Тёма/go/pkg/mod yy`, decoded them as ASCII, and searched:
  - for `C:/Users/Тёма`: `IndexOf` = **-1**;
  - for the ASCII prefix: `IndexOf` = 3.
- So the check is silently blind for any builder whose profile or repo path contains a non-ASCII character. The `Set-Location $PSScriptRoot` at :18 does make `Get-Location` the repo, so that part is fine.

**What would settle it:** decode with `[Text.Encoding]::UTF8`, or search the raw bytes. Prove it once with a build without `-trimpath` into a directory with a non-ASCII name, and expect `FATAL`.

### 6. "`play` checks its argument before `Ensure()`, so a typo no longer spawns a daemon"

**Where claimed:** SKILL.md and main.go:209-212.

**Tier:** COST, by code read. **Not run**, because showing the circuit needs egress.

**Basis:**
- The statement is accurate, but it is half of the reviewer's L-b fix ("call `ResolveIndexOrStdin` first, then `Up()`").
- main.go:202 still runs `Net.Up()` first. So `play 99` on an empty cache either opens Gate 4's Tor CONNECT to `api-v2.soundcloud.com`, or bootstraps a private tor for up to 45 s, and only then fails.
- That contradicts TASKS.md:405 ("a typo never costs a circuit"), which `likes` and `playlist` already follow.
- **Steelman.** `TestEgressVerbsGateOnUp` drives `play 1` on an empty store, and a user should not paste a URL only to learn afterwards that Tor is down.

**What would settle it:** check an integer index against the store before `Up()`, and keep the stdin path after it. Seed the store for the test's `play` row.

### 7. "A resolver that answers one name sees every name, and a blocked soundcloud.com would still have been seen asking"

**Where claimed:** dnsguard.go:39-47 and INSTRUCTIONS.md:128.

**Tier:** INFERENCE. Read, **not run**.

**Basis:**
- **The claim is false in two common setups.**
  - Split DNS routes names to different resolvers: Windows NRPT, systemd-resolved per-link domains, macOS `/etc/resolver`, VPN split tunnels.
  - A hosts-file sinkhole answers `soundcloud.com` without sending any packet.
- **The probe's result is nearly a constant.** `visible` is discarded (core.go:334), so every networked host prints the same line. The probe costs one plaintext query and up to 2 s for close to no information.
- **Two doc lines were left behind.** dnsguard.go:20-25 still describes the check as "CAN this host resolve SoundCloud names at all?". INSTRUCTIONS.md:19 still says no packet leaves except through the bridge, without the `audit` exception the reviewer asked for.

**Concession.** `example.com` is not a SoundCloud leak; the N-4 harm is gone.

**What would settle it:** delete the probe (the previous debate's position). Alternatively, replace it with a zero-packet local inspection that reports hosts-file entries for `soundcloud`/`sndcdn` and the configured resolver. Either way, fix the two doc lines.

### 8. "The error names the scheme only: a `--proxy` URL can carry credentials"

**Where claimed:** mpv.go:323.

**Tier:** SCOPE, measured.

**Basis:**
- **Run (rig1 `https-creds`).** `--proxy https://alice:s3cretpw@…` produced `--proxy https://alice:s3cretpw@127.0.0.1:60615 is a TLS (https://) proxy…` on the terminal.
- The commit's new refusal (bridge.go:955) breaks the rule that the same commit's `Enqueue` follows. The older sites at bridge.go:183, :186 and :966 do the same.
- Low severity: the user typed the credential themselves, but scrollback and logs keep it.

**What would settle it:** use `u.Redacted()` in every `--proxy` error.

### 9. "Still unmeasured: a real multi-segment HLS stream through the real bridge, which needs live egress"

**Where claimed:** PHASE10.md:143-144 and TASKS.md:675-676.

**Tier:** ALTERNATIVE. It does not need egress.

**Basis.** rig6 (Windows) used a 6-segment AAC HLS fixture generated with ffmpeg, a loopback origin, and a forwarding proxy that tags what it relays. The daemon was spawned before `Up()`, so its env held no `http_proxy`, the same as after `daemon start`.
- **Proxy alive.** The playlist and all six segments went through the proxy via the per-`loadfile` option. **0 direct.**
- **Proxy killed after `seg1`.** No later segment reached the origin by any route, and `time-pos` stayed 0. **0 direct.**

So the docs' inference ("a fresh segment dial … meets the same decision") **held** for plain HTTP. What remains is HTTPS segments over CONNECT through the real `ConnectBridge`, which needs a loopback TLS origin and a loopback SOCKS5 stand-in, not the internet.

**What would settle it:** promote rig6 to a `-tags integration` test, and add the TLS variant.

---

## Doc corrections in `9c6a7fd`, checked against the code

| doc line (as corrected) | verdict | wrong side |
|---|---|---|
| SKILL.md:6-9: only `pkg/scrub` is a stub | correct (grep `ErrNotImplemented`) | — |
| SKILL.md test counts 149 / per package; WSL `-race` green | correct, re-measured | — |
| SKILL.md "1 in 3" → n=6, one success | correct (ytdlp.go:29-31) | — |
| SKILL.md: `play` checks its argument before `Ensure()`; `np-none` | correct as written; `Up()` still runs first (#6) | code (half-fix) |
| SKILL.md: "Terminal" means a real console/tty | correct: NUL, `/dev/null` and pipes are false; a Linux pty is true (measured) | — |
| SKILL.md: `Clean` covers the ▶ line and `np` | correct for C0 and DEL; C1 passes (residual) | — |
| SKILL.md:369-383 / INSTRUCTIONS.md:43 and :138 / TASKS.md:672 / PHASE10.md:141-143: bypasses "closed" | **over-claims**: a reused daemon still bypasses (#1) | doc and code |
| SKILL.md:81-83: `build.ps1` checks paths | **over-claims** for non-ASCII paths (#5) | code |
| INSTRUCTIONS.md:128: neutral probe "answers the same question" | **over-claims** (#7) | doc |
| INSTRUCTIONS.md:138: per-load measured; propagates to HLS segments | **now true for the per-load form too** (rig6; until today only the CLI form had been measured) | — |
| INSTRUCTIONS.md:84/:89/:91, TASKS T10.1/T10.2/T10.10, PHASE10 B/F: banner rule | matches the code (`TestBannerClassification` PASS) | — |
| PHASE10.md:109-114 and main.go:472-476: Ctrl+C orphans the private tor | **wrong mechanism**: tor exits on terminal Ctrl+C; it is orphaned on SIGHUP and SIGTERM, and the DataDirectory always leaks (#2) | doc |
| PHASE10.md:116-125: the `Down()` race analysis | correct as read | — |
| TASKS.md:633 (not touched by the commit): "Ctrl+C never kill it" | **false** on both OSes, measured (#2) | doc |
| PHASE10.md:143-144 / TASKS.md:675-676: "needs live egress" | **false** (#9) | doc |
| mpv.go:29-32 (no longer "foreground") and mpv.go:100-104 (`--no-ytdl` comment) | correct | — |
| mpv.go:55-58: "cold start measured longer" | unrecorded measurement (#4) | doc |
| dnsguard.go:20-25 (not touched) | stale: still describes a SoundCloud-name check | doc |
| smoke.ps1:13 and :57-60: never leaves the box | **over-claims** when tor is on PATH (#3) | code |
| PHASE10 Part 2: `ultraviolet` pseudo-version recorded | present; not re-measured today | — |

---

## Unchallenged

I attacked these and could not break them.

- **The scheme and `no_proxy` fixes, for every daemon this build spawns.** rig1 was clean for `control`, `HTTP://`, `no_proxy=*`, `NO_PROXY=*` and `no_proxy=127.0.0.1`, and `https://` was refused at `Up()`. Normalisation stores `http://`, and `checkProxyScheme` rejects an opaque `http:host:port` because it requires `u.Host`.
- **"Does any other env var defeat the per-load proxy?" No.** FFmpeg master `libavformat/http.c` and `tls.c` (read 2026-10-03) consult exactly two variables, `getenv_utf8("http_proxy")` and `getenv_utf8("no_proxy")`. The latter is case-insensitive on Windows, so `withoutNoProxy`'s `EqualFold` is the right filter.
- **Per-load propagation to HLS segments, and fail-closed when the proxy dies mid-stream** (rig6, plain HTTP). This was the inference I most expected to break.
- **`--no-config` is not load-bearing for `--no-ytdl` in the configs I tried** (rig7). With config loading on, both `ytdl=yes` in mpv.conf and a `[protocol.http]` auto-profile (which mpv applied, per its log) left the ytdl hook dormant against a refusing proxy, and the origin saw nothing. So the test pinning only `--no-ytdl` is defensible. I tested only those two configs, not user scripts.
- **`isTerminal`:**
  - Windows: NUL, pipes and files are false. The old check called NUL a terminal (`Dcrw-rw-rw-` → true); the new one says false.
  - Linux: `/dev/null` and pipes are false, and a pty is true.
  - WSL interop (a Windows exe under a Linux pty) is false under **both** the old and new checks, because the handles are pipes. So it is a scope limit, not a regression, and the right answer for a future TUI guard.
  - mintty without ConPTY is the same case: pipes under both, so no regression.
  - The Windows real-console positive case is **not measured here**: my hidden-console launcher hung twice. I accept it on the strength of `golang.org/x/term`'s identical `GetConsoleMode` test.
- **Can `Ensure`'s wait-then-reap kill an mpv the user cares about? Rarely, and only a wedged one.** `daemons()` is an exact-token argv match on both Linux and Windows, so a user's mpv on another endpoint is spared (`TestDownKillsStrays` plus code). A live owner never reaches the reap: `live()` true returns at mpv.go:65. On Linux, an owner that stays unresponsive past the 10 s read deadline gets killed. On Windows it hangs instead, because there is no deadline on a named pipe; that is pre-existing.
- **Deferring the SIGINT handler**, and the race analysis behind it.
- **`Clean` at the ▶ line, `np` and `daemon status`** (main.go:430 goes through `NowPlaying`), with the C1 caveat below.
- **The commit's verification numbers**, all reproduced (table above).

---

## Stronger alternatives

1. **A daemon spawn marker (fixes #1).** Do `set_property user-data/chainsmoke/spawn <gen>` after spawn, and on reuse require the marker plus `ytdl == false`. Refuse an unmarked daemon, or replace it when idle.
   - *Cost:* about 20 lines and one test; mpv ≥0.36, measured on 0.41.
   - *Status quo:* every older or foreign daemon keeps the bypass for as long as it lives, and T10.7's second client inherits the blind spot.
2. **`__OwningControllerProcess` plus a DataDirectory in a purge-able place (replaces T10.9's tor half).**
   - *Cost:* one torrc line and about 10 lines for the directory.
   - *What it covers:* SIGHUP, SIGTERM, SIGKILL, crashes and OOM, none of which the planned handler can fully cover. It needs no mutex.
   - *Side effect worth weighing:* a DataDirectory that persists for the session keeps one entry guard instead of choosing a new one per command. That is Tor's own guard model, and it cuts bootstrap time (the previous debate's claim 8).
   - *Caveat:* under `--private` the DataDirectory must stay out of the store directories (residual risk).
   - *Status quo:* a handler built on a false premise, plus a mutex threaded through `ensureTor`.
3. **Give the mpv spawn its own process group**, if T0.1(a)'s "the daemon outlives" is still the intent. Use `Setpgid`, or `CREATE_NEW_PROCESS_GROUP` (measured to survive Ctrl+C).
   - *Cost:* about 6 lines, one per OS file.
   - *Status quo:* Ctrl+C in a play session kills the daemon on both OSes. The docs say otherwise.
4. **An explicit `--tor-socks` never spawns a private tor (fixes #3).** smoke becomes offline by construction, and the user's named address is honoured.
   - *Cost:* about 5 lines.
5. **Fail fast on mpv exit in `waitLive`**, and surface mpv's stderr (#4).
   - *Cost:* about 15 lines.
6. **Make rig6 an integration test, then add the TLS/CONNECT variant** (#9). It is the "sound + no leak" test the previous debate asked for (claim 6), and it runs offline.

---

## Residual risk

This covers what nobody has looked at, including me. Lines marked *speculation* are speculation.

- **C1 controls.** `Clean` passes U+0080-U+009F (table.go:256), and `TestNowPlayingScrubsTheTitle` asserts only `r < 0x20`. *Speculation:* U+009B can act as CSI in some UTF-8 terminals, and the commit has now routed `Clean` to two more terminal sites.
- **HTTPS segments over CONNECT through the real `ConnectBridge`.** Unmeasured, but measurable offline (#9).
- **Windows Ctrl+C after the spawner has exited.** *Inference:* a daemon stays attached to the console that spawned it, so any later Ctrl+C in that window may kill it. Unmeasured.
- **tor.exe on CTRL_C and CTRL_CLOSE.** *Inference:* it dies, as mpv did. There was no tor.exe here to test.
- **The private tor's DataDirectory under `%TEMP%`.** It is written even under `--private`, and it holds guard state (bridge.go:471). That is persisted bytes outside the Store, which is invariant #1 and #4 territory, and pre-existing.
- **A wedged owner hangs `Ensure()` forever on Windows.** `live()` has no read deadline on a named pipe (mpv.go:280-289). This is pre-existing; the new wait branch neither causes nor cures it.
- *Speculation:* a probe name only this tool resolves, right before a Tor connection, is a weak fingerprint of `chainsmoke audit`.

---

## Summary

Three objections should change what the dev does next.

1. **The `no_proxy` fix does not reach daemons the fixed build reuses** (measured BYPASS, #1). Add the spawn marker and the reuse check now. It is about 20 lines, before T10.7 adds a second spawn/reuse path and T10.8 paints `TOR ✓` over it. Delete or rebuild `dist/` today.
2. **Rewrite T10.9 before step 3** (#2, measured on both OSes). Ctrl+C already kills the private tor and the mpv daemon. The real orphans come from SIGHUP and from kill, SIGTERM or OOM, which the planned handler misses or cannot catch. Use `__OwningControllerProcess` plus a purge-able DataDirectory, and give mpv its own process group. The handler then only restores the terminal, and the race the commit documented never has to be solved.
3. **Smoke is offline only on boxes without `tor` on PATH** (#3, measured). An explicit `--tor-socks` should never spawn a private tor.

The rest (#4-#9) are cheap, mostly docs or tests, and none blocks.

**Verdict: GO for PHASE10 step 1, the A-refactor.** The commit is a real improvement: every fix holds where it reaches, and every number it reports reproduced. #1 should land before T10.7, #2 must reshape step 3 before it is built, and #3 should land before the next smoke-heavy change. **NO-GO only on building T10.9 as currently written in PHASE10.md Part D.**

---

## Appendix: rigs

All are scratch Go modules with `replace chainsmoke => D:/sccli`, run 2026-10-03, and loopback-only.

- **rig1 (proxy bypass).**
  - Two loopback servers. The origin logs the request line and User-Agent and answers 404. The fake proxy logs the same; it answers `CONNECT` with 200 and closes without dialling out, and answers an absolute-form GET with a 1-byte 206.
  - Flow: `csnet.New(scheme+proxy, false, "127.0.0.1:1").Up()` → `player.New(scratchPipe, b).Ensure()` → `Enqueue("http://"+origin+"/signed-stream.m3u8?Signature=SECRET", …)`, then a 5 s wait, then compare the logs.
  - `reuse-legacy` first starts `mpv --idle --no-video --no-config --no-ytdl --gapless-audio --cache-on-disk=no --input-ipc-server=<pipe>` with `Env = os.Environ()+"no_proxy=*"`.
  - Build pipe names with `string(rune(92))`: this tool's heredoc collapses `\\`. My first run had a bad pipe name, which is how the 10.05 s and 20.06 s figures were measured.
- **rig2.** `user-data` and `ytdl` readback on a fresh daemon, plus `Ensure()` against a fake `mpv.exe` that exits 1 (PATH in `/c/...` form, not `C:/...`).
- **rig3 (Windows Ctrl+C).** A child in a hidden new console calls `SetConsoleCtrlHandler(NULL, FALSE)`, because this sandbox inherits Ctrl+C off and a real terminal does not. It then does `signal.Notify(os.Interrupt)`, spawns via `Ensure()` (or with `CREATE_NEW_PROCESS_GROUP`), calls `GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0)`, and probes IPC 2 s later.
- **rig4 (WSL).** `setsid ./rig4 <tmp>` traps SIGINT, then runs `Ensure()` and `tor -f torrc` (`DisableNetwork 1`, as `spawnPrivateTor` does otherwise). Then `kill -INT|-HUP -- -<pgid>`, and read `/proc/<pid>/stat`.
- **`__OwningControllerProcess`.** `sleep 300 & OWNER=$!`, a torrc with `DisableNetwork 1` and `__OwningControllerProcess $OWNER`, then `kill $OWNER` and time tor's exit.
- **rig5.** Prints `core.StdinIsTerminal()`/`StdoutIsTerminal()` and the old `ModeCharDevice` answer, under NUL, pipes, files, `script -qc` (pty), and WSL interop.
- **rig6 (HLS).** WSL `ffmpeg -f lavfi -i sine=d=10 -c:a aac -f hls -hls_time 2` produced 6 segments. A Go file-server origin logs `X-Via`, and a forwarding proxy adds `X-Via: proxy`. The daemon is spawned before `Up()`; then mute, `Enqueue`. Case B closes the proxy server after `seg0`, with 1.5 s per segment on the origin.
- **rig7.** `mpv --config-dir=<scratch> [--no-config] --no-ytdl --stream-lavf-o=http_proxy=<502 proxy> http://<origin>/signed.mp3?…`, run with three configs, then grep the log for `ytdl_hook`.
- **Fake tor.** A `tor.exe` that appends its argv to `$FAKETOR_LOG` and sleeps, placed first on PATH, with the scratch `chainsmoke.exe` run under sandboxed `XDG_CONFIG_HOME`/`XDG_RUNTIME_DIR`/`XDG_STATE_HOME`.

Sources checked 2026-10-03:
- [FFmpeg libavformat/http.c (master)](https://raw.githubusercontent.com/FFmpeg/FFmpeg/master/libavformat/http.c)
- [FFmpeg libavformat/tls.c (master)](https://raw.githubusercontent.com/FFmpeg/FFmpeg/master/libavformat/tls.c)

# debaters-output: is privacy hardening worth what it costs?

**Date of debate and phase:** 2026-10-01 — `phase10`

**Target:** the dev's question of 2026-09-09, which was never answered because that run was interrupted. Read alongside it:
- `ch-writings/report-review/CH-2026-09-09-phase10.md` (Reviewer)
- `ch-writings/report-debate/CH-DEBATE-2026-09-09-phase10.md` (previous debate)
- the tree at `6a70a08`, read in an isolated worktree. The main agent is editing the main tree in parallel, so line numbers may have moved there.

> "can you prove me right or prove me wrong on whether or not adding a privacy hardening to a soundcloud daemon is a bad idea and realising it would require much more stuff to build? maybe i should withdraw some of the functions we've built and simplify the code, possibly leaning a LOT more towards usability, simplicity, speed and TUI building."

Everything marked *measured* was run on this box today, loopback only: no Tor, no egress, no smoke.ps1, no `-tags integration`, and nothing on `\\.\pipe\chainsmoke-mpv`. Everything marked *read* comes from the code and was not run.

---

## The answer, in five lines

1. **You are right about the cost.** Of 17 defects I can date since Phase 5, 10 came from the privacy machinery. That includes the two worst: five weeks in which `play` never got past opening a track, and the 2026-09-05 change that sent audio around Tor.
2. **You are wrong that the cure is to withdraw privacy.** The part that protects anyone (chokepoint, in-process bridge, fail-closed `Up()`, per-track proxy, no URLs in argv) is about 1,000 lines, has zero dependencies, and is the only thing that separates chainsmoke from the mpv + yt-dlp TUIs that already play SoundCloud.
3. **The cost sits in the periphery**: the DNS audit, saved-config drift, NEWNYM `rotate`, the private Tor, and the `get`/scrub stub. That is about 1,070 lines of code plus 980 of tests. Some of it does harm: the DNS check sends the very lookup it is supposed to catch, and one-shot `rotate` reports work it never did.
4. **Most of the waiting is not privacy.** yt-dlp takes 1.3 s just to start (measured), and `play` starts two or three of them. `--no-cache-dir` makes each one re-scrape SoundCloud's client_id, and every command draws a fresh Tor exit.
5. **Verdict: right about the cost, wrong about the cure.** Keep the core, cut the periphery, fix the two leaks measured today, then build the TUI and let it own the bridge. That ends the worst bug class the privacy design produced.

---

## Steelman

**Your case.** Privacy work is 43% of the production code (measured below). It has caused most of the bugs since Phase 5, including weeks of a player that did not play and a stretch where it leaked. Each command costs tens of seconds over Tor, while the threat (SoundCloud or the ISP learning what you stream) is modest for most listeners and the TUI is what people actually see. So freeze the machinery and ship a fast, simple TUI.

**The project's case.** Privacy over Tor is the premise: the product is "Privacy-Focused" (INSTRUCTIONS.md:1), nothing may leave without a verified circuit (INSTRUCTIONS.md:19), and four invariants are called "a rewrite" to retrofit (main.go:20-27). The expensive, risky part is already built and stable: the bridge, the fail-closed contract, and the `http_proxy` spelling trap. Without it, chainsmoke is a thinner copy of tools that already exist.

---

## The bill, measured

### Lines of code per mechanism

Counted at `6a70a08` with `sed -n A,Bp file | wc -l`, then again with comments and blank lines stripped. Production code is 5,125 lines in total and tests are 4,670.

| mechanism | where | lines / code-only | tests |
|---|---|---|---|
| **Core egress**: Bridge Up/Assert/Down, ConnectBridge, SOCKS5 dialer, helpers | bridge.go:35-277, 385-415, 518-998 | **753 / 512** | bridge_test 447, proxyenv_test 98 |
| Per-track proxy + preflight | mpv.go:284-294, 311-380 | 81 / 42 | in mpv_test |
| yt-dlp `minimalEnv` / `--proxy` | ytdlp.go:308-342 | 35 / 21 | in ytdlp_test |
| URL-in-argv refusals, `cleanHandle` | core.go:153-212, browse.go:95-148 | 114 / 74 | in core/browse tests |
| **Core total** | | **983 / 649** | |
| Private-Tor fallback | bridge.go:417-516 | 100 / 68 | — |
| `rotate`: NEWNYM, ControlPort client, cooldown stamp, re-isolation | bridge.go:280-383, 699-732; control.go | 325 / 224 | control_test 247 |
| Egress ledger | ledger.go; core.go:384-431, 601-674 | 200 / 122 | ledger_test 244 |
| Local-DNS audit | dnsguard.go (+ ~13 lines of glue) | ~96 / ~40 | **none** |
| `audit` + `--save` + drift config | config.go; core.go:286-382; main.go:350-412 | 306 / 189 | config_test 173, audit_test 317 |
| `get` / scrub / `--nuclear` | scrub/ffmpeg.go; main.go:235-265 | 58 / 37 | smoke only |
| **Periphery total** | | **~1,070 / ~680** | **~980** |
| `play` session hold (exists *because* of the in-process bridge) | session.go | 133 / 91 | session_test 164 |
| (not privacy) reaping stray mpv processes | reap_*.go, mpv.go:428-501 | 188 / 129 | in mpv_test |

### Dependencies and subprocesses

- **Privacy added zero modules.** `go.mod` lists `urfave/cli/v2` and three indirect modules, and `pkg/net` uses only the standard library. The TUI is what will break the one-dependency policy (INSTRUCTIONS.md:74): 8 linked modules with tview or 19 with Bubble Tea v2, per PHASE10.md Part 2 (2026-09-09, not re-measured today).
- **Privacy added one optional subprocess**, a private `tor`, used only when no system Tor is listening. mpv and yt-dlp would exist without any privacy at all.

### Defects since Phase 5, attributed

| when | defect | caused by privacy machinery? | source |
|---|---|---|---|
| 08-22 | HEAD preflight got 405 from the CDN and skipped healthy tracks | no (method choice) | mpv.go:325-330 |
| 08-22 | `np` printed the signed URL | no: privacy work *caught* it | mpv.go:276-279 |
| 08-23 | 5 of 6 proxy spellings fail open when the proxy dies | caught before ship by privacy work | TASKS.md T7.3 |
| 08-24 | a rejected `--impersonate` told users to run `yt-dlp -U` | **yes** (impersonation) | core.go:676-682 |
| 08-24 | a mistyped handle advised `yt-dlp -U` | no | core.go:684-692 |
| 08-24 | playlist sets had no titles | no | ytdlp.go:553-569 |
| ~08-24 → 09-28 | **`play` never got past opening a track**: the bridge died with the process | **yes** (in-process bridge vs. short-lived process) | TASKS.md:629-632, session.go:20-27 |
| 08-24 | a reused daemon pointed at a dead proxy port and stayed silent | **yes** (ephemeral bridge port) | SKILL.md at `4b64cca` |
| 09-05 → fixed | **`cmd.Env = PATH` sent all audio direct** | **yes** (an env-hygiene hardening pass) | `e2f2e01`, both 09-09 reports |
| 09-28 | `daemon stop` / `purge` did nothing | no (lazy dial) | mpv.go:446-453 |
| 09-28 | a second mpv on the pipe was unreachable | no (daemon model) | mpv.go:440-444 |
| 09-28 | repeated `Up()` inside a session orphaned bridges and Tor processes | **yes** (session.go, a consequence of row 7) | bridge.go:165-171 |
| open | Ctrl+C orphans a private Tor | **yes** | main.go:470-471 |
| open | bridge `Close()` blocks while a stream is live | **yes** | bridge.go:671-675 |
| today | `--proxy HTTP://…` and `no_proxy` send mpv direct | **yes** | contested claim 3 |
| today | `audit` resolves soundcloud.com in plaintext | **yes** | contested claim 4 |
| today | one-shot `rotate` reports a re-isolation that has no effect | **yes** | contested claim 5 |

**10 of 17 were caused by the privacy machinery, 5 were not, and 2 were leaks that privacy work caught.** One inference from the code at `e2f2e01` deserves to be said plainly. That commit had no per-track proxy and mpv's environment was `PATH` only. So from that commit until the fix, the committed code could produce sound only by going direct. Fail-closed made "broken" look like "safe", and then a leak made "broken" look like "fixed".

### Where the waiting comes from

| source | privacy? | cost | basis |
|---|---|---|---|
| yt-dlp process start | **no** | **1.30 / 1.37 / 1.35 s** each, offline. `play N` spawns 2 (one track) or 3 (a set) | *measured*: `time yt-dlp.exe --version` ×3 |
| `Meta` on a single track, then `Stream` on the same track | **no** | one redundant yt-dlp process per `play N` | *read*: main.go:212 + core.go:252 |
| `--no-cache-dir`, so client_id is re-scraped on every call | **a privacy choice** | at least two extra requests per call over Tor (soundcloud.com, then JS assets until the id appears) | *read*: ytdlp.go:277. yt-dlp master `_initialize_pre_login` → `_update_client_id`, fetched from GitHub 2026-10-01. Magnitude **not measured** |
| fresh SOCKS credentials per process, so each command gets a new circuit | **a privacy choice** | every command re-rolls the "which exit did I get" lottery, with 15 s socket timeout × 3 attempts | *read*: bridge.go:550-553, ytdlp.go:29-43 |
| private Tor fallback | **privacy** | full bootstrap from an empty temp DataDirectory **on every command**, budget 45 s | *read*: bridge.go:45, 466, 481-487, 271-276 |
| preflight ranged GET | usability (skips dead tracks), routed over Tor | one Tor request per track, at most 10 s | *read*: mpv.go:252, 311-351 |
| `Up()`'s verify CONNECT | privacy (egress contract) | ~one stream, and it pre-warms the circuit yt-dlp reuses | *read*: bridge.go:217-226 |
| Tor itself | inherent | 2026-08-19: the one success took 22.9 s, failures stalled at ~17 s | ytdlp.go:29-31 |

---

## Contested claims

Ordered by what each costs to fix later, most expensive first.

### 1. "Privacy hardening costs more than it returns, so withdraw it"
**INFERENCE**, against the dev's premise.
**Basis.** The bill above splits in two. The **core** (983 lines, 0 dependencies) has been stable since 09-28. Its two remaining holes (claim 3) are a few lines each, and it is invariants #2/#3 and the egress contract (INSTRUCTIONS.md:19) in code. The **periphery** (~1,070 lines + ~980 test lines) produced the self-inflicted DNS lookup (claim 4), the no-op `rotate` (claim 5), a config file that configures nothing (`audit --save` only feeds a diff, config.go:13-24), and a per-command Tor bootstrap (claim 8). Withdrawing the core changes what the product is. Withdrawing the periphery changes nothing a user sees except speed and honesty. Meanwhile **Yututui** (Rust, plays SoundCloud among seven catalogs, v1.7.7 on 2026-09-30) advertises no Tor, SOCKS or proxy support (awesometui.com/yututui, checked 2026-10-01). Remove the core and chainsmoke enters that field with fewer features.
**What would settle it:** the cut list below. Nothing in it touches an invariant.

### 2. T0.1's "mpv is the only daemon, invocations own no state" has aged, despite its verdict "approved, fixed"
**INFERENCE.** This argues against a dev's-conclusion verdict, explicitly.
**Basis.** T0.1 (TASKS.md:13-45) rejected option (c), "the TUI is the session owner", and bound everyone to "no in-process state survives an invocation". But the in-process bridge, decided a day earlier (2026-08-15, INSTRUCTIONS.md:48), *is* in-process state that every playing track depends on. The code settled the clash quietly: `play` now holds the bridge as a session (session.go:20-27, SKILL.md:340). The docs did not: mpv.go:1-4 still says "chainsmoke invocations are short-lived and own no state between runs", and core.go:233-237 says the same. If nobody rewrites T0.1 before the TUI exists, the TUI gets built against a process model the code no longer follows, and one-shot `play` and the TUI each own a bridge whose tracks die when its owner exits.
**What would settle it:** one dated line under T0.1: *"the front-end that queues a track owns its egress path while it plays; in the TUI, that is the TUI process."* Then delete the two stale sentences. This **helps your side**: the TUI is the natural home for the bridge, so leaning into the TUI and keeping privacy point the same way.

### 3. Two fail-open paths in mpv's proxy handling, measured today
**MEASUREMENT** (run, loopback only, 2/2 each).
**Basis.** A scratch rig drives the **real** `csnet.Up()` and `player.Ensure()/Enqueue()`. It runs a fake origin and a fake CONNECT proxy on 127.0.0.1 and a scratch mpv on `\\.\pipe\debater-scratch-<pid>` (rig in the appendix).

```
--proxy http://…   (control) Up ok; proxy saw: preflight GET [Go-http-client] + 10x GET [libmpv]; origin saw: []
--proxy HTTP://…            Up ok; proxy saw: preflight GET [Go-http-client];  origin saw: GET /signed-stream.m3u8?Signature=SECRET [libmpv]  -> BYPASS
http://… + no_proxy=127.0.0.1                     origin saw: GET /signed-stream.m3u8?Signature=SECRET [libmpv]  -> BYPASS
http://… + no_proxy=*                             origin saw: GET /signed-stream.m3u8?Signature=SECRET [libmpv]  -> BYPASS
```

These are two mechanisms. (a) `checkProxyScheme` lowercases the scheme before it validates it (bridge.go:943), but stores the raw string (bridge.go:189). libavformat uses the proxy only if the value starts with exactly `http://`. The mpv manual says it outright: *"silently ignored if it does not start with http://"* (mpv.io/manual, checked 2026-10-01). (b) mpv inherits the user's environment (no `cmd.Env` since `b375de1`), and the same manual says *"the no_proxy environment variable is still respected"*. Mechanism (b) **also applies to the built-in bridge**, because its URL has the same `http://127.0.0.1:<port>` shape. On Windows, environment variable names are case-insensitive, so `NO_PROXY=*` is the same variable (not run separately). Both have the shape the 09-09 debate called a correlation leak: resolve and preflight go over Tor, then the same signed URL is fetched from the real IP seconds later. yt-dlp (`minimalEnv`) and the Go preflight (`http.ProxyURL`) are immune. Only mpv is exposed.
**Reachability:** (a) needs someone to type an upper-case scheme. (b) needs `no_proxy=*` or a pattern that matches the CDN host. Both are narrow, both are silent, and both would sit under a TUI status bar reading `EGRESS: TOR ✓`.
**What would settle it:**
- Store `u.String()` (normalised) after `checkProxyScheme`.
- Remove `no_proxy`/`NO_PROXY` from mpv's spawn environment in `Ensure()`.
- In `Enqueue`, refuse any per-track proxy value that does not start with exactly `http://`.
- Re-run the rig. Expect `origin saw: []` in all four rows.

### 4. The DNS self-test sends the plaintext lookup it was built to catch
**INFERENCE**, read and **not run**. Running it would send a real DNS query off the box, and WSL, where a namespace rig would be safe, is blocked for this worktree agent.
**Basis.**
- `CheckLocalDNSVisibility` calls `net.DefaultResolver.LookupHost(ctx, "soundcloud.com")` and then the same for `"sndcdn.com"` (dnsguard.go:58, :40).
- `Audit()` reaches it every time (core.go:340-346 via bridge.go:1003-1005), including when egress is DOWN (SKILL.md:252).
- smoke.ps1 runs `audit` six times per run (smoke.ps1:154-168, with `--tor-socks 127.0.0.1:1` at :57).
- INSTRUCTIONS.md:128 sets the project's own bar: *"any observed local DNS query for a `*.sndcdn.com`/`soundcloud.com` host = fail the build"*. By that bar the self-test fails itself.
- The answer it reports ("resolves locally") is true on essentially every networked machine, so it carries almost no information. On a careful user's machine it may be the *only* thing that ever tells their ISP's resolver "this person uses SoundCloud". This also contests the SKILL gotcha that lists DNS among audit's honest probes.

**What would settle it:** in WSL as root, run `unshare -n -m`, bind-mount a `resolv.conf` containing `nameserver 127.0.0.1`, and start a UDP listener on :53. Then run `PATH=/nonexistent ./chainsmoke --tor-socks 127.0.0.1:1 audit` and look for `soundcloud.com` in the listener's log. The fix is to delete `AuditLocalDNS`. The real guarantee is `TestConnectRequestNeverResolvesHostnames`, which stays.

### 5. One-shot `rotate` is a no-op that reports success
**INFERENCE**, read.
**Basis.**
- `rotate` calls `Up()`, which builds a bridge with fresh random SOCKS credentials (main.go:338-349, bridge.go:550-553).
- `Reisolate()` then re-rolls them.
- With no ControlPort (this box), it prints *"re-isolated instead — the next stream opens a fresh circuit"* (bridge.go:313-315).
- `After` then runs `Down()` and closes that bridge, so there is no next stream in this process.
- Every *next* process already starts with fresh random credentials, so the verb changes nothing. NEWNYM (control.go, 187 lines, plus a cooldown stamp) acts only on a system Tor with ControlPort, and there it resets circuits for **every other app on that Tor**.
- Rotate's own doc says "no caller claims a fresh circuit it did not get" (bridge.go:568-569). In one-shot mode it does exactly that. This contests the SKILL.md:227 gotcha.

**What would settle it:** reading suffices. The fix is to keep `Reisolate` (34 lines, used by the retry policy and meaningful at the session prompt and in the TUI). Cut NEWNYM, control.go and the stamp, and make one-shot `rotate` say "every chainsmoke command already starts on a fresh circuit".

### 6. The suite proves silence, never sound
**INFERENCE**, read plus grep.
**Basis.** 141 top-level tests (140 run on Windows, matching SKILL), and **none** asserts that `time-pos` advances (`grep -rn "time-pos" --include=*_test.go` finds nothing). The player integration test only checks `idle-active` (pkg/player/integration_test.go:66-74). Phases 7, 8 and 9 were marked COMPLETED while `play` made no sound (TASKS.md:629-632). The first dated "verified live" playback is 2026-09-28 (`git log -S "verified live 2026"` → `6a70a08`). In a fail-closed tool, "nothing plays" and "nothing leaks" look identical from outside, and this suite only tests the second.
**What would settle it:** one integration test with a loopback HTTPS origin, a loopback SOCKS5 stand-in for Tor, the real bridge, and real mpv on a scratch pipe. Assert **both** `time-pos > 0` and "origin saw zero direct connections". It would have caught the five silent weeks on day one, and the 09-05 leak too. rig1 shows the fixture style works on Windows.

### 7. "Privacy is what makes it slow"
**MEASUREMENT**, partly run.
**Basis.** See the waiting table. Measured: yt-dlp start is 1.3 s × 2-3 per `play` before any packet. Read: `--no-cache-dir` forces a client_id scrape on every call, a redundant `Meta` call, and a fresh exit per command. Only the last two rows (Tor itself, `Up()`) are privacy costs you cannot design away while on Tor.
**What would settle it** (needs Tor, **not run**):
```
yt-dlp -v --no-cache-dir --proxy <bridge> -g <permalink> 2>&1 | grep -cE "Downloading (main page|JS asset)"
```
Time that, then time the same command twice with `--cache-dir "$env:TEMP\cs-ytdlp"` and compare the second run.

### 8. The private-Tor fallback bootstraps Tor from scratch on every command
**COST**, read.
**Basis.** Each one-shot command spawns Tor with a fresh temp `DataDirectory` (bridge.go:466, 481-487), and `After → Down()` deletes it (bridge.go:271-276). The next command therefore starts with no cached consensus, inside a 45 s budget (bridge.go:45). It also brings its own bug class: the Ctrl+C orphan (main.go:470-471, PHASE10.md Part D). In a TUI it would bootstrap once per session, which is tolerable.
**What would settle it** (needs egress, **not run**): on a box with `tor` on PATH and no system Tor, time `chainsmoke search x` twice.

### 9. "Roughly 1 in 3 fresh circuits" works (SKILL.md:225)
**MEASUREMENT.**
**Basis.** The same 2026-08-19 run is recorded in ytdlp.go:29-31 as *"over 6 fresh circuits, the one that worked"*, which is closer to 1 in 6. Two docs give two numbers for one sample of n=6, taken on one box on one day, and every timeout and the retry count are tuned on it.
**What would settle it:** 30 `search` runs with fresh credentials, spread over a day, logging outcome and duration. Not run (egress).

### 10. `--private` writes nothing
**INFERENCE**, read.
**Basis.** True of the Store (SKILL.md:296), but not of the process. `--private audit --save` writes `config.json`, because main.go:399 has no private check. `--private rotate` against a ControlPort writes `RuntimeDir()/last-newnym` (bridge.go:373-383), which is a store directory written outside the Store, against invariant #1. Both are small. The second disappears if NEWNYM is cut.
**What would settle it:** a wiring test that runs `--private --allow-direct audit --save` in a sandboxed `XDG_CONFIG_HOME` and asserts the directory stays empty.

### 11. Two stale mpv facts
**MEASUREMENT**, run.
**Basis.**
- (a) INSTRUCTIONS.md:43 says *"mpv has no `--http-proxy` option"*. The shipped mpv 0.41.0-244 lists `--http-proxy String (default: )` (`mpv.com --no-config --list-options`).
- (b) mpv.go:79-82 says the ytdl hook *"does not fire for direct media URLs"*. It fires on load **failure**. Without `--no-ytdl`, a failed load ran `ytdl_hook/on_load_fail`, which spawned `yt-dlp … -- http://127.0.0.1:…/x.mp3` with no proxy, and the origin got a direct connection (rig2, mpv log).

`--no-ytdl` is therefore more load-bearing than the comment admits. Ensure() passes it, so the shipped path is safe.
**What would settle it:** fix both sentences, and add a unit test that `Ensure()`'s argv contains `--no-ytdl`.

---

## The cut list

Each row is a function, a verdict, what it saves, what you lose, and why.

| function | verdict | saves | user loses | why |
|---|---|---|---|---|
| Egress chokepoint + in-process bridge + fail-closed `Up()` | **KEEP** | — | — | invariant #2 + egress contract. Fix claim 3 |
| Per-track proxy (`stream-lavf-o`) + preflight | **KEEP** | — | — | held in 15/15 error cases today (Unchallenged) |
| `play` session hold | **KEEP until the TUI replaces it**, and skip it when `--proxy` is external | a hold users don't need with an external proxy | — | an external proxy outlives the process |
| Private-Tor fallback | **SIMPLIFY → TUI only**, or **CUT** | 100 lines, the Ctrl+C orphan, a 10-45 s bootstrap per command | zero-config on boxes with `tor` installed but not running | claim 8. The "start tor" hint already exists (bridge.go:239-241) |
| `rotate`: NEWNYM + ControlPort client + stamp | **CUT** (keep `Reisolate`) | ~290 lines + 247 test lines | the power to reset *other* apps' circuits | claim 5 |
| Egress ledger | **KEEP, freeze** | — | — | already built; meaningful only in a session or TUI, which is where T10.8 reads it |
| Local-DNS audit | **CUT** | ~96 lines | a line that always says "resolves locally" | claim 4 |
| `audit --save` drift | **SIMPLIFY**: make `config.json` real defaults (`tor-socks`, `proxy`, `impersonate`; never `allow-direct`), and have `audit` show each setting's source | ~150 lines + part of 173 test lines | "you saved TOR, you're DIRECT now", which flag-only `--allow-direct` already prevents | today the config configures nothing |
| `--impersonate` (default `chrome`) | **KEEP the flag; default undecided** | — | — | 09-09 #8, still unmeasured (Tor-14.5 vs chrome) |
| `get` / scrub / `--nuclear` | **CUT from the surface** until Phase 6 is actually scheduled | 58 lines, a dead verb in `--help`, and the biggest unbuilt privacy job (TASKS.md:239-258) | nothing; it never worked | this is most of the "much more stuff to build" you fear |
| Memory and disk Stores | **KEEP** | — | — | invariants #1/#4. `search → play N` needs the runtime store anyway |
| `--private` | **KEEP** | — | — | invariant #4. Close claim 10's bypasses |
| URL-in-argv refusals | **KEEP** | — | — | invariant #3. 114 lines, zero latency |
| `clean()` | **KEEP and finish it** | — | — | still not applied at core.go:265 (`▶ %s`) or main.go:275 (`np`), the 09-09 CQ-4 finding, still open 22 days later. Also strips no C1 controls (U+0080-U+009F), table.go:256-268. A TUI hands third-party text the whole screen |
| `--no-cache-dir` | **SIMPLIFY**: put the cache in a dir the Store hands out under the runtime store (purged; none under `--private`) | at least two Tor requests per yt-dlp call (magnitude unmeasured) | a client_id crumb in `%TEMP%`, until `purge` | claim 7. Routing it through the Store keeps invariant #1 |
| yt-dlp `minimalEnv` | **KEEP** | — | — | it is why yt-dlp is immune to claim 3 |

**Net:** roughly 700 production lines and 550 test lines leave. Nothing users rely on goes, no invariant is touched, and the largest unbuilt item (Phase 6) stops being a promise.

---

## Unchallenged

I attacked these and could not break them.

- **The 09-28 per-track proxy fix.** I tried to make mpv fall back to direct while a *live* proxy relayed an error, which is what an expired signed URL looks like. That is 3 statuses (404, 403, 200 with garbage) × 5 option sets (`stream-lavf-o` alone, plus `demuxer-lavf-o`, mpv's `--http-proxy`, env only, env plus option), all with `--no-ytdl`. **15/15: zero direct connections** (rig3). The control in rig1 sent the Go preflight and all ten libmpv requests through the proxy.
- **The `cmd.Env` leak is fixed.** `b375de1` removes it, and `Ensure()` (mpv.go:86-92) has no `cmd.Env`.
- **`Up()`'s verify is not a real latency cost.** It opens the circuit under this process's credentials, and yt-dlp then reuses it.
- **"Privacy needs more dependencies."** It needed none (`go.mod`). The TUI is the dependency cost.
- **The SKILL test counts.** `go vet ./...` is clean and `go test ./... -count=1` is all ok. Per package: core 48, net 30, resolver 21, player 17 + 1 skip, store 13, cmd 11, exactly as stated.
- **yt-dlp and the Go preflight under claim 3's conditions.** Both ignore `no_proxy` and case games (`minimalEnv`, explicit `--proxy`, `http.ProxyURL`). The design is right where it was deliberate.

---

## Stronger alternatives

1. **Let the TUI own the bridge (amend T0.1 now).** Cost: a paragraph, plus lazy `Up()` in the TUI (09-09 #6). Status quo cost: two process models, and tracks that die with whichever front-end queued them.
2. **A session token on the bridge before it becomes long-lived. Now measured to be feasible.** libavformat sends `Proxy-Authorization` only after a 407: on the first request the header was empty, and after a `407 Basic` challenge it was `Basic dG9yLWlzbzpzZXNzaW9udG9rZW4=` (rig2). So the bridge can challenge with 407 and mpv will answer. **Caveat the 09-09 debate missed:** yt-dlp's `--proxy http://user:token@…` puts the token in **argv**, which is world-readable just like a signed URL. Hand it to yt-dlp through `minimalEnv` instead. mpv gets it through the per-track IPC option, and the Go preflight holds it in-process. Cost: about 30 lines. Status quo: an unauthenticated loopback Tor proxy that stays open for the whole TUI session.
3. **The sound test** (claim 6). About 100 lines behind the existing `integration` tag. Without it, the next silent regression ships green again.
4. **The speed package**, which is privacy-neutral:
   - (a) a yt-dlp cache dir under the runtime store
   - (b) one yt-dlp call for a single track (`-J -f bestaudio` returns the title and the stream URL together)
   - (c) skip the `play` hold when `--proxy` is external
   - (d) in the TUI, one circuit per session, re-isolated on failure

   Status quo: about 3-4 s of pure process start per `play`, plus a scrape per call.
5. **Tor's own HTTPTunnelPort as an option, not a switch.** It already works today via `--proxy http://127.0.0.1:9080` (Gate 1 verifies CONNECT), and it outlives chainsmoke. But Tor isolates HTTPTunnelPort streams only by `Proxy-Authorization` / `X-Tor-Stream-Isolation` (spec.torproject.org/http-connect, checked 2026-10-01), and libavformat never sends the former unprompted (rig2), so you would lose isolation and the ledger. Document it; don't migrate.
6. **If you truly want a no-Tor product**, the replacement cost of the egress contract is: rewrite INSTRUCTIONS.md:1-19 and main.go:2-27, make `Up()` default to DIRECT, and delete most of `pkg/net`. The result competes head-on with Yututui. That is a different product and deserves its own repo. I do not recommend it, and I have argued within the invariants everywhere else.

---

## Residual risk

These are things nobody has looked at yet, me included. Items marked *speculation* are speculation.

- **HTTPS + CONNECT + multi-segment HLS when the bridge dies mid-track** is still unmeasured (TASKS.md T10.9 item 2). My rigs used `http://` origins.
- **Per-track isolation is not implemented.** INSTRUCTIONS.md:129 says "default to per-track SOCKS auth". The code isolates per *process*, so in a TUI that becomes per *session*: one exit for hours, apart from Tor's own circuit expiry. *Speculation:* a clean per-track design would give each `loadfile` its own bridge listener and credentials, since the per-track proxy option already makes that possible.
- **C1 control characters in titles.** *Speculation:* U+009B can act as CSI in some UTF-8 terminals, and `clean()` passes it.
- **SoundCloud's attitude to Tor exits since 2026-08-19.** Unknown. It decides how often claim 9's lottery is lost.
- **yt-dlp is still one release behind** (installed 2026.07.04, latest 2026.08.19, checked via api.github.com 2026-10-01).
- **The main tree may have moved since `6a70a08`.** Every line number here comes from the worktree.

---

## Summary

Three objections should change what you do next week:

1. **Don't withdraw the core; cut the periphery.** Delete the DNS audit, NEWNYM/control.go, the drift machinery and the `get` surface. Move the private Tor to the TUI or delete it. That is about 1,250 lines out, no invariant touched, and every cut makes the tool faster or more honest.
2. **Build the TUI and let it own the bridge.** Amend T0.1 first. The TUI is the cure for the bug class the privacy design caused (bridge lifetime, session holds, orphans), so leaning into the TUI and keeping privacy are the same move.
3. **Before either: fix the two leaks measured today and add the sound test.** These are small changes. Without them, the TUI's status bar would read `TOR ✓` over a path that `--proxy HTTP://` or `no_proxy=*` silently routes around, and the next silent failure would ship as "done".

**Verdict on your question: right about the cost, wrong about the cure.** The privacy *instrumentation* has cost more than it returned. The privacy *core* is cheap, stable and the product, and most of the slowness lives elsewhere. **Go for Phase 10** once claim 3's fixes and the sound test land.

### Plan for next week

1. **Leaks.** Normalise the `--proxy` scheme, strip `no_proxy` from mpv's spawn environment, and make `Enqueue` refuse a non-`http://` proxy. Re-run the appendix rig and check: origin sees nothing in all four rows.
2. **Sound test.** Add a loopback HTTPS origin, a SOCKS stand-in, the real bridge, and real mpv on a scratch pipe. Check: `time-pos > 0` and zero direct origin hits.
3. **Cuts.** Delete `AuditLocalDNS`, NEWNYM/control.go/stamp, and hide `get`. Then flip `get-stdin` in smoke.ps1. Fix INSTRUCTIONS.md:43/:128/:129, the SKILL gotchas at :227/:252, and the comments at mpv.go:1-4 and :79-82. Check: `go vet`, `go test` and smoke are green.
4. **Decisions on paper.** Amend T0.1 (the TUI owns the bridge). Make T10.8 lazy-`Up()` plus a 407 token, with the token kept out of yt-dlp's argv.
5. **Speed.** Add the cache dir via the Store, one yt-dlp call per single track, and no hold for an external `--proxy`. Then time `search` → `play 1` over Tor before and after.
6. **Then** follow PHASE10.md's execution order, steps 1-3, and the library spike.

---

## Appendix: how claim 3 was measured

Everything below ran on 127.0.0.1. The scratch module points at the tree with `replace chainsmoke => <tree path>` and imports `chainsmoke/pkg/net` and `chainsmoke/pkg/player`.

```go
// origin: net.Listen("tcp","127.0.0.1:0"), logs request line + User-Agent, answers 404.
// proxy:  net.Listen("tcp","127.0.0.1:0"); CONNECT -> "200" then close (never dials out);
//         absolute-form GET -> "206", 1 byte (satisfies the Go preflight).
os.Unsetenv(all proxy vars); if arg2 != "" { os.Setenv("no_proxy", arg2) }
b := csnet.New(scheme+proxyAddr, false, "127.0.0.1:1")   // scheme = "http://" or "HTTP://"
_ = b.Up()                                                // Gate 1: verifies CONNECT against the fake proxy
mp := player.New(`\\.\pipe\debater-scratch-<pid>`, b)     // never the real pipe
_ = mp.Ensure(); defer mp.Down()                          // real spawn flags, incl. --no-ytdl
_ = mp.Enqueue("http://"+originAddr+"/signed-stream.m3u8?Signature=SECRET", "rig track")
time.Sleep(5 * time.Second)                               // then: did the origin see anything?
```

Run with `PATH="/c/Program Files/MPV Player:$PATH"`. Rig2 (the 407 test) and rig3 (the 15-cell error matrix) run `mpv.com --no-config --idle=no --no-video --ao=null` directly, with no IPC server.

Sources checked 2026-10-01:
- [mpv manual: `--http-proxy`, `no_proxy`](https://mpv.io/manual/master/)
- [mpv issue #5807](https://github.com/mpv-player/mpv/issues/5807)
- [Tor HTTP CONNECT spec](https://spec.torproject.org/http-connect.html)
- [yt-dlp soundcloud.py (master)](https://raw.githubusercontent.com/yt-dlp/yt-dlp/master/yt_dlp/extractor/soundcloud.py)
- [yt-dlp latest release](https://api.github.com/repos/yt-dlp/yt-dlp/releases/latest)
- [Yututui](https://awesometui.com/yututui)

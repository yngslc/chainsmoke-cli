# chainsmoke — development plan

Ordered task list. `cmd/chainsmoke` over `pkg/*` is the shippable unit;
`script.sh` is a frozen reference (T0.3) and is never run or fixed. Rule of
thumb throughout: every task that changes CLI behavior also flips the
corresponding expectation in
`.claude/skills/run-chainsmoke/smoke.ps1` (the wiring-regression harness).

---

## Phase 0 — Decisions & groundwork (gates everything else)

- **T0.1 — Process model. DECIDED 2026-08-16: option (a), mpv is the only daemon**
  (the bash model). `chainsmoke` invocations stay short-lived and own no state
  between runs; the result cache lives in `$XDG_RUNTIME_DIR/chainsmoke` (tmpfs)
  and mpv is the single long-lived process. Options (b) *a `chainsmoke daemon`*
  and (c) *the TUI is the session owner* are **rejected**.
  *Why:* (b) means writing, supervising, and securing a second daemon that does
  strictly less than the one we already depend on — mpv is already long-lived,
  already supervised, and already speaks a documented JSON-IPC protocol. That
  protocol is confirmed working on both dev targets (unix socket under WSL,
  named pipe on native Windows — see T0.5), so (a) costs no portability.

  Binding consequences — these stop being options:
  1. **No in-process state survives an invocation.** The preload goroutine at
     `main.go:259` is dead code: a goroutine dies the instant `main()` returns,
     unlike bash `&` (script.sh:237), which forks a process that outlives its
     parent. The idiom was ported; its semantics were not.
  2. **T4.1 (runtime store) is now mandatory and load-bearing**, not a default
     among alternatives. One-shot `search` → `play N` spans two processes and has
     nowhere else to keep results.
  3. **T1.4 (XDG_RUNTIME_DIR fallback) is a prerequisite for T4.1**, since it
     resolves the path the store lives at. WSL ships with the var *unset*
     (verified), so the `/run/user/$UID` fallback is on the primary dev path,
     not an edge case.
  4. **T5.4 resolves to: enqueue N+1 synchronously before exit.** Simplest
     correct option, no new machinery. Escalate to an mpv event hook
     (`observe_property` on `playlist-pos`) only if the synchronous resolve makes
     track boundaries audibly gappy. Delete the goroutine either way.
  5. **The `daemon start|stop|status` verbs (`main.go:236`) stay** — under (a)
     they manage *mpv*, which is precisely their job. They are not vestigial (b).
  6. **Phase 10:** the TUI is a *second front-end over the same runtime store*,
     never the source of truth. It may cache in memory for rendering, but one-shot
     `play N` and an open TUI must always resolve against the same result set.
     **dev's conclusion: approved, fixed**
  *(Amended 2026-10-19, factual — 2026-10-05 review, R7: `play` no longer exits
  once it has queued. mpv streams through the CONNECT bridge inside the `play`
  process, so that process stays up for the whole play session
  (`cmd/chainsmoke/session.go`): a prompt on a terminal, a wait for the queue
  otherwise. Within a session it does hold state in memory — the list being
  walked (`queue.go`) and a background preload goroutine. What still holds:
  nothing survives from one invocation to the next, the runtime store is the
  only result cache, and mpv is the only process that outlives a session.
  Consequence 4 is superseded: N+1 is resolved in the background inside the
  session, not synchronously before exit.)*
- **T0.2 — Define fail-closed egress semantics.** Write down the contract:
  no Resolver/Player call may run unless `Net.Up()` has *verified* the
  bridge accepts CONNECTs. Decide enforcement (resolver constructed only
  from a live Net handle; refuse-to-run otherwise). "Proxy env exported"
  is not the invariant — "traffic cannot leave except through the bridge" is. **dev's conclusion: fixed**
- **T0.3 — Decide the fate of script.sh.** Recommended: freeze it as a
  reference (fix nothing except annotating known bugs), and port lessons
  into Go. If instead it stays a *working* prototype, Phase 1B becomes
  mandatory.
  **dev's conclusion: approved !- *freeze the script but keep it for future references for golang*** 
- **T0.4 — Repo hygiene.** `git init` + first commit; split `main.go` into
  the planned `cmd/` + `pkg/{net,resolver,player,scrub,store}` layout
  (move code verbatim, no behavior change); one `gofmt` commit. **dev's conclusion: fixed**
- **T0.5 — Dev/test environment for playback.** Native Windows can never run
  the player (unix socket IPC) and local WSL is broken (missing ext4.vhdx).
  Options: repair/reinstall WSL, a Linux box, or Linux CI. Needed by
  Phase 2+ integration testing; the pure-Go unit tests don't need it. **dev's conclusion: fixed**

## Phase 1 - COMPLETED — Correctness fixes in the existing carcass (cheap, do before subsystems)

### 1A — Go
- **T1.1 — `search` must establish the chokepoint** — call `a.net.Up()`
  before `res.Search` (parity with `play`/`get`).
  **done**
- **T1.2 — Fix flag handling.** Delete `hasFlag`/raw-argv scanning; read
  `--remember/--private/--proxy/--impersonate` via urfave/cli (Before hook
  or `c.Bool` from context) and actually wire `--proxy`/`--impersonate`
  into `newApp`. Today they are parsed-but-ignored.
   **done**
- **T1.3 — `resolveIndexOrStdin`: reject non-integer argv** with a clear
  "URLs are read from stdin, not argv" error instead of silently falling
  through to a blocking stdin read.
   **done**
- **T1.4 — Runtime-dir fallback.** `XDG_RUNTIME_DIR` unset must not yield
  `/chainsmoke/mpv.sock`; port the bash default (`/run/user/$UID`), create dir 0700.
   **done**
- **T1.5 — Replace homemade `atoi`/`isInt` with `strconv.Atoi`** (overflow).
 **done**
- **T1.6 — `get`: remove hardcoded `/tmp/in.m4a`**; plumb a real temp path
  (full impl is deferred with download+scrub — see "Deferred: download + scrub").
   **done**
- **T1.7 — Honest stub errors.** Only append the "run: yt-dlp -U" hint for
  real resolver failures, never for `ErrNotImplemented`.
 **done**
### 1B — Bash — CLOSED, none of these will be done (T0.3)

T0.3 froze `script.sh` as a reference. These eight were explicitly conditional
on it staying a *working* prototype, and it isn't — so they are closed, not
pending. **Do not implement any of them.** They stay in the file because each
names a real bug whose lesson is already carried by a Go task, and the Go tasks
back-reference these numbers:

`T1.8 -> T4.3` (--private inside the store) · `T1.9 -> T4.4/T5.1` ·
`T1.10 -> T6.1/T6.2` · `T1.11 -> T3.1` (webpage_url) · `T1.12` no owner (Go uses
`net/http`, curl is not a dependency) · `T1.13 -> T4.4` · `T1.14 -> T5.3`
(skip-not-abort) · `T1.15 -> T5.1` (liveness probe).

The `shellcheck` block below closes with the same verdict: nothing it found was
a new bug, which is what made the freeze safe.

- **T1.8** `results_write`: real `CS_PRIVATE` guard (`[[ $CS_PRIVATE == 1 ]] && return 0`).
- **T1.9** `cleanup()`: implement the ephemeral wipe (and only kill an mpv we own).
- **T1.10** `cmd_get`: don't pre-create the output file (mktemp -u a dir, let
  yt-dlp create the file), and stop suppressing ffmpeg stderr.
- **T1.11** `resolve_meta`: `.webpage_url // .url` so single tracks yield
  permalinks, not expiring CDN URLs.
- **T1.12** `deps_check`: add `curl`.
- **T1.13** `cmd_purge`: `player_down` before `rm -rf $RUNDIR`.
- **T1.14** `enqueue` failures must actually skip (call sites `|| true` or
  restructure) instead of dying under `set -e`.
- **T1.15** `player_up`: liveness probe (IPC round-trip) instead of
  socket-file existence; remove stale sockets.

<!-- ------------------------------------------------------------------------ -->
<!-- Local reference only — NOT tasks. Do not assign T-numbers to these.       -->
<!-- ------------------------------------------------------------------------ -->

> #### Reference: `shellcheck script.sh` (run 2026-08-15) — 4 findings, regrouped
>
> Verdict: **clean**. Zero errors, four warnings, and nothing shellcheck found is
> a new bug. It corroborates the hand-written T1.x inventory rather than extending
> it, which is what makes the T0.3 freeze safe.
>
> **Covered by an existing task**
>
> - `SC2046` line 135 — `$(_impersonate_args)` unquoted, in `resolve_meta` → **T1.11's function.**
> - `SC2046` line 257 — same expression, in `cmd_get` → **T1.10's function.**
>
> **No task owns these**
>
> - `SC2046` line 144 — same expression, in `resolve_stream`. 1B has no
>   `resolve_stream` item; this is the only orphan of the four.
> - `SC2034` line 44 — `TOR_CTRL_PORT` unused. Not a defect: `net_rotate()`
>   (script.sh:106) is a `TODO(impl)` stub and its only consumer is commented out
>   on line 109. The warning is *evidence the stub is still a stub*. Already
>   implemented on the Go side (`torCtrlAddr`, main.go:330) → **T2.4**.
>
> **On the three SC2046 hits:** one defect in three places, and the unquoted
> expansion is *deliberate* — `_impersonate_args` (script.sh:129) emits two
> newline-separated words and relies on word splitting to become two argv items.
> Quoting it, as shellcheck suggests, would collapse it into the single argument
> `"--impersonate chrome"` and break yt-dlp. The correct bash fix is an array,
> not a quote. Left as-is under the freeze.
>
> **Lesson for the port → T3.4:** this bug class cannot exist in Go.
> `exec.Command` takes a `[]string`, so argv boundaries are explicit and no
> value of `CS_IMPERSONATE` (spaces, globs, empty) can change the argument count.

## Phase 2 - COMPLETED — pkg/net (spec ordering: network before playback)

- **T2.1** Tor lifecycle: detect a listening system tor on 9050; else spawn a
  private one; readiness = SOCKS handshake, not sleep.
  **done**
- **T2.2 — SUPERSEDED. No Privoxy, no generated config.** The CONNECT->SOCKS5
  hop runs *in-process* (INSTRUCTIONS.md:47, decided 2026-08-15). Write
  `StartConnectBridge`: listen on `127.0.0.1:0`, accept `CONNECT host:port`,
  dial Tor's SOCKS5 with `ATYP=0x03`, splice. Working tested code:
  TORNAGO-REFERENCE.md 4.2; SOCKS5 handshake 4.1.
  Consequences: `privoxy` leaves the dependency list (`tor` stays);
  `Bridge.ProxyURL()` returns our own ephemeral listener instead of the
  hardcoded `:8118`; remote DNS is true by construction, so T2.5 becomes a
  regression guard rather than a proof; per-track `IsolateSOCKSAuth` becomes
  possible, which no external bridge can do. Port 4.1/4.4 => MIT attribution
  header in `pkg/net/bridge.go`.
  **done**
- **T2.3** `Up()` = tor up -> bootstrapped -> bridge listening -> one real CONNECT through it, else hard error. Implements T0.2.
**done**
- **T2.4** `Rotate()`: control-port auth + `SIGNAL NEWNYM`, surface Tor's
  rate limiting.
  **done**
- **T2.5** `SelfTest()`: DNS-leak assertion (resolve through bridge; verify
  the local resolver never saw soundcloud/sndcdn hostnames).
  **done**
- **T2.6** `--proxy` override path: user-supplied proxy replaces the bridge
  but keeps the same verified-up contract.
  **done**
- **T2.7** Honest-caveat doc: Tor exits vs media CDNs; note the WireGuard
  netns swap seam.
  **done**

  `phase 2 is complete. Meet me you know where...`

## Phase 3 - COMPLETED — pkg/resolver (yt-dlp wrapper)

- **T3.1** `Meta`: `--flat-playlist -J`, parse entries; use
  `webpage_url // url` (bash lesson T1.11); playlists and single tracks.
  **waits for claude's review**
  **dev's override/done**
- **T3.2** `Stream`: `-f bestaudio -g` with
  `--extractor-args soundcloud:formats=hls_aac`.
  **done**
- **T3.3** `Search`: `scsearchN:<term>`, respect `--limit`.
  **done**
- **T3.4** Proxy + impersonate injection on *every* invocation, taken from
  the live Net handle (chokepoint enforcement), never from ambient env.
  **done**
- **T3.5** Error taxonomy: stale-extractor (→ the yt-dlp -U hint), 403/expired
  (→ rotate+retry), proxy-down (→ net error). Timeouts on every exec.
  **done**

## Phase 4 - COMPLETED (claude) — pkg/store (per T0.1)

- **T4.1** Runtime store (default): results in `$XDG_RUNTIME_DIR/chainsmoke`, 0700,
  survives across invocations, wiped by purge/cleanup. This is what makes
  `chainsmoke search` → `chainsmoke play N` work at all in the Go port.
  **done**
- **T4.2** Disk store (`--remember`): `$XDG_STATE_HOME/chainsmoke`, real persistence.
  **done**
- **T4.3** `--private`: memory/no-write semantics enforced *inside* the store
  (single write path), not at call sites — the bash bug T1.8 is the cautionary tale.
  **done**
- **T4.4** `Purge()`: wipe both stores; coordinate with player teardown
  (bash lesson T1.13).
  **done**

## Phase 5 - COMPLETED — pkg/player (mpv JSON-IPC)

- **T5.1** `Ensure()`: spawn `mpv --idle --no-video --input-ipc-server`,
  wait for socket, liveness-probe stale sockets (T1.15 lesson).
  **done**
- **T5.2** IPC client: newline-delimited JSON, `request_id` correlation,
  filter async event messages out of replies.
    **done**
- **T5.3** `Enqueue()`: preflight (HEAD via bridge) then
  `loadfile append-play`; skip-not-abort on failure (T1.14 lesson).
    **done**
- **T5.4** Preload-ahead per the T0.1 decision (daemon queue, or synchronous
  enqueue-next, or mpv event hook). The current goroutine is dead code.
    **done**
  *(2026-10-19: now one track resolved in the background inside the play
  session — see T0.1's amendment.)*
- **T5.5** Transport verbs: np/toggle/seek/vol/next/prev/stop on the real
  client; `daemon status`.
    **done**

## Deferred: download + scrub (was Phase 6)

Streaming-only means nothing persists, so metadata scrubbing has no target.
Revisit when `get` ships. Open question at that time: Go M4A/AAC tag writing
(bogem/id3v2 is MP3-only, dhowden/tag is read-only) — may force an
ffmpeg-only scrub or a Python sidecar.

Parked as written rather than deleted: the research below is the expensive
part and it does not rot. Three things to carry to the revisit:

- **T1.10's lesson parks with it.** "Don't pre-create the output file" is owned
  by T6.1/T6.2 and by nothing else, so it is unowned until `get` ships.
- **`get` remains a verb that cannot succeed.** It is wired to the scrubber
  stub and fails with `not implemented` (the `get-stdin` smoke case). That is
  honest but it is still a promise in `--help`; decide then whether to hide the
  verb until it works.
- **T6.3's "no python dependency" is no longer free.** The open question above
  supersedes it: if nothing in Go can write M4A tags, the real choice is an
  ffmpeg-only scrub or the sidecar, and that is a dependency decision, not a
  library-picking one.

- **T6.1** Download path: yt-dlp to a temp *dir* (never a pre-created file —
  T1.10 lesson), through the chokepoint.
- **T6.2** ffmpeg strip: `-map_metadata -1 +bitexact -c copy`; do not
  suppress stderr; verify output is non-empty/valid.
- **T6.3** Selective retag (keep artist/title/album/art; drop comments,
  encoder, source-URL frames, ISRC) — Go tag library, no python dependency.
- **T6.4** xattr cleanup + mtime normalize; `--nuclear` = mat2 pass when
  available; post-scrub audit print.

## Phase 7 - COMPLETED — Privacy verification (make `audit` real)

- **T7.1** `audit`: live bridge status (actually probe, don't print config),
  store mode, results-file presence, impersonation setting.
  Built as three questions in the order a user asks them: *am I anonymized
  right now* (probed posture + DNS + ledger), *am I anonymized the way I chose*
  (drift against a saved config), *what does this machine still hold* (store
  mode + cached count). The saved config added here (`audit --save`,
  `pkg/core/config.go`) is an EXPECTATION compared against the probe, never a
  substitute for it — the posture line can only be set by `Verified()`.
  Two things the implementation found that the task text did not anticipate:
  (a) posture has THREE states, not two — DOWN (nothing permitted to leave)
  is not DIRECT (permitted, unanonymized), and reporting the first as the
  second sends a user hunting a leak that is not happening;
  (b) `audit` must keep running when `Up()` refuses. Every other verb aborts
  there because it is about to send traffic; a diagnostic that refuses to run
  exactly when the thing it diagnoses is broken is useless. The exit code still
  carries the failure, and `--save` still refuses to record a setup that never
  carried traffic.
  **done**
- **T7.2** Egress accounting test: capture one `play`; assert every outbound
  connection goes to the bridge and only *.soundcloud/*.sndcdn beyond it.
  This is the acceptance test for T0.2/T2.x/T3.4.
  Built as `pkg/net/ledger.go` + `core.EgressLedger`: the bridge counts every
  CONNECT target, every refused plaintext request, and every tunnel that failed
  to open, and `audit` prints them classified against `core.ExpectedHost`.
  Three corrections to the task text, all of them load-bearing:
  (a) the allowlist as written would flag the tool's own audio. The December
  2025 HLS/AAC migration serves streams from
  `playback.media-streaming.soundcloud.cloud`, so `soundcloud.cloud` is in
  `expectedSuffixes` and the match is anchored on a leading dot, or
  `evilsoundcloud.com` would pass;
  (b) record-only, never blocking. A CDN redirect to a host we have not seen
  before would become silence instead of music, and the tool's first duty is to
  work — classification lives in core, where audit reports drift without the
  bridge enforcing it;
  (c) the honest scope is stated in the output itself: this accounts for what
  went THROUGH the bridge, not for what went around it. `Observed: false` when
  the run does not own a bridge (`--proxy`, DIRECT), because "we could not
  look" must never render as "nothing left the machine". Closing that half
  needs the OS, not us — see the open questions below.
  **done**
- **T7.3** Verify mpv/ffmpeg honor the proxy for HLS fetches on the target
  build. **RE-ANSWERED 2026-08-23 — the 2026-08-19 answer below was wrong on
  mechanism, and the correction matters.**
  Old answer: "`http_proxy`/`https_proxy` are honored; **`ALL_PROXY` is
  IGNORED**: mpv connects directly, with no error."
  Measured again with a listener that logs what it receives, on mpv 0.41 on
  BOTH targets (native Windows 10 and WSL Ubuntu 26.04): with a LIVE proxy,
  **all six spellings are honored, ALL_PROXY included**. ALL_PROXY is not
  ignored. The real question is what happens when the proxy DIES, and there the
  answers split — pointing each variable at a dead port and watching a local
  origin for a hit:

  | var | https:// URL | http:// URL |
  |---|---|---|
  | `http_proxy` | fail closed (win + linux) | fail closed (win + linux) |
  | `HTTP_PROXY` | fail closed win, **OPEN linux** | fail closed win, **OPEN linux** |
  | `https_proxy` | **OPEN** (win + linux) | n/a |
  | `HTTPS_PROXY` | **OPEN** (win + linux) | n/a |
  | `ALL_PROXY` | **OPEN** (win + linux) | **OPEN** (win + linux) |
  | the shipped four | fail closed (win + linux) | fail closed (win + linux) |

  **OPEN** = mpv silently opened a DIRECT connection to the origin when it
  could not reach the proxy. No error, no warning, audio keeps playing. For a
  tool whose premise is that nothing leaves except through Tor that is the
  worst failure mode there is, and a bridge dying mid-track is exactly how a
  user would meet it.
  Lowercase `http_proxy` is the single load-bearing variable: the only spelling
  that fails closed on both platforms and both schemes, and its presence makes
  the whole set fail closed (verified for the shipped four, and for those four
  plus ALL_PROXY). `exportProxyEnv` already sets it, so **the shipped path is
  safe** — but it is safe for a reason nobody had written down, which is how it
  gets "simplified" to the conventional upper-case spelling later.
  Both halves of the remaining work are now done: (a) re-measured on the
  WSL/Linux build, and (b) `TestExportProxyEnvSetsTheFailClosedSpelling` in
  `pkg/net/proxyenv_test.go` pins it, with the measurement in the comment.
  ffmpeg is deliberately NOT covered: scrubbing is deferred (see below), and a
  runtime proxy-enforcement layer was considered and rejected — a user running
  a VPN or proxychains on top of chainsmoke is a supported setup, and fighting
  their environment would break working configurations to defend against a case
  the shipped env already handles.
  **done**

## Phase 8 - COMPLETED — UX + release polish

- **T8.1** `search` output via tablewriter (index/title/uploader/duration),
  matching the reference repo's table.
  Built on the **standard library instead of tablewriter**, deliberately.
  `go get github.com/olekukonko/tablewriter@latest` resolves to EIGHT modules —
  tablewriter plus `olekukonko/{cat,errors,ll}`, `mattn/{go-isatty,go-runewidth}`,
  `olekukonko/ts` and `golang.org/x/sys`; one of them is a logging library. This
  project ships a single static binary to people who chose it for its threat
  model, and its dependency set is one module. T10.12 also already says the TUI
  gets its own table widget and the two share DATA, not formatting — so the
  dependency would serve exactly one function and be superseded two phases on.
  And it does not solve the actual problem: the hard part is display-width
  arithmetic for truncation, which is needed either way. `runeWidth` in
  `pkg/core/table.go` is the seam if exact East Asian width ever matters more
  than the dependency count.
  Three things the task text did not anticipate, all found by measuring:
  (a) **columns are chosen by the DATA.** `--flat-playlist` returns three
  shapes: `scsearch` carries title+uploader+duration, user pages (`/likes`,
  `/tracks`) carry title only, and SETS carry *neither* — `_type
  "url_transparent"` with album, id and url. Two columns of blanks on every row
  of a `likes` listing would be noise paid for out of the title's width, so
  empty columns are dropped;
  (b) **sets had no titles at all**, so every row of `playlist` read
  "(untitled)" — a numbered list nobody can choose from. `titleFromSlug` in
  `pkg/resolver` recovers the name from the permalink's last segment. It costs
  nothing (the alternative is one Tor round-trip per track, which is what T3.1
  made Meta flat to avoid) and breaches nothing: the slug is the track's own
  title lowercased and hyphenated, disclosing exactly what the title column
  already discloses, with no scheme, host or user segment;
  (c) **a pipe gets TSV, a terminal gets the table.** `chainsmoke search x |
  head -3` and `| cut -f2` are real uses that a box-drawn table serves badly,
  and width is meaningless down a pipe. Four stable fields, duration in
  seconds, empty rather than 0 for unknown.
  Titles are third-party text and are scrubbed accordingly: a newline in a title
  would forge a numbered row pointing `play N` at a track the user never saw, a
  tab would wreck the TSV, and an ESC would let a track repaint the terminal.
  **done**
- **T8.2** `likes` / `playlist` verbs (thin over Meta).
  `App.Collect` is the shared middle — resolve, cache, print — that `search`
  already ran inline, so a fourth listing verb now costs a URL builder rather
  than a copy of the flow. `chainsmoke likes <user>` and
  `chainsmoke playlist <user>/<set>`; both take `--limit`, both fall back to
  stdin, and both accept the `<user>/sets/<slug>` spelling people actually copy.
  The handle rides in **argv**, which is a bounded exception rather than an
  oversight: `search <term>` already does the same and a handle is the same
  class of disclosure as a search term, while a track URL identifies one exact
  thing you are about to play. Anyone wanting neither in argv pipes it on stdin.
  `cleanHandle`'s allowlist ([A-Za-z0-9_-] per segment) is what makes building a
  URL from user input safe: no input can introduce a second path component, a
  query, a scheme or a host, so the URL handed to the single egress chokepoint
  always points at literally soundcloud.com. A 17-case injection sweep asserts
  it. Validation also runs BEFORE `Net.Up()`, so a typo never costs a circuit.
  **done**
- **T8.3** Shell completion, `-ldflags -X main.BuildVersion` release build,
  `CGO_ENABLED=0` static Linux build.
  `chainsmoke completion <bash|zsh|fish>`. urfave/cli already answered the
  completion *protocol*; what it ships is two files in its own repo, so the glue
  is emitted here (fish uses the library's real generator, which is declarative
  and carries usage text). Verified by driving the emitted bash function
  directly: verbs, prefix filtering, per-command flags and custom values all
  complete.
  **The banner was the actual blocker**, not the protocol. Every wordmark line
  becomes a completion candidate on the protocol path, and lands in `~/.bashrc`
  on the script path. `bannerCorruptsOutput` suppresses it for exactly those two
  cases — this is NOT the general banner fix, which stays T10.1/T10.2's; it is
  the narrow suppression without which T8.3 cannot work at all. Two smoke cases
  assert the absence, which is why the harness grew a `NotMatch` field.
  *(Superseded 2026-09-28: `bannerCorruptsOutput` is gone — the banner became a
  TTY-only `Before` hook, which urfave skips for completion; PHASE10.md Part B.)*
  `build.ps1` cross-builds all six targets. **MEASURED 2026-08-24: without
  `-trimpath` the binary embeds absolute source paths** — `D:/sccli/pkg/core/
  browse.go` and 14 more — disclosing the builder's drive letter and directory
  layout to anyone holding it. With it: zero. `-s -w` takes 13 MB to 8.9 MB.
  `CGO_ENABLED=0` is set for every target, not just Linux, and the Linux
  artifact is confirmed `statically linked` by `file(1)`.
  Also fixed here, found live: a rejected `--impersonate` was classified
  correctly but then handed the DEFAULT hint, telling users to run `yt-dlp -U`
  for a flag we passed ourselves on a request that never left the machine.
  `core.ErrImpersonate` now carries the two fixes that can actually help.
  **done**

## Phase 9 - COMPLETED — Tests & CI (grows alongside, not after)

The header was accurate: most of this arrived with Phases 2-8 rather than as a
Phase 9 push. Reviewed task by task 2026-08-24; what follows separates what was
already deployed from what actually had to be built.

- **T9.1** Unit tests with fake `Resolver`/`Player`/`Net` (the interfaces
  exist precisely for this): verb wiring, store selection
  (remember/private matrix), index-vs-stdin resolution.
  **Already deployed:** the fakes, carrying most of the suite, and the store
  matrix (`TestDiskStore`, `TestPrivateWritesNothing`,
  `TestPrivateOverridesRemember`, `TestRuntimeStoreSurvivesTheProcess`).
  **Missing, now built:**
  (a) **verb wiring** — `cmd/chainsmoke` had ZERO test files. Added
  `main_test.go`: `TestEgressVerbsGateOnUp` drives all five egress verbs through
  real urfave/cli parsing with `Up()` refusing and asserts the resolver is never
  touched — T0.2 checked at the layer where it is easiest to lose, since adding
  a verb means remembering to gate it and nothing but a test enforces
  remembering (`search` was once exactly this bug, T1.1). Plus
  `TestNonEgressVerbsDoNotBringEgressUp` (a volume change must not bootstrap
  Tor), `TestCommandTableIsWellFormed` (duplicate verb/alias — urfave resolves
  the FIRST match, so a collision silently shadows), and
  `TestWireStoreSelection` / `TestWireHandsDownEgressFlags` for invariant #4 and
  T1.2 at the point the decision is made;
  (b) **index-vs-stdin** — `ResolveIndexOrStdin` had no test at all, despite
  being the invariant-#3 gate for `play`/`get`. Added a 9-row routing table plus
  three targeted tests: the refusal must name the exposure, the store must NOT
  be consulted on the argv-URL path (a cache miss would otherwise mask the
  invariant message), and a real cache miss must propagate as itself.
  Two things fell out of writing these. `globalFlags()` was extracted from
  `main()` so the flag set is testable at all — and that made
  `TestValueFlagsMatchesGlobalFlags` possible, which closes a footgun that was
  previously a hand-maintained list: a value-taking global flag missing from
  `valueFlags` in `completion.go` makes `--newflag completion` parse as the
  completion verb.
  Deliberately NOT tested: banner placement and bare-invocation routing. T10.1
  and T10.2 rewrite exactly that, so those assertions would be written twice
  against a worse seam. The command table, `wire()`, and subsystem call ORDER
  are the parts Phase 10 leaves alone, and that is what these tests pin.
  **done**
- **T9.2** Linux CI: build + vet + unit tests; integration tests behind a
  build tag (needs T0.5).
  **Nothing existed** — no `.github/`, no build tags anywhere.
  Built `.github/workflows/ci.yml`: gofmt check (reported, never auto-applied),
  `go vet ./...` over the whole module, `go test ./... -race`, a compile-only
  run of the tagged suite, and a six-target cross-build matrix mirroring
  `build.ps1`'s flags — plus an assertion that the artifact embeds no absolute
  build paths, which is `-trimpath` regression-guarded rather than trusted.
  **Linux specifically, and not by preference:** `-race` needs cgo and a C
  toolchain, and the Windows dev box has neither — measured 2026-08-24,
  `go: -race requires cgo`. **Narrowed 2026-09-09:** that is true of Windows,
  not of the box — WSL on the same machine has `gcc` and
  `wsl -e bash -c "cd /mnt/d/sccli && go test ./... -race -count=1"` is green,
  all packages. So the race detector has a local home as well as CI,
  and `pkg/net` is genuinely concurrent (accept/splice goroutines, a
  mutex-guarded ledger, an RWMutex in the SOCKS dialer). Every CI step was run
  locally in WSL before committing the workflow; all six targets build and the
  path assertion holds.
  **Integration suite** (`//go:build integration`, 5 tests): `pkg/net` proves
  the whole chain carries bytes through a live circuit, that remote DNS
  genuinely resolves at the exit, that the ledger records the self-test tunnel,
  and that `Rotate()` leaves egress usable; `pkg/player` spawns a real mpv and
  round-trips `idle-active`, which is the only way to catch a spawn flag mpv
  rejects — the fake socket can never surface that.
  Behind a tag rather than `testing.Short()` because the default
  `go test ./...` — the one CI runs and the one a contributor runs by reflex —
  **must never open a circuit**. Each test SKIPS rather than fails when its
  dependency is absent: a missing Tor is not a broken build, and a red suite
  that means "you do not have tor installed" teaches people to ignore red
  suites. Verified both ways — 5/5 pass against live Tor and real mpv on WSL,
  5/5 skip cleanly with the dependency removed. CI runs the compile check only.
  **done**
- **T9.3** Keep `smoke.ps1` baseline current as stubs become real.
  **Already deployed and maintained** — 35 cases (40 as of 2026-10-01), updated in step with every
  phase through T8.3, including the `NotMatch` field added so the harness can
  assert ABSENCES (no banner in a completion script). This is a standing
  obligation rather than a task that completes: T10.10 folds in the
  non-TTY/`--no-tui` cases next.
  **done**

## Phase 10 — TUI (the default surface) 
//05.10.2026 - Phase 10 HAS BEEN DROPPED DOWN FOR THE EARLIER RELEASE OF CLI VERSION OF CHAINSMOKE.
respectfully, decision of both dev and claude - TUI is an independent chunk of code, thus, building it inside of this project will do much more problems overtime, especially problems with privacy.

Decided 2026-08-14. Bare `chainsmoke` opens a **TUI**; ~~`-h`/`--help` prints the
ASCII banner + command table; explicit verbs stay one-shot and quiet~~ explicit
verbs stay one-shot (banner rule revised below). Full
rationale, library comparison, and constraints: INSTRUCTIONS.md → *"Go TUI
architecture — the default surface"*. **Banner revised 2026-09-28 (dev's
call):** help prints no art; the banner opens the SoundCloud/player verbs
(`search`, `likes`, `playlist`, `play`, `get`, `daemon start|stop|status`) on a
TTY only. The TUI decision is unchanged.

Build it the way `main.go` was built: **carcass first** — real layout, real
navigation, real keybindings, stubbed data — then wire the seams one at a time.
The TUI is a *second front-end over the existing `Store`/`Resolver`/`Player`
interfaces*, never a parallel implementation of the logic.

**Ordering note:** T10.1–T10.4 are doable now against stubs. T10.6 (real
playback) is blocked on Phase 5 + the Windows IPC question (T10.7); T10.5 wants
Phase 3/4. Don't let the carcass wait on any of that.

### 10A — Entry point & carcass (no dependencies; do first)

- **T10.1 — Split the entry point.** Route: no args + TTY on **both** stdin
  and stdout → TUI; ~~`-h`/`--help` → banner + command table~~ `-h`/`--help` →
  command table, no banner (2026-09-28); `--version` →
  version line only; any explicit verb → today's one-shot path. **Move the
  banner out of `main()`'s unconditional print** so it stops prefixing every
  verb's output and polluting pipes.
  **Half done 2026-09-28 (dev's call):** the banner is out of `main()` — a
  `Before: banner` hook on the SoundCloud/player verbs, TTY-only — and
  `-h`/`--help` prints the command table with **no** art, reversing the route
  above. What remains is the TUI route itself. smoke's `bare-pipe` case
  already pins the non-TTY bare invocation (help, exit 0).
- **T10.2 — TTY guard + `--no-tui`.** If stdin **or** stdout is not a
  terminal, a bare invocation must print help and exit, never launch the TUI.
  Without the stdout half, `smoke.ps1`, CI, and `chainsmoke | grep` hang
  forever; without the stdin half, `echo x | chainsmoke` in a real console
  launches an alt-screen TUI reading keys from a pipe that never produces any,
  with no way to quit (flagged 2026-09-09). Both helpers already exist —
  `core.StdinIsTerminal()` and `core.StdoutIsTerminal()` (exported 2026-09-28;
  real console/tty checks since 2026-10-01, so a null device is not a TTY) —
  so the rule costs nothing. Add `--no-tui` as the
  explicit escape hatch. This is a hard prerequisite for T10.10, which needs a
  case for each half.
- **T10.3 — Pick the library.** Bubble Tea + lipgloss + bubbles (recommended:
  our state is a small explicit machine and every slow call is async) vs tview
  (faster conventional panel layout). Verify current versions/maintenance —
  INSTRUCTIONS.md deliberately asserts none. One decision, then commit to it.
- **T10.4 — Carcass: layout, navigation, keybindings.** Frame, header/splash
  with the existing ASCII art, status bar, panel switching, quit/help overlay —
  all driven by fixture data. No network, no mpv, no store. Ships as something
  you can run and click around in on Windows today.

### 10B — Screens & wiring

- **T10.5 — Screen inventory (TBD — user is still deciding).** Candidates
  derived from the existing verb surface, to be confirmed/cut:
  search input · numbered results list · now-playing + transport (toggle /
  seek / vol / next / prev) · queue view · `get` (download + scrub) with
  progress · audit / privacy posture panel · settings (`--remember` /
  `--private` toggles) · help overlay. **Decide this before T10.6+**; the
  screen set determines which seams need wiring.
  **Constraint on the settings candidate (2026-09-09):** a live `--private`
  toggle collides with invariant #4 — the Store backend is chosen once in
  `wire()` "for the whole run", and under a TUI a run is hours, not seconds. A
  mid-session flip is either a lie (label changes, backend does not), a runtime
  Store swap (nothing supports it, and the Store is invariant #1's chokepoint),
  or a "restart required" no-op — and none of those remove the permalinks
  already on disk from earlier searches. If a settings panel ships, store mode
  should be **read-only** there (current backend + on-disk count from
  `AuditReport`), with `purge` as the action. A real toggle has to be specified
  as purge-then-switch or it silently leaves the disk copy. Also see PHASE10.md
  Part 3, which proposes collapsing search/likes/playlists to one screen and
  cutting `get`.
- **T10.6 — Wire results to the Store.** Search input → `Resolver.Search` →
  `Store.SaveResults` → results list, sharing the cache with one-shot
  `play N`.
- **T10.7 — Event-reading IPC client (the TUI's biggest dependency).**
  *Rewritten 2026-09-13; the original body described a foreground `Enqueue`
  that T5.3 replaced.* The blocking problem is solved: `Enqueue` hands the
  track to the `--idle` daemon over `loadfile` and returns, and the daemon
  outlives the process (T0.1(a)). What remains is that the Phase 5 client is
  **synchronous** — one request, one reply, async events discarded — and a TUI
  must read `observe_property` events (`time-pos`, `playlist-pos`, `pause`,
  `eof`) while idle. That is a second client behind the same `Command` seam
  (reader goroutine, pending-request map, event channel), **not** a change to
  the synchronous one, which the one-shot verbs depend on and the `pkg/player`
  suite pins.
  The socket-vs-named-pipe question is already answered by `dial()` in
  `pkg/player/mpv.go`: the stdlib talks to the pipe fine. What it cannot do on
  Windows is interrupt a blocked read — `Close()` does not unblock `os.File` on
  a named pipe — so a wedged daemon hangs the TUI. Cancellable reads need
  overlapped I/O (`go-winio`, a Windows-only dependency). **Decide that before
  writing the client, not after.** Full plan: PHASE10.md Part 1C.
- **T10.8 — Persistent egress status bar.** Always-visible
  `EGRESS: TOR ✓` / `EGRESS: DIRECT ⚠` indicator. The one-shot CLI prints its
  "NOT anonymized" warning once and it scrolls away; for a privacy tool a
  permanent posture indicator is a safety feature, not decoration. Pairs with
  T0.2 / T7.1.
  **Undecided, and this task decides it by implication (2026-09-09):** an
  honest always-on posture bar requires `Up()` to have run — i.e. at launch,
  before the user asks for anything. Under the CLI that is a three-second
  window; under a TUI the in-process CONNECT bridge (`pkg/net/bridge.go`,
  loopback, unauthenticated — `handle()` checks nothing) becomes a
  session-long local proxy. Decide explicitly whether the TUI brings egress up
  at launch or on first use (with the bar reading `DOWN` until then), and
  whether the bridge needs a per-session token before it is long-lived.

### 10C — Robustness & polish

- **T10.9 — Terminal restore on every exit path.** Alt-screen + raw mode must
  be restored on normal quit, on `SIGINT`/`SIGTERM`, **and on panic** — a crash
  that skips this leaves the user's shell with no echo. Deferred restore plus
  an explicit recover.
  **Observed 2026-09-28, not yet fixed — two separate concerns living under
  this one task, do not conflate them:**
  1. **mpv itself is left running by design.** `daemon-start` in `smoke.ps1`
     leaves a live `mpv.exe` after the harness exits (reproduced: `tasklist`
     showed it still up), and `stop` never kills it (~~nor Ctrl+C~~ — corrected
     2026-10-03: Ctrl+C DOES kill it on both systems, because the daemon shares
     the terminal's process group/console; CH-DEBATE-2026-10-03 #2) — this is T5.5/T0.1(a)
     as written, not a bug. An **idle** daemon has an empty playlist and issues
     no HTTP requests at all, so on its own it cannot leak; the open question is
     what an **already-playing** daemon does once its egress path dies mid-track.
     **Corrected 2026-09-28:** half of this was a bug, not the design. `daemon
     stop` and `purge` never sent `quit` — `Down()` returned before dialling in
     a fresh process — so nothing could take the daemon down. Fixed; smoke now
     ends with it stopped. `quit` still missed a *second* mpv on the endpoint,
     which never gets the pipe and idles with no IPC (measured), so `Down()`
     now also kills every mpv whose argv names the endpoint
     (`TestDownKillsStrays`). And the "open question" answered itself as a worse
     bug: the egress path died *every* time, because `play` returned right after
     `loadfile` and took the in-process bridge with it — no track ever got past
     opening. `play` now holds the bridge as a session (prompt on a terminal,
     wait-for-drain otherwise), which also traps Ctrl+C for that one verb; the
     global handler this task asks for is still open.
  2. **Measured (plain HTTP only): once the per-`loadfile` proxy dies
     mid-stream, mpv/ffmpeg does NOT fall back to a direct connection.** Method:
     a throwaway `--idle` daemon on a dedicated pipe, loaded via the real IPC
     wire format with the exact option `Enqueue` sends
     (`stream-lavf-o=http_proxy=<url>`), against a local origin behind a local
     forward-proxy standing in for the bridge; the proxy process was killed 34s
     into a drip-fed transfer. Result: `mpv.log` shows
     `[160.419] wav: Packet corrupt` → `EOF reached` at the exact instant the
     proxy died, then `finished playback, success (reason 0)` — the origin's own
     request log confirms **zero** further connections after the kill. This
     complements the earlier dead-from-the-start proxy test (T7.3/the
     `project-mpv-proxy-env-trap` finding): together they cover "proxy never up"
     and "proxy dies mid-transfer," and both fail closed for this mechanism.
     **Not yet covered, and still open:** the real bridge is CONNECT-only for
     HTTPS (the SoundCloud CDN's actual scheme), whereas this test used a plain
     absolute-form HTTP relay; and this tested one continuous connection dying,
     not a *fresh* segment dial made after the proxy is already dead (HLS's
     real shape). Re-run against a CONNECT-tunnelling proxy and a real
     multi-segment HLS fixture before treating this as closed.
     **Updated 2026-10-01:** the CONNECT half is measured. In the shipped shape
     over IPC with an `https` origin, CONNECT is used, and a dead, 404, 502 or
     200-then-close proxy produced zero direct connections (review
     CH-2026-10-01, M-2/M-3). A fresh segment dial is a fresh connection, so it
     meets the same decision. Two bypasses were found and closed the same day:
     an inherited `no_proxy`, and any proxy value that is not a literal
     `http://`. A loopback rig showed both BYPASS before the fix and clean
     after. ~~The real multi-segment HLS fixture through the real bridge remains
     open, because it needs live egress.~~ **Updated 2026-10-03
     (CH-DEBATE-2026-10-03 #1, #9):**
     - The no_proxy fix first missed a *reused* daemon, one started by an older
       build or by hand, and that one bypassed. `Ensure()` now marks what it
       spawns and replaces any daemon that lacks the mark (measured
       before/after).
     - A 6-segment HLS fixture on loopback, needing no egress, put the
       playlist and every segment through the per-`loadfile` proxy, with 0
       direct connections. With the proxy killed after segment 1, nothing
       reached the origin.
     - Still open: HTTPS segments over CONNECT through the real `ConnectBridge`.
       That takes a loopback TLS origin and a SOCKS5 stand-in, not the
       internet.
  3. **Unrelated to both of the above:** the private Tor daemon orphan on
     Ctrl+C (PHASE10.md Part D) is a different process leaking for a different
     reason — `Net.Down()`, not `Play.Down()`, and it isn't touched by anything
     measured here.
- **T10.10 — Smoke harness must not hang.** Add cases asserting a
  non-TTY/`--no-tui` bare invocation exits with help (proves T10.2), and that
  ~~verbs print no banner~~ no banner ever reaches a pipe (2026-09-28 rule).
  Never add a case that launches the real TUI.
  **Partly in place 2026-10-01:** `bare-pipe` covers the non-TTY bare
  invocation, and no case can see the banner through a pipe (`NotMatch '====='`
  on four of them). Still to add with T10.2: `--no-tui`.
- **T10.11 — Narrow-terminal degradation.** The wordmark is ~62 cols inside a
  78-col frame; the smoke cloud ~60. Below that, wrapping destroys the art —
  detect width and degrade (smaller art, or none) instead of wrapping. Keep the
  art in **raw string literals** (it contains backslashes), so it must never
  contain a backtick.
- **T10.12 — Reconcile with Phase 8.** T8.1 (tablewriter) is the *one-shot*
  results table; the TUI has its own list/table widget. Keep both fed by the
  same `Store` data rather than formatting results in two places.


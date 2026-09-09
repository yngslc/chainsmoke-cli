# CH-DEBATE-2026-09-09 — phase10

**Date of debate and phase:** 2026-09-09 — `phase10`

**Target:**
- `ch-writings/report-review/CH-2026-09-09-phase10.md` (the Reviewer, `chainsmoke-reviewer`), read in full.
- `ch-writings/PHASE10.md` (Main/dev, dated 2026-08-31; its Part 2 data is dated 2026-08-24).
- Tree at `e2f2e01` on `main`, `origin/main` identical (`git rev-list --left-right --count origin/main...HEAD` → `0 0`).
- Working tree carries four untracked paths: `.claude/agents/`, `.claude/agent-memory/`,
  `ch-archive/ai-agents-usage-guide`, **and `ch-writings/report-review/`** — the Reviewer's
  own report is not committed.

Everything below that is labelled *measured* was run on this box today. Everything
that was not run says so in as many words.

---

## Steelman

**The Reviewer's headline.** `e2f2e01` added four lines to `pkg/player/mpv.go`
that replace the mpv daemon's inherited environment with `PATH` alone; because
mpv has no proxy flag and no proxy default, the environment *is* the security
wiring, so the daemon that carries every byte of audio now connects to the CDN
outside the bridge — and it fails *open*, which reads to a user as the tool
finally working. Everything else in Phase 10 readiness is secondary to that, so
the gate should not open until it is fixed.

**PHASE10.md's T10.3 framing.** The dependency budget is a defended property of
this project (INSTRUCTIONS.md:74, one direct module), so the library decision is
correctly posed as a measured tension — 25 linked modules for the Charm line
against 9 for tview — weighed against an architectural argument that already
favours Bubble Tea, and settled by a spike rather than by taste.

**PHASE10.md's execution order.** The A-refactor first, then the entry point,
then signals, then the library: each step ends green, and the first three are
worth doing even if the TUI were cancelled.

---

## Contested claims

Ordered by cost to fix later, descending.

---

### 1. The `cmd.Env` leak is worse than "mpv fetches direct": it is a correlation leak that links the user's real IP to their Tor circuit

**TIER: INFERENCE** (against the Reviewer's severity framing and its Fixes ordering)

**BASIS.** Read the two egress paths side by side:

- `pkg/player/mpv.go:307-333` — `preflight()` builds its client from
  `p.net.ProxyURL()` explicitly (`mpv.go:342-360`, "ambient `http_proxy` must
  not get a vote"), so the one-byte ranged GET on the signed URL goes **through
  the bridge, over Tor**.
- `pkg/player/mpv.go:91-93` — the daemon that then fetches that same signed URL
  has `PATH` only, so it goes **direct**.

Same signed URL. Seconds apart. One request from a Tor exit, the next from the
user's real address. A signed SoundCloud stream URL is a high-entropy unique
token; anyone holding both sides of that CDN log has an exact join key. The leak
therefore does not merely expose *this track* — it retroactively attaches a real
IP to the Tor circuit that resolved it, and so to everything else that circuit
carried in the same run.

Two smaller escalations the report does not name, both measured or read today:

- The direct fetch does its **own DNS resolution** of the CDN hostname through
  the system resolver. `AuditLocalDNS` (`bridge.go:996`) reports what the local
  resolver *can* see; it does not and cannot see this. So the disclosure lands
  even when the TCP connection is later refused.
- The direct request identifies itself. Measured (see below): first bytes were
  `GET /probe.m3u8 HTTP/1.1\r\nUser-Agent: libmpv`. Not "a stream client" —
  specifically libmpv, from an address, with a SoundCloud signed URL.

**WHAT WOULD SETTLE IT.** Nothing to settle; it follows from `mpv.go:307-333`
against `mpv.go:91-93`. What it *changes* is the fix ordering. The Reviewer's
Fixes list puts "revert or allowlist" at CRITICAL and per-load proxy options at
HIGH. Under a correlation reading those swap: a revert closes the common case
but leaves `daemon start` → `play` (`main.go:399`, never calls `Net.Up()`) doing
exactly the same preflight-over-Tor / fetch-direct pairing. **CQ-2 is not the
"durable" fix, it is the fix; the revert is a stopgap that must not be allowed
to close the ticket.**

---

### 2. `stream-lavf-o=http_proxy` works, and it propagates to HLS child segments — measured, so CQ-2 is de-risked

**TIER: MEASUREMENT** (in the Reviewer's favour — their fix is sound, and they
correctly flagged it as inferred)

**BASIS.** Their Fixes line says "the option form is inferred from mpv's general
passthrough, so it needs measuring, not assuming." I measured it. Scratch rig
under the session scratchpad, everything on 127.0.0.1, mpv `0.41.0-244-gaf9c81fa1`,
spawned with `cmd.Env = []string{"PATH="+…}` exactly as HEAD does, loading a
real HLS manifest that references a separate segment URL:

```
== A: no proxy option at all
   origin saw : [GET /master.m3u8  GET /master.m3u8  GET /seg1.aac  GET /seg1.aac]
   proxy  saw : []
== B: --stream-lavf-o=http_proxy=http://127.0.0.1:1   (dead)
   origin saw : []                       <- FAILS CLOSED
   proxy  saw : []
== C: --stream-lavf-o=http_proxy=http://127.0.0.1:<live local forward proxy>
   proxy  saw : [GET /master.m3u8  GET /master.m3u8  GET /seg1.aac  GET /seg1.aac]
```

Case A also settles a question nobody asked: the leak is not confined to the
manifest — **the segment fetches leak too**, which is where the audio actually
is. Case C is the one that matters for CQ-2: `http_proxy` is on libavformat's
HLS option-inheritance whitelist, so the child segment opens carry it. The fix
does not stop at the playlist.

**WHAT WOULD SETTLE THE REMAINDER.** Two gaps I did **not** close, and they are
the two that decide whether this ships:

1. **The per-`loadfile` option form is unmeasured.** I tried and my rig never
   reached mpv's IPC pipe at all (`could not reach IPC` on all three cases —
   a fault in my scratch harness, not evidence about mpv; do not read it as
   corroboration of anything). Settle it inside the tree, where the IPC path
   already works, as a `pkg/player` integration test:
   `Ensure()` → `Command("loadfile", url, "append-play", -1, map[string]any{"stream-lavf-o": "http_proxy=http://127.0.0.1:1"})`
   against a loopback origin, assert the origin sees nothing.
2. **https + CONNECT is unmeasured.** I proxied plain `http://` with an
   absolute-form forward proxy. The real bridge is **CONNECT-only** and answers
   405 to absolute-form GET (`pkg/net/bridge.go:614-620`), and real streams are
   `https://`. That combination — `stream-lavf-o=http_proxy` → libavformat →
   CONNECT tunnel → bridge — is the one that has to work, and nobody has run it.
   `go test -tags integration ./pkg/player/` with a live bridge is the place.

**One design warning that comes out of this.** If the proxy ever carries a
credential (see #6), `--stream-lavf-o=…` **as a spawn flag** puts it in mpv's
argv. Invariant #3 is about stream URLs, but the reasoning is about argv being
world-readable, and a bridge token is a credential. Per-`loadfile` over IPC is
the only form that is safe under both readings.

---

### 3. PHASE10.md Part 1E is false at current versions, and the Reviewer said "the tview column holds up"

**TIER: MEASUREMENT**

**BASIS.** PHASE10.md:105-108 (Part 1E) states: "**both candidate libraries link
`mattn/go-runewidth`**, so Phase 8's ~40-line `runeWidth` … can be **deleted**
and replaced with the real table. Net code reduction". T10.12 in the execution
order (step 10) is that claim cashed in.

I rebuilt the same carcass (list + text input + status bar) in each candidate
today, same host, same flags PHASE10.md used (`CGO_ENABLED=0 -trimpath -s -w`),
counted with `go version -m`:

| line | modules linked | binary | links `mattn/go-runewidth`? |
|---|---|---|---|
| `github.com/charmbracelet/bubbletea` v1.3.10 + bubbles v1.0.0 + lipgloss v1.1.0 | **25** | 2.93 MB | yes (v0.0.19) |
| `charm.land/bubbletea/v2` v2.0.9 + bubbles/v2 v2.2.1 + lipgloss/v2 v2.0.6 | **19** | 3.31 MB | yes (v0.0.27) |
| `rivo/tview` v0.42.0 + `gdamore/tcell/v2` **v2.13.10** (current) | **8** | 2.35 MB | **no** |
| `rivo/tview` v0.42.0 + `gdamore/tcell/v2` **v2.8.1** (PHASE10.md's pin) | **9** | — | yes (v0.0.16) |

The last two rows are the finding. tcell dropped `mattn/go-runewidth` between
v2.8.1 and v2.13.10; the current tview line links `rivo/uniseg v0.4.7` instead.
So if T10.3 picks tview, **T10.12 is not a deletion, it is a new direct
dependency** — against the one-dependency policy that Part 2 spends its whole
argument defending. The Reviewer checked that tview's *tag* was still current
and concluded "the tview column holds up"; the column's load-bearing consequence
does not.

**WHAT WOULD SETTLE IT.** The commands above; they take four minutes. And the
better target is right there in the table: **`rivo/uniseg v0.4.7` is linked by
all three candidate lines, at the same version.** T10.12 should be rewritten to
adopt `uniseg` (or, on the Charm line, `charmbracelet/x/ansi`'s `StringWidth`,
which is grapheme-cluster correct and is what lipgloss itself uses), not
`go-runewidth`. That keeps T10.12 a deletion under every outcome of T10.3.

---

### 4. The T10.3 version data is wrong in ways that change the decision, not only in its major number

**TIER: MEASUREMENT**

**BASIS.** The Reviewer's D-4 is right that the table names v1 while v2 shipped
first. Three things they did not turn up, all measured today by building against
both lines:

- **The v2 module path is not `github.com/charmbracelet/…`.** It is
  `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, `charm.land/lipgloss/v2`.
  Asking for the github path is a hard error, verbatim:
  `module declares its path as: charm.land/lipgloss/v2 but was required as: github.com/charmbracelet/lipgloss/v2`.
  A spike that starts from the report's module names does not compile.
- **The count that frames the whole tension is not 25.** It is **19** for v2
  (`atotto/clipboard` and `xo/terminfo` survive the move; `muesli/termenv`,
  `mattn/go-isatty`, `go-localereader`, `go-osc52`, `x/cellbuf`,
  `erikgeiser/coninput` are gone; `charmbracelet/ultraviolet` and
  `charmbracelet/x/windows` are new). So the real 2026-09-09 tension is
  **19 vs 8**, not 25 vs 9. That narrows the gap by a third — it does not close
  it, and it is still the argument tview wins on.
- **v2's `Model` interface is not v1's.** Measured against the compiler:
  `Init() tea.Cmd` (unchanged), `Update(tea.Msg) (tea.Model, tea.Cmd)`
  (unchanged), and **`View() tea.View`, not `View() string`**. Every Bubble Tea
  example, tutorial and mental model the dev is likely carrying is the v1 shape.
  That is a real, unbudgeted cost on a "one session, then commit" spike.

**COST, unpriced in PHASE10.md.** The v2 linked set includes
`github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886` — a
**pseudo-version, i.e. no semver tag at all** — and it is the new renderer, so
it is load-bearing rather than incidental. A project whose stated dependency
posture is "one direct module, and that is a property worth defending" should
decide that deliberately rather than inherit it.

**WHAT WOULD SETTLE IT.** The three builds above. Then rewrite Part 2's table
with the `charm.land` paths, 19/8, the `View() tea.View` break, and the
`ultraviolet` pseudo-version, dated 2026-09-09 — and re-run the spike against
the line actually chosen.

---

### 5. T10.5's "settings (`--remember` / `--private` toggles)" screen cannot exist as written — it collides with invariant #4

**TIER: INFERENCE**

**BASIS.** `ch-writings/TASKS.md:530-536` lists, among the candidate screens,
"settings (`--remember` / `--private` toggles)". Invariant #4
(`cmd/chainsmoke/main.go:26-27`): "`--private` forces the memory Store **for the
whole run** regardless of config." The store backend is chosen once, in `wire()`
(`main.go:71`, `store.New(remember, private)`), from flags parsed in `Before`.

Under the one-shot CLI those two sentences agree, because a run is three
seconds. Under a TUI a "run" is hours, and a mid-session toggle is one of three
things: a lie (the label flips, the backend does not), a rewrite (swap `Store`
implementations at runtime — nothing in the tree supports it, and the Store is
invariant #1's chokepoint, so this is the most expensive place in the codebase
to bolt on a live swap), or an honest no-op with a "restart required" note.
There is a fourth problem none of those fix: by the time a user reaches the
settings panel and switches to private, the permalinks from their earlier
searches are **already on disk** — and on Windows `%TEMP%` is not tmpfs
(SKILL.md's own gotcha), so they stay there until `purge`.

This is cheap to decide now and expensive after T10.5 is confirmed and T10.4's
carcass has a panel for it.

**WHAT WOULD SETTLE IT.** A one-line decision in PHASE10.md Part 3: the privacy
panel is **read-only for store mode**, shows the current backend and the on-disk
count from `AuditReport`, and offers `purge` as the action instead of a toggle.
If a live toggle is genuinely wanted, it has to be specified as
"purge-then-switch", because anything else silently leaves the disk copy.

---

### 6. Nobody has decided when the TUI calls `Net.Up()`, and T10.8 quietly decides it for them

**TIER: COST**

**BASIS.** PHASE10.md never states it. T10.8 does, by implication: a
"persistent, always-visible `EGRESS: TOR ✓`" status bar (TASKS.md:552-557) can
only be honest if `Up()` has already run and `Verified()` is true — i.e. at
launch, before the user has asked for anything. Under the CLI that is
unremarkable. Under the TUI three things change scale, none of them written
down:

- **The in-process CONNECT bridge becomes a long-lived, unauthenticated local
  proxy.** `StartConnectBridge` (`pkg/net/bridge.go:531-548`) binds
  `127.0.0.1:0` — correctly, and the comment explaining why not `:0` is good —
  but `handle()` (`bridge.go:602`) authenticates nothing. Any local process
  that finds the port gets free Tor tunnelling; on Linux loopback is shared
  across local users, so "local process" includes other users' processes. Today
  that window is a few seconds per verb. In a TUI it is the session.
- **The T7.2 ledger changes meaning.** SKILL.md's gotcha argues, well, that the
  ledger is process-scoped and that `Observed: false` means "we could not look".
  In a TUI the process *is* the session, so the ledger becomes a session-long
  destination log held in memory — which is the right call, but it is a
  different object from the one the gotcha describes, and T10.8's status bar
  will be reading it.
- **A private tor (`ensureTor`, `bridge.go:422`) lives for hours** with a temp
  `DataDirectory`, and per A-4 nothing traps SIGINT. The orphan window grows
  with the session.

**WHAT WOULD SETTLE IT.** Two lines in PHASE10.md, before step 2:
(a) the TUI calls `Up()` **lazily**, on the first action that needs egress, and
the status bar renders a fourth state — `EGRESS: IDLE` — until then; posture
already has three honest states and adding a fourth is cheaper than lying with
three. (b) If `Up()` at launch is preferred anyway, then the bridge should
require a per-run random `Proxy-Authorization` before Phase 10 ships, carried
per-`loadfile` (never on argv — see #2). I have **not** measured whether
libavformat sends `Proxy-Authorization` on a CONNECT when `http_proxy` carries
userinfo; that would need checking before committing to (b).

---

### 7. `build.ps1` never asserts what CI asserts, and the disclosure includes the builder's username

**TIER: MEASUREMENT**

**BASIS.** `.github/workflows/ci.yml:104-110` greps each cross-built artifact for
`/home/runner/work` and fails if found. `build.ps1` — the script that actually
produces `dist/`, and the one a human runs — has no such check anywhere. So the
property is enforced only on the path that never ships.

I re-ran the 2026-08-24 measurement to see whether the claim still holds:

```
CGO_ENABLED=0 go build          -ldflags "-s -w" -o notrim.exe ./cmd/chainsmoke
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o trim.exe  ./cmd/chainsmoke
grep -ao "D:/sccli[^ ]*\.go" notrim.exe | sort -u | wc -l   ->  15
grep -ac "D:/sccli" trim.exe                                ->  0
```

Exactly 15, exactly as `build.ps1`'s comment says, 16 days later. **Unchallenged
and reproduced.** But the same untrimmed binary also embeds ~250 paths of the
form `C:/Users/der3t/AppData/Local/go-portable/go/src/…` — so the disclosure is
not "the builder's drive letter and directory layout", it is **the builder's
Windows username**. build.ps1's comment understates its own finding.

**WHAT WOULD SETTLE IT.** Add three lines to `build.ps1`'s `Build` function
mirroring the CI assertion, keyed on `$PSScriptRoot` rather than a hard-coded
path so it works on any builder's machine, and fix the comment to say
"username".

---

### 8. The default `--impersonate chrome` was chosen for Cloudflare evasion and applied to a Tor-exit context, where it works against the anonymity set

**TIER: ALTERNATIVE**

**BASIS.** `cmd/chainsmoke/main.go:93` defaults `--impersonate` to `chrome`.
The rationale is INSTRUCTIONS.md:59, which is a *Cloudflare-fingerprinting*
argument: a Python TLS stack behind a Chrome UA is an instant tell, so use
curl_cffi to reproduce Chrome's JA3/JA4.

That argument is sound and orthogonal to the one that matters here. chainsmoke's
requests arrive at SoundCloud from a **Tor exit**. The population to blend into
at a Tor exit is Tor Browser users, which is Firefox-derived. Chrome-over-Tor is
comparatively rare, so the default makes chainsmoke's traffic *more* distinctive
at the exit while making it less distinctive to Cloudflare. Two different threat
models; the project adopted one default from a source arguing the other.

Measured on this box today, `yt-dlp --list-impersonate-targets` (local, no
egress):

```
Chrome-133  Macos-15    curl_cffi     <- what `chrome` resolves toward: macOS, not Windows
Tor-14.5    Macos-14    curl_cffi     <- a Tor Browser target exists
Firefox-147 Macos-26    curl_cffi
Edge-101    Windows-10  curl_cffi
```

Note the second-order oddity as well: there is no Chrome-on-Windows target in
the list at all, so on the primary dev target `--impersonate chrome` presents a
**macOS** Chrome fingerprint from a Windows host.

**WHAT WOULD SETTLE IT.** I have **not** measured whether SoundCloud's api-v2
accepts `Tor-14.5`, and settling it requires live egress, which I did not and
will not run. The command is
`chainsmoke --impersonate Tor-14.5 search <term>` against a live circuit,
compared against the same with `chrome`, over several circuits. If it works, it
is a strictly better default for this tool and costs one string in
`main.go:93`. If it does not, that fact belongs in INSTRUCTIONS.md next to the
Cloudflare paragraph, because right now the two arguments are not distinguished
anywhere.

---

### 9. The resolver's error taxonomy is fixture-pinned, so a green suite says nothing about the yt-dlp on the box

**TIER: INFERENCE**

**BASIS.** SKILL.md: "`pkg/resolver` pins the retry policy and the error taxonomy
against stderr captured from real yt-dlp". True, and the captures are dated
(`ytdlp_test.go:111`, `:120`, `:228`, `:326` — 2026-08-19 and 2026-08-24).
But they are *fixtures*: `TestClassifyImpersonate` (`ytdlp_test.go:286`) feeds a
string literal into `classify()`. The suite passes identically whether the
installed yt-dlp still emits those strings or not. So "131 tests green" is not
evidence that the taxonomy still matches reality, and no one in this review
loop treated it as a separate question.

Two concrete pieces of drift, measured today:

- Installed yt-dlp is **2026.07.04**; latest release is **2026.08.19**
  (`api.github.com/repos/yt-dlp/yt-dlp/releases/latest`, checked 2026-09-09).
  One release behind, on the single dependency the whole tool's egress rests on,
  and INSTRUCTIONS.md:132 lists "bump yt-dlp nightly" as a change trigger.
- `ytdlp_test.go:111` records its capture as "Found live 2026-08-24 on a yt-dlp
  **without curl_cffi**." That premise no longer describes this box:
  `yt-dlp --list-impersonate-targets` returns 17+ curl_cffi targets, exit 0
  (measured above). The fixture is still a legitimate case to classify, but the
  comment now misdescribes the environment, which matters because the taxonomy
  branches on exactly that distinction (`ytdlp.go:385`, "curl_cffi (the
  `--impersonate` path) says `curl: (7) …`").

**WHAT WOULD SETTLE IT.** `yt-dlp -U`, then re-capture the four pinned stderr
strings against 2026.08.19 and diff them against the fixtures. That needs one
live failing invocation per case, so it is an integration-suite job, not a
smoke case.

---

### 10. T10.1/T10.2's routing rule keys on stdout alone; a TUI also needs stdin

**TIER: SCOPE**

**BASIS.** PHASE10.md Part 1B and TASKS.md T10.1 both spell the rule as
"no args **+ TTY** → TUI", and T10.2 as "if **stdout** is not a terminal, print
help and exit". Consider `echo x | chainsmoke` in a real console: stdout is a
character device, so the guard passes and the TUI launches — into an alt screen,
reading keys from a pipe that will never produce any. The user has no way to
quit except killing the process from another window. That is the same class of
failure T10.2 exists to prevent, arriving through the door the rule leaves open.

The fix is free: both halves already exist and are already used —
`stdinIsTerminal()` (`pkg/core/core.go:217`) and `stdoutIsTerminal()`
(`pkg/core/table.go:240`). The rule should require **both**.

**WHAT WOULD SETTLE IT.** Amend the rule in PHASE10.md Part 1B before step 2,
and add the smoke case as `echo x | .\chainsmoke.exe` → help, exit 0 alongside
the bare-invocation case the Reviewer already asked for in A-5.

**A hypothesis of mine that died, recorded because it was worth testing.** I
expected `fi.Mode()&os.ModeCharDevice` to report *false* under mintty/git-bash,
which would have meant the TUI could never launch in the dev's own terminal.
Measured, and it does not reproduce — modern Git-for-Windows mintty uses ConPTY:

```
new Windows console (Start-Process): ModeCharDevice=true   -> stdoutIsTerminal()=true
mintty -e (Git for Windows):         ModeCharDevice=true   -> stdoutIsTerminal()=true
piped:                               ModeNamedPipe=true    -> stdoutIsTerminal()=false
```

The detection is correct on this box. Older Cygwin/MSYS2 terminals without
ConPTY would still be named pipes, but that is speculation and belongs below.

---

## Unchallenged

I attacked these and could not break them.

- **CQ-1 itself.** I reproduced it independently and tried to break it in the
  one direction that would have mattered — a Windows child with only `PATH` has
  no `SystemRoot`, which classically breaks winsock/TLS init, so I expected the
  HTTPS path might fail closed even with the env stripped. It does not:

  ```
  http    cmd.Env=PATH only (HEAD)  -> CONNECTED, first bytes: "GET /probe.m3u8 HTTP/1.1\r\nUser-Agent: libmpv"
  http    inherited env (pre-HEAD)  -> NO CONNECTION in 8s (failed closed)
  https   cmd.Env=PATH only (HEAD)  -> CONNECTED, first bytes: "\x16\x03\x01\x05\xe0\x01..."   (TLS ClientHello)
  https   inherited env (pre-HEAD)  -> NO CONNECTION in 8s (failed closed)
  ```

  The Reviewer measured cleartext HTTP; the finding holds over TLS as well, and
  over HLS child segments (#2, case A). Their CRITICAL stands, unqualified.

- **Every verification number in their report.** Re-run today:
  `go vet ./...` exit 0 · `gofmt -l ./cmd ./pkg` empty ·
  `go test ./...` all ok, `pkg/scrub` no test files ·
  per-package top-level counts via `go test -list '.*'`:
  cmd 6, core 48, net 29, player 14, resolver 21, store 13 = **131**, exact ·
  `smoke.ps1` → "smoke: all 35 cases pass". No number in that report is
  unsourced.

- **PHASE10.md's own 25-module measurement**, reproduced exactly. Its method
  (`go version -m` on a real build rather than `go list -m all`) is the right
  one and I used it unchanged. My quarrel is with the versions and with Part 1E,
  not with the measurement.

- **The `-trimpath` claim**, reproduced exactly at 15 (see #7).

- **`-race`.** PHASE10.md's Verification section says the race detector cannot
  run on this Windows box and leans on CI for it. That is true of *Windows*, but
  WSL on this same box has `gcc`, so it is not true of the box. I ran it:
  `wsl -e bash -c "cd /mnt/d/sccli && go vet ./... && go test ./... -race -count=1"`
  → **all packages ok, no races**. The concurrent code in `pkg/net` is clean
  today. The only correction owed is to SKILL.md's and `ci.yml`'s "the race
  detector has no home except CI" — it has one, locally, and it should be in the
  Phase 10 inner loop given how concurrent T10.7 and the TUI event loop will be.

- **The invariant-#3 defence in `pkg/player`.** I went looking for a path by
  which a signed URL reaches argv, a log, an error string, `np` or a
  subprocess command line, and found none. `name(args)` (`mpv.go:216`) drops the
  arguments, `redactURL` (`mpv.go:362`) strips the URL out of transport errors,
  the `NowPlaying` guard (`mpv.go:409-411`) catches `://` and `Signature=`, and
  `force-media-title` prevents the filename fallback that would otherwise print
  the whole credential. The `%q` in the two `EnqueueAhead` skip warnings
  (`core.go:254`, `:258`) escapes control bytes, so the Reviewer is right that
  those two are already safe.

- **`pkg/scrub`.** 27 lines, all stub, `var _ core.Scrubber = (*FFmpeg)(nil)` as
  a compile-time assertion, and a doc comment that describes the pipeline rather
  than pretending to implement it. There is nothing here to test and nothing to
  attack; "no tests" is the correct state for it.

- **`go.sum` / dependency hygiene.** Four modules, every one with both `h1:` and
  `/go.mod` hashes, no `replace`, no `vendor/`, no `exclude`.
  `git ls-files dist` is empty and `.gitignore` covers `dist/`, `*.exe`,
  `token.txt`; no binary is committed.

- **The Reviewer's `ch-archive/` read (D-5).** I re-read both files.
  `chainsmoke-play-algo.md` is genuinely superseded — the `--unsafe-url`
  proposal is answered by T4.1 plus `ResolveIndexOrStdin`, and its own table
  already ranks the prompt above it. `ai-agents-usage-guide` is indeed misfiled
  and does end on an open question about this project's agent tooling. Nothing
  in the archive has become live again. Agreed, nothing to add.

- **`Rotate()`'s honesty.** I expected to find it claiming a fresh circuit it
  did not get. It does not: `Reisolate()` returns false when the bridge was
  built without isolation, and the stderr text
  (`bridge.go:305-309`) says exactly which half succeeded and scopes the claim
  to "the *next* stream". The `newnym` cooldown warning (`bridge.go:334-340`) is
  the same discipline. Correct as written.

---

## Stronger alternatives

1. **Make the per-`loadfile` proxy the fix, not the follow-up.** Cost: one
   integration test and a change to `Enqueue`'s existing option map
   (`mpv.go:284-287`) — the seam is already there, and #2 shows the mechanism
   propagates to HLS segments. Cost of the status quo (revert only):
   `daemon start` → `play` keeps pairing a Tor preflight with a direct fetch of
   the same signed URL, which is the correlation in #1, and it will not be
   caught by any test because none exists.

2. **Add the spawn-environment test *and* an egress-shape test.** The Reviewer
   asks for a test that the spawn command carries the proxy spellings. Go one
   step further and assert the *observable*: a `pkg/player` integration test with
   a loopback origin and a dead proxy that fails if the origin sees a byte.
   Asserting the command line proves the wiring; asserting the socket proves the
   property. Cost: ~40 lines behind the existing `integration` tag. The
   `http_proxy`-spelling finding was made exactly this way, by breaking the
   thing and watching.

3. **Retarget T10.12 at `rivo/uniseg`.** Cost: none — it is already linked by
   every candidate line, at v0.4.7 in all three. Cost of the status quo:
   T10.12 is written against `go-runewidth`, which the current tview line does
   not link, so the task is a deletion under one outcome of T10.3 and a new
   direct dependency under the other, and nobody will notice until step 10.

4. **Rewrite PHASE10.md Part 2 from today's numbers before the spike, not
   during it.** `charm.land/*` paths, 19 vs 8, `View() tea.View`, the
   `ultraviolet` pseudo-version. Cost: fifteen minutes and the three builds
   above. Cost of the status quo: the spike's first hour is spent discovering
   that the documented import paths do not resolve, which is the worst possible
   framing for a decision that is supposed to take one session.

5. **Give the egress status bar a fourth state and make `Up()` lazy.**
   `EGRESS: IDLE` until the first action needs a circuit, then TOR / DIRECT /
   DOWN as today. Cost: one enum value and a decision line in PHASE10.md.
   Cost of the status quo: launching the TUI opens a Tor circuit and an
   unauthenticated local CONNECT proxy for the whole session, for a user who may
   only have wanted to look at their cached results.

6. **Copy CI's build-path assertion into `build.ps1`.** Cost: three lines.
   Cost of the status quo: the property is guaranteed only on the artifacts
   nobody ships.

---

## Residual risk

Things nobody — not the Reviewer, not the Main agent, not me — has actually
looked at. Everything in this section is speculation unless it names a
measurement.

- **`https` + CONNECT through `stream-lavf-o`, and the per-`loadfile` option
  form.** Both unmeasured; my attempt at the second failed in my own rig. These
  are the two gates the CQ-2 fix has to pass, and they are currently assumed.
- **Whether CI has ever actually run green on `e2f2e01`.** `origin/main` equals
  `HEAD`, so the code is pushed, but the repository is private (unauthenticated
  `api.github.com` returns 404) and `gh` is not installed on this box or in WSL,
  so nobody in this loop has seen a workflow result. Phase 9 is declared closed
  partly on CI's existence. Speculation: it is probably fine, since I ran its
  three substantive steps locally (gofmt, vet, `-race`) and all pass. But
  "T9.2 done" currently rests on a YAML file being *well-written*, not on a
  green run.
- **`ch-writings/report-review/` and `.claude/agents/` are untracked.** A
  `git clean -fd` deletes the review this phase gate rests on, and the two agent
  definitions. The Reviewer flagged the agents; the report directory holding
  their own output is the same hazard and they did not notice it.
- **`dist/` holds six `0.1.0` binaries from 2026-08-24.** Gitignored, local
  only, built before `e2f2e01` — so they do not carry CQ-1, but they do carry
  the `daemon start` variant of it. Speculation: if any copy was ever handed to
  anyone, it should be treated as leaking on that path.
- **SoundCloud's api-v2 / `--flat-playlist` response shapes.** Last measured
  2026-08-24 (`ytdlp.go:553`, `ytdlp_test.go:228`). The three-shape finding that
  T8.1's whole column logic depends on has not been re-checked since, and
  SoundCloud is the one party in this system that changes without telling
  anyone.
- **Older Cygwin/MSYS2 terminals without ConPTY** would still present stdout as
  a named pipe and be misclassified as non-TTY. Speculation — I could not
  reproduce it on this box's mintty, and modern Git for Windows is fine.
- **`pkg/store`'s `--private` guarantee under a long-lived process.** The tests
  assert zero bytes written for a *run*. Nobody has asked what the memory
  backend does after several hours of TUI searching — it is unbounded as far as
  I read, but I did not measure growth.

---

## Summary

Three objections would change what happens next week; the rest are worth fixing
but would not.

1. **The `cmd.Env` leak is a correlation leak, and that changes the fix, not
   just its urgency.** `preflight` goes through Tor and the daemon does not, so
   the same signed URL is fetched from a Tor exit and from the real IP seconds
   apart — which links the address to the circuit, not merely to the track. A
   revert closes the common case and leaves `daemon start` → `play` doing
   exactly the same thing. Per-`loadfile` proxy options are the fix. I measured
   the mechanism today: `stream-lavf-o=http_proxy` fails closed on a dead proxy
   and **propagates to HLS child segments**, so the approach is sound; what is
   still unmeasured is the `https`+CONNECT path and the per-`loadfile` option
   form, and those two should be measured before the code is written.

2. **T10.3's decision data is wrong in ways that would waste the spike.** The
   v2 modules live at `charm.land/*`, not `github.com/charmbracelet/*`; the
   count is 19 vs 8, not 25 vs 9; `View()` returns `tea.View` now. And
   PHASE10.md Part 1E is measurably false at current versions — the current
   tview line does **not** link `mattn/go-runewidth`, so T10.12 flips from a
   deletion to a new direct dependency depending on which library wins.
   `rivo/uniseg v0.4.7` is common to all three candidates and is the right
   target instead.

3. **Two Phase 10 decisions are being made by implication rather than on
   purpose:** a runtime `--private` toggle (T10.5) that invariant #4 does not
   permit, and `Up()`-at-launch (implied by T10.8's always-on status bar), which
   turns a seconds-long unauthenticated local CONNECT proxy into a session-long
   one. Both are one line of PHASE10.md today and a rewrite after the carcass
   exists.

**Verdict on the flag.** I could not break the Reviewer's CRITICAL — I attacked
it from the one angle that might have saved it and it got worse instead. So the
gate stays shut, but not for long and not for much: **fix CQ-1/CQ-2 together
with the per-load option, add the egress-shape test, correct Part 2 and Part 1E,
and settle the two Phase 10 decisions above — then build the TUI.** None of that
is more than a session's work, none of it requires restructuring the plan, and
the plan itself survived everything I threw at it. Everything else in this
report can be done alongside Phase 10 rather than before it.

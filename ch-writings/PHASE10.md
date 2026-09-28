# Phase 10 — TUI (the default surface)

## Context

Bare `chainsmoke` should open a TUI; explicit verbs stay one-shot and quiet
(decided 2026-08-14, TASKS.md Phase 10). This is not a new feature bolted on
the side — it is a **second front-end over the same `Store`/`Resolver`/`Player`
interfaces**, and the existing code is not shaped to support one.

The blocker is structural: **`pkg/core` prints instead of returning data.**
`Collect` resolves, caches, *and* prints. `EnqueueAhead` writes `▶` lines to
stdout and skip warnings to stderr, returning only an `error`. `Audit` prints a
nine-line report. A TUI needs every one of those as values. That is the
"rebuilding existing code" this phase is really about, and it is why the
refactor comes first rather than after the widgets.

Measured to size it: **32 print sites in `pkg/core`**, but they are concentrated,
not sprawling —

| function | sites | disposition |
|---|---|---|
| `Audit` + `auditConfig` + `auditEgressLedger` | **22** | becomes a report struct |
| `EnqueueAhead` | 4 | returns per-track outcomes |
| `renderTable` / `renderTSV` | 4 | **stay** — already `io.Writer` renderers, correct as-is |
| `readLine` prompt, `Purge` | 2 | move to `cmd/` |

So 26 sites actually move, 22 of them in one subsystem.

Two decisions stay open by your call: **T10.3 (library)** is a spike run
immediately before building, and **T10.5 (screen inventory)** is proposed below
as research for you to nail down.

---

## Part 1 — What Phase 10 rebuilds

### A. Full split: `pkg/core` returns data, `cmd/` prints

The foundation. Nothing else can be built cleanly first.

| today | after |
|---|---|
| `Collect(url, limit) error` — resolves, caches, prints | `Collect(url, limit) ([]Track, error)` — resolves, caches, **returns**; `cmd/` prints |
| `EnqueueAhead(entries) error` | `EnqueueAhead(entries) (QueueResult, error)` — which tracks queued, which were skipped and why |
| `Audit() error` — 22 print sites | `AuditReport()` returning a struct; `cmd/` formats it, TUI's status bar and privacy panel read the same fields |
| `ResolveIndexOrStdin(arg)` — prompts on stdin | split: index lookup (both front-ends) vs the stdin/prompt path (`cmd/` only) |

`Audit` is the load-bearing one, and it is already half-done: `Config`, `Drift`
and `EgressLedger` are structs. What is missing is the *envelope* — posture
(TOR/DIRECT/DOWN), proxy URL, store mode + count, DNS lines, drift list — which
currently exists only as printed text. **T10.8's egress status bar consumes
exactly that struct**, so this refactor and T10.8 are the same work.

Critical files: `pkg/core/core.go`, `pkg/core/browse.go`, `cmd/chainsmoke/main.go`.

Reuse rather than rebuild — these are already the right shape:
- `RenderResults(w, ts, width, tty)` — `[]Track` in, writer out (`pkg/core/table.go`)
- `OutputWidth()` — `$COLUMNS` → ioctl/kernel32 → 80. **Directly reusable for T10.11.**
- `FormatDuration`, `clean()` — the TUI needs both for identical reasons
- `Config` / `Drift` / `LoadConfig` / `SaveConfig` — the privacy-config screen's whole model already exists (Phase 7)

### B. Entry point split (T10.1 / T10.2)

Route: no args + TTY on **both stdin and stdout** → TUI · `-h`/`--help` →
banner + command table · `--version` → version line · any verb → today's
one-shot path · `--no-tui` → help.

**T10.2 is a hard prerequisite for T10.10** — without the stdout guard,
`smoke.ps1`, CI and `chainsmoke | grep` hang forever. **The stdin half was
added 2026-09-09:** `echo x | chainsmoke` in a real console passes a
stdout-only check and launches an alt-screen TUI reading keys from a pipe that
never produces any — no way to quit short of killing it from another window.
`stdinIsTerminal()` (`pkg/core/core.go`) and `stdoutIsTerminal()`
(`pkg/core/table.go`) both exist; require both. T10.10 needs a case for each.

~~Note `bannerCorruptsOutput` (added T8.3) becomes partly redundant~~ —
**superseded 2026-09-28: `bannerCorruptsOutput` and `valueFlags` are gone.** The
banner is now a `Before` hook on the SoundCloud/session verbs, TTY-only, so the
`--generate-bash-completion` guard is urfave's own: `Command.Run` returns for
the completion protocol before it calls `Before`, and `Before` is additionally
skipped when `shellComplete` is set (command.go, v2.27.7) — which holds against
a TTY too. Nothing to port at T10.1. **But note the route line above**: it
still says `-h`/`--help` → banner, which the dev reversed the same day (help
prints no art). Reconcile it when T10.1 is picked up.

### C. Second IPC client (T10.7)

`pkg/player/mpv.go` already anticipates this in a comment on `ipc`:

> *"The Phase-10 TUI, which must read events while idle, needs a reader
> goroutine and a pending-request map instead — that is a different client
> behind this same seam, not a change to this one."*

So: new type in `pkg/player`, same `Command` seam, **do not modify the
synchronous client** — the one-shot verbs depend on its behavior and 14 tests
pin it. New client needs a reader goroutine, `map[int64]chan message`, and an
event channel for `observe_property` (`time-pos`, `playlist-pos`, `pause`, `eof`).

**Windows caveat, already documented in `dial()`:** a named pipe opened via
`os.OpenFile` cannot have a blocked read interrupted by `Close()`. A wedged
daemon hangs the TUI. Cancellable reads need overlapped I/O — i.e. `go-winio`,
a Windows-only dependency. Decide this *before* writing the client, not after.

### D. Signal handling (T10.9) — fixes a pre-existing leak

**Measured: nothing in the tree traps SIGINT.** `grep -rn "signal.Notify"` returns
only a comment. This is not merely a TUI concern — today Ctrl+C during playback
skips urfave's `After()` hook, so a **private tor daemon and its temp
DataDirectory are orphaned and outlive the process**. T10.9 fixes that for the
CLI as well as restoring alt-screen/raw mode for the TUI.

**A second, separate exit-path question — observed 2026-09-28, detail in
TASKS.md T10.9:** mpv itself always outlives the process (T0.1(a), by design),
so the question isn't whether it keeps running but whether an already-playing
daemon can keep fetching once its egress path dies. Measured for the plain-HTTP
case: no — mpv's own network layer does not fall back to a direct connection
when the per-`loadfile` proxy dies mid-stream, it just hits EOF. The real
bridge's CONNECT/HTTPS path and a genuine multi-segment HLS fixture are still
unmeasured; don't generalize past the plain-HTTP result yet.

### E. Table reconciliation (T10.12) — mostly already satisfied

Phase 8 built this correctly: formatting lives in core, both front-ends feed off
the same `Store`. One concrete win: Phase 8's ~40-line `runeWidth`
East-Asian-width approximation in `pkg/core/table.go` can be **deleted** and
replaced with a real table the TUI library already links. Net code reduction,
and exactness we currently trade away.

**Corrected 2026-09-09 — the target is `rivo/uniseg`, not `mattn/go-runewidth`.**
This section originally said both candidates link `go-runewidth`. At the
versions measured in August they did; at current versions they do not — tcell
dropped `go-runewidth` between v2.8.1 and v2.13.10, so on the tview line T10.12
would have become a *new direct dependency* rather than a deletion, against the
one-dependency policy Part 2 defends. `rivo/uniseg v0.4.7` is linked by **all
three** candidate lines at the same version (table in Part 2), so retargeting
T10.12 at `uniseg` keeps it a deletion under every outcome of T10.3. On the
Charm line, `charmbracelet/x/ansi.StringWidth` (grapheme-cluster correct; what
lipgloss itself uses) is the equivalent.

### F. smoke.ps1 (T10.10)

All 35 cases currently run against a binary that prints the banner
unconditionally. Add: non-TTY bare invocation exits with help; verbs print no
banner (the `NotMatch` field added in T8.3 already supports this). **Never add a
case that launches the real TUI.**

---

## Part 2 — Research: T10.3 library data (measured 2026-08-24, re-measured 2026-09-09)

Measured properly — `go list -m all` overcounts by including test-only deps.
These are modules **actually linked**, via `go version -m` on a real build
(`CGO_ENABLED=0 -trimpath -s -w`).

**The August table measured a superseded major line.** It pinned bubbletea
v1.3.10 / lipgloss v1.1.0 / bubbles v1.0.0, but `bubbletea/v2` v2.0.9
(2026-08-19), `lipgloss/v2` v2.0.6 (2026-08-11) and `bubbles/v2` v2.2.1
(2026-08-24) had all shipped before it was written, and bubbletea v1's last
release is 2025-09-17. T10.3's literal instruction is "verify current
versions"; committing off the old table would pin a dead line. Re-measured
2026-09-09, same host, same flags, current versions:

| line | modules linked | binary | links `mattn/go-runewidth`? | links `rivo/uniseg`? |
|---|---|---|---|---|
| `github.com/charmbracelet/bubbletea` v1.3.10 + bubbles v1.0.0 + lipgloss v1.1.0 *(superseded)* | **25** | 2.93 MB | yes (v0.0.19) | yes (v0.4.7) |
| `charm.land/bubbletea/v2` v2.0.9 + bubbles/v2 v2.2.1 + lipgloss/v2 v2.0.6 | **19** | 3.31 MB | yes (v0.0.27) | yes (v0.4.7) |
| `rivo/tview` v0.42.0 + `gdamore/tcell/v2` **v2.13.10** (current) | **8** | 2.35 MB | **no** | yes (v0.4.7) |
| `rivo/tview` v0.42.0 + `gdamore/tcell/v2` v2.8.1 *(the August pin)* | 9 | — | yes (v0.0.16) | yes (v0.4.7) |

Two things the spike must know about the v2 Charm line, both measured:
**the import path is `charm.land/bubbletea/v2`, not `github.com/...`** — the
module's own `go.mod` declares `module charm.land/bubbletea/v2`, so the github
path is a hard compile error even though the proxy resolves it; and **`View()`
returns `tea.View`, not `string`**, so v1 examples do not port verbatim.

| | Bubble Tea v2 + bubbles + lipgloss | tview (tcell) |
|---|---|---|
| async model | `tea.Cmd`/`tea.Msg` — every slow call is already message-shaped | `QueueUpdateDraw()` from your own goroutines |
| prebuilt widgets | list, textinput, viewport, spinner, progress | list, form, flex, table, modal |
| notable transitive | `atotto/clipboard`, `sahilm/fuzzy`, `xo/terminfo`, `muesli/termenv` | all terminal-related, nothing surprising |

`rivo/uniseg` is common to every line and is T10.12's target (see E above).

**The tension to weigh at T10.3:** INSTRUCTIONS.md's dependency policy (written
in T8.1) states the shipping binary has *one* direct dependency, and that this
is "a property worth defending." Bubble Tea v2 takes that to ~20; tview to ~9.
Against that, INSTRUCTIONS.md *also* already recommends Bubble Tea on
architectural grounds — chainsmoke's state is a small explicit machine and every
slow thing (yt-dlp resolve, mpv IPC, Tor bootstrap) completes asynchronously,
which is exactly the shape `tea.Cmd`/`tea.Msg` wants.

`atotto/clipboard` is worth one look before committing: it shells out to
`xclip`/`pbcopy` on some platforms, which is a subprocess a privacy tool did not
ask for.

**Spike shape:** build the same carcass screen (results list + text input +
status bar, fixture data) in both, on both targets. One session, then commit.

---

## Part 3 — Research: proposed screen inventory (T10.5, your call)

You said *"primarily a full scale interface — search, likes, playlists, then
privacy-config, etc."* Mapped against the actual verb surface, that is:

| screen | backed by | seam status |
|---|---|---|
| **Search** | `Resolver.Search` → `Store.SaveResults` | ready (T3.3/T4.1 done) |
| **Likes** | `LikesURL` → `Collect` | ready (T8.2 done) |
| **Playlists** | `SetURL` → `Collect` | ready (T8.2 done) |
| **Results list** (shared by the three above) | `[]Track` from `Store` | ready — same slice `RenderResults` takes |
| **Now-playing + transport** | `Player.Command`, `NowPlaying` | **needs T10.7** (event-reading client) |
| **Queue** | mpv `playlist` property | needs T10.7 |
| **Privacy / config** | `AuditReport`, `Config`, `Drift`, `SaveConfig` | needs the A-refactor; model exists (Phase 7) |
| **Egress status bar** (T10.8, always visible) | posture field of `AuditReport` | needs the A-refactor |
| **Help overlay** | static | ready |
| **Get / download** | `Scrubber` | **deferred — Phase 6 is deferred**, leave it out |

Five notes for your decision (4 and 5 added 2026-09-09 — two choices the
inventory was making by implication):

1. Search/Likes/Playlists are **one screen with three sources**, not three
   screens — they all produce `[]Track` into the same store and the same list
   widget. Building them as one saves roughly two thirds of that work.
2. `get`/download should be **cut** from the inventory: Phase 6 is deferred, so
   the screen would front a stub.
3. The egress status bar is the one screen I would argue for regardless of what
   else ships — the one-shot CLI prints its "NOT anonymized" warning once and it
   scrolls away, which for a privacy tool is a real gap.
4. **The privacy/config screen cannot carry a live `--private` toggle.**
   Invariant #4 fixes the Store backend once in `wire()` "for the whole run",
   and a TUI run is hours. A mid-session flip is a lie, a runtime Store swap
   (unsupported, and the Store is invariant #1's chokepoint), or a no-op — and
   none of them remove permalinks already written by earlier searches. Make
   store mode **read-only** on that screen (backend + on-disk count from
   `AuditReport`) with `purge` as the action. If a toggle is genuinely wanted,
   specify it as purge-then-switch; anything else silently leaves the disk copy.
5. **Decide when the TUI calls `Up()`.** Note 3's always-on bar is only honest
   if egress is already verified — i.e. at launch. Under the CLI that is a
   three-second window; under a TUI the in-process CONNECT bridge (loopback,
   unauthenticated — `handle()` in `pkg/net/bridge.go` checks nothing) becomes
   a session-long local proxy. Options: `Up()` at launch (and consider a
   per-session token on the bridge before it is long-lived), or `Up()` on first
   use with the bar reading `DOWN` until then. Either is defensible; the plan
   currently picks the first without saying so.

---

## Execution order

Each step ends green (`go vet ./...`, `go test ./...`, smoke) so an interruption
never leaves unknown-state code.

1. **A — full split.** `Collect`/`EnqueueAhead`/`AuditReport`/index-vs-stdin
   return data; move formatting to `cmd/`. Update `pkg/core` tests to assert
   returned values instead of scraped output. *No TUI code yet.*
2. **T10.1 + T10.2** — entry point split, TTY guard, `--no-tui`. Update
   smoke.ps1 in the same step (T10.10) so the harness never hangs.
3. **T10.9** — signal handling. Do it here, before any alt-screen code exists,
   because it also closes the orphaned-tor leak on the CLI path.
4. **T10.3** — library spike, then commit to one.
5. **T10.4** — carcass: frame, header, status bar, panel switching, keybindings,
   fixture data. Runs on Windows, nothing wired.
6. **T10.8** — egress status bar against the real `AuditReport`.
7. **T10.6** — results list wired to `Store`, fed by search/likes/playlists.
8. **T10.7** — event-reading IPC client, then now-playing + transport.
   Decide the `go-winio` question at the top of this step.
9. **T10.11** — narrow-terminal degradation using the existing `OutputWidth()`;
   measured: the banner's widest printed line is **78 cols**.
10. **T10.12** — delete `runeWidth`, adopt `rivo/uniseg` (retargeted
    2026-09-09; see E — `go-runewidth` is no longer linked by the current
    tview line).

Steps 1–3 are worth doing even if the TUI were cancelled: the split makes
`pkg/core` testable as values, and T10.9 fixes a live leak.

---

## Verification

- `go vet ./...` clean and `go test ./...` green after **every** step; expect the
  suite to grow as `pkg/core` tests move from output-scraping to value assertions.
- `go test ./... -race` in WSL, after every concurrent step. **Measured:
  `-race` cannot run under native Windows** — `go: -race requires cgo`, no C
  toolchain — **but it runs in WSL on the same box** (gcc 15.2; verified
  2026-09-09, all packages green, no races in `pkg/net` today). That is the
  baseline the TUI's event loop and T10.7's second IPC client are built on top
  of, so run it locally rather than waiting on CI:
  `wsl -e bash -c "cd /mnt/d/sccli && go test ./... -race -count=1"`.
- `smoke.ps1` — must still pass, and must **not hang**. The T10.2 guard is what
  makes that true; verify by running the harness *before* wiring any TUI screen.
- Live over Tor, from inside WSL: search → likes → playlist → play, confirming
  the TUI and the one-shot path read the same store. (Note: WSL2 localhost
  forwarding for Tor on :9050 dropped mid-session on 2026-08-24; running from
  inside WSL is the workaround.)
- Terminal restore: verify Ctrl+C, `SIGTERM`, and a deliberate panic all leave a
  usable shell — and that a private tor is not orphaned by any of them.

## Still open

- **T10.3** — library. Spike immediately before step 4; data in Part 2.
- **T10.5** — screen inventory. Proposal in Part 3 for you to confirm or cut.
- **go-winio** on Windows for cancellable named-pipe reads (step 8).


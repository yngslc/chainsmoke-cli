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

Route: no args + TTY → TUI · `-h`/`--help` → banner + command table ·
`--version` → version line · any verb → today's one-shot path · `--no-tui` → help.

**T10.2 is a hard prerequisite for T10.10** — without the non-TTY guard,
`smoke.ps1`, CI and `chainsmoke | grep` hang forever.

Note `bannerCorruptsOutput` (added T8.3) becomes partly redundant: it currently
special-cases the completion protocol and `completion <shell>`. Once the banner
is behind a TTY+no-args check, those cases fall out naturally — but **keep the
`--generate-bash-completion` guard**, because that path can run against a TTY.

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

### E. Table reconciliation (T10.12) — mostly already satisfied

Phase 8 built this correctly: formatting lives in core, both front-ends feed off
the same `Store`. One concrete win — **both candidate libraries link
`mattn/go-runewidth`**, so Phase 8's ~40-line `runeWidth` East-Asian-width
approximation in `pkg/core/table.go` can be **deleted** and replaced with the
real table. Net code reduction, and exactness we currently trade away.

### F. smoke.ps1 (T10.10)

All 35 cases currently run against a binary that prints the banner
unconditionally. Add: non-TTY bare invocation exits with help; verbs print no
banner (the `NotMatch` field added in T8.3 already supports this). **Never add a
case that launches the real TUI.**

---

## Part 2 — Research: T10.3 library data (measured 2026-08-24)

Measured properly — `go list -m all` overcounts by including test-only deps.
These are modules **actually linked**, via `go version -m` on a real build
(`CGO_ENABLED=0 -trimpath -s -w`):

| | Bubble Tea + bubbles + lipgloss | tview (tcell) |
|---|---|---|
| modules linked | **25** | **9** |
| test binary size | 2.8M | 2.4M |
| versions | bubbletea v1.3.10, lipgloss v1.1.0, bubbles v1.0.0 | tview v0.42.0, tcell v2.8.1 |
| async model | `tea.Cmd`/`tea.Msg` — every slow call is already message-shaped | `QueueUpdateDraw()` from your own goroutines |
| prebuilt widgets | list, textinput, viewport, spinner, progress | list, form, flex, table, modal |
| notable transitive | `atotto/clipboard`, `sahilm/fuzzy`, `xo/terminfo`, `muesli/termenv` | all terminal-related, nothing surprising |

Both bring `mattn/go-runewidth` (see E above).

**The tension to weigh at T10.3:** INSTRUCTIONS.md's dependency policy (written
in T8.1) states the shipping binary has *one* direct dependency, and that this
is "a property worth defending." Bubble Tea takes that to ~26; tview to ~10.
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

Three notes for your decision:

1. Search/Likes/Playlists are **one screen with three sources**, not three
   screens — they all produce `[]Track` into the same store and the same list
   widget. Building them as one saves roughly two thirds of that work.
2. `get`/download should be **cut** from the inventory: Phase 6 is deferred, so
   the screen would front a stub.
3. The egress status bar is the one screen I would argue for regardless of what
   else ships — the one-shot CLI prints its "NOT anonymized" warning once and it
   scrolls away, which for a privacy tool is a real gap.

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
10. **T10.12** — delete `runeWidth`, adopt `go-runewidth`.

Steps 1–3 are worth doing even if the TUI were cancelled: the split makes
`pkg/core` testable as values, and T10.9 fixes a live leak.

---

## Verification

- `go vet ./...` clean and `go test ./...` green after **every** step; expect the
  suite to grow as `pkg/core` tests move from output-scraping to value assertions.
- `go test ./... -race` on Linux (WSL). **Measured: `-race` cannot run on this
  Windows box at all** — `go: -race requires cgo`, no C toolchain. Since T10.7
  and the Bubble Tea loop are the most concurrent code in the project, T9.2's
  Linux CI matters more here than anywhere else so far.
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


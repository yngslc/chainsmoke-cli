# Phase 0 — read-then-code

Companion to `TASKS.md` § Phase 0. Read the whole thing before touching a file;
the decisions in §2 are cheap to make now and expensive to retrofit.

**Verdict: Phase 0 does require writing.** Four of the five tasks are decisions,
but **T0.4 is real file surgery** (the `cmd/` + `pkg/` split) and **T0.3 is a
comment-only pass over `script.sh`**. So we stay on Phase 0 — we do not skip
ahead to Phase 1.

---

## 1. What is already done (do not redo)

| task | state | evidence |
|---|---|---|
| T0.4 — `git init` + first commit | **done** | commit `813652d` "chainsmoke v0.2 (git-test)" on `main`; remote `origin` = `github.com/yngslc/Chainsmoke-CLI.git` |
| T0.4 — `gofmt` commit | **no-op today** | `gofmt -l main.go` prints nothing. `SKILL.md`'s "gofmt flags main.go" gotcha is stale — fix that line. |
| T0.4 — the package split | **not started** | one 637-line `main.go`, no `cmd/`, no `pkg/` |
| T1.3 (Phase 1, already landed) | **done** | [main.go:135](main.go#L135) refuses URLs in argv; `smoke.ps1` case `url-in-argv` covers it |

Baseline captured before any change — this is what the refactor must reproduce
byte-for-byte in behavior:

```
smoke: all 11 cases pass          (go vet ./...  exit 0)
```

Two stale lines in `.claude/skills/run-chainsmoke/SKILL.md` while you're there:
it says "PASS x10 / all 10 cases pass" (it's 11 now) and it claims `gofmt -l .`
flags `main.go` (it doesn't).

---

## 2. The four decisions (no code — but they gate the code)

### T0.1 — Process model → **pick (c)**

Phase 10 already decided (2026-08-14) that a bare `chainsmoke` opens a TUI. That
means a long-lived foreground process exists *anyway*, which is exactly what
option (b)'s `chainsmoke daemon` was invented to provide. Building (b) means
running a second daemon on top of mpv's whose only job duplicates what the TUI
already does while open.

So: **(c) the TUI is the session owner, one-shot verbs use the runtime-dir
store.** Consequences you should write down now:

- **T4.1 is mandatory, not optional.** `chainsmoke search` → `chainsmoke play N`
  crosses two processes; without an on-disk result set in `$XDG_RUNTIME_DIR`
  that path cannot work at all. (This is why the `play-nocache` smoke case
  exists — the memStore is empty on every fresh invocation.)
- **T5.4's preload-ahead splits in two:** one-shot mode enqueues N+1
  synchronously before exiting; the TUI keeps the lookahead in memory.
- The `go func()` at [main.go:259-263](main.go#L259-L263) is **dead code under
  every reading of (c)** — the process exits before it can finish. Leave it for
  T5.4 to delete, but stop treating it as a working preload.

### T0.2 — Fail-closed egress → write this contract down

Paste this into `INSTRUCTIONS.md` (or a new `CONTRACTS.md`) as the normative text:

> **Egress contract.** No `Resolver` or `Player` method may execute unless
> `Net.Up()` has returned nil *in this process* and that `Up()` verified a live
> CONNECT through the bridge. "Proxy env is exported" is not the invariant;
> "no packet can leave except through the bridge" is. Enforcement is a
> `verified` flag set only by a successful `Up()`; every resolver and player
> entry point calls `Net.Assert()` first and returns `ErrEgressNotVerified`
> when it is unset. A direct connection is reachable **only** via an explicit
> `--allow-direct` flag, which prints the DIRECT warning on every invocation
> and forces the audit posture to `DIRECT ⚠`.

Why an assert and not just constructor ordering: [`newApp`](main.go#L507)
builds the resolver at line 515, but `Up()` is not called until the `play`
action at [main.go:172](main.go#L172) — and `search` never calls it at all
(that's T1.1). Construction order alone cannot carry this invariant without
splitting `newApp` into two phases; the flag-plus-assert is the cheap correct
version. The code lands in **T2.3**, not here.

**Decide the `--allow-direct` escape hatch now**, because it is load-bearing for
the harness: today `smoke.ps1`'s `play-nocache` case only passes because
[`Up()`](main.go#L303-L321) returns nil in DIRECT mode. The moment T2.3 makes it
fail-closed, that row's expectation flips — unless the escape hatch exists and
smoke passes it.

### T0.3 — `script.sh` → **freeze it**

It cannot run on this machine (no bash, WSL's `ext4.vhdx` is gone), so "working
prototype" is an untestable claim here. Keeping it alive costs you eight tasks
(T1.8–T1.15) you have no way to verify.

Writing required, comment-only:

1. Header block at the top of `script.sh`: *frozen reference prototype, not
   maintained, see TASKS.md T0.3*.
2. At each bug site, a `# KNOWN BUG (T1.8): ...` line — one per T1.8–T1.15.
3. `TASKS.md` § 1B header → "dropped per T0.3".

### T0.5 — Dev/test env → **GitHub Actions on `ubuntu-latest`**

`origin` is already a GitHub repo, so this is the only option with no hardware
and no yak-shave (WSL repair is the yak-shave). Build + `go vet` + unit tests
run there today; the mpv IPC integration tests come later behind a build tag
and will want `--ao=null`. The workflow file itself is **T9.2's** deliverable,
not Phase 0's — Phase 0 just records the decision.

---

## 3. T0.4 — where to add, where to rewrite

Rule for this task: **move code verbatim, no behavior change.** The proof is
that all 11 smoke cases still pass with their expectations untouched.

### 3.1 Target tree

```
chainsmoke/
├── cmd/chainsmoke/main.go      ADD   package main — CLI surface + wiring only
├── pkg/core/core.go            ADD   package core — Track, the 5 interfaces, ErrNotImplemented
├── pkg/net/bridge.go           ADD   package net
├── pkg/resolver/ytdlp.go       ADD   package resolver
├── pkg/player/mpv.go           ADD   package player
├── pkg/scrub/ffmpeg.go         ADD   package scrub
├── pkg/store/store.go          ADD   package store
├── main.go                     DELETE (fully consumed by the above)
└── main/                       DELETE (empty leftover directory, confusing next to cmd/)
```

**`pkg/core` is not in the layout comment at the top of `main.go`, and it is
required.** `Track` is returned by `Resolver` and stored by `Store`; the five
interfaces are consumed by `cmd` and, in Phase 10, by `pkg/tui`. Without a
shared leaf package you get `resolver` importing `store` just to name a type.
`core` imports nothing but `errors`, so no cycle is reachable from it. Update
the layout comment (lines 7–13) to list it.

`cmd/chainsmoke/` rather than `cmd/`: a package `main` sitting directly in
`cmd/` builds to a binary literally named `cmd`, and it blocks a second binary
later. Cost of the choice is one line in `smoke.ps1` (§3.4).

### 3.2 Source map — every line of `main.go` has a destination

| `main.go` | symbol | goes to | rewrite while moving |
|---|---|---|---|
| 1–26 | header comment | `cmd/chainsmoke/main.go` | add the `pkg/core` row to the layout list |
| 30–45 | imports | split per file | each new file gets its own import set |
| 47–48 | `BuildVersion` | `cmd` | none — `-X main.BuildVersion` still resolves (see §3.5) |
| 50–51 | `ErrNotImplemented` | **`pkg/core`** | → `core.ErrNotImplemented` at 3 call sites (481, 482, 487, 457) |
| 57–63 | `Track` | **`pkg/core`** | every `Track` / `[]Track` → `core.Track` |
| 69–109 | `Net` `Resolver` `Player` `Scrubber` `Store` | **`pkg/core`** | already exported; referenced as `core.Net`, … |
| 115–121 | `App` struct | `cmd` | fields retyped `core.Net`, `core.Resolver`, … |
| 123–142 | `resolveIndexOrStdin` | `cmd` | returns `core.Track` |
| 149–242 | `commands()` | `cmd` | nothing beyond `core.Track` |
| 244–266 | `enqueueAhead` | `cmd` | `[]core.Track`; the goroutine stays (T5.4 deletes it) |
| 268–272 | `audit()` | `cmd` | none |
| 280–294 | `torBridge` + consts | **`pkg/net`** | → `net.Bridge`; add `func New(override string) *Bridge` |
| 296–321 | `Up()` | `pkg/net` | receiver `(t *torBridge)` → `(b *Bridge)` |
| 323–325 | `Down()` | `pkg/net` | " |
| 327–351 | `Rotate()` | `pkg/net` | **line 331** `net.DialTimeout` → `stdnet.DialTimeout` |
| 353–361 | `SelfTest()` | `pkg/net` | " |
| 363–372 | `ProxyURL()` | `pkg/net` | " |
| 376–383 | `dialCheck` | `pkg/net`, unexported | **line 378** `net.DialTimeout` → `stdnet.DialTimeout` |
| 385–392 | `proxyHostPort` | `pkg/net`, unexported | none |
| 394–401 | `exportProxyEnv` | `pkg/net`, unexported | none |
| 403–405 | `isProxyURL` | `pkg/net`, **export as `IsProxyURL`** | the resolver calls it across the package boundary |
| 407–414 | `ytdlpResolver` | **`pkg/resolver`** | → `resolver.YtDlp`; `func New(n core.Net, impersonate string) *YtDlp` |
| 416–423 | `Meta` | `pkg/resolver` | `[]core.Track` |
| 425–454 | `Stream` | `pkg/resolver` | **line 432** `isProxyURL(p)` → `csnet.IsProxyURL(p)` |
| 456–457 | `Search` | `pkg/resolver` | `core.Track`, `core.ErrNotImplemented` |
| 459–483 | `mpvPlayer` | **`pkg/player`** | → `player.MPV`; `func New(sock string) *MPV`; `core.ErrNotImplemented` |
| 485–487 | `ffmpegScrubber` | **`pkg/scrub`** | → `scrub.FFmpeg`; `func New() *FFmpeg`; `core.ErrNotImplemented` |
| 489–499 | `memStore` | **`pkg/store`** | → `store.Memory`; `func NewMemory() *Memory`; `core.Track` |
| 501 | `diskStore` | **`pkg/store`** | → `store.Disk`; `func NewDisk() *Disk` |
| 507–520 | `newApp` | `cmd` | **two shadowing fixes — see §3.3** |
| 522–575 | `main()` + banner | `cmd` | none. The banner stays put; T10.1 moves it, not T0.4 |
| 581–584 | `ytdlpHint` | `cmd` | none |
| 586–591 | `printNumbered` | `cmd` | `[]core.Track` |
| 593–600 | `hasFlag` | `cmd` | none (T1.2 deletes it) |
| 602–615 | `flagValue` | `cmd` | none (T1.2 deletes it) |
| 617–635 | `isInt` / `atoi` | `cmd` | none (T1.5 deletes them) |
| 637 | `// test1test2` | **delete** | cruft |

Resulting dependency graph — acyclic, verify it stays that way:

```
core  ←  net  ←  resolver
  ↑       ↑         ↑
  └── store, player, scrub, cmd ──┘
```

### 3.3 The three rewrites that will actually bite you

**(a) `newApp` line 509 — a hard compile error, not a style nit.**

```go
var store Store = &memStore{}      // local `store` now shadows the store PACKAGE
if remember && !private {
    store = &diskStore{}           // → store.NewDisk() would not compile
}
```

Rename the local:

```go
var st core.Store = store.NewMemory()
if remember && !private {          // --private always wins
    st = store.NewDisk()
}
```

**(b) `newApp` line 508 — same trap, softer.** The local is named `net`, which
is now also a package name in `cmd`. Rename it:

```go
br := csnet.New(flagValue("--proxy", ""))
return &App{
    net:   br,
    res:   resolver.New(br, flagValue("--impersonate", "chrome")),
    play:  player.New(os.Getenv("XDG_RUNTIME_DIR") + "/chainsmoke/mpv.sock"),
    scrub: scrub.New(),
    store: st,
}
```

(The `App` *field* named `store` is fine — `a.store` is a selector, it never
collides with a package name.)

**(c) `pkg/net` imports stdlib `net`.** This is legal Go — a package's own name
is not an identifier inside its own files — but it reads terribly. Alias it:

```go
import (
    stdnet "net"
    "net/url"   // no alias needed, this one is `url`
)
```

and fix the two call sites (lines 331, 378). Also add the compile-time
assertions in each impl package so a signature drift fails at build time rather
than at wiring time:

```go
var _ core.Net = (*Bridge)(nil)   // and the equivalent in resolver/player/scrub/store
```

### 3.4 Non-Go files that must change with T0.4

1. **`.claude/skills/run-chainsmoke/smoke.ps1` line 42** — the build target:
   ```powershell
   & $GoExe build -ldflags '-X main.BuildVersion=smoke' -o chainsmoke.exe ./cmd/chainsmoke
   ```
   This is the *only* smoke change T0.4 is allowed to make. **The case table
   must not move** — 11 unchanged cases still passing is the proof the move was
   verbatim. If you find yourself editing a case row, you changed behavior.
2. **`SKILL.md`** — the two `go build … .` lines → `./cmd/chainsmoke`;
   `findstr /n "ErrNotImplemented" main.go` → `findstr /s /n "ErrNotImplemented" cmd\*.go pkg\*.go`;
   drop the stale gofmt gotcha; fix "10 cases" → 11.
3. **`TASKS.md`** — tick T0.4's git half, record the T0.1/T0.2/T0.3/T0.5 decisions.

### 3.5 Two traps worth naming

- **Do not run `go mod tidy` as part of T0.4.** `go.mod` currently carries
  direct requires on `github.com/rivo/tview v0.42.0` plus `tcell`,
  `golang.org/x/term`, and a stray `therecipe/qt` — nothing imports any of them.
  `tidy` would drop the lot, which silently pre-empts **T10.3** (the Bubble Tea
  vs tview decision). Decide T10.3 first, then tidy. (`therecipe/qt` is a Qt
  binding and is junk in a CLI project either way.)
- **`-X main.BuildVersion` survives the move** to `cmd/chainsmoke` — the linker
  special-cases the main package — but nothing currently *proves* it: the
  `version` smoke case matches only `chainsmoke version`, not the stamped value.
  Tighten it to `Match = 'chainsmoke version smoke'` so a broken `-ldflags` path
  fails loudly instead of silently shipping `development`.

---

## 4. Order of operations

Each step ends with `go build ./...` green. Commit at the marked points only.

| # | do | verify |
|---|---|---|
| 0 | *(already captured)* | `smoke.ps1` → 11/11, `go vet ./...` → 0 |
| 1 | delete the empty `main/` dir and `main.go:637` | build |
| 2 | add `pkg/core/core.go`; delete those decls from `main.go`; qualify the references | build |
| 3 | add `pkg/store/store.go` | build |
| 4 | add `pkg/net/bridge.go` (do the `stdnet` alias here) | build |
| 5 | add `pkg/scrub/ffmpeg.go` and `pkg/player/mpv.go` | build |
| 6 | add `pkg/resolver/ytdlp.go` (last — it depends on `core` + `net`) | build |
| 7 | move the remainder to `cmd/chainsmoke/main.go`, apply §3.3 (a) and (b), delete root `main.go` | build |
| 8 | update `smoke.ps1` line 42 | **`smoke.ps1` → 11/11, case table untouched** ← the gate |
| 9 | `gofmt -l .` → must print nothing | — |
| 10 | commit: *"T0.4: split main.go into cmd/ + pkg/ (no behavior change)"* | — |
| 11 | T0.3 annotations in `script.sh`; `TASKS.md` decision record | — |
| 12 | commit: *"T0.3: freeze script.sh as reference"* | — |

If step 8 fails on anything other than the build path, stop and diff — a
verbatim move cannot change a verb's exit code.

---

## 5. What Phase 0 does **not** touch

Resist all of these until their own task; each one would break the "no behavior
change" gate that makes step 8 meaningful:

- `hasFlag` / `flagValue` / `isInt` / `atoi` — T1.2, T1.5
- `search` not calling `net.Up()` — T1.1
- the `/tmp/in.m4a` at [main.go:205](main.go#L205) — T1.6
- `ytdlpHint` wrapping `ErrNotImplemented` — T1.7
- the unconditional banner print at [main.go:529-554](main.go#L529-L554) — T10.1
  (and per the standing note: the ASCII art stays, it moves, it is not stripped)
- `Up()`'s fail-open DIRECT fallback — T2.3, under the contract you wrote in §2

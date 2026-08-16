---
name: run-chainsmoke
description: Build, run, and test chainsmoke, the privacy-focused SoundCloud Go CLI in this repo. Use when asked to run, start, build, launch, or test the app, verify CLI behavior, or re-check the verb baseline after implementing a subsystem. Make sure to read the Gotchas section.
---

`chainsmoke` is a privacy-focused SoundCloud CLI over Tor. The entry point is
`cmd/chainsmoke/main.go` — CLI surface and wiring only — over `pkg/{core,net,
resolver,player,scrub,store}`, whose subsystems are still largely
`ErrNotImplemented` stubs. Drive it via
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
privoxy) are **not** needed to build or drive the carcass's still-stubbed verbs.
But two are now installed (used by the implemented raw `play` path):

- **mpv** `0.41.0` — `winget install --id shinchiro.mpv` → `C:\Program Files\MPV Player\` (added to the User PATH).
- **yt-dlp** `2026.07.04` — portable exe at `%LOCALAPPDATA%\chainsmoke-tools\bin\yt-dlp.exe` (on the User PATH).

`ffmpeg` ships inside the mpv build; `tor`/`privoxy`/`jq`/`socat` are still not
installed (the egress path falls back to a DIRECT connection with a warning).
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

Release-style version stamp (works, verified):

```powershell
go build -ldflags "-X main.BuildVersion=0.0.1" -o chainsmoke.exe .\cmd\chainsmoke
```

`chainsmoke.exe` lands in the repo root. Note `go build .\cmd\chainsmoke`
compiles only that package's dependency graph — run `go vet ./...` (as smoke.ps1
now does first) to catch a broken `pkg/*` that nothing imports yet.

## Run (agent path)

The driver builds and exercises the whole verb surface, asserting exit codes
and output against the current baseline:

```powershell
powershell -ExecutionPolicy Bypass -File .claude\skills\run-chainsmoke\smoke.ps1
# → PASS x16, "smoke: all 16 cases pass", exit 0
```

**When you implement a subsystem, flip the corresponding expectations in the
case table inside `smoke.ps1`** — it is the CLI-wiring regression harness.

Verified behavior baseline (what the carcass does today):

| invocation | exit | behavior |
|---|---|---|
| `.\chainsmoke.exe --help` | 0 | full command/flag table |
| `.\chainsmoke.exe --version` | 0 | `chainsmoke version development` |
| `.\chainsmoke.exe search kendrick` | 1 | DIRECT warning (T1.1: `Up()` now runs first), then bare `not implemented` — the `yt-dlp -U` hint is correctly suppressed for stubs (T1.7) |
| `.\chainsmoke.exe np`, `toggle` | 1 | `chainsmoke: not implemented` (no IPC daemon in the raw player) |
| `.\chainsmoke.exe play 1` | 1 | `net.Up` warns DIRECT (no proxy), then `no cached result #1 — run \`chainsmoke search <term>\` first`. No egress. |
| `.\chainsmoke.exe --proxy http://127.0.0.1:9 play 1` | 1 | `--proxy ... not reachable` — proves the flag reaches the bridge (T1.2) |
| `.\chainsmoke.exe daemon start` | 0 | `Ensure()` finds mpv on PATH and returns nil (raw player has no real daemon yet) |
| `"https://soundcloud.com/artist/track" \| .\chainsmoke.exe get` | 1 | stdin URL is read (no-URLs-in-argv contract), then dies in the scrubber stub |
| `.\chainsmoke.exe purge`, `.\chainsmoke.exe stop` | 0 | silent success (memStore purge / player.Down are real no-ops) |
| `.\chainsmoke.exe audit` | 1 | `Up()` runs first (T7.1 posture is probed, not printed from config), so it warns DIRECT, prints `egress: DIRECT (no proxy)` + runtime dir + version, then SelfTest fails with "egress is DIRECT" |
| `.\chainsmoke.exe seek` / `vol 900` | 1 | arity/range rejected before mpv ever sees the argument |
| `.\chainsmoke.exe bogus` | 3 | `No help topic for 'bogus'` |

Raw `play` is implemented: `net.Up` (detect Privoxy `:8118` / `--proxy`, else
DIRECT + warning) → `mpv`/`yt-dlp` presence check → resolve one stream via
`yt-dlp -g` → play with a foreground `mpv --no-video`. A real end-to-end play
needs a live SoundCloud URL and makes a network request (DIRECT unless
tor+privoxy are up), so it is not part of the offline smoke baseline.

## Direct invocation (dev loop)

Most PRs here implement a stub. Find what's left with:

```powershell
findstr /s /n "ErrNotImplemented" cmd\*.go pkg\*.go
```

Then `go vet ./...` + the smoke driver are the inner loop. Real egress needs
tor+privoxy on :9050/:8118; without them every verb falls back to DIRECT with a
warning.

## Test

```powershell
go vet ./...    # clean, exit 0
go test ./...   # → "?  chainsmoke  [no test files]" — no tests exist yet
```

## Gotchas
- **Build `.\cmd\chainsmoke`, never `.`** — see Build. This is the single most
  common way to think the tree is broken when it isn't.
- **`go build` alone does not prove the module is sound.** It only compiles the
  target's import graph, so an unreferenced or malformed `pkg/*` file slips
  through. `go vet ./...` is the check that covers everything; smoke.ps1 runs it
  before building for exactly this reason.
- **`search` no longer lies about yt-dlp.** `Search` is still
  `ErrNotImplemented`, and `core.YtdlpHint` now suppresses the "run: yt-dlp -U"
  advice for stub errors (T1.7). A bare `not implemented` is correct output.
- **`search` → `play N` cannot work yet, by design.** Under T0.1(a) the two
  verbs are separate short-lived processes and the default store is in-memory,
  so results never survive. This is T4.1 (runtime store), not a bug to chase.
- **Unknown verbs exit 3, not 1** (urfave/cli's `CommandNotFound` path). Every
  other failure is exit 1 — there is no MISUSE/NOT_FOUND differentiation yet.
- **The banner prints on stdout for every verb**, before argument parsing. It
  pollutes pipes and corrupts `--generate-bash-completion`. Known, deliberate,
  and owned by T10.1/T10.2 — do not "fix" it in an unrelated change.
- **The raw `play` path works on native Windows** (foreground
  `mpv --no-video <resolved-url>`, no IPC socket) with mpv+yt-dlp on PATH. It is
  not gated on Tor: it falls back to a DIRECT, non-anonymized connection with a
  warning (TASKS.md T0.2 wants that flipped to fail-closed before shipping).
- **mpv JSON-IPC works on BOTH targets** — unix socket under WSL, named pipe
  (`\\.\pipe\chainsmoke-mpv`) on native Windows; verified against mpv 0.41 with
  a live `get_property` round-trip. Same protocol, different dial. `core.SocketPath()`
  already returns the right endpoint per platform; the Phase-5 client must
  abstract the dial (T10.7) rather than assume `net.Dial("unix", ...)`.

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

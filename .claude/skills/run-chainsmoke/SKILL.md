---
name: run-chainsmoke
description: Build, run, and test chainsmoke, the privacy-focused SoundCloud Go CLI in this repo. Use when asked to run, start, build, launch, or test the app, verify CLI behavior, or re-check the verb baseline after implementing a subsystem. Make sure to read the Gotchas section.
---

`chainsmoke` is a privacy-focused SoundCloud CLI over Tor. The unit is
`main.go` — a Go carcass whose CLI surface (verbs, flags, stdin contract) is
real but whose subsystems (net/resolver/player/scrub) are `ErrNotImplemented`
stubs. Drive it via `.claude\skills\run-chainsmoke\smoke.ps1`, which builds
`chainsmoke.exe` and asserts the current behavior baseline. `script.sh` is the
Linux-only bash prototype — **unrunnable on this machine**, see Gotchas.

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

```powershell
$env:Path = "$env:LOCALAPPDATA\go-portable\go\bin;$env:Path"   # skip if go is on PATH
go build -o chainsmoke.exe .
.\chainsmoke.exe --version
# → chainsmoke version development
```

Release-style version stamp (works, verified):

```powershell
go build -ldflags "-X main.BuildVersion=0.0.1" -o chainsmoke.exe .
```

`chainsmoke.exe` lands in the repo root.

## Run (agent path)

The driver builds and exercises the whole verb surface, asserting exit codes
and output against the current baseline:

```powershell
powershell -ExecutionPolicy Bypass -File .claude\skills\run-chainsmoke\smoke.ps1
# → PASS x10, "smoke: all 10 cases pass", exit 0
```

**When you implement a subsystem, flip the corresponding expectations in the
case table inside `smoke.ps1`** — it is the CLI-wiring regression harness.

Verified behavior baseline (what the carcass does today):

| invocation | exit | behavior |
|---|---|---|
| `.\chainsmoke.exe --help` | 0 | full command/flag table |
| `.\chainsmoke.exe --version` | 0 | `chainsmoke version development` |
| `.\chainsmoke.exe search kendrick` | 1 | `not implemented` + a (misleading, see Gotchas) `yt-dlp -U` hint (Search still stubbed) |
| `.\chainsmoke.exe np`, `toggle` | 1 | `chainsmoke: not implemented` (no IPC daemon in the raw player) |
| `.\chainsmoke.exe play 1` | 1 | `net.Up` warns DIRECT (no proxy), then `no cached result #1` — the store is empty until you `search`. No egress. |
| `.\chainsmoke.exe daemon start` | 0 | `Ensure()` finds mpv on PATH and returns nil (raw player has no real daemon yet) |
| `"https://soundcloud.com/artist/track" \| .\chainsmoke.exe get` | 1 | stdin URL is read (no-URLs-in-argv contract), then dies in the scrubber stub |
| `.\chainsmoke.exe purge`, `.\chainsmoke.exe stop` | 0 | silent success (memStore purge / player.Down are real no-ops) |
| `.\chainsmoke.exe audit` | 1 | prints `egress: <down>` + version, then SelfTest reports DIRECT (no proxy to test) |
| `.\chainsmoke.exe bogus` | 3 | `No help topic for 'bogus'` |

Raw `play` is implemented: `net.Up` (detect Privoxy `:8118` / `--proxy`, else
DIRECT + warning) → `mpv`/`yt-dlp` presence check → resolve one stream via
`yt-dlp -g` → play with a foreground `mpv --no-video`. A real end-to-end play
needs a live SoundCloud URL and makes a network request (DIRECT unless
tor+privoxy are up), so it is not part of the offline smoke baseline.

## Direct invocation (dev loop)

Most PRs here implement a stub. Find what's left with:

```powershell
findstr /n "ErrNotImplemented" main.go
```

Then `go vet` + the smoke driver are the inner loop; there is no way to reach
SoundCloud until `pkg/net` + `pkg/resolver` exist.

## Test

```powershell
go vet ./...    # clean, exit 0
go test ./...   # → "?  chainsmoke  [no test files]" — no tests exist yet
```

## Gotchas
- **The `search` failure hint lies while the resolver is a stub.** `Search` is
  still `ErrNotImplemented`; the error appends "SoundCloud extractor may be
  stale — run: yt-dlp -U", but the real cause is the stub. Don't chase yt-dlp.
  (`Stream`, which raw `play` uses, IS implemented.)
- **`audit` reporting `egress: <down>` is not a regression** — `audit` never
  calls `net.Up()`, so the proxy field is empty by design (today).
- **Unknown verbs exit 3, not 1** (urfave/cli's `CommandNotFound` path).
- **`gofmt -l .` flags `main.go` as-written.** Don't let an editor reformat
  the whole file inside an unrelated change; `go vet` is clean.
- **The raw `play` path DOES work on native Windows** (foreground
  `mpv --no-video <resolved-url>`, no IPC socket) with mpv+yt-dlp on PATH — it
  is not gated on Tor (falls back to a DIRECT, non-anonymized connection with a
  warning; TASKS.md T0.2 wants that flipped to fail-closed before shipping).
  What still **won't** work on native Windows is the *IPC daemon* model —
  `np`/`toggle`/`seek` and the future Phase-5 player speak mpv JSON-IPC over a
  unix socket at `$XDG_RUNTIME_DIR/chainsmoke/mpv.sock`; on Windows mpv IPC uses
  named pipes, so that development targets Linux/WSL.

## Troubleshooting

- **`go : The term 'go' is not recognized`**: no Go on PATH. Run the portable
  bootstrap in Prerequisites (needs no admin), or pass the toolchain
  explicitly: `smoke.ps1 -GoExe <path-to-go.exe>`.
- **`wsl -d Ubuntu-26.04` fails mounting `ext4.vhdx` (ERROR_PATH_NOT_FOUND)**:
  the distro's virtual disk is gone; every WSL-based path (including
  `bash.exe`) is dead until the distro is reinstalled. Don't attempt a WSL
  repair as part of running this app.

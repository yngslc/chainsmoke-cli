---
name: project-measurement-environment
description: What this box can actually measure — WSL (go, gcc, mpv, tor, ffmpeg) is reachable from the debater sandbox; offline tor via DisableNetwork; rig gotchas that cost a run.
metadata:
  type: project
---

Environment facts, verified 2026-09-09 and re-verified/extended 2026-10-03.

- **WSL is reachable from the debater's sandbox** (`wsl -e bash -lc ...` worked
  2026-10-03, though the 10-01 reviewer's isolated worktree refused it). WSL
  carries go1.26.5, gcc, mpv 0.41, **tor 0.4.9.11 at /usr/sbin/tor**, ffmpeg,
  `script`, `setsid`. So `-race`, Linux signal/process-group tests, ptys
  (`script -qc`), and HLS fixtures (`ffmpeg -f lavfi -i sine=... -f hls`) are all
  local. Cross-build rigs from Windows with `GOOS=linux` and run via /mnt/c.
- **Tor can be measured offline**: add `DisableNetwork 1` to the torrc — no
  connections at all. `__OwningControllerProcess <pid>` works from torrc (exit
  ~12s after owner dies).
- **The GitHub remote is private and unverifiable**: no `gh`, unauthenticated API
  404s. Treat "CI is green" as unverified.
- **Rig gotchas** (each cost a wasted run on 2026-10-03):
  - The Bash tool's heredoc collapses `\\` to `\` in Go source — build
    Windows pipe names with `string(rune(92))`.
  - In MSYS PATH use `/c/Users/...`, never `C:/...` (the colon splits it).
  - This sandbox's process tree inherits Ctrl+C processing OFF; a real
    terminal has it ON. Call `SetConsoleCtrlHandler(NULL, FALSE)` in a rig
    before testing console Ctrl+C.
  - A CREATE_NEW_CONSOLE+HideWindow child that only reads its console mode
    hung twice (unresolved); the same launcher worked for longer-lived children.
  - `tasklist` output is cp866-garbled; "no tasks" still reads as garbage.

**Why:** privacy findings here are settled by breaking something and watching.
Knowing what is measurable decides whether a claim gets a counter-measurement
or a MEASUREMENT-tier filing.

**How to apply:** build scratch Go rigs in the session scratchpad with
`replace chainsmoke => D:/sccli`, loopback origin + fake proxy, scratch pipes
only (never `\\.\pipe\chainsmoke-mpv`), and kill every mpv/tor afterwards.
Never add a smoke case that egresses; never run `-tags integration`.

Related: [[project-house-style-measured-claims]], [[project-spawn-vs-reuse]]

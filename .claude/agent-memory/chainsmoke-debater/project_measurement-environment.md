---
name: project-measurement-environment
description: What this box can actually measure — git-bash vs WSL paths, the three-fake offline egress rig, mutation-testing recipe, and rig gotchas that cost a run.
metadata:
  type: project
---

Environment facts, verified 2026-09-09, re-verified/extended 2026-10-03 and
2026-10-19.

- **The Bash tool is Git Bash, not WSL.** The repo is `/d/sccli` there;
  `/mnt/d/sccli` does not exist and `cd` to it fails. **`git` works directly in
  that shell** — the run-chainsmoke SKILL's "git is not on the native Windows
  PATH, reach it through WSL" is wrong for this agent and cost a wasted call.
  For WSL use `wsl.exe -e bash -lc 'export PATH=$PATH:/usr/local/go/bin; ...'`,
  where the repo is `/mnt/d/sccli` and the scratchpad is `/mnt/c/Users/...`.
- **WSL carries** go1.26.5, gcc, mpv 0.41, tor 0.4.9.11 (`/usr/sbin/tor`),
  ffmpeg, `script`, `setsid`. So `-race`, Linux signal/process-group tests,
  ptys and HLS fixtures are all local. `-race` on a tar-copy of the tree under
  /mnt/c works and took ~2 min.
- **Tor can be measured offline**: `DisableNetwork 1` in the torrc.
  `__OwningControllerProcess <pid>` works from torrc (tor exits ~12s after the
  owner dies).
- **The three-fake offline egress rig** (built 2026-10-19, zero egress, all in
  the session scratchpad):
  - *fake CONNECT proxy* — answers every `CONNECT` with `200 Connection
    established` and dials nothing. Feeds `--proxy http://127.0.0.1:P`.
  - *fake SOCKS5* — accepts user/pass, answers CONNECT with success, never
    dials. Feeds `--tor-socks 127.0.0.1:P`. Logs the isolation credentials, so
    it also shows which circuit a tunnel keys.
  - *fake yt-dlp.exe* first on PATH — dumps `os.Args` and returns canned
    flat-playlist JSON for `-J` / a fake URL for `-g`. **`resolver.minimalEnv`
    strips the child env**, so the fake must log to a hard-coded absolute path,
    not to `$CH_ARGV_LOG` or `os.TempDir()`.
  - *scratch Go module* with `replace chainsmoke => D:/sccli` to drive
    `csnet.StartConnectBridge` and `cb.Close()` directly.
  Together these settle posture, argv-exposure, bridge-relay and Close()
  questions without a circuit.
- **Mutation-testing recipe** (settles "each test was mutation-checked"):
  `tar --exclude=.git --exclude='*.exe' -cf - . | (cd $SCR/mut && tar -xf -)`
  (~1.2 MB), edit the copy with a small Python heredoc, `go test ./...` there.
  Never mutate the repo.
- **Rig gotchas** (each cost a wasted run):
  - The Bash tool's heredoc collapses `\\` to `\` in Go source — build Windows
    pipe names with `string(rune(92))`.
  - In MSYS PATH use `/c/Users/...`, never `C:/...` (the colon splits it).
  - `powershell.exe -File` needs **forward slashes**; bash eats the backslashes
    and PowerShell then errors in cp866.
  - Foreground `sleep` is blocked; long runs go to `run_in_background`.
  - `get` with a stdin URL does **not** call the resolver (it builds a Track
    straight from the line) — use `play` if you need yt-dlp to actually run.
  - This sandbox's process tree inherits Ctrl+C processing OFF; call
    `SetConsoleCtrlHandler(NULL, FALSE)` in a rig before testing console Ctrl+C.
  - `tasklist` output is cp866-garbled; prefer `Get-Process` via powershell.exe.
- **The GitHub remote is private and unverifiable**: no `gh`, unauthenticated
  API 404s. Treat "CI is green" as unverified.

**Why:** privacy findings here are settled by breaking something and watching.
Knowing what is measurable decides whether a claim gets a counter-measurement
or a MEASUREMENT-tier filing.

**How to apply:** build scratch rigs in the session scratchpad, scratch pipes
only (never `\\.\pipe\chainsmoke-mpv` unless you tear the daemon down after),
and kill every mpv/tor/fake afterwards. Never add a smoke case that egresses;
never run `-tags integration`.

Related: [[project-house-style-measured-claims]], [[project-spawn-vs-reuse]]


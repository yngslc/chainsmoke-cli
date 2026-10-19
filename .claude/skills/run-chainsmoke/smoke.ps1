# smoke.ps1 — build + drive the chainsmoke CLI.
#
# Usage (from the repo root):
#   powershell -ExecutionPolicy Bypass -File .claude\skills\run-chainsmoke\smoke.ps1
#
# Exit 0  = build succeeded and every case matched the expected baseline.
# Exit 1  = one or more cases diverged.
# Exit 2  = no Go toolchain / build failure.
#
# The case table below encodes the CURRENT behavior of the CLI. Only `get`
# still dies in a stub (pkg/scrub). When you change a verb's behavior, flip the
# corresponding expectations here — this script is the CLI-wiring regression
# harness, not a test of SoundCloud, and it must never leave the box.

param([string]$GoExe)

$repoRoot = Resolve-Path (Join-Path $PSScriptRoot '..\..\..')
Set-Location $repoRoot

# Compose PATH the way a fresh terminal would. A parent shell started before a
# tool install (e.g. mpv/yt-dlp persisted to the User PATH) carries a stale env
# block; reading Machine+User from the registry picks up those installs so the
# player/resolver tool checks reflect reality.
$env:Path = [System.Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' +
            [System.Environment]::GetEnvironmentVariable('Path', 'User')

# -- locate go.exe: PATH first, then the portable bootstrap from SKILL.md ----
if (-not $GoExe) {
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) { $GoExe = $cmd.Source }
    elseif (Test-Path "$env:LOCALAPPDATA\go-portable\go\bin\go.exe") {
        $GoExe = "$env:LOCALAPPDATA\go-portable\go\bin\go.exe"
    }
    else {
        Write-Host 'FATAL: no go.exe on PATH and no portable install at %LOCALAPPDATA%\go-portable.'
        Write-Host 'Run the bootstrap in .claude\skills\run-chainsmoke\SKILL.md (Prerequisites).'
        exit 2
    }
}

# -- vet + build -------------------------------------------------------------
# vet first: the whole module, so a broken pkg/* is caught here rather than
# silently excluded because only ./cmd/chainsmoke gets compiled.
& $GoExe vet ./...
if ($LASTEXITCODE -ne 0) { Write-Host 'FATAL: go vet failed (output above).'; exit 2 }

# The entry point moved to cmd/chainsmoke when main.go was split into cmd/+pkg/
# (T0.4). Building '.' now fails with "no Go files in <repo root>".
& $GoExe build -ldflags '-X main.BuildVersion=smoke' -o chainsmoke.exe .\cmd\chainsmoke
if ($LASTEXITCODE -ne 0) { Write-Host 'FATAL: go build failed (output above).'; exit 2 }

# Cases that must observe "no anonymized egress available" point at a port
# nothing can be listening on. Without this the baseline silently depends on
# whether THIS machine happens to be running tor — it passed on a box with no
# tor and failed the moment one appeared, which is a harness bug, not a
# regression. Port 1 (tcpmux) is never bound in practice.
# EVERY --allow-direct row needs it too: with a tor listening, Up() verifies a
# real CONNECT through it to api-v2.soundcloud.com before the verb even fails,
# so six "offline" rows were each opening a Tor stream (2026-10-01 review, N-5).
# And since 2026-10-03 a non-default --tor-socks never spawns a private tor,
# which is what makes $noTor offline on a machine that has tor on PATH too.
$noTor = @('--tor-socks', '127.0.0.1:1')

# Every audit case below reads and writes a saved config (T7.1). Point
# XDG_CONFIG_HOME at a sandbox for the whole run: a regression harness must
# never touch — or overwrite — the developer's real %APPDATA%\chainsmoke
# config, and the first audit cases assert the "no config yet" branch, which a
# real config on this machine would silently break.
$env:XDG_CONFIG_HOME = Join-Path ([System.IO.Path]::GetTempPath()) 'chainsmoke-smoke-cfg'
if (Test-Path $env:XDG_CONFIG_HOME) { Remove-Item -Recurse -Force $env:XDG_CONFIG_HOME }
# The same goes for both stores: the purge cases below would otherwise wipe the
# developer's real results cache and --remember state, and play-nocache would
# depend on whatever a real search left behind. core.RuntimeDir/StateDir honour
# these on Windows too. (The mpv pipe name is fixed, so the daemon cases still
# share the real one — purge-first stops any daemon left running.)
$env:XDG_RUNTIME_DIR = Join-Path ([System.IO.Path]::GetTempPath()) 'chainsmoke-smoke-run'
$env:XDG_STATE_HOME  = Join-Path ([System.IO.Path]::GetTempPath()) 'chainsmoke-smoke-state'
foreach ($d in $env:XDG_RUNTIME_DIR, $env:XDG_STATE_HOME) { if (Test-Path $d) { Remove-Item -Recurse -Force $d } }

# -- case table: args / stdin / expected exit / expected output substring ----
$cases = @(
    # The banner (framed by '=====') prints only for the SoundCloud/session verbs,
    # and only on a TTY (2026-09-28). This harness reads through a pipe, so no
    # case may carry it; the NotMatch on version/help/search-flags/daemon-start
    # pins that — all four printed it before. Which verbs get it on a terminal is
    # TestBannerClassification's job, not this harness's: a pipe cannot tell.
    @{ Name = 'version';      Args = @('--version');         Exit = 0; Match = 'chainsmoke version'; NotMatch = '=====' },
    @{ Name = 'help';         Args = @('--help');            Exit = 0; Match = 'privacy-focused SoundCloud CLI'; NotMatch = '=====' },
    # `get` is Hidden until download + scrub lands: help must not offer a verb
    # that cannot succeed (2026-10-19 review, Fix 10). get-stdin below proves
    # it still runs.
    @{ Name = 'help-no-get';  Args = @('--help');            Exit = 0; Match = 'COMMANDS:'; NotMatch = 'get, dl' },
    # Bare invocation down a pipe: today the command table. T10.1/T10.2 route a
    # bare TTY run to the TUI but must keep THIS (help, exit 0, no hang), so the
    # case exists before they land and they flip nothing (09/09 review, A-5).
    @{ Name = 'bare-pipe';    Args = @();                    Exit = 0; Match = 'COMMANDS:'; NotMatch = '=====' },
    # T3.3: `search` is implemented now, so a bare search would leave the box —
    # and with no tor up, leave it UNANONYMIZED. Pointing --impersonate at a
    # target yt-dlp cannot load kills it before it opens a socket, and $noTor
    # keeps Up() from probing through a running tor: the case stays offline AND
    # proves the resolver injected our flag (T3.4).
    @{ Name = 'search-flags'; Args = @($noTor + @('--allow-direct', '--impersonate', 'smoke-invalid', 'search', 'roshina')); Exit = 1; Match = 'rejected --impersonate'; NotMatch = '=====' },
    # With mpv+yt-dlp installed, play/daemon clear the tool checks. `play 1` then
    # fails in the store (empty cache) BEFORE any egress — deterministic, no
    # network. `daemon start` = Ensure() = mpv present, so it now succeeds.
    # (If mpv is NOT installed, daemon-start instead fails with 'mpv not found
    # on PATH' — reinstall per SKILL.md or adjust that row.)
    # The sandboxed stores above start empty, so play-nocache no longer needs a
    # purge to be deterministic. purge-first stays for the daemon: it stops any
    # mpv left on the shared pipe, which np-none below depends on.
    @{ Name = 'purge-first';  Args = @('purge');               Exit = 0; Match = '' },
    @{ Name = 'play-nocache'; Args = @($noTor + @('--allow-direct', 'play', '1')); Exit = 1; Match = 'no cached result' },
    # T4.3: --private is memory-only, so a cache written by a normal run must be
    # invisible to it. Nothing to set up — a fresh process has no memory.
    @{ Name = 'private-nocache'; Args = @($noTor + @('--private', '--allow-direct', 'play', '1')); Exit = 1; Match = 'no cached result' },
    # URL in argv is refused (invariant #3 / T1.3): must arrive on stdin.
    # Match the invariant itself, not an incidental word in the remediation
    # text — the previous 'stdin' match broke when the message was reworded.
    @{ Name = 'url-in-argv';  Args = @($noTor + @('--allow-direct', 'play', 'https://soundcloud.com/a/b')); Exit = 1; Match = 'not accepted in argv' },
    # The three play cases above fail on their argument, and play now checks it
    # BEFORE Ensure() — so none of them may have started a daemon (2026-10-01).
    @{ Name = 'np-none';      Args = @('np');                Exit = 1; Match = 'unreachable' },
    @{ Name = 'daemon-start'; Args = @('daemon', 'start');   Exit = 0; Match = ''; NotMatch = '=====' },
    # T5.2/T5.5: `np` is a real IPC round-trip now, so it runs AFTER daemon-start
    # — otherwise the result depends on whether a daemon happened to be left
    # over from an earlier run. An idle daemon is a state, not an error.
    @{ Name = 'np-idle';      Args = @('np');                Exit = 0; Match = 'nothing playing' },
    @{ Name = 'stop-ok';      Args = @('stop');              Exit = 0; Match = '' },
    # T5.5: `daemon stop` really quits mpv. Until 2026-09-28 it exited 0 and
    # left the daemon running (Down() returned before dialling in a fresh
    # process), which is also why every smoke run used to leave an mpv behind.
    # np-gone is the proof: the pipe no longer answers. stop-ok sits ABOVE this
    # pair because it needs a live daemon.
    @{ Name = 'daemon-stop';  Args = @('daemon', 'stop');    Exit = 0; Match = '' },
    @{ Name = 'np-gone';      Args = @('np');                Exit = 1; Match = 'unreachable' },
    # stdin contract: URLs never ride in argv; `get` reads one line from stdin,
    # then dies in the scrubber stub. Proves the stdin path is wired.
    @{ Name = 'get-stdin';    Args = @($noTor + @('--allow-direct', 'get')); Exit = 1; Match = 'not implemented'; Stdin = 'https://soundcloud.com/artist/track' },
    # With no daemon left, purge's player teardown finds nothing — not an error.
    @{ Name = 'purge-ok';     Args = @('purge');             Exit = 0; Match = '' },
    # urfave/cli exits 3 (not 1) for unknown commands
    @{ Name = 'unknown-verb'; Args = @('bogus');             Exit = 3; Match = 'No help topic' },

    # -- Phase 1A regression cases (T1.1-T1.7) --------------------------------
    # T1.1: search establishes the chokepoint before the resolver runs. With no
    # proxy up, that surfaces as the DIRECT warning — proof Up() was called at
    # all, which it previously was not for this verb. The invalid impersonate
    # target is what keeps the (now real) resolver call from reaching the wire.
    @{ Name = 'search-egress'; Args = @($noTor + @('--allow-direct', '--impersonate', 'smoke-invalid', 'search', 'roshina')); Exit = 1; Match = 'NOT anonymized' },
    # T1.2: --proxy is actually wired into the bridge now. Port 9 is the
    # discard port: reliably refuses, no network egress, deterministic offline.
    # Before this fix the flag was parsed by urfave/cli and then ignored.
    # Bare `play` (no index): an index is checked against the store BEFORE Up()
    # since 2026-10-03, and the sandboxed cache is empty, so `play 1` would stop
    # at "no cached result" without ever reaching the bridge.
    @{ Name = 'proxy-wired';   Args = @('--proxy', 'http://127.0.0.1:9', 'play'); Exit = 1; Match = 'not reachable' },
    # T1.5: strconv.Atoi rejects an out-of-range index instead of the old
    # accumulator silently wrapping it to a negative number.
    @{ Name = 'index-overflow'; Args = @($noTor + @('--allow-direct', 'play', '99999999999999999999')); Exit = 1; Match = 'out of range' },
    # Input validation on the transport verbs: mpv must never receive an empty
    # or non-numeric argument (cli-developer: "validate user input early").
    @{ Name = 'seek-arity';    Args = @('seek');              Exit = 1; Match = 'usage' },
    @{ Name = 'vol-range';     Args = @('vol', '900');        Exit = 1; Match = '0-100' },

    # -- Phase 2 T2.3: fail-closed egress -------------------------------------
    # THE case for T0.2. With no tor and no --allow-direct, an egress verb must
    # REFUSE, not warn-and-continue. If this ever goes green-by-warning again,
    # the fail-closed invariant has silently regressed. Bare `play` for the
    # same reason as proxy-wired: Up() comes before the stdin URL.
    @{ Name = 'failclosed';    Args = @($noTor + @('play')); Exit = 1; Match = 'refusing to run unanonymized' },
    @{ Name = 'failclosed-search'; Args = @($noTor + @('search', 'x')); Exit = 1; Match = 'refusing to run unanonymized' },

    # -- Phase 7 T7.1/T7.2: audit + saved config ------------------------------
    # These six run in order and share the sandboxed config dir set above:
    # 1-3 assert the "nothing saved yet" branch, 4 writes it, 5-6 read it back.
    #
    # audit is the ONE verb that still runs when egress is down. Every other
    # verb aborts on Up()'s refusal because it is about to send traffic; a
    # diagnostic that refuses to run exactly when the thing it diagnoses is
    # broken would be useless. Exit stays 1 — the report is extra, not a pass.
    @{ Name = 'audit-down';    Args = @($noTor + @('audit')); Exit = 1; Match = 'DOWN' },
    # ...but --save refuses on a setup that never carried traffic. Recording it
    # would teach every future audit to call this failure "expected".
    @{ Name = 'audit-nosave';  Args = @($noTor + @('audit', '--save')); Exit = 1; Match = 'NOT saving' },
    # DIRECT is a different state from DOWN and must not be reported as the
    # same thing: here traffic IS permitted to leave, just not anonymized.
    @{ Name = 'audit-noconfig'; Args = @($noTor + @('--allow-direct', 'audit')); Exit = 1; Match = 'audit --save' },
    # --save records the posture the probe PROVED, so this lands as DIRECT even
    # though nothing on the command line says so. Exit stays 1: adding a flag
    # must never turn a failing audit into a passing one.
    @{ Name = 'audit-save';    Args = @($noTor + @('--allow-direct', 'audit', '--save')); Exit = 1; Match = 'saved:' },
    @{ Name = 'audit-match';   Args = @($noTor + @('--allow-direct', 'audit')); Exit = 1; Match = 'this run matches it' },
    # The case the whole feature exists for: a flag left in a shell alias. The
    # probe is unchanged; only the comparison notices the drift.
    @{ Name = 'audit-drift';   Args = @($noTor + @('--allow-direct', '--impersonate', 'smoke-drift', 'audit')); Exit = 1; Match = 'impersonate' },

    # -- Phase 8 T8.2: likes / playlist ---------------------------------------
    # All of these fail in argument validation, BEFORE Net.Up() — so they prove
    # the wiring while staying offline. That ordering is itself the assertion:
    # a typo must not cost a Tor circuit.
    @{ Name = 'likes-url';     Args = @('likes', 'https://soundcloud.com/x/likes'); Exit = 1; Match = 'not accepted in argv' },
    @{ Name = 'likes-badhandle'; Args = @('likes', 'evil.com');   Exit = 1; Match = 'letters, digits' },
    @{ Name = 'playlist-arity'; Args = @('playlist', 'someuser'); Exit = 1; Match = '<user>/<set>' },
    # ...and a VALID handle must then hit the fail-closed gate rather than the
    # network, proving Up() still runs ahead of the resolver (T1.1/T0.2).
    @{ Name = 'likes-failclosed'; Args = @($noTor + @('likes', 'someuser')); Exit = 1; Match = 'refusing to run unanonymized' },
    @{ Name = 'playlist-failclosed'; Args = @($noTor + @('playlist', 'someuser/someset')); Exit = 1; Match = 'refusing to run unanonymized' },

    # -- Phase 8 T8.3: shell completion ---------------------------------------
    # The banner assertions are the point of these three. Every line of the
    # wordmark becomes a completion candidate on the protocol path, and lands in
    # ~/.bashrc on the script path — so "no banner" is the feature, and NotMatch
    # is how it gets tested.
    @{ Name = 'comp-protocol'; Args = @('--generate-bash-completion'); Exit = 0; Match = 'likes'; NotMatch = '=====' },
    @{ Name = 'comp-bash';     Args = @('completion', 'bash'); Exit = 0; Match = '_chainsmoke_complete'; NotMatch = '=====' },
    @{ Name = 'comp-fish';     Args = @('completion', 'fish'); Exit = 0; Match = 'complete -c chainsmoke'; NotMatch = '=====' },
    @{ Name = 'comp-badshell'; Args = @('completion', 'tcsh'); Exit = 1; Match = 'unknown shell' },
    # Last, always: a safety net so no run leaves an mpv behind, even if a case
    # above fails between daemon-start and daemon-stop.
    @{ Name = 'teardown';      Args = @('daemon', 'stop');   Exit = 0; Match = '' }
)

# -- run ---------------------------------------------------------------------
$failed = 0
foreach ($case in $cases) {
    if ($null -ne $case.Stdin) {
        $out = ($case.Stdin | & .\chainsmoke.exe $case.Args 2>&1 | ForEach-Object { "$_" }) -join "`n"
    } else {
        $out = (& .\chainsmoke.exe $case.Args 2>&1 | ForEach-Object { "$_" }) -join "`n"
    }
    $code = $LASTEXITCODE
    # NotMatch is as load-bearing as Match here: several of the properties this
    # harness guards are ABSENCES — no banner in a completion script, no URL in
    # output — and "the string is missing" is not something a positive match can
    # express.
    $ok = ($code -eq $case.Exit) -and
          ($case.Match -eq '' -or $out -like "*$($case.Match)*") -and
          ($null -eq $case.NotMatch -or $case.NotMatch -eq '' -or $out -notlike "*$($case.NotMatch)*")
    if ($ok) {
        Write-Host ('PASS  {0,-13} exit={1}' -f $case.Name, $code)
    } else {
        Write-Host ('FAIL  {0,-13} exit={1} want={2} match="{3}"' -f $case.Name, $code, $case.Exit, $case.Match)
        Write-Host ('      output: ' + $out)
        $failed++
    }
}

if ($failed) { Write-Host "smoke: $failed case(s) FAILED"; exit 1 }
Write-Host "smoke: all $($cases.Count) cases pass"

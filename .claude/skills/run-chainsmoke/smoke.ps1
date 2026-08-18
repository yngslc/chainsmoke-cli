# smoke.ps1 — build + drive the chainsmoke CLI.
#
# Usage (from the repo root):
#   powershell -ExecutionPolicy Bypass -File .claude\skills\run-chainsmoke\smoke.ps1
#
# Exit 0  = build succeeded and every case matched the expected baseline.
# Exit 1  = one or more cases diverged.
# Exit 2  = no Go toolchain / build failure.
#
# The case table below encodes the CURRENT behavior of the carcass: most verbs
# hit ErrNotImplemented stubs. When you implement a subsystem (pkg/net,
# pkg/resolver, pkg/player, ...), flip the corresponding expectations here —
# this script is the CLI-wiring regression harness, not a test of SoundCloud.

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

# -- case table: args / stdin / expected exit / expected output substring ----
$cases = @(
    @{ Name = 'version';      Args = @('--version');         Exit = 0; Match = 'chainsmoke version' },
    @{ Name = 'help';         Args = @('--help');            Exit = 0; Match = 'privacy-focused SoundCloud CLI' },
    @{ Name = 'search-stub';  Args = @('--allow-direct', 'search', 'roshina'); Exit = 1; Match = 'not implemented' },
    # With mpv+yt-dlp installed, play/daemon clear the tool checks. `play 1` then
    # fails in the store (empty cache) BEFORE any egress — deterministic, no
    # network. `daemon start` = Ensure() = mpv present, so it now succeeds.
    # np stays stubbed (raw play has no IPC daemon to query).
    # (If mpv/yt-dlp are NOT installed, play/daemon instead fail with 'mpv not
    # found on PATH' — reinstall per SKILL.md or adjust these two rows.)
    @{ Name = 'play-nocache'; Args = @('--allow-direct', 'play', '1'); Exit = 1; Match = 'no cached result' },
    # URL in argv is refused (invariant #3 / T1.3): must arrive on stdin.
    # Match the invariant itself, not an incidental word in the remediation
    # text — the previous 'stdin' match broke when the message was reworded.
    @{ Name = 'url-in-argv';  Args = @('--allow-direct', 'play', 'https://soundcloud.com/a/b'); Exit = 1; Match = 'not accepted in argv' },
    @{ Name = 'np-stub';      Args = @('np');                Exit = 1; Match = 'not implemented' },
    @{ Name = 'daemon-start'; Args = @('daemon', 'start');   Exit = 0; Match = '' },
    # stdin contract: URLs never ride in argv; `get` reads one line from stdin,
    # then dies in the scrubber stub. Proves the stdin path is wired.
    @{ Name = 'get-stdin';    Args = @('--allow-direct', 'get'); Exit = 1; Match = 'not implemented'; Stdin = 'https://soundcloud.com/artist/track' },
    @{ Name = 'purge-ok';     Args = @('purge');             Exit = 0; Match = '' },
    @{ Name = 'stop-ok';      Args = @('stop');              Exit = 0; Match = '' },
    # urfave/cli exits 3 (not 1) for unknown commands
    @{ Name = 'unknown-verb'; Args = @('bogus');             Exit = 3; Match = 'No help topic' },

    # -- Phase 1A regression cases (T1.1-T1.7) --------------------------------
    # T1.1: search establishes the chokepoint before the resolver runs. With no
    # proxy up, that surfaces as the DIRECT warning — proof Up() was called at
    # all, which it previously was not for this verb.
    @{ Name = 'search-egress'; Args = @('--allow-direct', 'search', 'roshina'); Exit = 1; Match = 'NOT anonymized' },
    # T1.2: --proxy is actually wired into the bridge now. Port 9 is the
    # discard port: reliably refuses, no network egress, deterministic offline.
    # Before this fix the flag was parsed by urfave/cli and then ignored.
    @{ Name = 'proxy-wired';   Args = @('--proxy', 'http://127.0.0.1:9', 'play', '1'); Exit = 1; Match = 'not reachable' },
    # T1.5: strconv.Atoi rejects an out-of-range index instead of the old
    # accumulator silently wrapping it to a negative number.
    @{ Name = 'index-overflow'; Args = @('--allow-direct', 'play', '99999999999999999999'); Exit = 1; Match = 'out of range' },
    # Input validation on the transport verbs: mpv must never receive an empty
    # or non-numeric argument (cli-developer: "validate user input early").
    @{ Name = 'seek-arity';    Args = @('seek');              Exit = 1; Match = 'usage' },
    @{ Name = 'vol-range';     Args = @('vol', '900');        Exit = 1; Match = '0-100' },

    # -- Phase 2 T2.3: fail-closed egress -------------------------------------
    # THE case for T0.2. With no tor and no --allow-direct, an egress verb must
    # REFUSE, not warn-and-continue. If this ever goes green-by-warning again,
    # the fail-closed invariant has silently regressed.
    @{ Name = 'failclosed';    Args = @('play', '1');         Exit = 1; Match = 'refusing to run unanonymized' },
    @{ Name = 'failclosed-search'; Args = @('search', 'x');   Exit = 1; Match = 'refusing to run unanonymized' }
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
    $ok = ($code -eq $case.Exit) -and ($case.Match -eq '' -or $out -like "*$($case.Match)*")
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

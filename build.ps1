# build.ps1 — release builds for chainsmoke (TASKS.md T8.3).
#
# Usage (from the repo root):
#   powershell -ExecutionPolicy Bypass -File .\build.ps1              # dev build, this host
#   powershell -ExecutionPolicy Bypass -File .\build.ps1 -Version 0.1.0
#   powershell -ExecutionPolicy Bypass -File .\build.ps1 -Version 0.1.0 -All
#
# -All cross-compiles every supported target into .\dist. No cgo is involved on
# any of them, so no cross toolchain is needed — plain `go build` is enough.

param(
    [string]$Version = 'development',
    [switch]$All,
    [string]$GoExe
)

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

if (-not $GoExe) {
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) { $GoExe = $cmd.Source }
    elseif (Test-Path "$env:LOCALAPPDATA\go-portable\go\bin\go.exe") {
        $GoExe = "$env:LOCALAPPDATA\go-portable\go\bin\go.exe"
    }
    else { Write-Host 'FATAL: no go.exe (see .claude\skills\run-chainsmoke\SKILL.md)'; exit 2 }
}

# The three flags below are the release build, and each earns its place:
#
#   -trimpath   MEASURED 2026-08-24: without it the binary embeds absolute
#               source paths — "D:/sccli/pkg/core/browse.go" and 14 more. That
#               discloses the builder's drive letter and directory layout (and,
#               through module-cache paths, their username) to anyone holding
#               the binary, which is a poor look on a tool people choose for its
#               threat model. With it: zero hits — and Build() checks that.
#   -s -w       drop the symbol table and DWARF. Smaller download, and one less
#               copy of the build environment to read out of the file.
#   -X          stamps the version `--version` and `audit` report. Without it
#               every release calls itself "development".
#
# CGO_ENABLED=0 is set for every target, not just Linux: it is what makes the
# Linux build a single static ELF with no libc to match (verified with `file`),
# and it keeps the build reproducible across machines.
$ldflags = "-s -w -X main.BuildVersion=$Version"

# CI asserts -trimpath on its artifacts; these are the ones that actually ship
# (09/09 debate, #7). Go embeds paths with forward slashes, and the module cache
# sits under the user profile, so the username is part of what would leak.
$buildPaths = @((Get-Location).Path, $env:USERPROFILE) | ForEach-Object { $_ -replace '\\', '/' }

function Build($goos, $goarch, $out) {
    $target = if ($goos) { "$goos/$goarch" } else { 'this host' }
    Write-Host ("building {0,-14} -> {1}" -f $target, $out)
    $env:CGO_ENABLED = '0'
    $env:GOOS = $goos
    $env:GOARCH = $goarch
    & $GoExe build -trimpath -ldflags $ldflags -o $out .\cmd\chainsmoke
    if ($LASTEXITCODE -ne 0) { Write-Host "FATAL: build failed for $goos/$goarch"; exit 1 }
    # UTF-8, not ASCII: Go embeds paths as UTF-8, and ASCII decoding turns a
    # Cyrillic username into '?' so it never matches (CH-DEBATE-2026-10-03 #5).
    $bytes = [System.Text.Encoding]::UTF8.GetString([System.IO.File]::ReadAllBytes((Resolve-Path $out)))
    foreach ($p in $buildPaths) {
        if ($bytes.IndexOf($p, [System.StringComparison]::OrdinalIgnoreCase) -ge 0) {
            Write-Host "FATAL: $out embeds the build path $p - is -trimpath still set?"; exit 1
        }
    }
}

# vet the whole module first: `go build .\cmd\chainsmoke` compiles only that
# package's import graph, so a broken pkg/* would otherwise ship unnoticed.
& $GoExe vet ./...
if ($LASTEXITCODE -ne 0) { Write-Host 'FATAL: go vet failed'; exit 2 }

try {
    if (-not $All) {
        Build $null $null '.\chainsmoke.exe'   # host defaults
        & .\chainsmoke.exe --version | Select-Object -Last 1
        exit 0
    }

    New-Item -ItemType Directory -Force .\dist | Out-Null
    Build 'windows' 'amd64' ".\dist\chainsmoke_${Version}_windows_amd64.exe"
    Build 'windows' 'arm64' ".\dist\chainsmoke_${Version}_windows_arm64.exe"
    Build 'linux'   'amd64' ".\dist\chainsmoke_${Version}_linux_amd64"
    Build 'linux'   'arm64' ".\dist\chainsmoke_${Version}_linux_arm64"
    Build 'darwin'  'amd64' ".\dist\chainsmoke_${Version}_darwin_amd64"
    Build 'darwin'  'arm64' ".\dist\chainsmoke_${Version}_darwin_arm64"

    Write-Host ''
    Get-ChildItem .\dist | ForEach-Object {
        '{0,-46} {1,8:N0} KB' -f $_.Name, ($_.Length / 1KB)
    }
    Write-Host ''
    Write-Host 'Reminder: darwin builds are compiled but NOT tested - chainsmoke has'
    Write-Host 'no macOS dev target, and terminalWidth() falls back to $COLUMNS/80 there.'
}
finally {
    # A cross-build must not leave the shell pinned to another platform: a later
    # `go test` in the same session would try to run a linux binary on Windows.
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}

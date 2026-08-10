<#
.SYNOPSIS
    Cross-platform build script for the Beuvian Desktop Agent (Windows host).

.DESCRIPTION
    Produces agent binaries for Windows, macOS, and Linux from a single host.
    Go's cross-compiler makes this possible without any target toolchain, which is
    why the agent is written in Go and why CGO_ENABLED is forced to 0 — a single
    cgo dependency would end cross-compilation and require six CI runners.

    Version metadata is injected via -ldflags so a shipped binary can report its
    own provenance. Agents run on machines we cannot inspect, so a bug report is
    only actionable if the binary states which commit built it.

.PARAMETER Target
    Which platforms to build: All, Windows, Darwin, Linux, or Host.

.PARAMETER Version
    Semantic version to stamp. Defaults to the current git tag, else "dev".

.PARAMETER OutputDir
    Where binaries are written. Defaults to ./dist.

.EXAMPLE
    ./scripts/build-agent.ps1
    Builds for the host platform only — the fast inner-loop case.

.EXAMPLE
    ./scripts/build-agent.ps1 -Target All -Version v0.1.0
    Builds all six release artifacts with checksums.
#>
[CmdletBinding()]
param(
    [ValidateSet('All', 'Windows', 'Darwin', 'Linux', 'Host')]
    [string]$Target = 'Host',

    [string]$Version = '',

    [string]$OutputDir = ''
)

# Fail fast on any error. Without this, a failed build would be followed by a
# cheerful "done" message and a stale or missing binary.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$RepoRoot = Split-Path -Parent $PSScriptRoot
$AgentDir = Join-Path $RepoRoot 'agent'

if ([string]::IsNullOrWhiteSpace($OutputDir)) {
    $OutputDir = Join-Path $RepoRoot 'dist'
}

# The ldflags paths must match the shared module's import path exactly; a typo
# fails silently and leaves the binary reporting "dev".
$VersionPkg = 'github.com/bhuvan0808/beuviancode/shared/version'

function Invoke-Git {
    <#
        Runs git and returns trimmed stdout, or $null on any failure.

        Wrapping every git call is not defensive padding: under
        $ErrorActionPreference = 'Stop', Windows PowerShell 5.1 converts a native
        command's stderr output into a terminating NativeCommandError. That means a
        perfectly ordinary "not a git repository" message would abort the whole
        build rather than being handled. Lowering the preference for the duration
        of the call is the only reliable way to treat git's exit code as the signal
        instead of its stderr.
    #>
    param([Parameter(Mandatory)][string[]]$Arguments)

    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $output = & git @Arguments 2>$null
        if ($LASTEXITCODE -eq 0 -and $output) {
            return ($output | Out-String).Trim()
        }
        return $null
    }
    catch {
        return $null
    }
    finally {
        $ErrorActionPreference = $previous
    }
}

function Resolve-BuildMetadata {
    <#
        Derives version, commit, and date from git.

        The date comes from the commit rather than "now", so rebuilding the same
        commit produces an identical binary. Using the wall clock would make every
        build byte-different and defeat reproducibility.

        A missing git, a shallow clone, an archive download, or a not-yet-initialised
        repository are all ordinary situations. Each degrades to the "dev" defaults
        with a warning, because refusing to build the agent over absent version
        metadata would be a poor trade.
    #>
    $meta = @{
        Version = 'dev'
        Commit  = 'none'
        Date    = 'unknown'
    }

    if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
        Write-Warning 'git not found; building without version metadata'
        return $meta
    }

    if (-not (Invoke-Git @('-C', $RepoRoot, 'rev-parse', '--git-dir'))) {
        Write-Warning 'not a git repository; building without version metadata'
        return $meta
    }

    if ($v = Invoke-Git @('-C', $RepoRoot, 'describe', '--tags', '--always', '--dirty')) { $meta.Version = $v }
    if ($c = Invoke-Git @('-C', $RepoRoot, 'rev-parse', 'HEAD')) { $meta.Commit = $c }
    if ($d = Invoke-Git @('-C', $RepoRoot, 'show', '-s', '--format=%cI', 'HEAD')) { $meta.Date = $d }

    return $meta
}

function Get-BuildMatrix {
    param([string]$Which)

    $all = @(
        @{ GOOS = 'windows'; GOARCH = 'amd64'; Ext = '.exe' }
        @{ GOOS = 'windows'; GOARCH = 'arm64'; Ext = '.exe' }
        @{ GOOS = 'darwin';  GOARCH = 'amd64'; Ext = '' }
        @{ GOOS = 'darwin';  GOARCH = 'arm64'; Ext = '' }
        @{ GOOS = 'linux';   GOARCH = 'amd64'; Ext = '' }
        @{ GOOS = 'linux';   GOARCH = 'arm64'; Ext = '' }
    )

    switch ($Which) {
        'All'     { return $all }
        'Windows' { return $all | Where-Object { $_.GOOS -eq 'windows' } }
        'Darwin'  { return $all | Where-Object { $_.GOOS -eq 'darwin' } }
        'Linux'   { return $all | Where-Object { $_.GOOS -eq 'linux' } }
        'Host'    {
            $hostArch = if ([Environment]::Is64BitOperatingSystem) { 'amd64' } else { '386' }
            if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { $hostArch = 'arm64' }
            return @(@{ GOOS = 'windows'; GOARCH = $hostArch; Ext = '.exe' })
        }
    }
}

# ---------------------------------------------------------------------------

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Go is not installed or not on PATH. Install Go 1.26 or later: https://go.dev/dl/'
}

$goVersion = (go version)
Write-Host "Toolchain : $goVersion" -ForegroundColor Cyan

$meta = Resolve-BuildMetadata
if (-not [string]::IsNullOrWhiteSpace($Version)) { $meta.Version = $Version }

Write-Host "Version   : $($meta.Version)" -ForegroundColor Cyan
Write-Host "Commit    : $($meta.Commit)" -ForegroundColor Cyan
Write-Host "Output    : $OutputDir" -ForegroundColor Cyan
Write-Host ''

New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null

$ldflags = @(
    '-w', '-s'
    "-X $VersionPkg.Version=$($meta.Version)"
    "-X $VersionPkg.Commit=$($meta.Commit)"
    "-X $VersionPkg.Date=$($meta.Date)"
) -join ' '

$matrix = Get-BuildMatrix -Which $Target
$built = @()
$failed = @()

# GOWORK=off so the build resolves through backend/agent go.mod replace directives
# exactly as CI and Docker do. Building via the workspace here could succeed while
# a clean clone of one module fails.
$originalGoWork = $env:GOWORK
$env:GOWORK = 'off'

try {
    foreach ($t in $matrix) {
        $name = "beuvian-agent-$($t.GOOS)-$($t.GOARCH)$($t.Ext)"
        $outPath = Join-Path $OutputDir $name

        Write-Host "Building $name ... " -NoNewline

        $env:GOOS = $t.GOOS
        $env:GOARCH = $t.GOARCH
        $env:CGO_ENABLED = '0'

        Push-Location $AgentDir
        try {
            # -trimpath strips build-host paths, so the binary does not disclose
            # local directory names and is identical across machines.
            $output = go build -trimpath -ldflags $ldflags -o $outPath ./cmd/beuvian-agent 2>&1
            if ($LASTEXITCODE -ne 0) {
                Write-Host 'FAILED' -ForegroundColor Red
                Write-Host $output -ForegroundColor Red
                $failed += $name
            }
            else {
                $size = [math]::Round((Get-Item $outPath).Length / 1MB, 2)
                Write-Host "ok ($size MB)" -ForegroundColor Green
                $built += $outPath
            }
        }
        finally {
            Pop-Location
        }
    }
}
finally {
    # Always restore the environment; leaving GOOS set would silently break the
    # developer's next command in the same shell.
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
    if ($null -eq $originalGoWork) {
        Remove-Item Env:GOWORK -ErrorAction SilentlyContinue
    } else {
        $env:GOWORK = $originalGoWork
    }
}

# Checksums, so a user can verify a download. Only for multi-target builds; the
# inner-loop host build does not need them.
if ($built.Count -gt 1) {
    $sumFile = Join-Path $OutputDir 'SHA256SUMS'
    Write-Host ''
    Write-Host 'Writing SHA256SUMS' -ForegroundColor Cyan
    $lines = foreach ($f in $built) {
        $h = (Get-FileHash -Algorithm SHA256 -Path $f).Hash.ToLower()
        "$h  $(Split-Path -Leaf $f)"
    }
    # UTF8 without BOM: sha256sum on Linux and macOS cannot parse a BOM.
    [IO.File]::WriteAllLines($sumFile, $lines)
}

Write-Host ''
if ($failed.Count -gt 0) {
    Write-Host "$($failed.Count) build(s) failed: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host "Built $($built.Count) binary/binaries into $OutputDir" -ForegroundColor Green

# A host build is meant to be run immediately, so prove it works.
if ($Target -eq 'Host' -and $built.Count -eq 1) {
    Write-Host ''
    & $built[0] -version
}

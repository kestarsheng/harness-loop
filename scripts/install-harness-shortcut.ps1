param([string]$Exe = "")

$ErrorActionPreference = "Stop"

if (-not $Exe) {
    $root = Split-Path -Parent $PSScriptRoot
    $Exe = Join-Path $root "harness-cli.exe"
}
if (-not (Test-Path $Exe)) {
    throw "harness-cli.exe not found: $Exe (build it with: go build -o harness-cli.exe ./cmd/harness-cli)"
}
$root = Split-Path -Parent (Resolve-Path $Exe)

$profilePath = $PROFILE.CurrentUserAllHosts
$profileDir = Split-Path -Parent $profilePath
if (-not (Test-Path $profileDir)) {
    New-Item -ItemType Directory -Path $profileDir -Force | Out-Null
}
if (-not (Test-Path $profilePath)) {
    New-Item -ItemType File -Path $profilePath -Force | Out-Null
}

$block = @"
# Harness Loop short commands (installed by scripts/install-harness-shortcut.ps1)
`$env:HARNESS_STACK_ROOT = "$root"
function global:/harness { & "$Exe" @args }
function global:harness { & "$Exe" @args }
"@

$current = Get-Content -Path $profilePath -Raw -ErrorAction SilentlyContinue
if ($current -notmatch "install-harness-shortcut") {
    Add-Content -Path $profilePath -Value $block
}

Write-Host "installed /harness and harness -> $Exe"
Write-Host "open a new terminal, then run: /harness loop start"
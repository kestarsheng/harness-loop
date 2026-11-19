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

# Install a copy under LOCALAPPDATA so the short command works from any directory
# and does not depend on the repository path (which may contain non-ASCII chars).
$installDir = Join-Path $env:LOCALAPPDATA "HarnessLoop\bin"
if (-not (Test-Path $installDir)) {
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
}
$installedExe = Join-Path $installDir "harness.exe"
Copy-Item -Path $Exe -Destination $installedExe -Force

# Add the install dir to the *user* PATH (idempotent, system PATH untouched).
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($null -eq $userPath) { $userPath = "" }
$parts = $userPath -split ';' | Where-Object { $_ -ne '' }
if ($parts -notcontains $installDir) {
    $newPath = (@($parts) + $installDir) -join ';'
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Write-Host "added to user PATH: $installDir"
}

# Rewrite the profile block idempotently (old blocks are removed first).
$profilePath = $PROFILE.CurrentUserAllHosts
$profileDir = Split-Path -Parent $profilePath
if (-not (Test-Path $profileDir)) {
    New-Item -ItemType Directory -Path $profileDir -Force | Out-Null
}
if (-not (Test-Path $profilePath)) {
    New-Item -ItemType File -Path $profilePath -Force | Out-Null
}
Copy-Item -Path $profilePath -Destination ($profilePath + ".bak") -Force

$current = Get-Content -Path $profilePath -Raw -ErrorAction SilentlyContinue
if ($null -eq $current) { $current = "" }
$current = [regex]::Replace($current, "(?s)# >>> harness-loop shortcut >>>.*?# <<< harness-loop shortcut <<<", "")
$current = [regex]::Replace($current, "(?s)# Harness Loop short commands.*\z", "")
$current = $current.TrimEnd()

$block = @"
# >>> harness-loop shortcut >>>
`$env:HARNESS_STACK_ROOT = "$root"
function global:/harness { & "$installedExe" @args }
function global:harness { & "$installedExe" @args }
# <<< harness-loop shortcut <<<
"@

Set-Content -Path $profilePath -Value ($current + "`r`n`r`n" + $block) -Encoding UTF8

Write-Host "installed /harness -> $installedExe"
Write-Host "installed harness  -> $installedExe (user PATH)"
Write-Host "current window: . `$PROFILE.CurrentUserAllHosts"
Write-Host "new windows: /harness loop start   (PowerShell)"
Write-Host "any shell:  harness loop start     (harness is on user PATH)"
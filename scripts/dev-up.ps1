# dev-up.sh / dev-down.sh 的 PowerShell 薄包装（Windows 专用）。
# 真正的逻辑都在 bash 脚本里，这里只负责找到 Git Bash 并转发参数。
#
# 用法：
#   pwsh scripts/dev-up.ps1                    # 等价于 scripts/dev-up.sh
#   pwsh scripts/dev-up.ps1 -Install -Rebuild  # = --install --rebuild
#   pwsh scripts/dev-up.ps1 -Down              # 等价于 scripts/dev-down.sh
#   pwsh scripts/dev-up.ps1 -Down -KeepDb      # = dev-down.sh --keep-db

[CmdletBinding()]
param(
    [switch]$Down,
    [switch]$KeepDb,
    [switch]$Install,
    [switch]$Rebuild,
    [switch]$NoBuild,
    [switch]$Demo
)

$ErrorActionPreference = 'Stop'

function Find-Bash {
    $cmd = Get-Command bash.exe -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    $candidates = @(
        "$env:ProgramFiles\Git\bin\bash.exe",
        "${env:ProgramFiles(x86)}\Git\bin\bash.exe",
        "$env:LOCALAPPDATA\Programs\Git\bin\bash.exe"
    )
    foreach ($c in $candidates) {
        if ($c -and (Test-Path $c)) { return $c }
    }
    return $null
}

$bash = Find-Bash
if (-not $bash) {
    Write-Host "[fail] 未找到 bash.exe，请先安装 Git for Windows。" -ForegroundColor Red
    exit 1
}

$repoRoot = Split-Path -Parent $PSScriptRoot
if ($Down) {
    $script = Join-Path $repoRoot 'scripts/dev-down.sh'
    $leftover = @()
    if ($Install) { $leftover += '--install' }
    if ($Rebuild) { $leftover += '--rebuild' }
    if ($NoBuild) { $leftover += '--no-build' }
    if ($leftover.Count -gt 0) {
        Write-Host "[warn] -Down 模式下忽略：$($leftover -join ', ')" -ForegroundColor Yellow
    }
    $bashArgs = @($script)
    if ($KeepDb) { $bashArgs += '--keep-db' }
}
else {
    $script = Join-Path $repoRoot 'scripts/dev-up.sh'
    $bashArgs = @($script)
    if ($Install) { $bashArgs += '--install' }
    if ($Rebuild) { $bashArgs += '--rebuild' }
    if ($NoBuild) { $bashArgs += '--no-build' }
    if ($Demo) { $bashArgs += '--demo' }
    if ($KeepDb) { Write-Host "[warn] -KeepDb 只对 -Down 生效" -ForegroundColor Yellow }
}

if (-not (Test-Path $script)) {
    Write-Host "[fail] 缺少脚本：$script" -ForegroundColor Red
    exit 1
}

& $bash @bashArgs
exit $LASTEXITCODE

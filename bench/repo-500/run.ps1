# P2 索引线全量压测入口（Windows PowerShell）。
#
# 两道闸门先过，才有数字可看：
#   1. -mode check：500 个夹具文件必须与 snapshot.json 的 sha256 逐条一致；
#   2. GOMAXPROCS 与 hardware.json 对齐（不匹配会照跑，但结果标 hardware_match=false）。
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$fixture = Join-Path $repoRoot 'bench\repo-500\testdata\repo'

if (-not $env:HARNESS_TEST_PG_DSN) {
    throw '请先设置 HARNESS_TEST_PG_DSN（专用 PG 实例，勿用共享库）'
}

$env:GOMAXPROCS = '8'
Set-Location $repoRoot

Write-Host '== snapshot check =='
go run ./bench/repo-500/cmd -mode check -dir $fixture
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host '== bench run =='
$benchArgs = @(
    './bench/repo-500/cmd'
    '-mode', 'run'
    '-dir', $fixture
    '-dsn', $env:HARNESS_TEST_PG_DSN
    '-tenant', 'tenant-bench-repo500'
    '-project', 'proj-bench-repo500'
    '-concurrency', '4'
    '-batch', '32'
    '-insert-batch', '200'
    '-probe-interval', '500ms'
    '-probe-query', 'handler'
    '-sample', '20'
    '-out', (Join-Path $repoRoot 'bench\repo-500\last-run.json')
)
go run @benchArgs @args
exit $LASTEXITCODE

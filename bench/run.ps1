# bench/run.ps1
# 一键运行全量 benchmark（带 -benchmem），结果保存到 docs/bench-result.txt
# 用法：powershell -File bench/run.ps1
param(
    [int]$Benchtime = 1000,        # 单个 benchmark 最短时间（ms）
    [int]$Count = 1,                # 重复次数
    [string]$Tag = "manual",        # 结果标签
    [string]$Output = "docs/bench-result-$Tag.txt"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# 包列表
$packages = @(
    "./db",
    "./rpc",
    "./websocket",
    "./golua",
    "./localtimer",
    "./vars",
    "./list",
    "./syncmap",
    "./random",
    "./ranking",
    "./corectx",
    "./metrics",
    "./swd"
)

Write-Host "=== TouchGoCore 全量 Benchmark ===" -ForegroundColor Cyan
Write-Host "Benchtime: ${Benchtime}ms  Count: $Count  Tag: $Tag"
Write-Host "Output:    $Output"
Write-Host ""

# 准备输出
$outputPath = Join-Path $root $Output
$outputDir = Split-Path -Parent $outputPath
if (-not (Test-Path $outputDir)) { New-Item -ItemType Directory -Path $outputDir -Force | Out-Null }

# 头部
@"
=== TouchGoCore Benchmark Run ===
Date:    $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')
Go:      $(go version)
Tag:     $Tag
Benchtime: ${Benchtime}ms
Count:    $Count

"@ | Set-Content -Path $outputPath -Encoding UTF8

foreach ($pkg in $packages) {
    Write-Host "[Running] $pkg" -ForegroundColor Yellow
    go test -bench=. -benchtime=${Benchtime}ms -count=$Count -benchmem -run=^$ $pkg 2>&1 |
        Tee-Object -Append -FilePath $outputPath
    Write-Host ""
}

Write-Host "=== 完成 ===" -ForegroundColor Green
Write-Host "结果已写入: $outputPath"

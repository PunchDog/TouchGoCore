# bench/race.ps1
# 一键运行全量测试（带 -race），验证并发安全
# 用法：powershell -File bench/race.ps1
param(
    [string]$Output = "docs/race-result.txt"
)

$ErrorActionPreference = "Continue"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$packages = @(
    ".", "./db", "./rpc", "./websocket", "./golua", "./localtimer",
    "./vars", "./list", "./syncmap", "./random", "./ranking",
    "./corectx", "./metrics", "./ini", "./gin",
    "./example/gatewayserver/gatepb"
)

$outputPath = Join-Path $root $Output
$outputDir = Split-Path -Parent $outputPath
if (-not (Test-Path $outputDir)) { New-Item -ItemType Directory -Path $outputDir -Force | Out-Null }

@"
=== TouchGoCore Race Detector Run ===
Date: $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')
Go:   $(go version)

"@ | Set-Content -Path $outputPath -Encoding UTF8

$failed = @()
foreach ($pkg in $packages) {
    Write-Host "[Race] $pkg" -ForegroundColor Yellow
    $result = go test -race -count=1 -timeout=120s $pkg 2>&1
    $result | Tee-Object -Append -FilePath $outputPath | Out-Null
    if ($LASTEXITCODE -ne 0) {
        $failed += $pkg
    }
}

if ($failed.Count -gt 0) {
    Write-Host ""
    Write-Host "=== 失败的包 ===" -ForegroundColor Red
    $failed | ForEach-Object { Write-Host "  $_" -ForegroundColor Red }
} else {
    Write-Host ""
    Write-Host "=== 全部通过 ===" -ForegroundColor Green
}
Write-Host "结果: $outputPath"

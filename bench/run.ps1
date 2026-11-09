# bench/run.ps1
# 一键运行全量 benchmark（带 -benchmem），结果保存到 docs/bench-result.txt
# 用法：powershell -File bench/run.ps1
#   快跑：powershell -File bench/run.ps1 -Short   # 先 go test -short ./... 再把 benchtime 收敛到 50ms
#   集成门禁：powershell -File bench/run.ps1 -Integration
#             校验 7 个 TASKENGINE_TEST_* 环境变量后跑 ./task_engine 集成用例；
#             缺任一个变量 exit 2；输出解析断言 SKIP=0 / FAIL=0 / PASS>=35 不达标 exit 3；
#             两者都拒绝以全 SKIP 静默通过。
param(
    [int]$Benchtime = 1000,        # 单个 benchmark 最短时间（ms）
    [int]$Count = 1,                # 重复次数
    [string]$Tag = "manual",        # 结果标签
    [switch]$Short,                 # 快跑：跳过重型用例（子进程宕机回归、真实 HTTP 负载）并压缩基准时长
    [switch]$Integration,           # 集成门禁：校验 TASKENGINE_TEST_* 供给后跑 task_engine 集成用例
    [string]$Output = "docs/bench-result-$Tag.txt"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# 集成测试环境变量的完整名单（与 task_engine/storage_test.go 的 7 个常量一致）。
# 脚本只校验变量是否「存在」，绝不读取、打印或保存它们的值：凭据一律由运行时注入。
$integrationVars = @(
    "TASKENGINE_TEST_MYSQL_HOST",
    "TASKENGINE_TEST_MYSQL_USER",
    "TASKENGINE_TEST_MYSQL_PASSWORD",
    "TASKENGINE_TEST_MYSQL_DB",
    "TASKENGINE_TEST_REDIS_ADDR",
    "TASKENGINE_TEST_REDIS_PASSWORD",
    "TASKENGINE_TEST_REDIS_DB"
)

if ($Integration) {
    # 只判「未定义」：Redis 密码允许空串、Redis DB 允许 0，按非空判缺会误伤合法配置。
    $missingVars = @()
    foreach ($name in $integrationVars) {
        if ($null -eq [Environment]::GetEnvironmentVariable($name)) {
            $missingVars += $name
        }
    }
    if ($missingVars.Count -gt 0) {
        Write-Host "[Integration] 集成门禁未通过：以下 TASKENGINE_TEST_* 环境变量未注入（缺任一个即拒绝运行）" -ForegroundColor Red
        foreach ($name in $missingVars) {
            Write-Host "  - $name" -ForegroundColor Red
        }
        Write-Host "[Integration] 请在运行时注入全部 7 个变量，值不写入仓库任何文件。" -ForegroundColor Red
        Write-Host "[Integration] MySQL 库名必须含 test 并指向专用隔离库（setupStorage 会 DROP TABLE 后重建）。" -ForegroundColor Red
        Write-Host "[Integration] 拒绝以全 SKIP 静默通过；详见 task_engine/README.md「集成测试门禁」。" -ForegroundColor Red
        exit 2
    }

    $integrationOutput = Join-Path $root "docs/taskengine-integration.txt"
    $integrationDir = Split-Path -Parent $integrationOutput
    if (-not (Test-Path $integrationDir)) { New-Item -ItemType Directory -Path $integrationDir -Force | Out-Null }

    Write-Host "=== TouchGoCore 集成门禁 ===" -ForegroundColor Cyan
    Write-Host "[Integration] 7 个 TASKENGINE_TEST_* 变量均已注入（值不落盘）。" -ForegroundColor Cyan
    Write-Host "[Integration] go test -timeout 600s -count=1 -v ./task_engine/" -ForegroundColor Yellow
    go test -timeout 600s -count=1 -v ./task_engine/ 2>&1 | Tee-Object -FilePath $integrationOutput
    # 必须紧跟 pipeline 取值：Tee-Object 是 cmdlet，后面任何 cmdlet 都可能改写 $LASTEXITCODE，
    # 晚取会丢掉 go test 的真实退出码。
    $integrationExit = $LASTEXITCODE
    Write-Host "[Integration] 输出已写入: $integrationOutput" -ForegroundColor Cyan

    # 门禁判据基于「实际 -v 输出」逐行解析，绝不硬编码结果行冒充通过。
    # 只统计行首的 --- PASS:/SKIP:/FAIL:（顶层用例）；子测试的行带缩进，不计入。
    # Get-Content 会自动识别 Tee-Object 写出的 UTF-16 BOM，无需假定编码。
    $resultLines = @(Get-Content -Path $integrationOutput)
    $passCount = @($resultLines | Where-Object { $_ -match '^--- PASS:' }).Count
    $skipCount = @($resultLines | Where-Object { $_ -match '^--- SKIP:' }).Count
    $failCount = @($resultLines | Where-Object { $_ -match '^--- FAIL:' }).Count
    Write-Host ("[Integration] 解析 go test -v 输出: PASS={0} SKIP={1} FAIL={2}" -f $passCount, $skipCount, $failCount) -ForegroundColor Cyan

    if ($skipCount -ne 0 -or $failCount -ne 0 -or $passCount -lt 35) {
        Write-Host "[Integration] 门禁未通过：要求 SKIP=0、FAIL=0、PASS>=35（真跑用例数下限）" -ForegroundColor Red
        Write-Host ("[Integration] 实测 PASS={0} SKIP={1} FAIL={2}，输出文件 {3}" -f $passCount, $skipCount, $failCount, $integrationOutput) -ForegroundColor Red
        Write-Host "[Integration] 出现 SKIP 说明环境变量 / 专用库供给不完整，禁止当作通过。" -ForegroundColor Red
        exit 3
    }
    if ($integrationExit -ne 0) {
        Write-Host ("[Integration] go test 退出码非零（{0}），即使计数达标也判失败。" -f $integrationExit) -ForegroundColor Red
        exit $integrationExit
    }
    Write-Host "[Integration] 门禁通过：SKIP=0、FAIL=0、PASS>=35。" -ForegroundColor Green
    exit 0
}

if ($Short) {
    # -short 只门控用例，不影响 benchmark 的迭代量，所以快跑必须同时压 benchtime
    $Benchtime = [Math]::Min($Benchtime, 50)
}

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
Write-Host "Benchtime: ${Benchtime}ms  Count: $Count  Tag: $Tag  Short: $Short"
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
Short:    $Short

"@ | Set-Content -Path $outputPath -Encoding UTF8

# 快跑先给一遍功能结论：-short 下被门控的用例（子进程宕机回归、真实 HTTP 负载）直接跳过
if ($Short) {
    Write-Host "[Short] go test -short -count=1 ./..." -ForegroundColor Yellow
    go test -short -count=1 -timeout=300s ./... 2>&1 | Tee-Object -Append -FilePath $outputPath
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[Short] 用例未全绿（退出码 $LASTEXITCODE），继续跑基准以便一次看完" -ForegroundColor Red
    }
    Write-Host ""
}

foreach ($pkg in $packages) {
    Write-Host "[Running] $pkg" -ForegroundColor Yellow
    go test -bench=. -benchtime=${Benchtime}ms -count=$Count -benchmem -run=^$ $pkg 2>&1 |
        Tee-Object -Append -FilePath $outputPath
    Write-Host ""
}

Write-Host "=== 完成 ===" -ForegroundColor Green
Write-Host "结果已写入: $outputPath"

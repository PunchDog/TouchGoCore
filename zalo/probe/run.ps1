# zalo/probe/run.ps1
# Zalo 卡片真机探针（Windows PowerShell）。
#
# 用法：
#   powershell -File zalo/probe/run.ps1
#   powershell -File zalo/probe/run.ps1 -EnvFile zalo/probe/probe.env -Group
#   powershell -File zalo/probe/run.ps1 -DryRun        # 只校验配置，不发送
#
# 安全：只解析 KEY=VALUE，不执行环境文件内容；DSN 不会打印。
# 警告：正常模式会真实发送一条消息。

param(
    [string]$EnvFile = "zalo/probe/probe.env",
    [switch]$Group,
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"
# Windows PowerShell 5.1 默认按本地代码页输出；统一成 UTF-8，避免中文乱码。
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $root

if (-not (Test-Path $EnvFile)) {
    Write-Host "缺少环境文件：$EnvFile" -ForegroundColor Red
    Write-Host "先复制模板：Copy-Item zalo/probe/env.example zalo/probe/probe.env" -ForegroundColor Yellow
    exit 1
}

# 只解析 KEY=VALUE；忽略空行与 # 注释；去掉成对引号。
$vars = @{}
foreach ($line in Get-Content $EnvFile -Encoding UTF8) {
    $t = $line.Trim()
    if ($t -eq "" -or $t.StartsWith("#")) { continue }
    $i = $t.IndexOf("=")
    if ($i -lt 1) { continue }
    $k = $t.Substring(0, $i).Trim()
    if ($k -notmatch '^[A-Za-z_][A-Za-z0-9_]*$') { continue }
    $v = $t.Substring($i + 1).Trim()
    if ($v.Length -ge 2 -and (($v.StartsWith('"') -and $v.EndsWith('"')) -or ($v.StartsWith("'") -and $v.EndsWith("'")))) {
        $v = $v.Substring(1, $v.Length - 2)
    }
    $vars[$k] = $v
}

$required = @(
    "TEST_ZALO_PROBE_DSN",
    "TEST_ZALO_PROBE_ACCOUNT",
    "TEST_ZALO_PROBE_TO",
    "TEST_ZALO_PROBE_LINK"
)
$missing = @($required | Where-Object { [string]::IsNullOrWhiteSpace($vars[$_]) })
if ($missing.Count -gt 0) {
    Write-Host ("缺少必填变量: " + ($missing -join ", ")) -ForegroundColor Red
    exit 1
}
if ($Group) { $vars["TEST_ZALO_PROBE_GROUP"] = "1" }

# 只注入当前进程，不改用户环境。
foreach ($k in $vars.Keys) { Set-Item -Path "Env:$k" -Value $vars[$k] }

$isGroup = $vars["TEST_ZALO_PROBE_GROUP"] -eq "1"
Write-Host "=== Zalo 卡片真机探针 ===" -ForegroundColor Cyan
Write-Host "账号: $($vars['TEST_ZALO_PROBE_ACCOUNT'])  目标: $($vars['TEST_ZALO_PROBE_TO'])  群聊: $isGroup"
Write-Host "外链: $($vars['TEST_ZALO_PROBE_LINK'])"
Write-Host "注意：会真实发送一条消息。" -ForegroundColor Yellow

if ($DryRun) {
    Write-Host "DryRun：配置校验通过，未发送。" -ForegroundColor Yellow
    exit 0
}

& go test -count=1 -v -run TestZACardProbeRealDevice ./zalo
exit $LASTEXITCODE

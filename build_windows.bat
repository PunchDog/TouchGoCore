@echo off
REM Windows 构建脚本
REM 需要 MinGW-w64 和 Lua C 库

setlocal enabledelayedexpansion

REM 显示帮助信息
if "%1" == "help" goto :help
if "%1" == "--help" goto :help
if "%1" == "-h" goto :help

REM 清理构建产物
if "%1" == "clean" goto :clean

REM 设置默认参数
set LUA_VERSION=5.1
set BUILD_MODE=debug

REM 解析命令行参数
:parse_args
if "%1" == "" goto :end_parse
if "%1" == "--lua51" set LUA_VERSION=5.1 && shift && goto :parse_args
if "%1" == "--lua52" set LUA_VERSION=5.2 && shift && goto :parse_args
if "%1" == "--lua53" set LUA_VERSION=5.3 && shift && goto :parse_args
if "%1" == "--lua54" set LUA_VERSION=5.4 && shift && goto :parse_args
if "%1" == "--release" set BUILD_MODE=release && shift && goto :parse_args
if "%1" == "--debug" set BUILD_MODE=debug && shift && goto :parse_args
goto :end_parse
:end_parse

REM 开始构建
echo =========================================
echo TouchGoCore 构建脚本
 echo =========================================
echo 构建模式: %BUILD_MODE%
echo Lua 版本: %LUA_VERSION%
echo 开始构建时间: %TIME%
echo =========================================

REM 设置 CGO 启用
set CGO_ENABLED=1

REM 检查是否有 GCC 编译器
echo 检查 GCC 编译器...
where gcc >nul 2>nul
if %ERRORLEVEL% NEQ 0 (
    echo 错误: 未找到 GCC 编译器
    echo 请安装 MinGW-w64 或其他支持 CGO 的编译器
    echo 可以从 https://www.mingw-w64.org/ 下载安装
    exit /b 1
)
echo GCC 编译器检查通过

REM 检查 Go 环境
echo 检查 Go 环境...
where go >nul 2>nul
if %ERRORLEVEL% NEQ 0 (
    echo 错误: 未找到 Go 编译器
    echo 请安装 Go 并将其添加到 PATH 环境变量
    exit /b 1
)
echo Go 环境检查通过

REM 构建标签
set BUILD_TAGS=
if "%LUA_VERSION%" == "5.1" set BUILD_TAGS=!BUILD_TAGS! "!lua52,!lua53,!lua54!"
if "%LUA_VERSION%" == "5.2" set BUILD_TAGS=!BUILD_TAGS! "!lua51,!lua53,!lua54!"
if "%LUA_VERSION%" == "5.3" set BUILD_TAGS=!BUILD_TAGS! "!lua51,!lua52,!lua54!"
if "%LUA_VERSION%" == "5.4" set BUILD_TAGS=!BUILD_TAGS! "!lua51,!lua52,!lua53!"

REM 构建参数
set BUILD_FLAGS=
if "%BUILD_MODE%" == "release" set BUILD_FLAGS=-ldflags "-s -w"

echo 使用 Lua %LUA_VERSION% 编译...
echo 构建命令: go build %BUILD_FLAGS% -tags %BUILD_TAGS%

go build %BUILD_FLAGS% -tags %BUILD_TAGS%

if %ERRORLEVEL% EQU 0 (
    echo =========================================
    echo 构建成功!
    echo 构建完成时间: %TIME%
    echo 输出文件: %CD%\touchgocore.exe
    echo =========================================
) else (
    echo =========================================
    echo 构建失败
    echo 请检查错误信息并确保所有依赖都已正确安装
    echo =========================================
    exit /b 1
)

goto :eof

:help
echo =========================================
echo TouchGoCore 构建脚本帮助
 echo =========================================
echo 用法: build_windows.bat [选项]
echo 
echo 选项:
echo   --lua51        使用 Lua 5.1 编译（默认）
echo   --lua52        使用 Lua 5.2 编译
echo   --lua53        使用 Lua 5.3 编译
echo   --lua54        使用 Lua 5.4 编译
echo   --release      发布模式构建
echo   --debug        调试模式构建（默认）
echo   clean          清理构建产物
echo   help, --help, -h  显示此帮助信息
echo 
echo 示例:
echo   build_windows.bat              # 使用默认设置构建
echo   build_windows.bat --lua54      # 使用 Lua 5.4 构建
echo   build_windows.bat --release    # 发布模式构建
echo   build_windows.bat clean        # 清理构建产物
echo =========================================
goto :eof

:clean
echo 清理构建产物...
if exist touchgocore.exe del touchgocore.exe
if exist touchgocore.exe.mdb del touchgocore.exe.mdb
echo 清理完成!
goto :eof

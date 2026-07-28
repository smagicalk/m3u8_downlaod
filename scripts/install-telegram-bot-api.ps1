[CmdletBinding()]
param(
    [switch]$InstallPrerequisites,
    [switch]$Force,
    [string]$SourceRef = 'master'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$projectRoot = Split-Path -Parent $PSScriptRoot
$toolsDirectory = Join-Path $projectRoot 'tools'
$sourceDirectory = Join-Path $toolsDirectory 'telegram-bot-api-source'
$vcpkgDirectory = Join-Path $toolsDirectory 'vcpkg'
$buildDirectory = Join-Path $sourceDirectory 'build-windows-x64'
$installDirectory = Join-Path $toolsDirectory 'telegram-bot-api'
$serverExecutable = Join-Path $installDirectory 'telegram-bot-api.exe'
$vcpkgExecutable = Join-Path $vcpkgDirectory 'vcpkg.exe'
$vcpkgToolchain = Join-Path $vcpkgDirectory 'scripts/buildsystems/vcpkg.cmake'
$gperfDirectory = Join-Path $vcpkgDirectory 'installed/x64-windows/tools/gperf'

function Invoke-ExternalCommand {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments
    )

    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "External command failed (exit code $LASTEXITCODE): $FilePath $($Arguments -join ' ')"
    }
}

function Test-AvailableCommand {
    param([Parameter(Mandatory = $true)][string]$Name)
    return $null -ne (Get-Command $Name -ErrorAction SilentlyContinue)
}

function Install-Prerequisite {
    param(
        [Parameter(Mandatory = $true)][string]$PackageId,
        [string[]]$AdditionalArguments = @()
    )

    if (-not (Test-AvailableCommand 'winget')) {
        throw 'winget was not found. Install Git, CMake, and Visual Studio 2022 Build Tools with the Desktop development with C++ workload, then run this script again.'
    }
    $wingetArguments = @('install', '--exact', '--id', $PackageId, '--accept-package-agreements', '--accept-source-agreements', '--silent') + $AdditionalArguments
    Invoke-ExternalCommand -FilePath 'winget' -Arguments $wingetArguments
}

if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    throw 'This script only supports Windows.'
}

if (Test-Path -LiteralPath $serverExecutable) {
    if (-not $Force) {
        Write-Host "Local Bot API Server already exists: $serverExecutable"
        Write-Host 'Use -Force to fetch and build it again.'
        exit 0
    }
    Write-Host 'Rebuilding the local Bot API Server.'
}

if ($InstallPrerequisites) {
    if (-not (Test-AvailableCommand 'git')) {
        Install-Prerequisite 'Git.Git'
    }
    if (-not (Test-AvailableCommand 'cmake')) {
        Install-Prerequisite 'Kitware.CMake'
    }
    $cmakeGenerators = if (Test-AvailableCommand 'cmake') { (& cmake --help 2>&1 | Out-String) } else { '' }
    if ($cmakeGenerators -notmatch 'Visual Studio 17 2022') {
        Install-Prerequisite 'Microsoft.VisualStudio.2022.BuildTools' @('--override', '--wait --quiet --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended')
    }
}

foreach ($command in @('git', 'cmake')) {
    if (-not (Test-AvailableCommand $command)) {
        throw "$command was not found. Use -InstallPrerequisites or install it manually, reopen PowerShell, and run this script again."
    }
}

$availableGenerators = & cmake --help 2>&1 | Out-String
if ($availableGenerators -notmatch 'Visual Studio 17 2022') {
    throw 'Visual Studio 2022 C++ Build Tools were not found. Install Visual Studio 2022 Build Tools with the Desktop development with C++ workload.'
}

New-Item -ItemType Directory -Force -Path $toolsDirectory | Out-Null

if (Test-Path -LiteralPath (Join-Path $sourceDirectory '.git')) {
    Write-Host 'Updating Telegram Bot API source...'
    Invoke-ExternalCommand 'git' '-C' $sourceDirectory 'fetch' '--tags' 'origin'
    Invoke-ExternalCommand 'git' '-C' $sourceDirectory 'checkout' $SourceRef
    Invoke-ExternalCommand 'git' '-C' $sourceDirectory 'submodule' 'update' '--init' '--recursive'
} else {
    Write-Host 'Downloading the official Telegram Bot API source...'
    Invoke-ExternalCommand 'git' 'clone' '--recursive' 'https://github.com/tdlib/telegram-bot-api.git' $sourceDirectory
    Invoke-ExternalCommand 'git' '-C' $sourceDirectory 'checkout' $SourceRef
}

if (-not (Test-Path -LiteralPath (Join-Path $vcpkgDirectory '.git'))) {
    Write-Host 'Downloading vcpkg...'
    Invoke-ExternalCommand 'git' 'clone' 'https://github.com/microsoft/vcpkg.git' $vcpkgDirectory
}
if (-not (Test-Path -LiteralPath $vcpkgExecutable)) {
    Write-Host 'Bootstrapping vcpkg...'
    Invoke-ExternalCommand (Join-Path $vcpkgDirectory 'bootstrap-vcpkg.bat') '-disableMetrics'
}

Write-Host 'Installing OpenSSL, zlib, and gperf...'
Invoke-ExternalCommand $vcpkgExecutable 'install' 'openssl:x64-windows' 'zlib:x64-windows' 'gperf:x64-windows'

if (-not (Test-Path -LiteralPath $vcpkgToolchain)) {
    throw "vcpkg CMake toolchain file was not found: $vcpkgToolchain"
}
if (-not (Test-Path -LiteralPath $gperfDirectory)) {
    throw "gperf tool directory was not found: $gperfDirectory"
}

# TDLib's CMake lookup expects gperf to be available from PATH.
$env:PATH = "$gperfDirectory;$env:PATH"
Write-Host 'Configuring Telegram Bot API Server (x64 Release)...'
Invoke-ExternalCommand 'cmake' '-S' $sourceDirectory '-B' $buildDirectory '-G' 'Visual Studio 17 2022' '-A' 'x64' "-DCMAKE_TOOLCHAIN_FILE=$vcpkgToolchain" '-DCMAKE_BUILD_TYPE=Release' "-DCMAKE_INSTALL_PREFIX=$installDirectory" '-DCMAKE_INSTALL_BINDIR=.'

Write-Host 'Building and installing Telegram Bot API Server...'
Invoke-ExternalCommand 'cmake' '--build' $buildDirectory '--config' 'Release' '--target' 'install' '--parallel'

if (-not (Test-Path -LiteralPath $serverExecutable)) {
    throw "Build completed but the executable was not found: $serverExecutable"
}

Write-Host ''
Write-Host 'Installation completed. Configure the Bot Token and Chat IDs in the settings page, then start the local server with:'
Write-Host "& '$serverExecutable' --api-id 'YOUR_API_ID' --api-hash 'YOUR_API_HASH' --local --http-port 8081 --dir '$projectRoot/data/telegram-bot-api'"

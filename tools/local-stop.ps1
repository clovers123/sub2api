param(
  [int]$BackendPort = $(if ($env:BACKEND_PORT) { [int]$env:BACKEND_PORT } else { 9004 }),
  [int]$FrontendPort = $(if ($env:FRONTEND_PORT) { [int]$env:FRONTEND_PORT } else { 9005 }),
  [string]$ProxyCoresConfig = ""
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")
$TmpDir = Join-Path $RootDir "tmp"
$BackendPidFile = Join-Path $TmpDir "sub2api-backend.pid"
$FrontendPidFile = Join-Path $TmpDir "sub2api-frontend.pid"
$ProxyWatcherPidFile = Join-Path $TmpDir "sub2api-proxy-cores-watcher.pid"

function Stop-ProcessFromPidFile {
  param(
    [string]$PidFile,
    [string]$Label
  )
  if (-not (Test-Path $PidFile)) {
    Write-Host "$Label is not running"
    return
  }
  $RawPid = (Get-Content $PidFile -Raw -ErrorAction SilentlyContinue).Trim()
  if (-not $RawPid) {
    Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
    Write-Host "$Label is not running"
    return
  }
  try {
    $Proc = Get-Process -Id ([int]$RawPid) -ErrorAction Stop
    Write-Host "Stopping $Label ($($Proc.Id))..."
    Stop-Process -Id $Proc.Id -Force -ErrorAction SilentlyContinue
    Write-Host "Stopped $Label"
  } catch {
    Write-Host "$Label is not running"
  }
  Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
}

function Stop-Port {
  param(
    [int]$Port,
    [string]$Label
  )
  try {
    $Connections = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction Stop
    $Pids = $Connections | Select-Object -ExpandProperty OwningProcess -Unique
    foreach ($Pid in $Pids) {
      if ($Pid -gt 0) {
        Write-Host "Stopping $Label listener on port $Port ($Pid)..."
        Stop-Process -Id $Pid -Force -ErrorAction SilentlyContinue
      }
    }
  } catch {
    $Pattern = "^\s*TCP\s+\S+:$Port\s+\S+\s+LISTENING\s+(\d+)\s*$"
    $Matches = (netstat -ano -p tcp 2>$null) | Select-String -Pattern $Pattern
    foreach ($Match in $Matches) {
      $Pid = [int]$Match.Matches[0].Groups[1].Value
      Write-Host "Stopping $Label listener on port $Port ($Pid)..."
      Stop-Process -Id $Pid -Force -ErrorAction SilentlyContinue
    }
  }
}

function Resolve-PowerShellExe {
  $Pwsh = Get-Command pwsh -ErrorAction SilentlyContinue
  if ($Pwsh) {
    return $Pwsh.Source
  }
  return (Get-Command powershell -ErrorAction Stop).Source
}

Stop-ProcessFromPidFile $FrontendPidFile "frontend"
Stop-ProcessFromPidFile $BackendPidFile "backend"
Stop-ProcessFromPidFile $ProxyWatcherPidFile "proxy core watcher"

$ConfigPath = $ProxyCoresConfig
if (-not $ConfigPath) {
  $ConfigPath = Join-Path $RootDir "tools\proxy-cores.windows.json"
}
if (Test-Path $ConfigPath) {
  & (Resolve-PowerShellExe) -NoProfile -ExecutionPolicy Bypass -File (Join-Path $RootDir "tools\watch-sub2api-proxy-cores.ps1") -Config $ConfigPath -Stop | Out-Host
}

Stop-Port $FrontendPort "frontend"
Stop-Port $BackendPort "backend"

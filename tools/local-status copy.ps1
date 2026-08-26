param(
  [int]$BackendPort = $(if ($env:BACKEND_PORT) { [int]$env:BACKEND_PORT } else { 9004 }),
  [int]$FrontendPort = $(if ($env:FRONTEND_PORT) { [int]$env:FRONTEND_PORT } else { 9005 }),
  [string]$ProxyCoresConfig = ""
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")
$TmpDir = Join-Path $RootDir "tmp"

function Show-PidFile {
  param(
    [string]$PidFile,
    [string]$Label
  )
  if (-not (Test-Path $PidFile)) {
    Write-Host "$Label pid: <none>"
    return
  }
  $RawPid = (Get-Content $PidFile -Raw -ErrorAction SilentlyContinue).Trim()
  if (-not $RawPid) {
    Write-Host "$Label pid: <empty>"
    return
  }
  try {
    $Proc = Get-Process -Id ([int]$RawPid) -ErrorAction Stop
    Write-Host "$Label pid: $RawPid running ($($Proc.ProcessName))"
  } catch {
    Write-Host "$Label pid: $RawPid stale"
  }
}

function Show-Port {
  param(
    [int]$Port,
    [string]$Label
  )
  Write-Host ""
  Write-Host "$Label port $Port:"
  try {
    Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction Stop |
      Select-Object LocalAddress, LocalPort, State, OwningProcess |
      Format-Table -AutoSize
  } catch {
    (netstat -ano -p tcp 2>$null) | Select-String -Pattern "^\s*TCP\s+\S+:$Port\s+\S+\s+LISTENING\s+"
  }
}

function Resolve-PowerShellExe {
  $Pwsh = Get-Command pwsh -ErrorAction SilentlyContinue
  if ($Pwsh) {
    return $Pwsh.Source
  }
  return (Get-Command powershell -ErrorAction Stop).Source
}

Show-PidFile (Join-Path $TmpDir "sub2api-backend.pid") "backend"
Show-PidFile (Join-Path $TmpDir "sub2api-frontend.pid") "frontend"
Show-PidFile (Join-Path $TmpDir "sub2api-proxy-cores-watcher.pid") "proxy core watcher"

Show-Port $BackendPort "backend"
Show-Port $FrontendPort "frontend"

$ConfigPath = $ProxyCoresConfig
if (-not $ConfigPath) {
  $ConfigPath = Join-Path $RootDir "tools\proxy-cores.windows.json"
}
if (Test-Path $ConfigPath) {
  Write-Host ""
  & (Resolve-PowerShellExe) -NoProfile -ExecutionPolicy Bypass -File (Join-Path $RootDir "tools\watch-sub2api-proxy-cores.ps1") -Config $ConfigPath -Status | Out-Host
}

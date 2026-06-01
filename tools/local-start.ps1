param(
  [int]$BackendPort = $(if ($env:BACKEND_PORT) { [int]$env:BACKEND_PORT } else { 9004 }),
  [int]$FrontendPort = $(if ($env:FRONTEND_PORT) { [int]$env:FRONTEND_PORT } else { 9005 }),
  [string]$ProxyCoresConfig = "",
  [switch]$NoProxyCores
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")
$TmpDir = Join-Path $RootDir "tmp"
$BackendLog = Join-Path $TmpDir "sub2api-backend.log"
$BackendErrLog = Join-Path $TmpDir "sub2api-backend.err.log"
$FrontendLog = Join-Path $TmpDir "sub2api-frontend.log"
$FrontendErrLog = Join-Path $TmpDir "sub2api-frontend.err.log"
$BackendPidFile = Join-Path $TmpDir "sub2api-backend.pid"
$FrontendPidFile = Join-Path $TmpDir "sub2api-frontend.pid"
$ProxyWatcherPidFile = Join-Path $TmpDir "sub2api-proxy-cores-watcher.pid"
$ProxyWatcherLog = Join-Path $TmpDir "sub2api-proxy-cores-watcher.log"
$ProxyWatcherErrLog = Join-Path $TmpDir "sub2api-proxy-cores-watcher.err.log"

function Require-Command {
  param([string]$Name)
  if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
    throw "Missing command: $Name"
  }
}

function Get-RunningProcess {
  param([string]$PidFile)
  if (-not (Test-Path $PidFile)) {
    return $null
  }
  $RawPid = (Get-Content $PidFile -Raw -ErrorAction SilentlyContinue).Trim()
  if (-not $RawPid) {
    Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
    return $null
  }
  try {
    return Get-Process -Id ([int]$RawPid) -ErrorAction Stop
  } catch {
    Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
    return $null
  }
}

function Test-PortListening {
  param([int]$Port)
  try {
    return [bool](Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction Stop | Select-Object -First 1)
  } catch {
    $Pattern = "^\s*TCP\s+\S+:$Port\s+\S+\s+LISTENING\s+"
    return [bool]((netstat -ano -p tcp 2>$null) | Select-String -Pattern $Pattern | Select-Object -First 1)
  }
}

function Wait-ForPort {
  param(
    [int]$Port,
    [string]$Label,
    [int]$TimeoutSeconds = 60
  )
  for ($i = 0; $i -lt $TimeoutSeconds; $i++) {
    if (Test-PortListening $Port) {
      Write-Host "$Label is listening on port $Port"
      return
    }
    Start-Sleep -Seconds 1
  }
  throw "$Label did not start on port $Port within ${TimeoutSeconds}s"
}

function Stop-ProcessFromPidFile {
  param(
    [string]$PidFile,
    [string]$Label
  )
  $Proc = Get-RunningProcess $PidFile
  if ($null -eq $Proc) {
    return
  }
  Write-Host "Stopping $Label ($($Proc.Id))..."
  Stop-Process -Id $Proc.Id -Force -ErrorAction SilentlyContinue
  Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
}

function Build-Backend {
  Require-Command go
  $VersionFile = Join-Path $RootDir "backend\cmd\server\VERSION"
  $Version = "0.0.0-dev"
  if (Test-Path $VersionFile) {
    $Version = (Get-Content $VersionFile -Raw).Trim()
  }
  $OutDir = Join-Path $RootDir "backend\bin"
  $OutFile = Join-Path $OutDir "server.exe"
  New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
  Write-Host "Building backend..."
  Push-Location (Join-Path $RootDir "backend")
  try {
    & go build "-ldflags=-s -w -X main.Version=$Version" -trimpath -o $OutFile ./cmd/server
    if ($LASTEXITCODE -ne 0) {
      throw "go build failed with exit code $LASTEXITCODE"
    }
  } finally {
    Pop-Location
  }
}

function Resolve-PowerShellExe {
  $Pwsh = Get-Command pwsh -ErrorAction SilentlyContinue
  if ($Pwsh) {
    return $Pwsh.Source
  }
  return (Get-Command powershell -ErrorAction Stop).Source
}

function Join-ProcessArguments {
  param([string[]]$Arguments)
  return ($Arguments | ForEach-Object {
    $Value = [string]$_
    if ($Value -match '[\s"]') {
      '"' + ($Value -replace '"', '\"') + '"'
    } else {
      $Value
    }
  }) -join " "
}

function Start-ProxyWatcherIfConfigured {
  if ($NoProxyCores) {
    return
  }

  $ConfigPath = $ProxyCoresConfig
  if (-not $ConfigPath) {
    $ConfigPath = Join-Path $RootDir "tools\proxy-cores.windows.json"
  }
  if (-not (Test-Path $ConfigPath)) {
    return
  }

  Stop-ProcessFromPidFile $ProxyWatcherPidFile "proxy core watcher"

  $ScriptPath = Join-Path $RootDir "tools\watch-sub2api-proxy-cores.ps1"
  $PowerShellExe = Resolve-PowerShellExe
  $Args = @(
    "-NoProfile",
    "-ExecutionPolicy", "Bypass",
    "-File", $ScriptPath,
    "-Config", (Resolve-Path $ConfigPath),
    "-BackendPort", "$BackendPort"
  )
  $Watcher = Start-Process `
    -FilePath $PowerShellExe `
    -ArgumentList (Join-ProcessArguments $Args) `
    -WorkingDirectory $RootDir `
    -RedirectStandardOutput $ProxyWatcherLog `
    -RedirectStandardError $ProxyWatcherErrLog `
    -PassThru
  Set-Content -Path $ProxyWatcherPidFile -Value $Watcher.Id
  Write-Host "Proxy core watcher started from $ConfigPath (pid $($Watcher.Id))"
}

function Stop-ProxyCores {
  if ($NoProxyCores) {
    return
  }

  $ConfigPath = $ProxyCoresConfig
  if (-not $ConfigPath) {
    $ConfigPath = Join-Path $RootDir "tools\proxy-cores.windows.json"
  }
  Stop-ProcessFromPidFile $ProxyWatcherPidFile "proxy core watcher"
  if (Test-Path $ConfigPath) {
    $ScriptPath = Join-Path $RootDir "tools\watch-sub2api-proxy-cores.ps1"
    & (Resolve-PowerShellExe) -NoProfile -ExecutionPolicy Bypass -File $ScriptPath -Config $ConfigPath -Stop | Out-Host
  }
}

New-Item -ItemType Directory -Force -Path $TmpDir | Out-Null

if (-not (Test-Path (Join-Path $RootDir "config.yaml")) -and -not (Test-Path (Join-Path $RootDir "backend\config.yaml"))) {
  throw "Missing config.yaml. Put your backend config at $RootDir\config.yaml or $RootDir\backend\config.yaml."
}

if (-not (Test-Path (Join-Path $RootDir "frontend\node_modules"))) {
  throw "Missing frontend\node_modules. Run: pnpm --dir frontend install --frozen-lockfile"
}

$ViteCmd = Join-Path $RootDir "frontend\node_modules\.bin\vite.cmd"
if (-not (Test-Path $ViteCmd)) {
  throw "Missing Vite binary. Run: pnpm --dir frontend install --frozen-lockfile"
}

if (Get-RunningProcess $BackendPidFile) {
  throw "Backend already appears to be running from $BackendPidFile. Run tools\local-stop.ps1 first."
}
if (Get-RunningProcess $FrontendPidFile) {
  throw "Frontend already appears to be running from $FrontendPidFile. Run tools\local-stop.ps1 first."
}
if (Test-PortListening $BackendPort) {
  throw "Port $BackendPort is already in use. Stop that process first or set BACKEND_PORT."
}
if (Test-PortListening $FrontendPort) {
  throw "Port $FrontendPort is already in use. Stop that process first or set FRONTEND_PORT."
}

Build-Backend

New-Item -ItemType File -Force -Path $BackendLog, $BackendErrLog, $FrontendLog, $FrontendErrLog | Out-Null
Clear-Content -Path $BackendLog, $BackendErrLog, $FrontendLog, $FrontendErrLog

$BackendExe = Join-Path $RootDir "backend\bin\server.exe"
$OldServerPort = $env:SERVER_PORT
$OldViteDevPort = $env:VITE_DEV_PORT
$OldViteProxyTarget = $env:VITE_DEV_PROXY_TARGET

try {
  $env:SERVER_PORT = "$BackendPort"
  $BackendProc = Start-Process `
    -FilePath $BackendExe `
    -WorkingDirectory $RootDir `
    -RedirectStandardOutput $BackendLog `
    -RedirectStandardError $BackendErrLog `
    -PassThru
  Set-Content -Path $BackendPidFile -Value $BackendProc.Id
  Wait-ForPort $BackendPort "Backend"

  Start-ProxyWatcherIfConfigured

  $env:VITE_DEV_PORT = "$FrontendPort"
  $env:VITE_DEV_PROXY_TARGET = "http://localhost:$BackendPort"
  $FrontendProc = Start-Process `
    -FilePath $ViteCmd `
    -ArgumentList @("--config", "vite.config.ts", "--host", "0.0.0.0") `
    -WorkingDirectory (Join-Path $RootDir "frontend") `
    -RedirectStandardOutput $FrontendLog `
    -RedirectStandardError $FrontendErrLog `
    -PassThru
  Set-Content -Path $FrontendPidFile -Value $FrontendProc.Id
  Wait-ForPort $FrontendPort "Frontend"

  Write-Host ""
  Write-Host "Sub2API is running:"
  Write-Host "  Frontend: http://localhost:$FrontendPort/"
  Write-Host "  Backend:  http://localhost:$BackendPort"
  Write-Host ""
  Write-Host "Logs:"
  Write-Host "  Get-Content -Wait tmp\sub2api-backend.log, tmp\sub2api-backend.err.log"
  Write-Host "  Get-Content -Wait tmp\sub2api-frontend.log, tmp\sub2api-frontend.err.log"
  Write-Host ""
  Write-Host "Press Ctrl-C here to stop both services."

  while ($true) {
    if ($BackendProc.HasExited) {
      Write-Error "Backend exited. Last log lines:"
      Get-Content $BackendLog, $BackendErrLog -Tail 60 -ErrorAction SilentlyContinue | Write-Error
      exit 1
    }
    if ($FrontendProc.HasExited) {
      Write-Error "Frontend exited. Last log lines:"
      Get-Content $FrontendLog, $FrontendErrLog -Tail 60 -ErrorAction SilentlyContinue | Write-Error
      exit 1
    }
    Start-Sleep -Seconds 2
  }
} finally {
  if ($null -ne $OldServerPort) { $env:SERVER_PORT = $OldServerPort } else { Remove-Item Env:\SERVER_PORT -ErrorAction SilentlyContinue }
  if ($null -ne $OldViteDevPort) { $env:VITE_DEV_PORT = $OldViteDevPort } else { Remove-Item Env:\VITE_DEV_PORT -ErrorAction SilentlyContinue }
  if ($null -ne $OldViteProxyTarget) { $env:VITE_DEV_PROXY_TARGET = $OldViteProxyTarget } else { Remove-Item Env:\VITE_DEV_PROXY_TARGET -ErrorAction SilentlyContinue }
  Stop-ProcessFromPidFile $FrontendPidFile "frontend"
  Stop-ProcessFromPidFile $BackendPidFile "backend"
  Stop-ProxyCores
}

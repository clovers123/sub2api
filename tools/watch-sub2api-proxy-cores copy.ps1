param(
  [string]$Config = "",
  [int]$BackendPort = 9004,
  [int]$IntervalSeconds = 2,
  [switch]$Stop,
  [switch]$Status
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")
if (-not $Config) {
  $Config = Join-Path $RootDir "tools\proxy-cores.windows.json"
}

function Expand-CorePath {
  param([AllowNull()][string]$Path)
  if (-not $Path) {
    return $Path
  }
  $Expanded = [Environment]::ExpandEnvironmentVariables($Path)
  if ($Expanded.StartsWith("~")) {
    $Expanded = Join-Path $HOME $Expanded.Substring(1).TrimStart("\", "/")
  }
  if ([System.IO.Path]::IsPathRooted($Expanded)) {
    return $Expanded
  }
  return Join-Path $RootDir $Expanded
}

function Read-Config {
  if (-not (Test-Path $Config)) {
    throw "Proxy core config not found: $Config. Copy tools\proxy-cores.windows.example.json to tools\proxy-cores.windows.json first."
  }
  return Get-Content $Config -Raw | ConvertFrom-Json
}

function Get-CorePidFile {
  param($Core)
  if ($Core.PSObject.Properties.Name -contains "pidFile" -and $Core.pidFile) {
    return Expand-CorePath $Core.pidFile
  }
  return Join-Path $RootDir ("tmp\proxy-cores\" + $Core.name + ".pid")
}

function Get-CoreLogPath {
  param($Core, [string]$Kind)
  $Property = if ($Kind -eq "stderr") { "stderrLog" } else { "stdoutLog" }
  if ($Core.PSObject.Properties.Name -contains $Property -and $Core.$Property) {
    return Expand-CorePath $Core.$Property
  }
  return Join-Path $RootDir ("tmp\proxy-cores\" + $Core.name + "." + $Kind + ".log")
}

function Get-RunningProcessFromPidFile {
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

function Test-Sub2ApiRunning {
  param($ConfigData)
  $PidFile = Join-Path $RootDir "tmp\sub2api-backend.pid"
  if ($ConfigData.PSObject.Properties.Name -contains "watch" -and $ConfigData.watch.PSObject.Properties.Name -contains "backendPidFile" -and $ConfigData.watch.backendPidFile) {
    $PidFile = Expand-CorePath $ConfigData.watch.backendPidFile
  }
  if (Get-RunningProcessFromPidFile $PidFile) {
    return $true
  }

  $Port = $BackendPort
  if ($ConfigData.PSObject.Properties.Name -contains "watch" -and $ConfigData.watch.PSObject.Properties.Name -contains "backendPort" -and $ConfigData.watch.backendPort) {
    $Port = [int]$ConfigData.watch.backendPort
  }
  return Test-PortListening $Port
}

function Get-EnabledCores {
  param($ConfigData)
  if (-not ($ConfigData.PSObject.Properties.Name -contains "cores")) {
    return @()
  }
  return @($ConfigData.cores | Where-Object { -not ($_.PSObject.Properties.Name -contains "enabled") -or $_.enabled })
}

function Start-Core {
  param($Core)
  $PidFile = Get-CorePidFile $Core
  if (Get-RunningProcessFromPidFile $PidFile) {
    return
  }

  $Exe = Expand-CorePath $Core.exe
  if (-not (Test-Path $Exe)) {
    Write-Warning "Proxy core '$($Core.name)' executable not found: $Exe"
    return
  }

  $StdoutLog = Get-CoreLogPath $Core "stdout"
  $StderrLog = Get-CoreLogPath $Core "stderr"
  New-Item -ItemType Directory -Force -Path (Split-Path $PidFile), (Split-Path $StdoutLog), (Split-Path $StderrLog) | Out-Null
  New-Item -ItemType File -Force -Path $StdoutLog, $StderrLog | Out-Null

  $Args = @()
  if ($Core.PSObject.Properties.Name -contains "args" -and $Core.args) {
    $Args = @($Core.args | ForEach-Object { [Environment]::ExpandEnvironmentVariables([string]$_) })
  }

  $WorkingDirectory = $RootDir
  if ($Core.PSObject.Properties.Name -contains "workingDirectory" -and $Core.workingDirectory) {
    $WorkingDirectory = Expand-CorePath $Core.workingDirectory
  } elseif (Test-Path $Exe) {
    $WorkingDirectory = Split-Path $Exe
  }

  Write-Host "Starting proxy core '$($Core.name)'..."
  $Proc = Start-Process `
    -FilePath $Exe `
    -ArgumentList (Join-ProcessArguments $Args) `
    -WorkingDirectory $WorkingDirectory `
    -RedirectStandardOutput $StdoutLog `
    -RedirectStandardError $StderrLog `
    -PassThru
  Set-Content -Path $PidFile -Value $Proc.Id
}

function Stop-Core {
  param($Core)
  $PidFile = Get-CorePidFile $Core
  $Proc = Get-RunningProcessFromPidFile $PidFile
  if ($null -eq $Proc) {
    return
  }
  Write-Host "Stopping proxy core '$($Core.name)' ($($Proc.Id))..."
  Stop-Process -Id $Proc.Id -Force -ErrorAction SilentlyContinue
  Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
}

function Show-CoreStatus {
  param($Core)
  $PidFile = Get-CorePidFile $Core
  $Proc = Get-RunningProcessFromPidFile $PidFile
  if ($null -eq $Proc) {
    Write-Host "proxy core $($Core.name): stopped"
  } else {
    Write-Host "proxy core $($Core.name): running pid=$($Proc.Id) process=$($Proc.ProcessName)"
  }
}

$ConfigData = Read-Config
$Cores = Get-EnabledCores $ConfigData

if ($Stop) {
  foreach ($Core in $Cores) {
    Stop-Core $Core
  }
  exit 0
}

if ($Status) {
  foreach ($Core in $Cores) {
    Show-CoreStatus $Core
  }
  exit 0
}

if ($ConfigData.PSObject.Properties.Name -contains "watch" -and $ConfigData.watch.PSObject.Properties.Name -contains "intervalSeconds" -and $ConfigData.watch.intervalSeconds) {
  $IntervalSeconds = [int]$ConfigData.watch.intervalSeconds
}

Write-Host "Proxy core watcher started. Config: $Config"
try {
  while ($true) {
    if (Test-Sub2ApiRunning $ConfigData) {
      foreach ($Core in $Cores) {
        Start-Core $Core
      }
    } else {
      foreach ($Core in $Cores) {
        Stop-Core $Core
      }
    }
    Start-Sleep -Seconds $IntervalSeconds
  }
} finally {
  foreach ($Core in $Cores) {
    Stop-Core $Core
  }
}

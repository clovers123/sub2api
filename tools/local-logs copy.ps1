param(
  [int]$Tail = 80
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")
$TmpDir = Join-Path $RootDir "tmp"
$Logs = @(
  Join-Path $TmpDir "sub2api-backend.log"
  Join-Path $TmpDir "sub2api-backend.err.log"
  Join-Path $TmpDir "sub2api-frontend.log"
  Join-Path $TmpDir "sub2api-frontend.err.log"
  Join-Path $TmpDir "sub2api-proxy-cores-watcher.log"
  Join-Path $TmpDir "sub2api-proxy-cores-watcher.err.log"
) | Where-Object { Test-Path $_ }

if ($Logs.Count -eq 0) {
  Write-Host "No local logs found under $TmpDir"
  exit 0
}

Get-Content -Path $Logs -Tail $Tail -Wait

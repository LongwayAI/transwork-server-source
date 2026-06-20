[CmdletBinding()]
param(
    [int]$Port = 3001,
    [string]$ExecutableName = "transwork-server.local.exe",
    [switch]$Start,
    [switch]$Foreground
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$GoExe = "C:\Program Files\Go\bin\go.exe"

if (-not (Test-Path $GoExe)) {
    throw "Go not found at '$GoExe'. Update the script or install Go there."
}

& (Join-Path $PSScriptRoot "stop-local.ps1") -Port $Port -ExecutableName $ExecutableName

Push-Location $RepoRoot
try {
    & $GoExe build -o $ExecutableName .
}
finally {
    Pop-Location
}

Write-Host "Built $ExecutableName"

if ($Start) {
    & (Join-Path $PSScriptRoot "start-local.ps1") -Port $Port -ExecutableName $ExecutableName -Foreground:$Foreground
}

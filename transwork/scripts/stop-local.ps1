[CmdletBinding()]
param(
    [int]$Port = 3001,
    [string]$ExecutableName = "transwork-server.local.exe",
    [switch]$KillPortOwner
)

$ErrorActionPreference = "Stop"

$ProcessName = [System.IO.Path]::GetFileNameWithoutExtension($ExecutableName)
$StoppedAny = $false

$NamedProcesses = Get-Process -Name $ProcessName -ErrorAction SilentlyContinue
if ($NamedProcesses) {
    $NamedProcesses | Stop-Process -Force
    foreach ($Process in $NamedProcesses) {
        Write-Host "Stopped $ProcessName (PID $($Process.Id))"
    }
    $StoppedAny = $true
}

$PortListener = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
if ($PortListener) {
    if ($KillPortOwner) {
        Stop-Process -Id $PortListener.OwningProcess -Force
        Write-Host "Stopped process on port $Port (PID $($PortListener.OwningProcess))"
        $StoppedAny = $true
    }
    elseif (-not $StoppedAny) {
        Write-Host "Port $Port is still owned by PID $($PortListener.OwningProcess)."
        Write-Host "Re-run with -KillPortOwner if you want this script to terminate that process."
    }
}
elseif (-not $StoppedAny) {
    Write-Host "No local Gressio server process found."
}

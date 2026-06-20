[CmdletBinding()]
param(
    [int]$Port = 3001,
    [string]$ExecutableName = "transwork-server.local.exe",
    [switch]$Build,
    [switch]$Foreground
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$ExecutablePath = Join-Path $RepoRoot $ExecutableName
$OutLog = Join-Path $RepoRoot "transwork-server.local.out.log"
$ErrLog = Join-Path $RepoRoot "transwork-server.local.err.log"
$ProcessName = [System.IO.Path]::GetFileNameWithoutExtension($ExecutableName)
$GoExe = "C:\Program Files\Go\bin\go.exe"

if ($Build -or -not (Test-Path $ExecutablePath)) {
    if (-not (Test-Path $GoExe)) {
        throw "Go not found at '$GoExe'. Update the script or install Go there."
    }

    Push-Location $RepoRoot
    try {
        & $GoExe build -o $ExecutableName .
    }
    finally {
        Pop-Location
    }
}

$ExistingProcess = Get-Process -Name $ProcessName -ErrorAction SilentlyContinue
if ($ExistingProcess) {
    throw "A '$ProcessName' process is already running. Stop it first with transwork\scripts\stop-local.ps1."
}

Push-Location $RepoRoot
try {
    if ($Foreground) {
        $env:PORT = $Port.ToString()
        & $ExecutablePath --port $Port
    }
    else {
        $Started = Start-Process `
            -FilePath $ExecutablePath `
            -ArgumentList @("--port", $Port) `
            -WorkingDirectory $RepoRoot `
            -RedirectStandardOutput $OutLog `
            -RedirectStandardError $ErrLog `
            -WindowStyle Hidden `
            -PassThru

        Write-Host "Started $ExecutableName (PID $($Started.Id)) on http://127.0.0.1:$Port"
        Write-Host "stdout: $OutLog"
        Write-Host "stderr: $ErrLog"
    }
}
finally {
    Pop-Location
}

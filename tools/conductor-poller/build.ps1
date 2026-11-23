$ErrorActionPreference = 'Stop'

$outputDirectory = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../.conductor/.local'))
$output = Join-Path $outputDirectory 'poller.exe'
$null = New-Item -ItemType Directory -Path $outputDirectory -Force

Push-Location -LiteralPath $PSScriptRoot
try {
    go build -o $output .
    if ($LASTEXITCODE -ne 0) {
        throw "Conductor Poller build failed (exit code $LASTEXITCODE)."
    }
} finally {
    Pop-Location
}

Write-Output $output

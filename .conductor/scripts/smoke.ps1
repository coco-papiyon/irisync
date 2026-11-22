param(
    [Parameter(Mandatory)]
    [ValidateSet('success', 'failure', 'timeout')]
    [string] $Mode
)

$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)

if ($Mode -eq 'timeout') {
    Start-Sleep -Seconds 45
}

[ordered]@{
    mode = $Mode
    working_directory = (Get-Location).ProviderPath
} | ConvertTo-Json -Compress

if ($Mode -eq 'failure') {
    exit 7
}

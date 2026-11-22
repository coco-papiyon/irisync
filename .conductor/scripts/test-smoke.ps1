$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

$workflow = Join-Path $PSScriptRoot '../workflows/smoke.yaml'
$tempDirectory = Join-Path ([System.IO.Path]::GetTempPath()) ('irisync-smoke-' + [guid]::NewGuid().ToString('N'))
$workDirectory = Join-Path $tempDirectory '日本語 空白'
$previousConductorHome = $env:CONDUCTOR_HOME
$previousUpdateCheck = $env:CONDUCTOR_NO_UPDATE_CHECK
$previousPythonUtf8 = $env:PYTHONUTF8

try {
    $null = New-Item -ItemType Directory -Path $tempDirectory
    $null = New-Item -ItemType Directory -Path $workDirectory
    $probe = Join-Path $workDirectory '確認 用.ps1'
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'smoke.ps1') -Destination $probe
    $env:CONDUCTOR_HOME = Join-Path $tempDirectory 'conductor-home'
    $env:CONDUCTOR_NO_UPDATE_CHECK = '1'
    $env:PYTHONUTF8 = '1'

    conductor validate $workflow
    if ($LASTEXITCODE -ne 0) {
        throw "Conductor validation failed with exit code $LASTEXITCODE."
    }

    $expectedExit = @{ success = '0'; failure = '7' }
    foreach ($mode in @('success', 'failure')) {
        $result = conductor --silent run $workflow --no-interactive `
            --input "mode=$mode" --input "work_dir=$workDirectory" --input "script_path=$probe"
        if ($LASTEXITCODE -ne 0) {
            throw "Conductor failed to route '$mode' with exit code $LASTEXITCODE."
        }
        $report = ($result -join "`n") | ConvertFrom-Json
        if ($report.branch -ne $mode -or $report.script_exit -ne $expectedExit[$mode] -or
            $report.mode -ne $mode -or $report.working_directory -cne $workDirectory) {
            throw "Unexpected workflow report for '$mode': $result"
        }
        Write-Output "Verified '$mode' through Conductor."
    }

    $result = conductor --silent run $workflow --no-interactive `
        --input 'mode=timeout' --input "work_dir=$workDirectory" --input "script_path=$probe" 2>&1
    if ($LASTEXITCODE -eq 0 -or ($result -join "`n") -notmatch 'timed out after 8s') {
        throw "Expected a Conductor timeout: $result"
    }
    Write-Output 'Verified the Script step timeout.'
} finally {
    $env:CONDUCTOR_HOME = $previousConductorHome
    $env:CONDUCTOR_NO_UPDATE_CHECK = $previousUpdateCheck
    $env:PYTHONUTF8 = $previousPythonUtf8
    if (Test-Path -LiteralPath $tempDirectory) {
        $resolvedDirectory = (Resolve-Path -LiteralPath $tempDirectory).ProviderPath
        if ($resolvedDirectory -ceq $tempDirectory) {
            Remove-Item -LiteralPath $resolvedDirectory -Recurse -Force
        }
    }
}

param([string] $ScratchDirectory)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$root = (Get-Location).ProviderPath
$scratch = $ScratchDirectory
$ownsScratch = !$scratch
if ($ownsScratch) {
    $scratch = Join-Path $root ('.conductor/.local/' + [guid]::NewGuid().ToString('N').Substring(0, 12))
    $scratch = [IO.Path]::GetFullPath($scratch)
    $null = New-Item -ItemType Directory -Path $scratch
}
$originalTemp = $env:TEMP
$originalTmp = $env:TMP
try {
    $env:TEMP = $scratch
    $env:TMP = $scratch
    $env:GOCACHE = Join-Path $scratch 'go-cache'
    $env:GOPROXY = 'off'
    $env:GOTOOLCHAIN = 'local'
    $env:GOWORK = 'off'
    # Tests create Git repositories under this worktree's temporary directory.
    $gitConfigIndex = [int]$env:GIT_CONFIG_COUNT
    Set-Item -LiteralPath "Env:GIT_CONFIG_KEY_$gitConfigIndex" -Value 'core.longpaths'
    Set-Item -LiteralPath "Env:GIT_CONFIG_VALUE_$gitConfigIndex" -Value 'true'
    $env:GIT_CONFIG_COUNT = [string]($gitConfigIndex + 1)
    foreach ($module in @('.', 'tools/conductor-poller')) {
        $path = Join-Path $root $module
        if (!(Test-Path -LiteralPath (Join-Path $path 'go.mod') -PathType Leaf)) { continue }
        Push-Location -LiteralPath $path
        try {
            go vet -mod=readonly ./...
            if ($LASTEXITCODE -ne 0) { Write-Output "go vet failed in $module"; exit 21 }
            go test -mod=readonly ./...
            if ($LASTEXITCODE -ne 0) { Write-Output "go test failed in $module"; exit 22 }
            go build -mod=readonly -buildvcs=false -o NUL ./...
            if ($LASTEXITCODE -ne 0) { Write-Output "go build failed in $module"; exit 23 }
        } finally {
            Pop-Location
        }
    }
    if (Test-Path -LiteralPath '.conductor/scripts/test-smoke.ps1' -PathType Leaf) {
        pwsh -NoProfile -NonInteractive -File .conductor/scripts/test-smoke.ps1
        if ($LASTEXITCODE -ne 0) { exit 24 }
    }
} finally {
    $env:TEMP = $originalTemp
    $env:TMP = $originalTmp
    if ($ownsScratch) {
        $resolved = (Resolve-Path -LiteralPath $scratch).ProviderPath
        if ($resolved -ceq $scratch) { Remove-Item -LiteralPath $resolved -Recurse -Force }
    }
}

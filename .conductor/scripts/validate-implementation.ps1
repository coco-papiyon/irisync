$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$root = (Get-Location).ProviderPath
$cache = Join-Path ([IO.Path]::GetTempPath()) ('irisync-go-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $cache
try {
    $env:GOCACHE = $cache
    $env:GOPROXY = 'off'
    $env:GOTOOLCHAIN = 'local'
    $env:GOWORK = 'off'
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
    $resolved = (Resolve-Path -LiteralPath $cache).ProviderPath
    if ($resolved -ceq $cache) { Remove-Item -LiteralPath $resolved -Recurse -Force }
}

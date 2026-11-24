$ErrorActionPreference = 'Stop'
$project = Split-Path -Parent $PSScriptRoot
Push-Location $project
try {
    $output = Join-Path $project 'bin/urio.exe'
    $staged = Join-Path $project 'bin/urio-new.exe'
    go build -trimpath -o $staged ./cmd/urio
    if ($LASTEXITCODE -ne 0) { throw 'build failed' }
    if (Test-Path -LiteralPath $output) {
        # A running executable may be renamed on Windows. Never stop its owner.
        Move-Item -LiteralPath $output -Destination ($output + '.obsolete-' + (Get-Date -Format 'yyyyMMdd-HHmmss-fff'))
    }
    Move-Item -LiteralPath $staged -Destination $output
    Write-Output $output
} finally { Pop-Location }

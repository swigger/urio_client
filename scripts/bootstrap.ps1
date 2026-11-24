$ErrorActionPreference = 'Stop'
$project = Split-Path -Parent $PSScriptRoot
$repos = @(
    @('connect', 'connect', 'c9948a7176a284f4dc6ffc7798ff9e321943adbb'),
    @('.deps/glog', 'glog', 'e4773a4579a56a710f4a90b68580d2e82263b114'),
    @('.deps/gvisor', 'gvisor', '551b6e3f1252d776f947863e9a3f5b802833e588'),
    @('.deps/quic-go', 'quic-go', '981433dd3ad638089c757ee40318d7ab8787bde5')
)
foreach ($repo in $repos) {
    $dest = Join-Path $project $repo[0]
    if (-not (Test-Path -LiteralPath $dest)) {
        git init $dest
        if ($LASTEXITCODE -ne 0) { throw 'git init failed' }
        git -C $dest remote add origin ('https://github.com/urnetwork/' + $repo[1])
        git -C $dest fetch --depth 1 origin $repo[2]
        if ($LASTEXITCODE -ne 0) { throw 'git fetch failed' }
        git -C $dest checkout --detach FETCH_HEAD
        if ($LASTEXITCODE -ne 0) { throw 'git checkout failed' }
    }
    $head = git -C $dest rev-parse HEAD
    if ($head -ne $repo[2]) { throw "Dependency $dest has a different revision; keep your changes and prepare the pinned revision separately." }
}
Push-Location $project
try { go mod download; if ($LASTEXITCODE -ne 0) { throw 'go mod download failed' } } finally { Pop-Location }

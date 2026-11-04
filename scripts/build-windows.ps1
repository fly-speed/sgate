# Build a standalone Windows client bundle; existing configuration is preserved.
param(
    [ValidateSet('amd64','arm64')][string]$Architecture = 'amd64',
    [string]$Destination = (Join-Path $PSScriptRoot '../dist/windows-client')
)
$ErrorActionPreference = 'Stop'
$SourceRoot = Split-Path -Parent $PSScriptRoot
$Destination = [IO.Path]::GetFullPath($Destination)
New-Item -ItemType Directory -Force -Path $Destination | Out-Null
foreach ($Directory in @('bin','conf','scripts','web/admin','licenses','docs')) {
    New-Item -ItemType Directory -Force -Path (Join-Path $Destination $Directory) | Out-Null
}
$PreviousOS = $env:GOOS
$PreviousArch = $env:GOARCH
$PreviousCGO = $env:CGO_ENABLED
Push-Location $SourceRoot
try {
    $env:GOOS='windows'; $env:GOARCH=$Architecture; $env:CGO_ENABLED='0'
    & go build -mod=mod -trimpath -o (Join-Path $Destination 'bin/sgate-client.exe') ./cmd/sgate-client
    if ($LASTEXITCODE -ne 0) { throw 'Windows client build failed' }
    & go build -mod=mod -trimpath '-ldflags=-H=windowsgui' -o (Join-Path $Destination 'bin/sgate-control.exe') ./cmd/sgate-control
    if ($LASTEXITCODE -ne 0) { throw 'Windows controller build failed' }
} finally {
    Pop-Location
    $env:GOOS=$PreviousOS; $env:GOARCH=$PreviousArch; $env:CGO_ENABLED=$PreviousCGO
}
$Archive = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName()+'.zip')
$Extracted = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
try {
    Invoke-WebRequest -UseBasicParsing -Uri 'https://www.wintun.net/builds/wintun-0.14.1.zip' -OutFile $Archive
    if ((Get-FileHash $Archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne '07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51') {
        throw 'Wintun download checksum mismatch'
    }
    Expand-Archive -Path $Archive -DestinationPath $Extracted
    Copy-Item (Join-Path $Extracted "wintun/bin/$Architecture/wintun.dll") (Join-Path $Destination 'bin/wintun.dll') -Force
    Copy-Item (Join-Path $Extracted 'wintun/LICENSE.txt') (Join-Path $Destination 'licenses/Wintun-LICENSE.txt') -Force
} finally {
    Remove-Item $Archive -ErrorAction SilentlyContinue
    Remove-Item $Extracted -Recurse -Force -ErrorAction SilentlyContinue
}
Copy-Item (Join-Path $SourceRoot 'web/admin/*') (Join-Path $Destination 'web/admin') -Recurse -Force
Copy-Item (Join-Path $SourceRoot 'docs/vpn.md') (Join-Path $Destination 'docs') -Force
Copy-Item (Join-Path $SourceRoot 'docs/vpn-udp.md') (Join-Path $Destination 'docs') -Force
Copy-Item (Join-Path $SourceRoot 'third_party/licenses/*.LICENSE') (Join-Path $Destination 'licenses') -Force
$ModuleCache = & go env GOMODCACHE
if ($LASTEXITCODE -ne 0) { throw 'Unable to locate the Go module cache' }
Copy-Item (Join-Path $ModuleCache 'golang.zx2c4.com/wintun@v0.0.0-20230126152724-0fa3db229ce2/LICENSE') (Join-Path $Destination 'licenses/wintun-go-LICENSE.txt') -Force
Copy-Item (Join-Path $SourceRoot 'scripts/start-windows-client.ps1') (Join-Path $Destination 'scripts') -Force
$Configuration = Join-Path $Destination 'conf/sgate-client.cf'
if (!(Test-Path $Configuration)) { Copy-Item (Join-Path $SourceRoot 'samples/windows-client.cf') $Configuration }
# Generate a fresh local placeholder, then replace it with the shared server key.
$KeyPath = Join-Path $Destination 'conf/sgate.key'
if (!(Test-Path $KeyPath)) {
    $Bytes = New-Object byte[] 32
    $Random = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $Random.GetBytes($Bytes) } finally { $Random.Dispose() }
    [IO.File]::WriteAllText($KeyPath, ([BitConverter]::ToString($Bytes).Replace('-','').ToLowerInvariant()))
}
Copy-Item (Join-Path $SourceRoot 'samples/test-tls/tls-cert.pem') (Join-Path $Destination 'conf') -Force
Write-Host "Built $Destination. Set endpoints and copy the matching shared server key to conf/sgate.key."
Write-Host 'Double-click bin/sgate-control.exe, accept the administrator prompt, then click Start client.'
Write-Host 'The controller starts/stops its own client process and opens the existing Web management UI.'

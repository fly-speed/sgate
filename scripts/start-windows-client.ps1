# Run this launcher from an elevated PowerShell session when using VPN.
$ErrorActionPreference = 'Stop'
$InstallRoot = Split-Path -Parent $PSScriptRoot
Push-Location (Join-Path $InstallRoot 'conf')
try {
    & (Join-Path $InstallRoot 'bin/sgate-client.exe') -alone -f sgate-client.cf
    exit $LASTEXITCODE
} finally {
    Pop-Location
}

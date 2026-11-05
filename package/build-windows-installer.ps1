# Build the client in a clean staging directory and compile a standalone Setup EXE.
param(
    [ValidateSet('amd64','arm64')][string]$Architecture = 'amd64',
    [string]$Destination = (Join-Path $PSScriptRoot '../dist/windows-installer'),
    [string]$Iscc
)
$ErrorActionPreference = 'Stop'
$Destination = [IO.Path]::GetFullPath($Destination)

if (!$Iscc) {
    $Compiler = Get-Command ISCC.exe -CommandType Application -ErrorAction SilentlyContinue
    if ($Compiler) {
        $Iscc = $Compiler.Source
    } else {
        foreach ($Root in @(${env:ProgramFiles(x86)}, $env:ProgramFiles, (Join-Path $env:LOCALAPPDATA 'Programs'))) {
            if (!$Root) { continue }
            foreach ($Edition in @('Inno Setup 6', 'Inno Setup 7')) {
                $Candidate = Join-Path $Root "$Edition/ISCC.exe"
                if (Test-Path -LiteralPath $Candidate -PathType Leaf) {
                    $Iscc = $Candidate
                    break
                }
            }
            if ($Iscc) { break }
        }
    }
}
if (!$Iscc) { throw 'Install Inno Setup 6.3 or newer, or pass -Iscc with the path to ISCC.exe.' }

$VersionFile = Join-Path $PSScriptRoot '../internal/buildinfo/version.go'
$VersionMatch = [regex]::Match((Get-Content -LiteralPath $VersionFile -Raw), '(?m)^const Version = "([0-9]+\.[0-9]+\.[0-9]+)"')
if (!$VersionMatch.Success) { throw 'Unable to read the sgate release version.' }
$Version = $VersionMatch.Groups[1].Value
$InstallerName = "sgate-client-$Version-windows-$Architecture-setup.exe"
$Workspace = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString('N'))
$Bundle = Join-Path $Workspace 'bundle'
New-Item -ItemType Directory -Path $Workspace | Out-Null
try {
    & (Join-Path $PSScriptRoot 'build-windows.ps1') -Architecture $Architecture -Destination $Bundle
    & $Iscc "/DBundle=$Bundle" "/DVersion=$Version" "/DArchitecture=$Architecture" "/O$Workspace" "/F$([IO.Path]::GetFileNameWithoutExtension($InstallerName))" (Join-Path $PSScriptRoot 'windows-installer.iss')
    if ($LASTEXITCODE -ne 0) { throw "Inno Setup compilation failed (exit code $LASTEXITCODE)." }
    New-Item -ItemType Directory -Force -Path $Destination | Out-Null
    Move-Item -LiteralPath (Join-Path $Workspace $InstallerName) -Destination (Join-Path $Destination $InstallerName) -Force
    Write-Host "Installer: $(Join-Path $Destination $InstallerName)"
} finally {
    Remove-Item -LiteralPath $Workspace -Recurse -Force
}

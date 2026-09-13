# Downloads wintun.dll, the virtual network adapter driver P2PV uses on Windows.
#
# Wintun is the WireGuard project's adapter driver: MIT-licensed, signed by
# WireGuard LLC, and installed by loading the DLL rather than by running an
# installer. That is why P2PV uses it -- a friend can unzip the client and run
# it, with no driver setup step.
#
# The DLL is not committed to the repository: it is a signed binary from another
# project, and shipping someone else's signed driver in your own repo is a good
# way to end up distributing a stale one.

$ErrorActionPreference = 'Stop'

$version = '0.14.1'
$url = "https://www.wintun.net/builds/wintun-$version.zip"
$root = Split-Path -Parent $PSScriptRoot
$zip = Join-Path $env:TEMP "wintun-$version.zip"
$extract = Join-Path $env:TEMP "wintun-$version"

# Known SHA256 for wintun-0.14.1.zip, published on wintun.net.
# Verified before use: this is a kernel driver, so an unverified download is
# not acceptable even over HTTPS.
$expectedHash = '07C256185D6EE3652E09FA55C0B673E2624B565E02C4B9091C79CA7D2F24EF51'

Write-Host "Downloading Wintun $version..." -ForegroundColor Cyan
Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing

$actualHash = (Get-FileHash -Path $zip -Algorithm SHA256).Hash
if ($actualHash -ne $expectedHash) {
    Remove-Item $zip -Force
    throw "SHA256 mismatch for wintun-$version.zip`n  expected $expectedHash`n  got      $actualHash`nRefusing to install an unverified driver."
}
Write-Host "SHA256 verified." -ForegroundColor Green

if (Test-Path $extract) { Remove-Item $extract -Recurse -Force }
Expand-Archive -Path $zip -DestinationPath $extract -Force

# Pick the DLL matching this machine's architecture.
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$dll = Join-Path $extract "wintun\bin\$arch\wintun.dll"
if (-not (Test-Path $dll)) { throw "wintun.dll for $arch not found in the archive" }

foreach ($dest in @((Join-Path $root 'build'), $root)) {
    if (-not (Test-Path $dest)) { New-Item -ItemType Directory -Path $dest -Force | Out-Null }
    Copy-Item $dll (Join-Path $dest 'wintun.dll') -Force
}

Remove-Item $zip -Force
Remove-Item $extract -Recurse -Force

Write-Host "wintun.dll ($arch) placed in $root and $root\build" -ForegroundColor Green
Write-Host "It must sit next to p2pv.exe when you run it." -ForegroundColor Gray

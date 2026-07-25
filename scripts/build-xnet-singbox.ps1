# ============================================================
#  Build the X-NET sing-box fork binary with the FULL production
#  build tags, forcing the SUPPORTED Go toolchain (1.25.x).
#
#  WHY: sing-box's common/badtls uses //go:linkname into crypto/tls
#  internals that changed in Go 1.26, so building with Go 1.26 fails at
#  LINK time (invalid reference to crypto/tls.(*Conn).handlePostHandshakeMessage).
#  The fork Dockerfile pins golang:1.25; this script does the same.
#
#  Usage:   powershell -File scripts\build-xnet-singbox.ps1 [-Out sing-box.exe]
#  Env:     $env:XNET_SINGBOX_GO = "go1.25.4"   # override the forced patch
#
#  NOTE: if this environment has GOSUMDB=off, Go cannot auto-download a
#  toolchain — install Go 1.25.x locally (then this script uses it directly)
#  or build via the fork Dockerfile (golang:1.25-alpine).
# ============================================================
param([string]$Out = "sing-box.exe")

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

$pinnedGo = if ($env:XNET_SINGBOX_GO) { $env:XNET_SINGBOX_GO } else { "go1.25.0" }
$tags = (Get-Content "release/DEFAULT_BUILD_TAGS_OTHERS" -Raw).Trim()
$localver = (go env GOVERSION)

if ($localver -like "go1.25.*") {
    $env:GOTOOLCHAIN = "local"      # supported local Go: use directly
} else {
    $env:GOTOOLCHAIN = $pinnedGo    # force the supported toolchain (auto-download on normal hosts)
}

Write-Output ">> local Go: $localver"
Write-Output ">> GOTOOLCHAIN: $($env:GOTOOLCHAIN)"
Write-Output ">> tags: $tags"
Write-Output ">> output: $Out"

go build -trimpath -ldflags "-s -w -buildid=" -tags "$tags" -o "$Out" ./cmd/sing-box
if ($LASTEXITCODE -ne 0) { throw "sing-box build failed (exit $LASTEXITCODE)" }
Write-Output ">> built: $Out"

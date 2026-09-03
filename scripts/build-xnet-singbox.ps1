# ============================================================
#  Build the X-NET sing-box fork binary with the FULL production
#  build tags and ldflags — the same combination CI ships.
#
#  WHY THE LDFLAGS MATTER
#
#  sing-box's production tag set includes `badlinkname` and
#  `tfogo_checklinkname0`, and several dependencies (common/badtls,
#  tfo-go) reach into stdlib internals with //go:linkname. Since Go 1.23
#  the linker rejects those references unless it is told not to check:
#
#     link: github.com/database64128/tfo-go/v2: invalid reference to net.(*netFD).init
#     link: .../common/badtls: invalid reference to
#           crypto/tls.(*Conn).handlePostHandshakeMessage
#
#  `release/LDFLAGS` carries the `-checklinkname=0` that suppresses this
#  (plus the godebug defaults the release binaries are built with). An
#  earlier version of this script omitted release/LDFLAGS and pinned the
#  toolchain to Go 1.25 instead, on the theory that Go 1.26 had broken
#  badtls. That diagnosis was wrong: the failure is the missing ldflag,
#  not the toolchain. Verified on go1.26.0 — building with these ldflags
#  links cleanly, and without them it fails on Go 1.25 too.
#
#  So: no toolchain pin. sing-box 1.14 needs Go >= 1.25 (enforced by the
#  `go` directive in go.mod); any newer Go works.
#
#  Usage:   powershell -File scripts\build-xnet-singbox.ps1 [-Out sing-box.exe]
# ============================================================
param([string]$Out = "sing-box.exe")

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

# X-NET production tags = upstream defaults + the two capability tags the
# panel probes for on the `sing-box version` Tags line:
#   with_v2ray_api          -> authoritative per-UUID stats accounting
#   with_session_admission  -> in-core pre-connection device-limit gate
# These MUST match .github/workflows/build-xnet.yml, or a locally built
# binary silently loses features the panel detects by tag.
$tags = (Get-Content "release/DEFAULT_BUILD_TAGS_OTHERS" -Raw).Trim() + ",with_v2ray_api,with_session_admission"
$ldflags = (Get-Content "release/LDFLAGS" -Raw).Trim() + " -s -w -buildid="

Write-Output ">> Go:      $(go env GOVERSION)"
Write-Output ">> tags:    $tags"
Write-Output ">> ldflags: $ldflags"
Write-Output ">> output:  $Out"

go build -trimpath -ldflags "$ldflags" -tags "$tags" -o "$Out" ./cmd/sing-box
if ($LASTEXITCODE -ne 0) { throw "sing-box build failed (exit $LASTEXITCODE)" }
Write-Output ">> built: $Out"

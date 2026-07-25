#!/usr/bin/env bash
# ============================================================
#  Build the X-NET sing-box fork binary with the FULL production
#  build tags, forcing the SUPPORTED Go toolchain (1.25.x).
#
#  WHY: sing-box's common/badtls uses //go:linkname into crypto/tls
#  internals that changed in Go 1.26, so building with Go 1.26 fails at
#  LINK time:
#     link: .../common/badtls: invalid reference to
#           crypto/tls.(*Conn).handlePostHandshakeMessage
#  The fork Dockerfile pins golang:1.25; this script does the same for
#  a bare-metal build. It is unrelated to the X-NET session-admission
#  code, which compiles cleanly.
#
#  Usage:  scripts/build-xnet-singbox.sh [output-path]
#  Env:    XNET_SINGBOX_GO=go1.25.4   # override the forced toolchain patch
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."

PINNED_GO="${XNET_SINGBOX_GO:-go1.25.0}"
TAGS="$(cat release/DEFAULT_BUILD_TAGS_OTHERS)"
OUT="${1:-sing-box}"

localver="$(go env GOVERSION 2>/dev/null || echo unknown)"
case "$localver" in
  go1.25.*)
    # A supported local Go: use it directly (no download).
    export GOTOOLCHAIN=local
    ;;
  *)
    # Any other local Go (incl. 1.26+ which breaks badtls): FORCE the
    # supported toolchain. Go auto-downloads it (needs network + an enabled
    # GOSUMDB; if your environment has GOSUMDB=off, install Go 1.25.x instead).
    export GOTOOLCHAIN="$PINNED_GO"
    ;;
esac

echo ">> local Go: $localver"
echo ">> GOTOOLCHAIN: $GOTOOLCHAIN"
echo ">> tags: $TAGS"
echo ">> output: $OUT"

go build -trimpath -ldflags "-s -w -buildid=" -tags "$TAGS" -o "$OUT" ./cmd/sing-box

echo ">> built: $OUT"

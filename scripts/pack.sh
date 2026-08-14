#!/usr/bin/env bash
# zlite release packaging: multi-platform matrix build → assemble tar.gz / zip.
#
# Shared with Makefile (make build) for local dev; this script is for release:
#   make build     — quick local single-binary build (no packaging)
#   pack.sh        — release: multi-platform matrix + tar.gz/zip (used by CI release.yml)
#
# Output (under bin/):
#   linux/*            -> zlite-v<version>-linux-<GOARCH>.tar.gz
#   darwin|windows/*   -> zlite-v<version>-<GOOS>-<GOARCH>.zip
# Package contents (top-level dir, avoids polluting extraction dir):
#   zlite-v<version>-<GOOS>-<GOARCH>/
#   └── zlite                      (Windows: zlite.exe)
#
# Format: Linux uses .tar.gz (tar is a base tool on every distro, unzip often missing);
# macOS / Windows use .zip.
#
# Version source: internal/version/version.go (the Version variable);
# overridable via ZLITE_VERSION env var.
#
# Usage:
#   ./scripts/pack.sh                        # native platform single package
#   ./scripts/pack.sh --all                  # 6 platforms (CI release)
#   GOOS=darwin GOARCH=arm64 ./scripts/pack.sh
#   ZLITE_VERSION=0.2.0 ./scripts/pack.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# --- Argument parsing ---
ALL=0
for arg in "$@"; do
  case "$arg" in
    --all) ALL=1 ;;
    *)
      echo "error: unknown argument $arg" >&2
      echo "usage: ./scripts/pack.sh [--all]" >&2
      exit 2
      ;;
  esac
done

# --- 1. Version / commit / build time (injected via -ldflags, same as Makefile) ---
# Version extracted from internal/version/version.go (the single source of truth).
VERSION="${ZLITE_VERSION:-$(grep -oP 'Version = "\K[^"]+' internal/version/version.go)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w \
  -X github.com/helloxz/zlite/internal/version.Commit=${COMMIT} \
  -X github.com/helloxz/zlite/internal/version.BuildTime=${BUILD_TIME}"
echo "==> Version: v${VERSION} (commit ${COMMIT}, built ${BUILD_TIME})"

# --- 2. Platform matrix ---
NATIVE_GOOS="$(env -u GOOS -u GOARCH go env GOOS)"
NATIVE_GOARCH="$(env -u GOOS -u GOARCH go env GOARCH)"
if [ "$ALL" = 1 ]; then
  PLATFORMS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"
  echo "==> Full platform matrix: ${PLATFORMS}"
else
  PLATFORMS="${GOOS:-$NATIVE_GOOS}/${GOARCH:-$NATIVE_GOARCH}"
  echo "==> Single platform: ${PLATFORMS}"
fi

mkdir -p bin
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

# --- 3. Per-platform: build → assemble package (linux = tar.gz, others = zip) ---
for p in $PLATFORMS; do
  GOOS="${p%/*}"
  GOARCH="${p#*/}"
  export GOOS GOARCH
  BIN="zlite"; [ "$GOOS" = "windows" ] && BIN="zlite.exe"
  DIR="zlite-v${VERSION}-${GOOS}-${GOARCH}"
  PKGDIR="${STAGE}/${DIR}"

  echo ""
  echo "==> [${GOOS}/${GOARCH}] Building (CGO_ENABLED=0)"
  mkdir -p "$PKGDIR"
  CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$PKGDIR/$BIN" ./cmd/zlite

  # Assemble package: binary only
  if [ "$GOOS" = "linux" ]; then
    OUT="${ROOT}/bin/${DIR}.tar.gz"
    (cd "$STAGE" && tar -czf "$OUT" "$DIR")
  else
    OUT="${ROOT}/bin/${DIR}.zip"
    if command -v zip >/dev/null 2>&1; then
      (cd "$STAGE" && zip -qr "$OUT" "$DIR")
    elif command -v python3 >/dev/null 2>&1; then
      (cd "$STAGE" && python3 -m zipfile -c "$OUT" "$DIR")
    else
      echo "error: packaging ${GOOS}/${GOARCH} requires zip or python3" >&2
      exit 1
    fi
  fi
  echo "    package: $(basename "$OUT") ($(wc -c < "$OUT") bytes)"
done

echo ""
echo "Done: $(ls bin/zlite-v*.tar.gz bin/zlite-v*.zip 2>/dev/null | wc -l) packages in bin/"

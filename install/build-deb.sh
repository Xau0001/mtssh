#!/usr/bin/env bash
# build-deb.sh — Builds a .deb package for MTSSH
# Usage: bash build-deb.sh [VERSION]
set -e

VERSION="${1:-1.0.0}"
ARCH="$(dpkg --print-architecture 2>/dev/null || echo amd64)"
PKGNAME="mtssh_${VERSION}_${ARCH}"
BUILD="dist/deb/${PKGNAME}"

echo "==> Building .deb package: ${PKGNAME}.deb"

# ── Dependency check ──────────────────────────────────────────────────────────
for cmd in go dpkg-deb; do
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: '$cmd' not found. Install it and try again."
    exit 1
  fi
done

# ── Build binary ──────────────────────────────────────────────────────────────
echo "--> Compiling binary…"
go build -ldflags "-s -w -X main.Version=${VERSION}" -o mtssh .

# ── Create package directory structure ───────────────────────────────────────
rm -rf "$BUILD"
mkdir -p "$BUILD/DEBIAN"
mkdir -p "$BUILD/usr/local/bin"
mkdir -p "$BUILD/usr/share/applications"
mkdir -p "$BUILD/usr/share/doc/mtssh"

# ── Copy files ────────────────────────────────────────────────────────────────
cp mtssh "$BUILD/usr/local/bin/mtssh"
chmod 755 "$BUILD/usr/local/bin/mtssh"

cp install/mtssh.desktop "$BUILD/usr/share/applications/mtssh.desktop"

cat > "$BUILD/usr/share/doc/mtssh/copyright" << 'EOF'
MTSSH — Multi-Tabbed SSH Client
Licensed under the MIT License.
EOF

# ── control file ──────────────────────────────────────────────────────────────
cat > "$BUILD/DEBIAN/control" << EOF
Package: mtssh
Version: ${VERSION}
Section: net
Priority: optional
Architecture: ${ARCH}
Depends: libgl1, libx11-6
Maintainer: MTSSH Project
Description: Multi-Tabbed SSH Client
 A graphical SSH client with tabs, SFTP file manager,
 AES-encrypted session storage, themes, and multi-window support.
EOF

# ── Build .deb ────────────────────────────────────────────────────────────────
mkdir -p dist/deb
dpkg-deb --build "$BUILD" "dist/deb/${PKGNAME}.deb"

echo ""
echo "==> SUCCESS: dist/deb/${PKGNAME}.deb"
echo ""
echo "Install with:"
echo "  sudo dpkg -i dist/deb/${PKGNAME}.deb"
echo "  sudo apt-get install -f    # fix dependencies if needed"

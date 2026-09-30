#!/usr/bin/env bash
# build-deb.sh — Builds a .deb package for MTSSH
# Usage: bash build-deb.sh [VERSION]
set -e

VERSION="${1:-1.0.0}"
# Debian sorts "~" before everything: 1.2.0~rc1 < 1.2.0 (a "-" would make
# "rc1" the Debian revision and sort the pre-release after the release).
PKGVER="${VERSION//-/\~}"
ARCH="$(dpkg --print-architecture 2>/dev/null || echo amd64)"
PKGNAME="mtssh_${PKGVER}_${ARCH}"
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
# Packaged builds are updated through the package manager, not in place.
go build -trimpath -ldflags "-s -w -X main.Version=${VERSION} -X mtssh/core.Packaged=true" -o mtssh .

# ── Create package directory structure ───────────────────────────────────────
rm -rf "$BUILD"
mkdir -p "$BUILD/DEBIAN"
mkdir -p "$BUILD/usr/bin"
mkdir -p "$BUILD/usr/share/applications"
mkdir -p "$BUILD/usr/share/icons/hicolor/512x512/apps"
mkdir -p "$BUILD/usr/share/doc/mtssh"

# ── Copy files ────────────────────────────────────────────────────────────────
cp mtssh "$BUILD/usr/bin/mtssh"
chmod 755 "$BUILD/usr/bin/mtssh"

cp install/mtssh.desktop "$BUILD/usr/share/applications/mtssh.desktop"
cp icon.png "$BUILD/usr/share/icons/hicolor/512x512/apps/mtssh.png"

cat > "$BUILD/usr/share/doc/mtssh/copyright" << 'EOF'
MTSSH — Multi-Tabbed SSH Client
Licensed under the MIT License.
EOF

# ── control file ──────────────────────────────────────────────────────────────
cat > "$BUILD/DEBIAN/control" << EOF
Package: mtssh
Version: ${PKGVER}
Section: net
Priority: optional
Architecture: ${ARCH}
Depends: libgl1, libx11-6, libwayland-client0
Maintainer: MTSSH Project
Description: Multi-Tabbed SSH Client
 A graphical SSH client with tabs, SFTP file manager,
 AES-encrypted session storage, themes, and multi-window support.
EOF

# ── Build .deb ────────────────────────────────────────────────────────────────
mkdir -p dist/deb
# Files must belong to root on the target system, not to whoever built the
# package (in CI: uid 1001, a regular user account on most machines).
dpkg-deb --root-owner-group --build "$BUILD" "dist/deb/${PKGNAME}.deb"
if dpkg-deb -c "dist/deb/${PKGNAME}.deb" | awk '$2 != "root/root" { bad = 1 } END { exit bad }'; then :; else
  echo "ERROR: package contains files not owned by root/root" >&2
  exit 1
fi

echo ""
echo "==> SUCCESS: dist/deb/${PKGNAME}.deb"
echo ""
echo "Install with:"
echo "  sudo dpkg -i dist/deb/${PKGNAME}.deb"
echo "  sudo apt-get install -f    # fix dependencies if needed"

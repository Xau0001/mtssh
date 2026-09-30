#!/usr/bin/env bash
# build-rpm.sh — Builds an .rpm package for MTSSH
# Usage: bash build-rpm.sh [VERSION]
set -e

VERSION="${1:-1.0.0}"
# RPM forbids "-" in Version; "~" sorts before the release: 1.2.0~rc1 < 1.2.0
PKGVER="${VERSION//-/\~}"
RELEASE="1"
ARCH="$(uname -m)"

echo "==> Building .rpm package: mtssh-${PKGVER}-${RELEASE}.${ARCH}.rpm"

# ── Dependency check ──────────────────────────────────────────────────────────
for cmd in go rpmbuild; do
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: '$cmd' not found."
    echo "Install with: sudo dnf install rpm-build golang gcc mesa-libGL-devel libX11-devel wayland-devel libxkbcommon-devel"
    exit 1
  fi
done

# ── Build binary ──────────────────────────────────────────────────────────────
echo "--> Compiling binary…"
# Packaged builds are updated through the package manager, not in place.
go build -trimpath -ldflags "-s -w -X main.Version=${VERSION} -X mtssh/core.Packaged=true" -o mtssh .

# ── Setup rpmbuild tree ───────────────────────────────────────────────────────
RPMBUILD="${HOME}/rpmbuild"
mkdir -p "${RPMBUILD}"/{BUILD,RPMS,SOURCES,SPECS,SRPMS}

cp mtssh "${RPMBUILD}/SOURCES/mtssh"
cp install/mtssh.desktop "${RPMBUILD}/SOURCES/mtssh.desktop"
cp icon.png "${RPMBUILD}/SOURCES/mtssh.png"

# ── Generate .spec ────────────────────────────────────────────────────────────
cat > "${RPMBUILD}/SPECS/mtssh.spec" << EOF
Name:           mtssh
Version:        ${PKGVER}
Release:        ${RELEASE}%{?dist}
Summary:        Multi-Tabbed SSH Client
License:        MIT
URL:            https://github.com/Xau0001/mtssh

Requires:       mesa-libGL libX11 libwayland-client

%description
A graphical SSH client with tabs, SFTP file manager,
AES-encrypted session storage, themes, and multi-window support.

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/share/applications
mkdir -p %{buildroot}/usr/share/icons/hicolor/512x512/apps
install -m 755 %{_sourcedir}/mtssh %{buildroot}/usr/bin/mtssh
install -m 644 %{_sourcedir}/mtssh.desktop %{buildroot}/usr/share/applications/mtssh.desktop
install -m 644 %{_sourcedir}/mtssh.png %{buildroot}/usr/share/icons/hicolor/512x512/apps/mtssh.png

%files
/usr/bin/mtssh
/usr/share/applications/mtssh.desktop
/usr/share/icons/hicolor/512x512/apps/mtssh.png

%changelog
* $(date "+%a %b %d %Y") Build System <build@localhost> - ${PKGVER}-${RELEASE}
- Initial package
EOF

# ── Build RPM ────────────────────────────────────────────────────────────────
rpmbuild -bb "${RPMBUILD}/SPECS/mtssh.spec"

RPMFILE="$(find "${RPMBUILD}/RPMS" -name "mtssh-${PKGVER}-*.rpm" | head -1)"
RPMNAME="$(basename "$RPMFILE")"
mkdir -p dist/rpm
cp "$RPMFILE" dist/rpm/

echo ""
echo "==> SUCCESS: dist/rpm/${RPMNAME}"
echo ""
echo "Install with:"
echo "  sudo rpm -i dist/rpm/${RPMNAME}"
echo "  # or:"
echo "  sudo dnf install dist/rpm/${RPMNAME}"

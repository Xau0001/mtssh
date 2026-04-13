#!/usr/bin/env bash
# build-rpm.sh — Builds an .rpm package for MTSSH
# Usage: bash build-rpm.sh [VERSION]
set -e

VERSION="${1:-1.0.0}"
RELEASE="1"
ARCH="$(uname -m)"

echo "==> Building .rpm package: mtssh-${VERSION}-${RELEASE}.${ARCH}.rpm"

# ── Dependency check ──────────────────────────────────────────────────────────
for cmd in go rpmbuild; do
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: '$cmd' not found."
    echo "Install with: sudo dnf install rpm-build golang gcc mesa-libGL-devel libX11-devel"
    exit 1
  fi
done

# ── Build binary ──────────────────────────────────────────────────────────────
echo "--> Compiling binary…"
go build -ldflags "-s -w -X main.Version=${VERSION}" -o mtssh .

# ── Setup rpmbuild tree ───────────────────────────────────────────────────────
RPMBUILD="${HOME}/rpmbuild"
mkdir -p "${RPMBUILD}"/{BUILD,RPMS,SOURCES,SPECS,SRPMS}

cp mtssh "${RPMBUILD}/SOURCES/mtssh"
cp install/mtssh.desktop "${RPMBUILD}/SOURCES/mtssh.desktop"

# ── Generate .spec ────────────────────────────────────────────────────────────
cat > "${RPMBUILD}/SPECS/mtssh.spec" << EOF
Name:           mtssh
Version:        ${VERSION}
Release:        ${RELEASE}%{?dist}
Summary:        Multi-Tabbed SSH Client
License:        MIT
URL:            https://github.com/Xau0001/mtssh

Requires:       mesa-libGL libX11

%description
A graphical SSH client with tabs, SFTP file manager,
AES-encrypted session storage, themes, and multi-window support.

%install
mkdir -p %{buildroot}/usr/local/bin
mkdir -p %{buildroot}/usr/share/applications
install -m 755 %{_sourcedir}/mtssh %{buildroot}/usr/local/bin/mtssh
install -m 644 %{_sourcedir}/mtssh.desktop %{buildroot}/usr/share/applications/mtssh.desktop

%files
/usr/local/bin/mtssh
/usr/share/applications/mtssh.desktop

%changelog
* $(date "+%a %b %d %Y") Build System <build@localhost> - ${VERSION}-${RELEASE}
- Initial package
EOF

# ── Build RPM ────────────────────────────────────────────────────────────────
rpmbuild -bb "${RPMBUILD}/SPECS/mtssh.spec"

RPMFILE="$(find ${RPMBUILD}/RPMS -name "mtssh-*.rpm" | head -1)"
mkdir -p dist/rpm
cp "$RPMFILE" dist/rpm/

echo ""
echo "==> SUCCESS: dist/rpm/$(basename $RPMFILE)"
echo ""
echo "Install with:"
echo "  sudo rpm -i dist/rpm/$(basename $RPMFILE)"
echo "  # or:"
echo "  sudo dnf install dist/rpm/$(basename $RPMFILE)"

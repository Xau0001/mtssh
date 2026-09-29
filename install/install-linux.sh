#!/usr/bin/env bash
# install-linux.sh — Installs MTSSH on Debian/Ubuntu, Fedora/RHEL, or Arch/CachyOS
# Usage: bash install/install-linux.sh [--uninstall]
set -e

BINARY="mtssh"
INSTALL_DIR="/usr/local/bin"
DESKTOP_DIR="/usr/share/applications"
ICON_DIR="/usr/share/icons/hicolor/512x512/apps"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
# Derive version from git tag, fall back to "1.0.0".
# (An empty version would make the updater treat every release as newer.)
VERSION="$(git -C "$REPO_DIR" describe --tags --abbrev=0 2>/dev/null || true)"
VERSION="${VERSION#v}"
VERSION="${VERSION:-1.0.0}"

# ── Uninstall ─────────────────────────────────────────────────────────────────
if [[ "$1" == "--uninstall" ]]; then
    echo "==> Uninstalling MTSSH…"
    sudo rm -f "${INSTALL_DIR}/${BINARY}"
    sudo rm -f "${DESKTOP_DIR}/mtssh.desktop"
    sudo rm -f "${ICON_DIR}/mtssh.png"
    sudo gtk-update-icon-cache /usr/share/icons/hicolor 2>/dev/null || true
    echo "==> Done. Config files remain at ~/.mtssh/"
    exit 0
fi

echo "==> MTSSH Linux Installer"
echo ""

# ── Detect distro ─────────────────────────────────────────────────────────────
if [[ -f /etc/os-release ]]; then
    . /etc/os-release
    DISTRO="${ID}"
else
    DISTRO="unknown"
fi

echo "--> Detected distro: ${DISTRO}"

# ── Install build dependencies ────────────────────────────────────────────────
install_deps() {
    case "${DISTRO}" in
        ubuntu|debian|linuxmint|pop)
            echo "--> Installing dependencies (apt)…"
            sudo apt-get update -qq
            sudo apt-get install -y gcc libgl1-mesa-dev xorg-dev libwayland-dev libxkbcommon-dev golang-go
            ;;
        fedora|rhel|centos|rocky|alma)
            echo "--> Installing dependencies (dnf)…"
            sudo dnf install -y gcc mesa-libGL-devel libX11-devel \
                libXrandr-devel libXcursor-devel libXinerama-devel libXi-devel \
                wayland-devel libxkbcommon-devel golang
            ;;
        arch|cachyos|manjaro|endeavouros|garuda)
            echo "--> Installing dependencies (pacman)…"
            # -S without -y: a bare -Sy causes partial upgrades on Arch
            sudo pacman -S --needed --noconfirm gcc mesa libxrandr libxcursor \
                libxinerama libxi wayland libxkbcommon go
            ;;
        opensuse*|sles)
            echo "--> Installing dependencies (zypper)…"
            sudo zypper install -y gcc Mesa-libGL-devel libX11-devel wayland-devel libxkbcommon-devel go
            ;;
        *)
            echo "WARNING: Unknown distro '${DISTRO}'. Trying to continue without installing deps."
            echo "If the build fails, install: gcc, libGL-dev, libX11-dev, libwayland-dev, libxkbcommon-dev, go (>=1.21)"
            ;;
    esac
}

# ── Dependencies + Go check ───────────────────────────────────────────────────
# gcc and the GL/X11 headers are needed in every case, not only without Go.
install_deps

if ! command -v go &>/dev/null; then
    echo "ERROR: Go not found. Install Go >= 1.21 from https://go.dev/dl/ and re-run."
    exit 1
fi
# Go >= 1.21 downloads the toolchain required by go.mod automatically.
GO_MINOR=$(go env GOVERSION | sed -E 's/^go1\.([0-9]+).*/\1/')
if [[ "$GO_MINOR" =~ ^[0-9]+$ ]] && (( GO_MINOR < 21 )); then
    echo "WARNING: $(go env GOVERSION) found, but >= go1.21 is required."
    echo "         Consider upgrading Go: https://go.dev/dl/"
else
    echo "--> $(go env GOVERSION) found."
fi

# ── Build ─────────────────────────────────────────────────────────────────────
echo "--> Building MTSSH ${VERSION} from ${REPO_DIR}…"
cd "$REPO_DIR"
go build -ldflags "-s -w -X main.Version=${VERSION}" -o "${BINARY}" .

echo "--> Build successful."

# ── Install ───────────────────────────────────────────────────────────────────
echo "--> Installing binary to ${INSTALL_DIR}/${BINARY}…"
sudo install -Dm755 "${BINARY}" "${INSTALL_DIR}/${BINARY}"

echo "--> Installing .desktop file…"
sudo install -Dm644 install/mtssh.desktop "${DESKTOP_DIR}/mtssh.desktop"

echo "--> Installing icon…"
sudo install -Dm644 icon.png "${ICON_DIR}/mtssh.png"

# Update icon cache and desktop database
sudo gtk-update-icon-cache /usr/share/icons/hicolor 2>/dev/null || true
if command -v update-desktop-database &>/dev/null; then
    sudo update-desktop-database "${DESKTOP_DIR}" 2>/dev/null || true
fi

echo ""
echo "==> MTSSH installed successfully!"
echo "    Binary   : ${INSTALL_DIR}/${BINARY}"
echo "    Run      : mtssh"
echo "    Uninstall: bash install/install-linux.sh --uninstall"

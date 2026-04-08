#!/usr/bin/env bash
# install-linux.sh — Installs MTPuTTY on Debian/Ubuntu, Fedora/RHEL, or Arch/CachyOS
# Usage: bash install/install-linux.sh [--uninstall]
set -e

BINARY="mtputty"
INSTALL_DIR="/usr/local/bin"
DESKTOP_DIR="/usr/share/applications"

# ── Uninstall ─────────────────────────────────────────────────────────────────
if [[ "$1" == "--uninstall" ]]; then
    echo "==> Uninstalling MTPuTTY…"
    sudo rm -f "${INSTALL_DIR}/${BINARY}"
    sudo rm -f "${DESKTOP_DIR}/mtputty.desktop"
    echo "==> Done. Config files remain at ~/.mtputty/"
    exit 0
fi

echo "==> MTPuTTY Linux Installer"
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
            sudo apt-get install -y gcc libgl1-mesa-dev xorg-dev golang-go
            ;;
        fedora|rhel|centos|rocky|alma)
            echo "--> Installing dependencies (dnf)…"
            sudo dnf install -y gcc mesa-libGL-devel libX11-devel \
                libXrandr-devel libXcursor-devel libXinerama-devel libXi-devel golang
            ;;
        arch|cachyos|manjaro|endeavouros|garuda)
            echo "--> Installing dependencies (pacman)…"
            sudo pacman -Sy --needed --noconfirm gcc mesa libxrandr libxcursor \
                libxinerama libxi go
            ;;
        opensuse*|sles)
            echo "--> Installing dependencies (zypper)…"
            sudo zypper install -y gcc Mesa-libGL-devel libX11-devel go
            ;;
        *)
            echo "WARNING: Unknown distro '${DISTRO}'. Trying to continue without installing deps."
            echo "If the build fails, install: gcc, libGL-dev, libX11-dev, go (>=1.21)"
            ;;
    esac
}

# ── Check Go ──────────────────────────────────────────────────────────────────
if ! command -v go &>/dev/null; then
    echo "--> Go not found. Installing dependencies…"
    install_deps
else
    GO_VERSION=$(go version | grep -oP '\d+\.\d+' | head -1)
    REQUIRED="1.21"
    if awk "BEGIN{exit !($GO_VERSION < $REQUIRED)}"; then
        echo "WARNING: Go ${GO_VERSION} found, but >= ${REQUIRED} required."
        echo "         Consider upgrading Go: https://go.dev/dl/"
    else
        echo "--> Go ${GO_VERSION} found."
        # Still install non-Go deps
        install_deps
    fi
fi

# ── Build ─────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

echo "--> Building MTPuTTY from ${REPO_DIR}…"
cd "$REPO_DIR"
go mod tidy
go build -ldflags "-s -w" -o "${BINARY}" .

echo "--> Build successful."

# ── Install ───────────────────────────────────────────────────────────────────
echo "--> Installing binary to ${INSTALL_DIR}/${BINARY}…"
sudo install -Dm755 "${BINARY}" "${INSTALL_DIR}/${BINARY}"

echo "--> Installing .desktop file…"
sudo install -Dm644 install/mtputty.desktop "${DESKTOP_DIR}/mtputty.desktop"

# Update desktop database if available
if command -v update-desktop-database &>/dev/null; then
    sudo update-desktop-database "${DESKTOP_DIR}" 2>/dev/null || true
fi

echo ""
echo "==> MTPuTTY installed successfully!"
echo "    Binary   : ${INSTALL_DIR}/${BINARY}"
echo "    Run      : mtputty"
echo "    Uninstall: bash install/install-linux.sh --uninstall"

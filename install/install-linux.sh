#!/usr/bin/env bash
# install-linux.sh — Installs MTSSH on Debian/Ubuntu, Fedora/RHEL, or Arch/CachyOS
# Usage: bash install/install-linux.sh [--uninstall]
set -e

BINARY="mtssh"
INSTALL_DIR="/usr/local/bin"
DESKTOP_DIR="/usr/share/applications"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
# Derive version from git tag, fall back to Makefile, then "1.0.0"
VERSION="$(git -C "$REPO_DIR" describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' \
    || grep '^VERSION' "$REPO_DIR/Makefile" 2>/dev/null | head -1 | sed 's/.*:=\s*//' \
    || echo "1.0.0")"

# ── Uninstall ─────────────────────────────────────────────────────────────────
if [[ "$1" == "--uninstall" ]]; then
    echo "==> Uninstalling MTSSH…"
    sudo rm -f "${INSTALL_DIR}/${BINARY}"
    sudo rm -f "${DESKTOP_DIR}/mtssh.desktop"
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
echo "--> Building MTSSH ${VERSION} from ${REPO_DIR}…"
cd "$REPO_DIR"
go mod tidy
go build -ldflags "-s -w -X main.Version=${VERSION}" -o "${BINARY}" .

echo "--> Build successful."

# ── Install ───────────────────────────────────────────────────────────────────
echo "--> Installing binary to ${INSTALL_DIR}/${BINARY}…"
sudo install -Dm755 "${BINARY}" "${INSTALL_DIR}/${BINARY}"

echo "--> Installing .desktop file…"
sudo install -Dm644 install/mtssh.desktop "${DESKTOP_DIR}/mtssh.desktop"

# Update desktop database if available
if command -v update-desktop-database &>/dev/null; then
    sudo update-desktop-database "${DESKTOP_DIR}" 2>/dev/null || true
fi

echo ""
echo "==> MTSSH installed successfully!"
echo "    Binary   : ${INSTALL_DIR}/${BINARY}"
echo "    Run      : mtssh"
echo "    Uninstall: bash install/install-linux.sh --uninstall"

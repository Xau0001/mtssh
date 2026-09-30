BINARY   := mtssh
# Latest git tag without "v"; override with `make VERSION=x.y.z`
VERSION  ?= $(patsubst v%,%,$(shell git describe --tags --abbrev=0 2>/dev/null))
ifeq ($(VERSION),)
VERSION  := 1.0.0
endif
# Public release key (UPDATE_PUBLIC_KEY, see tools/signsums), e.g.
# `make install UPDATE_PUBLIC_KEY=…`: MTSSH then announces only releases
# signed with it. Empty when building from source. Exported for the
# package scripts (make deb/rpm).
UPDATE_PUBLIC_KEY ?=
export UPDATE_PUBLIC_KEY
# A malformed key would build fine and silently check nothing.
ifneq ($(UPDATE_PUBLIC_KEY),)
ifeq ($(shell echo '$(UPDATE_PUBLIC_KEY)' | grep -Ex '[A-Za-z0-9+/]{43}='),)
$(error UPDATE_PUBLIC_KEY is not a base64 Ed25519 public key (see tools/signsums -genkey))
endif
endif
GOLDFLAGS := -X main.Version=$(VERSION) -X mtssh/core.UpdatePublicKey=$(UPDATE_PUBLIC_KEY) -s -w

# ── Build ──────────────────────────────────────────────────────────────────────

.PHONY: all build build-windows deps tidy test install uninstall deb rpm clean

all: build

deps:
	go mod download

# Rewrites go.mod/go.sum — run on purpose, not on every build.
tidy:
	go mod tidy

build:
	go build -trimpath -ldflags "$(GOLDFLAGS)" -o $(BINARY) .

test:
	go test ./...

build-windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 \
	CC=x86_64-w64-mingw32-gcc \
	go build -trimpath -ldflags "$(GOLDFLAGS)" -o $(BINARY).exe .

# ── Install (Linux) ────────────────────────────────────────────────────────────

PREFIX ?= /usr/local

# Installed as root: updated by reinstalling, not by the in-app updater.
# Builds its own binary: a `build` that already ran (make build install)
# would not pick up the extra flag.
# install and uninstall refuse to run over a .deb, .rpm or pacman package:
# the .desktop file and icon below belong to it (the check of
# install/install-linux.sh).
install:
	@bash install/install-linux.sh --check-packaged
	go build -trimpath -ldflags "$(GOLDFLAGS) -X mtssh/core.Packaged=true" -o $(BINARY) .
	install -Dm755 $(BINARY) $(PREFIX)/bin/$(BINARY)
	install -Dm644 install/mtssh.desktop /usr/share/applications/mtssh.desktop
	install -Dm644 icon.png /usr/share/icons/hicolor/512x512/apps/mtssh.png
	@echo "Installed to $(PREFIX)/bin/$(BINARY)"

uninstall:
	@bash install/install-linux.sh --check-packaged
	rm -f $(PREFIX)/bin/$(BINARY)
	rm -f /usr/share/applications/mtssh.desktop
	rm -f /usr/share/icons/hicolor/512x512/apps/mtssh.png

# ── Packaging (the scripts build the binary themselves) ─────────────────────

deb:
	bash install/build-deb.sh $(VERSION)

rpm:
	bash install/build-rpm.sh $(VERSION)

# ── Cleanup ───────────────────────────────────────────────────────────────────

clean:
	rm -f $(BINARY) $(BINARY).exe
	rm -rf dist/

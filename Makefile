BINARY   := mtputty
VERSION  := 1.0.0
PKG      := mtputty
LDFLAGS  := -ldflags "-X main.Version=$(VERSION) -s -w"

# ── Build ──────────────────────────────────────────────────────────────────────

.PHONY: all build build-windows build-linux deps clean

all: deps build

deps:
	go mod tidy

build:
	go build $(LDFLAGS) -o $(BINARY) .

build-windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 \
	CC=x86_64-w64-mingw32-gcc \
	go build $(LDFLAGS) -o $(BINARY).exe .

# ── Install (Linux) ────────────────────────────────────────────────────────────

PREFIX ?= /usr/local

install: build
	install -Dm755 $(BINARY) $(PREFIX)/bin/$(BINARY)
	install -Dm644 install/mtputty.desktop /usr/share/applications/mtputty.desktop
	@echo "Installed to $(PREFIX)/bin/$(BINARY)"

uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)
	rm -f /usr/share/applications/mtputty.desktop

# ── Packaging ─────────────────────────────────────────────────────────────────

deb: build
	bash install/build-deb.sh $(VERSION)

rpm: build
	bash install/build-rpm.sh $(VERSION)

# ── Cleanup ───────────────────────────────────────────────────────────────────

clean:
	rm -f $(BINARY) $(BINARY).exe
	rm -rf dist/

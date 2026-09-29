BINARY   := mtssh
# Latest git tag without "v"; override with `make VERSION=x.y.z`
VERSION  ?= $(patsubst v%,%,$(shell git describe --tags --abbrev=0 2>/dev/null))
ifeq ($(VERSION),)
VERSION  := 1.0.0
endif
LDFLAGS  := -ldflags "-X main.Version=$(VERSION) -s -w"

# ── Build ──────────────────────────────────────────────────────────────────────

.PHONY: all build build-windows deps test install uninstall deb rpm clean

all: deps build

deps:
	go mod tidy

build:
	go build $(LDFLAGS) -o $(BINARY) .

test:
	go test ./...

build-windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 \
	CC=x86_64-w64-mingw32-gcc \
	go build $(LDFLAGS) -o $(BINARY).exe .

# ── Install (Linux) ────────────────────────────────────────────────────────────

PREFIX ?= /usr/local

install: build
	install -Dm755 $(BINARY) $(PREFIX)/bin/$(BINARY)
	install -Dm644 install/mtssh.desktop /usr/share/applications/mtssh.desktop
	install -Dm644 icon.png /usr/share/icons/hicolor/512x512/apps/mtssh.png
	@echo "Installed to $(PREFIX)/bin/$(BINARY)"

uninstall:
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

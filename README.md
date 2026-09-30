# MTSSH — Multi-Tabbed SSH Client (Go)

Ein vollständiges, plattformübergreifendes SSH-Tool wie MTPutty, geschrieben in Go mit Fyne GUI.

## Features

| Feature | Details |
|---|---|
| **Multi-Tab Interface** | Beliebig viele SSH-Sessions als Tabs |
| **Vollwertiges Terminal** | VT100/xterm-Emulation: vim, htop, less, Farben, Tastenkürzel; Größe passt sich dem Fenster an |
| **Mehrfenstermodus** | Sessions in eigene unabhängige Fenster auslagern |
| **SFTP-Dateimanager** | Pro Session als eigener Tab: Upload, Download, Rename, Delete, Mkdir |
| **Known-Hosts-Validierung** | Accept/Reject-Dialog bei unbekannten Hosts, MITM-Schutz |
| **AES-256-GCM Verschlüsselung** | Alle Sessions inkl. Passwörter verschlüsselt gespeichert, Schlüssel per Argon2id aus der Master-Passphrase abgeleitet |
| **SSH Key Auth** | RSA / ED25519 Private Keys |
| **Password Auth** | Passwort und keyboard-interactive (z. B. PAM, Einmalcodes); ohne gespeichertes Passwort wird nachgefragt |
| **Auto-Connect** | Sessions verbinden automatisch beim Start |
| **Auto-Reconnect** | 3 Versuche mit je 3s Pause nach Verbindungsabbruch (nur bei Auto-Connect-Sessions; nicht nach `exit`, höchstens 5-mal in 10 Minuten, nicht bei Anmeldefehlern) |
| **Keepalive** | Erkennt tote Verbindungen (alle 30s, Abbruch nach 3 fehlenden Antworten) |
| **Themes** | Dark, Light, Solarized, Nord — zur Laufzeit umschaltbar, Auswahl wird gespeichert |
| **Logging** | Alle Events unter `~/.mtssh/logs/`, Logs älter als 30 Tage werden gelöscht |
| **Gruppen** | Sessions nach Gruppe kategorisieren |
| **Export / Import** | Sessions als JSON sichern; Passwörter nur auf ausdrücklichen Wunsch |
| **Auto-Update** | Prüft beim Start auf neue Releases; das Linux-Binary aktualisiert sich selbst, geprüft über eine Ed25519-signierte `SHA256SUMS`. Paket-Installationen (.deb, .rpm, AUR, Installer) werden über den Paketmanager aktualisiert |

## Projektstruktur

```
mtssh/
├── main.go                    # Einstieg, Passphrase-Unlock-Dialog, Update-Check
├── go.mod
├── config/
│   └── config.go              # Verschlüsselter Session-Store (Argon2id + AES-256-GCM)
├── core/
│   ├── ssh.go                 # SSH-Client (Key/Password Auth, Auto-Reconnect)
│   ├── known_hosts.go         # Known-Hosts-Validierung + Accept/Reject
│   ├── sftp.go                # SFTP-Client (Upload/Download/Rename/Delete/Mkdir)
│   └── updater.go             # GitHub-Release-Check + signaturgeprüftes Self-Update
├── logger/
│   └── logger.go              # File + Console Logging
├── third_party/fyne-terminal/ # Gepatchte Terminal-Bibliothek (siehe PATCHES.md)
├── tools/signsums/            # Signiert SHA256SUMS für den Updater
├── ui/
│   ├── main_window.go         # Haupt-GUI: Sidebar, Tabs, Theme-Wahl, Mehrfenster
│   ├── draggable_tabs.go      # Tab-Leiste mit Drag & Drop
│   ├── term_tab.go            # Terminal-Tab (fyne-io/terminal) mit SFTP- und New-Window-Button
│   ├── list_row.go            # Listeneintrag: Klick wählt aus, Doppelklick öffnet
│   ├── sftp_tab.go            # SFTP-Dateimanager Tab
│   ├── session_dialog.go      # Session anlegen/bearbeiten
│   ├── export_import.go       # Sessions als JSON exportieren/importieren
│   ├── known_hosts_editor.go  # Known-Hosts-Verwaltung
│   └── theme.go               # Dark / Light / Solarized / Nord Themes
└── install/                   # Installer (Linux/Windows) + .deb/.rpm/PKGBUILD
```

## Voraussetzungen

### Linux (Debian/Ubuntu)
```bash
sudo apt install gcc libgl1-mesa-dev xorg-dev libwayland-dev libxkbcommon-dev
```

### Linux (Fedora)
```bash
sudo dnf install gcc mesa-libGL-devel libX11-devel libXrandr-devel libXcursor-devel libXinerama-devel libXi-devel wayland-devel libxkbcommon-devel
```

### Linux (Arch)
```bash
sudo pacman -S gcc mesa libxrandr libxcursor libxinerama libxi wayland libxkbcommon
```

### Windows
MinGW-w64: https://www.mingw-w64.org/

### Go
https://go.dev/dl/ — mindestens Go 1.21; gebaut wird mit der in `go.mod` festgelegten Toolchain (Go 1.27.1), die automatisch heruntergeladen wird

## Build & Start

```bash
cd mtssh
go run .                          # direkt starten
go test ./...                     # Tests

go build -o mtssh .             # Linux Binary
go build -o mtssh.exe .         # Windows Binary (nativ)

# Windows cross-compile von Linux:
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 \
  CC=x86_64-w64-mingw32-gcc \
  go build -o mtssh.exe .
```

## Neue Features im Detail

### Known-Hosts-Validierung
- Erster Verbindungsversuch zu einem Host → Fingerprint-Dialog erscheint
- **Accept** → Key wird in `~/.mtssh/known_hosts` gespeichert
- **Reject** → Verbindung wird abgebrochen
- Geänderter Host-Key → Fehlermeldung (MITM-Schutz, keine stille Übernahme)
- Host-Zertifikate und `@cert-authority`-Zeilen werden nicht ausgewertet; solche Hosts werden wie unbekannte Hosts per Fingerprint bestätigt

### SFTP-Dateimanager
- Im Terminal-Tab auf **SFTP** klicken → neuer SFTP-Tab öffnet sich
- Doppelklick auf Ordner → Navigation; Doppelklick auf Datei → Optionen
- Dateioptionen: **Download**, **Rename**, **Delete**
- Toolbar: **Upload**, **New Folder**, **Refresh**, **Up**, **Cancel Transfer**
- Download: Zielordner wählen, Dateinamen bestätigen; vor dem Überschreiben einer lokalen Datei wird nachgefragt (bei einem Symlink wird die Datei ersetzt, auf die er zeigt)
- Übertragungen landen erst in einer temporären Datei und ersetzen das Ziel erst, wenn sie vollständig sind; vor dem Überschreiben einer Datei auf dem Server wird nachgefragt. Es läuft immer nur eine Übertragung pro SFTP-Tab

### Themes
- Theme-Dropdown in der linken Sidebar → sofortiger Wechsel ohne Neustart; die Auswahl bleibt nach einem Neustart erhalten
- **Dark** (VSCode-Dunkelgrau), **Light** (hell), **Solarized** (Teal-Dark), **Nord** (Blaugrau)

### Sessions & Terminal
- Klick auf eine Session wählt sie aus (für Edit / Delete / New Window), **Doppelklick verbindet**
- Tastatureingaben gehen direkt an den Server; Kopieren/Einfügen mit **Strg+Shift+C / Strg+Shift+V**
- Beim ersten Start wird eine Master-Passphrase mit mindestens 8 Zeichen festgelegt
- MTSSH läuft pro Benutzer nur einmal — zwei Instanzen würden sich sonst gegenseitig gespeicherte Sessions überschreiben. Weitere Fenster über **New Window**

### Mehrfenstermodus
- **New Window**-Button in der Terminal-Toolbar → Session öffnet sich in eigenem Fenster
- Oder: Session in der Sidebar auswählen → **New Window**-Button
- Jedes Fenster ist vollständig unabhängig inkl. eigenem SFTP-Manager

## Datei-Speicherorte

| Datei | Pfad |
|---|---|
| Sessions (verschlüsselt) | `~/.mtssh/sessions.enc` |
| Known Hosts | `~/.mtssh/known_hosts` |
| Logs | `~/.mtssh/logs/mtssh_YYYY-MM-DD.log` |
| Einstellungen (Theme) | Linux: `~/.config/fyne/onl.xau.mtssh/preferences.json` |

## Releases signieren

Das Linux-Binary installiert Updates nur, wenn `SHA256SUMS.sig` mit dem Release-Schlüssel für genau diese Version signiert ist:

1. Schlüsselpaar erzeugen: `go run ./tools/signsums -genkey`
2. In den Repository-Einstellungen eine Environment `release` anlegen (am besten mit Required Reviewers) und dort das Secret `UPDATE_SIGNING_KEY` setzen
3. Den öffentlichen Schlüssel als Repository-Variable `UPDATE_PUBLIC_KEY` hinterlegen; der Release-Workflow baut ihn ins Binary ein

Signiert wird die Version zusammen mit den Prüfsummen, nicht `SHA256SUMS` allein:

```
mtssh-release <Version>\n<Inhalt von SHA256SUMS, byte-genau>
```

`<Version>` ist der Tag ohne führendes `v` (Tag `v1.2.0-rc1` → `1.2.0-rc1`, nicht die Paketform `1.2.0~rc1`); erlaubt sind Buchstaben, Ziffern, `.`, `+` und `-`. Der Updater prüft die Signatur mit der Version des Releases, das er installieren soll. Die signierten Dateien eines alten Releases lassen sich so nicht unter einem neueren Tag erneut veröffentlichen (Downgrade auf eine verwundbare Version). `SHA256SUMS` selbst bleibt eine normale `sha256sum`-Datei.

Von Hand signieren und prüfen:

```bash
# schreibt SHA256SUMS.sig
UPDATE_SIGNING_KEY=… go run ./tools/signsums -version 1.2.0 SHA256SUMS
# Exit-Code 1, wenn SHA256SUMS.sig nicht zu Version und Schlüssel passt
go run ./tools/signsums -verify -version 1.2.0 -pubkey "<UPDATE_PUBLIC_KEY>" SHA256SUMS
```

Der Release-Workflow signiert mit der Version aus dem Tag und prüft die Signatur danach mit `-verify` gegen `UPDATE_PUBLIC_KEY`.

Fehlt `UPDATE_PUBLIC_KEY` oder ist er ungültig, fehlt `UPDATE_SIGNING_KEY` oder passt die Signatur nicht zum öffentlichen Schlüssel, bricht der Release-Workflow mit einem Fehler ab: ein Linux-Binary ohne Schlüssel könnte sich nie wieder selbst aktualisieren. Wer bewusst ohne Signatur veröffentlichen will, setzt die Repository-Variable `ALLOW_UNSIGNED_RELEASE` auf `true`. Fehlende Schlüssel ergeben dann nur Warnungen; für ein solches Release verweist MTSSH nur auf die Release-Seite.

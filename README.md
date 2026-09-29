# MTSSH — Multi-Tabbed SSH Client (Go)

Ein vollständiges, plattformübergreifendes SSH-Tool wie MTPutty, geschrieben in Go mit Fyne GUI.

## Features

| Feature | Details |
|---|---|
| **Multi-Tab Interface** | Beliebig viele SSH-Sessions als Tabs |
| **Mehrfenstermodus** | Sessions in eigene unabhängige Fenster auslagern |
| **SFTP-Dateimanager** | Pro Session als eigener Tab: Upload, Download, Rename, Delete, Mkdir |
| **Known-Hosts-Validierung** | Accept/Reject-Dialog bei unbekannten Hosts, MITM-Schutz |
| **AES-256-GCM Verschlüsselung** | Alle Sessions inkl. Passwörter verschlüsselt gespeichert, Schlüssel per Argon2id aus der Master-Passphrase abgeleitet |
| **SSH Key Auth** | RSA / ED25519 Private Keys |
| **Password Auth** | Klassische Passwort-Authentifizierung |
| **Auto-Connect** | Sessions verbinden automatisch beim Start |
| **Auto-Reconnect** | 3 Versuche mit je 3s Pause nach Verbindungsabbruch (nur bei Auto-Connect-Sessions) |
| **Themes** | Dark, Light, Solarized, Nord — zur Laufzeit umschaltbar |
| **Logging** | Alle Events unter `~/.mtssh/logs/` |
| **Gruppen** | Sessions nach Gruppe kategorisieren |
| **Export / Import** | Sessions als JSON sichern; Passwörter nur auf ausdrücklichen Wunsch |
| **Auto-Update** | Prüft beim Start auf neue Releases; Linux-Binary wird per SHA-256 verifiziert |

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
│   └── updater.go             # GitHub-Release-Check + verifiziertes Self-Update
├── logger/
│   └── logger.go              # File + Console Logging
├── ui/
│   ├── main_window.go         # Haupt-GUI: Sidebar, Tabs, Theme-Wahl, Mehrfenster
│   ├── draggable_tabs.go      # Tab-Leiste mit Drag & Drop
│   ├── term_tab.go            # Terminal-Tab mit SFTP- und New-Window-Button
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
sudo apt install gcc libgl1-mesa-dev xorg-dev
```

### Linux (Arch)
```bash
sudo pacman -S gcc mesa libxrandr libxcursor libxinerama libxi
```

### Windows
MinGW-w64: https://www.mingw-w64.org/

### Go
https://go.dev/dl/ — mindestens Go 1.21

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

### SFTP-Dateimanager
- Im Terminal-Tab auf **SFTP** klicken → neuer SFTP-Tab öffnet sich
- Klick auf Ordner → Navigation; Klick auf Datei → Optionen
- Dateioptionen: **Download**, **Rename**, **Delete**
- Toolbar: **Upload**, **New Folder**, **Refresh**, **Up**

### Themes
- Theme-Dropdown in der linken Sidebar → sofortiger Wechsel ohne Neustart
- **Dark** (VSCode-Dunkelgrau), **Light** (hell), **Solarized** (Teal-Dark), **Nord** (Blaugrau)

### Mehrfenstermodus
- **New Window**-Button in der Terminal-Toolbar → Session öffnet sich in eigenem Fenster
- Oder: Session in der Sidebar markieren → **New Window**-Button
- Jedes Fenster ist vollständig unabhängig inkl. eigenem SFTP-Manager

## Datei-Speicherorte

| Datei | Pfad |
|---|---|
| Sessions (verschlüsselt) | `~/.mtssh/sessions.enc` |
| Known Hosts | `~/.mtssh/known_hosts` |
| Logs | `~/.mtssh/logs/mtssh_YYYY-MM-DD.log` |

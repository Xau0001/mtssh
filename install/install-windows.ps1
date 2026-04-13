# MTSSH Windows Installer
# Run in PowerShell:
#   Set-ExecutionPolicy RemoteSigned -Scope CurrentUser
#   .\install\install-windows.ps1

param(
    [string]$InstallDir = "$env:LOCALAPPDATA\MTSSH",
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"

$Binary   = "mtssh.exe"
$AppName  = "MTSSH"
$RepoRoot = Split-Path -Parent $PSScriptRoot

# Derive version from git tag, fall back to 1.0.0
$Version = (git -C $RepoRoot describe --tags --abbrev=0 2>$null) -replace "^v", ""
if (-not $Version) { $Version = "1.0.0" }

# ── Uninstall ─────────────────────────────────────────────────────────────────
if ($Uninstall) {
    Write-Host "Uninstalling $AppName..." -ForegroundColor Yellow

    # Remove from PATH
    $userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
    $newPath = ($userPath -split ";" | Where-Object { $_ -ne $InstallDir }) -join ";"
    [Environment]::SetEnvironmentVariable("PATH", $newPath, "User")

    # Remove shortcut
    $shortcut = "$env:APPDATA\Microsoft\Windows\Start Menu\Programs\$AppName.lnk"
    if (Test-Path $shortcut) { Remove-Item $shortcut -Force }

    # Remove install dir
    if (Test-Path $InstallDir) { Remove-Item $InstallDir -Recurse -Force }

    Write-Host "Uninstalled $AppName." -ForegroundColor Green
    exit 0
}

# ── Check prerequisites ────────────────────────────────────────────────────────
Write-Host "==> Checking prerequisites..." -ForegroundColor Cyan

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host "ERROR: Go is not installed or not in PATH." -ForegroundColor Red
    Write-Host "Download Go from: https://go.dev/dl/"
    exit 1
}
$goVersion = (go version) -replace "go version go", "" -replace " .*", ""
Write-Host "Found Go $goVersion" -ForegroundColor Green

if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) {
    Write-Host "ERROR: gcc not found. MTSSH requires CGO (Fyne uses OpenGL)." -ForegroundColor Red
    Write-Host "Install MSYS2 with MinGW64: https://www.msys2.org/"
    Write-Host "Then add C:\msys64\mingw64\bin to your PATH and re-run."
    exit 1
}
Write-Host "Found gcc" -ForegroundColor Green

# ── Build ─────────────────────────────────────────────────────────────────────
Write-Host "==> Building $AppName..." -ForegroundColor Cyan
Push-Location $RepoRoot

$env:CGO_ENABLED = "1"
go mod tidy
go build -ldflags "-s -w -H windowsgui -X main.Version=$Version" -o $Binary .

if (-not (Test-Path $Binary)) {
    Write-Host "ERROR: Build failed. See output above." -ForegroundColor Red
    Pop-Location
    exit 1
}
Write-Host "Build successful." -ForegroundColor Green

# ── Install ───────────────────────────────────────────────────────────────────
Write-Host "==> Installing to $InstallDir..." -ForegroundColor Cyan

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item $Binary "$InstallDir\$Binary" -Force

# Add to user PATH if not already there
$userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if ($userPath -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable("PATH", "$userPath;$InstallDir", "User")
    Write-Host "Added $InstallDir to user PATH." -ForegroundColor Green
} else {
    Write-Host "$InstallDir already in PATH." -ForegroundColor Gray
}

# ── Start Menu shortcut ───────────────────────────────────────────────────────
$startMenuPath = "$env:APPDATA\Microsoft\Windows\Start Menu\Programs"
$shortcutPath  = "$startMenuPath\$AppName.lnk"

$shell    = New-Object -ComObject WScript.Shell
$shortcut = $shell.CreateShortcut($shortcutPath)
$shortcut.TargetPath       = "$InstallDir\$Binary"
$shortcut.WorkingDirectory = $InstallDir
$shortcut.Description      = "Multi-Tabbed SSH Client"
$shortcut.Save()

Write-Host "Start Menu shortcut created." -ForegroundColor Green

# ── Desktop shortcut (optional) ───────────────────────────────────────────────
$desktopPath = [Environment]::GetFolderPath("Desktop")
$desktopShortcut = $shell.CreateShortcut("$desktopPath\$AppName.lnk")
$desktopShortcut.TargetPath       = "$InstallDir\$Binary"
$desktopShortcut.WorkingDirectory = $InstallDir
$desktopShortcut.Description      = "Multi-Tabbed SSH Client"
$desktopShortcut.Save()

Write-Host "Desktop shortcut created." -ForegroundColor Green

Pop-Location

Write-Host ""
Write-Host "==> $AppName installed successfully!" -ForegroundColor Green
Write-Host "    Location : $InstallDir\$Binary"
Write-Host "    Run      : mtssh   (after opening a new terminal)"
Write-Host "    Uninstall: .\install\install-windows.ps1 -Uninstall"

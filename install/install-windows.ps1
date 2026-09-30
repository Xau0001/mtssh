# MTSSH Windows Installer
# Run in PowerShell (the policy change applies to this run only):
#   powershell -ExecutionPolicy Bypass -File .\install\install-windows.ps1

param(
    [string]$InstallDir = "$env:LOCALAPPDATA\MTSSH",
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"

$Binary   = "mtssh.exe"
$AppName  = "MTSSH"
$RepoRoot = Split-Path -Parent $PSScriptRoot

# An absolute path: it goes into PATH and into the shortcuts, and uninstall
# compares it with them.
$InstallDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($InstallDir)

# True if the shortcut at $Path starts $Exe. A shortcut of the same name
# that starts something else was not made by this installer: keep it.
function Test-OwnShortcut([string]$Path, [string]$Exe) {
    if (-not (Test-Path -LiteralPath $Path)) { return $false }
    try {
        $target = (New-Object -ComObject WScript.Shell).CreateShortcut($Path).TargetPath
        if (-not $target) { return $false }
        return [System.IO.Path]::GetFullPath($target) -eq [System.IO.Path]::GetFullPath($Exe)
    } catch {
        return $false
    }
}

# ── Uninstall ─────────────────────────────────────────────────────────────────
# Removes only what the installer created: mtssh.exe, its shortcuts and the
# PATH entry. Install also copies into a folder that already exists, so
# -InstallDir may hold other files: the folder goes only if it is empty.
if ($Uninstall) {
    Write-Host "Uninstalling $AppName..." -ForegroundColor Yellow
    $exePath = Join-Path $InstallDir $Binary

    # The shortcuts the installer made, if they start this installation's
    # mtssh.exe (read while it still exists)
    $shortcuts = @("$env:APPDATA\Microsoft\Windows\Start Menu\Programs\$AppName.lnk")
    $desktopPath = [Environment]::GetFolderPath("Desktop")
    if ($desktopPath) { $shortcuts += "$desktopPath\$AppName.lnk" }
    $ownShortcuts = @($shortcuts | Where-Object { Test-OwnShortcut $_ $exePath })

    # mtssh.exe first: if MTSSH is running this fails, and nothing else is
    # removed
    if (Test-Path -LiteralPath $exePath) { Remove-Item -LiteralPath $exePath -Force }
    foreach ($lnk in $ownShortcuts) { Remove-Item -LiteralPath $lnk -Force }

    # The folder, if nothing else is in it (hidden files included)
    if (Test-Path -LiteralPath $InstallDir) {
        if (Get-ChildItem -LiteralPath $InstallDir -Force | Select-Object -First 1) {
            Write-Host "Kept ${InstallDir}: it contains files the installer did not create." -ForegroundColor Gray
        } else {
            Remove-Item -LiteralPath $InstallDir -Force
        }
    }

    # The PATH entry only together with the folder: a folder that stays may
    # be on PATH for its other files.
    if (-not (Test-Path -LiteralPath $InstallDir)) {
        $userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
        if ($userPath) {
            $newPath = ($userPath -split ";" | Where-Object { $_.TrimEnd("\") -ne $InstallDir.TrimEnd("\") }) -join ";"
            if ($newPath -ne $userPath) {
                [Environment]::SetEnvironmentVariable("PATH", $newPath, "User")
            }
        }
    }

    Write-Host "Uninstalled $AppName." -ForegroundColor Green
    exit 0
}

# Derive version from git tag, fall back to 1.0.0: git may be missing, and a
# ZIP download or a clone without tags has no tag. Windows PowerShell 5.1
# turns redirected stderr of a native command into a terminating error
# under $ErrorActionPreference = "Stop", so relax it for this one call and
# check the exit code instead.
$Version = ""
if (Get-Command git -ErrorAction SilentlyContinue) {
    $savedPreference = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        $tag = git -C $RepoRoot describe --tags --abbrev=0 2>$null
        if ($LASTEXITCODE -eq 0 -and $tag) { $Version = "$tag".Trim() -replace "^v", "" }
    } catch {
        # keep the default
    } finally {
        $ErrorActionPreference = $savedPreference
    }
}
if (-not $Version) { $Version = "1.0.0" }

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
# Remove an old build so a failed build cannot be mistaken for success
if (Test-Path $Binary) { Remove-Item $Binary -Force }
go build -ldflags "-s -w -H windowsgui -X main.Version=$Version" -o $Binary .

# $ErrorActionPreference does not apply to native commands — check the exit code
if ($LASTEXITCODE -ne 0 -or -not (Test-Path $Binary)) {
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
$pathEntries = @($userPath -split ";" | ForEach-Object { $_.TrimEnd("\") })
if ($pathEntries -notcontains $InstallDir.TrimEnd("\")) {
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

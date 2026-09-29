package main

import (
	_ "embed"
	"errors"
	"fmt"
	"mtssh/config"
	"mtssh/core"
	"mtssh/logger"
	"mtssh/ui"
	"net/url"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

//go:embed icon.png
var iconData []byte

// Version is set at build time via -ldflags "-X main.Version=x.y.z"
var Version = "dev"

// appID must match FyneApp.toml; Fyne stores preferences (e.g. the theme) under it.
const appID = "onl.xau.mtssh"

// minPassphraseLen applies to new session stores; existing ones still open
// with the passphrase they were created with.
const minPassphraseLen = 8

func main() {
	_ = logger.Init()
	defer logger.Close()

	a := app.NewWithID(appID)
	a.SetIcon(fyne.NewStaticResource("icon.png", iconData))
	a.Settings().SetTheme(ui.NewTheme(ui.SavedTheme(a)))

	if err := config.Lock(); errors.Is(err, config.ErrAlreadyRunning) {
		showAlreadyRunning(a)
		return
	} else if err != nil {
		// e.g. a file system without locking support: carry on unprotected
		logger.Error("app", "session store lock: "+err.Error())
	}

	unlockWin := a.NewWindow("MTSSH — Unlock")
	unlockWin.Resize(fyne.NewSize(480, 220))

	// On first launch the passphrase is typed twice: a typo would otherwise
	// lock the user out of everything they save afterwards.
	firstRun := !config.Exists()

	passEntry := widget.NewPasswordEntry()
	passEntry.SetPlaceHolder("Enter master passphrase")
	confirmEntry := widget.NewPasswordEntry()
	confirmEntry.SetPlaceHolder("Repeat master passphrase")

	unlocked := false
	unlock := func() {
		if unlocked {
			return // e.g. Enter pressed twice: only one main window
		}
		pass := passEntry.Text
		if pass == "" {
			dialog.ShowError(fmt.Errorf("passphrase must not be empty"), unlockWin)
			return
		}
		if firstRun && utf8.RuneCountInString(pass) < minPassphraseLen {
			dialog.ShowError(fmt.Errorf("use at least %d characters", minPassphraseLen), unlockWin)
			return
		}
		if firstRun && confirmEntry.Text != pass {
			dialog.ShowError(fmt.Errorf("passphrases do not match"), unlockWin)
			return
		}
		sessions, err := config.Load(pass)
		if err != nil {
			dialog.ShowError(err, unlockWin)
			return
		}
		unlocked = true
		logger.Info("app", "session store unlocked")
		// Don't keep the master passphrase around in the hidden window
		passEntry.SetText("")
		confirmEntry.SetText("")
		unlockWin.Hide()

		mainWin := ui.MainWindow(a, sessions, func(updated []config.Session) error {
			return config.Save(updated)
		})
		mainWin.Show()
		go checkForUpdates(a, mainWin, Version)
	}

	passEntry.OnSubmitted = func(_ string) { unlock() }
	confirmEntry.OnSubmitted = passEntry.OnSubmitted

	content := container.NewVBox(widget.NewLabel("MTSSH — Multi-Tabbed SSH Client"))
	if firstRun {
		content.Add(widget.NewLabel(fmt.Sprintf("First launch: choose a master passphrase (at least %d characters) —\nit encrypts your sessions. It cannot be recovered, so don't forget it.", minPassphraseLen)))
		content.Add(passEntry)
		content.Add(confirmEntry)
		content.Add(widget.NewButton("Create", unlock))
	} else {
		content.Add(widget.NewLabel("Enter your master passphrase to unlock the session store."))
		content.Add(passEntry)
		content.Add(widget.NewButton("Unlock", unlock))
	}
	unlockWin.SetContent(content)
	unlockWin.Canvas().Focus(passEntry)

	unlockWin.ShowAndRun()
}

// showAlreadyRunning explains why this second instance does not start.
func showAlreadyRunning(a fyne.App) {
	w := a.NewWindow("MTSSH")
	w.SetContent(container.NewVBox(
		widget.NewLabel("MTSSH is already running.\nUse \"New Window\" in the running instance to open more windows."),
		widget.NewButton("OK", a.Quit),
	))
	w.ShowAndRun()
}

// checkForUpdates runs in a goroutine; UI work is handed to the UI goroutine.
func checkForUpdates(a fyne.App, win fyne.Window, currentVersion string) {
	if currentVersion == "dev" {
		return
	}
	rel, err := core.LatestRelease()
	if err != nil {
		logger.Error("updater", "update check: "+err.Error())
		return
	}
	if core.IsNewer(currentVersion, rel.Version) {
		fyne.Do(func() { offerUpdate(a, win, currentVersion, rel) })
	}
}

func offerUpdate(a fyne.App, win fyne.Window, currentVersion string, rel core.Release) {
	latest := rel.Version

	// Windows (running binary is locked), packaged installs, platforms
	// without a published binary, releases without signed checksums, and an
	// executable the user cannot replace: open the release page instead.
	if !rel.CanSelfUpdate() || core.CheckSelfUpdate() != nil {
		msg := fmt.Sprintf("Version %s is available (current: %s).\nOpen in browser?", latest, currentVersion)
		if core.Packaged == "true" {
			msg = fmt.Sprintf("Version %s is available (current: %s).\nUpdate MTSSH with your package manager, or open the release page?", latest, currentVersion)
		}
		dialog.ShowConfirm("Update Available", msg, func(ok bool) {
			if !ok {
				return
			}
			// Only open https links, whatever the API response contains
			if u, parseErr := url.Parse(rel.SafePageURL()); parseErr == nil && u.Host != "" {
				a.OpenURL(u)
			}
		}, win)
		return
	}

	// Linux: verified self-update with progress bar.
	msg := fmt.Sprintf("Version %s is available (current: %s).\nUpdate now?", latest, currentVersion)
	dialog.ShowConfirm("Update Available", msg, func(ok bool) {
		if !ok {
			return
		}
		prog := widget.NewProgressBar()
		status := widget.NewLabel("Downloading...")
		dlg := dialog.NewCustom("Updating MTSSH", "Close",
			container.NewVBox(status, prog), win)
		dlg.Show()

		go func() {
			updateErr := core.SelfUpdate(rel, func(p float64) {
				fyne.Do(func() { prog.SetValue(p) })
			})
			if updateErr != nil {
				logger.Error("updater", updateErr.Error())
				fyne.Do(func() { status.SetText("Error: " + updateErr.Error()) })
				return
			}
			logger.Info("updater", "updated to "+latest)
			fyne.Do(func() {
				prog.SetValue(1)
				status.SetText("Done! Please restart MTSSH.")
			})
		}()
	}, win)
}

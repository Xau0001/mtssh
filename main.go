package main

import (
	_ "embed"
	"fmt"
	"mtssh/config"
	"mtssh/core"
	"mtssh/logger"
	"mtssh/ui"
	"net/url"

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

func main() {
	_ = logger.Init()
	defer logger.Close()

	a := app.New()
	a.SetIcon(fyne.NewStaticResource("icon.png", iconData))

	unlockWin := a.NewWindow("MTSSH — Unlock")
	unlockWin.Resize(fyne.NewSize(480, 220))

	// On first launch the passphrase is typed twice: a typo would otherwise
	// lock the user out of everything they save afterwards.
	firstRun := !config.Exists()

	passEntry := widget.NewPasswordEntry()
	passEntry.SetPlaceHolder("Enter master passphrase")
	confirmEntry := widget.NewPasswordEntry()
	confirmEntry.SetPlaceHolder("Repeat master passphrase")

	unlock := func() {
		pass := passEntry.Text
		if pass == "" {
			dialog.ShowError(fmt.Errorf("passphrase must not be empty"), unlockWin)
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
		logger.Info("app", "session store unlocked")
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
		content.Add(widget.NewLabel("First launch: choose a master passphrase — it encrypts your sessions.\nIt cannot be recovered, so don't forget it."))
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

func checkForUpdates(a fyne.App, win fyne.Window, currentVersion string) {
	if currentVersion == "dev" {
		return
	}
	rel, err := core.LatestRelease()
	if err != nil {
		logger.Error("updater", "update check: "+err.Error())
		return
	}
	latest := rel.Version
	if !core.IsNewer(currentVersion, latest) {
		return
	}

	// Windows (running binary is locked), platforms without a published
	// binary, and releases without checksums: open the release page instead.
	if !rel.CanSelfUpdate() {
		msg := fmt.Sprintf("Version %s is available (current: %s).\nOpen in browser?", latest, currentVersion)
		dialog.ShowConfirm("Update Available", msg, func(ok bool) {
			if !ok {
				return
			}
			u, parseErr := url.Parse(rel.PageURL)
			if parseErr == nil {
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
				prog.SetValue(p)
			})
			if updateErr != nil {
				status.SetText("Error: " + updateErr.Error())
				logger.Error("updater", updateErr.Error())
				return
			}
			prog.SetValue(1)
			status.SetText("Done! Please restart MTSSH.")
			logger.Info("updater", "updated to "+latest)
		}()
	}, win)
}

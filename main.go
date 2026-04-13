package main

import (
	_ "embed"
	"fmt"
	"mtssh/config"
	"mtssh/logger"
	"mtssh/ui"

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

	passEntry := widget.NewPasswordEntry()
	passEntry.SetPlaceHolder("Enter master passphrase")

	unlock := func() {
		pass := passEntry.Text
		if pass == "" {
			dialog.ShowError(fmt.Errorf("passphrase must not be empty"), unlockWin)
			return
		}
		config.Init(pass)
		sessions, err := config.Load()
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
	}

	passEntry.OnSubmitted = func(_ string) { unlock() }

	unlockWin.SetContent(container.NewVBox(
		widget.NewLabel("MTSSH — Multi-Tabbed SSH Client"),
		widget.NewLabel("Enter your master passphrase to unlock the session store."),
		widget.NewLabel("First launch: choose any passphrase — it encrypts your sessions."),
		passEntry,
		widget.NewButton("Unlock", unlock),
	))

	unlockWin.ShowAndRun()
}

package ui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mtssh/config"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// ShowSessionDialog opens a dialog to create or edit a session.
// onSave is called with the new/edited session on confirmation.
func ShowSessionDialog(win fyne.Window, existing *config.Session, onSave func(config.Session)) {
	isEdit := existing != nil

	labelEntry := widget.NewEntry()
	labelEntry.SetPlaceHolder("My Server")

	hostEntry := widget.NewEntry()
	hostEntry.SetPlaceHolder("192.168.1.1")

	portEntry := widget.NewEntry()
	portEntry.SetText("22")

	userEntry := widget.NewEntry()
	userEntry.SetPlaceHolder("root")

	passEntry := widget.NewPasswordEntry()
	passEntry.SetPlaceHolder("password (optional)")

	keyEntry := widget.NewEntry()
	keyEntry.SetPlaceHolder("~/.ssh/id_rsa (leave empty for password)")

	useKeyCheck := widget.NewCheck("Use SSH Key", func(b bool) {
		if b {
			passEntry.Disable()
		} else {
			passEntry.Enable()
		}
	})

	groupEntry := widget.NewEntry()
	groupEntry.SetPlaceHolder("Production / Homelab / ...")

	autoCheck := widget.NewCheck("Auto-Connect on start", nil)

	if isEdit {
		labelEntry.SetText(existing.Label)
		hostEntry.SetText(existing.Host)
		portEntry.SetText(strconv.Itoa(existing.Port))
		userEntry.SetText(existing.User)
		passEntry.SetText(existing.Password)
		keyEntry.SetText(existing.KeyPath)
		useKeyCheck.SetChecked(existing.UseKey)
		groupEntry.SetText(existing.Group)
		autoCheck.SetChecked(existing.AutoConnect)
	}

	form := container.NewVBox(
		widget.NewLabel("Label"),
		labelEntry,
		widget.NewLabel("Hostname / IP"),
		hostEntry,
		widget.NewLabel("Port"),
		portEntry,
		widget.NewLabel("Username"),
		userEntry,
		widget.NewSeparator(),
		useKeyCheck,
		widget.NewLabel("SSH Key Path"),
		keyEntry,
		widget.NewLabel("Password"),
		passEntry,
		widget.NewSeparator(),
		widget.NewLabel("Group"),
		groupEntry,
		autoCheck,
	)

	title := "New Session"
	if isEdit {
		title = "Edit Session"
	}

	dialog.ShowCustomConfirm(title, "Save", "Cancel", form, func(ok bool) {
		if !ok {
			return
		}
		port, err := strconv.Atoi(strings.TrimSpace(portEntry.Text))
		if err != nil {
			dialog.ShowError(fmt.Errorf("invalid port number"), win)
			return
		}

		id := randomID()
		if isEdit {
			id = existing.ID
		}

		sess := config.Session{
			ID:          id,
			Label:       labelEntry.Text,
			Host:        hostEntry.Text,
			Port:        port,
			User:        userEntry.Text,
			Password:    passEntry.Text,
			KeyPath:     keyEntry.Text,
			UseKey:      useKeyCheck.Checked,
			Group:       groupEntry.Text,
			AutoConnect: autoCheck.Checked,
		}
		if err := normalizeSession(&sess); err != nil {
			dialog.ShowError(err, win)
			return
		}
		onSave(sess)
	}, win)
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

package ui

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	if existing == nil {
		sessShowDialog(win, false, config.Session{}, "22", onSave)
		return
	}
	sessShowDialog(win, true, *existing, strconv.Itoa(existing.Port), onSave)
}

// sessShowDialog shows the session dialog filled in with draft and
// portText. If what the user entered is invalid, it shows the error and
// then opens again with the entries as they were, so nothing typed is lost.
func sessShowDialog(win fyne.Window, isEdit bool, draft config.Session, portText string, onSave func(config.Session)) {
	labelEntry := widget.NewEntry()
	labelEntry.SetPlaceHolder("My Server")

	hostEntry := widget.NewEntry()
	hostEntry.SetPlaceHolder("192.168.1.1")

	portEntry := widget.NewEntry()
	portEntry.SetText(portText)

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

	labelEntry.SetText(draft.Label)
	hostEntry.SetText(draft.Host)
	userEntry.SetText(draft.User)
	passEntry.SetText(draft.Password)
	keyEntry.SetText(draft.KeyPath)
	useKeyCheck.SetChecked(draft.UseKey)
	groupEntry.SetText(draft.Group)
	autoCheck.SetChecked(draft.AutoConnect)

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
		// The dialog is already closed: on an error, reopen it with what
		// was entered once the error has been read.
		entered := config.Session{
			ID:          draft.ID,
			Label:       labelEntry.Text,
			Host:        hostEntry.Text,
			User:        userEntry.Text,
			Password:    passEntry.Text,
			KeyPath:     keyEntry.Text,
			UseKey:      useKeyCheck.Checked,
			Group:       groupEntry.Text,
			AutoConnect: autoCheck.Checked,
		}
		enteredPort := portEntry.Text
		retry := func(err error) {
			d := dialog.NewError(err, win)
			d.SetOnClosed(func() { sessShowDialog(win, isEdit, entered, enteredPort, onSave) })
			d.Show()
		}

		port, err := strconv.Atoi(strings.TrimSpace(enteredPort))
		if err != nil {
			retry(fmt.Errorf("invalid port number"))
			return
		}
		sess := entered
		sess.Port = port
		if !isEdit {
			sess.ID = randomID()
		}
		if err := normalizeSession(&sess); err != nil {
			retry(err)
			return
		}
		if err := sessApplyUseKey(&sess); err != nil {
			retry(err)
			return
		}
		onSave(sess)
	}, win)
}

// sessApplyUseKey makes a session saved from the dialog do what the "Use
// SSH Key" box shows. It needs a key path: Connect would otherwise skip the
// key without a word. And the password, whose field the box disables, is
// not saved: it would be exported, and sent to the server whenever the key
// is rejected. Imported sessions are not checked this way, so older
// entries still load.
func sessApplyUseKey(s *config.Session) error {
	if !s.UseKey {
		return nil
	}
	if s.KeyPath == "" {
		return errors.New(`"Use SSH Key" is checked, but the SSH key path is empty`)
	}
	s.Password = ""
	return nil
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

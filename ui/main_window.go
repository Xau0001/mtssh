package ui

import (
	"errors"
	"fmt"
	"mtssh/config"
	"mtssh/core"
	"mtssh/logger"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// MainWindow builds and returns the main application window
func MainWindow(app fyne.App, sessions []config.Session, onSave func([]config.Session) error) fyne.Window {
	win := app.NewWindow("MTSSH — Multi-Tabbed SSH Client")
	win.Resize(fyne.NewSize(1200, 750))

	// ── Draggable tab container (replaces container.AppTabs) ─────────────────
	tabs := NewDraggableTabContainer()

	termTabs := map[string]*TermTab{}

	// openSFTPTab opens SFTP manager as a new draggable tab
	openSFTPTab := func(targetWin fyne.Window, targetTabs *DraggableTabContainer, sess config.Session, sshSess *core.SSHSession) {
		client := sshSess.Client()
		if client == nil {
			dialog.ShowError(errors.New("SSH client not connected"), targetWin)
			return
		}
		// Opening the SFTP subsystem is a network round trip — keep it off the UI goroutine
		go func() {
			sc, err := core.NewSFTPClient(client)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, targetWin)
					return
				}
				sftpTab := NewSFTPTab(sc, targetWin)
				item := NewDraggableTabItem("SFTP: "+sess.Label, theme.FolderIcon(), sftpTab.Container)
				item.OnClose = sc.Close
				targetTabs.Append(item)
			})
		}()
	}

	// openSessionInWindow opens a session in a new independent window
	var openSessionInWindow func(sess config.Session)
	openSessionInWindow = func(sess config.Session) {
		newWin := app.NewWindow("MTSSH — " + sess.Label)
		newWin.Resize(fyne.NewSize(900, 600))
		newTabs := NewDraggableTabContainer()

		tt := NewTermTab(sess, newWin)
		tt.OnOpenSFTP = func(s config.Session, sshSess *core.SSHSession) {
			openSFTPTab(newWin, newTabs, s, sshSess)
		}
		tt.OnOpenInWindow = openSessionInWindow

		item := NewDraggableTabItem(sess.Label, theme.ComputerIcon(), tt.Container)
		item.OnClose = tt.Close
		item.OnSelected = tt.Focus
		// Content first, so the terminal is part of the window when Append focuses it
		newWin.SetContent(newTabs.Container())
		newTabs.Append(item)
		// Disconnects the terminal and closes any SFTP tabs opened in this window
		newWin.SetOnClosed(newTabs.CloseAll)
		newWin.Show()
		tt.Connect()
	}

	// openSession opens a session as a tab in the main window
	openSession := func(sess config.Session) {
		if tt, ok := termTabs[sess.ID]; ok {
			// already open — find and select it
			for i, item := range tabs.Items() {
				if item.Content == tt.Container {
					tabs.Select(i)
					return
				}
			}
			// Stale map entry (tab was removed without OnClose firing) — clean up
			tt.Close()
			delete(termTabs, sess.ID)
		}
		tt := NewTermTab(sess, win)
		tt.OnOpenSFTP = func(s config.Session, sshSess *core.SSHSession) {
			openSFTPTab(win, tabs, s, sshSess)
		}
		tt.OnOpenInWindow = openSessionInWindow
		termTabs[sess.ID] = tt

		item := NewDraggableTabItem(sess.Label, theme.ComputerIcon(), tt.Container)
		item.OnClose = func() {
			delete(termTabs, sess.ID)
			tt.Close()
		}
		item.OnSelected = tt.Focus
		tabs.Append(item)
		tt.Connect()
	}

	// ── Session list ─────────────────────────────────────────────────────────
	// Single click selects a session (for Edit / Delete / New Window),
	// double click connects.
	// selectedSession is the index of the selected session, or -1.
	selectedSession := -1
	selected := func() (int, bool) {
		ok := selectedSession >= 0 && selectedSession < len(sessions)
		return selectedSession, ok
	}
	sessionList := newDoubleTapList(
		func() int { return len(sessions) },
		func() fyne.CanvasObject {
			return container.NewHBox(
				widget.NewIcon(theme.ComputerIcon()),
				widget.NewLabel("placeholder"),
			)
		},
		func(i widget.ListItemID, obj fyne.CanvasObject) {
			box := obj.(*fyne.Container)
			lbl := box.Objects[1].(*widget.Label)
			s := sessions[i]
			text := s.Label
			if s.Group != "" {
				text = "[" + s.Group + "] " + s.Label
			}
			lbl.SetText(text)
		},
		func(id widget.ListItemID) {
			if id >= 0 && id < len(sessions) {
				openSession(sessions[id])
			}
		},
	)
	sessionList.OnSelected = func(id widget.ListItemID) { selectedSession = id }
	sessionList.OnUnselected = func(widget.ListItemID) { selectedSession = -1 }

	save := func() {
		if err := onSave(sessions); err != nil {
			logger.Error("config", err.Error())
			dialog.ShowError(fmt.Errorf("could not save sessions: %w", err), win)
		}
	}

	// ── Session buttons ───────────────────────────────────────────────────────
	addBtn := widget.NewButtonWithIcon("New", theme.ContentAddIcon(), func() {
		ShowSessionDialog(win, nil, func(s config.Session) {
			sessions = append(sessions, s)
			sessionList.Refresh()
			save()
		})
	})
	editBtn := widget.NewButtonWithIcon("Edit", theme.DocumentCreateIcon(), func() {
		sel, ok := selected()
		if !ok {
			dialog.ShowInformation("Edit", "Select a session first.", win)
			return
		}
		ShowSessionDialog(win, &sessions[sel], func(s config.Session) {
			sessions[sel] = s
			sessionList.Refresh()
			save()
		})
	})
	deleteBtn := widget.NewButtonWithIcon("Delete", theme.DeleteIcon(), func() {
		sel, ok := selected()
		if !ok {
			dialog.ShowInformation("Delete", "Select a session first.", win)
			return
		}
		dialog.ShowConfirm("Delete", "Delete \""+sessions[sel].Label+"\"?", func(ok bool) {
			if ok {
				sessions = append(sessions[:sel], sessions[sel+1:]...)
				// the index now points at another session (or past the end)
				sessionList.UnselectAll()
				selectedSession = -1
				sessionList.Refresh()
				save()
			}
		}, win)
	})
	newWinBtn := widget.NewButtonWithIcon("New Window", theme.ViewFullScreenIcon(), func() {
		sel, ok := selected()
		if !ok {
			dialog.ShowInformation("New Window", "Select a session first.", win)
			return
		}
		openSessionInWindow(sessions[sel])
	})

	// ── Tools ─────────────────────────────────────────────────────────────────
	exportBtn := widget.NewButtonWithIcon("Export", theme.UploadIcon(), func() {
		ExportSessions(win, sessions)
	})
	importBtn := widget.NewButtonWithIcon("Import", theme.DownloadIcon(), func() {
		ImportSessions(win, sessions, func(merged []config.Session) {
			sessions = merged
			sessionList.UnselectAll()
			selectedSession = -1
			sessionList.Refresh()
			save()
		})
	})
	knownHostsBtn := widget.NewButtonWithIcon("Known Hosts", theme.SettingsIcon(), func() {
		ShowKnownHostsEditor(app)
	})

	// ── Theme selector ────────────────────────────────────────────────────────
	themeNames := make([]string, len(AllThemes))
	for i, name := range AllThemes {
		themeNames[i] = string(name)
	}
	themeSelect := widget.NewSelect(themeNames, func(name string) {
		ApplyTheme(app, ThemeName(name))
	})
	themeSelect.SetSelected(string(SavedTheme(app)))

	// ── Sidebar layout ────────────────────────────────────────────────────────
	sidebar := container.NewBorder(
		container.NewVBox(
			widget.NewLabelWithStyle("Sessions", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
			container.NewHBox(widget.NewLabel("Theme:"), themeSelect),
		),
		container.NewVBox(
			widget.NewSeparator(),
			container.NewGridWithColumns(3, exportBtn, importBtn, knownHostsBtn),
			widget.NewSeparator(),
			container.NewGridWithColumns(2, addBtn, editBtn, deleteBtn, newWinBtn),
		),
		nil, nil,
		sessionList,
	)

	split := container.NewHSplit(sidebar, tabs.Container())
	split.SetOffset(0.22)
	win.SetContent(split)
	win.SetMaster()

	// Auto-connect
	for _, s := range sessions {
		if s.AutoConnect {
			openSession(s)
		}
	}

	return win
}

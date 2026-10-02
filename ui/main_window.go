package ui

import (
	"errors"
	"fmt"
	"maps"
	"mtssh/config"
	"mtssh/core"
	"mtssh/logger"
	"slices"

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
	detachedTabs := map[*TermTab]bool{} // terminals in windows of their own
	// updateOpenTabs gives the open terminals of session s its new
	// settings (after Edit or an import), so Reconnect and "New Window"
	// don't go on with the old host, user or password.
	updateOpenTabs := func(s config.Session) {
		if tt, ok := termTabs[s.ID]; ok {
			tt.SetSession(s)
		}
		for tt := range detachedTabs {
			if tt.settings().ID == s.ID {
				tt.SetSession(s)
			}
		}
	}
	windows := newOpenWindows()
	windows.addTabs(tabs)

	// Set when the window closes, so an SFTP connection that finishes
	// opening afterwards is not added to it (all on the UI goroutine).
	winClosed := false
	win.SetOnClosed(func() { winClosed = true })

	// openSFTPTab opens SFTP manager as a new draggable tab. *closed
	// reports whether targetWin has been closed.
	openSFTPTab := func(targetWin fyne.Window, targetTabs *DraggableTabContainer, closed *bool, sess config.Session, sshSess *core.SSHSession) {
		client := sshSess.Client()
		if client == nil {
			dialog.ShowError(errors.New("SSH client not connected"), targetWin)
			return
		}
		// Opening the SFTP subsystem is a network round trip — keep it off the UI goroutine
		go func() {
			sc, err := core.NewSFTPClient(client)
			fyne.Do(func() {
				if *closed || windows.quitting {
					// Nothing would ever close a tab added now.
					if err == nil {
						sc.Close()
					}
					return
				}
				if err != nil {
					// The server's message can be huge.
					logger.Error("sftp", "cannot open SFTP: "+err.Error())
					dialog.ShowError(errors.New(shortErr(err)), targetWin)
					return
				}
				select {
				case <-sshSess.Done():
					// The terminal was closed or disconnected meanwhile,
					// taking the connection with it.
					sc.Close()
					return
				default:
				}
				sftpTab := NewSFTPTab(sc, targetWin)
				item := NewDraggableTabItem("SFTP: "+sessDisplay(sess.Label), theme.FolderIcon(), sftpTab.Container)
				// Lets a running transfer clean up before the connection closes.
				item.OnClose = sftpTab.Close
				windows.addSFTP(sftpTab, targetTabs)
				targetTabs.Append(item)
			})
		}()
	}

	// openSessionInWindow opens a session in a new independent window
	var openSessionInWindow func(sess config.Session)
	openSessionInWindow = func(sess config.Session) {
		newWin := app.NewWindow("MTSSH — " + sessDisplay(sess.Label))
		newWin.Resize(fyne.NewSize(900, 600))
		newTabs := NewDraggableTabContainer()
		windows.addTabs(newTabs)
		newWinClosed := false

		tt := NewTermTab(sess, newWin)
		tt.OnOpenSFTP = func(s config.Session, sshSess *core.SSHSession) {
			openSFTPTab(newWin, newTabs, &newWinClosed, s, sshSess)
		}
		tt.OnOpenInWindow = openSessionInWindow
		detachedTabs[tt] = true

		item := NewDraggableTabItem(sessDisplay(sess.Label), theme.ComputerIcon(), tt.Container)
		item.OnClose = func() {
			delete(detachedTabs, tt)
			tt.Close()
		}
		item.OnSelected = tt.Focus
		// Content first, so the terminal is part of the window when Append focuses it
		newWin.SetContent(newTabs.Container())
		newTabs.Append(item)
		// Closes any SFTP tabs opened in this window and disconnects the terminal
		newWin.SetOnClosed(func() {
			newWinClosed = true
			windows.closeTabs([]*DraggableTabContainer{newTabs}, func() {})
		})
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
			openSFTPTab(win, tabs, &winClosed, s, sshSess)
		}
		tt.OnOpenInWindow = openSessionInWindow
		termTabs[sess.ID] = tt

		item := NewDraggableTabItem(sessDisplay(sess.Label), theme.ComputerIcon(), tt.Container)
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
			// Sessions stored before validation existed may hold
			// control or bidi characters.
			text := sessDisplay(s.Label)
			if s.Group != "" {
				text = "[" + sessDisplay(s.Group) + "] " + text
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
			updateOpenTabs(s)
		})
	})
	deleteBtn := widget.NewButtonWithIcon("Delete", theme.DeleteIcon(), func() {
		sel, ok := selected()
		if !ok {
			dialog.ShowInformation("Delete", "Select a session first.", win)
			return
		}
		dialog.ShowConfirm("Delete", "Delete \""+sessDisplay(sessions[sel].Label)+"\"?", func(ok bool) {
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
			for _, s := range merged { // overwritten duplicates
				updateOpenTabs(s)
			}
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
	// Closing the main window quits, which would skip the cleanup of
	// running transfers (partial remote files, local temp files) in every
	// window: close all tabs first.
	win.SetCloseIntercept(func() {
		switch {
		case windows.quitting:
			app.Quit() // asked again while transfers clean up: don't wait
		case windows.busy():
			dialog.ShowConfirm("Quit", "A file transfer is running.\nCancel it and quit?", func(ok bool) {
				if ok {
					win.SetTitle(win.Title() + " — stopping transfers…")
					windows.quit(app.Quit)
				}
			}, win)
		default:
			windows.quit(app.Quit)
		}
	})

	// Auto-connect
	for _, s := range sessions {
		if s.AutoConnect {
			openSession(s)
		}
	}

	return win
}

// openWindows keeps track of the tab containers of all windows and of the
// SFTP tabs in them, for closing a window and for quitting. Their SFTP
// tabs are closed first: a transfer cleans up on the server (partial file,
// staging directory) over its terminal's SSH connection, so the other tabs,
// and with them the SSH connections, are closed only once that is done.
// Used on the UI goroutine only.
type openWindows struct {
	tabs     map[*DraggableTabContainer]bool
	sftp     map[*SFTPTab]*DraggableTabContainer // until closed and done
	quitting bool
}

func newOpenWindows() *openWindows {
	return &openWindows{
		tabs: map[*DraggableTabContainer]bool{},
		sftp: map[*SFTPTab]*DraggableTabContainer{},
	}
}

// addTabs registers the tab container of a window.
func (w *openWindows) addTabs(tabs *DraggableTabContainer) {
	w.tabs[tabs] = true
}

// addSFTP registers an SFTP tab shown in tabs and forgets those that are
// closed and done.
func (w *openWindows) addSFTP(t *SFTPTab, tabs *DraggableTabContainer) {
	for old := range w.sftp {
		select {
		case <-old.Done():
			delete(w.sftp, old)
		default:
		}
	}
	w.sftp[t] = tabs
}

// busy reports whether an SFTP tab in any window has a transfer running
// (or about to start).
func (w *openWindows) busy() bool {
	for t := range w.sftp {
		if t.Busy() {
			return true
		}
	}
	return false
}

// quit closes the tabs of all windows (see closeTabs), then calls quitApp.
// Asked again meanwhile, it calls quitApp at once.
func (w *openWindows) quit(quitApp func()) {
	if w.quitting {
		quitApp()
		return
	}
	w.quitting = true
	w.closeTabs(slices.Collect(maps.Keys(w.tabs)), quitApp)
}

// closeTabs closes the SFTP tabs in all (cancelling their transfers) and
// waits in the background until they are done, at most sftpCloseWait (see
// SFTPTab.Close). Then, on the UI goroutine, it closes the other tabs in
// all, forgets all and calls then.
func (w *openWindows) closeTabs(all []*DraggableTabContainer, then func()) {
	var done []<-chan struct{}
	for t, tabs := range w.sftp {
		if slices.Contains(all, tabs) {
			t.Close()
			done = append(done, t.Done())
		}
	}
	go func() {
		for _, d := range done {
			<-d
		}
		fyne.Do(func() {
			for _, tabs := range all {
				tabs.CloseAll()
				delete(w.tabs, tabs)
			}
			then()
		})
	}()
}

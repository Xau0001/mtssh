package ui

import (
	"mtputty/config"
	"mtputty/core"
	"mtputty/logger"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// TermTab is the content for a single SSH terminal tab
type TermTab struct {
	Session    config.Session
	sshSession *core.SSHSession
	output     *widget.TextGrid
	input      *widget.Entry
	statusLbl  *widget.Label
	buf        strings.Builder
	Container  fyne.CanvasObject
	win        fyne.Window

	// OnOpenSFTP is called when the user clicks "SFTP"
	OnOpenSFTP func(sess config.Session, sshSess *core.SSHSession)
	// OnOpenInWindow detaches this session into its own window
	OnOpenInWindow func(sess config.Session)
}

// NewTermTab builds the UI for one SSH terminal tab
func NewTermTab(sess config.Session, win fyne.Window) *TermTab {
	t := &TermTab{Session: sess, win: win}

	t.output = widget.NewTextGrid()
	t.output.ShowLineNumbers = false

	scroll := container.NewScroll(t.output)
	scroll.SetMinSize(fyne.NewSize(600, 400))

	t.statusLbl = widget.NewLabel("⬤ Disconnected")

	t.input = widget.NewEntry()
	t.input.SetPlaceHolder("Type command and press Enter…")
	t.input.OnSubmitted = func(cmd string) {
		if t.sshSession == nil {
			return
		}
		if err := t.sshSession.SendCommand(cmd + "\n"); err != nil {
			t.appendOutput("[mtputty] send error: " + err.Error() + "\r\n")
			logger.Error(sess.Label, err.Error())
		}
		t.input.SetText("")
	}

	reconnectBtn := widget.NewButtonWithIcon("Reconnect", theme.ViewRefreshIcon(), func() {
		go t.connect()
	})
	disconnectBtn := widget.NewButtonWithIcon("Disconnect", theme.CancelIcon(), func() {
		if t.sshSession != nil {
			t.sshSession.Disconnect()
		}
	})
	sftpBtn := widget.NewButtonWithIcon("SFTP", theme.FolderOpenIcon(), func() {
		if t.OnOpenSFTP != nil && t.sshSession != nil && t.sshSession.IsRunning() {
			t.OnOpenSFTP(t.Session, t.sshSession)
		} else {
			dialog.ShowInformation("SFTP", "Not connected. Connect first.", t.win)
		}
	})
	newWinBtn := widget.NewButtonWithIcon("New Window", theme.ViewFullScreenIcon(), func() {
		if t.OnOpenInWindow != nil {
			t.OnOpenInWindow(t.Session)
		}
	})

	toolbar := container.NewHBox(t.statusLbl, reconnectBtn, disconnectBtn, sftpBtn, newWinBtn)
	bottom := container.NewBorder(nil, nil, nil, nil, t.input)
	t.Container = container.NewBorder(toolbar, bottom, nil, nil, scroll)
	return t
}

// Connect starts the SSH connection asynchronously
func (t *TermTab) Connect() {
	go t.connect()
}

func (t *TermTab) connect() {
	t.setStatus(false)
	t.appendOutput("[mtputty] Connecting to " + t.Session.Host + "…\r\n")

	t.sshSession = core.NewSSHSession(
		t.Session,
		func(line string) { t.appendOutput(line) },
		func(connected bool) {
			t.setStatus(connected)
			if !connected && t.Session.AutoConnect {
				t.appendOutput("[mtputty] Auto-reconnect in 5s…\r\n")
				go t.sshSession.ConnectWithRetry(5)
			}
		},
	)

	// Known-hosts: block goroutine until user decides in main thread
	t.sshSession.HostKeyPrompt = func(host, keyType, fp string) core.HostKeyDecision {
		result := make(chan core.HostKeyDecision, 1)
		fyne.CurrentApp().Driver().RunOnMain(func() {
			msg := "Unknown host key for:\n" + host +
				"\n\nType:        " + keyType +
				"\nFingerprint: " + fp +
				"\n\nDo you want to trust and save this host key?"
			dialog.ShowConfirm("Unknown Host Key", msg, func(ok bool) {
				if ok {
					result <- core.HostKeyAccept
				} else {
					result <- core.HostKeyReject
				}
			}, t.win)
		})
		return <-result
	}

	// Passphrase-protected SSH key: block until user enters passphrase
	t.sshSession.KeyPassphrasePrompt = func(keyPath string) string {
		result := make(chan string, 1)
		fyne.CurrentApp().Driver().RunOnMain(func() {
			entry := widget.NewPasswordEntry()
			entry.SetPlaceHolder("Key passphrase")
			dialog.ShowCustomConfirm(
				"SSH Key Passphrase",
				"Unlock", "Cancel",
				container.NewVBox(
					widget.NewLabel("Key: "+keyPath),
					widget.NewLabel("This key is passphrase-protected. Enter the passphrase to unlock it."),
					entry,
				),
				func(ok bool) {
					if ok {
						result <- entry.Text
					} else {
						result <- ""
					}
				}, t.win)
		})
		return <-result
	}

	if err := t.sshSession.Connect(); err != nil {
		t.appendOutput("[mtputty] Connection failed: " + err.Error() + "\r\n")
		logger.Error(t.Session.Label, err.Error())
		t.setStatus(false)
	}
}

func (t *TermTab) appendOutput(s string) {
	t.buf.WriteString(s)
	text := t.buf.String()
	// Cap buffer at 50 KB to prevent unbounded memory growth
	if len(text) > 50000 {
		text = text[len(text)-50000:]
		t.buf.Reset()
		t.buf.WriteString(text)
	}
	display := strings.ReplaceAll(text, "\r\n", "\n")
	display = strings.ReplaceAll(display, "\r", "\n")
	t.output.SetText(display)
}

func (t *TermTab) setStatus(connected bool) {
	if connected {
		t.statusLbl.SetText("⬤ Connected — " + t.Session.Host)
	} else {
		t.statusLbl.SetText("⬤ Disconnected")
	}
}

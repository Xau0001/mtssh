package ui

import (
	"bytes"
	"mtssh/config"
	"mtssh/core"
	"mtssh/logger"
	"strings"
	"sync"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// maxScrollback caps the terminal buffer to prevent unbounded memory growth.
const maxScrollback = 50000

// TermTab is the content for a single SSH terminal tab
type TermTab struct {
	Session   config.Session
	output    *widget.TextGrid
	scroll    *container.Scroll
	input     *widget.Entry
	statusLbl *widget.Label
	buf       strings.Builder
	bufMu     sync.Mutex
	Container fyne.CanvasObject
	win       fyne.Window

	sessMu     sync.Mutex // guards sshSession (read by UI callbacks, replaced by connect)
	sshSession *core.SSHSession

	connectMu sync.Mutex // guards concurrent connect() calls

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

	t.scroll = container.NewScroll(t.output)
	t.scroll.SetMinSize(fyne.NewSize(600, 400))

	t.statusLbl = widget.NewLabel("• Disconnected")

	t.input = widget.NewEntry()
	t.input.SetPlaceHolder("Type command and press Enter…")
	t.input.OnSubmitted = func(cmd string) {
		s := t.session()
		if s == nil {
			return
		}
		if err := s.SendCommand(cmd + "\n"); err != nil {
			t.appendOutput("[mtssh] send error: " + err.Error() + "\r\n")
			logger.Error(sess.Label, err.Error())
		}
		t.input.SetText("")
	}

	reconnectBtn := widget.NewButtonWithIcon("Reconnect", theme.ViewRefreshIcon(), t.Connect)
	disconnectBtn := widget.NewButtonWithIcon("Disconnect", theme.CancelIcon(), t.Disconnect)
	sftpBtn := widget.NewButtonWithIcon("SFTP", theme.FolderOpenIcon(), func() {
		if s := t.session(); t.OnOpenSFTP != nil && s != nil && s.IsRunning() {
			t.OnOpenSFTP(t.Session, s)
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
	t.Container = container.NewBorder(toolbar, t.input, nil, nil, t.scroll)
	return t
}

// Connect starts the SSH connection asynchronously
func (t *TermTab) Connect() {
	go t.connect()
}

// Disconnect closes the current SSH session, if any.
func (t *TermTab) Disconnect() {
	if s := t.session(); s != nil {
		s.Disconnect()
	}
}

func (t *TermTab) session() *core.SSHSession {
	t.sessMu.Lock()
	defer t.sessMu.Unlock()
	return t.sshSession
}

func (t *TermTab) connect() {
	t.connectMu.Lock()
	defer t.connectMu.Unlock()

	// Stop any previous session before creating a new one
	t.Disconnect()
	t.setStatus(false)
	t.appendOutput("[mtssh] Connecting to " + t.Session.Host + "…\r\n")

	var sess *core.SSHSession
	sess = core.NewSSHSession(
		t.Session,
		t.appendOutput,
		func(connected bool) {
			if t.session() != sess {
				return // late callback from a session that was replaced
			}
			t.setStatus(connected)
			if !connected && t.Session.AutoConnect {
				go sess.ConnectWithRetry(3)
			}
		},
	)

	// Known-hosts: block goroutine until user decides
	sess.HostKeyPrompt = func(host, keyType, fp string) core.HostKeyDecision {
		result := make(chan core.HostKeyDecision, 1)
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
		return <-result
	}

	// No password/key configured: ask with a masked entry
	sess.PasswordPrompt = func() string {
		return t.promptSecret("SSH Password", "Password",
			"Password for "+t.Session.User+"@"+t.Session.Host+":")
	}

	// Passphrase-protected SSH key: block until user enters passphrase
	sess.KeyPassphrasePrompt = func(keyPath string) string {
		return t.promptSecret("SSH Key Passphrase", "Key passphrase",
			"Key: "+keyPath,
			"This key is passphrase-protected. Enter the passphrase to unlock it.")
	}

	t.sessMu.Lock()
	t.sshSession = sess
	t.sessMu.Unlock()

	if err := sess.Connect(); err != nil {
		t.appendOutput("[mtssh] Connection failed: " + err.Error() + "\r\n")
		logger.Error(t.Session.Label, err.Error())
		t.setStatus(false)
	}
}

// promptSecret shows a modal dialog with a password entry and blocks until
// the user confirms or cancels (""). Must not be called on the UI goroutine.
func (t *TermTab) promptSecret(title, placeholder string, lines ...string) string {
	result := make(chan string, 1)
	entry := widget.NewPasswordEntry()
	entry.SetPlaceHolder(placeholder)
	content := container.NewVBox()
	for _, l := range lines {
		content.Add(widget.NewLabel(l))
	}
	content.Add(entry)

	var once sync.Once
	send := func(v string) { once.Do(func() { result <- v }) }
	d := dialog.NewCustomConfirm(title, "OK", "Cancel", content, func(ok bool) {
		if ok {
			send(entry.Text)
		} else {
			send("")
		}
	}, t.win)
	// Enter confirms; the callback that Hide() fires with false is then ignored.
	entry.OnSubmitted = func(text string) {
		send(text)
		d.Hide()
	}
	d.Show()
	t.win.Canvas().Focus(entry)
	return <-result
}

func (t *TermTab) appendOutput(s string) {
	t.bufMu.Lock()
	defer t.bufMu.Unlock()
	t.buf.WriteString(s)
	if t.buf.Len() > maxScrollback {
		text := t.buf.String()
		cut := len(text) - maxScrollback
		// Don't start the buffer in the middle of a multi-byte character
		for cut < len(text) && !utf8.RuneStart(text[cut]) {
			cut++
		}
		t.buf.Reset()
		t.buf.WriteString(text[cut:])
	}
	// Updating under bufMu keeps concurrent writers (stdout/stderr) from
	// overwriting newer text with an older snapshot.
	t.output.SetText(renderTerminal(t.buf.String()))
	t.scroll.ScrollToBottom()
}

func (t *TermTab) setStatus(connected bool) {
	if connected {
		t.statusLbl.SetText("• Connected — " + t.Session.Host)
	} else {
		t.statusLbl.SetText("• Disconnected")
	}
}

// renderTerminal turns raw PTY output into plain text for the TextGrid.
// The grid is no terminal emulator, so ANSI/VT100 sequences (colors,
// cursor movement, window titles, bracketed paste) and other control
// characters are removed instead of being shown as garbage. A lone "\r"
// lets the following text replace the current line (progress bars, prompts).
func renderTerminal(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == 0x1b: // ESC
			i = escapeEnd(s, i)
		case c == '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				continue
			}
			out = out[:bytes.LastIndexByte(out, '\n')+1]
		case c == '\n' || c == '\t' || (c >= 0x20 && c != 0x7f):
			out = append(out, c)
		}
		// Remaining control characters (BEL, backspace, …) are dropped.
	}
	return string(out)
}

// escapeEnd returns the index of the last byte of the escape sequence that
// starts at s[i]. An unterminated sequence extends to the end of s (the rest
// arrives with the next chunk).
func escapeEnd(s string, i int) int {
	if i+1 >= len(s) {
		return i
	}
	switch s[i+1] {
	case '[': // CSI: parameter bytes, then a final byte in 0x40–0x7E
		for j := i + 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j
			}
		}
	case ']', 'P', '_', '^': // OSC/DCS/APC/PM: terminated by BEL or ESC \
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 1
			}
		}
	case '(', ')', '*', '+': // character set designation, e.g. ESC ( B
		return min(i+2, len(s)-1)
	default: // two-byte sequence, e.g. ESC = or ESC 7
		return i + 1
	}
	return len(s) - 1
}

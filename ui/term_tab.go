package ui

import (
	"io"
	"mtssh/config"
	"mtssh/core"
	"mtssh/logger"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/fyne-io/terminal"
)

// resetScreen leaves the alternate screen (vim, less, htop), resets text
// attributes and shows the cursor, in case a session ends inside such a program.
const resetScreen = "\x1b[?1049l\x1b[0m\x1b[?25h"

// TermTab is the content for a single SSH terminal tab
type TermTab struct {
	Session   config.Session
	term      *terminal.Terminal
	output    *termBuffer // everything written here appears in the terminal
	sizes     chan terminal.Config
	statusLbl *widget.Label
	Container fyne.CanvasObject
	win       fyne.Window
	closeOnce sync.Once

	sessMu     sync.Mutex // guards sshSession, rows and cols
	sshSession *core.SSHSession
	rows, cols int // last size reported by the terminal widget

	connectMu sync.Mutex // guards concurrent connect() calls

	// OnOpenSFTP is called when the user clicks "SFTP"
	OnOpenSFTP func(sess config.Session, sshSess *core.SSHSession)
	// OnOpenInWindow detaches this session into its own window
	OnOpenInWindow func(sess config.Session)
}

// NewTermTab builds the UI for one SSH terminal tab. Must be called on the
// UI goroutine.
func NewTermTab(sess config.Session, win fyne.Window) *TermTab {
	t := &TermTab{Session: sess, win: win, output: newTermBuffer()}

	// The widget keeps a single connection for its whole life: keystrokes go
	// to whichever SSH session is current, and the output of every session
	// (plus our status lines) is read from t.output. Reconnecting therefore
	// keeps the screen content.
	t.term = terminal.New()
	go func() { _ = t.term.RunWithConnection(terminalInput{t}, t.output) }()
	t.sizes = make(chan terminal.Config, 8)
	t.term.AddListener(t.sizes)
	go t.forwardResizes()

	t.statusLbl = widget.NewLabel("• Disconnected")

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
	t.Container = container.NewBorder(toolbar, nil, nil, nil, t.term)
	return t
}

// Connect starts the SSH connection asynchronously
func (t *TermTab) Connect() {
	// End the old session right away: this also closes any dialog it is
	// waiting on, which would otherwise hold up the new connect.
	t.Disconnect()
	go t.connect()
}

// Disconnect closes the current SSH session, if any.
func (t *TermTab) Disconnect() {
	if s := t.session(); s != nil {
		s.Disconnect()
	}
}

// Close disconnects and releases the terminal; the tab cannot be reused.
func (t *TermTab) Close() {
	t.closeOnce.Do(func() {
		t.Disconnect()
		t.term.RemoveListener(t.sizes) // ends forwardResizes
		t.output.Close()               // ends the terminal's read loop
		t.term.Close()                 // in case it never started reading
	})
}

// Focus gives the terminal keyboard focus. Must be called on the UI goroutine.
func (t *TermTab) Focus() {
	t.win.Canvas().Focus(t.term)
}

func (t *TermTab) session() *core.SSHSession {
	t.sessMu.Lock()
	defer t.sessMu.Unlock()
	return t.sshSession
}

func (t *TermTab) write(s string) {
	_, _ = t.output.Write([]byte(s))
}

// forwardResizes passes terminal size changes on to the SSH session.
func (t *TermTab) forwardResizes() {
	for cfg := range t.sizes {
		// Only the most recent size matters
	drain:
		for {
			select {
			case next, ok := <-t.sizes:
				if !ok {
					break drain
				}
				cfg = next
			default:
				break drain
			}
		}
		rows, cols := int(cfg.Rows), int(cfg.Columns)
		t.sessMu.Lock()
		t.rows, t.cols = rows, cols
		s := t.sshSession
		t.sessMu.Unlock()
		if s != nil {
			s.Resize(rows, cols)
		}
	}
}

func (t *TermTab) connect() {
	t.connectMu.Lock()
	defer t.connectMu.Unlock()

	// Stop any previous session before creating a new one
	t.Disconnect()
	t.setStatus(false)
	t.write(resetScreen + "\r\n[mtssh] Connecting to " + t.Session.Host + "…\r\n")

	var sess *core.SSHSession
	sess = core.NewSSHSession(
		t.Session,
		t.write,
		func(connected bool) {
			if t.session() != sess {
				return // late callback from a session that was replaced
			}
			t.setStatus(connected)
			if !connected {
				t.write(resetScreen + "\r\n[mtssh] Session closed.\r\n")
				if t.Session.AutoConnect {
					go sess.ConnectWithRetry(3)
				}
			}
		},
	)

	// Known-hosts: block goroutine until user decides
	// Prompts block the connecting goroutine until the user answers — or
	// until the session is disconnected (tab or window closed, Reconnect),
	// which closes the dialog and counts as "no".
	sess.HostKeyPrompt = func(host, keyType, fp string) core.HostKeyDecision {
		result := make(chan bool, 1)
		msg := "Unknown host key for:\n" + host +
			"\n\nType:        " + keyType +
			"\nFingerprint: " + fp +
			"\n\nDo you want to trust and save this host key?"
		var d dialog.Dialog
		fyne.Do(func() {
			d = dialog.NewConfirm("Unknown Host Key", msg, func(ok bool) { result <- ok }, t.win)
			d.Show()
		})
		if ok, answered := awaitAnswer(result, sess.Done(), &d); answered && ok {
			return core.HostKeyAccept
		}
		return core.HostKeyReject
	}

	// Password / one-time code the server asks for: masked entry. The
	// prompt text comes from the server, so say clearly who is asking.
	sess.PasswordPrompt = func(prompt string) string {
		return t.promptSecret(sess.Done(), "SSH Authentication", "",
			"Server "+t.Session.User+"@"+t.Session.Host+" asks:", prompt)
	}

	// Passphrase-protected SSH key: block until user enters passphrase
	sess.KeyPassphrasePrompt = func(keyPath string) string {
		return t.promptSecret(sess.Done(), "SSH Key Passphrase", "Key passphrase",
			"Key: "+keyPath,
			"This key is passphrase-protected. Enter the passphrase to unlock it.")
	}

	t.sessMu.Lock()
	t.sshSession = sess
	rows, cols := t.rows, t.cols
	t.sessMu.Unlock()
	sess.Resize(rows, cols) // PTY size = current widget size (ignored if not laid out yet)

	if err := sess.Connect(); err != nil {
		t.write("[mtssh] Connection failed: " + err.Error() + "\r\n")
		logger.Error(t.Session.Label, err.Error())
		t.setStatus(false)
	}
}

// promptSecret shows a modal dialog with a password entry and blocks until
// the user confirms, cancels ("") or done is closed (""). Must not be called
// on the UI goroutine.
func (t *TermTab) promptSecret(done <-chan struct{}, title, placeholder string, lines ...string) string {
	result := make(chan string, 1)
	var once sync.Once
	send := func(v string) { once.Do(func() { result <- v }) }

	var d dialog.Dialog
	fyne.Do(func() {
		entry := widget.NewPasswordEntry()
		entry.SetPlaceHolder(placeholder)
		content := container.NewVBox()
		for _, l := range lines {
			content.Add(widget.NewLabel(l))
		}
		content.Add(entry)

		d = dialog.NewCustomConfirm(title, "OK", "Cancel", content, func(ok bool) {
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
	})
	answer, _ := awaitAnswer(result, done, &d)
	return answer
}

// awaitAnswer waits for a dialog's answer. If done is closed first, it
// closes the dialog (*d is set on the UI goroutine) and reports no answer.
func awaitAnswer[T any](result <-chan T, done <-chan struct{}, d *dialog.Dialog) (T, bool) {
	select {
	case v := <-result:
		return v, true
	case <-done:
		fyne.Do(func() {
			if *d != nil {
				(*d).Hide()
			}
		})
		var zero T
		return zero, false
	}
}

func (t *TermTab) setStatus(connected bool) {
	text := "• Disconnected"
	if connected {
		text = "• Connected — " + t.Session.Host
	}
	fyne.Do(func() { t.statusLbl.SetText(text) })
}

// terminalInput forwards keystrokes from the terminal widget to the current
// SSH session. Input typed while disconnected is dropped.
type terminalInput struct{ t *TermTab }

func (in terminalInput) Write(p []byte) (int, error) {
	if s := in.t.session(); s != nil {
		_, _ = s.Write(p)
	}
	return len(p), nil
}

func (terminalInput) Close() error { return nil }

// maxPending is how much output termBuffer queues before writers block.
const maxPending = 4 << 20

// termBuffer carries output to the terminal widget. Writes are accepted
// without blocking — also before the widget is laid out and starts reading,
// e.g. for tabs opened in the background by auto-connect — until maxPending
// bytes are queued; then writers wait like on a real terminal.
type termBuffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	seq    []byte // unfinished escape sequence at the end of the last write
	closed bool
}

func newTermBuffer() *termBuffer {
	b := &termBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *termBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) >= maxPending && !b.closed {
		b.cond.Wait()
	}
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	b.buf = filterMediaCopy(b.buf, p, &b.seq)
	b.cond.Broadcast()
	return len(p), nil
}

// maxCSI bounds how long a sequence filterMediaCopy holds back.
const maxCSI = 64

// filterMediaCopy appends p to dst without "media copy" sequences (CSI … i,
// e.g. ESC[5i / ESC[4i). fyne-io/terminal keeps everything after ESC[5i in
// memory until ESC[4i arrives, so a server could exhaust the client's
// memory with it — and there is no printer to send it to anyway.
// *seq carries a sequence split across writes; it is held back until
// complete. All other bytes pass through unchanged.
func filterMediaCopy(dst, p []byte, seq *[]byte) []byte {
	const esc = 0x1b
	s := *seq
	for _, c := range p {
		switch {
		case len(s) == 0:
			if c == esc {
				s = append(s, c)
				continue
			}
			dst = append(dst, c)
			continue
		case len(s) == 1 && c == '[',
			len(s) > 1 && len(s) < maxCSI && c >= 0x20 && c <= 0x3f:
			// CSI introducer, parameter or intermediate byte
			s = append(s, c)
			continue
		case len(s) > 1 && c >= 0x40 && c <= 0x7e: // final byte
			if c != 'i' {
				dst = append(append(dst, s...), c)
			}
			s = s[:0]
			continue
		}
		// Not a CSI sequence (or implausibly long): pass it on unchanged.
		dst = append(dst, s...)
		s = s[:0]
		if c == esc {
			s = append(s, c)
		} else {
			dst = append(dst, c)
		}
	}
	*seq = s
	return dst
}

func (b *termBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) == 0 && !b.closed {
		b.cond.Wait()
	}
	if len(b.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	if len(b.buf) == 0 {
		b.buf = nil // release the backing array
	}
	b.cond.Broadcast()
	return n, nil
}

// Close makes Read return io.EOF once the queued data is consumed and
// releases blocked writers.
func (b *termBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.cond.Broadcast()
	return nil
}

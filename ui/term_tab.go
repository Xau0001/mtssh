package ui

import (
	"io"
	"mtssh/config"
	"mtssh/core"
	"mtssh/logger"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/fyne-io/terminal"
)

// resetScreen is written before each (re)connect and when a session ends,
// in case it ended inside a full-screen program or left modes on. It
// leaves the alternate screen (vim, less, htop), resets text attributes and
// shows the cursor. It also turns off what the next session must not
// inherit: mouse reporting (clicks would type ESC [ M … into the new
// shell), bracketed paste, newline mode (?20 in the widget) and the scroll
// region; autowrap goes back on, G0 and G1 to ASCII and SI selects G0.
// The widget knows no other mouse modes (?1002, ?1003, ?1006).
const resetScreen = "\x1b[?1049l\x1b[0m\x1b[?25h" +
	"\x1b[?1000l\x1b[?9l\x1b[?2004l\x1b[?20l\x1b[?7h\x1b[r\x1b(B\x1b)B\x0f"

// TermTab is the content for a single SSH terminal tab
type TermTab struct {
	Session   config.Session
	term      *terminal.Terminal
	output    *termBuffer // everything written here appears in the terminal
	sizes     chan terminal.Config
	input     *terminalInput // keystrokes etc. on their way to the session
	statusLbl *widget.Label
	Container fyne.CanvasObject
	win       fyne.Window
	closeOnce sync.Once

	sessMu     sync.Mutex // guards sshSession, rows, cols and closed
	sshSession *core.SSHSession
	rows, cols int  // last size reported by the terminal widget
	closed     bool // Close was called: connect() must not start a session

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
	t.input = termStartInput(sess.Label, t.sendInput)
	go func() { _ = t.term.RunWithConnection(t.input, t.output) }()
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
		t.sessMu.Lock()
		t.closed = true // also stops a connect() that is under way
		t.sessMu.Unlock()
		t.Disconnect()                 // also ends a write stuck on the session
		t.input.stop()                 // ends the input goroutine
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

// termText prepares a message for the terminal: lines end in CR LF and all
// other control characters are shown escaped (see logger.Clean).
func termText(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = logger.Clean(l)
	}
	return strings.Join(lines, "\r\n")
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
	t.sessMu.Lock()
	closed := t.closed
	t.sessMu.Unlock()
	if closed {
		return // e.g. a Reconnect that waited for the lock
	}

	// Stop any previous session before creating a new one
	t.Disconnect()
	t.setStatus(false)
	t.write(resetScreen + "\r\n[mtssh] Connecting to " + logger.Clean(t.Session.Host) + "…\r\n")

	var sess *core.SSHSession
	sess = core.NewSSHSession(
		t.Session,
		t.write,
		func(connected bool) {
			if t.session() != sess {
				return // late callback from a session that was replaced
			}
			t.setStatus(connected)
			if connected {
				return
			}
			select {
			case <-sess.Done():
				// Stopped here (Disconnect, Reconnect or the tab closed):
				// nothing to report and no reason to reconnect.
				return
			default:
			}
			// Reconnect only when the connection dropped: not when the
			// shell exited ("exit", or a server that ends it at once).
			if !sess.ConnectionLost() {
				t.write(resetScreen + "\r\n[mtssh] Session closed.\r\n")
				return
			}
			t.write(resetScreen + "\r\n[mtssh] Connection lost.\r\n")
			if t.Session.AutoConnect {
				go sess.ConnectWithRetry(3)
			}
		},
	)

	// Known-hosts: block goroutine until user decides
	// Prompts block the connecting goroutine until the user answers — or
	// until the session is disconnected (tab or window closed, Reconnect),
	// which closes the dialog and counts as "no".
	sess.HostKeyPrompt = func(host, keyType, fp string) core.HostKeyDecision {
		result := make(chan bool, 1)
		send := answerOnce(result)
		msg := "Unknown host key for:\n" + logger.Clean(host) +
			"\n\nType:        " + logger.Clean(keyType) +
			"\nFingerprint: " + fp +
			"\n\nDo you want to trust and save this host key?"
		var d dialog.Dialog
		fyne.Do(func() {
			d = dialog.NewConfirm("Unknown Host Key", msg, send, t.win)
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
			"Server "+logger.Clean(t.Session.User)+"@"+logger.Clean(t.Session.Host)+" asks:", prompt)
	}

	// Passphrase-protected SSH key: block until user enters passphrase
	sess.KeyPassphrasePrompt = func(keyPath string) string {
		return t.promptSecret(sess.Done(), "SSH Key Passphrase", "Key passphrase",
			"Key: "+logger.Clean(keyPath),
			"This key is passphrase-protected. Enter the passphrase to unlock it.")
	}

	t.sessMu.Lock()
	if t.closed {
		// Closed while we got here: Close() did not see this session.
		t.sessMu.Unlock()
		sess.Disconnect()
		return
	}
	t.sshSession = sess
	rows, cols := t.rows, t.cols
	t.sessMu.Unlock()
	sess.Resize(rows, cols) // PTY size = current widget size (ignored if not laid out yet)

	if err := sess.Connect(); err != nil {
		// The error can contain text from the server (e.g. algorithm names
		// offered before the host key is checked): no control characters.
		t.write("[mtssh] Connection failed: " + termText(err.Error()) + "\r\n")
		logger.Error(t.Session.Label, err.Error())
		t.setStatus(false)
	}
}

// promptSecret shows a modal dialog with a password entry and blocks until
// the user confirms, cancels ("") or done is closed (""). Must not be called
// on the UI goroutine.
func (t *TermTab) promptSecret(done <-chan struct{}, title, placeholder string, lines ...string) string {
	result := make(chan string, 1)
	send := answerOnce(result)

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

// answerOnce returns the function a dialog calls with its answer; it sends
// the first answer to result, which must hold one value. Fyne's Hide()
// calls the callback of a confirm dialog again (with false) after it was
// answered, e.g. when awaitAnswer closes it; a second send would block the
// UI goroutine for good.
func answerOnce[T any](result chan<- T) func(T) {
	var once sync.Once
	return func(v T) { once.Do(func() { result <- v }) }
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
		text = "• Connected — " + logger.Clean(t.Session.Host)
	}
	fyne.Do(func() { t.statusLbl.SetText(text) })
}

// sendInput writes input from the terminal to the current SSH session.
// Input typed while disconnected is dropped.
func (t *TermTab) sendInput(p []byte) {
	if s := t.session(); s != nil {
		_, _ = s.Write(p)
	}
}

// Limits for input waiting to be sent to the SSH session.
const (
	termInputChunks   = 256     // writes
	termInputMaxBytes = 1 << 20 // bytes; a single larger write is allowed
)

// terminalInput carries input from the terminal widget (keystrokes, paste,
// mouse and device attribute reports) to a goroutine that sends it on.
// Writing to the SSH session blocks while the server does not read (its
// receive window is full); on the UI goroutine that froze the whole app.
// Write never blocks: once termInputChunks or termInputMaxBytes are queued,
// further input is dropped, like typing into a hung connection.
type terminalInput struct {
	label string       // session label, for the log
	send  func([]byte) // sends one chunk; replaced in tests
	queue chan []byte
	quit  chan struct{} // closed by stop
	done  chan struct{} // closed when the goroutine has ended
	once  sync.Once

	mu       sync.Mutex // guards pending and dropping
	pending  int        // bytes in queue
	dropping bool       // an overflow was logged; reset once input fits again
}

// termStartInput starts the goroutine that passes queued input to send.
func termStartInput(label string, send func([]byte)) *terminalInput {
	in := &terminalInput{
		label: label,
		send:  send,
		queue: make(chan []byte, termInputChunks),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go in.run()
	return in
}

// Write queues a copy of p (the widget may reuse its buffer).
func (in *terminalInput) Write(p []byte) (int, error) {
	select {
	case <-in.quit:
		return len(p), nil // tab closed
	default:
	}
	in.mu.Lock()
	queued := false
	if in.pending == 0 || in.pending+len(p) <= termInputMaxBytes {
		select {
		case in.queue <- append([]byte(nil), p...):
			in.pending += len(p)
			queued = true
		default: // termInputChunks writes queued
		}
	}
	report := !queued && !in.dropping // once per overflow
	in.dropping = !queued
	in.mu.Unlock()
	if report {
		logger.Error(in.label, "terminal input dropped: the server does not accept input")
	}
	return len(p), nil
}

// Close is called by the terminal widget when its connection ends. The
// goroutine keeps running until stop, like the tab.
func (in *terminalInput) Close() error { return nil }

// stop ends the goroutine once a send in progress has returned; input
// still queued may be dropped.
func (in *terminalInput) stop() {
	in.once.Do(func() { close(in.quit) })
}

func (in *terminalInput) run() {
	defer close(in.done)
	for {
		select {
		case <-in.quit:
			return
		case p := <-in.queue:
			in.mu.Lock()
			in.pending -= len(p)
			in.mu.Unlock()
			in.send(p)
		}
	}
}

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
	filter outputFilter // sanitizes everything written
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
	b.buf = b.filter.write(b.buf, p)
	b.cond.Broadcast()
	return len(p), nil
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

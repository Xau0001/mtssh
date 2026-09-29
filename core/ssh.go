package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mtssh/config"
	"mtssh/logger"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// retryDelay is the pause before each automatic reconnect attempt
// (variable so tests can shorten it).
var retryDelay = 3 * time.Second

// Automatic reconnects are limited to maxAutoReconnects within
// reconnectWindow, so a connection that keeps dropping right after login
// does not log in over and over.
var (
	maxAutoReconnects = 5
	reconnectWindow   = 10 * time.Minute
)

// dialTimeout bounds the TCP connect. handshakeTimeout bounds everything
// after it until the shell runs (banner, key exchange, authentication,
// session and PTY setup); time spent in a prompt does not count.
// Variables so tests can shorten them.
var (
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 30 * time.Second
)

// maxKeyFileSize bounds private key files; real ones are a few KB.
const maxKeyFileSize = 64 << 10

// Keepalive: ping the server every keepaliveInterval and drop the connection
// after keepaliveMaxMissed unanswered pings (like OpenSSH's ServerAlive*).
// Variables so tests can shorten them.
var (
	keepaliveInterval  = 30 * time.Second
	keepaliveMaxMissed = 3
)

// ErrCancelled is returned (wrapped) when the user cancels a password or
// passphrase prompt.
var ErrCancelled = errors.New("cancelled by user")

// ErrAuthFailed is returned (wrapped) when authentication fails or cannot
// be attempted (e.g. unreadable key). Retrying would fail the same way.
var ErrAuthFailed = errors.New("authentication failed")

// authError marks err as an authentication failure without changing its text.
type authError struct{ err error }

func (e authError) Error() string   { return e.err.Error() }
func (e authError) Unwrap() []error { return []error{e.err, ErrAuthFailed} }

// OutputCallback receives terminal output chunks
type OutputCallback func(line string)

// KeyPassphrasePrompt is called when a private key is passphrase-protected.
// Should block until the user provides input. Return "" to abort.
type KeyPassphrasePrompt func(keyPath string) string

// PasswordPrompt asks the user for a secret the server wants (password,
// one-time code), showing the server's prompt text. Should block until the
// user provides input. Return "" to abort.
type PasswordPrompt func(prompt string) string

// SSHSession wraps a live SSH connection + shell
type SSHSession struct {
	cfg                 config.Session
	conn                net.Conn // connection being set up; closed by Disconnect
	client              *ssh.Client
	session             *ssh.Session
	stdin               io.WriteCloser
	mu                  sync.Mutex
	winMu               sync.Mutex // serializes window-change requests
	running             bool
	lost                bool          // the last session ended without an exit status
	reconnects          []time.Time   // start times of automatic reconnects
	rows, cols          int           // terminal size requested for the PTY
	stopCh              chan struct{} // closed by Disconnect to cancel reconnect loops
	OnOutput            OutputCallback
	OnStatus            func(connected bool)
	HostKeyPrompt       HostKeyPrompt
	KeyPassphrasePrompt KeyPassphrasePrompt
	PasswordPrompt      PasswordPrompt
}

// NewSSHSession creates a new session wrapper (does not connect yet)
func NewSSHSession(cfg config.Session, onOutput OutputCallback, onStatus func(bool)) *SSHSession {
	return &SSHSession{
		cfg:      cfg,
		OnOutput: onOutput,
		OnStatus: onStatus,
		rows:     24,
		cols:     80,
		stopCh:   make(chan struct{}),
	}
}

// Done is closed when Disconnect is called. Prompts shown for this session
// should give up when it is closed.
func (s *SSHSession) Done() <-chan struct{} {
	return s.stopCh
}

// Client returns the underlying *ssh.Client (needed for SFTP).
func (s *SSHSession) Client() *ssh.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// Connect opens the SSH connection and starts the shell
func (s *SSHSession) Connect() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("already connected")
	}
	s.mu.Unlock()

	auth, err := s.buildAuth()
	if err != nil {
		return fmt.Errorf("auth error: %w", authError{err})
	}

	prompt := s.HostKeyPrompt
	if prompt == nil {
		// Without a way to ask the user, unknown hosts must not be trusted.
		prompt = func(host, keyType, fp string) HostKeyDecision { return HostKeyReject }
	}
	hostKeyPrompt := func(host, keyType, fp string) HostKeyDecision {
		defer s.pauseDeadline()()
		return prompt(host, keyType, fp)
	}

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	algos := knownHostKeyAlgorithms(addr)
	if algos == nil {
		algos = unknownHostKeyAlgorithms
	}
	sshCfg := &ssh.ClientConfig{
		User:              s.cfg.User,
		Auth:              auth,
		HostKeyCallback:   BuildHostKeyCallback(hostKeyPrompt),
		HostKeyAlgorithms: algos,
	}

	// Do not hold s.mu while connecting: it may take long, and the UI needs
	// IsRunning()/Client()/Disconnect() to stay responsive. Disconnect()
	// aborts the connect by cancelling the dial or closing s.conn.
	conn, err := s.dial(addr)
	if err != nil {
		return err
	}
	defer func() {
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.mu.Unlock()
	}()
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, sshCfg)
	if err != nil {
		conn.Close()
		if s.isStopped() {
			return errors.New("connection cancelled")
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			err = authError{err}
		}
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	client := ssh.NewClient(c, chans, reqs)
	// Closing the client also closes every session opened on it.
	fail := func(step string, err error) error {
		client.Close()
		if s.isStopped() {
			return errors.New("connection cancelled")
		}
		return fmt.Errorf("%s: %w", step, err)
	}

	sess, err := client.NewSession()
	if err != nil {
		return fail("new session", err)
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	s.mu.Lock()
	rows, cols := s.rows, s.cols
	s.mu.Unlock()
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		return fail("pty request", err)
	}

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return fail("stdout pipe", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return fail("stderr pipe", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return fail("stdin pipe", err)
	}

	if err := sess.Shell(); err != nil {
		return fail("shell", err)
	}
	// Set up: from now on the keepalive watches the connection.
	_ = conn.SetDeadline(time.Time{})

	s.mu.Lock()
	if s.stopped() {
		// Disconnect() ran while we were connecting (e.g. during a
		// reconnect loop) — don't resurrect a session the user closed.
		s.mu.Unlock()
		client.Close()
		return errors.New("connection cancelled")
	}
	s.client = client
	s.session = sess
	s.stdin = stdin
	s.running = true
	s.lost = false
	resized := s.rows != rows || s.cols != cols
	s.mu.Unlock()
	if resized {
		s.sendSize() // the terminal was resized while we were connecting
	}

	logger.Info(s.cfg.Label, "connected to "+addr)
	if s.OnStatus != nil {
		s.OnStatus(true)
	}

	go s.streamOutput(stdout)
	go s.streamOutput(stderr)

	done := make(chan struct{})
	go keepalive(client, done, keepaliveInterval, keepaliveMaxMissed)

	go func() {
		err := sess.Wait()
		close(done)
		// The shell may exit while the TCP connection stays up (e.g. the
		// user typed "exit"); close the client so it does not leak.
		client.Close()
		// A shell that exits sends its exit status; without one, the
		// connection itself went away (network, keepalive, server).
		var missing *ssh.ExitMissingError
		lost := errors.As(err, &missing)
		s.mu.Lock()
		if s.session == sess {
			s.running = false
			s.lost = lost
		}
		s.mu.Unlock()
		if lost {
			logger.Info(s.cfg.Label, "connection lost")
		} else {
			logger.Info(s.cfg.Label, "session ended")
		}
		if s.OnStatus != nil {
			s.OnStatus(false)
		}
	}()

	return nil
}

// dial opens the TCP connection and registers it in s.conn, so Disconnect
// can abort the rest of the setup. Disconnect also cancels the dial itself.
func (s *SSHSession) dial(addr string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if s.isStopped() {
			return nil, errors.New("connection cancelled")
		}
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped() {
		conn.Close()
		return nil, errors.New("connection cancelled")
	}
	s.conn = conn
	return conn, nil
}

// pauseDeadline lifts the handshake deadline while the user answers a
// prompt; call the returned function to restart it afterwards.
func (s *SSHSession) pauseDeadline() (resume func()) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return func() {}
	}
	_ = conn.SetDeadline(time.Time{})
	return func() { _ = conn.SetDeadline(time.Now().Add(handshakeTimeout)) }
}

// ConnectionLost reports whether the last session ended because the
// connection dropped, rather than the shell exiting (e.g. "exit").
func (s *SSHSession) ConnectionLost() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lost
}

// ConnectWithRetry waits retryDelay before each of up to maxRetries attempts.
// Stops immediately if Disconnect() is called, and does nothing once
// maxAutoReconnects runs have started within reconnectWindow.
func (s *SSHSession) ConnectWithRetry(maxRetries int) {
	if !s.allowReconnect() {
		logger.Error(s.cfg.Label, "automatic reconnect limit reached")
		s.output(fmt.Sprintf("[mtssh] The connection dropped %d times within %s — not reconnecting automatically. Use Reconnect.\r\n",
			maxAutoReconnects, reconnectWindow))
		return
	}
	for i := 1; i <= maxRetries; i++ {
		if s.isStopped() {
			return // user disconnected — nothing to announce
		}
		s.output(fmt.Sprintf("[mtssh] Reconnecting in %s (attempt %d/%d)…\r\n", retryDelay, i, maxRetries))
		select {
		case <-s.stopCh:
			return
		case <-time.After(retryDelay):
		}

		logger.Info(s.cfg.Label, fmt.Sprintf("connect attempt %d/%d", i, maxRetries))
		err := s.Connect()
		if err == nil {
			return
		}
		if s.isStopped() {
			return
		}
		logger.Error(s.cfg.Label, err.Error())
		s.output(fmt.Sprintf("[mtssh] Reconnect attempt %d/%d failed: %s\r\n", i, maxRetries, logger.Clean(err.Error())))
		if errors.Is(err, ErrCancelled) || errors.Is(err, ErrHostKeyRejected) ||
			errors.Is(err, ErrHostKeyMismatch) || errors.Is(err, ErrAuthFailed) {
			// Retrying would only ask the user again, keep talking to an
			// impostor, or fail to log in again (and maybe lock the account).
			s.output("[mtssh] Reconnect stopped.\r\n")
			return
		}
	}
	logger.Error(s.cfg.Label, "all reconnect attempts failed")
	s.output("[mtssh] Could not reconnect. Please reconnect manually.\r\n")
}

// allowReconnect records an automatic reconnect run, unless
// maxAutoReconnects already started within reconnectWindow.
func (s *SSHSession) allowReconnect() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	recent := s.reconnects[:0]
	for _, t := range s.reconnects {
		if now.Sub(t) < reconnectWindow {
			recent = append(recent, t)
		}
	}
	s.reconnects = recent
	if len(recent) >= maxAutoReconnects {
		return false
	}
	s.reconnects = append(s.reconnects, now)
	return true
}

// Write sends raw input (keystrokes, pasted text) to the remote shell.
func (s *SSHSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	stdin := s.stdin
	running := s.running
	s.mu.Unlock()
	if !running || stdin == nil {
		return 0, fmt.Errorf("session not active")
	}
	// Write without holding s.mu: it can block while the remote window is
	// full, and Disconnect()/IsRunning() must stay responsive meanwhile.
	return stdin.Write(p)
}

// Resize sets the terminal size. It is used for the PTY of the next
// connection and, while connected, sent to the server as window-change.
func (s *SSHSession) Resize(rows, cols int) {
	if rows <= 0 || cols <= 0 {
		return
	}
	s.mu.Lock()
	if rows == s.rows && cols == s.cols {
		s.mu.Unlock()
		return
	}
	s.rows, s.cols = rows, cols
	s.mu.Unlock()
	s.sendSize()
}

// sendSize sends the current terminal size to the server. winMu keeps the
// requests in order and each call reads the latest size, so the server ends
// up with the last one. s.mu is not held while sending: the write can block
// on a stalled connection, and Disconnect() must not wait for it.
func (s *SSHSession) sendSize() {
	s.winMu.Lock()
	defer s.winMu.Unlock()
	s.mu.Lock()
	sess, running, rows, cols := s.session, s.running, s.rows, s.cols
	s.mu.Unlock()
	if running && sess != nil {
		_ = sess.WindowChange(rows, cols)
	}
}

// keepalive pings the server so dead connections (suspended laptop, NAT
// timeout, pulled cable) are noticed: the client is closed, which ends the
// session and triggers auto-reconnect. It returns once done is closed.
func keepalive(client *ssh.Client, done <-chan struct{}, interval time.Duration, maxMissed int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	missed := 0
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		reply := make(chan error, 1)
		go func() {
			// OpenSSH answers with a failure message; any answer means alive.
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			reply <- err
		}()
		select {
		case <-done:
			return
		case err := <-reply:
			if err != nil {
				client.Close()
				return
			}
			missed = 0
		case <-time.After(interval):
			if missed++; missed >= maxMissed {
				client.Close()
				return
			}
		}
	}
}

// Disconnect closes the session and client, aborts a connect in progress
// and cancels any pending reconnect loop.
func (s *SSHSession) Disconnect() {
	s.mu.Lock()
	if !s.stopped() {
		close(s.stopCh)
	}
	conn, client := s.conn, s.client
	s.running = false
	s.mu.Unlock()
	// Close outside s.mu: closing can block behind a write stuck on a full
	// connection. Closing the client closes its sessions and connection.
	if client != nil {
		client.Close()
	}
	if conn != nil {
		conn.Close()
	}
	logger.Info(s.cfg.Label, "disconnected")
}

// IsRunning returns whether the session is active
func (s *SSHSession) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// stopped reports whether Disconnect has been called.
func (s *SSHSession) stopped() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

func (s *SSHSession) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped()
}

func (s *SSHSession) output(msg string) {
	if s.OnOutput != nil {
		s.OnOutput(msg)
	}
}

func (s *SSHSession) buildAuth() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	// ask shows the password prompt. Once the user cancels, it stops asking
	// for the rest of this connection attempt — x/crypto would otherwise go
	// on to the next method and prompt again right away.
	cancelled := false
	ask := func(prompt string) (string, error) {
		if cancelled || s.PasswordPrompt == nil {
			return "", ErrCancelled
		}
		defer s.pauseDeadline()()
		answer := s.PasswordPrompt(prompt)
		if answer == "" {
			cancelled = true
			return "", ErrCancelled
		}
		return answer, nil
	}

	if s.cfg.UseKey && s.cfg.KeyPath != "" {
		keyPath := expandHome(s.cfg.KeyPath)
		keyBytes, err := readKeyFile(keyPath)
		if err != nil {
			return nil, err
		}

		// Try parsing without passphrase first
		signer, err := ssh.ParsePrivateKey(keyBytes)
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			passphrase := ""
			if s.KeyPassphrasePrompt != nil {
				passphrase = s.KeyPassphrasePrompt(keyPath)
			}
			if passphrase == "" {
				return nil, fmt.Errorf("key %s is passphrase-protected: %w", keyPath, ErrCancelled)
			}
			signer, err = ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(passphrase))
			if err != nil {
				return nil, fmt.Errorf("wrong passphrase for key %s: %w", keyPath, err)
			}
		} else if err != nil {
			return nil, fmt.Errorf("parse key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if s.cfg.Password != "" {
		methods = append(methods, ssh.Password(s.cfg.Password))
	} else if s.PasswordPrompt != nil {
		methods = append(methods, ssh.PasswordCallback(func() (string, error) {
			return ask("Password:")
		}))
	}

	// Many servers (PAM) accept passwords only via keyboard-interactive,
	// which is also used for one-time codes.
	if s.cfg.Password != "" || s.PasswordPrompt != nil {
		stored := s.cfg.Password
		methods = append(methods, ssh.KeyboardInteractive(
			func(_, instruction string, questions []string, echos []bool) ([]string, error) {
				return answerQuestions(&stored, ask, instruction, questions, echos)
			}))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no authentication method configured")
	}
	return methods, nil
}

// answerQuestions answers keyboard-interactive prompts: the first single
// hidden question (normally "Password:") with the stored password, anything
// else — e.g. a one-time code asked next — by asking the user. *stored is
// cleared once used so it is not sent as the answer to a later question.
func answerQuestions(stored *string, ask func(string) (string, error), instruction string, questions []string, echos []bool) ([]string, error) {
	answers := make([]string, len(questions))
	for i, q := range questions {
		if len(questions) == 1 && !echos[i] && *stored != "" {
			answers[i], *stored = *stored, ""
			continue
		}
		answer, err := ask(strings.TrimSpace(instruction + "\n" + q))
		if err != nil {
			return nil, err
		}
		answers[i] = answer
	}
	return answers, nil
}

// CheckKeyPath rejects key paths MTSSH will not open: on Windows, UNC and
// device paths (\\host\share\…, \\?\…) — opening one sends the user's
// NTLM credentials to that host. It only looks at the path.
func CheckKeyPath(path string) error {
	if runtime.GOOS == "windows" && (strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//")) {
		return fmt.Errorf("key %s: network and device paths are not allowed", path)
	}
	return nil
}

// readKeyFile reads a private key: a regular file of at most
// maxKeyFileSize bytes. Anything else (a device such as /dev/zero, a FIFO,
// a huge file) could hang or exhaust memory, e.g. via an imported session.
func readKeyFile(path string) ([]byte, error) {
	if err := CheckKeyPath(path); err != nil {
		return nil, err
	}
	// Stat before opening: opening a FIFO would block.
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("key %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("key %s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	if len(data) > maxKeyFileSize {
		return nil, fmt.Errorf("key %s is larger than %d KB — not a private key", path, maxKeyFileSize>>10)
	}
	return data, nil
}

// expandHome resolves a leading "~" so paths like "~/.ssh/id_ed25519"
// (as suggested in the session dialog) work.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}

func (s *SSHSession) streamOutput(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.output(string(buf[:n]))
		}
		if err != nil {
			break
		}
	}
}

package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mtssh/config"
	"mtssh/logger"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

// dialTimeout bounds the TCP connect. handshakeTimeout bounds the banner
// and key exchange until the host key is trusted; authTimeout then bounds
// the rest until the shell runs (authentication, session and PTY setup),
// restarting after each prompt: the server may wait on the user too (push
// approval, slow PAM). Time spent in a prompt does not count.
// Variables so tests can shorten them.
var (
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 30 * time.Second
	authTimeout      = 2 * time.Minute
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

// ErrInvalidHost is returned (wrapped) for a host that is not a plain host
// name or IP address.
var ErrInvalidHost = errors.New("invalid host")

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

	// The host ends up in known_hosts: check it before anything else.
	host, err := validHost(s.cfg.Host)
	if err != nil {
		return err
	}
	port := strconv.Itoa(s.cfg.Port)
	// Dial the host as entered (a trailing dot changes how DNS resolves
	// it), but check and store its key under the normalized name.
	dialAddr := net.JoinHostPort(host, port)
	addr := net.JoinHostPort(knownHostName(host), port)
	legacyAddr := "" // how older versions stored the host, if different
	if dialAddr != addr {
		legacyAddr = dialAddr
	}

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

	algos := knownHostKeyAlgorithms(addr)
	if algos == nil && legacyAddr != "" {
		algos = knownHostKeyAlgorithms(legacyAddr)
	}
	if algos == nil {
		algos = unknownHostKeyAlgorithms
	}

	// Do not hold s.mu while connecting: it may take long, and the UI needs
	// IsRunning()/Client()/Disconnect() to stay responsive. Disconnect()
	// aborts the connect by cancelling the dial or closing s.conn.
	conn, err := s.dial(dialAddr)
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

	verifyHostKey := hostKeyCallback(hostKeyPrompt, legacyAddr)
	sshCfg := &ssh.ClientConfig{
		User: s.cfg.User,
		Auth: auth,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if err := verifyHostKey(hostname, remote, key); err != nil {
				return err
			}
			// Trusted: authentication may now wait on the user or the
			// server, so allow it more time.
			s.extendDeadline(conn, authTimeout)
			return nil
		},
		HostKeyAlgorithms: algos,
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, sshCfg)
	if err != nil {
		conn.Close()
		if s.isStopped() {
			return errors.New("connection cancelled")
		}
		if isAuthFailure(err) {
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

	s.mu.Lock()
	// Set up: from now on the keepalive watches the connection. Once s.conn
	// no longer points to it, nothing sets a deadline again (see
	// extendDeadline; re-keys run the host key callback).
	if s.conn == conn {
		s.conn = nil
	}
	_ = conn.SetDeadline(time.Time{})
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
		// Closing it ourselves (Disconnect) looks the same, but is no loss.
		var missing *ssh.ExitMissingError
		s.mu.Lock()
		lost := errors.As(err, &missing) && !s.stopped()
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

// pauseDeadline lifts the setup deadline while the user answers a prompt;
// call the returned function to restart it (with authTimeout) afterwards.
func (s *SSHSession) pauseDeadline() (resume func()) {
	s.mu.Lock()
	conn := s.conn
	if conn != nil {
		_ = conn.SetDeadline(time.Time{})
	}
	s.mu.Unlock()
	if conn == nil {
		return func() {}
	}
	return func() { s.extendDeadline(conn, authTimeout) }
}

// extendDeadline gives conn d more time to finish its setup. It does nothing
// once Connect is done with conn: the running session has no deadline.
func (s *SSHSession) extendDeadline(conn net.Conn, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == conn {
		_ = conn.SetDeadline(time.Now().Add(d))
	}
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
	if s.isStopped() {
		return // user disconnected — don't use up the reconnect budget
	}
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
			errors.Is(err, ErrHostKeyMismatch) || errors.Is(err, ErrAuthFailed) ||
			errors.Is(err, ErrInvalidHost) {
			// Retrying would only ask the user again, keep talking to an
			// impostor, fail to log in again (and maybe lock the account),
			// or reject the same host name again.
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
		answer, err := ask(strings.TrimSpace(promptText(instruction) + "\n" + promptText(q)))
		if err != nil {
			return nil, err
		}
		answers[i] = answer
	}
	return answers, nil
}

// Limits for server-supplied keyboard-interactive text (instruction and
// each question) shown in the password dialog.
const (
	maxPromptBytes = 1024
	maxPromptLines = 10
)

// promptText makes server-supplied prompt text safe to show: at most
// maxPromptBytes and maxPromptLines, control and invisible characters (e.g.
// bidi overrides) escaped. Lines are cleaned one by one because
// logger.Clean escapes newlines.
func promptText(s string) string {
	if len(s) > maxPromptBytes {
		n := maxPromptBytes
		for i := 0; i < utf8.UTFMax-1 && n > 0 && !utf8.RuneStart(s[n]); i++ {
			n-- // don't cut a character in half
		}
		s = s[:n] + "…"
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxPromptLines {
		lines = lines[:maxPromptLines]
		lines[maxPromptLines-1] += "…"
	}
	for i, l := range lines {
		lines[i] = logger.Clean(strings.TrimSuffix(l, "\r"))
	}
	return strings.Join(lines, "\n")
}

// isAuthFailure reports errors meaning the server refused the login:
// retrying with the same credentials would fail again and may lock the
// account (e.g. sshd's MaxAuthTries disconnect).
func isAuthFailure(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(strings.ToLower(msg), "too many authentication failures") ||
		strings.Contains(msg, "ssh: disconnect, reason 14:") // no more auth methods available
}

// CheckHost accepts a plain host name ("srv.example", with an optional
// trailing dot) or an IP address ("10.0.0.1", "::1", "fe80::1%eth0"),
// optionally in brackets. Anything else is rejected: user@host, host:port,
// and host lists or patterns such as "*,a.example", which would make a key
// stored in known_hosts trusted for other hosts too.
func CheckHost(host string) error {
	_, err := validHost(host)
	return err
}

// validHost checks host like CheckHost and returns it without surrounding
// spaces and brackets.
func validHost(host string) (string, error) {
	h := strings.TrimSpace(host)
	if len(h) >= 2 && h[0] == '[' && h[len(h)-1] == ']' {
		h = h[1 : len(h)-1]
	}
	if len(h) < 1 || len(h) > 253 || !isIPHost(h) && !isDNSName(h) {
		return "", fmt.Errorf("%w: enter only the host name or IP address — user and port have their own fields", ErrInvalidHost)
	}
	return h, nil
}

// isIPHost reports whether h is an IP address. A zone (fe80::1%eth0) may
// only contain letters, digits, '_', '.' and '-'.
func isIPHost(h string) bool {
	ip, err := netip.ParseAddr(h)
	if err != nil {
		return false
	}
	zone := ip.Zone()
	if len(zone) > 64 {
		return false
	}
	for i := 0; i < len(zone); i++ {
		if !hostNameByte(zone[i]) && zone[i] != '.' {
			return false
		}
	}
	return true
}

// isDNSName reports whether h is an ASCII host name: labels of letters,
// digits, '_' and '-' (1–63 bytes, not starting with '-') separated by
// single dots, with an optional trailing dot.
func isDNSName(h string) bool {
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			if !hostNameByte(label[i]) {
				return false
			}
		}
	}
	return true
}

func hostNameByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-'
}

// knownHostName is how a host checked by validHost is looked up and stored
// in known_hosts: lower case, without a trailing dot, like OpenSSH does, so
// "SRV.example." and "srv.example" share one entry.
func knownHostName(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// CheckKeyPath rejects key paths MTSSH will not open: on Windows, UNC and
// device paths (\\host\share\…, /\host\…, \\?\…, \??\UNC\…) — opening one
// sends the user's NTLM credentials to that host. It only looks at the path.
func CheckKeyPath(path string) error {
	if runtime.GOOS == "windows" && !isLocalWindowsPath(path) {
		return fmt.Errorf("key %s: network and device paths are not allowed", path)
	}
	return nil
}

// isLocalWindowsPath reports whether p, read as a Windows path, has no
// volume other than a drive letter: no UNC path (two leading separators of
// either kind), no device path (\\?\, \\.\, \??\). It is plain string logic
// so it can be tested on every OS; filepath.VolumeName only knows Windows
// paths on Windows.
func isLocalWindowsPath(p string) bool {
	sep := func(c byte) bool { return c == '\\' || c == '/' }
	switch {
	case len(p) >= 2 && sep(p[0]) && sep(p[1]):
		return false
	case len(p) >= 3 && sep(p[0]) && p[1] == '?' && p[2] == '?':
		return false
	case len(p) >= 2 && p[1] == ':':
		return isDriveVolume(p[:2])
	}
	return true
}

// isDriveVolume reports whether v is a drive letter volume such as "C:".
func isDriveVolume(v string) bool {
	return len(v) == 2 && v[1] == ':' && ('A' <= v[0] && v[0] <= 'Z' || 'a' <= v[0] && v[0] <= 'z')
}

// readKeyFile reads a private key: a regular file of at most
// maxKeyFileSize bytes. Anything else (a device such as /dev/zero, a FIFO,
// a huge file) could hang or exhaust memory, e.g. via an imported session.
func readKeyFile(path string) ([]byte, error) {
	if err := CheckKeyPath(path); err != nil {
		return nil, err
	}
	name := path
	if runtime.GOOS == "windows" {
		// Check what is opened: the absolute path, which may still point
		// to a network share (e.g. a home directory on one).
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("read key %s: %w", path, err)
		}
		if v := filepath.VolumeName(abs); !isLocalWindowsPath(abs) || v != "" && !isDriveVolume(v) {
			return nil, fmt.Errorf("key %s: network and device paths are not allowed", path)
		}
		name = abs
	}
	// A quick look first, so a device (e.g. a serial port, which may react
	// to being opened) is never opened. The file could still be swapped
	// before the open, so it is opened without blocking (a FIFO would wait
	// for a writer) and checked again below.
	if fi, err := os.Stat(name); err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("key %s is not a regular file", path)
	}
	f, err := openKeyFile(name)
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

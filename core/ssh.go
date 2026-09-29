package core

import (
	"errors"
	"fmt"
	"io"
	"mtssh/config"
	"mtssh/logger"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// retryDelay is the pause before each automatic reconnect attempt.
const retryDelay = 3 * time.Second

// Keepalive: ping the server every keepaliveInterval and drop the connection
// after keepaliveMaxMissed unanswered pings (like OpenSSH's ServerAlive*).
// Variables so tests can shorten them.
var (
	keepaliveInterval  = 30 * time.Second
	keepaliveMaxMissed = 3
)

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
	client              *ssh.Client
	session             *ssh.Session
	stdin               io.WriteCloser
	mu                  sync.Mutex
	winMu               sync.Mutex // serializes window-change requests
	running             bool
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
		return fmt.Errorf("auth error: %w", err)
	}

	prompt := s.HostKeyPrompt
	if prompt == nil {
		// Without a way to ask the user, unknown hosts must not be trusted.
		prompt = func(host, keyType, fp string) HostKeyDecision { return HostKeyReject }
	}

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	sshCfg := &ssh.ClientConfig{
		User:              s.cfg.User,
		Auth:              auth,
		HostKeyCallback:   BuildHostKeyCallback(prompt),
		HostKeyAlgorithms: knownHostKeyAlgorithms(addr),
		Timeout:           10 * time.Second,
	}

	// Do not hold s.mu during Dial — it may block for the full Timeout and
	// the UI needs IsRunning()/Client()/Disconnect() to stay responsive.
	client, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	// Closing the client also closes every session opened on it.
	fail := func(step string, err error) error {
		client.Close()
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
	if s.stopped() {
		// Disconnect() ran while we were dialing (e.g. during a reconnect
		// loop) — don't resurrect a session the user already closed.
		s.mu.Unlock()
		client.Close()
		return errors.New("connection cancelled")
	}
	s.client = client
	s.session = sess
	s.stdin = stdin
	s.running = true
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
		sess.Wait()
		close(done)
		// The shell may exit while the TCP connection stays up (e.g. the
		// user typed "exit"); close the client so it does not leak.
		client.Close()
		s.mu.Lock()
		if s.session == sess {
			s.running = false
		}
		s.mu.Unlock()
		logger.Info(s.cfg.Label, "session ended")
		if s.OnStatus != nil {
			s.OnStatus(false)
		}
	}()

	return nil
}

// ConnectWithRetry waits retryDelay before each of up to maxRetries attempts.
// Stops immediately if Disconnect() is called.
func (s *SSHSession) ConnectWithRetry(maxRetries int) {
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
		s.output(fmt.Sprintf("[mtssh] Reconnect attempt %d/%d failed: %s\r\n", i, maxRetries, err))
	}
	logger.Error(s.cfg.Label, "all reconnect attempts failed")
	s.output("[mtssh] Could not reconnect. Please reconnect manually.\r\n")
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

// Disconnect closes the session and client, and cancels any pending reconnect loop.
func (s *SSHSession) Disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped() {
		close(s.stopCh)
	}
	if s.session != nil {
		s.session.Close()
	}
	if s.client != nil {
		s.client.Close()
	}
	s.running = false
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

	if s.cfg.UseKey && s.cfg.KeyPath != "" {
		keyPath := expandHome(s.cfg.KeyPath)
		keyBytes, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read key %s: %w", keyPath, err)
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
				return nil, fmt.Errorf("key %s is passphrase-protected but no passphrase provided", keyPath)
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
			pw := s.PasswordPrompt("Password:")
			if pw == "" {
				return "", fmt.Errorf("password entry cancelled")
			}
			return pw, nil
		}))
	}

	// Many servers (PAM) accept passwords only via keyboard-interactive,
	// which is also used for one-time codes.
	if s.cfg.Password != "" || s.PasswordPrompt != nil {
		methods = append(methods, ssh.KeyboardInteractive(s.answerQuestions))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no authentication method configured")
	}
	return methods, nil
}

// answerQuestions answers keyboard-interactive prompts: the usual single
// hidden "Password:" question with the stored password, anything else by
// asking the user.
func (s *SSHSession) answerQuestions(_, instruction string, questions []string, echos []bool) ([]string, error) {
	answers := make([]string, len(questions))
	for i, q := range questions {
		if len(questions) == 1 && !echos[i] && s.cfg.Password != "" {
			answers[i] = s.cfg.Password
			continue
		}
		if s.PasswordPrompt == nil {
			return nil, errors.New("server asks for input, but no prompt is available")
		}
		answer := s.PasswordPrompt(strings.TrimSpace(instruction + "\n" + q))
		if answer == "" {
			return nil, errors.New("authentication cancelled")
		}
		answers[i] = answer
	}
	return answers, nil
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

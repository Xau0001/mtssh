package core

import (
	"errors"
	"fmt"
	"io"
	"mtssh/config"
	"mtssh/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// retryDelay is the pause before each automatic reconnect attempt.
const retryDelay = 3 * time.Second

// OutputCallback receives terminal output chunks
type OutputCallback func(line string)

// KeyPassphrasePrompt is called when a private key is passphrase-protected.
// Should block until the user provides input. Return "" to abort.
type KeyPassphrasePrompt func(keyPath string) string

// PasswordPrompt is called when no password or key is configured.
// Should block until the user provides input. Return "" to abort.
type PasswordPrompt func() string

// SSHSession wraps a live SSH connection + shell
type SSHSession struct {
	cfg                 config.Session
	client              *ssh.Client
	session             *ssh.Session
	stdin               io.WriteCloser
	mu                  sync.Mutex
	running             bool
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

	sshCfg := &ssh.ClientConfig{
		User:            s.cfg.User,
		Auth:            auth,
		HostKeyCallback: BuildHostKeyCallback(prompt),
		Timeout:         10 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
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
	if err := sess.RequestPty("xterm-256color", 40, 120, modes); err != nil {
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
	s.mu.Unlock()

	logger.Info(s.cfg.Label, "connected to "+addr)
	if s.OnStatus != nil {
		s.OnStatus(true)
	}

	go s.streamOutput(stdout)
	go s.streamOutput(stderr)

	go func() {
		sess.Wait()
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

// SendCommand writes a command to the shell stdin
func (s *SSHSession) SendCommand(cmd string) error {
	s.mu.Lock()
	stdin := s.stdin
	running := s.running
	s.mu.Unlock()
	if !running || stdin == nil {
		return fmt.Errorf("session not active")
	}
	// Write without holding s.mu: it can block while the remote window is
	// full, and Disconnect()/IsRunning() must stay responsive meanwhile.
	_, err := io.WriteString(stdin, cmd)
	return err
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
	}

	if len(methods) == 0 {
		if s.PasswordPrompt == nil {
			return nil, fmt.Errorf("no authentication method configured")
		}
		methods = append(methods, ssh.PasswordCallback(func() (string, error) {
			pw := s.PasswordPrompt()
			if pw == "" {
				return "", fmt.Errorf("password entry cancelled")
			}
			return pw, nil
		}))
	}
	return methods, nil
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

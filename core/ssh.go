package core

import (
	"fmt"
	"io"
	"mtputty/config"
	"mtputty/logger"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// OutputCallback receives terminal output chunks
type OutputCallback func(line string)

// KeyPassphrasePrompt is called when a private key is passphrase-protected.
// Should block until the user provides input. Return "" to abort.
type KeyPassphrasePrompt func(keyPath string) string

// SSHSession wraps a live SSH connection + shell
type SSHSession struct {
	cfg                 config.Session
	client              *ssh.Client
	session             *ssh.Session
	stdin               io.WriteCloser
	mu                  sync.Mutex
	running             bool
	OnOutput            OutputCallback
	OnStatus            func(connected bool)
	HostKeyPrompt       HostKeyPrompt
	KeyPassphrasePrompt KeyPassphrasePrompt
}

// NewSSHSession creates a new session wrapper (does not connect yet)
func NewSSHSession(cfg config.Session, onOutput OutputCallback, onStatus func(bool)) *SSHSession {
	return &SSHSession{
		cfg:      cfg,
		OnOutput: onOutput,
		OnStatus: onStatus,
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
	defer s.mu.Unlock()

	auth, err := s.buildAuth()
	if err != nil {
		return fmt.Errorf("auth error: %w", err)
	}

	prompt := s.HostKeyPrompt
	if prompt == nil {
		prompt = func(host, keyType, fp string) HostKeyDecision { return HostKeyAccept }
	}

	sshCfg := &ssh.ClientConfig{
		User:            s.cfg.User,
		Auth:            auth,
		HostKeyCallback: BuildHostKeyCallback(prompt),
		Timeout:         10 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	client, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	s.client = client

	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return fmt.Errorf("new session: %w", err)
	}
	s.session = sess

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty("xterm-256color", 40, 120, modes); err != nil {
		sess.Close()
		client.Close()
		return fmt.Errorf("pty request: %w", err)
	}

	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return fmt.Errorf("stdin pipe: %w", err)
	}
	s.stdin = stdin

	if err := sess.Shell(); err != nil {
		sess.Close()
		client.Close()
		return fmt.Errorf("shell: %w", err)
	}

	s.running = true
	logger.Info(s.cfg.Label, "connected to "+addr)
	if s.OnStatus != nil {
		s.OnStatus(true)
	}

	go s.streamOutput(stdout)
	go s.streamOutput(stderr)

	go func() {
		sess.Wait()
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		logger.Info(s.cfg.Label, "session ended")
		if s.OnStatus != nil {
			s.OnStatus(false)
		}
	}()

	return nil
}

// ConnectWithRetry retries up to maxRetries times with 3s delay
func (s *SSHSession) ConnectWithRetry(maxRetries int) {
	for i := 1; i <= maxRetries; i++ {
		logger.Info(s.cfg.Label, fmt.Sprintf("connect attempt %d/%d", i, maxRetries))
		if err := s.Connect(); err == nil {
			return
		} else {
			logger.Error(s.cfg.Label, err.Error())
			if s.OnOutput != nil {
				s.OnOutput(fmt.Sprintf("[mtputty] Reconnect attempt %d/%d failed: %s\r\n", i, maxRetries, err))
			}
		}
		time.Sleep(3 * time.Second)
	}
	logger.Error(s.cfg.Label, "all reconnect attempts failed")
	if s.OnOutput != nil {
		s.OnOutput("[mtputty] Could not reconnect. Please reconnect manually.\r\n")
	}
}

// SendCommand writes a command to the shell stdin
func (s *SSHSession) SendCommand(cmd string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.stdin == nil {
		return fmt.Errorf("session not active")
	}
	_, err := io.WriteString(s.stdin, cmd)
	return err
}

// Disconnect closes the session and client
func (s *SSHSession) Disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *SSHSession) buildAuth() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if s.cfg.UseKey && s.cfg.KeyPath != "" {
		keyBytes, err := os.ReadFile(s.cfg.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("read key %s: %w", s.cfg.KeyPath, err)
		}

		// Try parsing without passphrase first
		signer, err := ssh.ParsePrivateKey(keyBytes)
		if err != nil {
			// Check if it's a passphrase-protected key
			if _, ok := err.(*ssh.PassphraseMissingError); ok {
				passphrase := ""
				if s.KeyPassphrasePrompt != nil {
					passphrase = s.KeyPassphrasePrompt(s.cfg.KeyPath)
				}
				if passphrase == "" {
					return nil, fmt.Errorf("key %s is passphrase-protected but no passphrase provided", s.cfg.KeyPath)
				}
				signer, err = ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(passphrase))
				if err != nil {
					return nil, fmt.Errorf("wrong passphrase for key %s: %w", s.cfg.KeyPath, err)
				}
			} else {
				return nil, fmt.Errorf("parse key: %w", err)
			}
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if s.cfg.Password != "" {
		methods = append(methods, ssh.Password(s.cfg.Password))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no authentication method configured")
	}
	return methods, nil
}

func (s *SSHSession) streamOutput(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 && s.OnOutput != nil {
			s.OnOutput(string(buf[:n]))
		}
		if err != nil {
			break
		}
	}
}

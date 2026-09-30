package core

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"mtssh/config"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// testServer is a minimal in-process SSH server: password "pw", PTY + shell
// that echoes its input, and it reports pty-req/window-change sizes.
type testServer struct {
	addr   string
	port   int
	events chan string
	stop   func()
	// ignoreGlobal stops servicing global requests, which stalls the
	// connection like a dead network path.
	ignoreGlobal bool
	// exitShell makes the shell exit right away with status 0.
	exitShell bool
}

func newSigner(t *testing.T, ecdsaKey bool) ssh.Signer {
	t.Helper()
	var key any
	if ecdsaKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key = k
	} else {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key = k
	}
	s, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func startServer(t *testing.T, ignoreGlobal bool, hostKeys ...ssh.Signer) *testServer {
	t.Helper()
	return startServerAt(t, "127.0.0.1:0", ignoreGlobal, hostKeys...)
}

func startServerAt(t *testing.T, addr string, ignoreGlobal bool, hostKeys ...ssh.Signer) *testServer {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == "pw" {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	return startServerWith(t, addr, cfg, ignoreGlobal, hostKeys...)
}

func startServerWith(t *testing.T, addr string, cfg *ssh.ServerConfig, ignoreGlobal bool, hostKeys ...ssh.Signer) *testServer {
	t.Helper()
	for _, k := range hostKeys {
		cfg.AddHostKey(k)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	srv := &testServer{
		stop:         func() { ln.Close() },
		addr:         ln.Addr().String(),
		port:         ln.Addr().(*net.TCPAddr).Port,
		events:       make(chan string, 16),
		ignoreGlobal: ignoreGlobal,
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handle(conn, cfg)
		}
	}()
	return srv
}

func (srv *testServer) handle(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		conn.Close()
		return
	}
	if !srv.ignoreGlobal {
		go ssh.DiscardRequests(reqs)
	}
	for nc := range chans {
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				switch req.Type {
				case "pty-req":
					var p struct {
						Term             string
						Cols, Rows, W, H uint32
						Modes            string
					}
					ssh.Unmarshal(req.Payload, &p)
					srv.events <- fmt.Sprintf("pty %dx%d", p.Rows, p.Cols)
				case "window-change":
					var w struct{ Cols, Rows, W, H uint32 }
					ssh.Unmarshal(req.Payload, &w)
					srv.events <- fmt.Sprintf("resize %dx%d", w.Rows, w.Cols)
				case "shell":
					if srv.exitShell {
						req.Reply(true, nil)
						ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
						ch.Close()
						continue
					}
					go io.Copy(ch, ch) // echo
				}
				if req.WantReply {
					req.Reply(true, nil)
				}
			}
		}()
	}
}

func (srv *testServer) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-srv.events:
		if got != want {
			t.Fatalf("server event = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

func testHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func newTestSession(srv *testServer, out *syncBuffer, status chan bool) *SSHSession {
	cfg := config.Session{Label: "test", Host: "127.0.0.1", Port: srv.port, User: "u", Password: "pw"}
	s := NewSSHSession(cfg, func(o string) { out.WriteString(o) }, func(c bool) {
		if status != nil {
			status <- c
		}
	})
	s.HostKeyPrompt = func(host, keyType, fp string) HostKeyDecision { return HostKeyAccept }
	return s
}

type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuffer) WriteString(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sb.WriteString(s)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func TestSessionIO(t *testing.T) {
	testHome(t)
	srv := startServer(t, false, newSigner(t, false))
	var out syncBuffer
	s := newTestSession(srv, &out, nil)

	s.Resize(30, 100) // before connecting: used for the PTY request
	if err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	defer s.Disconnect()
	srv.expect(t, "pty 30x100")

	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "hello") {
		if time.Now().After(deadline) {
			t.Fatalf("echo not received, output %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	s.Resize(40, 120)
	srv.expect(t, "resize 40x120")
	s.Resize(40, 120) // unchanged: no request
	s.Resize(0, 0)    // invalid: ignored
	select {
	case ev := <-srv.events:
		t.Fatalf("unexpected server event %q", ev)
	case <-time.After(100 * time.Millisecond):
	}

	s.Disconnect()
	if _, err := s.Write([]byte("x")); err == nil {
		t.Fatal("Write after Disconnect should fail")
	}
}

func TestKeepaliveDropsDeadConnection(t *testing.T) {
	testHome(t)
	oldInterval, oldMissed := keepaliveInterval, keepaliveMaxMissed
	keepaliveInterval, keepaliveMaxMissed = 50*time.Millisecond, 2
	t.Cleanup(func() { keepaliveInterval, keepaliveMaxMissed = oldInterval, oldMissed })

	srv := startServer(t, true, newSigner(t, false))
	status := make(chan bool, 4)
	s := newTestSession(srv, &syncBuffer{}, status)
	if err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	defer s.Disconnect()
	if !<-status {
		t.Fatal("expected connected status first")
	}
	select {
	case connected := <-status:
		if connected {
			t.Fatal("expected disconnect")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dead connection was not detected by keepalive")
	}
	if !s.ConnectionLost() {
		t.Fatal("dropped connection not reported as lost")
	}
}

func TestShellExitIsNotConnectionLoss(t *testing.T) {
	testHome(t)
	srv := startServer(t, false, newSigner(t, false))
	srv.exitShell = true
	status := make(chan bool, 4)
	s := newTestSession(srv, &syncBuffer{}, status)
	if err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	defer s.Disconnect()
	<-status // connected
	select {
	case <-status:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end")
	}
	if s.ConnectionLost() {
		t.Fatal("a shell that exited was reported as a lost connection")
	}
}

func TestReconnectLimit(t *testing.T) {
	s := NewSSHSession(config.Session{Label: "test"}, nil, nil)
	for i := 0; i < maxAutoReconnects; i++ {
		if !s.allowReconnect() {
			t.Fatalf("reconnect %d refused", i+1)
		}
	}
	if s.allowReconnect() {
		t.Fatal("reconnect allowed beyond the limit")
	}
	// Old runs no longer count.
	for i := range s.reconnects {
		s.reconnects[i] = s.reconnects[i].Add(-reconnectWindow)
	}
	if !s.allowReconnect() {
		t.Fatal("reconnect refused after the window passed")
	}
}

func TestHandshakeTimeout(t *testing.T) {
	testHome(t)
	old := handshakeTimeout
	handshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	// Accepts TCP, never speaks SSH.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	s := NewSSHSession(config.Session{Label: "test", Host: "127.0.0.1", Port: port, User: "u", Password: "pw"}, nil, nil)
	start := time.Now()
	if err := s.Connect(); err == nil {
		t.Fatal("connect to a silent server succeeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("connect took %v", d)
	}

	// Disconnect aborts a connect that is waiting for the server.
	handshakeTimeout = time.Minute
	s = NewSSHSession(config.Session{Label: "test", Host: "127.0.0.1", Port: port, User: "u", Password: "pw"}, nil, nil)
	result := make(chan error, 1)
	go func() { result <- s.Connect() }()
	time.Sleep(100 * time.Millisecond)
	s.Disconnect()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("connect succeeded after Disconnect")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Disconnect did not abort the connect")
	}
}

func TestWrongPasswordIsAuthFailure(t *testing.T) {
	testHome(t)
	srv := startServer(t, false, newSigner(t, false))
	s := newTestSession(srv, &syncBuffer{}, nil)
	s.cfg.Password = "wrong"
	if err := s.Connect(); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

func TestReadKeyFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "id")
	os.WriteFile(good, []byte("key"), 0600)
	big := filepath.Join(dir, "big")
	os.WriteFile(big, make([]byte, maxKeyFileSize+1), 0600)

	if data, err := readKeyFile(good); err != nil || string(data) != "key" {
		t.Fatalf("regular key: %q, %v", data, err)
	}
	for _, p := range []string{big, dir, os.DevNull} {
		if _, err := readKeyFile(p); err == nil {
			t.Errorf("readKeyFile(%s) succeeded", p)
		}
	}
}

func TestNewHostKeyTypeIsNotMismatch(t *testing.T) {
	testHome(t)
	ed := newSigner(t, false)

	// First contact: the server only has an Ed25519 key, which gets stored.
	srv := startServerAt(t, "127.0.0.1:0", false, ed)
	s := newTestSession(srv, &syncBuffer{}, nil)
	if err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	s.Disconnect()
	srv.stop()

	// Same address, but the server now also offers ECDSA, which the client
	// prefers by default. The stored Ed25519 key must still be used.
	srv2 := startServerAt(t, srv.addr, false, newSigner(t, true), ed)
	s2 := newTestSession(srv2, &syncBuffer{}, nil)
	s2.HostKeyPrompt = func(host, keyType, fp string) HostKeyDecision {
		t.Errorf("unexpected host key prompt for %s (%s)", host, keyType)
		return HostKeyReject
	}
	if err := s2.Connect(); err != nil {
		t.Fatalf("connect after the server added an ECDSA key: %v", err)
	}
	s2.Disconnect()
}

func appendRaw(t *testing.T, line string) error {
	f, err := os.OpenFile(khPath(t), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func TestKnownHostKeyAlgorithms(t *testing.T) {
	testHome(t)
	if got := knownHostKeyAlgorithms("example.com:22"); got != nil {
		t.Fatalf("no known_hosts file: got %v", got)
	}
	if err := ensureFile(khPath(t)); err != nil {
		t.Fatal(err)
	}
	rsaLine := "example.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQDDHr/jh2Jy4yALcK4JyWbVkPRaWmhck3IgCoeOO3z1e2dBowLh64QAM+Qb72pxekALga2oi4GvT+TlWNhzPH4V4ZP0jXLbDdK+4Tj+4zGHmVpQvG8Cx+SNmGe07sCMV/q/eiD+5gt4zYwaDs8kcNR26kqa3XO4BqPE/o0B8C6vBw==\n"
	if err := appendRaw(t, rsaLine); err != nil {
		t.Fatal(err)
	}
	if err := appendKnownHost(khPath(t), "example.com:22", newSigner(t, false).PublicKey()); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(knownHostKeyAlgorithms("example.com:22"), ",")
	want := "rsa-sha2-512,rsa-sha2-256,ssh-rsa,ssh-ed25519"
	if got != want {
		t.Fatalf("algorithms = %s, want %s", got, want)
	}
	if got := knownHostKeyAlgorithms("other.example.com:22"); got != nil {
		t.Fatalf("unknown host: got %v", got)
	}
}

func TestKeyboardInteractive(t *testing.T) {
	testHome(t)
	// PAM-style server: keyboard-interactive only, no "password" method.
	cfg := &ssh.ServerConfig{
		KeyboardInteractiveCallback: func(_ ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			answers, err := challenge("", "", []string{"Password: "}, []bool{false})
			if err != nil || len(answers) != 1 || answers[0] != "pw" {
				return nil, fmt.Errorf("denied")
			}
			return nil, nil
		},
	}
	srv := startServerWith(t, "127.0.0.1:0", cfg, false, newSigner(t, false))

	// Stored password is used without asking.
	s := newTestSession(srv, &syncBuffer{}, nil)
	s.PasswordPrompt = func(string) string {
		t.Error("should not prompt when a password is stored")
		return ""
	}
	if err := s.Connect(); err != nil {
		t.Fatalf("stored password: %v", err)
	}
	s.Disconnect()

	// No stored password: the user is asked with the server's prompt.
	s = newTestSession(srv, &syncBuffer{}, nil)
	s.cfg.Password = ""
	var asked []string
	s.PasswordPrompt = func(q string) string {
		asked = append(asked, q)
		return "pw"
	}
	if err := s.Connect(); err != nil {
		t.Fatalf("prompted password: %v", err)
	}
	s.Disconnect()
	if len(asked) != 1 || asked[0] != "Password:" {
		t.Fatalf("prompts = %q, want one \"Password:\"", asked)
	}

	// Cancelling the prompt fails the login.
	s = newTestSession(srv, &syncBuffer{}, nil)
	s.cfg.Password = ""
	s.PasswordPrompt = func(string) string { return "" }
	if err := s.Connect(); err == nil {
		s.Disconnect()
		t.Fatal("cancelled prompt must not log in")
	}
}

func TestRemoveKnownHost(t *testing.T) {
	testHome(t)
	if err := RemoveKnownHost("anything"); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if err := ensureFile(khPath(t)); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		"a.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA",
		"# comment",
		"b.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB",
	}
	if err := appendRaw(t, strings.Join(lines, "\n")+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveKnownHost("  " + lines[0] + " "); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(khPath(t))
	if want := lines[1] + "\n" + lines[2] + "\n"; string(data) != want {
		t.Fatalf("after remove:\n%s\nwant:\n%s", data, want)
	}
	if fi, _ := os.Stat(khPath(t)); fi.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if err := RemoveKnownHost(""); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(khPath(t)); len(data) != 0 {
		t.Fatalf("clear left %q", data)
	}
}

func TestOpenHostKeyPromptDoesNotBlockOthers(t *testing.T) {
	testHome(t)
	srv1 := startServer(t, false, newSigner(t, false))
	srv2 := startServer(t, false, newSigner(t, false))

	// Connection 1: the host key dialog is never answered (e.g. its window
	// was closed).
	asked := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s1 := newTestSession(srv1, &syncBuffer{}, nil)
	s1.HostKeyPrompt = func(host, keyType, fp string) HostKeyDecision {
		close(asked)
		<-release
		return HostKeyReject
	}
	go s1.Connect()
	<-asked

	// Connection 2 must not wait for connection 1's dialog.
	s2 := newTestSession(srv2, &syncBuffer{}, nil)
	done := make(chan error, 1)
	go func() { done <- s2.Connect() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		s2.Disconnect()
	case <-time.After(3 * time.Second):
		t.Fatal("second connection blocked by the first one's open host key prompt")
	}
}

func TestCancelStopsAllPrompts(t *testing.T) {
	testHome(t)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			return nil, fmt.Errorf("denied")
		},
		KeyboardInteractiveCallback: func(_ ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			_, err := challenge("", "", []string{"Password: "}, []bool{false})
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("denied")
		},
	}
	srv := startServerWith(t, "127.0.0.1:0", cfg, false, newSigner(t, false))
	s := newTestSession(srv, &syncBuffer{}, nil)
	s.cfg.Password = ""
	prompts := 0
	s.PasswordPrompt = func(string) string {
		prompts++
		return "" // Cancel
	}
	err := s.Connect()
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	if prompts != 1 {
		t.Fatalf("prompted %d times after Cancel, want 1", prompts)
	}
}

func TestRetryStopsOnCancelAndMismatch(t *testing.T) {
	testHome(t)
	srv := startServer(t, false, newSigner(t, false))

	// Rejected host key: retrying would just ask again.
	out := &syncBuffer{}
	s := newTestSession(srv, out, nil)
	prompts := 0
	s.HostKeyPrompt = func(host, keyType, fp string) HostKeyDecision {
		prompts++
		return HostKeyReject
	}
	retryDelayForTest(t)
	s.ConnectWithRetry(3)
	if prompts != 1 {
		t.Fatalf("host key prompted %d times, want 1", prompts)
	}
	if !strings.Contains(out.String(), "Reconnect stopped") {
		t.Fatalf("output %q", out.String())
	}

	// Changed host key: errors.Is must see the mismatch through ssh.Dial.
	if err := appendKnownHost(khPath(t), srv.addr, newSigner(t, false).PublicKey()); err != nil {
		t.Fatal(err)
	}
	s = newTestSession(srv, &syncBuffer{}, nil)
	if err := s.Connect(); !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("err = %v, want ErrHostKeyMismatch", err)
	}
}

func retryDelayForTest(t *testing.T) {
	old := retryDelay
	retryDelay = 10 * time.Millisecond
	t.Cleanup(func() { retryDelay = old })
}

func TestKeyboardInteractiveSecondFactor(t *testing.T) {
	testHome(t)
	// PAM with 2FA: "Password:" first, then "Verification code:".
	cfg := &ssh.ServerConfig{
		KeyboardInteractiveCallback: func(_ ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			pw, err := challenge("", "", []string{"Password: "}, []bool{false})
			if err != nil || len(pw) != 1 || pw[0] != "pw" {
				return nil, fmt.Errorf("denied")
			}
			code, err := challenge("", "", []string{"Verification code: "}, []bool{false})
			if err != nil || len(code) != 1 || code[0] != "123456" {
				return nil, fmt.Errorf("denied")
			}
			return nil, nil
		},
	}
	srv := startServerWith(t, "127.0.0.1:0", cfg, false, newSigner(t, false))
	s := newTestSession(srv, &syncBuffer{}, nil) // stored password "pw"
	var asked []string
	s.PasswordPrompt = func(q string) string {
		asked = append(asked, q)
		return "123456"
	}
	if err := s.Connect(); err != nil {
		t.Fatalf("2FA login: %v", err)
	}
	s.Disconnect()
	if len(asked) != 1 || asked[0] != "Verification code:" {
		t.Fatalf("prompts = %q, want only the verification code", asked)
	}
}

func TestCheckHost(t *testing.T) {
	for _, h := range []string{
		"srv.example", "srv.example.", "SRV.Example", "10.0.0.1", "::1", "[::1]",
		"::ffff:1.2.3.4", "fe80::1%eth0", "host_name-1", "a-", "localhost",
		strings.Repeat("a", 63) + ".example",
	} {
		if err := CheckHost(h); err != nil {
			t.Errorf("CheckHost(%q) = %v, want ok", h, err)
		}
	}
	for _, h := range []string{
		"::ffff:203.0.113.5%x,*", "*,a.evil.com", "h:2222", "user@h", "::1]:2222",
		"[::1]:2222", "[::1", "a..b", "-a", "a.-b", "bücher.de", "a b", "", " ", ".",
		"srv.example..", "a/b", `a\b`, "a|b", "!a", "a?", "a*", "#a", "@a",
		"1.2.3.4%eth0", "fe80::1%", "fe80::1%et h0", "fe80::1%" + strings.Repeat("e", 65),
		strings.Repeat("a", 64) + ".example", strings.Repeat("a.", 127) + "ab",
		"srv\n.example", "srv\x00.example",
	} {
		if err := CheckHost(h); !errors.Is(err, ErrInvalidHost) {
			t.Errorf("CheckHost(%q) = %v, want ErrInvalidHost", h, err)
		}
	}
	if err := CheckHost("user@h"); !strings.Contains(err.Error(), "user and port have their own fields") {
		t.Errorf("error text %q", err)
	}
	if h, err := validHost(" [fe80::1%eth0] "); err != nil || h != "fe80::1%eth0" {
		t.Errorf("validHost = %q, %v", h, err)
	}
	if got := knownHostName("SRV.Example."); got != "srv.example" {
		t.Errorf("knownHostName = %q", got)
	}
}

func TestConnectRejectsInvalidHost(t *testing.T) {
	testHome(t)
	srv := startServer(t, false, newSigner(t, false))
	out := &syncBuffer{}
	s := newTestSession(srv, out, nil)
	// Go dials this (it ignores the unknown zone); stored as a known_hosts
	// pattern it would match every host.
	s.cfg.Host = "::ffff:127.0.0.1%x,*"
	s.cfg.UseKey, s.cfg.KeyPath = true, filepath.Join(t.TempDir(), "missing")
	s.HostKeyPrompt = func(host, keyType, fp string) HostKeyDecision {
		t.Errorf("host key prompt for %s", host)
		return HostKeyAccept
	}
	if err := s.Connect(); !errors.Is(err, ErrInvalidHost) {
		t.Fatalf("err = %v, want ErrInvalidHost (checked before the key is read)", err)
	}
	retryDelayForTest(t)
	s.ConnectWithRetry(3)
	if strings.Count(out.String(), "failed") != 1 || !strings.Contains(out.String(), "Reconnect stopped") {
		t.Fatalf("retry output %q", out.String())
	}
	if data, _ := os.ReadFile(khPath(t)); len(data) != 0 {
		t.Fatalf("known_hosts written: %q", data)
	}
}

func TestHostNormalizedInKnownHosts(t *testing.T) {
	testHome(t)
	srv := startServer(t, false, newSigner(t, false))
	s := newTestSession(srv, &syncBuffer{}, nil)
	s.cfg.Host = "::FFFF:127.0.0.1"
	if err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	s.Disconnect()
	data, _ := os.ReadFile(khPath(t))
	want := fmt.Sprintf("[::ffff:127.0.0.1]:%d ssh-ed25519 ", srv.port)
	if !strings.HasPrefix(string(data), want) {
		t.Fatalf("known_hosts = %q, want an entry starting with %q", data, want)
	}
	s = newTestSession(srv, &syncBuffer{}, nil)
	s.cfg.Host = "::ffff:127.0.0.1"
	s.HostKeyPrompt = func(host, keyType, fp string) HostKeyDecision {
		t.Errorf("prompted again for %s", host)
		return HostKeyReject
	}
	if err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	s.Disconnect()
}

func TestLegacyMixedCaseEntry(t *testing.T) {
	testHome(t)
	key := newSigner(t, false)
	srv := startServer(t, false, key)
	if err := ensureFile(khPath(t)); err != nil {
		t.Fatal(err)
	}
	// Stored by an older version under the host as entered.
	legacy := fmt.Sprintf("[::FFFF:127.0.0.1]:%d %s", srv.port, ssh.MarshalAuthorizedKey(key.PublicKey()))
	if err := appendRaw(t, legacy); err != nil {
		t.Fatal(err)
	}
	s := newTestSession(srv, &syncBuffer{}, nil)
	s.cfg.Host = "::FFFF:127.0.0.1"
	s.HostKeyPrompt = func(host, keyType, fp string) HostKeyDecision {
		t.Errorf("prompted for %s despite the legacy entry", host)
		return HostKeyReject
	}
	if err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	s.Disconnect()
	if data, _ := os.ReadFile(khPath(t)); string(data) != legacy {
		t.Fatalf("known_hosts changed:\n%s", data)
	}

	// Another key under the legacy entry is a mismatch, not a prompt.
	srv2 := startServer(t, false, newSigner(t, false))
	legacy2 := fmt.Sprintf("[::FFFF:127.0.0.1]:%d %s", srv2.port, ssh.MarshalAuthorizedKey(key.PublicKey()))
	if err := appendRaw(t, legacy2); err != nil {
		t.Fatal(err)
	}
	s = newTestSession(srv2, &syncBuffer{}, nil)
	s.cfg.Host = "::FFFF:127.0.0.1"
	s.HostKeyPrompt = func(host, keyType, fp string) HostKeyDecision {
		t.Errorf("prompted for %s despite the legacy entry", host)
		return HostKeyAccept
	}
	if err := s.Connect(); !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("err = %v, want ErrHostKeyMismatch", err)
	}
}

func TestIsLocalWindowsPath(t *testing.T) {
	for _, p := range []string{
		`\\h\s\k`, `//h/s/k`, `/\h\s\k`, `\/h/s/k`, `\??\UNC\h\s\k`,
		`\??\GLOBALROOT\Device\Mup\h\s`, `/??/UNC/h/s/k`, `\\?\C:\k`, `\\.\pipe\x`,
		`1:\k`, `\\`, `\??`,
	} {
		if isLocalWindowsPath(p) {
			t.Errorf("%s accepted", p)
		}
	}
	for _, p := range []string{
		`C:\Users\me\.ssh\id`, `c:/Users/me/id`, `C:id`, `~\.ssh\id`, `~/.ssh/id`,
		`.ssh\id`, `\Users\me\id`, `/home/me/.ssh/id`, `id`, "",
	} {
		if !isLocalWindowsPath(p) {
			t.Errorf("%s rejected", p)
		}
	}
	for v, want := range map[string]bool{"C:": true, "z:": true, "": false, "1:": false, `\\h\s`: false, "CC:": false} {
		if isDriveVolume(v) != want {
			t.Errorf("isDriveVolume(%q) = %v", v, !want)
		}
	}
}

func TestDisconnectIsNotConnectionLoss(t *testing.T) {
	testHome(t)
	srv := startServer(t, false, newSigner(t, false))
	status := make(chan bool, 4)
	out := &syncBuffer{}
	s := newTestSession(srv, out, status)
	if err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	<-status // connected
	s.Disconnect()
	select {
	case <-status:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end")
	}
	if s.ConnectionLost() {
		t.Fatal("Disconnect reported as a lost connection")
	}
	// A reconnect started anyway (e.g. by a UI race) does nothing.
	s.ConnectWithRetry(3)
	if out.String() != "" || len(s.reconnects) != 0 {
		t.Fatalf("ConnectWithRetry after Disconnect: output %q, %d reconnects counted", out.String(), len(s.reconnects))
	}
}

func TestPromptText(t *testing.T) {
	if got := promptText("Password: "); got != "Password: " {
		t.Errorf("plain prompt = %q", got)
	}
	if got := promptText("Duo two-factor\r\nPasscode or option (1-3):"); got != "Duo two-factor\nPasscode or option (1-3):" {
		t.Errorf("CRLF prompt = %q", got)
	}
	if got := promptText("a\u202egnp.exe\x1b[2J\x07"); got != `a\u202egnp.exe\x1b[2J\a` {
		t.Errorf("bidi/control = %q", got)
	}
	big := promptText("a" + strings.Repeat("é", 200_000))
	if len(big) > maxPromptBytes+len("…") || !strings.HasSuffix(big, "…") || !utf8.ValidString(big) {
		t.Errorf("oversize: %d bytes, suffix %q, valid %v", len(big), big[len(big)-8:], utf8.ValidString(big))
	}
	many := promptText(strings.Repeat("line\n", 100))
	if n := strings.Count(many, "\n") + 1; n != maxPromptLines || !strings.HasSuffix(many, "line…") {
		t.Errorf("many lines: %d lines, %q", n, many)
	}

	// Instruction and question are cleaned separately and joined by a newline.
	var asked string
	ask := func(q string) (string, error) { asked = q; return "123456", nil }
	stored := ""
	answers, err := answerQuestions(&stored, ask, "MTSSH: enter your master passphrase\n\n\n\n\n\n\n\n\n\n\n\x1b[8m",
		[]string{"Code\u2066:"}, []bool{true})
	if err != nil || len(answers) != 1 || answers[0] != "123456" {
		t.Fatalf("answers = %q, %v", answers, err)
	}
	if want := "MTSSH: enter your master passphrase\n\n\n\n\n\n\n\n\n…\nCode\\u2066:"; asked != want {
		t.Fatalf("asked %q, want %q", asked, want)
	}
}

func TestTooManyAuthFailuresIsAuthFailure(t *testing.T) {
	testHome(t)
	cfg := &ssh.ServerConfig{
		MaxAuthTries: 1,
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			return nil, fmt.Errorf("denied")
		},
	}
	srv := startServerWith(t, "127.0.0.1:0", cfg, false, newSigner(t, false))
	s := newTestSession(srv, &syncBuffer{}, nil)
	s.cfg.Password = "wrong"
	err := s.Connect()
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if !strings.Contains(err.Error(), "too many authentication failures") {
		t.Fatalf("err = %v, want the server's MaxAuthTries disconnect", err)
	}
	if !isAuthFailure(errors.New(`ssh: handshake failed: ssh: disconnect, reason 14: "No supported authentication methods available"`)) {
		t.Error("disconnect reason 14 not treated as an authentication failure")
	}
	if isAuthFailure(errors.New(`ssh: handshake failed: ssh: disconnect, reason 11: "bye"`)) {
		t.Error("disconnect reason 11 treated as an authentication failure")
	}
}

func TestAuthTimeout(t *testing.T) {
	testHome(t)
	oldHS, oldAuth := handshakeTimeout, authTimeout
	t.Cleanup(func() { handshakeTimeout, authTimeout = oldHS, oldAuth })
	const serverWait = 1200 * time.Millisecond

	// The server waits (push approval, slow PAM) after the host key is
	// trusted: longer than handshakeTimeout, shorter than authTimeout.
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			time.Sleep(serverWait)
			return nil, nil
		},
	}
	srv := startServerWith(t, "127.0.0.1:0", cfg, false, newSigner(t, false))
	s := newTestSession(srv, &syncBuffer{}, nil)
	if err := s.Connect(); err != nil { // stores the host key
		t.Fatal(err)
	}
	s.Disconnect()

	handshakeTimeout, authTimeout = 500*time.Millisecond, 10*time.Second
	// Known host (no prompt), stored password: the accepted host key
	// switches to authTimeout.
	s = newTestSession(srv, &syncBuffer{}, nil)
	s.HostKeyPrompt = nil
	if err := s.Connect(); err != nil {
		t.Fatalf("server wait after the host key: %v", err)
	}
	s.Disconnect()
	// After a prompt, the deadline restarts with authTimeout.
	s = newTestSession(srv, &syncBuffer{}, nil)
	s.HostKeyPrompt = nil
	s.cfg.Password = ""
	s.PasswordPrompt = func(string) string { return "1" }
	if err := s.Connect(); err != nil {
		t.Fatalf("server wait after a prompt: %v", err)
	}
	s.Disconnect()

	// authTimeout still bounds it.
	handshakeTimeout, authTimeout = 10*time.Second, 300*time.Millisecond
	s = newTestSession(srv, &syncBuffer{}, nil)
	s.HostKeyPrompt = nil
	start := time.Now()
	if err := s.Connect(); err == nil {
		s.Disconnect()
		t.Fatal("connect outlasted authTimeout")
	}
	if d := time.Since(start); d >= serverWait {
		t.Fatalf("connect took %v, want about authTimeout", d)
	}
}

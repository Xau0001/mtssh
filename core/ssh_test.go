package core

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"mtssh/config"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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

func appendRaw(line string) error {
	f, err := os.OpenFile(KnownHostsPath(), os.O_APPEND|os.O_WRONLY, 0600)
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
	if err := ensureFile(KnownHostsPath()); err != nil {
		t.Fatal(err)
	}
	rsaLine := "example.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQDDHr/jh2Jy4yALcK4JyWbVkPRaWmhck3IgCoeOO3z1e2dBowLh64QAM+Qb72pxekALga2oi4GvT+TlWNhzPH4V4ZP0jXLbDdK+4Tj+4zGHmVpQvG8Cx+SNmGe07sCMV/q/eiD+5gt4zYwaDs8kcNR26kqa3XO4BqPE/o0B8C6vBw==\n"
	if err := appendRaw(rsaLine); err != nil {
		t.Fatal(err)
	}
	if err := appendKnownHost(KnownHostsPath(), "example.com:22", newSigner(t, false).PublicKey()); err != nil {
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
	if err := ensureFile(KnownHostsPath()); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		"a.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA",
		"# comment",
		"b.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB",
	}
	if err := appendRaw(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveKnownHost("  " + lines[0] + " "); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(KnownHostsPath())
	if want := lines[1] + "\n" + lines[2] + "\n"; string(data) != want {
		t.Fatalf("after remove:\n%s\nwant:\n%s", data, want)
	}
	if fi, _ := os.Stat(KnownHostsPath()); fi.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if err := RemoveKnownHost(""); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(KnownHostsPath()); len(data) != 0 {
		t.Fatalf("clear left %q", data)
	}
}

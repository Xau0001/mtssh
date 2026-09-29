package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func newHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func khPath(t *testing.T) string {
	t.Helper()
	p, err := KnownHostsPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHostKeyCallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	prompts := 0
	accept := BuildHostKeyCallback(func(host, keyType, fp string) HostKeyDecision {
		prompts++
		return HostKeyAccept
	})
	reject := BuildHostKeyCallback(func(host, keyType, fp string) HostKeyDecision {
		return HostKeyReject
	})
	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 22}

	// A host whose name is a prefix of 10.0.0.1 must not count as "already known".
	other := newHostKey(t)
	if err := accept("10.0.0.10:22", addr, other); err != nil {
		t.Fatal(err)
	}

	key := newHostKey(t)
	if err := reject("10.0.0.1:22", addr, key); err == nil {
		t.Fatal("rejected key was accepted")
	}
	if err := accept("10.0.0.1:22", addr, key); err != nil {
		t.Fatal(err)
	}
	if err := accept("10.0.0.1:22", addr, key); err != nil {
		t.Fatal(err)
	}
	if prompts != 2 {
		t.Fatalf("prompted %d times, want 2 (key must be remembered)", prompts)
	}

	if err := accept("10.0.0.1:22", addr, newHostKey(t)); err == nil ||
		!strings.Contains(err.Error(), "MISMATCH") {
		t.Fatalf("changed host key: err = %v", err)
	}

	data, err := os.ReadFile(khPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("known_hosts has %d lines, want 2:\n%s", n, data)
	}
}

func TestKnownHostsRobustness(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := khPath(t)
	os.MkdirAll(filepath.Dir(path), 0700)

	known := newHostKey(t)
	ca := newHostKey(t)
	// Hand-edited: a CA line, a broken line, and no final newline.
	content := "@cert-authority *.corp " + string(ssh.MarshalAuthorizedKey(ca)) +
		"garbage line that is not a key\n" +
		strings.TrimSpace(knownhosts.Line([]string{"known.example:22"}, known))
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	prompts := 0
	accept := func() ssh.HostKeyCallback {
		return BuildHostKeyCallback(func(host, keyType, fp string) HostKeyDecision {
			prompts++
			return HostKeyAccept
		})
	}
	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 22}
	// The broken line does not stop the known host from working.
	if err := accept()("known.example:22", addr, known); err != nil {
		t.Fatal(err)
	}
	// A host covered only by the CA line is unknown, not a mismatch.
	if err := accept()("srv.corp:22", addr, newHostKey(t)); err != nil {
		t.Fatalf("CA-covered host: %v", err)
	}
	if prompts != 1 {
		t.Fatalf("prompts = %d, want 1", prompts)
	}
	// The new entry went on a line of its own.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "\nsrv.corp ") {
		t.Fatalf("entry appended to the previous line:\n%s", data)
	}
	if err := accept()("known.example:22", addr, known); err != nil {
		t.Fatalf("after append: %v", err)
	}
}

func TestHostKeyPinnedPerConnection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	prompts := 0
	cb := BuildHostKeyCallback(func(host, keyType, fp string) HostKeyDecision {
		prompts++
		return HostKeyAccept
	})
	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 3), Port: 22}
	key := newHostKey(t)
	if err := cb("pin.example:22", addr, key); err != nil {
		t.Fatal(err)
	}
	// Entry removed in the Known Hosts manager: re-keying must not prompt.
	if err := RemoveKnownHost(""); err != nil {
		t.Fatal(err)
	}
	if err := cb("pin.example:22", addr, key); err != nil || prompts != 1 {
		t.Fatalf("re-key: err = %v, prompts = %d", err, prompts)
	}
	// A different key during the same connection is a mismatch.
	if err := cb("pin.example:22", addr, newHostKey(t)); !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("changed key on re-key: %v", err)
	}
}

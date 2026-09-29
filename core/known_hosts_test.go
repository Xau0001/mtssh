package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
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

	data, err := os.ReadFile(KnownHostsPath())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("known_hosts has %d lines, want 2:\n%s", n, data)
	}
}

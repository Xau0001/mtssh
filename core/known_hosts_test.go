package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
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

// writeKnownHosts replaces the known_hosts file with lines.
func writeKnownHosts(t *testing.T, lines ...string) string {
	t.Helper()
	path := khPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func authorized(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// countingCallback accepts unknown hosts and counts the prompts.
func countingCallback(prompts *int) ssh.HostKeyCallback {
	return BuildHostKeyCallback(func(host, keyType, fp string) HostKeyDecision {
		*prompts++
		return HostKeyAccept
	})
}

var khRemote = &net.TCPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 22}

func TestAppendKnownHostRefusesPatterns(t *testing.T) {
	testHome(t)
	path := writeKnownHosts(t, "# mine")
	before, _ := os.ReadFile(path)
	key := newHostKey(t)
	for _, host := range []string{
		"::ffff:203.0.113.5%x,*", "*,a.evil.com", "*", "!srv.example", "srv?.example",
		"a b", "[srv.example", "srv.example]", " srv.example", "|1|abc", "@revoked",
	} {
		addr := net.JoinHostPort(host, "22")
		if err := appendKnownHost(path, addr, key); err == nil {
			t.Errorf("appendKnownHost(%q) succeeded", addr)
		}
		prompts := 0
		if err := countingCallback(&prompts)(addr, khRemote, key); err == nil || prompts != 0 {
			t.Errorf("callback for %q: err = %v, prompts = %d", addr, err, prompts)
		}
		addr = net.JoinHostPort(host, "2222")
		if err := appendKnownHost(path, addr, key); err == nil {
			t.Errorf("appendKnownHost(%q) succeeded", addr)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != string(before) {
		t.Fatalf("known_hosts written:\n%s", data)
	}

	// Plain hosts still work, including an IPv6 zone.
	for _, addr := range []string{"srv.example:22", "[fe80::1%eth0]:2222", "[::1]:22"} {
		if err := appendKnownHost(path, addr, key); err != nil {
			t.Errorf("appendKnownHost(%q): %v", addr, err)
		}
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"\nsrv.example ssh-ed25519 ", "\n[fe80::1%eth0]:2222 ssh-ed25519 ", "\n::1 ssh-ed25519 "} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q in:\n%s", want, data)
		}
	}
}

func TestLegacyEntryCallback(t *testing.T) {
	testHome(t)
	key := newHostKey(t)
	writeKnownHosts(t, "SRV.Example "+authorized(key), "dot.example. "+authorized(key))
	for legacy, addr := range map[string]string{"SRV.Example:22": "srv.example:22", "dot.example.:22": "dot.example:22"} {
		prompts := 0
		prompt := func(host, keyType, fp string) HostKeyDecision {
			prompts++
			return HostKeyAccept
		}
		if err := hostKeyCallback(prompt, legacy)(addr, khRemote, key); err != nil || prompts != 0 {
			t.Errorf("%s: err = %v, prompts = %d", legacy, err, prompts)
		}
		if err := hostKeyCallback(prompt, legacy)(addr, khRemote, newHostKey(t)); !errors.Is(err, ErrHostKeyMismatch) || prompts != 0 {
			t.Errorf("%s, other key: err = %v, prompts = %d", legacy, err, prompts)
		}
	}
	data, _ := os.ReadFile(khPath(t))
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("known_hosts has %d lines, want 2:\n%s", n, data)
	}
	// knownhosts itself compares case-sensitively: without the legacy
	// address the old entry is not found.
	prompts := 0
	if err := countingCallback(&prompts)("srv.example:22", khRemote, key); err != nil || prompts != 1 {
		t.Fatalf("without legacy address: err = %v, prompts = %d", err, prompts)
	}
}

func TestKnownHostsCommentWithSpaces(t *testing.T) {
	testHome(t)
	stored := newHostKey(t)
	writeKnownHosts(t, "srv.example "+authorized(stored)+" prod db server")
	prompts := 0
	if err := countingCallback(&prompts)("srv.example:22", khRemote, stored); err != nil {
		t.Fatalf("stored key: %v", err)
	}
	if err := countingCallback(&prompts)("srv.example:22", khRemote, newHostKey(t)); !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("other key: err = %v, want ErrHostKeyMismatch", err)
	}
	if prompts != 0 {
		t.Fatalf("prompted %d times", prompts)
	}
	if got := knownHostKeyAlgorithms("srv.example:22"); len(got) != 1 || got[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algorithms = %v", got)
	}
}

func TestKnownHostsRevoked(t *testing.T) {
	testHome(t)
	leaked := newHostKey(t)
	writeKnownHosts(t, "@revoked * "+authorized(leaked)+" leaked in incident 42")
	prompts := 0
	if err := countingCallback(&prompts)("any.example:22", khRemote, leaked); err == nil ||
		!strings.Contains(err.Error(), "revoked") || prompts != 0 {
		t.Fatalf("revoked key: err = %v, prompts = %d", err, prompts)
	}
	if err := countingCallback(&prompts)("any.example:22", khRemote, newHostKey(t)); err != nil || prompts != 1 {
		t.Fatalf("other key: err = %v, prompts = %d", err, prompts)
	}

	// An @revoked line that cannot be read fails every connection.
	known := newHostKey(t)
	writeKnownHosts(t, "known.example "+authorized(known), "@revoked * ssh-ed25519 AAAA!!!!")
	prompts = 0
	for _, host := range []string{"known.example:22", "new.example:22"} {
		err := countingCallback(&prompts)(host, khRemote, known)
		if err == nil || !strings.Contains(err.Error(), "known_hosts line 2: invalid @revoked entry") {
			t.Errorf("%s: err = %v", host, err)
		}
	}
	if prompts != 0 {
		t.Fatalf("prompted %d times", prompts)
	}
}

func TestKnownHostsSkipsInvalidLines(t *testing.T) {
	testHome(t)
	known, other := newHostKey(t), newHostKey(t)
	k := authorized(other)
	path := writeKnownHosts(t,
		"@unknown host "+k,
		"!,x "+k,
		"[badhost "+k,
		"|1|abc "+k,
		"known.example "+authorized(known),
		"\v",
		"@cert-authority *.corp ssh-ed25519 AAAA!!!!",
	)
	prompts := 0
	if err := countingCallback(&prompts)("known.example:22", khRemote, known); err != nil {
		t.Fatalf("known host: %v", err)
	}
	if err := countingCallback(&prompts)("known.example:22", khRemote, other); !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("known host, other key: %v", err)
	}
	if err := countingCallback(&prompts)("new.example:22", khRemote, other); err != nil || prompts != 1 {
		t.Fatalf("new host: err = %v, prompts = %d", err, prompts)
	}
	if got := knownHostKeyAlgorithms("known.example:22"); len(got) != 1 {
		t.Fatalf("algorithms = %v", got)
	}
	// The cleaned copies are removed.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".known_hosts-") {
			t.Errorf("temp copy %s left behind", e.Name())
		}
	}
}

func TestKnownHostsInvalidLineNamingHost(t *testing.T) {
	testHome(t)
	writeKnownHosts(t,
		"other.example,SRV.example. ssh-ed25519 AAAA!!!! prod",
		"[port.example]:2222 ssh-ed25519 AAAA!!!!",
	)
	key := newHostKey(t)
	for _, host := range []string{"srv.example:22", "other.example:22", "port.example:2222"} {
		prompts := 0
		err := countingCallback(&prompts)(host, khRemote, key)
		if err == nil || !strings.Contains(err.Error(), "is invalid and names") || prompts != 0 {
			t.Errorf("%s: err = %v, prompts = %d", host, err, prompts)
		}
	}
	for _, host := range []string{"new.example:22", "port.example:22", "srv.example.net:22"} {
		prompts := 0
		if err := countingCallback(&prompts)(host, khRemote, key); err != nil || prompts != 1 {
			t.Errorf("%s: err = %v, prompts = %d", host, err, prompts)
		}
	}
}

func TestKnownHostsFailClosed(t *testing.T) {
	testHome(t)
	key := newHostKey(t)
	// knownhosts cannot read past a line longer than 64 KB: no line number,
	// so nothing can be skipped.
	writeKnownHosts(t, "long.example ssh-ed25519 "+strings.Repeat("A", 70<<10), "known.example "+authorized(key))
	prompts := 0
	if err := countingCallback(&prompts)("known.example:22", khRemote, key); err == nil || prompts != 0 {
		t.Fatalf("over-long line: err = %v, prompts = %d", err, prompts)
	}

	var lines []string
	for i := 0; i <= maxSkippedKnownHosts; i++ {
		lines = append(lines, fmt.Sprintf("h%d.example ssh-ed25519 AAAA!!!!", i))
	}
	writeKnownHosts(t, lines...)
	if err := countingCallback(&prompts)("new.example:22", khRemote, key); err == nil || prompts != 0 {
		t.Fatalf("too many invalid lines: err = %v, prompts = %d", err, prompts)
	}
}

func TestKhErrorLine(t *testing.T) {
	file := `C:\Users\me\.mtssh\known_hosts`
	n, reason, ok := khErrorLine(fmt.Errorf("knownhosts: %s:%d: %v", file, 12, errors.New("knownhosts: missing host pattern")), file)
	if !ok || n != 12 || reason != "knownhosts: missing host pattern" {
		t.Fatalf("got %d, %q, %v", n, reason, ok)
	}
	for _, msg := range []string{"bufio.Scanner: token too long", "knownhosts: " + file + ": x", "knownhosts: other:3: x"} {
		if _, _, ok := khErrorLine(errors.New(msg), file); ok {
			t.Errorf("%q parsed as a line error", msg)
		}
	}
}

func TestRemoveKnownHostLeavesNoTemp(t *testing.T) {
	testHome(t)
	path := writeKnownHosts(t, "a.example "+authorized(newHostKey(t)))
	if err := RemoveKnownHost(""); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

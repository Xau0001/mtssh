package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseKnownHosts(t *testing.T) {
	key := "AAAAC3NzaC1lZDI1NTE5AAAAIA"
	// Longer than bufio.Scanner's limit: the lines after it must not vanish.
	long := "long.example ssh-ed25519 " + strings.Repeat("A", 70<<10)
	data := strings.Join([]string{
		"# comment",
		"srv.example ssh-ed25519 " + key,
		"@revoked * ssh-ed25519 " + key + " leaked",
		"  @cert-authority *.corp\tssh-rsa AAAAB3 ",
		long,
		"broken.example",
		"after.example ssh-ed25519 " + key + "\r",
		"",
	}, "\n")
	want := []KnownHostEntry{
		{"", "srv.example", "ssh-ed25519", "srv.example ssh-ed25519 " + key},
		{"@revoked", "*", "ssh-ed25519", "@revoked * ssh-ed25519 " + key + " leaked"},
		{"@cert-authority", "*.corp", "ssh-rsa", "@cert-authority *.corp\tssh-rsa AAAAB3"},
		{"", "long.example", "ssh-ed25519", long},
		{"", "broken.example", "", "broken.example"},
		{"", "after.example", "ssh-ed25519", "after.example ssh-ed25519 " + key},
	}
	got := parseKnownHosts([]byte(data))
	if len(got) != len(want) {
		t.Fatalf("%d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Marker != w.Marker || g.Hostname != w.Hostname || g.KeyType != w.KeyType {
			t.Errorf("entry %d = %q %q %q, want %q %q %q", i, g.Marker, g.Hostname, g.KeyType, w.Marker, w.Hostname, w.KeyType)
		}
		// Raw is what RemoveKnownHost looks for in the file.
		if g.Raw != w.Raw {
			t.Errorf("entry %d: Raw has %d bytes, want %d", i, len(g.Raw), len(w.Raw))
		}
	}
	if h := got[1].hostText(); h != "@revoked *" {
		t.Errorf("host column = %q", h)
	}
	if parseKnownHosts(nil) != nil {
		t.Error("entries for an empty file")
	}
}

func TestKnownHostsDisplay(t *testing.T) {
	e := parseKnownHosts([]byte("evil\x1b[2J" + sessRLO + ".example ssh-ed25519\x07 AAAA"))[0]
	if got, want := e.hostText(), `evil\x1b[2J\u202e.example`; got != want {
		t.Errorf("host = %q, want %q", got, want)
	}
	if got, want := khDisplay(e.KeyType), `ssh-ed25519\a`; got != want {
		t.Errorf("key type = %q, want %q", got, want)
	}
	long := khDisplay(strings.Repeat("h", 1000))
	if n := utf8.RuneCountInString(long); n != sessMaxDisplayLen || !strings.HasSuffix(long, "…") {
		t.Errorf("long host: %d characters, %q", n, long)
	}
}

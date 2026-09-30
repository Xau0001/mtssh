package ui

import (
	"mtssh/config"
	"strings"
	"testing"
	"unicode/utf8"
)

// Written as code points: these characters are invisible in source.
var (
	sessZWJ  = string(rune(0x200D))
	sessZWNJ = string(rune(0x200C))
	sessRLO  = string(rune(0x202E))
	sessBad  = string(utf8.RuneError)
)

func TestSessCheckHost(t *testing.T) {
	for _, h := range []string{
		"srv.example", "srv.example.", "SRV.Example", "10.0.0.1", "::1", "[::1]",
		"::ffff:1.2.3.4", "fe80::1%eth0", "[fe80::1%eth0]", "host_name-1", " srv.example ",
		strings.Repeat("a", 63), strings.Repeat("a.", 126) + "b", // 253 bytes
	} {
		if err := sessCheckHost(h); err != nil {
			t.Errorf("%q rejected: %v", h, err)
		}
	}
	for _, h := range []string{
		"::ffff:203.0.113.5%x,*", "*,a.evil.com", "h:2222", "user@h", "[::1]:2222", "::1]:2222",
		"a..b", "-a", "a.-b", "bücher.de", "a b", "", " ", "[]", ".", "..", "[[::1]]", "[::1", "a/b", `a\b`,
		"a|b", "h!", "h?", "a,b", "fe80::1%", "fe80::1%eth 0", "fe80::1%eth0%1", "1.2.3.4%eth0",
		"fe80::1%" + strings.Repeat("e", 65), strings.Repeat("a", 64), strings.Repeat("a.", 127),
		"a\nb", "a" + sessRLO + "b",
	} {
		if err := sessCheckHost(h); err == nil {
			t.Errorf("%q accepted", h)
		}
	}
}

func TestNormalizeSessionHost(t *testing.T) {
	s := config.Session{Label: "l", Host: " [fe80::1%eth0] ", Port: 22, User: "u"}
	if err := normalizeSession(&s); err != nil || s.Host != "fe80::1%eth0" {
		t.Fatalf("host %q, err %v", s.Host, err)
	}
	s = config.Session{Label: "l", Host: "SRV.Example.", Port: 22, User: "u"}
	if err := normalizeSession(&s); err != nil || s.Host != "SRV.Example." {
		t.Fatalf("host %q, err %v", s.Host, err)
	}
	for _, h := range []string{"[[::1]]", "[::1]:2222", "root@srv.example", "srv.example:22"} {
		s = config.Session{Label: "l", Host: h, Port: 22, User: "u"}
		err := normalizeSession(&s)
		if err == nil || !strings.Contains(err.Error(), "user and port") {
			t.Errorf("host %q: err %v", h, err)
		}
	}
}

func TestNormalizeSessionText(t *testing.T) {
	flag := "🏳" + string(rune(0xFE0F)) + sessZWJ + "🌈"
	england := "🏴" + string([]rune{0xE0067, 0xE0062, 0xE0065, 0xE006E, 0xE0067, 0xE007F})
	for _, label := range []string{
		"dev 👩" + sessZWJ + "💻", "می" + sessZWNJ + "خواهم", flag, england,
		"Ser" + string(rune(0x00AD)) + "ver", strings.Repeat("a", maxFieldLen),
		strings.Repeat("𝄞", maxFieldLen), // 4 bytes each: exactly the byte cap
	} {
		s := config.Session{Label: label, Host: "h", Port: 22, User: "u", Group: strings.Repeat("组", 99)}
		if err := normalizeSession(&s); err != nil {
			t.Errorf("label %q: %v", label, err)
		}
	}

	for _, label := range []string{
		"a" + sessRLO + "b", "a\nb", "a\x00b", "a\x7fb", "a" + string(rune(0x85)) + "b",
		"a" + string(rune(0x061C)) + "b", "a" + string(rune(0x200E)) + "b", "a" + string(rune(0x2066)) + "b",
		"a" + string(rune(0x2028)) + "b", "a" + string(rune(0xFEFF)) + "b",
	} {
		s := config.Session{Label: label, Host: "h", Port: 22, User: "u"}
		err := normalizeSession(&s)
		if err == nil || err.Error() != "label contains control or invisible formatting characters" {
			t.Errorf("label %q: err %v", label, err)
		}
	}

	for _, label := range []string{strings.Repeat("a", maxFieldLen+1), strings.Repeat("组", maxFieldLen+1)} {
		s := config.Session{Label: label, Host: "h", Port: 22, User: "u"}
		if err := normalizeSession(&s); err == nil || err.Error() != "label is too long" {
			t.Errorf("label of %d characters: err %v", utf8.RuneCountInString(label), err)
		}
	}
}

func TestSessDisplay(t *testing.T) {
	if got, want := sessDisplay("a"+sessRLO+"b\nc"), "a"+sessBad+"b"+sessBad+"c"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if emoji := "dev 👩" + sessZWJ + "💻"; sessDisplay(emoji) != emoji {
		t.Errorf("emoji changed: %q", sessDisplay(emoji))
	}
	got := sessDisplay(strings.Repeat("x", 1000))
	if utf8.RuneCountInString(got) != sessMaxDisplayLen || !strings.HasSuffix(got, "…") {
		t.Errorf("long text shown as %q", got)
	}
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{{"abc", 3, "abc"}, {"abcd", 3, "ab…"}, {"", 1, ""}, {"ab", 1, "…"}, {"äöüß", 3, "äö…"}} {
		if got := sessTruncate(c.in, c.n); got != c.want {
			t.Errorf("sessTruncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

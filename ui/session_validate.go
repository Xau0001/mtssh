package ui

import (
	"errors"
	"fmt"
	"mtssh/config"
	"mtssh/core"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxFieldLen bounds the text fields of a session, in characters.
const maxFieldLen = 256

// sessMaxFieldBytes is a hard cap on the size of a field in bytes, checked
// before the characters are counted.
const sessMaxFieldBytes = 1024

// sessMaxHostLen is the longest host name DNS allows.
const sessMaxHostLen = 253

// sessMaxDisplayLen caps session text shown in lists and titles.
const sessMaxDisplayLen = 128

var sessErrInvalidHost = errors.New("invalid host — enter only the host name or IP address; user and port have their own fields")

// normalizeSession trims s's fields and checks them. The session dialog and
// the import use it, so imported sessions obey the same rules as ones
// entered by hand. It returns a description of the first problem found.
func normalizeSession(s *config.Session) error {
	host := s.Host
	s.Label = strings.TrimSpace(s.Label)
	// "[::1]" → "::1"; the port is added separately
	s.Host = sessTrimHost(s.Host)
	s.User = strings.TrimSpace(s.User)
	s.KeyPath = strings.TrimSpace(s.KeyPath)
	s.Group = strings.TrimSpace(s.Group)

	if s.Label == "" || s.Host == "" || s.User == "" {
		return fmt.Errorf("label, host and user are required")
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("invalid port number %d", s.Port)
	}
	// Checked as entered, so "[[::1]]" does not pass as "[::1]". Pattern
	// characters such as * or , would end up in known_hosts lines.
	if err := sessCheckHost(host); err != nil {
		return err
	}
	for _, f := range []struct{ name, value string }{
		{"label", s.Label}, {"host", s.Host}, {"user", s.User},
		{"key path", s.KeyPath}, {"group", s.Group},
	} {
		if len(f.value) > sessMaxFieldBytes || utf8.RuneCountInString(f.value) > maxFieldLen {
			return fmt.Errorf("%s is too long", f.name)
		}
		if strings.ContainsFunc(f.value, badRune) {
			return fmt.Errorf("%s contains control or invisible formatting characters", f.name)
		}
	}
	if s.KeyPath != "" {
		if err := core.CheckKeyPath(s.KeyPath); err != nil {
			return err
		}
	}
	return nil
}

// sessTrimHost removes surrounding spaces and one pair of brackets.
func sessTrimHost(host string) string {
	host = strings.TrimSpace(host)
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	return host
}

// sessCheckHost accepts an IP address (an IPv6 zone only of
// [A-Za-z0-9_.-]) or an ASCII host name, optionally in brackets. Anything
// else — a user, a port, known_hosts pattern characters — is rejected. It
// follows the same rule as core.CheckHost.
func sessCheckHost(host string) error {
	h := sessTrimHost(host)
	if h == "" || len(h) > sessMaxHostLen {
		return sessErrInvalidHost
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		if z := ip.Zone(); z != "" && !sessValidZone(z) {
			return sessErrInvalidHost
		}
		return nil
	}
	if !sessValidHostName(h) {
		return sessErrInvalidHost
	}
	return nil
}

// sessValidZone reports whether z is a plausible interface name.
func sessValidZone(z string) bool {
	if len(z) > 64 {
		return false
	}
	for i := 0; i < len(z); i++ {
		if !sessHostChar(z[i]) && z[i] != '.' {
			return false
		}
	}
	return z != ""
}

// sessValidHostName reports whether h consists of labels of 1-63
// characters [A-Za-z0-9_-], not starting with "-", separated by single dots,
// with an optional trailing dot.
func sessValidHostName(h string) bool {
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			if !sessHostChar(label[i]) {
				return false
			}
		}
	}
	return true
}

func sessHostChar(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-'
}

// badRune reports control characters (C0, DEL, C1) and the invisible
// format characters that change how the text around them is displayed:
// bidi marks, embeddings, overrides and isolates, line and paragraph
// separators, and the byte order mark. Joiners (ZWJ, ZWNJ), soft hyphens,
// emoji tags and variation selectors are allowed: emoji and many scripts
// need them.
func badRune(r rune) bool {
	switch {
	case unicode.IsControl(r):
		return true
	case r == 0x061C, r == 0x200E, r == 0x200F: // ALM, LRM, RLM
		return true
	case 0x202A <= r && r <= 0x202E: // LRE, RLE, PDF, LRO, RLO
		return true
	case 0x2066 <= r && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	case r == 0x2028, r == 0x2029, r == 0xFEFF: // line/paragraph separator, BOM
		return true
	}
	return false
}

// sessDisplay prepares a session label, group or host for display: it
// replaces characters badRune rejects with U+FFFD and shortens the text to
// sessMaxDisplayLen characters. Sessions stored before these checks existed
// may contain such characters. Unlike logger.Clean it keeps joiners, so
// emoji sequences display as one symbol.
func sessDisplay(s string) string {
	return sessDisplayN(s, sessMaxDisplayLen)
}

// sessDisplayN is sessDisplay with a limit of n characters.
func sessDisplayN(s string, n int) string {
	return strings.Map(func(r rune) rune {
		if badRune(r) {
			return utf8.RuneError
		}
		return r
	}, sessTruncate(s, n))
}

// sessTruncate shortens s to at most n characters (n ≥ 1), ending a
// shortened text with "…".
func sessTruncate(s string, n int) string {
	count, cut := 0, 0
	for i := range s {
		if count == n-1 {
			cut = i
		}
		if count == n {
			return s[:cut] + "…"
		}
		count++
	}
	return s
}

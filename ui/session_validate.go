package ui

import (
	"fmt"
	"mtssh/config"
	"mtssh/core"
	"strings"
	"unicode"
)

// maxFieldLen bounds the text fields of a session.
const maxFieldLen = 256

// normalizeSession trims s's fields and checks them. The session dialog and
// the import use it, so imported sessions obey the same rules as ones
// entered by hand. It returns a description of the first problem found.
func normalizeSession(s *config.Session) error {
	s.Label = strings.TrimSpace(s.Label)
	// "[::1]" → "::1"; the port is added separately
	s.Host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s.Host), "["), "]")
	s.User = strings.TrimSpace(s.User)
	s.KeyPath = strings.TrimSpace(s.KeyPath)
	s.Group = strings.TrimSpace(s.Group)

	if s.Label == "" || s.Host == "" || s.User == "" {
		return fmt.Errorf("label, host and user are required")
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("invalid port number %d", s.Port)
	}
	// Host names never contain spaces; control characters in any field
	// would end up in logs, dialogs and known_hosts lines.
	if strings.ContainsFunc(s.Host, func(r rune) bool { return unicode.IsSpace(r) || badRune(r) }) {
		return fmt.Errorf("invalid host name")
	}
	for _, f := range []struct{ name, value string }{
		{"label", s.Label}, {"host", s.Host}, {"user", s.User},
		{"key path", s.KeyPath}, {"group", s.Group},
	} {
		if len(f.value) > maxFieldLen {
			return fmt.Errorf("%s is too long", f.name)
		}
		if strings.ContainsFunc(f.value, badRune) {
			return fmt.Errorf("%s contains control characters", f.name)
		}
	}
	if s.KeyPath != "" {
		if err := core.CheckKeyPath(s.KeyPath); err != nil {
			return err
		}
	}
	return nil
}

// badRune reports control and invisible format characters (e.g. bidi
// overrides that make text display differently than it reads).
func badRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

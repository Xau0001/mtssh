package core

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"mtssh/config"
	"mtssh/logger"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyDecision is the result of asking the user about an unknown host
type HostKeyDecision int

const (
	HostKeyAccept HostKeyDecision = iota
	HostKeyReject
)

// HostKeyPrompt is called when a host key is not yet known.
// It should block until the user makes a decision.
type HostKeyPrompt func(host, keyType, fingerprint string) HostKeyDecision

var khMu sync.Mutex

// KnownHostsPath returns the location of MTSSH's own known_hosts file.
func KnownHostsPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "known_hosts"), nil
}

// unknownHostKeyAlgorithms is offered to hosts without a stored key: the
// x/crypto defaults without certificate algorithms (MTSSH verifies plain
// host keys; a server with a host certificate would otherwise present it
// and fail without a prompt) and without DSA.
var unknownHostKeyAlgorithms = []string{
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA256,
	ssh.KeyAlgoRSASHA512,
	ssh.KeyAlgoRSA,
	ssh.KeyAlgoED25519,
}

// Errors from BuildHostKeyCallback. ssh.Dial wraps them, so use errors.Is.
var (
	// ErrHostKeyMismatch: the host presented a key other than the stored one.
	ErrHostKeyMismatch = errors.New("HOST KEY MISMATCH")
	// ErrHostKeyRejected: the user did not trust an unknown host's key.
	ErrHostKeyRejected = errors.New("host key rejected by user")

	errUnknownHost = errors.New("unknown host")
)

// BuildHostKeyCallback returns an ssh.HostKeyCallback that:
//  1. Accepts known hosts from ~/.mtssh/known_hosts
//  2. Calls prompt for unknown hosts and appends accepted keys
//  3. Rejects changed host keys (MITM protection)
//
// khMu is not held while prompting: the answer may take long or never come
// (window closed), and other connections must not wait for it.
//
// Build one callback per connection: it remembers the key it accepted, and
// later key exchanges on the connection (re-keying) must present the same
// key. They are not checked against the file again, so removing the entry
// in the Known Hosts manager does not pop up a prompt mid-session.
func BuildHostKeyCallback(prompt HostKeyPrompt) ssh.HostKeyCallback {
	var mu sync.Mutex
	accepted := map[string][]byte{} // host → key accepted on this connection
	check := hostKeyChecker(prompt)
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		mu.Lock()
		pinned, ok := accepted[hostname]
		mu.Unlock()
		if ok {
			if bytes.Equal(pinned, key.Marshal()) {
				return nil
			}
			return fmt.Errorf("%w for %s — the key changed during the session", ErrHostKeyMismatch, hostname)
		}
		if err := check(hostname, remote, key); err != nil {
			return err
		}
		mu.Lock()
		accepted[hostname] = key.Marshal()
		mu.Unlock()
		return nil
	}
}

func hostKeyChecker(prompt HostKeyPrompt) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		path, err := KnownHostsPath()
		if err != nil {
			return err
		}
		khMu.Lock()
		err = checkHostKey(path, hostname, remote, key)
		khMu.Unlock()
		if !errors.Is(err, errUnknownHost) {
			return err // nil (known and matching), mismatch or I/O error
		}

		// Unknown host — ask user
		if prompt(hostname, key.Type(), ssh.FingerprintSHA256(key)) != HostKeyAccept {
			return fmt.Errorf("%w for %s", ErrHostKeyRejected, hostname)
		}

		khMu.Lock()
		defer khMu.Unlock()
		// Another connection may have stored a key for this host meanwhile.
		if err := checkHostKey(path, hostname, remote, key); !errors.Is(err, errUnknownHost) {
			return err
		}
		if err := appendKnownHost(path, hostname, key); err != nil {
			return fmt.Errorf("could not save host key: %w", err)
		}
		return nil
	}
}

// checkHostKey returns nil if key is stored for hostname, errUnknownHost if
// the host has no stored key, and ErrHostKeyMismatch if it has other keys.
// khMu must be held.
func checkHostKey(path, hostname string, remote net.Addr, key ssh.PublicKey) error {
	// Ensure the file exists so knownhosts.New doesn't fail
	if err := ensureFile(path); err != nil {
		return err
	}
	checker, caLines, err := loadKnownHosts(path)
	if err != nil {
		return fmt.Errorf("known_hosts: %w", err)
	}
	err = checker(hostname, remote, key)
	if err == nil {
		return nil
	}
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		return fmt.Errorf("known_hosts: %w", err) // e.g. a revoked key
	}
	want := plainKeys(keyErr.Want, caLines)
	if len(want) == 0 {
		// No key stored, or only @cert-authority lines: MTSSH does not
		// use host certificates, so the host is unknown.
		return errUnknownHost
	}
	return fmt.Errorf("%w for %s — possible MITM attack\nExpected: %s\nGot: %s",
		ErrHostKeyMismatch, hostname,
		ssh.FingerprintSHA256(want[0].Key), ssh.FingerprintSHA256(key))
}

// loadKnownHosts parses the known_hosts file like knownhosts.New, but a
// line it cannot parse (e.g. edited by hand) is skipped and logged instead
// of making every connection fail. It also returns the line numbers of
// @cert-authority lines. khMu must be held.
func loadKnownHosts(path string) (ssh.HostKeyCallback, map[int]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.Split(string(data), "\n")
	caLines := map[int]bool{}
	bad := false
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "@cert-authority") {
			caLines[i+1] = true
		}
		if _, _, _, _, _, err := ssh.ParseKnownHosts([]byte(trimmed)); err != nil {
			logger.Error("known_hosts", fmt.Sprintf("ignoring invalid line %d: %v", i+1, err))
			lines[i] = "" // blank, so line numbers stay the same
			bad = true
		}
	}
	if !bad {
		checker, err := knownhosts.New(path)
		return checker, caLines, err
	}
	// knownhosts only reads files: check against a cleaned copy.
	f, err := os.CreateTemp(filepath.Dir(path), ".known_hosts-*") // mode 0600
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(f.Name())
	_, err = io.WriteString(f, strings.Join(lines, "\n"))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, nil, err
	}
	checker, err := knownhosts.New(f.Name())
	return checker, caLines, err
}

// plainKeys drops keys that come from @cert-authority lines.
func plainKeys(keys []knownhosts.KnownKey, caLines map[int]bool) []knownhosts.KnownKey {
	var out []knownhosts.KnownKey
	for _, k := range keys {
		if !caLines[k.Line] {
			out = append(out, k)
		}
	}
	return out
}

// probeKey is a key that is never stored; checking it makes knownhosts
// report every key stored for a host.
var probeKey, _ = ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public())

// knownHostKeyAlgorithms returns the host key algorithms matching the keys
// stored for addr, or nil if the host is unknown. Offering only these makes
// the server present the key we already trust. Otherwise a server that adds
// a key of a type the client prefers (e.g. ECDSA next to a stored Ed25519
// key) would be reported as a host key mismatch.
func knownHostKeyAlgorithms(addr string) []string {
	khMu.Lock()
	defer khMu.Unlock()

	path, err := KnownHostsPath()
	if err != nil {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	checker, caLines, err := loadKnownHosts(path)
	if err != nil {
		return nil
	}
	var keyErr *knownhosts.KeyError
	if err := checker(addr, &net.TCPAddr{}, probeKey); !errors.As(err, &keyErr) {
		return nil
	}

	var algos []string
	seen := map[string]bool{}
	for _, k := range plainKeys(keyErr.Want, caLines) {
		typ := k.Key.Type()
		if seen[typ] {
			continue
		}
		seen[typ] = true
		if typ == ssh.KeyAlgoRSA {
			// An RSA key can be used with any of the RSA signature algorithms.
			algos = append(algos, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		} else {
			algos = append(algos, typ)
		}
	}
	return algos
}

// RemoveKnownHost deletes every line of the known_hosts file equal to line
// (ignoring surrounding whitespace). An empty line clears the whole file.
func RemoveKnownHost(line string) error {
	khMu.Lock()
	defer khMu.Unlock()

	path, err := KnownHostsPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var out []string
	if line != "" {
		want := strings.TrimSpace(line)
		for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if strings.TrimSpace(l) != want {
				out = append(out, l)
			}
		}
	}
	content := strings.Join(out, "\n")
	if content != "" {
		content += "\n"
	}
	return writeFileAtomic(path, []byte(content))
}

// writeFileAtomic replaces path via a temp file in the same directory, so a
// crash cannot leave a truncated file behind.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*") // mode 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ensureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	return f.Close()
}

// appendKnownHost is only called for hosts checkHostKey reported as unknown,
// with khMu held, so the entry cannot already exist.
func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return err
	}

	line := knownhosts.Line([]string{hostname}, key) + "\n"
	// A file edited by hand may lack the final newline; appending would
	// then merge our entry into its last line.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			line = "\n" + line
		}
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

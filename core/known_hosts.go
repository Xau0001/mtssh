package core

import (
	"crypto/ed25519"
	"errors"
	"fmt"
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
func KnownHostsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".mtssh", "known_hosts")
}

// BuildHostKeyCallback returns an ssh.HostKeyCallback that:
//  1. Accepts known hosts from ~/.mtssh/known_hosts
//  2. Calls prompt for unknown hosts and appends accepted keys
//  3. Rejects changed host keys (MITM protection)
//
// khMu is held across the prompt so two connections to the same new host
// cannot both append a key.
func BuildHostKeyCallback(prompt HostKeyPrompt) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		khMu.Lock()
		defer khMu.Unlock()

		path := KnownHostsPath()

		// Ensure the file exists so knownhosts.New doesn't fail
		if err := ensureFile(path); err != nil {
			return err
		}

		checker, err := knownhosts.New(path)
		if err != nil {
			return fmt.Errorf("known_hosts: %w", err)
		}

		err = checker(hostname, remote, key)
		if err == nil {
			// Known and matches — all good
			return nil
		}

		// Check if it's a key-mismatch (potential MITM)
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return fmt.Errorf("known_hosts: %w", err)
		}
		if len(keyErr.Want) > 0 {
			return fmt.Errorf(
				"HOST KEY MISMATCH for %s!\nExpected: %s\nGot: %s\n⚠ Possible MITM attack!",
				hostname,
				ssh.FingerprintSHA256(keyErr.Want[0].Key),
				ssh.FingerprintSHA256(key),
			)
		}

		// Unknown host — ask user
		fp := ssh.FingerprintSHA256(key)
		decision := prompt(hostname, key.Type(), fp)
		if decision != HostKeyAccept {
			return fmt.Errorf("host key rejected by user for %s", hostname)
		}

		// Persist the accepted key
		if err := appendKnownHost(path, hostname, key); err != nil {
			return fmt.Errorf("could not save host key: %w", err)
		}
		return nil
	}
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

	path := KnownHostsPath()
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	checker, err := knownhosts.New(path)
	if err != nil {
		return nil
	}
	var keyErr *knownhosts.KeyError
	if err := checker(addr, &net.TCPAddr{}, probeKey); !errors.As(err, &keyErr) {
		return nil
	}

	var algos []string
	seen := map[string]bool{}
	for _, k := range keyErr.Want {
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

	path := KnownHostsPath()
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

// appendKnownHost is only called for hosts the checker reported as unknown,
// with khMu held, so the entry cannot already exist.
func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}

	line := knownhosts.Line([]string{hostname}, key) + "\n"
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

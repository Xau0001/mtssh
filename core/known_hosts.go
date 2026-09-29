package core

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

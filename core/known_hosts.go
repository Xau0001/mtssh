package core

import (
	"bufio"
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

func knownHostsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mtputty", "known_hosts")
}

// BuildHostKeyCallback returns an ssh.HostKeyCallback that:
//  1. Accepts known hosts from ~/.mtputty/known_hosts
//  2. Calls prompt for unknown hosts and appends accepted keys
//  3. Rejects changed host keys (MITM protection)
func BuildHostKeyCallback(prompt HostKeyPrompt) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		khMu.Lock()
		defer khMu.Unlock()

		path := knownHostsPath()

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
		if isKeyError(err, &keyErr) && len(keyErr.Want) > 0 {
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
		if decision == HostKeyReject {
			return fmt.Errorf("host key rejected by user for %s", hostname)
		}

		// Persist the accepted key
		if err := appendKnownHost(path, hostname, key); err != nil {
			return fmt.Errorf("could not save host key: %w", err)
		}
		return nil
	}
}

func isKeyError(err error, out **knownhosts.KeyError) bool {
	if ke, ok := err.(*knownhosts.KeyError); ok {
		*out = ke
		return true
	}
	return false
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

func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	// Check for duplicates before appending
	existing, _ := readLines(path)
	marker := knownhosts.Normalize(hostname)
	for _, line := range existing {
		if strings.HasPrefix(line, marker) {
			return nil // already present
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	line := knownhosts.Line([]string{hostname}, key) + "\n"
	_, err = f.WriteString(line)
	return err
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

package config

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrAlreadyRunning is returned by Lock when another MTSSH process uses the
// session store.
var ErrAlreadyRunning = errors.New("MTSSH is already running")

// errLocked is returned by lockFile when another process holds the lock.
var errLocked = errors.New("locked")

// lockHandle stays open for the life of the process; the OS drops the lock
// when the process exits, even after a crash.
var lockHandle *os.File

// Lock makes this process the only one using the session store. Two
// instances would each save their own copy of the sessions, and the last
// one to save would silently discard the other's changes.
func Lock() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "sessions.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, errLocked) {
			return ErrAlreadyRunning
		}
		return err
	}
	lockHandle = f
	return nil
}

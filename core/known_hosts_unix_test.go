//go:build !windows

package core

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLoadKnownHostsReadsFileOnce(t *testing.T) {
	testHome(t)
	path := khPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// A FIFO has its content only once: reading the file a second time
	// (knownhosts opening it again) would block.
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		// Release an open still waiting for the other side.
		for _, flag := range []int{os.O_WRONLY, os.O_RDONLY} {
			if f, err := os.OpenFile(path, flag|syscall.O_NONBLOCK, 0); err == nil {
				f.Close()
			}
		}
	})
	key := newHostKey(t)
	go func() {
		if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			f.WriteString("known.example " + authorized(key) + "\n")
			f.Close()
		}
	}()

	done := make(chan error, 1)
	go func() {
		khMu.Lock()
		defer khMu.Unlock()
		db, err := loadKnownHosts(path)
		if err == nil {
			err = db.check("known.example:22", khRemote, key)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("known_hosts was read a second time")
	}
}

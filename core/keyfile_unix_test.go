//go:build !windows

package core

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadKeyFileFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "id")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// If the open blocked, a writer would release it.
	t.Cleanup(func() {
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})

	done := make(chan error, 2)
	go func() {
		// Opening directly, as after the key was swapped for a FIFO
		// between a stat and the open.
		f, err := openKeyFile(fifo)
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	go func() {
		_, err := readKeyFile(fifo)
		if err == nil {
			t.Error("readKeyFile accepted a FIFO")
		}
		done <- nil
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("open: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("opening a FIFO key blocked")
		}
	}
}

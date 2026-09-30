//go:build unix

package core

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSFTPProbeUmask(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	for _, mask := range []int{0o022, 0o077, 0o027, 0o002, 0} {
		syscall.Umask(mask)
		if got := sftpProbeUmask(); got != os.FileMode(mask) {
			t.Errorf("umask %o probed as %o", mask, got)
		}
	}
}

// The in-process server shares the test's umask, so a restrictive umask
// here is a restrictive server umask.
func TestSFTPUploadServerUmask(t *testing.T) {
	sftpUmask() // learn the real umask before changing it
	c := newTestSFTP(t)
	dir := t.TempDir()
	local := filepath.Join(dir, "local")
	sftpWriteFile(t, local, []byte("#!/bin/sh\n"), 0o755)

	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	remote := filepath.Join(dir, "script")
	if err := c.Upload(context.Background(), local, remote, nil); err != nil {
		t.Fatal(err)
	}
	sftpCheckFile(t, remote, []byte("#!/bin/sh\n"), 0o700)
}

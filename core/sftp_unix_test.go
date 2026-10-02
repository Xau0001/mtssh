//go:build unix

package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// A FIFO (or a link to one) is listed as such and refused without being
// opened: opening it blocks the server until a writer comes. A link to a
// regular file stays downloadable.
func TestSFTPDownloadRefusesFIFO(t *testing.T) {
	c := newTestSFTP(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no FIFO: %v", err)
	}
	file := filepath.Join(dir, "file")
	sftpWriteFile(t, file, []byte("data"), 0o600)
	os.Symlink(fifo, filepath.Join(dir, "fifolink"))
	os.Symlink(file, filepath.Join(dir, "filelink"))

	entries, err := c.ListDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		pipe := e.Mode&os.ModeNamedPipe != 0
		switch {
		case e.Name == "fifo" && !pipe,
			e.Name == "fifolink" && (!e.IsLink || !pipe),
			e.Name == "filelink" && (!e.IsLink || !e.Mode.IsRegular()):
			t.Errorf("%s: %+v", e.Name, e)
		}
	}

	sftpWithin(t, 10*time.Second, func() {
		for _, name := range []string{"fifo", "fifolink"} {
			d, err := c.OpenDownload(context.Background(), filepath.Join(dir, name))
			if err == nil {
				d.Close()
				t.Errorf("OpenDownload(%s) succeeded", name)
			} else if !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("OpenDownload(%s): %v", name, err)
			}
		}
	})
	d, err := c.OpenDownload(context.Background(), filepath.Join(dir, "filelink"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "copy")
	if err := d.SaveTo(context.Background(), target, nil); err != nil {
		t.Fatal(err)
	}
	sftpCheckFile(t, target, []byte("data"), 0o600)
}

// sftpOtherGroup returns a group other than the process's own that the
// test may give its files: any as root, else a supplementary group.
func sftpOtherGroup(t *testing.T) int {
	if os.Geteuid() == 0 {
		return 4242
	}
	groups, _ := os.Getgroups()
	for _, g := range groups {
		if g != os.Getgid() {
			return g
		}
	}
	t.Skip("needs root or a supplementary group")
	return 0
}

// Replacing a file of another group: the group permissions stay only if
// the new file could keep that group, even when the owner could not be
// kept (only root can give a file away).
func TestSFTPUploadKeepsGroup(t *testing.T) {
	gid := sftpOtherGroup(t)
	c := newTestSFTP(t)
	dir := t.TempDir()
	local := filepath.Join(dir, "local")
	sftpWriteFile(t, local, []byte("new"), 0o644)
	remote := filepath.Join(dir, "remote")
	for _, tc := range []struct {
		name      string
		refused   string // chowns the server refuses
		keepGroup bool
	}{
		{"owner not kept", "chown", true},
		{"group not kept", "chown chgrp", false},
	} {
		sftpWriteFile(t, remote, []byte("old"), 0o660)
		if err := os.Chown(remote, -1, gid); err != nil {
			t.Fatal(err)
		}
		c.testFail = func(op, p string) error {
			if strings.Contains(tc.refused, op) {
				return os.ErrPermission
			}
			return nil
		}
		if err := c.Upload(context.Background(), local, remote, nil); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		fi, err := os.Stat(remote)
		if err != nil {
			t.Fatal(err)
		}
		got := int(fi.Sys().(*syscall.Stat_t).Gid)
		switch {
		case tc.keepGroup && (got != gid || fi.Mode().Perm() != 0o660):
			t.Errorf("%s: group %d, mode %v; want group %d, mode 0660", tc.name, got, fi.Mode().Perm(), gid)
		case !tc.keepGroup && (got == gid || fi.Mode().Perm() != 0o600):
			t.Errorf("%s: group %d, mode %v; want another group than %d, mode 0600", tc.name, got, fi.Mode().Perm(), gid)
		}
	}
}

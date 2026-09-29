package core

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pkg/sftp"
)

// newTestSFTP connects to an in-process SFTP server on the local file system.
func newTestSFTP(t *testing.T) *SFTPClient {
	t.Helper()
	c1, c2 := net.Pipe()
	srv, err := sftp.NewServer(c1)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	client, err := sftp.NewClientPipe(c2, c2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		srv.Close()
	})
	return &SFTPClient{client: client}
}

func TestSFTPTransfers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix paths and permissions")
	}
	c := newTestSFTP(t)
	dir := t.TempDir()
	local := filepath.Join(dir, "local.txt")
	remote := filepath.Join(dir, "remote.txt")
	os.WriteFile(local, []byte("secret"), 0600)
	os.WriteFile(remote, []byte("old remote"), 0644)

	// Upload replaces the existing file and keeps the private mode.
	if err := c.Upload(context.Background(), local, remote, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(remote); string(data) != "secret" {
		t.Fatalf("remote content %q", data)
	}
	if fi, _ := os.Stat(remote); fi.Mode().Perm() != 0600 {
		t.Fatalf("remote mode %v, want 0600", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("temp file left behind: %v", entries)
	}

	// Download replaces the local target with the remote content.
	target := filepath.Join(dir, "download.txt")
	os.WriteFile(target, []byte("old local"), 0644)
	d, err := c.OpenDownload(remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveTo(context.Background(), target, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "secret" {
		t.Fatalf("downloaded %q", data)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0600 {
		t.Fatalf("local mode %v, want 0600", fi.Mode().Perm())
	}

	// A cancelled download leaves the existing file alone.
	os.WriteFile(target, []byte("keep me"), 0644)
	d, err = c.OpenDownload(remote)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.SaveTo(ctx, target, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled download: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "keep me" {
		t.Fatalf("target changed by a cancelled download: %q", data)
	}

	// Only regular files can be downloaded.
	os.Symlink("/dev/zero", filepath.Join(dir, "zero"))
	for _, p := range []string{dir, filepath.Join(dir, "zero")} {
		if d, err := c.OpenDownload(p); err == nil {
			d.Close()
			t.Errorf("OpenDownload(%s) succeeded", p)
		}
	}
}

func TestSFTPListDirResolvesLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	c := newTestSFTP(t)
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "real"), 0700)
	os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link"))
	entries, err := c.ListDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "link" && (!e.IsLink || !e.IsDir) {
			t.Fatalf("link to directory: %+v", e)
		}
	}
}

package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	client, err := sftp.NewClientPipe(c2, c2, sftpClientOptions...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		srv.Close()
	})
	return &SFTPClient{client: client}
}

// sftpLeftovers returns the names of staging directories and temp files
// in dir.
func sftpLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mtssh-") {
			names = append(names, e.Name())
		}
	}
	return names
}

// sftpWriteFile writes a test file with exactly mode perm (not umasked).
func sftpWriteFile(t *testing.T, name string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, data, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, perm); err != nil {
		t.Fatal(err)
	}
}

// sftpCheckFile fails unless name holds data with mode perm.
func sftpCheckFile(t *testing.T, name string, data []byte, perm os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("%s holds %d bytes %.20q, want %d bytes %.20q", filepath.Base(name), len(got), got, len(data), data)
	}
	fi, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode() != perm {
		t.Errorf("%s has mode %v, want %v", filepath.Base(name), fi.Mode(), perm)
	}
}

func TestSFTPTransfers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix paths and permissions")
	}
	c := newTestSFTP(t)
	dir := t.TempDir()
	local := filepath.Join(dir, "local.txt")
	remote := filepath.Join(dir, "remote.txt")
	sftpWriteFile(t, local, []byte("secret"), 0644)
	sftpWriteFile(t, remote, []byte("old remote"), 0600)

	// Upload replaces the existing file and keeps its private mode.
	if err := c.Upload(context.Background(), local, remote, nil); err != nil {
		t.Fatal(err)
	}
	sftpCheckFile(t, remote, []byte("secret"), 0600)
	if left := sftpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}

	// Download replaces the local target with the remote content.
	target := filepath.Join(dir, "download.txt")
	sftpWriteFile(t, target, []byte("old local"), 0644)
	d, err := c.OpenDownload(remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveTo(context.Background(), target, nil); err != nil {
		t.Fatal(err)
	}
	sftpCheckFile(t, target, []byte("secret"), 0600)

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
	if left := sftpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
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
	os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "dangling"))
	// More links than workers, each to a file of its own size.
	for i := range 30 {
		name := filepath.Join(dir, "f"+string(rune('a'+i)))
		os.WriteFile(name, make([]byte, i+1), 0600)
		os.Symlink(name, filepath.Join(dir, "l"+string(rune('a'+i))))
	}
	raw, err := c.client.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := c.ListDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(raw) {
		t.Fatalf("%d entries, want %d", len(entries), len(raw))
	}
	for i, e := range entries {
		if e.Name != raw[i].Name() {
			t.Fatalf("entry %d is %s, want %s: order not kept", i, e.Name, raw[i].Name())
		}
		switch {
		case e.Name == "link" && (!e.IsLink || !e.IsDir):
			t.Errorf("link to directory: %+v", e)
		case e.Name == "dangling" && (!e.IsLink || e.IsDir):
			t.Errorf("dangling link: %+v", e)
		case e.Name[0] == 'l' && len(e.Name) == 2 && (!e.IsLink || e.Size != int64(e.Name[1]-'a'+1)):
			t.Errorf("link to file: %+v", e)
		case e.Name[0] == 'f' && e.IsLink:
			t.Errorf("file: %+v", e)
		}
	}
}

func TestSFTPUploadThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	c := newTestSFTP(t)
	dir := t.TempDir()
	sub := filepath.Join(dir, "dotfiles")
	os.Mkdir(sub, 0700)
	local := filepath.Join(dir, "local")
	sftpWriteFile(t, local, []byte("new"), 0644)
	realFile := filepath.Join(sub, "bashrc")
	for _, l := range []struct{ name, dest string }{
		{"abs", realFile},          // absolute link
		{"rel", "dotfiles/bashrc"}, // relative link
		{"chain", "abs"},           // link to a link
	} {
		name, dest := l.name, l.dest
		link := filepath.Join(dir, name)
		if err := os.Symlink(dest, link); err != nil {
			t.Fatal(err)
		}
		sftpWriteFile(t, realFile, []byte("old"), 0640)
		if err := c.Upload(context.Background(), local, link, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sftpCheckFile(t, realFile, []byte("new"), 0640)
		if got, err := os.Readlink(link); err != nil || got != dest {
			t.Errorf("%s: link now %q, %v", name, got, err)
		}
		if left := append(sftpLeftovers(t, dir), sftpLeftovers(t, sub)...); len(left) != 0 {
			t.Fatalf("%s: left behind: %v", name, left)
		}
	}
}

func TestSFTPUploadRefusesNonRegular(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	c := newTestSFTP(t)
	dir := t.TempDir()
	local := filepath.Join(dir, "local")
	sftpWriteFile(t, local, []byte("new"), 0644)
	os.Mkdir(filepath.Join(dir, "dir"), 0700)
	os.Symlink(filepath.Join(dir, "dir"), filepath.Join(dir, "dirlink"))
	os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "dangling"))
	os.Symlink("loop", filepath.Join(dir, "loop"))
	for _, name := range []string{"dir", "dirlink", "dangling", "loop"} {
		if err := c.Upload(context.Background(), local, filepath.Join(dir, name), nil); err == nil {
			t.Errorf("upload onto %s succeeded", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dangling link followed: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "dir")); len(entries) != 0 {
		t.Errorf("written into the directory: %v", entries)
	}
	if left := sftpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

func TestSFTPUploadStaging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix permissions")
	}
	c := newTestSFTP(t)
	dir := t.TempDir()
	local := filepath.Join(dir, "local")
	data := bytes.Repeat([]byte("x"), 1<<20)
	sftpWriteFile(t, local, data, 0600)
	remote := filepath.Join(dir, "remote")
	sftpWriteFile(t, remote, []byte("old"), 0644)

	// The data is only ever in a private staging directory.
	checked := false
	err := c.Upload(context.Background(), local, remote, func(done, total int64) {
		if checked {
			return
		}
		checked = true
		left := sftpLeftovers(t, dir)
		if len(left) != 1 {
			t.Fatalf("staging directories: %v", left)
		}
		stage := filepath.Join(dir, left[0])
		if fi, err := os.Stat(stage); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("staging directory: %v, %v", fi.Mode(), err)
		}
		if _, err := os.Stat(filepath.Join(stage, sftpStageData)); err != nil {
			t.Errorf("no data file in the staging directory: %v", err)
		}
	})
	if err != nil || !checked {
		t.Fatal(err, checked)
	}
	sftpCheckFile(t, remote, data, 0644)

	// Cancelled: the target is unchanged and nothing is left.
	sftpWriteFile(t, remote, []byte("old"), 0644)
	ctx, cancel := context.WithCancel(context.Background())
	err = c.Upload(ctx, local, remote, func(done, total int64) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upload: %v", err)
	}
	sftpCheckFile(t, remote, []byte("old"), 0644)
	if left := sftpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("left behind after cancel: %v", left)
	}

	// A new file gets the local mode, within the server's umask.
	fresh := filepath.Join(dir, "fresh")
	sftpWriteFile(t, local, []byte("run"), 0o777)
	if err := c.Upload(context.Background(), local, fresh, nil); err != nil {
		t.Fatal(err)
	}
	// The in-process server creates files with 0644 minus the umask.
	sftpCheckFile(t, fresh, []byte("run"), sftpUploadMode(0o777, 0o644&^sftpUmask()))
	if left := sftpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

func TestSFTPUploadWithoutPosixRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix paths")
	}
	c := newTestSFTP(t)
	c.noPosixRename = true
	dir := t.TempDir()
	local := filepath.Join(dir, "local")
	sftpWriteFile(t, local, []byte("new"), 0644)
	remote := filepath.Join(dir, "remote")

	// Works for new and existing files.
	for range 2 {
		if err := c.Upload(context.Background(), local, remote, nil); err != nil {
			t.Fatal(err)
		}
		sftpCheckFile(t, remote, []byte("new"), sftpUploadMode(0o644, 0o644&^sftpUmask()))
	}

	// The new file cannot take the old one's place: the old one is moved back.
	sftpWriteFile(t, remote, []byte("original"), 0600)
	failing := map[string]bool{sftpStageData: true}
	c.testFail = func(op, p string) error {
		if op == "rename" && failing[filepath.Base(p)] {
			return errors.New("rename refused")
		}
		return nil
	}
	if err := c.Upload(context.Background(), local, remote, nil); err == nil {
		t.Fatal("upload succeeded although the rename failed")
	}
	sftpCheckFile(t, remote, []byte("original"), 0600)
	if left := sftpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}

	// Moving it back fails too: the staging directory stays with the
	// original, and the error says where it is.
	failing[sftpStageOld] = true
	err := c.Upload(context.Background(), local, remote, nil)
	left := sftpLeftovers(t, dir)
	if err == nil || len(left) != 1 {
		t.Fatalf("error %v, left %v", err, left)
	}
	old := filepath.Join(dir, left[0], sftpStageOld)
	tmp := filepath.Join(dir, left[0], sftpStageData)
	if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), tmp) {
		t.Errorf("error does not name both files: %v", err)
	}
	sftpCheckFile(t, old, []byte("original"), 0600)
	if data, _ := os.ReadFile(tmp); string(data) != "new" {
		t.Errorf("new file: %q", data)
	}
}

func TestSFTPUploadWithoutStaging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix paths and permissions")
	}
	c := newTestSFTP(t)
	c.testFail = func(op, p string) error {
		if op == "mkdir" {
			return os.ErrPermission
		}
		return nil
	}
	dir := t.TempDir()
	local := filepath.Join(dir, "local")
	sftpWriteFile(t, local, []byte("new"), 0o755)

	// An existing file is written in place: mode and hard links stay.
	remote := filepath.Join(dir, "remote")
	sftpWriteFile(t, remote, []byte("old content"), 0640)
	if err := os.Link(remote, filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(context.Background(), local, remote, nil); err != nil {
		t.Fatal(err)
	}
	sftpCheckFile(t, remote, []byte("new"), 0640)
	sftpCheckFile(t, filepath.Join(dir, "hardlink"), []byte("new"), 0640)

	// A new file is created directly...
	fresh := filepath.Join(dir, "fresh")
	if err := c.Upload(context.Background(), local, fresh, nil); err != nil {
		t.Fatal(err)
	}
	sftpCheckFile(t, fresh, []byte("new"), sftpUploadMode(0o755, 0o644&^sftpUmask()))

	// ... and removed again if the upload fails.
	sftpWriteFile(t, local, bytes.Repeat([]byte("x"), 1<<20), 0644)
	ctx, cancel := context.WithCancel(context.Background())
	partial := filepath.Join(dir, "partial")
	err := c.Upload(ctx, local, partial, func(done, total int64) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upload: %v", err)
	}
	if _, err := os.Lstat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial file left: %v", err)
	}
}

func TestSFTPDownloadSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("changes a file that is open")
	}
	c := newTestSFTP(t)
	dir := t.TempDir()
	remote := filepath.Join(dir, "log")
	target := filepath.Join(dir, "copy")
	data := make([]byte, 4<<20)
	for i := range data {
		data[i] = byte(i * 7 / 5)
	}
	more := bytes.Repeat([]byte("appended line\n"), 5000)

	// Growing: the size is read again when SaveTo starts, and data
	// appended during the download is not read.
	if err := os.WriteFile(remote, data, 0644); err != nil {
		t.Fatal(err)
	}
	d, err := c.OpenDownload(remote)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(remote, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(more)
	calls := 0
	var first, last int64
	err = d.SaveTo(context.Background(), target, func(done int64) {
		if calls == 0 {
			first = d.Size
			f.Write(more)
		}
		calls++ // unsynchronized: the race detector checks the calls are not concurrent
		last = done
	})
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, data...), more...)
	if first != int64(len(want)) || last != int64(len(want)) {
		t.Errorf("size at the first progress call %d, last progress %d, want %d", first, last, len(want))
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, want) {
		t.Errorf("downloaded %d bytes, want the %d of the snapshot", len(got), len(want))
	}

	// Shrinking: the copy ends where the file ended.
	if err := os.WriteFile(remote, data, 0644); err != nil {
		t.Fatal(err)
	}
	d, err = c.OpenDownload(remote)
	if err != nil {
		t.Fatal(err)
	}
	calls = 0
	err = d.SaveTo(context.Background(), target, func(done int64) {
		if calls == 0 {
			os.Truncate(remote, 100000)
		}
		calls++
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if len(got) < 100000 || len(got) >= len(data) || !bytes.Equal(got, data[:len(got)]) {
		t.Errorf("after shrinking: %d bytes, want a prefix of at least 100000", len(got))
	}
	if left := sftpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

func TestSFTPDownloadCancel(t *testing.T) {
	c := newTestSFTP(t)
	dir := t.TempDir()
	remote := filepath.Join(dir, "big")
	target := filepath.Join(dir, "copy")
	os.WriteFile(remote, make([]byte, 4<<20), 0644)
	os.WriteFile(target, []byte("keep me"), 0644)
	d, err := c.OpenDownload(remote)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := d.SaveTo(ctx, target, func(int64) { cancel() }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled download: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "keep me" {
		t.Fatalf("target changed: %d bytes", len(data))
	}
	if left := sftpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

func TestSFTPDownloadProcFile(t *testing.T) {
	const proc = "/proc/self/status"
	if _, err := os.Stat(proc); err != nil {
		t.Skip("no procfs")
	}
	c := newTestSFTP(t)
	d, err := c.OpenDownload(proc)
	if err != nil {
		t.Fatal(err)
	}
	if d.Size != 0 {
		t.Skipf("%s reports size %d", proc, d.Size)
	}
	target := filepath.Join(t.TempDir(), "status")
	var last int64
	if err := d.SaveTo(context.Background(), target, func(done int64) { last = done }); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	if !bytes.HasPrefix(data, []byte("Name:")) || int64(len(data)) != last {
		t.Errorf("downloaded %q, progress %d", data, last)
	}
}

func TestSFTPDownloadToSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	c := newTestSFTP(t)
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote")
	sftpWriteFile(t, remote, []byte("new"), 0644)
	sub := filepath.Join(dir, "dotfiles")
	os.Mkdir(sub, 0700)
	realFile := filepath.Join(sub, "bashrc")
	sftpWriteFile(t, realFile, []byte("old"), 0644)
	link := filepath.Join(dir, "link")
	os.Symlink("dotfiles/bashrc", link)
	os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "dangling"))

	save := func(localPath string) error {
		d, err := c.OpenDownload(remote)
		if err != nil {
			t.Fatal(err)
		}
		return d.SaveTo(context.Background(), localPath, nil)
	}
	if err := save(link); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(realFile); string(data) != "new" {
		t.Errorf("link target holds %q", data)
	}
	if dest, err := os.Readlink(link); err != nil || dest != "dotfiles/bashrc" {
		t.Errorf("link now %q, %v", dest, err)
	}
	for _, p := range []string{filepath.Join(dir, "dangling"), sub} {
		if err := save(p); err == nil {
			t.Errorf("download to %s succeeded", p)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dangling link followed: %v", err)
	}
	if left := append(sftpLeftovers(t, dir), sftpLeftovers(t, sub)...); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

func TestSFTPLocalMode(t *testing.T) {
	for _, tc := range []struct {
		remote, umask os.FileMode
		reported      bool
		want          os.FileMode
	}{
		{0o755, 0o077, true, 0o700},
		{0o755, 0o022, true, 0o755},
		{0o444, 0o022, true, 0o644},
		{0o777, 0o002, true, 0o755},
		{0o600, 0o022, true, 0o600},
		{0o000, 0o022, true, 0o600}, // a real 0000 file stays private
		{0o000, 0o022, false, 0o644},
		{0o000, 0o077, false, 0o600},
		{0o4755, 0o022, true, 0o755},
	} {
		if got := sftpLocalMode(tc.remote, tc.reported, tc.umask); got != tc.want {
			t.Errorf("sftpLocalMode(%o, %v, %o) = %o, want %o", tc.remote, tc.reported, tc.umask, got, tc.want)
		}
	}
}

func TestSFTPUploadMode(t *testing.T) {
	for _, tc := range []struct{ local, created, want os.FileMode }{
		{0o755, 0o644, 0o755},
		{0o755, 0o600, 0o700},
		{0o666, 0o644, 0o644},
		{0o600, 0o644, 0o600},
		{0o777, 0o664, 0o755},
		{0o640, 0o640, 0o640},
	} {
		if got := sftpUploadMode(tc.local, tc.created); got != tc.want {
			t.Errorf("sftpUploadMode(%o, %o) = %o, want %o", tc.local, tc.created, got, tc.want)
		}
	}
}

// SFTP packet types and status codes the fake server uses.
const (
	sftpFxpInit    = 1
	sftpFxpVersion = 2
	sftpFxpOpen    = 3
	sftpFxpClose   = 4
	sftpFxpRead    = 5
	sftpFxpFstat   = 8
	sftpFxpOpendir = 11
	sftpFxpReaddir = 12
	sftpFxpStat    = 17
	sftpFxpStatus  = 101
	sftpFxpHandle  = 102
	sftpFxpData    = 103
	sftpFxpName    = 104
	sftpFxpAttrs   = 105
	sftpFxOK       = 0
	sftpFxEOF      = 1
)

// sftpPacket builds an SFTP packet (without its length) from its type and
// fields: uint32, uint64, string (with its length) or raw []byte.
func sftpPacket(typ byte, fields ...any) []byte {
	b := []byte{typ}
	for _, f := range fields {
		switch v := f.(type) {
		case uint32:
			b = binary.BigEndian.AppendUint32(b, v)
		case uint64:
			b = binary.BigEndian.AppendUint64(b, v)
		case string:
			b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
			b = append(b, v...)
		case []byte:
			b = append(b, v...)
		default:
			panic("sftpPacket: bad field")
		}
	}
	return b
}

// sftpFakeServer starts a client with the production options against a
// fake server on a pipe. reply gets each request's type, id and body
// (after the type) and returns the reply packet, or nil for none; INIT and
// CLOSE are answered normally when it returns nil.
func sftpFakeServer(t *testing.T, reply func(typ byte, id uint32, req []byte) []byte) (*SFTPClient, error) {
	t.Helper()
	srv, cli := net.Pipe()
	t.Cleanup(func() {
		srv.Close()
		cli.Close()
	})
	go func() {
		for {
			var n [4]byte
			if _, err := io.ReadFull(srv, n[:]); err != nil {
				return
			}
			pkt := make([]byte, binary.BigEndian.Uint32(n[:]))
			if _, err := io.ReadFull(srv, pkt); err != nil || len(pkt) < 5 {
				return
			}
			id := binary.BigEndian.Uint32(pkt[1:5])
			out := reply(pkt[0], id, pkt[1:])
			switch {
			case out != nil:
			case pkt[0] == sftpFxpInit:
				out = sftpPacket(sftpFxpVersion, uint32(3))
			case pkt[0] == sftpFxpClose:
				out = sftpPacket(sftpFxpStatus, id, uint32(sftpFxOK), "", "")
			default:
				continue
			}
			if _, err := srv.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(out))), out...)); err != nil {
				return
			}
		}
	}()
	return sftpNewClient(func() (*sftp.Client, error) {
		return sftp.NewClientPipe(cli, cli, sftpClientOptions...)
	})
}

// sftpRegularAttrs are the attributes of a regular file of size bytes.
func sftpRegularAttrs(size uint64) []byte {
	return sftpPacket(0, uint32(1|4), size, uint32(0o100644))[1:]
}

// sftpFakeFile answers OPEN and FSTAT for a regular file of size bytes and
// READ with read(offset, length).
func sftpFakeFile(size uint64, read func(id uint32, off uint64, n uint32) []byte) func(byte, uint32, []byte) []byte {
	return func(typ byte, id uint32, req []byte) []byte {
		switch typ {
		case sftpFxpOpen:
			return sftpPacket(sftpFxpHandle, id, "h")
		case sftpFxpFstat:
			return sftpPacket(sftpFxpAttrs, id, sftpRegularAttrs(size))
		case sftpFxpRead:
			// id, handle "h" (4+1 bytes), offset, length
			return read(id, binary.BigEndian.Uint64(req[9:17]), binary.BigEndian.Uint32(req[17:21]))
		}
		return nil
	}
}

// A malicious server's replies make pkg/sftp panic; that must become an
// error, not end the program.
func TestSFTPMaliciousServer(t *testing.T) {
	t.Run("init", func(t *testing.T) {
		_, err := sftpFakeServer(t, func(typ byte, id uint32, req []byte) []byte {
			if typ == sftpFxpInit {
				// An extension name far longer than the packet.
				return sftpPacket(sftpFxpVersion, uint32(3), uint32(0xFFFF), []byte("ab"))
			}
			return nil
		})
		if err == nil {
			t.Fatal("no error")
		}
	})

	t.Run("readdir", func(t *testing.T) {
		c, err := sftpFakeServer(t, func(typ byte, id uint32, req []byte) []byte {
			switch typ {
			case sftpFxpOpendir:
				return sftpPacket(sftpFxpHandle, id, "d")
			case sftpFxpReaddir:
				// One entry whose name is far longer than the packet.
				return sftpPacket(sftpFxpName, id, uint32(1), uint32(0xFFFF), []byte("ab"))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.ListDir("/"); err == nil || !strings.Contains(err.Error(), "invalid reply") {
			t.Fatalf("ListDir: %v", err)
		}
		// The connection is closed: later calls fail instead of hanging.
		if _, err := c.Getwd(); err == nil {
			t.Fatal("Getwd succeeded after the connection was closed")
		}
	})

	t.Run("stat of a link", func(t *testing.T) {
		readdirs := 0
		c, err := sftpFakeServer(t, func(typ byte, id uint32, req []byte) []byte {
			switch typ {
			case sftpFxpOpendir:
				return sftpPacket(sftpFxpHandle, id, "d")
			case sftpFxpReaddir:
				if readdirs++; readdirs > 1 {
					return sftpPacket(sftpFxpStatus, id, uint32(sftpFxEOF), "", "")
				}
				return sftpPacket(sftpFxpName, id, uint32(1), "link", "link", uint32(4), uint32(0o120777))
			case sftpFxpStat:
				// A status without its code: pkg/sftp reads past the end.
				return sftpPacket(sftpFxpStatus, id)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.ListDir("/"); err == nil || !strings.Contains(err.Error(), "invalid reply") {
			t.Fatalf("ListDir: %v", err)
		}
	})

	t.Run("read length beyond the packet", func(t *testing.T) {
		c, err := sftpFakeServer(t, sftpFakeFile(100000, func(id uint32, off uint64, n uint32) []byte {
			return sftpPacket(sftpFxpData, id, uint32(0xFFFFFF), make([]byte, 10))
		}))
		if err != nil {
			t.Fatal(err)
		}
		d, err := c.OpenDownload("/f")
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := d.SaveTo(context.Background(), filepath.Join(dir, "f"), nil); err == nil {
			t.Fatal("SaveTo succeeded")
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("left behind: %v", entries)
		}
	})

	t.Run("read more than requested", func(t *testing.T) {
		const size = 100000
		c, err := sftpFakeServer(t, sftpFakeFile(size, func(id uint32, off uint64, n uint32) []byte {
			return sftpPacket(sftpFxpData, id, n+8000, bytes.Repeat([]byte{'x'}, int(n+8000)))
		}))
		if err != nil {
			t.Fatal(err)
		}
		d, err := c.OpenDownload("/f")
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "f")
		if err := d.SaveTo(context.Background(), target, nil); err != nil {
			return // an error is fine too; not crashing is the point
		}
		if fi, err := os.Stat(target); err != nil || fi.Size() != size {
			t.Fatalf("downloaded %v, %v; want %d bytes", fi, err, size)
		}
	})

	t.Run("endless file without size", func(t *testing.T) {
		c, err := sftpFakeServer(t, sftpFakeFile(0, func(id uint32, off uint64, n uint32) []byte {
			return sftpPacket(sftpFxpData, id, n, make([]byte, n))
		}))
		if err != nil {
			t.Fatal(err)
		}
		d, err := c.OpenDownload("/f")
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		var last int64
		err = d.SaveTo(context.Background(), filepath.Join(dir, "f"), func(done int64) { last = done })
		if err == nil || last > sftpMaxUnsizedDownload {
			t.Fatalf("SaveTo: %v after %d bytes", err, last)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("left behind: %v", entries)
		}
	})
}

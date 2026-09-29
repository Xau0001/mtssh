package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SFTPClient wraps an sftp.Client for file operations
type SFTPClient struct {
	client *sftp.Client
}

// FileEntry is a simplified directory entry for the UI
type FileEntry struct {
	Name   string
	Size   int64
	IsDir  bool
	IsLink bool // symbolic link; IsDir and Size describe its target
	Mode   os.FileMode
}

// NewSFTPClient opens an SFTP subsystem over an existing ssh.Client
func NewSFTPClient(sshClient *ssh.Client) (*SFTPClient, error) {
	c, err := sftp.NewClient(sshClient)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return &SFTPClient{client: c}, nil
}

// ListDir returns the contents of a remote directory. Symbolic links are
// reported with their target's type and size (IsLink set), so links to
// directories can be opened.
func (s *SFTPClient) ListDir(dir string) ([]FileEntry, error) {
	infos, err := s.client.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	entries := make([]FileEntry, 0, len(infos))
	for _, info := range infos {
		e := FileEntry{
			Name:  info.Name(),
			Size:  info.Size(),
			IsDir: info.IsDir(),
			Mode:  info.Mode(),
		}
		if info.Mode()&os.ModeSymlink != 0 {
			e.IsLink = true
			if target, err := s.client.Stat(path.Join(dir, info.Name())); err == nil {
				e.IsDir, e.Size = target.IsDir(), target.Size()
			}
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Download is a remote file opened for downloading.
type Download struct {
	f    *sftp.File
	Size int64
	Mode os.FileMode
}

// OpenDownload opens a remote regular file. Anything else (a link to
// /dev/zero, a FIFO) is refused: it could stream data without end.
func (s *SFTPClient) OpenDownload(remotePath string) (*Download, error) {
	f, err := s.client.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("open remote %s: %w", remotePath, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat remote %s: %w", remotePath, err)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", remotePath)
	}
	return &Download{f: f, Size: fi.Size(), Mode: fi.Mode()}, nil
}

// Close releases the remote file.
func (d *Download) Close() error { return d.f.Close() }

// SaveTo writes the file to localPath and closes d. The data goes to a
// temp file next to localPath that replaces it only when complete, so a
// failed or cancelled download leaves no partial file. The copy is private
// (0600) while written, then gets the remote permissions without any write
// permission for group or others. More data than the size reported when the
// file was opened is an error.
func (d *Download) SaveTo(ctx context.Context, localPath string, progress func(done int64)) error {
	defer d.f.Close()
	tmp, err := os.CreateTemp(filepath.Dir(localPath), ".mtssh-download-*") // mode 0600
	if err != nil {
		return fmt.Errorf("create local file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once renamed

	w := &transferWriter{ctx: ctx, w: tmp, limit: d.Size, progress: progress}
	if _, err := d.f.WriteTo(w); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	// Close explicitly so write errors (e.g. disk full) are reported
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), d.Mode.Perm()&0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), localPath)
}

// Exists reports whether remotePath exists.
func (s *SFTPClient) Exists(remotePath string) (bool, error) {
	_, err := s.client.Lstat(remotePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Upload copies a local regular file to remotePath. Like SaveTo, it writes
// a temp file next to the target (private while written) and renames it
// over the target when complete. The remote copy gets the local file's
// permissions without write permission for group or others.
func (s *SFTPClient) Upload(ctx context.Context, localPath, remotePath string, progress func(done, total int64)) error {
	local, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local %s: %w", localPath, err)
	}
	defer local.Close()
	fi, err := local.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", localPath)
	}

	// A fresh name, created exclusively: nothing (not even a symlink planted
	// by someone else in a shared directory) may exist there yet.
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := path.Join(path.Dir(remotePath), "."+path.Base(remotePath)+".mtssh-"+hex.EncodeToString(suffix))
	remote, err := s.client.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return fmt.Errorf("create remote %s: %w", tmp, err)
	}
	done := false
	defer func() {
		if !done {
			s.client.Remove(tmp)
		}
	}()
	if err := remote.Chmod(0600); err != nil {
		remote.Close()
		return fmt.Errorf("chmod remote %s: %w", tmp, err)
	}
	var report func(int64)
	if progress != nil {
		report = func(n int64) { progress(n, fi.Size()) }
	}
	r := &transferReader{ctx: ctx, r: local, progress: report}
	if _, err := io.Copy(remote, r); err != nil {
		remote.Close()
		return err
	}
	// Close explicitly so SFTP flushes and reports any write errors
	if err := remote.Close(); err != nil {
		return err
	}
	if err := s.client.Chmod(tmp, fi.Mode().Perm()&0o755); err != nil {
		return fmt.Errorf("chmod remote %s: %w", tmp, err)
	}
	if err := s.replace(tmp, remotePath); err != nil {
		return err
	}
	done = true
	return nil
}

// replace renames tmp to target, replacing target if it exists.
func (s *SFTPClient) replace(tmp, target string) error {
	if _, ok := s.client.HasExtension("posix-rename@openssh.com"); ok {
		return s.client.PosixRename(tmp, target)
	}
	// Plain SFTP rename does not overwrite.
	if err := s.client.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.client.Rename(tmp, target)
}

// errTooLong is returned when a download delivers more than its size.
var errTooLong = errors.New("the file grew while it was downloaded")

// transferWriter passes writes on to w until ctx is cancelled or more than
// limit bytes arrive, and reports progress.
type transferWriter struct {
	ctx      context.Context
	w        io.Writer
	limit    int64
	done     int64
	progress func(int64)
}

func (t *transferWriter) Write(p []byte) (int, error) {
	if err := t.ctx.Err(); err != nil {
		return 0, err
	}
	if t.done+int64(len(p)) > t.limit {
		return 0, errTooLong
	}
	n, err := t.w.Write(p)
	t.done += int64(n)
	if t.progress != nil {
		t.progress(t.done)
	}
	return n, err
}

// transferReader reads from r until ctx is cancelled and reports progress.
type transferReader struct {
	ctx      context.Context
	r        io.Reader
	done     int64
	progress func(int64)
}

func (t *transferReader) Read(p []byte) (int, error) {
	if err := t.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := t.r.Read(p)
	t.done += int64(n)
	if t.progress != nil && n > 0 {
		t.progress(t.done)
	}
	return n, err
}

// Delete removes a remote file or empty directory
func (s *SFTPClient) Delete(remotePath string) error {
	return s.client.Remove(remotePath)
}

// Mkdir creates a remote directory
func (s *SFTPClient) Mkdir(remotePath string) error {
	return s.client.MkdirAll(remotePath)
}

// Rename moves/renames a remote path
func (s *SFTPClient) Rename(oldPath, newPath string) error {
	return s.client.Rename(oldPath, newPath)
}

// Getwd returns the remote working directory
func (s *SFTPClient) Getwd() (string, error) {
	return s.client.Getwd()
}

// Close closes the SFTP connection
func (s *SFTPClient) Close() {
	if s.client != nil {
		s.client.Close()
	}
}

package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mtssh/logger"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	// sftpChunkSize is the size of one READ request of a download. It is
	// not above pkg/sftp's maxPacket (32 KiB), so pkg/sftp sends it as one
	// request and parses the reply in the goroutine that asked.
	sftpChunkSize = 32 << 10
	// sftpReadWorkers is how many READ requests a download keeps in flight.
	sftpReadWorkers = 16
	// sftpMaxUnsizedDownload limits files the server reports with size 0
	// (procfs, sysfs), which are read until EOF.
	sftpMaxUnsizedDownload = 16 << 20
	// sftpStatWorkers is how many symbolic links ListDir resolves at a time.
	sftpStatWorkers = 8
	// sftpListTimeout bounds reading one directory.
	sftpListTimeout = 2 * time.Minute
	// sftpMaxLinkHops limits how many symbolic links Upload follows.
	sftpMaxLinkHops = 16
)

// Names in an upload's staging directory (see Upload).
const (
	sftpStagePrefix = ".mtssh-"
	sftpStageData   = "upload"
	sftpStageOld    = "old"
)

// sftpClientOptions are the pkg/sftp options of every connection. With
// concurrent reads off, pkg/sftp parses READ replies in the goroutine that
// asked, where sftpRecover can catch its panics; SaveTo pipelines reads
// itself.
var sftpClientOptions = []sftp.ClientOption{sftp.UseConcurrentReads(false)}

// SFTPClient wraps an sftp.Client for file operations
type SFTPClient struct {
	client *sftp.Client

	// Test seams for Upload: noPosixRename makes it act as if the server
	// lacked posix-rename; testFail, if set, can make the staging Mkdir (op
	// "mkdir") and the plain renames (op "rename", with the old path) fail.
	noPosixRename bool
	testFail      func(op, path string) error
}

// FileEntry is a simplified directory entry for the UI
type FileEntry struct {
	Name   string
	Size   int64
	IsDir  bool
	IsLink bool // symbolic link; IsDir and Size describe its target
	Mode   os.FileMode
}

// sftpRecover turns a panic into an error: use it as
// defer sftpRecover(c, &err) in every goroutine that talks to the server.
// pkg/sftp slices server-supplied lengths without checking them, so a
// broken or malicious server can make it panic, which would end the whole
// program. The connection c (may be nil) is closed, as its state is unknown;
// later calls then fail cleanly.
func sftpRecover(c *sftp.Client, err *error) {
	p := recover()
	if p == nil {
		return
	}
	if c != nil {
		// Close waits for the server to end the session: don't block on it.
		go c.Close()
	}
	*err = fmt.Errorf("SFTP server sent an invalid reply (%v); connection closed", p)
	logger.Error("sftp", (*err).Error())
}

// NewSFTPClient opens an SFTP subsystem over an existing ssh.Client
func NewSFTPClient(sshClient *ssh.Client) (*SFTPClient, error) {
	return sftpNewClient(func() (*sftp.Client, error) {
		return sftp.NewClient(sshClient, sftpClientOptions...)
	})
}

// sftpNewClient wraps the client that open starts (tests start one on a
// pipe).
func sftpNewClient(open func() (*sftp.Client, error)) (_ *SFTPClient, err error) {
	defer sftpRecover(nil, &err)
	c, err := open()
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return &SFTPClient{client: c}, nil
}

// ListDir returns the contents of a remote directory. Symbolic links are
// reported with their target's type and size (IsLink set), so links to
// directories can be opened. Reading the directory times out after
// sftpListTimeout.
//
// The number of entries is not limited yet: pkg/sftp's ReadDirContext
// collects all of them before it returns, so a cap needs a change there.
func (s *SFTPClient) ListDir(dir string) (_ []FileEntry, err error) {
	defer sftpRecover(s.client, &err)
	ctx, cancel := context.WithTimeout(context.Background(), sftpListTimeout)
	defer cancel()
	infos, err := s.client.ReadDirContext(ctx, dir)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("reading %s timed out", dir)
	}
	if err != nil {
		return nil, err
	}
	entries := make([]FileEntry, len(infos))
	var links []int
	for i, info := range infos {
		entries[i] = FileEntry{
			Name:  info.Name(),
			Size:  info.Size(),
			IsDir: info.IsDir(),
			Mode:  info.Mode(),
		}
		if info.Mode()&os.ModeSymlink != 0 {
			entries[i].IsLink = true
			links = append(links, i)
		}
	}
	if err := s.resolveLinks(dir, entries, links); err != nil {
		return nil, err
	}
	return entries, nil
}

// resolveLinks gives the entries at the indexes in links their target's
// type and size, with up to sftpStatWorkers Stat requests in flight (one
// round trip each). A link that cannot be resolved keeps its own attributes.
func (s *SFTPClient) resolveLinks(dir string, entries []FileEntry, links []int) error {
	var next atomic.Int64
	var wg sync.WaitGroup
	errs := make([]error, min(sftpStatWorkers, len(links)))
	for w := range errs {
		wg.Go(func() {
			defer sftpRecover(s.client, &errs[w])
			for {
				k := int(next.Add(1) - 1)
				if k >= len(links) {
					return
				}
				e := &entries[links[k]]
				if target, err := s.client.Stat(path.Join(dir, e.Name)); err == nil {
					e.IsDir, e.Size = target.IsDir(), target.Size()
				}
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// Download is a remote file opened for downloading.
type Download struct {
	f     *sftp.File
	c     *sftp.Client // closed by sftpRecover after a panic
	perms bool         // the server reported the file's permissions
	Size  int64
	Mode  os.FileMode
}

// OpenDownload opens a remote regular file. Anything else (a link to
// /dev/zero, a FIFO) is refused: it could stream data without end.
func (s *SFTPClient) OpenDownload(remotePath string) (_ *Download, err error) {
	defer sftpRecover(s.client, &err)
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
	// Without the permissions attribute, the mode is 0 including the type
	// bits; a real 0000 file still has its regular-file type bit.
	st, _ := fi.Sys().(*sftp.FileStat)
	return &Download{f: f, c: s.client, perms: st != nil && st.Mode != 0, Size: fi.Size(), Mode: fi.Mode()}, nil
}

// Close releases the remote file.
func (d *Download) Close() (err error) {
	defer sftpRecover(d.c, &err)
	return d.f.Close()
}

// SaveTo writes the file to localPath and closes d. If localPath is a
// symbolic link, the file it points to is replaced and the link stays; a
// dangling link, a directory or another non-regular file is refused.
//
// The data goes to a temp file next to that target that replaces it only
// when complete, so a failed or cancelled download leaves the old file
// alone. The copy is private (0600) while written, then gets the mode from
// sftpLocalMode: the remote read and execute bits plus owner read-write,
// no write permission for group or others, the local umask applied.
//
// The download is a snapshot: the size is read again when SaveTo starts
// (d.Size is updated before the first progress call) and nothing past it
// is read, so a growing log file ends there; a file that shrinks meanwhile
// ends where it ended. Files the server reports with size 0 (procfs,
// sysfs) are read until EOF, up to sftpMaxUnsizedDownload. progress is only
// called from the calling goroutine.
func (d *Download) SaveTo(ctx context.Context, localPath string, progress func(done int64)) (err error) {
	defer sftpRecover(d.c, &err)
	defer d.f.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := sftpLocalTarget(localPath)
	if err != nil {
		return err
	}
	fi, err := d.f.Stat()
	if err != nil {
		return fmt.Errorf("stat remote file: %w", err)
	}
	d.Size = fi.Size()

	tmp, err := os.CreateTemp(filepath.Dir(target), ".mtssh-download-*") // mode 0600
	if err != nil {
		return fmt.Errorf("create local file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if err := d.copyTo(ctx, tmp, progress); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(sftpLocalMode(d.Mode, d.perms, sftpUmask())); err != nil {
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
	return os.Rename(tmp.Name(), target)
}

// sftpLocalTarget returns the file a download to localPath replaces:
// localPath itself, or where a symbolic link there points. It must be a
// regular file or not exist yet.
func sftpLocalTarget(localPath string) (string, error) {
	fi, err := os.Lstat(localPath)
	if errors.Is(err, os.ErrNotExist) {
		return localPath, nil
	}
	if err != nil {
		return "", err
	}
	target := localPath
	if fi.Mode()&os.ModeSymlink != 0 {
		if target, err = filepath.EvalSymlinks(localPath); err == nil {
			fi, err = os.Stat(target)
		}
		if err != nil {
			return "", fmt.Errorf("%s is a symbolic link that cannot be followed: %w", localPath, err)
		}
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", target)
	}
	return target, nil
}

// copyTo downloads the file into w (see SaveTo) and truncates w where the
// file turned out to end.
func (d *Download) copyTo(ctx context.Context, w *os.File, progress func(int64)) error {
	if d.Size <= 0 { // also a size above 2^63 from a broken server
		return d.copyUnsized(ctx, w, progress)
	}
	n, err := d.copyPipelined(ctx, w, d.Size, progress)
	if err != nil {
		return err
	}
	if n < d.Size {
		return w.Truncate(n)
	}
	return nil
}

// sftpChunk is the result of reading one chunk of a download.
type sftpChunk struct {
	off int64
	n   int
	err error
}

// copyPipelined reads the first size bytes of the file into w with
// sftpReadWorkers READ requests of sftpChunkSize in flight. It returns
// where the file ended: size, or less if it shrank meanwhile.
func (d *Download) copyPipelined(ctx context.Context, w io.WriterAt, size int64, progress func(int64)) (int64, error) {
	chunks := (size-1)/sftpChunkSize + 1
	var next atomic.Int64
	// stop ends the handing out of chunks, which go in order: when a chunk
	// ends early or fails, all chunks before it have been handed out.
	var stop atomic.Bool
	defer context.AfterFunc(ctx, func() { stop.Store(true) })()

	results := make(chan sftpChunk)
	var wg sync.WaitGroup
	for range min(sftpReadWorkers, chunks) {
		wg.Go(func() {
			buf := make([]byte, sftpChunkSize)
			for !stop.Load() {
				k := next.Add(1) - 1
				if k >= chunks {
					return
				}
				off := k * sftpChunkSize
				n, err := d.readChunk(w, buf[:min(sftpChunkSize, size-off)], off)
				if err != nil {
					stop.Store(true)
				}
				results <- sftpChunk{off, n, err}
			}
		})
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	// Progress is reported here, in the caller's goroutine, never from the
	// workers.
	end := size
	var failed sftpChunk // the failed chunk with the lowest offset
	var done int64
	for r := range results {
		done += int64(r.n)
		switch {
		case r.err == nil:
		case errors.Is(r.err, io.EOF):
			end = min(end, r.off+int64(r.n))
		case failed.err == nil || r.off < failed.off:
			failed = r
		}
		if progress != nil {
			progress(min(done, size))
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if failed.err != nil && failed.off < end {
		return 0, failed.err
	}
	return end, nil
}

// copyUnsized reads a file the server reports with size 0 (procfs, sysfs)
// sequentially until EOF, but not more than sftpMaxUnsizedDownload bytes.
func (d *Download) copyUnsized(ctx context.Context, w io.WriterAt, progress func(int64)) error {
	buf := make([]byte, sftpChunkSize)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := d.readChunk(w, buf, done)
		done += int64(n)
		if done > sftpMaxUnsizedDownload {
			return fmt.Errorf("the server reports no size for this file, and it has more than %d MiB", sftpMaxUnsizedDownload>>20)
		}
		if n > 0 && progress != nil {
			progress(done)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readChunk reads len(b) bytes at offset off of the remote file, asking
// again for the rest after a short read, and writes what it got to w at
// the same offset. io.EOF means the file ended before b was full. It
// recovers pkg/sftp's panics itself, as it runs in download workers.
func (d *Download) readChunk(w io.WriterAt, b []byte, off int64) (n int, err error) {
	defer sftpRecover(d.c, &err)
	for n < len(b) && err == nil {
		var m int
		m, err = d.f.ReadAt(b[n:], off+int64(n))
		if m == 0 && err == nil {
			err = io.ErrNoProgress
		}
		n += m
	}
	if n > 0 && (err == nil || errors.Is(err, io.EOF)) {
		if _, werr := w.WriteAt(b[:n], off); werr != nil {
			return 0, werr
		}
	}
	return n, err
}

// sftpLocalMode returns the permissions of a downloaded file: the remote
// read and execute bits plus read-write for the owner (so the copy can be
// opened and replaced later), never write for group or others, and the
// local umask applied. If the server reported no permissions, the base is
// 0644.
func sftpLocalMode(remote os.FileMode, reported bool, umask os.FileMode) os.FileMode {
	perm := remote.Perm() & 0o755
	if !reported {
		perm = 0o644
	}
	return (perm | 0o600) &^ umask
}

var (
	sftpUmaskOnce  sync.Once
	sftpUmaskValue os.FileMode
)

// sftpUmask returns the process umask, learnt once with sftpProbeUmask:
// Go cannot read it without changing it for all threads. It is 0 on
// Windows.
func sftpUmask() os.FileMode {
	sftpUmaskOnce.Do(func() { sftpUmaskValue = sftpProbeUmask() })
	return sftpUmaskValue
}

// sftpProbeUmask creates a file with mode 0666 and derives the umask from
// the mode it got; the execute bits are assumed to be masked like the read
// bits. If that fails, it assumes 077, which errs on the private side.
func sftpProbeUmask() os.FileMode {
	if runtime.GOOS == "windows" {
		return 0
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return 0o077
	}
	name := filepath.Join(os.TempDir(), ".mtssh-umask-"+hex.EncodeToString(suffix))
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return 0o077
	}
	defer os.Remove(name)
	fi, err := f.Stat()
	f.Close()
	if err != nil {
		return 0o077
	}
	mask := 0o666 &^ fi.Mode().Perm()
	return mask | (mask&0o444)>>2
}

// Exists reports whether remotePath exists.
func (s *SFTPClient) Exists(remotePath string) (_ bool, err error) {
	defer sftpRecover(s.client, &err)
	_, err = s.client.Lstat(remotePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Upload copies a local regular file to remotePath. If remotePath is a
// symbolic link, the file it points to is replaced and the link stays. A
// directory, a link to anything but a regular file (or to nothing) and
// other non-regular files are refused.
//
// The data is written into a staging directory next to the target
// (.mtssh-<random>, made 0700 before anything is written, so no one else
// can open the file meanwhile) and moved over the target when complete: in
// one step with the posix-rename extension; without it, the old file is
// moved into the staging directory first and moved back if the new one
// cannot take its place. The staging directory is always removed, except
// when moving the old file back fails too: then the error says where both
// files are.
//
// An existing target keeps its permissions and, where the server allows
// it, its owner and group. A new file gets the local permissions without
// write permission for group or others, limited by the server's umask
// (sftpUploadMode). Where the server cannot change permissions, that is
// only logged.
//
// Where the staging directory cannot be created (directory not writable,
// object stores), an existing target is overwritten in place, which keeps
// its mode, owner and hard links but is not atomic: a failed upload leaves
// it incomplete. A new target is then created directly and removed again
// if the upload fails.
func (s *SFTPClient) Upload(ctx context.Context, localPath, remotePath string, progress func(done, total int64)) (err error) {
	defer sftpRecover(s.client, &err)
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
	target, existing, err := s.uploadTarget(remotePath)
	if err != nil {
		return err
	}
	var report func(int64)
	if progress != nil {
		report = func(n int64) { progress(n, fi.Size()) }
	}
	src := &transferReader{ctx: ctx, r: local, progress: report}

	stage, err := s.makeStage(path.Dir(target))
	if err != nil {
		logger.Info("sftp", fmt.Sprintf("no staging directory for %s (%v); writing it directly", target, err))
		return s.uploadDirect(src, fi.Mode(), target, existing)
	}
	keepStage := false
	defer func() {
		if !keepStage {
			s.removeStage(stage)
		}
	}()
	tmp := path.Join(stage, sftpStageData)
	created, err := s.create(tmp, src)
	if err != nil {
		return err
	}
	s.setMode(tmp, fi.Mode(), created, existing)
	keepStage, err = s.replace(stage, tmp, target, existing != nil)
	return err
}

// uploadTarget returns the file an upload to remotePath replaces
// (remotePath itself, or where a symbolic link there points) and its
// attributes, nil if it does not exist yet.
func (s *SFTPClient) uploadTarget(remotePath string) (string, os.FileInfo, error) {
	fi, err := s.client.Lstat(remotePath)
	if errors.Is(err, os.ErrNotExist) {
		return remotePath, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	target := remotePath
	if fi.Mode()&os.ModeSymlink != 0 {
		if target, fi, err = s.resolveLink(remotePath); err != nil {
			return "", nil, fmt.Errorf("%s is a symbolic link that cannot be followed: %w", remotePath, err)
		}
	}
	switch {
	case fi.IsDir():
		return "", nil, fmt.Errorf("%s is a directory", target)
	case !fi.Mode().IsRegular():
		return "", nil, fmt.Errorf("%s is not a regular file", target)
	}
	return target, fi, nil
}

// resolveLink returns where the symbolic link p finally points and that
// file's own attributes. OpenSSH's realpath resolves links; on servers
// whose realpath only cleans the path (like pkg/sftp's), each link is
// followed with ReadLink.
func (s *SFTPClient) resolveLink(p string) (string, os.FileInfo, error) {
	for range sftpMaxLinkHops {
		resolved, err := s.client.RealPath(p)
		if err != nil {
			return "", nil, err
		}
		fi, err := s.client.Lstat(resolved)
		if err != nil {
			return "", nil, err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return resolved, fi, nil
		}
		dest, err := s.client.ReadLink(resolved)
		if err != nil {
			return "", nil, err
		}
		if !path.IsAbs(dest) {
			dest = path.Join(path.Dir(resolved), dest)
		}
		p = dest
	}
	return "", nil, errors.New("too many levels of symbolic links")
}

// makeStage creates an upload's staging directory in dir and makes it
// private (0700) before anything is written into it. Where the server
// cannot change permissions, the upload goes on and that is logged.
func (s *SFTPClient) makeStage(dir string) (string, error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	stage := path.Join(dir, sftpStagePrefix+hex.EncodeToString(suffix))
	if s.testFail != nil {
		if err := s.testFail("mkdir", stage); err != nil {
			return "", err
		}
	}
	if err := s.client.Mkdir(stage); err != nil {
		return "", err
	}
	if err := s.client.Chmod(stage, 0o700); err != nil {
		logger.Info("sftp", fmt.Sprintf("cannot make %s private: %v", stage, err))
	}
	return stage, nil
}

// create creates the remote file p exclusively and copies src into it. It
// closes p explicitly, so SFTP flushes and reports any write error, and
// removes it again if the copy fails. It returns the attributes the server
// gave the new file (nil if it did not say).
func (s *SFTPClient) create(p string, src io.Reader) (*sftp.FileStat, error) {
	f, err := s.client.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return nil, fmt.Errorf("create remote %s: %w", p, err)
	}
	var created *sftp.FileStat
	if fi, err := f.Stat(); err == nil {
		created, _ = fi.Sys().(*sftp.FileStat)
	}
	_, err = io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		s.client.Remove(p)
		return nil, err
	}
	return created, nil
}

// setMode gives the uploaded file p its final permissions: those of the
// file it replaces (existing), with its owner and group where the server
// allows; for a new file sftpUploadMode of the local mode and the mode the
// server created p with. If the server reports no permissions, p keeps
// what it got. Failures are only logged: some servers (e.g. on object
// stores) cannot change permissions.
func (s *SFTPClient) setMode(p string, local os.FileMode, created *sftp.FileStat, existing os.FileInfo) {
	var old *sftp.FileStat
	if existing != nil {
		old, _ = existing.Sys().(*sftp.FileStat)
	}
	var mode os.FileMode
	switch {
	case old != nil && old.Mode != 0:
		if created == nil || old.UID != created.UID || old.GID != created.GID {
			s.client.Chown(p, int(old.UID), int(old.GID)) // fails unless root or an own group
		}
		mode = existing.Mode().Perm()
	case created != nil && created.Mode != 0:
		mode = sftpUploadMode(local, created.FileMode())
	default:
		return
	}
	if err := s.client.Chmod(p, mode); err != nil {
		logger.Info("sftp", fmt.Sprintf("cannot set the permissions of %s: %v", p, err))
	}
}

// sftpUploadMode returns the permissions of a new remote file: the local
// ones without write permission for group or others, limited by what the
// server's umask let through when it created the file (created). Its
// execute bits are assumed to be masked like the read bits.
func sftpUploadMode(local, created os.FileMode) os.FileMode {
	created = created.Perm()
	return local.Perm() & 0o755 & (created | (created&0o444)>>2)
}

// replace moves the finished upload tmp over target (see Upload). keep
// reports that the staging directory holds the only copy of the original
// and must stay.
func (s *SFTPClient) replace(stage, tmp, target string, exists bool) (keep bool, err error) {
	if _, ok := s.client.HasExtension("posix-rename@openssh.com"); ok && !s.noPosixRename {
		return false, s.client.PosixRename(tmp, target)
	}
	// Plain SFTP rename does not overwrite.
	if !exists {
		return false, s.plainRename(tmp, target)
	}
	old := path.Join(stage, sftpStageOld)
	if err := s.plainRename(target, old); err != nil {
		return false, err
	}
	if err := s.plainRename(tmp, target); err != nil {
		if rerr := s.plainRename(old, target); rerr != nil {
			return true, fmt.Errorf("replace %s: %v; the original is now %s and the new file %s (moving the original back failed: %v)", target, err, old, tmp, rerr)
		}
		return false, err
	}
	return false, nil
}

// plainRename is the SFTP rename, which tests can make fail.
func (s *SFTPClient) plainRename(oldPath, newPath string) error {
	if s.testFail != nil {
		if err := s.testFail("rename", oldPath); err != nil {
			return err
		}
	}
	return s.client.Rename(oldPath, newPath)
}

// removeStage deletes an upload's staging directory and what is left in it.
func (s *SFTPClient) removeStage(stage string) {
	if s.client.RemoveDirectory(stage) == nil {
		return
	}
	s.client.Remove(path.Join(stage, sftpStageData))
	s.client.Remove(path.Join(stage, sftpStageOld))
	if err := s.client.RemoveDirectory(stage); err != nil {
		logger.Error("sftp", fmt.Sprintf("cannot remove %s: %v", stage, err))
	}
}

// uploadDirect writes target without a staging directory (see Upload).
func (s *SFTPClient) uploadDirect(src io.Reader, local os.FileMode, target string, existing os.FileInfo) error {
	if existing == nil {
		created, err := s.create(target, src)
		if err != nil {
			return err
		}
		s.setMode(target, local, created, nil)
		return nil
	}
	f, err := s.client.OpenFile(target, os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("open remote %s: %w", target, err)
	}
	_, err = io.Copy(f, src)
	// Close explicitly so SFTP flushes and reports any write errors
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
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
func (s *SFTPClient) Delete(remotePath string) (err error) {
	defer sftpRecover(s.client, &err)
	return s.client.Remove(remotePath)
}

// Mkdir creates a remote directory
func (s *SFTPClient) Mkdir(remotePath string) (err error) {
	defer sftpRecover(s.client, &err)
	return s.client.MkdirAll(remotePath)
}

// Rename moves/renames a remote path
func (s *SFTPClient) Rename(oldPath, newPath string) (err error) {
	defer sftpRecover(s.client, &err)
	return s.client.Rename(oldPath, newPath)
}

// Getwd returns the remote working directory
func (s *SFTPClient) Getwd() (_ string, err error) {
	defer sftpRecover(s.client, &err)
	return s.client.Getwd()
}

// Close closes the SFTP connection
func (s *SFTPClient) Close() {
	if s.client != nil {
		s.client.Close()
	}
}

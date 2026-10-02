package ui

import (
	"context"
	"errors"
	"io/fs"
	"mtssh/core"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"fyne.io/fyne/v2/test"
)

// sftpTestTab returns an SFTP tab with its widgets but no server (a client
// that is never connected); the test driver runs fyne.Do immediately.
func sftpTestTab(t *testing.T) *SFTPTab {
	t.Helper()
	test.NewApp()
	return newSFTPTab(&core.SFTPClient{}, test.NewWindow(nil))
}

// sftpShortCloseWait shortens sftpCloseWait for the test.
func sftpShortCloseWait(t *testing.T) {
	old := sftpCloseWait
	sftpCloseWait = 50 * time.Millisecond
	t.Cleanup(func() { sftpCloseWait = old })
}

// sftpWaitDone fails the test unless ch is closed within a few seconds.
func sftpWaitDone(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: still waiting", what)
	}
}

func TestSFTPTransferSlot(t *testing.T) {
	tab := sftpTestTab(t)
	if !tab.cancelBtn.Disabled() {
		t.Fatal("Cancel Transfer enabled without a transfer")
	}

	ctx, release, ok := tab.reserveTransfer()
	if !ok {
		t.Fatal("free slot not reserved")
	}
	if tab.cancelBtn.Disabled() {
		t.Fatal("Cancel Transfer disabled while the slot is taken")
	}
	if _, _, ok := tab.reserveTransfer(); ok {
		t.Fatal("second transfer got the slot")
	}
	test.Tap(tab.cancelBtn)
	if ctx.Err() == nil {
		t.Fatal("Cancel Transfer did not cancel the reserved transfer")
	}
	release()
	if !tab.cancelBtn.Disabled() {
		t.Fatal("Cancel Transfer enabled after release")
	}

	ctx2, release2, ok := tab.reserveTransfer()
	if !ok {
		t.Fatal("slot not free after release")
	}
	// Releasing again (e.g. a dialog and the transfer both releasing) must
	// not free or cancel the next transfer's reservation.
	release()
	if _, _, ok := tab.reserveTransfer(); ok {
		t.Fatal("a stale release freed another transfer's slot")
	}
	if ctx2.Err() != nil || tab.cancelBtn.Disabled() {
		t.Fatal("a stale release cancelled another transfer")
	}
	release2()
	if _, release3, ok := tab.reserveTransfer(); !ok {
		t.Fatal("slot not free after the second release")
	} else {
		release3()
	}
}

func TestSFTPTransferReleasesSlot(t *testing.T) {
	tab := sftpTestTab(t)
	run := func(err error) func(context.Context, func(int64, int64)) error {
		return func(ctx context.Context, progress func(done, total int64)) error {
			if _, _, ok := tab.reserveTransfer(); ok {
				t.Error("slot free while the transfer runs")
			}
			progress(1, 2)
			return err
		}
	}
	free := func(name string) {
		t.Helper()
		_, release, ok := tab.reserveTransfer()
		if !ok {
			t.Fatalf("%s: slot still taken", name)
		}
		release()
		if !tab.cancelBtn.Disabled() {
			t.Fatalf("%s: Cancel Transfer still enabled", name)
		}
	}

	ctx, release, _ := tab.reserveTransfer()
	tab.transfer(ctx, release, "Uploading a", run(nil), "Uploaded a")
	if got := tab.statusLbl.Text; got != "Uploaded a" {
		t.Fatalf("status = %q", got)
	}
	free("success")

	ctx, release, _ = tab.reserveTransfer()
	tab.transfer(ctx, release, "Downloading b", run(errors.New("boom")), "Downloaded b")
	if got := tab.statusLbl.Text; got != "Downloading b failed: boom" {
		t.Fatalf("status = %q", got)
	}
	free("failure")

	// Cancelled while it waited for a dialog or check: run is not called.
	ctx, release, _ = tab.reserveTransfer()
	test.Tap(tab.cancelBtn)
	tab.transfer(ctx, release, "Downloading c", func(context.Context, func(int64, int64)) error {
		t.Error("cancelled transfer started")
		return nil
	}, "Downloaded c")
	if got := tab.statusLbl.Text; got != "Downloading c cancelled" {
		t.Fatalf("status = %q", got)
	}
	free("cancel")
}

func TestSFTPLoadGeneration(t *testing.T) {
	tab := sftpTestTab(t)
	slow := tab.startLoad("/slow")
	fast := tab.startLoad("/fast")
	if got := tab.refreshDir(); got != "/fast" {
		t.Fatalf("refreshDir during a listing = %q, want the newest listing's /fast", got)
	}
	if !tab.applyLoad(fast, "/fast", []core.FileEntry{{Name: "a"}}, nil) {
		t.Fatal("newest listing dropped")
	}
	if tab.applyLoad(slow, "/slow", []core.FileEntry{{Name: "b"}, {Name: "c"}}, nil) {
		t.Fatal("stale listing applied")
	}
	if tab.dir() != "/fast" || len(tab.entries) != 1 || tab.pathLabel.Text != "/fast" || tab.statusLbl.Text != "1 items" {
		t.Fatalf("after a stale listing: dir %q, %d entries, path %q, status %q",
			tab.dir(), len(tab.entries), tab.pathLabel.Text, tab.statusLbl.Text)
	}
	if got := tab.refreshDir(); got != "/fast" {
		t.Fatalf("refreshDir = %q, want /fast", got)
	}

	// A stale error is dropped as well; the newest one is shown and keeps
	// the current directory.
	stale := tab.startLoad("/x")
	newest := tab.startLoad("/y")
	if tab.applyLoad(stale, "/x", nil, errors.New("stale")) {
		t.Fatal("stale error applied")
	}
	if tab.statusLbl.Text != "1 items" {
		t.Fatalf("stale error shown: %q", tab.statusLbl.Text)
	}
	if !tab.applyLoad(newest, "/y", nil, errors.New("permission denied")) {
		t.Fatal("newest error dropped")
	}
	if tab.dir() != "/fast" || tab.statusLbl.Text != "Error: permission denied" {
		t.Fatalf("after an error: dir %q, status %q", tab.dir(), tab.statusLbl.Text)
	}
	if got := tab.refreshDir(); got != "/fast" {
		t.Fatalf("refreshDir after a failed listing = %q, want /fast", got)
	}
}

func TestSFTPOverwriteQuestion(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if q, err := sftpOverwriteQuestion(filepath.Join(dir, "new.txt")); q != "" || err != nil {
		t.Fatalf("new file: %q, %v", q, err)
	}
	if q, err := sftpOverwriteQuestion(file); err != nil || !strings.Contains(q, "already exists") {
		t.Fatalf("existing file: %q, %v", q, err)
	}
	if _, err := sftpOverwriteQuestion(dir); err == nil {
		t.Fatal("folder accepted as download target")
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	q, err := sftpOverwriteQuestion(link)
	if err != nil || !strings.Contains(q, "The file it points to will be replaced") || !strings.Contains(q, "file.txt") {
		t.Fatalf("link to a file: %q, %v", q, err)
	}
	dirLink := filepath.Join(dir, "dirlink")
	if err := os.Symlink(dir, dirLink); err != nil {
		t.Fatal(err)
	}
	if _, err := sftpOverwriteQuestion(dirLink); err == nil {
		t.Fatal("link to a folder accepted")
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := sftpOverwriteQuestion(dangling); err == nil {
		t.Fatal("broken link accepted")
	}
	// Checking must not touch the file (Fyne's save dialog emptied it).
	if data, _ := os.ReadFile(file); string(data) != "keep" {
		t.Fatalf("file changed to %q", data)
	}
}

func TestSFTPCheckFileName(t *testing.T) {
	for name, ok := range map[string]bool{
		"report.pdf": true,
		".bashrc":    true,
		"a b.txt":    true,
		"":           false,
		"   ":        false,
		".":          false,
		"..":         false,
		"../x":       false,
		`..\x`:       false,
		"sub/x.txt":  false,
	} {
		if err := sftpCheckFileName(name); (err == nil) != ok {
			t.Errorf("sftpCheckFileName(%q) = %v, want ok=%v", name, err, ok)
		}
	}
}

func TestSFTPLocalFileNameReserved(t *testing.T) {
	tests := map[string]string{
		"COM0":        "_COM0",
		"lpt0.txt":    "_lpt0.txt",
		"COM¹":        "_COM¹",
		"com².log":    "_com².log",
		"COM³":        "_COM³",
		"LPT¹.txt":    "_LPT¹.txt",
		"lpt²":        "_lpt²",
		"LPT³.tar.gz": "_LPT³.tar.gz",
		"CONIN$":      "_CONIN$",
		"conout$.txt": "_conout$.txt",
		"conin$ .txt": "_conin$ .txt",
		// Not reserved
		"COM10":     "COM10",
		"COM⁴":      "COM⁴",
		"CONIN":     "CONIN",
		"xCONOUT$":  "xCONOUT$",
		"lpt1x.txt": "lpt1x.txt",
	}
	for in, want := range tests {
		if got := localFileName(in); got != want {
			t.Errorf("localFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSFTPCanDownload(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    core.FileEntry
		want bool
	}{
		{"file", core.FileEntry{Mode: 0o644}, true},
		{"no type reported", core.FileEntry{}, true},
		{"link to a file", core.FileEntry{IsLink: true, Mode: 0o644}, true},
		{"directory", core.FileEntry{IsDir: true, Mode: fs.ModeDir | 0o755}, false},
		{"link to a directory", core.FileEntry{IsLink: true, IsDir: true, Mode: fs.ModeDir | 0o755}, false},
		{"FIFO", core.FileEntry{Mode: fs.ModeNamedPipe | 0o644}, false},
		{"link to a FIFO", core.FileEntry{IsLink: true, Mode: fs.ModeNamedPipe | 0o644}, false},
		{"unresolved link", core.FileEntry{IsLink: true, Mode: fs.ModeSymlink | 0o777}, false},
		{"device", core.FileEntry{Mode: fs.ModeDevice | fs.ModeCharDevice | 0o666}, false},
		{"socket", core.FileEntry{Mode: fs.ModeSocket | 0o777}, false},
	} {
		if got := sftpCanDownload(tc.e); got != tc.want {
			t.Errorf("%s: sftpCanDownload = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Closing the tab cancels the transfer and closes the connection only
// once the transfer has cleaned up, without blocking the caller.
func TestSFTPTabClose(t *testing.T) {
	tab := sftpTestTab(t)
	ctx, release, _ := tab.reserveTransfer()
	cleanedUp := make(chan bool)
	go func() {
		<-ctx.Done()
		cleanedUp <- tab.sftp.Closed() // cleanup needs the connection
		release()
	}()
	tab.Close()
	if closed := <-cleanedUp; closed {
		t.Error("connection closed before the transfer cleaned up")
	}
	sftpWaitDone(t, tab.Done(), "Close")
	if !tab.sftp.Closed() {
		t.Error("connection not closed")
	}
	tab.Close() // again: does nothing
	if _, _, ok := tab.reserveTransfer(); ok {
		t.Error("transfer started in a closed tab")
	}

	// Without a transfer the connection is closed at once.
	tab = sftpTestTab(t)
	tab.Close()
	sftpWaitDone(t, tab.Done(), "Close without a transfer")
	if !tab.sftp.Closed() {
		t.Error("connection not closed")
	}

	// A transfer that does not stop (or a dialog nobody answers) holds the
	// connection open for sftpCloseWait at most.
	sftpShortCloseWait(t)
	tab = sftpTestTab(t)
	tab.reserveTransfer()
	tab.Close()
	sftpWaitDone(t, tab.Done(), "Close with a stuck transfer")
	if !tab.sftp.Closed() {
		t.Error("connection not closed")
	}
}

// Once core closed the connection (a cancelled transfer the server did not
// end in time), the status says so instead of a later "EOF".
func TestSFTPTransferConnectionClosed(t *testing.T) {
	tab := sftpTestTab(t)
	ctx, release, _ := tab.reserveTransfer()
	tab.transfer(ctx, release, "Downloading a", func(context.Context, func(int64, int64)) error {
		tab.sftp.Close()
		return context.Canceled
	}, "Downloaded a")
	if got, want := tab.statusLbl.Text, "Downloading a cancelled; "+sftpClosedMsg; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
	if got := tab.remoteErr(errors.New("EOF")); got != sftpClosedMsg {
		t.Fatalf("later error shown as %q", got)
	}
}

func TestShortErr(t *testing.T) {
	if got := shortErr(errors.New("permission denied")); got != "permission denied" {
		t.Errorf("short error changed to %q", got)
	}
	// A huge message from the server (e.g. rejecting the SFTP channel).
	long := shortErr(errors.New("ssh: rejected: " + strings.Repeat("é", 125000)))
	if len(long) > shortErrMaxLen+len("…") || !strings.HasSuffix(long, "…") || !utf8.ValidString(long) {
		t.Errorf("long error: %d bytes, valid UTF-8 %v, ends %q", len(long), utf8.ValidString(long), long[max(0, len(long)-8):])
	}
	// Several long lines, or escapes that make a line longer, stay within
	// the cap too.
	for _, msg := range []string{strings.Repeat(strings.Repeat("x", 300)+"\n", 3), strings.Repeat("\x01", 500)} {
		if got := shortErr(errors.New(msg)); len(got) > shortErrMaxLen+len("…") || !strings.HasSuffix(got, "…") {
			t.Errorf("%.10q…: %d bytes, %q", msg, len(got), got[max(0, len(got)-8):])
		}
	}
	lines := shortErr(errors.New(strings.Repeat("line\n", 20)))
	if n := strings.Count(lines, "\n") + 1; n != shortErrMaxLines || !strings.HasSuffix(lines, "…") {
		t.Errorf("many lines: %d lines, %q", n, lines)
	}
	// Control and bidi characters are shown escaped; CR LF ends a line.
	if got, want := shortErr(errors.New("a\x1b[2Jb\r\nc\u202e")), `a\x1b[2Jb`+"\n"+`c\u202e`; got != want {
		t.Errorf("shortErr = %q, want %q", got, want)
	}
}

package ui

import (
	"context"
	"errors"
	"mtssh/core"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2/test"
)

// sftpTestTab returns an SFTP tab with its widgets but no connection; the
// test driver runs fyne.Do immediately.
func sftpTestTab(t *testing.T) *SFTPTab {
	t.Helper()
	test.NewApp()
	tab := &SFTPTab{win: test.NewWindow(nil)}
	tab.buildUI()
	return tab
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

package ui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"mtssh/core"
	"mtssh/logger"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// SFTPTab shows a file manager for a remote SSH server. Network operations
// run in goroutines; their results are applied on the UI goroutine.
type SFTPTab struct {
	sftp *core.SFTPClient
	win  fyne.Window
	// mu guards currentDir, entries, cancel, released, closing, loadGen
	// and loadDir.
	mu         sync.RWMutex
	currentDir string
	entries    []core.FileEntry
	// cancel cancels the transfer that holds the tab's single transfer
	// slot, nil if the slot is free (see reserveTransfer); released is
	// closed when that transfer frees the slot.
	cancel    context.CancelFunc
	released  chan struct{}
	closing   bool          // Close was called: no new transfers
	done      chan struct{} // closed when Close has finished (see Done)
	loadGen   uint64        // generation of the newest directory listing
	loadDir   string        // directory of the newest listing while it runs
	list      *widget.List
	pathLabel *widget.Label
	statusLbl *widget.Label
	cancelBtn *widget.Button
	Container fyne.CanvasObject
}

// sftpCloseWait bounds how long closing a tab waits for its transfer to
// stop and clean up before the connection is closed. It is longer than the
// few seconds core gives a cancelled transfer before it closes the
// connection itself. A variable so tests can shorten it.
var sftpCloseWait = 5 * time.Second

// NewSFTPTab creates an SFTP file manager. Must be called on the UI goroutine;
// the initial directory listing is loaded in the background.
func NewSFTPTab(sftpClient *core.SFTPClient, win fyne.Window) *SFTPTab {
	t := newSFTPTab(sftpClient, win)
	go func() {
		cwd, err := sftpClient.Getwd()
		if err != nil {
			cwd = "/"
		}
		t.load(cwd)
	}()
	return t
}

// newSFTPTab creates the tab without loading a directory.
func newSFTPTab(sftpClient *core.SFTPClient, win fyne.Window) *SFTPTab {
	t := &SFTPTab{sftp: sftpClient, win: win, done: make(chan struct{})}
	t.buildUI()
	return t
}

// Close closes the tab's SFTP connection, for the tab's close button or
// its window closing. A running transfer is cancelled first and may clean
// up (remove a partial remote file or staging directory, which needs the
// connection) for up to sftpCloseWait. That wait runs in the background,
// so Close never blocks the UI goroutine; Done is closed when it is over.
// Calling Close again does nothing.
func (t *SFTPTab) Close() {
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		return
	}
	t.closing = true
	cancel, released := t.cancel, t.released
	t.mu.Unlock()
	if cancel == nil {
		t.sftp.Close()
		close(t.done)
		return
	}
	cancel()
	go func() {
		timer := time.NewTimer(sftpCloseWait)
		defer timer.Stop()
		select {
		case <-released:
		case <-timer.C:
		}
		t.sftp.Close()
		close(t.done)
	}()
}

// Done is closed once Close has closed the connection, after a running
// transfer has cleaned up.
func (t *SFTPTab) Done() <-chan struct{} {
	return t.done
}

// Busy reports whether a transfer holds the transfer slot (it may still be
// waiting for a dialog).
func (t *SFTPTab) Busy() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cancel != nil
}

func (t *SFTPTab) buildUI() {
	t.pathLabel = widget.NewLabel("…")
	t.statusLbl = widget.NewLabel("")

	// File list: click selects, double click opens a directory or shows file options
	t.list = newDoubleTapList(
		func() int {
			t.mu.RLock()
			defer t.mu.RUnlock()
			return len(t.entries)
		},
		func() fyne.CanvasObject {
			return container.NewHBox(
				widget.NewIcon(theme.FileIcon()),
				widget.NewLabel("placeholder"),
				widget.NewLabel(""),
			)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			e, ok := t.entry(id)
			if !ok {
				return
			}

			row := obj.(*fyne.Container)
			icon := row.Objects[0].(*widget.Icon)
			name := row.Objects[1].(*widget.Label)
			size := row.Objects[2].(*widget.Label)

			if e.IsDir {
				icon.SetResource(theme.FolderIcon())
				size.SetText("<DIR>")
			} else {
				icon.SetResource(theme.FileIcon())
				size.SetText(humanSize(e.Size))
			}
			if e.IsLink {
				name.SetText(e.Name + " →") // symbolic link
			} else {
				name.SetText(e.Name)
			}
		},
		func(id widget.ListItemID) {
			e, ok := t.entry(id)
			if !ok {
				return
			}
			if e.IsDir {
				t.navigate(e.Name)
			} else {
				t.showFileMenu(e)
			}
		},
	)

	// Toolbar buttons
	upBtn := widget.NewButtonWithIcon("Up", theme.NavigateBackIcon(), func() {
		t.navigate("..")
	})
	refreshBtn := widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), func() {
		go t.refresh()
	})
	mkdirBtn := widget.NewButtonWithIcon("New Folder", theme.FolderNewIcon(), func() {
		t.showMkdirDialog()
	})
	uploadBtn := widget.NewButtonWithIcon("Upload", theme.UploadIcon(), func() {
		t.showUploadDialog()
	})
	t.cancelBtn = widget.NewButtonWithIcon("Cancel Transfer", theme.CancelIcon(), func() {
		t.mu.RLock()
		cancel := t.cancel
		t.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
	})
	t.cancelBtn.Disable()

	toolbar := container.NewHBox(upBtn, refreshBtn, mkdirBtn, uploadBtn, t.cancelBtn)
	pathRow := container.NewBorder(nil, nil, widget.NewLabel("Path:"), nil, t.pathLabel)

	t.Container = container.NewBorder(
		container.NewVBox(pathRow, toolbar),
		t.statusLbl,
		nil, nil,
		t.list,
	)
}

func (t *SFTPTab) entry(id widget.ListItemID) (core.FileEntry, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if id < 0 || id >= len(t.entries) {
		return core.FileEntry{}, false
	}
	return t.entries[id], true
}

func (t *SFTPTab) dir() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.currentDir
}

// navigate opens a subdirectory (or ".." for the parent) in the background.
func (t *SFTPTab) navigate(name string) {
	dir := t.dir()
	if dir == "" {
		return // initial listing not loaded yet
	}
	if name == ".." {
		dir = path.Dir(dir)
	} else {
		dir = path.Join(dir, name)
	}
	go t.load(dir)
}

// refresh reloads the directory shown, or the one a running listing is
// about to show. Blocks on the network — call it from a goroutine.
func (t *SFTPTab) refresh() {
	if dir := t.refreshDir(); dir != "" {
		t.load(dir)
	}
}

// refreshDir returns the directory refresh reloads: that of a running
// listing (a newer listing replaces it, see applyLoad), else the current one.
func (t *SFTPTab) refreshDir() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.loadDir != "" {
		return t.loadDir
	}
	return t.currentDir
}

// load lists dir and makes it the current directory. On error (e.g.
// permission denied) the previous directory stays current. Blocks on the
// network — call it from a goroutine.
func (t *SFTPTab) load(dir string) {
	gen := t.startLoad(dir)
	t.setStatus("Loading…")
	entries, err := t.sftp.ListDir(dir)
	if err == nil {
		// Sort: dirs first, then by name
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir != entries[j].IsDir {
				return entries[i].IsDir
			}
			return entries[i].Name < entries[j].Name
		})
	}
	fyne.Do(func() { t.applyLoad(gen, dir, entries, err) })
}

// startLoad registers a listing of dir and returns its generation.
func (t *SFTPTab) startLoad(dir string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadGen++
	t.loadDir = dir
	return t.loadGen
}

// applyLoad shows the result of listing dir, started as generation gen.
// A listing that is no longer the newest one is dropped, so a slow listing
// can't replace a newer one (or its error). Reports whether it was applied.
// Runs on the UI goroutine.
func (t *SFTPTab) applyLoad(gen uint64, dir string, entries []core.FileEntry, err error) bool {
	t.mu.Lock()
	if gen != t.loadGen {
		t.mu.Unlock()
		return false
	}
	t.loadDir = ""
	if err != nil {
		if t.currentDir == "" {
			// The first listing failed (e.g. no permission for the home
			// directory): still make dir current so Up and Refresh work.
			t.currentDir = dir
		}
		current := t.currentDir
		t.mu.Unlock()
		t.pathLabel.SetText(current)
		t.statusLbl.SetText("Error: " + t.remoteErr(err))
		return true
	}
	t.currentDir = dir
	t.entries = entries
	t.mu.Unlock()
	t.pathLabel.SetText(dir)
	t.list.UnselectAll()
	t.list.Refresh()
	t.statusLbl.SetText(fmt.Sprintf("%d items", len(entries)))
	return true
}

// sftpCanDownload reports whether Download is offered for e: a regular
// file, a link to one, or an entry the server gave no type for
// (OpenDownload checks again). Opening a FIFO would block the server, a
// device could stream data without end, and a link that could not be
// resolved cannot be downloaded either.
func sftpCanDownload(e core.FileEntry) bool {
	return !e.IsDir && e.Mode.IsRegular()
}

func (t *SFTPTab) showFileMenu(e core.FileEntry) {
	dir := t.dir()
	remotePath := path.Join(dir, e.Name)
	var menu dialog.Dialog

	var download fyne.CanvasObject = widget.NewLabel("Not a regular file: cannot be downloaded")
	if sftpCanDownload(e) {
		download = widget.NewButton("Download", func() {
			menu.Hide()
			t.download(remotePath, e.Name)
		})
	}

	deleteBtn := widget.NewButton("Delete", func() {
		dialog.ShowConfirm("Delete", "Delete "+e.Name+"?", func(ok bool) {
			if !ok {
				return
			}
			menu.Hide()
			go func() {
				if err := t.sftp.Delete(remotePath); err != nil {
					t.setStatus("Delete error: " + t.remoteErr(err))
				} else {
					t.refresh()
					t.setStatus("Deleted " + e.Name)
				}
			}()
		}, t.win)
	})

	renameEntry := widget.NewEntry()
	renameEntry.SetText(e.Name)
	renameBtn := widget.NewButton("Rename", func() {
		newName := renameEntry.Text
		if newName == "" || newName == e.Name {
			return
		}
		menu.Hide()
		newPath := path.Join(dir, newName)
		go func() {
			if err := t.sftp.Rename(remotePath, newPath); err != nil {
				t.setStatus("Rename error: " + t.remoteErr(err))
			} else {
				t.refresh()
				t.setStatus("Renamed to " + newName)
			}
		}()
	})

	content := container.NewVBox(
		widget.NewLabel("File: "+e.Name),
		widget.NewLabel("Size: "+humanSize(e.Size)),
		widget.NewSeparator(),
		download,
		widget.NewSeparator(),
		widget.NewLabel("Rename to:"),
		renameEntry,
		renameBtn,
		widget.NewSeparator(),
		deleteBtn,
	)
	menu = dialog.NewCustom("File Options", "Close", content, t.win)
	menu.Show()
}

func (t *SFTPTab) showMkdirDialog() {
	entry := widget.NewEntry()
	entry.SetPlaceHolder("New folder name")
	dialog.ShowCustomConfirm("New Folder", "Create", "Cancel", entry, func(ok bool) {
		if !ok || entry.Text == "" {
			return
		}
		name := entry.Text
		newPath := path.Join(t.dir(), name)
		go func() {
			if err := t.sftp.Mkdir(newPath); err != nil {
				t.setStatus("Mkdir error: " + t.remoteErr(err))
			} else {
				t.refresh()
				t.setStatus("Created " + name)
			}
		}()
	}, t.win)
}

// sftpBusy is shown when a transfer is requested while another one holds
// the transfer slot.
const sftpBusy = "Another transfer is running — wait for it or cancel it first"

// reserveTransfer takes the tab's single transfer slot. Uploads and
// downloads take it before their first dialog, so a second transfer is
// refused before the user picks anything. It returns the transfer's
// context, which Cancel Transfer and Close cancel, and release, which frees
// the slot (calling it again does nothing). ok is false if the slot is
// taken or the tab is closed.
func (t *SFTPTab) reserveTransfer() (ctx context.Context, release func(), ok bool) {
	t.mu.Lock()
	if t.cancel != nil || t.closing {
		t.mu.Unlock()
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	released := make(chan struct{})
	t.cancel, t.released = cancel, released
	t.mu.Unlock()
	t.updateCancelBtn()

	var once sync.Once
	release = func() {
		once.Do(func() {
			cancel()
			t.mu.Lock()
			t.cancel, t.released = nil, nil
			t.mu.Unlock()
			t.updateCancelBtn()
			close(released) // last: Close may close the connection now
		})
	}
	return ctx, release, true
}

// updateCancelBtn enables Cancel Transfer while the transfer slot is taken.
// The slot is read when the update runs on the UI goroutine, so updates
// queued by consecutive transfers can't apply a stale state.
func (t *SFTPTab) updateCancelBtn() {
	fyne.Do(func() {
		t.mu.RLock()
		busy := t.cancel != nil
		t.mu.RUnlock()
		if busy {
			t.cancelBtn.Enable()
		} else {
			t.cancelBtn.Disable()
		}
	})
}

// download asks where to save remotePath and downloads it there. The local
// file is chosen with a folder dialog and a name field: Fyne's save dialog
// empties an existing file before it returns, which would lose the file if
// the download then fails or is cancelled. Must be called on the UI
// goroutine.
func (t *SFTPTab) download(remotePath, name string) {
	ctx, release, ok := t.reserveTransfer()
	if !ok {
		t.statusLbl.SetText(sftpBusy)
		return
	}
	dialog.ShowFolderOpen(func(folder fyne.ListableURI, err error) {
		switch {
		case err != nil:
			release()
			t.statusLbl.SetText("Download error: " + err.Error())
		case folder == nil:
			release() // cancelled
		default:
			t.askDownloadName(ctx, release, remotePath, name, folder.Path())
		}
	}, t.win)
}

// askDownloadName asks for the local file name in folder, suggesting a safe
// form of the remote name, confirms replacing an existing file and starts
// the download. Must be called on the UI goroutine.
func (t *SFTPTab) askDownloadName(ctx context.Context, release func(), remotePath, name, folder string) {
	entry := widget.NewEntry()
	entry.SetText(localFileName(name))
	entry.Validator = sftpCheckFileName
	form := dialog.NewForm("Save As", "Save", "Cancel", []*widget.FormItem{
		widget.NewFormItem("Folder", widget.NewLabel(folder)),
		widget.NewFormItem("File name", entry),
	}, func(ok bool) {
		if !ok {
			release()
			return
		}
		localPath := filepath.Join(folder, entry.Text)
		start := func() {
			go t.transfer(ctx, release, "Downloading "+name, func(ctx context.Context, progress func(done, total int64)) error {
				// Opened only now, so its size is not older than the dialogs.
				d, err := t.sftp.OpenDownload(ctx, remotePath)
				if err != nil {
					return err
				}
				return d.SaveTo(ctx, localPath, func(done int64) { progress(done, d.Size) })
			}, "Downloaded → "+localPath)
		}
		question, err := sftpOverwriteQuestion(localPath)
		switch {
		case err != nil:
			release()
			t.statusLbl.SetText("Download error: " + err.Error())
		case question == "":
			start()
		default:
			dialog.ShowConfirm("Overwrite?", question, func(ok bool) {
				if ok {
					start()
				} else {
					release()
				}
			}, t.win)
		}
	}, t.win)
	entry.OnSubmitted = func(string) { form.Submit() }
	form.Resize(fyne.NewSize(480, form.MinSize().Height))
	form.Show()
}

// sftpCheckFileName accepts a file name typed for a download: a name in the
// chosen folder, not a path.
func sftpCheckFileName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("enter a file name")
	case name == "." || name == "..":
		return fmt.Errorf("%q is not a file name", name)
	case strings.ContainsAny(name, `/\`):
		return errors.New(`a file name cannot contain "/" or "\"`)
	}
	return nil
}

// sftpOverwriteQuestion checks the local target of a download. It returns
// "" if nothing exists at localPath, the question to ask before replacing
// the file there, or an error if a download can't replace it (a folder, a
// device, a broken link). A symbolic link stays: the file it points to is
// replaced.
func sftpOverwriteQuestion(localPath string) (string, error) {
	fi, err := os.Lstat(localPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	case fi.IsDir():
		return "", fmt.Errorf("%s is a folder", localPath)
	case fi.Mode().IsRegular():
		return localPath + " already exists.\nReplace it?", nil
	case fi.Mode()&fs.ModeSymlink == 0:
		return "", fmt.Errorf("%s is not a regular file", localPath)
	}
	target, err := os.Stat(localPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%s is a link to a file that does not exist", localPath)
	case err != nil:
		return "", err
	case target.IsDir():
		return "", fmt.Errorf("%s is a link to a folder", localPath)
	case !target.Mode().IsRegular():
		return "", fmt.Errorf("%s is a link to something other than a regular file", localPath)
	}
	dest, err := filepath.EvalSymlinks(localPath)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s is a symbolic link to\n%s.\nThe file it points to will be replaced.\nReplace it?", localPath, dest), nil
}

func (t *SFTPTab) showUploadDialog() {
	ctx, release, ok := t.reserveTransfer()
	if !ok {
		t.statusLbl.SetText(sftpBusy)
		return
	}
	dialog.ShowFileOpen(func(f fyne.URIReadCloser, err error) {
		if err != nil {
			release()
			t.statusLbl.SetText("Upload error: " + err.Error())
			return
		}
		if f == nil {
			release() // cancelled
			return
		}
		localPath := f.URI().Path()
		f.Close()
		name := filepath.Base(localPath)
		remotePath := path.Join(t.dir(), name)
		upload := func() {
			go t.transfer(ctx, release, "Uploading "+name, func(ctx context.Context, progress func(done, total int64)) error {
				return t.sftp.Upload(ctx, localPath, remotePath, progress)
			}, "Uploaded "+name)
		}
		go func() {
			exists, err := t.sftp.Exists(ctx, remotePath)
			if err != nil && ctx.Err() == nil {
				release()
				t.setStatus("Upload error: " + t.remoteErr(err))
				return
			}
			if !exists || ctx.Err() != nil {
				upload() // reports the cancel if Cancel Transfer was pressed meanwhile
				return
			}
			fyne.Do(func() {
				dialog.ShowConfirm("Overwrite?", remotePath+" already exists on the server.\nReplace it?", func(ok bool) {
					if ok {
						upload()
					} else {
						release()
					}
				}, t.win)
			})
		}()
	}, t.win)
}

// transfer runs an upload or download in the transfer slot taken with
// reserveTransfer, with progress in the status line. It frees the slot when
// run returns, before reloading the listing. Blocks — call it from a
// goroutine.
func (t *SFTPTab) transfer(ctx context.Context, release func(), what string, run func(ctx context.Context, progress func(done, total int64)) error, success string) {
	t.setStatus(what + "…")
	var last time.Time
	err := ctx.Err() // cancelled before it started
	if err == nil {
		err = run(ctx, func(done, total int64) {
			if time.Since(last) < 200*time.Millisecond {
				return
			}
			last = time.Now()
			if total > 0 {
				t.setStatus(fmt.Sprintf("%s… %d%% (%s of %s)", what, done*100/total, humanSize(done), humanSize(total)))
			} else {
				t.setStatus(fmt.Sprintf("%s… %s", what, humanSize(done)))
			}
		})
	}
	// Read before the slot is freed, which lets a closing tab close the
	// connection.
	closed := t.sftp.Closed()
	release()
	switch {
	case errors.Is(err, context.Canceled) && closed:
		// The server did not stop in time, so core closed the connection.
		t.setStatus(what + " cancelled; " + sftpClosedMsg)
	case errors.Is(err, context.Canceled):
		t.setStatus(what + " cancelled")
	case err != nil:
		t.setStatus(what + " failed: " + t.remoteErr(err))
	default:
		t.refresh()
		t.setStatus(success)
	}
}

// setStatus may be called from any goroutine.
func (t *SFTPTab) setStatus(msg string) {
	fyne.Do(func() { t.statusLbl.SetText(msg) })
}

// sftpClosedMsg is shown instead of the error of a request that failed
// because the tab's connection is closed (after a cancelled transfer the
// server did not end in time, or an invalid reply): only a new SFTP tab
// helps then.
const sftpClosedMsg = "SFTP connection closed — reopen SFTP"

// remoteErr is the status text for a failed SFTP request.
func (t *SFTPTab) remoteErr(err error) string {
	if t.sftp.Closed() {
		return sftpClosedMsg
	}
	return shortErr(err)
}

// Limits for error text in a dialog or status line: a server can send a
// huge message (Fyne then spends seconds laying out a dialog taller than
// the window) or one with control characters.
const (
	shortErrMaxLen   = 512
	shortErrMaxLines = 4
)

// shortErr is err's text for display: each line cleaned with logger.Clean,
// at most shortErrMaxLines lines and about shortErrMaxLen bytes, cut on a
// rune boundary, with "…" where something was left out. Log the full
// error where it matters.
func shortErr(err error) string {
	lines := strings.SplitN(err.Error(), "\n", shortErrMaxLines+1)
	cut := len(lines) > shortErrMaxLines
	if cut {
		lines = lines[:shortErrMaxLines]
	}
	for i, l := range lines {
		// Cut before cleaning, which only makes a line longer.
		l, c := cutText(strings.TrimSuffix(l, "\r"), shortErrMaxLen)
		lines[i] = logger.Clean(l)
		cut = cut || c
	}
	s, c := cutText(strings.Join(lines, "\n"), shortErrMaxLen)
	if cut || c {
		s += "…"
	}
	return s
}

// cutText returns s cut to at most n bytes on a rune boundary, and whether
// anything was cut.
func cutText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// localFileName makes a server-supplied file name safe to suggest in the
// local save dialog, which joins it to the chosen folder: a malicious server
// could otherwise name a file "../../.bashrc" and have it written elsewhere.
// Path separators, characters Windows forbids, control characters and
// invisible format characters are replaced — the latter include bidi
// overrides that make "invoice‮txt.exe" display as "invoiceexe.txt".
func localFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	// Windows drops trailing dots and spaces, and names like "nul" or
	// "COM1.txt" refer to devices instead of files.
	name = strings.TrimRight(name, ". ")
	if name == "" || strings.Trim(name, ".") == "" {
		return "download"
	}
	base := strings.ToUpper(strings.TrimSpace(strings.SplitN(name, ".", 2)[0]))
	if windowsDeviceNames[base] {
		name = "_" + name
	}
	return name
}

// windowsDeviceNames are the reserved device names, in upper case. Windows
// also counts 0 and the superscript digits ¹ ² ³ for COM and LPT.
var windowsDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"CONIN$": true, "CONOUT$": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"COM¹": true, "COM²": true, "COM³": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	"LPT¹": true, "LPT²": true, "LPT³": true,
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
		if exp >= 5 {
			break
		}
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

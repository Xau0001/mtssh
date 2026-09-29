package ui

import (
	"context"
	"errors"
	"fmt"
	"mtssh/core"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// SFTPTab shows a file manager for a remote SSH server. Network operations
// run in goroutines; their results are applied on the UI goroutine.
type SFTPTab struct {
	sftp       *core.SFTPClient
	win        fyne.Window
	mu         sync.RWMutex // guards currentDir, entries and cancel
	currentDir string
	entries    []core.FileEntry
	cancel     context.CancelFunc // cancels the running transfer, nil if none
	list       *widget.List
	pathLabel  *widget.Label
	statusLbl  *widget.Label
	cancelBtn  *widget.Button
	Container  fyne.CanvasObject
}

// NewSFTPTab creates an SFTP file manager. Must be called on the UI goroutine;
// the initial directory listing is loaded in the background.
func NewSFTPTab(sftpClient *core.SFTPClient, win fyne.Window) *SFTPTab {
	t := &SFTPTab{sftp: sftpClient, win: win}
	t.buildUI()
	go func() {
		cwd, err := sftpClient.Getwd()
		if err != nil {
			cwd = "/"
		}
		t.load(cwd)
	}()
	return t
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

// refresh reloads the current directory. Blocks on the network — call it
// from a goroutine.
func (t *SFTPTab) refresh() {
	if dir := t.dir(); dir != "" {
		t.load(dir)
	}
}

// load lists dir and makes it the current directory. On error (e.g.
// permission denied) the previous directory stays current. Blocks on the
// network — call it from a goroutine.
func (t *SFTPTab) load(dir string) {
	t.setStatus("Loading…")
	entries, err := t.sftp.ListDir(dir)
	if err != nil {
		fyne.Do(func() {
			t.mu.Lock()
			if t.currentDir == "" {
				// The first listing failed (e.g. no permission for the home
				// directory): still make dir current so Up and Refresh work.
				t.currentDir = dir
				t.pathLabel.SetText(dir)
			}
			t.mu.Unlock()
			t.statusLbl.SetText("Error: " + err.Error())
		})
		return
	}
	// Sort: dirs first, then by name
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	fyne.Do(func() {
		t.mu.Lock()
		t.currentDir = dir
		t.entries = entries
		t.mu.Unlock()
		t.pathLabel.SetText(dir)
		t.list.UnselectAll()
		t.list.Refresh()
		t.statusLbl.SetText(fmt.Sprintf("%d items", len(entries)))
	})
}

func (t *SFTPTab) showFileMenu(e core.FileEntry) {
	dir := t.dir()
	remotePath := path.Join(dir, e.Name)
	var menu dialog.Dialog

	downloadBtn := widget.NewButton("Download", func() {
		menu.Hide()
		go t.download(remotePath, e.Name)
	})

	deleteBtn := widget.NewButton("Delete", func() {
		dialog.ShowConfirm("Delete", "Delete "+e.Name+"?", func(ok bool) {
			if !ok {
				return
			}
			menu.Hide()
			go func() {
				if err := t.sftp.Delete(remotePath); err != nil {
					t.setStatus("Delete error: " + err.Error())
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
				t.setStatus("Rename error: " + err.Error())
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
		downloadBtn,
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
				t.setStatus("Mkdir error: " + err.Error())
			} else {
				t.refresh()
				t.setStatus("Created " + name)
			}
		}()
	}, t.win)
}

// download opens the remote file first, then asks where to save it: the
// save dialog empties the chosen file, so the remote side must not fail
// after that. Blocks on the network — call it from a goroutine.
func (t *SFTPTab) download(remotePath, name string) {
	d, err := t.sftp.OpenDownload(remotePath)
	if err != nil {
		t.setStatus("Download error: " + err.Error())
		return
	}
	fyne.Do(func() {
		save := dialog.NewFileSave(func(f fyne.URIWriteCloser, err error) {
			if err != nil || f == nil {
				d.Close()
				return
			}
			localPath := f.URI().Path()
			f.Close()
			go t.transfer("Downloading "+name, func(ctx context.Context, progress func(done, total int64)) error {
				return d.SaveTo(ctx, localPath, func(done int64) { progress(done, d.Size) })
			}, "Downloaded → "+localPath, d.Close)
		}, t.win)
		save.SetFileName(localFileName(name))
		save.Show()
	})
}

func (t *SFTPTab) showUploadDialog() {
	dialog.ShowFileOpen(func(f fyne.URIReadCloser, err error) {
		if err != nil || f == nil {
			return
		}
		localPath := f.URI().Path()
		f.Close()
		name := filepath.Base(localPath)
		remotePath := path.Join(t.dir(), name)
		upload := func() {
			go t.transfer("Uploading "+name, func(ctx context.Context, progress func(done, total int64)) error {
				return t.sftp.Upload(ctx, localPath, remotePath, progress)
			}, "Uploaded "+name, nil)
		}
		go func() {
			exists, err := t.sftp.Exists(remotePath)
			if err != nil {
				t.setStatus("Upload error: " + err.Error())
				return
			}
			if !exists {
				upload()
				return
			}
			fyne.Do(func() {
				dialog.ShowConfirm("Overwrite?", remotePath+" already exists on the server.\nReplace it?", func(ok bool) {
					if ok {
						upload()
					}
				}, t.win)
			})
		}()
	}, t.win)
}

// transfer runs one upload or download with progress in the status line and
// the Cancel Transfer button enabled. Only one transfer runs at a time;
// cleanup (if not nil) is called when a transfer cannot start. Blocks —
// call it from a goroutine.
func (t *SFTPTab) transfer(what string, run func(ctx context.Context, progress func(done, total int64)) error, success string, cleanup func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.mu.Lock()
	if t.cancel != nil {
		t.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		t.setStatus("Another transfer is running — wait for it or cancel it first")
		return
	}
	t.cancel = cancel
	t.mu.Unlock()
	fyne.Do(t.cancelBtn.Enable)
	defer func() {
		t.mu.Lock()
		t.cancel = nil
		t.mu.Unlock()
		fyne.Do(t.cancelBtn.Disable)
	}()

	t.setStatus(what + "…")
	var last time.Time
	err := run(ctx, func(done, total int64) {
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
	switch {
	case errors.Is(err, context.Canceled):
		t.setStatus(what + " cancelled")
	case err != nil:
		t.setStatus(what + " failed: " + err.Error())
	default:
		t.refresh()
		t.setStatus(success)
	}
}

// setStatus may be called from any goroutine.
func (t *SFTPTab) setStatus(msg string) {
	fyne.Do(func() { t.statusLbl.SetText(msg) })
}

// localFileName makes a server-supplied file name safe to suggest in the
// local save dialog, which joins it to the chosen folder: a malicious server
// could otherwise name a file "../../.bashrc" and have it written elsewhere.
// Path separators, characters Windows forbids, control characters and
// invisible format characters are replaced — the latter include bidi
// overrides that make "invoice\u202Etxt.exe" display as "invoiceexe.txt".
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

var windowsDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
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

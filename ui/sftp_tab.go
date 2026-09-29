package ui

import (
	"fmt"
	"mtssh/core"
	"path"
	"sort"
	"strings"
	"sync"

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
	mu         sync.RWMutex // guards currentDir and entries
	currentDir string
	entries    []core.FileEntry
	list       *widget.List
	pathLabel  *widget.Label
	statusLbl  *widget.Label
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
			name.SetText(e.Name)
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

	toolbar := container.NewHBox(upBtn, refreshBtn, mkdirBtn, uploadBtn)
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
		t.setStatus("Error: " + err.Error())
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
		save := dialog.NewFileSave(func(f fyne.URIWriteCloser, err error) {
			if err != nil || f == nil {
				return
			}
			localPath := f.URI().Path()
			f.Close()
			go func() {
				t.setStatus("Downloading " + e.Name + "…")
				if err := t.sftp.Download(remotePath, localPath); err != nil {
					t.setStatus("Download error: " + err.Error())
				} else {
					t.setStatus("Downloaded → " + localPath)
				}
			}()
		}, t.win)
		save.SetFileName(localFileName(e.Name))
		save.Show()
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

func (t *SFTPTab) showUploadDialog() {
	dialog.ShowFileOpen(func(f fyne.URIReadCloser, err error) {
		if err != nil || f == nil {
			return
		}
		localPath := f.URI().Path()
		f.Close()
		remotePath := path.Join(t.dir(), path.Base(localPath))
		go func() {
			t.setStatus("Uploading " + path.Base(localPath) + "…")
			if err := t.sftp.Upload(localPath, remotePath); err != nil {
				t.setStatus("Upload error: " + err.Error())
			} else {
				t.refresh()
				t.setStatus("Uploaded " + path.Base(localPath))
			}
		}()
	}, t.win)
}

// setStatus may be called from any goroutine.
func (t *SFTPTab) setStatus(msg string) {
	fyne.Do(func() { t.statusLbl.SetText(msg) })
}

// localFileName makes a server-supplied file name safe to suggest in the
// local save dialog, which joins it to the chosen folder: a malicious server
// could otherwise name a file "../../.bashrc" and have it written elsewhere.
// Path separators, characters Windows forbids and control characters are
// replaced.
func localFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	if strings.Trim(name, ". ") == "" {
		return "download"
	}
	return name
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

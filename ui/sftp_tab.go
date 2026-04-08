package ui

import (
	"fmt"
	"mtputty/core"
	"path"
	"sort"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"golang.org/x/crypto/ssh"
)

// SFTPTab shows a file manager for a remote SSH server
type SFTPTab struct {
	sftp      *core.SFTPClient
	win       fyne.Window
	currentDir string
	entries   []core.FileEntry
	list      *widget.List
	pathLabel *widget.Label
	statusLbl *widget.Label
	Container fyne.CanvasObject
}

// NewSFTPTab creates an SFTP file manager connected via sshClient
func NewSFTPTab(sshClient *ssh.Client, win fyne.Window) (*SFTPTab, error) {
	sftpClient, err := core.NewSFTPClient(sshClient)
	if err != nil {
		return nil, err
	}

	cwd, err := sftpClient.Getwd()
	if err != nil {
		cwd = "/"
	}

	t := &SFTPTab{
		sftp:       sftpClient,
		win:        win,
		currentDir: cwd,
	}
	t.buildUI()
	t.refresh()
	return t, nil
}

func (t *SFTPTab) buildUI() {
	t.pathLabel = widget.NewLabel(t.currentDir)
	t.statusLbl = widget.NewLabel("")

	// File list
	t.list = widget.NewList(
		func() int { return len(t.entries) },
		func() fyne.CanvasObject {
			return container.NewHBox(
				widget.NewIcon(theme.FileIcon()),
				widget.NewLabel("placeholder"),
				widget.NewLabel(""),
			)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*fyne.Container)
			icon := row.Objects[0].(*widget.Icon)
			name := row.Objects[1].(*widget.Label)
			size := row.Objects[2].(*widget.Label)

			e := t.entries[id]
			if e.IsDir {
				icon.SetResource(theme.FolderIcon())
				size.SetText("<DIR>")
			} else {
				icon.SetResource(theme.FileIcon())
				size.SetText(humanSize(e.Size))
			}
			name.SetText(e.Name)
		},
	)

	// Double-click: navigate into dir or show file options
	t.list.OnSelected = func(id widget.ListItemID) {
		e := t.entries[id]
		if e.IsDir {
			t.navigate(e.Name)
		} else {
			t.showFileMenu(e)
		}
		t.list.Unselect(id)
	}

	// Toolbar buttons
	upBtn := widget.NewButtonWithIcon("Up", theme.NavigateBackIcon(), func() {
		t.navigate("..")
	})
	refreshBtn := widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), func() {
		t.refresh()
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

func (t *SFTPTab) navigate(name string) {
	var newDir string
	if name == ".." {
		newDir = path.Dir(t.currentDir)
	} else {
		newDir = path.Join(t.currentDir, name)
	}
	t.currentDir = newDir
	t.pathLabel.SetText(t.currentDir)
	t.refresh()
}

func (t *SFTPTab) refresh() {
	t.setStatus("Loading…")
	entries, err := t.sftp.ListDir(t.currentDir)
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
	t.entries = entries
	t.list.Refresh()
	t.setStatus(fmt.Sprintf("%d items", len(entries)))
}

func (t *SFTPTab) showFileMenu(e core.FileEntry) {
	remotePath := path.Join(t.currentDir, e.Name)

	downloadBtn := widget.NewButton("Download", func() {
		dialog.ShowFileSave(func(f fyne.URIWriteCloser, err error) {
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
	})

	deleteBtn := widget.NewButton("Delete", func() {
		dialog.ShowConfirm("Delete", "Delete "+e.Name+"?", func(ok bool) {
			if !ok {
				return
			}
			go func() {
				if err := t.sftp.Delete(remotePath); err != nil {
					t.setStatus("Delete error: " + err.Error())
				} else {
					t.setStatus("Deleted " + e.Name)
					t.refresh()
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
		newPath := path.Join(t.currentDir, newName)
		go func() {
			if err := t.sftp.Rename(remotePath, newPath); err != nil {
				t.setStatus("Rename error: " + err.Error())
			} else {
				t.setStatus("Renamed to " + newName)
				t.refresh()
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
	dialog.ShowCustom("File Options", "Close", content, t.win)
}

func (t *SFTPTab) showMkdirDialog() {
	entry := widget.NewEntry()
	entry.SetPlaceHolder("New folder name")
	dialog.ShowCustomConfirm("New Folder", "Create", "Cancel", entry, func(ok bool) {
		if !ok || entry.Text == "" {
			return
		}
		newPath := path.Join(t.currentDir, entry.Text)
		go func() {
			if err := t.sftp.Mkdir(newPath); err != nil {
				t.setStatus("Mkdir error: " + err.Error())
			} else {
				t.setStatus("Created " + entry.Text)
				t.refresh()
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
		remotePath := path.Join(t.currentDir, path.Base(localPath))
		go func() {
			t.setStatus("Uploading " + path.Base(localPath) + "…")
			if err := t.sftp.Upload(localPath, remotePath); err != nil {
				t.setStatus("Upload error: " + err.Error())
			} else {
				t.setStatus("Uploaded " + path.Base(localPath))
				t.refresh()
			}
		}()
	}, t.win)
}

func (t *SFTPTab) setStatus(msg string) {
	t.statusLbl.SetText(msg)
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
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

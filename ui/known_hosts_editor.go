package ui

import (
	"bufio"
	"fmt"
	"mtssh/core"
	"os"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// KnownHostEntry represents a single parsed line from known_hosts
type KnownHostEntry struct {
	Hostname string
	KeyType  string
	Raw      string // full original line
}

// ShowKnownHostsEditor opens a window with a table of all known hosts
func ShowKnownHostsEditor(app fyne.App) {
	path, err := core.KnownHostsPath()
	if err != nil {
		if w := app.Driver().AllWindows(); len(w) > 0 {
			dialog.ShowError(err, w[0])
		}
		return
	}
	win := app.NewWindow("Known Hosts Manager")
	win.Resize(fyne.NewSize(800, 500))

	var entries []KnownHostEntry
	statusLbl := widget.NewLabel("")
	selectedKH := -1
	var list *widget.List

	loadEntries := func() {
		entries = parseKnownHosts(path)
		statusLbl.SetText(fmt.Sprintf("%d entries in %s", len(entries), path))
		// Indices change after a reload — drop the stale selection
		selectedKH = -1
		if list != nil {
			list.UnselectAll()
			list.Refresh()
		}
	}
	loadEntries()

	list = widget.NewList(
		func() int { return len(entries) },
		func() fyne.CanvasObject {
			return container.NewHBox(
				widget.NewIcon(theme.ComputerIcon()),
				widget.NewLabel("hostname"),
				widget.NewLabel("keytype"),
			)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*fyne.Container)
			row.Objects[1].(*widget.Label).SetText(entries[id].Hostname)
			row.Objects[2].(*widget.Label).SetText(entries[id].KeyType)
		},
	)
	list.OnSelected = func(id widget.ListItemID) { selectedKH = int(id) }
	list.OnUnselected = func(id widget.ListItemID) { selectedKH = -1 }

	deleteBtn := widget.NewButtonWithIcon("Delete Selected", theme.DeleteIcon(), func() {
		sel := selectedKH
		if sel < 0 || sel >= len(entries) {
			dialog.ShowInformation("Delete", "Please select an entry first.", win)
			return
		}
		entry := entries[sel]
		dialog.ShowConfirm(
			"Delete Host Key",
			fmt.Sprintf("Remove key for:\n%s (%s)\n\nThe next connection to this host will show the fingerprint dialog again.", entry.Hostname, entry.KeyType),
			func(ok bool) {
				if !ok {
					return
				}
				if err := core.RemoveKnownHost(entry.Raw); err != nil {
					dialog.ShowError(err, win)
					return
				}
				loadEntries()
			}, win)
	})

	deleteAllBtn := widget.NewButtonWithIcon("Delete All", theme.DeleteIcon(), func() {
		dialog.ShowConfirm("Delete All", "Remove ALL known host entries?\nYou will be prompted to verify fingerprints on next connections.", func(ok bool) {
			if !ok {
				return
			}
			if err := core.RemoveKnownHost(""); err != nil {
				dialog.ShowError(err, win)
				return
			}
			loadEntries()
		}, win)
	})

	refreshBtn := widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), loadEntries)

	toolbar := container.NewHBox(deleteBtn, deleteAllBtn, refreshBtn)
	win.SetContent(container.NewBorder(
		container.NewVBox(
			widget.NewLabelWithStyle("Known Hosts", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
			widget.NewLabel("These hosts have been verified and their keys saved. Delete an entry to re-verify on next connection."),
			toolbar,
		),
		statusLbl,
		nil, nil,
		list,
	))
	win.Show()
}

func parseKnownHosts(path string) []KnownHostEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []KnownHostEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		entries = append(entries, KnownHostEntry{
			Hostname: parts[0],
			KeyType:  parts[1],
			Raw:      line,
		})
	}
	return entries
}

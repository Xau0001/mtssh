package ui

import (
	"fmt"
	"mtssh/core"
	"mtssh/logger"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// KnownHostEntry represents a single parsed line from known_hosts
type KnownHostEntry struct {
	Marker   string // "@cert-authority", "@revoked" or ""
	Hostname string
	KeyType  string
	Raw      string // full original line
}

// hostText is the entry's host column: the marker, if any, and the hosts.
func (e KnownHostEntry) hostText() string {
	if e.Marker == "" {
		return khDisplay(e.Hostname)
	}
	return khDisplay(e.Marker + " " + e.Hostname)
}

// khDisplay makes a known_hosts field safe to show: the file may have been
// edited by hand or copied from elsewhere, so control and invisible
// characters are escaped (see logger.Clean) and long text is shortened.
func khDisplay(s string) string {
	return logger.Clean(sessTruncate(s, sessMaxDisplayLen))
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
		data, err := core.ReadKnownHosts()
		entries = parseKnownHosts(data)
		if err != nil {
			// Connections fail the same way: say so instead of an empty list.
			statusLbl.SetText("Could not read known_hosts: " + logger.Clean(err.Error()))
		} else {
			statusLbl.SetText(fmt.Sprintf("%d entries in %s", len(entries), path))
		}
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
			row.Objects[1].(*widget.Label).SetText(entries[id].hostText())
			row.Objects[2].(*widget.Label).SetText(khDisplay(entries[id].KeyType))
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
			fmt.Sprintf("Remove key for:\n%s (%s)\n\nThe next connection to this host will show the fingerprint dialog again.", entry.hostText(), khDisplay(entry.KeyType)),
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

// parseKnownHosts lists the lines of a known_hosts file that are not blank
// or comments. Invalid lines are listed too, so they can be removed here:
// one that names a host blocks connections to it, and one longer than 64 KB
// blocks every connection. Lines have no length limit here, unlike with
// bufio.Scanner, which would stop at such a line and hide the rest.
func parseKnownHosts(data []byte) []KnownHostEntry {
	var entries []KnownHostEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		e := KnownHostEntry{Raw: line}
		parts := strings.Fields(line)
		if strings.HasPrefix(parts[0], "@") {
			e.Marker, parts = parts[0], parts[1:]
		}
		if len(parts) > 0 {
			e.Hostname = parts[0]
		}
		if len(parts) > 1 {
			e.KeyType = parts[1]
		}
		entries = append(entries, e)
	}
	return entries
}

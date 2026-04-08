package ui

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
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

func knownHostsFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mtputty", "known_hosts")
}

// ShowKnownHostsEditor opens a window with a table of all known hosts
func ShowKnownHostsEditor(app fyne.App) {
	win := app.NewWindow("Known Hosts Manager")
	win.Resize(fyne.NewSize(800, 500))

	var entries []KnownHostEntry
	statusLbl := widget.NewLabel("")

	loadEntries := func() {
		entries = parseKnownHosts(knownHostsFilePath())
		statusLbl.SetText(fmt.Sprintf("%d entries in %s", len(entries), knownHostsFilePath()))
	}
	loadEntries()

	list := widget.NewList(
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

	deleteBtn := widget.NewButtonWithIcon("Delete Selected", theme.DeleteIcon(), func() {
		sel := list.GetSelectedIndex()
		if sel < 0 {
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
				if err := deleteKnownHostLine(knownHostsFilePath(), entry.Raw); err != nil {
					dialog.ShowError(err, win)
					return
				}
				loadEntries()
				list.Refresh()
			}, win)
	})

	deleteAllBtn := widget.NewButtonWithIcon("Delete All", theme.DeleteIcon(), func() {
		dialog.ShowConfirm("Delete All", "Remove ALL known host entries?\nYou will be prompted to verify fingerprints on next connections.", func(ok bool) {
			if !ok {
				return
			}
			os.WriteFile(knownHostsFilePath(), []byte{}, 0600)
			loadEntries()
			list.Refresh()
		}, win)
	})

	refreshBtn := widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), func() {
		loadEntries()
		list.Refresh()
	})

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

func deleteKnownHostLine(path, rawLine string) error {
	lines, err := readAllLines(path)
	if err != nil {
		return err
	}
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != strings.TrimSpace(rawLine) {
			out = append(out, l)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0600)
}

func readAllLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(data), "\n"), nil
}

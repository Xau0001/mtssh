package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"mtssh/config"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// ExportSessions writes sessions as readable JSON to a file chosen by the user.
// Passwords are only included (in plain text) if the user explicitly opts in.
func ExportSessions(win fyne.Window, sessions []config.Session) {
	withPasswords := widget.NewCheck("Include passwords (stored unencrypted!)", nil)
	content := container.NewVBox(
		widget.NewLabel("Sessions will be exported as plain JSON."),
		withPasswords,
	)
	dialog.ShowCustomConfirm("Export Sessions", "Export", "Cancel", content, func(ok bool) {
		if !ok {
			return
		}
		out := make([]config.Session, len(sessions))
		copy(out, sessions)
		if !withPasswords.Checked {
			for i := range out {
				out[i].Password = ""
			}
		}
		save := dialog.NewFileSave(func(f fyne.URIWriteCloser, err error) {
			if err != nil || f == nil {
				return
			}
			defer f.Close()

			data, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				dialog.ShowError(err, win)
				return
			}
			if _, err := f.Write(data); err != nil {
				dialog.ShowError(err, win)
				return
			}
			dialog.ShowInformation("Export", fmt.Sprintf("Exported %d sessions to:\n%s", len(out), f.URI().Path()), win)
		}, win)
		save.SetFileName("mtssh-sessions.json")
		save.Show()
	}, win)
}

// ImportSessions reads a JSON export file, merges it into the existing
// sessions and calls onImport with the merged slice.
func ImportSessions(win fyne.Window, existing []config.Session, onImport func([]config.Session)) {
	dialog.ShowFileOpen(func(f fyne.URIReadCloser, err error) {
		if err != nil || f == nil {
			return
		}
		defer f.Close()

		data, err := io.ReadAll(f)
		if err != nil {
			dialog.ShowError(err, win)
			return
		}

		var imported []config.Session
		if err := json.Unmarshal(data, &imported); err != nil {
			dialog.ShowError(fmt.Errorf("invalid session file: %w", err), win)
			return
		}

		if len(imported) == 0 {
			dialog.ShowInformation("Import", "No sessions found in file.", win)
			return
		}

		// Build existing ID set to avoid duplicates
		existingIDs := map[string]bool{}
		for _, s := range existing {
			existingIDs[s.ID] = true
		}

		// Copy so the caller's slice is never modified in place
		merged := append([]config.Session(nil), existing...)
		var added, skipped int
		for i := range imported {
			s := &imported[i]
			if s.Port == 0 {
				s.Port = 22
			}
			if existingIDs[s.ID] {
				skipped++
				continue
			}
			// Generate new ID if missing
			if s.ID == "" {
				s.ID = randomID()
			}
			existingIDs[s.ID] = true
			merged = append(merged, *s)
			added++
		}

		// Show merge result with a custom dialog
		msg := fmt.Sprintf(
			"Import complete:\n• %d new sessions added\n• %d skipped (already exist)\n• %d total sessions",
			added, skipped, len(merged),
		)

		if skipped > 0 {
			// Offer to overwrite duplicates
			dialog.ShowCustomConfirm("Import Result", "Overwrite Duplicates", "Keep Existing",
				widget.NewLabel(msg+"\n\nDo you want to overwrite existing sessions with imported data?"),
				func(overwrite bool) {
					if overwrite {
						// Replace existing with imported where IDs match
						idxByID := map[string]int{}
						for i, s := range merged {
							idxByID[s.ID] = i
						}
						for _, s := range imported {
							if i, ok := idxByID[s.ID]; ok {
								// Exports omit passwords by default — keep the stored one
								if s.Password == "" {
									s.Password = merged[i].Password
								}
								merged[i] = s
							}
						}
					}
					onImport(merged)
				}, win)
		} else {
			dialog.ShowInformation("Import Result", msg, win)
			onImport(merged)
		}
	}, win)
}

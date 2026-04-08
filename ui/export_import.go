package ui

import (
	"encoding/json"
	"fmt"
	"mtputty/config"
	"os"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// exportSessions writes sessions as readable JSON to a file chosen by the user.
// Passwords are included in plain text — warn the user.
func ExportSessions(win fyne.Window, sessions []config.Session) {
	dialog.ShowConfirm(
		"Export Sessions",
		"Sessions will be exported as plain JSON.\n⚠ Passwords are stored unencrypted in the export file.\n\nContinue?",
		func(ok bool) {
			if !ok {
				return
			}
			dialog.ShowFileSave(func(f fyne.URIWriteCloser, err error) {
				if err != nil || f == nil {
					return
				}
				defer f.Close()

				data, err := json.MarshalIndent(sessions, "", "  ")
				if err != nil {
					dialog.ShowError(err, win)
					return
				}
				if _, err := f.Write(data); err != nil {
					dialog.ShowError(err, win)
					return
				}
				dialog.ShowInformation("Export", fmt.Sprintf("Exported %d sessions to:\n%s", len(sessions), f.URI().Path()), win)
			}, win)
		}, win)
}

// ImportSessions reads a JSON export file and merges it into the existing sessions.
// Returns the merged slice and calls onImport with it.
func ImportSessions(win fyne.Window, existing []config.Session, onImport func([]config.Session)) {
	dialog.ShowFileOpen(func(f fyne.URIReadCloser, err error) {
		if err != nil || f == nil {
			return
		}
		defer f.Close()

		data, err := os.ReadFile(f.URI().Path())
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

		var newSessions []config.Session
		var skipped int
		for _, s := range imported {
			if existingIDs[s.ID] {
				skipped++
				continue
			}
			// Generate new ID if missing
			if s.ID == "" {
				s.ID = randomID()
			}
			newSessions = append(newSessions, s)
		}

		merged := append(existing, newSessions...)

		// Show merge result with a custom dialog
		msg := fmt.Sprintf(
			"Import complete:\n• %d new sessions added\n• %d skipped (already exist)\n• %d total sessions",
			len(newSessions), skipped, len(merged),
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

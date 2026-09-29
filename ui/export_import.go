package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"mtssh/config"
	"mtssh/logger"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// maxImportSize bounds the size of an import file.
const maxImportSize = 10 << 20

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
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			dialog.ShowError(err, win)
			return
		}
		save := dialog.NewFileSave(func(f fyne.URIWriteCloser, err error) {
			if err != nil || f == nil {
				return
			}
			uri := f.URI()
			if uri.Scheme() != "file" {
				defer f.Close()
				if withPasswords.Checked {
					dialog.ShowError(fmt.Errorf("passwords can only be exported to a local file"), win)
					return
				}
				if _, err := f.Write(data); err != nil {
					dialog.ShowError(err, win)
				}
				return
			}
			// The dialog has already created the file with default
			// permissions. Replace it with one that is private from the start.
			f.Close()
			if err := writePrivateFile(uri.Path(), data, withPasswords.Checked); err != nil {
				dialog.ShowError(err, win)
				return
			}
			dialog.ShowInformation("Export", fmt.Sprintf("Exported %d sessions to:\n%s", len(out), uri.Path()), win)
		}, win)
		save.SetFileName("mtssh-sessions.json")
		save.Show()
	}, win)
}

// writePrivateFile replaces path with a new file containing data that only
// the user can read (mode 0600 from its creation). With secret set, it
// refuses to write to a file system that cannot keep the file private (e.g.
// FAT, where every file is readable by all).
func writePrivateFile(path string, data []byte, secret bool) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".mtssh-export-*") // mode 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed
	if secret && runtime.GOOS != "windows" {
		if fi, err := f.Stat(); err != nil || fi.Mode().Perm()&0o077 != 0 {
			f.Close()
			return fmt.Errorf("files in %s are readable by other users (file system without permissions?) — choose another folder or export without passwords", filepath.Dir(path))
		}
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// importResult is an import file checked against the existing sessions.
type importResult struct {
	added      []config.Session // new sessions
	duplicates []config.Session // sessions whose ID exists already
	invalid    []string         // why entries were skipped
}

// parseImport reads an export file. Entries are checked like sessions
// entered in the dialog; imported sessions never connect automatically.
func parseImport(data []byte, existing []config.Session) (importResult, error) {
	var res importResult
	var imported []config.Session
	if err := json.Unmarshal(data, &imported); err != nil {
		return res, fmt.Errorf("invalid session file: %w", err)
	}
	existingIDs := map[string]bool{}
	for _, s := range existing {
		existingIDs[s.ID] = true
	}
	seen := map[string]bool{}
	for i, s := range imported {
		if s.Port == 0 {
			s.Port = 22
		}
		if err := normalizeSession(&s); err != nil {
			res.invalid = append(res.invalid, fmt.Sprintf("entry %d (%s): %v", i+1, logger.Clean(s.Label), err))
			continue
		}
		if s.ID == "" || len(s.ID) > 64 || strings.ContainsFunc(s.ID, badRune) {
			s.ID = randomID()
		}
		if seen[s.ID] {
			res.invalid = append(res.invalid, fmt.Sprintf("entry %d (%s): duplicate id", i+1, s.Label))
			continue
		}
		seen[s.ID] = true
		if existingIDs[s.ID] {
			res.duplicates = append(res.duplicates, s)
			continue
		}
		// Only the user decides which sessions connect on start.
		s.AutoConnect = false
		res.added = append(res.added, s)
	}
	return res, nil
}

// overwriteSessions replaces sessions with the imported duplicates of the
// same ID. The stored password is kept only if the imported entry has none
// and still points at the same account (host, port, user): otherwise an
// edited file could send it to another host. Auto-connect stays as it was.
func overwriteSessions(sessions, duplicates []config.Session) []config.Session {
	idx := map[string]int{}
	for i, s := range sessions {
		idx[s.ID] = i
	}
	for _, s := range duplicates {
		i, ok := idx[s.ID]
		if !ok {
			continue
		}
		old := sessions[i]
		if s.Password == "" && sameAccount(old, s) {
			s.Password = old.Password
		}
		s.AutoConnect = old.AutoConnect
		sessions[i] = s
	}
	return sessions
}

func sameAccount(a, b config.Session) bool {
	return strings.EqualFold(a.Host, b.Host) && a.Port == b.Port && a.User == b.User
}

// describeOverwrite lists what overwriting would change about where each
// session connects.
func describeOverwrite(sessions, duplicates []config.Session) string {
	byID := map[string]config.Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	var b strings.Builder
	for _, s := range duplicates {
		old := byID[s.ID]
		fmt.Fprintf(&b, "• %s", old.Label)
		if !sameAccount(old, s) {
			fmt.Fprintf(&b, ": %s@%s:%d → %s@%s:%d", old.User, old.Host, old.Port, s.User, s.Host, s.Port)
			if s.Password == "" && old.Password != "" {
				b.WriteString(" (stored password removed)")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ImportSessions reads a JSON export file, merges it into the existing
// sessions and calls onImport with the merged slice.
func ImportSessions(win fyne.Window, existing []config.Session, onImport func([]config.Session)) {
	dialog.ShowFileOpen(func(f fyne.URIReadCloser, err error) {
		if err != nil || f == nil {
			return
		}
		defer f.Close()

		data, err := io.ReadAll(io.LimitReader(f, maxImportSize+1))
		if err != nil {
			dialog.ShowError(err, win)
			return
		}
		if len(data) > maxImportSize {
			dialog.ShowError(fmt.Errorf("file is larger than %d MB — not a session export", maxImportSize>>20), win)
			return
		}
		res, err := parseImport(data, existing)
		if err != nil {
			dialog.ShowError(err, win)
			return
		}
		if len(res.added)+len(res.duplicates)+len(res.invalid) == 0 {
			dialog.ShowInformation("Import", "No sessions found in file.", win)
			return
		}

		// Copy so the caller's slice is never modified in place
		merged := append(append([]config.Session(nil), existing...), res.added...)
		msg := fmt.Sprintf(
			"Import complete:\n• %d new sessions added (auto-connect off)\n• %d skipped (already exist)\n• %d invalid entries skipped\n• %d total sessions",
			len(res.added), len(res.duplicates), len(res.invalid), len(merged),
		)
		if len(res.invalid) > 0 {
			msg += "\n\nSkipped:\n" + strings.Join(res.invalid, "\n")
		}

		if len(res.duplicates) == 0 {
			dialog.ShowInformation("Import Result", msg, win)
			onImport(merged)
			return
		}
		// Offer to overwrite duplicates, showing where they would connect.
		text := widget.NewLabel(msg + "\n\nOverwrite existing sessions with the imported data?\n" +
			describeOverwrite(existing, res.duplicates))
		text.Wrapping = fyne.TextWrapWord
		scroll := container.NewVScroll(text)
		scroll.SetMinSize(fyne.NewSize(480, 240))
		dialog.ShowCustomConfirm("Import Result", "Overwrite Duplicates", "Keep Existing", scroll,
			func(overwrite bool) {
				if overwrite {
					merged = overwriteSessions(merged, res.duplicates)
				}
				onImport(merged)
			}, win)
	}, win)
}

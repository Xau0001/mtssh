package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mtssh/config"
	"mtssh/logger"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// maxImportSize bounds the size of an import file.
const maxImportSize = 10 << 20

const (
	// sessMaxImportEntries bounds the number of entries in an import file.
	sessMaxImportEntries = 10000
	// sessMaxListed is how many skipped entries, lookalike sessions or
	// overwritten sessions the import result lists.
	sessMaxListed = 20
	// sessMaxShown is how many characters of a label or other value the
	// import result shows.
	sessMaxShown = 64
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
				dialog.ShowError(sessExportFailed(uri.Path(), err), win)
				return
			}
			dialog.ShowInformation("Export", fmt.Sprintf("Exported %d sessions to:\n%s", len(out), uri.Path()), win)
		}, win)
		save.SetFileName("mtssh-sessions.json")
		save.Show()
	}, win)
}

// writePrivateFile replaces path with a new file containing data. On
// Unix-like systems only the user can read it (mode 0600 from its
// creation), and with secret set it refuses to write to a file system that
// cannot keep the file private (e.g. FAT, where every file is readable by
// all). On Windows the mode does nothing: the file inherits the permissions
// of the folder it is written to, and there is no such check.
func writePrivateFile(path string, data []byte, secret bool) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".mtssh-export-*") // mode 0600 (not on Windows)
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

// sessExportFailed cleans up after writePrivateFile failed to write path
// and returns the error to show. The save dialog has already created path,
// or truncated it, so it holds nothing: it is removed instead of being left
// behind as an empty file (a new one with default permissions). Only a
// regular file that is verified to be empty is removed, never a symlink or
// a file with data in it.
func sessExportFailed(path string, err error) error {
	if fi, serr := os.Lstat(path); serr == nil && fi.Mode().IsRegular() && fi.Size() == 0 {
		os.Remove(path)
	}
	return fmt.Errorf("nothing was written: %w", err)
}

// importResult is an import file checked against the existing sessions.
type importResult struct {
	added      []config.Session // new sessions
	lookalikes []config.Session // added sessions named like an existing one, for another account
	duplicates []config.Session // sessions whose ID exists already
	invalid    []string         // why entries were skipped (the first sessMaxListed)
	nInvalid   int              // number of skipped entries
}

// skip records that entry n (counted from 1) was skipped.
func (r *importResult) skip(n int, label, reason string) {
	r.nInvalid++
	if len(r.invalid) < sessMaxListed {
		r.invalid = append(r.invalid, fmt.Sprintf("entry %d (%s): %s", n,
			logger.Clean(sessTruncate(label, sessMaxShown)), logger.Clean(sessTruncate(reason, 4*sessMaxShown))))
	}
}

// summary describes the result of the import; total is the number of
// sessions afterwards.
func (r *importResult) summary(total int) string {
	msg := fmt.Sprintf(
		"Import complete:\n• %d new sessions added (auto-connect off)\n• %d skipped (already exist)\n• %d invalid entries skipped\n• %d total sessions",
		len(r.added), len(r.duplicates), r.nInvalid, total,
	)
	// New sessions are otherwise only counted. One named like an existing
	// session but connecting elsewhere could pass for it in the list.
	if n := len(r.lookalikes); n > 0 {
		msg += "\n\nCheck these new sessions — same label and group as an existing session, but another account:"
		for _, s := range r.lookalikes[:min(n, sessMaxListed)] {
			msg += "\n• " + sessShow(s.Label)
			if s.Group != "" {
				msg += " (" + sessShow(s.Group) + ")"
			}
			msg += ": " + sessAccount(s)
		}
		if n > sessMaxListed {
			msg += fmt.Sprintf("\n…and %d more", n-sessMaxListed)
		}
	}
	if r.nInvalid > 0 {
		msg += "\n\nSkipped:\n" + strings.Join(r.invalid, "\n")
		if more := r.nInvalid - len(r.invalid); more > 0 {
			msg += fmt.Sprintf("\n…and %d more", more)
		}
	}
	return msg
}

// parseImport reads an export file: a JSON list of sessions (or null).
// Entries are checked like sessions entered in the dialog; imported
// sessions never connect automatically. A file that is not such a list, or
// has more than sessMaxImportEntries entries, is refused as a whole. New
// sessions with the label and group of an existing session but another
// account are also recorded as lookalikes: in the list they would look
// like the existing one.
func parseImport(data []byte, existing []config.Session) (importResult, error) {
	var res importResult
	// One entry at a time: a small file can hold a huge number of empty
	// entries.
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return res, fmt.Errorf("invalid session file: %w", err)
	}
	if tok == nil { // null
		return res, sessImportEnd(dec)
	}
	if tok != json.Delim('[') {
		return res, errors.New("invalid session file: not a list of sessions")
	}
	existingIDs := map[string]bool{}
	byName := map[string][]config.Session{}
	for _, s := range existing {
		existingIDs[s.ID] = true
		k := sessNameKey(s)
		byName[k] = append(byName[k], s)
	}
	seen := map[string]bool{}
	for i := 0; dec.More(); i++ {
		if i == sessMaxImportEntries {
			return importResult{}, fmt.Errorf("invalid session file: more than %d entries", sessMaxImportEntries)
		}
		var s config.Session
		if err := dec.Decode(&s); err != nil {
			return importResult{}, fmt.Errorf("invalid session file: %w", err)
		}
		if s.Port == 0 {
			s.Port = 22
		}
		if err := normalizeSession(&s); err != nil {
			res.skip(i+1, s.Label, err.Error())
			continue
		}
		if s.ID == "" || len(s.ID) > 64 || strings.ContainsFunc(s.ID, badRune) {
			s.ID = randomID()
		}
		if seen[s.ID] {
			res.skip(i+1, s.Label, "duplicate id")
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
		if same := byName[sessNameKey(s)]; len(same) > 0 &&
			!slices.ContainsFunc(same, func(e config.Session) bool { return sameAccount(e, s) }) {
			res.lookalikes = append(res.lookalikes, s)
		}
	}
	if _, err := dec.Token(); err != nil { // the closing "]"
		return importResult{}, fmt.Errorf("invalid session file: %w", err)
	}
	if err := sessImportEnd(dec); err != nil {
		return importResult{}, err
	}
	return res, nil
}

// sessImportEnd checks that nothing follows the session list.
func sessImportEnd(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("invalid session file: unexpected data after the session list")
	}
	return nil
}

// overwriteSessions replaces sessions with the imported duplicates of the
// same ID. The stored password is kept only if the imported entry has none
// and still points at the same account (host, port, user): otherwise an
// edited file could send it to another host. Auto-connect stays on only if
// the account and the key settings are unchanged (sessKeepAutoConnect), so
// an edited file cannot make MTSSH connect somewhere else on its own at the
// next start; imported entries never turn it on.
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
		s.AutoConnect = old.AutoConnect && sessKeepAutoConnect(old, s)
		sessions[i] = s
	}
	return sessions
}

func sameAccount(a, b config.Session) bool {
	return strings.EqualFold(a.Host, b.Host) && a.Port == b.Port && a.User == b.User
}

// sessNameKey is what a session is known by in the list: its label and
// group, ignoring case.
func sessNameKey(s config.Session) string {
	return strings.ToLower(s.Label) + "\x00" + strings.ToLower(s.Group)
}

// sessShow prepares a value for the import result: cleaned and shortened
// to sessMaxShown characters, "none" if empty.
func sessShow(v string) string {
	if v == "" {
		return "none"
	}
	return sessDisplayN(v, sessMaxShown)
}

// sessAccount shows the account s connects to as user@host:port.
func sessAccount(s config.Session) string {
	return sessShow(s.User) + "@" + net.JoinHostPort(sessShow(s.Host), strconv.Itoa(s.Port))
}

// sessKeepAutoConnect reports whether overwriting old with s leaves its
// auto-connect setting as it was: s connects to the same account with the
// same key settings.
func sessKeepAutoConnect(old, s config.Session) bool {
	return sameAccount(old, s) && old.UseKey == s.UseKey && old.KeyPath == s.KeyPath
}

// describeOverwrite lists what overwriting would change about each
// session (see overwriteSessions): its label, the account it connects to,
// the key settings, a stored password that is removed, replaced or added
// (never the password itself) and auto-connect that is turned off. Values
// are cleaned and shortened; at most sessMaxListed sessions are listed.
func describeOverwrite(sessions, duplicates []config.Session) string {
	byID := map[string]config.Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	onOff := map[bool]string{false: "off", true: "on"}
	var b strings.Builder
	for i, s := range duplicates {
		if i == sessMaxListed {
			fmt.Fprintf(&b, "…and %d more\n", len(duplicates)-i)
			break
		}
		old := byID[s.ID]
		change := func(what, from, to string) {
			fmt.Fprintf(&b, "    %s: %s → %s\n", what, from, to)
		}
		fmt.Fprintf(&b, "• %s\n", sessShow(old.Label))
		if old.Label != s.Label {
			change("label", sessShow(old.Label), sessShow(s.Label))
		}
		if !sameAccount(old, s) {
			change("account", sessAccount(old), sessAccount(s))
		}
		if old.UseKey != s.UseKey {
			change("use SSH key", onOff[old.UseKey], onOff[s.UseKey])
		}
		if old.KeyPath != s.KeyPath {
			change("key path", sessShow(old.KeyPath), sessShow(s.KeyPath))
		}
		// An imported password is used even for the same account, so say
		// so: an old export must not swap in a stale password unnoticed.
		switch {
		case s.Password == "" && old.Password != "" && !sameAccount(old, s):
			b.WriteString("    stored password removed\n")
		case s.Password != "" && old.Password == "":
			b.WriteString("    password added\n")
		case s.Password != "" && s.Password != old.Password:
			b.WriteString("    stored password replaced\n")
		}
		if old.AutoConnect && !sessKeepAutoConnect(old, s) {
			b.WriteString("    auto-connect turned off\n")
		}
	}
	return b.String()
}

// sessResultView shows text in a scrollable area, however long it is.
func sessResultView(text string) fyne.CanvasObject {
	lbl := widget.NewLabel(text)
	lbl.Wrapping = fyne.TextWrapWord
	scroll := container.NewVScroll(lbl)
	scroll.SetMinSize(fyne.NewSize(480, 240))
	return scroll
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
		if len(res.added)+len(res.duplicates)+res.nInvalid == 0 {
			dialog.ShowInformation("Import", "No sessions found in file.", win)
			return
		}

		// Copy so the caller's slice is never modified in place
		merged := append(append([]config.Session(nil), existing...), res.added...)
		msg := res.summary(len(merged))

		if len(res.duplicates) == 0 {
			dialog.ShowCustom("Import Result", "OK", sessResultView(msg), win)
			onImport(merged)
			return
		}
		// Offer to overwrite duplicates, showing what would change.
		view := sessResultView(msg + "\n\nOverwrite existing sessions with the imported data?\n" +
			describeOverwrite(existing, res.duplicates))
		dialog.ShowCustomConfirm("Import Result", "Overwrite Duplicates", "Keep Existing", view,
			func(overwrite bool) {
				if overwrite {
					merged = overwriteSessions(merged, res.duplicates)
				}
				onImport(merged)
			}, win)
	}, win)
}

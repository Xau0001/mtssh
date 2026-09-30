package ui

import (
	"mtssh/config"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// sessWalk calls visit for o and everything shown inside it.
func sessWalk(o fyne.CanvasObject, visit func(fyne.CanvasObject)) {
	visit(o)
	switch c := o.(type) {
	case *fyne.Container:
		for _, child := range c.Objects {
			sessWalk(child, visit)
		}
	case fyne.Widget:
		for _, child := range test.WidgetRenderer(c).Objects() {
			sessWalk(child, visit)
		}
	}
}

// sessTopDialog returns the entries (in order), and the checks and buttons
// (by text) of the dialog shown on top of w.
func sessTopDialog(t *testing.T, w fyne.Window) ([]*widget.Entry, map[string]*widget.Check, map[string]*widget.Button) {
	t.Helper()
	top := w.Canvas().Overlays().Top()
	if top == nil {
		t.Fatal("no dialog shown")
	}
	var entries []*widget.Entry
	checks := map[string]*widget.Check{}
	buttons := map[string]*widget.Button{}
	sessWalk(top, func(o fyne.CanvasObject) {
		switch x := o.(type) {
		case *widget.Entry:
			entries = append(entries, x)
		case *widget.Check:
			checks[x.Text] = x
		case *widget.Button:
			buttons[x.Text] = x
		}
	})
	return entries, checks, buttons
}

// sessDismissError closes the error dialog on top of w.
func sessDismissError(t *testing.T, w fyne.Window) {
	t.Helper()
	_, _, buttons := sessTopDialog(t, w)
	ok := buttons["OK"]
	if ok == nil || buttons["Save"] != nil {
		t.Fatal("no error shown")
	}
	test.Tap(ok)
}

func TestSessionDialogKeepsInputOnError(t *testing.T) {
	test.NewApp()
	w := test.NewWindow(widget.NewLabel("main"))
	defer w.Close()
	w.Resize(fyne.NewSize(900, 1000))

	var saved []config.Session
	onSave := func(s config.Session) { saved = append(saved, s) }

	ShowSessionDialog(w, nil, onSave)
	entries, checks, buttons := sessTopDialog(t, w)
	// label, host, port, user, key path, password, group
	values := []string{"web", "root@srv.example", "2222", "root", "", "secret", "grp"}
	if len(entries) != len(values) || checks["Auto-Connect on start"] == nil || buttons["Save"] == nil {
		t.Fatalf("unexpected dialog: %d entries, checks %v, buttons %v", len(entries), checks, buttons)
	}
	for i, v := range values {
		entries[i].SetText(v)
	}
	checks["Auto-Connect on start"].SetChecked(true)
	test.Tap(buttons["Save"]) // invalid host

	sessDismissError(t, w)
	entries, checks, buttons = sessTopDialog(t, w)
	for i, v := range values {
		if entries[i].Text != v {
			t.Errorf("entry %d reopened with %q, want %q", i, entries[i].Text, v)
		}
	}
	if !checks["Auto-Connect on start"].Checked || checks["Use SSH Key"].Checked {
		t.Error("checkboxes not restored")
	}
	if len(saved) != 0 {
		t.Fatalf("invalid session saved: %+v", saved)
	}

	entries[1].SetText("srv.example")
	test.Tap(buttons["Save"])
	if len(saved) != 1 {
		t.Fatalf("%d sessions saved", len(saved))
	}
	s := saved[0]
	if s.ID == "" || s.Host != "srv.example" || s.Port != 2222 || s.Password != "secret" || s.Group != "grp" || !s.AutoConnect {
		t.Fatalf("saved %+v", s)
	}

	// Editing: an invalid port is kept as typed, the ID stays the same.
	saved = nil
	ShowSessionDialog(w, &s, onSave)
	entries, checks, buttons = sessTopDialog(t, w)
	entries[2].SetText("22x")
	checks["Use SSH Key"].SetChecked(true)
	test.Tap(buttons["Save"])

	sessDismissError(t, w)
	entries, checks, buttons = sessTopDialog(t, w)
	if entries[0].Text != "web" || entries[2].Text != "22x" || !checks["Use SSH Key"].Checked {
		t.Fatalf("reopened with label %q, port %q, use key %v", entries[0].Text, entries[2].Text, checks["Use SSH Key"].Checked)
	}
	entries[2].SetText("22")
	entries[4].SetText("~/.ssh/id_ed25519") // "Use SSH Key" needs one
	test.Tap(buttons["Save"])
	if len(saved) != 1 || saved[0].ID != s.ID || saved[0].Port != 22 || !saved[0].UseKey {
		t.Fatalf("saved %+v", saved)
	}
}

// sessDialogText returns the text of the labels in the dialog on top of w.
func sessDialogText(t *testing.T, w fyne.Window) string {
	t.Helper()
	var b strings.Builder
	sessWalk(w.Canvas().Overlays().Top(), func(o fyne.CanvasObject) {
		if l, ok := o.(*widget.Label); ok {
			b.WriteString(l.Text + "\n")
		}
	})
	return b.String()
}

func TestSessionDialogUseKey(t *testing.T) {
	test.NewApp()
	w := test.NewWindow(widget.NewLabel("main"))
	defer w.Close()
	w.Resize(fyne.NewSize(900, 1000))

	var saved []config.Session
	ShowSessionDialog(w, nil, func(s config.Session) { saved = append(saved, s) })
	entries, checks, buttons := sessTopDialog(t, w)
	// label, host, port, user, key path, password, group
	for i, v := range []string{"web", "srv.example", "22", "root", " ", "secret", ""} {
		entries[i].SetText(v)
	}
	checks["Use SSH Key"].SetChecked(true)
	test.Tap(buttons["Save"])

	// No key path: refused, and the dialog reopens as it was.
	if msg := sessDialogText(t, w); !strings.Contains(msg, "SSH key path is empty") {
		t.Fatalf("error shown: %q", msg)
	}
	sessDismissError(t, w)
	entries, checks, buttons = sessTopDialog(t, w)
	if entries[5].Text != "secret" || !checks["Use SSH Key"].Checked || len(saved) != 0 {
		t.Fatalf("reopened with password %q, use key %v; saved %+v", entries[5].Text, checks["Use SSH Key"].Checked, saved)
	}

	// With a key path it is saved, without the password of the disabled field.
	entries[4].SetText("~/.ssh/id_ed25519")
	test.Tap(buttons["Save"])
	if len(saved) != 1 || !saved[0].UseKey || saved[0].KeyPath != "~/.ssh/id_ed25519" || saved[0].Password != "" {
		t.Fatalf("saved %+v", saved)
	}

	// Editing a session stored with both drops the password, too; without
	// "Use SSH Key" the password is kept.
	old := config.Session{ID: "x", Label: "l", Host: "h", Port: 22, User: "u", Password: "pw", UseKey: true, KeyPath: "~/.ssh/k"}
	saved = nil
	ShowSessionDialog(w, &old, func(s config.Session) { saved = append(saved, s) })
	_, _, buttons = sessTopDialog(t, w)
	test.Tap(buttons["Save"])
	old.UseKey = false
	ShowSessionDialog(w, &old, func(s config.Session) { saved = append(saved, s) })
	_, _, buttons = sessTopDialog(t, w)
	test.Tap(buttons["Save"])
	if len(saved) != 2 || saved[0].Password != "" || saved[1].Password != "pw" {
		t.Fatalf("saved %+v", saved)
	}
}

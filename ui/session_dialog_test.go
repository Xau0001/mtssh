package ui

import (
	"mtssh/config"
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
	test.Tap(buttons["Save"])
	if len(saved) != 1 || saved[0].ID != s.ID || saved[0].Port != 22 || !saved[0].UseKey {
		t.Fatalf("saved %+v", saved)
	}
}

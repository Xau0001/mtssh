package ui

import (
	"testing"

	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

func TestRemoveKeepsSelection(t *testing.T) {
	test.NewApp()
	d := NewDraggableTabContainer()
	for _, name := range []string{"A", "B", "C", "D"} {
		d.Append(NewDraggableTabItem(name, nil, widget.NewLabel(name)))
	}
	d.Select(2) // C

	d.Remove(0) // close A
	if got := d.Items()[d.selected].Title; got != "C" {
		t.Fatalf("after closing a tab left of the active one, %q is active, want C", got)
	}

	d.Remove(d.selected) // close C (active) → right neighbour
	if got := d.Items()[d.selected].Title; got != "D" {
		t.Fatalf("after closing the active tab, %q is active, want D", got)
	}

	d.Remove(d.selected) // close D (last) → left neighbour
	if got := d.Items()[d.selected].Title; got != "B" {
		t.Fatalf("after closing the last tab, %q is active, want B", got)
	}
}

func TestCloseAll(t *testing.T) {
	test.NewApp()
	d := NewDraggableTabContainer()
	closed := 0
	for i := 0; i < 3; i++ {
		item := NewDraggableTabItem("t", nil, widget.NewLabel("t"))
		item.OnClose = func() { closed++ }
		d.Append(item)
	}
	d.CloseAll()
	if closed != 3 || len(d.Items()) != 0 {
		t.Fatalf("closed %d tabs, %d left", closed, len(d.Items()))
	}
}

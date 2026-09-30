package ui

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2"
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

func TestDragMovesTabAcrossSeveral(t *testing.T) {
	test.NewApp()
	d := NewDraggableTabContainer()
	for _, name := range []string{"A", "B", "C", "D"} {
		d.Append(NewDraggableTabItem(name, nil, widget.NewLabel(name)))
	}
	// Fyne keeps sending the drag to the button it started on, even after
	// a swap replaced the buttons.
	btn := d.bar.Objects[0].(*dragTabButton)
	for i := 0; i < 3; i++ {
		btn.Dragged(&fyne.DragEvent{Dragged: fyne.Delta{DX: tabWidth/2 + 1}})
	}
	btn.DragEnd()
	var order []string
	for _, it := range d.Items() {
		order = append(order, it.Title)
	}
	if got := strings.Join(order, ""); got != "BCDA" {
		t.Fatalf("order after dragging A to the right end = %s, want BCDA", got)
	}
}

func TestSelectScrollsTabIntoView(t *testing.T) {
	test.NewApp()
	d := NewDraggableTabContainer()
	w := test.NewWindow(d.Container())
	defer w.Close()
	w.Resize(fyne.NewSize(3*tabWidth, 300)) // room for fewer than 3 tabs
	for i := 0; i < 10; i++ {
		d.Append(NewDraggableTabItem("tab", nil, widget.NewLabel("x")))
	}
	visible := func(i int) bool {
		tab := d.bar.Objects[i]
		left, right := tab.Position().X, tab.Position().X+tab.Size().Width
		off, width := d.scroll.Offset.X, d.scroll.Size().Width
		return width > 0 && left >= off && right <= off+width
	}
	if !visible(9) {
		t.Fatalf("appended tab not visible: offset %v, width %v", d.scroll.Offset.X, d.scroll.Size().Width)
	}
	d.Select(0)
	if !visible(0) || d.scroll.Offset.X != 0 {
		t.Fatalf("first tab not visible: offset %v", d.scroll.Offset.X)
	}
	d.Select(9)
	if !visible(9) {
		t.Fatalf("last tab not visible: offset %v", d.scroll.Offset.X)
	}
	off := d.scroll.Offset.X
	d.Select(8) // already visible: no scrolling
	if !visible(8) || d.scroll.Offset.X != off {
		t.Fatalf("offset %v after selecting a visible tab, was %v", d.scroll.Offset.X, off)
	}
	d.Select(4)
	if !visible(4) {
		t.Fatalf("middle tab not visible: offset %v", d.scroll.Offset.X)
	}
}

func TestTabBarDoesNotWidenWindow(t *testing.T) {
	test.NewApp()
	d := NewDraggableTabContainer()
	for i := 0; i < 20; i++ {
		d.Append(NewDraggableTabItem("tab", nil, widget.NewLabel("x")))
	}
	if w := d.Container().MinSize().Width; w > 2*tabWidth {
		t.Fatalf("min width with 20 tabs = %v", w)
	}
}

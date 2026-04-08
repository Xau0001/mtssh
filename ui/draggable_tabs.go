package ui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// ── Public API ────────────────────────────────────────────────────────────────

// DraggableTabItem holds the data for a single tab
type DraggableTabItem struct {
	Title   string
	Icon    fyne.Resource
	Content fyne.CanvasObject
}

// NewDraggableTabItem creates a tab item
func NewDraggableTabItem(title string, icon fyne.Resource, content fyne.CanvasObject) *DraggableTabItem {
	return &DraggableTabItem{Title: title, Icon: icon, Content: content}
}

// DraggableTabContainer is a plain struct (not a Fyne widget) that manages
// a draggable tab bar and content area. Embed it via Container() in any layout.
type DraggableTabContainer struct {
	items    []*DraggableTabItem
	selected int

	bar     *fyne.Container // horizontal row of tab buttons
	content *fyne.Container // shows the selected tab's content
	root    *fyne.Container // bar on top, content fills the rest

	// OnReordered is called whenever the user reorders tabs via drag
	OnReordered func(items []*DraggableTabItem)
}

// NewDraggableTabContainer creates an empty container (or pre-populated with items)
func NewDraggableTabContainer(items ...*DraggableTabItem) *DraggableTabContainer {
	d := &DraggableTabContainer{
		items:    items,
		selected: 0,
	}
	d.bar = container.NewHBox()
	d.content = container.NewStack()
	d.root = container.NewBorder(d.bar, nil, nil, nil, d.content)
	d.rebuild()
	return d
}

// Container returns the CanvasObject to place inside windows / layouts
func (d *DraggableTabContainer) Container() fyne.CanvasObject { return d.root }

// Append adds a new tab and selects it
func (d *DraggableTabContainer) Append(item *DraggableTabItem) {
	d.items = append(d.items, item)
	d.selected = len(d.items) - 1
	d.rebuild()
}

// Select makes tab i the active tab
func (d *DraggableTabContainer) Select(i int) {
	if i < 0 || i >= len(d.items) {
		return
	}
	d.selected = i
	d.rebuild()
}

// SelectItem selects the tab matching the given pointer
func (d *DraggableTabContainer) SelectItem(item *DraggableTabItem) {
	for i, it := range d.items {
		if it == item {
			d.Select(i)
			return
		}
	}
}

// Items returns the current (possibly reordered) tab list
func (d *DraggableTabContainer) Items() []*DraggableTabItem { return d.items }

// SelectedIndex returns the index of the active tab
func (d *DraggableTabContainer) SelectedIndex() int { return d.selected }

// ── Internal ──────────────────────────────────────────────────────────────────

// rebuild recreates all tab header buttons and refreshes the content pane.
// Called after every Append, Select, or swap.
func (d *DraggableTabContainer) rebuild() {
	buttons := make([]fyne.CanvasObject, len(d.items))
	for i, item := range d.items {
		i, item := i, item // capture loop vars

		btn := newDragTabButton(
			item.Title,
			item.Icon,
			i == d.selected,
			func() { d.Select(i) },      // onClick: select this tab
			func(from, to int) {          // onSwap: swap two tabs
				d.swapTabs(from, to)
			},
			func() int { return i },      // getIndex: current position
		)
		buttons[i] = btn
	}
	d.bar.Objects = buttons
	d.bar.Refresh()

	if len(d.items) > 0 && d.selected < len(d.items) {
		d.content.Objects = []fyne.CanvasObject{d.items[d.selected].Content}
	} else {
		d.content.Objects = nil
	}
	d.content.Refresh()
}

func (d *DraggableTabContainer) swapTabs(from, to int) {
	if from < 0 || to < 0 || from >= len(d.items) || to >= len(d.items) || from == to {
		return
	}
	d.items[from], d.items[to] = d.items[to], d.items[from]

	// Keep the selection following the dragged tab
	switch d.selected {
	case from:
		d.selected = to
	case to:
		d.selected = from
	}

	d.rebuild()

	if d.OnReordered != nil {
		d.OnReordered(d.items)
	}
}

// ── dragTabButton ─────────────────────────────────────────────────────────────

// dragTabButton is one tab header that implements fyne.Tappable and fyne.Draggable
type dragTabButton struct {
	widget.BaseWidget

	label    string
	icon     fyne.Resource
	active   bool
	getIndex func() int     // returns this button's current index in parent
	onClick  func()         // called on tap
	onSwap   func(int, int) // called with (thisIndex, neighbourIndex)
	dragAccX float32        // accumulated horizontal drag distance
}

// tabWidth is the visual width of each tab button (used for swap threshold)
const tabWidth float32 = 130

func newDragTabButton(
	label string,
	icon fyne.Resource,
	active bool,
	onClick func(),
	onSwap func(int, int),
	getIndex func() int,
) *dragTabButton {
	b := &dragTabButton{
		label:    label,
		icon:     icon,
		active:   active,
		getIndex: getIndex,
		onClick:  onClick,
		onSwap:   onSwap,
	}
	b.ExtendBaseWidget(b)
	return b
}

// Tapped selects this tab when clicked without dragging
func (b *dragTabButton) Tapped(_ *fyne.PointEvent) {
	if b.onClick != nil {
		b.onClick()
	}
}

// Dragged accumulates horizontal movement and triggers a swap at half-tab distance
func (b *dragTabButton) Dragged(ev *fyne.DragEvent) {
	b.dragAccX += ev.Dragged.DX

	if b.dragAccX > tabWidth/2 {
		b.dragAccX = 0
		cur := b.getIndex()
		if b.onSwap != nil {
			b.onSwap(cur, cur+1) // swap right
		}
	} else if b.dragAccX < -tabWidth/2 {
		b.dragAccX = 0
		cur := b.getIndex()
		if b.onSwap != nil {
			b.onSwap(cur, cur-1) // swap left
		}
	}
}

// DragEnd resets the drag accumulator
func (b *dragTabButton) DragEnd() { b.dragAccX = 0 }

// MinSize sets a consistent tab button size
func (b *dragTabButton) MinSize() fyne.Size { return fyne.NewSize(tabWidth, 36) }

func (b *dragTabButton) CreateRenderer() fyne.WidgetRenderer {
	lbl := widget.NewLabel(b.label)
	lbl.Truncation = fyne.TextTruncateEllipsis

	var ico *widget.Icon
	if b.icon != nil {
		ico = widget.NewIcon(b.icon)
	} else {
		ico = widget.NewIcon(theme.FileIcon())
	}

	// Active indicator bar at the bottom of the tab button
	indicator := canvas.NewRectangle(color.Transparent)
	indicator.SetMinSize(fyne.NewSize(tabWidth, 3))
	if b.active {
		indicator.FillColor = theme.PrimaryColor()
	}

	// Background: brighter for the active tab
	bg := canvas.NewRectangle(color.Transparent)
	if b.active {
		bg.FillColor = theme.Color(theme.ColorNameBackground)
	} else {
		bg.FillColor = theme.Color(theme.ColorNameInputBackground)
	}

	// "⠿" Braille character as a subtle drag-handle hint on the right
	dragHint := widget.NewLabel("⠿")
	dragHint.TextStyle = fyne.TextStyle{Monospace: true}

	row := container.NewHBox(ico, lbl)
	inner := container.NewBorder(nil, indicator, nil, dragHint, row)
	stacked := container.NewStack(bg, container.NewPadded(inner))

	return widget.NewSimpleRenderer(stacked)
}

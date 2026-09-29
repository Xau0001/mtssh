package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
)

// listRow is a widget.List item that selects on a single tap and runs an
// action on a double tap (widget.List itself only reports single taps).
type listRow struct {
	widget.BaseWidget
	content     fyne.CanvasObject
	id          widget.ListItemID // set by the list's update function
	onTap       func(widget.ListItemID)
	onDoubleTap func(widget.ListItemID)
}

func newListRow(content fyne.CanvasObject, onTap, onDoubleTap func(widget.ListItemID)) *listRow {
	r := &listRow{content: content, onTap: onTap, onDoubleTap: onDoubleTap}
	r.ExtendBaseWidget(r)
	return r
}

// newDoubleTapList returns a list whose rows are built by newContent and
// filled by update. A single tap selects a row, a double tap calls activate.
func newDoubleTapList(
	length func() int,
	newContent func() fyne.CanvasObject,
	update func(widget.ListItemID, fyne.CanvasObject),
	activate func(widget.ListItemID),
) *widget.List {
	var list *widget.List
	list = widget.NewList(
		length,
		func() fyne.CanvasObject {
			return newListRow(newContent(), func(id widget.ListItemID) { list.Select(id) }, activate)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*listRow)
			row.id = id
			update(id, row.content)
		},
	)
	return list
}

func (r *listRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(r.content)
}

// Tapped selects the row
func (r *listRow) Tapped(*fyne.PointEvent) {
	if r.onTap != nil {
		r.onTap(r.id)
	}
}

// DoubleTapped selects the row and runs the action
func (r *listRow) DoubleTapped(*fyne.PointEvent) {
	if r.onTap != nil {
		r.onTap(r.id)
	}
	if r.onDoubleTap != nil {
		r.onDoubleTap(r.id)
	}
}

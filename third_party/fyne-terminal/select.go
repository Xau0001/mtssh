package terminal

import (
	"strings"

	"fyne.io/fyne/v2"
	widget2 "github.com/fyne-io/terminal/internal/widget"
)

// getSelectedRange returns the current selection range, start row, start col, end row, end col
// It always returns a positive selection
func (t *Terminal) getSelectedRange() (int, int, int, int) {
	if t.selStart == nil || t.selEnd == nil {
		return 0, 0, 0, 0
	}

	startRow := t.selStart.Row
	startCol := t.selStart.Col
	endRow := t.selEnd.Row
	endCol := t.selEnd.Col

	if t.blockMode {
		if startCol > endCol {
			// Swap the start and end colums
			startCol, endCol = endCol, startCol
		}

		if startRow > endRow {
			// Swap the start and end rows
			startRow, endRow = endRow, startRow
		}

		return startRow - 1, startCol - 1, endRow - 1, endCol - 1
	}
	// Check if the user has selected in reverse
	if startRow > endRow || (startRow == endRow && startCol > endCol) {
		// Swap the start and end rows and columns
		startRow, endRow = endRow, startRow
		startCol, endCol = endCol, startCol
	}

	return startRow - 1, startCol - 1, endRow - 1, endCol - 1
}

func (t *Terminal) highlightSelectedText() {
	sr, sc, er, ec := t.getSelectedRange()
	widget2.HighlightRange(t.content, t.blockMode, sr, sc, er, ec, highlightBitMask)
	t.Refresh()
}

func (t *Terminal) clearSelectedText() {
	sr, sc, er, ec := t.getSelectedRange()
	widget2.ClearHighlightRange(t.content, t.blockMode, sr, sc, er, ec)
	t.Refresh()
	t.blockMode = false
	t.selecting = false
	t.selStart = nil
	t.selEnd = nil
}

// SelectedText gets the text that is currently selected.
func (t *Terminal) SelectedText() string {
	sr, sc, er, ec := t.getSelectedRange()
	return widget2.GetTextRange(t.content, t.blockMode, sr, sc, er, ec)
}

func (t *Terminal) copySelectedText(clipboard fyne.Clipboard) {
	// copy start and end sel to clipboard and clear the sel style
	text := t.SelectedText()
	clipboard.SetContent(text)
	t.clearSelectedText()
}

func (t *Terminal) pasteText(clipboard fyne.Clipboard) {
	content := clipboard.Content()

	if t.bracketedPasteMode {
		// MTSSH patch: the pasted text must not end the paste early
		// ("ESC[201~") or reach the remote tty as keys: a ^C there
		// (SIGINT) made the shell discard the paste and run the rest as
		// typed commands. Like xterm and VTE, keep only tab and line
		// breaks of the control characters.
		content = strings.Map(pasteChar, content)
		_, _ = t.Write(
			append(
				append(
					[]byte{asciiEscape, '[', '2', '0', '0', '~'},
					[]byte(content)...,
				),
				[]byte{asciiEscape, '[', '2', '0', '1', '~'}...,
			),
		)
		return
	}
	_, _ = t.Write([]byte(content))
}

// pasteChar keeps tab, LF and CR in a bracketed paste and drops the other
// C0 controls, DEL and the C1 controls (MTSSH patch). strings.Map turns
// bytes that are not UTF-8 into U+FFFD, so no raw C1 byte gets through.
func pasteChar(r rune) rune {
	switch {
	case r == '\t' || r == '\n' || r == '\r':
		return r
	case r < 0x20 || (r >= 0x7f && r <= 0x9f):
		return -1
	}
	return r
}

func (t *Terminal) hasSelectedText() bool {
	return t.selStart != nil && t.selEnd != nil
}

func (t *Terminal) selectClipboard() *selectClipboard {
	if t.selectClipSource == nil {
		t.selectClipSource = &selectClipboard{t: t}
	}

	return t.selectClipSource
}

type selectClipboard struct {
	t *Terminal

	content string
}

func (s *selectClipboard) Content() string {
	return s.content
}

func (s *selectClipboard) SetContent(c string) {
	s.content = c
}

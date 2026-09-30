package terminal

import (
	"fyne.io/fyne/v2"
)

func (t *Terminal) handleMouseDownV200(btn int, mods fyne.KeyModifier, pos fyne.Position) {
	_, _ = t.Write(t.encodeMouse(btn, mods, pos))
}

func (t *Terminal) handleMouseDownX10(btn int, _ fyne.KeyModifier, pos fyne.Position) {
	_, _ = t.Write(t.encodeMouse(btn, 0, pos))
}

func (t *Terminal) handleMouseUpV200(btn int, mods fyne.KeyModifier, pos fyne.Position) {
	_, _ = t.Write(t.encodeMouse(0, mods, pos))
}

func (t *Terminal) handleMouseUpX10(_ int, _ fyne.KeyModifier, _ fyne.Position) {
	// no-op for X10 mode
}

func (t *Terminal) encodeMouse(button int, mods fyne.KeyModifier, pos fyne.Position) []byte {
	p := t.getTermPosition(pos)
	var btn byte
	if button == 0 {
		btn = 3
	} else {
		btn = byte(button) - 1
	}

	if mods&fyne.KeyModifierShift != 0 {
		btn += 4
	}
	if mods&fyne.KeyModifierAlt != 0 {
		btn += 8
	}
	if mods&fyne.KeyModifierControl != 0 {
		btn += 16
	}

	cols, rows := int(t.config.Columns), int(t.config.Rows)
	return []byte{asciiEscape, '[', 'M', 32 + btn, mouseCoord(p.Col, cols), mouseCoord(p.Row, rows)}
}

// mouseLimit is the largest coordinate a mouse report can hold: each one
// is sent as a single byte, 32 + value.
const mouseLimit = 255 - 32

// mouseCoord encodes a 1-based mouse coordinate. MTSSH patch: values past
// mouseLimit wrapped into control characters (column 237 was sent as CR,
// which runs the shell's command line if mouse mode was left on), and a
// button released outside the widget gave values below 1. Like xterm, the
// position is limited to the screen and to what the encoding can hold;
// a report is still sent, so every press has its release.
func mouseCoord(v, size int) byte {
	if size > 0 && v > size {
		v = size
	}
	if v > mouseLimit {
		v = mouseLimit
	}
	if v < 1 {
		v = 1
	}
	return byte(32 + v)
}

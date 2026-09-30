package terminal

import (
	"bytes"
	"log"
	"time"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
	widget2 "github.com/fyne-io/terminal/internal/widget"
)

const (
	asciiBell      = 7
	asciiBackspace = 8
	asciiEscape    = 27

	tabWidth = 8
)

var charSetMap = map[charSet]func(rune) rune{
	charSetANSII: func(r rune) rune {
		return r
	},
	charSetDECSpecialGraphics: func(r rune) rune {
		m, ok := decSpecialGraphics[r]
		if ok {
			return m
		}
		return r
	},
	charSetAlternate: func(r rune) rune {
		return r
	},
}

var specialChars = map[rune]func(t *Terminal){
	asciiBell:      handleOutputBell,
	asciiBackspace: handleOutputBackspace,
	'\n':           handleOutputLineFeed,
	'\v':           handleOutputLineFeed,
	'\f':           handleOutputLineFeed,
	'\r':           handleOutputCarriageReturn,
	'\t':           handleOutputTab,
	// SO (0x0e) and SI (0x0f) are handled by handleOutput itself.
}

// decSpecialGraphics is for ESC(0 graphics mode
// https://en.wikipedia.org/wiki/DEC_Special_Graphics
var decSpecialGraphics = map[rune]rune{
	'`': '◆', // filled in diamond
	'a': '▒', // filled in box
	'b': '␉', // horizontal tab symbol
	'c': '␌', // form feed symbol
	'd': '␍', // carriage return symbol
	'e': '␊', // line feed symbol
	'f': '°', // degree symbol
	'g': '±', // plus-minus sign
	'h': '␤', // new line symbol
	'i': '␋', // vertical tab symbol
	'j': '┘', // bottom right
	'k': '┐', // top right
	'l': '┌', // top left
	'm': '└', // bottom left
	'n': '┼', // cross
	'o': '⎺', // scan line 1
	'p': '⎻', // scan line 2
	'q': '─', // scan line 3
	'r': '─', // scan line 4
	's': '⎽', // scan line 5
	't': '├', // vertical and right
	'u': '┤', // vertical and left
	'v': '┴', // horizontal and up
	'w': '┬', // horizontal and down
	'x': '│', // vertical bar
	'y': '≤', // less or equal
	'z': '≥', // greater or equal
	'{': 'π', // pi
	'|': '≠', // not equal
	'}': '£', // Pounds currency symbol
	'~': '·', // centered dot
}

type parseState struct {
	code string
	// esc is true inside a CSI sequence (after ESC [). MTSSH patch: it was
	// the position of the ESC in the read, with 5000 for none, so an ESC
	// at that position started no sequence and the rest showed as text.
	esc           bool
	escNext       bool // true when ESC was just seen; next char goes to parseEscState
	osc, apc, dcs bool
	vt100         rune
	printing      bool
}

func (t *Terminal) handleOutput(buf []byte) []byte {
	// MTSSH patch: the selection is UI state; clear it on the UI goroutine,
	// not on the read goroutine this runs on.
	safeDo(func() {
		if t.hasSelectedText() {
			t.clearSelectedText()
		}
	})
	if t.state == nil {
		t.state = &parseState{}
	}
	var (
		size int
		r    rune
	)
	for {
		buf = buf[size:]
		r, size = utf8.DecodeRune(buf)
		if size == 0 {
			break
		}
		if r == utf8.RuneError && size == 1 { // not UTF-8
			if !t.state.printing {
				// MTSSH patch: a character split across reads is kept for
				// the next read. Return a copy: run() reads into buf again
				// before it appends the next read to what we return.
				if !utf8.FullRune(buf) {
					return append([]byte(nil), buf...)
				}
				if t.debug {
					log.Println("Invalid UTF-8", buf[0])
				}
				continue
			}
		}

		if t.state.printing {
			t.parsePrinting(buf, size)
			continue
		}
		if r == asciiEscape {
			t.state.esc = true
			t.state.escNext = true
			continue
		}
		if t.state.dcs {
			t.parseDCS(r)
			continue
		}
		if t.state.escNext {
			t.state.escNext = false
			if cont := t.parseEscState(r); cont {
				continue
			}
			t.state.esc = false
			continue
		}
		if t.state.apc {
			t.parseAPC(r)
			continue
		}
		if t.state.osc {
			t.parseOSC(r)
			continue
		} else if t.state.vt100 != 0 {
			t.handleVT100(string([]rune{t.state.vt100, r}))
			t.state.vt100 = 0
			continue
		} else if t.state.esc {
			t.parseEscape(r)
			continue
		}

		// MTSSH patch: SO/SI switch the character set here, on the parser
		// goroutine that maps the characters that follow (like handleVT100
		// for G0/G1). A queued UI closure applied the switch too late.
		switch r {
		case 0x0e:
			handleShiftOut(t)
			continue
		case 0x0f:
			handleShiftIn(t)
			continue
		}

		if out, ok := specialChars[r]; ok {
			if out == nil {
				continue
			}
			safeDo(func() {
				out(t)
			})
		} else {
			// check to see which charset to use
			if t.useG1CharSet {
				chr := charSetMap[t.g1Charset](r)
				safeDo(func() {
					t.handleOutputChar(chr)
				})
			} else {
				chr := charSetMap[t.g0Charset](r)
				safeDo(func() {
					t.handleOutputChar(chr)
				})
			}
		}
	}

	return buf
}

func (t *Terminal) parseEscState(r rune) (shouldContinue bool) {
	switch r {
	case '[':
		return true
	case '\\':
		if t.state.osc {
			code := t.state.code
			safeDo(func() {
				t.handleOSC(code)
			})
		}
		t.state.code = ""
		t.state.osc = false
	case ']':
		t.state.osc = true
	case '(', ')':
		t.state.vt100 = r
	case '7':
		safeDo(func() {
			t.savedRow = t.cursorRow
			t.savedCol = t.cursorCol
		})
	case '8':
		safeDo(func() {
			t.cursorRow = t.savedRow
			t.cursorCol = t.savedCol
		})
	case 'D':
		safeDo(t.index)
	case 'M':
		safeDo(t.reverseIndex)
	case 'P':
		t.state.dcs = true
	case '_':
		t.state.apc = true
	case '=', '>':
	}
	return false
}

func (t *Terminal) parseEscape(r rune) {
	if len(t.state.code) >= maxCodeLen {
		t.resetParser()
		return
	}
	t.state.code += string(r)
	if (r < '0' || r > '9') && r != ';' && r != '=' && r != '?' && r != '>' {
		code := t.state.code
		safeDo(func() {
			t.handleEscape(code)
		})
		t.state.code = ""
		t.state.esc = false
	}
}

func (t *Terminal) parsePrinting(buf []byte, size int) {
	if len(t.printData) >= maxPrintData {
		t.resetParser()
		return
	}
	t.printData = append(t.printData, buf[:size]...)
	if bytes.HasSuffix(t.printData, []byte{asciiEscape, '[', '4', 'i'}) {
		// Handle the end of printing
		t.printData = t.printData[:len(t.printData)-4]
		escapePrinterMode(t, "4")
		t.state.esc = false
	}
}

func (t *Terminal) parseAPC(r rune) {
	if r == 0 {
		code := t.state.code
		safeDo(func() {
			t.handleAPC(code)
		})
		t.state.code = ""
		t.state.apc = false
	} else if len(t.state.code) >= maxCodeLen {
		t.resetParser()
	} else {
		t.state.code += string(r)
	}
}

func (t *Terminal) parseOSC(r rune) {
	if r == asciiBell || r == 0 {
		code := t.state.code
		safeDo(func() {
			t.handleOSC(code)
		})
		t.state.code = ""
		t.state.osc = false
	} else if len(t.state.code) >= maxCodeLen {
		t.resetParser()
	} else {
		t.state.code += string(r)
	}
}

func (t *Terminal) parseDCS(r rune) {
	if r == '\\' {
		code := t.state.code
		safeDo(func() {
			t.handleDCS(code)
		})
		t.state.code = ""
		t.state.dcs = false
	} else if len(t.state.code) >= maxCodeLen {
		t.resetParser()
	} else {
		t.state.code += string(r)
	}
}

func (t *Terminal) handleOutputChar(r rune) {
	if t.cursorCol >= int(t.config.Columns) { // may be beyond the edge if restored from a wider terminal
		if !t.disableAutoWrap {
			t.cursorCol = 0
			handleOutputLineFeed(t)
		} else {
			// In non-wrap mode, overwrite the last character
			t.cursorCol = int(t.config.Columns) - 1
		}
	}

	var cellStyle widget.TextGridStyle
	textStyle := fyne.TextStyle{
		Monospace: true,

		Bold:          t.bold,
		Italic:        t.italic,
		Underline:     t.underline,
		Strikethrough: t.strikethrough,
	}
	cellStyle = &widget.CustomTextGridStyle{
		FGColor: t.currentFG, BGColor: t.currentBG,
		TextStyle: textStyle,
	}
	if t.blinking {
		cellStyle = widget2.NewTermTextGridStyle(t.currentFG, t.currentBG, highlightBitMask, t.blinking, textStyle)
	}

	row, col := t.cursorRow, t.cursorCol
	cell := widget.TextGridCell{Rune: r, Style: cellStyle}
	oldLen := 0
	if len(t.content.Rows) > row {
		oldLen = len(t.content.Rows[row].Cells)
	}
	t.content.SetCell(row, col, cell)

	for i := oldLen; i < col; i++ {
		if t.content.Rows[row].Cells[i].Rune == 0 {
			t.content.Rows[row].Cells[i].Rune = ' '
		}
	}
	t.lastChar = r
	t.cursorCol++
}

func (t *Terminal) ringBell() {
	// MTSSH patch: a bell while one is shown is ignored, so a flood of BEL
	// characters costs no redraws and starts no goroutines.
	if t.bell {
		return
	}
	t.bell = true
	t.Refresh()

	go func() {
		time.Sleep(time.Millisecond * 300)
		safeDo(func() { // MTSSH patch: t.bell is UI state, set it on the UI goroutine
			t.bell = false
			t.Refresh()
		})
	}()
}

// scrollUp and scrollDown only change the grid. MTSSH patch: they no
// longer refresh it. Every line scrolled redrew the whole screen, so a
// flood of newlines kept the UI goroutine (all tabs and windows) busy for
// many seconds per read; they only run while output is parsed, and run()
// refreshes after every read.
func (t *Terminal) scrollUp() {
	for i := t.scrollBottom; i > t.scrollTop; i-- {
		t.content.SetRow(i, t.content.Row(i-1))
	}
	t.content.SetRow(t.scrollTop, widget.TextGridRow{})
}

// MTSSH patch: scrollDown works like scrollUp. When fewer rows than the
// scroll region had been written, it left the last of them on screen.
func (t *Terminal) scrollDown() {
	for i := t.scrollTop; i < t.scrollBottom; i++ {
		t.content.SetRow(i, t.content.Row(i+1))
	}
	t.content.SetRow(t.scrollBottom, widget.TextGridRow{})
}

// index (IND, ESC D) and reverseIndex (RI, ESC M) move the cursor one line
// down or up and scroll only at the bottom or top margin of the scroll
// region, like a line feed and a reverse line feed. MTSSH patch: they
// scrolled the region wherever the cursor was.
func (t *Terminal) index() {
	if t.cursorRow == t.scrollBottom {
		t.scrollDown()
		return
	}
	t.moveCursor(t.cursorRow+1, t.cursorCol)
}

func (t *Terminal) reverseIndex() {
	if t.cursorRow == t.scrollTop {
		t.scrollUp()
		return
	}
	t.moveCursor(t.cursorRow-1, t.cursorCol)
}

func handleOutputBackspace(t *Terminal) {
	row := t.content.Row(t.cursorRow)
	if len(row.Cells) == 0 {
		return
	}
	t.moveCursor(t.cursorRow, t.cursorCol-1)
}

func handleOutputBell(t *Terminal) {
	t.ringBell()
}

func handleOutputCarriageReturn(t *Terminal) {
	t.moveCursor(t.cursorRow, 0)
}

func handleOutputLineFeed(t *Terminal) {
	if t.cursorRow == t.scrollBottom {
		t.scrollDown()
		if t.newLineMode {
			t.moveCursor(t.cursorRow, 0)
		}
		return
	}
	if t.newLineMode {
		t.moveCursor(t.cursorRow+1, 0)
		return
	}
	t.moveCursor(t.cursorRow+1, t.cursorCol)
}

// handleOutputTab moves the cursor to the next tab stop, or to the last
// column if there is none, and writes nothing (like xterm). MTSSH patch: it
// wrote spaces until the cursor reached the tab stop. A stop past the
// right edge was never reached (the spaces wrapped, or with autowrap off
// stayed in the last column), an endless loop on the UI goroutine. A row
// that ends before the new cursor position is padded when a character is
// written there, as after other cursor moves.
func handleOutputTab(t *Terminal) {
	cols := int(t.config.Columns)
	if cols <= 0 {
		return
	}
	next := t.cursorCol - t.cursorCol%tabWidth + tabWidth
	if next > cols-1 {
		next = cols - 1
	}
	t.moveCursor(t.cursorRow, next)
}

// handleShiftOut and handleShiftIn run on the parser goroutine, which alone
// uses useG1CharSet, g0Charset and g1Charset.
func handleShiftOut(t *Terminal) {
	t.useG1CharSet = true
}

func handleShiftIn(t *Terminal) {
	t.useG1CharSet = false
}

// SetPrinterFunc sets the printer function which is executed when printing.
func (t *Terminal) SetPrinterFunc(printerFunc PrinterFunc) {
	t.printer = printerFunc
}

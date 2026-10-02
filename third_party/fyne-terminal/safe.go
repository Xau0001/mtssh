package terminal

import (
	"fmt"

	"fyne.io/fyne/v2"
)

// MTSSH patch: everything the parser hands to the UI goroutine acts on
// bytes chosen by the remote side. Fyne runs these functions on its main
// loop without recovering, so a single malformed escape sequence used to
// take down the whole application. The parser limits below keep a server
// from making it buffer without bound.

const (
	// maxCodeLen bounds an escape, OSC, APC or DCS sequence the parser
	// collects. Longer sequences are dropped.
	maxCodeLen = 4096
	// maxPrintData bounds data buffered in printer mode (ESC[5i … ESC[4i).
	maxPrintData = 1 << 20
)

// safeDo is fyne.Do for handlers of remote data: a panic is logged and the
// sequence ignored instead of crashing the application.
func safeDo(fn func()) {
	fyne.Do(func() {
		defer recoverHandler()
		fn()
	})
}

// safeDoAndWait is fyne.DoAndWait with the same protection as safeDo.
func safeDoAndWait(fn func()) {
	fyne.DoAndWait(func() {
		defer recoverHandler()
		fn()
	})
}

func recoverHandler() {
	if r := recover(); r != nil {
		fyne.LogError("terminal: ignored malformed output", fmt.Errorf("%v", r))
	}
}

// resetParser drops a partly parsed sequence.
func (t *Terminal) resetParser() {
	*t.state = parseState{}
	t.printData = nil
}

// handleOutputSafely is handleOutput for the read loop: a panic while
// parsing drops the rest of buf, including a character held back for the
// next read, instead of ending the program.
func (t *Terminal) handleOutputSafely(buf []byte) (leftOver []byte) {
	defer func() {
		if r := recover(); r != nil {
			fyne.LogError("terminal: ignored malformed output", fmt.Errorf("%v", r))
			if t.state != nil {
				t.resetParser()
			}
			leftOver = nil
		}
	}()
	return t.handleOutput(buf)
}

package ui

import (
	"fmt"
	"image/color"
	"io"
	"log"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/test"
	"github.com/fyne-io/terminal"
)

// termWidget runs the (patched) terminal widget wired up like in a TermTab:
// output passes outputFilter on its way to the widget. Every chunk reaches
// the widget in a Read of its own, and output returns once the widget has
// processed it and asks for more. The test driver runs fyne.Do at once, on
// the reading goroutine, so the grid is then complete and not in use.
type termWidget struct {
	t          *testing.T
	term       *terminal.Terminal
	win        fyne.Window
	filter     outputFilter
	chunks     chan []byte
	ready      chan struct{}
	done       chan error
	input      termRecorder
	rows, cols int
}

func termStartWidget(t *testing.T) *termWidget {
	t.Helper()
	test.NewApp()
	w := &termWidget{
		t:      t,
		term:   terminal.New(),
		chunks: make(chan []byte),
		ready:  make(chan struct{}),
		done:   make(chan error, 1),
	}
	sizes := make(chan terminal.Config, 16)
	w.term.AddListener(sizes)
	w.win = test.NewWindow(w.term)
	w.win.Resize(fyne.NewSize(400, 300))
drain:
	for {
		select {
		case cfg := <-sizes:
			w.rows, w.cols = int(cfg.Rows), int(cfg.Columns)
		default:
			break drain
		}
	}
	w.term.RemoveListener(sizes)
	if w.rows < 8 || w.cols < 10 {
		t.Fatalf("terminal is %dx%d, too small for the test", w.cols, w.rows)
	}

	go func() { w.done <- w.term.RunWithConnection(&w.input, w) }()
	w.wait() // first Read: the widget is running
	t.Cleanup(w.close)
	return w
}

// Read hands the widget the next chunk. Coming back for it means the
// previous one is processed.
func (w *termWidget) Read(p []byte) (int, error) {
	w.ready <- struct{}{}
	c, ok := <-w.chunks
	if !ok {
		return 0, io.EOF
	}
	return copy(p, c), nil // chunks are far below the widget's 32 KiB
}

func (w *termWidget) wait() {
	w.t.Helper()
	select {
	case <-w.ready:
	case <-time.After(10 * time.Second):
		w.t.Fatal("terminal did not process the output")
	}
}

// output feeds each chunk through the filter to the widget, in separate
// reads, and waits until the widget has processed it.
func (w *termWidget) output(chunks ...string) {
	w.t.Helper()
	for _, c := range chunks {
		out := w.filter.write(nil, []byte(c))
		if len(out) == 0 {
			continue // held back by the filter, or dropped
		}
		select {
		case w.chunks <- out:
		case <-time.After(10 * time.Second):
			w.t.Fatal("terminal stopped reading")
		}
		w.wait()
	}
}

// lines returns the first n screen lines without trailing blanks.
func (w *termWidget) lines(n int) []string {
	rows := strings.Split(w.term.Text(), "\n")
	out := make([]string, n)
	for i := range out {
		if i < len(rows) {
			out[i] = strings.TrimRight(rows[i], " \x00")
		}
	}
	return out
}

func (w *termWidget) wantLines(what string, want ...string) {
	w.t.Helper()
	if got := w.lines(len(want)); fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
		w.t.Errorf("%s: screen = %q, want %q", what, got, want)
	}
}

func (w *termWidget) close() {
	close(w.chunks)
	select {
	case <-w.done:
	case <-time.After(10 * time.Second):
		w.t.Error("terminal did not stop")
	}
	w.win.Close()
}

// termRecorder records what the widget sends to the server.
type termRecorder struct {
	mu  sync.Mutex
	buf []byte
}

func (r *termRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	return len(p), nil
}

func (r *termRecorder) Close() error { return nil }

func (r *termRecorder) take() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := string(r.buf)
	r.buf = nil
	return s
}

const termSixLines = "L0\r\nL1\r\nL2\r\nL3\r\nL4\r\nL5"

// A character split across reads is completed by the next read (M-08).
func TestTermSplitUTF8(t *testing.T) {
	w := termStartWidget(t)
	w.output("a\xc3", "\xa9b\xe2\x94", "\x80c")
	w.wantLines("split characters", "aéb─c")

	// Invalid bytes are still skipped.
	w.output("\r\n\xffx\xc3(y")
	w.wantLines("invalid bytes", "aéb─c", "x(y")
}

// SO selects G1 for the very next characters of the same read (M-07).
func TestTermShiftOut(t *testing.T) {
	w := termStartWidget(t)
	w.output("\x1b)0\x0eqqq\x0fqq")
	w.wantLines("SO/SI", "───qq")
}

// IL inserts blank lines at the cursor, within the scroll region (M-25).
func TestTermInsertLines(t *testing.T) {
	tests := []struct {
		name, seq string
		want      []string
	}{
		{"two lines", "\x1b[3;1H\x1b[2L", []string{"L0", "L1", "", "", "L2", "L3", "L4", "L5"}},
		{"in a region", "\x1b[2;4r\x1b[2;1H\x1b[L", []string{"L0", "", "L1", "L2", "L4", "L5"}},
		{"below the region", "\x1b[2;4r\x1b[6;1H\x1b[L", []string{"L0", "L1", "L2", "L3", "L4", "L5"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := termStartWidget(t)
			w.output(termSixLines, tt.seq)
			w.wantLines(tt.name, tt.want...)
		})
	}
}

// DECSTBM clamps the bottom margin and ignores regions of less than two
// rows (M-26). A line feed at the bottom margin shows the region.
func TestTermScrollRegion(t *testing.T) {
	t.Run("bottom clamped", func(t *testing.T) {
		w := termStartWidget(t)
		w.output(termSixLines, fmt.Sprintf("\x1b[3;9999r\x1b[%d;1H\n", w.rows))
		w.wantLines("bottom clamped", "L0", "L1", "L3", "L4", "L5")
	})
	tests := []struct {
		name string
		seqs []string // sent after setting the region to rows 2-4
		want []string
	}{
		{"invalid regions ignored", []string{"\x1b[5;5r", "\x1b[3;2r"}, []string{"L0", "L2", "L3", "", "L4", "L5"}},
		{"private form ignored", []string{"\x1b[?1;2r"}, []string{"L0", "L2", "L3", "", "L4", "L5"}},
		{"reset to the full screen", []string{"\x1b[r"}, []string{"L0", "L1", "L2", "L3", "L4", "L5"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := termStartWidget(t)
			w.output(termSixLines, "\x1b[2;4r")
			w.output(tt.seqs...)
			w.output("\x1b[4;1H\n")
			w.wantLines(tt.name, tt.want...)
		})
	}
}

// REP repeats a character at most one line's worth (M-06).
func TestTermRepeatCapped(t *testing.T) {
	w := termStartWidget(t)
	w.output("A\x1b[99999b")
	w.wantLines("REP", strings.Repeat("A", w.cols), "A", "")
}

// A bracketed paste cannot end itself: it contains no ESC (M-28). Nor can
// it signal the remote tty (a ^C made the shell drop the paste and run the
// rest as typed): of the control characters only tab, LF and CR are left.
func TestTermBracketedPaste(t *testing.T) {
	w := termStartWidget(t)
	var paste fyne.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyV, Modifier: fyne.KeyModifierShift | fyne.KeyModifierShortcutDefault}
	if runtime.GOOS == "darwin" {
		paste = &fyne.ShortcutPaste{}
	}
	tests := []struct{ clip, bracketed string }{
		{"ls\x1b[201~echo pasted\r", "ls[201~echo pasted\r"},
		{"\x03 touch X\r", " touch X\r"},
		{"a\tb\nc\r\n", "a\tb\nc\r\n"},
		// C0 controls, DEL and C1 controls
		{"\x00\x04\x08\x1a\x1c\x7fx\u0085\u009b\u00a0ü", "x\u00a0ü"},
		// a raw C1 byte is not UTF-8
		{"a\x9bb", "a\ufffdb"},
	}
	for _, tt := range tests {
		fyne.CurrentApp().Clipboard().SetContent(tt.clip)

		w.output("\x1b[?2004h")
		w.term.TypedShortcut(paste)
		if got, want := w.input.take(), "\x1b[200~"+tt.bracketed+"\x1b[201~"; got != want {
			t.Errorf("bracketed paste of %q sent %q, want %q", tt.clip, got, want)
		}

		w.output("\x1b[?2004l")
		w.term.TypedShortcut(paste)
		if got := w.input.take(); got != tt.clip {
			t.Errorf("plain paste of %q sent %q", tt.clip, got)
		}
	}
}

// TAB moves the cursor to the next tab stop or the last column, writes
// nothing and never wraps. A tab stop past the right edge used to hang the
// UI goroutine: the spaces written to reach it wrapped, or with autowrap
// off stayed in the last column. output fails after a timeout instead.
func TestTermTab(t *testing.T) {
	w := termStartWidget(t)
	blank := strings.Repeat(" ", w.cols-1)
	// Tab-separated fields wider than the screen: the ones past the last
	// tab stop overwrite the last column.
	tsv := []byte(blank + " ")
	for c := 0; c < w.cols; c += 8 {
		tsv[c] = '1'
	}
	tsv[w.cols-1] = '1'

	tests := []struct {
		name, out string
		want      []string
	}{
		{"tab stops", "a\tb\tc", []string{"a       b       c"}},
		{"no overwrite", "abcdefghij\r\tX", []string{"abcdefghXj"}},
		{"past the edge", "\x1b[999G\t\tX", []string{blank + "X", ""}},
		{"full line", strings.Repeat("x", w.cols) + "\tY", []string{strings.Repeat("x", w.cols-1) + "Y", ""}},
		{"autowrap off", "\x1b[?7l\x1b[999G\tX\tY\x1b[?7h", []string{blank + "Y", ""}},
		{"wide TSV", strings.Repeat("1\t", w.cols) + "\r\nend", []string{string(tsv), "end"}},
	}
	for _, tt := range tests {
		w.output("\x1b[H\x1b[2J", tt.out)
		w.wantLines(tt.name, tt.want...)
	}
}

// Scrolling does not redraw the screen for every line; only the refresh
// after each read does. A read full of line feeds on a large screen kept
// the UI goroutine busy for more than 30 s.
func TestTermScrollFlood(t *testing.T) {
	w := termStartWidget(t)
	w.win.Resize(fyne.NewSize(1920, 1080))
	w.output(strings.Repeat(strings.Repeat("y", 300)+"\r\n", 80))

	start := time.Now()
	w.output("top\r\n"+strings.Repeat("\n", 8192), strings.Repeat("\x1bD", 4096),
		strings.Repeat("\x1bM", 4096), "\x1b[999;1Hbottom")
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("16384 LF, IND and RI took %v", took)
	}
	if text := w.term.Text(); strings.Contains(text, "top") || strings.Contains(text, "y") || !strings.Contains(text, "bottom") {
		t.Errorf("screen after scrolling = %q", text)
	}
}

// A line feed at the bottom scrolls the screen also when the rows below
// the text were never written: the last written row stayed on screen.
func TestTermScrollUnwrittenRows(t *testing.T) {
	w := termStartWidget(t)
	w.output("top" + strings.Repeat("\n", w.rows-1) + "\n") // to the bottom row, then scroll
	if text := w.term.Text(); strings.Contains(text, "top") {
		t.Errorf("after scrolling one line of %d: screen = %q", w.rows, text)
	}
}

// An ESC at any position of a read starts a sequence. At byte 5001 the
// parser took it for "no sequence" and showed the rest as text.
func TestTermEscapeAnywhere(t *testing.T) {
	w := termStartWidget(t)
	for _, pos := range []int{0, 1, 4999, 5000, 5001, 5002} {
		w.output("\x1b[H\x1b[2J", strings.Repeat("x", pos)+"\x1b[31mred")
		text := strings.Join(strings.Fields(strings.ReplaceAll(w.term.Text(), "x", "")), "")
		if text != "red" {
			t.Errorf("ESC at %d: screen shows %q besides the x", pos, text)
		}
	}
}

// Keys typed and resizes while RunWithConnection starts, on a goroutine of
// its own as in NewTermTab, are no data race (run with -race): it sets the
// widget's input and waits for its size.
func TestTermStartRace(t *testing.T) {
	test.NewApp()
	term := terminal.New()
	var in termRecorder
	out := newTermBuffer()
	done := make(chan error, 1)
	go func() { done <- term.RunWithConnection(&in, out) }()
	w := test.NewWindow(term)
	defer w.Close()

	deadline := time.Now().Add(10 * time.Second)
	for width := float32(300); ; width++ {
		w.Resize(fyne.NewSize(width, 300))
		term.TypedRune('a') // dropped until the connection is set
		if in.take() != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("typed keys never reached the connection")
		}
		time.Sleep(time.Millisecond)
	}
	out.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal did not stop")
	}
}

// termBlinkGoroutines counts the goroutines that make text blink.
func termBlinkGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "(*TermGrid).runBlink") {
			count++
		}
	}
	return count
}

// Closing the terminal ends the goroutine for blinking text, for good. It
// ran on after the tab was closed, redrawing the grid twice a second.
func TestTermBlinkStops(t *testing.T) {
	w := termStartWidget(t)
	before := termBlinkGoroutines()
	wait := func(what string, want int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); termBlinkGoroutines() != want; {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d blink goroutines, want %d", what, termBlinkGoroutines(), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	w.output("\x1b[5mblink\x1b[0m")
	wait("blinking text", before+1)
	w.term.Close()
	wait("after Close", before)

	// Output still read after Close does not start it again.
	w.output("\x1b[5mmore\x1b[0m")
	time.Sleep(50 * time.Millisecond)
	if n := termBlinkGoroutines(); n != before {
		t.Fatalf("blinking output after Close: %d blink goroutines, want %d", n, before)
	}
}

// resetScreen turns off the modes a session can leave on, so the next
// session gets no mouse reports or bracketed pastes it did not ask for.
func TestTermResetScreen(t *testing.T) {
	w := termStartWidget(t)
	click := func() {
		w.term.MouseDown(&desktop.MouseEvent{Button: desktop.MouseButtonPrimary})
		w.term.MouseUp(&desktop.MouseEvent{Button: desktop.MouseButtonPrimary})
	}
	var paste fyne.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyV, Modifier: fyne.KeyModifierShift | fyne.KeyModifierShortcutDefault}
	if runtime.GOOS == "darwin" {
		paste = &fyne.ShortcutPaste{}
	}
	fyne.CurrentApp().Clipboard().SetContent("pasted")

	// Mouse reports, bracketed paste, autowrap off, newline mode, a scroll
	// region and the DEC graphics set in G1, selected with SO.
	w.output("\x1b[?1000h\x1b[?2004h\x1b[?7l\x1b[?20h\x1b[2;4r\x1b)0\x0e")
	click()
	if got := w.input.take(); !strings.HasPrefix(got, "\x1b[M") {
		t.Fatalf("mouse mode not on: a click sent %q", got)
	}

	w.output(resetScreen)
	click()
	w.term.TypedShortcut(paste)
	if got := w.input.take(); got != "pasted" {
		t.Errorf("after the reset a click and a paste sent %q, want %q", got, "pasted")
	}
	w.output("\x1b[H\x1b[2J", "q"+strings.Repeat("x", w.cols)+"\ny")
	w.wantLines("autowrap, newline mode, charset", "q"+strings.Repeat("x", w.cols-1), "x", " y")
	w.output("\x1b[H\x1b[2J", termSixLines, "\x1b[4;1H\n")
	w.wantLines("scroll region", "L0", "L1", "L2", "L3", "L4", "L5")
}

// termCellSize is the size of a terminal cell, computed like the widget does.
func termCellSize() fyne.Size {
	cell := canvas.NewText("M", color.White)
	cell.TextStyle.Monospace = true
	min := cell.MinSize()
	return fyne.NewSize(float32(math.Round(float64(min.Width))), float32(math.Round(float64(min.Height))))
}

// Mouse reports hold each coordinate in one byte, 32 + value. Positions
// past 223 or outside the widget are limited instead of wrapping around:
// column 237 was sent as CR.
func TestTermMouseReportLimits(t *testing.T) {
	w := termStartWidget(t)
	cell := termCellSize()
	w.win.Resize(fyne.NewSize(260*cell.Width+50, 300)) // over 250 columns
	w.output("\x1b[?1000h")

	at := func(col, row int) fyne.Position {
		return fyne.NewPos((float32(col)-0.5)*cell.Width, (float32(row)-0.5)*cell.Height)
	}
	report := func(button, col, row byte) string {
		return string([]byte{0x1b, '[', 'M', 32 + button, 32 + col, 32 + row})
	}
	tests := []struct {
		name string
		down bool
		pos  fyne.Position
		want string
	}{
		{"inside", true, at(10, 3), report(0, 10, 3)},
		{"column 223", true, at(223, 2), report(0, 223, 2)},
		{"column 237", true, at(237, 2), report(0, 223, 2)},
		{"past the screen", false, fyne.NewPos(10000, 10000), report(3, 223, uint8(w.rows))},
		{"before the screen", false, fyne.NewPos(-100, -100), report(3, 1, 1)},
	}
	for _, tt := range tests {
		ev := &desktop.MouseEvent{PointEvent: fyne.PointEvent{Position: tt.pos}, Button: desktop.MouseButtonPrimary}
		if tt.down {
			w.term.MouseDown(ev)
		} else {
			w.term.MouseUp(ev)
		}
		if got := w.input.take(); got != tt.want {
			t.Errorf("%s: sent %q, want %q", tt.name, got, tt.want)
		}
	}
}

// With Shift, special keys send what xterm sends.
func TestTermShiftedKeys(t *testing.T) {
	w := termStartWidget(t)
	shift := &fyne.KeyEvent{Name: desktop.KeyShiftLeft}
	w.term.KeyDown(shift)
	tests := []struct {
		key  fyne.KeyName
		want string
	}{
		{fyne.KeyTab, "\x1b[Z"},
		{fyne.KeyReturn, "\r"},
		{fyne.KeyEnter, "\n"},
		{fyne.KeyBackspace, "\b"},
		{fyne.KeyEscape, "\x1b"},
		{fyne.KeyUp, "\x1b[1;2A"},
		{fyne.KeyDown, "\x1b[1;2B"},
		{fyne.KeyRight, "\x1b[1;2C"},
		{fyne.KeyLeft, "\x1b[1;2D"},
		{fyne.KeyF1, "\x1b[1;2P"},
		{fyne.KeyF2, "\x1b[1;2Q"},
		{fyne.KeyF3, "\x1b[1;2R"},
		{fyne.KeyF4, "\x1b[1;2S"},
		{fyne.KeyF5, "\x1b[15;2~"},
		{fyne.KeyF12, "\x1b[24;2~"},
		{fyne.KeyHome, "\x1b[1;2H"},
		{fyne.KeyPageUp, "\x1b[5;2~"},
	}
	for _, tt := range tests {
		w.term.TypedKey(&fyne.KeyEvent{Name: tt.key})
		if got := w.input.take(); got != tt.want {
			t.Errorf("Shift+%s sent %q, want %q", tt.key, got, tt.want)
		}
	}

	w.term.KeyUp(shift)
	w.term.TypedKey(&fyne.KeyEvent{Name: fyne.KeyTab})
	w.term.TypedKey(&fyne.KeyEvent{Name: fyne.KeyUp})
	if got, want := w.input.take(), "\t\x1b[A"; got != want {
		t.Errorf("Tab and Up without Shift sent %q, want %q", got, want)
	}
}

// IND (ESC D) and RI (ESC M) move the cursor a line down or up and scroll
// only at the bottom or top margin of the scroll region; they scrolled
// wherever the cursor was.
func TestTermIndex(t *testing.T) {
	tests := []struct {
		name, seq string
		want      []string
	}{
		{"IND", "\x1b[2;1H\x1bDX", []string{"L0", "L1", "X2", "L3", "L4", "L5"}},
		{"RI", "\x1b[3;1H\x1bMX", []string{"L0", "X1", "L2", "L3", "L4", "L5"}},
		{"IND at the bottom margin", "\x1b[2;4r\x1b[4;1H\x1bDX", []string{"L0", "L2", "L3", "X", "L4", "L5"}},
		{"RI at the top margin", "\x1b[2;4r\x1b[2;1H\x1bMX", []string{"L0", "X", "L1", "L2", "L4", "L5"}},
		{"RI at the top", "\x1b[1;1H\x1bMX", []string{"X", "L0", "L1", "L2", "L3", "L4", "L5"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := termStartWidget(t)
			w.output(termSixLines, tt.seq)
			w.wantLines(tt.name, tt.want...)
		})
	}
}

// Unsupported SGR parameters are logged only in debug mode: a server could
// turn each byte of output into more than 100 bytes on stderr.
func TestTermNoLogSpam(t *testing.T) {
	var logs termRecorder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	w := termStartWidget(t)
	logs.take()
	w.output("\x1b[=m\x1b[1;=;=m")
	if got := logs.take(); got != "" {
		t.Errorf("output was logged: %q", got)
	}
}

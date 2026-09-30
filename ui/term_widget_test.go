package ui

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
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

// A bracketed paste cannot contain ESC, so it cannot end itself (M-28).
func TestTermBracketedPaste(t *testing.T) {
	w := termStartWidget(t)
	var paste fyne.Shortcut = &desktop.CustomShortcut{KeyName: fyne.KeyV, Modifier: fyne.KeyModifierShift | fyne.KeyModifierShortcutDefault}
	if runtime.GOOS == "darwin" {
		paste = &fyne.ShortcutPaste{}
	}
	fyne.CurrentApp().Clipboard().SetContent("ls\x1b[201~echo pasted\r")

	w.output("\x1b[?2004h")
	w.term.TypedShortcut(paste)
	if got, want := w.input.take(), "\x1b[200~ls[201~echo pasted\r\x1b[201~"; got != want {
		t.Errorf("bracketed paste sent %q, want %q", got, want)
	}

	w.output("\x1b[?2004l")
	w.term.TypedShortcut(paste)
	if got, want := w.input.take(), "ls\x1b[201~echo pasted\r"; got != want {
		t.Errorf("plain paste sent %q, want %q", got, want)
	}
}

package ui

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"github.com/fyne-io/terminal"
)

func TestTermBuffer(t *testing.T) {
	b := newTermBuffer()

	// Writes must not block while nobody reads (terminal not laid out yet).
	done := make(chan struct{})
	go func() {
		b.Write([]byte("hello "))
		b.Write([]byte("world"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("write blocked without a reader")
	}

	buf := make([]byte, 64)
	n, err := b.Read(buf)
	if err != nil || string(buf[:n]) != "hello world" {
		t.Fatalf("Read = %q, %v", buf[:n], err)
	}

	// A full buffer blocks the writer until the reader catches up.
	b.Write([]byte(strings.Repeat("x", maxPending)))
	unblocked := make(chan struct{})
	go func() {
		b.Write([]byte("x"))
		close(unblocked)
	}()
	select {
	case <-unblocked:
		t.Fatal("write did not block on a full buffer")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := io.ReadFull(b, make([]byte, maxPending)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-unblocked:
	case <-time.After(time.Second):
		t.Fatal("writer not released after read")
	}

	// Close: queued data is still delivered, then EOF; writes fail.
	b.Close()
	if n, err := b.Read(buf); err != nil || string(buf[:n]) != "x" {
		t.Fatalf("Read after Close = %q, %v", buf[:n], err)
	}
	if _, err := b.Read(buf); err != io.EOF {
		t.Fatalf("Read on drained closed buffer: err = %v, want EOF", err)
	}
	if _, err := b.Write([]byte("y")); err == nil {
		t.Fatal("Write after Close should fail")
	}
}

func TestTermBufferCloseReleasesWriter(t *testing.T) {
	b := newTermBuffer()
	b.Write([]byte(strings.Repeat("x", maxPending)))
	released := make(chan error, 1)
	go func() {
		_, err := b.Write([]byte("x"))
		released <- err
	}()
	time.Sleep(20 * time.Millisecond)
	b.Close()
	select {
	case err := <-released:
		if err == nil {
			t.Fatal("blocked write should fail after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release the blocked writer")
	}
}

func TestLocalFileName(t *testing.T) {
	tests := map[string]string{
		"report.pdf":           "report.pdf",
		"../../.bashrc":        ".._.._.bashrc",
		`..\..\evil.exe`:       ".._.._evil.exe",
		"/etc/passwd":          "_etc_passwd",
		"file.txt:stream":      "file.txt_stream",
		"a\x1b[2Jb":            "a_[2Jb",
		"..":                   "download",
		".":                    "download",
		"":                     "download",
		"Ümlaut ✓.txt":         "Ümlaut ✓.txt",
		"invoice\u202etxt.exe": "invoice_txt.exe",
		"zero\u200bwidth":      "zero_width",
		"c1\u009bcontrol":      "c1_control",
		"nul":                  "_nul",
		"COM1.txt":             "_COM1.txt",
		"trailing. ":           "trailing",
		"...":                  "download",
	}
	for in, want := range tests {
		if got := localFileName(in); got != want {
			t.Errorf("localFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPromptSecretEndsOnDisconnect(t *testing.T) {
	test.NewApp()
	w := test.NewWindow(nil)
	defer w.Close()
	tt := &TermTab{win: w}

	done := make(chan struct{})
	answer := make(chan string, 1)
	go func() { answer <- tt.promptSecret(done, "Title", "", "question") }()

	time.Sleep(50 * time.Millisecond) // dialog shown, nobody answers
	close(done)                       // e.g. tab closed / Reconnect
	select {
	case got := <-answer:
		if got != "" {
			t.Fatalf("answer = %q, want empty after disconnect", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not return after the session was disconnected")
	}
	if overlay := w.Canvas().Overlays().Top(); overlay != nil {
		t.Fatal("dialog still open after disconnect")
	}
}

func TestOutputFilter(t *testing.T) {
	tests := []struct {
		chunks []string
		want   string
	}{
		{[]string{"plain text\r\n"}, "plain text\r\n"},
		{[]string{"umlaut äöü ✓"}, "umlaut äöü ✓"},
		// media copy is dropped, whatever the parameter length
		{[]string{"\x1b[5ievil payload\x1b[4iafter"}, "evil payloadafter"},
		{[]string{"\x1b[?5i", "x", "\x1b[?4i"}, "x"},
		{[]string{"\x1b[" + strings.Repeat("0", 63) + "5ia"}, "a"},
		{[]string{"\x1b[" + strings.Repeat("0", 5000) + "5ia"}, "a"},
		// split across writes
		{[]string{"a\x1b", "[", "5", "ib"}, "ab"},
		{[]string{"x\x1b[3", "1m"}, "x\x1b[31m"},
		// supported sequences pass, numbers are capped
		{[]string{"\x1b[01;31mred\x1b[0m"}, "\x1b[1;31mred\x1b[0m"},
		{[]string{"\x1b[?1049h", "\x1b[?1049l"}, "\x1b[?1049h\x1b[?1049l"},
		{[]string{"A\x1b[2147483647b"}, "A\x1b[9999b"},
		{[]string{"\x1b[1;100000000r"}, "\x1b[1;9999r"},
		{[]string{"\x1bc\x1b7\x1b(B"}, "\x1bc\x1b7\x1b(B"},
		// titles pass, other OSC commands are dropped
		{[]string{"\x1b]0;title\a"}, "\x1b]0;title\a"},
		{[]string{"\x1b]2;tit", "le\x1b\\x"}, "\x1b]2;title\ax"},
		{[]string{"\x1b]7;file:///tmp\ax"}, "x"},
		{[]string{"\x1b]7;ab\a"}, ""},
		{[]string{"\x1b]52;c;ZXZpbA==\ax"}, "x"},
		{[]string{"\x1b]0;evil\x1b[2Jtitle\a"}, "\x1b[2Jtitle\a"},
		{[]string{"\x1b]0;" + strings.Repeat("t", 300) + "\ax"}, "x"},
		// DCS, APC, PM and SOS strings are dropped
		{[]string{"\x1bP+q544e\x1b\\x"}, "x"},
		{[]string{"\x1b_payload\x00more\x1b\\x"}, "x"},
		// sequences the widget would partly print as text
		{[]string{"\x1b[2 qx"}, "x"},
		{[]string{"\x1b[38:2::255:0:0mx"}, "x"},
		{[]string{"\x1b[éx"}, "éx"},
		// a control character or ESC breaks off a sequence
		{[]string{"\x1b[12\n34"}, "\n34"},
		{[]string{"\x1b\x1b[5i"}, ""},
		{[]string{"\x1b\x1b[1m"}, "\x1b[1m"},
		// other C0 controls and DEL are not printed
		{[]string{"a\x00b\x05c\x7fd\te"}, "abcd\te"},
	}
	for _, tt := range tests {
		var f outputFilter
		var out []byte
		for _, c := range tt.chunks {
			out = f.write(out, []byte(c))
		}
		if string(out) != tt.want {
			t.Errorf("filter(%q) = %q, want %q", tt.chunks, out, tt.want)
		}
	}

	// An unfinished string swallows at most maxStringLen bytes.
	var f outputFilter
	out := f.write(nil, []byte("\x1bP"))
	out = f.write(out, make([]byte, maxStringLen+1))
	out = f.write(out, []byte("visible"))
	if string(out) != "visible" {
		t.Fatalf("after overlong DCS: %q", out)
	}
	if cap(f.held) > maxCSILen+maxTitleLen {
		t.Fatalf("filter holds %d bytes", cap(f.held))
	}
}

// TestTerminalSurvivesMaliciousOutput feeds output that used to crash or
// freeze fyne-io/terminal directly to the (patched) widget, without the
// filter in front of it.
func TestTerminalSurvivesMaliciousOutput(t *testing.T) {
	test.NewApp()
	term := terminal.New()
	w := test.NewWindow(term)
	defer w.Close()
	w.Resize(fyne.NewSize(400, 300))

	r, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- term.RunWithConnection(discard{}, r) }()

	inputs := []string{
		"\x1b[é",
		"\x1b[10;5H\x1b[@",
		"abc\x1b[1;20H\x1b[@",
		"abc\x1b[1;20H\x1b[P",
		"\x1b[h\x1b[l",
		"A\x1b[2147483647b",
		"\x1b[1;100000000r\x1b[M\x1bM\x1b[L\x1b[S\x1b[T",
		"\x1b[99999999L\x1b[99999999M",
		"\x1b]7;ab\a\x1b]7;hello\a",
		"\x1b[" + strings.Repeat("1", 100000) + "m",
		"\x1b]0;" + strings.Repeat("t", 100000) + "\x1b\\",
		"still alive\r\n",
	}
	wd, _ := os.Getwd()
	write := make(chan struct{})
	go func() {
		for _, in := range inputs {
			pw.Write([]byte(in))
		}
		pw.Close()
		close(write)
	}()
	select {
	case <-write:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal did not process the output in time")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal did not finish")
	}
	if now, _ := os.Getwd(); now != wd {
		t.Fatalf("working directory changed to %s", now)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
func (discard) Close() error                { return nil }

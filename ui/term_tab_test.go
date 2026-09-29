package ui

import (
	"io"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"
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
	b.Write(make([]byte, maxPending))
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
	b.Write(make([]byte, maxPending))
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

func TestFilterMediaCopy(t *testing.T) {
	tests := []struct {
		chunks []string
		want   string
	}{
		{[]string{"plain text\r\n"}, "plain text\r\n"},
		{[]string{"\x1b[5ievil payload\x1b[4iafter"}, "evil payloadafter"},
		{[]string{"\x1b[?5i", "x", "\x1b[?4i"}, "x"},
		// split across writes
		{[]string{"a\x1b", "[", "5", "ib"}, "ab"},
		// other sequences pass through untouched
		{[]string{"\x1b[01;31mred\x1b[0m"}, "\x1b[01;31mred\x1b[0m"},
		{[]string{"\x1b[?1049h", "\x1b[?1049l"}, "\x1b[?1049h\x1b[?1049l"},
		{[]string{"\x1b]0;title\a"}, "\x1b]0;title\a"},
		{[]string{"\x1bc\x1b7"}, "\x1bc\x1b7"},
		{[]string{"\x1b\x1b[5i"}, "\x1b"},
		{[]string{"\x1b[12\n34"}, "\x1b[12\n34"},
		{[]string{"umlaut äöü ✓"}, "umlaut äöü ✓"},
	}
	for _, tt := range tests {
		var out, seq []byte
		for _, c := range tt.chunks {
			out = filterMediaCopy(out, []byte(c), &seq)
		}
		if string(out) != tt.want {
			t.Errorf("filter(%q) = %q, want %q", tt.chunks, out, tt.want)
		}
	}

	// An unfinished sequence is held back, not lost.
	var out, seq []byte
	out = filterMediaCopy(out, []byte("x\x1b[3"), &seq)
	if string(out) != "x" {
		t.Fatalf("partial: %q", out)
	}
	out = filterMediaCopy(out, []byte("1m"), &seq)
	if string(out) != "x\x1b[31m" {
		t.Fatalf("completed: %q", out)
	}
}

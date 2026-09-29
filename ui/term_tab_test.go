package ui

import (
	"io"
	"testing"
	"time"
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
		"report.pdf":      "report.pdf",
		"../../.bashrc":   ".._.._.bashrc",
		`..\..\evil.exe`:  ".._.._evil.exe",
		"/etc/passwd":     "_etc_passwd",
		"file.txt:stream": "file.txt_stream",
		"a\x1b[2Jb":       "a_[2Jb",
		"..":              "download",
		".":               "download",
		"":                "download",
		"Ümlaut ✓.txt":    "Ümlaut ✓.txt",
	}
	for in, want := range tests {
		if got := localFileName(in); got != want {
			t.Errorf("localFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

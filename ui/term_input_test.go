package ui

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"
	"mtssh/config"
)

// termBlockingSend is a send function for terminalInput that stands for a
// server that does not read: each chunk is handed to the test through
// entered, then the send blocks until release is closed.
type termBlockingSend struct {
	entered chan string
	release chan struct{}
}

func termNewBlockingSend() *termBlockingSend {
	return &termBlockingSend{entered: make(chan string), release: make(chan struct{})}
}

func (b *termBlockingSend) send(p []byte) {
	b.entered <- string(p)
	<-b.release
}

// next returns the chunk the input goroutine sends next.
func (b *termBlockingSend) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-b.entered:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no input was sent")
		return ""
	}
}

// termWrite calls in.Write and fails if it blocks.
func termWrite(t *testing.T, in *terminalInput, s string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if n, err := in.Write([]byte(s)); n != len(s) || err != nil {
			t.Errorf("Write(%d bytes) = %d, %v", len(s), n, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked")
	}
}

// Input never blocks the UI goroutine, keeps its order and is bounded (M-02).
func TestTermInputQueue(t *testing.T) {
	b := termNewBlockingSend()
	in := termStartInput("test", b.send)
	defer in.stop()

	buf := []byte("first")
	in.Write(buf)
	copy(buf, "XXXXX") // the widget may reuse its buffer
	if got := b.next(t); got != "first" {
		t.Fatalf("sent %q, want %q", got, "first")
	}

	// The send is stuck; the queue takes termInputChunks writes, then drops.
	for i := 0; i < termInputChunks; i++ {
		termWrite(t, in, strconv.Itoa(i)+",")
	}
	termWrite(t, in, "dropped,")

	close(b.release)
	var got strings.Builder
	for i := 0; i < termInputChunks; i++ {
		got.WriteString(b.next(t))
	}
	var want strings.Builder
	for i := 0; i < termInputChunks; i++ {
		want.WriteString(strconv.Itoa(i) + ",")
	}
	if got.String() != want.String() {
		t.Fatalf("sent %q, want %q", got.String(), want.String())
	}

	// Once there is room again, input is accepted again.
	termWrite(t, in, "again")
	if got := b.next(t); got != "again" {
		t.Fatalf("after the overflow sent %q, want %q", got, "again")
	}
}

func TestTermInputByteLimit(t *testing.T) {
	b := termNewBlockingSend()
	in := termStartInput("test", b.send)
	defer in.stop()

	termWrite(t, in, "a")
	b.next(t) // stuck sending "a"

	big := strings.Repeat("x", termInputMaxBytes)
	termWrite(t, in, big)
	termWrite(t, in, "b") // over termInputMaxBytes: dropped
	close(b.release)
	if got := b.next(t); got != big {
		t.Fatalf("sent %d bytes, want %d", len(got), len(big))
	}
	termWrite(t, in, "c")
	if got := b.next(t); got != "c" {
		t.Fatalf("sent %q after the big write, want %q", got, "c")
	}

	// A single larger write (a big paste) fits into an empty queue.
	huge := strings.Repeat("y", termInputMaxBytes+1)
	termWrite(t, in, huge)
	if got := b.next(t); len(got) != len(huge) {
		t.Fatalf("sent %d bytes, want %d", len(got), len(huge))
	}
}

func TestTermInputStop(t *testing.T) {
	b := termNewBlockingSend()
	in := termStartInput("test", b.send)

	termWrite(t, in, "a")
	b.next(t) // stuck sending "a"
	in.stop()
	select {
	case <-in.done:
		t.Fatal("goroutine ended during a send")
	default:
	}
	close(b.release) // e.g. Disconnect closed the client
	select {
	case <-in.done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not end after stop")
	}
	termWrite(t, in, "late") // accepted and ignored, no panic
	in.stop()                // twice is fine
}

// termListener counts TCP connections and closes them at once.
func termListener(t *testing.T) (port int, accepted chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted = make(chan struct{}, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, accepted
}

func termTestTab(t *testing.T, port int) *TermTab {
	t.Helper()
	// Should a connection be made after all, don't touch the user's files.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	test.NewApp()
	w := test.NewWindow(nil)
	t.Cleanup(w.Close)
	// Not shown: the terminal does not start reading its output.
	return NewTermTab(config.Session{Label: "test", Host: "127.0.0.1", Port: port, User: "u", Password: "p"}, w)
}

// A tab that is closed does not connect any more (M-24), and Close ends
// its input goroutine (M-02).
func TestTermCloseBeforeConnect(t *testing.T) {
	port, accepted := termListener(t)
	tt := termTestTab(t, port)
	tt.Close()
	select {
	case <-tt.input.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the input goroutine")
	}

	tt.connect() // e.g. a Reconnect that was waiting for connectMu
	if tt.session() != nil {
		t.Fatal("connect() started a session for a closed tab")
	}
	select {
	case <-accepted:
		t.Fatal("connect() dialed for a closed tab")
	default:
	}
}

// Close while connect() is under way, before it stores its session: the
// session must not connect (M-24).
func TestTermCloseDuringConnect(t *testing.T) {
	port, accepted := termListener(t)
	tt := termTestTab(t, port)

	// Nobody reads the output, so connect() blocks on its first status
	// line until Close() closes the output.
	tt.output.Write([]byte(strings.Repeat("x", maxPending)))
	done := make(chan struct{})
	go func() {
		tt.connect()
		close(done)
	}()
	for deadline := time.Now().Add(5 * time.Second); tt.connectMu.TryLock(); {
		tt.connectMu.Unlock() // connect() has not started yet
		if time.Now().After(deadline) {
			t.Fatal("connect() did not start")
		}
		time.Sleep(time.Millisecond)
	}
	// Give connect() time to get past its first check; if it has not, that
	// check stops it instead, which is fine too.
	time.Sleep(50 * time.Millisecond)

	tt.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("connect() did not return after Close")
	}
	if tt.session() != nil {
		t.Fatal("Close during connect left a session")
	}
	select {
	case <-accepted:
		t.Fatal("connect() dialed after Close")
	default:
	}
}

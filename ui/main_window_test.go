package ui

import (
	"mtssh/core"
	"slices"
	"sync"
	"testing"

	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// windowsTestSetup registers a window's tab container holding a terminal
// stand-in and an SFTP tab with a transfer that, once cancelled, waits for
// cleanup (if not nil) before it cleans up and frees the slot. events
// records what happened, in order.
type windowsTestSetup struct {
	windows *openWindows
	tabs    *DraggableTabContainer
	sftp    *SFTPTab
	mu      sync.Mutex
	events  []string
}

func newWindowsTestSetup(t *testing.T, cleanup <-chan struct{}) *windowsTestSetup {
	t.Helper()
	test.NewApp()
	w := &windowsTestSetup{windows: newOpenWindows(), tabs: NewDraggableTabContainer()}
	w.windows.addTabs(w.tabs)

	term := NewDraggableTabItem("terminal", nil, widget.NewLabel(""))
	term.OnClose = func() { w.record("terminal closed") }
	w.tabs.Append(term)

	w.sftp = newSFTPTab(&core.SFTPClient{}, test.NewWindow(nil))
	item := NewDraggableTabItem("SFTP", nil, w.sftp.Container)
	item.OnClose = w.sftp.Close
	w.windows.addSFTP(w.sftp, w.tabs)
	w.tabs.Append(item)

	ctx, release, _ := w.sftp.reserveTransfer()
	go func() {
		<-ctx.Done()
		if cleanup != nil {
			<-cleanup
		}
		if w.sftp.sftp.Closed() {
			t.Error("SFTP connection closed before the transfer cleaned up")
		}
		w.record("transfer cleaned up")
		release()
	}()
	return w
}

func (w *windowsTestSetup) record(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, event)
}

func (w *windowsTestSetup) recorded() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.events)
}

// Quitting cancels running transfers, lets them clean up over the SSH
// connection and only then closes the terminals (and their connections)
// and quits.
func TestOpenWindowsQuit(t *testing.T) {
	w := newWindowsTestSetup(t, nil)
	if !w.windows.busy() {
		t.Fatal("running transfer not reported")
	}
	quit := make(chan struct{})
	w.windows.quit(func() {
		w.record("quit")
		close(quit)
	})
	sftpWaitDone(t, quit, "quit")
	want := []string{"transfer cleaned up", "terminal closed", "quit"}
	if got := w.recorded(); !slices.Equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
	if !w.sftp.sftp.Closed() {
		t.Error("SFTP connection not closed")
	}
}

// Asked to quit again while transfers clean up, it quits at once.
func TestOpenWindowsQuitAgain(t *testing.T) {
	cleanup := make(chan struct{})
	w := newWindowsTestSetup(t, cleanup)
	quit := make(chan struct{})
	w.windows.quit(func() {
		w.record("quit")
		close(quit)
	})
	again := false
	w.windows.quit(func() { again = true })
	if !again {
		t.Fatal("second request did not quit at once")
	}
	if got := w.recorded(); len(got) != 0 {
		t.Fatalf("events before the transfer cleaned up: %q", got)
	}
	close(cleanup)
	sftpWaitDone(t, quit, "first quit")
}

// Closing a window closes its SFTP tabs first, and its terminals once
// their transfers have cleaned up.
func TestOpenWindowsCloseTabs(t *testing.T) {
	w := newWindowsTestSetup(t, nil)
	closed := make(chan struct{})
	w.windows.closeTabs([]*DraggableTabContainer{w.tabs}, func() { close(closed) })
	sftpWaitDone(t, closed, "closeTabs")
	want := []string{"transfer cleaned up", "terminal closed"}
	if got := w.recorded(); !slices.Equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
	if len(w.tabs.Items()) != 0 || w.windows.tabs[w.tabs] {
		t.Fatal("window's tabs not closed and forgotten")
	}
	// The closed SFTP tab is forgotten when the next one is added.
	next := newSFTPTab(&core.SFTPClient{}, test.NewWindow(nil))
	w.windows.addSFTP(next, NewDraggableTabContainer())
	if _, ok := w.windows.sftp[w.sftp]; ok || len(w.windows.sftp) != 1 {
		t.Fatalf("closed SFTP tab still tracked: %d tabs", len(w.windows.sftp))
	}
}

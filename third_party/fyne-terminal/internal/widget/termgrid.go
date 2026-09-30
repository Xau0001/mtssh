package widget

import (
	"context"
	"sync"
	"time"

	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"fyne.io/fyne/v2"
)

const blinkingInterval = 500 * time.Millisecond

// TermGrid is a monospaced grid of characters.
// This is designed to be used by our terminal emulator.
type TermGrid struct {
	widget.TextGrid

	// MTSSH patch: blinkLock guards tickerCancel and closed, which
	// StopBlink and Close may use from any goroutine.
	blinkLock    sync.Mutex
	tickerCancel context.CancelFunc
	closed       bool // blinking is stopped for good
}

// StopBlink stops the goroutine that makes text blink. The next Refresh
// starts it again if blinking text is on screen. MTSSH patch: used when
// the renderer is destroyed; the goroutine ran on, redrawing the grid
// twice a second and keeping it in memory.
func (t *TermGrid) StopBlink() {
	t.blinkLock.Lock()
	defer t.blinkLock.Unlock()
	if t.tickerCancel != nil {
		t.tickerCancel()
		t.tickerCancel = nil
	}
}

// Close stops blinking for good; the terminal is closed (MTSSH patch).
func (t *TermGrid) Close() {
	t.blinkLock.Lock()
	t.closed = true
	t.blinkLock.Unlock()
	t.StopBlink()
}

// CreateRenderer is a private method to Fyne which links this widget to it's renderer
func (t *TermGrid) CreateRenderer() fyne.WidgetRenderer {
	t.ExtendBaseWidget(t)

	return t.TextGrid.CreateRenderer()
}

// NewTermGrid creates a new empty TextGrid widget.
func NewTermGrid() *TermGrid {
	grid := &TermGrid{}
	grid.ExtendBaseWidget(grid)

	grid.Scroll = container.ScrollNone
	return grid
}

// Refresh will be called when this grid should update.
// We update our blinking status and then call the TextGrid we extended to refresh too.
func (t *TermGrid) Refresh() {
	t.refreshBlink(false)
}

func (t *TermGrid) refreshBlink(blink bool) {
	// reset shouldBlink which can be set by setCellRune if a cell with BlinkEnabled is found
	shouldBlink := false

	for _, row := range t.Rows {
		for _, r := range row.Cells {
			if s, ok := r.Style.(*TermTextGridStyle); ok && s != nil && s.BlinkEnabled {
				shouldBlink = true

				s.blink(blink)
			}
		}
	}
	fyne.Do(t.TextGrid.Refresh) // TODO fix root cause in refresh on wrong thread

	t.blinkLock.Lock()
	defer t.blinkLock.Unlock()
	switch {
	case shouldBlink && t.tickerCancel == nil && !t.closed:
		t.runBlink()
	case !shouldBlink && t.tickerCancel != nil:
		t.tickerCancel()
		t.tickerCancel = nil
	}
}

// runBlink is called with blinkLock held.
func (t *TermGrid) runBlink() {
	if t.tickerCancel != nil {
		t.tickerCancel()
		t.tickerCancel = nil
	}
	var tickerContext context.Context
	tickerContext, t.tickerCancel = context.WithCancel(context.Background())
	ticker := time.NewTicker(blinkingInterval)
	blinking := false
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-tickerContext.Done():
				return
			case <-ticker.C:
				blinking = !blinking
				b := blinking // MTSSH patch: the closure runs later, on the UI goroutine
				fyne.Do(func() {
					t.refreshBlink(b)
				})
			}
		}
	}()
}

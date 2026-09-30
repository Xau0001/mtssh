# Patched fyne-io/terminal

Copy of [github.com/fyne-io/terminal](https://github.com/fyne-io/terminal)
at `v0.0.0-20260927151117-c8f30fa130e3` (BSD 3-Clause, see `LICENSE`),
sources only. `go.mod` in the MTSSH root points here with a `replace`
directive.

The widget parses output from the SSH server. Upstream, several malformed
escape sequences panic inside handlers run by `fyne.Do` on Fyne's main
loop, where nothing recovers, so a server could crash the whole client
(every tab and window). Others looped for minutes or buffered without
limit, and OSC 7 called `os.Chdir` on the client process.

Changes against upstream:

- `safe.go` (new): `safeDo` / `safeDoAndWait` recover from panics in
  handlers; `handleOutputSafely` does the same for the parser in the read
  loop. Limits: `maxCodeLen` for collected sequences, `maxPrintData` for
  printer mode.
- `output.go`: all handlers dispatched through `safeDo`; ESC 7/8/D/M and
  clearing the selection at new output now also run on the UI goroutine
  (they changed screen state from the read goroutine); SO/SI switch the
  character set on the parser goroutine, which alone uses `useG1CharSet`,
  `g0Charset` and `g1Charset` (queued, the switch came too late for the
  characters after it); a UTF-8 character split across reads is returned
  as leftover (a copy) and completed by the next read instead of dropped;
  `scrollUp` uses `SetRow` (no out-of-range write); sequence and print
  buffers are bounded; a bell while one is shown is ignored (one redraw and
  one timer at a time) and its reset runs on the UI goroutine. TAB moves
  the cursor to the next tab stop or the last column and writes nothing,
  like xterm (it wrote spaces up to a stop past the right edge forever).
  `scrollUp` / `scrollDown` no longer refresh the grid for every line (a
  flood of newlines took over 30 s of UI time per read); `run()`
  refreshes after each read. `scrollDown` works like `scrollUp` (it left
  the last written row on screen when fewer rows than the region had been
  written). ESC D / ESC M (IND / RI) move the cursor and scroll only at
  the bottom / top margin. `parseState.esc` is a bool (it held the ESC's
  position, with 5000 for none, so an ESC at that position was lost).
- `term.go`: the read loop uses `handleOutputSafely` and `safeDoAndWait`
  and refreshes after every read, also when a character is held back.
  `RunWithConnection` runs on a goroutine of its own: it sets `in` under
  `inLock`, which `Write` takes (all input goes through `Write`), and
  reads the size / sets PWD in `config` under `listenerLock`, which
  `Resize` takes to set the size. `Close` stops blinking for good.
- `internal/widget/termgrid.go`: the blink goroutine can be stopped
  (`StopBlink`, `Close`, under `blinkLock`); it ran on after the terminal
  was closed with blinking text on screen. Its ticker is stopped, and the
  UI closure gets a copy of the blink state. `render.go`: `Destroy` stops
  it.
- `mouse.go`: coordinates in mouse reports are limited to the screen and
  to 1…223 (one byte each); larger ones wrapped into control characters.
- `input.go`: Shift+Tab sends `CSI Z`; Shift with Return, Enter,
  Backspace or Escape sends what the key sends alone (it sent nothing);
  Shift with the cursor keys and F1–F4 sends `CSI 1;2 A…D` / `CSI 1;2
  P…S` like xterm (F1 and F2 sent the VT220 codes of F13 and F14, F3 and
  the cursor keys malformed sequences).
- `color.go`: an SGR parameter that is not a number is logged only in
  debug mode, like other unsupported ones (a server could turn each byte
  of output into more than 100 bytes on stderr).
- `osc.go`: OSC 7 (set working directory) is ignored; `setDirectory` is gone.
- `escape.go`: final byte found by rune, not byte length (`ESC[é`
  panicked); DCH/ICH work on rows that end before the cursor or don't exist
  yet; `ESC[h` / `ESC[l` without parameters no longer panic; IL inserts
  blank lines (it copied lines from above the cursor) and does nothing
  outside the scroll region; DECSTBM works like in xterm: the bottom is
  clamped to the screen, 0 means the default, and regions of less than two
  rows are ignored, keeping the previous region, as are private forms
  (`CSI ? … r`); line counts (IL, DL, SU, SD) are capped to the screen
  height, REP to one line.
- `select.go`: a bracketed paste keeps only tab, LF and CR of the control
  characters (C0, DEL and C1 are removed, like xterm and VTE), so the
  pasted text can neither end the paste early with `ESC[201~` nor send
  e.g. ^C to the remote tty, which made the shell run the rest as typed.

MTSSH additionally filters the output before it reaches the widget
(`ui/term_filter.go`) and writes the widget's input to the SSH session from
a goroutine of its own (`ui/term_tab.go`), never on the UI goroutine. Before
each session and after it, `resetScreen` (`ui/term_tab.go`) turns off the
modes the widget knows (mouse reports, bracketed paste, …); extend it if
the widget learns more.

To update: copy the new upstream sources over these files (no tests or
sample data), re-apply the changes above and run `go test -race ./ui/`
(`TestTerminalSurvivesMaliciousOutput` and the tests in
`ui/term_widget_test.go` cover them).

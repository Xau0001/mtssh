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
  one timer at a time) and its reset runs on the UI goroutine.
- `term.go`: the read loop uses `handleOutputSafely` and `safeDoAndWait`
  and refreshes after every read, also when a character is held back.
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
- `select.go`: a bracketed paste has every ESC removed, so the pasted text
  cannot end the paste early with `ESC[201~`.

MTSSH additionally filters the output before it reaches the widget
(`ui/term_filter.go`) and writes the widget's input to the SSH session from
a goroutine of its own (`ui/term_tab.go`), never on the UI goroutine.

To update: copy the new upstream sources over these files (no tests or
sample data), re-apply the changes above and run `go test -race ./ui/`
(`TestTerminalSurvivesMaliciousOutput` and the tests in
`ui/term_widget_test.go` cover them).

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
- `output.go`: all handlers dispatched through `safeDo`; ESC 7/8/D/M now
  also run on the UI goroutine (they changed screen state from the read
  goroutine); `scrollUp` uses `SetRow` (no out-of-range write); sequence
  and print buffers are bounded; the bell reset runs on the UI goroutine.
- `term.go`: the read loop uses `handleOutputSafely` and `safeDoAndWait`.
- `osc.go`: OSC 7 (set working directory) is ignored; `setDirectory` is gone.
- `escape.go`: final byte found by rune, not byte length (`ESC[é`
  panicked); DCH/ICH work on rows that end before the cursor or don't exist
  yet; `ESC[h` / `ESC[l` without parameters no longer panic; invalid scroll
  regions are ignored like in xterm; line counts (IL, DL, SU, SD) and REP
  are capped to the screen size.

MTSSH additionally filters the output before it reaches the widget
(`ui/term_filter.go`).

To update: copy the new upstream sources over these files (no tests or
sample data), re-apply the changes above and run `go test -race ./ui/`
(`TestTerminalSurvivesMaliciousOutput` covers them).
